package broker

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

type geometryReadinessTransaction struct {
	epoch   *terminal.Epoch
	witness terminal.SourceWitness
	resize  func() error
}

func (tx *geometryReadinessTransaction) Witness(_ context.Context, pid int) (terminal.ProcessWitness, error) {
	if pid == tx.witness.Server.PID {
		return tx.witness.Server, nil
	}
	return tx.witness.Pane, nil
}

func (tx *geometryReadinessTransaction) RunPinned(_ context.Context, request terminal.TransactionRequest) (terminal.TransactionResult, error) {
	if request.Action == terminal.ActionResize {
		if err := tx.resize(); err != nil {
			return terminal.TransactionResult{}, err
		}
		tx.witness.Columns, tx.witness.Rows = request.Columns, request.Rows
	}
	if request.Action == terminal.ActionCut {
		if err := tx.epoch.PTYBytes([]byte("\x1b]52;;" + string(request.Marker) + "\a")); err != nil {
			return terminal.TransactionResult{}, err
		}
	}
	return terminal.TransactionResult{Witness: tx.witness, Attachment: terminal.AttachmentIDs{ShadowSessionID: "$geometry", ClientID: "geometry-client"}}, nil
}

type geometryReadinessInput struct{ writes chan string }

func (input geometryReadinessInput) WriteContext(_ context.Context, data []byte) (int, error) {
	input.writes <- string(data)
	return len(data), nil
}
func (geometryReadinessInput) Close() error { return nil }

func geometryReadinessReceive[T any](t *testing.T, values <-chan T, label string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("waiting for %s", label)
		var zero T
		return zero
	}
}

