package terminal

import (
	"bytes"
	"fmt"
	"unicode/utf8"
)

const (
	DefaultHistoryRows = 1_000
	HistoryRowCap      = 10_000
	HistoryByteCap     = 2 * 1024 * 1024
	HistoryRowByteCap  = 8 * 1024
	HeldByteCap        = 256 * 1024
	ReplayByteCap      = 256 * 1024
)

// ValidHistoryRows is the one exact operator choice set shared by every Go
// boundary. History depth is policy, never authority.
func ValidHistoryRows(rows int) bool {
	switch rows {
	case 0, 500, 1_000, 2_000, 5_000, 7_500, 10_000:
		return true
	default:
		return false
	}
}

// BoundedOffscreenRows retains the newest complete physical-row suffix. The
// caller supplies only off-screen rows and may include one extra oldest row as
// truncation evidence.
func boundedOffscreenRows(captured []byte, lineCap int) (rows []string, truncated bool, err error) {
	if !ValidHistoryRows(lineCap) {
		return nil, false, fmt.Errorf("invalid history row limit %d", lineCap)
	}
	if bytes.IndexByte(captured, 0) >= 0 {
		return nil, false, fmt.Errorf("capture-pane returned a NUL row")
	}
	if !utf8.Valid(captured) {
		return nil, false, fmt.Errorf("capture-pane returned invalid UTF-8")
	}
	physical := bytes.Split(captured, []byte{'\n'})
	if len(physical) > 0 && len(physical[len(physical)-1]) == 0 {
		physical = physical[:len(physical)-1]
	}
	for _, row := range physical {
		if len(row) > HistoryRowByteCap {
			return nil, false, fmt.Errorf("captured physical row exceeds %d-byte cap", HistoryRowByteCap)
		}
	}
	if len(physical) > lineCap {
		truncated = true
		physical = physical[len(physical)-lineCap:]
	}
	used, start := 0, len(physical)
	for start > 0 {
		row := physical[start-1]
		if used+len(row)+1 > HistoryByteCap {
			truncated = true
			break
		}
		used += len(row) + 1
		start--
	}
	for _, row := range physical[start:] {
		rows = append(rows, string(row))
	}
	return rows, truncated, nil
}

// cut owns one marker split and the bounded bytes held behind its COMMIT fence.
type cut struct {
	ID            uint64
	Kind          CutKind
	markerPayload []byte
	markerSeen    bool
	pre           []byte
	held          []byte
}

// cutRoute is the complete classification result for one PTY chunk.  Replay
// is private INITIAL/RECONNECT redraw, live is ONGOING pre-marker data that
// must immediately retain FIFO position, and activity is genuine post-marker
// output that must survive completion as scheduler dirtiness.
type cutRoute struct {
	live     []byte
	activity bool
}

type replayAdoptionKind uint8

const (
	replayAdoptionInvalid replayAdoptionKind = iota
	replayAdopted
	replayAdoptionFailed
)

type replayAdoption struct {
	kind  replayAdoptionKind
	cause error
}

func (o replayAdoption) valid() bool {
	switch o.kind {
	case replayAdopted:
		return o.cause == nil
	case replayAdoptionFailed:
		return o.cause != nil
	default:
		return false
	}
}

func newCut(id uint64, kind CutKind, markerPayload []byte) (*cut, error) {
	if id == 0 || (kind != CutInitial && kind != CutReconnect && kind != CutOngoing && kind != CutResize && kind != CutHistory) || len(markerPayload) == 0 {
		return nil, ErrMalformed
	}
	return &cut{ID: id, Kind: kind, markerPayload: append([]byte(nil), markerPayload...)}, nil
}

func (c *cut) adoptPreMarker(replay *[]byte) replayAdoption {
	if replay == nil || c.Kind == CutOngoing || c.Kind == CutHistory || c.markerSeen || len(c.pre) != 0 || len(*replay) > ReplayByteCap {
		return replayAdoption{kind: replayAdoptionFailed, cause: ErrInvariant}
	}
	c.pre = *replay
	*replay = nil
	return replayAdoption{kind: replayAdopted}
}

func (c *cut) feedOrdinary(chunk []byte) (cutRoute, error) {
	var route cutRoute
	if !c.markerSeen {
		if c.Kind == CutOngoing || c.Kind == CutHistory {
			route.live = append([]byte(nil), chunk...)
		} else {
			if len(chunk) > ReplayByteCap-len(c.pre) {
				return cutRoute{}, ErrSaturated
			}
			c.pre = append(c.pre, chunk...)
		}
		return route, nil
	}
	if len(chunk) > HeldByteCap-len(c.held) {
		return cutRoute{}, ErrSaturated
	}
	c.held = append(c.held, chunk...)
	route.activity = len(chunk) > 0
	return route, nil
}

func (c *cut) acceptMarker(payload []byte) error {
	if c.markerSeen {
		return fmt.Errorf("duplicate reserved Persea marker")
	}
	if !bytes.Equal(payload, c.markerPayload) {
		return fmt.Errorf("stale or wrong-nonce reserved Persea marker")
	}
	c.markerSeen = true
	return nil
}

func (c *cut) markerFound() bool { return c.markerSeen }
func (c *cut) preMarker() []byte { return append([]byte(nil), c.pre...) }
func (c *cut) heldBytes() []byte { return append([]byte(nil), c.held...) }

func (c *cut) discardHeld() {
	clear(c.held)
	c.held = nil
}

func (c *cut) discardAll() {
	clear(c.pre)
	c.pre = nil
	c.discardHeld()
}
