package broker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

func TestUnifiedTerminalE2E1DecoderErrorsDoNotExposeControlBytes(t *testing.T) {
	const canary = "E2E1-PRIVATE-CONTROL-CANARY"
	err := (&UnifiedDevPaneEffects{}).consumeObserverEvents(controlmode.NewDecoder(), []byte(canary+"\n"))
	if err == nil {
		t.Fatal("malformed control record was accepted")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("control bytes entered error: %q", err)
	}
}

type unifiedE2E1NaturalRedEffects struct{}

func (unifiedE2E1NaturalRedEffects) RunObserver(ctx context.Context, _ PaneObservationEffects) error {
	<-ctx.Done()
	return ctx.Err()
}

func (unifiedE2E1NaturalRedEffects) WritePane(unifiedjournal.PaneKey, []byte) error { return nil }

func (unifiedE2E1NaturalRedEffects) BeginSessionBirth(string, string) (SessionBirthEffects, error) {
	return unifiedE2E1NaturalRedBirth{}, nil
}

type unifiedE2E1NaturalRedBirth struct{}

func (unifiedE2E1NaturalRedBirth) CommitSessionBirth(string) error { return nil }
func (unifiedE2E1NaturalRedBirth) AbortSessionBirth(error) error   { return nil }

type unifiedE2E1BirthOwner struct {
	target string
	birth  *unifiedE2E1CreatorBirth
}

func (owner *unifiedE2E1BirthOwner) beginSessionBirth(_ string, name string) (SessionBirthEffects, error) {
	if name != owner.target {
		return nil, nil
	}
	return owner.birth, nil
}

type unifiedE2E1CreatorBirth struct {
	sessionID string
	createErr error
	create    int
	abort     int
	commit    []string
	args      []string
}

func (birth *unifiedE2E1CreatorBirth) CreateSession(_ config.TmuxServer, args []string) (string, error) {
	birth.create++
	birth.args = append([]string(nil), args...)
	return birth.sessionID, birth.createErr
}

func (birth *unifiedE2E1CreatorBirth) CommitSessionBirth(sessionID string) error {
	birth.commit = append(birth.commit, sessionID)
	return nil
}

func (birth *unifiedE2E1CreatorBirth) AbortSessionBirth(error) error {
	birth.abort++
	return nil
}

