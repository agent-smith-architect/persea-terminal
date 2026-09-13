package broker

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/unifiedjournal"
)

// Full ordinary capacity must not turn an explicitly settled recording fault
// into a permanent inability to reconstruct the same still-living source.
func TestRetentionFailedReconstructionFullSlots(t *testing.T) {
	fixture, session, key, witness, unit := failedReconstructionFixture(t)
	facts := recordingSourceFacts(t, fixture.disposable, session)
	failedReconstructionFill(t, fixture, key, witness)
	before := failedReconstructionBudget(t, fixture)
	failedReconstructionFault(t, fixture, witness, unit)
	failedReconstructionSettled(t, fixture, key, unit)
	failedReconstructionRetained(t, fixture, key, before)
	failedReconstructionRecover(t, fixture, session, key, facts)
}

func failedReconstructionFixture(t *testing.T) (*adoptionFixture, string, unifiedjournal.PaneKey, controlmode.PaneWitness, *unifiedDevUnit) {
	t.Helper()
	disposable := newDisposable(t)
	server := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	runtimeDir := t.TempDir()
	cfg := unifiedAdoptionDevConfig(t, server, runtimeDir, 0)
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A refused automatic rotation isolates the existing hard-cap fault path.
	// It does not change the cap, slot count, source allowance or production code.
	effects.rotationAttempt = func(context.Context, string) error { return ErrUnifiedRotateUnavailable }
	registry := newPaneRegistry(effects)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = effects.RunObserver(ctx, registry) }()
	t.Cleanup(func() { cancel(); _ = registry.Close() })
	fixture := &adoptionFixture{disposable: disposable, server: server, cfg: cfg, runtimeDir: runtimeDir, effects: effects, registry: registry, cancel: cancel}
	var session string
	var key unifiedjournal.PaneKey
	for source := 0; source < 8; source++ {
		command := "sleep 600"
		if source == 0 {
			command = "stty -echo -opost; exec env PS1='' /bin/sh"
		}
		id := fixture.startPaneCommand(t, fmt.Sprintf("failed-reconstruction-%d", source), command)
		adoption, err := effects.AdoptSession(ctx, id)
		if err != nil {
			t.Fatalf("source %d admission: %v", source, err)
		}
		if source == 0 {
			session, key = id, adoption.Key
		}
	}
	effects.journalMu.Lock()
	slots := effects.realm.AvailableCompletePaneSlots()
	_, paneCap := effects.realm.PaneLogical(key)
	_, _, realmCap := effects.realm.LogicalBudget()
	effects.journalMu.Unlock()
	if slots != 0 || paneCap != 8<<20 || realmCap != 64<<20 {
		t.Fatalf("production precondition slots=%d pane=%d realm=%d", slots, paneCap, realmCap)
	}
	effects.mu.Lock()
	unit := effects.units[session]
	var witness controlmode.PaneWitness
	if unit != nil && len(unit.witnesses) == 1 {
		witness = unit.witnesses[0]
	}
	effects.mu.Unlock()
	if unit == nil || witness == (controlmode.PaneWitness{}) {
		t.Fatal("exact source observer missing")
	}
	return fixture, session, key, witness, unit
}

func failedReconstructionFill(t *testing.T, fixture *adoptionFixture, key unifiedjournal.PaneKey, witness controlmode.PaneWitness) {
	failedReconstructionFillBeforeLast(t, fixture, key, witness, nil)
}

func failedReconstructionFillBeforeLast(t *testing.T, fixture *adoptionFixture, key unifiedjournal.PaneKey, witness controlmode.PaneWitness, beforeLast func()) {
	t.Helper()
	flush := func() {
		t.Helper()
		if err := fixture.registry.retention.Boundary(key, "failed_reconstruction_fill"); err != nil {
			t.Fatal(err)
		}
	}
	flush()
	fixture.effects.journalMu.Lock()
	before, cap := fixture.effects.realm.PaneLogical(key)
	fixture.effects.journalMu.Unlock()
	remaining := cap - before - 1
	if remaining <= 0 {
		t.Fatal("initial capture left no cap headroom")
	}
	for remaining > 0 {
		n := min(remaining, 64<<10)
		if err := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: witness, Data: bytes.Repeat([]byte{'X'}, int(n))}); err != nil {
			t.Fatal(err)
		}
		flush()
		remaining -= n
	}
	fixture.effects.journalMu.Lock()
	minusOne, _ := fixture.effects.realm.PaneLogical(key)
	fixture.effects.journalMu.Unlock()
	if minusOne != cap-1 {
		t.Fatalf("cap-1=%d want=%d", minusOne, cap-1)
	}
	if beforeLast != nil {
		beforeLast()
	}
	if err := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: witness, Data: []byte{'Y'}}); err != nil {
		t.Fatal(err)
	}
	flush()
	fixture.effects.journalMu.Lock()
	logical, _ := fixture.effects.realm.PaneLogical(key)
	fixture.effects.journalMu.Unlock()
	if logical != cap {
		t.Fatalf("exact cap=%d want=%d", logical, cap)
	}
}

