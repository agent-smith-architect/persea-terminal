package broker

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

// stallingConn reports when the broker starts a write after it is armed, and
// when the broker closes it.
type stallingConn struct {
	net.Conn
	armed     atomic.Bool
	writing   chan struct{}
	closed    chan struct{}
	writeOnce sync.Once
	closeOnce sync.Once
}

func (c *stallingConn) Write(p []byte) (int, error) {
	if c.armed.Load() {
		c.writeOnce.Do(func() { close(c.writing) })
	}
	return c.Conn.Write(p)
}

func (c *stallingConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// A front door at its flow limit stops reading output but still forwards
// INPUT. When the results the broker cannot write reach the bound, the broker
// closes the connection itself: teardown must not wait for the parked output
// write.
func TestInputBacklogOverflowClosesAStalledConnection(t *testing.T) {
	clock := &freshnessTestClock{now: time.Now()}
	f := newRecordingSettlementFixture(t, func(effects *UnifiedDevPaneEffects) {
		effects.inputFreshnessWindow = time.Second
		effects.inputResumeQuiet = 500 * time.Millisecond
		effects.freshnessNow = clock.Now
	})
	trigger := filepath.Join(t.TempDir(), "emit")
	session := f.startPaneCommand(t, "backlog", "while [ ! -e '"+trigger+"' ]; do sleep 0.02; done; printf 'OUTPUT-TO-STALL\\n'; exec /bin/cat")
	adopted, err := f.effects.AdoptSession(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	d, err := details(f.server, session)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(f.server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = f.cfg.Realm, "main", d.ID, d.Created

	serverSide, clientSide := net.Pipe()
	conn := &stallingConn{Conn: serverSide, writing: make(chan struct{}), closed: make(chan struct{})}
	finished := make(chan struct{})
	t.Cleanup(func() {
		_ = conn.Close()
		_ = clientSide.Close()
		<-finished
	})
	s := &Server{config: f.cfg, panes: f.registry, unified: f.effects}
	history := 5000
	go func() {
		defer close(finished)
		defer conn.Close() // handleConn closes after the attachment returns
		s.attachValidated(conn, &lockedWriter{w: conn}, f.server, d, proto.Control{Type: "attach", Mode: "control", Engine: "unified-dev", Authority: &authority, HistoryLimit: &history})
	}()

	client := &freshnessClient{t: t, conn: clientSide, effects: f.effects, key: adopted.Key}
	raw, _ := client.read()
	if ctrl, err := proto.DecodeControl(raw.Payload); err != nil || ctrl.Type != "attach_ok" {
		t.Fatalf("attach: %+v %v", ctrl, err)
	}
	_, client.prepared = client.read()
	if client.prepared.Type != terminal.FramePrepare {
		t.Fatalf("prepare: %s", client.prepared.Type)
	}
	client.attachment(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameReady, Source: client.prepared.Source, Epoch: client.prepared.Epoch, Cut: client.prepared.Cut})
	client.attachment(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: client.prepared.Source, Epoch: client.prepared.Epoch, Mode: terminal.ModeControl})
	for {
		if _, frame := client.read(); frame.Type == terminal.FrameMode && frame.Mode == terminal.ModeControl {
			break
		}
	}

	// Output the client never reads parks the broker's writer.
	conn.armed.Store(true)
	if err := os.WriteFile(trigger, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-conn.writing:
	case <-time.After(5 * time.Second):
		t.Fatal("no output was written")
	}
	clock.advance(2 * time.Second)
	_ = clientSide.SetWriteDeadline(time.Now().Add(10 * time.Second))
	for i := 0; i < maxInputBacklog+1; i++ {
		client.input("x")
	}
	select {
	case <-conn.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("backlog overflow did not close the connection")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("attachment did not end after the close")
	}
}
