package broker

import (
	"context"
	"errors"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/terminal"
	"sync"
	"testing"
	"time"
)

type supervisionInputWriter struct {
	entered       chan context.Context
	release       chan struct{}
	mu            sync.Mutex
	calls, closes int
}

func (w *supervisionInputWriter) WriteContext(ctx context.Context, b []byte) (int, error) {
	w.mu.Lock()
	w.calls++
	w.mu.Unlock()
	w.entered <- ctx
	<-w.release
	return len(b), nil
}
func (w *supervisionInputWriter) Close() error { w.mu.Lock(); w.closes++; w.mu.Unlock(); return nil }

type supervisionFrameWriter struct{}

func (supervisionFrameWriter) WriteFrame(context.Context, []byte) error { return nil }
func (supervisionFrameWriter) Close(context.Context) error              { return nil }

type supervisionInputTransaction struct {
	epoch   *terminal.Epoch
	witness terminal.SourceWitness
}

func (tx *supervisionInputTransaction) Witness(_ context.Context, pid int) (terminal.ProcessWitness, error) {
	if pid == tx.witness.Server.PID {
		return tx.witness.Server, nil
	}
	return tx.witness.Pane, nil
}
func (tx *supervisionInputTransaction) RunPinned(_ context.Context, request terminal.TransactionRequest) (terminal.TransactionResult, error) {
	if request.Action == terminal.ActionCut {
		if err := tx.epoch.PTYBytes([]byte("\x1b]52;;" + string(request.Marker) + "\a")); err != nil {
			return terminal.TransactionResult{}, err
		}
	}
	return terminal.TransactionResult{Witness: tx.witness, Attachment: terminal.AttachmentIDs{ShadowSessionID: "$input", ClientID: "input-client"}}, nil
}

func TestRecordingSupervisionSealsEstablishedAttachment(t *testing.T) {
	realm := openRetentionRealm(t, "supervision-input")
	shadow := &retentionShadowEffects{realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock()}
	registry := newPaneRegistry(shadow)
	defer registry.Close()
	witness := retentionWitness("%1", "input-source")
	witness.Session.Session = "$1"
	witness.Window = "@1"
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	tx := &supervisionInputTransaction{witness: terminal.SourceWitness{Incarnation: witness.Incarnation, Socket: terminal.SocketIdentity{Path: "/private/input.sock", Device: 1, Inode: 2}, Server: terminal.ProcessWitness{PID: 101, StartTime: 1}, Pane: terminal.ProcessWitness{PID: 102, StartTime: 2}, SessionID: "$1", WindowID: "@1", PaneID: "%1", Columns: 80, Rows: 24}}
	source, err := terminal.NewPinnedSource(tx.witness, tx, tx)
	if err != nil {
		t.Fatal(err)
	}
	writer := &supervisionInputWriter{entered: make(chan context.Context, 2), release: make(chan struct{})}
	attachment := registry.attachmentEffects(witness.Session.Server, tx.witness)
	epoch, err := terminal.NewEpochWithAttachmentEffects(context.Background(), source, 1, supervisionFrameWriter{}, writer, terminal.Config{Clock: terminal.RealClock{}, QuietInterval: time.Second, MaximumInterval: 2 * time.Second, CutTimeout: 5 * time.Second}, attachment)
	if err != nil {
		t.Fatal(err)
	}
	tx.epoch = epoch
	var release sync.Once
	defer func() { release.Do(func() { close(writer.release) }); _ = epoch.Finalize(context.Background()) }()
	if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	for _, frame := range []terminal.Frame{{Version: terminal.ProtocolVersion, Type: terminal.FrameReady, Source: witness.Incarnation, Epoch: 1, Cut: 1}, {Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: witness.Incarnation, Epoch: 1, Mode: terminal.ModeControl}} {
		if err := epoch.HandleFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	input := func(s string) error {
		return epoch.HandleFrame(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: witness.Incarnation, Epoch: 1, Data: []byte(s)})
	}
	if err := input("active"); err != nil {
		t.Fatal(err)
	}
	var held context.Context
	select {
	case held = <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("input writer not entered")
	}
	if err := input("queued"); err != nil {
		t.Fatal(err)
	}
	registry.superviseFault(journalKey(witness))
	select {
	case <-epoch.Done():
	default:
		t.Fatal("registry cutoff did not reach established Epoch")
	}
	if held.Err() == nil {
		t.Fatal("active write not cancelled")
	}
	if err := input("future"); !errors.Is(err, terminal.ErrClosed) {
		t.Fatalf("future input: %v", err)
	}
	writer.mu.Lock()
	closes := writer.closes
	writer.mu.Unlock()
	if closes != 0 {
		t.Fatal("held writer closed before return")
	}
	// A bind arriving after the exact cutoff cannot escape the coordinator seal.
	if err := attachment.BindAttachment(epoch); !errors.Is(err, errRecordingStalled) {
		t.Fatalf("late bind escaped cutoff: %v", err)
	}
	release.Do(func() { close(writer.release) })
	if err := epoch.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	writer.mu.Lock()
	calls, closes := writer.calls, writer.closes
	writer.mu.Unlock()
	if calls != 1 || closes != 1 {
		t.Fatalf("queued input/settlement calls=%d closes=%d", calls, closes)
	}
	// A stale generation sample must not create new failure authority.
	stale := controlmode.PaneWitness{Session: witness.Session, Window: witness.Window, Pane: witness.Pane, Incarnation: "stale"}
	registry.superviseFault(journalKey(stale))
	registry.mu.Lock()
	broken := registry.broken
	registry.mu.Unlock()
	if broken {
		t.Fatal("stale sample poisoned realm")
	}
}
