package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type boundaryFrameWriter struct {
	mu         sync.Mutex
	write      func(context.Context, Frame, int) error
	close      func(context.Context) error
	attempted  []Frame
	delivered  []Frame
	contexts   []context.Context
	closeCount int
	started    chan FrameType
}

func newBoundaryFrameWriter() *boundaryFrameWriter {
	return &boundaryFrameWriter{started: make(chan FrameType, MaxEgressFrames)}
}

func (w *boundaryFrameWriter) WriteFrame(ctx context.Context, raw []byte) error {
	frame, err := DecodeFrame(raw)
	if err != nil {
		return err
	}
	w.mu.Lock()
	index := len(w.attempted)
	w.attempted = append(w.attempted, frame)
	w.contexts = append(w.contexts, ctx)
	write := w.write
	w.mu.Unlock()
	w.started <- frame.Type
	if write != nil {
		err = write(ctx, frame, index)
	}
	if err == nil {
		w.mu.Lock()
		w.delivered = append(w.delivered, frame)
		w.mu.Unlock()
	}
	return err
}

func (w *boundaryFrameWriter) Close(ctx context.Context) error {
	w.mu.Lock()
	w.closeCount++
	closeFn := w.close
	w.mu.Unlock()
	if closeFn != nil {
		return closeFn(ctx)
	}
	return nil
}

func boundaryFrames() []Frame {
	return []Frame{
		{Version: ProtocolVersion, Type: FramePrepare, Source: "source", Epoch: 1, Cut: 1, Kind: CutInitial, Columns: 80, Rows: 24},
		{Version: ProtocolVersion, Type: FrameCommit, Source: "source", Epoch: 1, Cut: 1},
		{Version: ProtocolVersion, Type: FrameLive, Source: "source", Epoch: 1, Cut: 1, Data: []byte("live")},
		{Version: ProtocolVersion, Type: FrameEnd, Source: "source", Epoch: 1, Reason: "closed"},
	}
}

func assertEgressZero(t *testing.T, e *egress, w *boundaryFrameWriter) {
	t.Helper()
	e.mu.Lock()
	queued, bytes, inFlight := len(e.queue), e.queuedBytes, e.inFlight
	e.mu.Unlock()
	w.mu.Lock()
	closeCount := w.closeCount
	w.mu.Unlock()
	if queued != 0 || bytes != 0 || inFlight || closeCount != 1 {
		t.Fatalf("accounting queue=%d bytes=%d inFlight=%v close=%d", queued, bytes, inFlight, closeCount)
	}
}

