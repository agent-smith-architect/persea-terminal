package proto

import (
	"regexp"
	"testing"
)

var canonicalAttachmentCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func TestAttachmentErrorClassesAreDisjointCanonicalAndFailClosed(t *testing.T) {
	for _, code := range OperationalAttachmentCodes() {
		if !canonicalAttachmentCode.MatchString(code) {
			t.Fatalf("operational code %q is not canonical", code)
		}
		if ClassifyAttachmentError(code) != AttachmentErrorOperational {
			t.Fatalf("operational code %q classified %v", code, ClassifyAttachmentError(code))
		}
		if _, fatal := fatalAttachmentCodes[code]; fatal {
			t.Fatalf("code %q is in both classes", code)
		}
	}
	for _, code := range FatalAttachmentCodes() {
		if !canonicalAttachmentCode.MatchString(code) {
			t.Fatalf("fatal code %q is not canonical", code)
		}
		if ClassifyAttachmentError(code) != AttachmentErrorFatal {
			t.Fatalf("fatal code %q classified %v", code, ClassifyAttachmentError(code))
		}
	}
	for _, unknown := range []string{"", "some_future_code", "Resize_Failed", "pong"} {
		if ClassifyAttachmentError(unknown) != AttachmentErrorUnknown {
			t.Fatalf("unknown code %q did not fail closed", unknown)
		}
	}
	// The operational class is the closed set the design ruling named: one
	// request's outcome, never a verdict on the attachment.
	want := []string{"input_refused", "observe_mode", "resize_failed", "resize_rejected"}
	got := OperationalAttachmentCodes()
	if len(got) != len(want) {
		t.Fatalf("operational codes=%v want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("operational codes=%v want %v", got, want)
		}
	}
}

// Every subscriber close reason is a canonical FATAL attachment code: it ends
// the attachment it is written on and travels to the browser as the WebSocket
// close reason. Rotation adds `generation_rotated` by adding one member here;
// a member missing from the fatal set would be an unknown code (fail-closed
// fatal on the wire, but invisible to the browser's source-to-policy fence).
func TestSubscriberCloseReasonsAreFatalAttachmentCodes(t *testing.T) {
	reasons := SubscriberCloseReasons()
	if len(reasons) == 0 {
		t.Fatal("no subscriber close reasons")
	}
	for _, reason := range reasons {
		if !canonicalAttachmentCode.MatchString(string(reason)) {
			t.Fatalf("subscriber close reason %q is not canonical", reason)
		}
		if !IsSubscriberCloseReason(reason) {
			t.Fatalf("subscriber close reason %q is not a member of its own set", reason)
		}
		if ClassifyAttachmentError(string(reason)) != AttachmentErrorFatal {
			t.Fatalf("subscriber close reason %q classified %v, want fatal", reason, ClassifyAttachmentError(string(reason)))
		}
	}
	if IsSubscriberCloseReason("") || IsSubscriberCloseReason("stale_target") {
		t.Fatal("non-members must not classify as subscriber close reasons")
	}
	if !IsSubscriberCloseReason(SubscriberClosedLagged) {
		t.Fatal("subscriber_lagged must be a member")
	}
	if !IsSubscriberCloseReason(SubscriberClosedGenerationRotated) {
		t.Fatal("generation_rotated must be a member")
	}
	if !IsSubscriberCloseReason(SubscriberClosedGenerationRefit) || !IsSubscriberCloseReason(SubscriberClosedRefitFaulted) {
		t.Fatal("generation_refit and refit_faulted must be members")
	}
	want := []SubscriberCloseReason{SubscriberClosedGenerationFailed, SubscriberClosedGenerationRefit, SubscriberClosedGenerationRotated, SubscriberClosedRefitFaulted, SubscriberClosedLagged}
	if len(reasons) != len(want) {
		t.Fatalf("subscriber close reasons=%v want %v", reasons, want)
	}
	for index := range want {
		if reasons[index] != want[index] {
			t.Fatalf("subscriber close reasons=%v want %v", reasons, want)
		}
	}
}
