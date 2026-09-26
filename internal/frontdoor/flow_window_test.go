package frontdoor

import "testing"

func TestFlowWindowAccountsCumulativeAcknowledgements(t *testing.T) {
	var w flowWindow
	if !w.open() {
		t.Fatal("an empty window is closed")
	}
	// Frames are admitted while the window is open; the one that crosses it
	// is the last until an acknowledgement arrives.
	frame := 48 << 10
	for w.open() {
		w.record(frame)
	}
	if w.sent != 3 || w.inflight != 3*frame {
		t.Fatalf("sent=%d inflight=%d, want 3 frames of %d", w.sent, w.inflight, frame)
	}
	for _, bad := range []uint64{0, 4, 1 << 63} {
		if w.ack(bad) {
			t.Fatalf("accepted acknowledgement %d with 3 sent and none acknowledged", bad)
		}
	}
	if !w.ack(1) || w.inflight != 2*frame || !w.open() {
		t.Fatalf("after ACK 1: inflight=%d open=%t", w.inflight, w.open())
	}
	if w.ack(1) {
		t.Fatal("accepted a repeated acknowledgement")
	}
	w.record(100)
	if !w.ack(4) || w.inflight != 0 || w.acked != 4 || w.head != 0 || len(w.sizes) != 0 {
		t.Fatalf("after ACK 4: inflight=%d acked=%d head=%d queued=%d", w.inflight, w.acked, w.head, len(w.sizes))
	}
	if w.ack(3) {
		t.Fatal("accepted an acknowledgement that goes back")
	}
}

// A long session never grows the queue beyond the frames in flight.
func TestFlowWindowQueueStaysBounded(t *testing.T) {
	var w flowWindow
	for i := 0; i < 100_000; i++ {
		w.record(64)
		w.record(64)
		if !w.ack(w.sent - 1) {
			t.Fatalf("round %d: acknowledgement refused", i)
		}
		if len(w.sizes)-w.head != 1 || cap(w.sizes) > 64 {
			t.Fatalf("round %d: %d queued of %d capacity", i, len(w.sizes)-w.head, cap(w.sizes))
		}
	}
}
