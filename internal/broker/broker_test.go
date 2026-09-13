package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

type socketDirInfo struct {
	mode os.FileMode
	uid  uint32
}

func (i socketDirInfo) Name() string       { return "socket-dir" }
func (i socketDirInfo) Size() int64        { return 0 }
func (i socketDirInfo) Mode() os.FileMode  { return i.mode | os.ModeDir }
func (i socketDirInfo) ModTime() time.Time { return time.Time{} }
func (i socketDirInfo) IsDir() bool        { return true }
func (i socketDirInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid} }

func TestValidateSocketDirectoryExactProtectedModes(t *testing.T) {
	const uid = uint32(1000)
	tests := []struct {
		name     string
		mode     os.FileMode
		owner    uint32
		crossUID bool
		wantErr  bool
	}{
		{"same exact", 0700, uid, false, false},
		{"cross exact", 0710 | os.ModeSetgid, uid, true, false},
		{"wrong owner", 0700, uid + 1, false, true},
		{"same group write", 0720, uid, false, true},
		{"same other write", 0702, uid, false, true},
		{"cross group write", 0730 | os.ModeSetgid, uid, true, true},
		{"cross other write", 0712 | os.ModeSetgid, uid, true, true},
		{"cross missing setgid", 0710, uid, true, true},
		{"same extra setuid", 0700 | os.ModeSetuid, uid, false, true},
		{"same extra setgid", 0700 | os.ModeSetgid, uid, false, true},
		{"same extra sticky", 0700 | os.ModeSticky, uid, false, true},
		{"cross extra setuid", 0710 | os.ModeSetgid | os.ModeSetuid, uid, true, true},
		{"cross extra sticky", 0710 | os.ModeSetgid | os.ModeSticky, uid, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSocketDirectory(socketDirInfo{mode: tc.mode, uid: tc.owner}, uid, tc.crossUID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("mode %v owner %d crossUID %v: error = %v", tc.mode, tc.owner, tc.crossUID, err)
			}
		})
	}
}

func TestComputeGeometryStatusModes(t *testing.T) {
	for _, tc := range []struct {
		s    string
		rows int
	}{{"off", 40}, {"on", 41}, {"2", 42}} {
		g, e := ComputeGeometry(120, 40, tc.s)
		if e != nil || g.PTYRows != tc.rows {
			t.Fatalf("%q: %+v %v", tc.s, g, e)
		}
	}
}
func TestObserveRejectsInputWithoutWriting(t *testing.T) {
	var b bytes.Buffer
	if e := writeInput("observe", &b, []byte("x")); e != errObserveMode || b.Len() != 0 {
		t.Fatalf("input was not suppressed")
	}
}
func TestGuardedAttachArgvUsesOneClientExactIDAndReadonly(t *testing.T) {
	s := config.TmuxServer{Label: "main", SocketPath: "/tmp/example.sock"}
	a := proto.Authority{BootID: "boot", ServerPID: 123, ServerStart: 456, SessionID: "$7"}
	got, e := guardedAttachArgv(s, a, "observe", "PERSEA_nonce_STALE")
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 10 || strings.Join(got[:7], "\x00") != strings.Join([]string{"tmux", "-u", "-S", "/tmp/example.sock", "if-shell", "-t", "=$7:"}, "\x00") || got[8] != "attach-session -r -f ignore-size -t =$7" || got[9] != "display-message -p PERSEA_nonce_STALE" {
		t.Fatalf("%q", got)
	}
	if !strings.Contains(got[7], "#{pid}") || !strings.Contains(got[7], "${stat##*) }") || !strings.Contains(got[7], "${20}") {
		t.Fatalf("incarnation condition does not parse the final proc-stat suffix: %q", got[7])
	}
	control, e := guardedAttachArgv(s, a, "control", "PERSEA_nonce_STALE")
	if e != nil || control[8] != "attach-session -f ignore-size -t =$7" {
		t.Fatalf("control attach is not writable: %q %v", control, e)
	}
	a.SessionID = "alpha"
	if _, e = guardedAttachArgv(s, a, "observe", "PERSEA_nonce_STALE"); !errors.Is(e, errStaleTarget) {
		t.Fatal("non-ID target accepted")
	}
}
func TestBoundHistoryCaps(t *testing.T) {
	in := []byte(strings.Repeat("line\n", 1200))
	out, tr := boundHistory(in, 1000, 1024)
	if !tr || len(out) > 1024 || bytes.Count(out, []byte("\n")) > 1000 {
		t.Fatalf("bounds not enforced: %d %d", len(out), bytes.Count(out, []byte("\n")))
	}
}

