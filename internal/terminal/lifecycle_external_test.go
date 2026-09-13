package terminal_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	terminal "persea-terminal/internal/terminal"
)

type fakeProbe struct {
	mu      sync.Mutex
	witness map[int]terminal.ProcessWitness
}

func (p *fakeProbe) Witness(_ context.Context, pid int) (terminal.ProcessWitness, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.witness[pid]
	if !ok {
		return terminal.ProcessWitness{}, errors.New("missing process")
	}
	return w, nil
}

type fakeTransaction struct {
	mu               sync.Mutex
	actions          []terminal.PinnedAction
	requests         []terminal.TransactionRequest
	onBind           func(terminal.TransactionRequest) error
	onCut            func(terminal.TransactionRequest) error
	cutCapture       []byte
	callbackErr      error
	cleanupFailures  int
	cleanupErr       error
	cleanupResult    *terminal.TransactionResult
	cleanupStarted   chan struct{}
	cleanupRelease   chan struct{}
	cleanupStartOnce sync.Once
	bindResult       *terminal.TransactionResult
	bindErr          error
	cutErr           error
	resizeErr        error
	onResize         func(terminal.TransactionRequest) error
	bindStarted      chan struct{}
	bindRelease      chan struct{}
	bindStartedOnce  sync.Once
	cutStarted       chan struct{}
	cutRelease       chan struct{}
	cutStartedOnce   sync.Once
	activeCuts       int
	maximumActiveCut int
}

func (t *fakeTransaction) RunPinned(ctx context.Context, req terminal.TransactionRequest) (terminal.TransactionResult, error) {
	t.mu.Lock()
	t.actions = append(t.actions, req.Action)
	t.requests = append(t.requests, req)
	if req.Action == terminal.ActionCleanup && t.cleanupStarted != nil {
		t.cleanupStartOnce.Do(func() { close(t.cleanupStarted) })
		release := t.cleanupRelease
		t.mu.Unlock()
		if release != nil {
			<-release
		}
		t.mu.Lock()
	}
	if req.Action == terminal.ActionCleanup && t.cleanupFailures > 0 {
		t.cleanupFailures--
		err := t.cleanupErr
		t.mu.Unlock()
		return terminal.TransactionResult{}, err
	}
	if req.Action == terminal.ActionCut {
		t.activeCuts++
		if t.activeCuts > t.maximumActiveCut {
			t.maximumActiveCut = t.activeCuts
		}
	}
	onBind, onCut, capture, cutErr := t.onBind, t.onCut, append([]byte(nil), t.cutCapture...), t.cutErr
	onResize, resizeErr := t.onResize, t.resizeErr
	cleanupResult := t.cleanupResult
	bindResult, bindErr, bindStarted, bindRelease := t.bindResult, t.bindErr, t.bindStarted, t.bindRelease
	started, release := t.cutStarted, t.cutRelease
	t.mu.Unlock()
	if req.Action == terminal.ActionBind {
		if onBind != nil {
			if err := onBind(req); err != nil {
				t.mu.Lock()
				t.callbackErr = err
				t.mu.Unlock()
			}
		}
		if bindStarted != nil {
			t.bindStartedOnce.Do(func() { close(bindStarted) })
		}
		if bindRelease != nil {
			<-bindRelease
		}
		if bindResult != nil {
			return *bindResult, bindErr
		}
	}
	if req.Action == terminal.ActionCut {
		if onCut != nil {
			if err := onCut(req); err != nil {
				t.mu.Lock()
				t.callbackErr = err
				t.mu.Unlock()
			}
		}
		if started != nil {
			t.cutStartedOnce.Do(func() { close(started) })
		}
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				t.mu.Lock()
				t.activeCuts--
				t.mu.Unlock()
				return terminal.TransactionResult{}, ctx.Err()
			}
		}
		t.mu.Lock()
		t.activeCuts--
		t.mu.Unlock()
		if cutErr != nil {
			return terminal.TransactionResult{}, cutErr
		}
	}
	if req.Action == terminal.ActionCleanup && cleanupResult != nil {
		return *cleanupResult, nil
	}
	attachment := req.Attachment
	if req.Action == terminal.ActionBind {
		attachment = terminal.AttachmentIDs{ShadowSessionID: "$shadow", ClientID: "$client"}
		return terminal.TransactionResult{Witness: req.Witness, Attachment: attachment, Capture: terminal.CutCapture{Capture: capture}}, bindErr
	}
	if req.Action == terminal.ActionResize {
		if onResize != nil {
			if err := onResize(req); err != nil {
				t.mu.Lock()
				t.callbackErr = err
				t.mu.Unlock()
			}
		}
		if resizeErr != nil {
			return terminal.TransactionResult{}, resizeErr
		}
		resized := req.Witness
		resized.Columns, resized.Rows = req.Columns, req.Rows
		resized.Incarnation += "-resized"
		return terminal.TransactionResult{Witness: resized, Attachment: attachment}, nil
	}
	return terminal.TransactionResult{Witness: req.Witness, Attachment: attachment, Capture: terminal.CutCapture{Capture: capture}}, nil
}

