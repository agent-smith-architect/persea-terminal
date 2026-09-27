package frontdoor

import "time"

// FlowStallTimeout bounds how long attachment output may stay unacknowledged
// without any acknowledgement progress.
//
// The browser's liveness proof (BrowserProofTimeout) already ends a page whose
// link or scripts stop: its proof stops arriving. What that proof cannot see is
// a page that still proves liveness but no longer consumes output, for example
// one whose terminal stopped completing writes. Such a page holds its
// attachment, its control lease and a parked broker writer indefinitely. The
// clock starts when output becomes outstanding and restarts only when a valid
// acknowledgement advances; liveness, pings and socket write completion do not
// refresh it, and with nothing outstanding it is not armed at all. On a slow
// but working link an acknowledgement advances about once per attachment frame
// (a LIVE frame is at most 16 KiB of output, a few seconds even at a few KiB/s),
// and a link too slow for that already fails the liveness bound. The close is
// transient: a page that is consuming again reconnects. The close code is
// flow_stalled. The deadline is absolute: a browser message the relay handles
// after it has passed is not forwarded, and an acknowledgement arriving after
// it cannot restart the clock, even if the timer has not yet been serviced.
const FlowStallTimeout = 30 * time.Second

// flowStallClock arms the stall deadline while attachment output is
// outstanding. It is owned by the relay loop and needs no lock.
type flowStallClock struct {
	timer    *time.Timer
	timeout  time.Duration
	now      func() time.Time
	deadline time.Time
	armed    bool
}

func newFlowStallClock(timeout time.Duration, now func() time.Time) *flowStallClock {
	timer := time.NewTimer(timeout)
	stopTimer(timer)
	return &flowStallClock{timer: timer, timeout: timeout, now: now}
}

// outstanding starts the clock when output becomes outstanding; while it runs,
// more output does not restart it.
func (c *flowStallClock) outstanding() {
	if !c.armed {
		c.timer.Reset(c.timeout)
		c.deadline = c.now().Add(c.timeout)
		c.armed = true
	}
}

// expired reports whether the running deadline had passed at instant, which
// the caller read from the same clock.
func (c *flowStallClock) expired(instant time.Time) bool {
	return c.armed && !instant.Before(c.deadline)
}

// progressed restarts the clock after an acknowledgement advanced, or stops it
// when nothing is outstanding any more.
func (c *flowStallClock) progressed(outstanding bool) {
	stopTimer(c.timer)
	c.armed = false
	if outstanding {
		c.outstanding()
	}
}

func (c *flowStallClock) stop() {
	stopTimer(c.timer)
	c.armed = false
}

// ConsumptionReceiptInterval bounds how often a Control page's flow
// acknowledgements are forwarded to the broker as consumption receipts, which
// the broker uses to refuse input from a page far behind the session. An
// acknowledgement that empties the window is always forwarded, so the broker's
// view never stays behind a page that has consumed everything relayed, and one
// held back is forwarded before the page's next input, so input is always
// judged on what the page had consumed when it was typed. Otherwise a skipped
// acknowledgement is covered by a later one, or the page is ended by
// FlowStallTimeout.
const ConsumptionReceiptInterval = 100 * time.Millisecond
