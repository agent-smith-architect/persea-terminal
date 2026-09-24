package terminal_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	terminal "persea-terminal/internal/terminal"
)

func TestFinalOwnerOngoingSplitDeferIsFIFOAndLossless(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	clock := newManualDeadlineClock()
	cfg := terminal.Config{Clock: clock, QuietInterval: 5 * time.Millisecond, MaximumInterval: 10 * time.Millisecond, CutTimeout: 25 * time.Millisecond}
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error {
		if req.Kind == terminal.CutOngoing {
			return epoch.PTYBytes(append(append([]byte("delta"), marker(req)...), []byte("held")...))
		}
		return epoch.PTYBytes(append([]byte("redraw"), marker(req)...))
	}
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, cfg)
	if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	initialTimer := <-clock.created
	initial := wire.wait(t, func(fs []terminal.Frame) bool { return len(fs) == 1 })[0]
	if string(initial.Replay) != "redraw" {
		t.Fatalf("initial replay=%q", initial.Replay)
	}
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, initial.Cut)); err != nil {
		t.Fatal(err)
	}
	_ = initialTimer
	if err := epoch.PTYBytes([]byte("trigger")); err != nil {
		t.Fatal(err)
	}
	schedulerTimer := <-clock.created
	schedulerTimer.fire(clock.Now())
	cutTimer := <-clock.created
	_ = cutTimer
	frames := wire.wait(t, func(fs []terminal.Frame) bool {
		for _, f := range fs {
			if f.Type == terminal.FramePrepare && f.Kind == terminal.CutOngoing {
				return true
			}
		}
		return false
	})
	var ongoing terminal.Frame
	for _, f := range frames {
		if f.Type == terminal.FramePrepare && f.Kind == terminal.CutOngoing {
			ongoing = f
		}
	}
	if len(ongoing.Replay) != 0 {
		t.Fatalf("ongoing replay leaked pre-marker LIVE: %q", ongoing.Replay)
	}
	deferFrame := browserFrame(terminal.FrameDefer, ongoing.Cut)
	deferFrame.Reason = "unchanged"
	if err := epoch.HandleFrame(deferFrame); err != nil {
		t.Fatal(err)
	}
	data := []string{}
	for _, f := range wire.wait(t, func(fs []terminal.Frame) bool {
		for _, f := range fs {
			if f.Type == terminal.FrameLive && string(f.Data) == "held" {
				return true
			}
		}
		return false
	}) {
		if f.Type == terminal.FrameLive && (string(f.Data) == "delta" || string(f.Data) == "held") {
			data = append(data, string(f.Data))
		}
		if f.Type == terminal.FrameCommit && f.Cut == ongoing.Cut {
			t.Fatalf("DEFER emitted COMMIT: %#v", f)
		}
	}
	if !reflect.DeepEqual(data, []string{"delta", "held"}) {
		t.Fatalf("ongoing FIFO data=%v", data)
	}
	schedulerTimer.fire(clock.Now())
	<-clock.created
	if retry := waitLatestPrepare(t, wire, ongoing.Cut); retry.Cut <= ongoing.Cut {
		t.Fatal("ONGOING DEFER did not schedule retry")
	}
	_ = epoch.Finalize(context.Background())
}

