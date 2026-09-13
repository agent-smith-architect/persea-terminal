package frontdoor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

var integrationSource = strings.Repeat("A", 43)

type fakeAttachBroker struct {
	listener    net.Listener
	failed      atomic.Bool
	failFirst   bool
	inputs      chan []byte
	attachments chan []byte
	outbound    chan []byte
	controls    chan proto.Control
}

type browserProofTestClock struct {
	base   time.Time
	offset atomic.Int64
}

func (c *browserProofTestClock) now() time.Time {
	return c.base.Add(time.Duration(c.offset.Load()))
}

func startFakeAttachBroker(t *testing.T, failFirst bool) *fakeAttachBroker {
	t.Helper()
	path := shortTestDir(t) + "/broker.sock"
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeAttachBroker{listener: listener, failFirst: failFirst, inputs: make(chan []byte, 8), attachments: make(chan []byte, 8), outbound: make(chan []byte, 32), controls: make(chan proto.Control, 8)}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go b.serve(conn)
		}
	}()
	return b
}

func (b *fakeAttachBroker) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	frame, err := proto.ReadFrame(conn)
	if err != nil {
		return
	}
	hello, _ := proto.DecodeControl(frame.Payload)
	if hello.Type != "hello" {
		return
	}
	_ = writeControl(conn, proto.Control{Type: "hello_ok", V: 1})
	frame, err = proto.ReadFrame(conn)
	if err != nil {
		return
	}
	attach, _ := proto.DecodeControl(frame.Payload)
	if attach.Type != "attach" {
		return
	}
	if b.failFirst && b.failed.CompareAndSwap(false, true) {
		_ = writeControl(conn, proto.Control{Type: "error", Code: "attach_failed"})
		return
	}
	_ = writeControl(conn, proto.Control{Type: "attach_ok", Cols: 80, Rows: 24, InputMax: terminal.MaxInputBytes})
	prepare, _ := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FramePrepare, Source: integrationSource, Epoch: 1, Cut: 1, Kind: terminal.CutInitial, Columns: 80, Rows: 24}, attachmentwire.ServerToBrowser)
	_ = proto.WriteFrame(conn, proto.FrameAttachment, prepare)
	frame, err = proto.ReadFrame(conn)
	if err != nil || frame.Type != proto.FrameAttachment {
		return
	}
	ready, decodeErr := attachmentwire.Decode(frame.Payload, attachmentwire.BrowserToServer)
	if decodeErr != nil || ready.Type != terminal.FrameReady || ready.Source != integrationSource || ready.Epoch != 1 || ready.Cut != 1 {
		return
	}
	commit, _ := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameCommit, Source: integrationSource, Epoch: 1, Cut: 1}, attachmentwire.ServerToBrowser)
	_ = proto.WriteFrame(conn, proto.FrameAttachment, commit)
	live, _ := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameLive, Source: integrationSource, Epoch: 1, Cut: 1, Data: []byte("ready")}, attachmentwire.ServerToBrowser)
	_ = proto.WriteFrame(conn, proto.FrameAttachment, live)
	_ = conn.SetDeadline(time.Time{})
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case payload := <-b.outbound:
				if proto.WriteFrame(conn, proto.FrameAttachment, payload) != nil {
					return
				}
			case control := <-b.controls:
				if writeControl(conn, control) != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()
	for {
		frame, err = proto.ReadFrame(conn)
		if err != nil {
			return
		}
		if frame.Type == proto.FrameAttachment {
			b.attachments <- append([]byte(nil), frame.Payload...)
			typed, decodeErr := attachmentwire.Decode(frame.Payload, attachmentwire.BrowserToServer)
			if decodeErr == nil && typed.Type == terminal.FrameInput {
				b.inputs <- append([]byte(nil), typed.Data...)
			}
			continue
		}
		if frame.Type == proto.FrameControl {
			control, _ := proto.DecodeControl(frame.Payload)
			if control.Type == "ping" {
				_ = writeControl(conn, proto.Control{Type: "pong"})
			}
		}
	}
}

func snapshotLease(t *testing.T, leases *leaseStore, authority proto.Authority) leaseEntry {
	t.Helper()
	leases.mu.Lock()
	defer leases.mu.Unlock()
	entry, ok := leases.entries[authorityKey(authority)]
	if !ok {
		t.Fatal("control lease absent")
	}
	return entry
}

