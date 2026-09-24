package frontdoor

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

const preferencesSecret = "SENTINEL-theme-9c1f"

func decodePreferences(t *testing.T, w *httptest.ResponseRecorder) preferencesResponse {
	t.Helper()
	var got preferencesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return got
}

func preferencesBody(theme string, size int, session string) string {
	return `{"version":1,"theme":"` + theme + `","font_size":` + strconv.Itoa(size) + `,"default_session":` + session + `}`
}

func TestPreferencesAPIContract(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.preferencesErr != nil {
		t.Fatal(s.preferencesErr)
	}
	get := func() *httptest.ResponseRecorder {
		return ergoRequest(t, s, http.MethodGet, "http://localhost/api/preferences", "", nil, nil)
	}
	put := func(etag, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
		return ergoRequest(t, s, http.MethodPut, "http://localhost/api/preferences", "application/json", strings.NewReader(body), func(r *http.Request) {
			if etag != "" {
				r.Header.Set("If-Match", etag)
			}
			if mutate != nil {
				mutate(r)
			}
		})
	}

	defaults := get()
	if defaults.Code != http.StatusOK || defaults.Header().Get("Cache-Control") != "no-store" || defaults.Header().Get("ETag") != `"0"` {
		t.Fatalf("defaults=%d headers=%v body=%s", defaults.Code, defaults.Header(), defaults.Body.String())
	}
	if got := decodePreferences(t, defaults); got.Stored || !got.Available || got.Revision != 0 || got.Theme != "default" || got.FontSize != nil || got.DefaultSession != nil || got.Version != 1 {
		t.Fatalf("defaults body=%+v", got)
	}

	created := put(`"0"`, preferencesBody("dracula", 16, `{"realm":"desk-a7","server":"primary","name":"work"}`), nil)
	if created.Code != http.StatusOK || created.Header().Get("ETag") != `"1"` || created.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create=%d etag=%q body=%s", created.Code, created.Header().Get("ETag"), created.Body.String())
	}
	if got := decodePreferences(t, created); !got.Stored || !got.Available || got.Revision != 1 || got.Theme != "dracula" || !fontIs(got.FontSize, 16) || got.DefaultSession == nil || got.DefaultSession.Realm != "desk-a7" {
		t.Fatalf("created body=%+v", got)
	}
	if got := decodePreferences(t, get()); !got.Stored || got.Revision != 1 || got.Theme != "dracula" {
		t.Fatalf("stored GET=%+v", got)
	}

	// TF2 server half: a stale PUT is refused with the current record so the
	// page can re-read and re-apply.
	stale := put(`"0"`, preferencesBody("one-dark", 12, "null"), nil)
	if stale.Code != http.StatusPreconditionFailed || stale.Header().Get("ETag") != `"1"` || stale.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("stale=%d etag=%q body=%s", stale.Code, stale.Header().Get("ETag"), stale.Body.String())
	}
	if got := decodePreferences(t, stale); got.Revision != 1 || got.Theme != "dracula" || !got.Stored {
		t.Fatalf("stale body=%+v", got)
	}
	missing := put("", preferencesBody("one-dark", 12, "null"), nil)
	if missing.Code != http.StatusPreconditionFailed || decodePreferences(t, missing).Revision != 1 {
		t.Fatalf("missing If-Match=%d body=%s", missing.Code, missing.Body.String())
	}
	for _, etag := range []string{`1`, `"01"`, `"-1"`, `"x"`, `""`, `*`, `W/"1"`} {
		if w := put(etag, preferencesBody("one-dark", 12, "null"), nil); w.Code != http.StatusPreconditionFailed {
			t.Fatalf("If-Match %q=%d", etag, w.Code)
		}
	}
	updated := put(`"1"`, preferencesBody("one-dark", 12, "null"), nil)
	if updated.Code != http.StatusOK || decodePreferences(t, updated).Revision != 2 || decodePreferences(t, updated).DefaultSession != nil {
		t.Fatalf("update=%d body=%s", updated.Code, updated.Body.String())
	}

	// Route refusal cases: each clause has a refusal with fixed bounded text,
	// no-store, and no echo of the (secret-bearing) request.
	validBody := preferencesBody("gruvbox-dark", 18, "null")
	for _, tc := range ergoMutationRedCases(cfg) {
		w := put(`"2"`, strings.Replace(validBody, "gruvbox-dark", preferencesSecret, 1), tc.mutate)
		assertBoundedRefusal(t, tc.name, w, tc.status, preferencesSecret)
	}
	for _, tc := range ergoIdentityRedCases(cfg) {
		w := ergoRequest(t, s, http.MethodGet, "http://localhost/api/preferences", "", nil, tc.mutate)
		assertBoundedRefusal(t, "GET "+tc.name, w, tc.status)
	}
	for _, method := range []string{http.MethodPatch, http.MethodPost, http.MethodDelete} {
		w := ergoRequest(t, s, method, "http://localhost/api/preferences", "application/json", strings.NewReader(validBody), func(r *http.Request) { r.Header.Set("If-Match", `"2"`) })
		assertBoundedRefusal(t, method, w, http.StatusMethodNotAllowed, "gruvbox")
	}
	if w := ergoRequest(t, s, http.MethodHead, "http://localhost/api/preferences", "", nil, nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD=%d", w.Code)
	}
	for name, body := range map[string]string{
		"unknown_field":           `{"version":1,"theme":"` + preferencesSecret + `","font_size":14,"default_session":null,"extra":true}`,
		"revision_in_body":        `{"version":1,"theme":"` + preferencesSecret + `","font_size":14,"default_session":null,"revision":2}`,
		"trailing_garbage":        validBody + ` {"theme":"` + preferencesSecret + `"}`,
		"trailing_scalar":         validBody + ` 1`,
		"duplicate_key":           `{"version":1,"theme":"default","theme":"` + preferencesSecret + `","font_size":14,"default_session":null}`,
		"folded_duplicate_key":    `{"version":1,"theme":"default","THEME":"` + preferencesSecret + `","font_size":14,"default_session":null}`,
		"missing_default_session": `{"version":1,"theme":"default","font_size":14}`,
		"missing_theme":           `{"version":1,"font_size":14,"default_session":null}`,
		"unknown_theme":           preferencesBody(preferencesSecret, 14, "null"),
		"font_float":              `{"version":1,"theme":"default","font_size":14.0,"default_session":null}`,
		"font_string":             `{"version":1,"theme":"default","font_size":"14","default_session":null}`,
		"font_bool":               `{"version":1,"theme":"default","font_size":true,"default_session":null}`,
		"font_object":             `{"version":1,"theme":"default","font_size":{"px":14},"default_session":null}`,
		"missing_font":            `{"version":1,"theme":"default","default_session":null}`,
		"font_too_small":          preferencesBody("default", 8, "null"),
		"font_too_large":          preferencesBody("default", 25, "null"),
		"wrong_version":           `{"version":2,"theme":"default","font_size":14,"default_session":null}`,
		"session_unknown_field":   preferencesBody("default", 14, `{"realm":"r","server":"s","name":"`+preferencesSecret+`","extra":1}`),
		"session_missing_name":    preferencesBody("default", 14, `{"realm":"r","server":"s"}`),
		"session_bad_realm":       preferencesBody("default", 14, `{"realm":"bad realm","server":"s","name":"`+preferencesSecret+`"}`),
		"session_control_name":    preferencesBody("default", 14, `{"realm":"r","server":"s","name":"a\tb"}`),
		"session_scalar":          preferencesBody("default", 14, `"`+preferencesSecret+`"`),
		"session_array":           preferencesBody("default", 14, `[]`),
		"array_top_level":         `[]`,
		"scalar_top_level":        `"` + preferencesSecret + `"`,
		"null_top_level":          `null`,
		"empty":                   ``,
		"not_json":                preferencesSecret,
		"invalid_utf8":            `{"version":1,"theme":"` + string([]byte{0xff}) + `","font_size":14,"default_session":null}`,
	} {
		w := put(`"2"`, body, nil)
		assertBoundedRefusal(t, name, w, http.StatusBadRequest, preferencesSecret)
	}
	oversize := put(`"2"`, `{"version":1,"theme":"`+strings.Repeat("a", preferencesRequestMaxBytes)+`","font_size":14,"default_session":null}`, nil)
	assertBoundedRefusal(t, "oversize", oversize, http.StatusRequestEntityTooLarge, "aaaaaaaa")
	if got := decodePreferences(t, get()); got.Revision != 2 || got.Theme != "one-dark" {
		t.Fatalf("refusals mutated the record: %+v", got)
	}
	// A body inside the limit is accepted: the bound is the reader, not a
	// smaller hidden one.
	within := put(`"2"`, `{"version":1,"theme":"default","font_size":14,"default_session":{"realm":"r","server":"s","name":"`+strings.Repeat("n", 100)+`"}}`, nil)
	if within.Code != http.StatusOK {
		t.Fatalf("within-limit body=%d %s", within.Code, within.Body.String())
	}
}

