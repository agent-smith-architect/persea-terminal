package frontdoor

import (
	"bytes"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

func flowAuthority(label string) proto.Authority {
	return proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: label, BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$" + label, SessionCreated: 3}
}

// flowClient reads a control WebSocket the way a page does, counting the
// attachment frames and bytes it has received so it can acknowledge them.
type flowClient struct {
	t      *testing.T
	ws     *websocket.Conn
	frames uint64
	bytes  int
	data   []byte
}

// next returns the next text message, counting it when it is an attachment
// frame and answering PREPARE as a page does.
func (c *flowClient) next() []byte {
	c.t.Helper()
	_ = c.ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, payload, err := c.ws.ReadMessage()
	if err != nil {
		c.t.Fatal(err)
	}
	if kind != websocket.TextMessage {
		c.t.Fatalf("websocket message kind=%d", kind)
	}
	if bytes.HasPrefix(payload, []byte(attachmentwire.TransportLivenessPrefix)) {
		return payload
	}
	frame, err := attachmentwire.Decode(payload, attachmentwire.ServerToBrowser)
	if err != nil {
		c.t.Fatal(err)
	}
	c.frames++
	c.bytes += len(payload)
	switch frame.Type {
	case terminal.FramePrepare:
		writeWSAttachment(c.t, c.ws, terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameReady, Source: frame.Source, Epoch: frame.Epoch, Cut: frame.Cut})
	case terminal.FrameLive:
		c.data = append(c.data, frame.Data...)
	}
	return payload
}

func (c *flowClient) ack(count uint64) {
	c.t.Helper()
	if err := c.ws.WriteMessage(websocket.TextMessage, []byte(attachmentwire.TransportFlowPrefix+"ACK "+strconv.FormatUint(count, 10))); err != nil {
		c.t.Fatal(err)
	}
}

func flowOutput(t *testing.T, broker *fakeAttachBroker, index int, size int) []byte {
	t.Helper()
	data := bytes.Repeat([]byte{byte('a' + index%26)}, size)
	frame, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameLive, Source: integrationSource, Epoch: 1, Cut: 1, Data: data}, attachmentwire.ServerToBrowser)
	if err != nil {
		t.Fatal(err)
	}
	broker.outbound <- frame
	return data
}

// A browser that has not acknowledged a full window receives no further
// attachment frame, yet its liveness PING is answered at once; its
// acknowledgement then releases the rest in order.
func TestWebSocketFlowWindowPausesBrokerOutputButNotLiveness(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	handle, err := front.handles.mint(flowAuthority("flow-window"))
	if err != nil {
		t.Fatal(err)
	}
	client := &flowClient{t: t, ws: dialTerminalWS(t, front, addr, handle, "control")}
	defer client.ws.Close()
	for !bytes.Equal(client.data, []byte("ready")) {
		client.next()
	}
	var want []byte
	want = append(want, client.data...)
	const outputs, size = 24, 16 << 10
	for i := 0; i < outputs; i++ {
		want = append(want, flowOutput(t, broker, i, size)...)
	}
	// The frame that fills the window is the last one written before an
	// acknowledgement: everything the front door wrote has now been read.
	for client.bytes < FlowWindowBytes {
		client.next()
	}
	received := client.frames
	nonce := "00112233445566778899aabbccddeeff"
	writeApplicationPing(t, client.ws, nonce)
	if got := client.next(); string(got) != attachmentwire.TransportLivenessPrefix+"PONG "+nonce {
		t.Fatalf("with a full window, the PING was answered after %d more attachment frames; want the PONG first", client.frames-received)
	}
	if client.frames != received {
		t.Fatalf("attachment frames grew from %d to %d while the window was full", received, client.frames)
	}
	for len(client.data) < len(want) {
		client.ack(client.frames)
		client.next()
	}
	if !bytes.Equal(client.data, want) {
		t.Fatalf("acknowledged output differs: got %d bytes, want %d", len(client.data), len(want))
	}
}