func TestBrokerSocketPathLengthBoundHasClearError(t *testing.T) {
	path := "/tmp/" + strings.Repeat("socket-segment-", 9)
	err := Run(path, config.Broker{})
	if err == nil || !strings.Contains(err.Error(), "maximum is 107") {
		t.Fatalf("overlong socket path did not fail clearly: %v", err)
	}
}

func TestNoDestructiveOrImplicitResizeProductCallsites(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
	uiSourceRoot := filepath.Join(root, "ui", "src")
	forbidden := []string{"kill" + "-pane", "kill" + "-server", "send" + "-keys", "copy" + "-mode", "respawn" + "-pane", "respawn" + "-window", "resize" + "-pane", "TIOC" + "SWINSZ", "Fit" + "Addon", "/api/" + "history"}
	captureCalls, attachCalls := 0, 0
	legacyAttachCalls, unifiedObserverAttachCalls := 0, 0
	unifiedUnitSpawnShapeChecked := false
	unifiedAdoptionAttachShapeChecked := false
	unifiedRecoveryAttachShapeChecked := false
	previewCaptureShapeChecked := false
	shadowCreateCalls, shadowDestroyCalls := 0, 0
	operatorCreateCalls := 0
	resizeWindowCalls, ptySetSizeCalls := 0, 0
	resizeGuardChecked := false
	issuerSeamChecked, verticalFitAuthorityChecked := false, false
	blockingBoundaryCallsites, blockingBoundaryHelpers := 0, 0
	rotationRestoreBoundaryChecked := false
	for _, dir := range []string{filepath.Join(root, "cmd"), filepath.Join(root, "internal"), filepath.Join(root, "ui", "src")} {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.IsDir() || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			for _, token := range forbidden {
				if bytes.Contains(b, []byte(token)) {
					t.Fatalf("forbidden token %q in %s", token, path)
				}
			}
			base := filepath.Base(path)
			captureCalls += bytes.Count(b, []byte("capture"+"-pane"))
			attachHere := bytes.Count(b, []byte("attach"+"-session"))
			attachCalls += attachHere
			if base == "unified_dev.go" {
				// The unified provider spawns per-session observer units. A
				// birth unit issues no attach verb at all — its startup
				// command IS the session creation built by the audited
				// operator-create path. Adoption and transport recovery each
				// attach to an EXISTING session, by session ID, with ignore-size,
				// so observation can never implicitly resize a session the product
				// did not create. The shape pins below hold the config-less client
				// form and both exact argument vectors.
				unifiedObserverAttachCalls += attachHere
				unifiedUnitSpawnShapeChecked = bytes.Contains(b, []byte(`append(tmuxArgv(request.server, "-C", "-f", "/dev/null"), request.args...)`))
				unifiedAdoptionAttachShapeChecked = bytes.Contains(b, []byte(`[]string{"attach-session", "-f", "ignore-size", "-t", sessionID}`))
				unifiedRecoveryAttachShapeChecked = bytes.Contains(b, []byte(`[]string{"attach-session", "-f", "ignore-size", "-t", previous.sessionID}`))
			} else {
				legacyAttachCalls += attachHere
			}
			if strings.HasSuffix(path, filepath.Join("broker", "session_preview.go")) {
				// The dashboard preview is the one capture callsite outside the
				// guarded snapshot paths: a single read-only, plain-text
				// capture-pane of a session's resolved ACTIVE pane, targeted by
				// pane ID, with a pinned broker-owned depth and no -e. The
				// shape pin holds the exact invocation so the verb can neither
				// grow flags nor migrate to a name-based or writable form, and
				// the preview stays a stateless read that stores nothing.
				previewCaptureShapeChecked = bytes.Contains(b, []byte(`tmuxOutput(server, "capture-pane", "-p", "-e", "-t", d.PaneID, "-S", strconv.Itoa(-proto.PreviewRowLimit), "-E", "-")`))
			}
			creates := bytes.Count(b, []byte("new"+"-session"))
			destroys := bytes.Count(b, []byte("kill"+"-session"))
			// Two distinct session lifecycles, deliberately not conflated.
			//
			// Destroying a tmux session is the shadow transaction's exclusive right:
			// nothing else in the product may remove a session, including the
			// operator-creation path, because a session Persea Terminal did not
			// create is a session it must never delete.
			//
			// Creating one is permitted in exactly two audited files: attachment.go
			// for internal shadows, and session_create.go for operator-requested
			// sessions. The counters below stay scoped to attachment.go so the exact
			// shadow-lifecycle assertion keeps its original strength.
			if destroys != 0 && base != "attachment.go" {
				t.Fatalf("session destruction escaped canonical attachment transaction: %s", path)
			}
			if creates != 0 && base != "attachment.go" && base != "session_create.go" {
				t.Fatalf("session creation escaped its two audited callsites: %s", path)
			}
			if base == "session_create.go" {
				operatorCreateCalls += creates
			}
			if base == "attachment.go" {
				shadowCreateCalls += creates
				shadowDestroyCalls += destroys
			}
			resizes := bytes.Count(b, []byte("resize"+"-window"))
			setSizes := bytes.Count(b, []byte("pty.Set"+"size"))
			if (resizes != 0 || setSizes != 0) && filepath.Base(path) != "attachment.go" {
				t.Fatalf("explicit resize escaped canonical attachment transaction: %s", path)
			}
			resizeWindowCalls += resizes
			ptySetSizeCalls += setSizes
			if filepath.Base(path) == "attachment.go" {
				resizeGuardChecked = bytes.Contains(b, []byte("window_panes")) &&
					bytes.Contains(b, []byte("exactWitness")) &&
					bytes.Contains(b, []byte("terminal.ActionResize"))
				// The issuer seam may relocate where the guarded command is issued.
				// It may not relocate the guard, the witness recheck, or the PTY
				// resize, and it owns the release obligation itself.
				issuerSeamChecked = bytes.Contains(b, []byte("guardedResizeIssuer")) &&
					bytes.Contains(b, []byte("defer ticket.Release()"))
			}
			// Every observer-owned retention wait must keep decoding, including
			// rollback after the tmux command blocks have completed. Only the
			// external blocking helper's declaration and body may remain here.
			blockingBoundaryCallsites += bytes.Count(b, []byte(".Boundary("))
			blockingBoundaryHelpers += bytes.Count(b, []byte("retentionBoundary("))
			if base == "unified_rotation.go" {
				rotationRestoreBoundaryChecked = bytes.Count(b, []byte(`rotation.registry.retention.startBoundary(rotation.oldKey, "rotation_restore", false)`)) == 1 &&
					bytes.Contains(b, []byte(`err = rotation.waitDependency(nil, done)`))
			}
			if strings.HasSuffix(path, filepath.Join("broker", "server.go")) {
				// The one narrow exception to config-owned geometry: an explicit
				// vertical Fit for the unified-dev target, with columns derived from
				// the exact pane witness rather than trusted from the browser.
				verticalFitAuthorityChecked = bytes.Contains(b, []byte("terminal.ValidVerticalFit")) &&
					bytes.Contains(b, []byte("current.Columns != typed.Columns")) &&
					bytes.Contains(b, []byte("unifiedGeometryIssuer")) &&
					!bytes.Contains(b, []byte("resize"+"_disabled"))
			}
			if strings.HasPrefix(path, uiSourceRoot+string(os.PathSeparator)) && bytes.Contains(b, []byte("inner"+"HTML")) {
				t.Fatalf("unsafe DOM sink in %s", path)
			}

			return nil
		})
	}
	// Seven capture occurrences: the guarded legacy snapshot (tmux.go), the cut
	// transaction (attachment.go), two error strings naming the verb
	// (terminal/history.go), the adoption composite's two sub-commands —
	// the history+screen capture and the pending-prefix -P capture
	// (unified_dev.go) — and the dashboard preview's single read-only
	// plain-text capture (session_preview.go), pinned by shape above.
	if captureCalls != 7 || attachCalls != 4 {
		t.Fatalf("guarded tmux callsites: capture=%d attach=%d", captureCalls, attachCalls)
	}
	if !previewCaptureShapeChecked {
		t.Fatal("dashboard preview capture lost its pinned read-only shape")
	}
	if unifiedObserverAttachCalls != 2 || legacyAttachCalls != 2 || !unifiedUnitSpawnShapeChecked || !unifiedAdoptionAttachShapeChecked || !unifiedRecoveryAttachShapeChecked {
		t.Fatalf("guarded tmux attach ownership: unified=%d legacy=%d unit_spawn_shape=%t adoption_attach_shape=%t recovery_attach_shape=%t",
			unifiedObserverAttachCalls, legacyAttachCalls, unifiedUnitSpawnShapeChecked, unifiedAdoptionAttachShapeChecked, unifiedRecoveryAttachShapeChecked)
	}
	// One destroy is the normal transaction finalizer; the second is the
	// exact-identity startup reaper for a shadow orphaned by process death.
	if shadowCreateCalls != 1 || shadowDestroyCalls != 2 {
		t.Fatalf("exact-shadow lifecycle callsites: create=%d destroy=%d", shadowCreateCalls, shadowDestroyCalls)
	}
	// Operator session creation is a single bounded callsite, so it cannot
	// proliferate into several partially-guarded paths.
	if operatorCreateCalls != 1 {
		t.Fatalf("operator session-creation callsites: create=%d", operatorCreateCalls)
	}
	if resizeWindowCalls != 1 || ptySetSizeCalls != 1 || !resizeGuardChecked {
		t.Fatalf("guarded explicit resize callsites: resize-window=%d pty.Setsize=%d guard=%v", resizeWindowCalls, ptySetSizeCalls, resizeGuardChecked)
	}
	if !issuerSeamChecked || !verticalFitAuthorityChecked {
		t.Fatalf("explicit vertical fit authority: issuer_seam=%v vertical_fit=%v", issuerSeamChecked, verticalFitAuthorityChecked)
	}
	// The external helper's body is the sole direct Boundary call; its
	// declaration is the only helper occurrence. No observer lifecycle may use
	// that blocking helper, including the pinned rotation rollback path.
	if blockingBoundaryCallsites != 1 || blockingBoundaryHelpers != 1 || !rotationRestoreBoundaryChecked {
		t.Fatalf("blocking retention Boundary product callsites: boundary=%d helper=%d rotation_restore=%v", blockingBoundaryCallsites, blockingBoundaryHelpers, rotationRestoreBoundaryChecked)
	}
}

