package terminal_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	terminal "persea-terminal/internal/terminal"
)

func TestPinnedLifecycleRejectsReplacementBeforeExactCut(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, probe := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	epoch, err := terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
	if err != nil {
		t.Fatal(err)
	}
	probe.mu.Lock()
	probe.witness[witness().Pane.PID] = terminal.ProcessWitness{PID: witness().Pane.PID, StartTime: 9999}
	probe.mu.Unlock()
	if err := epoch.Start(context.Background(), terminal.CutInitial); !errors.Is(err, terminal.ErrReopenRequired) {
		t.Fatalf("replacement err=%v", err)
	}
	if err := epoch.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	actions, _, _, _ := tx.snapshot()
	if len(actions) != 0 {
		t.Fatalf("replacement reached side-effecting transaction: %v", actions)
	}
}

func TestLifecycleModelRejectsIllegalAndConflictingTransitions(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error { return epoch.PTYBytes(marker(req)) }
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, 1)); !errors.Is(err, terminal.ErrStale) {
		t.Fatalf("READY before PREPARE err=%v", err)
	}
	if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	prepare := wire.wait(t, func(fs []terminal.Frame) bool { return len(fs) > 0 })[0]
	mode := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: "opaque-source-1", Epoch: 9, Mode: terminal.ModeControl}
	if err := epoch.HandleFrame(mode); !errors.Is(err, terminal.ErrOutOfState) {
		t.Fatalf("mode before LIVE err=%v", err)
	}
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, prepare.Cut+1)); !errors.Is(err, terminal.ErrStale) {
		t.Fatalf("conflicting cut err=%v", err)
	}
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, prepare.Cut)); err != nil {
		t.Fatal(err)
	}
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, prepare.Cut)); !errors.Is(err, terminal.ErrStale) {
		t.Fatalf("duplicate READY err=%v", err)
	}
	deferFrame := browserFrame(terminal.FrameDefer, prepare.Cut)
	deferFrame.Reason = "late"
	if err := epoch.HandleFrame(deferFrame); !errors.Is(err, terminal.ErrStale) {
		t.Fatalf("post-COMMIT DEFER err=%v", err)
	}
	staleInput := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: "replacement", Epoch: 9, Data: []byte("x")}
	if err := epoch.HandleFrame(staleInput); !errors.Is(err, terminal.ErrStale) {
		t.Fatalf("replacement input err=%v", err)
	}
	_ = epoch.Finalize(context.Background())
}

func TestExplicitResizeRequiresCommittedControlAndPublishesOneExactCut(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error { return epoch.PTYBytes(marker(req)) }
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
	if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	initial := wire.wait(t, func(frames []terminal.Frame) bool { return len(frames) > 0 })[0]
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, initial.Cut)); err != nil {
		t.Fatal(err)
	}
	resize := terminal.Frame{
		Version: terminal.ProtocolVersion, Type: terminal.FrameResize, Source: "opaque-source-1",
		Epoch: 9, Columns: 97, Rows: 31,
	}
	if err := epoch.Resize(context.Background(), resize); !errors.Is(err, terminal.ErrOutOfState) {
		t.Fatalf("Observe resize err=%v", err)
	}
	mode := terminal.Frame{
		Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: "opaque-source-1",
		Epoch: 9, Mode: terminal.ModeControl,
	}
	if err := epoch.HandleFrame(mode); err != nil {
		t.Fatal(err)
	}
	stale := resize
	stale.Source = "replacement"
	if err := epoch.Resize(context.Background(), stale); !errors.Is(err, terminal.ErrOutOfState) {
		t.Fatalf("stale-source resize err=%v", err)
	}
	if err := epoch.Resize(context.Background(), resize); err != nil {
		actions, requests, callbackErr, _ := tx.snapshot()
		t.Fatalf("resize err=%v terminal=%v actions=%v requests=%+v callback=%v", err, epoch.Err(), actions, requests, callbackErr)
	}
	frames := wire.wait(t, func(frames []terminal.Frame) bool {
		for _, frame := range frames {
			if frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutResize {
				return true
			}
		}
		return false
	})
	var prepared terminal.Frame
	for _, frame := range frames {
		if frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutResize {
			prepared = frame
		}
	}
	if prepared.Columns != 97 || prepared.Rows != 31 || prepared.Source != resize.Source {
		t.Fatalf("resize PREPARE mismatch: %+v", prepared)
	}
	actions, requests, _, _ := tx.snapshot()
	resizeCalls := 0
	for index, action := range actions {
		if action == terminal.ActionResize {
			resizeCalls++
			if requests[index].Columns != 97 || requests[index].Rows != 31 {
				t.Fatalf("resize transaction mismatch: %+v", requests[index])
			}
		}
	}
	if resizeCalls != 1 {
		t.Fatalf("resize transaction calls=%d actions=%v", resizeCalls, actions)
	}
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, prepared.Cut)); err != nil {
		t.Fatal(err)
	}
	invalid := resize
	invalid.Columns = 1_001
	if err := epoch.Resize(context.Background(), invalid); !errors.Is(err, terminal.ErrMalformed) {
		t.Fatalf("oversized resize err=%v", err)
	}
	after, _, _, _ := tx.snapshot()
	if len(after) != len(actions) {
		t.Fatalf("invalid resize reached transaction: before=%v after=%v", actions, after)
	}
	_ = epoch.Finalize(context.Background())
}

