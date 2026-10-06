package frontdoor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"persea-terminal/internal/proto"
)

const dashboardFavoritesLimit = 128
const dashboardRevisionLimit = 1<<53 - 1

var errDashboardPreferencesUnavailable = errors.New("dashboard preferences unavailable")
var errDashboardPreferencesConflict = errors.New("dashboard preferences revision conflict")
var errDashboardPreferencesInvalid = errors.New("invalid dashboard preferences")

// This separate file keeps rollback compatible with the closed appearance
// preferences schema. Favorites contain incarnation identities, never handles.
type dashboardPreferences struct {
	Version   int      `json:"version"`
	Favorites []string `json:"favorites"`
}

type dashboardPreferenceRecord struct {
	dashboardPreferences
	Revision uint64 `json:"revision"`
	// Names keeps the tmux session name last seen for each favorite. After a
	// tmux restart it lets the favorite follow a session with the same name,
	// as an alias does. It stays on the server; pages receive scopes only.
	Names map[string]string `json:"-"`
}

type dashboardFavoriteName struct {
	Scope string `json:"scope"`
	Name  string `json:"name"`
}

type dashboardPreferenceEntry struct {
	dashboardPreferenceRecord
	Operator      string                  `json:"operator"`
	FavoriteNames []dashboardFavoriteName `json:"favorite_names,omitempty"`
}

// fileEntry lists a record's names in favorites order: the store file only
// allows identifier keys, so names cannot be an object keyed by scope.
func fileEntry(operator string, record dashboardPreferenceRecord) dashboardPreferenceEntry {
	entry := dashboardPreferenceEntry{dashboardPreferenceRecord: record, Operator: operator}
	for _, scope := range record.Favorites {
		if name, ok := record.Names[scope]; ok {
			entry.FavoriteNames = append(entry.FavoriteNames, dashboardFavoriteName{Scope: scope, Name: name})
		}
	}
	return entry
}

func (entry dashboardPreferenceEntry) record() (dashboardPreferenceRecord, bool) {
	record := copyDashboardRecord(entry.dashboardPreferenceRecord)
	for _, named := range entry.FavoriteNames {
		if _, duplicate := record.Names[named.Scope]; duplicate {
			return record, false
		}
		if record.Names == nil {
			record.Names = map[string]string{}
		}
		record.Names[named.Scope] = named.Name
	}
	return record, validDashboardRecord(record)
}

type dashboardPreferencesFile struct {
	Version   int                        `json:"version"`
	Operators []dashboardPreferenceEntry `json:"operators"`
}

type dashboardPreferencesStore struct {
	mu      sync.Mutex
	file    *durableFile
	records map[string]dashboardPreferenceRecord
}

func emptyDashboardPreferences() dashboardPreferenceRecord {
	return dashboardPreferenceRecord{dashboardPreferences: dashboardPreferences{Version: 1, Favorites: []string{}}}
}

func copyDashboardRecord(record dashboardPreferenceRecord) dashboardPreferenceRecord {
	record.Favorites = append([]string{}, record.Favorites...)
	record.Names = maps.Clone(record.Names)
	return record
}

// dashboardScopeAuthority reads the session identity a page used as a
// favorite scope: the JSON array the dashboard builds from an inventory row.
func dashboardScopeAuthority(scope string) (proto.Authority, bool) {
	if !validDashboardScope(scope) {
		return proto.Authority{}, false
	}
	var parts []json.RawMessage
	if json.Unmarshal([]byte(scope), &parts) != nil {
		return proto.Authority{}, false
	}
	var a proto.Authority
	targets := []any{&a.Realm, &a.Server, &a.SelectorKind, &a.SelectorValue, &a.BootID, &a.SessionID, &a.UID, &a.ServerPID, &a.ServerStart, &a.SessionCreated}
	for i, target := range targets {
		if json.Unmarshal(parts[i], target) != nil {
			return proto.Authority{}, false
		}
	}
	return a, true
}