type disposable struct {
	t    *testing.T
	tmux config.TmuxServer
	path string
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ptb-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newDisposable(t *testing.T) *disposable {
	t.Helper()
	if _, e := exec.LookPath("tmux"); e != nil {
		t.Skip("tmux unavailable")
	}
	dir := shortTempDir(t)
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	d := &disposable{t: t, tmux: config.TmuxServer{Label: "test", SocketPath: filepath.Join(dir, "tmux.sock")}}
	d.path = d.tmux.SocketPath
	d.run("new-session", "-d", "-s", "alpha", "-x", "80", "-y", "24", "sleep 600")
	t.Cleanup(func() { _, _ = tmuxCombinedOutput(d.path, "kill-server") })
	return d
}
func tmuxCombinedOutput(path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	all := append([]string{"-S", path}, args...)
	return exec.CommandContext(ctx, "tmux", all...).CombinedOutput()
}
func (d *disposable) run(args ...string) string {
	d.t.Helper()
	out, e := tmuxCombinedOutput(d.path, args...)
	if e != nil {
		d.t.Fatalf("tmux %v: %v %s", args, e, out)
	}
	return strings.TrimSpace(string(out))
}
func startBrokerTest(t *testing.T, cfg config.Broker, path string) (net.Listener, chan error) {
	t.Helper()
	_ = os.Remove(path)
	l, e := net.Listen("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- Serve(l, cfg) }()
	return l, done
}
func request(t *testing.T, path string, c proto.Control) (net.Conn, proto.Control) {
	t.Helper()
	if c.Type == "attach" && c.HistoryLimit == nil {
		limit := terminal.DefaultHistoryRows
		c.HistoryLimit = &limit
	}
	conn, e := net.Dial("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	send := func(m proto.Control) {
		p, _ := json.Marshal(m)
		if e := proto.WriteFrame(conn, proto.FrameControl, p); e != nil {
			t.Fatal(e)
		}
	}
	read := func() proto.Control {
		f, e := proto.ReadFrame(conn)
		if e != nil {
			t.Fatal(e)
		}
		m, e := proto.DecodeControl(f.Payload)
		if e != nil {
			t.Fatal(e)
		}
		return m
	}
	send(proto.Control{Type: "hello", V: 1})
	if m := read(); m.Type != "hello_ok" {
		t.Fatal(m)
	}
	send(c)
	return conn, read()
}
func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition timed out")
}

func TestWrongFrontUIDDeniedBeforeProtocolTraffic(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "denied.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan struct{})
	accepted := make(chan struct{})
	go func() {
		c, e := l.Accept()
		if e == nil {
			close(accepted)
			s := &Server{config: config.Broker{FrontUID: uint32(os.Getuid()) + 1}}
			s.handleConn(c)
		}
		close(done)
	}()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	<-accepted
	if writeErr := proto.WriteFrame(c, proto.FrameControl, []byte(`{"type":"hello","v":1}`)); writeErr != nil &&
		!errors.Is(writeErr, io.EOF) && !errors.Is(writeErr, syscall.EPIPE) && !errors.Is(writeErr, syscall.ECONNRESET) {
		t.Fatalf("unexpected hello write failure: %v", writeErr)
	}
	b := make([]byte, 1)
	if n, readErr := c.Read(b); n != 0 {
		t.Fatalf("broker emitted protocol oracle: %x", b[:n])
	} else if readErr == nil {
		t.Fatal("zero-byte read without denial error")
	}
	<-done
}

func TestDisposableRenameStaleAndLifecycle(t *testing.T) {
	d := newDisposable(t)
	cfg := config.Broker{Realm: "local", Servers: []config.TmuxServer{d.tmux}}
	brokerPath := filepath.Join(filepath.Dir(d.path), "broker.sock")
	listener, _ := startBrokerTest(t, cfg, brokerPath)
	defer listener.Close()
	conn, inventory := request(t, brokerPath, proto.Control{Type: "inventory"})
	conn.Close()
	if inventory.Type != "inventory_ok" || len(inventory.Servers) != 1 || len(inventory.Servers[0].Sessions) != 1 {
		t.Fatalf("inventory: %+v", inventory)
	}
	a := inventory.Servers[0].Sessions[0].Authority
	d.run("rename-session", "-t", "alpha", "renamed")
	rc, ri := request(t, brokerPath, proto.Control{Type: "inventory"})
	rc.Close()
	rs := ri.Servers[0].Sessions[0]
	if rs.Name != "renamed" || rs.Authority.SessionID != a.SessionID {
		t.Fatalf("rename inventory mismatch: %+v", rs)
	}
	hc, hm := request(t, brokerPath, proto.Control{Type: "history", Authority: &a})
	if hm.Type != "history_ok" {
		t.Fatalf("rename did not follow id: %+v", hm)
	}
	f, e := proto.ReadFrame(hc)
	hc.Close()
	if e != nil || f.Type != proto.FrameData {
		t.Fatal("history data missing")
	}
	listener.Close()
	waitFor(t, func() bool { _, e := net.Dial("unix", brokerPath); return e != nil })
	listener, _ = startBrokerTest(t, cfg, brokerPath)
	defer listener.Close()
	if d.run("has-session", "-t", "=$0"); false {
	} // successful command is the assertion
	d.run("kill-server")
	time.Sleep(100 * time.Millisecond)
	_ = os.Remove(d.path)
	d.run("new-session", "-d", "-s", "renamed", "sleep 600")
	sc, stale := request(t, brokerPath, proto.Control{Type: "history", Authority: &a})
	sc.Close()
	if stale.Code != "stale_target" {
		t.Fatalf("old incarnation accepted: %+v", stale)
	}
}

func TestRevalidateBindsCreationUIDAndSelector(t *testing.T) {
	d := newDisposable(t)
	a, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	a.Realm, a.Server = "local", d.tmux.Label
	detail, err := details(d.tmux, "$0")
	if err != nil {
		t.Fatal(err)
	}
	a.SessionID, a.SessionCreated = detail.ID, detail.Created
	if _, err = revalidate("local", d.tmux, a); err != nil {
		t.Fatalf("complete authority rejected: %v", err)
	}
	mutations := []func(*proto.Authority){
		func(x *proto.Authority) { x.SessionCreated++ },
		func(x *proto.Authority) { x.UID++ },
		func(x *proto.Authority) { x.SelectorValue += "-other" },
	}
	server := &Server{config: config.Broker{Realm: "local", Servers: []config.TmuxServer{d.tmux}}}
	for i, mutate := range mutations {
		changed := a
		mutate(&changed)
		if _, err = revalidate("local", d.tmux, changed); !errors.Is(err, errStaleTarget) {
			t.Fatalf("mutation %d revalidate=%v", i, err)
		}
		if i > 0 {
			if _, err = server.configured(&changed); !errors.Is(err, errStaleTarget) {
				t.Fatalf("mutation %d configured=%v", i, err)
			}
		}
	}
	condition := incarnationCondition(a)
	if !strings.Contains(condition, strconv.FormatInt(a.SessionCreated, 10)) || !strings.Contains(condition, strconv.FormatUint(uint64(a.UID), 10)) {
		t.Fatalf("guard does not bind session creation and UID: %s", condition)
	}
}

func TestSameClientRestartInterleavingHistoryAndAttachFailClosed(t *testing.T) {
	d := newDisposable(t)
	inc, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	a := inc
	a.Realm = "local"
	a.Server = d.tmux.Label
	a.SessionID = "$0"
	a.SessionCreated, _ = strconv.ParseInt(d.run("display-message", "-p", "-t", "=$0:", "#{session_created}"), 10, 64)
	detailsBeforeRestart, err := revalidate("local", d.tmux, a)
	if err != nil {
		t.Fatalf("old authority did not revalidate: %v", err)
	}
	d.run("kill-server")
	_ = os.Remove(d.path)
	d.run("new-session", "-d", "-s", "replacement", "sh", "-c", "printf replacement-incarnation; exec sleep 600")
	if got := d.run("display-message", "-p", "-t", "=$0:", "#{session_id}"); got != "$0" {
		t.Fatalf("replacement did not reuse $0: %q", got)
	}
	if replacement := d.run("capture-pane", "-p", "-t", "=$0:"); !strings.Contains(replacement, "replacement-incarnation") {
		t.Fatalf("replacement evidence missing: %q", replacement)
	}

	cfg := unifiedAdoptionDevConfig(t, d.tmux, t.TempDir(), 0)
	cfg.Realm = "local"
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = effects.realm.Close() })
	s := &Server{config: cfg, unified: effects}
	var history bytes.Buffer
	s.historyValidated(&lockedWriter{w: &history}, d.tmux, detailsBeforeRestart, a)
	f, err := proto.ReadFrame(&history)
	if err != nil || f.Type != proto.FrameControl {
		t.Fatalf("history response missing: %v", err)
	}
	historyControl, err := proto.DecodeControl(f.Payload)
	if err != nil || historyControl.Code != "stale_target" {
		t.Fatalf("history did not fail stale: %+v %v", historyControl, err)
	}
	if bytes.Contains(history.Bytes(), []byte("replacement-incarnation")) {
		t.Fatal("history exposed replacement output")
	}

	brokerSide, clientSide := net.Pipe()
	historyLimit := terminal.DefaultHistoryRows
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.attachValidated(brokerSide, &lockedWriter{w: brokerSide}, d.tmux, detailsBeforeRestart, proto.Control{Type: "attach", Engine: "unified-dev", Authority: &a, Mode: "observe", HistoryLimit: &historyLimit})
		brokerSide.Close()
	}()
	attachFrame, err := proto.ReadFrame(clientSide)
	if err != nil || attachFrame.Type != proto.FrameControl {
		t.Fatalf("attach response missing: %v", err)
	}
	attachControl, err := proto.DecodeControl(attachFrame.Payload)
	if err != nil || attachControl.Code != "stale_target" || attachControl.Type == "attach_ok" {
		t.Fatalf("attach did not return stale before attach_ok: %+v %v", attachControl, err)
	}
	_ = clientSide.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	extra, readErr := io.ReadAll(clientSide)
	clientSide.Close()
	<-done
	if len(extra) != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		t.Fatalf("attach emitted replacement/internal bytes: %q %v", extra, readErr)
	}
	clients, err := tmuxCombinedOutput(d.path, "list-clients", "-F", "#{client_pid}")
	if err == nil && len(bytes.TrimSpace(clients)) != 0 {
		t.Fatalf("replacement gained a client: %q", clients)
	}
}

