package broker

import (
	"errors"
	"testing"

	"persea-terminal/internal/unifiedjournal"
)

// No manager is needed to exercise reservation ownership. These limits are
// deliberately small so overlapping generations reach the source limit while
// the realm still has room.
func sourceReservationRuntime() *retentionTrialRuntime {
	return &retentionTrialRuntime{
		options: retentionTrialOptions{sourceRealm: "source-test", sourceServer: "test", maxPanes: 8, maxCommands: 128, maxEnvelopes: 1024, maxIngressBytes: 1024, maxOutboxBytes: 1024, maxBatchBytes: 64,
			sourceLimits: recordingSourceUsage{64, 512, 10, 10}},
		generations: make(map[unifiedjournal.PaneKey]*retentionGeneration),
	}
}

func TestRecordingSourceIdentityUsesExactHostFacts(t *testing.T) {
	witness := retentionWitness("%1", "opaque-a")
	source := recordingSourceForTest(witness)
	base, err := recordingSourceIdentity("realm", "test", source)
	if err != nil {
		t.Fatal(err)
	}
	changed := source
	changed.Columns, changed.Rows, changed.Incarnation = 120, 40, "opaque-b"
	same, _ := recordingSourceIdentity("realm", "test", changed)
	if base != same || base.journalID() != same.journalID() {
		t.Fatal("geometry or opaque incarnation split source allowance")
	}
	for _, field := range []string{"path", "device", "inode", "server-pid", "server-start", "session", "window", "pane", "pane-pid", "pane-start", "realm", "configured-server"} {
		t.Run(field, func(t *testing.T) {
			changed, realm, server := source, "realm", "test"
			switch field {
			case "path":
				changed.Socket.Path += "-replacement"
			case "device":
				changed.Socket.Device++
			case "inode":
				changed.Socket.Inode++
			case "server-pid":
				changed.Server.PID++
			case "server-start":
				changed.Server.StartTime++
			case "session":
				changed.SessionID += "1"
			case "window":
				changed.WindowID += "1"
			case "pane":
				changed.PaneID += "1"
			case "pane-pid":
				changed.Pane.PID++
			case "pane-start":
				changed.Pane.StartTime++
			case "realm":
				realm += "-replacement"
			case "configured-server":
				server += "-replacement"
			}
			got, err := recordingSourceIdentity(realm, server, changed)
			if err != nil || got == base || got.journalID() == base.journalID() {
				t.Fatalf("replacement reused allowance: %v", err)
			}
		})
	}
}

func TestRecordingSourceCreditsSpanHeldGenerations(t *testing.T) {
	runtime := sourceReservationRuntime()
	witness := retentionWitness("%1", "old")
	first, source := journalKey(witness), recordingSourceForTest(witness)
	if err := runtime.bindSource(first, source); err != nil {
		t.Fatal(err)
	}
	held, err := runtime.reserve(first, 6)
	if err != nil {
		t.Fatal(err)
	}
	next := first
	next.ControlGeneration++
	next.Incarnation = "new-geometry"
	source.Columns++
	if err := runtime.bindSource(next, source); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.reserve(next, 5); !errors.Is(err, unifiedjournal.ErrSourceQuota) {
		t.Fatalf("overlap escaped source limit: %v", err)
	}
	if len(runtime.sources) != 1 {
		t.Fatal("generation churn split source account")
	}
	runtime.requestRetire(first)
	runtime.releaseCommand(held)
	if _, err := runtime.reserve(next, 5); !errors.Is(err, unifiedjournal.ErrSourceQuota) {
		t.Fatal("manager completion refunded held callback payload")
	}
	runtime.completePart(held)
	accepted, err := runtime.reserve(next, 10)
	if err != nil {
		t.Fatal(err)
	}
	runtime.cancelReservation(accepted)
	runtime.cancelReservation(accepted)
	account := runtime.generations[next].source
	if account.usage != (recordingSourceUsage{commands: 2}) || account.generations != 1 {
		t.Fatalf("cancel/retire ownership=%+v generations=%d", account.usage, account.generations)
	}
	runtime.requestRetire(next)
	if len(runtime.sources) != 0 || runtime.qUsed != 0 || runtime.pUsed != 0 {
		t.Fatal("last generation retained source account")
	}
}

func TestRecordingSourceProvisionalBindingAndReplacement(t *testing.T) {
	runtime := sourceReservationRuntime()
	witness := retentionWitness("%1", "old")
	key, source := journalKey(witness), recordingSourceForTest(witness)
	if _, err := runtime.reserve(key, 1); !errors.Is(err, unifiedjournal.ErrSourceQuota) {
		t.Fatalf("unbound payload admission=%v", err)
	}
	if len(runtime.generations) != 0 || runtime.qUsed != 0 {
		t.Fatal("failed admission leaked provisional ownership")
	}
	provisional, err := runtime.reserve(key, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.bindSource(key, source); err != nil {
		t.Fatal(err)
	}
	if runtime.generations[key].source.usage.commands != 3 {
		t.Fatal("binding omitted provisional reservation")
	}
	changed := source
	changed.Pane.StartTime++
	if err := runtime.bindSource(key, changed); err == nil {
		t.Fatal("same generation rebound to replacement process")
	}
	replacement := key
	replacement.ControlGeneration++
	if err := runtime.bindSource(replacement, changed); err != nil {
		t.Fatal(err)
	}
	if len(runtime.sources) != 2 {
		t.Fatal("actual process replacement shared source allowance")
	}
	runtime.cancelReservation(provisional)
	runtime.requestRetire(key)
	runtime.requestRetire(replacement)
	if len(runtime.sources) != 0 {
		t.Fatal("provisional cancellation retained identity")
	}
}

func TestRecordingSourceAccountWaitsForManagerAfterFastDispatch(t *testing.T) {
	runtime := sourceReservationRuntime()
	witness := retentionWitness("%fast", "fast-dispatch")
	key := journalKey(witness)
	if err := runtime.bindSource(key, recordingSourceForTest(witness)); err != nil {
		t.Fatal(err)
	}
	reservation, err := runtime.reserve(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	runtime.requestRetire(key)
	// A callback can finish before processCommand refunds Q. The exact
	// source account must retain that command owner until both sides settle.
	runtime.completePart(reservation)
	if len(runtime.sources) != 1 || runtime.generations[key] == nil || runtime.generations[key].source.usage.commands != 3 {
		t.Fatal("fast dispatcher dropped outstanding command ownership")
	}
	runtime.releaseCommand(reservation)
	if len(runtime.sources) != 0 || len(runtime.generations) != 0 || runtime.qUsed != 0 {
		t.Fatal("manager completion did not settle last source owner")
	}
}
