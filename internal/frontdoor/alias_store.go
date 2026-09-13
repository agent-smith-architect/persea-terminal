package frontdoor

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"persea-terminal/internal/proto"
)

const aliasStoreVersion = 1
const aliasStoreMaxBytes = 1 << 20
const aliasStoreMaxRecords = 4096

var errAliasConflict = errors.New("alias revision conflict")
var errAliasNotFound = errors.New("alias not found")
var errAliasValidation = errors.New("invalid alias")
var errAliasStoreUnavailable = errors.New("alias store unavailable")

type aliasFS interface {
	Lstat(string) (os.FileInfo, error)
	Open(string) (*os.File, error)
	OpenFile(string, int, os.FileMode) (*os.File, error)
	Remove(string) error
	Rename(string, string) error
}

type rootAliasFS struct{ root *os.Root }

func (f rootAliasFS) Lstat(name string) (os.FileInfo, error) { return f.root.Lstat(name) }
func (f rootAliasFS) Open(name string) (*os.File, error)     { return f.root.Open(name) }
func (f rootAliasFS) OpenFile(name string, flag int, mode os.FileMode) (*os.File, error) {
	return f.root.OpenFile(name, flag, mode)
}
func (f rootAliasFS) Remove(name string) error             { return f.root.Remove(name) }
func (f rootAliasFS) Rename(oldname, newname string) error { return f.root.Rename(oldname, newname) }

