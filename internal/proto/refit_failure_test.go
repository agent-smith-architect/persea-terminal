package proto

import "testing"

func TestRefitFailureMetadataIsClosed(t *testing.T) {
	for _, stage := range []RefitFailureStage{
		RefitFailureCapture,
		RefitFailureBindSuccessor,
		RefitFailureMaterialize,
		RefitFailureBeginRegistry,
		RefitFailureQueueBootstrap,
		RefitFailureSubmitBoundary,
		RefitFailureAwaitBoundary,
		RefitFailureValidateRegistry,
		RefitFailureSubmitSeal,
		RefitFailureAwaitSeal,
		RefitFailureCommitRegistry,
		RefitFailureCommitJournal,
		RefitFailureQueuePending,
		RefitFailureSubmitPending,
		RefitFailureAwaitPending,
	} {
		if !IsRefitFailureStage(stage) {
			t.Fatalf("closed refit stage %q rejected", stage)
		}
	}
	if IsRefitFailureStage("private/path") || IsRefitFailureStage("") {
		t.Fatal("arbitrary refit stage accepted")
	}
	for _, class := range []RefitFailureClass{
		RefitFailureInvalidated,
		RefitFailureCapacity,
		RefitFailureStorage,
		RefitFailureObserver,
		RefitFailureInternal,
	} {
		if !IsRefitFailureClass(class) {
			t.Fatalf("closed refit class %q rejected", class)
		}
	}
	if IsRefitFailureClass("permission_denied:/private") || IsRefitFailureClass("") {
		t.Fatal("arbitrary refit class accepted")
	}
}

func TestClientRefitCarriesOptionalRowsButNeverFailureMetadata(t *testing.T) {
	authority := `{"realm":"r","server":"s","uid":1,"selector_kind":"socket_name","selector_value":"sock","boot_id":"b","server_pid":1,"server_start":2,"session_id":"$0","session_created":3}`
	operation := "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr"
	for _, wire := range []string{
		`{"type":"refit","authority":` + authority + `,"cols":96,"id":"` + operation + `"}`,
		`{"type":"refit","authority":` + authority + `,"cols":96,"rows":24,"id":"` + operation + `"}`,
	} {
		if _, err := DecodeClientControl([]byte(wire)); err != nil {
			t.Fatalf("valid refit shape rejected: %v", err)
		}
	}
	for _, wire := range []string{
		`{"type":"refit","authority":` + authority + `,"cols":96,"rows":-1,"id":"` + operation + `"}`,
		`{"type":"refit","authority":` + authority + `,"cols":96,"rows":1001,"id":"` + operation + `"}`,
		`{"type":"refit","authority":` + authority + `,"cols":96,"id":"` + operation + `","refit_stage":"begin_registry","refit_class":"storage"}`,
	} {
		if _, err := DecodeClientControl([]byte(wire)); err == nil {
			t.Fatalf("invalid refit shape accepted: %s", wire)
		}
	}
}