func TestFinalOwnerControlInputCrossingOngoingPrepareWaitsForCommit(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	clock := newManualDeadlineClock()
	cfg := terminal.Config{Clock: clock, QuietInterval: 5 * time.Millisecond, MaximumInterval: 10 * time.Millisecond, CutTimeout: 25 * time.Millisecond}
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error {
		return epoch.PTYBytes(marker(req))
	}
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, cfg)
	if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	<-clock.created
	initial := wire.wait(t, func(fs []terminal.Frame) bool { return len(fs) == 1 })[0]
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, initial.Cut)); err != nil {
		t.Fatal(err)
	}
	if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: initial.Source, Epoch: initial.Epoch, Mode: terminal.ModeControl}); err != nil {
		t.Fatal(err)
	}
	if err := epoch.PTYBytes([]byte("trigger")); err != nil {
		t.Fatal(err)
	}
	schedulerTimer := <-clock.created
	schedulerTimer.fire(clock.Now())
	<-clock.created
	frames := wire.wait(t, func(fs []terminal.Frame) bool {
		for _, frame := range fs {
			if frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutOngoing {
				return true
			}
		}
		return false
	})
	var ongoing terminal.Frame
	for _, frame := range frames {
		if frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutOngoing {
			ongoing = frame
		}
	}
	want := []byte("typed-before-prepare-arrived")
	if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: ongoing.Source, Epoch: ongoing.Epoch, Data: want}); err != nil {
		t.Fatalf("authorized input crossing PREPARE was rejected: %v", err)
	}
	if got := pty.bytes(); len(got) != 0 {
		t.Fatalf("transitional input reached PTY before COMMIT: %q", got)
	}
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, ongoing.Cut)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !reflect.DeepEqual(pty.bytes(), want) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := pty.bytes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("transitional input after COMMIT=%q want=%q", got, want)
	}
	_ = epoch.Finalize(context.Background())
}

func ongoingControlFixture(t *testing.T, control bool) (*terminal.Epoch, *memoryTransport, *recordingPTY, terminal.Frame) {
	t.Helper()
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	clock := newManualDeadlineClock()
	cfg := terminal.Config{Clock: clock, QuietInterval: 5 * time.Millisecond, MaximumInterval: 10 * time.Millisecond, CutTimeout: 25 * time.Millisecond}
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error { return epoch.PTYBytes(marker(req)) }
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, cfg)
	if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	<-clock.created
	initial := wire.wait(t, func(fs []terminal.Frame) bool { return len(fs) == 1 })[0]
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, initial.Cut)); err != nil {
		t.Fatal(err)
	}
	if control {
		if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: initial.Source, Epoch: initial.Epoch, Mode: terminal.ModeControl}); err != nil {
			t.Fatal(err)
		}
	}
	if err := epoch.PTYBytes([]byte("trigger")); err != nil {
		t.Fatal(err)
	}
	schedulerTimer := <-clock.created
	schedulerTimer.fire(clock.Now())
	<-clock.created
	frames := wire.wait(t, func(fs []terminal.Frame) bool {
		for _, frame := range fs {
			if frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutOngoing {
				return true
			}
		}
		return false
	})
	for _, frame := range frames {
		if frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutOngoing {
			return epoch, wire, pty, frame
		}
	}
	t.Fatal("ONGOING PREPARE missing")
	return nil, nil, nil, terminal.Frame{}
}

func waitPTYBytes(t *testing.T, pty *recordingPTY, want []byte) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !reflect.DeepEqual(pty.bytes(), want) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := pty.bytes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PTY bytes=%q want=%q", got, want)
	}
}

