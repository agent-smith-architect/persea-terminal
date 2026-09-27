package frontdoor

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// The product upgrader's per-connection buffers are small: four upgraded
// connections retain well under 2 MiB (buffers the size of the largest
// attachment would retain 32 MiB each), and messages far larger than a buffer
// still cross intact in both directions.
func TestWebSocketUpgraderRetainsSmallBuffers(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, _ := startFrontHTTP(t, broker.listener.Addr().String())
	upgraded := make(chan *websocket.Conn, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := front.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		upgraded <- conn
	}))
	defer server.Close()
	var clients, servers []*websocket.Conn
	defer func() {
		for _, conn := range append(clients, servers...) {
			_ = conn.Close()
		}
	}()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < 4; i++ {
		client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		servers = append(servers, <-upgraded)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if retained := int64(after.HeapAlloc) - int64(before.HeapAlloc); retained > 2<<20 {
		t.Fatalf("four upgraded connections retain %d bytes", retained)
	}

	payload := bytes.Repeat([]byte("x"), 512<<10)
	for name, pair := range map[string][2]*websocket.Conn{"to browser": {servers[0], clients[0]}, "from browser": {clients[1], servers[1]}} {
		written := make(chan error, 1)
		go func() { written <- pair[0].WriteMessage(websocket.TextMessage, payload) }()
		_, got, err := pair[1].ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("a %d-byte message %s changed in transit", len(payload), name)
		}
	}
}