func TestTargetRemovalWithKeeperHistoryAndAttachAreStale(t *testing.T) {
	d := newDisposable(t)
	d.run("new-session", "-d", "-s", "keeper", "sleep 600")
	inc, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	a := inc
	a.Realm = "local"
	a.Server = d.tmux.Label
	a.SessionID = "$0"
	a.SessionCreated, _ = strconv.ParseInt(d.run("display-message", "-p", "-t", "=$0:", "#{session_created}"), 10, 64)
	detailsBeforeRemoval, err := revalidate("local", d.tmux, a)
	if err != nil {
		t.Fatalf("authority did not revalidate before removal: %v", err)
	}
	keeperID := d.run("display-message", "-p", "-t", "keeper:", "#{session_id}")
	d.run("kill-session", "-t", "=$0")

	cfg := unifiedAdoptionDevConfig(t, d.tmux, t.TempDir(), 0)
	cfg.Realm = "local"
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = effects.realm.Close() })
	s := &Server{config: cfg, unified: effects}
	var history bytes.Buffer
	s.historyValidated(&lockedWriter{w: &history}, d.tmux, detailsBeforeRemoval, a)
	historyFrame, err := proto.ReadFrame(&history)
	if err != nil || historyFrame.Type != proto.FrameControl {
		t.Fatalf("history response missing: %v", err)
	}
	historyControl, err := proto.DecodeControl(historyFrame.Payload)
	if err != nil || historyControl.Code != "stale_target" {
		t.Fatalf("history did not classify removed target as stale: %+v %v", historyControl, err)
	}

	brokerSide, clientSide := net.Pipe()
	historyLimit := terminal.DefaultHistoryRows
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.attachValidated(brokerSide, &lockedWriter{w: brokerSide}, d.tmux, detailsBeforeRemoval, proto.Control{Type: "attach", Engine: "unified-dev", Authority: &a, Mode: "observe", HistoryLimit: &historyLimit})
		_ = brokerSide.Close()
	}()
	attachFrame, err := proto.ReadFrame(clientSide)
	if err != nil || attachFrame.Type != proto.FrameControl {
		t.Fatalf("attach response missing: %v", err)
	}
	attachControl, err := proto.DecodeControl(attachFrame.Payload)
	if err != nil || attachControl.Code != "stale_target" || attachControl.Type == "attach_ok" {
		t.Fatalf("attach did not return stale first: %+v %v", attachControl, err)
	}
	extra, readErr := io.ReadAll(clientSide)
	_ = clientSide.Close()
	<-done
	if len(extra) != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		t.Fatalf("attach emitted data or handshake bytes: %q %v", extra, readErr)
	}
	clients, clientsErr := tmuxCombinedOutput(d.path, "list-clients", "-F", "#{client_pid}")
	if clientsErr == nil && len(bytes.TrimSpace(clients)) != 0 {
		t.Fatalf("removed target gained a tmux client: %q", clients)
	}
	if got := d.run("display-message", "-p", "-t", "="+keeperID+":", "#{session_id}"); got != keeperID {
		t.Fatalf("keeper changed: got %q want %q", got, keeperID)
	}
}

