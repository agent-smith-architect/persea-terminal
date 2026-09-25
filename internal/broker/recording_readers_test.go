package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
	"unsafe"

	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

func recordingReaderFixture(t *testing.T, payload int) (*UnifiedDevPaneEffects, unifiedjournal.PaneKey) {
	t.Helper()
	realm := openRetentionRealm(t, "reader-ownership")
	key := unifiedjournal.PaneKey{Server: "test", Session: "$1", ControlGeneration: 1, Window: "@1", Pane: "%1", Incarnation: "reader"}
	if err := realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	effects := &UnifiedDevPaneEffects{realm: realm, active: map[string]unifiedjournal.PaneKey{key.Session: key}, subscribers: make(map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{})}
	prepareRecordingProviderForTest(t, effects, key)
	if payload != 0 {
		b1Commit(t, effects, key, bytes.Repeat([]byte{'s'}, payload))
	}
	return effects, key
}

func TestRecordingReaderSnapshotAdmissionPrecedesCopy(t *testing.T) {
	effects, key := recordingReaderFixture(t, 2<<20)
	effects.readers.limit = 1
	allocs := testing.AllocsPerRun(30, func() {
		events, _, tail, cancel, err := effects.openSnapshotTail(key.Session)
		if !errors.Is(err, errRecordingReaders) || events != nil || tail != nil || cancel != nil {
			t.Fatalf("refused snapshot allocated or registered: %v", err)
		}
	})
	if allocs != 0 {
		t.Fatalf("refused snapshot allocated %.0f objects before admission", allocs)
	}
	if got := effects.readers.snapshot(); got.Bytes != 0 || got.Readers != 0 || got.Snapshots != 0 {
		t.Fatalf("refused snapshot retained capacity: %+v", got)
	}
}

func TestRecordingReaderCancellationDoesNotSettleSnapshot(t *testing.T) {
	effects, key := recordingReaderFixture(t, 2<<20)
	events, _, tail, cancel, err := effects.openSnapshotTail(key.Session)
	if err != nil {
		t.Fatal(err)
	}
	before := effects.readers.snapshot()
	effects.readers.limit = before.Bytes
	cancel()
	if got := effects.readers.snapshot(); got.Bytes != before.Bytes || got.Snapshots != 1 || got.Readers != 1 {
		t.Fatalf("tail cancel refunded caller snapshot: before=%+v after=%+v", before, got)
	}
	if _, _, _, _, err := effects.openSnapshotTail(key.Session); !errors.Is(err, errRecordingReaders) {
		t.Fatalf("held snapshot's capacity was reusable: %v", err)
	}
	if len(events) != 2 || len(events[1].Payload) != 2<<20 {
		t.Fatal("cancellation changed the caller's snapshot")
	}
	events = nil
	tail.releaseSnapshot()
	tail.releaseSnapshot()
	if got := effects.readers.snapshot(); got.Bytes != 0 || got.Readers != 0 || got.Snapshots != 0 {
		t.Fatalf("snapshot did not settle exactly once: %+v", got)
	}
	_, _, next, stop, err := effects.openSnapshotTail(key.Session)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	next.releaseSnapshot()
}