func (t *fakeTransaction) snapshot() ([]terminal.PinnedAction, []terminal.TransactionRequest, error, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]terminal.PinnedAction(nil), t.actions...), append([]terminal.TransactionRequest(nil), t.requests...), t.callbackErr, t.maximumActiveCut
}

type memoryTransport struct {
	mu         sync.Mutex
	frames     []terminal.Frame
	notify     chan struct{}
	block      bool
	writeErr   error
	closeErr   error
	closed     bool
	closeCount int
}

func newMemoryTransport() *memoryTransport {
	return &memoryTransport{notify: make(chan struct{}, 1)}
}

func (w *memoryTransport) WriteFrame(ctx context.Context, raw []byte) error {
	if w.block {
		<-ctx.Done()
		if w.writeErr != nil {
			return w.writeErr
		}
		return ctx.Err()
	}
	f, err := terminal.DecodeFrame(raw)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.frames = append(w.frames, f)
	w.mu.Unlock()
	select {
	case w.notify <- struct{}{}:
	default:
	}
	return nil
}

func (w *memoryTransport) Close(context.Context) error {
	w.mu.Lock()
	w.closed = true
	w.closeCount++
	w.mu.Unlock()
	return w.closeErr
}

func (w *memoryTransport) snapshot() []terminal.Frame {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]terminal.Frame(nil), w.frames...)
}

func (w *memoryTransport) wait(t *testing.T, predicate func([]terminal.Frame) bool) []terminal.Frame {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		frames := w.snapshot()
		if predicate(frames) {
			return frames
		}
		select {
		case <-w.notify:
		case <-deadline.C:
			t.Fatalf("timed out waiting for frames: %#v", frames)
		}
	}
}

type recordingPTY struct {
	mu         sync.Mutex
	written    []byte
	maxWrite   int
	blockFirst bool
	started    chan struct{}
	startOnce  sync.Once
	calls      int
	closeErr   error
}

func newRecordingPTY() *recordingPTY { return &recordingPTY{started: make(chan struct{})} }

func (w *recordingPTY) WriteContext(ctx context.Context, data []byte) (int, error) {
	w.mu.Lock()
	w.calls++
	call := w.calls
	limit := len(data)
	if w.maxWrite > 0 && limit > w.maxWrite {
		limit = w.maxWrite
	}
	if w.blockFirst && call == 1 && limit > 1 {
		limit /= 2
	}
	w.written = append(w.written, data[:limit]...)
	w.mu.Unlock()
	if w.blockFirst && call == 1 {
		w.startOnce.Do(func() { close(w.started) })
		<-ctx.Done()
		return limit, terminal.ErrCanceled
	}
	return limit, nil
}

func (w *recordingPTY) Close() error { return w.closeErr }

func (w *recordingPTY) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.written...)
}

func witness() terminal.SourceWitness {
	return terminal.SourceWitness{
		Incarnation: "opaque-source-1",
		Socket:      terminal.SocketIdentity{Path: "/private/tmp/tmux.sock", Device: 1, Inode: 2},
		Server:      terminal.ProcessWitness{PID: 101, StartTime: 1001},
		SessionID:   "$1", WindowID: "@2", PaneID: "%3",
		Pane: terminal.ProcessWitness{PID: 202, StartTime: 2002}, Columns: 120, Rows: 40,
	}
}

