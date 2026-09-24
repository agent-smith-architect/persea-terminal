package frontdoor

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Operator preferences: one closed record per ingress
// operator login, persisted with the alias store's durability contract.

const preferencesStoreVersion = 1
const preferencesStoreMaxBytes = 1 << 20
const preferencesStoreMaxRecords = 256
const preferenceFontSizeMin = 9
const preferenceFontSizeMax = 24

// The composer's face, in px. It shares the terminal font's range because the
// same operator eyes read both, and its default is one step below the face the
// composer used to hardcode. Unlike FontSize it is NOT a tri-state: the
// composer has no auto-fit to hand the decision back to, so every record holds
// a number and a record written before this field existed reads as this
// default.
const preferenceDefaultComposerFontSize = 11
const preferenceDefaultTheme = "default"
const preferenceOperatorMaxBytes = 254
const preferenceSessionNameMaxBytes = 128

// preferenceThemes is the curated palette list; the browser ships exactly
// these ids, so an unknown id is a validation failure, never a fallback.
var preferenceThemes = map[string]bool{
	"default":          true,
	"rose-pine":        true,
	"rose-pine-dawn":   true,
	"solarized-dark":   true,
	"gruvbox-dark":     true,
	"one-dark":         true,
	"dracula":          true,
	"catppuccin-mocha": true,
}

var errPreferencesConflict = errors.New("preferences revision conflict")
var errPreferencesValidation = errors.New("invalid preferences")
var errPreferencesStoreUnavailable = errors.New("preferences store unavailable")

// DefaultSession names the session card a landing shows first (§7); the
// front validates its shape only, existence is a landing-time question.
type DefaultSession struct {
	Realm  string `json:"realm"`
	Server string `json:"server"`
	Name   string `json:"name"`
}

// Preferences is the closed operator-visible schema (version 1).
//
// FontSize is a tri-state: a nil pointer is "auto" — the page
// fits the font to the viewport — and a non-nil pointer is the operator's
// explicit size in the 9…24 range, which overrides the fit on every load and
// reattach. The distinction is carried by the pointer, never by a sentinel:
// every value in the range is a legitimate explicit choice, so no number is
// free to mean "auto", and 0/absent is not a value the schema admits.
type Preferences struct {
	Version          int             `json:"version"`
	Theme            string          `json:"theme"`
	FontSize         *int            `json:"font_size"`
	ComposerFontSize int             `json:"composer_font_size"`
	DefaultSession   *DefaultSession `json:"default_session"`
}

// PreferenceRecord is what GET/PUT return: the record (or the defaults when
// nothing is stored, revision 0) plus whether it came from the store.
type PreferenceRecord struct {
	Preferences
	Revision uint64 `json:"revision"`
	Stored   bool   `json:"stored"`
}

