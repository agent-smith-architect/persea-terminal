package frontdoor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Snippets / clips store: one global store in the alias
// store's shape. Bodies are terminal text and may carry secrets: the file is
// 0600 under the front's 0700 state directory, bodies are never logged, and
// the API never places them in a URL.
//
// Two record kinds share the file. A snippet is an operator-authored,
// labelled, optionally pinned command. A clip is a cross-device clipboard
// entry. Both support selectable retention; legacy snippets remain permanent.
// Short-lived clips share a ring of 20; extended and permanent entries are
// protected. Expired entries are reclaimed on load, persist, and maintenance.
//
// One global, server-owned distinguished clip — the OSC 52 record, id
// snippetOSCID — is the sink for automatic pane output.
// It is reached only through the idempotent upsert (upsertOSC, served
// as PUT /api/snippets/osc52), never through create. Its private publication
// state is omitted from lists; each value atomically renews an ordinary,
// editable canonical entry. The latest canonical entry is outside the ring;
// cross-device writes serialize in the store and the last committed write
// wins. Its origin is sanitized presentation metadata naming the latest
// writer and never chooses identity; the token "osc52" is refused as an
// origin everywhere so no label can masquerade as the record.

const snippetStoreVersion = 2
const snippetStoreMaxBytes = 1 << 20
const snippetStoreMaxSnippets = 256
const snippetClipRing = 20
const snippetClipTTL = 30 * time.Minute
const snippetBodyMaxBytes = 16 << 10
const snippetLabelMaxRunes = 64
const snippetOriginMaxRunes = 32
const snippetPreviewRunes = 80
const snippetKindSnippet = "snippet"
const snippetKindClip = "clip"

// snippetOSCID is the fixed id of the distinguished OSC 52 record and the
// reserved token that is never accepted as an origin.
const snippetOSCID = "osc52"

var snippetIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

func isOSCSnippetID(id string) bool { return id == snippetOSCID }

var errSnippetConflict = errors.New("snippet revision conflict")
var errSnippetNotFound = errors.New("snippet not found")
var errSnippetValidation = errors.New("invalid snippet")
var errSnippetTooLarge = errors.New("snippet body too large")
var errSnippetStoreFull = errors.New("snippet store full")
var errSnippetStoreUnavailable = errors.New("snippet store unavailable")

