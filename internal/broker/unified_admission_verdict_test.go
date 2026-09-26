package broker

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

// The gate acknowledges the exact frame being written. Ending the attachment
// does not pretend that a write which still references its snapshot settled.
type admissionWire struct {
	bytes.Buffer
	park             terminal.FrameType
	entered, release chan struct{}
	once             sync.Once
}

func (w *admissionWire) Write(p []byte) (int, error) {
	if frame, err := attachmentwire.Decode(p, attachmentwire.ServerToBrowser); err == nil && frame.Type == w.park {
		w.once.Do(func() { close(w.entered); <-w.release })
	}
	return w.Buffer.Write(p)
}

func admissionFrame(t *testing.T, writer *unifiedAttachmentFrameWriter, kind terminal.FrameType) error {
	t.Helper()
	f := terminal.Frame{Version: terminal.ProtocolVersion, Type: kind, Source: "admission", Epoch: 1, Cut: 1}
	if kind == terminal.FramePrepare {
		f.Kind, f.Columns, f.Rows = terminal.CutInitial, 80, 24
	}
	return writer.WriteFrame(context.Background(), unifiedE2E1RawFrame(t, f))
}

func admissionTypes(t *testing.T, wire *admissionWire) []terminal.FrameType {
	t.Helper()
	var types []terminal.FrameType
	for wire.Len() != 0 {
		frame, err := proto.ReadFrame(&wire.Buffer)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == proto.FrameControl {
			control, err := proto.DecodeControl(frame.Payload)
			if err != nil || control.Code != string(proto.SubscriberClosedLagged) {
				t.Fatalf("control=%+v err=%v", control, err)
			}
			types = append(types, "verdict")
		} else {
			decoded, err := attachmentwire.Decode(frame.Payload, attachmentwire.ServerToBrowser)
			if err != nil {
				t.Fatal(err)
			}
			types = append(types, decoded.Type)
		}
	}
	return types
}

func TestUnifiedAdmissionEvictionPrecedesCommit(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	for attempt := 0; attempt < 3; attempt++ {
		wire := &admissionWire{}
		writer := &unifiedAttachmentFrameWriter{provider: effects, session: key.Session, downstream: &attachmentFrameWriter{wire: &lockedWriter{w: wire}}}
		if err := admissionFrame(t, writer, terminal.FramePrepare); err != nil {
			t.Fatal(err)
		}
		for i := 0; i <= recordingTailBytes/(64<<10); i++ {
			b1Commit(t, effects, key, bytes.Repeat([]byte{'x'}, 64<<10))
		}
		if writer.tail.closeLimit() != tailLimitQueueBytes {
			t.Fatal(writer.tail.closeLimit())
		}
		err := admissionFrame(t, writer, terminal.FrameCommit)
		if !errors.Is(err, terminal.ErrClosed) {
			t.Errorf("known eviction COMMIT returned %v", err)
		}
		select {
		case <-writer.ended:
		case <-time.After(5 * time.Second):
			t.Fatal("verdict did not finish")
		}
		types := admissionTypes(t, wire)
		if len(types) != 2 || types[0] != terminal.FramePrepare || types[1] != "verdict" {
			t.Fatalf("known eviction published %v", types)
		}
		_ = writer.Close(context.Background())
	}
}

func TestUnifiedAdmissionVerdictCutsParkedWrites(t *testing.T) {
	for _, phase := range []terminal.FrameType{terminal.FramePrepare, terminal.FrameCommit, terminal.FrameLive} {
		t.Run(string(phase), func(t *testing.T) {
			effects, key := recordingReaderFixture(t, 512<<10)
			wire := &admissionWire{park: phase, entered: make(chan struct{}), release: make(chan struct{})}
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(wire.release) }) }
			defer unblock()
			ended := make(chan struct{})
			writer := &unifiedAttachmentFrameWriter{provider: effects, session: key.Session, downstream: &attachmentFrameWriter{wire: &lockedWriter{w: wire}}, end: func() { close(ended) }}
			done := make(chan error, 1)
			go func() {
				err := admissionFrame(t, writer, terminal.FramePrepare)
				if err == nil && phase != terminal.FramePrepare {
					err = admissionFrame(t, writer, terminal.FrameCommit)
				}
				done <- err
			}()
			select {
			case <-wire.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("write did not park")
			}
			before := effects.readers.snapshot()
			effects.subscriberMu.Lock()
			for tail := range effects.subscribers[key] {
				effects.closeLaggedSubscriberLocked(key, tail, tailLimitQueueBytes)
			}
			effects.subscriberMu.Unlock()
			select {
			case <-ended:
			case <-time.After(unifiedSubscriberCloseGrace + time.Second):
				t.Error("parked admission had no bounded verdict watcher")
			}
			if got := effects.readers.snapshot(); got.Bytes != before.Bytes || got.Snapshots != 1 {
				t.Errorf("parked write lost ownership: %+v -> %+v", before, got)
			}
			unblock()
			<-done
			_ = writer.Close(context.Background())
			if got := effects.readers.snapshot(); got.Bytes != 0 {
				t.Fatal(got)
			}
		})
	}
}

func TestUnifiedAdmissionVerdictStopsBacklog(t *testing.T) {
	effects, key := recordingReaderFixture(t, 512<<10)
	b1Commit(t, effects, key, []byte("later backlog"))
	wire := &admissionWire{park: terminal.FrameLive, entered: make(chan struct{}), release: make(chan struct{})}
	writer := &unifiedAttachmentFrameWriter{provider: effects, session: key.Session, downstream: &attachmentFrameWriter{wire: &lockedWriter{w: wire}}}
	if err := admissionFrame(t, writer, terminal.FramePrepare); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- admissionFrame(t, writer, terminal.FrameCommit) }()
	select {
	case <-wire.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("backlog did not park")
	}
	effects.closeSubscribers(key, proto.SubscriberClosedLagged)
	close(wire.release)
	if err := <-done; !errors.Is(err, terminal.ErrClosed) {
		t.Errorf("evicted backlog returned %v", err)
	}
	select {
	case <-writer.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("verdict did not finish")
	}
	types := admissionTypes(t, wire)
	// The first backlog frame is already in flight; the next must not start,
	// even though it would continue the same snapshot event.
	if len(types) != 4 || types[2] != terminal.FrameLive || types[3] != "verdict" {
		t.Fatalf("backlog frames=%v, want PREPARE, COMMIT, 1 chunk, verdict", types)
	}
	_ = writer.Close(context.Background())
}