func sourceFor(t *testing.T, tx *fakeTransaction) (*terminal.PinnedSource, *fakeProbe) {
	t.Helper()
	w := witness()
	probe := &fakeProbe{witness: map[int]terminal.ProcessWitness{w.Server.PID: w.Server, w.Pane.PID: w.Pane}}
	source, err := terminal.NewPinnedSource(w, tx, probe)
	if err != nil {
		t.Fatal(err)
	}
	return source, probe
}

func config() terminal.Config {
	return terminal.Config{Clock: terminal.RealClock{}, QuietInterval: 20 * time.Millisecond, MaximumInterval: 60 * time.Millisecond, CutTimeout: time.Second}
}

func marker(req terminal.TransactionRequest) []byte {
	return []byte("\x1b]52;;" + string(req.Marker) + "\a")
}

func browserFrame(kind terminal.FrameType, cut uint64) terminal.Frame {
	return terminal.Frame{Version: terminal.ProtocolVersion, Type: kind, Source: "opaque-source-1", Epoch: 9, Cut: cut}
}

func startLive(t *testing.T, e *terminal.Epoch, wire *memoryTransport) {
	t.Helper()
	if err := e.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	frames := wire.wait(t, func(fs []terminal.Frame) bool { return len(fs) >= 1 && fs[0].Type == terminal.FramePrepare })
	if err := e.HandleFrame(browserFrame(terminal.FrameReady, frames[0].Cut)); err != nil {
		t.Fatal(err)
	}
	wire.wait(t, func(fs []terminal.Frame) bool {
		for _, f := range fs {
			if f.Type == terminal.FrameCommit && f.Cut == frames[0].Cut {
				return true
			}
		}
		return false
	})
}

func TestPublicInitialLifecycleReplayCommitLiveInputAndExactCleanup(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("old-1\nold-2\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error {
		return epoch.PTYBytes(append(append([]byte("redraw"), marker(req)...), []byte("held")...))
	}
	var err error
	epoch, err = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
	if err != nil {
		t.Fatal(err)
	}
	if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatalf("exact active start was not idempotent: %v", err)
	}
	if err := epoch.Start(context.Background(), terminal.CutReconnect); !errors.Is(err, terminal.ErrOutOfState) {
		t.Fatalf("conflicting active start err=%v", err)
	}
	frames := wire.wait(t, func(fs []terminal.Frame) bool { return len(fs) >= 1 })
	prepare := frames[0]
	if prepare.Type != terminal.FramePrepare || prepare.Kind != terminal.CutInitial || string(prepare.Replay) != "redraw" || !reflect.DeepEqual(prepare.History, []string{"old-1", "old-2"}) {
		t.Fatalf("prepare=%#v", prepare)
	}
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, prepare.Cut)); err != nil {
		t.Fatal(err)
	}
	if err := epoch.PTYBytes([]byte("ordinary")); err != nil {
		t.Fatal(err)
	}
	if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: "opaque-source-1", Epoch: 9, Data: []byte("denied")}); !errors.Is(err, terminal.ErrObserveOnly) {
		t.Fatalf("Observe input err=%v", err)
	}
	mode := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: "opaque-source-1", Epoch: 9, Mode: terminal.ModeControl}
	if err := epoch.HandleFrame(mode); err != nil {
		t.Fatal(err)
	}
	input := []byte("\x1b[A\x1b[Bpaste\n")
	if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: "opaque-source-1", Epoch: 9, Data: input}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !reflect.DeepEqual(pty.bytes(), input) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !reflect.DeepEqual(pty.bytes(), input) {
		t.Fatalf("pty=%q want=%q", pty.bytes(), input)
	}
	frames = wire.wait(t, func(fs []terminal.Frame) bool { return len(fs) >= 5 })
	want := []terminal.FrameType{terminal.FramePrepare, terminal.FrameCommit, terminal.FrameLive, terminal.FrameLive, terminal.FrameMode}
	got := make([]terminal.FrameType, 0, len(want))
	for _, frame := range frames[:len(want)] {
		got = append(got, frame.Type)
	}
	if !reflect.DeepEqual(got, want) || string(frames[2].Data) != "held" || string(frames[3].Data) != "ordinary" {
		t.Fatalf("frames=%#v", frames)
	}
	if err := epoch.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	actions, requests, callbackErr, maximum := tx.snapshot()
	if callbackErr != nil || maximum != 1 || !reflect.DeepEqual(actions, []terminal.PinnedAction{terminal.ActionBind, terminal.ActionCut, terminal.ActionCleanup}) {
		t.Fatalf("actions=%v callback=%v max=%d", actions, callbackErr, maximum)
	}
	if requests[1].Cut != prepare.Cut || requests[1].Kind != terminal.CutInitial || len(requests[1].Marker) == 0 {
		t.Fatalf("cut request=%#v", requests[1])
	}
}