// Geometry is committed and reaches the tail before the real epoch's READY
// handler runs. Neither geometry nor following output may unseal the browser
// until that handler has actually restored input admission. No tmux or sleep
// is involved: the source and the handler are held at explicit lifecycle seams.
func TestUnifiedGeometryReadinessWaitsForActualReady(t *testing.T) {
	for _, outcome := range []string{"ready", "close", "ready_failure", "subscriber_verdict"} {
		t.Run(outcome, func(t *testing.T) {
			effects, key := unifiedE2E1JournalProvider(t, "geometry-ready-"+outcome, "$0")
			server, client := net.Pipe()
			frames := make(chan terminal.Frame, 16)
			readerDone := make(chan struct{})
			go func() {
				defer close(readerDone)
				for {
					raw, err := proto.ReadFrame(client)
					if err != nil {
						return
					}
					if raw.Type == proto.FrameAttachment {
						frame, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
						if err != nil {
							return
						}
						frames <- frame
					}
				}
			}()
			writer := &unifiedAttachmentFrameWriter{
				downstream: &attachmentFrameWriter{wire: &lockedWriter{w: server}},
				provider:   effects, session: "$0", end: func() { _ = server.Close() },
			}
			tx := &geometryReadinessTransaction{witness: terminal.SourceWitness{
				Incarnation: "geometry-source", Socket: terminal.SocketIdentity{Path: "/private/geometry.sock", Device: 1, Inode: 2},
				Server: terminal.ProcessWitness{PID: 101, StartTime: 1}, Pane: terminal.ProcessWitness{PID: 102, StartTime: 2},
				SessionID: "$0", WindowID: "@0", PaneID: "%0", Columns: 80, Rows: 24,
			}}
			source, err := terminal.NewPinnedSource(tx.witness, tx, tx)
			if err != nil {
				t.Fatal(err)
			}
			input := geometryReadinessInput{writes: make(chan string, 8)}
			epoch, err := terminal.NewEpoch(context.Background(), source, 1, writer, input, terminal.Config{
				Clock: terminal.RealClock{}, QuietInterval: time.Hour, MaximumInterval: 2 * time.Hour, CutTimeout: 3 * time.Hour,
			})
			if err != nil {
				t.Fatal(err)
			}
			tx.epoch, writer.epoch = epoch, epoch
			releaseReady := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(releaseReady) })
				_ = writer.Close(context.Background())
				_ = server.Close()
				_ = client.Close()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = epoch.Finalize(ctx)
				geometryReadinessReceive(t, readerDone, "wire reader shutdown")
			})
			if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
				t.Fatal(err)
			}
			prepared := geometryReadinessReceive(t, frames, "initial PREPARE")
			if prepared.Type != terminal.FramePrepare {
				t.Fatalf("initial frame: %+v", prepared)
			}
			if err := epoch.HandleFrame(terminal.Frame{Version: 1, Type: terminal.FrameReady, Source: prepared.Source, Epoch: 1, Cut: prepared.Cut}); err != nil {
				t.Fatal(err)
			}
			if frame := geometryReadinessReceive(t, frames, "initial COMMIT"); frame.Type != terminal.FrameCommit {
				t.Fatalf("initial commit: %+v", frame)
			}
			if err := epoch.HandleFrame(terminal.Frame{Version: 1, Type: terminal.FrameModeRequest, Source: prepared.Source, Epoch: 1, Mode: terminal.ModeControl}); err != nil {
				t.Fatal(err)
			}
			if frame := geometryReadinessReceive(t, frames, "Control grant"); frame.Type != terminal.FrameMode {
				t.Fatalf("mode: %+v", frame)
			}
			readyEntered, tailWaiting := make(chan struct{}), make(chan struct{})
			var waitingOnce sync.Once
			writer.mu.Lock()
			writer.handleReady = func(frame terminal.Frame) error {
				close(readyEntered)
				<-releaseReady
				if outcome == "ready_failure" {
					return terminal.ErrStale
				}
				return epoch.HandleFrame(frame)
			}
			writer.geometryWaiting = func() { waitingOnce.Do(func() { close(tailWaiting) }) }
			writer.mu.Unlock()
			fence, err := writer.beginResize()
			if err != nil {
				t.Fatal(err)
			}
			tx.resize = func() error {
				for _, event := range []unifiedjournal.Event{
					{Kind: unifiedjournal.RecordOutput, Sequence: 2, Start: 0, End: 6, Payload: []byte("before")},
					{Kind: unifiedjournal.RecordGeometry, Sequence: 3, Start: 6, End: 6, Geometry: unifiedjournal.Geometry{Columns: 80, Rows: 37}},
					{Kind: unifiedjournal.RecordOutput, Sequence: 4, Start: 6, End: 11, Payload: []byte("after")},
				} {
					if err := effects.publishEvent(key, event); err != nil {
						return err
					}
				}
				return nil
			}
			if err := epoch.Resize(context.Background(), terminal.Frame{Version: 1, Type: terminal.FrameResize, Source: prepared.Source, Epoch: 1, Columns: 80, Rows: 37}); err != nil {
				t.Fatal(err)
			}
			geometryReadinessReceive(t, readyEntered, "held real READY handler")
			geometryReadinessReceive(t, tailWaiting, "geometry readiness fence")
			if frame := geometryReadinessReceive(t, frames, "preceding output"); frame.Type != terminal.FrameLive || string(frame.Data) != "before" {
				t.Fatalf("preceding output: %+v", frame)
			}
			select {
			case frame := <-frames:
				t.Fatalf("geometry escaped before READY: %+v", frame)
			default:
			}
			attempt := terminal.Frame{Version: 1, Type: terminal.FrameInput, Source: prepared.Source, Epoch: 1, Data: []byte("during")}
			if err := epoch.HandleFrame(attempt); !errors.Is(err, terminal.ErrObserveOnly) {
				t.Fatalf("in-cut input must still be refused, got %v", err)
			}
			switch outcome {
			case "close":
				_ = writer.Close(context.Background())
			case "subscriber_verdict":
				effects.subscriberMu.Lock()
				effects.closeSubscriberLocked(key, writer.tail, proto.SubscriberClosedLagged)
				effects.subscriberMu.Unlock()
			default:
				releaseOnce.Do(func() { close(releaseReady) })
			}
			if outcome == "ready" {
				if frame := geometryReadinessReceive(t, frames, "committed geometry"); frame.Type != terminal.FramePrepare || frame.Kind != terminal.CutResize || frame.Rows != 37 {
					t.Fatalf("geometry: %+v", frame)
				}
				attempt.Data = []byte("after-ready")
				if err := epoch.HandleFrame(attempt); err != nil {
					t.Fatalf("visible geometry did not establish input readiness: %v", err)
				}
				if frame := geometryReadinessReceive(t, frames, "following output"); frame.Type != terminal.FrameLive || string(frame.Data) != "after" {
					t.Fatalf("following output: %+v", frame)
				}
				if got := geometryReadinessReceive(t, input.writes, "accepted input"); got != "after-ready" {
					t.Fatalf("refused input was replayed: %q", got)
				}
			} else {
				if outcome != "subscriber_verdict" {
					geometryReadinessReceive(t, fence.done, "failed or cancelled fence")
				} else {
					geometryReadinessReceive(t, writer.ended, "subscriber shutdown")
				}
				select {
				case frame := <-frames:
					t.Fatalf("failed/cancelled resize published geometry or output: %+v", frame)
				default:
				}
			}
		})
	}
}