// TF3 server half: an unconfigured, unopenable or faulted store never hides
// behind a fresh-operator answer.
func TestPreferencesAPIStoreUnavailable(t *testing.T) {
	t.Run("unconfigured", func(t *testing.T) {
		cfg := ergoFrontConfig(t)
		cfg.PreferencesStorePath = ""
		s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
		if !errors.Is(s.preferencesErr, errPreferencesStoreUnavailable) {
			t.Fatalf("unconfigured store error=%v", s.preferencesErr)
		}
		assertPreferencesUnavailable(t, s, "default", nil)
	})
	t.Run("corrupt_file", func(t *testing.T) {
		cfg := ergoFrontConfig(t)
		if err := os.WriteFile(cfg.PreferencesStorePath, []byte(`{"version":1,"operators":[],"operators":[]}`), 0600); err != nil {
			t.Fatal(err)
		}
		s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
		if s.preferencesErr == nil {
			t.Fatal("duplicate-key store accepted")
		}
		assertPreferencesUnavailable(t, s, "default", nil)
	})
	t.Run("unknown_version", func(t *testing.T) {
		cfg := ergoFrontConfig(t)
		if err := os.WriteFile(cfg.PreferencesStorePath, []byte(`{"version":7,"operators":[]}`), 0600); err != nil {
			t.Fatal(err)
		}
		s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
		if s.preferencesErr == nil {
			t.Fatal("unknown version accepted")
		}
		assertPreferencesUnavailable(t, s, "default", nil)
	})
	t.Run("faulted_after_publication", func(t *testing.T) {
		cfg := ergoFrontConfig(t)
		s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
		if s.preferencesErr != nil {
			t.Fatal(s.preferencesErr)
		}
		s.preferences.file.dirSync = func(*os.File) error { return errors.New("injected directory sync") }
		w := ergoRequest(t, s, http.MethodPut, "http://localhost/api/preferences", "application/json", strings.NewReader(preferencesBody("dracula", 16, "null")), func(r *http.Request) { r.Header.Set("If-Match", `"0"`) })
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("faulted put=%d %s", w.Code, w.Body.String())
		}
		// The bytes were published, so a GET reports them, and reports that
		// the store can no longer accept writes.
		assertPreferencesUnavailable(t, s, "dracula", fontPtr(16))
	})
}

