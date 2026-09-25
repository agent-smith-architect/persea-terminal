// Unified journal publication, subscriptions, and snapshot tails.
package broker

import (
	"errors"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/unifiedjournal"
	"sync"
	"time"
)

type unifiedDevSubscriber struct {
	lease *recordingReaderLease
	// cursor is a sequence, not a byte offset. Geometry events carry no payload,
	// so consecutive ones share a byte offset and a byte cursor cannot order them.
	cursor int64
	data   chan unifiedjournal.Event
	done   chan struct{}
	once   sync.Once
	// closedReason is the typed outcome of the provider removing this
	// subscriber. It is written under subscriberMu strictly before data is
	// closed, so a consumer that has observed the close reads it without a
	// lock. It stays empty when the subscriber's own cancel closed data: that
	// is the attachment ending on its own terms, not a verdict on it.
	closedReason proto.SubscriberCloseReason
	// verdict is closed, after closedReason is written and strictly before
	// data is closed, when the provider removes this subscriber with a typed
	// reason. It is the one signal of that removal a consumer can observe
	// WITHOUT receiving from data — which is what the attachment's bounded
	// close path needs, because the consumer of data is the goroutine that
	// may be parked inside a downstream write to a peer that has stopped
	// reading. Nil on a subscriber that was never given one; such a
	// subscriber has no bounded path and only the tail's own close.
	verdict chan struct{}
}

// events transfers one copied event to its consumer, which must call
// releaseEvent only after its final write/use returns. Removal drains queued
// events; it cannot settle an event already received by that consumer.
func (subscriber *unifiedDevSubscriber) events() <-chan unifiedjournal.Event { return subscriber.data }

// verdictSignal is closed once the provider has removed this subscriber with
// a typed reason; closeReason is valid after it is observed. Nil (never ready)
// on a subscriber without one.
func (subscriber *unifiedDevSubscriber) verdictSignal() <-chan struct{} { return subscriber.verdict }

// closeTyped records the provider's verdict and publishes it: reason first,
// then the verdict signal, then the tail. Caller holds subscriberMu and has
// already removed the subscriber from the registry, so this runs at most once.
func (subscriber *unifiedDevSubscriber) closeTyped(reason proto.SubscriberCloseReason) {
	subscriber.closedReason = reason
	if subscriber.verdict != nil {
		close(subscriber.verdict)
	}
	close(subscriber.data)
	subscriber.drainClosed()
}

// closeReason is valid only after events() has been observed closed. Empty
// means the subscriber cancelled itself; anything else is the provider's typed
// verdict, which the attachment must carry to the browser as its close reason.
func (subscriber *unifiedDevSubscriber) closeReason() proto.SubscriberCloseReason {
	return subscriber.closedReason
}

func (effects *UnifiedDevPaneEffects) WritePane(key unifiedjournal.PaneKey, payload []byte) error {
	return errors.New("unified feed range metadata is required")
}

func (effects *UnifiedDevPaneEffects) WritePaneRange(key unifiedjournal.PaneKey, payload []byte, start, end, sequence int64) error {
	if start < 0 || end < start || end-start != int64(len(payload)) || sequence < 1 {
		return errors.New("invalid unified feed range")
	}
	return effects.publishEvent(key, unifiedjournal.Event{
		Kind: unifiedjournal.RecordOutput, Sequence: sequence, Start: start, End: end, Payload: payload,
	})
}

// WritePaneGeometry delivers a committed geometry event on the same ordered
// stream as output, which is the only way a subscriber can see it in the
// position the journal committed it at.
func (effects *UnifiedDevPaneEffects) WritePaneGeometry(key unifiedjournal.PaneKey, event unifiedjournal.Event) error {
	if event.Kind != unifiedjournal.RecordGeometry || event.Sequence < 1 {
		return errors.New("invalid unified geometry event")
	}
	return effects.publishEvent(key, event)
}

