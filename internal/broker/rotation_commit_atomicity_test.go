package broker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/unifiedjournal"
)

type rotationRotation struct {
	fixture     *rotationFixture
	next        controlmode.PaneWitness
	txn         *paneRotationTxn
	reservation *unifiedjournal.AdoptionReservation
}

func rotationPrepareRotation(t *testing.T, fixture *rotationFixture) *rotationRotation {
	t.Helper()
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := txn.WriteBootstrap([]byte("lifecycle-bootstrap")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.registry.retentionBoundary(next, "rotation_bootstrap"); err != nil {
		t.Fatal(err)
	}
	if err := txn.Validate(); err != nil {
		t.Fatal(err)
	}
	return &rotationRotation{fixture: fixture, next: next, txn: txn, reservation: reservation}
}

func (rotation *rotationRotation) commit(t *testing.T) {
	t.Helper()
	sealed, err := rotation.fixture.registry.retention.startBoundary(
		journalKey(rotation.fixture.previous), "rotation_seal", true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-sealed; err != nil {
		t.Fatal(err)
	}
	rotation.txn.markSealed()
	if disposition := rotation.txn.Commit(); disposition != paneRotationCommitted {
		t.Fatalf("Commit disposition=%d", disposition)
	}
	rotation.fixture.effects.journalMu.Lock()
	rotation.reservation.Commit()
	rotation.fixture.effects.journalMu.Unlock()
}

func rotationAssertSuccessorOnly(t *testing.T, rotation *rotationRotation) {
	t.Helper()
	fixture := rotation.fixture
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := rotationRuntimeLedger(fixture.registry.retention); got == (rotationLedger{p: 1, q: 2}) {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("rotation ledger=%+v want {p:1 q:2}", got)
		}
		time.Sleep(time.Millisecond)
	}

	fixture.registry.mu.Lock()
	session := fixture.registry.sessions[sessionKey(rotation.next.Session)]
	admission, admitted := fixture.registry.admitted[routeCoordinateKey(rotation.next)]
	routerOK := session != nil && session.router.UnifiedEligible(rotation.next)
	fixture.registry.retention.mu.Lock()
	oldGeneration := fixture.registry.retention.generations[journalKey(fixture.previous)]
	activeGeneration := fixture.registry.retention.generations[journalKey(rotation.next)]
	fixture.registry.retention.mu.Unlock()
	fixture.registry.mu.Unlock()
	if !admitted || admission.witness != rotation.next || !routerOK {
		t.Fatalf("stale Disconnect mutated successor authority: admitted=%v witness=%+v routerOK=%v", admitted, admission.witness, routerOK)
	}
	if oldGeneration != nil || activeGeneration == nil {
		t.Fatalf("generation residue: predecessor=%p successor=%p", oldGeneration, activeGeneration)
	}

	var oldDisconnects, activeDisconnects atomic.Int64
	fixture.registry.retention.setHook(func(point string, key unifiedjournal.PaneKey) {
		if point != "after_ingress_reserve" {
			return
		}
		switch key {
		case journalKey(fixture.previous):
			oldDisconnects.Add(1)
		case journalKey(rotation.next):
			activeDisconnects.Add(1)
		}
	})
	fixture.effects.reapFaultedUnitOnce(fixture.unit)
	if oldDisconnects.Load() != 0 || activeDisconnects.Load() != 1 {
		t.Fatalf("reap reservations: predecessor=%d successor=%d want 0/1", oldDisconnects.Load(), activeDisconnects.Load())
	}
}

func TestRotationDisconnectCannotReserveAcrossRotationCommit(t *testing.T) {
	fixture := newRotationFixture(t)
	rotation := rotationPrepareRotation(t, fixture)
	var once sync.Once
	var oldReservations atomic.Int64
	fixture.registry.retention.setHook(func(point string, key unifiedjournal.PaneKey) {
		if key != journalKey(fixture.previous) {
			return
		}
		if point == "before_disconnect_reserve" {
			once.Do(func() { rotation.commit(t) })
		}
		if point == "after_ingress_reserve" {
			oldReservations.Add(1)
		}
	})
	if err := fixture.registry.ObservePane(controlmode.Observation{
		Kind: controlmode.ObservationDisconnect, Witness: fixture.previous,
	}); err != nil {
		t.Fatal(err)
	}
	if got := oldReservations.Load(); got != 0 {
		t.Fatalf("stale Disconnect reserved retired predecessor %d times", got)
	}
	rotationAssertSuccessorOnly(t, rotation)
}

func TestRotationDisconnectRechecksAuthorityAfterReservation(t *testing.T) {
	fixture := newRotationFixture(t)
	rotation := rotationPrepareRotation(t, fixture)
	var once sync.Once
	fixture.registry.retention.setHook(func(point string, key unifiedjournal.PaneKey) {
		if point == "after_ingress_reserve" && key == journalKey(fixture.previous) {
			once.Do(func() { rotation.commit(t) })
		}
	})
	if err := fixture.registry.ObservePane(controlmode.Observation{
		Kind: controlmode.ObservationDisconnect, Witness: fixture.previous,
	}); err != nil {
		t.Fatal(err)
	}
	rotationAssertSuccessorOnly(t, rotation)
}

func TestRotationCommitPublishesActiveAndClosesPredecessorSubscribers(t *testing.T) {
	fixture := newRotationFixture(t)
	oldKey := journalKey(fixture.previous)
	subscribers := []*unifiedDevSubscriber{fixture.subscriber}
	for index := 0; index < 5; index++ {
		_, _, subscriber, cancel, err := openSnapshotTailForTest(t, fixture.effects, fixture.unit.sessionID)
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
		subscribers = append(subscribers, subscriber)
	}

	rotation := rotationPrepareRotation(t, fixture)
	rotation.commit(t)

	fixture.effects.mu.Lock()
	active := fixture.effects.active[fixture.unit.sessionID]
	fixture.effects.mu.Unlock()
	if active != journalKey(rotation.next) {
		t.Fatalf("active key=%+v want successor %+v", active, journalKey(rotation.next))
	}
	for index, subscriber := range subscribers {
		select {
		case deliveredForTest, open := <-subscriber.events():
			if open {
				subscriber.releaseEvent(deliveredForTest)
			}
			if open {
				t.Fatalf("predecessor subscriber %d remained open", index)
			}
			if reason := subscriber.closeReason(); reason != proto.SubscriberClosedGenerationRotated {
				t.Fatalf("predecessor subscriber %d reason=%q", index, reason)
			}
		case <-time.After(time.Second):
			t.Fatalf("predecessor subscriber %d was not closed", index)
		}
	}
	fixture.effects.subscriberMu.Lock()
	oldSubscribers := len(fixture.effects.subscribers[oldKey])
	fixture.effects.subscriberMu.Unlock()
	if oldSubscribers != 0 {
		t.Fatalf("predecessor subscriber residue: subscribers=%d", oldSubscribers)
	}
	_, _, successor, cancel, err := openSnapshotTailForTest(t, fixture.effects, fixture.unit.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	fixture.effects.subscriberMu.Lock()
	_, onSuccessor := fixture.effects.subscribers[journalKey(rotation.next)][successor]
	fixture.effects.subscriberMu.Unlock()
	if !onSuccessor {
		t.Fatal("post-Commit snapshot did not bind the successor")
	}
}

func TestRotationAttachCannotObserveCommitInterior(t *testing.T) {
	for _, edge := range []string{"after_install_before_close", "after_close_before_unlock"} {
		t.Run(edge, func(t *testing.T) {
			fixture := newRotationFixture(t)
			rotation := rotationPrepareRotation(t, fixture)
			type attachResult struct {
				subscriber *unifiedDevSubscriber
				cancel     func()
				err        error
			}
			started := make(chan struct{})
			done := make(chan attachResult, 1)
			fixture.effects.rotationCommitEdge = func(got string) {
				if got != edge {
					return
				}
				go func() {
					close(started)
					_, _, subscriber, cancel, err := openSnapshotTailForTest(t, fixture.effects, fixture.unit.sessionID)
					done <- attachResult{subscriber: subscriber, cancel: cancel, err: err}
				}()
				<-started
				select {
				case <-done:
					t.Fatal("attachment observed the active/subscriber swap interior")
				case <-time.After(20 * time.Millisecond):
				}
			}
			rotation.commit(t)
			result := <-done
			if result.err != nil {
				t.Fatal(result.err)
			}
			defer result.cancel()
			fixture.effects.subscriberMu.Lock()
			_, bound := fixture.effects.subscribers[journalKey(rotation.next)][result.subscriber]
			fixture.effects.subscriberMu.Unlock()
			if !bound {
				t.Fatal("attachment did not bind the complete successor state")
			}
		})
	}
}

func TestRotationReapCannotObserveCommitInterior(t *testing.T) {
	for _, edge := range []string{"after_install_before_close", "after_close_before_unlock"} {
		t.Run(edge, func(t *testing.T) {
			fixture := newRotationFixture(t)
			rotation := rotationPrepareRotation(t, fixture)
			var oldDisconnects, successorDisconnects atomic.Int64
			fixture.registry.retention.setHook(func(point string, key unifiedjournal.PaneKey) {
				if point != "after_ingress_reserve" {
					return
				}
				switch key {
				case journalKey(fixture.previous):
					oldDisconnects.Add(1)
				case journalKey(rotation.next):
					successorDisconnects.Add(1)
				}
			})
			started := make(chan struct{})
			done := make(chan struct{})
			fixture.effects.rotationCommitEdge = func(got string) {
				if got != edge {
					return
				}
				go func() {
					close(started)
					fixture.effects.reapFaultedUnitOnce(fixture.unit)
					close(done)
				}()
				<-started
				select {
				case <-done:
					t.Fatal("unit reap observed the active/subscriber swap interior")
				case <-time.After(20 * time.Millisecond):
				}
			}
			rotation.commit(t)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("unit reap remained blocked after Commit")
			}
			if oldDisconnects.Load() != 0 || successorDisconnects.Load() != 1 {
				t.Fatalf("disconnect reservations predecessor=%d successor=%d want 0/1",
					oldDisconnects.Load(), successorDisconnects.Load())
			}
		})
	}
}
