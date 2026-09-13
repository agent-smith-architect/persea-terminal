package terminal

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

type SocketIdentity struct {
	Path   string
	Device uint64
	Inode  uint64
}

type ProcessWitness struct {
	PID       int
	StartTime uint64
}

// SourceWitness contains immutable host facts. No name-addressable tmux target
// is present, so a same-name replacement cannot be resolved by this package.
type SourceWitness struct {
	Incarnation string
	Socket      SocketIdentity
	Server      ProcessWitness
	SessionID   string
	WindowID    string
	PaneID      string
	Pane        ProcessWitness
	Columns     int
	Rows        int
}

func (w SourceWitness) Validate() error {
	if w.Incarnation == "" || !filepath.IsAbs(w.Socket.Path) || w.Socket.Device == 0 || w.Socket.Inode == 0 ||
		w.Server.PID <= 0 || w.Server.StartTime == 0 || w.Pane.PID <= 0 || w.Pane.StartTime == 0 ||
		!strings.HasPrefix(w.SessionID, "$") || !strings.HasPrefix(w.WindowID, "@") || !strings.HasPrefix(w.PaneID, "%") ||
		w.Columns <= 0 || w.Rows <= 0 {
		return ErrMalformed
	}
	return nil
}

type PinnedAction string

const (
	ActionBind    PinnedAction = "BIND"
	ActionCut     PinnedAction = "CUT"
	ActionResize  PinnedAction = "RESIZE"
	ActionCleanup PinnedAction = "CLEANUP_ATTACHMENT"
)

type AttachmentIDs struct {
	ShadowSessionID string
	ClientID        string
}

func (ids AttachmentIDs) exact() bool {
	return strings.HasPrefix(ids.ShadowSessionID, "$") && ids.ClientID != ""
}

type TransactionRequest struct {
	Action      PinnedAction
	Witness     SourceWitness
	Attachment  AttachmentIDs
	Cut         uint64
	Kind        CutKind
	Marker      []byte
	HistoryRows int
	Columns     int
	Rows        int
}

// CutCapture is produced only by the exact pinned CUT transaction. Capture is
// the inert off-screen history snapshot; redraw/replay bytes arrive through
// Epoch.PTYBytes on the one PTY stream and are separated by Marker.
type CutCapture struct {
	Capture []byte
}

type TransactionResult struct {
	Witness    SourceWitness
	Attachment AttachmentIDs
	Capture    CutCapture
}

// PinnedTransaction is one non-yielding tmux client transaction. Its concrete
// implementation must check the incarnation guard and exact object IDs inside
// the same transaction that performs the requested action.
//
// A BIND result is atomic ownership evidence: valid exact attachment IDs mean
// the transaction mutated tmux and the caller owns those IDs, even when the
// same result also returns an error. A result without valid exact IDs means no
// attachment was created. An implementation must never mutate tmux and return
// an empty or partial identity that makes the attachment unknowable.
//
// A RESIZE result is mutation-certainty evidence. The implementation issues
// one guarded geometry command and then performs further steps (witness
// recheck, attachment PTY resize, durable commit) that can fail after tmux has
// already changed. Its error therefore carries a certainty class:
//
//   - An error wrapping *ResizeRefusal certifies that the transaction was
//     rejected BEFORE the geometry command was submitted and that the source
//     is intact: tmux, the attachment PTY, and the journal are exactly as they
//     were. Only capacity and request-policy rejections may be typed this way,
//     and only when the implementation has proven non-submission (an exhausted
//     journal budget checked before issue, an observer that never accepted the
//     command, a malformed request). The caller may treat it as one request's
//     outcome and carry on.
//   - Every other error means the command was submitted, or its transport
//     outcome is unknown, or the source itself is dead or changed. The caller
//     must assume tmux may hold the new geometry while the attachment and the
//     journal do not, and must retire the attachment so a reopen re-mints.
//
// A refusal is the ONLY operational outcome; the default is fatal. An
// implementation that cannot prove non-submission must not type a refusal.
type PinnedTransaction interface {
	RunPinned(context.Context, TransactionRequest) (TransactionResult, error)
}

// ResizeRefusal is the typed pre-issue rejection of a RESIZE transaction. It
// exists so that "the resize did not apply" and "the resize may have applied"
// can never be confused: only an implementation that has proven the guarded
// command was never submitted constructs one, and the epoch keeps itself live
// on nothing else.
type ResizeRefusal struct {
	Cause error
}

func (r *ResizeRefusal) Error() string {
	if r == nil || r.Cause == nil {
		return "resize refused before issue"
	}
	return "resize refused before issue: " + r.Cause.Error()
}

func (r *ResizeRefusal) Unwrap() error {
	if r == nil {
		return nil
	}
	return r.Cause
}

