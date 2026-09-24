package terminal_test

import (
	"context"
	"errors"
	"testing"

	terminal "persea-terminal/internal/terminal"
)

// A resize error the source did NOT type as a pre-issue refusal means the
// geometry command was issued, or its outcome is unknown, or the source is
// dead: tmux may hold the new geometry while this epoch's cut sequence and the
// durable journal do not. The epoch must fault — publishing END, refusing
// further frames, reporting a reopen verdict — and must NOT release the held
// bytes as LIVE output of the old cut, because they would be rendered under a
// geometry that is no longer true. Taking the operational path here would
// leave a live attachment at the old geometry after a journal failure.
func TestResizeUntypedSourceFailureFaultsTheEpoch(t *testing.T) {
	for _, variant := range []struct {
		name  string
		cause error
	}{
		{name: "plain error after issue", cause: errors.New("durable geometry commit after tmux mutated: journal quota exceeded")},
		{name: "reopen required", cause: terminal.ErrReopenRequired},
		{name: "malformed without refusal typing", cause: terminal.ErrMalformed},
	} {
		t.Run(variant.name, func(t *testing.T) {
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

			tx.mu.Lock()
			tx.resizeErr = variant.cause
			// Output that lands while the resize is in flight is held by the
			// pending cut; a fault must not re-publish it as LIVE of the old cut.
			tx.onResize = func(terminal.TransactionRequest) error { return epoch.PTYBytes([]byte("during-resize")) }
			tx.mu.Unlock()
			resize := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameResize, Source: "opaque-source-1", Epoch: 9, Columns: 80, Rows: 40}
			err := epoch.Resize(context.Background(), resize)
			if err == nil || errors.Is(err, terminal.ErrResizeFailed) {
				t.Fatalf("untyped resize failure was treated as operational: err=%v", err)
			}
			if terminal := epoch.Err(); terminal == nil {
				t.Fatalf("untyped resize failure did not fault the epoch: err=%v", err)
			}
			frames := wire.wait(t, func(frames []terminal.Frame) bool {
				for _, frame := range frames[before:] {
					if frame.Type == terminal.FrameEnd {
						return true
					}
				}
				return false
			})
			for _, frame := range frames[before:] {
				if frame.Type == terminal.FrameLive && string(frame.Data) == "during-resize" {
					t.Fatalf("a faulted resize released held bytes as LIVE of the old cut: %+v", frame)
				}
				if frame.Type == terminal.FramePrepare {
					t.Fatalf("a faulted resize published a PREPARE: %+v", frame)
				}
			}
			input := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: "opaque-source-1", Epoch: 9, Data: []byte("ls\n")}
			if err := epoch.HandleFrame(input); err == nil {
				t.Fatal("input was accepted on a faulted epoch")
			}
			_ = epoch.Finalize(context.Background())
		})
	}
}

// The source classifies for the epoch: a transaction's typed refusal passes
// through intact, and every other transaction error is joined to the reopen
// verdict so no caller can mistake it for a refusal.
func TestPinnedSourceResizeKeepsRefusalTypingAndFaultsEverythingElse(t *testing.T) {
	cause := errors.New("journal quota exceeded")
	if !terminal.IsResizeRefusal(terminal.RefuseResize(cause)) {
		t.Fatal("RefuseResize did not type its cause")
	}
	if !errors.Is(terminal.RefuseResize(cause), cause) {
		t.Fatal("RefuseResize hid its cause")
	}
	if terminal.IsResizeRefusal(errors.Join(terminal.ErrReopenRequired, cause)) {
		t.Fatal("an untyped error read as a refusal")
	}
	if terminal.IsResizeRefusal(nil) {
		t.Fatal("nil read as a refusal")
	}
}
