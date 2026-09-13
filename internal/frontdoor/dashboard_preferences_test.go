package frontdoor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func dashboardTestScope(id int) string {
	return fmt.Sprintf(`["local","private","socket_path","/tmp/test.sock","boot","$%d",1000,42,100,%d]`, id, id+200)
}

func dashboardRequest(t *testing.T, s *Server, method, body, etag string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	return ergoRequest(t, s, method, "http://localhost/api/dashboard-preferences", "application/json", strings.NewReader(body), func(r *http.Request) {
		if etag != "" {
			r.Header.Set("If-Match", etag)
		}
		if mutate != nil {
			mutate(r)
		}
	})
}

func TestDashboardPreferencesDurabilityCASAndIsolation(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "appearance.json")
	store, err := openDashboardPreferencesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	initial, available := store.get("operator")
	if !available || initial.Revision != 0 || initial.Favorites == nil || len(initial.Favorites) != 0 {
		t.Fatalf("initial: %+v", initial)
	}
	first := dashboardPreferences{Version: 1, Favorites: []string{dashboardTestScope(1)}}
	saved, err := store.put("operator", first, 0)
	if err != nil || saved.Revision != 1 {
		t.Fatalf("save: %+v %v", saved, err)
	}
	first.Favorites[0] = dashboardTestScope(9)
	saved.Favorites[0] = dashboardTestScope(8)
	current, _ := store.get("operator")
	if current.Favorites[0] != dashboardTestScope(1) {
		t.Fatal("caller mutated durable state through a slice")
	}
	stale, err := store.put("operator", dashboardPreferences{Version: 1, Favorites: []string{dashboardTestScope(2)}}, 0)
	if !errors.Is(err, errDashboardPreferencesConflict) || !reflect.DeepEqual(stale, current) {
		t.Fatal("stale device overwrote favorites")
	}
	other, _ := store.get("another-operator")
	if len(other.Favorites) != 0 {
		t.Fatal("operator isolation failed")
	}
	restarted, err := openDashboardPreferencesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, available := restarted.get("operator")
	if !available || !reflect.DeepEqual(current, got) {
		t.Fatal("favorites did not survive restart")
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(path), "dashboard-preferences.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("store permissions: %v %v", info, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("appearance store was modified")
	}
	// Persistence failure cannot publish an in-memory favorite that will vanish.
	_ = store.file.fs.(rootAliasFS).root.Close()
	if _, err := store.put("operator", dashboardPreferences{Version: 1, Favorites: []string{}}, 1); err == nil {
		t.Fatal("closed store accepted a save")
	}
	after, _ := store.get("operator")
	if !reflect.DeepEqual(after, current) {
		t.Fatal("failed save changed current state")
	}
}

func TestDashboardPreferencesAPIContract(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.dashboardPreferencesErr != nil {
		t.Fatal(s.dashboardPreferencesErr)
	}
	get := dashboardRequest(t, s, "GET", "", "", nil)
	if get.Code != 200 || get.Header().Get("ETag") != `"0"` || get.Header().Get("Cache-Control") != "no-store" || !strings.Contains(get.Body.String(), `"available":true`) {
		t.Fatalf("initial: %d %s", get.Code, get.Body)
	}
	prefs := dashboardPreferences{Version: 1, Favorites: []string{dashboardTestScope(1)}}
	body, _ := json.Marshal(prefs)
	put := dashboardRequest(t, s, "PUT", string(body), `"0"`, nil)
	if put.Code != 200 || put.Header().Get("ETag") != `"1"` {
		t.Fatalf("save: %d %s", put.Code, put.Body)
	}
	for _, etag := range []string{"", `"0"`, `1`, `"01"`, `*`, `W/"1"`, `"-1"`} {
		w := dashboardRequest(t, s, "PUT", string(body), etag, nil)
		if w.Code != 412 || w.Header().Get("ETag") != `"1"` {
			t.Fatalf("stale precondition %q: %d", etag, w.Code)
		}
	}
	for _, tc := range ergoMutationRedCases(cfg) {
		assertBoundedRefusal(t, tc.name, dashboardRequest(t, s, "PUT", string(body), `"1"`, tc.mutate), tc.status, "/tmp/test.sock")
	}
	for _, tc := range ergoIdentityRedCases(cfg) {
		assertBoundedRefusal(t, tc.name, dashboardRequest(t, s, "GET", "", "", tc.mutate), tc.status)
	}
	for _, method := range []string{"HEAD", "POST", "PATCH", "DELETE"} {
		assertBoundedRefusal(t, method, dashboardRequest(t, s, method, string(body), `"1"`, nil), 405)
	}
	for name, bad := range map[string]string{
		"missing": `{"version":1}`, "null favorites": `{"version":1,"favorites":null}`,
		"unknown": `{"version":1,"favorites":[],"command":"private"}`, "duplicate": `{"version":1,"version":1,"favorites":[]}`,
		"folded": `{"Version":1,"favorites":[]}`, "trailing": `{"version":1,"favorites":[]} {}`,
		"capability": `{"version":1,"favorites":["https://private/#handle=secret"]}`, "null": `null`,
	} {
		assertBoundedRefusal(t, name, dashboardRequest(t, s, "PUT", bad, `"1"`, nil), 400, "private", "secret")
	}
	assertBoundedRefusal(t, "oversize", dashboardRequest(t, s, "PUT", strings.Repeat(" ", 320<<10)+string(body), `"1"`, nil), 413)
	if got := dashboardRequest(t, s, "GET", "", "", nil); got.Header().Get("ETag") != `"1"` {
		t.Fatal("refusal mutated favorites")
	}
}

func TestDashboardPreferencesRejectsMalformedScopesAndStore(t *testing.T) {
	for _, bad := range []string{"", `[]`, dashboardTestScope(1) + "x", strings.Replace(dashboardTestScope(1), "1000", "null", 1), strings.Replace(dashboardTestScope(1), "1000", "-1", 1), strings.Replace(dashboardTestScope(1), "1000", "9007199254740992", 1)} {
		if validDashboardScope(bad) {
			t.Fatalf("accepted malformed scope: %q", bad)
		}
	}
	if validDashboardPreferences(dashboardPreferences{Version: 1, Favorites: []string{dashboardTestScope(1), dashboardTestScope(1)}}) {
		t.Fatal("duplicate favorite accepted")
	}
	tooMany := dashboardPreferences{Version: 1, Favorites: []string{}}
	for i := 0; i <= dashboardFavoritesLimit; i++ {
		tooMany.Favorites = append(tooMany.Favorites, dashboardTestScope(i))
	}
	if validDashboardPreferences(tooMany) {
		t.Fatal("favorite count exceeded")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dashboard-preferences.json"), []byte(`{"version":1,"operators":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openDashboardPreferencesStore(filepath.Join(dir, "appearance.json")); err == nil {
		t.Fatal("corrupt durable store silently reset")
	}
}