func TestReconnectUsesTheSamePinnedReplayPipeline(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("reconnect-history\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error {
		return epoch.PTYBytes(append(append([]byte("reconnect-redraw"), marker(req)...), []byte("reconnect-held")...))
	}
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
	if err := epoch.Start(context.Background(), terminal.CutReconnect); err != nil {
		t.Fatal(err)
	}
	prepare := wire.wait(t, func(fs []terminal.Frame) bool { return len(fs) > 0 })[0]
	if prepare.Kind != terminal.CutReconnect || string(prepare.Replay) != "reconnect-redraw" {
		t.Fatalf("reconnect prepare=%#v", prepare)
	}
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, prepare.Cut)); err != nil {
		t.Fatal(err)
	}
	frames := wire.wait(t, func(fs []terminal.Frame) bool { return len(fs) >= 3 })
	if frames[1].Type != terminal.FrameCommit || frames[2].Type != terminal.FrameLive || string(frames[2].Data) != "reconnect-held" {
		t.Fatalf("reconnect frames=%#v", frames)
	}
	_ = epoch.Finalize(context.Background())
}

func TestOngoingDeferRetriesAndActivityDuringCutIsNotLost(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error {
		prefix, suffix := "initial", ""
		if req.Kind == terminal.CutOngoing {
			prefix, suffix = "delta", "held-"+string(rune('0'+req.Cut))
		}
		return epoch.PTYBytes(append(append([]byte(prefix), marker(req)...), []byte(suffix)...))
	}
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
	startLive(t, epoch, wire)
	if err := epoch.PTYBytes([]byte("activity")); err != nil {
		t.Fatal(err)
	}
	frames := wire.wait(t, func(fs []terminal.Frame) bool {
		for _, f := range fs {
			if f.Type == terminal.FramePrepare && f.Kind == terminal.CutOngoing {
				return true
			}
		}
		return false
	})
	var second terminal.Frame
	for _, f := range frames {
		if f.Type == terminal.FramePrepare && f.Kind == terminal.CutOngoing {
			second = f
			break
		}
	}
	deferFrame := browserFrame(terminal.FrameDefer, second.Cut)
	deferFrame.Reason = "visible aggregate untouched"
	if err := epoch.HandleFrame(deferFrame); err != nil {
		t.Fatal(err)
	}
	frames = wire.wait(t, func(fs []terminal.Frame) bool {
		prepares := 0
		for _, f := range fs {
			if f.Type == terminal.FramePrepare {
				prepares++
			}
		}
		return prepares >= 3
	})
	var third terminal.Frame
	for _, f := range frames {
		if f.Type == terminal.FramePrepare && f.Cut > second.Cut {
			third = f
			break
		}
	}
	if third.Cut == 0 {
		t.Fatalf("DEFER did not retry: %#v", frames)
	}
	if err := epoch.PTYBytes([]byte("during-cut")); err != nil {
		t.Fatal(err)
	}
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, third.Cut)); err != nil {
		t.Fatal(err)
	}
	wire.wait(t, func(fs []terminal.Frame) bool {
		for _, f := range fs {
			if f.Type == terminal.FramePrepare && f.Cut > third.Cut {
				return true
			}
		}
		return false
	})
	frames = wire.snapshot()
	deferLive, badCommit := false, false
	for _, f := range frames {
		if f.Cut == second.Cut && f.Type == terminal.FrameLive && strings.HasPrefix(string(f.Data), "held-") {
			deferLive = true
		}
		if f.Cut == second.Cut && f.Type == terminal.FrameCommit {
			badCommit = true
		}
	}
	if !deferLive || badCommit {
		t.Fatalf("operational DEFER frames=%#v", frames)
	}
	_ = epoch.Finalize(context.Background())
}