func TestFinalOwnerTransitionalInputFIFODeferCapsAndTeardown(t *testing.T) {
	t.Run("defer-fifo", func(t *testing.T) {
		epoch, _, pty, ongoing := ongoingControlFixture(t, true)
		for _, data := range [][]byte{[]byte("one"), []byte("-two"), []byte("-three")} {
			if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: ongoing.Source, Epoch: ongoing.Epoch, Data: data}); err != nil {
				t.Fatal(err)
			}
		}
		if got := pty.bytes(); len(got) != 0 {
			t.Fatalf("input escaped before DEFER: %q", got)
		}
		deferFrame := browserFrame(terminal.FrameDefer, ongoing.Cut)
		deferFrame.Reason = "unchanged"
		if err := epoch.HandleFrame(deferFrame); err != nil {
			t.Fatal(err)
		}
		waitPTYBytes(t, pty, []byte("one-two-three"))
		_ = epoch.Finalize(context.Background())
	})

	t.Run("exact-byte-cap", func(t *testing.T) {
		epoch, _, pty, ongoing := ongoingControlFixture(t, true)
		want := make([]byte, terminal.MaxInputBytes)
		for i := range want {
			want[i] = byte(i)
		}
		if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: ongoing.Source, Epoch: ongoing.Epoch, Data: want}); err != nil {
			t.Fatal(err)
		}
		if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, ongoing.Cut)); err != nil {
			t.Fatal(err)
		}
		waitPTYBytes(t, pty, want)
		_ = epoch.Finalize(context.Background())
	})

	t.Run("byte-cap-minus-one", func(t *testing.T) {
		epoch, _, pty, ongoing := ongoingControlFixture(t, true)
		want := make([]byte, terminal.MaxInputBytes-1)
		for i := range want {
			want[i] = byte(i)
		}
		if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: ongoing.Source, Epoch: ongoing.Epoch, Data: want}); err != nil {
			t.Fatal(err)
		}
		if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, ongoing.Cut)); err != nil {
			t.Fatal(err)
		}
		waitPTYBytes(t, pty, want)
		_ = epoch.Finalize(context.Background())
	})

	for _, count := range []int{terminal.MaxInputFrames - 1, terminal.MaxInputFrames} {
		t.Run(fmt.Sprintf("frame-cap-%d", count), func(t *testing.T) {
			epoch, _, pty, ongoing := ongoingControlFixture(t, true)
			want := make([]byte, count)
			for i := range want {
				want[i] = byte(i)
				if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: ongoing.Source, Epoch: ongoing.Epoch, Data: []byte{want[i]}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, ongoing.Cut)); err != nil {
				t.Fatal(err)
			}
			waitPTYBytes(t, pty, want)
			_ = epoch.Finalize(context.Background())
		})
	}

	for _, tc := range []struct {
		name   string
		frames [][]byte
	}{
		{name: "byte-cap-plus-one", frames: [][]byte{make([]byte, terminal.MaxInputBytes), []byte("x")}},
		{name: "frame-cap-plus-one", frames: func() [][]byte {
			frames := make([][]byte, terminal.MaxInputFrames+1)
			for i := range frames {
				frames[i] = []byte{byte(i)}
			}
			return frames
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			epoch, _, pty, ongoing := ongoingControlFixture(t, true)
			var overflow error
			for _, data := range tc.frames {
				overflow = epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: ongoing.Source, Epoch: ongoing.Epoch, Data: data})
				if overflow != nil {
					break
				}
			}
			if !errors.Is(overflow, terminal.ErrSaturated) || !errors.Is(overflow, terminal.ErrClosed) {
				t.Fatalf("overflow=%v", overflow)
			}
			<-epoch.Done()
			if got := pty.bytes(); len(got) != 0 {
				t.Fatalf("overflow partially delivered input: %d bytes", len(got))
			}
			_ = epoch.Finalize(context.Background())
		})
	}

	t.Run("teardown-discards", func(t *testing.T) {
		epoch, _, pty, ongoing := ongoingControlFixture(t, true)
		if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: ongoing.Source, Epoch: ongoing.Epoch, Data: []byte("never")}); err != nil {
			t.Fatal(err)
		}
		_ = epoch.Finalize(context.Background())
		if got := pty.bytes(); len(got) != 0 {
			t.Fatalf("teardown delivered transitional input: %q", got)
		}
	})
}

