package frontdoor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"
)

const keyboardPreferencesRequestMaxBytes = 32 << 10

type keyboardPreferencesResponse struct {
	KeyboardPreferenceRecord
	Available bool `json:"available"`
}

func writeKeyboardPreferences(w http.ResponseWriter, status int, record KeyboardPreferenceRecord, available bool) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", fmt.Sprintf("\"%d\"", record.Revision))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(keyboardPreferencesResponse{KeyboardPreferenceRecord: record, Available: available})
}

func (s *Server) currentKeyboardPreferences(operator string) (KeyboardPreferenceRecord, bool) {
	if s.keyboardPreferencesErr != nil || s.keyboardPreferences == nil {
		return defaultKeyboardPreferences(), false
	}
	return s.keyboardPreferences.get(operator)
}

func (s *Server) getKeyboardPreferences(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	operator, ok := s.preferencesIdentity(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.RawQuery != "" {
		http.Error(w, "invalid keyboard preferences request", http.StatusBadRequest)
		return
	}
	record, available := s.currentKeyboardPreferences(operator)
	writeKeyboardPreferences(w, http.StatusOK, record, available)
}

func (s *Server) putKeyboardPreferences(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	operator, ok := s.preferencesIdentity(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.RawQuery != "" || !jsonContentType(r) {
		http.Error(w, "invalid keyboard preferences request", http.StatusBadRequest)
		return
	}
	revision, ok := parsePreferencesIfMatch(r)
	if !ok {
		record, available := s.currentKeyboardPreferences(operator)
		writeKeyboardPreferences(w, http.StatusPreconditionFailed, record, available)
		return
	}
	var prefs KeyboardPreferences
	if err := decodeKeyboardPreferences(w, r, &prefs); err != nil {
		if errors.Is(err, errRequestTooLarge) {
			http.Error(w, "keyboard preferences request too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid keyboard preferences request", http.StatusBadRequest)
		return
	}
	if validateKeyboardPreferences(prefs) != nil {
		http.Error(w, "invalid keyboard preferences request", http.StatusBadRequest)
		return
	}
	if s.keyboardPreferencesErr != nil || s.keyboardPreferences == nil {
		http.Error(w, "keyboard preferences store unavailable", http.StatusServiceUnavailable)
		return
	}
	record, err := s.keyboardPreferences.put(operator, prefs, revision)
	if errors.Is(err, errKeyboardPreferencesConflict) {
		writeKeyboardPreferences(w, http.StatusPreconditionFailed, record, true)
		return
	}
	if err != nil {
		http.Error(w, "keyboard preferences store unavailable", http.StatusServiceUnavailable)
		return
	}
	writeKeyboardPreferences(w, http.StatusOK, record, true)
}

// Prefix names are user-defined map keys, unlike lowercase structural fields.
// Keep the common decoder's size, UTF-8, duplicate and closed-schema checks
// while admitting the declared prefix-name grammar at precisely that map.
func decodeKeyboardPreferences(w http.ResponseWriter, r *http.Request, dst *KeyboardPreferences) error {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, keyboardPreferencesRequestMaxBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errRequestTooLarge
		}
		return errRequestInvalid
	}
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || trimmed[0] != '{' || !utf8.Valid(b) {
		return errRequestInvalid
	}
	if rejectAliasDuplicateKeys(b) != nil || rejectKeyboardNonCanonicalKeys(b) != nil {
		return errRequestInvalid
	}
	return decodeStrict(b, dst)
}

func rejectKeyboardNonCanonicalKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func(bool) error
	walk = func(prefixes bool) error {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			for dec.More() {
				token, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || prefixes && !validKeyboardPrefixName(key) || !prefixes && !canonicalKeyRE.MatchString(key) {
					return errRequestInvalid
				}
				if err := walk(!prefixes && key == "prefixes"); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := walk(false); err != nil {
					return err
				}
			}
		}
		_, err = dec.Token()
		return err
	}
	return walk(false)
}
