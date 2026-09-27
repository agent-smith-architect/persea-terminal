package frontdoor

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"persea-terminal/internal/attachmentwire"
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