// publishEvent fans one committed event out to every live subscriber. A
// subscriber that has already advanced past this sequence is skipped; one that
// is behind it has missed an event and is closed rather than fed a gap; one
// whose buffer is full is wedged and is evicted rather than blocking the
// publication path — publication holds the realm-wide subscriber lock, so a
// single stalled reader must never hold back every other pane's live tail.
// Both closures are one typed outcome, subscriber_lagged, delivered through
// closeSubscriberLocked: the attachment that owns the subscriber ends with
// that reason, the browser classifies it reconnectable, and the client is
// rebuilt from snapshot+tail — which includes the very event that evicted it.
func (effects *UnifiedDevPaneEffects) publishEvent(key unifiedjournal.PaneKey, event unifiedjournal.Event) error {
	effects.subscriberMu.Lock()
	defer effects.subscriberMu.Unlock()
	// Only admitted runtime generations call publishEvent. Record their last
	// publication under subscriberMu; rotation and unit reap remove the old
	// generation's entry while holding the same lock, so this remains bounded
	// without consulting provider authority on the dispatch hot path.
	if effects.publishedSequence == nil {
		effects.publishedSequence = make(map[unifiedjournal.PaneKey]int64)
	}
	if event.Sequence > effects.publishedSequence[key] {
		effects.publishedSequence[key] = event.Sequence
	}
	for subscriber := range effects.subscribers[key] {
		if subscriber.cursor >= event.Sequence {
			continue
		}
		if subscriber.cursor != event.Sequence-1 {
			effects.closeSubscriberLocked(key, subscriber, proto.SubscriberClosedLagged)
			continue
		}
		if len(subscriber.data) == cap(subscriber.data) || !subscriber.lease.reserveEvent(recordingEventBytes(event)) {
			effects.closeSubscriberLocked(key, subscriber, proto.SubscriberClosedLagged)
			continue
		}
		delivered := event
		if len(event.Payload) != 0 {
			delivered.Payload = make([]byte, len(event.Payload))
			copy(delivered.Payload, event.Payload)
		}
		select {
		case subscriber.data <- delivered:
			subscriber.cursor = event.Sequence
		case <-subscriber.done:
			subscriber.releaseEvent(delivered)
			if effects.removeSubscriberLocked(key, subscriber) {
				close(subscriber.data)
				subscriber.drainClosed()
			}
		default:
			subscriber.releaseEvent(delivered)
			effects.closeSubscriberLocked(key, subscriber, proto.SubscriberClosedLagged)
		}
	}
	return nil
}

// removeSubscriberLocked removes one subscriber without publishing a provider
// verdict. It is used only when the subscriber has already cancelled itself.
// Caller holds subscriberMu.
func (effects *UnifiedDevPaneEffects) removeSubscriberLocked(key unifiedjournal.PaneKey, subscriber *unifiedDevSubscriber) bool {
	bucket := effects.subscribers[key]
	if _, present := bucket[subscriber]; !present {
		return false
	}
	delete(bucket, subscriber)
	if len(bucket) == 0 {
		delete(effects.subscribers, key)
	}
	return true
}

// closeSubscriberLocked is the one way the provider removes a subscriber it
// did not merely see cancel itself. It records the typed reason and only then
// closes the tail, so the consumer that observes the close can read the reason
// without a lock and carry it to the browser. Caller holds subscriberMu. A
// subscriber that is no longer registered has already been closed by its own
// cancel and is left alone: the channel is closed at most once.
func (effects *UnifiedDevPaneEffects) closeSubscriberLocked(key unifiedjournal.PaneKey, subscriber *unifiedDevSubscriber, reason proto.SubscriberCloseReason) {
	if edge := effects.subscriberCloseEdge; edge != nil {
		edge(key, subscriber, reason)
	}
	if !effects.removeSubscriberLocked(key, subscriber) {
		return
	}
	if !proto.IsSubscriberCloseReason(reason) {
		// A reason outside the closed set would reach the browser as an
		// unknown code and dead-end there; never invent one at a call site.
		reason = proto.SubscriberClosedLagged
	}
	subscriber.closeTyped(reason)
}

// closeSubscribersLocked closes every present subscriber and unconditionally
// removes its bucket. The unconditional delete is load-bearing: cancellation
// may have emptied a bucket immediately before a fatal settlement. Caller
// holds subscriberMu.
func (effects *UnifiedDevPaneEffects) closeSubscribersLocked(key unifiedjournal.PaneKey, reason proto.SubscriberCloseReason) int {
	closed := 0
	for subscriber := range effects.subscribers[key] {
		effects.closeSubscriberLocked(key, subscriber, reason)
		closed++
	}
	delete(effects.subscribers, key)
	return closed
}

// closeSubscribers removes every subscriber of one pane key with one typed
// reason. This is the shape a deliberate, provider-initiated close takes —
// journal rotation closes a predecessor generation's subscribers with
// generation_rotated through exactly this call — and returns how many
// subscribers it ended.
func (effects *UnifiedDevPaneEffects) closeSubscribers(key unifiedjournal.PaneKey, reason proto.SubscriberCloseReason) int {
	effects.subscriberMu.Lock()
	defer effects.subscriberMu.Unlock()
	return effects.closeSubscribersLocked(key, reason)
}

// openSnapshotTail returns the committed event projection and a live tail of the
// same representation. Snapshot and tail cannot drift, because they are the same
// events read at one instant under one lock.
//
// The returned tail is the subscriber itself: its events() channel closes when
// either side removes it, and closeReason() then tells the consumer whether
// that was its own cancel (empty) or the provider's typed verdict, which the
// attachment must end with.
func (effects *UnifiedDevPaneEffects) openSnapshotTail(sessionID string) ([]unifiedjournal.Event, unifiedjournal.Geometry, *unifiedDevSubscriber, func(), error) {
	return effects.openSnapshotTailWithLease(sessionID, nil)
}

