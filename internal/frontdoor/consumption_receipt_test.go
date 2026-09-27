package frontdoor

import (
	"bytes"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/terminal"
)

func nextReceipt(t *testing.T, broker *fakeAttachBroker) uint64 {
	t.Helper()
	select {
	case frames := <-broker.receipts:
		return frames
	case <-time.After(3 * time.Second):
		t.Fatal("no consumption receipt reached the broker")
		return 0
	}
}

// A Control page's acknowledgements reach the broker as consumption receipts:
// strictly increasing, and always including the one that empties the window,
// so the broker never keeps a stale view of a page that consumed everything.
func TestWebSocketFlowForwardsConsumptionReceipts(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	handle, err := front.handles.mint(flowAuthority("receipts"))
	if err != nil {
		t.Fatal(err)
	}
	client := &flowClient{t: t, ws: dialTerminalWS(t, front, addr, handle, "control")}
	defer client.ws.Close()
	for !bytes.Equal(client.data, []byte("ready")) {
		client.next()
	}
	client.ack(client.frames)
	if got := nextReceipt(t, broker); got != client.frames {
		t.Fatalf("first receipt=%d, want %d", got, client.frames)
	}
	// A burst of acknowledgements while output is outstanding may be
	// coalesced; the final one, which empties the window, is always forwarded.
	const outputs = 6
	for i := 0; i < outputs; i++ {
		flowOutput(t, broker, i, 1<<10)
	}
	for i := 0; i < outputs; i++ {
		client.next()
	}
	final := client.frames
	for count := final - outputs + 1; count <= final; count++ {
		client.ack(count)
	}
	previous := final - outputs
	for previous != final {
		got := nextReceipt(t, broker)
		if got <= previous || got > final {
			t.Fatalf("receipt %d after %d, want strictly increasing up to %d", got, previous, final)
		}
		previous = got
	}
}

// An observe page cannot type, so its acknowledgements are not forwarded.
func TestWebSocketFlowForwardsNoReceiptsForObservers(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	handle, err := front.handles.mint(flowAuthority("receipts-observe"))
	if err != nil {
		t.Fatal(err)
	}
	client := &flowClient{t: t, ws: dialTerminalWS(t, front, addr, handle, "observe")}
	defer client.ws.Close()
	for !bytes.Equal(client.data, []byte("ready")) {
		client.next()
	}
	client.ack(client.frames)
	// The relay handles browser messages in order: once the PONG is back, the
	// acknowledgement before it has been handled.
	requireFlowPaused(t, client, "0123456789abcdef0123456789abcdef")
	select {
	case frames := <-broker.receipts:
		t.Fatalf("an observer's acknowledgement was forwarded as receipt %d", frames)
	default:
	}
}

// An acknowledgement the receipt interval held back goes to the broker before
// the page's next input, so the input is judged on everything the page had
// consumed when it typed rather than on an older receipt.
func TestWebSocketFlowForwardsAHeldBackReceiptBeforeInput(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	front.receiptInterval = time.Hour
	handle, err := front.handles.mint(flowAuthority("receipts-before-input"))
	if err != nil {
		t.Fatal(err)
	}
	client := &flowClient{t: t, ws: dialTerminalWS(t, front, addr, handle, "control")}
	defer client.ws.Close()
	for !bytes.Equal(client.data, []byte("ready")) {
		client.next()
	}
	client.ack(client.frames)
	if got := nextReceipt(t, broker); got != client.frames {
		t.Fatalf("window-emptying receipt=%d, want %d", got, client.frames)
	}
	flowOutput(t, broker, 0, 1<<10)
	flowOutput(t, broker, 1, 1<<10)
	client.next()
	client.next()
	held := client.frames - 1
	client.ack(held)
	requireFlowPaused(t, client, "0123456789abcdef0123456789abcdef")
	select {
	case got := <-broker.receipts:
		t.Fatalf("receipt %d was forwarded inside the receipt interval", got)
	default:
	}
	input, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: integrationSource, Epoch: 1, Data: []byte("typed")}, attachmentwire.BrowserToServer)
	if err != nil || client.ws.WriteMessage(websocket.TextMessage, input) != nil {
		t.Fatalf("send input: %v", err)
	}
	select {
	case <-broker.inputs:
	case <-time.After(3 * time.Second):
		t.Fatal("the input did not reach the broker")
	}
	// The fake broker reads frames in order, so a receipt forwarded before
	// the input is already queued when the input arrives.
	select {
	case got := <-broker.receipts:
		if got != held {
			t.Fatalf("receipt before input=%d, want %d", got, held)
		}
	default:
		t.Fatal("the input reached the broker before the held-back receipt")
	}
}
