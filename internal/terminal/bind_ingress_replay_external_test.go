package terminal_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"persea-terminal/internal/terminal"
)

func newBindIngressEpoch(t *testing.T, tx *fakeTransaction, parent context.Context) (*terminal.Epoch, *memoryTransport) {
	t.Helper()
	if parent == nil {
		parent = context.Background()
	}
	source, err := terminal.NewPinnedSource(witness(), tx, newScriptedProbe())
	if err != nil {
		t.Fatal(err)
	}
	wire := newMemoryTransport()
	e, err := terminal.NewEpoch(parent, source, 9, wire, newRecordingPTY(), config())
	if err != nil {
		t.Fatal(err)
	}
	return e, wire
}

func bindOwnerResult() *terminal.TransactionResult {
	return &terminal.TransactionResult{
		Witness:    witness(),
		Attachment: terminal.AttachmentIDs{ShadowSessionID: "$bind-owner-shadow", ClientID: "$bind-owner-client"},
	}
}

func onePrepare(t *testing.T, wire *memoryTransport) terminal.Frame {
	t.Helper()
	frames := wire.wait(t, func(frames []terminal.Frame) bool {
		return len(frames) > 0 && frames[0].Type == terminal.FramePrepare
	})
	if len(frames) != 1 || frames[0].Type != terminal.FramePrepare {
		t.Fatalf("frames = %#v, want exactly one PREPARE", frames)
	}
	return frames[0]
}

func assertNoCutOrPrepare(t *testing.T, tx *fakeTransaction, wire *memoryTransport) {
	t.Helper()
	actions, _, _, _ := tx.snapshot()
	for _, action := range actions {
		if action == terminal.ActionCut {
			t.Fatalf("actions = %v, CUT must be suppressed", actions)
		}
	}
	for _, frame := range wire.snapshot() {
		if frame.Type == terminal.FramePrepare {
			t.Fatalf("frames = %#v, PREPARE must be suppressed", wire.snapshot())
		}
	}
}

func TestBindIngressReplayInitialAndReconnectFIFOExactlyOnce(t *testing.T) {
	for _, kind := range []terminal.CutKind{terminal.CutInitial, terminal.CutReconnect} {
		t.Run(string(kind), func(t *testing.T) {
			tx := &fakeTransaction{}
			var e *terminal.Epoch
			tx.onBind = func(terminal.TransactionRequest) error {
				if err := e.PTYBytes([]byte("bind-one/")); err != nil {
					return err
				}
				return e.PTYBytes([]byte("bind-two"))
			}
			tx.onCut = func(req terminal.TransactionRequest) error {
				return e.PTYBytes(marker(req))
			}
			var wire *memoryTransport
			e, wire = newBindIngressEpoch(t, tx, nil)
			if err := e.Start(context.Background(), kind); err != nil {
				t.Fatal(err)
			}
			prepare := onePrepare(t, wire)
			if got, want := prepare.Replay, []byte("bind-one/bind-two"); !bytes.Equal(got, want) {
				t.Fatalf("PREPARE.Replay = %q, want %q", got, want)
			}
		})
	}
}

