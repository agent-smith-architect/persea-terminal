package terminal

import (
	"context"
	"errors"
)

type lifecycleResultKind uint8

const (
	lifecycleResultInvalid lifecycleResultKind = iota
	lifecycleNotAdmitted
	lifecyclePrepared
	lifecycleTerminal
	lifecycleAlreadyTerminal
)

type admissionReason uint8

const (
	admissionInvalid admissionReason = iota
	admissionMalformedRequest
	admissionCallerPreCanceled
	admissionConflictingAttempt
)

type terminalCauseClass uint8

const (
	terminalCauseInvalid terminalCauseClass = iota
	terminalCauseOrderly
	terminalCauseCanceled
	terminalCauseFault
	terminalCauseInvariant
)

type ownershipKind uint8

const (
	ownershipInvalid ownershipKind = iota
	ownershipPending
	ownershipNone
	ownershipExact
)

type startPhase uint8

const (
	startPhaseInvalid startPhase = iota
	startPhaseBinding
	startPhaseCutting
	startPhaseAwaitingPrepare
	startPhasePrepared
	startPhaseTerminal
)

type prepareOutcomeKind uint8

const (
	prepareOutcomeInvalid prepareOutcomeKind = iota
	prepareWaiting
	preparePublished
	prepareFailed
	prepareAlreadyTerminal
)

type terminalRequestKind uint8

const (
	terminalRequestInvalid terminalRequestKind = iota
	terminalRequestDeferred
	terminalRequestResolved
)

type terminalCause struct {
	class      terminalCauseClass
	diagnostic error
}

func orderlyTerminalCause() terminalCause {
	return terminalCause{class: terminalCauseOrderly}
}

func canceledTerminalCause(err error) terminalCause {
	if err == nil {
		err = context.Canceled
	}
	return terminalCause{class: terminalCauseCanceled, diagnostic: err}
}

func faultTerminalCause(err error) terminalCause {
	if err == nil {
		err = ErrReopenRequired
	}
	return terminalCause{class: terminalCauseFault, diagnostic: err}
}

func invariantTerminalCause(err error) terminalCause {
	if err == nil {
		err = ErrInvariant
	} else {
		err = errors.Join(ErrInvariant, err)
	}
	return terminalCause{class: terminalCauseInvariant, diagnostic: err}
}

func (c terminalCause) valid() bool {
	switch c.class {
	case terminalCauseOrderly:
		return c.diagnostic == nil
	case terminalCauseCanceled, terminalCauseFault, terminalCauseInvariant:
		return c.diagnostic != nil
	default:
		return false
	}
}

func (c terminalCause) endReason() string {
	switch c.class {
	case terminalCauseOrderly:
		return "closed"
	case terminalCauseCanceled:
		return "canceled"
	case terminalCauseFault, terminalCauseInvariant:
		return "fault"
	default:
		return "fault"
	}
}

type attachmentOwnership struct {
	kind ownershipKind
	ids  AttachmentIDs
}

func pendingOwnership() attachmentOwnership {
	return attachmentOwnership{kind: ownershipPending}
}

func noOwnership() attachmentOwnership {
	return attachmentOwnership{kind: ownershipNone}
}

func exactOwnership(ids AttachmentIDs) attachmentOwnership {
	return attachmentOwnership{kind: ownershipExact, ids: ids}
}

func (o attachmentOwnership) valid() bool {
	switch o.kind {
	case ownershipPending, ownershipNone:
		return o.ids == (AttachmentIDs{})
	case ownershipExact:
		return o.ids.exact()
	default:
		return false
	}
}

type terminalResult struct {
	cause     terminalCause
	publicErr error
	endReason string
	ownership attachmentOwnership
	endErr    error
}

func (r *terminalResult) valid() bool {
	return r != nil && r.cause.valid() && r.publicErr != nil &&
		r.endReason == r.cause.endReason() && r.ownership.valid() &&
		r.ownership.kind != ownershipPending
}