// RefuseResize types cause as a proven pre-issue rejection. See the RESIZE
// guarantee on PinnedTransaction: callers must only wrap errors raised before
// any geometry command reached tmux.
func RefuseResize(cause error) error {
	if cause == nil {
		cause = ErrMalformed
	}
	return &ResizeRefusal{Cause: cause}
}

// IsResizeRefusal reports whether err carries a typed pre-issue refusal.
func IsResizeRefusal(err error) bool {
	var refusal *ResizeRefusal
	return errors.As(err, &refusal)
}

type ProcessProbe interface {
	Witness(context.Context, int) (ProcessWitness, error)
}

// PinnedSource couples before/after process witnesses with exactly one
// exact-ID tmux transaction. It exposes attachment cleanup but no source or
// fixture destruction capability.
type PinnedSource struct {
	witness SourceWitness
	tmux    PinnedTransaction
	probe   ProcessProbe
}

type bindOutcomeKind uint8

const (
	bindOutcomeInvalid bindOutcomeKind = iota
	bindSucceededExact
	bindFailedNoOwner
	bindFailedExact
	bindInvalid
)

type bindOutcome struct {
	kind  bindOutcomeKind
	ids   AttachmentIDs
	cause error
}

func (o bindOutcome) valid() bool {
	switch o.kind {
	case bindSucceededExact:
		return o.ids.exact() && o.cause == nil
	case bindFailedNoOwner:
		return o.ids == (AttachmentIDs{}) && o.cause != nil
	case bindFailedExact:
		return o.ids.exact() && o.cause != nil
	case bindInvalid:
		return o.cause != nil
	default:
		return false
	}
}

type cutOutcomeKind uint8

const (
	cutOutcomeInvalid cutOutcomeKind = iota
	cutSucceeded
	cutFailed
)

type cutOutcome struct {
	kind    cutOutcomeKind
	capture CutCapture
	cause   error
}

func (o cutOutcome) valid() bool {
	switch o.kind {
	case cutSucceeded:
		return o.cause == nil
	case cutFailed:
		return o.cause != nil && len(o.capture.Capture) == 0
	default:
		return false
	}
}

func NewPinnedSource(w SourceWitness, tmux PinnedTransaction, probe ProcessProbe) (*PinnedSource, error) {
	if err := w.Validate(); err != nil || tmux == nil || probe == nil {
		return nil, ErrMalformed
	}
	return &PinnedSource{witness: w, tmux: tmux, probe: probe}, nil
}

func (s *PinnedSource) publicIdentity() (incarnation string, columns, rows int) {
	return s.witness.Incarnation, s.witness.Columns, s.witness.Rows
}

func (s *PinnedSource) bind(ctx context.Context, transfer func(AttachmentIDs) error) bindOutcome {
	if transfer == nil {
		return bindOutcome{kind: bindInvalid, cause: errors.Join(ErrInvariant, ErrMalformed)}
	}
	if err := s.validateProcesses(ctx); err != nil {
		return bindOutcome{kind: bindFailedNoOwner, cause: err}
	}
	r, runErr := s.tmux.RunPinned(ctx, TransactionRequest{Action: ActionBind, Witness: s.witness})
	ids := r.Attachment
	if ids.exact() {
		// This callback is deliberately the first operation after RunPinned. No
		// diagnostic classification may precede accounting for exact ownership.
		if err := transfer(ids); err != nil {
			return bindOutcome{kind: bindInvalid, ids: ids, cause: errors.Join(ErrInvariant, err)}
		}
	} else if ids == (AttachmentIDs{}) {
		cause := runErr
		if cause == nil {
			cause = fmt.Errorf("BIND returned no exact attachment IDs")
		}
		return bindOutcome{kind: bindFailedNoOwner, cause: errors.Join(ErrReopenRequired, cause)}
	} else {
		cause := fmt.Errorf("BIND returned partial attachment IDs")
		if runErr != nil {
			cause = errors.Join(cause, runErr)
		}
		return bindOutcome{kind: bindInvalid, cause: errors.Join(ErrInvariant, cause)}
	}
	if runErr != nil {
		return bindOutcome{kind: bindFailedExact, ids: ids, cause: errors.Join(ErrReopenRequired, runErr)}
	}
	if r.Witness != s.witness {
		return bindOutcome{kind: bindFailedExact, ids: ids, cause: ErrReopenRequired}
	}
	if err := s.validateProcesses(ctx); err != nil {
		return bindOutcome{kind: bindFailedExact, ids: ids, cause: err}
	}
	return bindOutcome{kind: bindSucceededExact, ids: ids}
}

