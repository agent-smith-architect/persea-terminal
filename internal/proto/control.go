package proto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

const (
	ControlImageStage   = "image_stage"
	ControlImageStaged  = "image_staged"
	ControlImageRefused = "image_refused"

	ImageRefusalImagesDisabled  = "images_disabled"
	ImageRefusalTooLarge        = "too_large"
	ImageRefusalUnsupportedType = "unsupported_type"
	ImageRefusalCapacity        = "capacity"
	ImageRefusalIO              = "io"
)

const UnifiedSessionDetailRotationDeferredAltScreen = "rotation_deferred_alt_screen"

type Authority struct {
	Realm          string `json:"realm"`
	Server         string `json:"server"`
	UID            uint32 `json:"uid"`
	SelectorKind   string `json:"selector_kind"`
	SelectorValue  string `json:"selector_value"`
	BootID         string `json:"boot_id"`
	ServerPID      int    `json:"server_pid"`
	ServerStart    uint64 `json:"server_start"`
	SessionID      string `json:"session_id"`
	SessionCreated int64  `json:"session_created"`
}

func (a *Authority) UnmarshalJSON(data []byte) error {
	type wire struct {
		Realm          *string `json:"realm"`
		Server         *string `json:"server"`
		UID            *uint32 `json:"uid"`
		SelectorKind   *string `json:"selector_kind"`
		SelectorValue  *string `json:"selector_value"`
		BootID         *string `json:"boot_id"`
		ServerPID      *int    `json:"server_pid"`
		ServerStart    *uint64 `json:"server_start"`
		SessionID      *string `json:"session_id"`
		SessionCreated *int64  `json:"session_created"`
	}
	var v wire
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return err
	}
	if v.Realm == nil || v.Server == nil || v.UID == nil || v.SelectorKind == nil || v.SelectorValue == nil || v.BootID == nil || v.ServerPID == nil || v.ServerStart == nil || v.SessionID == nil || v.SessionCreated == nil {
		return fmt.Errorf("incomplete authority")
	}
	*a = Authority{Realm: *v.Realm, Server: *v.Server, UID: *v.UID, SelectorKind: *v.SelectorKind, SelectorValue: *v.SelectorValue, BootID: *v.BootID, ServerPID: *v.ServerPID, ServerStart: *v.ServerStart, SessionID: *v.SessionID, SessionCreated: *v.SessionCreated}
	if !a.Valid() {
		return fmt.Errorf("invalid authority")
	}
	return nil
}

func (a Authority) Valid() bool {
	selectorValid := a.SelectorKind == "socket_name" && a.SelectorValue != "" && !strings.Contains(a.SelectorValue, "/")
	selectorValid = selectorValid || a.SelectorKind == "socket_path" && filepath.IsAbs(a.SelectorValue) && filepath.Clean(a.SelectorValue) == a.SelectorValue
	return a.Realm != "" && a.Server != "" && a.BootID != "" && a.ServerPID > 0 && a.ServerStart > 0 && a.SessionID != "" && a.SessionCreated > 0 && selectorValid
}

