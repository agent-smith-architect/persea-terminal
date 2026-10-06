package broker

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

// resumeFixture commits a stream that exercises every frame boundary: small
// records coalesced into one LIVE frame, a record larger than a frame, and
// geometry between output and back to back.
func resumeFixture(t *testing.T) (*UnifiedDevPaneEffects, unifiedjournal.PaneKey) {
	t.Helper()
	effects, key := recordingReaderFixture(t, 0)
	admissionSmallEvents(t, effects, key, "a", unifiedAdmissionReplayBytes/100+40, 100)
	admissionCommitGeometry(t, effects, key, unifiedjournal.Geometry{Columns: 90, Rows: 30})
	b1Commit(t, effects, key, bytes.Repeat([]byte("B"), 2*unifiedLiveFrameBytes+500))
	admissionCommitGeometry(t, effects, key, unifiedjournal.Geometry{Columns: 100, Rows: 31})
	admissionCommitGeometry(t, effects, key, unifiedjournal.Geometry{Columns: 101, Rows: 32})
	admissionSmallEvents(t, effects, key, "c", 30, 70)
	return effects, key
}

// resumeAttach admits one attachment, resuming from resume when it is set,
// and returns its frames once the last one reaches the committed frontier.
func resumeAttach(t *testing.T, effects *UnifiedDevPaneEffects, key unifiedjournal.PaneKey, resume *proto.Resume) []terminal.Frame {
	t.Helper()
	wire := &lockedWriter{w: &bytes.Buffer{}}
	writer := &unifiedAttachmentFrameWriter{provider: effects, session: key.Session, downstream: &attachmentFrameWriter{wire: wire}, resume: resume}
	defer writer.Close(context.Background())
	if err := admissionFrame(t, writer, terminal.FramePrepare); err != nil {
		t.Fatal(err)
	}
	if err := admissionFrame(t, writer, terminal.FrameCommit); err != nil {
		t.Fatal(err)
	}
	effects.journalMu.Lock()
	frontier := effects.realm.CommittedSequence(key)
	effects.journalMu.Unlock()
	var frames []terminal.Frame
	deadline := time.Now().Add(5 * time.Second)
	for {
		wire.mu.Lock()
		copied := bytes.NewBuffer(append([]byte(nil), wire.w.(*bytes.Buffer).Bytes()...))
		wire.mu.Unlock()
		frames = admissionFrames(t, copied)
		reached := false
		for _, frame := range frames {
			reached = reached || frame.Position != nil && int64(frame.Position.Sequence) == frontier
		}
		if reached {
			break
		}
		if time.Now().After(deadline) {
			for _, f := range frames {
				t.Logf("frame %s/%s data=%d replay=%d pos=%+v resumed=%v", f.Type, f.Kind, len(f.Data), len(f.Replay), f.Position, f.Resumed)
			}
			t.Fatalf("attachment did not reach frontier %d; tail reason=%q", frontier, writer.tail.closeReason())
		}
		time.Sleep(5 * time.Millisecond)
	}
	return frames
}

// resumeView is what a page shows after applying frames: output bytes, with
// each committed geometry marked where it applies.
func resumeView(frames []terminal.Frame) string {
	var view []byte
	for _, frame := range frames {
		switch {
		case frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutResize:
			view = fmt.Appendf(view, "<%dx%d>", frame.Columns, frame.Rows)
		case frame.Type == terminal.FramePrepare:
			view = append(view, frame.Replay...)
		case frame.Type == terminal.FrameLive:
			view = append(view, frame.Data...)
		}
	}
	return string(view)
}

// A page that resumes from the position after ANY frame it applied — inside a
// record, between coalesced records, before or after a geometry — sees exactly
// what a page that never dropped sees: the resumed attachment sends only what
// follows that position, admitted as a snapshot, at the geometry in force
// there.
func TestUnifiedResumeContinuesFromEveryFramePosition(t *testing.T) {
	effects, key := resumeFixture(t)
	full := resumeAttach(t, effects, key, nil)
	if full[0].Resumed || !terminal.ValidStream(full[0].Stream) {
		t.Fatalf("first admission resumed=%v stream=%q", full[0].Resumed, full[0].Stream)
	}
	want := resumeView(full)
	columns, rows := full[0].Columns, full[0].Rows
	split := false
	for index, frame := range full {
		if frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutResize {
			columns, rows = frame.Columns, frame.Rows
		}
		if frame.Type == terminal.FrameCommit {
			continue
		}
		if frame.Position == nil {
			t.Fatalf("frame %d (%s) carries no position", index, frame.Type)
		}
		at := *frame.Position
		events, err := effects.realm.ReadCommittedEvents(key)
		if err != nil {
			t.Fatal(err)
		}
		if at.Sequence < uint64(len(events)) && int64(at.Offset) > events[at.Sequence].Start {
			split = true
		}
		resumed := resumeAttach(t, effects, key, &proto.Resume{Stream: full[0].Stream, Sequence: int64(at.Sequence), Offset: int64(at.Offset)})
		head := resumed[0]
		if !head.Resumed || head.Stream != full[0].Stream || head.Columns != columns || head.Rows != rows {
			t.Fatalf("resume after frame %d at %+v: PREPARE resumed=%v geometry=%dx%d, want geometry %dx%d",
				index, at, head.Resumed, head.Columns, head.Rows, columns, rows)
		}
		if len(head.Replay) == 0 && *head.Position != at {
			t.Fatalf("resume after frame %d at %+v: an empty replay is at %+v", index, at, head.Position)
		}
		if got := resumeView(full[:index+1]) + resumeView(resumed); got != want {
			t.Fatalf("resume after frame %d at %+v shows %d bytes, want the %d-byte view", index, at, len(got), len(want))
		}
	}
	if !split {
		t.Fatal("fixture: no frame ended inside a record")
	}
}

