package frontdoor

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

func TestBinaryAttachmentBrokerEncodingRelaysExactly(t *testing.T) {
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	handle, err := front.handles.mint(flowAuthority("binary-relay"))
	if err != nil {
		t.Fatal(err)
	}
	ws := dialTerminalWS(t, front, addr, handle, "observe")
	defer ws.Close()
	_ = readWSAttachment(t, ws)
	frames := []terminal.Frame{
		{Type: terminal.FrameLive, Cut: 2, Data: []byte{0xff, 0, 0x1b, '[', 0xe2}},
		{Type: terminal.FramePrepare, Cut: 3, Kind: terminal.CutHistory, Request: ^uint64(0), EffectiveHistoryRows: 10000, Columns: 80, Rows: 24, History: []string{"界", "older"}, Truncated: true, Replay: []byte{0x82, 0xac, 0xff}},
		{Type: terminal.FrameCommit, Cut: 3},
		{Type: terminal.FrameMode, Mode: terminal.ModeObserve, Reason: "observing"},
		{Type: terminal.FrameEnd, Reason: "closed"},
	}
	for _, frame := range frames {
		frame.Version, frame.Source, frame.Epoch = 1, integrationSource, ^uint64(0)
		core, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		// This is the broker writer's translation boundary.
		wire, err := attachmentwire.EncodeCoreJSON(core, attachmentwire.ServerToBrowser)
		if err != nil {
			t.Fatal(err)
		}
		broker.outbound <- wire
		_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
		kind, got, err := ws.ReadMessage()
		if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(got, wire) {
			t.Fatalf("%s relay changed message kind or bytes: kind=%d err=%v", frame.Type, kind, err)
		}
		decoded, err := attachmentwire.Decode(got, attachmentwire.ServerToBrowser)
		if err != nil || decoded.Type != frame.Type || !bytes.Equal(decoded.Data, frame.Data) || !bytes.Equal(decoded.Replay, frame.Replay) {
			t.Fatalf("%s relay lost bytes: %v", frame.Type, err)
		}
	}
}

func TestBinaryAttachmentRelayValidatesEveryFrame(t *testing.T) {
	for _, mutation := range []string{"text", "header length", "trailing bytes", "missing bytes", "direction", "metadata"} {
		t.Run(mutation, func(t *testing.T) {
			broker := startFakeAttachBroker(t, false)
			front, addr := startFrontHTTP(t, broker.listener.Addr().String())
			handle, err := front.handles.mint(flowAuthority("binary-invalid"))
			if err != nil {
				t.Fatal(err)
			}
			ws := dialTerminalWS(t, front, addr, handle, "observe")
			defer ws.Close()
			_ = readWSAttachment(t, ws)
			wire, err := attachmentwire.Encode(terminal.Frame{Version: 1, Type: terminal.FrameLive, Source: integrationSource, Epoch: 1, Cut: 1, Data: []byte{0xff}}, attachmentwire.ServerToBrowser)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "text":
				wire = wire[4 : len(wire)-1]
			case "header length":
				binary.BigEndian.PutUint32(wire, attachmentwire.MaxHeaderBytes+1)
			case "trailing bytes":
				wire = append(wire, 0)
			case "missing bytes":
				wire = wire[:len(wire)-1]
			case "direction":
				ready, err := attachmentwire.Encode(terminal.Frame{Version: 1, Type: terminal.FrameReady, Source: integrationSource, Epoch: 1, Cut: 1}, attachmentwire.BrowserToServer)
				if err != nil {
					t.Fatal(err)
				}
				wire = append(make([]byte, 4), ready...)
				binary.BigEndian.PutUint32(wire, uint32(len(ready)))
			case "metadata":
				wire = bytes.Replace(wire, []byte(`"epoch":"1"`), []byte(`"epoch":"0"`), 1)
			}
			broker.outbound <- wire
			if reason := readWSCloseReason(t, ws); reason != "broker_protocol" {
				t.Fatalf("invalid frame crossed relay: close=%q", reason)
			}
		})
	}
}

func TestBinaryAttachmentHardProtocolBump(t *testing.T) {
	for _, version := range []string{"persea-terminal.v1", "persea-terminal.v2", "persea-terminal.v3"} {
		request, err := http.NewRequest("GET", "http://localhost/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Sec-WebSocket-Protocol", strings.Join([]string{version, "persea-handle.h", "persea-mode.observe", "persea-csrf." + testCSRF, "persea-history.5000", "persea-engine.unified-dev"}, ", "))
		if _, ok := parseWSAuthority(request); ok != (version == "persea-terminal.v3") {
			t.Fatalf("page protocol %s acceptance=%v", version, ok)
		}
	}
}

func TestBinaryAttachmentFrontDoorPrePrepareOrder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		send     func(net.Conn)
		wantCode string
	}{
		{
			name: "end_before_prepare_rejected_as_broker_protocol",
			send: func(c net.Conn) {
				payload, err := attachmentwire.Encode(terminal.Frame{Version: 1, Type: terminal.FrameEnd, Source: integrationSource, Epoch: 1, Reason: "fault"}, attachmentwire.ServerToBrowser)
				if err == nil {
					_ = proto.WriteFrame(c, proto.FrameAttachment, payload)
				}
			},
			wantCode: "broker_protocol",
		},
		{
			name: "control_error_after_attach_ok_delivers_attach_failed",
			send: func(c net.Conn) {
				_ = writeControl(c, proto.Control{Type: "error", Code: "attach_failed"})
			},
			wantCode: "attach_failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "fd-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			sock := filepath.Join(dir, "b.sock")
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				if _, err := proto.ReadFrame(c); err != nil {
					return
				}
				_ = writeControl(c, proto.Control{Type: "hello_ok", V: 1})
				if _, err := proto.ReadFrame(c); err != nil {
					return
				}
				_ = writeControl(c, proto.Control{Type: "attach_ok", Cols: 80, Rows: 24, InputMax: terminal.MaxInputBytes})
				tc.send(c)
				_, _ = proto.ReadFrame(c)
			}()
			front, addr := startFrontHTTP(t, sock)
			handle, err := front.handles.mint(flowAuthority("pre-prepare-" + tc.name))
			if err != nil {
				t.Fatal(err)
			}
			ws := dialTerminalWS(t, front, addr, handle, "observe")
			defer ws.Close()
			if reason := readWSCloseReason(t, ws); reason != tc.wantCode {
				t.Fatalf("close=%q want %q", reason, tc.wantCode)
			}
			if _, err := front.bindings.resolve(front.cfg.Ingress.OperatorLogin, integrationSource); err == nil {
				t.Fatalf("source binding leaked on %s", tc.name)
			}
		})
	}
}
