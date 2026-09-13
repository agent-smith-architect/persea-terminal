package frontdoor

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// UX8-F1 (ruling J-UX-9): the operator record carries a font-size tri-state —
// an explicit 9…24, or "auto". "Auto" is JSON null on the wire and on disk,
// never a sentinel inside the range and never the absence of the key, so no
// number is spent on it and "never chosen" is not "deliberately chose 14".
//
// Every case here speaks JSON rather than the Go field, so this file compiles
// against the pre-ruling shape too and its failures are assertion failures with
// the observed bytes, not compile errors.

// f1Preferences builds a Preferences value the way a request body does.
func f1Preferences(t *testing.T, body string) Preferences {
	t.Helper()
	var p Preferences
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("fixture %s: %v", body, err)
	}
	// composer_font_size is a plain number with a default rather than a
	// tri-state, so a body that omits it means the default — which is exactly
	// what putPreferences does with the same body. Applying the rule here
	// keeps every fixture below speaking the JSON it was written to speak.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &keys); err != nil {
		t.Fatalf("fixture %s: %v", body, err)
	}
	if _, ok := keys["composer_font_size"]; !ok {
		p.ComposerFontSize = preferenceDefaultComposerFontSize
	}
	return p
}

// f1FontRaw is the record's font_size exactly as it reaches a client.
func f1FontRaw(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	raw, ok := fields["font_size"]
	if !ok {
		t.Fatalf("font_size key absent from %s", encoded)
	}
	return string(raw)
}

const (
	f1AutoRecord      = `{"version":1,"theme":"dracula","font_size":null,"default_session":null}`
	f1ExplicitRecord  = `{"version":1,"theme":"dracula","font_size":11,"default_session":null}`
	f1CeilingRecord   = `{"version":1,"theme":"dracula","font_size":24,"default_session":null}`
	f1BelowFloor      = `{"version":1,"theme":"dracula","font_size":8,"default_session":null}`
	f1AboveCeiling    = `{"version":1,"theme":"dracula","font_size":25,"default_session":null}`
	f1AutoOtherTheme  = `{"version":1,"theme":"one-dark","font_size":null,"default_session":null}`
	f1MissingFontSize = `{"version":1,"theme":"default","default_session":null}`
)

func TestPreferencesStoreFontSizeTriState(t *testing.T) {
	dir := shortTestDir(t)
	path := filepath.Join(dir, "preferences.json")
	s, err := newPreferencesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing stored is "auto", not a number. A default number would be
	// indistinguishable from the same number chosen on purpose, which is the
	// ambiguity J-UX-9 forbids.
	if got := f1FontRaw(t, s.get("operator@example.com")); got != "null" {
		t.Fatalf("the default record's font_size is %s, want null", got)
	}
	auto, err := s.put("operator@example.com", f1Preferences(t, f1AutoRecord), 0)
	if err != nil {
		t.Fatalf("auto record refused: %v", err)
	}
	if got := f1FontRaw(t, auto); got != "null" {
		t.Fatalf("stored auto record returned font_size %s, want null", got)
	}
	// Auto is durable, not merely tolerated: the file says null and a reopen
	// reads it back as auto rather than as a number.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"font_size":null`) {
		t.Fatalf("auto was not persisted as null: %s", raw)
	}
	reopened, err := newPreferencesStore(path)
	if err != nil {
		t.Fatalf("reopen of an auto record failed: %v", err)
	}
	if got := f1FontRaw(t, reopened.get("operator@example.com")); got != "null" {
		t.Fatalf("reopened auto record returned font_size %s, want null", got)
	}
	// Both directions on the one revision chain the record has always had.
	explicit, err := reopened.put("operator@example.com", f1Preferences(t, f1ExplicitRecord), 1)
	if err != nil || f1FontRaw(t, explicit) != "11" || explicit.Revision != 2 {
		t.Fatalf("explicit after auto=%s revision=%d err=%v", f1FontRaw(t, explicit), explicit.Revision, err)
	}
	back, err := reopened.put("operator@example.com", f1Preferences(t, f1AutoOtherTheme), 2)
	if err != nil || f1FontRaw(t, back) != "null" || back.Revision != 3 {
		t.Fatalf("auto after explicit=%s revision=%d err=%v", f1FontRaw(t, back), back.Revision, err)
	}
	// The closed range still holds for an explicit value; auto did not open it.
	for name, body := range map[string]string{"below_floor": f1BelowFloor, "above_ceiling": f1AboveCeiling} {
		if _, err := reopened.put("operator@example.com", f1Preferences(t, body), 3); !errors.Is(err, errPreferencesValidation) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	if got := f1FontRaw(t, reopened.get("operator@example.com")); got != "null" {
		t.Fatalf("a refused write changed the record: font_size %s", got)
	}
	// The ceiling is an ordinary explicit value, not a second way to say auto.
	ceiling, err := reopened.put("operator@example.com", f1Preferences(t, f1CeilingRecord), 3)
	if err != nil || f1FontRaw(t, ceiling) != "24" {
		t.Fatalf("ceiling put=%s err=%v", f1FontRaw(t, ceiling), err)
	}
}