func TestBindIngressReplayConcatenatesCutPreMarkerAndKeepsPostMarkerHeld(t *testing.T) {
	tx := &fakeTransaction{bindResult: bindOwnerResult()}
	var e *terminal.Epoch
	tx.onBind = func(terminal.TransactionRequest) error {
		return e.PTYBytes([]byte("bind/"))
	}
	tx.onCut = func(req terminal.TransactionRequest) error {
		if err := e.PTYBytes([]byte("cut-pre/")); err != nil {
			return err
		}
		if err := e.PTYBytes(marker(req)); err != nil {
			return err
		}
		return e.PTYBytes([]byte("post-marker"))
	}
	var wire *memoryTransport
	e, wire = newBindIngressEpoch(t, tx, nil)
	if err := e.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	prepare := onePrepare(t, wire)
	if got, want := prepare.Replay, []byte("bind/cut-pre/"); !bytes.Equal(got, want) {
		t.Fatalf("PREPARE.Replay = %q, want %q", got, want)
	}
	if err := e.HandleFrame(browserFrame(terminal.FrameReady, prepare.Cut)); err != nil {
		t.Fatal(err)
	}
	frames := wire.wait(t, func(frames []terminal.Frame) bool {
		for _, frame := range frames {
			if frame.Type == terminal.FrameLive && bytes.Equal(frame.Data, []byte("post-marker")) {
				return true
			}
		}
		return false
	})
	var replayCopies, heldCopies int
	for _, frame := range frames {
		replayCopies += bytes.Count(frame.Replay, []byte("bind/cut-pre/"))
		heldCopies += bytes.Count(frame.Data, []byte("post-marker"))
	}
	if replayCopies != 1 || heldCopies != 1 {
		t.Fatalf("replay copies = %d, held copies = %d; frames = %#v", replayCopies, heldCopies, frames)
	}
}

func TestBindIngressReplaySplitMarkerCrossesOwnershipSeam(t *testing.T) {
	tx := &fakeTransaction{}
	var e *terminal.Epoch
	prefix := []byte("\x1b]52;;persea:v1:")
	tx.onBind = func(terminal.TransactionRequest) error {
		if err := e.PTYBytes([]byte("before/")); err != nil {
			return err
		}
		return e.PTYBytes(prefix)
	}
	tx.onCut = func(req terminal.TransactionRequest) error {
		wireMarker := marker(req)
		if !bytes.HasPrefix(wireMarker, prefix) {
			t.Fatalf("marker %q does not have expected prefix %q", wireMarker, prefix)
		}
		return e.PTYBytes(wireMarker[len(prefix):])
	}
	var wire *memoryTransport
	e, wire = newBindIngressEpoch(t, tx, nil)
	if err := e.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	prepare := onePrepare(t, wire)
	if got, want := prepare.Replay, []byte("before/"); !bytes.Equal(got, want) {
		t.Fatalf("PREPARE.Replay = %q, want %q", got, want)
	}
}

func TestBindIngressReplaySaturationTerminalizesAfterExactOwnershipDecision(t *testing.T) {
	tx := &fakeTransaction{bindResult: bindOwnerResult()}
	var e *terminal.Epoch
	tx.onBind = func(terminal.TransactionRequest) error {
		return e.PTYBytes(bytes.Repeat([]byte{'x'}, terminal.ReplayByteCap+1))
	}
	e, wire := newBindIngressEpoch(t, tx, nil)
	if err := e.Start(context.Background(), terminal.CutInitial); !errors.Is(err, terminal.ErrSaturated) {
		t.Fatalf("Start error = %v, want ErrSaturated", err)
	}
	if err := e.Finalize(context.Background()); err != nil {
		t.Fatalf("Finalize error = %v, want nil after successful cleanup", err)
	}
	assertNoCutOrPrepare(t, tx, wire)
	_, _, callbackErr, _ := tx.snapshot()
	if !errors.Is(callbackErr, terminal.ErrSaturated) {
		t.Fatalf("BIND callback error = %v, want ErrSaturated", callbackErr)
	}
	assertOneExactCleanup(t, tx)
}

func TestBindIngressReplayWrappedStateLikeBindErrorsTerminalizeAndCleanExactOwner(t *testing.T) {
	for _, bindErr := range []error{terminal.ErrClosed, terminal.ErrStale, terminal.ErrOutOfState} {
		t.Run(bindErr.Error(), func(t *testing.T) {
			tx := &fakeTransaction{bindResult: bindOwnerResult(), bindErr: errors.Join(errors.New("BIND failed"), bindErr)}
			var e *terminal.Epoch
			tx.onBind = func(terminal.TransactionRequest) error {
				return e.PTYBytes([]byte("discard-me"))
			}
			var wire *memoryTransport
			e, wire = newBindIngressEpoch(t, tx, nil)
			if err := e.Start(context.Background(), terminal.CutInitial); !errors.Is(err, terminal.ErrClosed) || !errors.Is(err, bindErr) {
				t.Fatalf("Start error = %v, want terminal ErrClosed joined with %v", err, bindErr)
			}
			if err := e.Finalize(context.Background()); err != nil {
				t.Fatalf("Finalize error = %v, want nil after successful cleanup", err)
			}
			assertNoCutOrPrepare(t, tx, wire)
			assertOneExactCleanup(t, tx)
		})
	}
}

