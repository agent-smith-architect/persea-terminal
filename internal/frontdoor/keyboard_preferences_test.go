package frontdoor

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"persea-terminal/internal/config"
)

func keyboardFrontConfig(t *testing.T) config.Front {
	t.Helper()
	cfg := ergoFrontConfig(t)
	cfg.KeyboardPreferencesStorePath = filepath.Join(filepath.Dir(cfg.PreferencesStorePath), "keyboard-v1.json")
	return cfg
}

const keyboardTestBody = `{"version":1,"layout":{"bar":["modifier:ctrl","key:escape:0"],"favorites":["sequence:My%20tmux:key:%3A:7","key:A:7"]},"prefixes":{"My tmux":"key:b:1"}}`

func keyboardRequest(t *testing.T, s *Server, method, body, etag string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	return ergoRequest(t, s, method, "http://localhost/api/keyboard-preferences", "application/json", strings.NewReader(body), func(r *http.Request) {
		if etag != "" {
			r.Header.Set("If-Match", etag)
		}
		if mutate != nil {
			mutate(r)
		}
	})
}

func decodeKeyboard(t *testing.T, w *httptest.ResponseRecorder) keyboardPreferencesResponse {
	t.Helper()
	var got keyboardPreferencesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return got
}

func TestKeyboardPreferencesAPIContract(t *testing.T) {
	cfg := keyboardFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.keyboardPreferencesErr != nil {
		t.Fatal(s.keyboardPreferencesErr)
	}
	get := keyboardRequest(t, s, http.MethodGet, "", "", nil)
	got := decodeKeyboard(t, get)
	if get.Code != 200 || get.Header().Get("ETag") != `"0"` || get.Header().Get("Cache-Control") != "no-store" || !got.Available || got.Stored || !reflect.DeepEqual(got.KeyboardPreferenceRecord, defaultKeyboardPreferences()) {
		t.Fatalf("defaults: %+v %v", got, get.Header())
	}
	created := keyboardRequest(t, s, http.MethodPut, keyboardTestBody, `"0"`, nil)
	if created.Code != 200 || created.Header().Get("ETag") != `"1"` || !decodeKeyboard(t, created).Stored {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	for _, etag := range []string{"", `"0"`, `1`, `"01"`, `*`, `W/"1"`, `"-1"`} {
		w := keyboardRequest(t, s, http.MethodPut, keyboardTestBody, etag, nil)
		if w.Code != 412 || decodeKeyboard(t, w).Revision != 1 || w.Header().Get("ETag") != `"1"` {
			t.Fatalf("precondition %q: %d %s", etag, w.Code, w.Body)
		}
	}
	for _, tc := range ergoMutationRedCases(cfg) {
		w := keyboardRequest(t, s, http.MethodPut, keyboardTestBody, `"1"`, tc.mutate)
		assertBoundedRefusal(t, tc.name, w, tc.status, "My tmux")
	}
	for _, tc := range ergoIdentityRedCases(cfg) {
		w := keyboardRequest(t, s, http.MethodGet, "", "", tc.mutate)
		assertBoundedRefusal(t, tc.name, w, tc.status)
	}
	for _, method := range []string{"HEAD", "PATCH", "POST", "DELETE"} {
		assertBoundedRefusal(t, method, keyboardRequest(t, s, method, keyboardTestBody, `"1"`, nil), 405)
	}
	for name, body := range map[string]string{
		"missing layout":          `{"version":1,"prefixes":{}}`,
		"missing bar":             `{"version":1,"layout":{"favorites":[]},"prefixes":{}}`,
		"null favorites":          `{"version":1,"layout":{"bar":[],"favorites":null},"prefixes":{}}`,
		"null prefixes":           `{"version":1,"layout":{"bar":[],"favorites":[]},"prefixes":null}`,
		"wrong version":           strings.Replace(keyboardTestBody, `"version":1`, `"version":2`, 1),
		"float version":           strings.Replace(keyboardTestBody, `"version":1`, `"version":1.0`, 1),
		"uppercase structural":    strings.Replace(keyboardTestBody, `"layout"`, `"LAYOUT"`, 1),
		"unknown nested":          strings.Replace(keyboardTestBody, `"bar":`, `"extra":1,"bar":`, 1),
		"response field":          strings.Replace(keyboardTestBody, `"version":1`, `"version":1,"revision":1`, 1),
		"duplicate structural":    strings.Replace(keyboardTestBody, `"version":1`, `"version":1,"version":1`, 1),
		"duplicate prefix":        strings.Replace(keyboardTestBody, `"My tmux":"key:b:1"`, `"My tmux":"key:b:1","My tmux":"key:a:1"`, 1),
		"folded duplicate prefix": strings.Replace(keyboardTestBody, `"My tmux":"key:b:1"`, `"My tmux":"key:b:1","my tmux":"key:a:1"`, 1),
		"prototype prefix":        strings.Replace(keyboardTestBody, `"My tmux":`, `"constructor":`, 1),
		"prefix null":             strings.Replace(keyboardTestBody, `"My tmux":"key:b:1"`, `"My tmux":null`, 1),
		"trailing":                keyboardTestBody + ` {}`,
		"invalid utf8":            strings.Replace(keyboardTestBody, "My tmux", string([]byte{0xff}), 1),
		"array":                   `[]`, "null": `null`, "empty": ``,
	} {
		assertBoundedRefusal(t, name, keyboardRequest(t, s, http.MethodPut, body, `"1"`, nil), 400, "My tmux")
	}
	assertBoundedRefusal(t, "oversize", keyboardRequest(t, s, http.MethodPut, strings.Repeat(" ", keyboardPreferencesRequestMaxBytes)+keyboardTestBody, `"1"`, nil), 413)
	if got := decodeKeyboard(t, keyboardRequest(t, s, http.MethodGet, "", "", nil)); got.Revision != 1 {
		t.Fatal("refusal mutated record")
	}
	// Empty arrays and an unknown-prefix sequence are valid persistent intent.
	body := `{"version":1,"layout":{"bar":[],"favorites":["sequence:Missing:key:n:0"]},"prefixes":{}}`
	w := keyboardRequest(t, s, http.MethodPut, body, `"1"`, nil)
	if w.Code != 200 || decodeKeyboard(t, w).Revision != 2 {
		t.Fatalf("empty/missing prefix: %d %s", w.Code, w.Body)
	}
}

func TestKeyboardActionValidation(t *testing.T) {
	for _, id := range []string{"key:escape:7", "key:insert:7", "key:A:7", "key:%20:0", "key:%2B:0", "key:%3A:0", "key:!:0", "key:':0", "key:f12:7", "sequence:My%20tmux:key:%5B:7"} {
		if !validKeyboardActionID(id, false) {
			t.Errorf("refused structural action %q", id)
		}
	}
	for _, id := range []string{"key:escape:8", "key:a:01", "key:%61:0", "key:%2b:0", "key:+:0", "key: :0", "key:%C3%A9:0", "key:%00:0", "key:f13:0", "key:Escape:0", "key:ab:0", "key:a:0:0", "sequence:My+tmux:key:a:0", "sequence:constructor:key:a:0", "sequence:a:sequence:a:key:a:0", "sequence:a:modifier:ctrl", "tmux:n", "local:copy"} {
		if validKeyboardActionID(id, true) {
			t.Errorf("accepted invalid action %q", id)
		}
	}
	for _, id := range []string{"modifier:ctrl", "modifier:alt", "modifier:shift"} {
		p := defaultKeyboardPreferences().KeyboardPreferences
		p.Layout.Favorites = []string{id}
		if err := validateKeyboardPreferences(p); err != nil {
			t.Errorf("favorite modifier %q: %v", id, err)
		}
		if !validKeyboardActionID(id, true) || validKeyboardActionID(id, false) || validKeyboardKeyID(id) {
			t.Errorf("modifier context %q", id)
		}
	}
	for name, mutate := range map[string]func(*KeyboardPreferences){
		"duplicate bar":    func(p *KeyboardPreferences) { p.Layout.Bar = []string{"key:a:0", "key:a:0"} },
		"too many":         func(p *KeyboardPreferences) { p.Layout.Favorites = make([]string, 101) },
		"unknown modifier": func(p *KeyboardPreferences) { p.Layout.Favorites = []string{"modifier:meta"} },
		"sequence prefix":  func(p *KeyboardPreferences) { p.Prefixes["x"] = "sequence:x:key:a:0" },
		"long name":        func(p *KeyboardPreferences) { p.Prefixes[strings.Repeat("a", 33)] = "key:a:0" },
		"many prefixes": func(p *KeyboardPreferences) {
			for i := 0; i < 21; i++ {
				p.Prefixes[string(rune('A'+i))] = "key:a:0"
			}
		},
		"folded prefix": func(p *KeyboardPreferences) { p.Prefixes["TMUX"] = "key:a:0" },
	} {
		p := defaultKeyboardPreferences().KeyboardPreferences
		mutate(&p)
		if validateKeyboardPreferences(p) == nil {
			t.Errorf("accepted %s", name)
		}
	}
}

func TestKeyboardPreferencesDurabilityIsolationAndCAS(t *testing.T) {
	cfg := keyboardFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	before, err := os.ReadFile(cfg.PreferencesStorePath)
	if err != nil {
		t.Fatal(err)
	}
	p := defaultKeyboardPreferences().KeyboardPreferences
	p.Layout.Bar = []string{}
	p.Prefixes = map[string]string{"My tmux": "key:b:1"}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.keyboardPreferences.put(cfg.Ingress.OperatorLogin, p, 0)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	saved, conflicts := 0, 0
	for err := range results {
		if err == nil {
			saved++
		} else if errors.Is(err, errKeyboardPreferencesConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if saved != 1 || conflicts != 1 {
		t.Fatalf("CAS: saved=%d conflicts=%d", saved, conflicts)
	}
	p.Prefixes["My tmux"] = "key:a:0"
	reopened, err := newKeyboardPreferencesStore(cfg.KeyboardPreferencesStorePath)
	if err != nil {
		t.Fatal(err)
	}
	got, available := reopened.get(cfg.Ingress.OperatorLogin)
	if !available || got.Revision != 1 || got.Prefixes["My tmux"] != "key:b:1" || got.Layout.Bar == nil || len(got.Layout.Bar) != 0 {
		t.Fatalf("reload: %+v", got)
	}
	got.Prefixes["My tmux"] = "key:z:0"
	again, _ := reopened.get(cfg.Ingress.OperatorLogin)
	if again.Prefixes["My tmux"] != "key:b:1" {
		t.Fatal("GET exposed mutable state")
	}
	after, err := os.ReadFile(cfg.PreferencesStorePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("keyboard write changed appearance store")
	}
	if _, err := newPreferencesStore(cfg.PreferencesStorePath); err != nil {
		t.Fatalf("existing schema reload after keyboard write: %v", err)
	}
	info, err := os.Stat(cfg.KeyboardPreferencesStorePath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("file mode: %v %v", info, err)
	}
}

func TestKeyboardPreferencesUnavailableAndFaults(t *testing.T) {
	for _, kind := range []string{"unconfigured", "corrupt", "before publication", "after publication"} {
		t.Run(kind, func(t *testing.T) {
			cfg := keyboardFrontConfig(t)
			if kind == "unconfigured" {
				cfg.KeyboardPreferencesStorePath = ""
			}
			if kind == "corrupt" {
				if err := os.WriteFile(cfg.KeyboardPreferencesStorePath, []byte(`{"version":1,"operators":[],"extra":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
			if kind == "before publication" {
				s.keyboardPreferences.file.fileSync = func(*os.File) error { return errors.New("injected") }
			}
			if kind == "after publication" {
				s.keyboardPreferences.file.dirSync = func(*os.File) error { return errors.New("injected") }
			}
			w := keyboardRequest(t, s, http.MethodPut, keyboardTestBody, `"0"`, nil)
			assertBoundedRefusal(t, kind, w, 503, "My tmux")
			got := decodeKeyboard(t, keyboardRequest(t, s, http.MethodGet, "", "", nil))
			if got.Available != (kind == "before publication") {
				t.Fatalf("availability: %+v", got)
			}
			wantRevision := uint64(0)
			if kind == "after publication" {
				wantRevision = 1
			}
			if got.Revision != wantRevision {
				t.Fatalf("revision: %+v", got)
			}
			if kind == "before publication" || kind == "after publication" {
				reopened, err := newKeyboardPreferencesStore(cfg.KeyboardPreferencesStorePath)
				if err != nil {
					t.Fatal(err)
				}
				record, _ := reopened.get(cfg.Ingress.OperatorLogin)
				if record.Revision != wantRevision {
					t.Fatalf("disk revision=%d want=%d", record.Revision, wantRevision)
				}
			}
		})
	}
}