// A record written before J-UX-9 holds a JSON number. It must read back as an
// explicit size — its operator chose it — with no migration step.
func TestPreferencesStoreReadsPreTriStateRecord(t *testing.T) {
	dir := shortTestDir(t)
	path := filepath.Join(dir, "preferences.json")
	legacy := `{"version":1,"operators":[{"operator":"operator@example.com","theme":"dracula","font_size":16,"default_session":null,"revision":3,"created_at":"2026-08-01T10:00:00Z","updated_at":"2026-08-02T11:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := newPreferencesStore(path)
	if err != nil {
		t.Fatalf("a record in the pre-tri-state shape was refused: %v", err)
	}
	got := s.get("operator@example.com")
	if !got.Stored || got.Revision != 3 || got.Theme != "dracula" || f1FontRaw(t, got) != "16" {
		t.Fatalf("pre-tri-state record read back as stored=%v revision=%d theme=%s font_size=%s", got.Stored, got.Revision, got.Theme, f1FontRaw(t, got))
	}
	// It keeps its number across a write of an unrelated field, and keeps its
	// created_at, so nothing of the old shape is lost.
	next, err := s.put("operator@example.com", f1Preferences(t, `{"version":1,"theme":"one-dark","font_size":16,"default_session":null}`), 3)
	if err != nil || f1FontRaw(t, next) != "16" || next.Revision != 4 {
		t.Fatalf("write over a pre-tri-state record=%s revision=%d err=%v", f1FontRaw(t, next), next.Revision, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"font_size":16`) || !strings.Contains(string(raw), `"created_at":"2026-08-01T10:00:00Z"`) {
		t.Fatalf("rewritten record=%s", raw)
	}
}

func TestPreferencesAPIFontSizeTriState(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.preferencesErr != nil {
		t.Fatal(s.preferencesErr)
	}
	get := func() *httptest.ResponseRecorder {
		return ergoRequest(t, s, http.MethodGet, "http://localhost/api/preferences", "", nil, nil)
	}
	put := func(etag, body string) *httptest.ResponseRecorder {
		return ergoRequest(t, s, http.MethodPut, "http://localhost/api/preferences", "application/json", strings.NewReader(body), func(r *http.Request) {
			r.Header.Set("If-Match", etag)
		})
	}
	fontOf := func(w *httptest.ResponseRecorder) string {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
		raw, ok := fields["font_size"]
		if !ok {
			t.Fatalf("font_size key absent from %s", w.Body.String())
		}
		return string(raw)
	}

	// The defaults answer says auto in the JSON itself: the key is present and
	// null, so a client tells it apart from a number without guessing.
	if got := fontOf(get()); got != "null" {
		t.Fatalf("defaults font_size=%s, want null", got)
	}

	stored := put(`"0"`, f1AutoRecord)
	if stored.Code != http.StatusOK || stored.Header().Get("ETag") != `"1"` {
		t.Fatalf("auto PUT=%d etag=%q body=%s", stored.Code, stored.Header().Get("ETag"), stored.Body.String())
	}
	if got := fontOf(stored); got != "null" {
		t.Fatalf("auto PUT returned font_size=%s, want null", got)
	}
	if got := fontOf(get()); got != "null" {
		t.Fatalf("stored auto GET font_size=%s, want null", got)
	}

	// An absent key is not "auto": a PUT states the whole record, so a missing
	// font_size stays the malformed request it has always been.
	if w := put(`"1"`, f1MissingFontSize); w.Code != http.StatusBadRequest {
		t.Fatalf("absent font_size=%d %s", w.Code, w.Body.String())
	}
	if got := fontOf(get()); got != "null" {
		t.Fatalf("a refused PUT changed the record: font_size=%s", got)
	}

	// CAS is untouched by the new state: a stale If-Match on an auto PUT is
	// still a 412 that returns server authority.
	stale := put(`"0"`, f1AutoOtherTheme)
	if stale.Code != http.StatusPreconditionFailed || stale.Header().Get("ETag") != `"1"` {
		t.Fatalf("stale auto PUT=%d etag=%q", stale.Code, stale.Header().Get("ETag"))
	}
	if got := decodePreferences(t, stale); got.Theme != "dracula" || got.Revision != 1 {
		t.Fatalf("the 412 body did not carry server authority: %+v", got)
	}
	if got := fontOf(stale); got != "null" {
		t.Fatalf("the 412 body font_size=%s, want null", got)
	}

	// Explicit and auto alternate on one revision chain.
	explicit := put(`"1"`, f1ExplicitRecord)
	if explicit.Code != http.StatusOK || fontOf(explicit) != "11" {
		t.Fatalf("explicit PUT=%d font_size=%s", explicit.Code, fontOf(explicit))
	}
	if got := fontOf(get()); got != "11" {
		t.Fatalf("stored explicit GET font_size=%s", got)
	}
	if again := put(`"2"`, f1AutoRecord); again.Code != http.StatusOK || fontOf(again) != "null" {
		t.Fatalf("auto after explicit=%d font_size=%s", again.Code, fontOf(again))
	}
	// Out-of-range explicit values are still refused, in both directions.
	for name, body := range map[string]string{"below_floor": f1BelowFloor, "above_ceiling": f1AboveCeiling} {
		if w := put(`"3"`, body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s=%d %s", name, w.Code, w.Body.String())
		}
	}
	if got := fontOf(get()); got != "null" {
		t.Fatalf("final record font_size=%s, want null", got)
	}
}
