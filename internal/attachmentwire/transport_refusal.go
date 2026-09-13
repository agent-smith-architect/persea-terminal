package attachmentwire

import (
	"bytes"
	"fmt"
	"regexp"
)

// Operational refusals travel in-band.
//
// A broker `error` control whose code is operational — one request's outcome,
// not a verdict on the attachment — must reach the browser WITHOUT closing the
// WebSocket. It is carried as a reserved text frame beside ordinary attachment
// frames and transport liveness, in the same reserved-prefix style, so the
// page can recognize it before attempting an attachment decode. Server to
// browser only; a browser may not send one.
const TransportRefusalPrefix = "PERSEA-REFUSAL/1 "

var transportRefusalCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// EncodeTransportRefusal encodes one operational refusal code.
func EncodeTransportRefusal(code string, direction Direction) ([]byte, error) {
	if direction != ServerToBrowser || !transportRefusalCode.MatchString(code) {
		return nil, fmt.Errorf("transport refusal protocol violation")
	}
	return []byte(TransportRefusalPrefix + code), nil
}

// DecodeTransportRefusal recognizes the reserved refusal namespace without
// interpreting ordinary attachment frames. Once the prefix is present, any
// syntax or direction error is reported without reflecting payload material.
func DecodeTransportRefusal(raw []byte, direction Direction) (string, bool, error) {
	if !bytes.HasPrefix(raw, []byte(TransportRefusalPrefix)) {
		return "", false, nil
	}
	code := string(raw[len(TransportRefusalPrefix):])
	if direction != ServerToBrowser || !transportRefusalCode.MatchString(code) {
		return "", true, fmt.Errorf("transport refusal protocol violation")
	}
	return code, true, nil
}