type SnippetRecord struct {
	ID               string     `json:"id"`
	Kind             string     `json:"kind"`
	Label            string     `json:"label"`
	Body             string     `json:"body"`
	Pinned           bool       `json:"pinned"`
	Origin           string     `json:"origin"`
	Revision         uint64     `json:"revision"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	ExpiresAt        *time.Time `json:"expires_at"`
	RetentionSeconds int        `json:"retention_seconds"`
}

// snippetView is the list shape: the record plus, for clips, an 80-rune
// preview derived from the body (§3b).
type snippetView struct {
	SnippetRecord
	Preview string `json:"preview,omitempty"`
}

type snippetStoreFile struct {
	Version int             `json:"version"`
	Items   []SnippetRecord `json:"items"`
}

type snippetStore struct {
	mu      sync.Mutex
	file    *durableFile
	now     func() time.Time
	records map[string]SnippetRecord
}

// snippetUpdate carries the optional PATCH fields; nil means "leave alone".
type snippetUpdate struct {
	Label            *string
	Body             *string
	Pinned           *bool
	RetentionSeconds *int
}

func newSnippetID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func validSnippetText(v string, maxRunes int) bool {
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > maxRunes {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validSnippetBody(body string) error {
	if len(body) > snippetBodyMaxBytes {
		return errSnippetTooLarge
	}
	if body == "" || !utf8.ValidString(body) {
		return errSnippetValidation
	}
	for _, r := range body {
		if r == utf8.RuneError || (unicode.IsControl(r) && r != '\n' && r != '\t') {
			return errSnippetValidation
		}
	}
	return nil
}

// validSnippetRecord is the closed per-record grammar, shared by the request
// path and the load path so a file can never hold what a request could not
// create.
func validSnippetRecord(r SnippetRecord) error {
	if (!snippetIDRE.MatchString(r.ID) && !isOSCSnippetID(r.ID)) || r.Revision == 0 || r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return errSnippetValidation
	}
	if err := validSnippetBody(r.Body); err != nil {
		return err
	}
	if !validSnippetText(r.Origin, snippetOriginMaxRunes) || r.Origin != strings.TrimSpace(r.Origin) || r.Origin == snippetOSCID {
		return errSnippetValidation
	}
	if isOSCSnippetID(r.ID) && r.Kind != snippetKindClip {
		return errSnippetValidation
	}
	if !validClipboardRetention(r.RetentionSeconds) || (r.RetentionSeconds == 0) != (r.ExpiresAt == nil) || (r.ExpiresAt != nil && r.ExpiresAt.Before(r.CreatedAt)) {
		return errSnippetValidation
	}
	switch r.Kind {
	case snippetKindSnippet:
		if r.Label == "" || r.Label != strings.TrimSpace(r.Label) || !validSnippetText(r.Label, snippetLabelMaxRunes) {
			return errSnippetValidation
		}
	case snippetKindClip:
		if r.Label != "" || r.Pinned {
			return errSnippetValidation
		}
	default:
		return errSnippetValidation
	}
	return nil
}

func snippetPreview(body string) string {
	flat := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, body)
	if utf8.RuneCountInString(flat) <= snippetPreviewRunes {
		return flat
	}
	runes := []rune(flat)
	return string(runes[:snippetPreviewRunes])
}

func newSnippetStore(path string) (*snippetStore, error) {
	return newSnippetStoreWithClock(path, time.Now)
}

func newSnippetStoreWithClock(path string, now func() time.Time) (*snippetStore, error) {
	file, err := openDurableFile(path, "snippet store", snippetStoreMaxBytes, errSnippetStoreUnavailable)
	if err != nil {
		return nil, err
	}
	s := &snippetStore{file: file, now: now, records: map[string]SnippetRecord{}}
	if err := s.load(); err != nil {
		_ = file.fs.(rootAliasFS).root.Close()
		return nil, err
	}
	return s, nil
}

func (s *snippetStore) expired(r SnippetRecord, now time.Time) bool {
	return r.ExpiresAt != nil && !now.Before(*r.ExpiresAt)
}

func (s *snippetStore) load() error {
	b, exists, err := s.file.read()
	if err != nil {
		return err
	}
	if !exists {
		return s.persistLocked()
	}
	var f snippetStoreFile
	if err := decodeStrict(b, &f); err != nil {
		return fmt.Errorf("invalid snippet store: %w", err)
	}
	if (f.Version != 1 && f.Version != snippetStoreVersion) || len(f.Items) > snippetStoreMaxSnippets+snippetClipRing+2 {
		return errors.New("unsupported or oversized snippet store")
	}
	snippets, clips, osc := 0, 0, 0
	for _, r := range f.Items {
		if f.Version == 1 {
			if (r.Kind == snippetKindSnippet && r.ExpiresAt != nil) || (r.Kind == snippetKindClip && (r.ExpiresAt == nil || r.Label != "")) {
				return errors.New("invalid legacy snippet record")
			}
			r.RetentionSeconds = legacyClipboardRetention(r.CreatedAt, r.ExpiresAt)
		}
		if err := validSnippetRecord(r); err != nil {
			return errors.New("invalid snippet record")
		}
		if _, dup := s.records[r.ID]; dup {
			return errors.New("duplicate snippet id")
		}
		if r.Kind == snippetKindSnippet {
			snippets++
		} else if isOSCSnippetID(r.ID) {
			osc++
		} else {
			clips++
		}
		s.records[r.ID] = r
	}
	clipLimit := snippetClipRing + 1
	if f.Version == 1 {
		clipLimit = snippetClipRing
	}
	if snippets > snippetStoreMaxSnippets || clips > clipLimit || osc > 1 {
		return errors.New("snippet store exceeds its bounds")
	}
	changed := f.Version != snippetStoreVersion
	now := s.now().UTC()
	for id, r := range s.records {
		if s.expired(r, now) {
			delete(s.records, id)
			changed = true
		}
	}
	// Version 1 exposed OSC's authority record. Preserve its content as a
	// normal editable record while retaining the publication CAS internally.
	if publication, ok := s.records[snippetOSCID]; f.Version == 1 && ok && s.duplicateLocked(publication.Body, "") == nil {
		id, err := newSnippetID()
		if err != nil {
			return err
		}
		canonical := publication
		canonical.ID = id
		s.records[id] = canonical
		changed = true
	}
	// Migration chooses a deterministic survivor and never shortens a policy.
	ids := make([]string, 0, len(s.records))
	for id := range s.records {
		if !isOSCSnippetID(id) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.records[ids[i]], s.records[ids[j]]
		if (a.Kind == snippetKindSnippet) != (b.Kind == snippetKindSnippet) {
			return a.Kind == snippetKindSnippet
		}
		return ids[i] < ids[j]
	})
	seen := map[string]string{}
	for _, id := range ids {
		r := s.records[id]
		if prior, ok := seen[r.Body]; ok {
			keep := s.records[prior]
			keep.RetentionSeconds = longerClipboardRetention(keep.RetentionSeconds, r.RetentionSeconds)
			if keep.RetentionSeconds == 0 {
				keep.ExpiresAt = nil
			} else if r.ExpiresAt != nil && r.ExpiresAt.After(*keep.ExpiresAt) {
				keep.ExpiresAt = r.ExpiresAt
			}
			if r.UpdatedAt.After(keep.UpdatedAt) {
				keep.UpdatedAt = r.UpdatedAt
			}
			if r.Revision >= keep.Revision {
				if r.Revision == ^uint64(0) {
					return errSnippetStoreUnavailable
				}
				keep.Revision = r.Revision + 1
			}
			s.records[prior] = keep
			delete(s.records, id)
			changed = true
		} else {
			seen[r.Body] = id
		}
	}
	if s.constrainPublicationLocked() {
		changed = true
	}
	if changed {
		return s.persistLocked()
	}
	return nil
}

// reap commits expiry even when no client reads or writes the clipboard.
func (s *snippetStore) reap() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return err
	}
	for _, r := range s.records {
		if s.expired(r, s.now().UTC()) {
			before := s.snapshotLocked()
			if err := s.persistLocked(); err != nil {
				if s.file.fault == nil {
					s.records = before
				}
				return err
			}
			break
		}
	}
	return nil
}

// constrainPublicationLocked keeps private publication content within the
// visible item's lifetime. It also repairs files saved before this invariant
// was enforced, without resurrecting deleted or edited content.
func (s *snippetStore) constrainPublicationLocked() bool {
	publication, ok := s.records[snippetOSCID]
	if !ok {
		return false
	}
	canonical := s.duplicateLocked(publication.Body, "")
	if canonical == nil {
		delete(s.records, snippetOSCID)
		return true
	}
	if canonical.ExpiresAt != nil && (publication.ExpiresAt == nil || canonical.ExpiresAt.Before(*publication.ExpiresAt)) {
		publication.ExpiresAt = canonical.ExpiresAt
		publication.RetentionSeconds = canonical.RetentionSeconds
		s.records[snippetOSCID] = publication
		return true
	}
	return false
}

// persistLocked drops expired clips, constrains private publication retention,
// then publishes. A file over the cap is reported as errSnippetStoreFull without
// touching the disk; the caller rolls the mutation back.
func (s *snippetStore) persistLocked() error {
	now := s.now().UTC()
	for id, r := range s.records {
		if s.expired(r, now) {
			delete(s.records, id)
		}
	}
	s.constrainPublicationLocked()
	list := make([]SnippetRecord, 0, len(s.records))
	for _, r := range s.records {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	b, err := json.Marshal(snippetStoreFile{Version: snippetStoreVersion, Items: list})
	if err != nil {
		return err
	}
	if err := s.file.write(b); err != nil {
		if errors.Is(err, errStoreOversized) {
			return errSnippetStoreFull
		}
		return err
	}
	return nil
}

func (s *snippetStore) availableLocked() error {
	return s.file.fault
}

// list returns live records: pinned first, then updated_at descending, id
// ascending as the tiebreak; expired clips are omitted without a write.
func (s *snippetStore) list() []snippetView {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	out := make([]snippetView, 0, len(s.records))
	for _, r := range s.records {
		if s.expired(r, now) || isOSCSnippetID(r.ID) {
			continue
		}
		v := snippetView{SnippetRecord: r}
		if r.Kind == snippetKindClip {
			v.Preview = snippetPreview(r.Body)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *snippetStore) liveLocked(id string, now time.Time) (SnippetRecord, bool) {
	r, ok := s.records[id]
	if !ok || s.expired(r, now) {
		return SnippetRecord{}, false
	}
	return r, true
}

func (s *snippetStore) snapshotLocked() map[string]SnippetRecord {
	before := make(map[string]SnippetRecord, len(s.records))
	for id, r := range s.records {
		before[id] = r
	}
	return before
}

// create adds a snippet or a manual clip. It never creates or touches the
// OSC 52 record: the reserved origin token is refused by the grammar and the
// record's fixed id is never minted here.
func (s *snippetStore) create(kind, label, body string, pinned bool, origin string, retention ...int) (SnippetRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return SnippetRecord{}, err
	}
	now := s.now().UTC()
	label, origin = strings.TrimSpace(label), strings.TrimSpace(origin)
	seconds := clipboardDefaultRetention
	if kind == snippetKindSnippet {
		seconds = 0
	}
	if len(retention) > 1 {
		return SnippetRecord{}, errSnippetValidation
	}
	if len(retention) == 1 {
		seconds = retention[0]
	}
	id, err := newSnippetID()
	if err != nil {
		return SnippetRecord{}, err
	}
	r := SnippetRecord{ID: id, Kind: kind, Label: label, Body: body, Pinned: pinned, Origin: origin, Revision: 1, CreatedAt: now, UpdatedAt: now, RetentionSeconds: seconds, ExpiresAt: clipboardExpiry(now, seconds, nil)}
	if err := validSnippetRecord(r); err != nil {
		return SnippetRecord{}, err
	}
	before := s.snapshotLocked()
	if duplicate := s.duplicateLocked(body, ""); duplicate != nil {
		r = *duplicate
		if r.Revision == ^uint64(0) {
			return SnippetRecord{}, errSnippetStoreUnavailable
		}
		r.RetentionSeconds = longerClipboardRetention(r.RetentionSeconds, seconds)
		r.UpdatedAt = laterClipboardTime(now, r.UpdatedAt)
		r.ExpiresAt = clipboardExpiry(r.UpdatedAt, r.RetentionSeconds, r.ExpiresAt)
		r.Revision++
		r.Origin = origin
	}
	s.records[r.ID] = r
	if err := s.enforceCapacityLocked(now, r.ID); err != nil {
		s.records = before
		return SnippetRecord{}, err
	}
	if err := s.persistLocked(); err != nil {
		if s.file.fault == nil {
			s.records = before
		}
		return SnippetRecord{}, err
	}
	return r, nil
}

// Exact bytes are the identity; OSC's separate publication state is never a
// user-editable candidate. Migration guarantees at most one visible match.
func (s *snippetStore) duplicateLocked(body, except string) *SnippetRecord {
	var found *SnippetRecord
	for id, record := range s.records {
		if id == except || isOSCSnippetID(id) || record.Body != body || s.expired(record, s.now().UTC()) {
			continue
		}
		if found == nil || record.ID < found.ID {
			copy := record
			found = &copy
		}
	}
	return found
}

func (s *snippetStore) enforceCapacityLocked(now time.Time, keep string) error {
	count := 0
	for _, record := range s.records {
		if !s.expired(record, now) && record.Kind == snippetKindSnippet {
			count++
		}
	}
	if count > snippetStoreMaxSnippets {
		return errSnippetStoreFull
	}
	publication, hasPublication := s.liveLocked(snippetOSCID, now)
	clips := []SnippetRecord{}
	for id, record := range s.records {
		if s.expired(record, now) {
			delete(s.records, id)
			continue
		}
		if record.Kind != snippetKindClip || isOSCSnippetID(id) || (hasPublication && record.Body == publication.Body) {
			continue
		}
		clips = append(clips, record)
	}
	sort.Slice(clips, func(i, j int) bool {
		if !clips[i].UpdatedAt.Equal(clips[j].UpdatedAt) {
			return clips[i].UpdatedAt.Before(clips[j].UpdatedAt)
		}
		return clips[i].ID < clips[j].ID
	})
	remaining := len(clips)
	for _, record := range clips {
		if remaining <= snippetClipRing {
			break
		}
		// Explicitly extended and permanent entries are never ring victims.
		if record.ID == keep || record.RetentionSeconds != clipboardDefaultRetention || record.ExpiresAt == nil || record.ExpiresAt.After(now.Add(snippetClipTTL)) {
			continue
		}
		delete(s.records, record.ID)
		remaining--
	}
	if remaining > snippetClipRing {
		return errSnippetStoreFull
	}
	return nil
}

// upsertOSC is the idempotent write to the distinguished OSC 52 record.
// A nil expect is unconditional (last committed write wins);
// a non-nil expect must equal the current revision (0 when the record is
// absent or expired) or errSnippetConflict returns the current record. The
// record keeps its fixed id and updates its origin and revision. Each write
// renews the canonical entry without shortening its retention. An absent
// authority record starts at revision 1.
func (s *snippetStore) upsertOSC(body, origin string, expect *uint64, retention ...int) (SnippetRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return SnippetRecord{}, err
	}
	now := s.now().UTC()
	origin = strings.TrimSpace(origin)
	current, exists := s.liveLocked(snippetOSCID, now)
	var currentRevision uint64
	if exists {
		currentRevision = current.Revision
	}
	if expect != nil && *expect != currentRevision {
		return current, errSnippetConflict
	}
	seconds := clipboardDefaultRetention
	if len(retention) > 1 {
		return SnippetRecord{}, errSnippetValidation
	}
	if len(retention) == 1 {
		seconds = retention[0]
	}
	if currentRevision == ^uint64(0) {
		return SnippetRecord{}, errSnippetStoreUnavailable
	}
	next := SnippetRecord{ID: snippetOSCID, Kind: snippetKindClip, Body: body, Origin: origin, Revision: currentRevision + 1, CreatedAt: now, UpdatedAt: now, RetentionSeconds: seconds, ExpiresAt: clipboardExpiry(now, seconds, nil)}
	if exists {
		next.CreatedAt = current.CreatedAt
		next.UpdatedAt = laterClipboardTime(now, current.UpdatedAt)
		if body == current.Body {
			next.RetentionSeconds = longerClipboardRetention(current.RetentionSeconds, seconds)
			next.ExpiresAt = clipboardExpiry(next.UpdatedAt, next.RetentionSeconds, current.ExpiresAt)
		}
	}
	if err := validSnippetRecord(next); err != nil {
		return SnippetRecord{}, err
	}
	before := s.snapshotLocked()
	canonical := s.duplicateLocked(body, "")
	if canonical == nil {
		id, err := newSnippetID()
		if err != nil {
			return SnippetRecord{}, err
		}
		copy := next
		copy.ID = id
		copy.Revision = 1
		copy.CreatedAt = now
		canonical = &copy
	} else {
		if canonical.Revision == ^uint64(0) {
			return SnippetRecord{}, errSnippetStoreUnavailable
		}
		canonical.RetentionSeconds = longerClipboardRetention(canonical.RetentionSeconds, seconds)
		canonical.UpdatedAt = laterClipboardTime(now, canonical.UpdatedAt)
		canonical.ExpiresAt = clipboardExpiry(canonical.UpdatedAt, canonical.RetentionSeconds, canonical.ExpiresAt)
		canonical.Revision++
		canonical.Origin = origin
	}
	s.records[snippetOSCID] = next
	s.records[canonical.ID] = *canonical
	if err := s.enforceCapacityLocked(now, canonical.ID); err != nil {
		s.records = before
		return SnippetRecord{}, err
	}
	if err := s.persistLocked(); err != nil {
		if s.file.fault == nil {
			s.records = before
		}
		return SnippetRecord{}, err
	}
	return next, nil
}

// evictClipsLocked keeps at most keep live manual clips, dropping the ones
// whose value is oldest (earliest expiry). The OSC record is never ranked.
func (s *snippetStore) evictClipsLocked(now time.Time, keep int) {
	clips := make([]SnippetRecord, 0, snippetClipRing+1)
	for id, r := range s.records {
		if r.Kind != snippetKindClip {
			continue
		}
		if s.expired(r, now) {
			delete(s.records, id)
			continue
		}
		if isOSCSnippetID(r.ID) {
			continue
		}
		clips = append(clips, r)
	}
	sort.Slice(clips, func(i, j int) bool {
		if !clips[i].ExpiresAt.Equal(*clips[j].ExpiresAt) {
			return clips[i].ExpiresAt.Before(*clips[j].ExpiresAt)
		}
		return clips[i].ID < clips[j].ID
	})
	for len(clips) > keep {
		delete(s.records, clips[0].ID)
		clips = clips[1:]
	}
}

// precondition returns the live record when its revision matches; on a
// mismatch it returns the current record with errSnippetConflict.
func (s *snippetStore) precondition(id string, revision uint64) (SnippetRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return SnippetRecord{}, err
	}
	r, ok := s.liveLocked(id, s.now().UTC())
	if !ok {
		return SnippetRecord{}, errSnippetNotFound
	}
	if r.Revision != revision {
		return r, errSnippetConflict
	}
	return r, nil
}

func (s *snippetStore) update(id string, change snippetUpdate, revision uint64) (SnippetRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return SnippetRecord{}, err
	}
	now := s.now().UTC()
	old, ok := s.liveLocked(id, now)
	if !ok {
		return SnippetRecord{}, errSnippetNotFound
	}
	if old.Revision != revision {
		return old, errSnippetConflict
	}
	if change.Label == nil && change.Body == nil && change.Pinned == nil && change.RetentionSeconds == nil {
		return SnippetRecord{}, errSnippetValidation
	}
	if old.Kind == snippetKindClip && (change.Label != nil || change.Pinned != nil) {
		return SnippetRecord{}, errSnippetValidation
	}
	if isOSCSnippetID(id) {
		return SnippetRecord{}, errSnippetValidation
	}
	next := old
	if change.Label != nil {
		next.Label = strings.TrimSpace(*change.Label)
	}
	if change.Body != nil {
		next.Body = *change.Body
	}
	if change.Pinned != nil {
		next.Pinned = *change.Pinned
	}
	if change.RetentionSeconds != nil {
		if !validClipboardRetention(*change.RetentionSeconds) {
			return SnippetRecord{}, errSnippetValidation
		}
		next.RetentionSeconds = *change.RetentionSeconds
	}
	if next.Revision == ^uint64(0) {
		return SnippetRecord{}, errSnippetStoreUnavailable
	}
	next.Revision++
	next.UpdatedAt = laterClipboardTime(now, old.UpdatedAt)
	if change.RetentionSeconds != nil {
		// An explicit choice replaces the deadline, including a shorter one.
		// Automatic saves and duplicate captures retain their renewal policy.
		next.ExpiresAt = clipboardExpiry(next.UpdatedAt, next.RetentionSeconds, nil)
	} else if change.Body != nil {
		next.ExpiresAt = clipboardExpiry(next.UpdatedAt, next.RetentionSeconds, next.ExpiresAt)
	}
	if err := validSnippetRecord(next); err != nil {
		return SnippetRecord{}, err
	}
	before := s.snapshotLocked()
	if change.Body != nil {
		if duplicate := s.duplicateLocked(next.Body, id); duplicate != nil {
			if duplicate.Revision == ^uint64(0) {
				return SnippetRecord{}, errSnippetStoreUnavailable
			}
			duplicate.RetentionSeconds = longerClipboardRetention(duplicate.RetentionSeconds, next.RetentionSeconds)
			previous := duplicate.ExpiresAt
			if previous == nil || (next.ExpiresAt != nil && next.ExpiresAt.After(*previous)) {
				previous = next.ExpiresAt
			}
			duplicate.UpdatedAt = laterClipboardTime(next.UpdatedAt, duplicate.UpdatedAt)
			duplicate.ExpiresAt = clipboardExpiry(duplicate.UpdatedAt, duplicate.RetentionSeconds, previous)
			duplicate.Revision++
			next = *duplicate
			delete(s.records, id)
		}
	}
	s.records[next.ID] = next
	if err := s.persistLocked(); err != nil {
		if s.file.fault == nil {
			s.records = before
		}
		return SnippetRecord{}, err
	}
	return next, nil
}

func (s *snippetStore) delete(id string, revision uint64) (SnippetRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return SnippetRecord{}, err
	}
	r, ok := s.liveLocked(id, s.now().UTC())
	if !ok {
		return SnippetRecord{}, errSnippetNotFound
	}
	if r.Revision != revision {
		return r, errSnippetConflict
	}
	before := s.snapshotLocked()
	delete(s.records, id)
	if isOSCSnippetID(id) {
		if canonical := s.duplicateLocked(r.Body, ""); canonical != nil {
			delete(s.records, canonical.ID)
		}
	}
	if err := s.persistLocked(); err != nil {
		if s.file.fault == nil {
			s.records = before
		}
		return SnippetRecord{}, err
	}
	return r, nil
}
