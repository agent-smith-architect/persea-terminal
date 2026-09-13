package terminal

import (
	"context"
	"testing"
	"time"
)

type recordingOutputTransport struct{ frames chan Frame }

func (*recordingOutputTransport) OwnsTerminalOutput() {}
func (w *recordingOutputTransport) WriteFrame(_ context.Context, raw []byte) error {
	f, err := DecodeFrame(raw)
	if err == nil {
		w.frames <- f
	}
	return err
}
func (*recordingOutputTransport) Close(context.Context) error { return nil }

func TestRecordingWriterOutputBypassesLegacyLiveAllocationAndQueue(t *testing.T) {
	w := &recordingOutputTransport{frames: make(chan Frame, 1)}
	e, err := newEgress(w, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer e.finishAndWait()
	f := Frame{Version: 1, Type: FrameLive, Source: "recording", Epoch: 1, Cut: 1, Data: make([]byte, MaxEgressBytes)}
	if allocations := testing.AllocsPerRun(100, func() {
		if err := e.enqueue(f); err != nil {
			t.Fatal(err)
		}
	}); allocations != 0 {
		t.Fatalf("unused live output allocated %.0f objects", allocations)
	}
	e.mu.Lock()
	queued, bytes, flight := len(e.queue), e.queuedBytes, e.inFlight
	e.mu.Unlock()
	if queued != 0 || bytes != 0 || flight {
		t.Fatal("unused live output entered egress")
	}
	if err := e.enqueue(Frame{Version: 1, Type: FrameMode, Source: "recording", Epoch: 1, Mode: ModeObserve}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-w.frames:
		if got.Type != FrameMode {
			t.Fatal(got.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("control frame was suppressed")
	}
}
