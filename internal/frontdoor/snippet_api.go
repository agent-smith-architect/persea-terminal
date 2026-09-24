package frontdoor

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// /api/snippets routes  under the M4 route contract:
// ingress identity on every method, CSRF on every mutation (secure wrapper),
// Cache-Control: no-store on every response, strict method and content type,
// http.MaxBytesReader before any decode, DisallowUnknownFields, exactly one
// JSON value then EOF, and fixed bounded error text that never echoes a body,
// a label or a preview.

// snippetRequestMaxBytes bounds the raw request. A 16 KiB body whose every
// byte is a JSON-escaped control character costs six bytes on the wire, so
// the bound leaves room for a maximal body plus label, origin and framing
// while staying far below the file cap.
const snippetRequestMaxBytes = 128 << 10

func (s *Server) snippetIdentity(w http.ResponseWriter, r *http.Request) bool {
	id, ok := identity(r)
	if !ok || id.uid != s.cfg.Ingress.PeerUID || id.operator == "" || id.operator != s.cfg.Ingress.OperatorLogin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

// snippetGate is the shared prologue: no-store, identity, exact method,
// no query string, and the exact media type on mutations. It reports
// whether the handler may continue.
func (s *Server) snippetGate(w http.ResponseWriter, r *http.Request, method, allow string) bool {
	w.Header().Set("Cache-Control", "no-store")
	if !s.snippetIdentity(w, r) {
		return false
	}
	if r.Method != method {
		w.Header().Set("Allow", allow)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if r.URL.RawQuery != "" || (method != http.MethodGet && !jsonContentType(r)) {
		http.Error(w, "invalid snippet request", http.StatusBadRequest)
		return false
	}
	return true
}

func writeSnippet(w http.ResponseWriter, status int, r SnippetRecord) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(r)
}

// writeSnippetError maps store errors to the fixed status/text table. A
// conflict carries the current record so the page can re-read.
func writeSnippetError(w http.ResponseWriter, err error, current SnippetRecord) {
	switch {
	case errors.Is(err, errSnippetConflict):
		writeSnippet(w, http.StatusConflict, current)
	case errors.Is(err, errSnippetNotFound):
		http.Error(w, "snippet not found", http.StatusNotFound)
	case errors.Is(err, errSnippetTooLarge):
		http.Error(w, "snippet too large", http.StatusRequestEntityTooLarge)
	case errors.Is(err, errSnippetStoreFull):
		http.Error(w, "snippet store full", http.StatusInsufficientStorage)
	case errors.Is(err, errSnippetValidation):
		http.Error(w, "invalid snippet request", http.StatusBadRequest)
	default:
		http.Error(w, "snippet store unavailable", http.StatusServiceUnavailable)
	}
}

func (s *Server) snippetStoreReady(w http.ResponseWriter) bool {
	if s.snippetErr != nil || s.snippets == nil {
		http.Error(w, "snippet store unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func decodeSnippetRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := decodeBoundedJSON(w, r, snippetRequestMaxBytes, dst); err != nil {
		if errors.Is(err, errRequestTooLarge) {
			http.Error(w, "snippet too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid snippet request", http.StatusBadRequest)
		}
		return false
	}
	return true
}

func (s *Server) listSnippets(w http.ResponseWriter, r *http.Request) {
	if !s.snippetGate(w, r, http.MethodGet, "GET, POST") || !s.snippetStoreReady(w) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"items": s.snippets.list()})
}

type snippetCreateWire struct {
	Kind      *string                 `json:"kind"`
	Label     *string                 `json:"label"`
	Body      *string                 `json:"body"`
	Pinned    *bool                   `json:"pinned"`
	Origin    *string                 `json:"origin"`
	Retention clipboardRetentionInput `json:"retention_seconds"`
}

func (s *Server) createSnippet(w http.ResponseWriter, r *http.Request) {
	if !s.snippetGate(w, r, http.MethodPost, "GET, POST") {
		return
	}
	var wire snippetCreateWire
	if !decodeSnippetRequest(w, r, &wire) {
		return
	}
	if wire.Kind == nil || wire.Body == nil {
		http.Error(w, "invalid snippet request", http.StatusBadRequest)
		return
	}
	// The closed grammar lives in the store; the route only refuses fields a
	// kind cannot carry so a clip never smuggles a label or a pin.
	if *wire.Kind == snippetKindClip && (wire.Label != nil || wire.Pinned != nil) {
		http.Error(w, "invalid snippet request", http.StatusBadRequest)
		return
	}
	label, pinned, origin := "", false, ""
	if wire.Label != nil {
		label = *wire.Label
	}
	if wire.Pinned != nil {
		pinned = *wire.Pinned
	}
	if wire.Origin != nil {
		origin = *wire.Origin
	}
	// The OSC 52 record is reached only through PUT /api/snippets/osc52; a
	// POST that names the reserved token is refused before the store sees it.
	if strings.TrimSpace(origin) == snippetOSCID {
		http.Error(w, "invalid snippet request", http.StatusBadRequest)
		return
	}
	if !s.snippetStoreReady(w) {
		return
	}
	retention := 0
	if wire.Retention.Present {
		retention = wire.Retention.Value
	} else if *wire.Kind == snippetKindClip {
		var ok bool
		retention, ok = s.clipboardDefault(w)
		if !ok {
			return
		}
	}
	record, err := s.snippets.create(*wire.Kind, label, *wire.Body, pinned, origin, retention)
	if err != nil {
		writeSnippetError(w, err, SnippetRecord{})
		return
	}
	writeSnippet(w, http.StatusCreated, record)
}

