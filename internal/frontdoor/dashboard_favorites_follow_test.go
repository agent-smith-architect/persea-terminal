package frontdoor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"persea-terminal/internal/broker"
	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func favoriteAuthority(sessionID string, serverPID int, created int64) proto.Authority {
	return proto.Authority{Realm: "local", Server: "private", UID: 1000, SelectorKind: "socket_path", SelectorValue: "/tmp/test.sock", BootID: "boot", ServerPID: serverPID, ServerStart: 100, SessionID: sessionID, SessionCreated: created}
}

func TestDashboardScopeMatchesThePageScope(t *testing.T) {
	scope, ok := dashboardScope(favoriteAuthority("$1", 42, 201))
	if !ok || scope != dashboardTestScope(1) {
		t.Fatalf("scope %q differs from the page's JSON.stringify form %q", scope, dashboardTestScope(1))
	}
	html := favoriteAuthority("$1", 42, 201)
	html.SelectorValue = "/tmp/a<b>&c.sock"
	if scope, ok := dashboardScope(html); !ok || !strings.Contains(scope, `"/tmp/a<b>&c.sock"`) {
		t.Fatalf("HTML characters must stay literal as in JSON.stringify: %q", scope)
	}
	separator := favoriteAuthority("$1", 42, 201)
	separator.SelectorValue = "/tmp/a .sock"
	if _, ok := dashboardScope(separator); ok {
		t.Fatal("a scope that JSON.stringify writes differently was accepted")
	}
}

func TestDashboardFavoritesFollowTheirSessionsLikeAliases(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "appearance.json")
	store, err := openDashboardPreferencesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first := favoriteAuthority("$1", 42, 201)
	firstScope, _ := dashboardScope(first)
	complete := map[string]bool{"local\x00private": true}
	saved, err := store.put("operator", dashboardPreferences{Version: 1, Favorites: []string{firstScope}}, 0, func(string) string { return "" })
	if err != nil || saved.Names != nil {
		t.Fatalf("save without a known name: %+v %v", saved, err)
	}
	revision, err := store.reconcile("operator", []aliasSession{{Authority: first, Name: "build"}}, complete)
	if err != nil || revision != 2 {
		t.Fatalf("live favorite did not record its name: revision=%d %v", revision, err)
	}
	if again, err := store.reconcile("operator", []aliasSession{{Authority: first, Name: "build"}}, complete); err != nil || again != 2 {
		t.Fatalf("an unchanged inventory wrote the store: revision=%d %v", again, err)
	}
	if renamed, _ := store.reconcile("operator", []aliasSession{{Authority: first, Name: "build-2"}}, complete); renamed != 3 {
		t.Fatalf("a tmux rename did not update the name: revision=%d", renamed)
	}
	restarted := favoriteAuthority("$4", 77, 900)
	restartedScope, _ := dashboardScope(restarted)
	live := []aliasSession{{Authority: restarted, Name: "build-2"}}
	if partial, _ := store.reconcile("operator", live, map[string]bool{}); partial != 3 {
		t.Fatal("a favorite moved on an incomplete session list")
	}
	moved, err := store.reconcile("operator", live, complete)
	if err != nil || moved != 4 {
		t.Fatalf("favorite did not follow the restart: revision=%d %v", moved, err)
	}
	reopened, err := openDashboardPreferencesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	record, _ := reopened.get("operator")
	if !reflect.DeepEqual(record.Favorites, []string{restartedScope}) || !reflect.DeepEqual(record.Names, map[string]string{restartedScope: "build-2"}) {
		t.Fatalf("persisted record: %+v", record)
	}

	// Two old favorites with one name: the newer moves, and the older, a dead
	// duplicate of a name a favorite now holds, is dropped — or it would take
	// the session back once the operator removed that star.
	oldA, oldB := favoriteAuthority("$1", 11, 301), favoriteAuthority("$2", 11, 302)
	scopeA, _ := dashboardScope(oldA)
	scopeB, _ := dashboardScope(oldB)
	names := map[string]string{scopeA: "shell", scopeB: "shell", restartedScope: "build-2"}
	if _, err := reopened.put("operator", dashboardPreferences{Version: 1, Favorites: []string{scopeA, restartedScope, scopeB}}, 4, func(scope string) string { return names[scope] }); err != nil {
		t.Fatal(err)
	}
	shell := favoriteAuthority("$9", 77, 950)
	shellScope, _ := dashboardScope(shell)
	live = []aliasSession{{Authority: restarted, Name: "build-2"}, {Authority: shell, Name: "shell"}}
	revision, err = reopened.reconcile("operator", live, complete)
	if err != nil {
		t.Fatal(err)
	}
	record, _ = reopened.get("operator")
	if !reflect.DeepEqual(record.Favorites, []string{restartedScope, shellScope}) || !reflect.DeepEqual(record.Names, map[string]string{restartedScope: "build-2", shellScope: "shell"}) {
		t.Fatalf("newest favorite must take the name and the older one go: %+v", record)
	}
	if _, err := reopened.put("operator", dashboardPreferences{Version: 1, Favorites: []string{restartedScope}}, revision, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.reconcile("operator", live, complete); err != nil {
		t.Fatal(err)
	}
	if again, _ := reopened.get("operator"); !reflect.DeepEqual(again.Favorites, []string{restartedScope}) {
		t.Fatalf("a removed star came back: %+v", again)
	}

	// A favorite saved while its session was live owns its name the same way:
	// an old favorite with that name is dropped, not left to reclaim it.
	if _, err := reopened.put("operator", dashboardPreferences{Version: 1, Favorites: []string{scopeA, shellScope}}, revision+1, func(scope string) string { return names[scope] }); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.reconcile("operator", live, complete); err != nil {
		t.Fatal(err)
	}
	if again, _ := reopened.get("operator"); !reflect.DeepEqual(again.Favorites, []string{shellScope}) {
		t.Fatalf("an old duplicate survived beside the live favorite: %+v", again)
	}
}

