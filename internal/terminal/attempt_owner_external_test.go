package terminal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/terminal"
)

func assertOpen(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s closed prematurely", label)
	default:
	}
}

func assertNoResult(t *testing.T, ch <-chan error, label string) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("%s returned prematurely: %v", label, err)
	default:
	}
}

func countFrames(frames []terminal.Frame, kind terminal.FrameType) int {
	count := 0
	for _, frame := range frames {
		if frame.Type == kind {
			count++
		}
	}
	return count
}

func TestAttemptOwnerStartWaitsForPrepareConvergence(t *testing.T) {
	tx := &fakeTransaction{
		bindResult: bindOwnerResult(), cutCapture: []byte("history\n"),
		cutStarted: make(chan struct{}),
	}
	epoch, wire := newBindIngressEpoch(t, tx, nil)
	startDone := make(chan error, 1)
	go func() { startDone <- epoch.Start(context.Background(), terminal.CutInitial) }()
	<-tx.cutStarted
	assertNoResult(t, startDone, "Start before PREPARE")
	assertOpen(t, epoch.Done(), "Done before PREPARE")
	if got := wire.snapshot(); len(got) != 0 {
		t.Fatalf("frame published before marker convergence: %#v", got)
	}
	_, requests, _, _ := tx.snapshot()
	var cut terminal.TransactionRequest
	for _, request := range requests {
		if request.Action == terminal.ActionCut {
			cut = request
		}
	}
	if cut.Cut == 0 {
		t.Fatal("missing CUT request")
	}
	if err := epoch.PTYBytes(marker(cut)); err != nil {
		t.Fatal(err)
	}
	if err := <-startDone; err != nil {
		t.Fatalf("Start after PREPARE convergence: %v", err)
	}
	frames := wire.wait(t, func(frames []terminal.Frame) bool {
		return countFrames(frames, terminal.FramePrepare) == 1
	})
	if countFrames(frames, terminal.FramePrepare) != 1 {
		t.Fatalf("PREPARE count=%d", countFrames(frames, terminal.FramePrepare))
	}
	if err := epoch.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAttemptOwnerCompatibleWaiterCancellationWhileAwaitingPrepareIsLocal(t *testing.T) {
	tx := &fakeTransaction{bindResult: bindOwnerResult(), cutStarted: make(chan struct{})}
	epoch, _ := newBindIngressEpoch(t, tx, nil)
	leaderDone := make(chan error, 1)
	go func() { leaderDone <- epoch.Start(context.Background(), terminal.CutInitial) }()
	<-tx.cutStarted
	waiterBase, cancelWaiter := context.WithCancel(context.Background())
	waiterCtx := &observedContext{Context: waiterBase, entered: make(chan struct{})}
	waiterDone := make(chan error, 1)
	go func() { waiterDone <- epoch.Start(waiterCtx, terminal.CutInitial) }()
	<-waiterCtx.entered
	cancelWaiter()
	if err := <-waiterDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter cancellation=%v", err)
	}
	assertNoResult(t, leaderDone, "owner Start after waiter cancellation")
	assertOpen(t, epoch.Done(), "Done after waiter cancellation")
	_, requests, _, _ := tx.snapshot()
	var cut terminal.TransactionRequest
	for _, request := range requests {
		if request.Action == terminal.ActionCut {
			cut = request
		}
	}
	if err := epoch.PTYBytes(marker(cut)); err != nil {
		t.Fatal(err)
	}
	if err := <-leaderDone; err != nil {
		t.Fatal(err)
	}
	if err := epoch.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAttemptOwnerA4PendingBindRacersPublishOnceAfterSettlement(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	owner, cancelOwner := context.WithCancel(context.Background())
	tx := &fakeTransaction{
		bindResult: bindOwnerResult(), bindStarted: make(chan struct{}), bindRelease: make(chan struct{}),
	}
	epoch, wire := newBindIngressEpoch(t, tx, parent)
	leaderDone := make(chan error, 1)
	waiterDone := make(chan error, 1)
	go func() { leaderDone <- epoch.Start(owner, terminal.CutInitial) }()
	<-tx.bindStarted
	go func() { waiterDone <- epoch.Start(context.Background(), terminal.CutInitial) }()

	barrier := make(chan struct{})
	ack := make(chan struct{}, 4)
	go func() { <-barrier; cancelOwner(); ack <- struct{}{} }()
	go func() { <-barrier; cancelParent(); ack <- struct{}{} }()
	go func() {
		<-barrier
		_ = epoch.Fault(errors.Join(errors.New("fault racer"), terminal.ErrOutOfState))
		ack <- struct{}{}
	}()
	go func() {
		<-barrier
		reserved := []byte("\x1b]52;;persea:v1:" + string(bytes.Repeat([]byte{'A'}, 43)) + "\a")
		_ = epoch.PTYBytes(reserved)
		ack <- struct{}{}
	}()
	finalizeDone := make(chan error, 1)
	go func() { <-barrier; finalizeDone <- epoch.Finalize(context.Background()) }()
	close(barrier)
	for i := 0; i < cap(ack); i++ {
		<-ack
	}
	assertOpen(t, epoch.Done(), "Done with ownership pending")
	if err := epoch.Err(); err != nil {
		t.Fatalf("Err with ownership pending=%v", err)
	}
	assertNoResult(t, leaderDone, "owner Start with ownership pending")
	assertNoResult(t, waiterDone, "compatible waiter with ownership pending")
	assertNoResult(t, finalizeDone, "Finalize with ownership pending")
	actions, _, _, _ := tx.snapshot()
	if len(actions) != 1 || actions[0] != terminal.ActionBind {
		t.Fatalf("actions before ownership settlement=%v", actions)
	}

	close(tx.bindRelease)
	leaderErr, waiterErr := <-leaderDone, <-waiterDone
	<-epoch.Done()
	terminalErr := epoch.Err()
	if leaderErr != terminalErr || waiterErr != terminalErr {
		t.Fatalf("terminal identity leader=%v waiter=%v Err=%v", leaderErr, waiterErr, terminalErr)
	}
	if err := <-finalizeDone; err != nil {
		t.Fatal(err)
	}
	if err := epoch.Start(context.Background(), terminal.CutReconnect); err != terminalErr {
		t.Fatalf("post-terminal Start=%v want %v", err, terminalErr)
	}
	actions, _, _, _ = tx.snapshot()
	if len(actions) != 2 || actions[0] != terminal.ActionBind || actions[1] != terminal.ActionCleanup {
		t.Fatalf("final actions=%v", actions)
	}
	if count := countFrames(wire.snapshot(), terminal.FrameEnd); count != 1 {
		t.Fatalf("END count=%d", count)
	}
}

func TestAttemptOwnerA4ExactTransferPublishesBeforeSourceReturnAndCleansAfterGate(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	probe := newScriptedProbe()
	probe.blockAt = 3
	probe.blocked = make(chan struct{})
	probe.release = make(chan struct{})
	tx := &fakeTransaction{bindResult: bindOwnerResult()}
	epoch := newBindOwnerEpoch(t, tx, probe, parent)
	startDone := make(chan error, 1)
	go func() { startDone <- epoch.Start(context.Background(), terminal.CutInitial) }()
	<-probe.blocked
	if err := epoch.Fault(errors.Join(errors.New("exact-transfer racer"), terminal.ErrStale)); err != nil {
		t.Fatal(err)
	}
	cancelParent()
	<-epoch.Done()
	terminalErr := epoch.Err()
	assertNoResult(t, startDone, "Start before source return")
	if err := epoch.Start(context.Background(), terminal.CutReconnect); err != terminalErr {
		t.Fatalf("post-terminal Start=%v want %v", err, terminalErr)
	}
	finalizeDone := make(chan error, 1)
	go func() { finalizeDone <- epoch.Finalize(context.Background()) }()
	assertNoResult(t, finalizeDone, "Finalize before WorkGate quiescence")
	actions, _, _, _ := tx.snapshot()
	if len(actions) != 1 || actions[0] != terminal.ActionBind {
		t.Fatalf("cleanup ran before WorkGate quiescence: %v", actions)
	}
	close(probe.release)
	if err := <-startDone; err != terminalErr {
		t.Fatalf("Start=%v want immutable %v", err, terminalErr)
	}
	if err := <-finalizeDone; err != nil {
		t.Fatal(err)
	}
	assertOneExactCleanup(t, tx)
}

func TestAttemptOwnerAdmittedSourceErrorSmuggling(t *testing.T) {
	sentinels := []error{
		terminal.ErrOutOfState,
		terminal.ErrStale,
		terminal.ErrClosed,
		terminal.ErrReopenRequired,
		terminal.ErrSaturated,
	}
	for _, boundary := range []string{"BIND", "CUT"} {
		for _, sentinel := range sentinels {
			t.Run(boundary+"/"+sentinel.Error(), func(t *testing.T) {
				smuggled := errors.Join(errors.New("opaque diagnostic"), fmt.Errorf("wrapped: %w", sentinel))
				tx := &fakeTransaction{bindResult: bindOwnerResult()}
				if boundary == "BIND" {
					tx.bindErr = smuggled
				} else {
					tx.cutErr = smuggled
				}
				epoch, _ := newBindIngressEpoch(t, tx, nil)
				err := epoch.Start(context.Background(), terminal.CutInitial)
				if !errors.Is(err, terminal.ErrClosed) || !errors.Is(err, sentinel) {
					t.Fatalf("smuggled %s error=%v", boundary, err)
				}
				if err != epoch.Err() {
					t.Fatalf("smuggled %s result is not immutable Err", boundary)
				}
				if err := epoch.Finalize(context.Background()); err != nil {
					t.Fatal(err)
				}
				assertOneExactCleanup(t, tx)
			})
		}
	}
}

func TestAttemptOwnerCaptureAndPrepareFailuresAreTerminal(t *testing.T) {
	captures := []struct {
		name string
		data []byte
	}{
		{name: "NUL", data: []byte{'x', 0, '\n'}},
		{name: "invalid-UTF8", data: []byte{0xff, '\n'}},
		{name: "row-cap", data: append(bytes.Repeat([]byte{'x'}, terminal.HistoryRowByteCap+1), '\n')},
	}
	for _, capture := range captures {
		t.Run("capture/"+capture.name, func(t *testing.T) {
			tx := &fakeTransaction{bindResult: bindOwnerResult(), cutCapture: capture.data}
			var epoch *terminal.Epoch
			tx.onCut = func(request terminal.TransactionRequest) error { return epoch.PTYBytes(marker(request)) }
			epoch, _ = newBindIngressEpoch(t, tx, nil)
			if err := epoch.Start(context.Background(), terminal.CutInitial); !errors.Is(err, terminal.ErrClosed) {
				t.Fatalf("capture failure=%v", err)
			}
			if err := epoch.Finalize(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertOneExactCleanup(t, tx)
		})
	}

	t.Run("PREPARE-worst-case-capacity", func(t *testing.T) {
		row := append(bytes.Repeat([]byte{1}, terminal.HistoryRowByteCap-1), '\n')
		tx := &fakeTransaction{bindResult: bindOwnerResult(), cutCapture: bytes.Repeat(row, terminal.HistoryByteCap/len(row))}
		var epoch *terminal.Epoch
		tx.onBind = func(terminal.TransactionRequest) error {
			return epoch.PTYBytes(bytes.Repeat([]byte{'r'}, terminal.ReplayByteCap))
		}
		tx.onCut = func(request terminal.TransactionRequest) error { return epoch.PTYBytes(marker(request)) }
		source, err := terminal.NewPinnedSource(witness(), tx, newScriptedProbe())
		if err != nil {
			t.Fatal(err)
		}
		wire := &capacityTransport{frames: make(chan []byte, 2)}
		capacityConfig := config()
		// Race instrumentation makes decoding the maximum valid frame several
		// times slower; this case proves capacity, not the one-second drain bound.
		capacityConfig.CutTimeout = 10 * time.Second
		epoch, err = terminal.NewEpoch(context.Background(), source, 9, wire, newRecordingPTY(), capacityConfig)
		if err != nil {
			t.Fatal(err)
		}
		if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
			t.Fatalf("worst-case valid PREPARE was rejected: %v", err)
		}
		// Observe transport delivery before decoding the maximum escaped frame.
		// Decoding inside memoryTransport incorrectly puts race-instrumented
		// JSON processing under its unrelated two-second delivery deadline.
		var raw []byte
		select {
		case raw = <-wire.frames:
		case <-time.After(2 * time.Second):
			t.Fatal("worst-case PREPARE was not delivered")
		}
		prepared, err := terminal.DecodeFrame(raw)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.Type != terminal.FramePrepare || len(prepared.History) != terminal.HistoryByteCap/len(row) || len(prepared.Replay) != terminal.ReplayByteCap {
			t.Fatalf("worst-case PREPARE type=%s history rows=%d replay bytes=%d", prepared.Type, len(prepared.History), len(prepared.Replay))
		}
		if err := epoch.Finalize(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(wire.frames) != 1 {
			t.Fatalf("trailing frame count=%d, want one END", len(wire.frames))
		}
		ended, err := terminal.DecodeFrame(<-wire.frames)
		if err != nil || ended.Type != terminal.FrameEnd {
			t.Fatalf("trailing frame type=%s err=%v, want END", ended.Type, err)
		}
		assertOneExactCleanup(t, tx)
	})
}

type capacityTransport struct {
	frames chan []byte
}

func (w *capacityTransport) WriteFrame(ctx context.Context, raw []byte) error {
	// Egress clears its buffer after this call; the receiver owns a copy.
	select {
	case w.frames <- append([]byte(nil), raw...):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *capacityTransport) Close(context.Context) error { return nil }

type smugglingTransport struct {
	err       error
	attempted chan struct{}
	once      sync.Once
}

func (w *smugglingTransport) WriteFrame(context.Context, []byte) error {
	w.once.Do(func() { close(w.attempted) })
	return w.err
}

func (w *smugglingTransport) Close(context.Context) error { return nil }

type smugglingPTY struct {
	err     error
	started chan struct{}
	once    sync.Once
}

func (w *smugglingPTY) WriteContext(context.Context, []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return 0, w.err
}

func (w *smugglingPTY) Close() error { return nil }

func TestAttemptOwnerAdmittedInputAndEgressErrorSmuggling(t *testing.T) {
	sentinel := terminal.ErrOutOfState
	smuggled := errors.Join(errors.New("async boundary"), fmt.Errorf("wrapped: %w", sentinel))

	t.Run("egress", func(t *testing.T) {
		tx := &fakeTransaction{bindResult: bindOwnerResult()}
		var epoch *terminal.Epoch
		tx.onCut = func(request terminal.TransactionRequest) error { return epoch.PTYBytes(marker(request)) }
		source, _ := sourceFor(t, tx)
		writer := &smugglingTransport{err: smuggled, attempted: make(chan struct{})}
		var err error
		epoch, err = terminal.NewEpoch(context.Background(), source, 31, writer, newRecordingPTY(), config())
		if err != nil {
			t.Fatal(err)
		}
		if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
			t.Fatalf("PREPARE enqueue was not successful: %v", err)
		}
		<-writer.attempted
		<-epoch.Done()
		if err := epoch.Err(); !errors.Is(err, sentinel) || !errors.Is(err, terminal.ErrClosed) {
			t.Fatalf("egress smuggling=%v", err)
		}
		if err := epoch.Finalize(context.Background()); !errors.Is(err, sentinel) {
			t.Fatalf("egress finalization=%v", err)
		}
	})

	t.Run("input", func(t *testing.T) {
		tx := &fakeTransaction{bindResult: bindOwnerResult()}
		var epoch *terminal.Epoch
		tx.onCut = func(request terminal.TransactionRequest) error { return epoch.PTYBytes(marker(request)) }
		source, _ := sourceFor(t, tx)
		wire := newMemoryTransport()
		pty := &smugglingPTY{err: smuggled, started: make(chan struct{})}
		var err error
		epoch, err = terminal.NewEpoch(context.Background(), source, 32, wire, pty, config())
		if err != nil {
			t.Fatal(err)
		}
		if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
			t.Fatal(err)
		}
		prepare := onePrepare(t, wire)
		if err := epoch.HandleFrame(terminal.Frame{
			Version: terminal.ProtocolVersion, Type: terminal.FrameReady,
			Source: prepare.Source, Epoch: prepare.Epoch, Cut: prepare.Cut,
		}); err != nil {
			t.Fatal(err)
		}
		if err := epoch.HandleFrame(terminal.Frame{
			Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest,
			Source: prepare.Source, Epoch: prepare.Epoch, Mode: terminal.ModeControl,
		}); err != nil {
			t.Fatal(err)
		}
		if err := epoch.HandleFrame(terminal.Frame{
			Version: terminal.ProtocolVersion, Type: terminal.FrameInput,
			Source: prepare.Source, Epoch: prepare.Epoch, Data: []byte("x"),
		}); err != nil {
			t.Fatal(err)
		}
		<-pty.started
		<-epoch.Done()
		if err := epoch.Err(); !errors.Is(err, sentinel) || !errors.Is(err, terminal.ErrClosed) {
			t.Fatalf("input smuggling=%v", err)
		}
		if err := epoch.Finalize(context.Background()); !errors.Is(err, sentinel) {
			t.Fatalf("input finalization=%v", err)
		}
	})
}
