package broker

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

// A keepalive ping never holds up the input behind it. While the front
// door's flow window is closed the broker's output writer sits parked on a
// full socket; a pong written from the read loop would wait behind that
// writer, and so would the next input frame.
func TestUnifiedAttachmentPingDoesNotStallInputBehindParkedOutput(t *testing.T) {
	f := newRecordingSettlementFixture(t, func(*UnifiedDevPaneEffects) {})
	session := f.startPaneCommand(t, "ping-fence", "exec /bin/sh")
	if _, err := f.effects.AdoptSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &Server{config: f.cfg, panes: f.registry, unified: f.effects}
	go func() { _ = server.accept(listener) }()
	d, err := details(f.server, session)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(f.server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = f.cfg.Realm, "main", d.ID, d.Created
	conn, prepared := unifiedE2E1OpenAttachment(t, listener.Addr().String(), authority)
	defer conn.Close()
	input := func(text string) {
		t.Helper()
		raw, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: prepared.Source, Epoch: prepared.Epoch, Data: []byte(text)}, attachmentwire.BrowserToServer)
		if err != nil || proto.WriteFrame(conn, proto.FrameAttachment, raw) != nil {
			t.Fatalf("input %q: %v", text, err)
		}
	}
	shown := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(f.capture(t, "ping-fence"), want) {
			if time.Now().After(deadline) {
				t.Fatalf("%q never reached the pane", want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	// This client reads nothing more: a MiB of output fills the socket and
	// parks the broker's writer, as a closed flow window does.
	input("head -c 1048576 /dev/zero | tr '\\0' x; printf '\\nFLOOD-%s\\n' DONE\n")
	shown("FLOOD-DONE")
	ping, err := proto.MarshalControl(proto.Control{Type: "ping"})
	if err != nil || proto.WriteFrame(conn, proto.FrameControl, ping) != nil {
		t.Fatalf("ping: %v", err)
	}
	input("printf 'AFTER-%s\\n' PING\n")
	shown("AFTER-PING")
}
