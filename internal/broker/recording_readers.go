package broker

import (
	"errors"
	"sync"

	"persea-terminal/internal/unifiedjournal"
)

const (
	recordingReaderBytes = 128 << 20
	recordingReaderLimit = 64
	// recordingTailSlots is the capacity of one reader's tail channel; the
	// reader may own one event more, the one its writer is sending. It is
	// sized for admission, the window between PREPARE and COMMIT in which the
	// writer deliberately does not drain the tail: a full-screen program
	// repaints up to a frame rate's worth of records of a few dozen bytes per
	// second, and a browser can take seconds to load the replay, so a count
	// sized for bulk output evicts such a reader on every attempt. 1024 slots
	// cover several seconds of admission at 120 frames per second while the
	// fixed charge below still admits every public attachment the reader
	// budget admitted before. Bulk output reaches recordingTailBytes first.
	recordingTailSlots = 1024
	// recordingTailSlotBytes is the channel memory one slot preallocates:
	// unsafe.Sizeof(unifiedjournal.Event{}), pinned by a test.
	recordingTailSlotBytes = 72
	// The writer uses at most one 64 KiB LIVE encoding at a time. This covers
	// its base64, JSON work buffer and result, plus maps and fixed reader
	// state, including the tail channel's preallocated slots.
	recordingWriterBytes = (512+16)<<10 + recordingTailSlots*recordingTailSlotBytes
	recordingReaderFloor = 64 << 10
	// PREPARE additionally owns replay assembly and a 256 KiB encoding. Keep
	// this reserve through backlog settlement, including a blocked PREPARE.
	recordingPrepareBytes = 2 << 20
	// An actual Epoch additionally owns bounded marker/replay/held buffers,
	// input/transitional queues, its PTY reader and small control egress. Bulk
	// capture and legacy LIVE JSON are absent on this explicit writer pairing.
	recordingAttachmentBytes = 4 << 20
	// recordingTailBytes bounds the payload one reader's tail may own: 4 MiB
	// of pending output plus one maximal 64 KiB record. Output that outruns a
	// reader by more than this is cheaper to rebuild from the snapshot.
	recordingTailBytes = 4<<20 + 64<<10
)

// recordingTailLimit names the bound that refused a tail event. The browser
// always sees one typed reason, subscriber_lagged; the name is for the
// operator log, so a reconnect can be traced to the bound that caused it.
type recordingTailLimit string

const (
	tailLimitSequenceGap  recordingTailLimit = "sequence_gap"
	tailLimitQueueEvents  recordingTailLimit = "queue_events"
	tailLimitQueueBytes   recordingTailLimit = "queue_bytes"
	tailLimitReaderEvents recordingTailLimit = "reader_events"
	tailLimitReaderBytes  recordingTailLimit = "reader_bytes"
	tailLimitDetached     recordingTailLimit = "detached"
	tailLimitInvalid      recordingTailLimit = "invalid_event"
)

var errRecordingReaders = errors.New("recording reader capacity exhausted")

// The budget follows copies, not registry membership. A removed subscriber can
// still own a snapshot or the event inside a blocked socket write. The budget
// lock never calls provider, journal, writer, or transport code.
type recordingReaderBudget struct {
	copies    *recordingCopyBudget
	mu        sync.Mutex
	limit     int64 // zero selects the production limit; tests can narrow it
	bytes     int64
	readers   int
	events    int
	snapshots int
}

type recordingReaderLease struct {
	budget          *recordingReaderBudget
	snapshotBytes   int64
	eventBytes      int64
	events          int
	detached        bool
	released        bool
	owners          int
	attachmentBytes int64
	transportBytes  int64
}

type recordingReaderUsage struct {
	Bytes, Limit    int64
	Readers, Events int
	Snapshots       int
}

func (budget *recordingReaderBudget) capacity() int64 {
	if budget.limit > 0 {
		return budget.limit
	}
	return recordingReaderBytes
}