func waitForNoLease(t *testing.T, leases *leaseStore, authority proto.Authority) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		leases.mu.Lock()
		_, present := leases.entries[authorityKey(authority)]
		leases.mu.Unlock()
		if !present {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("control lease was not released")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func writeApplicationPing(t *testing.T, ws *websocket.Conn, nonce string) {
	t.Helper()
	ping := attachmentwire.TransportLivenessPrefix + "PING " + nonce
	if err := ws.WriteMessage(websocket.TextMessage, []byte(ping)); err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketPrepareBindsExactAuthorityAndRejectsSourceChange(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	authority := proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "test", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$1", SessionCreated: 3}
	handle, err := front.handles.mint(authority)
	if err != nil {
		t.Fatal(err)
	}
	ws := dialTerminalWS(t, front, addr, handle, "observe")
	defer ws.Close()
	if got := readWSAttachment(t, ws); got.Type != terminal.FrameLive {
		t.Fatalf("initial lifecycle did not reach LIVE: %+v", got)
	}
	bound, err := front.bindings.resolve(front.cfg.Ingress.OperatorLogin, integrationSource)
	if err != nil || authorityKey(bound) != authorityKey(authority) {
		t.Fatalf("PREPARE source lost consumed handle authority: %+v %v", bound, err)
	}
	wrongSource := strings.Repeat("B", 43)
	mutant, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameLive, Source: wrongSource, Epoch: 1, Cut: 2, Data: []byte("mutant")}, attachmentwire.ServerToBrowser)
	if err != nil {
		t.Fatal(err)
	}
	broker.outbound <- mutant
	if reason := readWSCloseReason(t, ws); reason != "broker_protocol" {
		t.Fatalf("source-change mutant close reason=%q", reason)
	}
	if rebound, resolveErr := front.bindings.resolve(front.cfg.Ingress.OperatorLogin, integrationSource); resolveErr != nil || authorityKey(rebound) != authorityKey(authority) {
		t.Fatalf("closed source did not retain exact bounded reconnect binding: %+v %v", rebound, resolveErr)
	}
}

func TestWebSocketApplicationLivenessPongIsFrontLocalAndDoesNotRenewLease(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	a := proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "test", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$1", SessionCreated: 3}
	handle, err := front.handles.mint(a)
	if err != nil {
		t.Fatal(err)
	}
	control := dialTerminalWS(t, front, addr, handle, "control")
	defer control.Close()
	if got := readWSAttachment(t, control); got.Type != terminal.FrameLive {
		t.Fatalf("control first frame=%+v", got)
	}
	leaseBefore := snapshotLease(t, front.leases, a)
	nonce := "0123456789abcdef0123456789abcdef"
	ping := attachmentwire.TransportLivenessPrefix + "PING " + nonce
	if err := control.WriteMessage(websocket.TextMessage, []byte(ping)); err != nil {
		t.Fatal(err)
	}
	_ = control.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, payload, err := control.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if kind != websocket.TextMessage || string(payload) != attachmentwire.TransportLivenessPrefix+"PONG "+nonce {
		t.Fatalf("liveness response kind=%d payload=%q", kind, payload)
	}
	select {
	case frame := <-broker.attachments:
		t.Fatalf("liveness frame reached broker: %d bytes", len(frame))
	case <-time.After(100 * time.Millisecond):
	}
	if leaseAfter := snapshotLease(t, front.leases, a); leaseAfter != leaseBefore {
		t.Fatalf("liveness renewed or mutated control lease: before=%+v after=%+v", leaseBefore, leaseAfter)
	}

	var logs bytes.Buffer
	prior := frontLogf
	frontLogf = func(format string, args ...any) { _, _ = fmt.Fprintf(&logs, format, args...) }
	t.Cleanup(func() { frontLogf = prior })
	canary := attachmentwire.TransportLivenessPrefix + "PING " + nonce + "x"
	if err := control.WriteMessage(websocket.TextMessage, []byte(canary)); err != nil {
		t.Fatal(err)
	}
	if reason := readWSCloseReason(t, control); reason != "bad_liveness" {
		t.Fatalf("malformed liveness close reason=%q", reason)
	}
	if bytes.Contains(logs.Bytes(), []byte(nonce)) || bytes.Contains(logs.Bytes(), []byte(canary)) {
		t.Fatal("liveness payload or nonce entered front log")
	}
}