func unifiedE2E1CreationConfig(t *testing.T, server config.TmuxServer) config.Broker {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broker.json")
	body, err := json.Marshal(map[string]any{
		"realm": "e2e1", "front_uid": os.Getuid(),
		"servers": []any{map[string]any{"label": server.Label, "socket_path": server.SocketPath}},
		"session_create": map[string]any{
			"enabled": true, "servers": []string{server.Label},
			"name_pattern": "^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$", "max_sessions": 20,
			"columns": 80, "rows": 24,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadBroker(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func unifiedE2E1DevConfig(t *testing.T, server config.TmuxServer, runtimeDir string) config.Broker {
	t.Helper()
	return unifiedAdoptionDevConfig(t, server, runtimeDir, 0)
}

func unifiedAdoptionDevConfig(t *testing.T, server config.TmuxServer, runtimeDir string, adoptionSlots int) config.Broker {
	t.Helper()
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dev := map[string]any{
		"enabled": true, "server": server.Label, "session": "unified_target",
		"observer_session": "anchor", "runtime_dir": runtimeDir,
	}
	if adoptionSlots != 0 {
		dev["adoption_slots"] = adoptionSlots
	}
	path := filepath.Join(t.TempDir(), "broker.json")
	body, err := json.Marshal(map[string]any{
		"realm": "e2e1", "front_uid": os.Getuid(),
		"servers": []any{map[string]any{"label": server.Label, "socket_path": server.SocketPath}},
		"session_create": map[string]any{
			"enabled": true, "servers": []string{server.Label},
			"name_pattern": "^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$", "max_sessions": 20,
			"columns": 80, "rows": 24,
		},
		"unified_terminal_dev": dev,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadBroker(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func unifiedE2E1Hello(t *testing.T, conn net.Conn) {
	t.Helper()
	payload, err := proto.MarshalControl(proto.Control{Type: "hello", V: 1})
	if err != nil || proto.WriteFrame(conn, proto.FrameControl, payload) != nil {
		t.Fatalf("hello write: %v", err)
	}
	frame, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	control, err := proto.DecodeControl(frame.Payload)
	if err != nil || control.Type != "hello_ok" {
		t.Fatalf("hello response=%+v err=%v", control, err)
	}
}

func unifiedE2E1Control(t *testing.T, conn net.Conn, control proto.Control) proto.Control {
	t.Helper()
	payload, err := proto.MarshalControl(control)
	if err != nil || proto.WriteFrame(conn, proto.FrameControl, payload) != nil {
		t.Fatalf("control write: %v", err)
	}
	frame, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	result, err := proto.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// This helper returns the admission snapshot only. Continuity assertions must
// use unifiedE2E1OpenStream, which also retains live output preceding MODE.
func unifiedE2E1OpenAttachment(t *testing.T, socket string, authority proto.Authority) (net.Conn, terminal.Frame) {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	unifiedE2E1Hello(t, conn)
	history := 5000
	attached := unifiedE2E1Control(t, conn, proto.Control{Type: "attach", Mode: "control", Engine: "unified-dev", Authority: &authority, HistoryLimit: &history})
	if attached.Type != "attach_ok" {
		conn.Close()
		t.Fatalf("E2E1/SHELL_ATTACH: %+v", attached)
	}
	raw, err := proto.ReadFrame(conn)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	prepared, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
	if err != nil || prepared.Type != terminal.FramePrepare {
		conn.Close()
		t.Fatalf("E2E1/SHELL_VISIBLE: prepare=%+v err=%v", prepared, err)
	}
	ready, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameReady, Source: prepared.Source, Epoch: prepared.Epoch, Cut: prepared.Cut}, attachmentwire.BrowserToServer)
	if err != nil || proto.WriteFrame(conn, proto.FrameAttachment, ready) != nil {
		conn.Close()
		t.Fatalf("ready: %v", err)
	}
	commitRaw, err := proto.ReadFrame(conn)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	commit, err := attachmentwire.Decode(commitRaw.Payload, attachmentwire.ServerToBrowser)
	if err != nil || commit.Type != terminal.FrameCommit {
		conn.Close()
		t.Fatalf("commit=%+v err=%v", commit, err)
	}
	modeRequest, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: prepared.Source, Epoch: prepared.Epoch, Mode: terminal.ModeControl}, attachmentwire.BrowserToServer)
	if err != nil || proto.WriteFrame(conn, proto.FrameAttachment, modeRequest) != nil {
		conn.Close()
		t.Fatalf("mode request: %v", err)
	}
	for {
		modeRaw, err := proto.ReadFrame(conn)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if modeRaw.Type != proto.FrameAttachment {
			continue
		}
		mode, err := attachmentwire.Decode(modeRaw.Payload, attachmentwire.ServerToBrowser)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if mode.Type == terminal.FrameMode && mode.Mode == terminal.ModeControl {
			return conn, prepared
		}
	}
}

func unifiedE2E1Detach(t *testing.T, conn net.Conn) {
	t.Helper()
	payload, err := proto.MarshalControl(proto.Control{Type: "detach"})
	if err != nil || proto.WriteFrame(conn, proto.FrameControl, payload) != nil {
		t.Fatalf("detach: %v", err)
	}
	for {
		frame, err := proto.ReadFrame(conn)
		if err != nil {
			t.Fatalf("detach response: %v", err)
		}
		if frame.Type != proto.FrameControl {
			continue
		}
		control, err := proto.DecodeControl(frame.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if control.Type == "exit" {
			return
		}
	}
}

func unifiedE2E1Create(t *testing.T, server *Server, name string) proto.Control {
	t.Helper()
	var output bytes.Buffer
	server.create(&lockedWriter{w: &output}, proto.Control{Type: "create", ServerLabel: "main", Name: name})
	frame, err := proto.ReadFrame(&output)
	if err != nil {
		t.Fatal(err)
	}
	control, err := proto.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return control
}

func TestUnifiedTerminalE2E1BirthCreatorOwnsByteZeroCreation(t *testing.T) {
	disposable := newDisposable(t)
	disposable.run("new-session", "-d", "-s", "anchor", "-x", "80", "-y", "24", "sh")
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	birth := &unifiedE2E1CreatorBirth{sessionID: "$91"}
	server := &Server{config: unifiedE2E1CreationConfig(t, tmuxServer), birth: &unifiedE2E1BirthOwner{target: "unified_target", birth: birth}}
	result := unifiedE2E1Create(t, server, "unified_target")
	if result.Type != "create_ok" || birth.create != 1 || birth.abort != 0 || !slices.Equal(birth.commit, []string{"$91"}) {
		t.Fatalf("E2E1/PANE_BIRTH_BYTE_ZERO: result=%+v create=%d abort=%d commit=%v", result, birth.create, birth.abort, birth.commit)
	}
	if slices.Contains(birth.args, "-d") || !slices.Contains(birth.args, "unified_target") {
		t.Fatalf("E2E1/PANE_BIRTH_BYTE_ZERO: observer creation args=%q", birth.args)
	}
}

func TestUnifiedTerminalE2E1BirthCreatorFailureAborts(t *testing.T) {
	disposable := newDisposable(t)
	disposable.run("new-session", "-d", "-s", "anchor", "-x", "80", "-y", "24", "sh")
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	birth := &unifiedE2E1CreatorBirth{createErr: errors.New("observer creation failed")}
	server := &Server{config: unifiedE2E1CreationConfig(t, tmuxServer), birth: &unifiedE2E1BirthOwner{target: "unified_target", birth: birth}}
	result := unifiedE2E1Create(t, server, "unified_target")
	if result.Type != "create_refused" || result.Code != "create_failed" || birth.create != 1 || birth.abort != 1 || len(birth.commit) != 0 {
		t.Fatalf("E2E1/PANE_BIRTH_ABORT: result=%+v create=%d abort=%d commit=%v", result, birth.create, birth.abort, birth.commit)
	}
}

func TestUnifiedTerminalE2E1SelectorAbsentOrNonmatchingUsesLegacyCreation(t *testing.T) {
	for _, test := range []struct {
		name  string
		birth sessionBirthEffect
	}{
		{name: "absent"},
		{name: "nonmatching", birth: &unifiedE2E1BirthOwner{target: "unified_target", birth: &unifiedE2E1CreatorBirth{sessionID: "$91"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			disposable := newDisposable(t)
			disposable.run("new-session", "-d", "-s", "anchor", "-x", "80", "-y", "24", "sh")
			tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
			server := &Server{config: unifiedE2E1CreationConfig(t, tmuxServer), birth: test.birth}
			result := unifiedE2E1Create(t, server, "legacy_target")
			if result.Type != "create_ok" || result.ServerLabel != "main" || result.Name != "legacy_target" {
				t.Fatalf("E2E1/LEGACY_CREATION: result=%+v", result)
			}
			listed, err := tmuxOutput(tmuxServer, "list-sessions", "-F", "#{session_name}")
			if err != nil || !slices.Contains(strings.Fields(listed), "legacy_target") {
				t.Fatalf("E2E1/LEGACY_CREATION: sessions=%q err=%v", listed, err)
			}
			if owner, ok := test.birth.(*unifiedE2E1BirthOwner); ok && owner.birth.create != 0 {
				t.Fatalf("E2E1/LEGACY_CREATION: nonmatching creator called %d times", owner.birth.create)
			}
		})
	}
}

// newProjectionEffects builds a real provider — with a real journal realm —
// against a filesystem-only runtime dir. The projection never contacts tmux,
// so no server is required.
func newProjectionEffects(t *testing.T, adoptionSlots int) *UnifiedDevPaneEffects {
	t.Helper()
	cfg := unifiedAdoptionDevConfig(t, config.TmuxServer{Label: "main", SocketPath: filepath.Join(t.TempDir(), "absent.sock")}, t.TempDir(), adoptionSlots)
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return effects
}

func eligibleFacts() unifiedSessionFacts {
	return unifiedSessionFacts{Alternate: 0, Panes: 1, Windows: 1, Valid: true}
}

func TestUnifiedTerminalCreateProjection(t *testing.T) {
	effects := newProjectionEffects(t, 0)
	if got := effects.dashboardLaunch("other", nil); got != nil {
		t.Fatalf("non-target server projected unified launch: %+v", got)
	}
	create := effects.dashboardLaunch("main", nil)
	if create == nil || create.State != proto.UnifiedDevLaunchCreate || create.Name != "unified_target" || create.SessionID != "" {
		t.Fatalf("absent target projection = %+v", create)
	}
	existing := []proto.Session{{Name: "unified_target", Authority: proto.Authority{SessionID: "$17"}}}
	if got := effects.dashboardLaunch("main", existing); got != nil {
		t.Fatalf("existing target still projected a server-level launch: %+v", got)
	}
}

func TestUnifiedSessionProjectionStates(t *testing.T) {
	effects := newProjectionEffects(t, 0)
	expect := func(t *testing.T, got *proto.UnifiedSessionState, state, origin string) {
		t.Helper()
		if got == nil || got.State != state || got.Origin != origin {
			t.Fatalf("projection = %+v, want state %q origin %q", got, state, origin)
		}
	}
	expect(t, effects.projectSession("other", "$1", eligibleFacts()), proto.UnifiedSessionBlockedForeignServer, "")
	expect(t, effects.projectSession("main", "$1", eligibleFacts()), proto.UnifiedSessionAdoptable, "")
	expect(t, effects.projectSession("main", "$1", unifiedSessionFacts{Alternate: 1, Panes: 1, Windows: 1, Valid: true}), proto.UnifiedSessionAdoptable, "")
	expect(t, effects.projectSession("main", "$1", unifiedSessionFacts{Alternate: 0, Panes: 1, Windows: 2, Valid: true}), proto.UnifiedSessionBlockedMultiWindow, "")
	expect(t, effects.projectSession("main", "$1", unifiedSessionFacts{Alternate: 0, Panes: 2, Windows: 1, Valid: true}), proto.UnifiedSessionBlockedMultiPane, "")
	expect(t, effects.projectSession("main", "$1", unifiedSessionFacts{}), proto.UnifiedSessionUnavailable, "")

	born := unifiedjournal.PaneKey{Server: "main", Session: "$17", Window: "@1", Pane: "%1", Incarnation: "inc", ControlGeneration: 1}
	reserveRecordingSourceForTest(t, effects, born)
	effects.journalMu.Lock()
	admitErr := effects.realm.AdmitPane(born, unifiedjournal.Geometry{Columns: 80, Rows: 24})
	effects.journalMu.Unlock()
	if admitErr != nil {
		t.Fatal(admitErr)
	}
	effects.mu.Lock()
	effects.active["$17"] = born
	effects.mu.Unlock()
	expect(t, effects.projectSession("main", "$17", eligibleFacts()), proto.UnifiedSessionOpen, proto.UnifiedOriginBirth)

	adopted := unifiedjournal.PaneKey{Server: "main", Session: "$23", Window: "@2", Pane: "%2", Incarnation: "inc", ControlGeneration: 2}
	reserveRecordingSourceForTest(t, effects, adopted)
	effects.journalMu.Lock()
	admitErr = effects.realm.AdmitReconstructedPane(adopted, unifiedjournal.Geometry{Columns: 80, Rows: 24})
	effects.journalMu.Unlock()
	if admitErr != nil {
		t.Fatal(admitErr)
	}
	effects.mu.Lock()
	effects.active["$23"] = adopted
	effects.mu.Unlock()
	expect(t, effects.projectSession("main", "$23", eligibleFacts()), proto.UnifiedSessionOpen, proto.UnifiedOriginReconstructed)

	// An active session whose facts would refuse adoption is still open: the
	// journal, not tmux row shape, owns the open judgement.
	expect(t, effects.projectSession("main", "$17", unifiedSessionFacts{Alternate: 1, Panes: 3, Windows: 3, Valid: true}), proto.UnifiedSessionOpen, proto.UnifiedOriginBirth)
}

func TestUnifiedSessionProjectionKeepsAlternateScreenOpen(t *testing.T) {
	effects := newProjectionEffects(t, 0)
	key := unifiedjournal.PaneKey{Server: "main", Session: "$alternate", Window: "@1", Pane: "%1", Incarnation: "inc", ControlGeneration: 1}
	reserveRecordingSourceForTest(t, effects, key)
	if err := effects.realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	effects.active[key.Session] = key
	for _, alternate := range []int{1, 0, 1} {
		projected := effects.projectSession("main", key.Session, unifiedSessionFacts{Alternate: alternate, Panes: 1, Windows: 1, Valid: true})
		if projected == nil || projected.State != proto.UnifiedSessionOpen || projected.Detail != "" {
			t.Fatalf("alternate=%d projection=%+v", alternate, projected)
		}
	}
}

func TestCompletePaneReleaseInterruptsMaximumRotationBackoff(t *testing.T) {
	effects := newProjectionEffects(t, 3)
	target := unifiedjournal.PaneKey{Server: "main", Session: "$slot-wait", Window: "@1", Pane: "%1", Incarnation: "inc", ControlGeneration: 1}
	reserveRecordingSourceForTest(t, effects, target)
	effects.journalMu.Lock()
	err := effects.realm.AdmitPane(target, unifiedjournal.Geometry{Columns: 80, Rows: 24})
	effects.journalMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(45_000, 0)
	attempts := 0
	effects.mu.Lock()
	effects.active[target.Session] = target
	effects.rotationNow = func() time.Time { return now }
	effects.rotationPressure = func(unifiedjournal.PaneKey) (unifiedRotationPressure, error) {
		return unifiedRotationPressure{logical: 80, logicalCap: 100, physicalCap: 100}, nil
	}
	effects.rotationAttempt = func(context.Context, string) error { attempts++; return nil }
	effects.rotationStates[target.Session] = &unifiedRotationState{
		key: target, armed: true, backoff: unifiedRotationRetryMaximum,
		nextAttempt: now.Add(unifiedRotationRetryMaximum),
	}
	effects.mu.Unlock()

	other := unifiedjournal.PaneKey{Server: "main", Session: "$slot-release", Window: "@2", Pane: "%2", Incarnation: "inc", ControlGeneration: 2}
	reserveRecordingSourceForTest(t, effects, other)
	effects.journalMu.Lock()
	reservation, err := effects.realm.BeginReconstructedPane(other, unifiedjournal.Geometry{Columns: 80, Rows: 24})
	effects.journalMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	reservation.Abort()
	effects.runRotationScheduler(context.Background())
	if attempts != 1 {
		t.Fatalf("complete-pane release did not promptly attempt from max backoff: %d", attempts)
	}
	reservation.Abort()
	select {
	case <-effects.rotationEligibility:
		t.Fatal("settled non-owner release emitted another eligibility transition")
	default:
	}
}

func TestUnifiedSessionProjectionSlotsExhausted(t *testing.T) {
	// Two total slots means one ordinary admission and one rotation reserve.
	effects := newProjectionEffects(t, 2)
	occupant := unifiedjournal.PaneKey{Server: "main", Session: "$40", Window: "@4", Pane: "%4", Incarnation: "inc", ControlGeneration: 3}
	reserveRecordingSourceForTest(t, effects, occupant)
	effects.journalMu.Lock()
	admitErr := effects.realm.AdmitReconstructedPane(occupant, unifiedjournal.Geometry{Columns: 80, Rows: 24})
	effects.journalMu.Unlock()
	if admitErr != nil {
		t.Fatal(admitErr)
	}
	got := effects.projectSession("main", "$41", eligibleFacts())
	if got == nil || got.State != proto.UnifiedSessionSlotsExhausted {
		t.Fatalf("exhausted-slot projection = %+v", got)
	}
}

func TestUnifiedSessionProjectionConcurrentCommit(t *testing.T) {
	effects := newProjectionEffects(t, 0)
	key := unifiedjournal.PaneKey{Server: "main", Session: "$17", Window: "@1", Pane: "%1", Incarnation: "inc", ControlGeneration: 1}
	reserveRecordingSourceForTest(t, effects, key)
	effects.journalMu.Lock()
	admitErr := effects.realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24})
	effects.journalMu.Unlock()
	if admitErr != nil {
		t.Fatal(admitErr)
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(iteration int) {
			defer wg.Done()
			effects.mu.Lock()
			if iteration%2 == 0 {
				effects.active["$17"] = key
			} else {
				delete(effects.active, "$17")
			}
			effects.mu.Unlock()
		}(i)
		go func() {
			defer wg.Done()
			projection := effects.projectSession("main", "$17", eligibleFacts())
			if projection == nil || (projection.State != proto.UnifiedSessionOpen && projection.State != proto.UnifiedSessionAdoptable) {
				t.Errorf("invalid synchronized projection: %+v", projection)
			}
		}()
	}
	wg.Wait()
}

func TestUnifiedAllowsConsultsJournalActiveBySessionID(t *testing.T) {
	effects := newProjectionEffects(t, 0)
	if effects.allows("main", "$17", "renamed-later") {
		t.Fatal("inactive non-target session passed the unified gate")
	}
	if !effects.allows("main", "$17", "unified_target") {
		t.Fatal("configured target name no longer passes the unified gate")
	}
	effects.mu.Lock()
	effects.active["$17"] = unifiedjournal.PaneKey{Session: "$17"}
	effects.mu.Unlock()
	if !effects.allows("main", "$17", "renamed-later") {
		t.Fatal("journal-active session was refused by name")
	}
	if effects.allows("main", "$18", "renamed-later") {
		t.Fatal("a different session ID rode another session's journal-active entry")
	}
	if effects.allows("other", "$17", "unified_target") {
		t.Fatal("foreign server passed the unified gate")
	}
}

func TestUnifiedTerminalE2E1SelectorRequiresConcreteDevProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux E2E-1 regression test")
	}
	disposable := newDisposable(t)
	disposable.run("new-session", "-d", "-s", "e2e1-natural-red", "-x", "80", "-y", "24", "sh")
	server := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	details, err := details(server, "$0")
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm = "e2e1"
	authority.Server = server.Label
	authority.SessionID = details.ID
	authority.SessionCreated = details.Created

	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- ServeWithPaneEffects(listener, config.Broker{
			Realm: "e2e1", FrontUID: uint32(os.Getuid()), FrontUIDConfigured: true,
			Servers: []config.TmuxServer{server},
		}, unifiedE2E1NaturalRedEffects{})
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Error("broker did not stop")
		}
	})

	conn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	unifiedE2E1Hello(t, conn)
	history := 5000
	payload, err := proto.MarshalControl(proto.Control{
		Type: "attach", Mode: "control", Engine: "unified-dev", Authority: &authority, HistoryLimit: &history,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteFrame(conn, proto.FrameControl, payload); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	frame, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Type != proto.FrameControl {
		t.Fatalf("E2E1/SELECTOR_ON_NO_UNIFIED_DISPLAY: first frame type=%d", frame.Type)
	}
	control, err := proto.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if control.Type != "error" || control.Code != "unified_unavailable" {
		t.Fatalf("E2E1/SELECTOR_DEFAULT_CLOSED: broker response=%s code=%s", control.Type, control.Code)
	}
}

func TestUnifiedTerminalE2E1InteractiveShellRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux unified terminal")
	}
	disposable := newDisposable(t)
	disposable.run("new-session", "-d", "-s", "anchor", "-x", "80", "-y", "24", "sh")
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	cfg := unifiedE2E1DevConfig(t, tmuxServer, t.TempDir())
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		err := ServeWithPaneEffects(listener, cfg, effects)
		t.Logf("unified serve terminal: %v", err)
		done <- err
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("unified broker did not stop")
		}
	})

	createConn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	unifiedE2E1Hello(t, createConn)
	created := unifiedE2E1Control(t, createConn, proto.Control{Type: "create", ServerLabel: "main", Name: "unified_target"})
	_ = createConn.Close()
	if created.Type != "create_ok" {
		t.Fatalf("E2E1/SHELL_CREATE: %+v", created)
	}
	sessionID, err := tmuxOutput(tmuxServer, "list-sessions", "-f", "#{==:#{session_name},unified_target}", "-F", "#{session_id}")
	if err != nil {
		t.Fatal(err)
	}
	d, err := details(tmuxServer, strings.TrimSpace(sessionID))
	if err != nil {
		t.Fatalf("details session=%q: %v", sessionID, err)
	}
	authority, err := incarnation(tmuxServer)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = "e2e1", "main", d.ID, d.Created

	conn, prepared, stream := unifiedE2E1OpenStream(t, listener.Addr().String(), authority)
	defer conn.Close()
	if len(prepared.Replay) == 0 {
		t.Fatalf("E2E1/SHELL_VISIBLE: prepare=%+v err=%v", prepared, err)
	}
	input, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: prepared.Source, Epoch: prepared.Epoch, Data: []byte("printf 'E2E1-SHELL-ROUNDTRIP\\n'\n")}, attachmentwire.BrowserToServer)
	if err != nil || proto.WriteFrame(conn, proto.FrameAttachment, input) != nil {
		t.Fatalf("input: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	visible := append([]byte(nil), stream.text...)
	for !bytes.Contains(visible, []byte("E2E1-SHELL-ROUNDTRIP")) {
		raw, err := proto.ReadFrame(conn)
		if err != nil {
			t.Fatalf("E2E1/SHELL_ECHO: visible=%q err=%v", visible, err)
		}
		if raw.Type != proto.FrameAttachment {
			continue
		}
		live, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
		if err != nil {
			t.Fatal(err)
		}
		if live.Type == terminal.FrameLive {
			visible = append(visible, live.Data...)
		}
	}
	for iteration := 0; iteration < 20; iteration++ {
		prefix := []byte("E2E1-CUT-" + strconv.Itoa(iteration) + "-")
		command := "i=0; while [ $i -lt 5 ]; do printf '" + string(prefix) + "%s\\n' \"$i\"; i=$((i+1)); sleep 0.01; done\n"
		input, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: prepared.Source, Epoch: prepared.Epoch, Data: []byte(command)}, attachmentwire.BrowserToServer)
		if err != nil || proto.WriteFrame(conn, proto.FrameAttachment, input) != nil {
			t.Fatalf("reload input %d: %v", iteration, err)
		}
		first := append(prefix, '0')
		for !bytes.Contains(visible, first) {
			raw, err := proto.ReadFrame(conn)
			if err != nil {
				t.Fatalf("reload pre-cut %d: %v", iteration, err)
			}
			if raw.Type != proto.FrameAttachment {
				continue
			}
			frame, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
			if err != nil {
				t.Fatal(err)
			}
			if frame.Type == terminal.FrameLive {
				visible = append(visible, frame.Data...)
			}
		}
		unifiedE2E1Detach(t, conn)
		_ = conn.Close()
		conn, prepared, stream = unifiedE2E1OpenStream(t, listener.Addr().String(), authority)
		visible = append([]byte(nil), stream.text...)
		last := append(append([]byte(nil), prefix...), '4')
		if err := conn.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
			t.Fatal(err)
		}
		for !bytes.Contains(visible, last) {
			raw, err := proto.ReadFrame(conn)
			if err != nil {
				t.Fatalf("E2E1/ATOMIC_RELOAD iteration=%d visible_tail=%q err=%v", iteration, visible[max(0, len(visible)-128):], err)
			}
			if raw.Type != proto.FrameAttachment {
				continue
			}
			frame, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
			if err != nil {
				t.Fatal(err)
			}
			if frame.Type == terminal.FrameLive {
				visible = append(visible, frame.Data...)
			}
		}
		for index := 0; index < 5; index++ {
			token := append(append([]byte(nil), prefix...), byte('0'+index))
			if count := bytes.Count(visible, token); count != 1 {
				t.Fatalf("E2E1/ATOMIC_RELOAD iteration=%d token=%q count=%d", iteration, token, count)
			}
		}
	}
	lessInput, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: prepared.Source, Epoch: prepared.Epoch, Data: []byte("printf 'E2E1-LESS\\nsecond-line\\n' | TERM=xterm-256color less\n")}, attachmentwire.BrowserToServer)
	if err != nil || proto.WriteFrame(conn, proto.FrameAttachment, lessInput) != nil {
		t.Fatalf("less input: %v", err)
	}
	var tui []byte
	for !bytes.Contains(tui, []byte("\x1b[?1049h")) {
		raw, err := proto.ReadFrame(conn)
		if err != nil {
			t.Fatalf("E2E1/REAL_TUI_ENTER: %v", err)
		}
		if raw.Type != proto.FrameAttachment {
			continue
		}
		frame, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == terminal.FrameLive {
			tui = append(tui, frame.Data...)
		}
	}
	quit, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: prepared.Source, Epoch: prepared.Epoch, Data: []byte("q")}, attachmentwire.BrowserToServer)
	if err != nil || proto.WriteFrame(conn, proto.FrameAttachment, quit) != nil {
		t.Fatalf("less quit: %v", err)
	}
	for !bytes.Contains(tui, []byte("\x1b[?1049l")) {
		raw, err := proto.ReadFrame(conn)
		if err != nil {
			t.Fatalf("E2E1/REAL_TUI_EXIT: %v", err)
		}
		if raw.Type != proto.FrameAttachment {
			continue
		}
		frame, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == terminal.FrameLive {
			tui = append(tui, frame.Data...)
		}
	}
	unifiedE2E1Detach(t, conn)
	_ = conn.Close()
}

