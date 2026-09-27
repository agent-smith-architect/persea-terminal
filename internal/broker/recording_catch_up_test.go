package broker

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

// catchUpCommit commits one record without publishing it: the state between a
// durable commit and its asynchronous publication.
func catchUpCommit(t *testing.T, effects *UnifiedDevPaneEffects, key unifiedjournal.PaneKey, payload []byte) unifiedjournal.Record {
	t.Helper()
	record, err := catchUpCommitLocked(effects, key, payload)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// catchUpCommitLocked commits under journalMu, as the retention runtime does.
func catchUpCommitLocked(effects *UnifiedDevPaneEffects, key unifiedjournal.PaneKey, payload []byte) (unifiedjournal.Record, error) {
	effects.journalMu.Lock()
	defer effects.journalMu.Unlock()
	record, err := effects.realm.Append(key, payload)
	if err == nil {
		err = effects.realm.Sync(key)
	}
	if err == nil {
		err = effects.realm.AdvanceCommitted(key, record)
	}
	return record, err
}

func catchUpPublish(t *testing.T, effects *UnifiedDevPaneEffects, key unifiedjournal.PaneKey, record unifiedjournal.Record, payload []byte) {
	t.Helper()
	if err := effects.WritePaneRange(key, payload, record.Start, record.End, record.Sequence); err != nil {
		t.Fatal(err)
	}
}

func catchUpPayloads(events []unifiedjournal.Event) string {
	var out []byte
	for _, event := range events {
		out = append(out, event.Payload...)
	}
	return string(out)
}

// Rejoining the live queue is atomic with commit and publication. Records
// published while the tail catches up are read from the journal instead; a
// record committed but not yet published when the tail rejoins is not copied
// again when its publication arrives; the next record reaches the queue once.
func TestRecordingCatchUpRejoinsTheLiveQueueExactlyOnce(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	events, _, tail, cancel, err := effects.openSnapshotTailWithLease(key.Session, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	tail.releaseSnapshot()
	delivered := unifiedjournal.CommittedCursor{}
	if len(events) != 0 {
		last := events[len(events)-1]
		delivered = unifiedjournal.CommittedCursor{Sequence: last.Sequence, Offset: last.End}
	}
	read := func(want string) {
		t.Helper()
		got, charge, rejoined, err := effects.readCatchUp(tail, delivered)
		if err != nil || rejoined || catchUpPayloads(got) != want {
			t.Fatalf("catch-up read=%q rejoined=%v err=%v, want %q", catchUpPayloads(got), rejoined, err, want)
		}
		last := got[len(got)-1]
		delivered = unifiedjournal.CommittedCursor{Sequence: last.Sequence, Offset: last.End}
		tail.lease.releaseCatchUp(charge)
	}

	for _, payload := range []string{"one", "two"} {
		catchUpPublish(t, effects, key, catchUpCommit(t, effects, key, []byte(payload)), []byte(payload))
	}
	if tail.data.len() != 0 {
		t.Fatal("publication copied to a tail that is catching up")
	}
	read("onetwo")
	pending := catchUpCommit(t, effects, key, []byte("three"))
	read("three")
	if _, _, rejoined, err := effects.readCatchUp(tail, delivered); err != nil || !rejoined {
		t.Fatalf("frontier read rejoined=%v err=%v", rejoined, err)
	}
	if tail.data.isCatchingUp() || tail.cursor != delivered.Sequence {
		t.Fatalf("rejoined tail catching up=%v cursor=%d, want %d", tail.data.isCatchingUp(), tail.cursor, delivered.Sequence)
	}
	catchUpPublish(t, effects, key, pending, []byte("three"))
	if tail.data.len() != 0 {
		t.Fatal("a record read from the journal was copied again when its publication arrived")
	}
	catchUpPublish(t, effects, key, catchUpCommit(t, effects, key, []byte("four")), []byte("four"))
	if event, state := tail.data.next(); state != recordingTailEvent || string(event.Payload) != "four" || tail.data.len() != 0 {
		t.Fatalf("live record state=%v payload=%q queued after=%d", state, event.Payload, tail.data.len())
	} else {
		tail.releaseEvent(event)
	}
	if tail.closeReason() != "" {
		t.Fatalf("catch-up closed the tail: reason=%q limit=%q", tail.closeReason(), tail.closeLimit())
	}
}

// Many attachments admitted while a producer commits and publishes without
// pause each deliver the committed stream exactly once and in order: every
// admission crosses PREPARE, the backlog, catch-up and the rejoin seam while
// records land on either side of it.
func TestRecordingCatchUpUnderConcurrentOutputDeliversEveryRecordOnce(t *testing.T) {
	effects, key := recordingReaderFixture(t, 64<<10)
	expected := bytes.Repeat([]byte{'s'}, 64<<10)
	const attachments = 12
	for round := 0; round < attachments; round++ {
		stop, done := make(chan struct{}), make(chan []byte)
		var count atomic.Int64
		go func() {
			var produced []byte
			defer func() { done <- produced }()
			for index := 0; ; index++ {
				select {
				case <-stop:
					return
				default:
				}
				payload := []byte(fmt.Sprintf("[%02d:%05d]", round, index))
				record, err := catchUpCommitLocked(effects, key, payload)
				if err == nil {
					err = effects.WritePaneRange(key, payload, record.Start, record.End, record.Sequence)
				}
				if err != nil {
					t.Error(err)
					return
				}
				produced = append(produced, payload...)
				count.Add(1)
			}
		}()
		attachment := b1Open(t, effects, key.Session, fmt.Sprintf("catch-up-%d", round))
		// Keep output landing after admission, then stop the producer.
		admitted := count.Load()
		pollUntil(t, 10*time.Second, "output after admission", func() bool { return count.Load() >= admitted+64 })
		close(stop)
		expected = append(expected, <-done...)
		pollUntil(t, 10*time.Second, fmt.Sprintf("attachment %d delivering the committed stream", round), func() bool {
			live, _, _, _ := attachment.snapshot()
			return bytes.Equal(append(attachment.replayBytes(), live...), expected)
		})
		if _, controls, closed, _ := attachment.snapshot(); len(controls) != 0 || closed {
			t.Fatalf("attachment %d controls=%+v closed=%v", round, controls, closed)
		}
		_ = attachment.writer.Close(context.Background())
	}
}

// A catch-up read owns its copy until its last write returns: cancelling the
// attachment while that write is parked does not refund it, and the settled
// write releases it exactly once.
func TestRecordingCatchUpReadStaysChargedThroughAParkedWrite(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	wire := &admissionWire{park: terminal.FrameLive, entered: make(chan struct{}), release: make(chan struct{})}
	writer := &unifiedAttachmentFrameWriter{provider: effects, session: key.Session, downstream: &attachmentFrameWriter{wire: &lockedWriter{w: wire}}}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(wire.release) }) }
	defer writer.Close(context.Background())
	defer unblock()
	if err := admissionFrame(t, writer, terminal.FramePrepare); err != nil {
		t.Fatal(err)
	}
	// The snapshot's end and the tail's cancel; streamTail owns both after COMMIT.
	after, cancel := writer.delivered, writer.cancel
	payload := bytes.Repeat([]byte{'c'}, 256<<10)
	catchUpPublish(t, effects, key, catchUpCommit(t, effects, key, payload), payload)
	if err := admissionFrame(t, writer, terminal.FrameCommit); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wire.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("catch-up write did not park")
	}
	effects.journalMu.Lock()
	charge, _, err := effects.realm.SuffixAllocation(key, after, recordingCatchUpReadBytes)
	effects.journalMu.Unlock()
	if err != nil || charge < int64(len(payload)) {
		t.Fatalf("catch-up charge=%d err=%v", charge, err)
	}
	held := int64(recordingWriterBytes+recordingReaderFloor) + charge
	if got := effects.readers.snapshot(); got.Bytes != held {
		t.Fatalf("parked catch-up write charged %d bytes, want %d (snapshot settled, read held)", got.Bytes, held)
	}
	cancel()
	if got := effects.readers.snapshot(); got.Bytes != held || got.Readers != 1 {
		t.Fatalf("cancellation refunded a parked catch-up read: %+v, want %d bytes", got, held)
	}
	unblock()
	pollUntil(t, 5*time.Second, "catch-up read settlement", func() bool { return effects.readers.snapshot().Bytes == 0 })
	if got := effects.readers.snapshot(); got.Readers != 0 {
		t.Fatalf("reader did not settle: %+v", got)
	}
}