func startFrontHTTP(t *testing.T, brokerPath string) (*Server, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	cfg := currentFrontFixtureConfig(addr, brokerPath)
	front := newServer(cfg, ".", addr)
	startJoinedHTTPServer(t, listener, f3BrowserOracleTrustedHop(front.handler(), cfg.Ingress))
	return front, addr
}

type joinedHTTPServerConnKey struct{}

type joinedHTTPServer struct {
	server *http.Server

	mu        sync.Mutex
	conns     map[net.Conn]bool
	serveDone chan struct{}
	wg        sync.WaitGroup
}

func startJoinedHTTPServer(t *testing.T, listener net.Listener, handler http.Handler) {
	t.Helper()
	joined := &joinedHTTPServer{conns: make(map[net.Conn]bool), serveDone: make(chan struct{})}
	joined.server = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer joined.handlerDone(r)
			handler.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: time.Second,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			return context.WithValue(ctx, joinedHTTPServerConnKey{}, conn)
		},
		ConnState: joined.connState,
	}
	go func() {
		_ = joined.server.Serve(listener)
		close(joined.serveDone)
	}()
	t.Cleanup(joined.close)
}

func (joined *joinedHTTPServer) connState(conn net.Conn, state http.ConnState) {
	joined.mu.Lock()
	defer joined.mu.Unlock()
	switch state {
	case http.StateNew:
		joined.conns[conn] = false
		joined.wg.Add(1)
	case http.StateHijacked:
		joined.conns[conn] = true
	case http.StateClosed:
		if _, ok := joined.conns[conn]; ok {
			delete(joined.conns, conn)
			joined.wg.Done()
		}
	}
}

func (joined *joinedHTTPServer) handlerDone(r *http.Request) {
	conn, _ := r.Context().Value(joinedHTTPServerConnKey{}).(net.Conn)
	if conn == nil {
		return
	}
	joined.mu.Lock()
	if hijacked, ok := joined.conns[conn]; ok && hijacked {
		delete(joined.conns, conn)
		joined.mu.Unlock()
		joined.wg.Done()
		return
	}
	joined.mu.Unlock()
}

func (joined *joinedHTTPServer) close() {
	_ = joined.server.Close()
	<-joined.serveDone
	joined.mu.Lock()
	conns := make([]net.Conn, 0, len(joined.conns))
	for conn := range joined.conns {
		conns = append(conns, conn)
	}
	joined.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	joined.wg.Wait()
}

func dialTerminalWS(t *testing.T, front *Server, addr, fixtureHandle, mode string) *websocket.Conn {
	t.Helper()
	// The reusable handle is only a fixture source reference. Each wire attach
	// gets the same one-shot, purpose-bound capability as the current client.
	a, err := front.handles.resolve(fixtureHandle)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := front.handles.mint(a, front.cfg.Ingress.OperatorLogin, mode)
	if err != nil {
		t.Fatal(err)
	}
	ws, response, err := currentFixtureDial(addr, handle, mode)
	if err != nil {
		if response != nil {
			t.Fatalf("websocket dial: %v status=%d", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	return ws
}

func readWSControl(t *testing.T, ws *websocket.Conn) map[string]any {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		kind, payload, err := ws.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind != websocket.TextMessage {
			continue
		}
		var control map[string]any
		if err := json.Unmarshal(payload, &control); err != nil {
			t.Fatal(err)
		}
		return control
	}
}

func readWSAttachment(t *testing.T, ws *websocket.Conn) terminal.Frame {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		kind, payload, err := ws.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind != websocket.TextMessage {
			t.Fatalf("attachment websocket kind=%d", kind)
		}
		typed, err := attachmentwire.Decode(payload, attachmentwire.ServerToBrowser)
		if err != nil {
			t.Fatal(err)
		}
		if typed.Type == terminal.FramePrepare {
			ready, encodeErr := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameReady, Source: typed.Source, Epoch: typed.Epoch, Cut: typed.Cut}, attachmentwire.BrowserToServer)
			if encodeErr != nil || ws.WriteMessage(websocket.TextMessage, ready) != nil {
				t.Fatalf("attachment handshake failed: %v", encodeErr)
			}
			continue
		}
		if typed.Type == terminal.FrameCommit {
			continue
		}
		return typed
	}
}