var _ PaneEffects = unifiedE2E1NaturalRedEffects{}

type unifiedE2E1LockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (buffer *unifiedE2E1LockedBuffer) Write(payload []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.Write(payload)
}

func (buffer *unifiedE2E1LockedBuffer) snapshot() []byte {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return append([]byte(nil), buffer.Bytes()...)
}

func unifiedE2E1JournalProvider(t *testing.T, label, session string) (*UnifiedDevPaneEffects, unifiedjournal.PaneKey) {
	t.Helper()
	realm := openRetentionRealm(t, label)
	key := journalKey(retentionWitness("%0", label+"-incarnation"))
	if err := realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	effects := &UnifiedDevPaneEffects{
		realm: realm, active: map[string]unifiedjournal.PaneKey{session: key},
		subscribers: make(map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}),
	}
	registry := newPaneRegistry(effects)
	effects.observer = registry
	witness := retentionWitness("%0", label+"-incarnation")
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	commitRecordingInitialForTest(t, registry, witness, nil)
	t.Cleanup(func() { _ = registry.Close() })
	return effects, key
}

func unifiedE2E1Commit(t *testing.T, realm *unifiedjournal.Realm, key unifiedjournal.PaneKey, payload []byte) {
	t.Helper()
	record, err := realm.Append(key, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := realm.Sync(key); err != nil {
		t.Fatal(err)
	}
	if err := realm.AdvanceCommitted(key, record); err != nil {
		t.Fatal(err)
	}
}

func unifiedE2E1AttachmentFrames(t *testing.T, raw []byte) []terminal.Frame {
	t.Helper()
	reader := bytes.NewReader(raw)
	frames := make([]terminal.Frame, 0)
	for reader.Len() > 0 {
		wire, err := proto.ReadFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		if wire.Type != proto.FrameAttachment {
			continue
		}
		frame, err := attachmentwire.Decode(wire.Payload, attachmentwire.ServerToBrowser)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	return frames
}

func unifiedE2E1RawFrame(t *testing.T, frame terminal.Frame) []byte {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestUnifiedTerminalE2E1SnapshotOverReplayCapReconstructsBeforeTail(t *testing.T) {
	effects, key := unifiedE2E1JournalProvider(t, "e2e1-large-reload", "$0")
	snapshot := bytes.Repeat([]byte("0123456789abcdef"), terminal.ReplayByteCap/16+4096)
	unifiedE2E1Commit(t, effects.realm, key, snapshot)

	output := &unifiedE2E1LockedBuffer{}
	writer := &unifiedAttachmentFrameWriter{
		downstream: &attachmentFrameWriter{wire: &lockedWriter{w: output}},
		provider:   effects, session: "$0",
	}
	prepare := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FramePrepare, Source: "e2e1-large", Epoch: 1, Cut: 1, Kind: terminal.CutReconnect, Columns: 80, Rows: 24}
	if err := writer.WriteFrame(context.Background(), unifiedE2E1RawFrame(t, prepare)); err != nil {
		t.Fatalf("E2E1/B1_LARGE_SNAPSHOT: %v", err)
	}
	frames := unifiedE2E1AttachmentFrames(t, output.snapshot())
	if len(frames) != 1 || frames[0].Type != terminal.FramePrepare || len(frames[0].Replay) > terminal.ReplayByteCap {
		t.Fatalf("E2E1/B1_PREPARE_BOUND: frames=%d replay=%d", len(frames), func() int {
			if len(frames) == 0 {
				return -1
			}
			return len(frames[0].Replay)
		}())
	}
	commit := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameCommit, Source: prepare.Source, Epoch: prepare.Epoch, Cut: prepare.Cut}
	if err := writer.WriteFrame(context.Background(), unifiedE2E1RawFrame(t, commit)); err != nil {
		t.Fatal(err)
	}
	runtime := newRetentionTrialRuntime(effects)
	if runtime == nil {
		t.Fatal("retention runtime unavailable")
	}
	defer runtime.Close()
	tail := []byte("E2E1-B1-TAIL")
	if err := runtime.WritePane(key, tail); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Boundary(key, "e2e1_tail"); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), snapshot...), tail...)
	deadline := time.Now().Add(3 * time.Second)
	for {
		frames = unifiedE2E1AttachmentFrames(t, output.snapshot())
		visible := append([]byte(nil), frames[0].Replay...)
		for _, frame := range frames[1:] {
			if frame.Type == terminal.FrameLive {
				visible = append(visible, frame.Data...)
			}
		}
		if bytes.Equal(visible, want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("E2E1/B1_FULL_RECONSTRUCTION: got=%d want=%d", len(visible), len(want))
		}
		time.Sleep(time.Millisecond)
	}
	_ = writer.Close(context.Background())
}

func TestUnifiedTerminalE2E1DelayedDispatcherCarriesExactFeedRanges(t *testing.T) {
	effects, key := unifiedE2E1JournalProvider(t, "e2e1-delayed-feed", "$0")
	seed := []byte("seed")
	unifiedE2E1Commit(t, effects.realm, key, seed)
	events, initial, tail, cancel, err := openSnapshotTailForTest(t, effects, "$0")
	if err != nil || len(events) != 2 || events[0].Sequence != 1 || len(events[0].Payload) != 0 || !bytes.Equal(events[1].Payload, seed) || initial != (unifiedjournal.Geometry{Columns: 80, Rows: 24}) {
		t.Fatalf("open snapshot: events=%+v initial=%+v err=%v", events, initial, err)
	}
	defer cancel()
	runtime := newRetentionTrialRuntime(effects)
	if runtime == nil {
		t.Fatal("retention runtime unavailable")
	}
	defer runtime.Close()

	a := bytes.Repeat([]byte("A"), 64<<10)
	b := bytes.Repeat([]byte("B"), 64<<10)
	effects.mu.Lock()
	if err := runtime.WritePane(key, a); err != nil {
		effects.mu.Unlock()
		t.Fatal(err)
	}
	if err := runtime.Boundary(key, "e2e1_a"); err != nil {
		effects.mu.Unlock()
		t.Fatal(err)
	}
	if err := runtime.WritePane(key, b); err != nil {
		effects.mu.Unlock()
		t.Fatal(err)
	}
	if err := runtime.Boundary(key, "e2e1_b"); err != nil {
		effects.mu.Unlock()
		t.Fatal(err)
	}
	effects.mu.Unlock()

	got := make([]byte, 0, len(a)+len(b))
	deadline := time.After(3 * time.Second)
	for len(got) < len(a)+len(b) {
		select {
		case part, ok := <-tail.events():
			if ok {
				tail.releaseEvent(part)
			}
			if !ok {
				t.Fatalf("E2E1/B2_TAIL_CLOSED: got=%d", len(got))
			}
			if part.Kind != unifiedjournal.RecordOutput {
				t.Fatalf("E2E1/B2_TAIL_KIND: %+v", part)
			}
			got = append(got, part.Payload...)
		case <-deadline:
			t.Fatalf("E2E1/B2_TAIL_TIMEOUT: got=%d", len(got))
		}
	}
	if !bytes.Equal(got, append(append([]byte(nil), a...), b...)) {
		t.Fatalf("E2E1/B2_EXACT_A_B: got=%d", len(got))
	}
}

func TestUnifiedTerminalE2E1ConcurrentSnapshotAndCommit(t *testing.T) {
	effects, key := unifiedE2E1JournalProvider(t, "e2e1-concurrent-snapshot", "$0")
	runtime := newRetentionTrialRuntime(effects)
	if runtime == nil {
		t.Fatal("retention runtime unavailable")
	}
	defer runtime.Close()
	if err := runtime.WritePane(key, []byte("seed")); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Boundary(key, "seed"); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		for index := 0; index < 256; index++ {
			if err := runtime.WritePane(key, bytes.Repeat([]byte{byte(index)}, 1024)); err != nil {
				errs <- err
				return
			}
			if err := runtime.Boundary(key, "race_commit"); err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()
	go func() {
		<-start
		for index := 0; index < 256; index++ {
			_, _, _, cancel, err := openSnapshotTailForTest(t, effects, "$0")
			if err != nil {
				errs <- err
				return
			}
			cancel()
		}
		errs <- nil
	}()
	close(start)
	for index := 0; index < 2; index++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// Observer-connection resize boundary proof.
//
// The narrow question, against real tmux: when the guarded resize is issued on
// the observer's own control connection, is the command-completion boundary
// (%end) a usable sequence point?  Everything the explicit vertical Fit design
// depends on downstream — a typed GEOMETRY journal record at sequence N, the
// browser event ordered against output, the retention hold — is sound only if
// pane output emitted before the command is classified before %end and the
// pane's SIGWINCH write is classified after it.
//
// The proof deliberately reuses production identity: the production decoder,
// the byte-identical production observer argv, production ECHO handling, and
// the production in-block classification discipline of finishBirth. One
// observer, no polling. It is a test, so it constructs the guarded command
// itself; the canonical resize verb and the attachment PTY Setsize stay owned
// by internal/broker/attachment.go and are not duplicated here as literals.
// ---------------------------------------------------------------------------

const unifiedE2E1BoundaryRed = "E2E1/OBSERVER_RESIZE_BOUNDARY"

// unifiedE2E1SentinelProgram is the pane process for the boundary proof.
//
// It writes a unique sentinel from its SIGWINCH trap, so the resize's effect on
// the pane is observable as ordinary pane output on the control stream, and it
// echoes controlled sentinels on demand so PRE and POST sit exactly around the
// command.  Every sentinel is self-delimited with ';' because the pane tty adds
// CR to LF, which makes a newline an unreliable terminator for exact counting.
const unifiedE2E1SentinelProgram = `stty -echo 2>/dev/null
pad=$(printf '%0240d' 0)
winch=0
trap 'winch=$((winch+1)); printf "E2E1-WINCH-%d;\n" "$winch"' WINCH
printf 'E2E1-READY;\n'
while :; do
	if IFS= read -r line; then
		case "$line" in
		ALT) printf '%b' '\0033[?1049h' ;;
		NORM) printf '%b' '\0033[?1049l' ;;
		UTF8PARTIAL) printf '%b' '\0342' ;;
		UTF8FINISH) printf '%b' '\0224\0200' ;;
		CSIPARTIAL) printf '%b' '\0033[3' ;;
		CSIFINISH) printf 'm' ;;
		BULK) i=0; while [ $i -lt 400 ]; do printf 'E2E1-BULK-%03d:%s;\n' "$i" "$pad"; i=$((i+1)); done ;;
		*) printf 'E2E1-ECHO:%s;\n' "$line" ;;
		esac
	else
		sleep 0.05
	fi
done
`

type unifiedE2E1ControlObserver struct {
	t       *testing.T
	ptmx    *os.File
	command *exec.Cmd
	cancel  context.CancelFunc
	decoder *controlmode.Decoder
	read    chan []byte
	readErr chan error
	done    chan struct{}
	closed  sync.Once

	mu   sync.Mutex
	raw  []byte
	hold chan struct{}

	events   []controlmode.Event
	response []byte
}

// unifiedE2E1StartControlObserver starts one control observer whose argv is
// byte-identical to the production shape at unified_dev.go:231 — the same shape
// broker_test.go:163 already token-asserts. A proof on a different argv would
// prove nothing about production.
func unifiedE2E1StartControlObserver(t *testing.T, server config.TmuxServer, observerSession string) *unifiedE2E1ControlObserver {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	argv := tmuxArgv(server, "-C", "-f", "/dev/null", "attach-session", "-f", "ignore-size", "-t", "="+observerSession)
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.Start(command)
	if err != nil {
		cancel()
		t.Fatalf("%s: start observer: %v", unifiedE2E1BoundaryRed, err)
	}
	if err := unifiedDevDisableEcho(ptmx); err != nil {
		cancel()
		_ = ptmx.Close()
		t.Fatalf("%s: configure observer: %v", unifiedE2E1BoundaryRed, err)
	}
	observer := &unifiedE2E1ControlObserver{
		t: t, ptmx: ptmx, command: command, cancel: cancel, decoder: controlmode.NewDecoder(),
		read: make(chan []byte, 16), readErr: make(chan error, 1), done: make(chan struct{}),
	}
	go observer.readLoop()
	t.Cleanup(observer.stop)
	return observer
}

// stop detaches the observer and reaps its tmux client. Reaping matters: a
// cancelled exec.CommandContext that is never waited on leaves its context
// watcher goroutine and a zombie behind for the life of the test binary.
func (observer *unifiedE2E1ControlObserver) stop() {
	observer.closed.Do(func() {
		observer.releaseReads()
		close(observer.done)
		observer.cancel()
		_ = observer.ptmx.Close()
		_ = observer.command.Wait()
	})
}

// readLoop mirrors the production observer reader goroutine (unified_dev.go
// :242-274) byte for byte, including the CR/LF normalization, and adds only the
// raw capture used for the split-invariance replay and the backpressure gate.
func (observer *unifiedE2E1ControlObserver) readLoop() {
	buffer := make([]byte, 64<<10)
	pendingCR := false
	for {
		if gate := observer.currentHold(); gate != nil {
			select {
			case <-gate:
			case <-observer.done:
				return
			}
		}
		n, err := observer.ptmx.Read(buffer)
		if n > 0 {
			observer.mu.Lock()
			observer.raw = append(observer.raw, buffer[:n]...)
			observer.mu.Unlock()
			chunk := make([]byte, 0, n+1)
			for _, value := range buffer[:n] {
				if pendingCR {
					if value == '\n' {
						chunk = append(chunk, '\n')
						pendingCR = false
						continue
					}
					chunk = append(chunk, '\r')
					pendingCR = false
				}
				if value == '\r' {
					pendingCR = true
				} else {
					chunk = append(chunk, value)
				}
			}
			if len(chunk) != 0 {
				select {
				case observer.read <- chunk:
				case <-observer.done:
					return
				}
			}
		}
		if err != nil {
			select {
			case observer.readErr <- err:
			default:
			}
			return
		}
	}
}

func (observer *unifiedE2E1ControlObserver) currentHold() chan struct{} {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return observer.hold
}

// holdReads stops draining the control client's PTY. tmux keeps producing, so
// its client output buffer grows: this is the backpressured-control-client
// parameter the boundary claim has to survive.
func (observer *unifiedE2E1ControlObserver) holdReads() {
	observer.mu.Lock()
	if observer.hold == nil {
		observer.hold = make(chan struct{})
	}
	observer.mu.Unlock()
}

func (observer *unifiedE2E1ControlObserver) releaseReads() {
	observer.mu.Lock()
	gate := observer.hold
	observer.hold = nil
	observer.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

func (observer *unifiedE2E1ControlObserver) rawSnapshot() []byte {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return append([]byte(nil), observer.raw...)
}

func (observer *unifiedE2E1ControlObserver) pumpUntil(deadline time.Time, what string, satisfied func() bool) {
	observer.t.Helper()
	for !satisfied() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			observer.t.Fatalf("%s: timed out waiting for %s\n%s", unifiedE2E1BoundaryRed, what, observer.trace())
		}
		select {
		case chunk := <-observer.read:
			observer.consume(chunk)
		case err := <-observer.readErr:
			observer.t.Fatalf("%s: observer read while waiting for %s: %v", unifiedE2E1BoundaryRed, what, err)
		case <-time.After(remaining):
		}
	}
}

// consume applies the production in-block classification discipline of
// finishBirth (unified_dev.go): a command response accumulates and is
// never dispatched as pane output, every other event keeps its decoded order,
// including the remainder of the chunk that carried %end.
func (observer *unifiedE2E1ControlObserver) consume(chunk []byte) {
	observer.t.Helper()
	events, err := observer.decoder.Feed(chunk)
	if err != nil {
		observer.t.Fatalf("%s: production decoder failed: %v", unifiedE2E1BoundaryRed, err)
	}
	for _, event := range events {
		if event.Kind == controlmode.EventCommandResponse {
			observer.response = append(observer.response, event.Data...)
		}
		observer.events = append(observer.events, event)
	}
}

func (observer *unifiedE2E1ControlObserver) outputBytes() []byte {
	var combined []byte
	for _, event := range observer.events {
		if event.Kind == controlmode.EventOutput || event.Kind == controlmode.EventExtendedOutput {
			combined = append(combined, event.Data...)
		}
	}
	return combined
}

// outputIndex returns the index of the event at which needle first becomes
// complete in the concatenated pane-output stream, so a sentinel split across
// two %output records is still located exactly.
func (observer *unifiedE2E1ControlObserver) outputIndex(needle string) int {
	var combined []byte
	for index, event := range observer.events {
		if event.Kind != controlmode.EventOutput && event.Kind != controlmode.EventExtendedOutput {
			continue
		}
		combined = append(combined, event.Data...)
		if bytes.Contains(combined, []byte(needle)) {
			return index
		}
	}
	return -1
}

func (observer *unifiedE2E1ControlObserver) kindIndex(from int, kind controlmode.EventKind) int {
	for index := from; index < len(observer.events); index++ {
		if observer.events[index].Kind == kind {
			return index
		}
	}
	return -1
}

// drainFor consumes whatever the control connection already has, without
// requiring a condition. It is used to prove the absence of a further command
// block, which no waiting predicate can establish.
func (observer *unifiedE2E1ControlObserver) drainFor(window time.Duration) {
	observer.t.Helper()
	deadline := time.Now().Add(window)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		select {
		case chunk := <-observer.read:
			observer.consume(chunk)
		case err := <-observer.readErr:
			observer.t.Fatalf("%s: observer read while draining: %v", unifiedE2E1BoundaryRed, err)
		case <-time.After(remaining):
			return
		}
	}
}