// When the aggregate reader budget cannot fund a catch-up read, the read does
// not happen: the attachment ends with the typed subscriber_lagged verdict,
// naming reader_bytes, which the browser treats as a reconnect.
func TestRecordingCatchUpRefusedReadEndsTheAttachmentTyped(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	wire := &admissionWire{}
	ended := make(chan struct{})
	writer := &unifiedAttachmentFrameWriter{provider: effects, session: key.Session, downstream: &attachmentFrameWriter{wire: &lockedWriter{w: wire}}, end: func() { close(ended) }}
	defer writer.Close(context.Background())
	if err := admissionFrame(t, writer, terminal.FramePrepare); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{'r'}, 256<<10)
	catchUpPublish(t, effects, key, catchUpCommit(t, effects, key, payload), payload)
	writer.mu.Lock()
	writer.releaseSnapshotLocked()
	writer.mu.Unlock()
	effects.readers.mutex().Lock()
	effects.readers.limit = effects.readers.bytes + 1
	effects.readers.mutex().Unlock()
	if err := admissionFrame(t, writer, terminal.FrameCommit); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("refused catch-up did not end the attachment")
	}
	if writer.tail.closeReason() != proto.SubscriberClosedLagged || writer.tail.closeLimit() != tailLimitReaderBytes {
		t.Fatalf("verdict=%q limit=%q, want %q/%q", writer.tail.closeReason(), writer.tail.closeLimit(), proto.SubscriberClosedLagged, tailLimitReaderBytes)
	}
	if types := admissionTypes(t, wire); len(types) != 3 || types[1] != terminal.FrameCommit || types[2] != "verdict" {
		t.Fatalf("wire=%v, want PREPARE, COMMIT, verdict", types)
	}
}