// A name in Go's JSON is the page's only when both write it the same way:
// literal text that merely looks like an escape is fine, actual line and
// paragraph separators are not.
func TestDashboardScopeRefusesOnlyRealSeparators(t *testing.T) {
	literal := favoriteAuthority("$1", 42, 201)
	literal.SelectorValue = `/tmp/literal\u2028.sock`
	if scope, ok := dashboardScope(literal); !ok || !strings.Contains(scope, `literal\\u2028`) {
		t.Fatalf("literal backslash-u text refused or changed: %q ok=%v", scope, ok)
	}
	for _, separator := range []string{"\u2028", "\u2029"} {
		real := favoriteAuthority("$1", 42, 201)
		real.SelectorValue = "/tmp/a" + separator + ".sock"
		if _, ok := dashboardScope(real); ok {
			t.Fatalf("a real %U separator was accepted", []rune(separator)[0])
		}
	}
}

func TestDashboardFavoriteFollowsATmuxRestartOverRealBroker(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatal("tmux is required")
	}
	label := fmt.Sprintf("pt-fav-%d-%d", os.Getpid(), time.Now().UnixNano())
	tmux := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("tmux", append([]string{"-L", label, "-f", "/dev/null"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("private tmux: %v: %s", err, out)
		}
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", label, "kill-server").Run() })
	tmux("new-session", "-d", "-s", "tm2", "sleep 300")
	tmux("new-session", "-d", "-s", "other", "sleep 300")
	socket := filepath.Join(shortTestDir(t), "broker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- broker.Serve(listener, config.Broker{Realm: "r", FrontUID: uint32(os.Getuid()), FrontUIDConfigured: true, Servers: []config.TmuxServer{{Label: "s", SocketName: label}}})
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("broker did not stop")
		}
	})
	cfg := ergoFrontConfig(t)
	cfg.Realms[0].Socket = socket
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.dashboardPreferencesErr != nil {
		t.Fatal(s.dashboardPreferencesErr)
	}
	inventory := func() (map[string]proto.Authority, uint64) {
		t.Helper()
		w := ergoRequest(t, s, "GET", "http://localhost/api/inventory", "", nil, nil)
		var parsed struct {
			Realms []struct {
				Servers []struct {
					Sessions []struct {
						Name      string          `json:"name"`
						Authority proto.Authority `json:"authority"`
					} `json:"sessions"`
				} `json:"servers"`
			} `json:"realms"`
			FavoritesRevision *uint64 `json:"favorites_revision"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &parsed) != nil || parsed.FavoritesRevision == nil || len(parsed.Realms) != 1 || len(parsed.Realms[0].Servers) != 1 {
			t.Fatalf("inventory=%d %s", w.Code, w.Body.String())
		}
		sessions := map[string]proto.Authority{}
		for _, session := range parsed.Realms[0].Servers[0].Sessions {
			sessions[session.Name] = session.Authority
		}
		return sessions, *parsed.FavoritesRevision
	}
	favorites := func() []string {
		t.Helper()
		w := dashboardRequest(t, s, "GET", "", "", nil)
		var body map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("favorites=%d %s", w.Code, w.Body.String())
		}
		keys := make([]string, 0, len(body))
		for key := range body {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "available,favorites,revision,version" {
			t.Fatalf("favorites reply carries more than the page needs: %s", w.Body.String())
		}
		scopes := []string{}
		for _, scope := range body["favorites"].([]any) {
			scopes = append(scopes, scope.(string))
		}
		return scopes
	}
	before, revision := inventory()
	if revision != 0 {
		t.Fatalf("initial favorites revision %d", revision)
	}
	scope, _ := dashboardScope(before["tm2"])
	if w := dashboardRequest(t, s, "PUT", `{"version":1,"favorites":[`+fmt.Sprintf("%q", scope)+`]}`, `"0"`, nil); w.Code != 200 {
		t.Fatalf("favorite=%d %s", w.Code, w.Body.String())
	}
	tmux("kill-server")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		out, err := exec.Command("tmux", "-L", label, "has-session").CombinedOutput()
		if err != nil && strings.Contains(string(out), "no server running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("private tmux server did not stop: %s", out)
		}
	}
	tmux("new-session", "-d", "-s", "other", "sleep 300")
	tmux("new-session", "-d", "-s", "tm2", "sleep 300")
	after, revision := inventory()
	if sameAuthority(before["tm2"], after["tm2"]) {
		t.Fatal("restart did not change the session identity")
	}
	moved, _ := dashboardScope(after["tm2"])
	if got := favorites(); revision != 2 || !reflect.DeepEqual(got, []string{moved}) {
		t.Fatalf("favorite did not follow tm2 across the restart: revision=%d favorites=%q want %q", revision, got, moved)
	}
}

// Inventory side effects follow the order in which inventories observed the
// sessions: one that read an old list before a newer inventory finished
// changes nothing, so it cannot move a favorite back to a dead session. A
// server whose latest list is incomplete keeps the names it had, so a star
// added meanwhile still records its session's name.
func TestInventoryEffectsFollowObservationOrder(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.dashboardPreferencesErr != nil || s.aliasErr != nil {
		t.Fatal(s.dashboardPreferencesErr, s.aliasErr)
	}
	const operator = "operator@example.com"
	complete := map[string]bool{"local\x00private": true}
	old, replacement := favoriteAuthority("$1", 42, 201), favoriteAuthority("$5", 77, 900)
	oldScope, _ := dashboardScope(old)
	replacementScope, _ := dashboardScope(replacement)
	if _, ok := s.inventoryEffects(1, operator, []aliasSession{{Authority: old, Name: "shell"}}, complete); !ok {
		t.Fatal("favorites unavailable")
	}
	if _, err := s.dashboardPreferences.put(operator, dashboardPreferences{Version: 1, Favorites: []string{oldScope}}, 0, s.inventorySessionName); err != nil {
		t.Fatal(err)
	}
	newer, _ := s.inventoryEffects(3, operator, []aliasSession{{Authority: replacement, Name: "shell"}}, complete)
	late, _ := s.inventoryEffects(2, operator, []aliasSession{{Authority: old, Name: "shell"}}, complete)
	record, _ := s.dashboardPreferences.get(operator)
	if late != newer || !reflect.DeepEqual(record.Favorites, []string{replacementScope}) {
		t.Fatalf("a late inventory moved the favorite back: revision %d after %d, favorites %q", late, newer, record.Favorites)
	}
	if s.inventorySessionName(oldScope) != "" || s.inventorySessionName(replacementScope) != "shell" {
		t.Fatal("a late inventory replaced the session names")
	}

	other := favoriteAuthority("$6", 77, 901)
	otherScope, _ := dashboardScope(other)
	s.inventoryEffects(4, operator, []aliasSession{{Authority: replacement, Name: "shell"}, {Authority: other, Name: "logs"}}, complete)
	s.inventoryEffects(5, operator, nil, map[string]bool{})
	record, _ = s.dashboardPreferences.get(operator)
	saved, err := s.dashboardPreferences.put(operator, dashboardPreferences{Version: 1, Favorites: []string{replacementScope, otherScope}}, record.Revision, s.inventorySessionName)
	if err != nil || saved.Names[otherScope] != "logs" {
		t.Fatalf("a star added while its server's list was incomplete lost its name: %+v %v", saved, err)
	}
	s.inventoryEffects(6, operator, nil, complete)
	if s.inventorySessionName(otherScope) != "" {
		t.Fatal("a complete list kept the name of a session it no longer has")
	}
}

// A store whose file faulted stays unavailable: reconciliation reports it,
// so the inventory never logs it as recovered.
func TestDashboardFavoritesReconcileReportsALatchedFault(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := openDashboardPreferencesStore(filepath.Join(directory, "appearance.json"))
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := dashboardScope(favoriteAuthority("$1", 42, 201))
	if _, err := store.put("operator", dashboardPreferences{Version: 1, Favorites: []string{scope}}, 0, nil); err != nil {
		t.Fatal(err)
	}
	store.file.fault = errors.New("directory sync failed")
	for range 2 {
		if _, err := store.reconcile("operator", nil, nil); !errors.Is(err, errDashboardPreferencesUnavailable) {
			t.Fatalf("faulted store reconciled: %v", err)
		}
	}
}
