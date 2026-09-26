package frontdoor

// FlowWindowBytes bounds the attachment bytes the front door keeps in flight
// to one browser before the browser acknowledges them.
//
// A WebSocket write completes once its bytes reach kernel and proxy buffers,
// so without a bound a slow link queues everything the broker has sent ahead
// of the next liveness PONG and WebSocket ping, and a browser that is merely
// slow is declared dead. With the window, a control frame waits behind at
// most this window plus one attachment frame (a LIVE frame carries at most
// 16 KiB of output, about 22 KiB encoded): at 32 KiB/s that is about 4.7 s,
// inside the browser's 10 s liveness challenge and the front door's 20 s
// WebSocket ping cycle, with room for link overhead and delay. On a fast
// link the window caps output at one window per round trip, far above what
// an interactive terminal needs. When the browser falls behind, the front
// door stops reading the broker, and the broker's byte-bounded subscriber
// tail ends the view with its typed lag verdict rather than buffering without
// end. That verdict queues behind the window too; the broker's close grace
// (unifiedSubscriberCloseGrace) is sized so it still gets through at 32 KiB/s.
const FlowWindowBytes = 128 << 10

// flowWindow tracks the attachment frames written to one WebSocket that the
// browser has not acknowledged yet. It is owned by the relay loop and needs
// no lock. While the window is open at most one frame is admitted past it,
// so the queue holds at most FlowWindowBytes of frames plus one.
type flowWindow struct {
	sizes    []int
	head     int
	sent     uint64
	acked    uint64
	inflight int
}

// open reports whether the relay may take another broker frame.
func (w *flowWindow) open() bool {
	return w.inflight < FlowWindowBytes
}

// record counts one attachment frame the relay has written.
func (w *flowWindow) record(size int) {
	w.sizes = append(w.sizes, size)
	w.sent++
	w.inflight += size
}

// ack applies a cumulative acknowledgement. It must acknowledge at least one
// frame not acknowledged before and no frame that was never sent: a count
// that repeats, goes back or runs ahead is a protocol violation.
func (w *flowWindow) ack(count uint64) bool {
	if count <= w.acked || count > w.sent {
		return false
	}
	for ; w.acked < count; w.acked++ {
		w.inflight -= w.sizes[w.head]
		w.head++
	}
	if w.head == len(w.sizes) {
		w.sizes, w.head = w.sizes[:0], 0
	} else if w.head > len(w.sizes)/2 {
		w.sizes = append(w.sizes[:0], w.sizes[w.head:]...)
		w.head = 0
	}
	return true
}
