package broker

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"persea-terminal/internal/proto"
)

// Results leave in frame order: a frame settled early waits for every frame
// before it, and consecutive frames with one result become one run.
func TestInputLedgerReportsSettledPrefixInRuns(t *testing.T) {
	l := newInputLedger()
	for want := uint64(1); want <= 6; want++ {
		if id, ok := l.receive(); !ok || id != want {
			t.Fatalf("receive = %d %t, want %d", id, ok, want)
		}
	}
	// Frame 2 was refused at admission while frame 1 is still in the PTY.
	l.settle(2, inputPaused)
	if runs := l.take(); len(runs) != 0 {
		t.Fatalf("reported %v ahead of an unsettled frame", runs)
	}
	l.settle(1, inputWritten)
	l.settle(3, inputPaused)
	l.settle(5, inputWritten)
	want := []inputRun{{1, inputWritten}, {3, inputPaused}}
	if runs := l.take(); !reflect.DeepEqual(runs, want) {
		t.Fatalf("runs = %v, want %v", runs, want)
	}
	// The first outcome stands; reported or unknown frames are ignored.
	l.settle(4, inputWritten)
	l.settle(4, inputDropped)
	l.settle(1, inputDropped)
	l.settle(7, inputWritten)
	l.settle(6, inputPartial)
	want = []inputRun{{5, inputWritten}, {6, inputPartial}}
	if runs := l.take(); !reflect.DeepEqual(runs, want) {
		t.Fatalf("runs = %v, want %v", runs, want)
	}
	if runs := l.take(); len(runs) != 0 {
		t.Fatalf("reported twice: %v", runs)
	}
}

// Each settlement wakes the reporter; a wake that is already pending covers
// it.
func TestInputLedgerWakesTheReporter(t *testing.T) {
	l := newInputLedger()
	id, _ := l.receive()
	l.settle(id, inputWritten)
	select {
	case <-l.wake:
	default:
		t.Fatal("settle did not wake the reporter")
	}
	l.settle(id, inputWritten)
	select {
	case <-l.wake:
		t.Fatal("a repeated settlement woke the reporter")
	default:
	}
}

// Frames whose result has not reached the front door are bounded, whether
// they wait for a PTY write or for the reporter.
func TestInputLedgerBoundsTheBacklog(t *testing.T) {
	l := newInputLedger()
	for i := 0; i < maxInputBacklog; i++ {
		id, ok := l.receive()
		if !ok {
			t.Fatalf("frame %d refused below the bound", i+1)
		}
		if id > 1 {
			l.settle(id, inputPaused)
		}
	}
	if _, ok := l.receive(); ok {
		t.Fatal("backlog grew past its bound")
	}
	l.settle(1, inputWritten)
	runs := l.take()
	if len(runs) != 2 || runs[1].through != maxInputBacklog {
		t.Fatalf("runs = %v", runs)
	}
	// Results the reporter holds still count until they are written.
	if _, ok := l.receive(); ok {
		t.Fatal("backlog cleared before its results were written")
	}
	l.wrote(runs[1].through)
	if id, ok := l.receive(); !ok || id != maxInputBacklog+1 {
		t.Fatalf("receive after the results were written = %d %t", id, ok)
	}
}

func TestInputResultCodesMatchTheWire(t *testing.T) {
	for result, code := range map[inputResult]string{
		inputWritten: "", inputPaused: "input_paused", inputRefused: "input_refused",
		inputDropped: "input_dropped", inputPartial: "input_partial",
	} {
		if result.code() != code {
			t.Fatalf("%d code %q, want %q", result, result.code(), code)
		}
	}
}

// The reporter writes settled results in order and counts them written; a
// result it cannot write ends the attachment.
func TestInputReporterWritesResultsAndEndsOnFailure(t *testing.T) {
	l := newInputLedger()
	writes := make(chan proto.Control, 4)
	fail := make(chan struct{})
	ended := make(chan struct{})
	done := make(chan struct{})
	defer close(done)
	write := func(c proto.Control) error {
		select {
		case <-fail:
			return errors.New("socket gone")
		default:
		}
		writes <- c
		return nil
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		l.report(nil, done, write, func() { close(ended) })
	}()
	for i := 0; i < 3; i++ {
		l.receive()
	}
	l.settle(1, inputWritten)
	l.settle(2, inputPaused)
	for _, want := range []proto.Control{{Type: "input", Frames: 1}, {Type: "input", Frames: 2, Code: "input_paused"}} {
		select {
		case got := <-writes:
			if got.Type != want.Type || got.Frames != want.Frames || got.Code != want.Code {
				t.Fatalf("wrote %+v, want %+v", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("result %+v never written", want)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		l.mu.Lock()
		written := l.written
		l.mu.Unlock()
		if written == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("written = %d, want 2", written)
		}
		time.Sleep(time.Millisecond)
	}
	close(fail)
	l.settle(3, inputWritten)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("a result that could not be written did not end the attachment")
	}
	<-finished
}
