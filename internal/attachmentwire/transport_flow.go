package attachmentwire

import (
	"bytes"
	"fmt"
	"strconv"
)

// Flow acknowledgements pace server-to-browser output.
//
// Front-door WebSocket writes complete as soon as the bytes reach kernel and
// proxy buffers, so on a slow link everything written ahead of a liveness
// PONG — or of a WebSocket ping — waits in those buffers before it. The
// browser therefore acknowledges the attachment frames it has consumed, and
// the front door keeps only a bounded window of unacknowledged attachment
// bytes in flight. The count is cumulative over one WebSocket: "ACK 7" means
// the first seven attachment frames have been written into the terminal.
// Liveness and refusal frames are transport frames, not attachment frames,
// and are not counted. Browser to server only.
const TransportFlowPrefix = "PERSEA-FLOW/1 "

const transportFlowAck = "ACK "

// transportFlowDigits bounds the count to what a uint64 holds.
const transportFlowDigits = 20

// EncodeTransportFlowAck encodes one cumulative acknowledgement.
func EncodeTransportFlowAck(count uint64, direction Direction) ([]byte, error) {
	if direction != BrowserToServer || count == 0 {
		return nil, fmt.Errorf("transport flow protocol violation")
	}
	return []byte(TransportFlowPrefix + transportFlowAck + strconv.FormatUint(count, 10)), nil
}

// DecodeTransportFlowAck recognizes the reserved flow namespace without
// interpreting ordinary attachment frames. Once the prefix is present, the
// count must be a canonical positive decimal; any other syntax, or the wrong
// direction, is a protocol violation reported without payload material.
func DecodeTransportFlowAck(raw []byte, direction Direction) (uint64, bool, error) {
	if !bytes.HasPrefix(raw, []byte(TransportFlowPrefix)) {
		return 0, false, nil
	}
	violation := func() (uint64, bool, error) {
		return 0, true, fmt.Errorf("transport flow protocol violation")
	}
	body := raw[len(TransportFlowPrefix):]
	if !bytes.HasPrefix(body, []byte(transportFlowAck)) {
		return violation()
	}
	digits := body[len(transportFlowAck):]
	if direction != BrowserToServer || len(digits) == 0 || len(digits) > transportFlowDigits || digits[0] == '0' {
		return violation()
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return violation()
		}
	}
	count, err := strconv.ParseUint(string(digits), 10, 64)
	if err != nil {
		return violation()
	}
	return count, true, nil
}
