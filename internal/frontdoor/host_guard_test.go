package frontdoor

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

type dataBroker struct {
	listener     net.Listener
	authority    proto.Authority
	snapshotCode string
	accepted     atomic.Int32
}

func startDataBroker(t *testing.T, authority proto.Authority, snapshotCode string) *dataBroker {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join(shortTestDir(t), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	b := &dataBroker{listener: listener, authority: authority, snapshotCode: snapshotCode}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			b.accepted.Add(1)
			go b.serve(conn)
		}
	}()
	return b
}

func (b *dataBroker) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	frame, err := proto.ReadFrame(conn)
	if err != nil || frame.Type != proto.FrameControl {
		return
	}
	hello, err := proto.DecodeControl(frame.Payload)
	if err != nil || hello.Type != "hello" {
		return
	}
	if writeControl(conn, proto.Control{Type: "hello_ok", V: 1}) != nil {
		return
	}
	frame, err = proto.ReadFrame(conn)
	if err != nil || frame.Type != proto.FrameControl {
		return
	}
	request, err := proto.DecodeControl(frame.Payload)
	if err != nil {
		return
	}
	switch request.Type {
	case "inventory":
		_ = writeControl(conn, proto.Control{Type: "inventory_ok", Servers: []proto.ServerInventory{{Label: b.authority.Server, Status: "ok", Sessions: []proto.Session{{Authority: b.authority, Name: "alpha", Width: 80, Height: 24}}}}})
	case "snapshot":
		if b.snapshotCode != "" {
			_ = writeControl(conn, proto.Control{Type: "error", Code: b.snapshotCode})
			return
		}
		historyRows := 0
		if writeControl(conn, proto.Control{Type: "snapshot_ok", Width: 80, Height: 24, Pane: "%1", FrozenAt: time.Now().UnixMilli(), Depth: SnapshotLineLimit, HistoryRows: &historyRows}) != nil {
			return
		}
		if proto.WriteFrame(conn, proto.FrameData, []byte("snapshot\n")) != nil {
			return
		}
		_ = writeControl(conn, proto.Control{Type: "snapshot_end"})
	}
}

func TestDataAPIsRejectHostBeforeBrokerAndServeCanonicalHost(t *testing.T) {
	const listen = "127.0.0.1:8080"
	authority := proto.Authority{Realm: "r", Server: "main", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "test", BootID: "boot", ServerPID: 1, ServerStart: 2, SessionID: "$0", SessionCreated: 3}
	broker := startDataBroker(t, authority, "")
	s := newServer(config.Front{Realms: []config.Realm{{Name: "r", Socket: broker.listener.Addr().String()}}, HandleTTLSeconds: 120, HandleCapacity: 10}, ".", listen)
	h := s.handler()

	inventory := httptest.NewRequest(http.MethodGet, "http://"+listen+"/api/inventory", nil)
	inventoryResult := httptest.NewRecorder()
	h.ServeHTTP(inventoryResult, inventory)
	if inventoryResult.Code != http.StatusOK {
		t.Fatalf("canonical inventory status = %d, want 200", inventoryResult.Code)
	}
	var body struct {
		Realms []realmView `json:"realms"`
	}
	if err := json.Unmarshal(inventoryResult.Body.Bytes(), &body); err != nil || len(body.Realms) != 1 || len(body.Realms[0].Servers) != 1 || len(body.Realms[0].Servers[0].Sessions) != 1 {
		t.Fatalf("canonical inventory body = %+v %v", body, err)
	}
	handle := body.Realms[0].Servers[0].Sessions[0].Handle
	if broker.accepted.Load() != 1 {
		t.Fatalf("canonical inventory broker calls = %d, want 1", broker.accepted.Load())
	}

	for _, path := range []string{"/api/inventory", "/api/snapshot?handle=" + url.QueryEscape(handle)} {
		req := httptest.NewRequest(http.MethodGet, "http://"+listen+path, nil)
		req.Host = "hostile.invalid"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("hostile %s status = %d, want 400", path, rr.Code)
		}
		if broker.accepted.Load() != 1 {
			t.Fatalf("hostile %s reached broker: calls=%d", path, broker.accepted.Load())
		}
	}

	snapshot := httptest.NewRequest(http.MethodGet, "http://"+listen+"/api/snapshot?handle="+url.QueryEscape(handle), nil)
	snapshotResult := httptest.NewRecorder()
	h.ServeHTTP(snapshotResult, snapshot)
	if snapshotResult.Code != http.StatusOK {
		t.Fatalf("canonical snapshot status = %d, want 200", snapshotResult.Code)
	}
	if broker.accepted.Load() != 2 {
		t.Fatalf("canonical snapshot broker calls = %d, want 2", broker.accepted.Load())
	}
}

func TestSnapshotBrokerErrorsMapToGoneAndBadGateway(t *testing.T) {
	const listen = "127.0.0.1:8080"
	for code, wantStatus := range map[string]int{"stale_target": http.StatusGone, "snapshot_failed": http.StatusBadGateway} {
		t.Run(code, func(t *testing.T) {
			authority := proto.Authority{Realm: "r", Server: "main", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "test", BootID: "boot", ServerPID: 1, ServerStart: 2, SessionID: "$0", SessionCreated: 3}
			broker := startDataBroker(t, authority, code)
			s := newServer(config.Front{Realms: []config.Realm{{Name: "r", Socket: broker.listener.Addr().String()}}, HandleTTLSeconds: 120, HandleCapacity: 10}, ".", listen)
			handle, err := s.handles.mint(authority)
			if err != nil {
				t.Fatal(err)
			}
			rr := httptest.NewRecorder()
			s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://"+listen+"/api/snapshot?handle="+url.QueryEscape(handle), nil))
			if rr.Code != wantStatus {
				t.Fatalf("snapshot %s status = %d, want %d", code, rr.Code, wantStatus)
			}
		})
	}
}