func TestSchedulerMaximumCadenceQuietAndOneInflightCut(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	var epoch *terminal.Epoch
	cutNumber := 0
	tx.onCut = func(req terminal.TransactionRequest) error {
		cutNumber++
		return epoch.PTYBytes(marker(req))
	}
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
	startLive(t, epoch, wire)
	start := time.Now()
	for time.Since(start) < 100*time.Millisecond {
		if err := epoch.PTYBytes([]byte("x")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
		frames := wire.snapshot()
		for _, f := range frames {
			if f.Type == terminal.FramePrepare && f.Kind == terminal.CutOngoing {
				goto due
			}
		}
	}
	t.Fatal("sustained activity postponed cut beyond maximum cadence")
due:
	if time.Since(start) > 95*time.Millisecond {
		t.Fatalf("maximum cadence fired too late: %v", time.Since(start))
	}
	actions, _, _, maximum := tx.snapshot()
	cutActions := 0
	for _, action := range actions {
		if action == terminal.ActionCut {
			cutActions++
		}
	}
	if cutActions != 2 || maximum != 1 || cutNumber != 2 {
		t.Fatalf("cutActions=%d max=%d cutNumber=%d", cutActions, maximum, cutNumber)
	}
	_ = epoch.Finalize(context.Background())
}

func TestOnlyOneCutRunsAndBlockedCutActivitySchedulesSuccessor(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error { return epoch.PTYBytes(marker(req)) }
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
	startLive(t, epoch, wire)
	started, release := make(chan struct{}), make(chan struct{})
	tx.mu.Lock()
	tx.cutStarted, tx.cutRelease = started, release
	tx.mu.Unlock()
	if err := epoch.PTYBytes([]byte("trigger")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("ongoing cut did not start")
	}
	if err := epoch.PTYBytes([]byte("during-blocked-cut")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * config().MaximumInterval)
	actions, _, _, maximum := tx.snapshot()
	cutCount := 0
	for _, action := range actions {
		if action == terminal.ActionCut {
			cutCount++
		}
	}
	if cutCount != 2 || maximum != 1 {
		t.Fatalf("parallel/lost cut evidence actions=%v maximum=%d", actions, maximum)
	}
	close(release)
	frames := wire.wait(t, func(fs []terminal.Frame) bool {
		for _, f := range fs {
			if f.Type == terminal.FramePrepare && f.Kind == terminal.CutOngoing {
				return true
			}
		}
		return false
	})
	var ongoing terminal.Frame
	for _, frame := range frames {
		if frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutOngoing {
			ongoing = frame
			break
		}
	}
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, ongoing.Cut)); err != nil {
		t.Fatal(err)
	}
	wire.wait(t, func(fs []terminal.Frame) bool {
		for _, f := range fs {
			if f.Type == terminal.FramePrepare && f.Cut > ongoing.Cut {
				return true
			}
		}
		return false
	})
	_ = epoch.Finalize(context.Background())
}