func (effects *UnifiedDevPaneEffects) openSnapshotTailWithLease(sessionID string, attachment *recordingReaderLease) ([]unifiedjournal.Event, unifiedjournal.Geometry, *unifiedDevSubscriber, func(), error) {
	releaseAttempt := func(lease *recordingReaderLease) {
		lease.releaseSnapshot()
		if attachment == nil {
			lease.detach()
		}
	}
	// Storage reads deliberately run without subscriberMu. journalMu remains
	// held through final registration, so every commit is either in the snapshot
	// or reaches the registered tail. The final section re-resolves active and
	// compares the publication head while every publisher and rotation swap is
	// excluded; a winner in either race retries instead of registering a gap.
	const registrationAttempts = 128
	for attempt := 0; attempt < registrationAttempts; attempt++ {
		effects.subscriberMu.Lock()
		effects.mu.Lock()
		key, ok := effects.active[sessionID]
		effects.mu.Unlock()
		effects.subscriberMu.Unlock()
		if !ok {
			return nil, unifiedjournal.Geometry{}, nil, nil, errors.New("unified target is not journal-ready")
		}
		if !effects.recordingReady(key) {
			// The active generation can swap between lookup and receipt check.
			// Retry against its successor instead of reporting a transient gap.
			effects.mu.Lock()
			current, present := effects.active[sessionID]
			effects.mu.Unlock()
			if present && current != key {
				continue
			}
			return nil, unifiedjournal.Geometry{}, nil, nil, errRecordingInitial
		}

		if edge := effects.snapshotReadEdge; edge != nil {
			edge()
		}
		effects.journalMu.Lock()
		var events []unifiedjournal.Event
		var lease *recordingReaderLease
		var err error
		if !effects.realm.UnifiedEligible(key) {
			err = unifiedjournal.ErrInvalidated
		} else {
			var bytes int64
			bytes, _, err = effects.realm.SnapshotAllocation(key)
			if err == nil {
				if attachment == nil {
					lease, err = effects.readers.acquire(bytes)
				} else {
					lease = attachment
					err = lease.reserveSnapshot(bytes)
				}
			}
			if err == nil {
				events, err = effects.realm.ReadCommittedEvents(key)
			}
		}
		var initial unifiedjournal.Geometry
		if err == nil {
			initial, err = effects.realm.InitialGeometry(key)
		}
		cursor := int64(0)
		if len(events) != 0 {
			cursor = events[len(events)-1].Sequence
		}
		if edge := effects.snapshotRegisterEdge; edge != nil {
			edge()
		}

		effects.subscriberMu.Lock()
		effects.mu.Lock()
		activeKey, active := effects.active[sessionID]
		rotatingOld := effects.rotation != nil && effects.rotation.session == sessionID && effects.rotation.oldKey == key
		effects.mu.Unlock()
		if err != nil {
			releaseAttempt(lease)
			effects.subscriberMu.Unlock()
			effects.journalMu.Unlock()
			if active && (activeKey != key || rotatingOld) {
				time.Sleep(time.Millisecond)
				continue
			}
			return nil, unifiedjournal.Geometry{}, nil, nil, err
		}
		if !active {
			releaseAttempt(lease)
			effects.subscriberMu.Unlock()
			effects.journalMu.Unlock()
			return nil, unifiedjournal.Geometry{}, nil, nil, errors.New("unified target is not journal-ready")
		}
		if activeKey != key || effects.publishedSequence[key] > cursor {
			releaseAttempt(lease)
			effects.subscriberMu.Unlock()
			effects.journalMu.Unlock()
			time.Sleep(time.Millisecond)
			continue
		}
		if !effects.recordingReady(key) {
			releaseAttempt(lease)
			effects.subscriberMu.Unlock()
			effects.journalMu.Unlock()
			if rotatingOld {
				time.Sleep(time.Millisecond)
				continue
			}
			return nil, unifiedjournal.Geometry{}, nil, nil, errRecordingInitial
		}
		subscriber := &unifiedDevSubscriber{lease: lease, cursor: cursor, data: make(chan unifiedjournal.Event, recordingTailSlots), done: make(chan struct{}), verdict: make(chan struct{})}
		if effects.subscribers[key] == nil {
			effects.subscribers[key] = make(map[*unifiedDevSubscriber]struct{})
		}
		effects.subscribers[key][subscriber] = struct{}{}
		effects.subscriberMu.Unlock()
		effects.journalMu.Unlock()
		cancel := func() {
			subscriber.once.Do(func() { close(subscriber.done) })
			effects.subscriberMu.Lock()
			if effects.removeSubscriberLocked(key, subscriber) {
				close(subscriber.data)
				subscriber.drainClosed()
			}
			subscriber.lease.detach()
			effects.subscriberMu.Unlock()
		}
		return events, initial, subscriber, cancel, nil
	}
	return nil, unifiedjournal.Geometry{}, nil, nil, errors.New("unified snapshot registration did not stabilize")
}
