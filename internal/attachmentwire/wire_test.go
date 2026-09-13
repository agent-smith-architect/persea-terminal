package attachmentwire

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

func TestWireRoundTripAllFrameFamilies(t *testing.T) {
	server := []terminal.Frame{
		{Version: 1, Type: terminal.FramePrepare, Source: "source", Epoch: ^uint64(0), Cut: 9, Kind: terminal.CutInitial, Columns: 80, Rows: 24, History: []string{}, Replay: []byte{}, Truncated: false},
		{Version: 1, Type: terminal.FramePrepare, Source: "source", Epoch: 1, Cut: 10, Kind: terminal.CutHistory, Request: 7, EffectiveHistoryRows: 1_000, Columns: 80, Rows: 24, History: []string{"older"}, Replay: []byte{}, Truncated: false},
		{Version: 1, Type: terminal.FrameCommit, Source: "source", Epoch: 1, Cut: 9},
		{Version: 1, Type: terminal.FrameLive, Source: "source", Epoch: 1, Cut: 9, Data: []byte{0, 1, 2, 255}},
		{Version: 1, Type: terminal.FrameMode, Source: "source", Epoch: 1, Mode: terminal.ModeControl, Reason: "granted"},
		{Version: 1, Type: terminal.FrameEnd, Source: "source", Epoch: 1, Reason: "closed"},
	}
	browser := []terminal.Frame{
		{Version: 1, Type: terminal.FrameReady, Source: "source", Epoch: 1, Cut: 9},
		{Version: 1, Type: terminal.FrameDefer, Source: "source", Epoch: 1, Cut: 9, Reason: "ACTIVE_CONTACT_OR_SCROLL_GESTURE"},
		{Version: 1, Type: terminal.FrameModeRequest, Source: "source", Epoch: 1, Mode: terminal.ModeControl},
		{Version: 1, Type: terminal.FrameInput, Source: "source", Epoch: 1, Data: []byte{0, 1, 2, 255}},
		{Version: 1, Type: terminal.FrameResize, Source: "source", Epoch: 1, Columns: 120, Rows: 40},
		{Version: 1, Type: terminal.FrameHistory, Source: "source", Epoch: 1, Request: 7, HistoryRows: 1_000},
	}
	for _, group := range []struct {
		direction Direction
		frames    []terminal.Frame
	}{{ServerToBrowser, server}, {BrowserToServer, browser}} {
		for _, want := range group.frames {
			raw, err := Encode(want, group.direction)
			if err != nil {
				t.Fatalf("Encode(%s): %v", want.Type, err)
			}
			got, err := Decode(raw, group.direction)
			if err != nil {
				t.Fatalf("Decode(%s): %v", want.Type, err)
			}
			if got.Version != want.Version || got.Type != want.Type || got.Source != want.Source || got.Epoch != want.Epoch || got.Cut != want.Cut ||
				got.Kind != want.Kind || got.Mode != want.Mode || got.Columns != want.Columns || got.Rows != want.Rows || got.Request != want.Request ||
				got.HistoryRows != want.HistoryRows || got.EffectiveHistoryRows != want.EffectiveHistoryRows || got.Truncated != want.Truncated ||
				got.Reason != want.Reason || !bytes.Equal(got.Data, want.Data) || !bytes.Equal(got.Replay, want.Replay) || strings.Join(got.History, "\x00") != strings.Join(want.History, "\x00") {
				t.Fatalf("round trip mismatch for %s: %#v != %#v", want.Type, got, want)
			}
		}
	}
}

func TestWorstCaseValidPrepareFitsDocumentedFiniteWireBound(t *testing.T) {
	if MaxWireBytes != proto.MaxAttachment {
		t.Fatalf("attachment bounds differ: wire=%d proto=%d", MaxWireBytes, proto.MaxAttachment)
	}
	if MaxWireBytes != 16<<20 {
		t.Fatalf("unexpected wire bound: %d", MaxWireBytes)
	}
	row := strings.Repeat("\x01", terminal.HistoryRowByteCap-1)
	rows := make([]string, terminal.HistoryByteCap/(len(row)+1))
	for i := range rows {
		rows[i] = row
	}
	frame := terminal.Frame{Version: 1, Type: terminal.FramePrepare, Source: strings.Repeat("s", 256), Epoch: 1, Cut: 1, Kind: terminal.CutInitial, Columns: 164, Rows: 38, History: rows, Replay: bytes.Repeat([]byte{0xff}, terminal.ReplayByteCap)}
	raw, err := Encode(frame, ServerToBrowser)
	if err != nil {
		t.Fatalf("worst-case valid PREPARE rejected: %v", err)
	}
	if len(raw) >= MaxWireBytes {
		t.Fatalf("worst-case wire=%d bound=%d", len(raw), MaxWireBytes)
	}
	if _, err := Decode(bytes.Repeat([]byte{'x'}, MaxWireBytes+1), ServerToBrowser); !errors.Is(err, terminal.ErrMalformed) {
		t.Fatalf("oversized wire did not fail closed: %v", err)
	}
}

