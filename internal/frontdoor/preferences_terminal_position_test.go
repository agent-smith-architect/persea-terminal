package frontdoor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Terminal position is one of three closed values, "top-center" by default.
// The browser ships exactly these (ui/src/terminal_position.ts); anything else
// is refused rather than mapped to a fallback.

const terminalPositionSecret = "SENTINEL-position-4be2"

func positionRecord(position string) string {
	return `{"version":1,"theme":"dracula","font_size":null,"composer_font_size":12,"terminal_position":` + position + `,"default_session":null}`
}

func TestPreferencesTerminalPositionValues(t *testing.T) {
	for _, c := range []struct {
		value string
		valid bool
	}{
		{"top-center", true}, {"top-left", true}, {"center", true},
		{"", false}, {"Center", false}, {"TOP-LEFT", false}, {"top_center", false}, {"topcenter", false},
		{" center", false}, {"center ", false}, {"bottom", false}, {"top-right", false}, {"middle", false},
	} {
		p := preferencesFixture("default", 14)
		p.TerminalPosition = c.value
		if err := validatePreferences(p); (err == nil) != c.valid {
			t.Fatalf("terminal position %q -> %v, wanted valid=%v", c.value, err, c.valid)
		}
	}
	if got := defaultPreferences().TerminalPosition; got != "top-center" {
		t.Fatalf("the defaults record places terminals at %q, want top-center", got)
	}
}

// A record from before the field loads and reads as the default; the field is
// then written, persisted and read back. A stored value outside the set does
// not load: the store fails closed rather than inventing a position.
func TestPreferencesStoreTerminalPosition(t *testing.T) {
	dir := shortTestDir(t)
	path := filepath.Join(dir, "preferences.json")
	const before = `{"version":1,"operators":[{"operator":"operator@example.com","theme":"dracula","font_size":16,` +
		`"composer_font_size":13,"default_session":null,"revision":4,` +
		`"created_at":"2026-08-01T10:00:00Z","updated_at":"2026-08-02T11:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := newPreferencesStore(path)
	if err != nil {
		t.Fatalf("a record from before terminal positions was refused: %v", err)
	}
	got := s.get("operator@example.com")
	if !got.Stored || got.Revision != 4 || got.TerminalPosition != "top-center" || got.ComposerFontSize != 13 || !fontIs(got.FontSize, 16) {
		t.Fatalf("a record from before terminal positions lost fidelity: %+v", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "terminal_position") {
		t.Fatalf("loading alone rewrote the store: %s", raw)
	}

	p := preferencesFixture("dracula", 16)
	p.TerminalPosition = "center"
	next, err := s.put("operator@example.com", p, 4)
	if err != nil || next.TerminalPosition != "center" || next.Revision != 5 {
		t.Fatalf("storing center=%+v err=%v", next, err)
	}
	if raw, err = os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"terminal_position":"center"`) || !strings.Contains(string(raw), `"created_at":"2026-08-01T10:00:00Z"`) {
		t.Fatalf("the position was not persisted with the record: %s", raw)
	}
	reopened, err := newPreferencesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.get("operator@example.com"); got.TerminalPosition != "center" || got.Revision != 5 {
		t.Fatalf("the stored position did not survive a reopen: %+v", got)
	}
	p.TerminalPosition = "bottom"
	if _, err := reopened.put("operator@example.com", p, 5); err == nil {
		t.Fatal("an unknown position was stored")
	}
	if got := reopened.get("operator@example.com"); got.TerminalPosition != "center" || got.Revision != 5 {
		t.Fatalf("a refused write changed the record: %+v", got)
	}

	for name, stored := range map[string]string{"unknown": `"bottom"`, "empty": `""`, "wrong_case": `"Center"`, "number": `1`, "null": `null`} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(shortTestDir(t), "preferences.json")
			file := `{"version":1,"operators":[{"operator":"operator@example.com","theme":"dracula","font_size":null,` +
				`"composer_font_size":11,"terminal_position":` + stored + `,"default_session":null,"revision":1,` +
				`"created_at":"2026-08-01T10:00:00Z","updated_at":"2026-08-01T10:00:00Z"}]}`
			if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := newPreferencesStore(path); err == nil {
				t.Fatalf("a store holding terminal_position %s loaded", stored)
			}
		})
	}
}

