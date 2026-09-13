package terminal

import "errors"

var (
	ErrClosed         = errors.New("terminal attachment closed")
	ErrSaturated      = errors.New("terminal attachment bounded queue exhausted")
	ErrStale          = errors.New("stale terminal source, epoch, or cut")
	ErrOutOfState     = errors.New("terminal frame is not legal in the current state")
	ErrMalformed      = errors.New("malformed terminal frame")
	ErrObserveOnly    = errors.New("terminal attachment is observe-only")
	ErrReopenRequired = errors.New("pinned terminal source changed; reopen required")
	ErrCanceled       = errors.New("terminal attachment canceled")
	ErrNoProgress     = errors.New("terminal writer made no progress")
	ErrInvariant      = errors.New("terminal lifecycle invariant violated")
	// ErrResizeFailed reports that one explicit resize request was refused
	// before its geometry command was issued. It is an operational outcome of
	// that request, not a verdict on the attachment: the epoch stays live, its
	// cut sequence continues, and the caller may try again. It always wraps the
	// source's typed *ResizeRefusal, which is the only evidence the epoch
	// accepts that tmux was not mutated; any other resize error faults the
	// epoch instead.
	ErrResizeFailed = errors.New("terminal resize did not apply")
)
