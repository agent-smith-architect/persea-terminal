package terminal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// This writer deliberately holds ownership after cancellation, then returns a
// successful partial write. Cancellation cannot be treated as settlement.
type sealHeldWriter struct {
	entered       chan context.Context
	release       chan struct{}
	mu            sync.Mutex
	calls, closes int
}

func (w *sealHeldWriter) WriteContext(ctx context.Context, data []byte) (int, error) {
	w.mu.Lock()
	w.calls++
	call := w.calls
	w.mu.Unlock()
	if call == 1 {
		w.entered <- ctx
		<-w.release
		return 1, nil
	}
	return len(data), nil
}

func (w *sealHeldWriter) Close() error {
	w.mu.Lock()
	w.closes++
	w.mu.Unlock()
	return nil
}

type sealTransaction struct {
	epoch   *Epoch
	witness SourceWitness
}

func (s *sealTransaction) Witness(_ context.Context, pid int) (ProcessWitness, error) {
	if pid == s.witness.Server.PID {
		return s.witness.Server, nil
	}
	return s.witness.Pane, nil
}

func (s *sealTransaction) RunPinned(_ context.Context, req TransactionRequest) (TransactionResult, error) {
	if req.Action == ActionCut {
		if err := s.epoch.PTYBytes([]byte("\x1b]52;;" + string(req.Marker) + "\a")); err != nil {
			return TransactionResult{}, err
		}
	}
	return TransactionResult{Witness: s.witness, Attachment: AttachmentIDs{ShadowSessionID: "$seal", ClientID: "seal-client"}}, nil
}

func TestEpochTerminalPublicationSealsHeldInputBeforeTeardown(t *testing.T) {
	w := &sealHeldWriter{entered: make(chan context.Context, 1), release: make(chan struct{})}
	tx := &sealTransaction{witness: SourceWitness{
		Incarnation: "seal-source", Socket: SocketIdentity{Path: "/private/seal.sock", Device: 1, Inode: 2},
		Server: ProcessWitness{PID: 101, StartTime: 1}, Pane: ProcessWitness{PID: 102, StartTime: 2},
		SessionID: "$1", WindowID: "@1", PaneID: "%1", Columns: 80, Rows: 24,
	}}
	source, err := NewPinnedSource(tx.witness, tx, tx)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEpoch(context.Background(), source, 1, newBoundaryFrameWriter(), w, Config{
		Clock: RealClock{}, QuietInterval: time.Second, MaximumInterval: 2 * time.Second, CutTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	tx.epoch = e
	var releaseWrite sync.Once
	defer func() { releaseWrite.Do(func() { close(w.release) }); _ = e.Finalize(context.Background()) }()
	if err := e.Start(context.Background(), CutInitial); err != nil {
		t.Fatal(err)
	}
	if err := e.HandleFrame(Frame{Version: ProtocolVersion, Type: FrameReady, Source: "seal-source", Epoch: 1, Cut: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.HandleFrame(Frame{Version: ProtocolVersion, Type: FrameModeRequest, Source: "seal-source", Epoch: 1, Mode: ModeControl}); err != nil {
		t.Fatal(err)
	}
	input := func(data string) error {
		return e.HandleFrame(Frame{Version: ProtocolVersion, Type: FrameInput, Source: "seal-source", Epoch: 1, Data: []byte(data)})
	}
	if err := input("active"); err != nil {
		t.Fatal(err)
	}
	var writeCtx context.Context
	select {
	case writeCtx = <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("active write did not start")
	}
	if err := input("queued"); err != nil {
		t.Fatal(err)
	}

	// Hold teardown at its first operation; terminal publication must seal the
	// pump independently of this lifecycle owner making progress.
	hold, entered := make(chan struct{}), make(chan struct{})
	go e.scheduler.stopOnce.Do(func() { close(entered); <-hold; _ = e.scheduler.command('s', false) })
	<-entered
	var releaseStop sync.Once
	defer releaseStop.Do(func() { close(hold) })
	cause := errors.New("recording stalled")
	faulted := make(chan struct{})
	go func() { _ = e.Fault(cause); close(faulted) }()
	select {
	case <-faulted:
	case <-time.After(time.Second):
		t.Fatal("Fault waited for held writer or teardown")
	}
	select {
	case <-e.Done():
	default:
		t.Fatal("terminal outcome not published")
	}
	if !errors.Is(e.Err(), cause) {
		t.Fatalf("terminal cause=%v", e.Err())
	}
	if err := input("future"); !errors.Is(err, ErrClosed) {
		t.Fatalf("future input=%v", err)
	}
	if writeCtx.Err() == nil {
		t.Error("terminal publication did not cancel active input")
	}
	e.input.mu.Lock()
	closed, queued, bytes := e.input.closed, len(e.input.queue), e.input.bytes
	e.input.mu.Unlock()
	if !closed || queued != 0 || bytes != len("active") {
		t.Errorf("sealed=%v queued=%d bytes=%d", closed, queued, bytes)
	}
	select {
	case <-e.input.done:
		t.Error("active writer ownership released early")
	default:
	}
	w.mu.Lock()
	closes := w.closes
	w.mu.Unlock()
	if closes != 0 {
		t.Errorf("writer closed while active: %d", closes)
	}
	releaseStop.Do(func() { close(hold) })
	releaseWrite.Do(func() { close(w.release) })
	if err := e.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	calls, closes := w.calls, w.closes
	w.mu.Unlock()
	if calls != 1 || closes != 1 {
		t.Errorf("write calls=%d close calls=%d", calls, closes)
	}
	e.input.mu.Lock()
	bytes = e.input.bytes
	e.input.mu.Unlock()
	if bytes != 0 {
		t.Errorf("settled input bytes=%d", bytes)
	}
}
