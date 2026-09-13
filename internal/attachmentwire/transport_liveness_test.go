package attachmentwire

import (
	"bytes"
	"strings"
	"testing"
)

const testLivenessNonce = "0123456789abcdef0123456789abcdef"

func TestTransportLivenessExactAcceptanceAndRoundTrip(t *testing.T) {
	ping := []byte(TransportLivenessPrefix + "PING " + testLivenessNonce)
	if len(ping) != TransportLivenessFrameBytes {
		t.Fatalf("ping length=%d", len(ping))
	}
	decoded, recognized, err := DecodeTransportLiveness(ping, BrowserToServer)
	if err != nil || !recognized || decoded.Kind != TransportLivenessPing || decoded.Nonce != testLivenessNonce {
		t.Fatalf("decode ping=(%+v,%v,%v)", decoded, recognized, err)
	}
	pong, err := EncodeTransportLiveness(TransportLivenessFrame{Kind: TransportLivenessPong, Nonce: decoded.Nonce}, ServerToBrowser)
	if err != nil || string(pong) != TransportLivenessPrefix+"PONG "+testLivenessNonce || len(pong) != TransportLivenessFrameBytes {
		t.Fatalf("encode pong=(%q,%v)", pong, err)
	}
	decoded, recognized, err = DecodeTransportLiveness(pong, ServerToBrowser)
	if err != nil || !recognized || decoded.Kind != TransportLivenessPong || decoded.Nonce != testLivenessNonce {
		t.Fatalf("decode pong=(%+v,%v,%v)", decoded, recognized, err)
	}
}

func TestTransportLivenessReservedPrefixFailsClosedAndRedacted(t *testing.T) {
	ordinary := []byte(`{"type":"READY"}`)
	if _, recognized, err := DecodeTransportLiveness(ordinary, BrowserToServer); recognized || err != nil {
		t.Fatalf("ordinary frame recognized=%v err=%v", recognized, err)
	}
	cases := [][]byte{
		[]byte(TransportLivenessPrefix + "PONG " + testLivenessNonce),
		[]byte(TransportLivenessPrefix + "PING " + strings.ToUpper(testLivenessNonce)),
		[]byte(TransportLivenessPrefix + "PING " + testLivenessNonce + "x"),
		[]byte(TransportLivenessPrefix + "PING " + testLivenessNonce[:31]),
		[]byte(TransportLivenessPrefix + "PING!" + testLivenessNonce),
		[]byte(TransportLivenessPrefix + "NOPE " + testLivenessNonce),
	}
	for _, value := range cases {
		_, recognized, err := DecodeTransportLiveness(value, BrowserToServer)
		if !recognized || err == nil {
			t.Fatalf("reserved value accepted: recognized=%v err=%v", recognized, err)
		}
		if bytes.Contains([]byte(err.Error()), []byte(testLivenessNonce)) {
			t.Fatalf("protocol error reflected nonce")
		}
	}
	if _, err := EncodeTransportLiveness(TransportLivenessFrame{Kind: TransportLivenessPing, Nonce: testLivenessNonce}, ServerToBrowser); err == nil {
		t.Fatal("wrong-direction ping encoded")
	}
}
