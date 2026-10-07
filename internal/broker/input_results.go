package broker

import (
	"sync"

	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

// Input results tell the page what became of each INPUT frame it sent on one
// attachment. Both ends number INPUT frames 1, 2, … in the order they cross
// the socket; the ledger records each frame's outcome as it becomes known —
// refused at admission, or reported by the input pump once its PTY write has
// finished — and the reporter sends them in frame order, coalesced into runs:
// "frames up to N were written", or "frames up to N were refused with code".
// Nothing is resent. A frame whose result the page never receives is
// uncertain, and the page says so.

type inputResult uint8

const (
	inputUnsettled inputResult = iota
	inputWritten
	inputPaused
	inputRefused
	inputDropped
	inputPartial
)

// code is the result's name on the wire; written has none.
func (r inputResult) code() string {
	switch r {
	case inputPaused:
		return "input_paused"
	case inputRefused:
		return "input_refused"
	case inputDropped:
		return "input_dropped"
	case inputPartial:
		return "input_partial"
	}
	return ""
}

func pumpInputResult(outcome terminal.InputOutcome) inputResult {
	switch outcome {
	case terminal.InputWritten:
		return inputWritten
	case terminal.InputPartial:
		return inputPartial
	}
	return inputDropped
}

// maxInputBacklog bounds the frames received whose result has not yet been
// written to the front door. Results wait behind a PTY write that has not
// returned, and behind output the front door is not reading; the bound is
// reached only when one of those stays stuck while the operator keeps typing.
// One result is written per frame when nothing is waiting, and the runs
// coalesce only while results wait.
const maxInputBacklog = 1024

type inputRun struct {
	through uint64
	result  inputResult
}

type inputLedger struct {
	mu       sync.Mutex
	received uint64
	// reported: results taken by the reporter; written: results it has
	// written to the front door.
	reported, written uint64
	// window[i] is the result of frame reported+1+i.
	window []inputResult
	wake   chan struct{}
}

func newInputLedger() *inputLedger {
	return &inputLedger{wake: make(chan struct{}, 1)}
}

// receive numbers the next INPUT frame. It fails when too many frames are
// still waiting for a result.
func (l *inputLedger) receive() (uint64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.received-l.written >= maxInputBacklog {
		return 0, false
	}
	l.received++
	l.window = append(l.window, inputUnsettled)
	return l.received, true
}

// settle records a frame's outcome; the first one stands. It never blocks, so
// the input pump may call it under its own lock.
func (l *inputLedger) settle(id uint64, result inputResult) {
	l.mu.Lock()
	settled := false
	if id > l.reported && id <= l.received {
		if slot := &l.window[id-l.reported-1]; *slot == inputUnsettled {
			*slot, settled = result, true
		}
	}
	l.mu.Unlock()
	if settled {
		select {
		case l.wake <- struct{}{}:
		default:
		}
	}
}

// wrote records that the results through `through` reached the front door.
func (l *inputLedger) wrote(through uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.written = through
}

// take removes the settled prefix and returns it as runs of one result.
func (l *inputLedger) take() []inputRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	var runs []inputRun
	n := 0
	for n < len(l.window) && l.window[n] != inputUnsettled {
		through := l.reported + uint64(n) + 1
		if last := len(runs) - 1; last >= 0 && runs[last].result == l.window[n] {
			runs[last].through = through
		} else {
			runs = append(runs, inputRun{through: through, result: l.window[n]})
		}
		n++
	}
	l.window = append(l.window[:0], l.window[n:]...)
	l.reported += uint64(n)
	return runs
}

// report writes pongs and input results until done closes. A pong that
// cannot be written stops it; a result that cannot be written also ends the
// attachment, because the page could no longer learn what became of its
// input.
func (l *inputLedger) report(pongs <-chan struct{}, done <-chan struct{}, write func(proto.Control) error, end func()) {
	for {
		select {
		case <-pongs:
			if write(proto.Control{Type: "pong"}) != nil {
				return
			}
		case <-l.wake:
			for _, run := range l.take() {
				if write(proto.Control{Type: "input", Frames: run.through, Code: run.result.code()}) != nil {
					end()
					return
				}
				l.wrote(run.through)
			}
		case <-done:
			return
		}
	}
}
