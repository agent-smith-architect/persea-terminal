package broker

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

func admissionFrames(t *testing.T, buffer *bytes.Buffer) []terminal.Frame {
	t.Helper()
	var frames []terminal.Frame
	for buffer.Len() != 0 {
		raw, err := proto.ReadFrame(buffer)
		if err != nil {
			t.Fatal(err)
		}
		if raw.Type != proto.FrameAttachment {
			t.Fatalf("broker frame type %d, want attachment", raw.Type)
		}
		frame, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	return frames
}

func admissionCommitGeometry(t *testing.T, effects *UnifiedDevPaneEffects, key unifiedjournal.PaneKey, geometry unifiedjournal.Geometry) {
	t.Helper()
	record, err := effects.realm.AppendGeometry(key, geometry)
	if err != nil {
		t.Fatal(err)
	}
	if err := effects.realm.Sync(key); err != nil {
		t.Fatal(err)
	}
	if err := effects.realm.AdvanceCommitted(key, record); err != nil {
		t.Fatal(err)
	}
}

// admissionSmallEvents commits count output events of size bytes each, every
// one distinct, and returns their concatenation.
func admissionSmallEvents(t *testing.T, effects *UnifiedDevPaneEffects, key unifiedjournal.PaneKey, label string, count, size int) []byte {
	t.Helper()
	var all []byte
	for i := 0; i < count; i++ {
		payload := []byte(fmt.Sprintf("%s%0*d", label, size-len(label), i))
		all = append(all, payload...)
		b1Commit(t, effects, key, payload)
	}
	return all
}

// Admission must not grow with the history: PREPARE carries at most the
// admission budget, in whole events, and everything after it follows COMMIT
// as the fewest LIVE frames the frame bound allows, not one frame per event.
func TestUnifiedAdmissionReplayIsBoundedAndBacklogCoalesced(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	const count, size = 2000, 100
	want := admissionSmallEvents(t, effects, key, "e", count, size)
	wire := &admissionWire{}
	writer := &unifiedAttachmentFrameWriter{provider: effects, session: key.Session, downstream: &attachmentFrameWriter{wire: &lockedWriter{w: wire}}}
	defer writer.Close(context.Background())
	if err := admissionFrame(t, writer, terminal.FramePrepare); err != nil {
		t.Fatal(err)
	}
	if err := admissionFrame(t, writer, terminal.FrameCommit); err != nil {
		t.Fatal(err)
	}
	frames := admissionFrames(t, &wire.Buffer)
	if len(frames) < 2 || frames[0].Type != terminal.FramePrepare || frames[1].Type != terminal.FrameCommit {
		t.Fatalf("admission frames start %v", frames[:min(len(frames), 2)])
	}
	prefix := unifiedAdmissionReplayBytes / size * size
	if len(frames[0].Replay) != prefix {
		t.Fatalf("PREPARE replay=%d bytes, want the %d-byte whole-event prefix of the %d-byte budget", len(frames[0].Replay), prefix, unifiedAdmissionReplayBytes)
	}
	visible := append([]byte(nil), frames[0].Replay...)
	for _, frame := range frames[2:] {
		if frame.Type != terminal.FrameLive {
			t.Fatalf("backlog frame %s, want LIVE", frame.Type)
		}
		visible = append(visible, frame.Data...)
	}
	if !bytes.Equal(visible, want) {
		t.Fatalf("admission reconstructs %d bytes, want the %d committed bytes in order", len(visible), len(want))
	}
	backlog := len(want) - prefix
	if lives, wantLives := len(frames)-2, (backlog+unifiedLiveFrameBytes-1)/unifiedLiveFrameBytes; lives != wantLives {
		t.Fatalf("a %d-byte backlog of %d-byte events took %d LIVE frames, want %d", backlog, size, lives, wantLives)
	}
}

// Coalescing never moves a committed geometry: the output run before it is
// sent first, and the output after it starts a new frame.
func TestUnifiedAdmissionBacklogKeepsGeometryBetweenOutput(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	const size = 100
	// Half a frame of backlog before the geometry and a quarter frame after:
	// without the geometry, one LIVE frame would carry both.
	before := admissionSmallEvents(t, effects, key, "b", unifiedAdmissionReplayBytes/size+unifiedLiveFrameBytes/size/2, size)
	admissionCommitGeometry(t, effects, key, unifiedjournal.Geometry{Columns: 80, Rows: 30})
	after := admissionSmallEvents(t, effects, key, "a", unifiedLiveFrameBytes/size/4, size)
	wire := &admissionWire{}
	writer := &unifiedAttachmentFrameWriter{provider: effects, session: key.Session, downstream: &attachmentFrameWriter{wire: &lockedWriter{w: wire}}}
	defer writer.Close(context.Background())
	if err := admissionFrame(t, writer, terminal.FramePrepare); err != nil {
		t.Fatal(err)
	}
	if err := admissionFrame(t, writer, terminal.FrameCommit); err != nil {
		t.Fatal(err)
	}
	frames := admissionFrames(t, &wire.Buffer)
	prefix := unifiedAdmissionReplayBytes / size * size
	if len(before)-prefix+len(after) > unifiedLiveFrameBytes {
		t.Fatalf("fixture: %d bytes of backlog around the geometry exceed one %d-byte LIVE frame", len(before)-prefix+len(after), unifiedLiveFrameBytes)
	}
	if len(frames) != 5 || frames[0].Type != terminal.FramePrepare || frames[1].Type != terminal.FrameCommit ||
		frames[2].Type != terminal.FrameLive || frames[3].Type != terminal.FramePrepare || frames[3].Kind != terminal.CutResize || frames[4].Type != terminal.FrameLive {
		kinds := make([]string, len(frames))
		for i, frame := range frames {
			kinds[i] = string(frame.Type) + "/" + string(frame.Kind)
		}
		t.Fatalf("frames=%v, want PREPARE, COMMIT, LIVE, PREPARE/RESIZE, LIVE", kinds)
	}
	if !bytes.Equal(frames[0].Replay, before[:prefix]) || !bytes.Equal(frames[2].Data, before[prefix:]) ||
		frames[3].Columns != 80 || frames[3].Rows != 30 || !bytes.Equal(frames[4].Data, after) {
		t.Fatalf("geometry moved: replay=%d live=%d resize=%dx%d live=%d; want %d, %d, 80x30, %d",
			len(frames[0].Replay), len(frames[2].Data), frames[3].Columns, frames[3].Rows, len(frames[4].Data), prefix, len(before)-prefix, len(after))
	}
}

// steppedWire holds every LIVE write until the test lets it through, so a
// frame another writer has ready can take any gap between backlog frames. It
// records each frame's type as written; once the test is done, late writes
// pass without holding so teardown cannot wedge on a writer that went wrong.
type steppedWire struct {
	mu      sync.Mutex
	order   []terminal.FrameType
	pending bool
	parked  chan struct{}
	resume  chan struct{}
	done    chan struct{}
}

func (w *steppedWire) Write(p []byte) (int, error) {
	w.mu.Lock()
	header := !w.pending
	w.pending = header
	w.mu.Unlock()
	if header {
		return len(p), nil
	}
	frame, err := attachmentwire.Decode(p, attachmentwire.ServerToBrowser)
	if err != nil {
		return 0, err
	}
	if frame.Type == terminal.FrameLive {
		select {
		case w.parked <- struct{}{}:
			<-w.resume
		case <-w.done:
		}
	}
	w.mu.Lock()
	w.order = append(w.order, frame.Type)
	w.mu.Unlock()
	return len(p), nil
}

func (w *steppedWire) written() []terminal.FrameType {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]terminal.FrameType(nil), w.order...)
}