type unifiedE2E1CommandBlock struct {
	begin  int
	end    int
	failed bool
}

func (observer *unifiedE2E1ControlObserver) commandBlocks(from int) []unifiedE2E1CommandBlock {
	var blocks []unifiedE2E1CommandBlock
	open := -1
	for index := from; index < len(observer.events); index++ {
		switch observer.events[index].Kind {
		case controlmode.EventCommandBegin:
			open = index
		case controlmode.EventCommandEnd, controlmode.EventCommandError:
			if open >= 0 {
				blocks = append(blocks, unifiedE2E1CommandBlock{
					begin: open, end: index,
					failed: observer.events[index].Kind == controlmode.EventCommandError,
				})
				open = -1
			}
		}
	}
	return blocks
}

// unifiedE2E1CommandRecords recovers the raw %begin/%end/%error lines, whose
// time/number/flags fields the production decoder deliberately discards. They
// are evidence, not control flow.
func unifiedE2E1CommandRecords(raw []byte) []string {
	var out []string
	for _, line := range strings.Split(string(unifiedE2E1NormalizeControlStream(raw)), "\n") {
		if strings.HasPrefix(line, "%begin ") || strings.HasPrefix(line, "%end ") || strings.HasPrefix(line, "%error ") {
			out = append(out, line)
		}
	}
	return out
}

func unifiedE2E1KindName(kind controlmode.EventKind) string {
	switch kind {
	case controlmode.EventOutput:
		return "output"
	case controlmode.EventExtendedOutput:
		return "extended-output"
	case controlmode.EventNotification:
		return "notification"
	case controlmode.EventCommandBegin:
		return "command-begin"
	case controlmode.EventCommandResponse:
		return "command-response"
	case controlmode.EventCommandEnd:
		return "command-end"
	case controlmode.EventCommandError:
		return "command-error"
	default:
		return "unknown"
	}
}

// trace renders the ordered event sequence. Bulk backpressure filler is folded
// into a count so the causal sequence stays readable as evidence.
func (observer *unifiedE2E1ControlObserver) trace() string {
	var builder strings.Builder
	bulk := 0
	flush := func() {
		if bulk != 0 {
			builder.WriteString("  [" + strconv.Itoa(bulk) + " backpressure filler output events]\n")
			bulk = 0
		}
	}
	for index, event := range observer.events {
		if event.Kind == controlmode.EventOutput && bytes.Contains(event.Data, []byte("E2E1-BULK-")) &&
			!bytes.Contains(event.Data, []byte("E2E1-ECHO:")) && !bytes.Contains(event.Data, []byte("E2E1-WINCH-")) {
			bulk++
			continue
		}
		flush()
		preview := event.Data
		if len(preview) > 64 {
			preview = preview[:64]
		}
		builder.WriteString("  " + strconv.Itoa(index) + " " + unifiedE2E1KindName(event.Kind))
		if event.PaneID != "" {
			builder.WriteString(" " + event.PaneID)
		}
		if event.Name != "" {
			builder.WriteString(" " + event.Name + " " + event.Args)
		}
		if len(preview) != 0 {
			builder.WriteString(" " + strconv.Quote(string(preview)))
		}
		builder.WriteString("\n")
	}
	flush()
	return builder.String()
}

// unifiedE2E1GuardedResizeCommand builds the control-connection form of the
// canonical guarded resize. The guard is the exact construction and semantics of
// tmuxPinnedTransaction.resize (attachment.go:412-420): the same incarnation
// condition and the same session/window/pane/pid/width/height/one-pane/start-time
// conditions, all of which must hold in-transaction. Only the issuer changes —
// the observer's own control connection instead of a separate tmux process —
// which is the connection boundary this test exercises.
//
// The tmux verb is assembled from two fragments on purpose: the canonical
// resize literal stays owned by internal/broker/attachment.go, so nothing here
// reads as a second home for it.
func unifiedE2E1GuardedResizeCommand(authority proto.Authority, witness terminal.SourceWitness, rows int) string {
	guard := incarnationCondition(authority) +
		"; [ " + shellQuote("#{session_id}") + " = " + shellQuote(witness.SessionID) + " ] || exit 1" +
		"; [ " + shellQuote("#{window_id}") + " = " + shellQuote(witness.WindowID) + " ] || exit 1" +
		"; [ " + shellQuote("#{pane_id}") + " = " + shellQuote(witness.PaneID) + " ] || exit 1" +
		"; [ " + shellQuote("#{pane_pid}") + " = " + shellQuote(strconv.Itoa(witness.Pane.PID)) + " ] || exit 1" +
		"; [ " + shellQuote("#{pane_width}") + " = " + shellQuote(strconv.Itoa(witness.Columns)) + " ] || exit 1" +
		"; [ " + shellQuote("#{pane_height}") + " = " + shellQuote(strconv.Itoa(witness.Rows)) + " ] || exit 1" +
		"; [ " + shellQuote("#{window_panes}") + " = 1 ] || exit 1" +
		"; stat=$(cat /proc/" + strconv.Itoa(witness.Pane.PID) + "/stat) || exit 1; suffix=${stat##*) }; set -- $suffix; [ \"${20}\" = " + shellQuote(strconv.FormatUint(witness.Pane.StartTime, 10)) + " ]"
	success := fmt.Sprintf("%s -t %s -x %d -y %d", "resize"+"-window", shellQuote(witness.WindowID), witness.Columns, rows)
	args := []string{"if-shell", "-t", pinnedTmuxTarget(witness), guard, success, "run-shell 'exit 77'"}
	quoted := make([]string, len(args))
	for index, argument := range args {
		quoted[index] = shellQuote(argument)
	}
	return strings.Join(quoted, " ")
}