func writeWSAttachment(t *testing.T, ws *websocket.Conn, frame terminal.Frame) {
	t.Helper()
	raw, err := attachmentwire.Encode(frame, attachmentwire.BrowserToServer)
	if err != nil {
		t.Fatal(err)
	}
	if err = ws.WriteMessage(websocket.TextMessage, raw); err != nil {
		t.Fatal(err)
	}
}

func readWSCloseReason(t *testing.T, ws *websocket.Conn) string {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err := ws.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("websocket did not close with a reason: %v", err)
	}
	return closeErr.Text
}

func readWSCloseReasonSkippingText(t *testing.T, ws *websocket.Conn) string {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := ws.ReadMessage()
		if err == nil {
			continue
		}
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) {
			t.Fatalf("websocket did not close with a reason: %v", err)
		}
		return closeErr.Text
	}
}

func TestBrowserProofExpiresDespiteWebSocketPongsAndReleasesExactLease(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	front.browserProofTimeout = 150 * time.Millisecond
	a := proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "proof-pong", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$proof-pong", SessionCreated: 3}
	handle, err := front.handles.mint(a)
	if err != nil {
		t.Fatal(err)
	}
	ghost := dialTerminalWS(t, front, addr, handle, "control")
	if got := readWSAttachment(t, ghost); got.Type != terminal.FrameLive {
		t.Fatalf("control first frame=%+v", got)
	}
	stopPongs := make(chan struct{})
	defer close(stopPongs)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = ghost.WriteControl(websocket.PongMessage, []byte("proxy"), time.Now().Add(time.Second))
			case <-stopPongs:
				return
			}
		}
	}()
	if reason := readWSCloseReason(t, ghost); reason != "browser_liveness" {
		t.Fatalf("proxy-Pong ghost close reason=%q", reason)
	}
	waitForNoLease(t, front.leases, a)
	fresh := dialTerminalWS(t, front, addr, handle, "control")
	defer fresh.Close()
	if got := readWSAttachment(t, fresh); got.Type != terminal.FrameLive {
		t.Fatalf("fresh control after proof expiry=%+v", got)
	}
}

func TestBrowserProofCanonicalPingResetsOnceWithoutLeaseMutation(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	front.browserProofTimeout = 500 * time.Millisecond
	a := proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "proof-reset", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$proof-reset", SessionCreated: 3}
	handle, _ := front.handles.mint(a)
	control := dialTerminalWS(t, front, addr, handle, "control")
	defer control.Close()
	_ = readWSAttachment(t, control)
	leaseBefore := snapshotLease(t, front.leases, a)
	time.Sleep(200 * time.Millisecond)
	nonce := "fedcba9876543210fedcba9876543210"
	proofAt := time.Now()
	writeApplicationPing(t, control, nonce)
	_ = control.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, payload, err := control.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if kind != websocket.TextMessage || string(payload) != attachmentwire.TransportLivenessPrefix+"PONG "+nonce {
		t.Fatalf("proof response kind=%d payload=%q", kind, payload)
	}
	if leaseAfter := snapshotLease(t, front.leases, a); leaseAfter != leaseBefore {
		t.Fatalf("proof mutated lease: before=%+v after=%+v", leaseBefore, leaseAfter)
	}
	if reason := readWSCloseReason(t, control); reason != "browser_liveness" {
		t.Fatalf("post-proof close reason=%q", reason)
	}
	if elapsed := time.Since(proofAt); elapsed < 400*time.Millisecond {
		t.Fatalf("proof did not advance watchdog: expired after %v", elapsed)
	}
}

