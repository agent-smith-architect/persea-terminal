package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

type inventoryBudgetFixture struct {
	server config.TmuxServer
	binary string
	nonce  int
}

func newInventoryBudgetFixture(t *testing.T, dir string) *inventoryBudgetFixture {
	t.Helper()
	binary, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", dir)
	f := &inventoryBudgetFixture{server: config.TmuxServer{Label: "budget", SocketName: fmt.Sprintf("pt-si-%d-%d", os.Getpid(), time.Now().UnixNano())}, binary: binary}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, f.binary, "-u", "-N", "-L", f.server.SocketName, "kill-server").Run()
	})
	f.run(t, "-f", "/dev/null", "new-session", "-d", "-s", "operator", "sleep 600")
	return f
}

func (f *inventoryBudgetFixture) run(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	argv := append([]string{"-u", "-L", f.server.SocketName}, args...)
	out, err := exec.CommandContext(ctx, f.binary, argv...).CombinedOutput()
	if err != nil {
		t.Fatalf("fixture tmux %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *inventoryBudgetFixture) owner(t *testing.T) string {
	t.Helper()
	owner, _ := f.ownerHelper(t)
	return owner
}

func (f *inventoryBudgetFixture) ownerHelper(t *testing.T) (string, func()) {
	t.Helper()
	inc, err := readIncarnation(f.server)
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command("sleep", "600")
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	stop := func() {
		if helper.ProcessState == nil {
			_ = helper.Process.Kill()
			_ = helper.Wait()
		}
	}
	t.Cleanup(stop)
	owner, err := (procProbe{}).Witness(context.Background(), helper.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s=%s:%d:%d", attachmentOwnerEnvironment, inc.BootID, owner.PID, owner.StartTime), stop
}

func (f *inventoryBudgetFixture) shadow(t *testing.T, owner string) string {
	t.Helper()
	f.nonce++
	args := []string{"-N", "new-session", "-d", "-P", "-F", "#{session_id}", "-t", "operator", "-s", fmt.Sprintf("%s%032x", attachmentShadowPrefix, f.nonce)}
	if owner != "" {
		args = append(args, "-e", owner)
	}
	return f.run(t, args...)
}

func (f *inventoryBudgetFixture) requirePresent(t *testing.T, id string, want bool) {
	t.Helper()
	present, err := tmuxSessionPresent(f.server, id)
	if err != nil || present != want {
		t.Fatalf("session %s: present=%v want=%v err=%v", id, present, want, err)
	}
}

func inventoryOwnerRPC(t *testing.T, s *Server) (proto.Control, time.Duration, error) {
	t.Helper()
	path := filepath.Join(shortTempDir(t), "inventory.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			s.handleConn(conn)
		}
	}()
	defer func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("inventory handler did not settle")
		}
	}()
	start := time.Now()
	conn, err := net.DialTimeout("unix", path, SetupTimeout)
	if err != nil {
		return proto.Control{}, time.Since(start), err
	}
	defer conn.Close()
	if err := conn.SetDeadline(start.Add(SetupTimeout)); err != nil {
		return proto.Control{}, time.Since(start), err
	}
	write := func(control proto.Control) error {
		payload, err := json.Marshal(control)
		if err != nil {
			return err
		}
		return proto.WriteFrame(conn, proto.FrameControl, payload)
	}
	read := func() (proto.Control, error) {
		frame, err := proto.ReadFrame(conn)
		if err != nil {
			return proto.Control{}, err
		}
		if frame.Type != proto.FrameControl {
			return proto.Control{}, fmt.Errorf("unexpected frame type %v", frame.Type)
		}
		return proto.DecodeControl(frame.Payload)
	}
	if err := write(proto.Control{Type: "hello", V: 1}); err != nil {
		return proto.Control{}, time.Since(start), err
	}
	hello, err := read()
	if err != nil || hello.Type != "hello_ok" {
		return proto.Control{}, time.Since(start), fmt.Errorf("hello: %+v: %v", hello, err)
	}
	if err := write(proto.Control{Type: "inventory"}); err != nil {
		return proto.Control{}, time.Since(start), err
	}
	result, err := read()
	return result, time.Since(start), err
}