// Every malformed, repeated, backwards or premature acknowledgement ends the
// attachment with the closed code bad_flow.
func TestWebSocketFlowRefusesBadAcknowledgements(t *testing.T) {
	cases := map[string]func(c *flowClient) []byte{
		"beyond sent": func(c *flowClient) []byte {
			return []byte(attachmentwire.TransportFlowPrefix + "ACK " + strconv.FormatUint(c.frames+1, 10))
		},
		"zero":      func(*flowClient) []byte { return []byte(attachmentwire.TransportFlowPrefix + "ACK 0") },
		"malformed": func(*flowClient) []byte { return []byte(attachmentwire.TransportFlowPrefix + "ACK 1x") },
		"unknown verb": func(*flowClient) []byte {
			return []byte(attachmentwire.TransportFlowPrefix + "NAK 1")
		},
		"repeated": func(c *flowClient) []byte {
			c.ack(c.frames)
			return []byte(attachmentwire.TransportFlowPrefix + "ACK " + strconv.FormatUint(c.frames, 10))
		},
		"backwards": func(c *flowClient) []byte {
			c.ack(c.frames)
			return []byte(attachmentwire.TransportFlowPrefix + "ACK " + strconv.FormatUint(c.frames-1, 10))
		},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			broker := startFakeAttachBroker(t, false)
			front, addr := startFrontHTTP(t, broker.listener.Addr().String())
			authority := flowAuthority("flow-refusal")
			handle, err := front.handles.mint(authority)
			if err != nil {
				t.Fatal(err)
			}
			client := &flowClient{t: t, ws: dialTerminalWS(t, front, addr, handle, "control")}
			defer client.ws.Close()
			for !bytes.Equal(client.data, []byte("ready")) {
				client.next()
			}
			if err := client.ws.WriteMessage(websocket.TextMessage, bad(client)); err != nil {
				t.Fatal(err)
			}
			if reason := readWSCloseReason(t, client.ws); reason != "bad_flow" {
				t.Fatalf("close reason=%q, want bad_flow", reason)
			}
			waitForNoLease(t, front.leases, authority)
		})
	}
}

// A browser that goes away while its window is full still releases the
// attachment and its control lease.
func TestWebSocketFlowWindowReleasesOnCloseWhilePaused(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	authority := flowAuthority("flow-close")
	handle, err := front.handles.mint(authority)
	if err != nil {
		t.Fatal(err)
	}
	client := &flowClient{t: t, ws: dialTerminalWS(t, front, addr, handle, "control")}
	for !bytes.Equal(client.data, []byte("ready")) {
		client.next()
	}
	for i := 0; i < 24; i++ {
		flowOutput(t, broker, i, 16<<10)
	}
	for client.bytes < FlowWindowBytes {
		client.next()
	}
	snapshotLease(t, front.leases, authority)
	if err := client.ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")); err != nil {
		t.Fatal(err)
	}
	_ = client.ws.Close()
	waitForNoLease(t, front.leases, authority)
}

// A broker that stops reading cannot wedge the relay loop: the write that
// cannot complete ends the attachment within BrokerWriteTimeout.
func TestWebSocketBrokerWriteIsBounded(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	authority := flowAuthority("flow-broker-write")
	handle, err := front.handles.mint(authority)
	if err != nil {
		t.Fatal(err)
	}
	client := &flowClient{t: t, ws: dialTerminalWS(t, front, addr, handle, "control")}
	defer client.ws.Close()
	for !bytes.Equal(client.data, []byte("ready")) {
		client.next()
	}
	// The fake broker parks once its attachment queue is full and stops
	// reading; the input behind it then fills the socket towards the broker.
	input := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: integrationSource, Epoch: 1, Data: bytes.Repeat([]byte{'i'}, 16<<10)}
	raw, err := attachmentwire.Encode(input, attachmentwire.BrowserToServer)
	if err != nil {
		t.Fatal(err)
	}
	for sent := 0; sent < 4<<20; sent += len(raw) {
		if err := client.ws.WriteMessage(websocket.TextMessage, raw); err != nil {
			break
		}
	}
	started := time.Now()
	_ = client.ws.SetReadDeadline(time.Now().Add(BrokerWriteTimeout + 3*time.Second))
	for {
		_, _, err := client.ws.ReadMessage()
		if err == nil {
			continue
		}
		closeErr, ok := err.(*websocket.CloseError)
		if !ok || closeErr.Text != "broker_write" {
			t.Fatalf("after %s: %v, want close broker_write", time.Since(started).Round(time.Millisecond), err)
		}
		break
	}
	waitForNoLease(t, front.leases, authority)
}