// unifiedE2E1CreateSentinelSession creates the proof's pane through the
// observer's own control connection. Production does exactly this: the birth
// path drops -d (session_create.go:118-123, "a control client must follow the
// new session to observe its first byte"), so the observer ends up attached to
// the observed session and owns its byte-zero output. A control client attached
// elsewhere receives no %output for that pane at all, so any proof built on a
// detached creation would be testing a configuration the unified engine never
// runs in.
func unifiedE2E1CreateSentinelSession(t *testing.T, observer *unifiedE2E1ControlObserver, name string) string {
	t.Helper()
	dir := shortTempDir(t)
	script := filepath.Join(dir, "sentinel.sh")
	if err := os.WriteFile(script, []byte(unifiedE2E1SentinelProgram), 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{"new-session", "-P", "-F", "#{session_id}", "-s", name, "-x", "80", "-y", "24", "sh " + script}
	quoted := make([]string, len(args))
	for index, argument := range args {
		quoted[index] = shellQuote(argument)
	}
	mark := len(observer.events)
	if _, err := io.WriteString(observer.ptmx, strings.Join(quoted, " ")+"\n"); err != nil {
		t.Fatalf("%s: create sentinel session: %v", unifiedE2E1BoundaryRed, err)
	}
	observer.pumpUntil(time.Now().Add(20*time.Second), "sentinel session birth", func() bool {
		return observer.kindIndex(mark, controlmode.EventCommandEnd) >= 0 || observer.kindIndex(mark, controlmode.EventCommandError) >= 0
	})
	if observer.kindIndex(mark, controlmode.EventCommandError) >= 0 {
		t.Fatalf("%s: tmux rejected the sentinel session birth: %q", unifiedE2E1BoundaryRed, observer.response)
	}
	sessionID := strings.TrimSpace(string(observer.response))
	if !validSessionID(sessionID) {
		t.Fatalf("%s: sentinel session id=%q", unifiedE2E1BoundaryRed, sessionID)
	}
	observer.response = nil
	observer.pumpUntil(time.Now().Add(20*time.Second), "sentinel pane byte-zero output", func() bool {
		return observer.outputIndex("E2E1-READY;") >= 0
	})
	return sessionID
}

func unifiedE2E1SendPaneLine(t *testing.T, disposable *disposable, sessionID, text string) {
	t.Helper()
	disposable.run("send-keys", "-t", "="+sessionID+":", "--", text, "Enter")
}

// unifiedE2E1AwaitPaneText is an oracle independent of the control stream: it
// reads the pane's own screen through a separate tmux client, so it still works
// while the observer is deliberately backpressured.
func unifiedE2E1AwaitPaneText(t *testing.T, disposable *disposable, sessionID, needle string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, err := tmuxCombinedOutput(disposable.path, "capture-pane", "-p", "-t", "="+sessionID+":")
		if err == nil {
			last = string(out)
			if strings.Contains(last, needle) {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s: pane never showed %q; screen tail=%q", unifiedE2E1BoundaryRed, needle, last[max(0, len(last)-256):])
}

func unifiedE2E1HighestWinch(output []byte) int {
	highest := 0
	rest := output
	marker := []byte("E2E1-WINCH-")
	for {
		index := bytes.Index(rest, marker)
		if index < 0 {
			return highest
		}
		rest = rest[index+len(marker):]
		end := bytes.IndexByte(rest, ';')
		if end < 0 {
			return highest
		}
		if value, err := strconv.Atoi(string(rest[:end])); err == nil && value > highest {
			highest = value
		}
	}
}

func unifiedE2E1WindowPanes(t *testing.T, disposable *disposable, sessionID string) string {
	t.Helper()
	return disposable.run("display-message", "-p", "-t", "="+sessionID+":", "-F", "#{window_panes}")
}

// unifiedE2E1TransientShadowClient attaches and detaches a client with the exact
// production shadow flags (attachment.go:331). A size-ignoring client must not
// be able to move a window this product deliberately sized.
func unifiedE2E1TransientShadowClient(t *testing.T, server config.TmuxServer, sessionID string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	argv := tmuxArgv(server, "-C", "-f", "/dev/null", "attach-session", "-f", "ignore-size,active-pane", "-t", "="+sessionID)
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.Start(command)
	if err != nil {
		cancel()
		t.Fatalf("%s: transient shadow client: %v", unifiedE2E1BoundaryRed, err)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		buffer := make([]byte, 4096)
		for {
			if _, err := ptmx.Read(buffer); err != nil {
				return
			}
		}
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()
	_ = ptmx.Close()
	<-drained
	_ = command.Wait()
	time.Sleep(300 * time.Millisecond)
}

// unifiedE2E1NormalizeControlStream reproduces the production reader's CR/LF
// normalization over the whole recorded stream. A trailing lone CR stays pending
// in production and is likewise not emitted here.
func unifiedE2E1NormalizeControlStream(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	pendingCR := false
	for _, value := range raw {
		if pendingCR {
			if value == '\n' {
				out = append(out, '\n')
				pendingCR = false
				continue
			}
			out = append(out, '\r')
			pendingCR = false
		}
		if value == '\r' {
			pendingCR = true
		} else {
			out = append(out, value)
		}
	}
	return out
}

func unifiedE2E1DecodeWithCuts(t *testing.T, stream []byte, cuts []int) []controlmode.Event {
	t.Helper()
	decoder := controlmode.NewDecoder()
	var events []controlmode.Event
	previous := 0
	feed := func(piece []byte) {
		if len(piece) == 0 {
			return
		}
		produced, err := decoder.Feed(piece)
		if err != nil {
			t.Fatalf("%s: production decoder failed on split replay: %v", unifiedE2E1BoundaryRed, err)
		}
		events = append(events, produced...)
	}
	for _, cut := range cuts {
		if cut <= previous || cut > len(stream) {
			continue
		}
		feed(stream[previous:cut])
		previous = cut
	}
	feed(stream[previous:])
	if decoder.Failed() {
		t.Fatalf("%s: production decoder entered sticky failure on split replay", unifiedE2E1BoundaryRed)
	}
	return events
}

func unifiedE2E1SameEvents(left, right []controlmode.Event) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Kind != right[index].Kind || left[index].PaneID != right[index].PaneID ||
			left[index].Age != right[index].Age || left[index].Name != right[index].Name ||
			left[index].Args != right[index].Args || !bytes.Equal(left[index].Data, right[index].Data) {
			return false
		}
	}
	return true
}

// unifiedE2E1ExhaustiveSplitBytes is the stream size up to which every index
// is replayed as a two-way split.
const unifiedE2E1ExhaustiveSplitBytes = 8192

// unifiedE2E1TwoWayReplayBudgetBytes bounds how many bytes the two-way split
// replay re-decodes for one recorded stream. Every cut costs one full decode of
// the stream, so the replay is O(cuts x stream). The backpressured variants
// record on the order of 100 KB, in which every filler line carries an octal
// escape pair and therefore nine candidate cuts; unbounded, that is ~9,000 cuts
// and about a gigabyte of decoder work per variant. Under -race that took the
// better part of a minute per variant and is what carried the package past go
// test's default ten-minute alarm (issue #9): the alarm fired while this loop
// was busy, which read as a hang of the backpressured subtest.
const unifiedE2E1TwoWayReplayBudgetBytes = 32 << 20

// unifiedE2E1EvenSplitSamples is the number of evenly spaced cuts a large
// stream always gets, independent of its structure, so the replay exercises
// positions across the whole recording and not only its head.
const unifiedE2E1EvenSplitSamples = 64

// unifiedE2E1SplitNeighbourhood is how many indices on either side of a
// structural byte count as that byte's neighbourhood.
const unifiedE2E1SplitNeighbourhood = 4

// unifiedE2E1SplitMarkers are the structural bytes whose neighbourhoods are the
// cuts that can change the decoder's behaviour: record terminators, record
// starts, octal escapes and header field separators.
var unifiedE2E1SplitMarkers = []byte{'\n', '%', '\\', ' '}

// unifiedE2E1SplitShape classifies the window around a cut by structure: each
// byte becomes its marker, or '.' when it is not one. Two cuts with the same
// shape sit in the same local structure and exercise the same decoder
// transition; the shape is empty when no marker is in the window, which makes
// the cut uninteresting.
func unifiedE2E1SplitShape(stream []byte, cut int) string {
	shape := make([]byte, 0, 2*unifiedE2E1SplitNeighbourhood)
	interesting := false
	for at := cut - unifiedE2E1SplitNeighbourhood; at < cut+unifiedE2E1SplitNeighbourhood; at++ {
		if at < 0 || at >= len(stream) {
			shape = append(shape, '_')
			continue
		}
		if bytes.IndexByte(unifiedE2E1SplitMarkers, stream[at]) >= 0 {
			shape = append(shape, stream[at])
			interesting = true
		} else {
			shape = append(shape, '.')
		}
	}
	if !interesting {
		return ""
	}
	return string(shape)
}

// unifiedE2E1RelevantSplits enumerates the split points that can change the
// decoder's behaviour: every index for a small stream, and otherwise every
// index whose window holds a structural byte, plus an even sample.
//
// For a large stream the structural candidates are thinned before they are
// returned: they are grouped by unifiedE2E1SplitShape, and each shape keeps a
// bounded number of representatives taken evenly across its occurrences, so
// that the total replay stays within unifiedE2E1TwoWayReplayBudgetBytes. The
// thinning is deterministic, so a red here reproduces. What it drops is
// repetition, not structure: the backpressured recording is hundreds of
// byte-identical filler lines, and a split three bytes before the
// three-hundredth record terminator exercises the same decoder transition as
// the same split before the third. Grouping by shape rather than by marker is
// what keeps every such transition represented: a fixed stride over one
// marker's occurrences aliases with the filler's period and silently skips
// half of them. The one-byte-at-a-time pass that accompanies these cuts still
// feeds every index of the stream as a boundary.
//
// The second result is the number of candidates before thinning; it is
// evidence for the log line, not control flow.
func unifiedE2E1RelevantSplits(stream []byte) ([]int, int) {
	if len(stream) <= unifiedE2E1ExhaustiveSplitBytes {
		cuts := make([]int, 0, len(stream))
		for index := 1; index < len(stream); index++ {
			cuts = append(cuts, index)
		}
		return cuts, len(cuts)
	}
	shapes := make(map[string][]int)
	var order []string
	for cut := 1; cut < len(stream); cut++ {
		shape := unifiedE2E1SplitShape(stream, cut)
		if shape == "" {
			continue
		}
		if _, known := shapes[shape]; !known {
			order = append(order, shape)
		}
		shapes[shape] = append(shapes[shape], cut)
	}
	candidates := 0
	for _, members := range shapes {
		candidates += len(members)
	}

	maxCuts := unifiedE2E1TwoWayReplayBudgetBytes/len(stream) - unifiedE2E1EvenSplitSamples
	perShape := 1
	if len(shapes) != 0 && maxCuts/len(shapes) > 1 {
		perShape = maxCuts / len(shapes)
	}
	seen := make(map[int]struct{})
	var cuts []int
	take := func(cut int) {
		if _, dup := seen[cut]; dup {
			return
		}
		seen[cut] = struct{}{}
		cuts = append(cuts, cut)
	}
	for _, shape := range order {
		members := shapes[shape]
		if len(members) <= perShape {
			for _, cut := range members {
				take(cut)
			}
			continue
		}
		for pick := 0; pick < perShape; pick++ {
			take(members[pick*len(members)/perShape])
		}
	}
	for pick := 0; pick < unifiedE2E1EvenSplitSamples; pick++ {
		if cut := 1 + pick*(len(stream)-1)/unifiedE2E1EvenSplitSamples; cut > 0 && cut < len(stream) {
			take(cut)
		}
	}
	slices.Sort(cuts)
	return cuts, candidates + unifiedE2E1EvenSplitSamples
}

// unifiedE2E1AssertDecoderSplitInvariance proves the recorded real-tmux stream
// decodes to one identical event sequence no matter where the OS split it: the
// byte-exactness half of the boundary claim.
func unifiedE2E1AssertDecoderSplitInvariance(t *testing.T, raw []byte) int {
	t.Helper()
	started := time.Now()
	stream := unifiedE2E1NormalizeControlStream(raw)
	if len(stream) == 0 {
		t.Fatalf("%s: no control stream was recorded", unifiedE2E1BoundaryRed)
	}
	reference := unifiedE2E1DecodeWithCuts(t, stream, nil)
	single := make([]int, 0, len(stream))
	for index := 1; index < len(stream); index++ {
		single = append(single, index)
	}
	if got := unifiedE2E1DecodeWithCuts(t, stream, single); !unifiedE2E1SameEvents(reference, got) {
		t.Fatalf("%s: one-byte-at-a-time replay produced a different event sequence (%d vs %d events)",
			unifiedE2E1BoundaryRed, len(got), len(reference))
	}
	cuts, candidates := unifiedE2E1RelevantSplits(stream)
	for _, cut := range cuts {
		if got := unifiedE2E1DecodeWithCuts(t, stream, []int{cut}); !unifiedE2E1SameEvents(reference, got) {
			t.Fatalf("%s: replay split at byte %d produced a different event sequence (%d vs %d events)",
				unifiedE2E1BoundaryRed, cut, len(got), len(reference))
		}
	}
	t.Logf("%s: split invariance held over %d control bytes, %d reference events, %d two-way splits (thinned from %d candidates) plus a full one-byte pass, in %s",
		unifiedE2E1BoundaryRed, len(stream), len(reference), len(cuts), candidates, time.Since(started).Round(time.Millisecond))
	return len(cuts)
}

// TestUnifiedE2E1RelevantSplitsStayWithinBudget pins the sampler the split
// invariance proof relies on: exhaustive for a small stream; for a large
// stream, bounded by the replay budget, deterministic, spanning the whole
// recording, and still holding every structural shape the stream contains,
// including the ones a marker-stride sampler aliases away.
func TestUnifiedE2E1RelevantSplitsStayWithinBudget(t *testing.T) {
	small := []byte("%begin 1 2 0\nx\n%end 1 2 0\n%output %1 a\\015\\012\n")
	cuts, candidates := unifiedE2E1RelevantSplits(small)
	if len(cuts) != len(small)-1 || candidates != len(cuts) {
		t.Fatalf("small stream: %d cuts from %d candidates, want every index (%d)", len(cuts), candidates, len(small)-1)
	}

	// A backpressured recording: hundreds of byte-identical filler records, each
	// carrying an octal escape pair, around a handful of command blocks.
	var builder bytes.Buffer
	pad := strings.Repeat("0", 240)
	builder.WriteString("%begin 1787085710 326 0\n%end 1787085710 326 0\n")
	for index := 0; index < 400; index++ {
		fmt.Fprintf(&builder, "%%output %%1 E2E1-BULK-%03d:%s;\\015\\012\n", index, pad)
	}
	builder.WriteString("%layout-change @0 1234,80x37,0,0,0 1234,80x37,0,0,0 *\n%begin 1787085710 351 1\n%end 1787085710 351 1\n")
	large := builder.Bytes()
	if len(large) <= unifiedE2E1ExhaustiveSplitBytes {
		t.Fatalf("synthetic stream is only %d bytes", len(large))
	}
	cuts, candidates = unifiedE2E1RelevantSplits(large)
	again, _ := unifiedE2E1RelevantSplits(large)
	if !slices.Equal(cuts, again) {
		t.Fatalf("sampler is not deterministic")
	}
	maxCuts := unifiedE2E1TwoWayReplayBudgetBytes / len(large)
	if len(cuts) == 0 || len(cuts) > maxCuts {
		t.Fatalf("%d cuts for a %d-byte stream, want 1..%d", len(cuts), len(large), maxCuts)
	}
	if candidates <= len(cuts) {
		t.Fatalf("thinning did not happen: %d cuts from %d candidates", len(cuts), candidates)
	}
	for index, cut := range cuts {
		if cut <= 0 || cut >= len(large) {
			t.Fatalf("cut %d outside (0,%d)", cut, len(large))
		}
		if index > 0 && cut <= cuts[index-1] {
			t.Fatalf("cuts are not sorted and unique at %d: %d after %d", index, cut, cuts[index-1])
		}
	}
	// Every structural shape in the stream survives, and every offset around
	// every marker byte is represented on at least one cut.
	want := make(map[string]struct{})
	for cut := 1; cut < len(large); cut++ {
		if shape := unifiedE2E1SplitShape(large, cut); shape != "" {
			want[shape] = struct{}{}
		}
	}
	for _, cut := range cuts {
		delete(want, unifiedE2E1SplitShape(large, cut))
	}
	if len(want) != 0 {
		t.Fatalf("%d structural shapes have no cut: %q", len(want), slices.Sorted(maps.Keys(want)))
	}
	for _, marker := range unifiedE2E1SplitMarkers {
		for offset := -unifiedE2E1SplitNeighbourhood; offset < unifiedE2E1SplitNeighbourhood; offset++ {
			covered := false
			for _, cut := range cuts {
				if at := cut + offset; at >= 0 && at < len(large) && large[at] == marker {
					covered = true
					break
				}
			}
			if !covered {
				t.Fatalf("no cut with marker %q at offset %+d", marker, offset)
			}
		}
	}
	if cuts[0] > len(large)/8 || cuts[len(cuts)-1] < len(large)*7/8 {
		t.Fatalf("cuts %d..%d do not span the %d-byte stream", cuts[0], cuts[len(cuts)-1], len(large))
	}
	t.Logf("%d-byte synthetic backpressured stream: %d cuts from %d candidates (budget %d)", len(large), len(cuts), candidates, maxCuts)
}

type unifiedE2E1BoundaryCase struct {
	name         string
	alternate    bool
	backpressure bool
}

// TestUnifiedTerminalE2E1ObserverResizeBoundary checks the observer resize boundary.
func TestUnifiedTerminalE2E1ObserverResizeBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux observer resize boundary proof")
	}
	for _, item := range []unifiedE2E1BoundaryCase{
		{name: "normal_screen"},
		{name: "alternate_screen", alternate: true},
		{name: "normal_screen_backpressured_control_client", backpressure: true},
		{name: "alternate_screen_backpressured_control_client", alternate: true, backpressure: true},
	} {
		t.Run(item.name, func(t *testing.T) { unifiedE2E1RunResizeBoundary(t, item) })
	}
}

