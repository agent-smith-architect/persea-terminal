package frontdoor

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/terminal"
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

// The stall deadline is absolute. A browser message the relay handles after
// the deadline has passed, before the timer was serviced, is not forwarded and
// cannot restart the clock: the attachment ends with flow_stalled.
func TestFlowStallDeadlineIsCheckedBeforeBrowserMessages(t *testing.T) {
	for name, send := range map[string]func(*flowClient){
		"input": func(client *flowClient) {
			input, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: integrationSource, Epoch: 1, Data: []byte("past-deadline")}, attachmentwire.BrowserToServer)
			if err != nil || client.ws.WriteMessage(websocket.TextMessage, input) != nil {
				t.Fatalf("send input: %v", err)
			}
		},
		"acknowledgement": func(client *flowClient) { client.ack(client.frames) },
	} {
		t.Run(name, func(t *testing.T) {
			broker := startFakeAttachBroker(t, false)
			front, addr := startFrontHTTP(t, broker.listener.Addr().String())
			// The timers stay far away; only the relay's clock moves.
			front.flowStallTimeout = time.Minute
			front.browserProofTimeout = time.Hour
			clock := &browserProofTestClock{base: time.Now()}
			front.browserProofNow = clock.now
			handle, err := front.handles.mint(flowAuthority("stall-deadline-" + name))
			if err != nil {
				t.Fatal(err)
			}
			client := &flowClient{t: t, ws: dialTerminalWS(t, front, addr, handle, "control")}
			defer client.ws.Close()
			for !bytes.Equal(client.data, []byte("ready")) {
				client.next()
			}
			client.ack(client.frames)
			requireFlowPaused(t, client, "0123456789abcdef0123456789abcdef")
			flowOutput(t, broker, 0, 1024)
			client.next()
			clock.offset.Add(int64(2 * time.Minute))
			send(client)
			_ = client.ws.SetReadDeadline(time.Now().Add(3 * time.Second))
			for {
				_, _, err := client.ws.ReadMessage()
				var closeErr *websocket.CloseError
				if errors.As(err, &closeErr) {
					if closeErr.Text != "flow_stalled" {
						t.Fatalf("closed with %q, want flow_stalled", closeErr.Text)
					}
					break
				}
				if err != nil {
					t.Fatalf("no close after the deadline: %v", err)
				}
			}
			select {
			case <-broker.served:
			case <-time.After(5 * time.Second):
				t.Fatal("the relay did not end its broker connection")
			}
			select {
			case got := <-broker.inputs:
				t.Fatalf("forwarded %q after the stall deadline", got)
			default:
			}
		})
	}
}