func (budget *recordingReaderBudget) snapshot() recordingReaderUsage {
	budget.mutex().Lock()
	defer budget.mutex().Unlock()
	return recordingReaderUsage{budget.bytes, budget.capacity(), budget.readers, budget.events, budget.snapshots}
}

func (budget *recordingReaderBudget) acquire(snapshotBytes int64) (*recordingReaderLease, error) {
	budget.mutex().Lock()
	defer budget.mutex().Unlock()
	snapshotBytes += recordingPrepareBytes
	cost := int64(recordingWriterBytes+recordingReaderFloor) + snapshotBytes
	if snapshotBytes < recordingPrepareBytes || budget.readers >= recordingReaderLimit || cost > budget.capacity()-budget.bytes || !budget.copies.available(cost) {
		return nil, errRecordingReaders
	}
	budget.charge(cost)
	budget.readers++
	budget.snapshots++
	return &recordingReaderLease{budget: budget, snapshotBytes: snapshotBytes}, nil
}

func (lease *recordingReaderLease) reserveEvent(bytes int64) bool {
	return lease.reserveTailEvent(bytes) == ""
}

// reserveTailEvent charges one tail event to this lease, or names the bound
// that refuses it. The empty name means the event is owned until releaseEvent.
func (lease *recordingReaderLease) reserveTailEvent(bytes int64) recordingTailLimit {
	if lease == nil {
		return "" // hand-built subscribers are used only by protocol fixtures
	}
	budget := lease.budget
	budget.mutex().Lock()
	defer budget.mutex().Unlock()
	before := max(lease.eventBytes, int64(recordingReaderFloor))
	after := max(lease.eventBytes+bytes, int64(recordingReaderFloor))
	switch {
	case lease.detached:
		return tailLimitDetached
	case bytes < 0:
		return tailLimitInvalid
	case lease.events >= recordingTailSlots+1:
		return tailLimitQueueEvents
	case lease.eventBytes+bytes > recordingTailBytes:
		return tailLimitQueueBytes
	case budget.events >= recordingReaderLimit*(recordingTailSlots+1):
		return tailLimitReaderEvents
	case after-before > budget.capacity()-budget.bytes || !budget.copies.available(after-before):
		return tailLimitReaderBytes
	}
	budget.charge(after - before)
	budget.events++
	lease.eventBytes += bytes
	lease.events++
	return ""
}

func (lease *recordingReaderLease) releaseEvent(bytes int64) {
	if lease == nil {
		return
	}
	budget := lease.budget
	budget.mutex().Lock()
	defer budget.mutex().Unlock()
	if lease.events <= 0 || bytes > lease.eventBytes {
		panic("recording: reader event released without ownership")
	}
	before := max(lease.eventBytes, int64(recordingReaderFloor))
	lease.eventBytes -= bytes
	budget.charge(max(lease.eventBytes, int64(recordingReaderFloor)) - before)
	lease.events--
	budget.events--
	lease.releaseLocked()
}

func (lease *recordingReaderLease) releaseSnapshot() {
	if lease == nil {
		return
	}
	budget := lease.budget
	budget.mutex().Lock()
	defer budget.mutex().Unlock()
	if lease.snapshotBytes != 0 {
		budget.charge(-lease.snapshotBytes)
		lease.snapshotBytes = 0
		budget.snapshots--
	}
	lease.releaseLocked()
}

func (lease *recordingReaderLease) detach() {
	if lease == nil {
		return
	}
	lease.budget.mutex().Lock()
	defer lease.budget.mutex().Unlock()
	lease.detached = true
	lease.releaseLocked()
}

func (lease *recordingReaderLease) releaseLocked() {
	if lease.released || !lease.detached || lease.snapshotBytes != 0 || lease.events != 0 || lease.owners != 0 || lease.transportBytes != 0 {
		return
	}
	lease.released = true
	lease.budget.charge(-(recordingWriterBytes + recordingReaderFloor + lease.attachmentBytes))
	lease.budget.readers--
}