type manualDeadlineClock struct {
	now     time.Time
	created chan *manualDeadlineTimer
}

func newManualDeadlineClock() *manualDeadlineClock {
	return &manualDeadlineClock{now: time.Unix(1, 0), created: make(chan *manualDeadlineTimer, 4)}
}

func (c *manualDeadlineClock) Now() time.Time { return c.now }

func (c *manualDeadlineClock) NewTimer(time.Duration) terminal.Timer {
	timer := &manualDeadlineTimer{ch: make(chan time.Time, 1), active: true}
	c.created <- timer
	return timer
}

type manualDeadlineTimer struct {
	mu     sync.Mutex
	ch     chan time.Time
	active bool
}

func (t *manualDeadlineTimer) C() <-chan time.Time { return t.ch }

func (t *manualDeadlineTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := t.active
	t.active = false
	return wasActive
}

func (t *manualDeadlineTimer) Reset(time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := t.active
	t.active = true
	return wasActive
}

func (t *manualDeadlineTimer) fire(at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.active {
		panic("firing inactive manual deadline timer")
	}
	t.active = false
	t.ch <- at
}

func TestMissingMarkerDeadlineFaultLinearizesTerminalState(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	clock := newManualDeadlineClock()
	cfg := terminal.Config{Clock: clock, QuietInterval: 5 * time.Millisecond, MaximumInterval: 10 * time.Millisecond, CutTimeout: 25 * time.Millisecond}
	epoch, err := terminal.NewEpoch(context.Background(), source, 9, wire, pty, cfg)
	if err != nil {
		t.Fatal(err)
	}
	startDone := make(chan error, 1)
	go func() { startDone <- epoch.Start(context.Background(), terminal.CutInitial) }()
	cutDeadline := <-clock.created
	select {
	case err := <-startDone:
		t.Fatalf("Start returned before marker/PREPARE convergence: %v", err)
	default:
	}
	cutDeadline.fire(clock.Now())
	startErr := <-startDone
	<-epoch.Done()

	terminalErr := epoch.Err()
	if !errors.Is(terminalErr, terminal.ErrClosed) || !errors.Is(terminalErr, terminal.ErrReopenRequired) {
		t.Fatalf("terminal result=%v", terminalErr)
	}
	if startErr != terminalErr {
		t.Fatalf("Start result=%v want immutable terminal result %v", startErr, terminalErr)
	}
	operations := []struct {
		name string
		run  func() error
	}{
		{name: "Start", run: func() error { return epoch.Start(context.Background(), terminal.CutInitial) }},
		{name: "PTYBytes", run: func() error { return epoch.PTYBytes([]byte("late")) }},
		{name: "HandleFrame", run: func() error {
			return epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: "opaque-source-1", Epoch: 9, Mode: terminal.ModeObserve})
		}},
	}
	for _, operation := range operations {
		err := operation.run()
		if !errors.Is(err, terminal.ErrClosed) || !errors.Is(err, terminal.ErrReopenRequired) || errors.Is(err, terminal.ErrOutOfState) {
			t.Fatalf("%s observed contradictory terminal result: %v", operation.name, err)
		}
		if err != terminalErr {
			t.Fatalf("%s terminal result=%v is not immutable Err %v", operation.name, err, terminalErr)
		}
	}
	if err := epoch.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func equalActions(a, b []terminal.PinnedAction) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMalformedStaleObserveAndBoundedBackpressureFailClosed(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire := newThresholdTransport(2)
	pty := newRecordingPTY()
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error { return epoch.PTYBytes(marker(req)) }
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
	if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: "opaque-source-1", Epoch: 9, Data: []byte("x")}); !errors.Is(err, terminal.ErrObserveOnly) && !errors.Is(err, terminal.ErrOutOfState) {
		t.Fatalf("pre-live input err=%v", err)
	}
	if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	prepare := wire.waitFrame(t, terminal.FramePrepare)
	stale := browserFrame(terminal.FrameReady, prepare.Cut)
	stale.Epoch++
	if err := epoch.HandleFrame(stale); !errors.Is(err, terminal.ErrStale) {
		t.Fatalf("stale err=%v", err)
	}
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, prepare.Cut)); err != nil {
		t.Fatal(err)
	}
	if err := epoch.HandleFrame(terminal.Frame{Version: 99, Type: terminal.FrameInput, Source: "opaque-source-1", Epoch: 9, Data: []byte("x")}); !errors.Is(err, terminal.ErrMalformed) {
		t.Fatalf("malformed err=%v", err)
	}
	var saturated error
	for i := 0; i < terminal.MaxEgressFrames+4; i++ {
		if err := epoch.PTYBytes([]byte("bounded")); err != nil {
			saturated = err
			break
		}
	}
	if !errors.Is(saturated, terminal.ErrSaturated) {
		t.Fatalf("bounded egress err=%v", saturated)
	}
	if err := epoch.PTYBytes([]byte("late")); !errors.Is(err, terminal.ErrClosed) {
		t.Fatalf("saturation did not close epoch: %v", err)
	}
}