func requireBudgetInventory(t *testing.T, result proto.Control, servers int) {
	t.Helper()
	if result.Type != "inventory_ok" || len(result.Servers) != servers {
		t.Fatalf("inventory reply: %+v", result)
	}
	for _, r := range result.Servers {
		if r.Status != "ok" || r.Error != "" || len(r.Sessions) != 1 || r.Sessions[0].Name != "operator" {
			t.Fatalf("hidden cleanup work affected visible completeness: %+v", r)
		}
	}
}

func TestInventoryOwnerBudgetTimedRPC(t *testing.T) {
	f := newInventoryBudgetFixture(t, shortTempDir(t))
	owner := f.owner(t)
	for range 600 {
		f.shadow(t, owner)
	}
	start := time.Now()
	if _, err := incarnation(f.server); err != nil {
		t.Fatal(err)
	}
	t.Logf("complete first admission with 600 unmarked live-owned rows: %v", time.Since(start))
	s := &Server{config: config.Broker{Realm: "budget", Servers: []config.TmuxServer{f.server}}}
	for pass := 1; pass <= 6; pass++ {
		result, elapsed, err := inventoryOwnerRPC(t, s)
		t.Logf("600-row real protocol inventory pass=%d duration=%v error=%v", pass, elapsed, err)
		if err != nil {
			t.Fatalf("inventory exceeded real RPC deadline: %v", err)
		}
		if elapsed >= SetupTimeout/4 {
			t.Fatalf("inventory used too much RPC budget: %v >= %v", elapsed, SetupTimeout/4)
		}
		requireBudgetInventory(t, result, 1)
	}
	if count := len(strings.Fields(f.run(t, "-N", "list-sessions", "-F", "#{session_id}"))); count != 601 {
		t.Fatalf("live shadows were removed: sessions=%d want=601", count)
	}
	// Configuration permits 64 labels, including selectors of one identity.
	// Every visible list must still be read, but the hidden probe budget is shared.
	s.config.Servers = nil
	for i := range 64 {
		server := f.server
		server.Label = fmt.Sprintf("server-%02d", i)
		s.config.Servers = append(s.config.Servers, server)
	}
	for pass := 1; pass <= 3; pass++ {
		result, elapsed, err := inventoryOwnerRPC(t, s)
		t.Logf("600-row, 64-selector real protocol inventory pass=%d duration=%v error=%v", pass, elapsed, err)
		if err != nil || elapsed >= SetupTimeout/2 {
			t.Fatalf("64-selector inventory used too much RPC budget: %v error=%v", elapsed, err)
		}
		requireBudgetInventory(t, result, 64)
	}
}

