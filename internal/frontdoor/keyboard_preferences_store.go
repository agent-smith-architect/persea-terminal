package frontdoor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const keyboardPreferencesStoreMaxBytes = 1 << 20
const keyboardPreferencesStoreMaxRecords = 256
const keyboardPreferencesMaxRevision = 1<<53 - 1 // Browser revisions remain exact integers.

var errKeyboardPreferencesConflict = errors.New("keyboard preferences revision conflict")
var errKeyboardPreferencesValidation = errors.New("invalid keyboard preferences")
var errKeyboardPreferencesStoreUnavailable = errors.New("keyboard preferences store unavailable")

type KeyboardLayout struct {
	Bar       []string `json:"bar"`
	Favorites []string `json:"favorites"`
}

// KeyboardPreferences has its own versioned file: older releases must continue
// reading their unchanged appearance preference records after rollback.
type KeyboardPreferences struct {
	Version  int               `json:"version"`
	Layout   KeyboardLayout    `json:"layout"`
	Prefixes map[string]string `json:"prefixes"`
}

type KeyboardPreferenceRecord struct {
	KeyboardPreferences
	Revision uint64 `json:"revision"`
	Stored   bool   `json:"stored"`
}

type keyboardPreferenceEntry struct {
	KeyboardPreferences
	Operator  string    `json:"operator"`
	Revision  uint64    `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type keyboardPreferencesStoreFile struct {
	Version   int                       `json:"version"`
	Operators []keyboardPreferenceEntry `json:"operators"`
}

type keyboardPreferencesStore struct {
	mu      sync.Mutex
	file    *durableFile
	now     func() time.Time
	records map[string]keyboardPreferenceEntry
}

func defaultKeyboardPreferences() KeyboardPreferenceRecord {
	return KeyboardPreferenceRecord{KeyboardPreferences: KeyboardPreferences{
		Version: 1,
		Layout: KeyboardLayout{
			Bar:       []string{"key:escape:0", "key:tab:0", "modifier:ctrl", "key:arrow-left:0", "key:arrow-down:0", "key:arrow-up:0", "key:arrow-right:0"},
			Favorites: []string{"modifier:alt", "key:c:1", "key:d:1", "key:enter:0", "key:home:0", "key:end:0", "key:page-up:0", "key:page-down:0", "key:f1:0"},
		},
		Prefixes: map[string]string{"tmux": "key:b:1", "screen": "key:a:1"},
	}}
}

var keyboardPrefixNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 _-]{0,31}$`)
var keyboardFunctionRE = regexp.MustCompile(`^f([1-9]|1[0-2])$`)

func validKeyboardPrefixName(name string) bool {
	return keyboardPrefixNameRE.MatchString(name) && name != "constructor" && name != "prototype" && name != "__proto__"
}

// encodeKeyboardComponent matches encodeURIComponent, including its unescaped
// punctuation and uppercase escapes. QueryEscape has different space rules.
func encodeKeyboardComponent(value string) string {
	const hex = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("-_.!~*'()", rune(b)) {
			out.WriteByte(b)
		} else {
			out.WriteByte('%')
			out.WriteByte(hex[b>>4])
			out.WriteByte(hex[b&15])
		}
	}
	return out.String()
}

func validKeyboardKeyID(id string) bool {
	parts := strings.Split(id, ":")
	if len(parts) != 3 || parts[0] != "key" || len(parts[2]) != 1 || parts[2][0] < '0' || parts[2][0] > '7' {
		return false
	}
	key, err := url.PathUnescape(parts[1])
	if err != nil || encodeKeyboardComponent(key) != parts[1] {
		return false
	}
	if len(key) == 1 && key[0] >= 32 && key[0] <= 126 {
		return true
	}
	if keyboardFunctionRE.MatchString(key) {
		return true
	}
	switch key {
	case "escape", "tab", "enter", "backspace", "insert", "delete", "arrow-left", "arrow-down", "arrow-up", "arrow-right", "home", "end", "page-up", "page-down":
		return true
	}
	return false
}

func validKeyboardActionID(id string, bar bool) bool {
	if bar && (id == "modifier:ctrl" || id == "modifier:alt" || id == "modifier:shift") {
		return true
	}
	if validKeyboardKeyID(id) {
		return true
	}
	parts := strings.SplitN(id, ":", 3)
	if len(parts) != 3 || parts[0] != "sequence" {
		return false
	}
	name, err := url.PathUnescape(parts[1])
	// A missing configured prefix is a UI availability condition, not corruption.
	return err == nil && validKeyboardPrefixName(name) && encodeKeyboardComponent(name) == parts[1] && validKeyboardKeyID(parts[2])
}