type failedReconstructionCharges struct {
	logical, physical, logicalReserved, physicalReserved, slots int64
	files                                                       int
}

func failedReconstructionBudget(t *testing.T, fixture *adoptionFixture) failedReconstructionCharges {
	t.Helper()
	fixture.effects.journalMu.Lock()
	defer fixture.effects.journalMu.Unlock()
	l, lr, _ := fixture.effects.realm.LogicalBudget()
	p, pr, _ := fixture.effects.realm.PhysicalBudget()
	return failedReconstructionCharges{l, p, lr, pr, fixture.effects.realm.AvailableCompletePaneSlots(), fixture.journalFileCount(t)}
}

func failedReconstructionFault(t *testing.T, fixture *adoptionFixture, witness controlmode.PaneWitness, unit *unifiedDevUnit) {
	t.Helper()
	if err := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: witness, Data: []byte{'Z'}}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.registry.retention.Boundary(journalKey(witness), "failed_reconstruction_first_over"); err == nil {
		t.Fatal("cap+1 unexpectedly succeeded")
	}
	// Direct observer injection bypasses the run loop's fatal return. Invoke
	// its existing known-fault teardown owner, which stops the exact observer;
	// the real process owner still performs Wait before the settlement check.
	fixture.effects.reapFaultedUnit(unit)
}

func failedReconstructionSettled(t *testing.T, fixture *adoptionFixture, key unifiedjournal.PaneKey, unit *unifiedDevUnit) {
	t.Helper()
	fence, err := fixture.registry.retention.startDispatchFence()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-fence:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch settlement fence timed out")
	}
	pollUntil(t, 5*time.Second, "old runtime owner and actual observer Wait", func() bool {
		fixture.registry.retention.mu.Lock()
		_, old := fixture.registry.retention.generations[key]
		fixture.registry.retention.mu.Unlock()
		fixture.effects.mu.Lock()
		_, active := fixture.effects.active[key.Session]
		_, running := fixture.effects.units[key.Session]
		fixture.effects.mu.Unlock()
		return !old && !active && !running && syscall.Kill(unit.process.Process.Pid, 0) == syscall.ESRCH
	})
	readers := fixture.effects.readers.snapshot()
	if readers.Readers != 0 || readers.Events != 0 || readers.Snapshots != 0 {
		t.Fatalf("old reader ownership remains: %+v", readers)
	}
}

func failedReconstructionRetained(t *testing.T, fixture *adoptionFixture, key unifiedjournal.PaneKey, before failedReconstructionCharges) {
	t.Helper()
	after := failedReconstructionBudget(t, fixture)
	fixture.effects.journalMu.Lock()
	reason := fixture.effects.realm.Reason(key)
	state := fixture.effects.realm.Eligibility(key)
	fixture.effects.journalMu.Unlock()
	if after != before || reason != unifiedjournal.ReasonQuota || state != unifiedjournal.EligibilityUntrusted {
		t.Fatalf("fault must retain original journal liabilities: before=%+v after=%+v reason=%v state=%v", before, after, reason, state)
	}
	t.Logf("settled quota fault retained bytes/files/slots: %+v key=%+v", after, key)
}