// dashboardScope builds the scope the dashboard computes for a session
// (JSON.stringify of the same ten fields). It refuses an identity whose JSON
// form would differ between Go and JavaScript, so a rebound favorite always
// matches the page's own scope.
func dashboardScope(a proto.Authority) (string, bool) {
	// Go escapes U+2028 and U+2029; JSON.stringify writes them as they are.
	for _, field := range []string{a.Realm, a.Server, a.SelectorKind, a.SelectorValue, a.BootID, a.SessionID} {
		if strings.ContainsAny(field, "\u2028\u2029") {
			return "", false
		}
	}
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if encoder.Encode([]any{a.Realm, a.Server, a.SelectorKind, a.SelectorValue, a.BootID, a.SessionID, a.UID, a.ServerPID, a.ServerStart, a.SessionCreated}) != nil {
		return "", false
	}
	scope := strings.TrimSuffix(b.String(), "\n")
	if !validDashboardScope(scope) {
		return "", false
	}
	parsed, ok := dashboardScopeAuthority(scope)
	return scope, ok && authorityKey(parsed) == authorityKey(a)
}

func validDashboardSessionName(name string) bool {
	return name != "" && len(name) <= 1024 && utf8.ValidString(name)
}

func validDashboardScope(scope string) bool {
	if len(scope) == 0 || len(scope) > 2048 || !utf8.ValidString(scope) {
		return false
	}
	var parts []json.RawMessage
	if json.Unmarshal([]byte(scope), &parts) != nil || len(parts) != 10 {
		return false
	}
	for i, part := range parts {
		if i < 6 {
			var value string
			if json.Unmarshal(part, &value) != nil || value == "" {
				return false
			}
			for _, char := range value {
				if char < 0x20 || char == 0x7f {
					return false
				}
			}
		} else {
			var value uint64
			if string(part) == "null" || json.Unmarshal(part, &value) != nil || value > dashboardRevisionLimit {
				return false
			}
		}
	}
	return true
}

func validDashboardRecord(record dashboardPreferenceRecord) bool {
	if !validDashboardPreferences(record.dashboardPreferences) {
		return false
	}
	for scope, name := range record.Names {
		if !validDashboardSessionName(name) || !slices.Contains(record.Favorites, scope) {
			return false
		}
	}
	return true
}

func validDashboardPreferences(p dashboardPreferences) bool {
	if p.Version != 1 || p.Favorites == nil || len(p.Favorites) > dashboardFavoritesLimit {
		return false
	}
	seen := make(map[string]bool, len(p.Favorites))
	for _, scope := range p.Favorites {
		if seen[scope] || !validDashboardScope(scope) {
			return false
		}
		seen[scope] = true
	}
	return true
}

func openDashboardPreferencesStore(preferencesPath string) (*dashboardPreferencesStore, error) {
	if preferencesPath == "" {
		return nil, errDashboardPreferencesUnavailable
	}
	file, err := openDurableFile(filepath.Join(filepath.Dir(preferencesPath), "dashboard-preferences.json"), "dashboard preferences", 1<<20, errDashboardPreferencesUnavailable)
	if err != nil {
		return nil, err
	}
	s := &dashboardPreferencesStore{file: file, records: map[string]dashboardPreferenceRecord{}}
	body, exists, err := file.read()
	if err == nil && exists {
		var wire dashboardPreferencesFile
		if rejectAliasDuplicateKeys(body) != nil || rejectNonCanonicalKeys(body) != nil || decodeStrict(body, &wire) != nil || wire.Version != 1 || wire.Operators == nil || len(wire.Operators) > 256 {
			err = errDashboardPreferencesUnavailable
		} else {
			for _, entry := range wire.Operators {
				_, duplicate := s.records[entry.Operator]
				record, valid := entry.record()
				if duplicate || !validOperatorLogin(entry.Operator) || entry.Revision == 0 || entry.Revision > dashboardRevisionLimit || !valid {
					err = errDashboardPreferencesUnavailable
					break
				}
				s.records[entry.Operator] = record
			}
		}
	}
	if err != nil {
		_ = file.fs.(rootAliasFS).root.Close()
		return nil, err
	}
	return s, nil
}

func (s *dashboardPreferencesStore) get(operator string) (dashboardPreferenceRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record, found := s.records[operator]; found {
		return copyDashboardRecord(record), s.file.fault == nil
	}
	return emptyDashboardPreferences(), s.file.fault == nil
}

