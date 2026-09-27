package attachmentwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"persea-terminal/internal/terminal"
)

func binaryTestFrame(header string, payload []byte) []byte {
	raw := make([]byte, 4+len(header)+len(payload))
	binary.BigEndian.PutUint32(raw, uint32(len(header)))
	copy(raw[4:], header)
	copy(raw[4+len(header):], payload)
	return raw
}

func TestBinaryPayloadsLossless(t *testing.T) {
	random := rand.New(rand.NewSource(19))
	for _, kind := range []terminal.FrameType{terminal.FrameLive, terminal.FramePrepare} {
		maximum := terminal.MaxEgressBytes
		if kind == terminal.FramePrepare {
			maximum = terminal.ReplayByteCap
		}
		for _, size := range []int{1, 2, 3, 256, 1023, maximum} {
			t.Run(fmt.Sprintf("%s/%d", kind, size), func(t *testing.T) {
				payload := make([]byte, size)
				_, _ = random.Read(payload)
				if size == 256 {
					for i := range payload {
						payload[i] = byte(i)
					}
				}
				want := terminal.Frame{Version: 1, Type: kind, Source: "source", Epoch: ^uint64(0), Cut: ^uint64(0)}
				if kind == terminal.FrameLive {
					want.Data = payload
				} else {
					want.Kind, want.Columns, want.Rows = terminal.CutHistory, 1000, 1000
					want.Request, want.EffectiveHistoryRows = ^uint64(0), 10000
					want.History, want.Truncated, want.Replay = []string{"é\x1b[0m", "界"}, true, payload
				}
				raw, err := Encode(want, ServerToBrowser)
				if err != nil {
					t.Fatal(err)
				}
				got, err := Decode(raw, ServerToBrowser)
				if err != nil {
					t.Fatal(err)
				}
				if got.Epoch != want.Epoch || got.Cut != want.Cut || got.Source != want.Source ||
					got.Request != want.Request || got.EffectiveHistoryRows != want.EffectiveHistoryRows ||
					got.Columns != want.Columns || got.Rows != want.Rows || got.Truncated != want.Truncated ||
					!bytes.Equal(got.Data, want.Data) || !bytes.Equal(got.Replay, want.Replay) ||
					strings.Join(got.History, "\n") != strings.Join(want.History, "\n") {
					t.Fatal("binary round trip changed the frame")
				}
				headerLength := int(binary.BigEndian.Uint32(raw))
				if !bytes.Equal(raw[4+headerLength:], payload) {
					t.Fatal("wire payload is not the original raw bytes")
				}
			})
		}
	}
	// Each boundary is a separate LIVE record, including invalid UTF-8 and
	// incomplete escape / Unicode sequences. The codec must not interpret them.
	for _, payload := range [][]byte{{0xff, 0xc0, 0, 0x80}, {0x1b}, {'[', '3'}, {'1', 'm', 0xe2}, {0x82}, {0xac}} {
		raw, err := Encode(terminal.Frame{Version: 1, Type: terminal.FrameLive, Source: "s", Epoch: 1, Cut: 1, Data: payload}, ServerToBrowser)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(raw, ServerToBrowser)
		if err != nil || !bytes.Equal(got.Data, payload) {
			t.Fatalf("split bytes changed: %x: %v", payload, err)
		}
	}
}