func failedReconstructionRecover(t *testing.T, fixture *adoptionFixture, session string, old unifiedjournal.PaneKey, facts string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, session)
	if err != nil {
		t.Fatalf("settled exact-source reconstruction at full ordinary slots: %v", err)
	}
	if adoption.Key == old || !fixture.effects.recordingReady(adoption.Key) {
		t.Fatal("reconstruction did not publish new authoritative generation")
	}
	_, _, tail, release, err := fixture.effects.openSnapshotTail(session)
	if err != nil {
		t.Fatal(err)
	}
	tail.releaseSnapshot()
	defer release()
	marker := "SETTLED_FAILED_RECONSTRUCTION_OK"
	fixture.disposable.run("send-keys", "-l", "-t", "="+session+":", "printf '\\n"+marker+"\\n'")
	fixture.disposable.run("send-keys", "-t", "="+session+":", "Enter")
	var received string
	for !strings.Contains(received, marker) {
		select {
		case event, ok := <-tail.events():
			if !ok {
				t.Fatalf("new reader closed before marker: %s", tail.closeReason())
			}
			received += string(event.Payload)
			tail.releaseEvent(event)
			if len(received) > 256 {
				received = received[len(received)-256:]
			}
		case <-ctx.Done():
			t.Fatal("new authoritative committed marker timed out")
		}
	}
	if got := recordingSourceFacts(t, fixture.disposable, session); got != facts {
		t.Fatal("reconstruction changed source geometry/options")
	}
}

func TestRetentionFailedReconstructionCaptureRetry(t *testing.T) {
	fixture, session, key, witness, unit := failedReconstructionFixture(t)
	facts := recordingSourceFacts(t, fixture.disposable, session)
	failedReconstructionFill(t, fixture, key, witness)
	before := failedReconstructionBudget(t, fixture)
	failedReconstructionFault(t, fixture, witness, unit)
	failedReconstructionSettled(t, fixture, key, unit)
	fixture.effects.adoptionPostTamper = func(_ int, post string) string { return post + " INVALID_CAPTURE_WITNESS" }
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := fixture.effects.AdoptSession(ctx, session)
		cancel()
		if err == nil {
			t.Fatal("invalid capture granted reconstruction")
		}
		failedReconstructionRetained(t, fixture, key, before)
	}
	fixture.effects.adoptionPostTamper = nil
	failedReconstructionRecover(t, fixture, session, key, facts)
}

func TestRetentionFailedReconstructionHeldWriter(t *testing.T) {
	fixture, session, key, witness, unit := failedReconstructionFixture(t)
	facts := recordingSourceFacts(t, fixture.disposable, session)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	failedReconstructionFillBeforeLast(t, fixture, key, witness, func() {
		fence, err := fixture.registry.retention.startDispatchFence()
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-fence:
		case <-time.After(5 * time.Second):
			t.Fatal("pre-hold writer fence timed out")
		}
		fixture.registry.retention.setHook(func(stage string, candidate unifiedjournal.PaneKey) {
			if stage == "before_downstream_write" && candidate == key {
				close(entered)
				<-release
			}
		})
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("writer hold was not reached")
	}
	before := failedReconstructionBudget(t, fixture)
	failedReconstructionFault(t, fixture, witness, unit)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	_, err := fixture.effects.AdoptSession(ctx, session)
	cancel()
	if err == nil {
		t.Fatal("held writer granted fresh reconstruction")
	}
	failedReconstructionRetained(t, fixture, key, before)
	once.Do(func() { close(release) })
	failedReconstructionSettled(t, fixture, key, unit)
	failedReconstructionRecover(t, fixture, session, key, facts)
}

// Retained work must still prevent the faulted generation from becoming an
// eligible full-slot reconstruction candidate merely because clients closed.
func TestRetentionFailedReconstructionHeldCleanup(t *testing.T) {
	fixture, session, key, witness, unit := failedReconstructionFixture(t)
	facts := recordingSourceFacts(t, fixture.disposable, session)
	failedReconstructionFill(t, fixture, key, witness)
	before := failedReconstructionBudget(t, fixture)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	fixture.registry.retention.setHook(func(stage string, candidate unifiedjournal.PaneKey) {
		if stage == "before_cleanup" && candidate == key {
			close(entered)
			<-release
		}
	})
	failedReconstructionFault(t, fixture, witness, unit)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup hold was not reached")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	_, err := fixture.effects.AdoptSession(ctx, session)
	cancel()
	if err == nil {
		t.Fatal("held cleanup granted fresh reconstruction")
	}
	failedReconstructionRetained(t, fixture, key, before)
	once.Do(func() { close(release) })
	failedReconstructionSettled(t, fixture, key, unit)
	failedReconstructionRecover(t, fixture, session, key, facts)
}