type AliasRecord struct {
	AliasID         string          `json:"alias_id"`
	DisplayAlias    string          `json:"display_alias"`
	NormalizedAlias string          `json:"normalized_alias"`
	Incarnation     proto.Authority `json:"session_incarnation"`
	Revision        uint64          `json:"revision"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	State           string          `json:"state"`
}

type aliasStoreFile struct {
	Version int           `json:"version"`
	Aliases []AliasRecord `json:"aliases"`
}

type aliasStore struct {
	mu        sync.Mutex
	path      string
	now       func() time.Time
	records   map[string]AliasRecord
	fs        aliasFS
	base      string
	fault     error
	writeFile func(*os.File, []byte) (int, error)
	fileSync  func(*os.File) error
	closeFile func(*os.File) error
	dirSync   func(*os.File) error
}

func foldKey(v string) string {
	var b strings.Builder
	for _, r := range v {
		min := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < min {
				min = next
			}
		}
		b.WriteRune(min)
	}
	return b.String()
}

func normalizeAlias(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 128 {
		return "", errAliasValidation
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", errAliasValidation
		}
	}
	return foldKey(v), nil
}

func newAliasID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func newAliasStore(path string) (*aliasStore, error) {
	s := &aliasStore{path: path, now: time.Now, records: map[string]AliasRecord{}}
	if path == "" {
		return s, nil
	} // Direct unit-test seam; loaded runtime config requires a path.
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("alias store path must be clean and absolute")
	}
	parent := filepath.Dir(path)
	if err := validateAliasParent(parent); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, fmt.Errorf("%w: open alias store parent: %v", errAliasStoreUnavailable, err)
	}
	dir, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("%w: pin alias store parent: %v", errAliasStoreUnavailable, err)
	}
	pinned, statErr := dir.Stat()
	_ = dir.Close()
	if statErr != nil || !pinned.IsDir() || pinned.Mode().Perm() != 0700 {
		_ = root.Close()
		return nil, fmt.Errorf("%w: pinned alias store parent failed validation", errAliasStoreUnavailable)
	}
	pinnedStat, ok := pinned.Sys().(*syscall.Stat_t)
	if !ok || pinnedStat.Uid != uint32(os.Getuid()) {
		_ = root.Close()
		return nil, fmt.Errorf("%w: pinned alias store parent has wrong owner", errAliasStoreUnavailable)
	}
	s.fs, s.base = rootAliasFS{root: root}, filepath.Base(path)
	s.writeFile = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	s.fileSync = func(f *os.File) error { return f.Sync() }
	s.closeFile = func(f *os.File) error { return f.Close() }
	s.dirSync = func(f *os.File) error { return f.Sync() }
	if err := s.load(); err != nil {
		_ = root.Close()
		return nil, err
	}
	return s, nil
}

func validateAliasParent(parent string) error {
	current := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(parent, string(filepath.Separator)), string(filepath.Separator))
	for _, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("%w: inspect alias store parent: %v", errAliasStoreUnavailable, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: alias store parent component is not a real directory", errAliasStoreUnavailable)
		}
	}
	info, err := os.Lstat(parent)
	if err != nil || info.Mode().Perm() != 0700 {
		return fmt.Errorf("%w: alias store parent must have mode 0700", errAliasStoreUnavailable)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%w: alias store parent has wrong owner", errAliasStoreUnavailable)
	}
	return nil
}

func rejectAliasDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := []string{}
			for dec.More() {
				k, e := dec.Token()
				if e != nil {
					return e
				}
				key := k.(string)
				for _, prior := range seen {
					if strings.EqualFold(prior, key) {
						return errors.New("duplicate object key")
					}
				}
				seen = append(seen, key)
				if e = walk(); e != nil {
					return e
				}
			}
		case '[':
			for dec.More() {
				if e := walk(); e != nil {
					return e
				}
			}
		}
		_, err = dec.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

func (s *aliasStore) load() error {
	info, err := s.fs.Lstat(s.base)
	if errors.Is(err, os.ErrNotExist) {
		return s.persistLocked()
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errAliasStoreUnavailable, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0600 {
		return errors.New("alias store must be a regular owner-only file")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) {
		return errors.New("alias store has wrong owner")
	}
	if info.Size() > aliasStoreMaxBytes {
		return errors.New("alias store is oversized")
	}
	fh, err := s.fs.OpenFile(s.base, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("%w: %v", errAliasStoreUnavailable, err)
	}
	defer fh.Close()
	opened, err := fh.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0600 || opened.Size() > aliasStoreMaxBytes {
		return fmt.Errorf("%w: opened alias store failed validation", errAliasStoreUnavailable)
	}
	openedStat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || openedStat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%w: opened alias store has wrong owner", errAliasStoreUnavailable)
	}
	b, err := io.ReadAll(io.LimitReader(fh, aliasStoreMaxBytes+1))
	if err != nil || len(b) > aliasStoreMaxBytes {
		return fmt.Errorf("%w: bounded alias store read failed", errAliasStoreUnavailable)
	}
	if !utf8.Valid(b) {
		return errors.New("invalid alias store: invalid UTF-8")
	}
	if err := rejectAliasDuplicateKeys(b); err != nil {
		return fmt.Errorf("invalid alias store: %w", err)
	}
	var f aliasStoreFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("invalid alias store: %w", err)
	}
	if f.Version != aliasStoreVersion || len(f.Aliases) > aliasStoreMaxRecords {
		return errors.New("unsupported or oversized alias store")
	}
	norms := map[string]bool{}
	ids := map[string]bool{}
	for _, r := range f.Aliases {
		n, e := normalizeAlias(r.DisplayAlias)
		if e != nil || n != r.NormalizedAlias || r.AliasID == "" || r.Revision == 0 || r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) ||
			!r.Incarnation.Valid() || (r.State != "active" && r.State != "tombstone" && r.State != "rebound") || norms[n] || ids[r.AliasID] {
			return errors.New("invalid alias record")
		}
		if r.Incarnation.SessionCreated <= 0 {
			return errors.New("invalid alias incarnation")
		}
		norms[n] = true
		ids[r.AliasID] = true
		s.records[r.AliasID] = r
	}
	return nil
}

func (s *aliasStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	if s.fault != nil {
		return s.fault
	}
	list := make([]AliasRecord, 0, len(s.records))
	for _, r := range s.records {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].AliasID < list[j].AliasID })
	b, err := json.Marshal(aliasStoreFile{Version: aliasStoreVersion, Aliases: list})
	if err != nil {
		return err
	}
	if len(b) > aliasStoreMaxBytes {
		return errors.New("alias store is oversized")
	}
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := "." + s.base + "." + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := s.fs.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = s.fs.Remove(tmp)
		}
	}()
	n, err := s.writeFile(f, b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return err
	}
	if err = s.fileSync(f); err != nil {
		return err
	}
	if err = s.closeFile(f); err != nil {
		return err
	}
	if err = s.fs.Rename(tmp, s.base); err != nil {
		return err
	}
	ok = true
	d, err := s.fs.Open(".")
	if err != nil {
		s.fault = fmt.Errorf("%w: published alias store directory open failed: %v", errAliasStoreUnavailable, err)
		return s.fault
	}
	defer d.Close()
	if err = s.dirSync(d); err != nil {
		s.fault = fmt.Errorf("%w: published alias store directory sync failed: %v", errAliasStoreUnavailable, err)
		return s.fault
	}
	return nil
}

func (s *aliasStore) availableLocked() error {
	if s.fault != nil {
		return s.fault
	}
	return nil
}

func (s *aliasStore) list() []AliasRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AliasRecord, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AliasID < out[j].AliasID })
	return out
}

func (s *aliasStore) create(display string, a proto.Authority) (AliasRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return AliasRecord{}, err
	}
	if !a.Valid() {
		return AliasRecord{}, errAliasValidation
	}
	if len(s.records) >= aliasStoreMaxRecords {
		return AliasRecord{}, errors.New("alias capacity reached")
	}
	n, e := normalizeAlias(display)
	if e != nil {
		return AliasRecord{}, e
	}
	for _, r := range s.records {
		if r.NormalizedAlias == n {
			return AliasRecord{}, errAliasConflict
		}
	}
	id, e := newAliasID()
	if e != nil {
		return AliasRecord{}, e
	}
	now := s.now().UTC()
	r := AliasRecord{AliasID: id, DisplayAlias: strings.TrimSpace(display), NormalizedAlias: n, Incarnation: a, Revision: 1, CreatedAt: now, UpdatedAt: now, State: "active"}
	s.records[id] = r
	if e = s.persistLocked(); e != nil {
		if s.fault == nil {
			delete(s.records, id)
		}
		return AliasRecord{}, e
	}
	return r, nil
}

func (s *aliasStore) precondition(id string, revision uint64) (AliasRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return AliasRecord{}, err
	}
	r, ok := s.records[id]
	if !ok {
		return AliasRecord{}, errAliasNotFound
	}
	if r.Revision != revision {
		return r, errAliasConflict
	}
	return r, nil
}

func (s *aliasStore) update(id, display string, a *proto.Authority, revision uint64) (AliasRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return AliasRecord{}, err
	}
	if a != nil && !a.Valid() {
		return AliasRecord{}, errAliasValidation
	}
	r, ok := s.records[id]
	if !ok {
		return AliasRecord{}, errAliasNotFound
	}
	if r.Revision != revision {
		return r, errAliasConflict
	}
	n, e := normalizeAlias(display)
	if e != nil {
		return AliasRecord{}, e
	}
	for oid, o := range s.records {
		if oid != id && o.NormalizedAlias == n {
			return AliasRecord{}, errAliasConflict
		}
	}
	old := r
	r.DisplayAlias = strings.TrimSpace(display)
	r.NormalizedAlias = n
	if a != nil {
		r.Incarnation = *a
		r.State = "rebound"
	}
	r.Revision++
	r.UpdatedAt = s.now().UTC()
	s.records[id] = r
	if e = s.persistLocked(); e != nil {
		if s.fault == nil {
			s.records[id] = old
		}
		return AliasRecord{}, e
	}
	return r, nil
}

func (s *aliasStore) delete(id string, revision uint64) (AliasRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return AliasRecord{}, err
	}
	r, ok := s.records[id]
	if !ok {
		return AliasRecord{}, errAliasNotFound
	}
	if r.Revision != revision {
		return r, errAliasConflict
	}
	delete(s.records, id)
	if e := s.persistLocked(); e != nil {
		if s.fault == nil {
			s.records[id] = r
		}
		return AliasRecord{}, e
	}
	return r, nil
}

func sameAuthority(a, b proto.Authority) bool { return authorityKey(a) == authorityKey(b) }

func (s *aliasStore) reconcile(live []proto.Authority, complete map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return err
	}
	before := make(map[string]AliasRecord, len(s.records))
	for id, record := range s.records {
		before[id] = record
	}
	changed := false
	now := s.now().UTC()
	for id, r := range s.records {
		found := false
		for _, a := range live {
			if sameAuthority(a, r.Incarnation) {
				found = true
				break
			}
		}
		next := r.State
		if !found && complete[r.Incarnation.Realm+"\x00"+r.Incarnation.Server] {
			next = "tombstone"
		}
		if next != r.State {
			r.State = next
			r.Revision++
			r.UpdatedAt = now
			s.records[id] = r
			changed = true
		}
	}
	if changed {
		if err := s.persistLocked(); err != nil {
			if s.fault == nil {
				s.records = before
			}
			return err
		}
	}
	return nil
}
