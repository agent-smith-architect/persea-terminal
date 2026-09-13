package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const (
	MaxEgressFrames     = 64
	MaxEgressBytes      = 512 * 1024
	MaxEgressQueueBytes = 16 * 1024 * 1024
)

// FrameWriter is the transport seam. Every write receives the same egress-owned
// context. Cancellation must wake the current write and remain observable by
// every later write. Close must honor its bounded context.
type FrameWriter interface {
	WriteFrame(context.Context, []byte) error
	Close(context.Context) error
}

// outputOwner is an explicit pairing: this writer supplies authoritative replay
// and live output itself. Epoch still owns the complete control/marker protocol.
// Only that writer can opt in; a configuration flag cannot suppress output on a
// transport that has no replacement source.
type outputOwner interface {
	FrameWriter
	OwnsTerminalOutput()
}

// egress is the sole FIFO and sender. Its mutex owns queue admission, dequeue,
// and aggregate queued/in-flight accounting. Closing drains admitted frames;
// a canceled finish interrupts a blocked writer and remains bounded.
type egress struct {
	writer           FrameWriter
	writerOwnsOutput bool
	wake             chan struct{}
	stopped          chan struct{}
	fail             chan error
	writeCtx         context.Context
	cancelWrite      context.CancelFunc
	drainBound       time.Duration

	mu          sync.Mutex
	queue       [][]byte
	queuedBytes int
	inFlight    bool
	closed      bool
	closing     bool
	writeErr    error
	drainErr    error
	stopOnce    sync.Once
	finalErr    error
}

func newEgress(writer FrameWriter, drainBound time.Duration) (*egress, error) {
	if writer == nil || drainBound <= 0 {
		return nil, ErrMalformed
	}
	writeCtx, cancelWrite := context.WithCancel(context.Background())
	e := &egress{
		writer: writer, wake: make(chan struct{}, 1), stopped: make(chan struct{}), fail: make(chan error, 1),
		writeCtx: writeCtx, cancelWrite: cancelWrite, drainBound: drainBound,
	}
	_, e.writerOwnsOutput = writer.(outputOwner)
	go e.pump()
	return e, nil
}

func (e *egress) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *egress) failures() <-chan error { return e.fail }
func (e *egress) enqueue(f Frame) error  { return e.enqueueBatch([]Frame{f}) }

// enqueueBatch admits every frame or none, preserving release fences.
func (e *egress) enqueueBatch(frames []Frame) error {
	if len(frames) == 0 || len(frames) > MaxEgressFrames {
		return ErrMalformed
	}
	var payloads [][]byte
	total := 0
	for _, f := range frames {
		if err := f.Validate(); err != nil {
			return err
		}
		if e.writerOwnsOutput && f.Type == FrameLive {
			continue
		}
		b, err := json.Marshal(f)
		if err != nil {
			return err
		}
		payloads = append(payloads, b)
		total += len(b)
	}
	e.mu.Lock()
	framesOwned := len(e.queue)
	if e.inFlight {
		framesOwned++
	}
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	if len(payloads) == 0 {
		e.mu.Unlock()
		return nil
	}
	if total > MaxEgressQueueBytes-e.queuedBytes || framesOwned+len(payloads) > MaxEgressFrames {
		e.mu.Unlock()
		return ErrSaturated
	}
	e.queue = append(e.queue, payloads...)
	e.queuedBytes += total
	e.mu.Unlock()
	e.signal()
	return nil
}

func (e *egress) finishAndWait() error {
	e.stopOnce.Do(func() {
		e.mu.Lock()
		e.closed, e.closing = true, true
		e.mu.Unlock()
		e.signal()
		timer := time.NewTimer(e.drainBound)
		select {
		case <-e.stopped:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			e.mu.Lock()
			e.drainErr = errors.New("egress drain deadline exceeded")
			e.mu.Unlock()
			e.cancelWrite()
			<-e.stopped
		}
		closeCtx, cancelClose := context.WithTimeout(context.Background(), e.drainBound)
		closeErr := e.writer.Close(closeCtx)
		cancelClose()
		e.cancelWrite()
		e.mu.Lock()
		e.finalErr = errors.Join(e.writeErr, e.drainErr, closeErr)
		e.mu.Unlock()
	})
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.finalErr
}

func (e *egress) pump() {
	defer close(e.stopped)
	for {
		select {
		case <-e.wake:
		}
		for {
			e.mu.Lock()
			if len(e.queue) == 0 {
				closing := e.closing
				e.mu.Unlock()
				if closing {
					return
				}
				break
			}
			payload := e.queue[0]
			e.queue[0] = nil
			e.queue = e.queue[1:]
			e.inFlight = true
			e.mu.Unlock()

			err := e.writer.WriteFrame(e.writeCtx, payload)
			e.mu.Lock()
			e.queuedBytes -= len(payload)
			e.inFlight = false
			clear(payload)
			if err != nil {
				e.writeErr = err
				e.closed, e.closing = true, true
				for i := range e.queue {
					e.queuedBytes -= len(e.queue[i])
					clear(e.queue[i])
				}
				e.queue = nil
			}
			e.mu.Unlock()
			if err != nil {
				select {
				case e.fail <- err:
				default:
				}
				return
			}
		}
	}
}