func TestBinaryMalformedEnvelope(t *testing.T) {
	header := `{"version":1,"type":"LIVE","source":"source","epoch":"1","cut":"1","data":1}`
	valid := binaryTestFrame(header, []byte{0xff})
	withLength := func(length uint32) []byte {
		raw := bytes.Clone(valid)
		binary.BigEndian.PutUint32(raw, length)
		return raw
	}
	cases := map[string][]byte{
		"empty":               nil,
		"short prefix":        valid[:3],
		"zero header":         withLength(0),
		"truncated header":    withLength(uint32(len(valid))),
		"oversized header":    withLength(MaxHeaderBytes + 1),
		"overflow header":     withLength(^uint32(0)),
		"oversized wire":      make([]byte, MaxWireBytes+1),
		"legacy text":         []byte(header),
		"invalid header UTF8": binaryTestFrame(strings.Replace(header, `"source":"source"`, "\"source\":\"s\xff\"", 1), []byte{0xff}),
		"trailing JSON":       binaryTestFrame(header+"{}", []byte{0xff}),
		"truncated JSON":      binaryTestFrame(header[:len(header)-1], []byte{0xff}),
		"truncated payload":   valid[:len(valid)-1],
		"trailing payload":    append(bytes.Clone(valid), 0),
		"wrong direction":     binaryTestFrame(`{"version":1,"type":"READY","source":"s","epoch":"1","cut":"1"}`, nil),
		"body on COMMIT":      binaryTestFrame(`{"version":1,"type":"COMMIT","source":"s","epoch":"1","cut":"1"}`, []byte{0}),
		"oversized LIVE":      binaryTestFrame(strings.Replace(header, `"data":1`, fmt.Sprintf(`"data":%d`, terminal.MaxEgressBytes+1), 1), make([]byte, terminal.MaxEgressBytes+1)),
		"oversized body":      binaryTestFrame(header, make([]byte, terminal.MaxEgressBytes+1)),
	}
	commit := `{"version":1,"type":"COMMIT","source":"s","epoch":"1","cut":"1"}`
	cases["oversized complete header"] = binaryTestFrame(commit+strings.Repeat(" ", MaxHeaderBytes+1-len(commit)), nil)
	// The bound is inclusive: a complete header of exactly the bound decodes.
	if frame, err := Decode(binaryTestFrame(commit+strings.Repeat(" ", MaxHeaderBytes-len(commit)), nil), ServerToBrowser); err != nil || frame.Type != terminal.FrameCommit {
		t.Fatalf("a header of exactly the bound: %v", err)
	}
	for _, replacement := range []struct{ before, after string }{
		{`"version":1`, `"version":2`},
		{`"type":"LIVE"`, `"type":"UNKNOWN"`},
		{`"source":"source"`, `"source":null`},
		{`"epoch":"1"`, `"epoch":1`},
		{`"epoch":"1"`, `"epoch":"01"`},
		{`"epoch":"1"`, `"epoch":"0"`},
		{`"epoch":"1"`, `"epoch":"18446744073709551616"`},
		{`"cut":"1",`, ""},
		{`"cut":"1"`, `"cut":"1","cut":"2"`},
		{`"data":1`, `"data":"1"`},
		{`"data":1`, `"data":null`},
		{`"data":1`, `"data":-1`},
		{`"data":1`, `"data":1.5`},
		{`"data":1`, `"data":1.0`},
		{`"data":1`, `"data":1e0`},
		{`"data":1`, `"data":01`},
		{`"data":1`, `"data":1,"extra":true`},
	} {
		cases[replacement.after] = binaryTestFrame(strings.Replace(header, replacement.before, replacement.after, 1), []byte{0xff})
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(raw, ServerToBrowser); !errors.Is(err, terminal.ErrMalformed) {
				t.Fatalf("malformed binary accepted: %v", err)
			}
		})
	}
	if _, err := Decode(valid, BrowserToServer); !errors.Is(err, terminal.ErrMalformed) {
		t.Fatal("browser binary frame accepted")
	}
}

func TestBinaryPrepareFields(t *testing.T) {
	header := `{"version":1,"type":"PREPARE","source":"s","epoch":"1","cut":"1","kind":"HISTORY","columns":80,"rows":24,"history":[],"truncated":false,"replay":0,"request":"1","effective_history_rows":1000}`
	if _, err := Decode(binaryTestFrame(header, nil), ServerToBrowser); err != nil {
		t.Fatalf("empty replay rejected: %v", err)
	}
	for _, replacement := range []struct{ before, after string }{
		{`"columns":80`, `"columns":0`},
		{`"rows":24`, `"rows":1001`},
		{`"history":[]`, `"history":null`},
		{`"history":[]`, `"history":[null]`},
		{`"truncated":false`, `"truncated":null`},
		{`"replay":0`, `"replay":null`},
		{`"replay":0`, `"replay":1`},
		{`"request":"1"`, `"request":"0"`},
		{`"effective_history_rows":1000`, `"effective_history_rows":999`},
		{`"effective_history_rows":1000`, `"effectiveHistoryRows":1000`},
	} {
		t.Run(replacement.after, func(t *testing.T) {
			if _, err := Decode(binaryTestFrame(strings.Replace(header, replacement.before, replacement.after, 1), nil), ServerToBrowser); err == nil {
				t.Fatal("invalid PREPARE field accepted")
			}
		})
	}
}