// acquireAttachment funds construction before an Epoch, PTY or initial frame
// exists. PREPARE extends this same lease; it never creates a second reader.
func (budget *recordingReaderBudget) acquireAttachment() (*recordingReaderLease, error) {
	lease, err := budget.acquire(0)
	if err != nil {
		return nil, err
	}
	budget.mutex().Lock()
	if recordingAttachmentBytes > budget.capacity()-budget.bytes || !budget.copies.available(recordingAttachmentBytes) {
		budget.mutex().Unlock()
		lease.releaseSnapshot()
		lease.detach()
		return nil, errRecordingReaders
	}
	budget.charge(recordingAttachmentBytes)
	lease.attachmentBytes, lease.owners = recordingAttachmentBytes, 1
	budget.mutex().Unlock()
	return lease, nil
}

func (lease *recordingReaderLease) hold() {
	if lease == nil {
		return
	}
	lease.budget.mutex().Lock()
	defer lease.budget.mutex().Unlock()
	if lease.released || lease.owners == 0 {
		panic("recording: hold after attachment settlement")
	}
	lease.owners++
}

func (lease *recordingReaderLease) done() {
	if lease == nil {
		return
	}
	lease.budget.mutex().Lock()
	defer lease.budget.mutex().Unlock()
	if lease.owners <= 0 {
		panic("recording: attachment owner released twice")
	}
	lease.owners--
	lease.releaseLocked()
}

func (lease *recordingReaderLease) reserveSnapshot(bytes int64) error {
	budget := lease.budget
	budget.mutex().Lock()
	defer budget.mutex().Unlock()
	bytes += recordingPrepareBytes
	delta := bytes - lease.snapshotBytes
	if lease.detached || bytes < recordingPrepareBytes || delta > budget.capacity()-budget.bytes || !budget.copies.available(delta) {
		return errRecordingReaders
	}
	if lease.snapshotBytes == 0 {
		budget.snapshots++
	}
	budget.charge(delta)
	lease.snapshotBytes = bytes
	return nil
}

func (lease *recordingReaderLease) reserveTransport(bytes int64) bool {
	if lease == nil {
		return true
	}
	budget := lease.budget
	budget.mutex().Lock()
	defer budget.mutex().Unlock()
	if lease.detached || bytes < 0 || bytes > budget.capacity()-budget.bytes || !budget.copies.available(bytes) {
		return false
	}
	budget.charge(bytes)
	lease.transportBytes += bytes
	return true
}

func (lease *recordingReaderLease) releaseTransport(bytes int64) {
	if lease == nil {
		return
	}
	budget := lease.budget
	budget.mutex().Lock()
	defer budget.mutex().Unlock()
	if bytes < 0 || bytes > lease.transportBytes {
		panic("recording: transport released without ownership")
	}
	budget.charge(-bytes)
	lease.transportBytes -= bytes
	lease.releaseLocked()
}

// AllocationCharge is shared with the journal so admission accounts for the
// allocation made by make([]byte, len), including allocator rounding.
func recordingEventBytes(event unifiedjournal.Event) int64 {
	return unifiedjournal.AllocationCharge(int64(len(event.Payload)))
}

func (subscriber *unifiedDevSubscriber) releaseEvent(event unifiedjournal.Event) {
	subscriber.lease.releaseEvent(recordingEventBytes(event))
}

// releaseSnapshot is a consumer settlement, separate from cancel. The returned
// event array and all of its payloads must be unreachable by that consumer.
func (subscriber *unifiedDevSubscriber) releaseSnapshot() {
	subscriber.lease.releaseSnapshot()
}

// The producer has closed data under subscriberMu. A concurrent receive owns
// its event until releaseEvent; only events actually drained here are refunded.
func (subscriber *unifiedDevSubscriber) drainClosed() {
	for event := range subscriber.data {
		subscriber.releaseEvent(event)
	}
}