func TestFinalOwnerModeRequestCrossingOngoingPrepareIsNonTerminal(t *testing.T) {
	epoch, wire, pty, ongoing := ongoingControlFixture(t, true)
	if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: ongoing.Source, Epoch: ongoing.Epoch, Data: []byte("owned")}); err != nil {
		t.Fatal(err)
	}
	if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: ongoing.Source, Epoch: ongoing.Epoch, Mode: terminal.ModeObserve}); err != nil {
		t.Fatalf("crossing MODE_REQUEST terminated attachment: %v", err)
	}
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, ongoing.Cut)); err != nil {
		t.Fatal(err)
	}
	waitPTYBytes(t, pty, []byte("owned"))
	frames := wire.wait(t, func(fs []terminal.Frame) bool {
		for _, frame := range fs {
			if frame.Type == terminal.FrameMode && frame.Reason == "retry_after_cut" {
				return true
			}
		}
		return false
	})
	for _, frame := range frames {
		if frame.Type == terminal.FrameMode && frame.Reason == "retry_after_cut" && frame.Mode != terminal.ModeControl {
			t.Fatalf("retry response changed authority: %#v", frame)
		}
	}
	select {
	case <-epoch.Done():
		t.Fatalf("crossing MODE_REQUEST terminalized epoch: %v", epoch.Err())
	default:
	}
	_ = epoch.Finalize(context.Background())
}

func TestFinalOwnerTransitionalInputRejectsObserveAndStaleTupleWithoutPoisoningCut(t *testing.T) {
	t.Run("observe", func(t *testing.T) {
		epoch, _, pty, ongoing := ongoingControlFixture(t, false)
		err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: ongoing.Source, Epoch: ongoing.Epoch, Data: []byte("denied")})
		if !errors.Is(err, terminal.ErrObserveOnly) {
			t.Fatalf("Observe transitional input=%v", err)
		}
		if got := pty.bytes(); len(got) != 0 {
			t.Fatalf("Observe transitional input reached PTY: %q", got)
		}
		select {
		case <-epoch.Done():
			t.Fatalf("Observe request poisoned cut: %v", epoch.Err())
		default:
		}
		_ = epoch.Finalize(context.Background())
	})

	t.Run("stale-source-and-epoch", func(t *testing.T) {
		epoch, _, pty, ongoing := ongoingControlFixture(t, true)
		for _, frame := range []terminal.Frame{
			{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: "replacement", Epoch: ongoing.Epoch, Data: []byte("wrong-source")},
			{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: ongoing.Source, Epoch: ongoing.Epoch + 1, Data: []byte("wrong-epoch")},
		} {
			if err := epoch.HandleFrame(frame); !errors.Is(err, terminal.ErrStale) {
				t.Fatalf("stale tuple=%v", err)
			}
		}
		want := []byte("right")
		if err := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: ongoing.Source, Epoch: ongoing.Epoch, Data: want}); err != nil {
			t.Fatal(err)
		}
		if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, ongoing.Cut)); err != nil {
			t.Fatal(err)
		}
		waitPTYBytes(t, pty, want)
		_ = epoch.Finalize(context.Background())
	})
}

func TestFinalOwnerMarkerTrafficConvergesAndPostMarkerStaysDirty(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	clock := newManualDeadlineClock()
	cfg := terminal.Config{Clock: clock, QuietInterval: 5 * time.Millisecond, MaximumInterval: 10 * time.Millisecond, CutTimeout: 25 * time.Millisecond}
	var epoch *terminal.Epoch
	postMarker := false
	tx.onCut = func(req terminal.TransactionRequest) error {
		bytes := marker(req)
		if req.Kind == terminal.CutOngoing && postMarker {
			bytes = append(bytes, []byte("genuine")...)
		}
		return epoch.PTYBytes(bytes)
	}
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, cfg)
	if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	<-clock.created
	initial := wire.wait(t, func(fs []terminal.Frame) bool { return len(fs) == 1 })[0]
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, initial.Cut)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-clock.created:
		t.Fatal("marker-only initial cut armed an unsolicited successor")
	default:
	}

	if err := epoch.PTYBytes([]byte("activity-1")); err != nil {
		t.Fatal(err)
	}
	timer := <-clock.created
	timer.fire(clock.Now())
	<-clock.created
	ongoing := waitLatestPrepare(t, wire, initial.Cut)
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, ongoing.Cut)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-clock.created:
		t.Fatal("marker-only ongoing cut armed an unsolicited successor")
	default:
	}

	postMarker = true
	if err := epoch.PTYBytes([]byte("activity-2")); err != nil {
		t.Fatal(err)
	}
	timer.fire(clock.Now())
	<-clock.created
	ongoing = waitLatestPrepare(t, wire, ongoing.Cut)
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, ongoing.Cut)); err != nil {
		t.Fatal(err)
	}
	timer.fire(clock.Now())
	<-clock.created
	if successor := waitLatestPrepare(t, wire, ongoing.Cut); successor.Cut <= ongoing.Cut {
		t.Fatal("genuine post-marker output was lost")
	}
	_ = epoch.Finalize(context.Background())
}

