package terminal

import (
	"context"
	"errors"
	"sync"
)

const (
	MaxInputFrames = 64
	MaxInputBytes  = 256 * 1024
)

// InterruptiblePTYWriter is the PTY seam. Every call receives a fresh,
// rearmable context; cancellation revokes only that mode generation. Close
// releases writer-owned wake resources and must not close the source PTY.
type InterruptiblePTYWriter interface {
	WriteContext(context.Context, []byte) (int, error)
	Close() error
}

type inputItem struct {
	data       []byte
	generation uint64
}

// inputPump owns both its queue and its aggregate byte count under mu. bytes
// includes the current in-flight item until that write returns, even after
// revocation, so dequeue, Observe, and regrant cannot manufacture capacity.
type inputPump struct {
	source string
	epoch  uint64
	writer InterruptiblePTYWriter
	wake   chan struct{}
	stop   chan struct{}
	done   chan struct{}
	fail   chan error

	mu            sync.Mutex
	queue         []inputItem
	closed        bool
	mode          Mode
	generation    uint64
	bytes         int
	currentCancel context.CancelFunc
	runErr        error
	stopOnce      sync.Once
	closeOnce     sync.Once
	closeErr      error
}

func newInputPump(source string, epoch uint64, writer InterruptiblePTYWriter) (*inputPump, error) {
	if source == "" || epoch == 0 || writer == nil {
		return nil, ErrMalformed
	}
	p := &inputPump{
		source: source, epoch: epoch, writer: writer,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}), fail: make(chan error, 1),
		mode: ModeObserve, generation: 1,
	}
	go p.run()
	return p, nil
}

func (p *inputPump) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *inputPump) setMode(mode Mode) error {
	if mode != ModeObserve && mode != ModeControl {
		return ErrMalformed
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	p.mode = mode
	var cancel context.CancelFunc
	if mode == ModeObserve {
		p.generation++
		p.discardQueuedLocked()
		cancel = p.currentCancel
	}
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.signal()
	return nil
}

func (p *inputPump) enqueue(source string, epoch uint64, data []byte) error {
	return p.enqueueBatch(source, epoch, [][]byte{data})
}

// enqueueBatch reserves and publishes a whole admitted browser batch under one
// lock. A cut transition therefore cannot deliver a prefix and then discover
// that the remainder exceeds the PTY queue.
func (p *inputPump) enqueueBatch(source string, epoch uint64, batch [][]byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkBatchLocked(source, epoch, batch); err != nil {
		return err
	}
	for _, data := range batch {
		item := inputItem{data: append([]byte(nil), data...), generation: p.generation}
		p.queue = append(p.queue, item)
		p.bytes += len(item.data)
	}
	p.signal()
	return nil
}

func (p *inputPump) canEnqueueBatch(source string, epoch uint64, batch [][]byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checkBatchLocked(source, epoch, batch)
}

func (p *inputPump) checkBatchLocked(source string, epoch uint64, batch [][]byte) error {
	if p.closed {
		return ErrClosed
	}
	if source != p.source || epoch != p.epoch {
		return ErrStale
	}
	if p.mode != ModeControl {
		return ErrObserveOnly
	}
	bytes := 0
	for _, data := range batch {
		if len(data) == 0 {
			return ErrMalformed
		}
		if len(data) > MaxInputBytes-bytes {
			return ErrSaturated
		}
		bytes += len(data)
	}
	if len(batch) > MaxInputFrames-len(p.queue) || bytes > MaxInputBytes-p.bytes {
		return ErrSaturated
	}
	return nil
}

func (p *inputPump) failures() <-chan error { return p.fail }

// seal revokes input admission without waiting for the active writer. That
// writer keeps its bytes and resources until sealAndStop joins its return.
func (p *inputPump) seal() {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.closed, p.mode = true, ModeObserve
		p.generation++
		p.discardQueuedLocked()
		cancel := p.currentCancel
		close(p.stop)
		p.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		p.signal()
	})
}

func (p *inputPump) sealAndStop() error {
	p.seal()
	<-p.done
	p.closeOnce.Do(func() { p.closeErr = p.writer.Close() })
	p.mu.Lock()
	defer p.mu.Unlock()
	return errors.Join(p.runErr, p.closeErr)
}

func (p *inputPump) discardQueuedLocked() {
	for i := range p.queue {
		p.bytes -= len(p.queue[i].data)
		clear(p.queue[i].data)
	}
	p.queue = nil
}

func (p *inputPump) failLocked(err error) {
	if p.runErr == nil {
		p.runErr = err
	}
	p.closed, p.mode = true, ModeObserve
	p.generation++
	p.discardQueuedLocked()
	select {
	case p.fail <- err:
	default:
	}
}

func (p *inputPump) finishItemLocked(item *inputItem) {
	p.bytes -= len(item.data)
	if p.bytes < 0 {
		panic("terminal: negative input accounting")
	}
	p.currentCancel = nil
	clear(item.data)
}

func (p *inputPump) run() {
	defer close(p.done)
	for {
		select {
		case <-p.stop:
			return
		case <-p.wake:
		}
		for {
			p.mu.Lock()
			if p.closed || len(p.queue) == 0 {
				p.mu.Unlock()
				break
			}
			item := p.queue[0]
			p.queue[0] = inputItem{}
			p.queue = p.queue[1:]
			if p.mode != ModeControl || item.generation != p.generation {
				p.finishItemLocked(&item)
				p.mu.Unlock()
				continue
			}
			ctx, cancel := context.WithCancel(context.Background())
			p.currentCancel = cancel
			generation := p.generation
			p.mu.Unlock()

			written := 0
			var writeErr error
			for written < len(item.data) {
				if err := ctx.Err(); err != nil {
					writeErr = err
					break
				}
				n, err := p.writer.WriteContext(ctx, item.data[written:])
				if n < 0 || n > len(item.data)-written {
					err, n = ErrMalformed, 0
				}
				written += n
				if err == nil && n == 0 {
					err = ErrNoProgress
				}
				if err != nil {
					writeErr = err
					break
				}
			}

			p.mu.Lock()
			revoked := ctx.Err() != nil || p.closed || p.mode != ModeControl || p.generation != generation
			p.finishItemLocked(&item)
			if writeErr != nil && !revoked {
				p.failLocked(writeErr)
			}
			p.mu.Unlock()
			cancel()
			if writeErr != nil && !revoked {
				return
			}
		}
	}
}
