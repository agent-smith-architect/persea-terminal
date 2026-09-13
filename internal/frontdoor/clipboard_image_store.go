package frontdoor

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"persea-terminal/internal/proto"
)

const clipboardImageTTL = 30 * time.Minute
const clipboardImageMaxCount = 20
const clipboardImageMaxTotalBytes = 64 << 20
const clipboardImageMetadataMaxBytes = 1024
const clipboardImageDirectory = "clipboard-images"

var errClipboardImageUnavailable = errors.New("image clipboard unavailable")
var errClipboardImageNotFound = errors.New("clipboard image not found")
var errClipboardImageFull = errors.New("image clipboard full")
var errClipboardImageInvalid = errors.New("invalid clipboard image")
var errClipboardImageTooLarge = errors.New("clipboard image too large")
var errClipboardImageConflict = errors.New("clipboard image revision conflict")
var errClipboardImageRetention = errors.New("invalid clipboard image retention")
var clipboardImageTempRE = regexp.MustCompile(`^\.[0-9a-f]{32}\.[0-9a-f]{16}\.tmp$`)

type clipboardImageRecord struct {
	ID               string    `json:"id"`
	MediaType        string    `json:"media_type"`
	ByteSize         int       `json:"byte_size"`
	CreatedAt        time.Time `json:"created_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	Origin           string    `json:"origin"`
	UpdatedAt        time.Time `json:"updated_at"`
	Revision         uint64    `json:"revision"`
	RetentionSeconds int       `json:"retention_seconds"`
}

func (r clipboardImageRecord) MarshalJSON() ([]byte, error) {
	type record clipboardImageRecord
	var expiry *time.Time
	if !r.ExpiresAt.IsZero() {
		value := r.ExpiresAt
		expiry = &value
	}
	return json.Marshal(struct {
		record
		ExpiresAt *time.Time `json:"expires_at"`
	}{record(r), expiry})
}

func (r clipboardImageRecord) expired(now time.Time) bool {
	return !r.ExpiresAt.IsZero() && !now.Before(r.ExpiresAt)
}

// Images are immutable, individually atomic records outside snippets.json.
// Each 0600 file contains PCI1, a bounded JSON header length, the header, then
// original image bytes. One rename publishes both metadata and payload, so a
// crash cannot leave an index referring to an absent image. Old releases leave
// this directory alone; no text store schema or saved snippet is changed.
type clipboardImageStore struct {
	mu            sync.Mutex
	file          *durableFile
	now           func() time.Time
	maxImageBytes int
	records       map[string]clipboardImageRecord
	digests       map[string][sha256.Size]byte
}

func openClipboardImageStore(snippetPath string, maxImageBytes int) (*clipboardImageStore, error) {
	if snippetPath == "" || maxImageBytes < 0 || maxImageBytes > proto.MaxImage {
		return nil, errClipboardImageUnavailable
	}
	path := filepath.Join(filepath.Dir(snippetPath), clipboardImageDirectory)
	if maxImageBytes == 0 {
		// Turning off uploads must not strand previously stored secrets. An
		// existing store still loads and expires; the API remains disabled.
		if _, err := os.Lstat(path); err != nil {
			return nil, errClipboardImageUnavailable
		}
		maxImageBytes = proto.MaxImage
	}
	return newClipboardImageStore(path, maxImageBytes, time.Now)
}

func newClipboardImageStore(path string, maxImageBytes int, now func() time.Time) (*clipboardImageStore, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || maxImageBytes <= 0 || maxImageBytes > proto.MaxImage {
		return nil, errClipboardImageUnavailable
	}
	parent := filepath.Dir(path)
	if err := validateStoreParent(parent, "image clipboard", errClipboardImageUnavailable); err != nil {
		return nil, errClipboardImageUnavailable
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, errClipboardImageUnavailable
	}
	err = root.Mkdir(filepath.Base(path), 0700)
	if err != nil && !errors.Is(err, os.ErrExist) {
		_ = root.Close()
		return nil, errClipboardImageUnavailable
	}
	// Persist the new directory entry before any upload can be acknowledged.
	dir, err := root.Open(".")
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	_ = root.Close()
	if err != nil {
		return nil, errClipboardImageUnavailable
	}
	file, err := openDurableFile(filepath.Join(path, "record"), "image clipboard", proto.MaxImage+clipboardImageMetadataMaxBytes+8, errClipboardImageUnavailable)
	if err != nil {
		return nil, errClipboardImageUnavailable
	}
	s := &clipboardImageStore{file: file, now: now, maxImageBytes: maxImageBytes, records: make(map[string]clipboardImageRecord), digests: make(map[string][sha256.Size]byte)}
	if err := s.load(); err != nil {
		s.close()
		return nil, errClipboardImageUnavailable
	}
	return s, nil
}

func (s *clipboardImageStore) close() {
	if s != nil {
		_ = s.file.fs.(rootAliasFS).root.Close()
	}
}

func validClipboardImageOrigin(origin string) bool {
	return origin == strings.TrimSpace(origin) && validSnippetText(origin, snippetOriginMaxRunes)
}

func validClipboardImageRecord(record clipboardImageRecord) bool {
	return snippetIDRE.MatchString(record.ID) && allowedImageMediaTypes[record.MediaType] &&
		record.ByteSize > 0 && record.ByteSize <= proto.MaxImage && !record.CreatedAt.IsZero() &&
		!record.UpdatedAt.Before(record.CreatedAt) && record.Revision > 0 && validClipboardRetention(record.RetentionSeconds) &&
		(record.RetentionSeconds == 0) == record.ExpiresAt.IsZero() && (record.ExpiresAt.IsZero() || !record.ExpiresAt.Before(record.CreatedAt)) && validClipboardImageOrigin(record.Origin)
}

func (s *clipboardImageStore) directory() (*os.File, error) {
	dir, err := s.file.fs.Open(".")
	if err != nil {
		return nil, errClipboardImageUnavailable
	}
	info, err := dir.Stat()
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		_ = dir.Close()
		return nil, errClipboardImageUnavailable
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		_ = dir.Close()
		return nil, errClipboardImageUnavailable
	}
	return dir, nil
}

func validClipboardImageFile(info os.FileInfo, maxBytes int) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() < 0 || info.Size() > int64(maxBytes) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid()) && stat.Nlink == 1
}

func (s *clipboardImageStore) readFile(id string) (clipboardImageRecord, []byte, error) {
	var record clipboardImageRecord
	info, err := s.file.fs.Lstat(id)
	if err != nil || !validClipboardImageFile(info, s.file.maxBytes) {
		return record, nil, errClipboardImageUnavailable
	}
	f, err := s.file.fs.OpenFile(id, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return record, nil, errClipboardImageUnavailable
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !validClipboardImageFile(opened, s.file.maxBytes) || !os.SameFile(info, opened) {
		return record, nil, errClipboardImageUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(s.file.maxBytes)+1))
	if err != nil || len(data) > s.file.maxBytes || len(data) < 8 || (string(data[:4]) != "PCI1" && string(data[:4]) != "PCI2") {
		return record, nil, errClipboardImageUnavailable
	}
	headerSize := int(binary.BigEndian.Uint32(data[4:8]))
	if headerSize < 2 || headerSize > clipboardImageMetadataMaxBytes || headerSize > len(data)-8 {
		return record, nil, errClipboardImageUnavailable
	}
	header := data[8 : 8+headerSize]
	if !utf8.Valid(header) || rejectAliasDuplicateKeys(header) != nil || rejectNonCanonicalKeys(header) != nil || decodeStrict(header, &record) != nil {
		return clipboardImageRecord{}, nil, errClipboardImageUnavailable
	}
	if string(data[:4]) == "PCI1" && record.Revision == 0 {
		if !record.ExpiresAt.Equal(record.CreatedAt.Add(clipboardImageTTL)) {
			return clipboardImageRecord{}, nil, errClipboardImageUnavailable
		}
		record.Revision = 1
		record.UpdatedAt = record.CreatedAt
		record.RetentionSeconds = clipboardDefaultRetention
	}
	if !validClipboardImageRecord(record) || record.ID != id {
		return clipboardImageRecord{}, nil, errClipboardImageUnavailable
	}
	body := data[8+headerSize:]
	if len(body) != record.ByteSize || imageMediaType(body) != record.MediaType || !validImageDimensions(record.MediaType, body) {
		return clipboardImageRecord{}, nil, errClipboardImageUnavailable
	}
	return record, body, nil
}

func (s *clipboardImageStore) load() error {
	dir, err := s.directory()
	if err != nil {
		return err
	}
	defer dir.Close()
	// A single interrupted publication can leave one temp file. More files
	// than the entire capacity plus one are unsafe state, not an unbounded scan.
	entries, err := dir.ReadDir(clipboardImageMaxCount + 2)
	if (err != nil && !errors.Is(err, io.EOF)) || len(entries) > clipboardImageMaxCount+1 {
		return errClipboardImageUnavailable
	}
	removed := false
	total := 0
	for _, entry := range entries {
		name := entry.Name()
		if clipboardImageTempRE.MatchString(name) {
			info, err := s.file.fs.Lstat(name)
			if err != nil || !validClipboardImageFile(info, s.file.maxBytes) || s.file.fs.Remove(name) != nil {
				return errClipboardImageUnavailable
			}
			removed = true
			continue
		}
		if !snippetIDRE.MatchString(name) {
			return errClipboardImageUnavailable
		}
		record, body, err := s.readFile(name)
		if err != nil {
			return err
		}
		if record.expired(s.now()) {
			if err := s.file.fs.Remove(name); err != nil {
				return errClipboardImageUnavailable
			}
			removed = true
			continue
		}
		s.records[name] = record
		s.digests[name] = sha256.Sum256(body)
		total += record.ByteSize
	}
	if removed && s.file.dirSync(dir) != nil {
		return errClipboardImageUnavailable
	}
	if len(s.records) > clipboardImageMaxCount || total > clipboardImageMaxTotalBytes {
		return errClipboardImageUnavailable
	}
	// Legacy uploads could contain identical payloads under different IDs.
	// Validate every file first, publish the merged survivor before removing
	// any duplicate, and expose no store if migration cannot finish durably.
	ids := make([]string, 0, len(s.records))
	for id := range s.records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	seen := map[[sha256.Size]byte]string{}
	for _, id := range ids {
		digest := s.digests[id]
		if survivor, ok := seen[digest]; ok {
			keep, payload, err := s.readFile(survivor)
			if err != nil {
				return err
			}
			other, duplicate, err := s.readFile(id)
			if err != nil {
				return err
			}
			if !bytes.Equal(payload, duplicate) {
				return errClipboardImageUnavailable
			}
			keep.RetentionSeconds = longerClipboardRetention(keep.RetentionSeconds, other.RetentionSeconds)
			if keep.RetentionSeconds == 0 {
				keep.ExpiresAt = time.Time{}
			} else if other.ExpiresAt.After(keep.ExpiresAt) {
				keep.ExpiresAt = other.ExpiresAt
			}
			if other.UpdatedAt.After(keep.UpdatedAt) {
				keep.UpdatedAt = other.UpdatedAt
			}
			if other.Revision >= keep.Revision {
				if other.Revision == ^uint64(0) {
					return errClipboardImageUnavailable
				}
				keep.Revision = other.Revision + 1
			}
			if err := s.publishLocked(keep, payload); err != nil {
				return err
			}
			if err := s.file.fs.Remove(id); err != nil {
				return errClipboardImageUnavailable
			}
			delete(s.records, id)
			delete(s.digests, id)
			removed = true
		} else {
			seen[digest] = id
		}
	}
	if removed && s.file.dirSync(dir) != nil {
		return errClipboardImageUnavailable
	}
	return nil
}

func (s *clipboardImageStore) reapLocked() error {
	if s.file.fault != nil {
		return errClipboardImageUnavailable
	}
	dir, err := s.directory()
	if err != nil {
		return err
	}
	defer dir.Close()
	removed := false
	entries, err := dir.ReadDir(clipboardImageMaxCount + 1)
	if (err != nil && !errors.Is(err, io.EOF)) || len(entries) != len(s.records) {
		return errClipboardImageUnavailable
	}
	for _, entry := range entries {
		if _, ok := s.records[entry.Name()]; !ok {
			return errClipboardImageUnavailable
		}
	}
	for id, record := range s.records {
		info, err := s.file.fs.Lstat(id)
		if err != nil || !validClipboardImageFile(info, s.file.maxBytes) {
			return errClipboardImageUnavailable
		}
		if record.expired(s.now()) {
			if s.file.fs.Remove(id) != nil {
				return errClipboardImageUnavailable
			}
			delete(s.records, id)
			delete(s.digests, id)
			removed = true
		}
	}
	if removed && s.file.dirSync(dir) != nil {
		s.file.fault = errClipboardImageUnavailable
		return s.file.fault
	}
	return nil
}

func (s *clipboardImageStore) reap() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reapLocked()
}

func (s *clipboardImageStore) list() ([]clipboardImageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reapLocked(); err != nil {
		return nil, err
	}
	items := make([]clipboardImageRecord, 0, len(s.records))
	for _, record := range s.records {
		items = append(items, record)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
	return items, nil
}

func (s *clipboardImageStore) create(media string, body []byte, origin string, retention ...int) (clipboardImageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(body) > s.maxImageBytes {
		return clipboardImageRecord{}, errClipboardImageTooLarge
	}
	origin = strings.TrimSpace(origin)
	seconds := clipboardDefaultRetention
	if len(retention) > 1 {
		return clipboardImageRecord{}, errClipboardImageRetention
	}
	if len(retention) == 1 {
		seconds = retention[0]
	}
	if !validClipboardRetention(seconds) {
		return clipboardImageRecord{}, errClipboardImageRetention
	}
	if len(body) == 0 || !allowedImageMediaTypes[media] || imageMediaType(body) != media || !validImageDimensions(media, body) || !validClipboardImageOrigin(origin) {
		return clipboardImageRecord{}, errClipboardImageInvalid
	}
	if err := s.reapLocked(); err != nil {
		return clipboardImageRecord{}, err
	}
	digest := sha256.Sum256(body)
	for id, known := range s.digests {
		if known != digest {
			continue
		}
		existing, payload, err := s.readFile(id)
		if err != nil || existing != s.records[id] || !bytes.Equal(payload, body) {
			return clipboardImageRecord{}, errClipboardImageUnavailable
		}
		if existing.Revision == ^uint64(0) {
			return clipboardImageRecord{}, errClipboardImageUnavailable
		}
		existing.RetentionSeconds = longerClipboardRetention(existing.RetentionSeconds, seconds)
		existing.UpdatedAt = laterClipboardTime(s.now().UTC(), existing.UpdatedAt)
		existing.Revision++
		existing.Origin = origin
		var previous *time.Time
		if !existing.ExpiresAt.IsZero() {
			previous = &existing.ExpiresAt
		}
		expiry := clipboardExpiry(existing.UpdatedAt, existing.RetentionSeconds, previous)
		existing.ExpiresAt = time.Time{}
		if expiry != nil {
			existing.ExpiresAt = *expiry
		}
		if err := s.publishLocked(existing, body); err != nil {
			return clipboardImageRecord{}, err
		}
		return existing, nil
	}
	total := len(body)
	for _, record := range s.records {
		total += record.ByteSize
	}
	if len(s.records) >= clipboardImageMaxCount || total > clipboardImageMaxTotalBytes {
		return clipboardImageRecord{}, errClipboardImageFull
	}
	id, err := newSnippetID()
	if err != nil {
		return clipboardImageRecord{}, errClipboardImageUnavailable
	}
	// Even an unexpected collision or foreign entry must never be overwritten.
	if _, err := s.file.fs.Lstat(id); !errors.Is(err, os.ErrNotExist) {
		return clipboardImageRecord{}, errClipboardImageUnavailable
	}
	now := s.now().UTC()
	record := clipboardImageRecord{ID: id, MediaType: media, ByteSize: len(body), CreatedAt: now, UpdatedAt: now, Revision: 1, RetentionSeconds: seconds, Origin: origin}
	if expiry := clipboardExpiry(now, seconds, nil); expiry != nil {
		record.ExpiresAt = *expiry
	}
	if err := s.publishLocked(record, body); err != nil {
		return clipboardImageRecord{}, err
	}
	return record, nil
}

func (s *clipboardImageStore) publishLocked(record clipboardImageRecord, body []byte) error {
	header, err := json.Marshal(record)
	if err != nil || len(header) > clipboardImageMetadataMaxBytes {
		return errClipboardImageUnavailable
	}
	data := make([]byte, 8+len(header)+len(body))
	copy(data, "PCI2")
	binary.BigEndian.PutUint32(data[4:8], uint32(len(header)))
	copy(data[8:], header)
	copy(data[8+len(header):], body)
	file := *s.file
	file.base = record.ID
	if err := file.write(data); err != nil {
		if file.fault != nil {
			s.file.fault = errClipboardImageUnavailable
		}
		return errClipboardImageUnavailable
	}
	s.records[record.ID] = record
	s.digests[record.ID] = sha256.Sum256(body)
	return nil
}

func (s *clipboardImageStore) get(id string) (clipboardImageRecord, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !snippetIDRE.MatchString(id) {
		return clipboardImageRecord{}, nil, errClipboardImageNotFound
	}
	record, ok := s.records[id]
	if !ok || record.expired(s.now()) {
		return clipboardImageRecord{}, nil, errClipboardImageNotFound
	}
	if err := s.reapLocked(); err != nil {
		return clipboardImageRecord{}, nil, err
	}
	stored, body, err := s.readFile(id)
	if err != nil || stored != record {
		return clipboardImageRecord{}, nil, errClipboardImageUnavailable
	}
	return record, body, nil
}

func (s *clipboardImageStore) updateRetention(id string, seconds int, revision uint64) (clipboardImageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validClipboardRetention(seconds) || revision == 0 {
		return clipboardImageRecord{}, errClipboardImageRetention
	}
	if !snippetIDRE.MatchString(id) {
		return clipboardImageRecord{}, errClipboardImageNotFound
	}
	if err := s.reapLocked(); err != nil {
		return clipboardImageRecord{}, err
	}
	current, ok := s.records[id]
	if !ok {
		return clipboardImageRecord{}, errClipboardImageNotFound
	}
	if current.Revision != revision {
		return current, errClipboardImageConflict
	}
	stored, body, err := s.readFile(id)
	if err != nil || stored != current {
		return clipboardImageRecord{}, errClipboardImageUnavailable
	}
	if current.Revision == ^uint64(0) {
		return clipboardImageRecord{}, errClipboardImageUnavailable
	}
	current.RetentionSeconds = seconds
	current.UpdatedAt = laterClipboardTime(s.now().UTC(), current.UpdatedAt)
	current.Revision++
	// PATCH is a deliberate expiry choice, not a duplicate-upload renewal.
	expiry := clipboardExpiry(current.UpdatedAt, current.RetentionSeconds, nil)
	current.ExpiresAt = time.Time{}
	if expiry != nil {
		current.ExpiresAt = *expiry
	}
	if err := s.publishLocked(current, body); err != nil {
		return clipboardImageRecord{}, err
	}
	return current, nil
}

func (s *clipboardImageStore) delete(id string, revision ...uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !snippetIDRE.MatchString(id) {
		return errClipboardImageNotFound
	}
	record, ok := s.records[id]
	if !ok || record.expired(s.now()) {
		return errClipboardImageNotFound
	}
	if len(revision) > 1 || (len(revision) == 1 && revision[0] != record.Revision) {
		return errClipboardImageConflict
	}
	if err := s.reapLocked(); err != nil {
		return err
	}
	if _, _, err := s.readFile(id); err != nil {
		return err
	}
	if err := s.file.fs.Remove(id); err != nil {
		return errClipboardImageUnavailable
	}
	delete(s.records, id)
	delete(s.digests, id)
	dir, err := s.directory()
	if err != nil {
		s.file.fault = errClipboardImageUnavailable
		return s.file.fault
	}
	defer dir.Close()
	if s.file.dirSync(dir) != nil {
		s.file.fault = errClipboardImageUnavailable
		return s.file.fault
	}
	return nil
}
