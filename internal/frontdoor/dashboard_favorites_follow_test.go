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
	"runtime"
	"slices"
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
	if _, ok := s.inventoryEffects(1, operator, []aliasSession{{Authority: old, Name: "shell"}}, complete, nil); !ok {
		t.Fatal("favorites unavailable")
	}
	if _, err := s.dashboardPreferences.put(operator, dashboardPreferences{Version: 1, Favorites: []string{oldScope}}, 0, s.inventorySessionName); err != nil {
		t.Fatal(err)
	}
	newer, _ := s.inventoryEffects(3, operator, []aliasSession{{Authority: replacement, Name: "shell"}}, complete, nil)
	late, _ := s.inventoryEffects(2, operator, []aliasSession{{Authority: old, Name: "shell"}}, complete, nil)
	record, _ := s.dashboardPreferences.get(operator)
	if late != newer || !reflect.DeepEqual(record.Favorites, []string{replacementScope}) {
		t.Fatalf("a late inventory moved the favorite back: revision %d after %d, favorites %q", late, newer, record.Favorites)
	}
	if s.inventorySessionName(oldScope) != "" || s.inventorySessionName(replacementScope) != "shell" {
		t.Fatal("a late inventory replaced the session names")
	}

	other := favoriteAuthority("$6", 77, 901)
	otherScope, _ := dashboardScope(other)
	s.inventoryEffects(4, operator, []aliasSession{{Authority: replacement, Name: "shell"}, {Authority: other, Name: "logs"}}, complete, nil)
	s.inventoryEffects(5, operator, nil, map[string]bool{}, nil)
	record, _ = s.dashboardPreferences.get(operator)
	saved, err := s.dashboardPreferences.put(operator, dashboardPreferences{Version: 1, Favorites: []string{replacementScope, otherScope}}, record.Revision, s.inventorySessionName)
	if err != nil || saved.Names[otherScope] != "logs" {
		t.Fatalf("a star added while its server's list was incomplete lost its name: %+v %v", saved, err)
	}
	s.inventoryEffects(6, operator, nil, complete, nil)
	if s.inventorySessionName(otherScope) != "" {
		t.Fatal("a complete list kept the name of a session it no longer has")
	}
}

// A favorite PUT looks its session's name up while it holds the favorites
// store; an inventory reconciles favorites while it holds the inventory
// effects. Neither may wait for the other.
func TestFavoritePutAndInventoryDoNotDeadlock(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.dashboardPreferencesErr != nil || s.aliasErr != nil {
		t.Fatal(s.dashboardPreferencesErr, s.aliasErr)
	}
	a := favoriteAuthority("$1", 42, 201)
	scope, _ := dashboardScope(a)
	lookingUp, lookUp := make(chan struct{}), make(chan struct{})
	put, inventory := make(chan error, 1), make(chan struct{})
	go func() {
		_, err := s.dashboardPreferences.put("operator", dashboardPreferences{Version: 1, Favorites: []string{scope}}, 0, func(scope string) string {
			close(lookingUp)
			<-lookUp
			return s.inventorySessionName(scope)
		})
		put <- err
	}()
	<-lookingUp
	go func() {
		s.inventoryEffects(1, "operator", []aliasSession{{Authority: a, Name: "shell"}}, map[string]bool{"local\x00private": true}, nil)
		close(inventory)
	}()
	// The inventory now holds its effects and waits for the store.
	for deadline := time.Now().Add(5 * time.Second); s.inventoryEffectsMu.TryLock(); {
		s.inventoryEffectsMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("the inventory did not take its effects")
		}
		runtime.Gosched()
	}
	close(lookUp)
	select {
	case err := <-put:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the favorite PUT blocked")
	}
	select {
	case <-inventory:
	case <-time.After(5 * time.Second):
		t.Fatal("the inventory blocked")
	}
}