func TestAttachHandshakeClassification(t *testing.T) {
	d := newDisposable(t)
	inc, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	a := inc
	a.Realm = "local"
	a.Server = d.tmux.Label
	a.SessionID = "$0"
	a.SessionCreated, _ = strconv.ParseInt(d.run("display-message", "-p", "-t", "=$0:", "#{session_created}"), 10, 64)

	t.Run("split_sentinel", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		sentinel := "PERSEA_split_STALE"
		go func() {
			_, _ = w.Write([]byte(sentinel[:7]))
			_, _ = w.Write([]byte(sentinel[7:13]))
			_, _ = w.Write([]byte(sentinel[13:] + "\r\n"))
			_ = w.Close()
		}()
		initial, staleOutcome, err := readAttachHandshake(r, sentinel)
		if err != nil || !staleOutcome || len(initial) != 0 {
			t.Fatalf("split sentinel was not consumed: initial=%q stale=%v err=%v", initial, staleOutcome, err)
		}
	})

	t.Run("plain_text_healthy", func(t *testing.T) {
		handshakeErr := pipeHandshakeError(t, []byte("plain pre-native diagnostic\r\n"), "PERSEA_plain_STALE", true)
		_, revalidateErr := revalidate("local", d.tmux, a)
		if got := attachHandshakeFailureCode(handshakeErr, revalidateErr); got != "attach_failed" {
			t.Fatalf("healthy plain-text failure classified as %q", got)
		}
	})

	t.Run("eof_healthy", func(t *testing.T) {
		handshakeErr := pipeHandshakeError(t, nil, "PERSEA_eof_STALE", true)
		_, revalidateErr := revalidate("local", d.tmux, a)
		if got := attachHandshakeFailureCode(handshakeErr, revalidateErr); got != "attach_failed" {
			t.Fatalf("healthy EOF classified as %q", got)
		}
	})

	t.Run("timeout_stale", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		d.run("kill-session", "-t", "=$0")
		_, staleOutcome, handshakeErr := readAttachHandshake(r, "PERSEA_timeout_STALE")
		if handshakeErr == nil || staleOutcome {
			t.Fatalf("timeout did not produce a handshake error: stale=%v err=%v", staleOutcome, handshakeErr)
		}
		_, revalidateErr := revalidate("local", d.tmux, a)
		if got := attachHandshakeFailureCode(handshakeErr, revalidateErr); got != "stale_target" {
			t.Fatalf("stale timeout classified as %q", got)
		}
	})
}