func TestBrowserProofRejectsReadyControlInputAtAbsoluteExpiry(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	front.browserProofTimeout = 2 * time.Second
	proofClock := &browserProofTestClock{base: time.Now()}
	front.browserProofNow = proofClock.now
	leaseBase := time.Now()
	var leaseClockCalls atomic.Int64
	front.leases.now = func() time.Time {
		leaseClockCalls.Add(1)
		return leaseBase
	}
	a := proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "proof-expired-input", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$proof-expired-input", SessionCreated: 3}
	handle, _ := front.handles.mint(a)
	control := dialTerminalWS(t, front, addr, handle, "control")
	defer control.Close()
	_ = readWSAttachment(t, control)
	leaseCallsBefore := leaseClockCalls.Load()
	if leaseCallsBefore == 0 {
		t.Fatal("control acquisition did not consult lease clock")
	}

	proofClock.offset.Store(int64(front.browserProofTimeout))
	writeWSAttachment(t, control, terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: integrationSource, Epoch: 1, Data: []byte("post-expiry")})
	if reason := readWSCloseReason(t, control); reason != "browser_liveness" {
		t.Fatalf("post-expiry input close reason=%q", reason)
	}
	if got := leaseClockCalls.Load(); got != leaseCallsBefore {
		t.Fatalf("post-expiry input renewed lease: clock calls %d -> %d", leaseCallsBefore, got)
	}
	select {
	case input := <-broker.inputs:
		t.Fatalf("post-expiry input reached broker: %q", input)
	case <-time.After(100 * time.Millisecond):
	}
	waitForNoLease(t, front.leases, a)
}

func TestBrowserProofRejectsReadyBrokerOutputAtAbsoluteExpiry(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	front.browserProofTimeout = 2 * time.Second
	proofClock := &browserProofTestClock{base: time.Now()}
	front.browserProofNow = proofClock.now
	a := proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "proof-expired-broker", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$proof-expired-broker", SessionCreated: 3}
	handle, _ := front.handles.mint(a)
	observe := dialTerminalWS(t, front, addr, handle, "observe")
	defer observe.Close()
	_ = readWSAttachment(t, observe)
	outbound, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameLive, Source: integrationSource, Epoch: 1, Cut: 2, Data: []byte("post-expiry")}, attachmentwire.ServerToBrowser)
	if err != nil {
		t.Fatal(err)
	}

	proofClock.offset.Store(int64(front.browserProofTimeout))
	broker.outbound <- outbound
	if reason := readWSCloseReason(t, observe); reason != "browser_liveness" {
		t.Fatalf("post-expiry broker output close reason=%q", reason)
	}
}

func TestBrowserProofIgnoresAttachmentAndBrokerTraffic(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	front.browserProofTimeout = 150 * time.Millisecond
	a := proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "proof-traffic", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$proof-traffic", SessionCreated: 3}
	handle, _ := front.handles.mint(a)
	control := dialTerminalWS(t, front, addr, handle, "control")
	defer control.Close()
	_ = readWSAttachment(t, control)
	outbound, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameLive, Source: integrationSource, Epoch: 1, Cut: 1, Data: []byte("traffic")}, attachmentwire.ServerToBrowser)
	if err != nil {
		t.Fatal(err)
	}
	stopTraffic := make(chan struct{})
	defer close(stopTraffic)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				frame, encodeErr := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: integrationSource, Epoch: 1, Data: []byte("x")}, attachmentwire.BrowserToServer)
				if encodeErr == nil {
					_ = control.WriteMessage(websocket.TextMessage, frame)
				}
				select {
				case broker.outbound <- outbound:
				default:
				}
			case <-stopTraffic:
				return
			}
		}
	}()
	if reason := readWSCloseReasonSkippingText(t, control); reason != "browser_liveness" {
		t.Fatalf("traffic ghost close reason=%q", reason)
	}
}

func TestBrowserProofExpiresObserveWithoutControlLease(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	front.browserProofTimeout = 100 * time.Millisecond
	a := proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "proof-observe", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$proof-observe", SessionCreated: 3}
	handle, _ := front.handles.mint(a)
	observe := dialTerminalWS(t, front, addr, handle, "observe")
	defer observe.Close()
	_ = readWSAttachment(t, observe)
	if reason := readWSCloseReason(t, observe); reason != "browser_liveness" {
		t.Fatalf("observe ghost close reason=%q", reason)
	}
	front.leases.mu.Lock()
	_, present := front.leases.entries[authorityKey(a)]
	front.leases.mu.Unlock()
	if present {
		t.Fatal("Observe browser proof acquired or mutated a Control lease")
	}
}

