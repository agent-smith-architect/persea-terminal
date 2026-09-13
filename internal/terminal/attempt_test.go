package terminal

import (
	"errors"
	"testing"
)

func TestAttemptResultInvalid(t *testing.T) {
	ids := AttachmentIDs{ShadowSessionID: "$shadow", ClientID: "$client"}
	cause := faultTerminalCause(errors.New("fault"))
	terminal := &terminalResult{
		cause: cause, publicErr: ErrClosed, endReason: "fault", ownership: exactOwnership(ids),
	}
	if !terminal.valid() {
		t.Fatal("valid terminal result rejected")
	}
	validResults := []lifecycleResult{
		notAdmittedResult(admissionMalformedRequest, ErrMalformed),
		preparedResult(),
		terminalLifecycleResult(terminal),
		alreadyTerminalResult(terminal),
	}
	for _, result := range validResults {
		if !result.valid() {
			t.Fatalf("valid lifecycle result rejected: %#v", result)
		}
	}
	invalidResults := []lifecycleResult{
		{},
		{kind: lifecycleResultKind(255)},
		{kind: lifecycleNotAdmitted, reason: admissionInvalid, err: ErrMalformed},
		{kind: lifecycleNotAdmitted, reason: admissionMalformedRequest},
		{kind: lifecyclePrepared, err: ErrClosed},
		{kind: lifecycleTerminal},
		{kind: lifecycleAlreadyTerminal, terminal: &terminalResult{}},
	}
	for _, result := range invalidResults {
		if result.valid() {
			t.Fatalf("invalid lifecycle result accepted: %#v", result)
		}
		if result.publicError() != ErrInvariant {
			t.Fatalf("invalid lifecycle result mapped to %v", result.publicError())
		}
	}

	for _, invalid := range []terminalCause{
		{},
		{class: terminalCauseClass(255)},
		{class: terminalCauseOrderly, diagnostic: errors.New("unexpected")},
		{class: terminalCauseFault},
		{class: terminalCauseInvariant},
	} {
		if invalid.valid() {
			t.Fatalf("invalid terminal cause accepted: %#v", invalid)
		}
	}
	for _, valid := range []terminalCause{
		orderlyTerminalCause(), canceledTerminalCause(nil), faultTerminalCause(nil), invariantTerminalCause(nil),
	} {
		if !valid.valid() {
			t.Fatalf("valid terminal cause rejected: %#v", valid)
		}
	}

	for _, invalid := range []attachmentOwnership{
		{},
		{kind: ownershipKind(255)},
		{kind: ownershipPending, ids: ids},
		{kind: ownershipNone, ids: ids},
		{kind: ownershipExact},
		{kind: ownershipExact, ids: AttachmentIDs{ShadowSessionID: "$partial"}},
	} {
		if invalid.valid() {
			t.Fatalf("invalid ownership accepted: %#v", invalid)
		}
	}
	for _, valid := range []attachmentOwnership{pendingOwnership(), noOwnership(), exactOwnership(ids)} {
		if !valid.valid() {
			t.Fatalf("valid ownership rejected: %#v", valid)
		}
	}

	for _, phase := range []startPhase{startPhaseInvalid, startPhase(255)} {
		attempt := &startAttempt{
			kind: CutInitial, phase: phase, done: make(chan struct{}),
			ownership: pendingOwnership(), operationStop: func() {},
		}
		if attempt.validPending() {
			t.Fatalf("invalid start phase accepted: %d", phase)
		}
	}
	validAttempt := &startAttempt{
		kind: CutInitial, phase: startPhaseBinding, done: make(chan struct{}),
		ownership: pendingOwnership(), operationStop: func() {},
	}
	if !validAttempt.validPending() {
		t.Fatal("valid binding attempt rejected")
	}

	for _, invalid := range []bindOutcome{{}, {kind: bindOutcomeKind(255)}, {kind: bindSucceededExact}, {kind: bindFailedNoOwner}} {
		if invalid.valid() {
			t.Fatalf("invalid BIND outcome accepted: %#v", invalid)
		}
	}
	for _, valid := range []bindOutcome{
		{kind: bindSucceededExact, ids: ids},
		{kind: bindFailedNoOwner, cause: ErrClosed},
		{kind: bindFailedExact, ids: ids, cause: ErrClosed},
		{kind: bindInvalid, cause: ErrInvariant},
	} {
		if !valid.valid() {
			t.Fatalf("valid BIND outcome rejected: %#v", valid)
		}
	}
	for _, invalid := range []cutOutcome{{}, {kind: cutOutcomeKind(255)}, {kind: cutSucceeded, cause: ErrClosed}, {kind: cutFailed}} {
		if invalid.valid() {
			t.Fatalf("invalid CUT outcome accepted: %#v", invalid)
		}
	}
	if !(cutOutcome{kind: cutSucceeded}).valid() || !(cutOutcome{kind: cutFailed, cause: ErrClosed}).valid() {
		t.Fatal("valid CUT outcome rejected")
	}
	if (replayAdoption{}).valid() || (prepareOutcome{}).valid() || (terminalRequest{}).valid() {
		t.Fatal("zero internal outcome accepted")
	}
}