func TestRecordingReaderFullQueuesAndInflightWritesRetainOwnership(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	const readers = 16
	tails := make([]*unifiedDevSubscriber, readers)
	cancels := make([]func(), readers)
	for i := range tails {
		_, _, tail, cancel, err := effects.openSnapshotTail(key.Session)
		if err != nil {
			t.Fatal(err)
		}
		tail.releaseSnapshot()
		tails[i], cancels[i] = tail, cancel
	}
	// Maximal 64 KiB records reach the byte bound long before the count
	// bound: each tail can own fullTail of them, queued plus in flight.
	const fullTail = recordingTailBytes / (64 << 10)
	payload := bytes.Repeat([]byte{'q'}, 64<<10)
	for seq := int64(2); seq <= fullTail; seq++ {
		_ = effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Sequence: seq, Payload: payload})
	}
	if got := effects.readers.snapshot(); got.Events != readers*(fullTail-1) {
		t.Fatalf("full queues=%+v", got)
	}
	active := make([]unifiedjournal.Event, readers)
	for i, tail := range tails {
		active[i] = <-tail.events()
	}
	before := effects.readers.snapshot()
	if before.Events != readers*(fullTail-1) {
		t.Fatal("dequeue minted capacity before writer settlement")
	}
	_ = effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Sequence: fullTail + 1, Payload: payload})
	if got := effects.readers.snapshot(); got.Events != readers*fullTail {
		t.Fatalf("queued + in-flight=%+v", got)
	}
	_ = effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Sequence: fullTail + 2, Payload: payload})
	for i, tail := range tails {
		if tail.closeReason() != proto.SubscriberClosedLagged || tail.closeLimit() != tailLimitQueueBytes {
			t.Fatalf("tail %d past its byte bound: reason=%q limit=%q", i, tail.closeReason(), tail.closeLimit())
		}
	}
	for _, cancel := range cancels {
		cancel()
	}
	if got := effects.readers.snapshot(); got.Events != readers || got.Readers != readers || got.Bytes != readers*(recordingWriterBytes+recordingReaderFloor) {
		t.Fatalf("eviction must drain queued bytes but retain actual writes: %+v", got)
	}
	for i, tail := range tails {
		event := active[i]
		active[i] = unifiedjournal.Event{}
		tail.releaseEvent(event)
	}
	if got := effects.readers.snapshot(); got.Bytes != 0 || got.Readers != 0 || got.Events != 0 {
		t.Fatalf("settled writes retained capacity: %+v", got)
	}
}

// A full-screen program repaints in records of a few dozen bytes. A tail that
// is not being drained, as between PREPARE and COMMIT, must absorb
// recordingTailSlots of them; only the next one is refused, by count, and the
// refusal is named for the operator log.
func TestRecordingTailAbsorbsSmallRecordsUpToTheCountBound(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	_, _, tail, cancel, err := effects.openSnapshotTail(key.Session)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	tail.releaseSnapshot()
	record := func(seq int64) []byte {
		return []byte(fmt.Sprintf("\x1b]0;%d Working\x07\x1b[?2026h\x1b[47;3H\x1b[?2026l", seq))
	}
	for seq := int64(2); seq <= recordingTailSlots+1; seq++ {
		_ = effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Sequence: seq, Payload: record(seq)})
	}
	if b1SubscriberCount(effects, key) != 1 || tail.closeReason() != "" {
		t.Fatalf("%d small records evicted an undrained tail: reason=%q limit=%q", recordingTailSlots, tail.closeReason(), tail.closeLimit())
	}
	if got := effects.readers.snapshot(); got.Events != recordingTailSlots {
		t.Fatalf("burst ownership=%+v", got)
	}
	_ = effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Sequence: recordingTailSlots + 2, Payload: record(recordingTailSlots + 2)})
	if b1SubscriberCount(effects, key) != 0 || tail.closeReason() != proto.SubscriberClosedLagged || tail.closeLimit() != tailLimitQueueEvents {
		t.Fatalf("record past the count bound: subscribers=%d reason=%q limit=%q", b1SubscriberCount(effects, key), tail.closeReason(), tail.closeLimit())
	}
	if got := effects.readers.snapshot(); got.Events != 0 {
		t.Fatalf("eviction kept queued records: %+v", got)
	}
}

func TestRecordingTailSequenceGapIsNamed(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	_, _, tail, cancel, err := effects.openSnapshotTail(key.Session)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	tail.releaseSnapshot()
	_ = effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Sequence: 3, Payload: []byte("gap")})
	if tail.closeReason() != proto.SubscriberClosedLagged || tail.closeLimit() != tailLimitSequenceGap {
		t.Fatalf("gap: reason=%q limit=%q", tail.closeReason(), tail.closeLimit())
	}
}

// The tail channel preallocates its slots, and recordingWriterBytes funds them
// through recordingTailSlotBytes; the constant must cover one Event.
func TestRecordingTailSlotBytesCoverOneEvent(t *testing.T) {
	if got := unsafe.Sizeof(unifiedjournal.Event{}); got > recordingTailSlotBytes {
		t.Fatalf("unifiedjournal.Event is %d bytes; recordingTailSlotBytes=%d leaves the tail channel unfunded", got, recordingTailSlotBytes)
	}
}

