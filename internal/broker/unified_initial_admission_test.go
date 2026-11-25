package broker

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

type initialAdmissionWire struct {
	mu          sync.Mutex
	buf         bytes.Buffer
	failWrites  int
	writeErr    error
	writeCalls  int
	attachCount int
	controls    []proto.Control
}

func (w *initialAdmissionWire) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeCalls++
	if w.writeErr != nil && w.writeCalls > w.failWrites {
		return 0, w.writeErr
	}
	return w.buf.Write(p)
}

func (w *initialAdmissionWire) decodeAll(t *testing.T) (attachments []terminal.FrameType, controls []proto.Control) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	raw := bytes.NewReader(w.buf.Bytes())
	for raw.Len() != 0 {
		frame, err := proto.ReadFrame(raw)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch frame.Type {
		case proto.FrameControl:
			ctrl, err := proto.DecodeControl(frame.Payload)
			if err != nil {
				t.Fatalf("decode control: %v", err)
			}
			controls = append(controls, ctrl)
		case proto.FrameAttachment:
			decoded, err := attachmentwire.Decode(frame.Payload, attachmentwire.ServerToBrowser)
			if err != nil {
				t.Fatalf("decode attachment: %v", err)
			}
			attachments = append(attachments, decoded.Type)
		default:
			t.Fatalf("unexpected frame type %d", frame.Type)
		}
	}
	return attachments, controls
}

func TestUnifiedInitialAdmissionSuppressesPrePrepareEnd(t *testing.T) {
	wire := &initialAdmissionWire{}
	endCalls := 0
	failLogs := []string{}
	writer := &unifiedAttachmentFrameWriter{
		downstream:       &attachmentFrameWriter{wire: &lockedWriter{w: wire}},
		onInitialFailure: func(code string) { failLogs = append(failLogs, code) },
		end:              func() { endCalls++ },
	}
	fence, err := writer.beginResize()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate egress winning the scheduling race and writing END before
	// PREPARE was ever published on the wire.
	endFrame := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameEnd, Source: "admission", Epoch: 1, Reason: "fault"}
	if err := writer.WriteFrame(context.Background(), unifiedE2E1RawFrame(t, endFrame)); err != nil {
		t.Fatalf("pre-prepare END returned error: %v", err)
	}
	select {
	case <-fence.done:
	default:
		t.Fatal("pre-prepare END must settle pending resize fence")
	}
	writer.failBeforePrepare("attach_failed")
	// Second invocation must be a no-op (single fatal control owner).
	writer.failBeforePrepare("attach_failed")

	attachments, controls := wire.decodeAll(t)
	if len(attachments) != 0 {
		t.Fatalf("pre-prepare END crossed the wire: attachments=%v", attachments)
	}
	if len(controls) != 1 || controls[0].Type != "error" || controls[0].Code != "attach_failed" {
		t.Fatalf("controls=%+v, want single error(attach_failed)", controls)
	}
	if endCalls != 1 {
		t.Fatalf("endCalls=%d, want 1", endCalls)
	}
	if len(failLogs) != 1 || failLogs[0] != "attach_failed" {
		t.Fatalf("failLogs=%v, want [attach_failed]", failLogs)
	}
}

func TestUnifiedInitialAdmissionOpenStreamFailureClosesCleanly(t *testing.T) {
	effects, _ := recordingReaderFixture(t, 0)
	wire := &initialAdmissionWire{}
	endCalls := 0
	failLogs := []string{}
	writer := &unifiedAttachmentFrameWriter{
		provider:         effects,
		session:          "$missing-session",
		downstream:       &attachmentFrameWriter{wire: &lockedWriter{w: wire}},
		onInitialFailure: func(code string) { failLogs = append(failLogs, code) },
		end:              func() { endCalls++ },
	}
	if err := admissionFrame(t, writer, terminal.FramePrepare); err == nil {
		t.Fatal("openStream for missing session unexpectedly succeeded")
	}
	attachments, controls := wire.decodeAll(t)
	if len(attachments) != 0 {
		t.Fatalf("attachments=%v, want none", attachments)
	}
	if len(controls) != 1 || controls[0].Type != "error" || controls[0].Code != "attach_failed" {
		t.Fatalf("controls=%+v, want single error(attach_failed)", controls)
	}
	if endCalls != 1 {
		t.Fatalf("endCalls=%d, want 1", endCalls)
	}
	if len(failLogs) != 1 || failLogs[0] != "attach_failed" {
		t.Fatalf("failLogs=%v, want [attach_failed]", failLogs)
	}
	if got := effects.readers.snapshot(); got.Bytes != 0 || got.Snapshots != 0 {
		t.Fatalf("reader accounting leaked after openStream failure: %+v", got)
	}
}

func TestUnifiedInitialAdmissionBrokenPrepareWireClosesWithoutControl(t *testing.T) {
	effects, key := recordingReaderFixture(t, 4<<10)
	wire := &initialAdmissionWire{failWrites: 0, writeErr: errors.New("broken pipe")}
	endCalls := 0
	failLogs := []string{}
	writer := &unifiedAttachmentFrameWriter{
		provider:         effects,
		session:          key.Session,
		downstream:       &attachmentFrameWriter{wire: &lockedWriter{w: wire}},
		onInitialFailure: func(code string) { failLogs = append(failLogs, code) },
		end:              func() { endCalls++ },
	}
	if err := admissionFrame(t, writer, terminal.FramePrepare); err == nil {
		t.Fatal("broken PREPARE wire unexpectedly succeeded")
	}
	if endCalls != 1 {
		t.Fatalf("endCalls=%d, want 1", endCalls)
	}
	if len(failLogs) != 0 {
		t.Fatalf("broken wire must not send an attach_failed control: failLogs=%v", failLogs)
	}
	// Only the single failed PREPARE write attempt should have hit the broken wire.
	wire.mu.Lock()
	calls := wire.writeCalls
	wire.mu.Unlock()
	if calls != 1 {
		t.Fatalf("writeCalls=%d, want 1 (no control write after broken transport)", calls)
	}
	_ = writer.Close(context.Background())
	if got := effects.readers.snapshot(); got.Bytes != 0 || got.Snapshots != 0 {
		t.Fatalf("reader accounting leaked after broken PREPARE write: %+v", got)
	}
}

func TestUnifiedInitialAdmissionDeliversPostPrepareEnd(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	wire := &initialAdmissionWire{}
	writer := &unifiedAttachmentFrameWriter{
		provider:   effects,
		session:    key.Session,
		downstream: &attachmentFrameWriter{wire: &lockedWriter{w: wire}},
	}
	defer writer.Close(context.Background())
	if err := admissionFrame(t, writer, terminal.FramePrepare); err != nil {
		t.Fatalf("PREPARE failed: %v", err)
	}
	endFrame := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameEnd, Source: "admission", Epoch: 1, Reason: "closed"}
	if err := writer.WriteFrame(context.Background(), unifiedE2E1RawFrame(t, endFrame)); err != nil {
		t.Fatalf("post-prepare END failed: %v", err)
	}
	attachments, controls := wire.decodeAll(t)
	if len(controls) != 0 {
		t.Fatalf("unexpected controls=%+v", controls)
	}
	if len(attachments) != 2 || attachments[0] != terminal.FramePrepare || attachments[1] != terminal.FrameEnd {
		t.Fatalf("attachments=%v, want [PREPARE END]", attachments)
	}
}
