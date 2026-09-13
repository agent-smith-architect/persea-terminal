package proto

import "sort"

// Attachment error policy.
//
// Every `error` control a broker writes on a live attachment carries a code
// from a closed set, and every code belongs to exactly one of two classes.
// This is the single place that decides which — the broker consults it to know
// whether its attachment loop continues, the front door consults it to know
// whether to relay the code in-band or close the WebSocket, and a test holds
// every emitted code to membership so a new code cannot inherit a class by
// accident.
//
//   - OPERATIONAL: the outcome of ONE request. The session, its journal, and
//     the attachment are all still valid; the broker keeps the epoch alive and
//     reports the outcome, and the browser renders it as a passing notice. A
//     Fit that did not apply, a Fit outside policy, a keystroke that landed a
//     beat early, control traffic on an observe handle.
//   - FATAL: a verdict on the attachment itself. The broker ends the
//     attachment after writing it, and the front door closes the WebSocket
//     with the code as the reason.
//
// An unknown code fails closed: it is treated as fatal.
type AttachmentErrorClass uint8

const (
	AttachmentErrorUnknown AttachmentErrorClass = iota
	AttachmentErrorOperational
	AttachmentErrorFatal
)

var operationalAttachmentCodes = map[string]struct{}{
	"resize_failed":   {},
	"resize_rejected": {},
	"input_refused":   {},
	"observe_mode":    {},
}

var fatalAttachmentCodes = map[string]struct{}{
	"attach_failed":       {},
	"attachment_failed":   {},
	"bad_attachment":      {},
	"bad_control":         {},
	"bad_frame":           {},
	"bad_history":         {},
	"bad_mode":            {},
	"history_failed":      {},
	"protocol":            {},
	"snapshot_failed":     {},
	"stale_target":        {},
	"unified_unavailable": {},
	// Subscriber close reasons (below) are FATAL to the attachment they end
	// and RECONNECTABLE to the browser; every member of that set is also a
	// member of this one, and a test holds the two in agreement.
	string(SubscriberClosedGenerationRotated): {},
	string(SubscriberClosedGenerationRefit):   {},
	string(SubscriberClosedLagged):            {},
	string(SubscriberClosedGenerationFailed):  {},
	string(SubscriberClosedRefitFaulted):      {},
}

// SubscriberCloseReason is the typed outcome of removing a unified journal
// subscriber. A unified attachment's live tail IS one subscriber, so every
// removal the broker performs — never a bare channel close — ends the
// attachment with the reason as its error code: the broker writes it as a
// FATAL error control, the front door closes the WebSocket with it, and the
// browser classifies it (ui/src/unified_close_policy.ts) explicitly. Rotation
// and lag closes are reconnectable because the session remains intact; a
// committed-generation failure is terminal because automatic reattachment
// must not target a journal generation that was faulted and removed. The set
// is closed and lives here so every removal inherits the typed delivery path.
type SubscriberCloseReason string

const (
	// SubscriberClosedGenerationRotated: the provider atomically replaced the
	// journal generation. The old tail ends so a fresh attachment replays the
	// successor bootstrap instead of inheriting an incomplete predecessor view.
	SubscriberClosedGenerationRotated SubscriberCloseReason = "generation_rotated"
	// SubscriberClosedGenerationRefit: an explicit capture-authoritative width
	// refit installed a successor generation. Reattach rebuilds the view from
	// that successor's post-width bootstrap.
	SubscriberClosedGenerationRefit SubscriberCloseReason = "generation_refit"
	// SubscriberClosedLagged: the subscriber fell behind the committed stream
	// — its buffer was full when a committed event had to be delivered, or its
	// cursor was not at the event's predecessor — and was evicted rather than
	// fed a gap or allowed to stall every sibling pane's publication.
	SubscriberClosedLagged SubscriberCloseReason = "subscriber_lagged"
	// SubscriberClosedGenerationFailed: the authoritative journal generation
	// failed after rotation passed its point of no return. Its attachment ends
	// terminally; a fresh operator action may adopt the preserved tmux session.
	SubscriberClosedGenerationFailed SubscriberCloseReason = "generation_failed"
	// SubscriberClosedRefitFaulted: an explicit width refit crossed its point
	// of no return but could not establish a usable successor generation.
	SubscriberClosedRefitFaulted SubscriberCloseReason = "refit_faulted"
)

var subscriberCloseReasons = map[SubscriberCloseReason]struct{}{
	SubscriberClosedGenerationRotated: {},
	SubscriberClosedGenerationRefit:   {},
	SubscriberClosedLagged:            {},
	SubscriberClosedGenerationFailed:  {},
	SubscriberClosedRefitFaulted:      {},
}

// IsSubscriberCloseReason reports whether reason is a member of the closed set.
func IsSubscriberCloseReason(reason SubscriberCloseReason) bool {
	_, ok := subscriberCloseReasons[reason]
	return ok
}

// SubscriberCloseReasons lists the closed set, sorted.
func SubscriberCloseReasons() []SubscriberCloseReason {
	reasons := make([]SubscriberCloseReason, 0, len(subscriberCloseReasons))
	for reason := range subscriberCloseReasons {
		reasons = append(reasons, reason)
	}
	sort.Slice(reasons, func(i, j int) bool { return reasons[i] < reasons[j] })
	return reasons
}

// ClassifyAttachmentError reports the class of a broker attachment error code.
func ClassifyAttachmentError(code string) AttachmentErrorClass {
	if _, ok := operationalAttachmentCodes[code]; ok {
		return AttachmentErrorOperational
	}
	if _, ok := fatalAttachmentCodes[code]; ok {
		return AttachmentErrorFatal
	}
	return AttachmentErrorUnknown
}

// OperationalAttachmentCodes lists the operational class, sorted.
func OperationalAttachmentCodes() []string { return sortedCodes(operationalAttachmentCodes) }

// FatalAttachmentCodes lists the fatal class, sorted.
func FatalAttachmentCodes() []string { return sortedCodes(fatalAttachmentCodes) }

func sortedCodes(set map[string]struct{}) []string {
	codes := make([]string, 0, len(set))
	for code := range set {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}