// A resume that does not name a position in the current stream replays the
// whole stream, as a first attach does.
func TestUnifiedResumeFallsBackToTheWholeStream(t *testing.T) {
	effects, key := resumeFixture(t)
	full := resumeAttach(t, effects, key, nil)
	want := resumeView(full)
	events, err := effects.realm.ReadCommittedEvents(key)
	if err != nil {
		t.Fatal(err)
	}
	var big unifiedjournal.Event
	for _, event := range events {
		if event.End-event.Start > unifiedLiveFrameBytes {
			big = event
		}
	}
	if big.Sequence < 2 {
		t.Fatal("fixture: no record larger than a frame")
	}
	stream := full[0].Stream
	other := "A" + stream[1:]
	if other == stream {
		other = "B" + stream[1:]
	}
	for name, resume := range map[string]proto.Resume{
		"another stream":              {Stream: other, Sequence: 1, Offset: events[0].End},
		"past the frontier":           {Stream: stream, Sequence: int64(len(events)) + 1, Offset: events[len(events)-1].End},
		"offset before its record":    {Stream: stream, Sequence: big.Sequence, Offset: big.End - 1},
		"offset past the next record": {Stream: stream, Sequence: big.Sequence - 1, Offset: big.End},
	} {
		got := resumeAttach(t, effects, key, &resume)
		if got[0].Resumed || got[0].Columns != 80 || got[0].Rows != 24 || resumeView(got) != want {
			t.Fatalf("%s: resumed=%v geometry=%dx%d view=%d bytes, want the whole %d-byte stream", name, got[0].Resumed, got[0].Columns, got[0].Rows, len(resumeView(got)), len(want))
		}
	}
}

// A resumed attachment admits what the page missed as its snapshot: by the
// time COMMIT returns — before the page can ask for MODE — every missed byte
// is written, however many frames it takes. A quiet session's resume sends
// nothing.
func TestUnifiedResumedSuffixIsWrittenBeforeCommitReturns(t *testing.T) {
	effects, key := resumeFixture(t)
	full := resumeAttach(t, effects, key, nil)
	effects.journalMu.Lock()
	frontier := effects.realm.CommittedSequence(key)
	effects.journalMu.Unlock()
	for name, at := range map[string]terminal.StreamPosition{"start": {}, "frontier": *full[len(full)-1].Position} {
		wire := &lockedWriter{w: &bytes.Buffer{}}
		writer := &unifiedAttachmentFrameWriter{provider: effects, session: key.Session, downstream: &attachmentFrameWriter{wire: wire},
			resume: &proto.Resume{Stream: full[0].Stream, Sequence: int64(at.Sequence), Offset: int64(at.Offset)}}
		for _, kind := range []terminal.FrameType{terminal.FramePrepare, terminal.FrameCommit} {
			if err := admissionFrame(t, writer, kind); err != nil {
				t.Fatal(err)
			}
		}
		wire.mu.Lock()
		frames := admissionFrames(t, bytes.NewBuffer(append([]byte(nil), wire.w.(*bytes.Buffer).Bytes()...)))
		wire.mu.Unlock()
		writer.Close(context.Background())
		reached, output := false, 0
		for _, frame := range frames {
			reached = reached || frame.Position != nil && int64(frame.Position.Sequence) == frontier
			output += len(frame.Replay) + len(frame.Data)
		}
		if !frames[0].Resumed || !reached {
			t.Fatalf("%s: resumed=%v; the frames written before COMMIT returned reach the frontier: %v", name, frames[0].Resumed, reached)
		}
		want := 0
		if name == "start" {
			for _, frame := range full {
				want += len(frame.Replay) + len(frame.Data)
			}
		}
		if output != want {
			t.Fatalf("%s: resume sent %d output bytes, want %d", name, output, want)
		}
	}
}