type snippetOSCWire struct {
	Body   *string `json:"body"`
	Origin *string `json:"origin"`
}

// parseOptionalIfMatch reads an optional quoted decimal revision. The three
// results are: absent (nil, true), valid (n, true), malformed (nil, false).
func parseOptionalIfMatch(r *http.Request) (*uint64, bool) {
	if _, present := r.Header[http.CanonicalHeaderKey("If-Match")]; !present {
		return nil, true
	}
	n, ok := parsePreferencesIfMatch(r)
	if !ok {
		return nil, false
	}
	return &n, true
}

// upsertOSCSnippet serves PUT /api/snippets/osc52 (advisor E2): the
// idempotent upsert of the one global, server-owned OSC 52 record under the
// same M4 contract as every other mutation. Body {body, origin?}; origin is
// sanitized presentation metadata for the latest writer. An optional
// If-Match lets a device retry against a stale revision; absent means the
// last committed write wins.
func (s *Server) upsertOSCSnippet(w http.ResponseWriter, r *http.Request) {
	if !s.snippetGate(w, r, http.MethodPut, "PUT") {
		return
	}
	var wire snippetOSCWire
	if !decodeSnippetRequest(w, r, &wire) {
		return
	}
	expect, ok := parseOptionalIfMatch(r)
	if wire.Body == nil || !ok {
		http.Error(w, "invalid snippet request", http.StatusBadRequest)
		return
	}
	origin := ""
	if wire.Origin != nil {
		origin = *wire.Origin
	}
	if !s.snippetStoreReady(w) {
		return
	}
	retention, ready := s.clipboardDefault(w)
	if !ready {
		return
	}
	record, err := s.snippets.upsertOSC(*wire.Body, origin, expect, retention)
	if errors.Is(err, errSnippetConflict) {
		// CAS needs only a revision. Private publication content is never a
		// conflict-response read path, including after deletion or expiry.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": snippetOSCID, "revision": record.Revision})
		return
	}
	if err != nil {
		writeSnippetError(w, err, SnippetRecord{})
		return
	}
	writeSnippet(w, http.StatusOK, record)
}

type snippetUpdateWire struct {
	Label     *string                 `json:"label"`
	Body      *string                 `json:"body"`
	Pinned    *bool                   `json:"pinned"`
	Revision  *uint64                 `json:"revision"`
	Retention clipboardRetentionInput `json:"retention_seconds"`
}

func (s *Server) updateSnippet(w http.ResponseWriter, r *http.Request) {
	if !s.snippetGate(w, r, http.MethodPatch, "PATCH, DELETE") {
		return
	}
	id := r.PathValue("id")
	var wire snippetUpdateWire
	if !decodeSnippetRequest(w, r, &wire) {
		return
	}
	if wire.Revision == nil || *wire.Revision == 0 {
		http.Error(w, "invalid snippet request", http.StatusBadRequest)
		return
	}
	if !snippetIDRE.MatchString(id) {
		http.Error(w, "snippet not found", http.StatusNotFound)
		return
	}
	if !s.snippetStoreReady(w) {
		return
	}
	record, err := s.snippets.update(id, snippetUpdate{Label: wire.Label, Body: wire.Body, Pinned: wire.Pinned, RetentionSeconds: wire.Retention.pointer()}, *wire.Revision)
	if err != nil {
		writeSnippetError(w, err, record)
		return
	}
	writeSnippet(w, http.StatusOK, record)
}

type snippetDeleteWire struct {
	Revision *uint64 `json:"revision"`
}

func (s *Server) deleteSnippet(w http.ResponseWriter, r *http.Request) {
	if !s.snippetGate(w, r, http.MethodDelete, "PATCH, DELETE") {
		return
	}
	id := r.PathValue("id")
	var wire snippetDeleteWire
	if !decodeSnippetRequest(w, r, &wire) {
		return
	}
	if wire.Revision == nil || *wire.Revision == 0 {
		http.Error(w, "invalid snippet request", http.StatusBadRequest)
		return
	}
	if !snippetIDRE.MatchString(id) && !isOSCSnippetID(id) {
		http.Error(w, "snippet not found", http.StatusNotFound)
		return
	}
	if !s.snippetStoreReady(w) {
		return
	}
	record, err := s.snippets.delete(id, *wire.Revision)
	if err != nil {
		writeSnippetError(w, err, record)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
