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
	wake       chan struct{}
}

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
