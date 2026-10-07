package attachmentwire

import "testing"

func TestTransportInputRoundTripAndDirection(t *testing.T) {
	for _, tc := range []struct {
		through uint64
		code    string
		wire    string
	}{
		{9, "", "PERSEA-INPUT/1 9"},
		{12, "input_paused", "PERSEA-INPUT/1 12 input_paused"},
		{1, "input_partial", "PERSEA-INPUT/1 1 input_partial"},
	} {
		encoded, err := EncodeTransportInput(tc.through, tc.code, ServerToBrowser)
		if err != nil || string(encoded) != tc.wire {
			t.Fatalf("encode=%q err=%v want %q", encoded, err, tc.wire)
		}
		through, code, recognized, err := DecodeTransportInput(encoded, ServerToBrowser)
		if err != nil || !recognized || through != tc.through || code != tc.code {
			t.Fatalf("decode %q = %d %q %t %v", encoded, through, code, recognized, err)
		}
		if _, _, recognized, err := DecodeTransportInput(encoded, BrowserToServer); !recognized || err == nil {
			t.Fatalf("browser-direction decode of %q recognized=%t err=%v", encoded, recognized, err)
		}
	}
	if _, err := EncodeTransportInput(3, "", BrowserToServer); err == nil {
		t.Fatal("a browser may not send an input result")
	}
	for _, bad := range []struct {
		through uint64
		code    string
	}{{0, ""}, {3, "written"}, {3, "observe_mode"}, {3, "Input_paused"}} {
		if _, err := EncodeTransportInput(bad.through, bad.code, ServerToBrowser); err == nil {
			t.Fatalf("encoded %d %q", bad.through, bad.code)
		}
	}
	for _, bad := range []string{"", "0", "07", "1 ", "1  input_paused", "1 written", "x", "-1", "18446744073709551616", "1 input_paused extra"} {
		if _, _, recognized, err := DecodeTransportInput([]byte(TransportInputPrefix+bad), ServerToBrowser); !recognized || err == nil {
			t.Fatalf("decoded %q: recognized=%t err=%v", bad, recognized, err)
		}
	}
	for _, other := range []string{`{"type":"LIVE"}`, "PERSEA-REFUSAL/1 resize_failed", "PERSEA-INPUT/2 1"} {
		if _, _, recognized, err := DecodeTransportInput([]byte(other), ServerToBrowser); recognized || err != nil {
			t.Fatalf("%q recognized=%t err=%v", other, recognized, err)
		}
	}
}
