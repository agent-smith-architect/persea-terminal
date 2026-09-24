package frontdoor

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// the composer's face is an operator preference. Unlike the terminal
// font it is a PLAIN NUMBER with a default, not a tri-state — the composer has
// no auto-fit to hand the decision back to, so "auto" would mean nothing.
//
// A record from before configurable composer fonts loads, and its absent composer face reads as the
// default rather than as a zero. The fixture is literal JSON so this case says
// what an existing production file actually contains.
func TestPreferencesStoreReadsPreComposerFontRecord(t *testing.T) {
	dir := shortTestDir(t)
	path := filepath.Join(dir, "preferences.json")
	const legacy = `{"version":1,"operators":[{"operator":"operator@example.com","theme":"dracula",` +
		`"font_size":16,"default_session":{"realm":"desk-a7","server":"primary","name":"work"},` +
		`"revision":3,"created_at":"2026-08-01T10:00:00Z","updated_at":"2026-08-02T11:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := newPreferencesStore(path)
	if err != nil {
		t.Fatalf("a record from before configurable composer fonts was refused: %v", err)
	}
	got := s.get("operator@example.com")
	if !got.Stored || got.Revision != 3 || got.Theme != "dracula" || !fontIs(got.FontSize, 16) {
		t.Fatalf("record from before configurable composer fonts lost fidelity: %+v", got)
	}
	if got.ComposerFontSize != preferenceDefaultComposerFontSize {
		t.Fatalf("an absent composer face read as %d, want the default %d", got.ComposerFontSize, preferenceDefaultComposerFontSize)
	}

	// A write over it states the face, and every other field survives.
	next, err := s.put("operator@example.com", Preferences{
		Version: 1, Theme: "one-dark", FontSize: fontPtr(16), ComposerFontSize: 11,
		DefaultSession: &DefaultSession{Realm: "desk-a7", Server: "primary", Name: "work"},
	}, 3)
	if err != nil || next.ComposerFontSize != 11 || next.Revision != 4 {
		t.Fatalf("write over a record from before configurable composer fonts: %+v err=%v", next, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"composer_font_size":11`) {
		t.Fatalf("the composer face was not persisted: %s", raw)
	}
	if !strings.Contains(string(raw), `"created_at":"2026-08-01T10:00:00Z"`) {
		t.Fatalf("the rewrite dropped created_at: %s", raw)
	}
	reopened, err := newPreferencesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.get("operator@example.com"); got.ComposerFontSize != 11 {
		t.Fatalf("the stored composer face did not survive a reopen: %+v", got)
	}
}

// The range is the terminal font's, and it is closed on both sides.
func TestPreferencesComposerFontSizeRange(t *testing.T) {
	for _, c := range []struct {
		size  int
		valid bool
	}{{8, false}, {9, true}, {13, true}, {24, true}, {25, false}, {0, false}, {-1, false}} {
		err := validatePreferences(Preferences{Version: 1, Theme: "default", ComposerFontSize: c.size})
		if (err == nil) != c.valid {
			t.Fatalf("composer face %d -> %v, wanted valid=%v", c.size, err, c.valid)
		}
	}
	if defaultPreferences().ComposerFontSize != preferenceDefaultComposerFontSize {
		t.Fatalf("the defaults record does not carry the default composer face: %+v", defaultPreferences())
	}
	if !validComposerFontSize(preferenceDefaultComposerFontSize) {
		t.Fatal("the default composer face is outside the range the schema admits")
	}
}

// The wire: a PUT that states the face stores it; a PUT from a browser that
// predates the field is NOT refused — the field is a plain number with a
// default, so an absent key is that default. Refusing would cost that operator
// every other preference over a field they have never heard of.
func TestPreferencesAPIComposerFontSize(t *testing.T) {
	cfg := ergoFrontConfig(t)
	server := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if server.preferencesErr != nil {
		t.Fatal(server.preferencesErr)
	}
	put := func(etag, body string) *httptest.ResponseRecorder {
		t.Helper()
		return ergoRequest(t, server, http.MethodPut, "http://localhost/api/preferences", "application/json", strings.NewReader(body), func(r *http.Request) {
			r.Header.Set("If-Match", etag)
		})
	}

	stated := put(`"0"`, `{"version":1,"theme":"dracula","font_size":null,"composer_font_size":18,"default_session":null}`)
	if stated.Code != http.StatusOK {
		t.Fatalf("a PUT stating the composer face was refused: %d %s", stated.Code, stated.Body.String())
	}
	if got := decodePreferences(t, stated); got.ComposerFontSize != 18 {
		t.Fatalf("the stored composer face came back as %d", got.ComposerFontSize)
	}

	absent := put(`"1"`, `{"version":1,"theme":"dracula","font_size":null,"default_session":null}`)
	if absent.Code != http.StatusOK {
		t.Fatalf("a PUT from a browser that predates the field was refused: %d %s", absent.Code, absent.Body.String())
	}
	if got := decodePreferences(t, absent); got.ComposerFontSize != preferenceDefaultComposerFontSize {
		t.Fatalf("an absent composer face stored %d, want the default %d", got.ComposerFontSize, preferenceDefaultComposerFontSize)
	}

	// A GET states it too: the record is closed, so a client that requires the
	// key finds it.
	read := ergoRequest(t, server, http.MethodGet, "http://localhost/api/preferences", "", nil, nil)
	if read.Code != http.StatusOK || decodePreferences(t, read).ComposerFontSize != preferenceDefaultComposerFontSize {
		t.Fatalf("GET did not state the composer face: %d %s", read.Code, read.Body.String())
	}

	for name, body := range map[string]string{
		"below the floor": `{"version":1,"theme":"dracula","font_size":null,"composer_font_size":8,"default_session":null}`,
		"above the top":   `{"version":1,"theme":"dracula","font_size":null,"composer_font_size":25,"default_session":null}`,
		"not a number":    `{"version":1,"theme":"dracula","font_size":null,"composer_font_size":"14","default_session":null}`,
		"not an integer":  `{"version":1,"theme":"dracula","font_size":null,"composer_font_size":14.5,"default_session":null}`,
		"explicitly null": `{"version":1,"theme":"dracula","font_size":null,"composer_font_size":null,"default_session":null}`,
		"a nested object": `{"version":1,"theme":"dracula","font_size":null,"composer_font_size":{"px":14},"default_session":null}`,
		"a boolean":       `{"version":1,"theme":"dracula","font_size":null,"composer_font_size":true,"default_session":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := put(`"2"`, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("%s was accepted: %d %s", name, recorder.Code, recorder.Body.String())
			}
		})
	}
}
