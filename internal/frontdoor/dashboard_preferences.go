package frontdoor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"sync"
	"unicode/utf8"
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
}

type dashboardPreferenceEntry struct {
	dashboardPreferenceRecord
	Operator string `json:"operator"`
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
	return record
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
				if duplicate || !validOperatorLogin(entry.Operator) || entry.Revision == 0 || entry.Revision > dashboardRevisionLimit || !validDashboardPreferences(entry.dashboardPreferences) {
					err = errDashboardPreferencesUnavailable
					break
				}
				s.records[entry.Operator] = copyDashboardRecord(entry.dashboardPreferenceRecord)
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

func (s *dashboardPreferencesStore) put(operator string, preferences dashboardPreferences, revision uint64) (dashboardPreferenceRecord, error) {
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
	if revision >= dashboardRevisionLimit || !exists && len(s.records) >= 256 {
		return dashboardPreferenceRecord{}, errDashboardPreferencesUnavailable
	}
	next := copyDashboardRecord(dashboardPreferenceRecord{dashboardPreferences: preferences, Revision: revision + 1})
	entries := make([]dashboardPreferenceEntry, 0, len(s.records)+1)
	for name, record := range s.records {
		if name != operator {
			entries = append(entries, dashboardPreferenceEntry{dashboardPreferenceRecord: record, Operator: name})
		}
	}
	entries = append(entries, dashboardPreferenceEntry{dashboardPreferenceRecord: next, Operator: operator})
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
		dashboardPreferenceRecord
		Available bool `json:"available"`
	}{record, available})
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
	record, err := s.dashboardPreferences.put(operator, preferences, revision)
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