func TestRecordingReaderCountAndGuaranteedFirstEvent(t *testing.T) {
	var budget recordingReaderBudget
	leases := make([]*recordingReaderLease, 0, recordingReaderLimit)
	for i := 0; i < recordingReaderLimit; i++ {
		lease, err := budget.acquire(0)
		if err != nil {
			t.Fatal(err)
		}
		lease.releaseSnapshot()
		leases = append(leases, lease)
	}
	if _, err := budget.acquire(0); !errors.Is(err, errRecordingReaders) {
		t.Fatal("reader count did not bound independent reader owners")
	}
	budget.limit = budget.snapshot().Bytes
	for _, lease := range leases {
		if !lease.reserveEvent(64 << 10) {
			t.Fatal("admitted reader lost its funded first event")
		}
		lease.detach()
		lease.releaseEvent(64 << 10)
	}
	if got := budget.snapshot(); got.Bytes != 0 || got.Readers != 0 {
		t.Fatalf("reader floor leaked: %+v", got)
	}
}

type recordingHeldWire struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (wire *recordingHeldWire) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"data"`)) {
		wire.once.Do(func() { close(wire.entered) })
		<-wire.release
	}
	return len(p), nil
}

func TestRecordingReaderBacklogChargeSurvivesBlockedWriteAndCancel(t *testing.T) {
	effects, key := recordingReaderFixture(t, 512<<10)
	wire := &recordingHeldWire{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(wire.release) }) }
	defer unblock()
	writer := &unifiedAttachmentFrameWriter{provider: effects, session: key.Session, downstream: &attachmentFrameWriter{wire: &lockedWriter{w: wire}}}
	prepare := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FramePrepare, Source: "reader", Epoch: 1, Cut: 1, Kind: terminal.CutInitial, Columns: 80, Rows: 24}
	if err := writer.WriteFrame(context.Background(), unifiedE2E1RawFrame(t, prepare)); err != nil {
		t.Fatal(err)
	}
	commit := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameCommit, Source: "reader", Epoch: 1, Cut: 1}
	done := make(chan error, 1)
	go func() { done <- writer.WriteFrame(context.Background(), unifiedE2E1RawFrame(t, commit)) }()
	select {
	case <-wire.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("backlog write did not park")
	}
	before := effects.readers.snapshot()
	closed := make(chan struct{})
	go func() { _ = writer.Close(context.Background()); close(closed) }()
	effects.closeSubscribers(key, proto.SubscriberClosedGenerationRotated)
	if got := effects.readers.snapshot(); got.Bytes != before.Bytes || got.Snapshots != 1 {
		t.Fatalf("blocked backlog lost its snapshot or scratch ownership: %+v -> %+v", before, got)
	}
	select {
	case <-closed:
		t.Fatal("Close claimed settlement while write was held")
	default:
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-closed
	pollUntil(t, time.Second, "reader settlement", func() bool { return effects.readers.snapshot().Bytes == 0 })
}

func TestRecordingReaderLiveWriteCannotRefundAtDequeue(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	wire := &recordingHeldWire{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(wire.release) }) }
	defer unblock()
	writer := &unifiedAttachmentFrameWriter{provider: effects, session: key.Session, downstream: &attachmentFrameWriter{wire: &lockedWriter{w: wire}}}
	prepare := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FramePrepare, Source: "reader", Epoch: 1, Cut: 1, Kind: terminal.CutInitial, Columns: 80, Rows: 24}
	if err := writer.WriteFrame(context.Background(), unifiedE2E1RawFrame(t, prepare)); err != nil {
		t.Fatal(err)
	}
	commit := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameCommit, Source: "reader", Epoch: 1, Cut: 1}
	if err := writer.WriteFrame(context.Background(), unifiedE2E1RawFrame(t, commit)); err != nil {
		t.Fatal(err)
	}
	tail, cancel := writer.tail, writer.cancel
	b1Commit(t, effects, key, bytes.Repeat([]byte{'w'}, 64<<10))
	select {
	case <-wire.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("live write did not park")
	}
	if len(tail.data) != 0 {
		t.Fatal("writer did not dequeue its event")
	}
	cancel()
	if got := effects.readers.snapshot(); got.Events != 1 || got.Readers != 1 || got.Bytes != recordingWriterBytes+recordingReaderFloor {
		t.Fatalf("dequeue/cancel refunded blocked writer: %+v", got)
	}
	unblock()
	pollUntil(t, time.Second, "actual live write settlement", func() bool { return effects.readers.snapshot().Bytes == 0 })
	_ = writer.Close(context.Background())
}

func TestRecordingReaderZeroByteEventsStillOwnCount(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	_, _, tail, cancel, err := effects.openSnapshotTail(key.Session)
	if err != nil {
		t.Fatal(err)
	}
	tail.releaseSnapshot()
	for seq := int64(2); seq <= recordingTailSlots+1; seq++ {
		_ = effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordGeometry, Sequence: seq, Geometry: unifiedjournal.Geometry{Columns: 80, Rows: 24}})
	}
	active := <-tail.data
	_ = effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordGeometry, Sequence: recordingTailSlots + 2, Geometry: unifiedjournal.Geometry{Columns: 80, Rows: 24}})
	if got := effects.readers.snapshot(); got.Events != recordingTailSlots+1 {
		t.Fatalf("zero-byte events escaped count: %+v", got)
	}
	cancel()
	if got := effects.readers.snapshot(); got.Events != 1 || got.Readers != 1 {
		t.Fatalf("active zero-byte event escaped ownership: %+v", got)
	}
	tail.releaseEvent(active)
	if got := effects.readers.snapshot(); got.Bytes != 0 {
		t.Fatalf("zero-byte event leaked: %+v", got)
	}
}

func TestRecordingReaderOldSnapshotOutlivesGeneration(t *testing.T) {
	effects, key := recordingReaderFixture(t, 2<<20)
	snapshot, _, tail, cancel, err := effects.openSnapshotTail(key.Session)
	if err != nil {
		t.Fatal(err)
	}
	before := effects.readers.snapshot()
	cancel()
	effects.journalMu.Lock()
	retired := effects.realm.RetirePane(key)
	effects.journalMu.Unlock()
	if !retired {
		t.Fatal("fixture generation did not retire")
	}
	if got := effects.readers.snapshot(); got.Bytes != before.Bytes {
		t.Fatal("generation removal refunded caller's copy")
	}
	if len(snapshot[1].Payload) != 2<<20 {
		t.Fatal("old snapshot was lost")
	}
	snapshot = nil
	tail.releaseSnapshot()
	if got := effects.readers.snapshot(); got.Bytes != 0 {
		t.Fatalf("old snapshot leaked: %+v", got)
	}
}

func TestRecordingProductionWriterRequiresExactAdmittedOwner(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	trial := effects.initialObserver().recordingRegistry().retention
	// Exercise the manager-owned adapter after its goroutine has settled.
	// Concurrent direct adapter calls would themselves violate its contract.
	if err := trial.Close(); err != nil {
		t.Fatal(err)
	}
	trial.panes = make(map[unifiedjournal.PaneKey]*retentionPaneRuntime)
	missing := key
	missing.ControlGeneration++
	if _, err := trial.realmAppend(missing, []byte("late")); !errors.Is(err, unifiedjournal.ErrInvalidated) {
		t.Fatalf("production adapter implicitly created missing generation: %v", err)
	}
	if effects.realm.IdentityCount() != 1 {
		t.Fatal("missing write minted an identity")
	}
	if _, err := trial.realmAppend(key, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if !effects.realm.RetirePane(key) {
		t.Fatal("test owner did not retire")
	}
	if err := effects.realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	if _, err := trial.realmAppend(key, []byte("stale")); !errors.Is(err, unifiedjournal.ErrInvalidated) {
		t.Fatal("production adapter rebound a stale pane runtime")
	}
}