func TestWireUsesDecimalUint64AndCanonicalBase64(t *testing.T) {
	frame := terminal.Frame{Version: 1, Type: terminal.FrameLive, Source: "source", Epoch: ^uint64(0), Cut: 7, Data: []byte{0, 1, 2, 255}}
	raw, err := Encode(frame, ServerToBrowser)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{`"epoch":"18446744073709551615"`, `"cut":"7"`, `"data":"AAEC/w=="`} {
		if !strings.Contains(text, want) {
			t.Fatalf("wire payload lacks %s: %s", want, text)
		}
	}
}

func TestWireRejectsDirectionShapeAndNonCanonicalScalars(t *testing.T) {
	valid := `{"version":1,"type":"READY","source":"source","epoch":"1","cut":"1"}`
	cases := []string{
		`{"version":1,"type":"READY","source":"source","epoch":1,"cut":"1"}`,
		`{"version":1,"type":"READY","source":"source","epoch":"01","cut":"1"}`,
		`{"version":1,"type":"READY","source":"source","epoch":"18446744073709551616","cut":"1"}`,
		`{"version":1,"type":"READY","source":"source","epoch":"1","cut":"1","reason":""}`,
		`{"version":1,"type":"READY","source":"source","epoch":"1","cut":"1","cut":"2"}`,
		`{"version":1,"type":"INPUT","source":"source","epoch":"1","data":"AQ"}`,
		`{"version":1,"type":"RESIZE_REQUEST","source":"source","epoch":"1","columns":0,"rows":24}`,
		`{"version":1,"type":"RESIZE_REQUEST","source":"source","epoch":"1","columns":80,"rows":1001}`,
		`{"version":1,"type":"DEFER","source":"source","epoch":"1","cut":"1","reason":"NOT_A_BROWSER_REASON"}`,
		`{"version":1,"type":"HISTORY_REQUEST","source":"source","epoch":"1","request":"1","history_rows":999}`,
		`{"version":1,"type":"HISTORY_REQUEST","source":"source","epoch":"2","request":"0","history_rows":1000}`,
	}
	for _, raw := range cases {
		if _, err := Decode([]byte(raw), BrowserToServer); !errors.Is(err, terminal.ErrMalformed) {
			t.Fatalf("invalid payload did not fail malformed: %s: %v", raw, err)
		}
	}
	if _, err := Decode([]byte(valid), ServerToBrowser); !errors.Is(err, terminal.ErrMalformed) {
		t.Fatalf("wrong-direction browser frame accepted: %v", err)
	}
	frame := terminal.Frame{Version: 1, Type: terminal.FrameReady, Source: "source", Epoch: 1, Cut: 1}
	if _, err := Encode(frame, ServerToBrowser); !errors.Is(err, terminal.ErrMalformed) {
		t.Fatalf("wrong-direction typed frame accepted: %v", err)
	}
	invalidDefer := terminal.Frame{Version: 1, Type: terminal.FrameDefer, Source: "source", Epoch: 1, Cut: 1, Reason: "NOT_A_BROWSER_REASON"}
	if _, err := Encode(invalidDefer, BrowserToServer); !errors.Is(err, terminal.ErrMalformed) {
		t.Fatalf("non-canonical typed DEFER reason accepted: %v", err)
	}
	for _, reason := range []string{"ACTIVE_CONTACT_OR_SCROLL_GESTURE", "INTERSECTING_DOM_SELECTION", "AFFECTED_XTERM_SELECTION", "VISIBLE_ANCHOR_WOULD_BE_EVICTED"} {
		canonical := invalidDefer
		canonical.Reason = reason
		raw, err := Encode(canonical, BrowserToServer)
		if err != nil {
			t.Fatalf("canonical DEFER reason %q rejected by Encode: %v", reason, err)
		}
		if _, err := Decode(raw, BrowserToServer); err != nil {
			t.Fatalf("canonical DEFER reason %q rejected by Decode: %v", reason, err)
		}
	}
}

func TestEncodeCoreJSONReusesTerminalFrameValidation(t *testing.T) {
	core := []byte(`{"version":1,"type":"LIVE","source":"source","epoch":2,"cut":3,"data":"AQI="}`)
	wire, err := EncodeCoreJSON(core, ServerToBrowser)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != `{"cut":"3","data":"AQI=","epoch":"2","source":"source","type":"LIVE","version":1}` {
		t.Fatalf("unexpected canonical translation: %s", wire)
	}
	if _, err := EncodeCoreJSON([]byte(`{"version":1,"type":"LIVE","source":"source","epoch":2,"cut":3,"data":""}`), ServerToBrowser); err == nil {
		t.Fatal("invalid core frame bypassed terminal validation")
	}
}