func (s *PinnedSource) cut(ctx context.Context, ids AttachmentIDs, cut uint64, kind CutKind, marker []byte, historyRows int) cutOutcome {
	if cut == 0 || (kind != CutInitial && kind != CutReconnect && kind != CutOngoing && kind != CutResize && kind != CutHistory) || len(marker) == 0 || len(marker) > markerPayloadLimit || !ValidHistoryRows(historyRows) {
		return cutOutcome{kind: cutFailed, cause: ErrMalformed}
	}
	r, err := s.perform(ctx, TransactionRequest{Action: ActionCut, Attachment: ids, Cut: cut, Kind: kind, Marker: append([]byte(nil), marker...), HistoryRows: historyRows})
	if err != nil {
		return cutOutcome{kind: cutFailed, cause: err}
	}
	return cutOutcome{kind: cutSucceeded, capture: CutCapture{Capture: append([]byte(nil), r.Capture.Capture...)}}
}

func (s *PinnedSource) cleanupAttachment(ctx context.Context, ids AttachmentIDs) error {
	if !ids.exact() {
		return ErrMalformed
	}
	_, err := s.perform(ctx, TransactionRequest{Action: ActionCleanup, Attachment: ids})
	return err
}

// resize performs the exact-incarnation RESIZE transaction and classifies its
// outcome under the RESIZE guarantee. A *ResizeRefusal passes through
// untouched: the transaction proved nothing was issued and the source is
// intact. Everything else — a dead or changed process before issue, a
// transaction error after issue, a result that does not witness the requested
// geometry, a process that changed afterwards — is a reopen verdict, because
// tmux may already hold a geometry the attachment and journal do not.
func (s *PinnedSource) resize(ctx context.Context, ids AttachmentIDs, columns, rows int) error {
	if !ids.exact() || columns < 1 || columns > 1000 || rows < 1 || rows > 1000 {
		// A malformed request is rejected here, before any transaction exists.
		return RefuseResize(ErrMalformed)
	}
	if err := s.validateProcesses(ctx); err != nil {
		// Proven pre-issue, but not a rejection: the source is dead or replaced,
		// and no later cut could succeed on it. Stale is fatal, never operational.
		return err
	}
	before := s.witness
	result, err := s.tmux.RunPinned(ctx, TransactionRequest{
		Action: ActionResize, Witness: before, Attachment: ids, Columns: columns, Rows: rows,
	})
	if err != nil {
		if IsResizeRefusal(err) {
			return err
		}
		return errors.Join(ErrReopenRequired, err)
	}
	after := result.Witness
	if result.Attachment != ids || after.Validate() != nil || after.Columns != columns || after.Rows != rows ||
		!sameSourceApartFromGeometry(before, after) {
		return ErrReopenRequired
	}
	s.witness = after
	if err := s.validateProcesses(ctx); err != nil {
		return err
	}
	return nil
}

func (s *PinnedSource) perform(ctx context.Context, req TransactionRequest) (TransactionResult, error) {
	if req.Action != ActionCut && req.Action != ActionCleanup {
		return TransactionResult{}, ErrMalformed
	}
	validate := s.validateProcesses
	if req.Action == ActionCleanup {
		validate = s.validateServer
	}
	if err := validate(ctx); err != nil {
		return TransactionResult{}, err
	}
	req.Witness = s.witness
	r, err := s.tmux.RunPinned(ctx, req)
	if err != nil {
		return TransactionResult{}, errors.Join(ErrReopenRequired, err)
	}
	if req.Action == ActionCut && r.Witness != s.witness {
		return TransactionResult{}, ErrReopenRequired
	}
	if req.Action == ActionCleanup && !sameServerIncarnation(r.Witness, s.witness) {
		return TransactionResult{}, ErrReopenRequired
	}
	if r.Attachment != req.Attachment {
		return TransactionResult{}, ErrReopenRequired
	}
	if err := validate(ctx); err != nil {
		return TransactionResult{}, err
	}
	return r, nil
}

func sameSourceApartFromGeometry(before, after SourceWitness) bool {
	before.Incarnation, after.Incarnation = "", ""
	before.Columns, before.Rows, after.Columns, after.Rows = 0, 0, 0, 0
	return before == after
}

func sameServerIncarnation(got, want SourceWitness) bool {
	return got.Incarnation == want.Incarnation && got.Socket == want.Socket && got.Server == want.Server
}

func (s *PinnedSource) validateServer(ctx context.Context) error {
	server, err := s.probe.Witness(ctx, s.witness.Server.PID)
	if err != nil {
		return errors.Join(ErrReopenRequired, err)
	}
	if server != s.witness.Server {
		return ErrReopenRequired
	}
	return nil
}

func (s *PinnedSource) validateProcesses(ctx context.Context) error {
	if err := s.validateServer(ctx); err != nil {
		return err
	}
	pane, err := s.probe.Witness(ctx, s.witness.Pane.PID)
	if err != nil {
		return errors.Join(ErrReopenRequired, err)
	}
	if pane != s.witness.Pane {
		return ErrReopenRequired
	}
	return nil
}