// A favorite not on an incomplete list may be running under a new name, so a
// newer favorite holding its old name does not drop it; the next complete
// list shows both.
func TestFavoriteMissingFromAnIncompleteListStays(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.dashboardPreferencesErr != nil || s.aliasErr != nil {
		t.Fatal(s.dashboardPreferencesErr, s.aliasErr)
	}
	complete := map[string]bool{"local\x00private": true}
	renamed, other := favoriteAuthority("$1", 42, 201), favoriteAuthority("$2", 42, 202)
	renamedScope, _ := dashboardScope(renamed)
	otherScope, _ := dashboardScope(other)
	s.inventoryEffects(1, "operator", []aliasSession{{Authority: renamed, Name: "shell"}}, complete, nil)
	if _, err := s.dashboardPreferences.put("operator", dashboardPreferences{Version: 1, Favorites: []string{renamedScope}}, 0, s.inventorySessionName); err != nil {
		t.Fatal(err)
	}
	revision, _ := s.inventoryEffects(2, "operator", []aliasSession{{Authority: other, Name: "shell"}}, map[string]bool{}, nil)
	if _, err := s.dashboardPreferences.put("operator", dashboardPreferences{Version: 1, Favorites: []string{renamedScope, otherScope}}, revision, s.inventorySessionName); err != nil {
		t.Fatal(err)
	}
	s.inventoryEffects(3, "operator", []aliasSession{{Authority: other, Name: "shell"}}, map[string]bool{}, nil)
	partial, _ := s.dashboardPreferences.get("operator")
	s.inventoryEffects(4, "operator", []aliasSession{{Authority: renamed, Name: "renamed"}, {Authority: other, Name: "shell"}}, complete, nil)
	record, _ := s.dashboardPreferences.get("operator")
	want := []string{renamedScope, otherScope}
	if !reflect.DeepEqual(partial.Favorites, want) || !reflect.DeepEqual(record.Favorites, want) || record.Names[renamedScope] != "renamed" {
		t.Fatalf("an incomplete list dropped a favorite: after it %q, after a complete list %q %v", partial.Favorites, record.Favorites, record.Names)
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

// A full list keeps room for one more favorite: the inventory drops the
// oldest favorite whose session is gone from a complete list. A server that
// is stopped, failing or cut short by the front door may still run its
// sessions, so their favorites stay.
func TestFullFavoritesDropTheOldestGoneOnACompleteList(t *testing.T) {
	socket := filepath.Join(shortTestDir(t), "broker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	authority := func(server, id string) proto.Authority {
		return proto.Authority{Realm: "r", Server: server, UID: uint32(os.Geteuid()), SelectorKind: "socket_name", SelectorValue: server, BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: id, SessionCreated: 3}
	}
	session := func(server, id string) proto.Session {
		return proto.Session{Authority: authority(server, id), Name: "s" + id, Width: 80, Height: 24}
	}
	go func() {
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			_, _ = proto.ReadFrame(c)
			_ = writeControl(c, proto.Control{Type: "hello_ok", V: 1})
			_, _ = proto.ReadFrame(c)
			_ = writeControl(c, proto.Control{Type: "inventory_ok", Servers: []proto.ServerInventory{
				{Label: "main", Status: "ok", Sessions: []proto.Session{session("main", "$1")}},
				{Label: "stopped", Status: "no_server", Sessions: []proto.Session{}},
				{Label: "failing", Status: "ok", Error: "list_failed", Sessions: []proto.Session{}},
				// The front door lists three sessions in all: this server's third is cut.
				{Label: "cut", Status: "ok", Sessions: []proto.Session{session("cut", "$2"), session("cut", "$3"), session("cut", "$4")}},
			}})
			c.Close()
		}
	}()
	cfg := ergoFrontConfig(t)
	cfg.Realms[0].Socket = socket
	cfg.HandleCapacity = 9
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.dashboardPreferencesErr != nil {
		t.Fatal(s.dashboardPreferencesErr)
	}
	scope := func(server, id string) string {
		value, ok := dashboardScope(authority(server, id))
		if !ok {
			t.Fatal("invalid scope")
		}
		return value
	}
	oldest := []string{scope("stopped", "$10"), scope("failing", "$11"), scope("cut", "$4"), scope("main", "$20"), scope("main", "$21"), scope("main", "$1")}
	favorites := append([]string{}, oldest...)
	for i := len(favorites); i < dashboardFavoritesLimit; i++ {
		favorites = append(favorites, scope("stopped", fmt.Sprintf("$%d", 100+i)))
	}
	body, _ := json.Marshal(dashboardPreferences{Version: 1, Favorites: favorites})
	if w := dashboardRequest(t, s, "PUT", string(body), `"0"`, nil); w.Code != 200 {
		t.Fatalf("favorites=%d %s", w.Code, w.Body.String())
	}
	if w := ergoRequest(t, s, "GET", "http://localhost/api/inventory", "", nil, nil); w.Code != 200 {
		t.Fatalf("inventory=%d %s", w.Code, w.Body.String())
	}
	record, _ := s.dashboardPreferences.get(s.cfg.Ingress.OperatorLogin)
	want := append(append([]string{}, favorites[:3]...), favorites[4:]...)
	if !reflect.DeepEqual(record.Favorites, want) {
		t.Fatalf("the full list did not drop only its oldest gone favorite on a complete list: lost %q", removed(favorites, record.Favorites))
	}
	// One slot is free: nothing more is dropped.
	if w := ergoRequest(t, s, "GET", "http://localhost/api/inventory", "", nil, nil); w.Code != 200 {
		t.Fatalf("inventory=%d %s", w.Code, w.Body.String())
	}
	if again, _ := s.dashboardPreferences.get(s.cfg.Ingress.OperatorLogin); !reflect.DeepEqual(again.Favorites, want) {
		t.Fatalf("a list with room dropped a favorite: lost %q", removed(want, again.Favorites))
	}
}

// A gone favorite that moves to the session now holding its name is kept,
// even when it is the oldest: the full list drops the next gone one.
func TestFullFavoritesMoveBeforeTheyDrop(t *testing.T) {
	cfg := ergoFrontConfig(t)
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.dashboardPreferencesErr != nil {
		t.Fatal(s.dashboardPreferencesErr)
	}
	complete := map[string]bool{"local\x00private": true}
	renamed, replacement := favoriteAuthority("$1", 42, 201), favoriteAuthority("$1", 43, 301)
	s.inventoryEffects(1, "operator", []aliasSession{{Authority: renamed, Name: "shell"}}, complete, nil)
	favorites := []string{}
	for i := 0; i < dashboardFavoritesLimit; i++ {
		value, _ := dashboardScope(favoriteAuthority(fmt.Sprintf("$%d", i+1), 42, int64(201+i)))
		favorites = append(favorites, value)
	}
	if _, err := s.dashboardPreferences.put("operator", dashboardPreferences{Version: 1, Favorites: favorites}, 0, s.inventorySessionName); err != nil {
		t.Fatal(err)
	}
	s.inventoryEffects(2, "operator", []aliasSession{{Authority: replacement, Name: "shell"}}, complete, nil)
	record, _ := s.dashboardPreferences.get("operator")
	moved, _ := dashboardScope(replacement)
	want := append([]string{moved}, favorites[2:]...)
	if !reflect.DeepEqual(record.Favorites, want) || record.Names[moved] != "shell" {
		t.Fatalf("favorites %d starting %q, names %v; want the moved favorite kept and the next gone one dropped", len(record.Favorites), record.Favorites[:2], record.Names)
	}
}

func removed(before, after []string) []string {
	out := []string{}
	for _, scope := range before {
		if !slices.Contains(after, scope) {
			out = append(out, scope)
		}
	}
	return out
}

// gatedBroker answers each inventory request only when the test sends its
// reply, so a test can order two overlapping inventory reads.
type gatedInventory struct {
	reply chan []proto.ServerInventory
	sent  chan struct{}
}

func gatedBroker(t *testing.T) (string, <-chan gatedInventory) {
	t.Helper()
	socket := filepath.Join(shortTestDir(t), "broker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); _ = listener.Close() })
	requests := make(chan gatedInventory, 8)
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := proto.ReadFrame(c); err != nil {
					return
				}
				if writeControl(c, proto.Control{Type: "hello_ok", V: 1}) != nil {
					return
				}
				if _, err := proto.ReadFrame(c); err != nil {
					return
				}
				request := gatedInventory{make(chan []proto.ServerInventory, 1), make(chan struct{})}
				select {
				case requests <- request:
				case <-done:
					return
				}
				select {
				case servers := <-request.reply:
					_ = writeControl(c, proto.Control{Type: "inventory_ok", Servers: servers})
					close(request.sent)
				case <-done:
				}
			}()
		}
	}()
	return socket, requests
}