func TestBindIngressReplayWrappedStateLikeCutErrorsTerminalizeAndCleanExactOwner(t *testing.T) {
	for _, kind := range []terminal.CutKind{terminal.CutInitial, terminal.CutReconnect} {
		for _, cutErr := range []error{terminal.ErrClosed, terminal.ErrStale, terminal.ErrOutOfState} {
			t.Run(string(kind)+"/"+cutErr.Error(), func(t *testing.T) {
				tx := &fakeTransaction{
					bindResult: bindOwnerResult(),
					cutErr:     errors.Join(errors.New("CUT failed"), cutErr),
				}
				var e *terminal.Epoch
				tx.onBind = func(terminal.TransactionRequest) error {
					return e.PTYBytes([]byte("discard-me"))
				}
				var wire *memoryTransport
				e, wire = newBindIngressEpoch(t, tx, nil)
				if err := e.Start(context.Background(), kind); !errors.Is(err, terminal.ErrClosed) || !errors.Is(err, terminal.ErrReopenRequired) || !errors.Is(err, cutErr) {
					t.Fatalf("Start error = %v, want terminal ErrClosed joined with ErrReopenRequired and %v", err, cutErr)
				}
				select {
				case <-e.Done():
				default:
					t.Fatal("Done remains open after admitted CUT failure")
				}
				if err := e.Err(); !errors.Is(err, terminal.ErrClosed) || !errors.Is(err, cutErr) {
					t.Fatalf("Err = %v, want terminal ErrClosed joined with %v", err, cutErr)
				}
				if err := e.PTYBytes([]byte("after-terminal")); !errors.Is(err, terminal.ErrClosed) || !errors.Is(err, cutErr) {
					t.Fatalf("PTYBytes after terminal CUT failure = %v, want terminal ErrClosed joined with %v", err, cutErr)
				}
				if err := e.Finalize(context.Background()); err != nil {
					t.Fatalf("Finalize error = %v, want nil after successful cleanup", err)
				}
				actions, _, _, _ := tx.snapshot()
				want := []terminal.PinnedAction{terminal.ActionBind, terminal.ActionCut, terminal.ActionCleanup}
				if len(actions) != len(want) {
					t.Fatalf("actions = %v, want %v", actions, want)
				}
				for i := range want {
					if actions[i] != want[i] {
						t.Fatalf("actions = %v, want %v", actions, want)
					}
				}
				for _, frame := range wire.snapshot() {
					if frame.Type == terminal.FramePrepare {
						t.Fatalf("frames = %#v, PREPARE must be discarded after CUT failure", wire.snapshot())
					}
				}
				assertOneExactCleanup(t, tx)
			})
		}
	}
}