// Input authority follows catch-up. The epoch's egress calls the writer for
// COMMIT and then for the CONTROL grant, one after the other; COMMIT must not
// return before the whole backlog is written, or the grant, and with it the
// page's input, would overtake history the page has not received yet.
func TestUnifiedAdmissionModeFollowsBacklog(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	admissionSmallEvents(t, effects, key, "m", 4*unifiedLiveFrameBytes/100, 100)
	wire := &steppedWire{parked: make(chan struct{}), resume: make(chan struct{}), done: make(chan struct{})}
	writer := &unifiedAttachmentFrameWriter{provider: effects, session: key.Session, downstream: &attachmentFrameWriter{wire: &lockedWriter{w: wire}}}
	defer writer.Close(context.Background())
	if err := admissionFrame(t, writer, terminal.FramePrepare); err != nil {
		t.Fatal(err)
	}
	pumped := make(chan error, 1)
	go func() {
		if err := admissionFrame(t, writer, terminal.FrameCommit); err != nil {
			pumped <- err
			return
		}
		mode := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameMode, Source: "admission", Epoch: 1, Mode: terminal.ModeControl}
		pumped <- writer.WriteFrame(context.Background(), unifiedE2E1RawFrame(t, mode))
	}()
	for finished := false; !finished; {
		select {
		case <-wire.parked:
			wire.resume <- struct{}{}
		case err := <-pumped:
			if err != nil {
				t.Fatal(err)
			}
			finished = true
		case <-time.After(5 * time.Second):
			t.Fatal("admission stalled")
		}
	}
	order := wire.written()
	close(wire.done)
	lives := 0
	for _, kind := range order {
		if kind == terminal.FrameLive {
			lives++
		}
	}
	if lives < 3 || order[len(order)-1] != terminal.FrameMode {
		t.Fatalf("frames=%v, want every backlog LIVE frame before MODE", order)
	}
}