func TestUnifiedGeometryReadinessOwnsExactResize(t *testing.T) {
	writer := &unifiedAttachmentFrameWriter{}
	first, err := writer.beginResize()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.beginResize(); !errors.Is(err, terminal.ErrOutOfState) {
		t.Fatalf("overlapping resize replaced owner: %v", err)
	}
	// Proven pre-issue rejection releases the old stream, without manufacturing
	// a geometry or waiting for a READY that the refused request cannot produce.
	writer.settleResize(first, nil)
	second, err := writer.beginResize()
	if err != nil {
		t.Fatal(err)
	}
	writer.settleResize(first, terminal.ErrStale)
	select {
	case <-second.done:
		t.Fatal("late old completion released the next resize")
	default:
	}
	_ = writer.Close(context.Background())
	geometryReadinessReceive(t, second.done, "close settlement")
	if !errors.Is(second.err, terminal.ErrClosed) {
		t.Fatalf("closed fence result: %v", second.err)
	}
}

func TestUnifiedGeometryReadinessCancellationWakesWaiter(t *testing.T) {
	for _, cancelKind := range []string{"close", "stop_output", "tail_cancel", "verdict", "end"} {
		t.Run(cancelKind, func(t *testing.T) {
			waiting := make(chan struct{})
			writer := &unifiedAttachmentFrameWriter{geometryWaiting: func() { close(waiting) }}
			if _, err := writer.beginResize(); err != nil {
				t.Fatal(err)
			}
			tail := &unifiedDevSubscriber{done: make(chan struct{}), verdict: make(chan struct{})}
			result := make(chan error, 1)
			go func() { result <- writer.awaitGeometryReadiness(tail) }()
			geometryReadinessReceive(t, waiting, "parked geometry waiter")
			switch cancelKind {
			case "close":
				_ = writer.Close(context.Background())
			case "stop_output":
				writer.stopOutput()
			case "tail_cancel":
				close(tail.done)
			case "verdict":
				close(tail.verdict)
			case "end":
				writer.settleResize(writer.resize, terminal.ErrClosed)
			}
			if err := geometryReadinessReceive(t, result, "cancelled waiter return"); !errors.Is(err, terminal.ErrClosed) {
				t.Fatalf("wait result: %v", err)
			}
		})
	}
}

func TestUnifiedGeometryReadinessReplacementBeforePublication(t *testing.T) {
	for _, priorFence := range []bool{false, true} {
		name := "nil_to_resize"
		if priorFence {
			name = "resize_A_to_B"
		}
		t.Run(name, func(t *testing.T) {
			output := &unifiedE2E1LockedBuffer{}
			writer := &unifiedAttachmentFrameWriter{
				downstream: &attachmentFrameWriter{wire: &lockedWriter{w: output}},
				source:     "geometry-source", epochID: 1, cut: 1,
			}
			if priorFence {
				first, err := writer.beginResize()
				if err != nil {
					t.Fatal(err)
				}
				writer.settleResize(first, nil)
			}
			tail := &unifiedDevSubscriber{done: make(chan struct{}), verdict: make(chan struct{})}
			replaced := make(chan *unifiedGeometryReadiness, 1)
			waiting := make(chan struct{})
			var replacement *unifiedGeometryReadiness
			var replaceOnce, waitOnce sync.Once
			writer.geometryReady = func() {
				replaceOnce.Do(func() {
					var err error
					replacement, err = writer.beginResize()
					if err != nil {
						panic(err)
					}
					replaced <- replacement
				})
			}
			writer.geometryWaiting = func() {
				if replacement != nil {
					waitOnce.Do(func() { close(waiting) })
				}
			}
			result := make(chan error, 1)
			go func() {
				result <- writer.writeTailEvent(tail, unifiedjournal.Event{
					Kind: unifiedjournal.RecordGeometry, Sequence: 1,
					Geometry: unifiedjournal.Geometry{Columns: 80, Rows: 37},
				})
			}()
			second := geometryReadinessReceive(t, replaced, "replacement after completed wait")
			t.Cleanup(func() { _ = writer.Close(context.Background()) })
			select {
			case <-waiting:
			case err := <-result:
				t.Fatalf("geometry published before replacement READY: err=%v frames=%+v", err, unifiedE2E1AttachmentFrames(t, output.snapshot()))
			case <-time.After(5 * time.Second):
				t.Fatal("replacement did not regain the geometry fence")
			}
			if raw := output.snapshot(); len(raw) != 0 {
				t.Fatalf("geometry escaped replacement fence: %+v", unifiedE2E1AttachmentFrames(t, raw))
			}
			writer.settleResize(second, nil)
			if err := geometryReadinessReceive(t, result, "publication after replacement READY"); err != nil {
				t.Fatal(err)
			}
			frames := unifiedE2E1AttachmentFrames(t, output.snapshot())
			if len(frames) != 1 || frames[0].Type != terminal.FramePrepare || frames[0].Rows != 37 {
				t.Fatalf("committed geometry: %+v", frames)
			}
		})
	}
}