func TestBindIngressReplayCancellationAndFinalizeDiscardWhileAwaitingExactOwnership(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(context.CancelFunc, *terminal.Epoch) <-chan error
	}{
		{
			name: "parent-cancel",
			stop: func(cancel context.CancelFunc, _ *terminal.Epoch) <-chan error {
				cancel()
				return nil
			},
		},
		{
			name: "Finalize",
			stop: func(_ context.CancelFunc, e *terminal.Epoch) <-chan error {
				done := make(chan error, 1)
				go func() { done <- e.Finalize(context.Background()) }()
				return done
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			tx := &fakeTransaction{
				bindResult:  bindOwnerResult(),
				bindStarted: make(chan struct{}),
				bindRelease: make(chan struct{}),
			}
			var e *terminal.Epoch
			tx.onBind = func(terminal.TransactionRequest) error {
				return e.PTYBytes([]byte("discard-me"))
			}
			var wire *memoryTransport
			e, wire = newBindIngressEpoch(t, tx, parent)
			startDone := make(chan error, 1)
			go func() { startDone <- e.Start(context.Background(), terminal.CutInitial) }()
			<-tx.bindStarted
			finalizeDone := tc.stop(cancel, e)
			// Fault is a synchronous barrier proving that at least one terminal
			// request has linearized while exact ownership is still unresolved.
			if err := e.Fault(errors.New("pending ownership barrier")); err != nil {
				t.Fatal(err)
			}
			select {
			case <-e.Done():
				t.Fatal("Done closed before exact BIND ownership decision")
			default:
			}
			if err := e.Err(); err != nil {
				t.Fatalf("Err published before exact BIND ownership decision: %v", err)
			}
			select {
			case err := <-startDone:
				t.Fatalf("Start returned before exact BIND ownership decision: %v", err)
			default:
			}
			if finalizeDone != nil {
				select {
				case err := <-finalizeDone:
					t.Fatalf("Finalize returned before exact BIND ownership decision: %v", err)
				default:
				}
			}
			close(tx.bindRelease)
			startErr := <-startDone
			if !errors.Is(startErr, terminal.ErrClosed) {
				t.Fatalf("Start error = %v, want ErrClosed", startErr)
			}
			<-e.Done()
			if startErr != e.Err() {
				t.Fatalf("Start result=%v want immutable Err=%v", startErr, e.Err())
			}
			if finalizeDone != nil {
				if err := <-finalizeDone; err != nil {
					t.Fatalf("Finalize error = %v, want nil after successful cleanup", err)
				}
			} else if err := e.Finalize(context.Background()); err != nil {
				t.Fatalf("Finalize error = %v, want nil after successful cleanup", err)
			}
			assertNoCutOrPrepare(t, tx, wire)
			assertOneExactCleanup(t, tx)
		})
	}
}

func TestBindIngressReplayReservedMarkerDuringBindFailsClosed(t *testing.T) {
	tx := &fakeTransaction{bindResult: bindOwnerResult()}
	var e *terminal.Epoch
	tx.onBind = func(terminal.TransactionRequest) error {
		reserved := []byte("\x1b]52;;persea:v1:" + string(bytes.Repeat([]byte{'A'}, 43)) + "\a")
		return e.PTYBytes(reserved)
	}
	e, wire := newBindIngressEpoch(t, tx, nil)
	if err := e.Start(context.Background(), terminal.CutInitial); err == nil || !errors.Is(err, terminal.ErrClosed) {
		t.Fatalf("Start error = %v, want terminal ErrClosed", err)
	}
	if err := e.Finalize(context.Background()); err != nil {
		t.Fatalf("Finalize error = %v, want nil after successful cleanup", err)
	}
	assertNoCutOrPrepare(t, tx, wire)
	_, _, callbackErr, _ := tx.snapshot()
	if callbackErr == nil {
		t.Fatal("reserved marker BIND callback unexpectedly succeeded")
	}
	assertOneExactCleanup(t, tx)
}

func TestBindIngressReplayRejectsBytesBeforeOwnerWithoutFramerContamination(t *testing.T) {
	tx := &fakeTransaction{}
	var e *terminal.Epoch
	tx.onBind = func(terminal.TransactionRequest) error {
		return e.PTYBytes([]byte("clean"))
	}
	tx.onCut = func(req terminal.TransactionRequest) error {
		return e.PTYBytes(marker(req))
	}
	var wire *memoryTransport
	e, wire = newBindIngressEpoch(t, tx, nil)
	if err := e.PTYBytes([]byte("\x1b]52;;persea:")); !errors.Is(err, terminal.ErrOutOfState) {
		t.Fatalf("pre-owner PTYBytes error = %v, want ErrOutOfState", err)
	}
	if err := e.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	prepare := onePrepare(t, wire)
	if got, want := prepare.Replay, []byte("clean"); !bytes.Equal(got, want) {
		t.Fatalf("PREPARE.Replay = %q, want %q", got, want)
	}
}