// The wire: every GET states the position, every PUT must state one of the
// three, and every other shape is refused with fixed text and no mutation.
func TestPreferencesAPITerminalPosition(t *testing.T) {
	cfg := ergoFrontConfig(t)
	server := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if server.preferencesErr != nil {
		t.Fatal(server.preferencesErr)
	}
	get := func() *httptest.ResponseRecorder {
		return ergoRequest(t, server, http.MethodGet, "http://localhost/api/preferences", "", nil, nil)
	}
	put := func(etag, body string) *httptest.ResponseRecorder {
		return ergoRequest(t, server, http.MethodPut, "http://localhost/api/preferences", "application/json", strings.NewReader(body), func(r *http.Request) {
			r.Header.Set("If-Match", etag)
		})
	}
	positionOf := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
		raw, ok := fields["terminal_position"]
		if !ok {
			t.Fatalf("terminal_position key absent from %s", w.Body.String())
		}
		return string(raw)
	}

	if got := positionOf(get()); got != `"top-center"` {
		t.Fatalf("defaults terminal_position=%s, want \"top-center\"", got)
	}
	revision := 0
	for _, position := range []string{"center", "top-left", "top-center"} {
		w := put(`"`+strconv.Itoa(revision)+`"`, positionRecord(`"`+position+`"`))
		if w.Code != http.StatusOK || positionOf(w) != `"`+position+`"` {
			t.Fatalf("PUT %s=%d %s", position, w.Code, w.Body.String())
		}
		revision++
		if got := positionOf(get()); got != `"`+position+`"` {
			t.Fatalf("GET after storing %s returned %s", position, got)
		}
	}

	stale := put(`"0"`, positionRecord(`"center"`))
	if stale.Code != http.StatusPreconditionFailed || positionOf(stale) != `"top-center"` {
		t.Fatalf("a stale PUT did not return server authority: %d %s", stale.Code, stale.Body.String())
	}

	etag := `"` + strconv.Itoa(revision) + `"`
	for name, body := range map[string]string{
		"absent":         `{"version":1,"theme":"dracula","font_size":null,"composer_font_size":12,"default_session":null}`,
		"null":           positionRecord(`null`),
		"empty":          positionRecord(`""`),
		"unknown":        positionRecord(`"` + terminalPositionSecret + `"`),
		"wrong_case":     positionRecord(`"Center"`),
		"underscored":    positionRecord(`"top_center"`),
		"padded":         positionRecord(`" center"`),
		"number":         positionRecord(`1`),
		"boolean":        positionRecord(`true`),
		"object":         positionRecord(`{"value":"center"}`),
		"array":          positionRecord(`["center"]`),
		"duplicate_key":  `{"version":1,"theme":"dracula","font_size":null,"composer_font_size":12,"terminal_position":"center","terminal_position":"top-left","default_session":null}`,
		"folded_key":     `{"version":1,"theme":"dracula","font_size":null,"composer_font_size":12,"TERMINAL_POSITION":"center","default_session":null}`,
		"unknown_field":  `{"version":1,"theme":"dracula","font_size":null,"composer_font_size":12,"terminal_position":"center","terminal_alignment":"center","default_session":null}`,
		"nested_unknown": positionRecord(`"center","position":{"x":"` + terminalPositionSecret + `"}`),
	} {
		assertBoundedRefusal(t, name, put(etag, body), http.StatusBadRequest, terminalPositionSecret)
	}
	if got := decodePreferences(t, get()); got.TerminalPosition != "top-center" || got.Revision != uint64(revision) {
		t.Fatalf("refusals mutated the record: %+v", got)
	}
}
