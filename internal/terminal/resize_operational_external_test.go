package terminal_test

import (
	"context"
	"errors"
	"testing"

	terminal "persea-terminal/internal/terminal"
)

// A Fit whose tmux transaction is refused BEFORE its geometry command is
// issued — proven by the source's typed *ResizeRefusal — is one request's
// outcome. The epoch stays live, the bytes the pending cut was holding back
// are delivered as LIVE output of the cut still in force, no RESIZE PREPARE is
// published, and the next request — a resize or input — proceeds on the same
// epoch. The untyped counterpart is pinned by
// TestResizeUntypedSourceFailureFaultsTheEpoch: only the typed refusal earns
// this outcome.
func TestResizeSourceFailureIsOperationalAndKeepsTheEpochLive(t *testing.T) {
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
	mode := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: "opaque-source-1", Epoch: 9, Mode: terminal.ModeControl}
	if err := epoch.HandleFrame(mode); err != nil {
		t.Fatal(err)
	}
	before := len(wire.wait(t, func(frames []terminal.Frame) bool {
		for _, frame := range frames {
			if frame.Type == terminal.FrameMode && frame.Mode == terminal.ModeControl {
				return true
			}
		}
		return false
	}))

	refusal := errors.New("guard refused the resize")
	tx.mu.Lock()
	tx.resizeErr = terminal.RefuseResize(refusal)
	// Pane output that lands while the resize is in flight is held by the
	// pending cut; abandonment must release it, not lose it.
	tx.onResize = func(terminal.TransactionRequest) error { return epoch.PTYBytes([]byte("during-resize")) }
	tx.mu.Unlock()
	resize := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameResize, Source: "opaque-source-1", Epoch: 9, Columns: 80, Rows: 40}
	err := epoch.Resize(context.Background(), resize)
	if !errors.Is(err, terminal.ErrResizeFailed) || !errors.Is(err, refusal) {
		t.Fatalf("failed resize err=%v want ErrResizeFailed wrapping the cause", err)
	}
	if errors.Is(err, terminal.ErrClosed) || epoch.Err() != nil {
		t.Fatalf("a failed resize terminated the epoch: err=%v terminal=%v", err, epoch.Err())
	}
	frames := wire.wait(t, func(frames []terminal.Frame) bool {
		for _, frame := range frames[before:] {
			if frame.Type == terminal.FrameLive && string(frame.Data) == "during-resize" {
				return true
			}
		}
		return false
	})
	for _, frame := range frames[before:] {
		if frame.Type == terminal.FramePrepare {
			t.Fatalf("a failed resize published a PREPARE: %+v", frame)
		}
		if frame.Type == terminal.FrameLive && frame.Cut != initial.Cut {
			t.Fatalf("released bytes carried cut %d, want the cut in force %d", frame.Cut, initial.Cut)
		}
		if frame.Type == terminal.FrameEnd {
			t.Fatalf("a failed resize ended the attachment: %+v", frame)
		}
	}

	// The epoch carries on: input is accepted and a later resize applies.
	input := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: "opaque-source-1", Epoch: 9, Data: []byte("ls\n")}
	if err := epoch.HandleFrame(input); err != nil {
		t.Fatalf("input after a failed resize err=%v", err)
	}
	tx.mu.Lock()
	tx.resizeErr, tx.onResize = nil, nil
	tx.mu.Unlock()
	if err := epoch.Resize(context.Background(), resize); err != nil {
		t.Fatalf("resize after a failed resize err=%v terminal=%v", err, epoch.Err())
	}
	prepared := wire.wait(t, func(frames []terminal.Frame) bool {
		for _, frame := range frames {
			if frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutResize {
				return true
			}
		}
		return false
	})
	var resizePrepare terminal.Frame
	for _, frame := range prepared {
		if frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutResize {
			resizePrepare = frame
		}
	}
	if resizePrepare.Rows != 40 || resizePrepare.Cut <= initial.Cut {
		t.Fatalf("later resize PREPARE mismatch: %+v", resizePrepare)
	}
	actions, _, _, _ := tx.snapshot()
	resizeCalls := 0
	for _, action := range actions {
		if action == terminal.ActionResize {
			resizeCalls++
		}
	}
	if resizeCalls != 2 {
		t.Fatalf("resize transaction calls=%d want 2: %v", resizeCalls, actions)
	}
	if err := epoch.HandleFrame(browserFrame(terminal.FrameReady, resizePrepare.Cut)); err != nil {
		t.Fatal(err)
	}
	_ = epoch.Finalize(context.Background())
}
