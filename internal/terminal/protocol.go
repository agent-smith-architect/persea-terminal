package terminal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const ProtocolVersion = 1

type FrameType string

const (
	FramePrepare     FrameType = "PREPARE"
	FrameReady       FrameType = "READY"
	FrameDefer       FrameType = "DEFER"
	FrameCommit      FrameType = "COMMIT"
	FrameLive        FrameType = "LIVE"
	FrameModeRequest FrameType = "MODE_REQUEST"
	FrameMode        FrameType = "MODE"
	FrameInput       FrameType = "INPUT"
	FrameResize      FrameType = "RESIZE_REQUEST"
	FrameHistory     FrameType = "HISTORY_REQUEST"
	FrameEnd         FrameType = "END"
)

type Mode string

const (
	ModeObserve Mode = "OBSERVE"
	ModeControl Mode = "CONTROL"
)

type CutKind string

const (
	CutInitial   CutKind = "INITIAL"
	CutReconnect CutKind = "RECONNECT"
	CutOngoing   CutKind = "ONGOING"
	CutResize    CutKind = "RESIZE"
	CutHistory   CutKind = "HISTORY"
)

type Frame struct {
	Version              int       `json:"version"`
	Type                 FrameType `json:"type"`
	Source               string    `json:"source"`
	Epoch                uint64    `json:"epoch"`
	Cut                  uint64    `json:"cut,omitempty"`
	Kind                 CutKind   `json:"kind,omitempty"`
	Mode                 Mode      `json:"mode,omitempty"`
	Data                 []byte    `json:"data,omitempty"`
	Replay               []byte    `json:"replay,omitempty"`
	History              []string  `json:"history,omitempty"`
	Columns              int       `json:"columns,omitempty"`
	Rows                 int       `json:"rows,omitempty"`
	Request              uint64    `json:"request,omitempty"`
	HistoryRows          int       `json:"history_rows,omitempty"`
	EffectiveHistoryRows int       `json:"effective_history_rows,omitempty"`
	Truncated            bool      `json:"truncated,omitempty"`
	Reason               string    `json:"reason,omitempty"`
}

// Explicit vertical Fit policy. The generic 1..1000 frame ceiling is a protocol
// bound, not product policy: an explicit row-fit request is narrower, and the
// broker rejects rather than clamps anything outside it.
const (
	MinFitRows  = 8
	MaxFitRows  = 120
	MaxFitCells = 36000
)

// ValidVerticalFit reports whether an explicit row-fit request is inside closed
// product policy. Columns are not the browser's to choose: the caller must
// separately require them to equal the exact pane witness, which is what keeps
// the browser non-authoritative for width.
func ValidVerticalFit(columns, rows int) bool {
	return columns >= 1 && columns <= 1000 &&
		rows >= MinFitRows && rows <= MaxFitRows &&
		columns*rows <= MaxFitCells
}

func DecodeFrame(raw []byte) (Frame, error) {
	if !utf8.Valid(raw) {
		return Frame{}, fmt.Errorf("%w: invalid UTF-8", ErrMalformed)
	}
	var f Frame
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&f); err != nil {
		return Frame{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return Frame{}, fmt.Errorf("%w: trailing JSON", ErrMalformed)
	}
	if err := f.Validate(); err != nil {
		return Frame{}, err
	}
	return f, nil
}

func (f Frame) Validate() error {
	if f.Version != ProtocolVersion || f.Source == "" || len(f.Source) > 256 || f.Epoch == 0 || len(f.Reason) > 64 {
		return ErrMalformed
	}
	requireCut := f.Type == FramePrepare || f.Type == FrameReady || f.Type == FrameDefer || f.Type == FrameCommit || f.Type == FrameLive
	if requireCut != (f.Cut != 0) {
		return ErrMalformed
	}
	switch f.Type {
	case FramePrepare:
		if (f.Kind != CutInitial && f.Kind != CutReconnect && f.Kind != CutOngoing && f.Kind != CutResize && f.Kind != CutHistory) || f.Columns <= 0 || f.Columns > 1000 || f.Rows <= 0 || f.Rows > 1000 {
			return ErrMalformed
		}
		if f.Kind == CutHistory {
			if f.Request == 0 || !ValidHistoryRows(f.EffectiveHistoryRows) {
				return ErrMalformed
			}
		} else if f.Request != 0 || f.EffectiveHistoryRows != 0 {
			return ErrMalformed
		}
		if len(f.History) > HistoryRowCap {
			return ErrSaturated
		}
		historyBytes := 0
		for _, row := range f.History {
			if len(row) > HistoryRowByteCap || strings.IndexByte(row, 0) >= 0 || !utf8.ValidString(row) {
				return ErrMalformed
			}
			historyBytes += len(row) + 1
		}
		if historyBytes > HistoryByteCap {
			return ErrSaturated
		}
		if len(f.Replay) > ReplayByteCap {
			return ErrSaturated
		}
	case FrameModeRequest, FrameMode:
		if f.Mode != ModeObserve && f.Mode != ModeControl {
			return ErrMalformed
		}
	case FrameInput:
		if len(f.Data) == 0 || len(f.Data) > MaxInputBytes || f.Mode != "" || f.Kind != "" || f.Cut != 0 {
			return ErrMalformed
		}
	case FrameResize:
		if f.Columns <= 0 || f.Columns > 1000 || f.Rows <= 0 || f.Rows > 1000 || f.Mode != "" || f.Kind != "" || f.Cut != 0 {
			return ErrMalformed
		}
	case FrameHistory:
		if f.Request == 0 || !ValidHistoryRows(f.HistoryRows) || f.Mode != "" || f.Kind != "" || f.Cut != 0 {
			return ErrMalformed
		}
	case FrameLive:
		if len(f.Data) == 0 || len(f.Data) > MaxEgressBytes {
			return ErrMalformed
		}
	case FrameReady, FrameCommit:
	case FrameDefer, FrameEnd:
		if f.Reason == "" {
			return ErrMalformed
		}
	default:
		return ErrMalformed
	}
	if f.Type != FramePrepare && f.Kind != "" {
		return ErrMalformed
	}
	if f.Type != FrameMode && f.Type != FrameModeRequest && f.Mode != "" {
		return ErrMalformed
	}
	if f.Type != FrameInput && f.Type != FrameLive && len(f.Data) != 0 {
		return ErrMalformed
	}
	if f.Type != FramePrepare && len(f.Replay) != 0 {
		return ErrMalformed
	}
	if f.Type != FramePrepare && f.Type != FrameResize && (f.Columns != 0 || f.Rows != 0) {
		return ErrMalformed
	}
	if f.Type != FrameHistory && f.HistoryRows != 0 {
		return ErrMalformed
	}
	if f.Type != FramePrepare && f.EffectiveHistoryRows != 0 {
		return ErrMalformed
	}
	if f.Type != FramePrepare && f.Type != FrameHistory && f.Request != 0 {
		return ErrMalformed
	}
	if f.Type != FramePrepare && (len(f.History) != 0 || f.Truncated) {
		return ErrMalformed
	}
	if f.Type != FrameDefer && f.Type != FrameEnd && f.Type != FrameMode && f.Reason != "" {
		return ErrMalformed
	}
	return nil
}