func waitLatestPrepare(t *testing.T, wire *memoryTransport, after uint64) terminal.Frame {
	t.Helper()
	frames := wire.wait(t, func(fs []terminal.Frame) bool {
		for _, f := range fs {
			if f.Type == terminal.FramePrepare && f.Cut > after {
				return true
			}
		}
		return false
	})
	var found terminal.Frame
	for _, f := range frames {
		if f.Type == terminal.FramePrepare && f.Cut > found.Cut {
			found = f
		}
	}
	return found
}

func (t *manualDeadlineTimer) fireStale(at time.Time) {
	t.mu.Lock()
	t.ch <- at
	t.mu.Unlock()
}

func TestFinalOwnerReadyAndDeadlineHaveOneWinner(t *testing.T) {
	newPrepared := func(t *testing.T) (*terminal.Epoch, *memoryTransport, *manualDeadlineClock, *manualDeadlineTimer) {
		t.Helper()
		tx := &fakeTransaction{cutCapture: []byte("history\n")}
		source, _ := sourceFor(t, tx)
		wire, pty := newMemoryTransport(), newRecordingPTY()
		clock := newManualDeadlineClock()
		cfg := terminal.Config{Clock: clock, QuietInterval: 5 * time.Millisecond, MaximumInterval: 10 * time.Millisecond, CutTimeout: 25 * time.Millisecond}
		var epoch *terminal.Epoch
		tx.onCut = func(req terminal.TransactionRequest) error { return epoch.PTYBytes(marker(req)) }
		epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, cfg)
		if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
			t.Fatal(err)
		}
		cutTimer := <-clock.created
		wire.wait(t, func(fs []terminal.Frame) bool { return len(fs) == 1 })
		return epoch, wire, clock, cutTimer
	}

	t.Run("ready-wins", func(t *testing.T) {
		epoch, wire, clock, cutTimer := newPrepared(t)
		prepare := wire.snapshot()[0]
		if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, prepare.Cut)); err != nil {
			t.Fatal(err)
		}
		cutTimer.fireStale(clock.Now())
		select {
		case <-epoch.Done():
			t.Fatalf("stale deadline changed committed state: %v", epoch.Err())
		default:
		}
		_ = epoch.Finalize(context.Background())
	})

	t.Run("deadline-wins", func(t *testing.T) {
		epoch, wire, clock, cutTimer := newPrepared(t)
		prepare := wire.snapshot()[0]
		cutTimer.fire(clock.Now())
		<-epoch.Done()
		err := epoch.HandleFrame(browserFrame(terminal.FrameReady, prepare.Cut))
		if !errors.Is(err, terminal.ErrClosed) || !errors.Is(err, terminal.ErrReopenRequired) {
			t.Fatalf("READY changed deadline terminal state: %v", err)
		}
		_ = epoch.Finalize(context.Background())
	})
}

