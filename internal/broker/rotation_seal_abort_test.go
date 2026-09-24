package broker

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/unifiedjournal"
)

func (fixture *rotationFixture) sealPredecessor(t *testing.T) {
	t.Helper()
	sealed, err := fixture.registry.retention.startBoundary(journalKey(fixture.previous), "rotation_seal", true)
	if err != nil {
		t.Fatalf("start seal: %v", err)
	}
	select {
	case err := <-sealed:
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("seal did not complete")
	}
}

// Past the seal there is no abort. Abort after
// markSealed must not be able to settle the transaction as "aborted" and
// leave the predecessor admitted-and-routable over a retired generation.
func TestRotationAbortAfterSealIsNotARollback(t *testing.T) {
	fixture := newRotationFixture(t)
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}
	fixture.writeBootstrap(txn)
	if err := txn.Validate(); err != nil {
		t.Fatal(err)
	}
	fixture.sealPredecessor(t)
	txn.markSealed()
	if txn.Abort() {
		t.Fatal("Abort settled a transaction after the predecessor seal")
	}

	fixture.registry.mu.Lock()
	session := fixture.registry.sessions[sessionKey(fixture.previous.Session)]
	oldAdmission, oldAdmitted := fixture.registry.admitted[routeCoordinateKey(fixture.previous)]
	predecessorRoutable := session != nil && session.router.UnifiedEligible(fixture.previous)
	broken := fixture.registry.broken
	fixture.registry.mu.Unlock()
	fixture.registry.retention.mu.Lock()
	predecessorGeneration := fixture.registry.retention.generations[journalKey(fixture.previous)]
	predecessorLive := predecessorGeneration != nil && predecessorGeneration.admitted && !predecessorGeneration.retireRequested
	fixture.registry.retention.mu.Unlock()

	// Abort is refused and leaves the transaction under its sequenced owner;
	// Commit is the only legal next transition after the seal.
	if broken {
		t.Fatal("Abort after the seal tripped the realm breaker")
	}
	if txn.settled || txn.disposition == paneRotationAborted {
		t.Fatalf("refused post-seal Abort settled transaction: settled=%v disposition=%d", txn.settled, txn.disposition)
	}
	if !oldAdmitted || oldAdmission.failedIncarnation != "" || !predecessorRoutable || predecessorLive {
		t.Fatalf("post-seal authority before Commit: admitted=%v failed=%q routable=%v generation_live=%v",
			oldAdmitted, oldAdmission.failedIncarnation, predecessorRoutable, predecessorLive)
	}
	if disposition := txn.Commit(); disposition != paneRotationCommitted {
		t.Fatalf("refused post-seal Abort left Commit disposition=%d", disposition)
	}
	fixture.effects.journalMu.Lock()
	reservation.Commit()
	fixture.effects.journalMu.Unlock()
}

func TestRotationAbortImmediatelyBeforeSealSettlesOnce(t *testing.T) {
	fixture := newRotationFixture(t)
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}
	fixture.writeBootstrap(txn)
	if err := txn.Validate(); err != nil {
		t.Fatal(err)
	}
	if !txn.Abort() || !txn.Abort() {
		t.Fatal("pre-seal Abort did not settle idempotently")
	}
	if txn.Commit() != paneRotationAborted {
		t.Fatalf("pre-seal Abort disposition=%d", txn.Commit())
	}
	fixture.abortReservation(reservation)
}

