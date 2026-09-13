package attachmentwire

import (
	"bytes"
	"fmt"
)

const TransportLivenessPrefix = "PERSEA-LIVENESS/1 "
const TransportLivenessFrameBytes = 55

type TransportLivenessKind uint8

const (
	TransportLivenessPing TransportLivenessKind = iota + 1
	TransportLivenessPong
)

type TransportLivenessFrame struct {
	Kind  TransportLivenessKind
	Nonce string
}

func validTransportLivenessNonce(nonce string) bool {
	if len(nonce) != 32 {
		return false
	}
	for index := 0; index < len(nonce); index++ {
		if (nonce[index] < '0' || nonce[index] > '9') && (nonce[index] < 'a' || nonce[index] > 'f') {
			return false
		}
	}
	return true
}

func transportLivenessVerb(kind TransportLivenessKind, direction Direction) (string, bool) {
	switch {
	case kind == TransportLivenessPing && direction == BrowserToServer:
		return "PING", true
	case kind == TransportLivenessPong && direction == ServerToBrowser:
		return "PONG", true
	default:
		return "", false
	}
}

// DecodeTransportLiveness recognizes the reserved application-heartbeat
// namespace without interpreting ordinary attachment frames. Once the prefix
// is present, every syntax or direction error is reported without reflecting
// any payload material into the error.
func DecodeTransportLiveness(raw []byte, direction Direction) (TransportLivenessFrame, bool, error) {
	if !bytes.HasPrefix(raw, []byte(TransportLivenessPrefix)) {
		return TransportLivenessFrame{}, false, nil
	}
	malformed := func() (TransportLivenessFrame, bool, error) {
		return TransportLivenessFrame{}, true, fmt.Errorf("transport liveness protocol violation")
	}
	if len(raw) != TransportLivenessFrameBytes {
		return malformed()
	}
	verb := string(raw[len(TransportLivenessPrefix) : len(TransportLivenessPrefix)+4])
	if raw[len(TransportLivenessPrefix)+4] != ' ' {
		return malformed()
	}
	nonce := string(raw[len(TransportLivenessPrefix)+5:])
	if !validTransportLivenessNonce(nonce) {
		return malformed()
	}
	frame := TransportLivenessFrame{Nonce: nonce}
	switch verb {
	case "PING":
		frame.Kind = TransportLivenessPing
	case "PONG":
		frame.Kind = TransportLivenessPong
	default:
		return malformed()
	}
	if _, allowed := transportLivenessVerb(frame.Kind, direction); !allowed {
		return malformed()
	}
	return frame, true, nil
}

func EncodeTransportLiveness(frame TransportLivenessFrame, direction Direction) ([]byte, error) {
	verb, allowed := transportLivenessVerb(frame.Kind, direction)
	if !allowed || !validTransportLivenessNonce(frame.Nonce) {
		return nil, fmt.Errorf("transport liveness protocol violation")
	}
	return []byte(TransportLivenessPrefix + verb + " " + frame.Nonce), nil
}
