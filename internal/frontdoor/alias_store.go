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

const aliasStoreVersion = 2
const aliasStoreMaxBytes = 1 << 20
const aliasStoreMaxRecords = 4096

var errAliasConflict = errors.New("alias revision conflict")
var errAliasInUse = errors.New("alias in use")
var errAliasExists = errors.New("session already has an alias")
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
	Realm           string          `json:"realm"`
	Server          string          `json:"server"`
	SessionName     string          `json:"session_name"`
	Incarnation     proto.Authority `json:"session_incarnation"`
	Revision        uint64          `json:"revision"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	State           string          `json:"state"`
}

type aliasSession struct {
	Authority proto.Authority
	Name      string
}

// A seed has never been shown on a session, so its last witness is null.
// Keep the broker's Authority decoder strict: only this store record can
// represent an absent witness, and load still requires one for active aliases.
func (r AliasRecord) MarshalJSON() ([]byte, error) {
	type record AliasRecord
	var incarnation *proto.Authority
	if r.Incarnation != (proto.Authority{}) {
		incarnation = &r.Incarnation
	}
	return json.Marshal(struct {
		record
		Incarnation *proto.Authority `json:"session_incarnation"`
	}{record: record(r), Incarnation: incarnation})
}

func (r *AliasRecord) UnmarshalJSON(data []byte) error {
	type record AliasRecord
	wire := struct {
		*record
		Incarnation *proto.Authority `json:"session_incarnation"`
	}{record: (*record)(r)}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return err
	}
	r.Incarnation = proto.Authority{}
	if wire.Incarnation != nil {
		r.Incarnation = *wire.Incarnation
	}
	return nil
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
	if !utf8.ValidString(v) {
		return "", errAliasValidation
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", errAliasValidation
		}
	}
	v = strings.TrimSpace(v)
	if v == "" || utf8.RuneCountInString(v) > 128 {
		return "", errAliasValidation
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
	if len(f.Aliases) > aliasStoreMaxRecords {
		return errors.New("unsupported or oversized alias store")
	}
	if f.Version > 0 && f.Version < aliasStoreVersion {
		if err := s.persistLocked(); err != nil {
			return err
		}
		frontLogf("component=frontdoor event=alias_store_reset old_version=%d version=%d", f.Version, aliasStoreVersion)
		return nil
	}
	if f.Version != aliasStoreVersion {
		return errors.New("unsupported alias store version")
	}
	norms := map[string]bool{}
	ids := map[string]bool{}
	sessions := map[string]bool{}
	for _, r := range f.Aliases {
		n, e := normalizeAlias(r.DisplayAlias)
		if e != nil || n != r.NormalizedAlias || r.AliasID == "" || r.Revision == 0 || r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) ||
			r.Realm == "" || r.Server == "" || r.SessionName == "" || (r.State != "active" && r.State != "detached") || ids[r.AliasID] {
			return errors.New("invalid alias record")
		}
		if r.Incarnation != (proto.Authority{}) && (!r.Incarnation.Valid() || r.Incarnation.SessionCreated <= 0 || r.Incarnation.Realm != r.Realm || r.Incarnation.Server != r.Server) {
			return errors.New("invalid alias incarnation")
		}
		if r.State == "active" {
			key := authorityKey(r.Incarnation)
			if !r.Incarnation.Valid() || norms[n] || sessions[key] {
				return errors.New("duplicate or invalid active alias")
			}
			norms[n], sessions[key] = true, true
		}
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

func (s *aliasStore) snapshotLocked() map[string]AliasRecord {
	before := make(map[string]AliasRecord, len(s.records))
	for id, r := range s.records {
		before[id] = r
	}
	return before
}

func (s *aliasStore) commitLocked(before map[string]AliasRecord) error {
	if err := s.persistLocked(); err != nil {
		if s.fault == nil {
			s.records = before
		}
		return err
	}
	return nil
}

// Equal timestamps use the ID so inventory order and map iteration cannot
// change which detached alias returns or which history entry is evicted.
func aliasNewer(a, b AliasRecord) bool {
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	return a.AliasID < b.AliasID
}

func (s *aliasStore) makeRoomLocked() error {
	if len(s.records) < aliasStoreMaxRecords {
		return nil
	}
	var oldest AliasRecord
	for _, r := range s.records {
		if r.State == "detached" && (oldest.AliasID == "" || aliasNewer(oldest, r)) {
			oldest = r
		}
	}
	if oldest.AliasID == "" {
		return errAliasStoreUnavailable
	}
	delete(s.records, oldest.AliasID)
	return nil
}

func (s *aliasStore) supersedeLocked(normalized, except string) {
	for id, r := range s.records {
		if id != except && r.State == "detached" && r.NormalizedAlias == normalized {
			delete(s.records, id)
		}
	}
}

func (s *aliasStore) create(display string, target aliasSession) (AliasRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return AliasRecord{}, err
	}
	a := target.Authority
	if !a.Valid() || a.SessionCreated <= 0 || target.Name == "" {
		return AliasRecord{}, errAliasValidation
	}
	n, e := normalizeAlias(display)
	if e != nil {
		return AliasRecord{}, e
	}
	for _, r := range s.records {
		if r.State == "active" && sameAuthority(r.Incarnation, a) {
			return r, errAliasExists
		}
	}
	for _, r := range s.records {
		if r.State == "active" && r.NormalizedAlias == n {
			return AliasRecord{}, errAliasInUse
		}
	}
	id, e := newAliasID()
	if e != nil {
		return AliasRecord{}, e
	}
	now := s.now().UTC()
	r := AliasRecord{AliasID: id, DisplayAlias: strings.TrimSpace(display), NormalizedAlias: n, Realm: a.Realm, Server: a.Server, SessionName: target.Name, Incarnation: a, Revision: 1, CreatedAt: now, UpdatedAt: now, State: "active"}
	before := s.snapshotLocked()
	s.supersedeLocked(n, "")
	if e = s.makeRoomLocked(); e != nil {
		s.records = before
		return AliasRecord{}, e
	}
	s.records[id] = r
	if e = s.commitLocked(before); e != nil {
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

func (s *aliasStore) update(id, display string, revision uint64) (AliasRecord, error) {
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
	n, e := normalizeAlias(display)
	if e != nil {
		return AliasRecord{}, e
	}
	for oid, o := range s.records {
		if oid != id && o.State == "active" && o.NormalizedAlias == n {
			return AliasRecord{}, errAliasInUse
		}
	}
	before := s.snapshotLocked()
	r.DisplayAlias = strings.TrimSpace(display)
	r.NormalizedAlias = n
	r.Revision++
	r.UpdatedAt = s.now().UTC()
	s.supersedeLocked(n, id)
	s.records[id] = r
	if e = s.commitLocked(before); e != nil {
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

func (s *aliasStore) seed(display, realm, server, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return err
	}
	for _, r := range s.records {
		if r.Realm == realm && r.Server == server && r.SessionName == name {
			return nil
		}
	}
	n, err := normalizeAlias(display)
	if err != nil || realm == "" || server == "" || name == "" {
		return errAliasValidation
	}
	id, err := newAliasID()
	if err != nil {
		return err
	}
	before := s.snapshotLocked()
	if err = s.makeRoomLocked(); err != nil {
		return err
	}
	now := s.now().UTC()
	s.records[id] = AliasRecord{AliasID: id, DisplayAlias: strings.TrimSpace(display), NormalizedAlias: n, Realm: realm, Server: server, SessionName: name, Revision: 1, CreatedAt: now, UpdatedAt: now, State: "detached"}
	return s.commitLocked(before)
}

func (s *aliasStore) reconcile(live []aliasSession, complete map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return err
	}
	before := s.snapshotLocked()
	byIncarnation := map[string]aliasSession{}
	byName := map[string]aliasSession{}
	for _, target := range live {
		a := target.Authority
		if !a.Valid() || a.SessionCreated <= 0 || target.Name == "" || !complete[a.Realm+"\x00"+a.Server] {
			continue
		}
		byIncarnation[authorityKey(a)] = target
		byName[a.Realm+"\x00"+a.Server+"\x00"+target.Name] = target
	}
	ordered := make([]AliasRecord, 0, len(s.records))
	usedSessions, usedNames := map[string]bool{}, map[string]bool{}
	for _, r := range s.records {
		ordered = append(ordered, r)
		if !complete[r.Realm+"\x00"+r.Server] && r.State == "active" {
			usedSessions[authorityKey(r.Incarnation)], usedNames[r.NormalizedAlias] = true, true
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return aliasNewer(ordered[i], ordered[j]) })
	bound := map[string]aliasSession{}
	claim := func(r AliasRecord, target aliasSession) bool {
		key := authorityKey(target.Authority)
		if usedSessions[key] || usedNames[r.NormalizedAlias] {
			return false
		}
		bound[r.AliasID] = target
		usedSessions[key], usedNames[r.NormalizedAlias] = true, true
		return true
	}
	// Exact witnesses take precedence over names, including a tmux rename.
	for _, r := range ordered {
		if target, ok := byIncarnation[authorityKey(r.Incarnation)]; ok {
			claim(r, target)
		}
	}
	for _, r := range ordered {
		if _, ok := bound[r.AliasID]; ok || !complete[r.Realm+"\x00"+r.Server] {
			continue
		}
		if target, ok := byName[r.Realm+"\x00"+r.Server+"\x00"+r.SessionName]; ok {
			claim(r, target)
		}
	}
	changed := false
	now := s.now().UTC()
	for id, r := range s.records {
		if !complete[r.Realm+"\x00"+r.Server] {
			continue
		}
		old := r
		r.State = "detached"
		if target, ok := bound[id]; ok {
			r.State, r.Incarnation, r.SessionName = "active", target.Authority, target.Name
		}
		if r != old {
			r.Revision++
			r.UpdatedAt = now
			s.records[id] = r
			changed = true
		}
	}
	if changed {
		return s.commitLocked(before)
	}
	return nil
}
