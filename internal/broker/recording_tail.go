package broker

import (
	"sync"

	"persea-terminal/internal/unifiedjournal"
)

// Nodes are charged before allocation. A linked queue avoids unused array
// capacity and keeps every geometry and sequence boundary intact.
type recordingTailNode struct {
	event unifiedjournal.Event
	next  *recordingTailNode
}

type recordingTailQueue struct {
	mu         sync.Mutex
	head, last *recordingTailNode
	count      int
	closed     bool
	// catchingUp marks a subscriber that publication no longer copies to: its
	// consumer drains what is queued and then reads the rest from the journal.
	// The publisher sets it under subscriberMu; only the consumer clears it,
	// under journalMu and subscriberMu, when it rejoins at the committed frontier.
	catchingUp bool
	wake       chan struct{}
}

// recordingTailState is what next found: an event, a caught-up-from-journal
// turn, or the closed tail.
type recordingTailState int

const (
	recordingTailEvent recordingTailState = iota
	recordingTailCatchUp
	recordingTailClosed
)

func newRecordingTailQueue() *recordingTailQueue {
	return &recordingTailQueue{wake: make(chan struct{}, 1)}
}

func (q *recordingTailQueue) push(event unifiedjournal.Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		panic("recording: publication after tail close")
	}
	node := &recordingTailNode{event: event}
	if q.last == nil {
		q.head = node
	} else {
		q.last.next = node
	}
	q.last = node
	q.count++
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *recordingTailQueue) pop() (unifiedjournal.Event, bool) {
	for {
		q.mu.Lock()
		if q.head != nil {
			event := q.takeLocked()
			q.mu.Unlock()
			return event, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return unifiedjournal.Event{}, false
		}
		<-q.wake
	}
}

// takeLocked removes the head event, leaving wake armed exactly while events
// remain. Caller holds mu and has seen a head.
func (q *recordingTailQueue) takeLocked() unifiedjournal.Event {
	node := q.head
	q.head = node.next
	node.next = nil
	q.count--
	if q.head == nil {
		q.last = nil
	}
	if !q.closed {
		select {
		case <-q.wake:
		default:
		}
		if q.head != nil {
			select {
			case q.wake <- struct{}{}:
			default:
			}
		}
	}
	event := node.event
	node.event = unifiedjournal.Event{}
	return event
}

// next is the consumer's receive: a queued event first, then, once the queue
// is empty, a catch-up turn while publication is not copying to it, and
// otherwise it waits for publication or closure.
func (q *recordingTailQueue) next() (unifiedjournal.Event, recordingTailState) {
	for {
		q.mu.Lock()
		if q.head != nil {
			event := q.takeLocked()
			q.mu.Unlock()
			return event, recordingTailEvent
		}
		closed, catchingUp := q.closed, q.catchingUp
		q.mu.Unlock()
		switch {
		case closed:
			return unifiedjournal.Event{}, recordingTailClosed
		case catchingUp:
			return unifiedjournal.Event{}, recordingTailCatchUp
		}
		<-q.wake
	}
}

// enterCatchUp stops publication copying to this queue and wakes a consumer
// waiting on it, which then catches up from the journal.
func (q *recordingTailQueue) enterCatchUp() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.catchingUp {
		return
	}
	q.catchingUp = true
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *recordingTailQueue) isCatchingUp() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.catchingUp
}

func (q *recordingTailQueue) leaveCatchUp() {
	q.mu.Lock()
	q.catchingUp = false
	q.mu.Unlock()
}

func (q *recordingTailQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	close(q.wake)
}

func (q *recordingTailQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.count
}