// put stores a page's whole favorites list. A favorite keeps its known name;
// a new one takes the name the latest inventory reported for that scope.
func (s *dashboardPreferencesStore) put(operator string, preferences dashboardPreferences, revision uint64, sessionName func(scope string) string) (dashboardPreferenceRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file.fault != nil {
		return dashboardPreferenceRecord{}, errDashboardPreferencesUnavailable
	}
	if !validOperatorLogin(operator) || !validDashboardPreferences(preferences) {
		return dashboardPreferenceRecord{}, errDashboardPreferencesInvalid
	}
	current, exists := s.records[operator]
	if !exists {
		current = emptyDashboardPreferences()
	}
	if revision != current.Revision {
		return copyDashboardRecord(current), errDashboardPreferencesConflict
	}
	next := dashboardPreferenceRecord{dashboardPreferences: preferences}
	for _, scope := range preferences.Favorites {
		name, known := current.Names[scope]
		if !known && sessionName != nil {
			name = sessionName(scope)
		}
		if validDashboardSessionName(name) {
			if next.Names == nil {
				next.Names = map[string]string{}
			}
			next.Names[scope] = name
		}
	}
	return s.commitLocked(operator, next)
}

// commitLocked persists next as the operator's record with the next revision.
func (s *dashboardPreferencesStore) commitLocked(operator string, next dashboardPreferenceRecord) (dashboardPreferenceRecord, error) {
	current, exists := s.records[operator]
	if current.Revision >= dashboardRevisionLimit || !exists && len(s.records) >= 256 {
		return dashboardPreferenceRecord{}, errDashboardPreferencesUnavailable
	}
	next = copyDashboardRecord(next)
	next.Revision = current.Revision + 1
	entries := make([]dashboardPreferenceEntry, 0, len(s.records)+1)
	for name, record := range s.records {
		if name != operator {
			entries = append(entries, fileEntry(name, record))
		}
	}
	entries = append(entries, fileEntry(operator, next))
	sort.Slice(entries, func(i, j int) bool { return entries[i].Operator < entries[j].Operator })
	body, err := json.Marshal(dashboardPreferencesFile{Version: 1, Operators: entries})
	if err == nil {
		err = s.file.write(body)
	}
	if err != nil {
		return dashboardPreferenceRecord{}, errDashboardPreferencesUnavailable
	}
	s.records[operator] = next
	return copyDashboardRecord(next), nil
}

