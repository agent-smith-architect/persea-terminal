package frontdoor

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"persea-terminal/internal/proto"
)

func TestReadPumpsExitWhenHandlerReturnsEarly(t *testing.T) {
	baseline := runtime.NumGoroutine()

	var wire bytes.Buffer
	if err := proto.WriteFrame(&wire, proto.FrameData, []byte("buffered")); err != nil {
		t.Fatal(err)
	}
	frames := make(chan readBrokerFrame, 1)
	frames <- readBrokerFrame{}
	brokerDone := make(chan struct{})
	brokerExited := make(chan struct{})
	go func() {
		brokerReadPump(&wire, frames, brokerDone)
		close(brokerExited)
	}()
	close(brokerDone)
	select {
	case <-brokerExited:
	case <-time.After(time.Second):
		t.Fatal("broker read pump remained blocked after handler exit")
	}

	ready := make(chan struct{})
	cancel := make(chan struct{})
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		reads := make(chan wsRead, 1)
		reads <- wsRead{}
		done := make(chan struct{})
		exited := make(chan struct{})
		go func() {
			wsReadPump(ws, reads, done)
			close(exited)
		}()
		close(ready)
		<-cancel // simulate a handler return before the pump can publish
		close(done)
		_ = ws.Close()
		select {
		case <-exited:
		case <-time.After(time.Second):
			t.Error("websocket read pump remained blocked after handler exit")
		}
		close(serverDone)
	}))
	defer server.Close()
	url := "ws" + server.URL[len("http"):]
	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	<-ready
	if err := client.WriteMessage(websocket.BinaryMessage, []byte("queued")); err != nil {
		t.Fatal(err)
	}
	close(cancel)
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("early-exit websocket handler did not finish")
	}

	runtime.GC()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+8 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("read-pump goroutine leak: baseline=%d current=%d", baseline, runtime.NumGoroutine())
}