func TestFinalOwnerInitialAndReconnectInputUnavailableAndDeferFailClosed(t *testing.T) {
	for _, kind := range []terminal.CutKind{terminal.CutInitial, terminal.CutReconnect} {
		t.Run(string(kind), func(t *testing.T) {
			tx := &fakeTransaction{cutCapture: []byte("history\n")}
			source, _ := sourceFor(t, tx)
			wire, pty := newMemoryTransport(), newRecordingPTY()
			var epoch *terminal.Epoch
			tx.onCut = func(req terminal.TransactionRequest) error {
				return epoch.PTYBytes(append(append([]byte("redraw"), marker(req)...), []byte("forbidden")...))
			}
			epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
			if err := epoch.Start(context.Background(), kind); err != nil {
				t.Fatal(err)
			}
			prepare := wire.wait(t, func(fs []terminal.Frame) bool { return len(fs) == 1 })[0]
			inputErr := epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: prepare.Source, Epoch: prepare.Epoch, Data: []byte("unavailable")})
			if !errors.Is(inputErr, terminal.ErrObserveOnly) && !errors.Is(inputErr, terminal.ErrOutOfState) {
				t.Fatalf("%s input became available before COMMIT: %v", kind, inputErr)
			}
			if got := pty.bytes(); len(got) != 0 {
				t.Fatalf("%s input reached PTY before COMMIT: %q", kind, got)
			}
			select {
			case <-epoch.Done():
				t.Fatalf("%s unavailable input terminalized the epoch: %v", kind, epoch.Err())
			default:
			}
			deferFrame := browserFrame(terminal.FrameDefer, prepare.Cut)
			deferFrame.Reason = "unchanged"
			err := epoch.HandleFrame(deferFrame)
			if !errors.Is(err, terminal.ErrClosed) || !errors.Is(err, terminal.ErrReopenRequired) {
				t.Fatalf("DEFER did not fail closed: %v", err)
			}
			if err := epoch.Finalize(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, f := range wire.snapshot() {
				if f.Type == terminal.FrameLive || f.Type == terminal.FrameCommit {
					t.Fatalf("illegal release after %s DEFER: %#v", kind, f)
				}
			}
		})
	}
}

func TestFinalOwnerParentCancellationTerminalizesAndCleans(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error { return epoch.PTYBytes(marker(req)) }
	epoch, _ = terminal.NewEpoch(parent, source, 9, wire, pty, config())
	startLive(t, epoch, wire)
	cancel()
	<-epoch.Done()
	terminalErr := epoch.Err()
	if !errors.Is(terminalErr, terminal.ErrClosed) || !errors.Is(terminalErr, context.Canceled) {
		t.Fatalf("cancel terminal identity=%v", terminalErr)
	}
	if err := epoch.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []func() error{
		func() error { return epoch.Start(context.Background(), terminal.CutInitial) },
		func() error { return epoch.PTYBytes([]byte("late")) },
		func() error { return epoch.HandleFrame(browserFrame(terminal.FrameReady, 1)) },
	} {
		if err := operation(); err == nil || err.Error() != terminalErr.Error() {
			t.Fatalf("post-cancel operation=%v want=%v", err, terminalErr)
		}
	}
	actions, _, _, _ := tx.snapshot()
	if !reflect.DeepEqual(actions, []terminal.PinnedAction{terminal.ActionBind, terminal.ActionCut, terminal.ActionCleanup}) {
		t.Fatalf("cancel cleanup actions=%v", actions)
	}
}