func (g gatedInventory) answer(t *testing.T, servers ...proto.ServerInventory) {
	t.Helper()
	g.reply <- servers
	select {
	case <-g.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("inventory reply was not written")
	}
}

func nextInventory(t *testing.T, requests <-chan gatedInventory) gatedInventory {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("inventory request did not arrive")
	}
	return gatedInventory{}
}

// overlapFixture serves a front door with two realms behind gated brokers,
// "r" (one server, "main") and "slow", and a full favorites list.
type overlapFixture struct {
	s          *Server
	main, slow <-chan gatedInventory
}

func newOverlapFixture(t *testing.T, favorites []string, names map[string]string) *overlapFixture {
	t.Helper()
	mainSocket, mainRequests := gatedBroker(t)
	slowSocket, slowRequests := gatedBroker(t)
	cfg := ergoFrontConfig(t)
	cfg.Realms[0].Socket = mainSocket
	slow := cfg.Realms[0]
	slow.Name, slow.Socket = "slow", slowSocket
	cfg.Realms = append(cfg.Realms, slow)
	cfg.HandleCapacity = 2048
	s := newServer(cfg, ".", cfg.Ingress.CanonicalHost)
	if s.dashboardPreferencesErr != nil || s.aliasErr != nil {
		t.Fatal(s.dashboardPreferencesErr, s.aliasErr)
	}
	if _, err := s.dashboardPreferences.put(cfg.Ingress.OperatorLogin, dashboardPreferences{Version: 1, Favorites: favorites}, 0, func(scope string) string { return names[scope] }); err != nil {
		t.Fatal(err)
	}
	return &overlapFixture{s: s, main: mainRequests, slow: slowRequests}
}

