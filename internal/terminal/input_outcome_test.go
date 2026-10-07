package terminal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// scriptedWriter hands every PTY write to the test, which decides how many
// bytes it took and when it returns.
type scriptedWriter struct{ calls chan scriptedWrite }

type scriptedWrite struct {
	ctx   context.Context
	data  []byte
	reply chan scriptedReply
}

type scriptedReply struct {
	n   int
	err error
}

func (w *scriptedWriter) WriteContext(ctx context.Context, data []byte) (int, error) {
	call := scriptedWrite{ctx: ctx, data: data, reply: make(chan scriptedReply)}
	w.calls <- call
	r := <-call.reply
	return r.n, r.err
}

func (w *scriptedWriter) Close() error { return nil }

func (w *scriptedWriter) next(t *testing.T) scriptedWrite {
	t.Helper()
	select {
	case call := <-w.calls:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("no PTY write")
		return scriptedWrite{}
	}
}

type outcomeLog struct {
	mu   sync.Mutex
	seen map[uint64][]InputOutcome
}

func (l *outcomeLog) settle(id uint64, outcome InputOutcome) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[id] = append(l.seen[id], outcome)
}

func (l *outcomeLog) waitFor(t *testing.T, want map[uint64]InputOutcome) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		l.mu.Lock()
		done := len(l.seen) == len(want)
		for id, outcome := range want {
			if got := l.seen[id]; len(got) != 1 || got[0] != outcome {
				done = false
			}
		}
		snapshot := make(map[uint64][]InputOutcome, len(l.seen))
		for id, got := range l.seen {
			snapshot[id] = append([]InputOutcome(nil), got...)
		}
		l.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("outcomes = %v, want exactly one each of %v", snapshot, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func item(id uint64, data string) inputItem { return inputItem{id: id, data: []byte(data)} }

// Every admitted input is reported exactly once, by what reached the PTY:
// written in full, revoked part-way (partial), or discarded before its write
// began (dropped) — including by a seal.
func TestInputPumpReportsOneOutcomePerAdmittedInput(t *testing.T) {
	w := &scriptedWriter{calls: make(chan scriptedWrite)}
	log := &outcomeLog{seen: map[uint64][]InputOutcome{}}
	p, err := newInputPump("src", 1, w, log.settle)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.setMode(ModeControl); err != nil {
		t.Fatal(err)
	}
	if err := p.enqueueBatch("src", 1, []inputItem{item(1, "abc"), item(2, "d"), item(3, "e")}); err != nil {
		t.Fatal(err)
	}
	first := w.next(t)
	// Control is revoked while the first write is in the PTY: the queue is
	// discarded and the write is cancelled after one byte.
	if err := p.setMode(ModeObserve); err != nil {
		t.Fatal(err)
	}
	<-first.ctx.Done()
	first.reply <- scriptedReply{n: 1, err: first.ctx.Err()}
	log.waitFor(t, map[uint64]InputOutcome{1: InputPartial, 2: InputDropped, 3: InputDropped})

	if err := p.setMode(ModeControl); err != nil {
		t.Fatal(err)
	}
	if err := p.enqueue("src", 1, item(4, "fg")); err != nil {
		t.Fatal(err)
	}
	// A write that takes its bytes in two calls is written only once both
	// have returned.
	call := w.next(t)
	call.reply <- scriptedReply{n: 1}
	call = w.next(t)
	if got := string(call.data); got != "g" {
		t.Fatalf("second write = %q, want the remainder", got)
	}
	log.mu.Lock()
	early := len(log.seen[4])
	log.mu.Unlock()
	if early != 0 {
		t.Fatal("input reported before its write completed")
	}
	call.reply <- scriptedReply{n: 1}
	log.waitFor(t, map[uint64]InputOutcome{1: InputPartial, 2: InputDropped, 3: InputDropped, 4: InputWritten})

	// A seal drops what is queued; a write already in the PTY that completes
	// in full is still written.
	if err := p.enqueueBatch("src", 1, []inputItem{item(5, "h"), item(6, "i")}); err != nil {
		t.Fatal(err)
	}
	call = w.next(t)
	p.seal()
	call.reply <- scriptedReply{n: 1}
	if err := p.sealAndStop(); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, map[uint64]InputOutcome{1: InputPartial, 2: InputDropped, 3: InputDropped, 4: InputWritten, 5: InputWritten, 6: InputDropped})
}

// A write that fails on its own — no revocation — or reports an impossible
// count is decided by what it delivered; an impossible count leaves that
// unknown (partial), never "nothing".
func TestInputPumpSettlesFailedWritesByWhatTheyDelivered(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replies []scriptedReply
		want    InputOutcome
	}{
		{"fails before any byte", []scriptedReply{{n: 0, err: errors.New("pty gone")}}, InputDropped},
		{"fails after a byte", []scriptedReply{{n: 1}, {n: 0, err: errors.New("pty gone")}}, InputPartial},
		{"count beyond the data", []scriptedReply{{n: 9}}, InputPartial},
		{"negative count", []scriptedReply{{n: -1}}, InputPartial},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &scriptedWriter{calls: make(chan scriptedWrite)}
			log := &outcomeLog{seen: map[uint64][]InputOutcome{}}
			p, err := newInputPump("src", 1, w, log.settle)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.setMode(ModeControl); err != nil {
				t.Fatal(err)
			}
			if err := p.enqueueBatch("src", 1, []inputItem{item(1, "ab"), item(2, "c")}); err != nil {
				t.Fatal(err)
			}
			for _, reply := range tc.replies {
				w.next(t).reply <- reply
			}
			// The failure ends the pump; what was still queued never ran.
			select {
			case <-p.failures():
			case <-time.After(5 * time.Second):
				t.Fatal("the failed write did not fail the pump")
			}
			log.waitFor(t, map[uint64]InputOutcome{1: tc.want, 2: InputDropped})
			_ = p.sealAndStop()
		})
	}
}
