package frontdoor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// GET /api/preferences and PUT /api/preferences require ingress identity
// on every method, CSRF on the mutation
// (the secure wrapper), Cache-Control: no-store on every response, strict
// method and content type, bounded body before decode, closed schema, fixed
// error text that never echoes the request.

const preferencesRequestMaxBytes = 4096

// preferencesResponse is the wire shape of every 2xx/412 body. "available"
// says whether a PUT can currently succeed, so a page can show "preferences
// unavailable" instead of mistaking a downed store for a fresh operator.
type preferencesResponse struct {
	PreferenceRecord
	Available bool `json:"available"`
}

func (s *Server) preferencesIdentity(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := identity(r)
	if !ok || id.uid != s.cfg.Ingress.PeerUID || id.operator == "" || id.operator != s.cfg.Ingress.OperatorLogin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", false
	}
	return id.operator, true
}

func writePreferences(w http.ResponseWriter, status int, record PreferenceRecord, available bool) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", fmt.Sprintf("\"%d\"", record.Revision))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(preferencesResponse{PreferenceRecord: record, Available: available})
}

func (s *Server) currentPreferences(operator string) (PreferenceRecord, bool) {
	if s.preferencesErr != nil || s.preferences == nil {
		return defaultPreferences(), false
	}
	record := s.preferences.get(operator)
	s.preferences.mu.Lock()
	available := s.preferences.availableLocked() == nil
	s.preferences.mu.Unlock()
	return record, available
}

func (s *Server) getPreferences(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "invalid preferences request", http.StatusBadRequest)
		return
	}
	record, available := s.currentPreferences(operator)
	writePreferences(w, http.StatusOK, record, available)
}

// parsePreferencesIfMatch accepts a quoted decimal revision; "0" means "no
// stored record", which is what a defaults GET reports.
func parsePreferencesIfMatch(r *http.Request) (uint64, bool) {
	values, ok := r.Header[http.CanonicalHeaderKey("If-Match")]
	if !ok || len(values) != 1 {
		return 0, false
	}
	v := values[0]
	if len(v) < 3 || len(v) > 22 || v[0] != '"' || v[len(v)-1] != '"' {
		return 0, false
	}
	digits := v[1 : len(v)-1]
	if digits != "0" && digits[0] == '0' {
		return 0, false
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// font_size is a tri-state on the wire: a number is the
// operator's explicit size, JSON null is "auto", and an absent key is neither —
// a PUT states the whole record, so a missing field is a malformed request, as
// it has always been. A *int cannot tell "absent" from "null" (both decode to
// nil), so the field is captured raw and decoded in two steps, exactly the
// pattern default_session already uses.
type preferencesWire struct {
	Version  *int            `json:"version"`
	Theme    *string         `json:"theme"`
	FontSize json.RawMessage `json:"font_size"`
	// composer_font_size is a plain number with a default, not a tri-state, so
	// an ABSENT key is the default rather than a malformed request. That is
	// deliberate and it is the field's own rule: a browser from before the
	// field existed still states a whole record as far as IT knows, and
	// refusing its PUT would cost that operator every other preference. Every
	// client this release ships sends it.
	//
	// An explicit null is a different thing and is refused: the schema admits
	// no such value, and a *int cannot tell it from absent, which is why this
	// is captured raw like font_size and default_session.
	ComposerFontSize json.RawMessage `json:"composer_font_size"`
	DefaultSession   json.RawMessage `json:"default_session"`
}

func (s *Server) putPreferences(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "invalid preferences request", http.StatusBadRequest)
		return
	}
	revision, ok := parsePreferencesIfMatch(r)
	if !ok {
		record, available := s.currentPreferences(operator)
		writePreferences(w, http.StatusPreconditionFailed, record, available)
		return
	}
	var wire preferencesWire
	if err := decodeBoundedJSON(w, r, preferencesRequestMaxBytes, &wire); err != nil {
		if errors.Is(err, errRequestTooLarge) {
			http.Error(w, "preferences request too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid preferences request", http.StatusBadRequest)
		return
	}
	if wire.Version == nil || wire.Theme == nil || wire.FontSize == nil || wire.DefaultSession == nil {
		http.Error(w, "invalid preferences request", http.StatusBadRequest)
		return
	}
	prefs := Preferences{Version: *wire.Version, Theme: *wire.Theme, ComposerFontSize: preferenceDefaultComposerFontSize}
	if wire.ComposerFontSize != nil {
		var size int
		if err := decodeStrict(wire.ComposerFontSize, &size); err != nil {
			http.Error(w, "invalid preferences request", http.StatusBadRequest)
			return
		}
		prefs.ComposerFontSize = size
	}
	if string(wire.FontSize) != "null" {
		var size int
		if err := decodeStrict(wire.FontSize, &size); err != nil {
			http.Error(w, "invalid preferences request", http.StatusBadRequest)
			return
		}
		prefs.FontSize = &size
	}
	if string(wire.DefaultSession) != "null" {
		var session DefaultSession
		if err := decodeStrict(wire.DefaultSession, &session); err != nil {
			http.Error(w, "invalid preferences request", http.StatusBadRequest)
			return
		}
		prefs.DefaultSession = &session
	}
	if err := validatePreferences(prefs); err != nil {
		http.Error(w, "invalid preferences request", http.StatusBadRequest)
		return
	}
	if s.preferencesErr != nil || s.preferences == nil {
		http.Error(w, "preferences store unavailable", http.StatusServiceUnavailable)
		return
	}
	record, err := s.preferences.put(operator, prefs, revision)
	if errors.Is(err, errPreferencesConflict) {
		writePreferences(w, http.StatusPreconditionFailed, record, true)
		return
	}
	if errors.Is(err, errPreferencesValidation) {
		http.Error(w, "invalid preferences request", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "preferences store unavailable", http.StatusServiceUnavailable)
		return
	}
	writePreferences(w, http.StatusOK, record, true)
}