func TestInventoryOwnerProbeCost(t *testing.T) {
	f := newInventoryBudgetFixture(t, shortTempDir(t))
	owner := f.owner(t)
	id := f.shadow(t, owner)
	inc, err := readIncarnation(f.server)
	if err != nil {
		t.Fatal(err)
	}
	measure := func(label string, prepare func() func()) {
		t.Helper()
		samples := make([]time.Duration, 100)
		for i := range samples {
			action := prepare()
			start := time.Now()
			action()
			samples[i] = time.Since(start)
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		t.Logf("%s n=%d p50=%v p95=%v max=%v", label, len(samples), samples[50], samples[95], samples[99])
	}
	measure("live-owner creation-record read and liveness", func() func() {
		return func() {
			if alive, err := restoredShadowOwnerAlive(f.server, id, inc.BootID); err != nil || !alive {
				t.Fatalf("live helper: alive=%v err=%v", alive, err)
			}
		}
	})
	measure("creation-record read and guarded removal", func() func() {
		orphanID := f.shadow(t, "")
		row, ok := sessionField(f.server, orphanID, shadowSessionFormat)
		if !ok {
			t.Fatal("missing measurement row")
		}
		return func() {
			shadow, ok, err := restoredShadowCandidate(f.server, strings.Split(row, "\t"), inc)
			if err != nil || !ok {
				t.Fatalf("measurement candidate: ok=%v err=%v", ok, err)
			}
			if removed, err := reapOrphanShadow(f.server, shadow); err != nil || !removed {
				t.Fatalf("measurement removal: removed=%v err=%v", removed, err)
			}
		}
	})
}

func inventoryOwnerReadLog(t *testing.T) func() []string {
	t.Helper()
	path := filepath.Join(shortTempDir(t), "owner-reads")
	installShadowTmuxWrapper(t, fmt.Sprintf(`if args[4] == 'show-environment':
    with open(%q, 'a') as f:
        f.write(args[3] + ' ' + args[args.index('-t')+1] + '\n')
`, path))
	return func() []string {
		t.Helper()
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		return strings.FieldsFunc(strings.TrimSpace(string(data)), func(r rune) bool { return r == '\n' })
	}
}

func TestInventoryOwnerBudgetProgress(t *testing.T) {
	for _, record := range []string{"missing", "exited-owner"} {
		t.Run(record, func(t *testing.T) {
			f := newInventoryBudgetFixture(t, shortTempDir(t))
			owner := f.owner(t)
			if _, err := incarnation(f.server); err != nil {
				t.Fatal(err)
			}
			live := 9*inventoryOwnerReadBudget + 1
			for range live {
				f.shadow(t, owner)
			}
			targetOwner := ""
			stop := func() {}
			if record == "exited-owner" {
				targetOwner, stop = f.ownerHelper(t)
			}
			last := f.shadow(t, targetOwner)
			// Reap the helper before inventory, so owner exit is deterministic.
			stop()
			reads := inventoryOwnerReadLog(t)
			s := &Server{config: config.Broker{Realm: "budget", Servers: []config.TmuxServer{f.server}}}
			passes := (live + 1 + inventoryOwnerReadBudget - 1) / inventoryOwnerReadBudget
			for pass := 1; pass <= passes; pass++ {
				requireBudgetInventory(t, inventoryWireResult(t, s), 1)
				if got := len(reads()); got > inventoryOwnerReadBudget {
					t.Fatalf("pass %d owner reads=%d exceed %d", pass, got, inventoryOwnerReadBudget)
				}
				f.requirePresent(t, last, pass != passes)
			}
			t.Logf("%s row behind %d live owners removed on promised pass %d", record, live, passes)
			if count := len(strings.Fields(f.run(t, "-N", "list-sessions", "-F", "#{session_id}"))); count != live+1 {
				t.Fatalf("live owners or operator were removed: %d", count)
			}
		})
	}
}

func TestInventoryOwnerBudgetSharedReplyAndAliases(t *testing.T) {
	dir := shortTempDir(t)
	a, b := newInventoryBudgetFixture(t, dir), newInventoryBudgetFixture(t, dir)
	for _, f := range []*inventoryBudgetFixture{a, b} {
		if _, err := incarnation(f.server); err != nil {
			t.Fatal(err)
		}
	}
	owner := b.owner(t)
	var targets []string
	for _, f := range []*inventoryBudgetFixture{a, b} {
		for range inventoryOwnerReadBudget + 1 {
			f.shadow(t, owner)
		}
		targets = append(targets, f.shadow(t, ""))
	}
	a.server.Label = "first"
	alias := config.TmuxServer{Label: "alias", SocketPath: filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), a.server.SocketName)}
	b.server.Label = "last"
	s := &Server{config: config.Broker{Realm: "budget", Servers: []config.TmuxServer{a.server, alias, b.server}}}
	reads := inventoryOwnerReadLog(t)
	for pass := 1; pass <= 6; pass++ {
		result := inventoryWireResult(t, s)
		requireBudgetInventory(t, result, 3)
		if result.Servers[0].Label != "first" || result.Servers[1].Label != "alias" || result.Servers[2].Label != "last" {
			t.Fatal("cleanup rotation changed visible server ordering")
		}
		if got := len(reads()); got > inventoryOwnerReadBudget {
			t.Fatalf("whole reply owner reads=%d exceed %d", got, inventoryOwnerReadBudget)
		}
		if pass == 2 {
			a.requirePresent(t, targets[0], false)
		}
	}
	a.requirePresent(t, targets[0], false)
	b.requirePresent(t, targets[1], false)
}

