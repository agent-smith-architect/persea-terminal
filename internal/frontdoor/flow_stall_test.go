package frontdoor

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"persea-terminal/internal/attachmentwire"
)

// stallClient opens a control attachment and consumes its initial frames, so
// that nothing is outstanding when a test starts.
func stallClient(t *testing.T, label string, timeout time.Duration) (*fakeAttachBroker, *Server, *flowClient) {
	t.Helper()
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	front.flowStallTimeout = timeout
	handle, err := front.handles.mint(flowAuthority(label))
	if err != nil {
		t.Fatal(err)
	}
	client := &flowClient{t: t, ws: dialTerminalWS(t, front, addr, handle, "control")}
	t.Cleanup(func() { _ = client.ws.Close() })
	for !bytes.Equal(client.data, []byte("ready")) {
		client.next()
	}
	client.ack(client.frames)
	return broker, front, client
}

// proveLivenessUntil keeps answering the front door's liveness contract for
// the whole window, one PING per tick, and returns the close reason if the
// attachment ends first ("" if it stays open). Attachment frames that arrive
// meanwhile are counted, never acknowledged.
func proveLivenessUntil(t *testing.T, client *flowClient, window time.Duration) (string, time.Time) {
	t.Helper()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	end := time.Now().Add(window)
	for i := 0; time.Now().Before(end); i++ {
		nonce := fmt.Sprintf("%032x", i)
		writeApplicationPing(t, client.ws, nonce)
		for {
			_ = client.ws.SetReadDeadline(time.Now().Add(3 * time.Second))
			kind, payload, err := client.ws.ReadMessage()
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				return closeErr.Text, time.Now()
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind == websocket.TextMessage && string(payload) == attachmentwire.TransportLivenessPrefix+"PONG "+nonce {
				break
			}
			client.frames++
		}
		<-tick.C
	}
	return "", time.Now()
}

// A page that keeps proving liveness but stops acknowledging output loses its
// attachment FlowStallTimeout after output became outstanding, or after its
// last acknowledgement progress while output is still outstanding, even when
// that output is far smaller than the flow window, and its lease is released.
func TestWebSocketFlowStallEndsAPageThatStopsAcknowledging(t *testing.T) {
	const timeout = 400 * time.Millisecond
	cases := map[string]func(*fakeAttachBroker, *flowClient){
		"new output never acknowledged": func(broker *fakeAttachBroker, client *flowClient) {
			flowOutput(t, broker, 0, 1<<10)
		},
		"progress, then no more": func(broker *fakeAttachBroker, client *flowClient) {
			flowOutput(t, broker, 0, 1<<10)
			flowOutput(t, broker, 1, 1<<10)
			client.next()
			client.next()
			// Progress on the first output, while the second stays outstanding.
			client.ack(client.frames - 1)
		},
	}
	for name, stop := range cases {
		t.Run(name, func(t *testing.T) {
			broker, front, client := stallClient(t, "flow-stall", timeout)
			started := time.Now()
			stop(broker, client)
			reason, closedAt := proveLivenessUntil(t, client, 10*timeout)
			if reason != "flow_stalled" {
				t.Fatalf("close reason=%q, want flow_stalled while output stayed unacknowledged", reason)
			}
			if elapsed := closedAt.Sub(started); elapsed < timeout {
				t.Fatalf("closed after %v, before the %v stall bound", elapsed, timeout)
			}
			waitForNoLease(t, front.leases, flowAuthority("flow-stall"))
		})
	}
}

// Acknowledgement progress restarts the clock: a page that keeps consuming,
// however slowly, keeps its attachment far beyond one stall bound.
func TestWebSocketFlowStallRestartsOnAcknowledgementProgress(t *testing.T) {
	const timeout = 400 * time.Millisecond
	broker, _, client := stallClient(t, "flow-stall-progress", timeout)
	want := append([]byte(nil), client.data...)
	const outputs, size = 12, 16 << 10
	for i := 0; i < outputs; i++ {
		want = append(want, flowOutput(t, broker, i, size)...)
	}
	pace := time.NewTicker(timeout / 3)
	defer pace.Stop()
	started := time.Now()
	for len(client.data) < len(want) {
		client.next()
		<-pace.C
		client.ack(client.frames)
	}
	if elapsed := time.Since(started); elapsed < 2*timeout {
		t.Fatalf("output drained in %v; the test must span several stall bounds", elapsed)
	}
	if !bytes.Equal(client.data, want) {
		t.Fatalf("consumed output differs: got %d bytes, want %d", len(client.data), len(want))
	}
	if reason, _ := proveLivenessUntil(t, client, 2*timeout); reason != "" {
		t.Fatalf("a consuming page was closed with %q", reason)
	}
}

// With nothing outstanding the clock is not armed: an idle, acknowledged page
// that keeps proving liveness is never closed for stalling.
func TestWebSocketFlowStallIsNotArmedWithNothingOutstanding(t *testing.T) {
	const timeout = 200 * time.Millisecond
	_, _, client := stallClient(t, "flow-stall-idle", timeout)
	if reason, _ := proveLivenessUntil(t, client, 5*timeout); reason != "" {
		t.Fatalf("an idle page with nothing outstanding was closed with %q", reason)
	}
}
