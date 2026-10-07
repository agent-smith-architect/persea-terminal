package frontdoor

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
)

// recordingConn keeps every byte the client reads, so a test can see the
// WebSocket frames as they crossed the wire.
type recordingConn struct {
	net.Conn
	mu   sync.Mutex
	read bytes.Buffer
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.read.Write(p[:n])
	c.mu.Unlock()
	return n, err
}

// wireMessage is one server message as sent: its opcode, whether it was
// compressed (RSV1 on its first frame) and its payload bytes on the wire.
type wireMessage struct {
	opcode     byte
	compressed bool
	wireBytes  int
}

// messages parses the recorded server frames after the HTTP upgrade
// response.
func (c *recordingConn) messages(t *testing.T) []wireMessage {
	t.Helper()
	c.mu.Lock()
	raw := append([]byte(nil), c.read.Bytes()...)
	c.mu.Unlock()
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if end < 0 {
		t.Fatal("no upgrade response recorded")
	}
	out, err := parseServerFrames(raw[end+4:])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// parseServerFrames groups server frames (never masked) into messages. A
// control frame stands alone, also between the fragments of a message. A
// frame still arriving at the end is left out.
func parseServerFrames(raw []byte) ([]wireMessage, error) {
	var out []wireMessage
	current := -1 // the data message whose fragments are arriving
	for len(raw) >= 2 {
		fin, rsv1, opcode := raw[0]&0x80 != 0, raw[0]&0x40 != 0, raw[0]&0x0f
		length, header := uint64(raw[1]&0x7f), 2
		switch length {
		case 126:
			header = 4
		case 127:
			header = 10
		}
		if len(raw) < header {
			break
		}
		switch header {
		case 4:
			length = uint64(binary.BigEndian.Uint16(raw[2:4]))
		case 10:
			length = binary.BigEndian.Uint64(raw[2:10])
		}
		if uint64(len(raw)-header) < length {
			break
		}
		switch {
		case opcode >= 0x8:
			out = append(out, wireMessage{opcode: opcode, compressed: rsv1, wireBytes: int(length)})
		case opcode != 0:
			out = append(out, wireMessage{opcode: opcode, compressed: rsv1, wireBytes: int(length)})
			current = len(out) - 1
		case current < 0:
			return nil, fmt.Errorf("continuation frame without a message")
		default:
			out[current].wireBytes += int(length)
		}
		if opcode < 0x8 && fin {
			current = -1
		}
		raw = raw[uint64(header)+length:]
	}
	return out, nil
}

// The parser keeps a fragmented message whole across an interleaved Ping,
// and leaves out a frame whose header has not fully arrived.
func TestParseServerFramesFragmentsAndPartialHeaders(t *testing.T) {
	raw := []byte{0x01, 3, 'a', 'b', 'c', 0x89, 0, 0x80, 3, 'd', 'e', 'f', 0x82, 126, 0x01}
	got, err := parseServerFrames(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []wireMessage{{opcode: websocket.TextMessage, wireBytes: 6}, {opcode: websocket.PingMessage}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("messages %+v, want %+v", got, want)
	}
}

func dialRecordedTerminalWS(t *testing.T, front *Server, addr, fixtureHandle string, compress bool) (*websocket.Conn, *recordingConn, string) {
	t.Helper()
	a, err := front.handles.resolve(fixtureHandle)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := front.handles.mint(a, front.cfg.Ingress.OperatorLogin, "control")
	if err != nil {
		t.Fatal(err)
	}
	var recorder *recordingConn
	dialer := websocket.Dialer{EnableCompression: compress, NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		recorder = &recordingConn{Conn: conn}
		return recorder, nil
	}}
	ws, response, err := currentFixtureDialWith(dialer, addr, handle, "control")
	if err != nil {
		t.Fatal(err)
	}
	return ws, recorder, response.Header.Get("Sec-WebSocket-Extensions")
}

// A browser that offers permessage-deflate gets large attachment messages
// compressed, each on its own; liveness, flow and small output stay plain.
func TestWebSocketCompressesOnlyLargeAttachments(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	handle, err := front.handles.mint(flowAuthority("compress"))
	if err != nil {
		t.Fatal(err)
	}
	ws, recorder, extensions := dialRecordedTerminalWS(t, front, addr, handle, true)
	defer ws.Close()
	if !strings.Contains(extensions, "permessage-deflate") || !strings.Contains(extensions, "server_no_context_takeover") {
		t.Fatalf("negotiated extensions %q", extensions)
	}
	client := &flowClient{t: t, ws: ws}
	for !bytes.Equal(client.data, []byte("ready")) {
		client.next()
	}
	before := len(recorder.messages(t))
	large := flowOutput(t, broker, 0, 16<<10)
	small := flowOutput(t, broker, 1, compressMinBytes/2)
	nonce := "0123456789abcdef0123456789abcdef"
	for len(client.data) < len("ready")+len(large)+len(small) {
		client.next()
	}
	writeApplicationPing(t, ws, nonce)
	if got := client.next(); string(got) != attachmentwire.TransportLivenessPrefix+"PONG "+nonce {
		t.Fatalf("liveness answer %q", got)
	}
	if !bytes.Equal(client.data, append(append([]byte("ready"), large...), small...)) {
		t.Fatal("decoded output differs")
	}
	got := recorder.messages(t)[before:]
	if len(got) != 3 {
		t.Fatalf("server messages after ready: %+v", got)
	}
	if m := got[0]; m.opcode != websocket.BinaryMessage || !m.compressed || m.wireBytes > len(large)/4 {
		t.Fatalf("large attachment on the wire: %+v for %d bytes", m, len(large))
	}
	if m := got[1]; m.opcode != websocket.BinaryMessage || m.compressed {
		t.Fatalf("small attachment on the wire: %+v", m)
	}
	if m := got[2]; m.opcode != websocket.TextMessage || m.compressed {
		t.Fatalf("liveness answer on the wire: %+v", m)
	}
}

// A browser that does not offer the extension gets plain messages.
func TestWebSocketWithoutCompressionStaysPlain(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	handle, err := front.handles.mint(flowAuthority("plain"))
	if err != nil {
		t.Fatal(err)
	}
	ws, recorder, extensions := dialRecordedTerminalWS(t, front, addr, handle, false)
	defer ws.Close()
	if extensions != "" {
		t.Fatalf("negotiated extensions %q without an offer", extensions)
	}
	client := &flowClient{t: t, ws: ws}
	for !bytes.Equal(client.data, []byte("ready")) {
		client.next()
	}
	large := flowOutput(t, broker, 0, 16<<10)
	for len(client.data) < len("ready")+len(large) {
		client.next()
	}
	for _, m := range recorder.messages(t) {
		if m.compressed {
			t.Fatalf("compressed message without negotiation: %+v", m)
		}
	}
}

// The read limit counts wire bytes; a compressed browser message is also
// bounded by its decoded size. One that inflates past the limit ends the
// socket as a read failure, closed 1009 like a message over the wire limit,
// before anything parses it.
func TestWebSocketBoundsDecodedBrowserMessages(t *testing.T) {
	var logMu sync.Mutex
	var logs []string
	prior := frontLogf
	frontLogf = func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { frontLogf = prior })
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	authority := flowAuthority("inflate")
	handle, err := front.handles.mint(authority)
	if err != nil {
		t.Fatal(err)
	}
	ws, _, _ := dialRecordedTerminalWS(t, front, addr, handle, true)
	defer ws.Close()
	client := &flowClient{t: t, ws: ws}
	for !bytes.Equal(client.data, []byte("ready")) {
		client.next()
	}
	ws.EnableWriteCompression(true)
	if err := ws.SetCompressionLevel(flate.BestCompression); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.TextMessage, bytes.Repeat([]byte{'a'}, proto.MaxAttachment+1)); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var readErr error
	for readErr == nil {
		_, _, readErr = ws.ReadMessage()
	}
	if !websocket.IsCloseError(readErr, websocket.CloseMessageTooBig) {
		t.Fatalf("socket ended with %v, want close 1009", readErr)
	}
	waitForNoLease(t, front.leases, authority)
	logMu.Lock()
	defer logMu.Unlock()
	failures := 0
	for _, line := range logs {
		if strings.Contains(line, "event=terminal_failure") {
			failures++
			if !strings.Contains(line, `code="websocket_read"`) {
				t.Fatalf("over-limit message ended the attachment as %s", line)
			}
		}
	}
	if failures != 1 {
		t.Fatalf("terminal failures logged: %q", logs)
	}
}