func TestFinalOwnerSynchronousCallbackTerminalizationDoesNotSelfWait(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newRecordingPTY()
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error {
		duplicate := append(marker(req), marker(req)...)
		return epoch.PTYBytes(duplicate)
	}
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
	returned := make(chan error, 1)
	go func() { returned <- epoch.Start(context.Background(), terminal.CutInitial) }()
	select {
	case err := <-returned:
		if err == nil || !errors.Is(epoch.Err(), terminal.ErrClosed) {
			t.Fatalf("fatal callback result=%v terminal=%v", err, epoch.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("synchronous callback waited on its own WorkGate claim")
	}
	if err := epoch.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type accountingPTY struct {
	mu       sync.Mutex
	started  chan chan struct{}
	returned chan struct{}
}

func newAccountingPTY() *accountingPTY {
	return &accountingPTY{started: make(chan chan struct{}, 4), returned: make(chan struct{}, 4)}
}

func (w *accountingPTY) WriteContext(ctx context.Context, _ []byte) (int, error) {
	entered := make(chan struct{})
	w.started <- entered
	close(entered)
	<-ctx.Done()
	w.returned <- struct{}{}
	return 0, terminal.ErrCanceled
}

func (w *accountingPTY) Close() error { return nil }

func TestFinalOwnerInputAccountingAcrossDequeueRevokeRegrant(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire, pty := newMemoryTransport(), newAccountingPTY()
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error { return epoch.PTYBytes(marker(req)) }
	epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
	startLive(t, epoch, wire)
	control := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: "opaque-source-1", Epoch: 9, Mode: terminal.ModeControl}
	observe := control
	observe.Mode = terminal.ModeObserve
	input := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: "opaque-source-1", Epoch: 9, Data: make([]byte, terminal.MaxInputBytes)}
	for cycle := 0; cycle < 3; cycle++ {
		if err := epoch.HandleFrame(control); err != nil {
			t.Fatal(err)
		}
		if err := epoch.HandleFrame(input); err != nil {
			t.Fatalf("cycle %d full-cap admission=%v", cycle, err)
		}
		<-pty.started // dequeue has happened and the production writer owns it
		if err := epoch.HandleFrame(observe); err != nil {
			t.Fatal(err)
		}
		<-pty.returned
		deadline := time.Now().Add(time.Second)
		for !epoch.InputAccountingSettledForTest() {
			if time.Now().After(deadline) {
				t.Fatal("input accounting did not settle after revocation")
			}
			time.Sleep(time.Millisecond)
		}
	}
	if err := epoch.HandleFrame(control); err != nil {
		t.Fatal(err)
	}
	if err := epoch.HandleFrame(input); err != nil {
		t.Fatal(err)
	}
	<-pty.started
	extra := input
	extra.Data = []byte("x")
	if err := epoch.HandleFrame(extra); !errors.Is(err, terminal.ErrSaturated) {
		t.Fatalf("aggregate cap admitted an extra byte: %v", err)
	}
	if err := epoch.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type endBlockingTransport struct {
	mu          sync.Mutex
	frames      []terminal.Frame
	endAttempts int
}

func newEndBlockingTransport() *endBlockingTransport {
	return &endBlockingTransport{}
}

func (w *endBlockingTransport) WriteFrame(ctx context.Context, raw []byte) error {
	f, err := terminal.DecodeFrame(raw)
	if err != nil {
		return err
	}
	if f.Type == terminal.FrameEnd {
		w.mu.Lock()
		w.endAttempts++
		w.mu.Unlock()
		<-ctx.Done()
		return errors.New("end write failed")
	}
	w.mu.Lock()
	w.frames = append(w.frames, f)
	w.mu.Unlock()
	return nil
}

func (w *endBlockingTransport) Close(context.Context) error { return nil }

func TestFinalOwnerServerENDIsSingleSanitizedFIFOAndBounded(t *testing.T) {
	t.Run("graceful-and-browser-rejected", func(t *testing.T) {
		tx := &fakeTransaction{cutCapture: []byte("history\n")}
		source, _ := sourceFor(t, tx)
		wire, pty := newMemoryTransport(), newRecordingPTY()
		var epoch *terminal.Epoch
		tx.onCut = func(req terminal.TransactionRequest) error { return epoch.PTYBytes(marker(req)) }
		epoch, _ = terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
		startLive(t, epoch, wire)
		clientEnd := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameEnd, Source: "opaque-source-1", Epoch: 9, Reason: "client"}
		if err := epoch.HandleFrame(clientEnd); !errors.Is(err, terminal.ErrOutOfState) {
			t.Fatalf("browser END accepted: %v", err)
		}
		if err := epoch.PTYBytes([]byte("before-end")); err != nil {
			t.Fatal(err)
		}
		if err := epoch.Finalize(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := epoch.Finalize(context.Background()); err != nil {
			t.Fatal(err)
		}
		frames := wire.snapshot()
		endCount := 0
		for i, f := range frames {
			if f.Type == terminal.FrameEnd {
				endCount++
				if f.Reason != "closed" || i != len(frames)-1 {
					t.Fatalf("END not sanitized/FIFO-final: %#v frames=%#v", f, frames)
				}
			}
		}
		if endCount != 1 {
			t.Fatalf("END count=%d frames=%#v", endCount, frames)
		}
	})

	t.Run("fault-sanitized", func(t *testing.T) {
		tx := &fakeTransaction{cutCapture: []byte("history\n")}
		source, _ := sourceFor(t, tx)
		wire, pty := newMemoryTransport(), newRecordingPTY()
		epoch, _ := terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
		secret := "/host/private/path command --token"
		if err := epoch.Fault(errors.New(secret)); err != nil {
			t.Fatal(err)
		}
		if err := epoch.Finalize(context.Background()); err != nil {
			t.Fatal(err)
		}
		frames := wire.snapshot()
		if len(frames) != 1 || frames[0].Type != terminal.FrameEnd || frames[0].Reason != "fault" || strings.Contains(frames[0].Reason, secret) {
			t.Fatalf("unsanitized fault END=%#v", frames)
		}
	})

	t.Run("blocked-end-bounded", func(t *testing.T) {
		tx := &fakeTransaction{cutCapture: []byte("history\n")}
		source, _ := sourceFor(t, tx)
		wire, pty := newEndBlockingTransport(), newRecordingPTY()
		epoch, _ := terminal.NewEpoch(context.Background(), source, 9, wire, pty, config())
		done := make(chan error, 1)
		go func() { done <- epoch.Finalize(context.Background()) }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "end write failed") {
				t.Fatalf("blocked END failure not reported: %v", err)
			}
		case <-time.After(2 * config().CutTimeout):
			t.Fatal("blocked END teardown was unbounded")
		}
		wire.mu.Lock()
		attempts := wire.endAttempts
		wire.mu.Unlock()
		if attempts != 1 {
			t.Fatalf("END attempts=%d", attempts)
		}
	})
}