// preferenceEntry is the persisted shape. FontSize keeps the wire's tri-state:
// an older record holds a JSON number and decodes into a non-nil
// pointer — an explicit size, exactly what its operator chose — while "auto" is
// written and read as JSON null. The file's shape is unchanged for every record
// that already exists, so there is no migration.
type preferenceEntry struct {
	Operator string `json:"operator"`
	Theme    string `json:"theme"`
	FontSize *int   `json:"font_size"`
	// A pointer for ONE reason: to tell "this record predates the field" from
	// "this operator chose a number". Absent reads as the default; every
	// record this release writes carries a number, so the pointer is nil only
	// for records written before the field existed. It is omitted when nil so
	// a store this release rewrites is byte-identical in shape to what it was
	// for records it did not touch.
	ComposerFontSize *int            `json:"composer_font_size,omitempty"`
	DefaultSession   *DefaultSession `json:"default_session"`
	Revision         uint64          `json:"revision"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type preferencesStoreFile struct {
	Version   int               `json:"version"`
	Operators []preferenceEntry `json:"operators"`
}

type preferencesStore struct {
	mu      sync.Mutex
	file    *durableFile
	now     func() time.Time
	records map[string]preferenceEntry
}

// defaultPreferences is what an operator with nothing stored reads: the default
// theme and "auto" font. A default font *number* would be indistinguishable
// from the same number chosen deliberately. The nullable field preserves that
// distinction.
func defaultPreferences() PreferenceRecord {
	return PreferenceRecord{Preferences: Preferences{Version: preferencesStoreVersion, Theme: preferenceDefaultTheme, ComposerFontSize: preferenceDefaultComposerFontSize}}
}

// composerFontSizeOf resolves the stored pointer: a record written before the
// field existed carries no key and reads as the default.
func composerFontSizeOf(v *int) int {
	if v == nil {
		return preferenceDefaultComposerFontSize
	}
	return *v
}

func validOperatorLogin(v string) bool {
	if v == "" || len(v) > preferenceOperatorMaxBytes || !utf8.ValidString(v) {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func validDefaultSession(d *DefaultSession) bool {
	if d == nil {
		return true
	}
	if !storeLabelRE.MatchString(d.Realm) || !storeLabelRE.MatchString(d.Server) {
		return false
	}
	if d.Name == "" || len(d.Name) > preferenceSessionNameMaxBytes || !utf8.ValidString(d.Name) {
		return false
	}
	for _, r := range d.Name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validFontSize(v *int) bool {
	return v == nil || (*v >= preferenceFontSizeMin && *v <= preferenceFontSizeMax)
}

func validComposerFontSize(v int) bool {
	return v >= preferenceFontSizeMin && v <= preferenceFontSizeMax
}

func validatePreferences(p Preferences) error {
	if p.Version != preferencesStoreVersion || !preferenceThemes[p.Theme] || !validFontSize(p.FontSize) ||
		!validComposerFontSize(p.ComposerFontSize) || !validDefaultSession(p.DefaultSession) {
		return errPreferencesValidation
	}
	return nil
}

func newPreferencesStore(path string) (*preferencesStore, error) {
	file, err := openDurableFile(path, "preferences store", preferencesStoreMaxBytes, errPreferencesStoreUnavailable)
	if err != nil {
		return nil, err
	}
	s := &preferencesStore{file: file, now: time.Now, records: map[string]preferenceEntry{}}
	if err := s.load(); err != nil {
		_ = file.fs.(rootAliasFS).root.Close()
		return nil, err
	}
	return s, nil
}

func (s *preferencesStore) load() error {
	b, exists, err := s.file.read()
	if err != nil {
		return err
	}
	if !exists {
		return s.persistLocked()
	}
	var f preferencesStoreFile
	if err := decodeStrict(b, &f); err != nil {
		return fmt.Errorf("invalid preferences store: %w", err)
	}
	if f.Version != preferencesStoreVersion || len(f.Operators) > preferencesStoreMaxRecords {
		return errors.New("unsupported or oversized preferences store")
	}
	for _, e := range f.Operators {
		if !validOperatorLogin(e.Operator) || e.Revision == 0 || e.CreatedAt.IsZero() || e.UpdatedAt.Before(e.CreatedAt) {
			return errors.New("invalid preferences record")
		}
		if _, dup := s.records[e.Operator]; dup {
			return errors.New("duplicate preferences operator")
		}
		if err := validatePreferences(Preferences{Version: preferencesStoreVersion, Theme: e.Theme, FontSize: e.FontSize, ComposerFontSize: composerFontSizeOf(e.ComposerFontSize), DefaultSession: e.DefaultSession}); err != nil {
			return errors.New("invalid preferences record")
		}
		s.records[e.Operator] = e
	}
	return nil
}

func (s *preferencesStore) persistLocked() error {
	list := make([]preferenceEntry, 0, len(s.records))
	for _, e := range s.records {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Operator < list[j].Operator })
	b, err := json.Marshal(preferencesStoreFile{Version: preferencesStoreVersion, Operators: list})
	if err != nil {
		return err
	}
	if err := s.file.write(b); err != nil {
		if errors.Is(err, errStoreOversized) {
			return fmt.Errorf("%w: %v", errPreferencesStoreUnavailable, err)
		}
		return err
	}
	return nil
}

func (s *preferencesStore) availableLocked() error {
	return s.file.fault
}

func recordOf(e preferenceEntry) PreferenceRecord {
	return PreferenceRecord{Preferences: Preferences{
		Version:          preferencesStoreVersion,
		Theme:            e.Theme,
		FontSize:         copyFontSize(e.FontSize),
		ComposerFontSize: composerFontSizeOf(e.ComposerFontSize),
		DefaultSession:   copySession(e.DefaultSession),
	}, Revision: e.Revision, Stored: true}
}

// get returns the operator's record, or the defaults with Stored=false and
// revision 0 when nothing is stored.
func (s *preferencesStore) get(operator string) PreferenceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.records[operator]; ok {
		return recordOf(e)
	}
	return defaultPreferences()
}

// put replaces the operator's record when revision matches the stored one (0
// when nothing is stored). On errPreferencesConflict the returned record is
// the current one so the caller can re-read and re-apply.
func (s *preferencesStore) put(operator string, p Preferences, revision uint64) (PreferenceRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return PreferenceRecord{}, err
	}
	if !validOperatorLogin(operator) {
		return PreferenceRecord{}, errPreferencesValidation
	}
	if err := validatePreferences(p); err != nil {
		return PreferenceRecord{}, err
	}
	now := s.now().UTC()
	old, exists := s.records[operator]
	current := defaultPreferences()
	if exists {
		current = recordOf(old)
	}
	if current.Revision != revision {
		return current, errPreferencesConflict
	}
	if !exists && len(s.records) >= preferencesStoreMaxRecords {
		return PreferenceRecord{}, fmt.Errorf("%w: capacity reached", errPreferencesStoreUnavailable)
	}
	composerFont := p.ComposerFontSize
	next := preferenceEntry{Operator: operator, Theme: p.Theme, FontSize: copyFontSize(p.FontSize), ComposerFontSize: &composerFont, DefaultSession: copySession(p.DefaultSession), Revision: revision + 1, CreatedAt: now, UpdatedAt: now}
	if exists {
		next.CreatedAt = old.CreatedAt
	}
	s.records[operator] = next
	if err := s.persistLocked(); err != nil {
		if s.file.fault == nil {
			if exists {
				s.records[operator] = old
			} else {
				delete(s.records, operator)
			}
		}
		return PreferenceRecord{}, err
	}
	return recordOf(next), nil
}

func copyFontSize(v *int) *int {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func copySession(d *DefaultSession) *DefaultSession {
	if d == nil {
		return nil
	}
	c := *d
	return &c
}