func TestWebSocketControlLeaseObservePolicyAndByteTransparency(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	a := proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "test", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$1", SessionCreated: 3}
	handle, err := front.handles.mint(a)
	if err != nil {
		t.Fatal(err)
	}

	control := dialTerminalWS(t, front, addr, handle, "control")
	if got := readWSAttachment(t, control); got.Type != terminal.FrameLive {
		t.Fatalf("control first frame=%+v", got)
	}
	second := dialTerminalWS(t, front, addr, handle, "control")
	if reason := readWSCloseReason(t, second); reason != "lease_held" {
		t.Fatalf("second control close reason=%q", reason)
	}
	_ = second.Close()
	stale, response, staleErr := websocket.DefaultDialer.Dial("ws://"+addr+"/ws?handle="+handle+"&mode=observe", http.Header{"Origin": []string{"http://" + addr}})
	if stale != nil {
		_ = stale.Close()
	}
	if staleErr == nil || response == nil || response.StatusCode != http.StatusForbidden {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("retired query authority: status=%d want=403", status)
	}

	payload := []byte{'x', '\r', 0x03, 0x1b, '[', 'A', '\t', 0x02, 'd'}
	writeWSAttachment(t, control, terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: integrationSource, Epoch: 1, Data: payload})
	select {
	case got := <-broker.inputs:
		if !bytes.Equal(got, payload) {
			t.Fatalf("input changed: %x != %x", got, payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("control input did not reach broker")
	}

	observeHandle, err := front.handles.mint(a)
	if err != nil {
		t.Fatal(err)
	}
	observe := dialTerminalWS(t, front, addr, observeHandle, "observe")
	if got := readWSAttachment(t, observe); got.Type != terminal.FrameLive {
		t.Fatalf("observe first frame=%+v", got)
	}
	writeWSAttachment(t, observe, terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: integrationSource, Epoch: 1, Data: []byte("denied")})
	writeWSAttachment(t, observe, terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: integrationSource, Epoch: 1, Mode: terminal.ModeControl})
	select {
	case got := <-broker.inputs:
		t.Fatalf("observe input reached broker: %q", got)
	case <-time.After(100 * time.Millisecond):
	}
	_ = observe.Close()

	_ = control.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		nextHandle, err := front.handles.mint(a)
		if err != nil {
			t.Fatal(err)
		}
		next := dialTerminalWS(t, front, addr, nextHandle, "control")
		message := readWSAttachment(t, next)
		if message.Type == terminal.FrameLive {
			_ = next.Close()
			break
		}
		_ = next.Close()
		if time.Now().After(deadline) {
			t.Fatalf("close did not release lease: %+v", message)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFailedBrokerAttachRollsBackLease(t *testing.T) {
	broker := startFakeAttachBroker(t, true)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	a := proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "test", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$1", SessionCreated: 3}
	handle, _ := front.handles.mint(a)
	failed := dialTerminalWS(t, front, addr, handle, "control")
	if reason := readWSCloseReason(t, failed); reason != "attach_failed" {
		t.Fatalf("first attach close reason=%q", reason)
	}
	_ = failed.Close()
	next := dialTerminalWS(t, front, addr, handle, "control")
	if message := readWSAttachment(t, next); message.Type != terminal.FrameLive {
		t.Fatalf("failed attach leaked lease: %+v", message)
	}
	_ = next.Close()
}

func TestExpiredControlCannotWriteAfterAnotherControlReacquires(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	clock := time.Unix(1000, 0)
	front.leases.now = func() time.Time { return clock }
	a := proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "test", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$1", SessionCreated: 3}
	handle, err := front.handles.mint(a)
	if err != nil {
		t.Fatal(err)
	}

	first := dialTerminalWS(t, front, addr, handle, "control")
	if got := readWSAttachment(t, first); got.Type != terminal.FrameLive {
		t.Fatalf("first control attach=%+v", got)
	}
	clock = clock.Add(LeaseTTL + time.Second)
	second := dialTerminalWS(t, front, addr, handle, "control")
	if got := readWSAttachment(t, second); got.Type != terminal.FrameLive {
		t.Fatalf("reacquired control attach=%+v", got)
	}

	writeWSAttachment(t, first, terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: integrationSource, Epoch: 1, Data: []byte("stale-input")})
	if reason := readWSCloseReason(t, first); reason != "lease_lost" {
		t.Fatalf("stale control close reason=%q", reason)
	}
	select {
	case input := <-broker.inputs:
		t.Fatalf("stale input reached broker: %q", input)
	case <-time.After(150 * time.Millisecond):
	}
	_ = second.Close()
}
