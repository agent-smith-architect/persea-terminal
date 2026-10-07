package attachmentwire

import (
	"bytes"
	"fmt"
	"strconv"

	"persea-terminal/internal/proto"
)

// Input results say what became of the INPUT frames a page sent.
//
// Both ends number the INPUT frames of one WebSocket 1, 2, … in send order.
// "PERSEA-INPUT/1 9" means every frame after the previous result, through the
// ninth, was written to the terminal in full; "PERSEA-INPUT/1 9 input_paused"
// means they were all refused with that code. Results arrive in frame order
// and always advance. A frame the page has no result for when the WebSocket
// ends is uncertain: it may or may not have reached the terminal, and nothing
// resends it. Server to browser only; a transport frame like liveness and
// refusals, not an attachment frame, so not counted for flow.
const TransportInputPrefix = "PERSEA-INPUT/1 "

// EncodeTransportInput encodes one run of input results.
func EncodeTransportInput(through uint64, code string, direction Direction) ([]byte, error) {
	if direction != ServerToBrowser || through == 0 || !proto.ValidInputResultCode(code) {
		return nil, fmt.Errorf("transport input protocol violation")
	}
	frame := TransportInputPrefix + strconv.FormatUint(through, 10)
	if code != "" {
		frame += " " + code
	}
	return []byte(frame), nil
}

// DecodeTransportInput recognizes the reserved input namespace. Once the
// prefix is present, the count must be a canonical positive decimal and the
// code one of the result codes.
func DecodeTransportInput(raw []byte, direction Direction) (uint64, string, bool, error) {
	if !bytes.HasPrefix(raw, []byte(TransportInputPrefix)) {
		return 0, "", false, nil
	}
	violation := func() (uint64, string, bool, error) {
		return 0, "", true, fmt.Errorf("transport input protocol violation")
	}
	digits, code, coded := bytes.Cut(raw[len(TransportInputPrefix):], []byte(" "))
	if direction != ServerToBrowser || len(digits) == 0 || len(digits) > transportFlowDigits || digits[0] == '0' ||
		(coded && len(code) == 0) || !proto.ValidInputResultCode(string(code)) {
		return violation()
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return violation()
		}
	}
	through, err := strconv.ParseUint(string(digits), 10, 64)
	if err != nil {
		return violation()
	}
	return through, string(code), true, nil
}