type Session struct {
	Authority Authority `json:"authority"`
	Name      string    `json:"name"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	Attached  int       `json:"attached"`
	Activity  int64     `json:"activity"`
	// Last output in the active window. Session Activity measures client interaction.
	OutputActivity int64                `json:"output_activity,omitempty"`
	Unified        *UnifiedSessionState `json:"unified,omitempty"`
}

const (
	UnifiedDevLaunchCreate = "create"
)

// Per-session unified projection states. The set is closed: the dashboard's
// strict parser hides the affordance for anything it does not recognise, so a
// new state is a coordinated broker/frontdoor/UI change by design.
const (
	UnifiedSessionOpen                 = "open"
	UnifiedSessionAdoptable            = "adoptable"
	UnifiedSessionBlockedAltScreen     = "blocked_alt_screen"
	UnifiedSessionBlockedMultiPane     = "blocked_multi_pane"
	UnifiedSessionBlockedMultiWindow   = "blocked_multi_window"
	UnifiedSessionBlockedForeignServer = "blocked_foreign_server"
	UnifiedSessionSlotsExhausted       = "slots_exhausted"
	UnifiedSessionUnavailable          = "unavailable"
)

// Origin values for an open unified session, mirroring the journal's
// generation origin without importing it: birth generations replay byte truth
// from byte zero, reconstructed ones begin with a capture-equivalent bootstrap.
const (
	UnifiedOriginBirth         = "birth"
	UnifiedOriginReconstructed = "reconstructed"
)

// UnifiedSessionState is the non-secret per-session unified projection. Origin
// is present exactly when State is open. Detail is an optional closed active
// condition; blocked states continue to carry their reason in State itself.
type UnifiedSessionState struct {
	State  string `json:"state"`
	Origin string `json:"origin,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// UnifiedDevLaunch is a closed, non-secret projection of the one configured
// development target. SessionID is an identity witness, never an attachment
// capability; the front door binds it to a freshly minted handle.
type UnifiedDevLaunch struct {
	State     string `json:"state"`
	Name      string `json:"name"`
	SessionID string `json:"session_id,omitempty"`
}

type ServerInventory struct {
	Label  string `json:"label"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// CanCreate reports the realm's own creation policy for this server. The
	// broker owns that policy, so capability discovery flows outward from it
	// rather than being guessed by the front door or the browser. It is advice
	// for the UI only: the broker re-decides on every create request.
	CanCreate bool `json:"can_create,omitempty"`
	// CanStageImages reports whether this realm's broker stages operator
	// images. Same doctrine as CanCreate: the broker owns the policy, the
	// field is UI advice only, and the broker re-decides on every
	// image_stage exchange (a nil stager refuses images_disabled).
	CanStageImages bool              `json:"can_stage_images,omitempty"`
	Sessions       []Session         `json:"sessions,omitempty"`
	UnifiedDev     *UnifiedDevLaunch `json:"unified_dev,omitempty"`
}

type Control struct {
	Type         string            `json:"type"`
	MediaType    string            `json:"media_type,omitempty"`
	Bytes        int               `json:"bytes,omitempty"`
	Path         string            `json:"path,omitempty"`
	ID           string            `json:"id,omitempty"`
	ExpiresAt    string            `json:"expires_at,omitempty"`
	V            int               `json:"v,omitempty"`
	Mode         string            `json:"mode,omitempty"`
	Engine       string            `json:"engine,omitempty"`
	Authority    *Authority        `json:"authority,omitempty"`
	Servers      []ServerInventory `json:"servers,omitempty"`
	Cols         int               `json:"cols,omitempty"`
	Rows         int               `json:"rows,omitempty"`
	InputMax     int               `json:"input_max,omitempty"`
	HistoryLimit *int              `json:"history_limit,omitempty"`
	Depth        int               `json:"depth,omitempty"`
	HistoryRows  *int              `json:"history_rows,omitempty"`
	Width        int               `json:"width,omitempty"`
	Height       int               `json:"height,omitempty"`
	Pane         string            `json:"pane,omitempty"`
	FrozenAt     int64             `json:"frozen_at,omitempty"`
	Truncated    bool              `json:"truncated,omitempty"`
	Alternate    bool              `json:"alternate_on,omitempty"`
	Code         string            `json:"code,omitempty"`
	Msg          string            `json:"msg,omitempty"`
	Reason       string            `json:"reason,omitempty"`
	RefitStage   RefitFailureStage `json:"refit_stage,omitempty"`
	RefitClass   RefitFailureClass `json:"refit_class,omitempty"`
	// Session creation. ServerLabel and Name are the only client-supplied inputs;
	// the working directory and geometry come from broker configuration so that no
	// browser can choose what a new shell runs or where.
	ServerLabel string `json:"server_label,omitempty"`
	Name        string `json:"name,omitempty"`
	// Session adoption targets an EXISTING session by identity and optionally
	// bounds its read-only capture with HistoryRows. It never carries a name,
	// command, directory, or geometry.
	SessionID string `json:"session_id,omitempty"`
	// Successor is the broker-minted public incarnation of a completed width
	// refit. It binds the out-of-band result to the one attachment PREPARE that
	// may settle that operation; it is never accepted from a browser request.
	Successor string `json:"successor,omitempty"`
	// Lines carries the dashboard preview rows in preview_ok: plain text with
	// control bytes stripped broker-side, split on newlines. The row and byte
	// bounds keep the whole response inside one control frame.
	Lines []string `json:"lines,omitempty"`
	// ANSILines is an optional colored representation of those same preview
	// rows. Only SGR styling survives the broker's sanitizer.
	ANSILines []string `json:"ansi_lines,omitempty"`
}

// PreviewRowLimit bounds the dashboard preview to roughly one screenful of
// recent rows. It is shared wire shape, not presentation: the broker captures
// and returns at most this many rows, and the front door refuses to relay a
// response that claims more.
const PreviewRowLimit = 40

// AdoptionHistoryMaxRows bounds the shared read-only tmux history import.
// The browser retains its own selected number of rows from that recording.
const AdoptionHistoryMaxRows = 10000

func validHistoryLimit(rows int) bool {
	// This independent server-side allow-list is a trust boundary. Browser
	// choices are presentation metadata and cannot widen broker policy.
	switch rows {
	case 0, 500, 1_000, 2_000, 5_000, 7_500, 10_000:
		return true
	default:
		return false
	}
}

func MarshalControl(c Control) ([]byte, error) { return json.Marshal(c) }

func DecodeControl(payload []byte) (Control, error) {
	var c Control
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Control{}, fmt.Errorf("invalid control message: %w", err)
	}
	if c.Type == "" {
		return Control{}, fmt.Errorf("control message has no type")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return Control{}, fmt.Errorf("trailing control JSON")
	}
	return c, nil
}

func DecodeClientControl(payload []byte) (Control, error) {
	c, err := DecodeControl(payload)
	if err != nil {
		return Control{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return Control{}, err
	}
	allowed := map[string]bool{"type": true}
	switch c.Type {
	case "hello":
		allowed["v"] = true
		if c.V != 1 {
			return Control{}, fmt.Errorf("invalid hello")
		}
	case "inventory", "detach", "ping":
	case "attach":
		allowed["authority"], allowed["mode"], allowed["history_limit"], allowed["engine"] = true, true, true, true
		if c.Authority == nil || !c.Authority.Valid() || (c.Mode != "observe" && c.Mode != "control") || c.HistoryLimit == nil || !validHistoryLimit(*c.HistoryLimit) || (c.Engine != "" && c.Engine != "unified-dev") {
			return Control{}, fmt.Errorf("invalid attach mode")
		}
	case "create":
		allowed["server_label"], allowed["name"] = true, true
		// Grammar is enforced by broker configuration; this only rejects shapes that
		// cannot be a request at all, so a bad name yields a clean refusal rather
		// than a protocol error.
		if c.ServerLabel == "" || c.Name == "" {
			return Control{}, fmt.Errorf("invalid create")
		}
	case "adopt":
		allowed["server_label"], allowed["session_id"] = true, true
		allowed["history_rows"] = true
		// Whether the session exists, is eligible, or has slots is the broker's
		// judgement and arrives as a typed refusal.
		if c.ServerLabel == "" || c.SessionID == "" || c.HistoryRows != nil && (*c.HistoryRows < 0 || *c.HistoryRows > AdoptionHistoryMaxRows) {
			return Control{}, fmt.Errorf("invalid adopt")
		}
	case "refit":
		allowed["authority"], allowed["cols"], allowed["rows"], allowed["id"] = true, true, true, true
		// Rows are optional. This decoder enforces only the bounded wire shape;
		// the broker independently applies the stricter terminal geometry law.
		if c.Authority == nil || !c.Authority.Valid() || c.Cols < 20 || c.Cols > 300 || c.Rows < 0 || c.Rows > 1000 || len(c.ID) != 43 {
			return Control{}, fmt.Errorf("invalid refit")
		}
		for _, r := range c.ID {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
				return Control{}, fmt.Errorf("invalid refit")
			}
		}
	case "preview":
		allowed["server_label"], allowed["session_id"] = true, true
		// Identity shape only, exactly like adopt: depth and geometry are
		// broker-owned, so a client cannot widen the read-only capture.
		if c.ServerLabel == "" || c.SessionID == "" {
			return Control{}, fmt.Errorf("invalid preview")
		}
	case "history":
		allowed["authority"] = true
		if c.Authority == nil || !c.Authority.Valid() {
			return Control{}, fmt.Errorf("invalid history")
		}
	case "snapshot":
		allowed["authority"], allowed["depth"] = true, true
		if c.Authority == nil || !c.Authority.Valid() {
			return Control{}, fmt.Errorf("invalid snapshot")
		}
		if _, present := fields["depth"]; present && c.Depth <= 0 {
			return Control{}, fmt.Errorf("invalid snapshot depth")
		}
	default:
		return Control{}, fmt.Errorf("unknown control type %q", c.Type)
	}
	for field := range fields {
		if !allowed[field] {
			return Control{}, fmt.Errorf("field %q is not valid for %s", field, c.Type)
		}
	}
	return c, nil
}
