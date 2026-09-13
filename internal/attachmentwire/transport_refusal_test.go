package attachmentwire

import "testing"

func TestTransportRefusalRoundTripAndDirection(t *testing.T) {
	encoded, err := EncodeTransportRefusal("resize_failed", ServerToBrowser)
	if err != nil || string(encoded) != "PERSEA-REFUSAL/1 resize_failed" {
		t.Fatalf("encode=%q err=%v", encoded, err)
	}
	code, recognized, err := DecodeTransportRefusal(encoded, ServerToBrowser)
	if err != nil || !recognized || code != "resize_failed" {
		t.Fatalf("decode code=%q recognized=%t err=%v", code, recognized, err)
	}
	if _, err := EncodeTransportRefusal("resize_failed", BrowserToServer); err == nil {
		t.Fatal("a browser may not send a refusal")
	}
	if _, recognized, err := DecodeTransportRefusal(encoded, BrowserToServer); !recognized || err == nil {
		t.Fatalf("browser-direction decode recognized=%t err=%v", recognized, err)
	}
	for _, bad := range []string{"", "Resize_Failed", "a b", "<img>", "x_" + string(make([]byte, 70))} {
		if _, err := EncodeTransportRefusal(bad, ServerToBrowser); err == nil {
			t.Fatalf("encoded a non-canonical code %q", bad)
		}
		if _, recognized, err := DecodeTransportRefusal([]byte(TransportRefusalPrefix+bad), ServerToBrowser); !recognized || err == nil {
			t.Fatalf("decoded a non-canonical code %q: recognized=%t err=%v", bad, recognized, err)
		}
	}
	// Ordinary attachment frames and liveness frames are not refusals.
	for _, other := range []string{`{"type":"LIVE"}`, "PERSEA-LIVENESS/1 PONG 0123", "PERSEA-REFUSAL/2 x"} {
		if _, recognized, err := DecodeTransportRefusal([]byte(other), ServerToBrowser); recognized || err != nil {
			t.Fatalf("%q recognized=%t err=%v", other, recognized, err)
		}
	}
}
