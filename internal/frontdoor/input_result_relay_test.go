package frontdoor

import (
	"os"
	"testing"
	"time"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

// The broker's input results reach the page as reserved text frames, in
// order. A result that does not advance, passes the INPUT frames this socket
// relayed, or carries an unknown code is a broker protocol violation.
func TestInputResultsRelayInOrderAndAreChecked(t *testing.T) {
	authority := proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "test", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$1", SessionCreated: 3}
	open := func(inputs int) (*fakeAttachBroker, func() string, func() string) {
		broker := startFakeAttachBroker(t, false)
		front, addr := startFrontHTTP(t, broker.listener.Addr().String())
		handle, err := front.handles.mint(authority)
		if err != nil {
			t.Fatal(err)
		}
		ws := dialTerminalWS(t, front, addr, handle, "control")
		t.Cleanup(func() { _ = ws.Close() })
		if got := readWSAttachment(t, ws); got.Type != terminal.FrameLive {
			t.Fatalf("initial lifecycle did not reach LIVE: %+v", got)
		}
		for i := 0; i < inputs; i++ {
			writeWSAttachment(t, ws, terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: integrationSource, Epoch: 1, Data: []byte{'a' + byte(i)}})
			select {
			case <-broker.inputs:
			case <-time.After(3 * time.Second):
				t.Fatal("input never reached the broker")
			}
		}
		return broker, func() string { return readWSText(t, ws) }, func() string { return readWSCloseReasonSkippingText(t, ws) }
	}

	broker, text, _ := open(3)
	broker.controls <- proto.Control{Type: "input", Frames: 1}
	if got := text(); got != attachmentwire.TransportInputPrefix+"1" {
		t.Fatalf("written result relayed as %q", got)
	}
	broker.controls <- proto.Control{Type: "input", Frames: 3, Code: "input_paused"}
	if got := text(); got != attachmentwire.TransportInputPrefix+"3 input_paused" {
		t.Fatalf("refused result relayed as %q", got)
	}

	for name, results := range map[string][]proto.Control{
		"does not advance":     {{Type: "input", Frames: 1}, {Type: "input", Frames: 1}},
		"passes what was sent": {{Type: "input", Frames: 3}},
		"unknown code":         {{Type: "input", Frames: 1, Code: "observe_mode"}},
		"zero":                 {{Type: "input"}},
	} {
		broker, _, closed := open(2)
		for _, result := range results {
			broker.controls <- result
		}
		if reason := closed(); reason != "broker_protocol" {
			t.Fatalf("%s: closed with %q, want broker_protocol", name, reason)
		}
	}
}
