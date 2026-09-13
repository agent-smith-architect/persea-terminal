package frontdoor

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
)

var errClipboardPreferencesUnavailable = errors.New("clipboard preferences unavailable")

type clipboardPreferencesRecord struct {
	Version                 int    `json:"version"`
	DefaultRetentionSeconds int    `json:"default_retention_seconds"`
	Revision                uint64 `json:"revision"`
}

type clipboardPreferencesStore struct {
	mu     sync.Mutex
	file   *durableFile
	record clipboardPreferencesRecord
}

func openClipboardPreferencesStore(snippetPath string) (*clipboardPreferencesStore, error) {
	if snippetPath == "" {
		return nil, errClipboardPreferencesUnavailable
	}
	file, err := openDurableFile(filepath.Join(filepath.Dir(snippetPath), "clipboard-preferences.json"), "clipboard preferences", 4096, errClipboardPreferencesUnavailable)
	if err != nil {
		return nil, err
	}
	s := &clipboardPreferencesStore{file: file, record: clipboardPreferencesRecord{Version: 1, DefaultRetentionSeconds: clipboardDefaultRetention, Revision: 1}}
	body, exists, err := file.read()
	if err == nil && exists {
		var wire struct {
			Version                 *int    `json:"version"`
			DefaultRetentionSeconds *int    `json:"default_retention_seconds"`
			Revision                *uint64 `json:"revision"`
		}
		if rejectAliasDuplicateKeys(body) != nil || rejectNonCanonicalKeys(body) != nil || decodeStrict(body, &wire) != nil || wire.Version == nil || *wire.Version != 1 || wire.Revision == nil || *wire.Revision == 0 || wire.DefaultRetentionSeconds == nil || !validClipboardRetention(*wire.DefaultRetentionSeconds) {
			err = errClipboardPreferencesUnavailable
		} else {
			s.record = clipboardPreferencesRecord{Version: *wire.Version, DefaultRetentionSeconds: *wire.DefaultRetentionSeconds, Revision: *wire.Revision}
		}
	} else if err == nil {
		body, err = json.Marshal(s.record)
		if err == nil {
			err = file.write(body)
		}
	}
	if err != nil {
		_ = file.fs.(rootAliasFS).root.Close()
		return nil, err
	}
	return s, nil
}

func (s *clipboardPreferencesStore) get() (clipboardPreferencesRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file.fault != nil {
		return clipboardPreferencesRecord{}, errClipboardPreferencesUnavailable
	}
	return s.record, nil
}

func (s *clipboardPreferencesStore) put(seconds int, revision uint64) (clipboardPreferencesRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file.fault != nil {
		return clipboardPreferencesRecord{}, errClipboardPreferencesUnavailable
	}
	if !validClipboardRetention(seconds) || revision == 0 {
		return clipboardPreferencesRecord{}, errSnippetValidation
	}
	if revision != s.record.Revision {
		return s.record, errSnippetConflict
	}
	if revision == ^uint64(0) {
		return clipboardPreferencesRecord{}, errClipboardPreferencesUnavailable
	}
	next := clipboardPreferencesRecord{Version: 1, DefaultRetentionSeconds: seconds, Revision: revision + 1}
	body, err := json.Marshal(next)
	if err == nil {
		err = s.file.write(body)
	}
	if err != nil {
		return clipboardPreferencesRecord{}, errClipboardPreferencesUnavailable
	}
	s.record = next
	return next, nil
}

func (s *Server) clipboardDefault(w http.ResponseWriter) (int, bool) {
	if s.clipboardPreferences == nil || s.clipboardPreferencesErr != nil {
		http.Error(w, "clipboard preferences unavailable", http.StatusServiceUnavailable)
		return 0, false
	}
	record, err := s.clipboardPreferences.get()
	if err != nil {
		http.Error(w, "clipboard preferences unavailable", http.StatusServiceUnavailable)
		return 0, false
	}
	return record.DefaultRetentionSeconds, true
}

func writeClipboardPreferences(w http.ResponseWriter, status int, record clipboardPreferencesRecord) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", `"`+strconv.FormatUint(record.Revision, 10)+`"`)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(record)
}

func (s *Server) getClipboardPreferences(w http.ResponseWriter, r *http.Request) {
	if !s.clipboardImageGate(w, r, http.MethodGet, "GET, PUT") {
		return
	}
	if _, ok := s.clipboardDefault(w); !ok {
		return
	}
	record, err := s.clipboardPreferences.get()
	if err != nil {
		http.Error(w, "clipboard preferences unavailable", http.StatusServiceUnavailable)
		return
	}
	writeClipboardPreferences(w, http.StatusOK, record)
}

func (s *Server) putClipboardPreferences(w http.ResponseWriter, r *http.Request) {
	if !s.clipboardImageGate(w, r, http.MethodPut, "GET, PUT") {
		return
	}
	var wire struct {
		DefaultRetentionSeconds *int    `json:"default_retention_seconds"`
		Revision                *uint64 `json:"revision"`
	}
	if !jsonContentType(r) || decodeBoundedJSON(w, r, 1024, &wire) != nil || wire.DefaultRetentionSeconds == nil || wire.Revision == nil || *wire.Revision == 0 || !validClipboardRetention(*wire.DefaultRetentionSeconds) {
		http.Error(w, "invalid clipboard preferences", http.StatusBadRequest)
		return
	}
	if _, ok := s.clipboardDefault(w); !ok {
		return
	}
	record, err := s.clipboardPreferences.put(*wire.DefaultRetentionSeconds, *wire.Revision)
	if errors.Is(err, errSnippetConflict) {
		writeClipboardPreferences(w, http.StatusPreconditionFailed, record)
		return
	}
	if err != nil {
		http.Error(w, "clipboard preferences unavailable", http.StatusServiceUnavailable)
		return
	}
	writeClipboardPreferences(w, http.StatusOK, record)
}