func pipeHandshakeError(t *testing.T, payload []byte, sentinel string, closeWriter bool) error {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if closeWriter {
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		defer w.Close()
	}
	_, staleOutcome, handshakeErr := readAttachHandshake(r, sentinel)
	if handshakeErr == nil || staleOutcome {
		t.Fatalf("expected handshake error: stale=%v err=%v", staleOutcome, handshakeErr)
	}
	return handshakeErr
}

func TestBrokerSetupDeadlineClosesIncompleteRequest(t *testing.T) {
	dir := shortTempDir(t)
	listener, _ := startBrokerTest(t, config.Broker{Realm: "r"}, filepath.Join(dir, "broker.sock"))
	defer listener.Close()
	conn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := proto.MarshalControl(proto.Control{Type: "hello", V: 1})
	if err := proto.WriteFrame(conn, proto.FrameControl, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := proto.ReadFrame(conn); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte{byte(proto.FrameControl), 0, 0}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(SetupTimeout + time.Second))
	started := time.Now()
	_, err = proto.ReadFrame(conn)
	conn.Close()
	if err == nil || time.Since(started) > SetupTimeout+750*time.Millisecond {
		t.Fatalf("incomplete request was not closed on setup deadline: %v after %s", err, time.Since(started))
	}
}