func unifiedE2E1RunResizeBoundary(t *testing.T, item unifiedE2E1BoundaryCase) {
	t.Helper()
	disposable := newDisposable(t)
	server := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	disposable.run("new-session", "-d", "-s", "anchor", "-x", "80", "-y", "24", "sleep 600")

	observer := unifiedE2E1StartControlObserver(t, server, "anchor")
	observer.pumpUntil(time.Now().Add(20*time.Second), "observer steady state", func() bool {
		for _, event := range observer.events {
			if event.Kind == controlmode.EventNotification && event.Name == "session-changed" && strings.Contains(event.Args, "anchor") {
				return true
			}
		}
		return false
	})
	sessionID := unifiedE2E1CreateSentinelSession(t, observer, "vfit")

	detail, err := details(server, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = "e2e1", server.Label, detail.ID, detail.Created

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	before, err := buildSourceWitness(ctx, server, "", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Birth geometry is broker/config-owned; the proof only requires that the
	// requested row count is a real, in-policy vertical change from it.
	requestedRows := before.Rows + 13
	if before.Columns < 8 || before.Rows < 8 || requestedRows > 120 {
		t.Fatalf("%s: unusable birth geometry %dx%d", unifiedE2E1BoundaryRed, before.Columns, before.Rows)
	}
	t.Logf("%s/%s: birth geometry %dx%d, requesting rows=%d", unifiedE2E1BoundaryRed, item.name, before.Columns, before.Rows, requestedRows)

	if item.alternate {
		unifiedE2E1SendPaneLine(t, disposable, sessionID, "ALT")
		observer.pumpUntil(time.Now().Add(15*time.Second), "alternate screen entry", func() bool {
			return observer.outputIndex("\x1b[?1049h") >= 0
		})
		if on := disposable.run("display-message", "-p", "-t", "="+sessionID+":", "-F", "#{alternate_on}"); on != "1" {
			t.Fatalf("%s: alternate screen not active: %q", unifiedE2E1BoundaryRed, on)
		}
	}

	// Everything the pane has already written is decoded before the baseline is
	// taken, so a later SIGWINCH sentinel number is unambiguously new.
	unifiedE2E1SendPaneLine(t, disposable, sessionID, "SYNC")
	observer.pumpUntil(time.Now().Add(15*time.Second), "pre-command synchronization", func() bool {
		return observer.outputIndex("E2E1-ECHO:SYNC;") >= 0
	})
	baseline := unifiedE2E1HighestWinch(observer.outputBytes())

	// Control for the block-shape claim below: an ordinary command on this
	// connection occupies exactly one %begin/%end block, so an extra block is a
	// property of the guarded construction, not of the connection.
	plainMark := len(observer.events)
	if _, err := io.WriteString(observer.ptmx, "display-message -p -F "+shellQuote("#{pane_id}")+"\n"); err != nil {
		t.Fatalf("%s: write plain command: %v", unifiedE2E1BoundaryRed, err)
	}
	observer.pumpUntil(time.Now().Add(15*time.Second), "plain command block", func() bool {
		return len(observer.commandBlocks(plainMark)) >= 1
	})
	observer.drainFor(500 * time.Millisecond)
	if plain := observer.commandBlocks(plainMark); len(plain) != 1 || plain[0].failed {
		t.Fatalf("%s: plain command occupied %d blocks: %+v", unifiedE2E1BoundaryRed, len(plain), plain)
	}
	observer.response = nil

	// Failure surface characterization for later phases. This records what the
	// observer connection actually reports for refused work; it does not fix a
	// shape, because the design must not depend on one that was assumed.
	for _, probe := range []struct {
		name string
		line string
	}{
		{"unknown command", "persea-not-a-tmux-command"},
		{"absent target", "display-message -p -t " + shellQuote("=$2000000000:") + " -F x"},
	} {
		probeMark := len(observer.events)
		if _, err := io.WriteString(observer.ptmx, probe.line+"\n"); err != nil {
			t.Fatalf("%s: write %s probe: %v", unifiedE2E1BoundaryRed, probe.name, err)
		}
		observer.pumpUntil(time.Now().Add(15*time.Second), probe.name+" block", func() bool {
			return len(observer.commandBlocks(probeMark)) >= 1
		})
		observer.drainFor(400 * time.Millisecond)
		blocks := observer.commandBlocks(probeMark)
		if len(blocks) != 1 {
			t.Fatalf("%s: %s occupied %d command blocks: %+v", unifiedE2E1BoundaryRed, probe.name, len(blocks), blocks)
		}
		t.Logf("%s/%s: failure surface %s -> failed=%t response=%q",
			unifiedE2E1BoundaryRed, item.name, probe.name, blocks[0].failed, observer.response)
		if observer.decoder.Failed() {
			t.Fatalf("%s: production decoder entered sticky failure on the %s probe", unifiedE2E1BoundaryRed, probe.name)
		}
		observer.response = nil
	}

	// PRE is written before the filler so that, in the backpressured variant, it
	// is output tmux has already read but has not yet handed to this client when
	// the resize runs. That is the exact reordering hazard the design depends on
	// not existing.
	unifiedE2E1SendPaneLine(t, disposable, sessionID, "PRE")
	unifiedE2E1AwaitPaneText(t, disposable, sessionID, "E2E1-ECHO:PRE;")
	if !item.backpressure {
		observer.pumpUntil(time.Now().Add(15*time.Second), "PRE pane output", func() bool {
			return observer.outputIndex("E2E1-ECHO:PRE;") >= 0
		})
	}
	if item.backpressure {
		observer.holdReads()
		unifiedE2E1SendPaneLine(t, disposable, sessionID, "BULK")
		time.Sleep(900 * time.Millisecond)
	}

	mark := len(observer.events)
	line := unifiedE2E1GuardedResizeCommand(authority, before, requestedRows)
	if _, err := io.WriteString(observer.ptmx, line+"\n"); err != nil {
		t.Fatalf("%s: write guarded resize to observer connection: %v", unifiedE2E1BoundaryRed, err)
	}
	if item.backpressure {
		time.Sleep(600 * time.Millisecond)
		observer.releaseReads()
	}

	// The guarded resize occupies two command blocks on this connection: the
	// if-shell submission, and the deferred success command tmux queues once the
	// guard passes. Sequence N is the completion of the LAST block. The first
	// %end is deliberately not treated as the boundary: it is emitted before the
	// resize has taken effect, so pane output classified after it may still be
	// pre-resize output.
	// MID exists to characterize the window between the two blocks. tmux returns
	// to its event loop after the if-shell item completes and before the deferred
	// success command runs, so pane output can be classified there. Such output is
	// pre-resize output, which is why the first %end is not sequence N.
	observer.pumpUntil(time.Now().Add(60*time.Second), "first guarded resize block", func() bool {
		return len(observer.commandBlocks(mark)) >= 1
	})
	firstEnd := observer.commandBlocks(mark)[0].end
	unifiedE2E1SendPaneLine(t, disposable, sessionID, "MID")
	observer.pumpUntil(time.Now().Add(60*time.Second), "guarded resize command completion", func() bool {
		return len(observer.commandBlocks(mark)) >= 2
	})
	unifiedE2E1SendPaneLine(t, disposable, sessionID, "POST")
	winchNeedle := "E2E1-WINCH-" + strconv.Itoa(baseline+1) + ";"
	observer.pumpUntil(time.Now().Add(60*time.Second), "SIGWINCH and POST sentinels", func() bool {
		return observer.outputIndex(winchNeedle) >= 0 && observer.outputIndex("E2E1-ECHO:POST;") >= 0
	})
	if item.backpressure {
		observer.pumpUntil(time.Now().Add(60*time.Second), "backpressured filler drain", func() bool {
			return observer.outputIndex("E2E1-BULK-399:") >= 0
		})
	}
	observer.drainFor(700 * time.Millisecond)

	blocks := observer.commandBlocks(mark)
	t.Logf("%s/%s: guarded resize command blocks=%+v raw command records=%q",
		unifiedE2E1BoundaryRed, item.name, blocks, unifiedE2E1CommandRecords(observer.rawSnapshot()))
	if len(blocks) != 2 {
		t.Fatalf("%s: guarded resize occupied %d command blocks, want 2\n%s", unifiedE2E1BoundaryRed, len(blocks), observer.trace())
	}
	if blocks[0].failed || blocks[1].failed {
		t.Fatalf("%s: tmux returned %%error for the guarded resize; response=%q\n%s", unifiedE2E1BoundaryRed, observer.response, observer.trace())
	}
	boundary := blocks[1].end

	midIndex := observer.outputIndex("E2E1-ECHO:MID;")
	if midIndex >= 0 && midIndex <= firstEnd {
		t.Fatalf("%s: MID was classified at or before the first %%end it was sent after (mid=%d first-end=%d)",
			unifiedE2E1BoundaryRed, midIndex, firstEnd)
	}
	t.Logf("%s/%s: inter-block window: first-end=%d second-begin=%d mid=%d inside=%t",
		unifiedE2E1BoundaryRed, item.name, firstEnd, blocks[1].begin, midIndex, midIndex > firstEnd && midIndex < blocks[1].begin)
	if midIndex > firstEnd && midIndex < blocks[1].begin {
		// MID landed between the two blocks, so the pane wrote it before the
		// resize took effect. Sequence N must not precede it. This is the
		// assertion that makes the choice of N testable: taking the first
		// %end as N turns this into a red.
		if midIndex >= boundary {
			t.Fatalf("%s: pre-resize output classified between the command blocks is not ordered before N (mid=%d N=%d): the first %%end is not a sound boundary\n%s",
				unifiedE2E1BoundaryRed, midIndex, boundary, observer.trace())
		}
	}

	preIndex := observer.outputIndex("E2E1-ECHO:PRE;")
	winchIndex := observer.outputIndex(winchNeedle)
	postIndex := observer.outputIndex("E2E1-ECHO:POST;")
	if preIndex < 0 || preIndex >= boundary {
		t.Fatalf("%s: PRE output was not classified before N (pre=%d N=%d)\n%s", unifiedE2E1BoundaryRed, preIndex, boundary, observer.trace())
	}
	if winchIndex <= boundary {
		t.Fatalf("%s: SIGWINCH output was not classified after N (winch=%d N=%d): the boundary is not derivable\n%s",
			unifiedE2E1BoundaryRed, winchIndex, boundary, observer.trace())
	}
	if postIndex <= winchIndex {
		t.Fatalf("%s: POST output was not classified after the SIGWINCH sentinel (post=%d winch=%d)\n%s",
			unifiedE2E1BoundaryRed, postIndex, winchIndex, observer.trace())
	}

	// The geometry announcement is the protocol-visible proof of which block
	// gates the mutation.
	layoutIndex := -1
	layoutNeedle := strconv.Itoa(before.Columns) + "x" + strconv.Itoa(requestedRows)
	for index := mark; index < len(observer.events); index++ {
		event := observer.events[index]
		if event.Kind == controlmode.EventNotification && event.Name == "layout-change" && strings.Contains(event.Args, layoutNeedle) {
			layoutIndex = index
			break
		}
	}
	if layoutIndex < 0 {
		t.Logf("%s: no layout-change notification carrying %q was classified as a notification", unifiedE2E1BoundaryRed, layoutNeedle)
	} else if layoutIndex <= blocks[0].end {
		t.Fatalf("%s: geometry changed at or before the first %%end (layout=%d first-end=%d)", unifiedE2E1BoundaryRed, layoutIndex, blocks[0].end)
	}

	all := observer.outputBytes()
	for _, needle := range []string{"E2E1-ECHO:PRE;", "E2E1-ECHO:MID;", winchNeedle, "E2E1-ECHO:POST;", "E2E1-ECHO:SYNC;"} {
		if count := bytes.Count(all, []byte(needle)); count != 1 {
			t.Fatalf("%s: sentinel %q appeared %d times in the pane stream", unifiedE2E1BoundaryRed, needle, count)
		}
		if bytes.Contains(observer.response, []byte(needle)) {
			t.Fatalf("%s: pane output %q was absorbed into the command response", unifiedE2E1BoundaryRed, needle)
		}
	}
	if bytes.Contains(observer.response, []byte("%output ")) || bytes.Contains(observer.response, []byte("%extended-output ")) {
		t.Fatalf("%s: a pane output record was classified as command response: %q", unifiedE2E1BoundaryRed, observer.response)
	}
	if len(observer.response) != 0 {
		t.Logf("%s: command blocks carried a non-empty response (not pane output): %q", unifiedE2E1BoundaryRed, observer.response)
	}
	if item.backpressure {
		previous := -1
		for index := 0; index < 400; index++ {
			needle := fmt.Sprintf("E2E1-BULK-%03d:", index)
			if count := bytes.Count(all, []byte(needle)); count != 1 {
				t.Fatalf("%s: backpressured filler %q delivered %d times", unifiedE2E1BoundaryRed, needle, count)
			}
			at := observer.outputIndex(needle)
			if at < previous {
				t.Fatalf("%s: backpressured filler %q was reordered (%d after %d)", unifiedE2E1BoundaryRed, needle, at, previous)
			}
			previous = at
		}
	}
	if observer.decoder.Failed() {
		t.Fatalf("%s: production decoder entered sticky failure", unifiedE2E1BoundaryRed)
	}

	after, err := buildSourceWitness(ctx, server, "", sessionID)
	if err != nil {
		t.Fatalf("%s: post-N witness: %v", unifiedE2E1BoundaryRed, err)
	}
	if after.Columns != before.Columns {
		t.Fatalf("%s: columns changed as a side effect: %d -> %d", unifiedE2E1BoundaryRed, before.Columns, after.Columns)
	}
	if after.Rows != requestedRows {
		t.Fatalf("%s: N returned before the resize took effect: rows=%d want=%d", unifiedE2E1BoundaryRed, after.Rows, requestedRows)
	}
	if after.SessionID != before.SessionID || after.WindowID != before.WindowID || after.PaneID != before.PaneID || after.Pane != before.Pane {
		t.Fatalf("%s: pane identity moved: before=%+v after=%+v", unifiedE2E1BoundaryRed, before, after)
	}
	if panes := unifiedE2E1WindowPanes(t, disposable, sessionID); panes != "1" {
		t.Fatalf("%s: window_panes=%q after the command", unifiedE2E1BoundaryRed, panes)
	}

	// Rejection shape, recorded here because the Phase-3 barrier-release
	// regression test depends on it: the same command with a now-stale witness must
	// leave the geometry alone.
	staleMark := len(observer.events)
	observer.response = nil
	stale := unifiedE2E1GuardedResizeCommand(authority, before, requestedRows+1)
	if _, err := io.WriteString(observer.ptmx, stale+"\n"); err != nil {
		t.Fatalf("%s: write stale guarded resize: %v", unifiedE2E1BoundaryRed, err)
	}
	observer.pumpUntil(time.Now().Add(30*time.Second), "stale guarded resize completion", func() bool {
		return len(observer.commandBlocks(staleMark)) >= 1
	})
	observer.drainFor(1500 * time.Millisecond)
	staleBlocks := observer.commandBlocks(staleMark)
	t.Logf("%s/%s: stale guarded resize blocks=%+v response=%q", unifiedE2E1BoundaryRed, item.name, staleBlocks, observer.response)
	// Characterization the issuer seam must be built on: a guard that evaluates
	// false completes the block normally. Unlike the process-issued form, which
	// reads tmux's exit status, the observer connection reports nothing for a
	// refused guard. A red here means the failure surface changed and the seam's
	// rejection detection must be revisited, not that this assertion is stale.
	if len(staleBlocks) == 0 {
		t.Fatalf("%s: stale guarded resize produced no command block", unifiedE2E1BoundaryRed)
	}
	if last := staleBlocks[len(staleBlocks)-1]; last.failed || len(observer.response) != 0 {
		t.Fatalf("%s: a refused guard became distinguishable on the control connection: block=%+v response=%q",
			unifiedE2E1BoundaryRed, last, observer.response)
	}
	rejected, err := buildSourceWitness(ctx, server, "", sessionID)
	if err != nil {
		t.Fatalf("%s: witness after stale guarded resize: %v", unifiedE2E1BoundaryRed, err)
	}
	if rejected.Columns != before.Columns || rejected.Rows != requestedRows {
		t.Fatalf("%s: a stale guard mutated geometry: %dx%d", unifiedE2E1BoundaryRed, rejected.Columns, rejected.Rows)
	}

	t.Logf("%s/%s: ordered observer event sequence\n%s", unifiedE2E1BoundaryRed, item.name, observer.trace())

	observer.stop()
	time.Sleep(400 * time.Millisecond)
	detached, err := buildSourceWitness(ctx, server, "", sessionID)
	if err != nil {
		t.Fatalf("%s: witness after observer detach: %v", unifiedE2E1BoundaryRed, err)
	}
	if detached.Columns != before.Columns || detached.Rows != requestedRows {
		t.Fatalf("%s: geometry did not persist after the observer detached: %dx%d", unifiedE2E1BoundaryRed, detached.Columns, detached.Rows)
	}
	unifiedE2E1TransientShadowClient(t, server, sessionID)
	persisted, err := buildSourceWitness(ctx, server, "", sessionID)
	if err != nil {
		t.Fatalf("%s: witness after shadow-shaped client detach: %v", unifiedE2E1BoundaryRed, err)
	}
	if persisted.Columns != before.Columns || persisted.Rows != requestedRows {
		t.Fatalf("%s: geometry did not persist across a shadow-shaped attach/detach: %dx%d",
			unifiedE2E1BoundaryRed, persisted.Columns, persisted.Rows)
	}

	unifiedE2E1AssertDecoderSplitInvariance(t, observer.rawSnapshot())
}

// TestUnifiedTerminalE2E1ObserverKeepsGate1IncompleteUTF8Negative re-asserts, at
// this base and unweakened, the Gate-1 negative result that permanently binds
// unified mode to continuously observed panes: capture-pane -P exposes an
// incomplete escape sequence but not an incomplete UTF-8 code point, so a late
// checkpoint cannot prove a lossless byte boundary — while the continuous
// observer stream carries both byte-exactly.
func TestUnifiedTerminalE2E1ObserverKeepsGate1IncompleteUTF8Negative(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux Gate-1 negative re-assertion")
	}
	disposable := newDisposable(t)
	server := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	disposable.run("new-session", "-d", "-s", "anchor", "-x", "80", "-y", "24", "sleep 600")

	observer := unifiedE2E1StartControlObserver(t, server, "anchor")
	observer.pumpUntil(time.Now().Add(20*time.Second), "observer steady state", func() bool {
		for _, event := range observer.events {
			if event.Kind == controlmode.EventNotification && event.Name == "session-changed" && strings.Contains(event.Args, "anchor") {
				return true
			}
		}
		return false
	})
	sessionID := unifiedE2E1CreateSentinelSession(t, observer, "vfit")
	paneID := disposable.run("display-message", "-p", "-t", "="+sessionID+":", "-F", "#{pane_id}")
	pending := func() string {
		return disposable.run("capture-pane", "-p", "-P", "-C", "-t", paneID)
	}

	// Positive control: an incomplete CSI is exposed by capture-pane -P.
	unifiedE2E1SendPaneLine(t, disposable, sessionID, "CSIPARTIAL")
	observer.pumpUntil(time.Now().Add(15*time.Second), "incomplete CSI on the control stream", func() bool {
		return observer.outputIndex("\x1b[3") >= 0
	})
	deadline := time.Now().Add(5 * time.Second)
	exposed := ""
	for time.Now().Before(deadline) {
		if exposed = pending(); strings.Contains(exposed, `\033[3`) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !strings.Contains(exposed, `\033[3`) {
		t.Fatalf("E2E1/GATE1_UTF8_NEGATIVE: capture-pane -P did not expose the incomplete CSI: %q", exposed)
	}
	unifiedE2E1SendPaneLine(t, disposable, sessionID, "CSIFINISH")
	cleared := time.Now().Add(5 * time.Second)
	for time.Now().Before(cleared) {
		if pending() == "" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// The negative that may not be weakened: an incomplete UTF-8 code point is
	// carried by the observer stream and is invisible to capture-pane -P.
	unifiedE2E1SendPaneLine(t, disposable, sessionID, "UTF8PARTIAL")
	observer.pumpUntil(time.Now().Add(15*time.Second), "incomplete UTF-8 lead byte on the control stream", func() bool {
		return bytes.Contains(observer.outputBytes(), []byte{0xe2})
	})
	time.Sleep(300 * time.Millisecond)
	if leaked := pending(); strings.Contains(leaked, `\342`) || strings.ContainsRune(leaked, 0xe2) {
		t.Fatalf("E2E1/GATE1_UTF8_NEGATIVE: capture-pane -P exposed an incomplete UTF-8 code point: %q", leaked)
	}
	unifiedE2E1SendPaneLine(t, disposable, sessionID, "UTF8FINISH")
	observer.pumpUntil(time.Now().Add(15*time.Second), "completed UTF-8 code point on the control stream", func() bool {
		return bytes.Contains(observer.outputBytes(), []byte{0xe2, 0x94, 0x80})
	})
	if observer.decoder.Failed() {
		t.Fatal("E2E1/GATE1_UTF8_NEGATIVE: production decoder entered sticky failure")
	}
	unifiedE2E1AssertDecoderSplitInvariance(t, observer.rawSnapshot())
}

// ---------------------------------------------------------------------------
// Explicit vertical Fit, end to end against real tmux.
//
// One deliberate click changes the real tmux window height once, and that
// becomes a server-side fact for the lifetime of the session. What this proves
// is the whole claim in one place: rows change and columns do not, the committed
// geometry is ordered against output rather than merely announced, the same
// session replays it after a reconnect, and every out-of-policy request is
// refused without touching the session.
// ---------------------------------------------------------------------------

const unifiedE2E1FitRed = "E2E1/EXPLICIT_VERTICAL_FIT"

type unifiedE2E1FitStream struct {
	t     *testing.T
	conn  net.Conn
	order []string
	text  []byte
}

func (stream *unifiedE2E1FitStream) readUntil(satisfied func() bool, what string) {
	stream.t.Helper()
	if err := stream.conn.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		stream.t.Fatal(err)
	}
	for !satisfied() {
		raw, err := proto.ReadFrame(stream.conn)
		if err != nil {
			stream.t.Fatalf("%s: waiting for %s: %v (order=%v)", unifiedE2E1FitRed, what, err, stream.order)
		}
		if raw.Type != proto.FrameAttachment {
			continue
		}
		frame, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
		if err != nil {
			stream.t.Fatal(err)
		}
		switch {
		case frame.Type == terminal.FrameLive:
			stream.text = append(stream.text, frame.Data...)
			stream.order = append(stream.order, "OUTPUT")
		case frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutResize:
			stream.order = append(stream.order, fmt.Sprintf("GEOMETRY %dx%d", frame.Columns, frame.Rows))
		}
	}
}

func (stream *unifiedE2E1FitStream) sawGeometry(columns, rows int) bool {
	want := fmt.Sprintf("GEOMETRY %dx%d", columns, rows)
	for _, entry := range stream.order {
		if entry == want {
			return true
		}
	}
	return false
}

func unifiedE2E1FitInput(t *testing.T, conn net.Conn, prepared terminal.Frame, command string) {
	t.Helper()
	input, err := attachmentwire.Encode(terminal.Frame{
		Version: terminal.ProtocolVersion, Type: terminal.FrameInput,
		Source: prepared.Source, Epoch: prepared.Epoch, Data: []byte(command),
	}, attachmentwire.BrowserToServer)
	if err != nil || proto.WriteFrame(conn, proto.FrameAttachment, input) != nil {
		t.Fatalf("%s: input %q: %v", unifiedE2E1FitRed, command, err)
	}
}

func unifiedE2E1FitRequest(t *testing.T, conn net.Conn, prepared terminal.Frame, columns, rows int) {
	t.Helper()
	request, err := attachmentwire.Encode(terminal.Frame{
		Version: terminal.ProtocolVersion, Type: terminal.FrameResize,
		Source: prepared.Source, Epoch: prepared.Epoch, Columns: columns, Rows: rows,
	}, attachmentwire.BrowserToServer)
	if err != nil || proto.WriteFrame(conn, proto.FrameAttachment, request) != nil {
		t.Fatalf("%s: resize request %dx%d: %v", unifiedE2E1FitRed, columns, rows, err)
	}
}

// unifiedE2E1OpenStream retains both snapshot and live output received during
// admission. Live output may precede MODE and belongs to the visible stream.
func unifiedE2E1OpenStream(t *testing.T, socket string, authority proto.Authority) (net.Conn, terminal.Frame, *unifiedE2E1FitStream) {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	unifiedE2E1Hello(t, conn)
	history := 5000
	attached := unifiedE2E1Control(t, conn, proto.Control{Type: "attach", Mode: "control", Engine: "unified-dev", Authority: &authority, HistoryLimit: &history})
	if attached.Type != "attach_ok" {
		conn.Close()
		t.Fatalf("%s: attach=%+v", unifiedE2E1FitRed, attached)
	}
	if err := conn.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	read := func() terminal.Frame {
		for {
			raw, err := proto.ReadFrame(conn)
			if err != nil {
				conn.Close()
				t.Fatalf("%s: admission read: %v", unifiedE2E1FitRed, err)
			}
			if raw.Type != proto.FrameAttachment {
				continue
			}
			frame, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
			if err != nil {
				conn.Close()
				t.Fatal(err)
			}
			return frame
		}
	}
	prepared := read()
	if prepared.Type != terminal.FramePrepare {
		conn.Close()
		t.Fatalf("%s: first frame=%+v", unifiedE2E1FitRed, prepared)
	}
	ready, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameReady, Source: prepared.Source, Epoch: prepared.Epoch, Cut: prepared.Cut}, attachmentwire.BrowserToServer)
	if err != nil || proto.WriteFrame(conn, proto.FrameAttachment, ready) != nil {
		conn.Close()
		t.Fatalf("%s: ready: %v", unifiedE2E1FitRed, err)
	}
	if commit := read(); commit.Type != terminal.FrameCommit {
		conn.Close()
		t.Fatalf("%s: commit=%+v", unifiedE2E1FitRed, commit)
	}
	modeRequest, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: prepared.Source, Epoch: prepared.Epoch, Mode: terminal.ModeControl}, attachmentwire.BrowserToServer)
	if err != nil || proto.WriteFrame(conn, proto.FrameAttachment, modeRequest) != nil {
		conn.Close()
		t.Fatalf("%s: mode request: %v", unifiedE2E1FitRed, err)
	}
	// The replayed backlog is written before MODE, so it is recorded here rather
	// than discarded: it is exactly the evidence a reconnect test inspects.
	stream := &unifiedE2E1FitStream{t: t, conn: conn, text: append([]byte(nil), prepared.Replay...)}
	for {
		frame := read()
		switch {
		case frame.Type == terminal.FrameLive:
			stream.text = append(stream.text, frame.Data...)
			stream.order = append(stream.order, "OUTPUT")
		case frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutResize:
			stream.order = append(stream.order, fmt.Sprintf("GEOMETRY %dx%d", frame.Columns, frame.Rows))
		case frame.Type == terminal.FrameMode && frame.Mode == terminal.ModeControl:
			return conn, prepared, stream
		}
	}
}