type thresholdTransport struct {
	mu     sync.Mutex
	frames []terminal.Frame
	allow  int
	writes int
	notify chan struct{}
}

func newThresholdTransport(allow int) *thresholdTransport {
	return &thresholdTransport{allow: allow, notify: make(chan struct{}, 1)}
}

func (w *thresholdTransport) WriteFrame(ctx context.Context, raw []byte) error {
	w.mu.Lock()
	w.writes++
	blocked := w.writes > w.allow
	w.mu.Unlock()
	if blocked {
		<-ctx.Done()
		return terminal.ErrCanceled
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

func (w *thresholdTransport) Close(context.Context) error { return nil }

func (w *thresholdTransport) waitFrame(t *testing.T, kind terminal.FrameType) terminal.Frame {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		w.mu.Lock()
		for _, frame := range w.frames {
			if frame.Type == kind {
				w.mu.Unlock()
				return frame
			}
		}
		w.mu.Unlock()
		select {
		case <-w.notify:
		case <-deadline.C:
			t.Fatal("frame timeout")
		}
	}
}

type zeroProgressPTY struct {
	closed  bool
	started chan struct{}
	once    sync.Once
}

func (w *zeroProgressPTY) WriteContext(context.Context, []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return 0, nil
}
func (w *zeroProgressPTY) Close() error { w.closed = true; return nil }

func TestZeroProgressInputFailsAndFinalizationReportsIt(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire := newMemoryTransport()
	pty := &zeroProgressPTY{started: make(chan struct{})}
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error { return epoch.PTYBytes(marker(req)) }
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
	startLive(t, epoch, wire)
	if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: "opaque-source-1", Epoch: 9, Mode: terminal.ModeControl}); err != nil {
		t.Fatal(err)
	}
	if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: "opaque-source-1", Epoch: 9, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pty.started:
	case <-time.After(time.Second):
		t.Fatal("input writer did not start")
	}
	if err := epoch.Finalize(context.Background()); !errors.Is(err, terminal.ErrNoProgress) {
		t.Fatalf("no-progress error hidden: %v", err)
	}
}

// TestVerticalFitPolicyIsNarrowerThanTheProtocolCeiling pins the explicit
// row-fit policy at the public boundary. The generic frame ceiling says what a
// resize frame may carry; this says what an operator may ask for, and the two
// are deliberately not the same number. A policy that quietly widened to the
// protocol's bound would let one click ask for a 1000x1000 session.
func TestVerticalFitPolicyIsNarrowerThanTheProtocolCeiling(t *testing.T) {
	if terminal.MinFitRows != 8 || terminal.MaxFitRows != 120 || terminal.MaxFitCells != 36000 {
		t.Fatalf("vertical fit policy moved: rows=%d..%d cells=%d", terminal.MinFitRows, terminal.MaxFitRows, terminal.MaxFitCells)
	}
	accepted := []struct{ columns, rows int }{
		{80, terminal.MinFitRows}, {80, 24}, {80, 43}, {80, terminal.MaxFitRows}, {300, terminal.MaxFitRows}, {1, terminal.MinFitRows},
	}
	for _, item := range accepted {
		if !terminal.ValidVerticalFit(item.columns, item.rows) {
			t.Fatalf("in-policy vertical fit %dx%d was refused", item.columns, item.rows)
		}
	}
	refused := []struct {
		name          string
		columns, rows int
	}{
		{"rows below policy", 80, terminal.MinFitRows - 1},
		{"rows above policy", 80, terminal.MaxFitRows + 1},
		{"zero rows", 80, 0},
		{"negative rows", 80, -24},
		{"zero columns", 0, 24},
		{"negative columns", -80, 24},
		{"columns above the protocol ceiling", 1001, 24},
		{"cells above policy", 400, 100},
		{"the protocol ceiling itself", 1000, 1000},
	}
	for _, item := range refused {
		if terminal.ValidVerticalFit(item.columns, item.rows) {
			t.Fatalf("out-of-policy vertical fit %s (%dx%d) was accepted", item.name, item.columns, item.rows)
		}
	}
	// The refused pair at the protocol ceiling must still be a structurally valid
	// frame: the narrowing is product policy, not a protocol change.
	frame := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameResize, Source: "s", Epoch: 1, Columns: 1000, Rows: 1000}
	if err := frame.Validate(); err != nil {
		t.Fatalf("the protocol ceiling stopped validating: %v", err)
	}
}