type lifecycleResult struct {
	kind     lifecycleResultKind
	reason   admissionReason
	err      error
	terminal *terminalResult
}

func notAdmittedResult(reason admissionReason, err error) lifecycleResult {
	return lifecycleResult{kind: lifecycleNotAdmitted, reason: reason, err: err}
}

func preparedResult() lifecycleResult {
	return lifecycleResult{kind: lifecyclePrepared}
}

func terminalLifecycleResult(result *terminalResult) lifecycleResult {
	return lifecycleResult{kind: lifecycleTerminal, terminal: result}
}

func alreadyTerminalResult(result *terminalResult) lifecycleResult {
	return lifecycleResult{kind: lifecycleAlreadyTerminal, terminal: result}
}

func (r lifecycleResult) valid() bool {
	switch r.kind {
	case lifecycleNotAdmitted:
		return r.reason >= admissionMalformedRequest && r.reason <= admissionConflictingAttempt &&
			r.err != nil && r.terminal == nil
	case lifecyclePrepared:
		return r.reason == admissionInvalid && r.err == nil && r.terminal == nil
	case lifecycleTerminal, lifecycleAlreadyTerminal:
		return r.reason == admissionInvalid && r.err == nil && r.terminal.valid()
	default:
		return false
	}
}

func (r lifecycleResult) publicError() error {
	if !r.valid() {
		return ErrInvariant
	}
	switch r.kind {
	case lifecycleNotAdmitted:
		return r.err
	case lifecyclePrepared:
		return nil
	case lifecycleTerminal, lifecycleAlreadyTerminal:
		return r.terminal.publicErr
	default:
		return ErrInvariant
	}
}

type startAttempt struct {
	kind          CutKind
	phase         startPhase
	done          chan struct{}
	result        lifecycleResult
	replay        []byte
	ownership     attachmentOwnership
	pendingCause  *terminalCause
	operationStop context.CancelFunc
}

func (a *startAttempt) validPending() bool {
	if a == nil || (a.kind != CutInitial && a.kind != CutReconnect) || a.done == nil ||
		a.operationStop == nil || !a.ownership.valid() {
		return false
	}
	switch a.phase {
	case startPhaseBinding:
		return a.result.kind == lifecycleResultInvalid
	case startPhaseCutting, startPhaseAwaitingPrepare:
		return a.ownership.kind == ownershipExact && a.pendingCause == nil &&
			a.result.kind == lifecycleResultInvalid
	case startPhasePrepared:
		return a.ownership.kind == ownershipExact && a.pendingCause == nil &&
			a.result.kind == lifecyclePrepared && a.result.valid()
	case startPhaseTerminal:
		return a.ownership.kind != ownershipPending && a.result.kind == lifecycleTerminal && a.result.valid()
	default:
		return false
	}
}

type cleanupState struct {
	teardownErr error
	err         error
	reported    bool
	complete    bool
}

type prepareOutcome struct {
	kind   prepareOutcomeKind
	cause  terminalCause
	result lifecycleResult
}

func (o prepareOutcome) valid() bool {
	switch o.kind {
	case prepareWaiting, preparePublished:
		return o.cause.class == terminalCauseInvalid && o.result.kind == lifecycleResultInvalid
	case prepareFailed:
		return o.cause.valid() && o.result.kind == lifecycleResultInvalid
	case prepareAlreadyTerminal:
		return o.cause.class == terminalCauseInvalid && o.result.kind == lifecycleAlreadyTerminal && o.result.valid()
	default:
		return false
	}
}

type terminalRequest struct {
	kind   terminalRequestKind
	wait   *startAttempt
	result lifecycleResult
}

func (r terminalRequest) valid() bool {
	switch r.kind {
	case terminalRequestDeferred:
		return r.wait != nil && r.result.kind == lifecycleResultInvalid
	case terminalRequestResolved:
		return r.wait == nil && (r.result.kind == lifecycleTerminal || r.result.kind == lifecycleAlreadyTerminal) && r.result.valid()
	default:
		return false
	}
}