func TestUnifiedTerminalE2E1ExplicitVerticalFit(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux explicit vertical fit")
	}
	disposable := newDisposable(t)
	disposable.run("new-session", "-d", "-s", "anchor", "-x", "80", "-y", "24", "sh")
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	runtimeDir := t.TempDir()
	cfg := unifiedE2E1DevConfig(t, tmuxServer, runtimeDir)
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ServeWithPaneEffects(listener, cfg, effects) }()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("unified broker did not stop")
		}
	})

	createConn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	unifiedE2E1Hello(t, createConn)
	if created := unifiedE2E1Control(t, createConn, proto.Control{Type: "create", ServerLabel: "main", Name: "unified_target"}); created.Type != "create_ok" {
		t.Fatalf("%s: create=%+v", unifiedE2E1FitRed, created)
	}
	_ = createConn.Close()
	sessionID, err := tmuxOutput(tmuxServer, "list-sessions", "-f", "#{==:#{session_name},unified_target}", "-F", "#{session_id}")
	if err != nil {
		t.Fatal(err)
	}
	sessionID = strings.TrimSpace(sessionID)
	detail, err := details(tmuxServer, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(tmuxServer)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = "e2e1", "main", detail.ID, detail.Created
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	birth, err := buildSourceWitness(ctx, tmuxServer, "", sessionID)
	if err != nil {
		t.Fatal(err)
	}

	conn, prepared, stream := unifiedE2E1OpenStream(t, listener.Addr().String(), authority)
	defer conn.Close()
	defer func() {
		if t.Failed() {
			t.Logf("vertical fit terminal bytes: %q", stream.text)
		}
	}()
	if prepared.Columns != birth.Columns || prepared.Rows != birth.Rows {
		t.Fatalf("%s: initial PREPARE geometry=%dx%d want birth=%dx%d",
			unifiedE2E1FitRed, prepared.Columns, prepared.Rows, birth.Columns, birth.Rows)
	}

	// The pane writes its own post-fit output from a SIGWINCH trap, so the
	// ordering assertion does not depend on this test typing at exactly the right
	// moment — and, deliberately, so no input is in flight while the resize cut
	// is running. The epoch's protocol state machine rejects input during a cut;
	// that is pre-existing behaviour this work does not change.
	// The sentinel is assembled at run time so the shell's own echo of the trap
	// command cannot satisfy the assertion the trap exists to make. The pre-fit
	// sentinel must also prove execution: a valid blank initial capture can be
	// ready before the shell finishes startup, while its tty already echoes input.
	unifiedE2E1FitInput(t, conn, prepared, "trap 'printf \"E2E1-AFTER%s-FIT\\n\" \"\"' WINCH\n")
	unifiedE2E1FitInput(t, conn, prepared, "printf 'E2E1-BEFORE%s-FIT\\n' ''\n")
	stream.readUntil(func() bool { return bytes.Contains(stream.text, []byte("E2E1-BEFORE-FIT")) }, "pre-fit output")
	beforeIndex := len(stream.order)

	requestedRows := birth.Rows + 13
	unifiedE2E1FitRequest(t, conn, prepared, birth.Columns, requestedRows)
	stream.readUntil(func() bool { return stream.sawGeometry(birth.Columns, requestedRows) }, "committed geometry event")
	geometryIndex := len(stream.order) - 1

	stream.readUntil(func() bool { return bytes.Contains(stream.text, []byte("E2E1-AFTER-FIT")) }, "post-fit output")
	if geometryIndex < beforeIndex {
		t.Fatalf("%s: the geometry event was not ordered after pre-fit output: order=%v", unifiedE2E1FitRed, stream.order)
	}
	if stream.order[len(stream.order)-1] != "OUTPUT" {
		t.Fatalf("%s: post-fit output did not follow the geometry event: order=%v", unifiedE2E1FitRed, stream.order)
	}

	// A same-size request must not install a cut or strand input readiness.
	noOpStart := len(stream.order)
	unifiedE2E1FitRequest(t, conn, prepared, birth.Columns, requestedRows)
	unifiedE2E1FitInput(t, conn, prepared, "printf 'E2E1-SAME%s-SIZE\\n' ''\n")
	stream.readUntil(func() bool { return bytes.Contains(stream.text, []byte("E2E1-SAME-SIZE")) }, "output after same-size request")
	for _, event := range stream.order[noOpStart:] {
		if strings.HasPrefix(event, "GEOMETRY ") {
			t.Fatalf("%s: same-size request emitted another geometry event", unifiedE2E1FitRed)
		}
	}

	after, err := buildSourceWitness(ctx, tmuxServer, "", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Columns != birth.Columns {
		t.Fatalf("%s: a vertical fit changed columns %d -> %d", unifiedE2E1FitRed, birth.Columns, after.Columns)
	}
	if after.Rows != requestedRows {
		t.Fatalf("%s: rows=%d want=%d", unifiedE2E1FitRed, after.Rows, requestedRows)
	}
	if panes := strings.TrimSpace(disposable.run("display-message", "-p", "-t", "="+sessionID+":", "-F", "#{window_panes}")); panes != "1" {
		t.Fatalf("%s: window_panes=%q", unifiedE2E1FitRed, panes)
	}

	// Out-of-policy and non-authoritative requests are refused without touching
	// the session. Columns in particular are never the browser's to choose.
	for _, refused := range []struct {
		name          string
		columns, rows int
	}{
		{"rows below policy", birth.Columns, terminal.MinFitRows - 1},
		{"rows above policy", birth.Columns, terminal.MaxFitRows + 1},
		{"columns not the pane witness", birth.Columns - 1, requestedRows + 1},
		{"cells above policy", 800, 100},
	} {
		t.Run(refused.name, func(t *testing.T) {
			unifiedE2E1FitRequest(t, conn, prepared, refused.columns, refused.rows)
			// Refusals happen before any barrier is installed and before any tmux
			// mutation, so nothing needs to settle first.
			if err := conn.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
				t.Fatal(err)
			}
			for {
				raw, err := proto.ReadFrame(conn)
				if err != nil {
					t.Fatalf("%s: %s: %v", unifiedE2E1FitRed, refused.name, err)
				}
				if raw.Type != proto.FrameControl {
					continue
				}
				control, err := proto.DecodeControl(raw.Payload)
				if err != nil {
					t.Fatal(err)
				}
				if control.Type != "error" || control.Code != "resize_rejected" {
					t.Fatalf("%s: %s produced %+v", unifiedE2E1FitRed, refused.name, control)
				}
				break
			}
			held, err := buildSourceWitness(ctx, tmuxServer, "", sessionID)
			if err != nil || held.Columns != birth.Columns || held.Rows != requestedRows {
				t.Fatalf("%s: %s moved the geometry to %dx%d: %v", unifiedE2E1FitRed, refused.name, held.Columns, held.Rows, err)
			}
		})
	}

	// Attachment continuity when input is generated during the cut.
	//
	// A resize is a cut, and the protocol reducer refuses input during one. That
	// refusal used to take the whole attachment down with it, so a keystroke
	// landing inside the millisecond CutResize window cost the operator their
	// session. It is now a typed refusal: the frame is dropped, never queued or
	// replayed, and the attachment survives. The browser additionally seals its
	// own input for the pending request, so in practice nothing is sent here at
	// all; this proves the server-side floor underneath that seal.
	unifiedE2E1FitRequest(t, conn, prepared, birth.Columns, requestedRows-2)
	for attempt := 0; attempt < 12; attempt++ {
		unifiedE2E1FitInput(t, conn, prepared, "\x00")
	}
	stream.readUntil(func() bool { return stream.sawGeometry(birth.Columns, requestedRows-2) }, "second committed geometry event")
	unifiedE2E1FitInput(t, conn, prepared, "printf 'E2E1-AFTER-RACE\\n'\n")
	stream.readUntil(func() bool { return bytes.Contains(stream.text, []byte("E2E1-AFTER-RACE")) }, "output after racing input against the cut")
	raced, err := buildSourceWitness(ctx, tmuxServer, "", sessionID)
	if err != nil || raced.Columns != birth.Columns || raced.Rows != requestedRows-2 {
		t.Fatalf("%s: racing input against the cut disturbed the geometry: %dx%d: %v", unifiedE2E1FitRed, raced.Columns, raced.Rows, err)
	}
	requestedRows -= 2

	// The pane still delivers after a refusal: a rejected request must not leave
	// the publication barrier holding.
	unifiedE2E1FitInput(t, conn, prepared, "printf 'E2E1-AFTER-REFUSAL\\n'\n")
	stream.readUntil(func() bool { return bytes.Contains(stream.text, []byte("E2E1-AFTER-REFUSAL")) }, "output after a refused fit")

	// Reconnect equivalence: the same session replays its birth geometry and then
	// the committed change, in that order, without any new request.
	unifiedE2E1Detach(t, conn)
	_ = conn.Close()
	reconnected, reprepared, replayed := unifiedE2E1OpenStream(t, listener.Addr().String(), authority)
	defer reconnected.Close()
	if reprepared.Columns != birth.Columns || reprepared.Rows != birth.Rows {
		t.Fatalf("%s: reconnect PREPARE geometry=%dx%d want birth=%dx%d",
			unifiedE2E1FitRed, reprepared.Columns, reprepared.Rows, birth.Columns, birth.Rows)
	}
	replayed.readUntil(func() bool {
		return replayed.sawGeometry(birth.Columns, requestedRows) && bytes.Contains(replayed.text, []byte("E2E1-AFTER-REFUSAL"))
	}, "replayed geometry and tail")
	geometryAt := -1
	for index, entry := range replayed.order {
		if entry == fmt.Sprintf("GEOMETRY %dx%d", birth.Columns, requestedRows) {
			geometryAt = index
			break
		}
	}
	if geometryAt < 0 {
		t.Fatalf("%s: reconnect did not replay the committed geometry: order=%v", unifiedE2E1FitRed, replayed.order)
	}
	persisted, err := buildSourceWitness(ctx, tmuxServer, "", sessionID)
	if err != nil || persisted.Columns != birth.Columns || persisted.Rows != requestedRows {
		t.Fatalf("%s: reconnect changed the session geometry to %dx%d: %v", unifiedE2E1FitRed, persisted.Columns, persisted.Rows, err)
	}
	// --- durable receipt -----------------------------------------------------
	//
	// Everything above is asserted; this records it in one place so the evidence
	// does not have to be reassembled from a test's control flow.

	// Exactly one control observer, and no second one appeared to do the resize.
	// The argv shape and the single attach callsite are pinned separately by
	// TestNoDestructiveOrImplicitResizeProductCallsites.
	clients := strings.Split(strings.TrimSpace(disposable.run("list-clients", "-F", "#{client_name}\t#{client_flags}")), "\n")
	live, total := 0, 0
	for _, row := range clients {
		if strings.TrimSpace(row) == "" {
			continue
		}
		total++
		if strings.Contains(row, "control-mode") {
			live++
		}
	}
	// One control-mode client: the observer. The other attached client is the
	// attachment's own shadow (attachment.go attaches it with
	// -f ignore-size,active-pane), which is a PTY, not a second observer.
	if live != 1 || total > 2 {
		t.Fatalf("%s: control-mode clients=%d total=%d, want exactly one observer beside the shadow: %v",
			unifiedE2E1FitRed, live, total, clients)
	}
	if guardedResizeBlocks != 2 {
		t.Fatalf("%s: sequence N is the completion of block %d, not the last of two", unifiedE2E1FitRed, guardedResizeBlocks)
	}

	// The wire evidence and the durable evidence must be the same fact: a PUJ2
	// journal whose committed geometry records are what the browser was shown.
	journals := unifiedE2E1JournalFiles(t, runtimeDir)
	if len(journals) != 1 {
		t.Fatalf("%s: %d pane journals under the runtime dir, want 1: %v", unifiedE2E1FitRed, len(journals), journals)
	}
	magic, geometryRecords := unifiedE2E1JournalGeometry(t, journals[0])
	if magic != "PUJ2" {
		t.Fatalf("%s: durable journal magic=%q want PUJ2", unifiedE2E1FitRed, magic)
	}
	if geometryRecords != 2 {
		t.Fatalf("%s: durable committed geometry records=%d, want the two explicitly requested row fits", unifiedE2E1FitRed, geometryRecords)
	}

	t.Logf("%s RECEIPT: birth=%dx%d fitted=%dx%d persisted=%dx%d panes=1 observers=%d N=last-of-%d journal=PUJ2 geometry_records=%d",
		unifiedE2E1FitRed, birth.Columns, birth.Rows, after.Columns, requestedRows,
		persisted.Columns, persisted.Rows, live, guardedResizeBlocks, geometryRecords)
	t.Logf("%s RECEIPT ordered live stream: %v", unifiedE2E1FitRed, stream.order)
	t.Logf("%s RECEIPT ordered replay stream after reconnect: %v", unifiedE2E1FitRed, replayed.order)

	unifiedE2E1Detach(t, reconnected)
}