// An abandoned successor generation (Abort while a bootstrap write still
// holds a reference) is a retiring authority. Nothing may re-admit it: a
// later admission of the same key would be deleted underneath the admitted
// pane when the held reference settles.
func TestRotationAbandonedSuccessorCannotBeReadmitted(t *testing.T) {
	fixture := newRotationFixture(t)
	usage := func() [5]int64 {
		fixture.registry.retention.mu.Lock()
		defer fixture.registry.retention.mu.Unlock()
		runtime := fixture.registry.retention
		return [5]int64{int64(runtime.pUsed), int64(runtime.qUsed), int64(runtime.eUsed), runtime.bUsed, runtime.oUsed}
	}
	baseline := usage()
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	fixture.effects.mu.Lock()
	fixture.effects.lockProbe = func(stage string) {
		if stage == "append" {
			once.Do(func() { close(entered); <-release })
		}
	}
	fixture.effects.mu.Unlock()
	if err := txn.WriteBootstrap([]byte("held")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("bootstrap append did not enter")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, refs := fixture.successorGeneration(next); refs > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bootstrap write never took a successor reference")
		}
		time.Sleep(time.Millisecond)
	}
	held := usage()
	if !txn.Abort() {
		t.Fatal("pre-seal Abort was refused")
	}
	if txn.disposition != paneRotationAborted || txn.Commit() != paneRotationAborted {
		t.Fatalf("aborted settlement disposition=%d", txn.disposition)
	}
	if afterAbort := usage(); afterAbort != held {
		t.Fatalf("Abort refunded a referenced successor early: held=%v after=%v", held, afterAbort)
	}
	_, _, ensureErr := fixture.registry.retention.ensureGeneration(journalKey(next), true)
	_, markErr := fixture.registry.retention.markAdmitted(journalKey(next))

	// Re-admission of the abandoned key while the reference is still held is
	// permanently refused, including repeated attempts.
	var admitErrs []error
	for attempt := 0; attempt < 2; attempt++ {
		admitErrs = append(admitErrs, fixture.registry.AdmitPane(next))
	}
	_, _, _ = fixture.successorGeneration(next)
	releaseOnce.Do(func() { close(release) })
	fixture.effects.mu.Lock()
	fixture.effects.lockProbe = nil
	fixture.effects.mu.Unlock()
	fixture.waitSuccessorQuiescent(next)
	time.Sleep(20 * time.Millisecond)
	present, admitted, _ := fixture.successorGeneration(next)
	fixture.registry.mu.Lock()
	_, routeAdmitted := fixture.registry.admitted[routeCoordinateKey(next)]
	broken := fixture.registry.broken
	fixture.registry.mu.Unlock()
	t.Logf("re-admission of abandoned key: ensure=%v mark=%v admits=%v generation present=%v admitted=%v route admitted=%v broken=%v", ensureErr, markErr, admitErrs, present, admitted, routeAdmitted, broken)
	if !errors.Is(ensureErr, unifiedjournal.ErrInvalidated) {
		t.Errorf("ensureGeneration admitted abandoned successor: %v", ensureErr)
	}
	if !errors.Is(markErr, unifiedjournal.ErrInvalidated) {
		t.Errorf("markAdmitted accepted abandoned successor: %v", markErr)
	}
	for attempt, admitErr := range admitErrs {
		if !errors.Is(admitErr, unifiedjournal.ErrInvalidated) {
			t.Errorf("re-admission attempt %d error = %v", attempt, admitErr)
		}
	}
	if broken {
		t.Fatal("re-admission of an abandoned key tripped the realm breaker")
	}
	if routeAdmitted {
		t.Fatal("refused re-admission left a route admission")
	}
	if present || admitted {
		t.Fatalf("abandoned successor remained after its last reference: present=%v admitted=%v", present, admitted)
	}
	if afterRetire := usage(); afterRetire != baseline {
		t.Fatalf("last-reference retirement did not refund P/Q/E/B/O: baseline=%v after=%v", baseline, afterRetire)
	}
	fixture.abortReservation(reservation)
}

// A seal that never happened must not let markSealed relax Commit: with the
// predecessor still admitted, the post-seal predicate must refuse rather
// than install over a live predecessor generation as if it were retired.
func TestRotationMarkSealedWithoutSealDoesNotRelaxCommit(t *testing.T) {
	fixture := newRotationFixture(t)
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}
	fixture.writeBootstrap(txn)
	if err := txn.Validate(); err != nil {
		t.Fatal(err)
	}
	txn.markSealed()
	disposition := txn.Commit()
	fixture.registry.mu.Lock()
	_, successorAdmitted := fixture.registry.admitted[routeCoordinateKey(next)]
	broken := fixture.registry.broken
	fixture.registry.mu.Unlock()
	if broken {
		t.Fatal("markSealed without a seal tripped the realm breaker")
	}
	if disposition == paneRotationCommitted && !successorAdmitted {
		t.Fatal("committed disposition without successor admission")
	}
	t.Logf("markSealed without seal: disposition=%d successor admitted=%v", disposition, successorAdmitted)
	fixture.abortReservation(reservation)
}