func TestFinalOwnerConcurrentFinalizeSharesSingleCleanupObligation(t *testing.T) {
	tx := &fakeTransaction{
		bindResult: bindOwnerResult(), cleanupStarted: make(chan struct{}), cleanupRelease: make(chan struct{}),
	}
	var epoch *terminal.Epoch
	tx.onCut = func(request terminal.TransactionRequest) error { return epoch.PTYBytes(marker(request)) }
	epoch, wire := newBindIngressEpoch(t, tx, nil)
	if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	if err := epoch.Fault(errors.New("concurrent finalization")); err != nil {
		t.Fatal(err)
	}
	<-epoch.Done()
	<-tx.cleanupStarted

	const callers = 8
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() { results <- epoch.Finalize(context.Background()) }()
	}
	select {
	case err := <-results:
		t.Fatalf("Finalize bypassed in-flight cleanup: %v", err)
	default:
	}
	actions, _, _, _ := tx.snapshot()
	cleanupCount := 0
	for _, action := range actions {
		if action == terminal.ActionCleanup {
			cleanupCount++
		}
	}
	if cleanupCount != 1 {
		t.Fatalf("in-flight cleanup count=%d actions=%v", cleanupCount, actions)
	}
	close(tx.cleanupRelease)
	for i := 0; i < callers; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	actions, _, _, _ = tx.snapshot()
	cleanupCount = 0
	for _, action := range actions {
		if action == terminal.ActionCleanup {
			cleanupCount++
		}
	}
	if cleanupCount != 1 {
		t.Fatalf("completed cleanup count=%d actions=%v", cleanupCount, actions)
	}
	if count := countFrames(wire.snapshot(), terminal.FrameEnd); count != 1 {
		t.Fatalf("END count=%d", count)
	}
}
