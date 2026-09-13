package terminal

import (
	"sync"
	"time"
)

// Clock and Timer are the only time seam used by production and tests.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(time.Duration) bool
}

type RealClock struct{}

func (RealClock) Now() time.Time                 { return time.Now() }
func (RealClock) NewTimer(d time.Duration) Timer { return &realTimer{timer: time.NewTimer(d)} }

type realTimer struct{ timer *time.Timer }

func (t *realTimer) C() <-chan time.Time        { return t.timer.C }
func (t *realTimer) Stop() bool                 { return t.timer.Stop() }
func (t *realTimer) Reset(d time.Duration) bool { return t.timer.Reset(d) }

// generationLease rejects async completion after an epoch has sealed. It is
// private so adapters cannot manufacture or advance owner generations.
type generationLease struct {
	source     string
	epoch      uint64
	generation uint64
}

func (l generationLease) validate(other generationLease) error {
	if l.source == "" || l.epoch == 0 || l.generation == 0 || l != other {
		return ErrStale
	}
	return nil
}

type schedulerCommand struct {
	kind  byte
	retry bool
	ack   chan error
}

// scheduler owns dirty/in-flight state. A due notification is never dropped:
// inFlight remains true until Epoch reports COMMIT, DEFER, or failure.
type scheduler struct {
	clock    Clock
	quiet    time.Duration
	maximum  time.Duration
	commands chan schedulerCommand
	due      chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func newScheduler(clock Clock, quiet, maximum time.Duration) (*scheduler, error) {
	if clock == nil || quiet <= 0 || maximum < quiet {
		return nil, ErrMalformed
	}
	s := &scheduler{
		clock: clock, quiet: quiet, maximum: maximum,
		commands: make(chan schedulerCommand), due: make(chan struct{}, 1), done: make(chan struct{}),
	}
	go s.run()
	return s, nil
}

func (s *scheduler) command(kind byte, retry bool) error {
	ack := make(chan error, 1)
	select {
	case <-s.done:
		return ErrClosed
	case s.commands <- schedulerCommand{kind: kind, retry: retry, ack: ack}:
	}
	return <-ack
}

func (s *scheduler) Activity() error           { return s.command('a', false) }
func (s *scheduler) BeginExternal() error      { return s.command('b', false) }
func (s *scheduler) Complete(retry bool) error { return s.command('c', retry) }
func (s *scheduler) Due() <-chan struct{}      { return s.due }

func (s *scheduler) Stop() {
	s.stopOnce.Do(func() { _ = s.command('s', false) })
	<-s.done
}

func (s *scheduler) run() {
	defer close(s.done)
	var timer Timer
	var timerC <-chan time.Time
	var dirty, inFlight bool
	var first, latest time.Time

	stopTimer := func() {
		if timer != nil {
			timer.Stop()
		}
		timerC = nil
	}
	arm := func() {
		stopTimer()
		if !dirty || inFlight {
			return
		}
		now := s.clock.Now()
		quietDue, maxDue := latest.Add(s.quiet), first.Add(s.maximum)
		due := quietDue
		if maxDue.Before(due) {
			due = maxDue
		}
		d := due.Sub(now)
		if d < 0 {
			d = 0
		}
		if timer == nil {
			timer = s.clock.NewTimer(d)
		} else {
			timer.Reset(d)
		}
		timerC = timer.C()
	}

	for {
		select {
		case cmd := <-s.commands:
			switch cmd.kind {
			case 'a':
				now := s.clock.Now()
				if !dirty {
					dirty, first = true, now
				}
				latest = now
				arm()
				cmd.ack <- nil
			case 'b':
				if inFlight {
					cmd.ack <- ErrOutOfState
					continue
				}
				inFlight = true
				arm()
				cmd.ack <- nil
			case 'c':
				if !inFlight {
					cmd.ack <- ErrOutOfState
					continue
				}
				inFlight = false
				if cmd.retry && !dirty {
					now := s.clock.Now()
					dirty, first, latest = true, now, now
				}
				arm()
				cmd.ack <- nil
			case 's':
				stopTimer()
				cmd.ack <- nil
				return
			default:
				cmd.ack <- ErrMalformed
			}
		case <-timerC:
			timerC = nil
			if dirty && !inFlight {
				dirty, inFlight = false, true
				s.due <- struct{}{}
			}
		}
	}
}
