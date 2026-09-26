package broker

import (
	"bytes"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

// A browser page asks for its mode at every COMMIT: CONTROL when it may type,
// OBSERVE when it only watches. The answer follows the whole history backlog,
// so it also tells the page that its view has caught up. An observe
// attachment's OBSERVE request must be answered with MODE OBSERVE, and it must
// change nothing for the attachment that holds control.
func TestUnifiedTerminalE2E1ObserveModeRequestIsAnsweredAndLeavesControl(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux unified terminal")
	}
	socket, authority := unifiedE2E1StartShellBroker(t)
	control, prepared, controlStream := unifiedE2E1OpenStream(t, socket, authority)
	defer control.Close()
	observe, observed := unifiedE2E1OpenObserver(t, socket, authority)
	defer observe.Close()

	const token = "E2E1-OBSERVE-MODE-KEEPS-CONTROL"
	unifiedE2E1FitInput(t, control, prepared, "printf '"+token+"\\n'\n")
	// The shell echoes the command with a literal \n; only the printed line
	// ends with CR LF.
	want := []byte(token + "\r\n")
	unifiedE2E1ReadEcho(t, control, controlStream.text, want, "control")
	unifiedE2E1ReadEcho(t, observe, observed, want, "observer")
}

func unifiedE2E1StartShellBroker(t *testing.T) (string, proto.Authority) {
	t.Helper()
	disposable := newDisposable(t)
	disposable.run("new-session", "-d", "-s", "anchor", "-x", "80", "-y", "24", "sh")
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	cfg := unifiedE2E1DevConfig(t, tmuxServer, t.TempDir())
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(shortSocketDir(t), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ServeWithPaneEffects(listener, cfg, effects) }()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("unified broker did not stop")
		}
	})
	createConn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	unifiedE2E1Hello(t, createConn)
	created := unifiedE2E1Control(t, createConn, proto.Control{Type: "create", ServerLabel: "main", Name: "unified_target"})
	_ = createConn.Close()
	if created.Type != "create_ok" {
		t.Fatalf("create: %+v", created)
	}
	sessionID, err := tmuxOutput(tmuxServer, "list-sessions", "-f", "#{==:#{session_name},unified_target}", "-F", "#{session_id}")
	if err != nil {
		t.Fatal(err)
	}
	d, err := details(tmuxServer, strings.TrimSpace(sessionID))
	if err != nil {
		t.Fatalf("details session=%q: %v", sessionID, err)
	}
	authority, err := incarnation(tmuxServer)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = "e2e1", "main", d.ID, d.Created
	return listener.Addr().String(), authority
}

// unifiedE2E1OpenObserver admits an observe attachment the way an observe
// page does: READY after PREPARE, then MODE_REQUEST OBSERVE at COMMIT. It
// returns the output received before MODE, and fails unless MODE is OBSERVE.
func unifiedE2E1OpenObserver(t *testing.T, socket string, authority proto.Authority) (net.Conn, []byte) {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	unifiedE2E1Hello(t, conn)
	history := 5000
	attached := unifiedE2E1Control(t, conn, proto.Control{Type: "attach", Mode: "observe", Engine: "unified-dev", Authority: &authority, HistoryLimit: &history})
	if attached.Type != "attach_ok" {
		conn.Close()
		t.Fatalf("observe attach: %+v", attached)
	}
	if err := conn.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	send := func(frame terminal.Frame) {
		encoded, err := attachmentwire.Encode(frame, attachmentwire.BrowserToServer)
		if err != nil || proto.WriteFrame(conn, proto.FrameAttachment, encoded) != nil {
			conn.Close()
			t.Fatalf("observer %s: %v", frame.Type, err)
		}
	}
	var text []byte
	for {
		raw, err := proto.ReadFrame(conn)
		if err != nil {
			conn.Close()
			t.Fatalf("observer admission read: %v", err)
		}
		if refusal := unifiedE2E1BrokerError(raw); refusal != "" {
			conn.Close()
			t.Fatalf("observer refused during admission: %s", refusal)
		}
		if raw.Type != proto.FrameAttachment {
			continue
		}
		frame, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		switch frame.Type {
		case terminal.FramePrepare:
			text = append(text[:0], frame.Replay...)
			send(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameReady, Source: frame.Source, Epoch: frame.Epoch, Cut: frame.Cut})
		case terminal.FrameCommit:
			send(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: frame.Source, Epoch: frame.Epoch, Mode: terminal.ModeObserve})
		case terminal.FrameLive:
			text = append(text, frame.Data...)
		case terminal.FrameMode:
			if frame.Mode != terminal.ModeObserve {
				conn.Close()
				t.Fatalf("observer was answered with MODE %s", frame.Mode)
			}
			return conn, text
		}
	}
}

// unifiedE2E1ReadEcho reads one attachment until its output holds want. A
// MODE frame on the way fails the test: nothing the other attachment asked
// for may change this one's mode.
func unifiedE2E1ReadEcho(t *testing.T, conn net.Conn, text, want []byte, who string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for !bytes.Contains(text, want) {
		raw, err := proto.ReadFrame(conn)
		if err != nil {
			t.Fatalf("%s: waiting for %q: %v (output=%q)", who, want, err, text)
		}
		if refusal := unifiedE2E1BrokerError(raw); refusal != "" {
			t.Fatalf("%s: refused while waiting for %q: %s", who, want, refusal)
		}
		if raw.Type != proto.FrameAttachment {
			continue
		}
		frame, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
		if err != nil {
			t.Fatal(err)
		}
		switch frame.Type {
		case terminal.FrameLive:
			text = append(text, frame.Data...)
		case terminal.FrameMode:
			t.Fatalf("%s: mode changed to %s", who, frame.Mode)
		}
	}
}

// unifiedE2E1BrokerError names a broker error control, or returns "".
func unifiedE2E1BrokerError(raw proto.Frame) string {
	if raw.Type != proto.FrameControl {
		return ""
	}
	control, err := proto.DecodeControl(raw.Payload)
	if err != nil {
		return "undecodable control: " + err.Error()
	}
	if control.Type != "error" {
		return ""
	}
	return control.Code + " " + control.Msg
}