type direction uint8

const (
	fromServer direction = iota + 1
	fromBrowser
)

type protocolState uint8

const (
	stateAwaitPrepare protocolState = iota
	statePrepared
	stateReady
	stateLive
	stateEnded
)

// automaton is the attachment's sole protocol reducer. It is deliberately
// private so adapters cannot inject protocol transitions beside Epoch.
type automaton struct {
	source  string
	epoch   uint64
	state   protocolState
	cut     uint64
	kind    CutKind
	mode    Mode
	prepare []byte
}

func newAutomaton(source string, epoch uint64) (*automaton, error) {
	if source == "" || epoch == 0 {
		return nil, ErrMalformed
	}
	return &automaton{source: source, epoch: epoch, mode: ModeObserve}, nil
}

func (a *automaton) apply(direction direction, f Frame) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if f.Source != a.source || f.Epoch != a.epoch {
		return ErrStale
	}
	if a.state == stateEnded {
		return ErrOutOfState
	}
	if f.Type == FrameEnd {
		if direction != fromServer {
			return ErrOutOfState
		}
		a.state = stateEnded
		return nil
	}
	switch f.Type {
	case FramePrepare:
		encoded, err := json.Marshal(f)
		if err != nil {
			return err
		}
		if direction == fromServer && a.state == statePrepared && f.Cut == a.cut {
			if bytes.Equal(encoded, a.prepare) {
				return nil
			}
			return ErrOutOfState
		}
		if direction != fromServer || (a.state != stateAwaitPrepare && a.state != stateLive) || f.Cut <= a.cut {
			return ErrOutOfState
		}
		if a.state == stateAwaitPrepare && (f.Kind == CutOngoing || f.Kind == CutResize || f.Kind == CutHistory) {
			return ErrOutOfState
		}
		if a.state == stateLive && f.Kind != CutOngoing && f.Kind != CutResize && f.Kind != CutHistory {
			return ErrOutOfState
		}
		a.state, a.cut, a.kind, a.prepare = statePrepared, f.Cut, f.Kind, encoded
	case FrameDefer:
		if direction != fromBrowser || a.state != statePrepared || a.cut != f.Cut {
			return ErrOutOfState
		}
		if a.kind == CutInitial || a.kind == CutReconnect {
			a.state = stateAwaitPrepare
		} else {
			a.state = stateLive
		}
		a.prepare = nil
	case FrameReady:
		if direction != fromBrowser || a.state != statePrepared || a.cut != f.Cut {
			return ErrOutOfState
		}
		a.state = stateReady
	case FrameCommit:
		if direction != fromServer || a.state != stateReady || a.cut != f.Cut {
			return ErrOutOfState
		}
		a.state = stateLive
		a.prepare = nil
	case FrameLive:
		if direction != fromServer || a.state != stateLive || a.cut != f.Cut {
			return ErrOutOfState
		}
	case FrameModeRequest:
		if direction != fromBrowser || a.state != stateLive {
			return ErrOutOfState
		}
	case FrameMode:
		if direction != fromServer || a.state != stateLive {
			return ErrOutOfState
		}
		a.mode = f.Mode
	case FrameInput:
		if direction != fromBrowser || a.state != stateLive || a.mode != ModeControl {
			return ErrObserveOnly
		}
	case FrameResize:
		if direction != fromBrowser || a.state != stateLive || a.mode != ModeControl {
			return ErrObserveOnly
		}
	case FrameHistory:
		if direction != fromBrowser || (a.state != stateLive && a.state != statePrepared && a.state != stateReady) {
			return ErrOutOfState
		}
	default:
		return ErrOutOfState
	}
	return nil
}

func (a *automaton) fault() {
	a.mode = ModeObserve
	a.state = stateEnded
}
