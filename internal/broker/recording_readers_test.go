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
	// The byte bound includes both nodes and payload, queued plus in flight.
	const fullTail = recordingTailBytes / (64<<10 + recordingTailNodeBytes)
	payload := bytes.Repeat([]byte{'q'}, 64<<10)
	for seq := int64(2); seq <= fullTail; seq++ {
		_ = effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Sequence: seq, Payload: payload})
	}
	if got := effects.readers.snapshot(); got.Events != readers*(fullTail-1) {
		t.Fatalf("full queues=%+v", got)
	}
	active := make([]unifiedjournal.Event, readers)
	for i, tail := range tails {
		active[i], _ = tail.receive()
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

// Small records and geometry consume their allocations, including node overhead.
func TestRecordingTailAbsorbsSmallRecordsUpToTheByteBound(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	_, _, tail, cancel, err := effects.openSnapshotTail(key.Session)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	tail.releaseSnapshot()
	payload := []byte("small repaint")
	cost := recordingEventBytes(unifiedjournal.Event{Payload: payload})
	count := int64(recordingTailBytes) / cost
	for seq := int64(2); seq <= count+1; seq++ {
		_ = effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Sequence: seq, Payload: payload})
	}
	if b1SubscriberCount(effects, key) != 1 || tail.closeReason() != "" {
		t.Fatalf("byte-funded burst evicted: reason=%q limit=%q", tail.closeReason(), tail.closeLimit())
	}
	if got := effects.readers.snapshot(); int64(got.Events) != count || got.Bytes != recordingWriterBytes+count*cost {
		t.Fatalf("burst ownership=%+v", got)
	}
	_ = effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Sequence: count + 2, Payload: payload})
	if b1SubscriberCount(effects, key) != 0 || tail.closeReason() != proto.SubscriberClosedLagged || tail.closeLimit() != tailLimitQueueBytes {
		t.Fatalf("byte overflow: reason=%q limit=%q", tail.closeReason(), tail.closeLimit())
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

// Pointer-bearing allocations include the allocator header before rounding.
func TestRecordingTailNodeBytesCoverAllocation(t *testing.T) {
	if got := unifiedjournal.AllocationCharge(int64(unsafe.Sizeof(recordingTailNode{})) + 8); got != recordingTailNodeBytes {
		t.Fatalf("node allocation=%d charge=%d", got, recordingTailNodeBytes)
	}
}

func TestRecordingTailPreservesSmallRecordAndGeometryOrder(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	_, _, tail, cancel, err := effects.openSnapshotTail(key.Session)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	tail.releaseSnapshot()
	eventAt := func(sequence int64) unifiedjournal.Event {
		if sequence%31 == 0 {
			return unifiedjournal.Event{Kind: unifiedjournal.RecordGeometry, Sequence: sequence, Geometry: unifiedjournal.Geometry{Columns: 80, Rows: int(sequence%100) + 8}}
		}
		return unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Sequence: sequence, Start: sequence * 32, End: sequence*32 + 32, Payload: []byte(fmt.Sprintf("%032d", sequence))}
	}
	// Admission has no consumer yet. Each publication must own its copy and
	// preserve geometry at the exact sequence between output records.
	const count = 4096
	for sequence := int64(2); sequence < count+2; sequence++ {
		event := eventAt(sequence)
		if err := effects.publishEvent(key, event); err != nil {
			t.Fatal(err)
		}
		clear(event.Payload)
	}
	if got := effects.readers.snapshot(); got.Events != count {
		t.Fatalf("funded burst lost events: %+v", got)
	}
	for sequence := int64(2); sequence < count+2; sequence++ {
		select {
		case <-tail.events():
		case <-time.After(5 * time.Second):
			t.Fatal("queued event did not signal readiness", sequence)
		}
		event, open := tail.receive()
		want := eventAt(sequence)
		if !open || event.Kind != want.Kind || event.Sequence != want.Sequence || event.Start != want.Start || event.End != want.End || event.Geometry != want.Geometry || !bytes.Equal(event.Payload, want.Payload) {
			t.Fatalf("event %d: got=%+v want=%+v open=%t", sequence, event, want, open)
		}
		tail.releaseEvent(event)
	}
	if got := effects.readers.snapshot(); got.Events != 0 || got.Bytes != recordingWriterBytes+recordingReaderFloor {
		t.Fatalf("drained burst retained event charges: %+v", got)
	}
	cancel()
	if _, open := tail.receive(); open {
		t.Fatal("cancelled tail remained open")
	}
	if got := effects.readers.snapshot(); got.Bytes != 0 {
		t.Fatalf("settled burst retained owner: %+v", got)
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
		if !lease.reserveEvent(recordingReaderFloor) {
			t.Fatal("admitted reader lost its funded first event")
		}
		lease.detach()
		lease.releaseEvent(recordingReaderFloor)
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
	// The verdict arrived while the first backlog frame was held, so the rest
	// of the backlog is not written, even the rest of the same event.
	if err := <-done; !errors.Is(err, terminal.ErrClosed) {
		t.Fatalf("COMMIT after a verdict returned %v, want ErrClosed", err)
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
	if tail.data.len() != 0 {
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

func TestRecordingReaderZeroByteEventsStillOwnBytes(t *testing.T) {
	effects, key := recordingReaderFixture(t, 0)
	_, _, tail, cancel, err := effects.openSnapshotTail(key.Session)
	if err != nil {
		t.Fatal(err)
	}
	tail.releaseSnapshot()
	const count = recordingTailBytes / recordingTailNodeBytes
	for seq := int64(2); seq <= count; seq++ {
		_ = effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordGeometry, Sequence: seq, Geometry: unifiedjournal.Geometry{Columns: 80, Rows: 24}})
	}
	active, _ := tail.receive()
	_ = effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordGeometry, Sequence: count + 1, Geometry: unifiedjournal.Geometry{Columns: 80, Rows: 24}})
	if got := effects.readers.snapshot(); got.Events != count || got.Bytes != recordingWriterBytes+recordingTailBytes {
		t.Fatalf("zero-byte events escaped byte charge: %+v", got)
	}
	_ = effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordGeometry, Sequence: count + 2, Geometry: unifiedjournal.Geometry{Columns: 80, Rows: 24}})
	if tail.closeReason() != proto.SubscriberClosedLagged || tail.closeLimit() != tailLimitQueueBytes {
		t.Fatalf("geometry overflow: reason=%q limit=%q", tail.closeReason(), tail.closeLimit())
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
