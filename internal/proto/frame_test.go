package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestFrameRoundTripAtBounds(t *testing.T) {
	for _, tc := range []struct {
		typ FrameType
		n   int
	}{{FrameControl, MaxControl}, {FrameData, MaxData}, {FrameInput, MaxInput}, {FrameAttachment, MaxAttachment}} {
		payload := bytes.Repeat([]byte{'x'}, tc.n)
		var wire bytes.Buffer
		if err := WriteFrame(&wire, tc.typ, payload); err != nil {
			t.Fatalf("WriteFrame(%x): %v", tc.typ, err)
		}
		got, err := ReadFrame(&wire)
		if err != nil {
			t.Fatalf("ReadFrame(%x): %v", tc.typ, err)
		}
		if got.Type != tc.typ || !bytes.Equal(got.Payload, payload) {
			t.Fatalf("round trip mismatch for type %x", tc.typ)
		}
	}
}

func TestReadRejectsOversizeBeforePayloadAllocation(t *testing.T) {
	var header [5]byte
	header[0] = byte(FrameInput)
	binary.BigEndian.PutUint32(header[1:], MaxInput+1)
	_, err := ReadFrame(bytes.NewReader(header[:]))
	if !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("expected ErrInvalidFrame, got %v", err)
	}
}

func TestReadAdmissionPrecedesBodyReadAndAllocation(t *testing.T) {
	var header [5]byte
	header[0] = byte(FrameAttachment)
	binary.BigEndian.PutUint32(header[1:], MaxAttachment)
	refused := errors.New("owner full")
	reader := bytes.NewReader(header[:])
	frame, err := ReadFrameWithAdmission(reader, func(kind FrameType, bytes uint32) error {
		if kind != FrameAttachment || bytes != MaxAttachment {
			t.Fatal(kind, bytes)
		}
		return refused
	})
	if err != refused || frame.Payload != nil || reader.Len() != 0 {
		t.Fatal(frame, err, reader.Len())
	}
}

func TestFrameRejectsUnknownType(t *testing.T) {
	_, err := ReadFrame(bytes.NewReader([]byte{0xff, 0, 0, 0, 0}))
	if !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("expected ErrInvalidFrame, got %v", err)
	}
	if err := WriteFrame(&bytes.Buffer{}, 0xff, nil); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("expected WriteFrame ErrInvalidFrame, got %v", err)
	}
}

func TestClientControlRejectsUnknownVerb(t *testing.T) {
	if _, err := DecodeClientControl([]byte(`{"type":"resize","cols":80,"rows":24}`)); err == nil {
		t.Fatal("unknown resize control verb was accepted")
	}
}

func TestClientControlRejectsFieldsFromAnotherVerb(t *testing.T) {
	if _, err := DecodeClientControl([]byte(`{"type":"inventory","mode":"control"}`)); err == nil {
		t.Fatal("inventory accepted an attach-only field")
	}
}

// Adoption carries identity only: server label and session ID. A name, a
// command, or any attach field on the verb is a protocol error, never a
// silently ignored extra.
func TestAdoptControlIsStrict(t *testing.T) {
	good, err := DecodeClientControl([]byte(`{"type":"adopt","server_label":"main","session_id":"$17"}`))
	if err != nil || good.ServerLabel != "main" || good.SessionID != "$17" {
		t.Fatalf("valid adopt rejected: %+v %v", good, err)
	}
	for _, bad := range []string{
		`{"type":"adopt","server_label":"main"}`,
		`{"type":"adopt","session_id":"$17"}`,
		`{"type":"adopt","server_label":"","session_id":"$17"}`,
		`{"type":"adopt","server_label":"main","session_id":""}`,
		`{"type":"adopt","server_label":"main","session_id":"$17","name":"work"}`,
		`{"type":"adopt","server_label":"main","session_id":"$17","mode":"control"}`,
	} {
		if _, err := DecodeClientControl([]byte(bad)); err == nil {
			t.Fatalf("invalid adopt accepted: %s", bad)
		}
	}
}