func TestRevokeRegrantShortWritesAndPostMutationFault(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire := newMemoryTransport()
	pty := newRecordingPTY()
	pty.maxWrite, pty.blockFirst = 3, true
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error {
		return epoch.PTYBytes(append(marker(req), []byte("must-not-release")...))
	}
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
	startLive(t, epoch, wire)
	control := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: "opaque-source-1", Epoch: 9, Mode: terminal.ModeControl}
	observe := control
	observe.Mode = terminal.ModeObserve
	if err := epoch.HandleFrame(control); err != nil {
		t.Fatal(err)
	}
	first := []byte("partly-written-and-revoked")
	if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: "opaque-source-1", Epoch: 9, Data: first}); err != nil {
		t.Fatal(err)
	}
	<-pty.started
	if err := epoch.HandleFrame(observe); err != nil {
		t.Fatal(err)
	}
	if err := epoch.HandleFrame(control); err != nil {
		t.Fatal(err)
	}
	second := []byte("\x1b[A-paste")
	if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: "opaque-source-1", Epoch: 9, Data: second}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !strings.HasSuffix(string(pty.bytes()), string(second)) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := pty.bytes()
	if !strings.HasSuffix(string(got), string(second)) || len(got) >= len(first)+len(second) {
		t.Fatalf("revoke/regrant bytes=%q", got)
	}
	if err := epoch.PTYBytes([]byte("force-cut")); err != nil {
		t.Fatal(err)
	}
	frames := wire.wait(t, func(fs []terminal.Frame) bool {
		for _, f := range fs {
			if f.Type == terminal.FramePrepare && f.Kind == terminal.CutOngoing {
				return true
			}
		}
		return false
	})
	_ = frames
	if err := epoch.Fault(errors.New("browser mutated then failed")); err != nil {
		t.Fatal(err)
	}
	for _, frame := range wire.snapshot() {
		if frame.Type == terminal.FrameLive && string(frame.Data) == "must-not-release" && frame.Cut > 1 {
			t.Fatalf("fault released held bytes: %#v", frame)
		}
	}
	if err := epoch.Start(context.Background(), terminal.CutInitial); !errors.Is(err, terminal.ErrClosed) {
		t.Fatalf("faulted epoch reusable: %v", err)
	}
}

func TestKernelCapacityDetachAndErrorAggregationRetryableCleanup(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	fdWriter, err := terminal.NewNonblockingFDWriter(w)
	if err != nil {
		t.Fatal(err)
	}
	tx := &fakeTransaction{cutCapture: []byte("history\n"), cleanupFailures: 1, cleanupErr: errors.New("cleanup transient")}
	source, probe := sourceFor(t, tx)
	wire := newMemoryTransport()
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error { return epoch.PTYBytes(marker(req)) }
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, fdWriter, config())
	startLive(t, epoch, wire)
	if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: "opaque-source-1", Epoch: 9, Mode: terminal.ModeControl}); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, terminal.MaxInputBytes)
	for i := range data {
		data[i] = 'x'
	}
	if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: "opaque-source-1", Epoch: 9, Data: data}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- epoch.Finalize(context.Background()) }()
	select {
	case firstErr := <-done:
		if firstErr == nil {
			t.Fatal("first cleanup failure was hidden")
		}
	case <-time.After(time.Second):
		t.Fatal("kernel-capacity write did not cancel")
	}
	if err := epoch.Finalize(context.Background()); err != nil {
		t.Fatalf("exact-ID cleanup retry failed: %v", err)
	}
	if _, err := w.Stat(); err != nil {
		t.Fatalf("PTY target was closed: %v", err)
	}
	probe.mu.Lock()
	pane := probe.witness[witness().Pane.PID]
	probe.mu.Unlock()
	if pane != witness().Pane {
		t.Fatal("cleanup mutated source pane")
	}
	actions, _, _, _ := tx.snapshot()
	cleanupCount := 0
	for _, action := range actions {
		if action == terminal.ActionCleanup {
			cleanupCount++
		}
	}
	if cleanupCount != 2 {
		t.Fatalf("cleanup attempts=%d actions=%v", cleanupCount, actions)
	}
}

func TestBlockedEgressInterruptsAndAggregatesWriteInterruptClose(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire := newMemoryTransport()
	wire.block = true
	wire.writeErr = errors.New("write failed")
	wire.closeErr = errors.New("close failed")
	pty := newRecordingPTY()
	pty.closeErr = errors.New("pty close failed")
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error { return epoch.PTYBytes(marker(req)) }
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
	if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	err := epoch.Finalize(context.Background())
	for _, want := range []string{"write failed", "egress drain deadline exceeded", "close failed", "pty close failed"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("final error %q missing %q", err, want)
		}
	}
}
