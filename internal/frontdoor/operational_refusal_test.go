package frontdoor

import (
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

func readWSText(t *testing.T, ws *websocket.Conn) string {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, payload, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read text: %v", err)
	}
	if kind != websocket.TextMessage {
		t.Fatalf("message kind=%d want text", kind)
	}
	return string(payload)
}

// The front door's attachment error policy, driven over a real WebSocket:
// every operational broker code is relayed in-band as a reserved refusal
// frame and the socket keeps carrying input afterwards; every fatal code —
// and any code the policy does not know — still closes the socket with the
// code as the reason, exactly as before.
func TestOperationalBrokerErrorsRelayInBandAndFatalOnesClose(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	authority := proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "test", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$1", SessionCreated: 3}
	handle, err := front.handles.mint(authority)
	if err != nil {
		t.Fatal(err)
	}
	ws := dialTerminalWS(t, front, addr, handle, "control")
	defer ws.Close()
	if got := readWSAttachment(t, ws); got.Type != terminal.FrameLive {
		t.Fatalf("initial lifecycle did not reach LIVE: %+v", got)
	}

	for _, code := range proto.OperationalAttachmentCodes() {
		broker.controls <- proto.Control{Type: "error", Code: code}
		if got, want := readWSText(t, ws), attachmentwire.TransportRefusalPrefix+code; got != want {
			t.Fatalf("operational %s relayed as %q want %q", code, got, want)
		}
		// Still attached: input crosses the same socket to the same broker.
		payload := []byte("after-" + code)
		writeWSAttachment(t, ws, terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: integrationSource, Epoch: 1, Data: payload})
		select {
		case got := <-broker.inputs:
			if string(got) != string(payload) {
				t.Fatalf("input after %s: %q want %q", code, got, payload)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("input after operational %s never reached the broker", code)
		}
	}

	// The fake broker fans its controls out to whichever attachment is being
	// served, so each fatal case gets a broker and a front of its own.
	fatal := append(proto.FatalAttachmentCodes(), "some_future_code")
	for _, code := range fatal {
		broker := startFakeAttachBroker(t, false)
		front, addr := startFrontHTTP(t, broker.listener.Addr().String())
		handle, err := front.handles.mint(authority)
		if err != nil {
			t.Fatal(err)
		}
		socket := dialTerminalWS(t, front, addr, handle, "observe")
		if got := readWSAttachment(t, socket); got.Type != terminal.FrameLive {
			t.Fatalf("%s: initial lifecycle did not reach LIVE: %+v", code, got)
		}
		broker.controls <- proto.Control{Type: "error", Code: code}
		if reason := readWSCloseReasonSkippingText(t, socket); reason != code {
			t.Fatalf("fatal %s closed with reason %q", code, reason)
		}
		_ = socket.Close()
	}
}