// Preview shares adopt's identity-only shape: depth and geometry stay
// broker-owned, so a request carrying them is a protocol error, not a hint.
func TestPreviewControlIsStrict(t *testing.T) {
	good, err := DecodeClientControl([]byte(`{"type":"preview","server_label":"main","session_id":"$17"}`))
	if err != nil || good.ServerLabel != "main" || good.SessionID != "$17" {
		t.Fatalf("valid preview rejected: %+v %v", good, err)
	}
	for _, bad := range []string{
		`{"type":"preview","server_label":"main"}`,
		`{"type":"preview","session_id":"$17"}`,
		`{"type":"preview","server_label":"","session_id":"$17"}`,
		`{"type":"preview","server_label":"main","session_id":""}`,
		`{"type":"preview","server_label":"main","session_id":"$17","depth":40}`,
		`{"type":"preview","server_label":"main","session_id":"$17","lines":["x"]}`,
		`{"type":"preview","server_label":"main","session_id":"$17","name":"work"}`,
	} {
		if _, err := DecodeClientControl([]byte(bad)); err == nil {
			t.Fatalf("invalid preview accepted: %s", bad)
		}
	}
}

func TestAuthorityControlsAreStrict(t *testing.T) {
	a := `{"realm":"r","server":"s","uid":1,"selector_kind":"socket_name","selector_value":"sock","boot_id":"b","server_pid":1,"server_start":2,"session_id":"$0","session_created":3}`
	for _, good := range []string{`{"type":"attach","mode":"observe","history_limit":0,"authority":` + a + `}`, `{"type":"attach","mode":"observe","history_limit":1000,"authority":` + a + `}`, `{"type":"attach","mode":"control","history_limit":10000,"authority":` + a + `}`, `{"type":"history","authority":` + a + `}`, `{"type":"snapshot","authority":` + a + `}`, `{"type":"snapshot","authority":` + a + `,"depth":5001}`, `{"type":"refit","authority":` + a + `,"cols":96,"id":"rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr"}`, `{"type":"ping"}`} {
		if _, err := DecodeClientControl([]byte(good)); err != nil {
			t.Fatalf("valid control rejected: %v", err)
		}
	}
	for _, bad := range []string{`{"type":"attach","mode":"write","history_limit":5000,"authority":` + a + `}`, `{"type":"attach","mode":"observe","authority":` + a + `}`, `{"type":"attach","mode":"observe","history_limit":1,"authority":` + a + `}`, `{"type":"history","authority":` + a + `,"extra":1}`, `{"type":"snapshot","authority":` + a + `,"depth":0}`, `{"type":"snapshot","authority":` + a + `,"depth":-1}`, `{"type":"snapshot","authority":` + a + `,"depth":"5"}`, `{"type":"refit","authority":` + a + `,"cols":96,"id":"rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr","successor":"sssssssssssssssssssssssssssssssssssssssssss"}`, `{"type":"ping","extra":1}`} {
		if _, err := DecodeClientControl([]byte(bad)); err == nil {
			t.Fatalf("invalid control accepted: %s", bad)
		}
	}
}

func TestAuthorityWireRoundTripBindsCompleteIdentity(t *testing.T) {
	want := Authority{Realm: "r", Server: "s", UID: 1000, SelectorKind: "socket_path", SelectorValue: "/run/tmux.sock", BootID: "boot", ServerPID: 7, ServerStart: 8, SessionID: "$9", SessionCreated: 10}
	b, err := MarshalControl(Control{Type: "history", Authority: &want})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeClientControl(b)
	if err != nil || got.Authority == nil || *got.Authority != want {
		t.Fatalf("authority round trip=%+v %v", got.Authority, err)
	}
	for _, missing := range []string{
		`{"type":"history","authority":{"realm":"r","server":"s","selector_kind":"socket_name","selector_value":"x","boot_id":"b","server_pid":1,"server_start":2,"session_id":"$0","session_created":3}}`,
		`{"type":"history","authority":{"realm":"r","server":"s","uid":1,"selector_value":"x","boot_id":"b","server_pid":1,"server_start":2,"session_id":"$0","session_created":3}}`,
	} {
		if _, err := DecodeClientControl([]byte(missing)); err == nil {
			t.Fatalf("incomplete authority accepted: %s", missing)
		}
	}
}

func TestAttachOKInputMaxUsesStrictControlDecoding(t *testing.T) {
	good, err := DecodeControl([]byte(`{"type":"attach_ok","cols":80,"rows":24,"input_max":8192}`))
	if err != nil || good.InputMax != MaxInput {
		t.Fatalf("attach_ok input_max rejected or changed: %+v %v", good, err)
	}
	for _, payload := range []string{
		`{"type":"attach_ok","cols":80,"rows":24,"input_max":"8192"}`,
		`{"type":"attach_ok","cols":80,"rows":24,"input_max":8192,"extra":true}`,
	} {
		if _, err := DecodeControl([]byte(payload)); err == nil {
			t.Fatalf("invalid attach_ok accepted: %s", payload)
		}
	}
}