func assertPreferencesUnavailable(t *testing.T, s *Server, theme string, size *int) {
	t.Helper()
	get := ergoRequest(t, s, http.MethodGet, "http://localhost/api/preferences", "", nil, nil)
	if get.Code != http.StatusOK || get.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET=%d %s", get.Code, get.Body.String())
	}
	if got := decodePreferences(t, get); got.Available || got.Theme != theme || !fontEqual(got.FontSize, size) {
		t.Fatalf("GET body=%+v", got)
	}
	put := ergoRequest(t, s, http.MethodPut, "http://localhost/api/preferences", "application/json", strings.NewReader(preferencesBody("one-dark", 12, "null")), func(r *http.Request) { r.Header.Set("If-Match", `"0"`) })
	assertBoundedRefusal(t, "PUT unavailable", put, http.StatusServiceUnavailable, "one-dark")
	if !bytes.Equal(bytes.TrimSpace(put.Body.Bytes()), []byte("preferences store unavailable")) {
		t.Fatalf("unavailable text=%q", put.Body.String())
	}
}

// The ergonomics routes sit behind the same operator limiter as every other
// ingress route; they add no budget of their own.
func TestPreferencesAPISharesOperatorLimiter(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	limited := 0
	for i := 0; i < 60; i++ {
		if w := ergoRequestMetered(t, s, http.MethodGet, "http://localhost/api/preferences", "", nil, nil); w.Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("preferences route bypassed the shared operator limiter")
	}
}
