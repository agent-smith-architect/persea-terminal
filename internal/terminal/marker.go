package terminal

import (
	"bytes"
	"errors"
)

const (
	markerNamespace    = "persea:v1:"
	markerNonceSize    = 43 // base64.RawStdEncoding of one SHA-256 digest
	markerPayloadLimit = len(markerNamespace) + markerNonceSize
	markerWirePrefix   = "\x1b]52;;" + markerNamespace
)

type ingressEventKind uint8

const (
	ingressOrdinary ingressEventKind = iota + 1
	ingressPrivateMarker
)

type ingressEvent struct {
	kind    ingressEventKind
	payload []byte
}

// ingressFramer is the epoch's sole PTY ingress parser. It retains only a
// bounded prefix that can still become the reserved, versioned Persea marker
// syntax and never emits bytes belonging to a complete private marker.
type ingressFramer struct {
	pending []byte
}

func (f *ingressFramer) feed(chunk []byte) ([]ingressEvent, error) {
	f.pending = append(f.pending, chunk...)
	var events []ingressEvent
	prefix := []byte(markerWirePrefix)
	for len(f.pending) > 0 {
		start := bytes.Index(f.pending, prefix)
		if start < 0 {
			keep := longestMarkerPrefixSuffix(f.pending, prefix)
			if ordinary := len(f.pending) - keep; ordinary > 0 {
				events = appendOrdinary(events, f.pending[:ordinary])
				f.pending = append(f.pending[:0], f.pending[ordinary:]...)
			}
			return events, nil
		}
		if start > 0 {
			events = appendOrdinary(events, f.pending[:start])
			f.pending = append(f.pending[:0], f.pending[start:]...)
		}

		bodyStart := len(prefix)
		available := len(f.pending) - bodyStart
		check := available
		if check > markerNonceSize {
			check = markerNonceSize
		}
		for _, b := range f.pending[bodyStart : bodyStart+check] {
			if !markerNonceByte(b) {
				return events, errors.New("malformed reserved Persea marker nonce")
			}
		}
		if available < markerNonceSize {
			return events, nil
		}

		end := bodyStart + markerNonceSize
		if len(f.pending) == end {
			return events, nil
		}
		switch f.pending[end] {
		case '\a':
			end++
		case '\x1b':
			if len(f.pending) == end+1 {
				return events, nil
			}
			if f.pending[end+1] != '\\' {
				return events, errors.New("malformed reserved Persea marker terminator")
			}
			end += 2
		default:
			return events, errors.New("over-limit or unterminated reserved Persea marker")
		}
		payload := append([]byte(nil), f.pending[len("\x1b]52;;"):end]...)
		if payload[len(payload)-1] == '\a' {
			payload = payload[:len(payload)-1]
		} else {
			payload = payload[:len(payload)-2]
		}
		events = append(events, ingressEvent{kind: ingressPrivateMarker, payload: payload})
		f.pending = append(f.pending[:0], f.pending[end:]...)
	}
	return events, nil
}

// finish consumes the one unresolved suffix at graceful ingress EOF. A proper
// prefix of the reserved namespace is then proven ordinary; once the complete
// namespace has arrived, any incomplete nonce or terminator is private,
// unterminated syntax and must fail closed.
func (f *ingressFramer) finish() ([]ingressEvent, error) {
	pending := f.pending
	f.pending = nil
	if len(pending) == 0 {
		return nil, nil
	}
	if len(pending) < len(markerWirePrefix) && bytes.Equal(pending, []byte(markerWirePrefix)[:len(pending)]) {
		return appendOrdinary(nil, pending), nil
	}
	return nil, errors.New("unterminated reserved Persea marker at ingress EOF")
}

func (f *ingressFramer) discard() {
	clear(f.pending)
	f.pending = nil
}

func appendOrdinary(events []ingressEvent, data []byte) []ingressEvent {
	if len(data) == 0 {
		return events
	}
	copyOfData := append([]byte(nil), data...)
	if len(events) > 0 && events[len(events)-1].kind == ingressOrdinary {
		events[len(events)-1].payload = append(events[len(events)-1].payload, copyOfData...)
		return events
	}
	return append(events, ingressEvent{kind: ingressOrdinary, payload: copyOfData})
}

func longestMarkerPrefixSuffix(data, prefix []byte) int {
	limit := len(data)
	if limit >= len(prefix) {
		limit = len(prefix) - 1
	}
	for n := limit; n > 0; n-- {
		if bytes.Equal(data[len(data)-n:], prefix[:n]) {
			return n
		}
	}
	return 0
}

func markerNonceByte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '+' || b == '/'
}