// read starts an inventory read; finish waits for its reply.
func (f *overlapFixture) read(t *testing.T) func() {
	done := make(chan int, 1)
	go func() { done <- ergoRequest(t, f.s, "GET", "http://localhost/api/inventory", "", nil, nil).Code }()
	return func() {
		t.Helper()
		select {
		case code := <-done:
			if code != 200 {
				t.Fatalf("inventory=%d", code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("inventory did not finish")
		}
	}
}

func (f *overlapFixture) favorites() dashboardPreferenceRecord {
	record, _ := f.s.dashboardPreferences.get(f.s.cfg.Ingress.OperatorLogin)
	return record
}

func overlapAuthority(id string) proto.Authority {
	return proto.Authority{Realm: "r", Server: "main", UID: uint32(os.Geteuid()), SelectorKind: "socket_name", SelectorValue: "main", BootID: "b", ServerPID: 42, ServerStart: 100, SessionID: id, SessionCreated: 200}
}

func overlapScope(a proto.Authority) string { scope, _ := dashboardScope(a); return scope }

func overlapList(sessions ...proto.Session) proto.ServerInventory {
	return proto.ServerInventory{Label: "main", Status: "ok", Sessions: sessions}
}

// fullOverlapList is a full favorites list whose first favorite is first;
// the others are running sessions returned as others.
func fullOverlapList(first proto.Authority, name string) ([]string, map[string]string, []proto.Session) {
	favorites, names, others := []string{overlapScope(first)}, map[string]string{overlapScope(first): name}, []proto.Session{}
	for i := 1; i < dashboardFavoritesLimit; i++ {
		a := overlapAuthority(fmt.Sprintf("$%d", 900+i))
		favorites, names[overlapScope(a)] = append(favorites, overlapScope(a)), fmt.Sprintf("f%d", i)
		others = append(others, proto.Session{Authority: a, Name: names[overlapScope(a)], Width: 80, Height: 24})
	}
	return favorites, names, others
}

func overlapSession(a proto.Authority, name string) proto.Session {
	return proto.Session{Authority: a, Name: name, Width: 80, Height: 24}
}

// Two inventory reads overlap. The later read sees the main realm while a
// session restarts (gone), then waits for another realm; the earlier read
// sees the restarted session, moves its favorite and finishes first. The
// later read's older view must not drop the favorite that just moved.
func TestOverlappingInventoriesDoNotDropOnAnOlderView(t *testing.T) {
	old, restarted := overlapAuthority("$1"), overlapAuthority("$501")
	favorites, names, others := fullOverlapList(old, "kept")
	f := newOverlapFixture(t, favorites, names)
	first := f.read(t)
	firstMain, firstSlow := nextInventory(t, f.main), nextInventory(t, f.slow)
	firstSlow.answer(t)
	second := f.read(t)
	secondMain, secondSlow := nextInventory(t, f.main), nextInventory(t, f.slow)
	secondMain.answer(t, overlapList(others...))
	firstMain.answer(t, overlapList(append([]proto.Session{overlapSession(restarted, "kept")}, others...)...))
	first()
	if moved := f.favorites(); len(moved.Favorites) != dashboardFavoritesLimit || moved.Favorites[0] != overlapScope(restarted) {
		t.Fatalf("the newer view did not move the favorite: %d favorites", len(moved.Favorites))
	}
	secondSlow.answer(t)
	second()
	if after := f.favorites(); len(after.Favorites) != dashboardFavoritesLimit || after.Favorites[0] != overlapScope(restarted) || after.Names[overlapScope(restarted)] != "kept" {
		t.Fatalf("an older view dropped the moved favorite: %d favorites, first %q", len(after.Favorites), after.Favorites[0])
	}
}

// The same overlap across a rename: the older view still has the old name.
// It must not record that name, or the session's next restart under its
// current name would lose the favorite.
func TestOverlappingInventoriesKeepTheNewerName(t *testing.T) {
	session, restarted := overlapAuthority("$1"), overlapAuthority("$501")
	favorites, names, others := fullOverlapList(session, "before")
	f := newOverlapFixture(t, favorites, names)
	first := f.read(t)
	firstMain, firstSlow := nextInventory(t, f.main), nextInventory(t, f.slow)
	firstSlow.answer(t)
	second := f.read(t)
	secondMain, secondSlow := nextInventory(t, f.main), nextInventory(t, f.slow)
	secondMain.answer(t, overlapList(append([]proto.Session{overlapSession(session, "before")}, others...)...))
	firstMain.answer(t, overlapList(append([]proto.Session{overlapSession(session, "after")}, others...)...))
	first()
	secondSlow.answer(t)
	second()
	if name := f.favorites().Names[overlapScope(session)]; name != "after" || f.s.inventorySessionName(overlapScope(session)) != "after" {
		t.Fatalf("an older view recorded the old name: favorite %q, inventory %q", name, f.s.inventorySessionName(overlapScope(session)))
	}
	third := f.read(t)
	nextInventory(t, f.main).answer(t, overlapList(append([]proto.Session{overlapSession(restarted, "after")}, others...)...))
	nextInventory(t, f.slow).answer(t)
	third()
	if after := f.favorites(); len(after.Favorites) != dashboardFavoritesLimit || after.Favorites[0] != overlapScope(restarted) {
		t.Fatalf("the restarted session lost its favorite: %d favorites, first %q", len(after.Favorites), after.Favorites[0])
	}
}
