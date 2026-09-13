package unifiedjournal

import "errors"

var ErrJournalGap = errors.New("journal byte range is not contiguous")

type EligibilityState uint8

const (
	EligibilityLegacy EligibilityState = iota
	EligibilityContinuous
	EligibilityUntrusted
)

type InvalidationReason uint8

const (
	ReasonNone InvalidationReason = iota
	ReasonControlPaused
	ReasonControlDisconnected
	ReasonDecoderAmbiguous
	ReasonSourceReplacement
	ReasonQuota
	ReasonENOSPC
	ReasonCorruptJournal
	ReasonVolatileRestart
	ReasonJournalGap
	// ReasonAdoptionAborted marks the tombstone of an adoption reservation
	// that failed before activation: its slot and realm charge were released,
	// and the key fails closed for any append still in flight.
	ReasonAdoptionAborted
	// ReasonPhysicalQuota is the physical ledger's counterpart of ReasonQuota:
	// the generation's file, or the realm's files together, reached the
	// on-disk budget before the logical caps did. It fails closed exactly as
	// ReasonQuota does; the distinct reason keeps a framing-heavy pane
	// diagnosable as such.
	ReasonPhysicalQuota
)

type AdmissionKind uint8

const (
	AdmissionObservedBeforeFirstByte AdmissionKind = iota + 1
	AdmissionExistingPane
	AdmissionReconstructedPane
	AdmissionAfterBrokerRestart
)

type Admission struct {
	Kind         AdmissionKind
	Key          PaneKey
	JournalStart int64
}

type EligibilityTracker struct {
	state     EligibilityState
	reason    InvalidationReason
	committed int64
}

func NewEligibility(admission Admission) *EligibilityTracker {
	tracker := &EligibilityTracker{state: EligibilityLegacy, committed: admission.JournalStart}
	if admission.Kind == AdmissionObservedBeforeFirstByte && admission.JournalStart == 0 {
		tracker.state = EligibilityContinuous
	}
	return tracker
}

func (tracker *EligibilityTracker) State() EligibilityState { return tracker.state }

func (tracker *EligibilityTracker) Reason() InvalidationReason { return tracker.reason }

func (tracker *EligibilityTracker) CommittedOffset() int64 { return tracker.committed }

func (tracker *EligibilityTracker) UnifiedEligible() bool {
	return tracker.state == EligibilityContinuous
}

func (tracker *EligibilityTracker) Invalidate(reason InvalidationReason) {
	if tracker.state == EligibilityUntrusted {
		return
	}
	tracker.state = EligibilityUntrusted
	tracker.reason = reason
}

func (tracker *EligibilityTracker) ObserveCommittedRange(start, end int64) error {
	if tracker.state == EligibilityUntrusted {
		return ErrInvalidated
	}
	if tracker.state != EligibilityContinuous || start != tracker.committed || end < start {
		tracker.Invalidate(ReasonJournalGap)
		return ErrJournalGap
	}
	tracker.committed = end
	return nil
}

type WriteAhead interface {
	Append(PaneKey, []byte) (Record, error)
	Sync(PaneKey) error
	AdvanceCommitted(PaneKey, Record) error
}

type FeedFunc func(PaneKey, Record, []byte) error

type Sequencer struct {
	sink   WriteAhead
	feed   FeedFunc
	failed bool
}

func NewSequencer(sink WriteAhead, feed FeedFunc) *Sequencer {
	return &Sequencer{sink: sink, feed: feed}
}

func (sequencer *Sequencer) Failed() bool { return sequencer.failed }

func (sequencer *Sequencer) Write(key PaneKey, payload []byte) error {
	_, err := sequencer.WriteRecord(key, payload)
	return err
}

// WriteRecord returns a record only after its storage verification and feed
// admission succeed. Callers must still check their operation's live authority.
func (sequencer *Sequencer) WriteRecord(key PaneKey, payload []byte) (Record, error) {
	if sequencer.failed {
		return Record{}, ErrInvalidated
	}
	if sequencer.sink == nil || sequencer.feed == nil {
		sequencer.failed = true
		return Record{}, ErrStorage
	}
	record, err := sequencer.sink.Append(key, payload)
	if err == nil {
		err = sequencer.sink.Sync(key)
	}
	if err == nil {
		err = sequencer.sink.AdvanceCommitted(key, record)
	}
	if err == nil {
		err = sequencer.feed(key, record, payload)
	}
	if err != nil {
		sequencer.failed = true
		return Record{}, ErrStorage
	}
	return record, nil
}