// A stale predecessor observation after the seal (something rotation flow must never
// produce) re-materializes unknown old-key payload. It must take the local F14
// path rather than broadening the post-seal predecessor predicate.
func TestRotationStalePredecessorObservationAfterSeal(t *testing.T) {
	fixture := newRotationFixture(t)
	other := controlmode.PaneWitness{
		Session: controlmode.SessionWitness{Server: "main", Session: "$rotation-other", ControlGeneration: 1},
		Window:  "@seal-other", Pane: "%seal-other", Incarnation: "3000,1,0",
	}
	fixture.admitRotationPane(other)
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}
	fixture.writeBootstrap(txn)
	if err := txn.Validate(); err != nil {
		t.Fatal(err)
	}
	fixture.sealPredecessor(t)
	fixture.waitSuccessorQuiescentKey(journalKey(fixture.previous))
	staleErr := fixture.registry.ObservePane(controlmode.Observation{
		Kind: controlmode.ObservationOutput, Witness: fixture.previous, Data: []byte("stale-after-seal"),
	})
	fixture.registry.retention.mu.Lock()
	regenerated := fixture.registry.retention.generations[journalKey(fixture.previous)]
	fixture.registry.retention.mu.Unlock()
	fixture.effects.mu.Lock()
	activeBefore := fixture.effects.active[fixture.unit.sessionID]
	fixture.effects.mu.Unlock()
	witnessesBefore := append([]controlmode.PaneWitness(nil), fixture.unit.witnesses...)
	txn.markSealed()
	disposition := txn.Commit()
	fixture.registry.mu.Lock()
	broken := fixture.registry.broken
	_, otherAdmitted := fixture.registry.admitted[routeCoordinateKey(other)]
	_, successorAdmitted := fixture.registry.admitted[routeCoordinateKey(next)]
	session := fixture.registry.sessions[sessionKey(fixture.previous.Session)]
	fixture.registry.mu.Unlock()
	t.Logf("stale predecessor observation after seal: observe err=%v regenerated=%v disposition=%d", staleErr, regenerated != nil, disposition)
	if broken {
		t.Fatal("stale predecessor observation after the seal tripped the realm breaker")
	}
	if disposition != paneRotationCommitFatal {
		t.Fatalf("stale predecessor disposition=%d want fatal", disposition)
	}
	if !otherAdmitted || successorAdmitted || session == txn.successorSession {
		t.Fatalf("fatal-local settlement changed admissions/router: other=%v successor=%v successor_router=%v", otherAdmitted, successorAdmitted, session == txn.successorSession)
	}
	fixture.effects.mu.Lock()
	activeAfter, active := fixture.effects.active[fixture.unit.sessionID]
	fixture.effects.mu.Unlock()
	if active && activeAfter != activeBefore {
		t.Fatalf("fatal-local settlement swapped active key: before=%#v after=%#v", activeBefore, activeAfter)
	}
	if !slices.Equal(fixture.unit.witnesses, witnessesBefore) {
		t.Fatalf("fatal-local settlement installed successor witness: %#v", fixture.unit.witnesses)
	}
	if err := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: other, Data: []byte("other-after-local-fatal")}); err != nil {
		t.Fatalf("unrelated admission stopped after local fatal: %v", err)
	}
	fixture.waitSuccessorQuiescent(next)
	fixture.abortReservation(reservation)
}

func (fixture *rotationFixture) waitSuccessorQuiescentKey(key unifiedjournal.PaneKey) {
	fixture.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		fixture.registry.retention.mu.Lock()
		generation := fixture.registry.retention.generations[key]
		quiescent := generation == nil || generation.refs == 0
		fixture.registry.retention.mu.Unlock()
		if quiescent || time.Now().After(deadline) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}
