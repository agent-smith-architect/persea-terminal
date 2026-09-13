package broker

// Provenance: Fable adversarial adjudication of M11 P2a candidate 03ec589
// (docs: 2026-08-27_p2a_adjudication.md). Seam regression for the §3.3 step 7
// order: 7-pre Validate → 7a seal the predecessor with retire → 7b Commit.

import (
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
)

// The design seals the predecessor (rotation_seal, retire: true) BEFORE
// Commit. Retirement clears the predecessor generation's admitted flag and
// may delete the generation outright once its references drop. Commit is
// infallible after a green Validate and must therefore not re-require a live
// predecessor generation; if it does, every real rotation takes the F14
// fatal path.
func TestP2AReviewCommitSucceedsAfterPredecessorSeal(t *testing.T) {
	fixture := newP2AFixture(t)
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := txn.WriteBootstrap([]byte("seal-bootstrap")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.boundary(next); err != nil {
		t.Fatal(err)
	}
	if err := txn.Validate(); err != nil {
		t.Fatal(err)
	}
	// Step 7a, exactly as §3.3 specifies: seal the predecessor with retire,
	// waiting while holding no locks.
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
	txn.markSealed()
	// Step 7b.
	if disposition := txn.Commit(); disposition != paneRotationCommitted {
		t.Fatalf("Commit after the predecessor seal disposition=%d, want committed", disposition)
	}
	fixture.effects.journalMu.Lock()
	reservation.Commit()
	fixture.effects.journalMu.Unlock()

	fixture.registry.mu.Lock()
	session := fixture.registry.sessions[sessionKey(next.Session)]
	_, oldAdmitted := fixture.registry.admitted[routeCoordinateKey(fixture.previous)]
	newAdmission, newAdmitted := fixture.registry.admitted[routeCoordinateKey(next)]
	broken := fixture.registry.broken
	fixture.registry.mu.Unlock()
	if broken {
		t.Fatal("Commit after the predecessor seal tripped the realm breaker")
	}
	if session != txn.successorSession || oldAdmitted || !newAdmitted || newAdmission.witness != next {
		t.Fatalf("Commit after the predecessor seal did not install the successor: session=%p successor=%p old=%v new=%v",
			session, txn.successorSession, oldAdmitted, newAdmitted)
	}
	if len(fixture.unit.witnesses) != 2 || fixture.unit.witnesses[1] != next {
		t.Fatalf("Commit after the predecessor seal did not install the staged witness: %#v", fixture.unit.witnesses)
	}
	if err := fixture.registry.ObservePane(controlmode.Observation{
		Kind: controlmode.ObservationOutput, Witness: next, Data: []byte("post-seal-output"),
	}); err != nil {
		t.Fatalf("successor output after seal+commit: %v", err)
	}
}