func TestInventoryOwnerBudgetFiniteRounds(t *testing.T) {
	f := newInventoryBudgetFixture(t, shortTempDir(t))
	owner := f.owner(t)
	if _, err := incarnation(f.server); err != nil {
		t.Fatal(err)
	}
	live := 3*inventoryOwnerReadBudget + 1
	first := f.shadow(t, owner)
	for i := 1; i < live; i++ {
		f.shadow(t, owner)
	}
	s := &Server{config: config.Broker{Realm: "budget", Servers: []config.TmuxServer{f.server}}}
	requireBudgetInventory(t, inventoryWireResult(t, s), 1)
	f.run(t, "-N", "set-environment", "-u", "-t", "="+first+":", attachmentOwnerEnvironment)
	remaining := (live+inventoryOwnerReadBudget-1)/inventoryOwnerReadBudget - 1
	for pass := 1; pass <= remaining+1; pass++ {
		for range inventoryOwnerReadBudget {
			f.shadow(t, owner)
		}
		requireBudgetInventory(t, inventoryWireResult(t, s), 1)
		f.requirePresent(t, first, pass <= remaining)
	}
	t.Logf("previously inspected row reclaimed after %d further passes despite new arrivals", remaining+1)
}

func TestInventoryOwnerBudgetVisibleRows(t *testing.T) {
	f := newInventoryBudgetFixture(t, shortTempDir(t))
	if _, err := incarnation(f.server); err != nil {
		t.Fatal(err)
	}
	names := []string{
		"operator",
		attachmentShadowPrefix + strings.Repeat("a", 31),
		attachmentShadowPrefix + strings.Repeat("a", 33),
		attachmentShadowPrefix + strings.Repeat("g", 32),
		"Persea-attach-" + strings.Repeat("a", 32),
	}
	for _, name := range names[1:] {
		f.run(t, "-N", "new-session", "-d", "-t", "operator", "-s", name)
	}
	owner := f.owner(t)
	for _, name := range []string{
		attachmentShadowPrefix + strings.Repeat("a", 32),
		attachmentShadowPrefix + strings.Repeat("A", 32),
	} {
		f.run(t, "-N", "new-session", "-d", "-t", "operator", "-s", name, "-e", owner)
	}
	partial := f.shadow(t, "")
	f.run(t, "-N", "set-option", "-t", "="+partial+":", attachmentOwnerPIDOption, "1")
	s := &Server{config: config.Broker{Realm: "budget", Servers: []config.TmuxServer{f.server}}}
	sort.Strings(names)
	for _, budget := range []int{0, inventoryOwnerReadBudget} {
		result := s.inventoryServerWithOwnerBudget(f.server, InventorySessionLimit, budget)
		if result.Status != "ok" || result.Error != "" {
			t.Fatalf("budget %d affected visible completeness: %+v", budget, result)
		}
		var got []string
		for _, session := range result.Sessions {
			got = append(got, session.Name)
		}
		sort.Strings(got)
		if strings.Join(got, "\n") != strings.Join(names, "\n") {
			t.Fatalf("budget %d visible names=%q want=%q", budget, got, names)
		}
		f.requirePresent(t, partial, true)
	}
}