func validateKeyboardPreferences(p KeyboardPreferences) error {
	if p.Version != 1 || p.Layout.Bar == nil || p.Layout.Favorites == nil || p.Prefixes == nil || len(p.Prefixes) > 20 {
		return errKeyboardPreferencesValidation
	}
	for _, list := range [][]string{p.Layout.Bar, p.Layout.Favorites} {
		if len(list) > 100 {
			return errKeyboardPreferencesValidation
		}
		seen := map[string]bool{}
		for _, id := range list {
			if seen[id] || !validKeyboardActionID(id, true) {
				return errKeyboardPreferencesValidation
			}
			seen[id] = true
		}
	}
	names := []string{}
	for name, id := range p.Prefixes {
		if !validKeyboardPrefixName(name) || !validKeyboardKeyID(id) {
			return errKeyboardPreferencesValidation
		}
		for _, prior := range names {
			if strings.EqualFold(prior, name) {
				return errKeyboardPreferencesValidation
			}
		}
		names = append(names, name)
	}
	return nil
}

func copyKeyboardPreferences(p KeyboardPreferences) KeyboardPreferences {
	p.Layout.Bar = append([]string{}, p.Layout.Bar...)
	p.Layout.Favorites = append([]string{}, p.Layout.Favorites...)
	prefixes := make(map[string]string, len(p.Prefixes))
	for name, id := range p.Prefixes {
		prefixes[name] = id
	}
	p.Prefixes = prefixes
	return p
}

func keyboardRecordOf(e keyboardPreferenceEntry) KeyboardPreferenceRecord {
	return KeyboardPreferenceRecord{KeyboardPreferences: copyKeyboardPreferences(e.KeyboardPreferences), Revision: e.Revision, Stored: true}
}

func newKeyboardPreferencesStore(path string) (*keyboardPreferencesStore, error) {
	file, err := openDurableFile(path, "keyboard preferences store", keyboardPreferencesStoreMaxBytes, errKeyboardPreferencesStoreUnavailable)
	if err != nil {
		return nil, err
	}
	s := &keyboardPreferencesStore{file: file, now: time.Now, records: map[string]keyboardPreferenceEntry{}}
	if err := s.load(); err != nil {
		_ = file.fs.(rootAliasFS).root.Close()
		return nil, err
	}
	return s, nil
}

func (s *keyboardPreferencesStore) load() error {
	b, exists, err := s.file.read()
	if err != nil {
		return err
	}
	if !exists {
		return s.persistLocked()
	}
	var f keyboardPreferencesStoreFile
	if rejectKeyboardNonCanonicalKeys(b) != nil {
		return errors.New("invalid keyboard preferences store keys")
	}
	if err := decodeStrict(b, &f); err != nil {
		return fmt.Errorf("invalid keyboard preferences store: %w", err)
	}
	if f.Version != 1 || f.Operators == nil || len(f.Operators) > keyboardPreferencesStoreMaxRecords {
		return errors.New("unsupported or oversized keyboard preferences store")
	}
	for _, e := range f.Operators {
		if !validOperatorLogin(e.Operator) || e.Revision == 0 || e.Revision > keyboardPreferencesMaxRevision || e.CreatedAt.IsZero() || e.UpdatedAt.Before(e.CreatedAt) || validateKeyboardPreferences(e.KeyboardPreferences) != nil {
			return errors.New("invalid keyboard preferences record")
		}
		if _, dup := s.records[e.Operator]; dup {
			return errors.New("duplicate keyboard preferences operator")
		}
		s.records[e.Operator] = e
	}
	return nil
}

func (s *keyboardPreferencesStore) persistLocked() error {
	list := make([]keyboardPreferenceEntry, 0, len(s.records))
	for _, e := range s.records {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Operator < list[j].Operator })
	b, err := json.Marshal(keyboardPreferencesStoreFile{Version: 1, Operators: list})
	if err != nil {
		return err
	}
	return s.file.write(b)
}

func (s *keyboardPreferencesStore) get(operator string) (KeyboardPreferenceRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.records[operator]; ok {
		return keyboardRecordOf(e), s.file.fault == nil
	}
	return defaultKeyboardPreferences(), s.file.fault == nil
}

func (s *keyboardPreferencesStore) put(operator string, p KeyboardPreferences, revision uint64) (KeyboardPreferenceRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file.fault != nil {
		return KeyboardPreferenceRecord{}, s.file.fault
	}
	if !validOperatorLogin(operator) || validateKeyboardPreferences(p) != nil {
		return KeyboardPreferenceRecord{}, errKeyboardPreferencesValidation
	}
	old, exists := s.records[operator]
	current := defaultKeyboardPreferences()
	if exists {
		current = keyboardRecordOf(old)
	}
	if current.Revision != revision {
		return current, errKeyboardPreferencesConflict
	}
	if revision >= keyboardPreferencesMaxRevision || !exists && len(s.records) >= keyboardPreferencesStoreMaxRecords {
		return KeyboardPreferenceRecord{}, errKeyboardPreferencesStoreUnavailable
	}
	now := s.now().UTC()
	next := keyboardPreferenceEntry{KeyboardPreferences: copyKeyboardPreferences(p), Operator: operator, Revision: revision + 1, CreatedAt: now, UpdatedAt: now}
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
		return KeyboardPreferenceRecord{}, err
	}
	return keyboardRecordOf(next), nil
}
