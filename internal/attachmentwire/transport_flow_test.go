package attachmentwire

import "testing"

func TestTransportFlowAckRoundTripAndDirection(t *testing.T) {
	encoded, err := EncodeTransportFlowAck(42, BrowserToServer)
	if err != nil || string(encoded) != "PERSEA-FLOW/1 ACK 42" {
		t.Fatalf("encode=%q err=%v", encoded, err)
	}
	count, recognized, err := DecodeTransportFlowAck(encoded, BrowserToServer)
	if err != nil || !recognized || count != 42 {
		t.Fatalf("decode count=%d recognized=%t err=%v", count, recognized, err)
	}
	if _, err := EncodeTransportFlowAck(42, ServerToBrowser); err == nil {
		t.Fatal("a server may not send a flow acknowledgement")
	}
	if _, err := EncodeTransportFlowAck(0, BrowserToServer); err == nil {
		t.Fatal("encoded an acknowledgement of nothing")
	}
	if _, recognized, err := DecodeTransportFlowAck(encoded, ServerToBrowser); !recognized || err == nil {
		t.Fatalf("server-direction decode recognized=%t err=%v", recognized, err)
	}
	if count, _, err := DecodeTransportFlowAck([]byte("PERSEA-FLOW/1 ACK 18446744073709551615"), BrowserToServer); err != nil || count != 1<<64-1 {
		t.Fatalf("largest count=%d err=%v", count, err)
	}
	for _, bad := range []string{"ACK ", "ACK 0", "ACK 007", "ACK -1", "ACK +1", "ACK 1 ", "ACK  1", "ACK 1.0", "ACK 0x10", "ACK 18446744073709551616", "ACK 123456789012345678901", "ACK １", "NAK 1", "ack 1", "ACK", ""} {
		if _, recognized, err := DecodeTransportFlowAck([]byte(TransportFlowPrefix+bad), BrowserToServer); !recognized || err == nil {
			t.Fatalf("decoded a non-canonical acknowledgement %q: recognized=%t err=%v", bad, recognized, err)
		}
	}
	// Ordinary attachment frames and other reserved frames are not flow frames.
	for _, other := range []string{`{"type":"READY"}`, "PERSEA-LIVENESS/1 PING 0123", "PERSEA-FLOW/2 ACK 1", "PERSEA-FLOW/1"} {
		if _, recognized, err := DecodeTransportFlowAck([]byte(other), BrowserToServer); recognized || err != nil {
			t.Fatalf("%q recognized=%t err=%v", other, recognized, err)
		}
	}
}