// reconcile applies the alias rule to one operator's favorites after an
// inventory read: a favorite whose session is live records its current name;
// a favorite whose session is gone moves to the live session with the same
// realm, server and name, when that server's list is complete and no other
// favorite holds that session. The newest favorite wins a name, and the
// others waiting for it on a server whose list is complete are dropped: a
// name on one tmux server belongs to one session at a time, so they are dead
// duplicates that would otherwise take the session back after the operator
// removed its star. On an incomplete list a favorite that is not seen may
// still be running under another name, so it stays. It returns the record's
// revision, which changes only when something changed.
func (s *dashboardPreferencesStore) reconcile(operator string, live []aliasSession, complete map[string]bool) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.records[operator]
	if s.file.fault != nil {
		return current.Revision, errDashboardPreferencesUnavailable
	}
	if !exists {
		return current.Revision, nil
	}
	byIncarnation := map[string]aliasSession{}
	byName := map[string]aliasSession{}
	for _, target := range live {
		a := target.Authority
		if !a.Valid() || !validDashboardSessionName(target.Name) {
			continue
		}
		byIncarnation[authorityKey(a)] = target
		if complete[a.Realm+"\x00"+a.Server] {
			byName[a.Realm+"\x00"+a.Server+"\x00"+target.Name] = target
		}
	}
	next := copyDashboardRecord(current)
	if next.Names == nil {
		next.Names = map[string]string{}
	}
	nameKey := func(a proto.Authority, name string) string { return a.Realm + "\x00" + a.Server + "\x00" + name }
	held := map[string]bool{}
	owned := map[string]bool{}
	stale := []int{}
	for i, scope := range next.Favorites {
		a, ok := dashboardScopeAuthority(scope)
		if target, running := byIncarnation[authorityKey(a)]; ok && running {
			held[authorityKey(a)] = true
			owned[nameKey(a, target.Name)] = true
			next.Names[scope] = target.Name
		} else if ok {
			stale = append(stale, i)
		}
	}
	dropped := map[string]bool{}
	for j := len(stale) - 1; j >= 0; j-- {
		i := stale[j]
		scope := next.Favorites[i]
		a, _ := dashboardScopeAuthority(scope)
		name, known := next.Names[scope]
		if !known {
			continue
		}
		if owned[nameKey(a, name)] {
			if complete[a.Realm+"\x00"+a.Server] {
				dropped[scope] = true
			}
			continue
		}
		target, found := byName[nameKey(a, name)]
		if !found || held[authorityKey(target.Authority)] {
			continue
		}
		moved, ok := dashboardScope(target.Authority)
		if !ok || slices.Contains(next.Favorites, moved) {
			continue
		}
		held[authorityKey(target.Authority)] = true
		owned[nameKey(a, name)] = true
		delete(next.Names, scope)
		next.Favorites[i], next.Names[moved] = moved, target.Name
	}
	next.Favorites = slices.DeleteFunc(next.Favorites, func(scope string) bool { return dropped[scope] })
	for scope := range dropped {
		delete(next.Names, scope)
	}
	if len(next.Names) == 0 {
		next.Names = nil
	}
	if slices.Equal(current.Favorites, next.Favorites) && maps.Equal(current.Names, next.Names) {
		return current.Revision, nil
	}
	saved, err := s.commitLocked(operator, next)
	if err != nil {
		return current.Revision, err
	}
	return saved.Revision, nil
}

func (s *Server) currentDashboardPreferences(operator string) (dashboardPreferenceRecord, bool) {
	if s.dashboardPreferences == nil || s.dashboardPreferencesErr != nil {
		return emptyDashboardPreferences(), false
	}
	return s.dashboardPreferences.get(operator)
}

func writeDashboardPreferences(w http.ResponseWriter, status int, record dashboardPreferenceRecord, available bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", fmt.Sprintf("\"%d\"", record.Revision))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Version   int      `json:"version"`
		Favorites []string `json:"favorites"`
		Revision  uint64   `json:"revision"`
		Available bool     `json:"available"`
	}{record.Version, record.Favorites, record.Revision, available})
}

func (s *Server) dashboardPreferencesAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	operator, ok := s.preferencesIdentity(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.RawQuery != "" {
		http.Error(w, "invalid dashboard preferences request", http.StatusBadRequest)
		return
	}
	current, available := s.currentDashboardPreferences(operator)
	if r.Method == http.MethodGet {
		writeDashboardPreferences(w, http.StatusOK, current, available)
		return
	}
	revision, ok := parsePreferencesIfMatch(r)
	if !ok {
		writeDashboardPreferences(w, http.StatusPreconditionFailed, current, available)
		return
	}
	var preferences dashboardPreferences
	if !jsonContentType(r) {
		http.Error(w, "invalid dashboard preferences request", http.StatusBadRequest)
		return
	}
	if err := decodeBoundedJSON(w, r, 320<<10, &preferences); err != nil {
		if errors.Is(err, errRequestTooLarge) {
			http.Error(w, "dashboard preferences request too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid dashboard preferences request", http.StatusBadRequest)
		return
	}
	if !validDashboardPreferences(preferences) {
		http.Error(w, "invalid dashboard preferences request", http.StatusBadRequest)
		return
	}
	if !available {
		http.Error(w, "dashboard preferences unavailable", http.StatusServiceUnavailable)
		return
	}
	record, err := s.dashboardPreferences.put(operator, preferences, revision, s.inventorySessionName)
	if errors.Is(err, errDashboardPreferencesConflict) {
		writeDashboardPreferences(w, http.StatusPreconditionFailed, record, true)
		return
	}
	if err != nil {
		http.Error(w, "dashboard preferences unavailable", http.StatusServiceUnavailable)
		return
	}
	writeDashboardPreferences(w, http.StatusOK, record, true)
}
