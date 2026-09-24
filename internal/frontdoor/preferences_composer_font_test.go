package frontdoor

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// the composer's face is an operator preference. Unlike the terminal
// font it is a PLAIN NUMBER with a default, not a tri-state — the composer has
// no auto-fit to hand the decision back to, so "auto" would mean nothing.
//
// Two compatibility directions are at stake and both are pinned here:
//
//   - forward: a record written before the field existed carries no key at all
//     and must read as the default, not as a zero the range check then rejects;
//   - backward: a release without configurable font sizes loads this file with
//     DisallowUnknownFields, so the new key makes it reject the WHOLE store —
//     which is what deploy/preferences-store-downgrade.sh now repairs.

// preComposerFontEntry is the record shape before configurable composer fonts: the nullable terminal font
// tri-state, and no composer face. loadPreComposerFontStore is that release's
// load path, including the strict decode that is the whole hazard.
type preComposerFontEntry struct {
	Operator       string          `json:"operator"`
	Theme          string          `json:"theme"`
	FontSize       *int            `json:"font_size"`
	DefaultSession *DefaultSession `json:"default_session"`
	Revision       uint64          `json:"revision"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type preComposerFontStoreFile struct {
	Version   int                    `json:"version"`
	Operators []preComposerFontEntry `json:"operators"`
}

func loadPreComposerFontStore(b []byte) error {
	var f preComposerFontStoreFile
	if err := decodeStrict(b, &f); err != nil {
		return errors.New("invalid preferences store")
	}
	if f.Version != preferencesStoreVersion || len(f.Operators) > preferencesStoreMaxRecords {
		return errors.New("unsupported or oversized preferences store")
	}
	for _, e := range f.Operators {
		if !validOperatorLogin(e.Operator) || e.Revision == 0 || e.CreatedAt.IsZero() || e.UpdatedAt.Before(e.CreatedAt) {
			return errors.New("invalid preferences record")
		}
		if !preferenceThemes[e.Theme] || !validFontSize(e.FontSize) || !validDefaultSession(e.DefaultSession) {
			return errors.New("invalid preferences record")
		}
	}
	return nil
}

// A record in the pre-C1 shape loads, and its absent composer face reads as the
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
		t.Fatalf("a record in the pre-C1 shape was refused: %v", err)
	}
	got := s.get("operator@example.com")
	if !got.Stored || got.Revision != 3 || got.Theme != "dracula" || !fontIs(got.FontSize, 16) {
		t.Fatalf("pre-C1 record lost fidelity: %+v", got)
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
		t.Fatalf("write over a pre-C1 record: %+v err=%v", next, err)
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

// The backward direction, end to end. A store this release wrote is REJECTED by
// a pre-C1 loader because of the new key, and the shipped downgrade script is
// what makes it readable again.
func TestPreferencesStoreDowngradeStripsComposerFont(t *testing.T) {
	run, path := downgradeScriptHarness(t)

	s, err := newPreferencesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.put("a@example.com", preferencesFixture("dracula", 16), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.put("b@example.com", preferencesFixture("one-dark", 12), 0); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"composer_font_size"`) {
		t.Fatalf("this release did not write the composer face at all: %s", raw)
	}
	// The blast radius itself: the key alone is enough to lose the whole file.
	if err := loadPreComposerFontStore(raw); err == nil {
		t.Fatal("a pre-C1 release accepted a store carrying composer_font_size; the downgrade would be pointless")
	}

	if out := run("--dry-run", path); !strings.Contains(out, "remove composer_font_size from 2 record(s)") {
		t.Fatalf("the dry run did not report the pending removal: %s", out)
	}
	if unchanged, err := os.ReadFile(path); err != nil || string(unchanged) != string(raw) {
		t.Fatalf("the dry run modified the store: err=%v", err)
	}

	out := run(path)
	if !strings.Contains(out, "composer_font_size removed from: 2 record(s)") {
		t.Fatalf("the summary did not report the removal: %s", out)
	}
	downgraded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(downgraded), "composer_font_size") {
		t.Fatalf("the key survived the downgrade: %s", downgraded)
	}
	if err := loadPreComposerFontStore(downgraded); err != nil {
		t.Fatalf("a pre-C1 release still rejects the downgraded store: %v", err)
	}
	// And the older-still release, whose font clause is not a tri-state.
	if err := loadLegacyPreferencesStore(downgraded); err != nil {
		t.Fatalf("a release without configurable fonts rejects the downgraded store: %v", err)
	}

	// Rolling forward: this release reads the repaired file, and the operators
	// it stripped read the default face again. That is the stated cost.
	reopened, err := newPreferencesStore(path)
	if err != nil {
		t.Fatalf("the current release rejects the downgraded store: %v", err)
	}
	for _, operator := range []string{"a@example.com", "b@example.com"} {
		if got := reopened.get(operator); got.ComposerFontSize != preferenceDefaultComposerFontSize {
			t.Fatalf("%s did not read the default face after the downgrade: %+v", operator, got)
		}
	}
	if got := reopened.get("a@example.com"); got.Theme != "dracula" || !fontIs(got.FontSize, 16) {
		t.Fatalf("the downgrade disturbed an unrelated field: %+v", got)
	}

	// Idempotent: a second run has nothing left to do.
	if out := run(path); !strings.Contains(out, "no change") {
		t.Fatalf("a second run was not a no-op: %s", out)
	}
}