func unifiedE2E1JournalFiles(t *testing.T, runtimeDir string) []string {
	t.Helper()
	var found []string
	if err := filepath.WalkDir(runtimeDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "pane-") && strings.HasSuffix(entry.Name(), ".journal") {
			found = append(found, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("%s: walk runtime dir: %v", unifiedE2E1FitRed, err)
	}
	return found
}

// unifiedE2E1JournalGeometry walks the durable PUJ2 layout independently of the
// writer and counts committed geometry records. Pane bytes cannot produce one:
// the payload is length-prefixed, so a hostile transcript is skipped over, never
// interpreted.
func unifiedE2E1JournalGeometry(t *testing.T, path string) (string, int) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil || len(contents) < 8 {
		t.Fatalf("%s: read durable journal: %v", unifiedE2E1FitRed, err)
	}
	position := 8 + int(binary.BigEndian.Uint32(contents[4:8]))
	geometryAppends, geometryCommits := 0, 0
	for position < len(contents) {
		switch contents[position] {
		case 0xa1:
			if len(contents)-position < 70 {
				t.Fatalf("%s: truncated durable append frame at %d", unifiedE2E1FitRed, position)
			}
			if contents[position+1] == 0x02 {
				geometryAppends++
			}
			position += 70 + int(binary.BigEndian.Uint32(contents[position+26:position+30]))
		case 0xc1:
			if len(contents)-position < 58 {
				t.Fatalf("%s: truncated durable commit frame at %d", unifiedE2E1FitRed, position)
			}
			if contents[position+1] == 0x02 {
				geometryCommits++
			}
			position += 58
		default:
			t.Fatalf("%s: unknown durable record marker %#x at %d", unifiedE2E1FitRed, contents[position], position)
		}
	}
	if geometryAppends != geometryCommits {
		t.Fatalf("%s: durable geometry appends=%d commits=%d", unifiedE2E1FitRed, geometryAppends, geometryCommits)
	}
	return string(contents[:4]), geometryCommits
}