func TestTerminalBoundaryEgressHealthyFullFIFO(t *testing.T) {
	w := newBoundaryFrameWriter()
	e, err := newEgress(w, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	frames := boundaryFrames()
	if err := e.enqueueBatch(frames); err != nil {
		t.Fatal(err)
	}
	if err := e.finishAndWait(); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	delivered := append([]Frame(nil), w.delivered...)
	contexts := append([]context.Context(nil), w.contexts...)
	w.mu.Unlock()
	if len(delivered) != len(frames) {
		t.Fatalf("delivered=%#v", delivered)
	}
	for i := range frames {
		if delivered[i].Type != frames[i].Type || contexts[i] != contexts[0] {
			t.Fatalf("FIFO/context mismatch at %d: %#v", i, delivered)
		}
	}
	assertEgressZero(t, e, w)
}

func TestTerminalBoundaryEgressAdmitsValidMaximumAndDeniesQueueOverflow(t *testing.T) {
	row := strings.Repeat("h", HistoryRowByteCap-1)
	frame := Frame{
		Version: ProtocolVersion, Type: FramePrepare, Source: "source", Epoch: 1, Cut: 1,
		Kind: CutInitial, Columns: 164, Rows: 38,
		History: make([]string, HistoryByteCap/(len(row)+1)),
		Replay:  []byte(strings.Repeat("r", ReplayByteCap)),
	}
	for i := range frame.History {
		frame.History[i] = row
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > MaxEgressQueueBytes {
		t.Fatalf("valid maximum PREPARE exceeds queue bound: wire=%d queue=%d", len(raw), MaxEgressQueueBytes)
	}

	release := make(chan struct{})
	w := newBoundaryFrameWriter()
	w.write = func(context.Context, Frame, int) error {
		<-release
		return nil
	}
	e, err := newEgress(w, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.enqueue(frame); err != nil {
		t.Fatalf("valid maximum PREPARE rejected: %v", err)
	}
	<-w.started
	admitted := 1
	for {
		err := e.enqueue(frame)
		if errors.Is(err, ErrSaturated) {
			break
		}
		if err != nil {
			t.Fatalf("unexpected queue admission error after %d frames: %v", admitted, err)
		}
		admitted++
	}
	want := MaxEgressQueueBytes / len(raw)
	if admitted != want {
		t.Fatalf("admitted=%d want=%d wire=%d queue=%d", admitted, want, len(raw), MaxEgressQueueBytes)
	}
	t.Logf("valid_max_wire_bytes=%d admitted_frames=%d next_admission=ErrSaturated queue_bytes=%d", len(raw), admitted, MaxEgressQueueBytes)
	close(release)
	if err := e.finishAndWait(); err != nil {
		t.Fatal(err)
	}
	assertEgressZero(t, e, w)
}

func TestTerminalBoundaryEgressBlockedPrepareDiscardsQueuedEND(t *testing.T) {
	writeErr := errors.New("prepare write failed")
	closeErr := errors.New("transport close failed")
	w := newBoundaryFrameWriter()
	w.write = func(ctx context.Context, _ Frame, _ int) error {
		<-ctx.Done()
		return writeErr
	}
	w.close = func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("close context is not bounded")
		}
		return closeErr
	}
	e, err := newEgress(w, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	frames := boundaryFrames()
	if err := e.enqueue(frames[0]); err != nil {
		t.Fatal(err)
	}
	if got := <-w.started; got != FramePrepare {
		t.Fatalf("first attempt=%s", got)
	}
	if err := e.enqueue(frames[3]); err != nil {
		t.Fatal(err)
	}
	err = e.finishAndWait()
	for _, want := range []string{writeErr.Error(), "egress drain deadline exceeded", closeErr.Error()} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
	w.mu.Lock()
	attempts := append([]Frame(nil), w.attempted...)
	w.mu.Unlock()
	if len(attempts) != 1 || attempts[0].Type != FramePrepare {
		t.Fatalf("queued END was not discarded after blocked PREPARE: %#v", attempts)
	}
	assertEgressZero(t, e, w)
}

func TestTerminalBoundaryEgressIdleThenBlockedEND(t *testing.T) {
	w := newBoundaryFrameWriter()
	w.write = func(ctx context.Context, _ Frame, _ int) error {
		<-ctx.Done()
		return ctx.Err()
	}
	e, err := newEgress(w, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.enqueue(boundaryFrames()[3]); err != nil {
		t.Fatal(err)
	}
	err = e.finishAndWait()
	if err == nil || !strings.Contains(err.Error(), "egress drain deadline exceeded") || !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked END result=%v", err)
	}
	assertEgressZero(t, e, w)
}

func TestTerminalBoundaryEgressCompletionRacesCancellationLevelTriggersNext(t *testing.T) {
	w := newBoundaryFrameWriter()
	w.write = func(ctx context.Context, _ Frame, index int) error {
		if index == 0 {
			<-ctx.Done()
			return nil
		}
		return ctx.Err()
	}
	e, err := newEgress(w, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	frames := boundaryFrames()
	if err := e.enqueueBatch([]Frame{frames[0], frames[3]}); err != nil {
		t.Fatal(err)
	}
	err = e.finishAndWait()
	if err == nil || !strings.Contains(err.Error(), "egress drain deadline exceeded") || !errors.Is(err, context.Canceled) {
		t.Fatalf("race result=%v", err)
	}
	w.mu.Lock()
	attempted := append([]Frame(nil), w.attempted...)
	delivered := append([]Frame(nil), w.delivered...)
	contexts := append([]context.Context(nil), w.contexts...)
	w.mu.Unlock()
	if len(attempted) != 2 || attempted[1].Type != FrameEnd || len(delivered) != 1 || contexts[0] != contexts[1] || contexts[1].Err() == nil {
		t.Fatalf("level cancellation not retained: attempted=%#v delivered=%#v contexts=%d", attempted, delivered, len(contexts))
	}
	assertEgressZero(t, e, w)
}

func TestTerminalBoundaryParentCancellationSuppressesGracefulEOF(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pending := []byte(markerWirePrefix[:9])
	e := &Epoch{ctx: ctx, ingress: ingressFramer{pending: append([]byte(nil), pending...)}}
	if cause := e.finalizeCauseLocked(); !errors.Is(cause, context.Canceled) {
		t.Fatalf("finalize cause=%v", cause)
	}
	if string(e.ingress.pending) != string(pending) {
		t.Fatalf("canceled finalization consumed pending ingress: %q", e.ingress.pending)
	}
}

func TestTerminalBoundaryFDWriterCancellationIsJoinedBeforeClose(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	writer, err := NewNonblockingFDWriter(w)
	if err != nil {
		t.Fatal(err)
	}

	chunk := make([]byte, 4096)
	for {
		_, err = syscall.Write(writer.targetFD, chunk)
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		_, err := writer.WriteContext(ctx, []byte("blocked"))
		returned <- err
	}()
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("blocked cancellation=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked writer did not observe cancellation")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Stat(); err != nil {
		t.Fatalf("writer closed source PTY: %v", err)
	}
}

func TestTerminalBoundaryFDWriterNoWakeAfterSuccessfulReturn(t *testing.T) {
	for iteration := 0; iteration < 128; iteration++ {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		writer, err := NewNonblockingFDWriter(w)
		if err != nil {
			r.Close()
			w.Close()
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		if n, err := writer.WriteContext(ctx, []byte("x")); n != 1 || err != nil {
			t.Fatalf("iteration %d write=(%d,%v)", iteration, n, err)
		}
		cancel()
		for yield := 0; yield < 8; yield++ {
			runtime.Gosched()
		}
		var wake [1]byte
		if n, err := syscall.Read(writer.wakeRFD, wake[:]); n != -1 || (!errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EWOULDBLOCK)) {
			t.Fatalf("iteration %d post-return wake=(%d,%v)", iteration, n, err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Stat(); err != nil {
			t.Fatalf("iteration %d source PTY closed: %v", iteration, err)
		}
		r.Close()
		w.Close()
	}
}
