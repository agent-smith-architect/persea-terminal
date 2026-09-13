package broker

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/unifiedjournal"
)

type p2aReview3Ledger struct {
	p, q, e int
	b, o    int64
}

func p2aReview3RuntimeLedger(runtime *retentionTrialRuntime) p2aReview3Ledger {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return p2aReview3Ledger{p: runtime.pUsed, q: runtime.qUsed, e: runtime.eUsed, b: runtime.bUsed, o: runtime.oUsed}
}

func TestP2AReview3AbortRefusedAfterRealSealWithoutMarkSealed(t *testing.T) {
	for _, boundaryError := range []bool{false, true} {
		t.Run(map[bool]string{false: "settled", true: "boundary_error"}[boundaryError], func(t *testing.T) {
			fixture := newP2AFixture(t)
			next := fixture.next(2)
			reservation := fixture.materialize(next)
			txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
			if err != nil {
				t.Fatal(err)
			}
			fixture.reviewBootstrap(txn)
			if err := txn.Validate(); err != nil {
				t.Fatal(err)
			}

			var fail atomic.Bool
			if boundaryError {
				fixture.registry.retention.options.stage = func(operation string, key unifiedjournal.PaneKey) error {
					if fail.Load() && operation == "append" && key == journalKey(fixture.previous) {
						return errors.New("injected predecessor seal flush failure")
					}
					return nil
				}
				if err := fixture.registry.retention.WritePane(journalKey(fixture.previous), []byte("P2A-REVIEW3-SEAL-FLUSH")); err != nil {
					t.Fatal(err)
				}
				fail.Store(true)
			}
			sealed, err := fixture.registry.retention.startBoundary(journalKey(fixture.previous), "rotation_seal", true)
			if err != nil {
				t.Fatal(err)
			}
			boundaryResult := <-sealed
			if boundaryError && !errors.Is(boundaryResult, unifiedjournal.ErrStorage) {
				t.Fatalf("boundary result=%v, want storage failure", boundaryResult)
			}
			if !boundaryError && boundaryResult != nil {
				t.Fatal(boundaryResult)
			}

			// No markSealed call: retention state, not the driver's local bit, is
			// authoritative once the retiring boundary has actually processed.
			if txn.Abort() {
				t.Fatal("Abort settled after the predecessor seal processed")
			}
			if txn.Abort() {
				t.Fatal("second Abort double-settled after the predecessor seal")
			}
			if txn.settled || txn.disposition == paneRotationAborted {
				t.Fatalf("refused Abort claimed rollback: settled=%v disposition=%d", txn.settled, txn.disposition)
			}
			fixture.registry.mu.Lock()
			owned := fixture.registry.rotations[routeCoordinateKey(fixture.previous)] == txn && fixture.registry.rotationGenerations[journalKey(next)] == txn
			fixture.registry.mu.Unlock()
			if !owned {
				t.Fatal("refused Abort removed transaction ownership")
			}
			fixture.registry.retention.mu.Lock()
			predecessorBeforeReap := fixture.registry.retention.generations[journalKey(fixture.previous)]
			panesBeforeReap := fixture.registry.retention.pUsed
			fixture.registry.retention.mu.Unlock()
			fixture.effects.reapFaultedUnitOnce(fixture.unit)
			fixture.registry.retention.mu.Lock()
			predecessorAfterReap := fixture.registry.retention.generations[journalKey(fixture.previous)]
			panesAfterReap := fixture.registry.retention.pUsed
			fixture.registry.retention.mu.Unlock()
			if predecessorAfterReap != nil && predecessorAfterReap != predecessorBeforeReap || panesAfterReap > panesBeforeReap {
				t.Fatalf("fatal reap recreated the sealed predecessor: before=%p/%d after=%p/%d", predecessorBeforeReap, panesBeforeReap, predecessorAfterReap, panesAfterReap)
			}
			fixture.abortReservation(reservation)
		})
	}
}

func TestP2AReview3AbandonedSuccessorRejectsEveryIngress(t *testing.T) {
	fixture := newP2AFixture(t)
	baseline := p2aReview3RuntimeLedger(fixture.registry.retention)
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var blockOnce sync.Once
	fixture.registry.retention.setHook(func(point string, key unifiedjournal.PaneKey) {
		if point == "before_append" && key == journalKey(next) {
			blockOnce.Do(func() {
				close(entered)
				<-release
			})
		}
	})
	original := []byte("P2A-REVIEW3-ORIGINAL-WRITER")
	if err := fixture.registry.retention.WritePane(journalKey(next), original); err != nil {
		t.Fatal(err)
	}
	flushDone := make(chan error, 1)
	go func() { flushDone <- fixture.registry.retention.Boundary(journalKey(next), "review3_writer_flush") }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("original writer never reached storage")
	}
	if !txn.Abort() {
		t.Fatal("pre-seal Abort was refused")
	}
	retained := p2aReview3RuntimeLedger(fixture.registry.retention)
	if _, _, err := fixture.registry.retention.ensureGeneration(journalKey(next), false); !errors.Is(err, unifiedjournal.ErrInvalidated) {
		t.Errorf("abandoned staging=%v, want ErrInvalidated", err)
	}
	refused := []byte("P2A-REVIEW3-REFUSED-WRITER")
	refusedAccepted := false
	if err := fixture.registry.retention.WritePane(journalKey(next), refused); !errors.Is(err, unifiedjournal.ErrInvalidated) {
		refusedAccepted = err == nil
		t.Errorf("abandoned WritePane=%v, want ErrInvalidated", err)
	}
	if after := p2aReview3RuntimeLedger(fixture.registry.retention); after != retained {
		t.Errorf("refused ingress changed retained ledger: before=%+v after=%+v", retained, after)
	}
	close(release)
	if err := <-flushDone; err != nil {
		t.Fatal(err)
	}
	if refusedAccepted {
		if err := fixture.registry.retention.Boundary(journalKey(next), "review3_refused_writer_drain"); err != nil {
			t.Fatal(err)
		}
	}
	fixture.effects.journalMu.Lock()
	committed, readErr := fixture.effects.realm.ReadCommitted(journalKey(next))
	fixture.effects.journalMu.Unlock()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := bytes.Count(committed, original); got != 1 {
		t.Errorf("original writer count=%d want 1", got)
	}
	if got := bytes.Count(committed, refused); got != 0 {
		t.Errorf("refused writer count=%d want 0", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		fixture.registry.retention.mu.Lock()
		_, present := fixture.registry.retention.generations[journalKey(next)]
		fixture.registry.retention.mu.Unlock()
		if !present {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("abandoned successor did not retire after its original writer completed")
		}
		time.Sleep(time.Millisecond)
	}
	if settled := p2aReview3RuntimeLedger(fixture.registry.retention); settled != baseline {
		t.Errorf("last-reference retirement ledger=%+v want %+v", settled, baseline)
	}
	fixture.abortReservation(reservation)
}

func p2aReview3CommitRotation(t *testing.T, fixture *p2aFixture, previous, next controlmode.PaneWitness) {
	t.Helper()
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(previous, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := txn.WriteBootstrap([]byte("review3-bootstrap")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.registry.retentionBoundary(next, "rotation_bootstrap"); err != nil {
		t.Fatal(err)
	}
	if err := txn.Validate(); err != nil {
		t.Fatal(err)
	}
	sealed, err := fixture.registry.retention.startBoundary(journalKey(previous), "rotation_seal", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-sealed; err != nil {
		t.Fatal(err)
	}
	txn.markSealed()
	if disposition := txn.Commit(); disposition != paneRotationCommitted {
		t.Fatalf("Commit disposition=%d", disposition)
	}
	fixture.effects.journalMu.Lock()
	reservation.Commit()
	// The production rotation reservation owns this unlink; this fixture
	// uses an adoption reservation to isolate registry behavior.
	fixture.effects.realm.RetirePane(journalKey(previous))
	fixture.effects.journalMu.Unlock()
	fixture.effects.mu.Lock()
	fixture.effects.active[fixture.unit.sessionID] = journalKey(next)
	fixture.effects.mu.Unlock()
}

func TestP2AReview3ReapAfterRotationLeavesNoRetiredKeyResidue(t *testing.T) {
	fixture := newP2AFixture(t)
	first := fixture.next(2)
	second := fixture.next(3)
	p2aReview3CommitRotation(t, fixture, fixture.previous, first)
	p2aReview3CommitRotation(t, fixture, first, second)

	var baseline p2aReview3Ledger
	deadline := time.Now().Add(2 * time.Second)
	for {
		baseline = p2aReview3RuntimeLedger(fixture.registry.retention)
		if baseline == (p2aReview3Ledger{p: 1, q: 2}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rotation accounting did not settle to one active generation: %+v", baseline)
		}
		time.Sleep(time.Millisecond)
	}
	counts := make(map[unifiedjournal.PaneKey]int)
	var countsMu sync.Mutex
	fixture.registry.retention.setHook(func(point string, key unifiedjournal.PaneKey) {
		if point == "after_ingress_reserve" {
			countsMu.Lock()
			counts[key]++
			countsMu.Unlock()
		}
	})
	reap := func(unit *unifiedDevUnit) {
		fixture.effects.reapFaultedUnitOnce(unit)
		deadline := time.Now().Add(2 * time.Second)
		for {
			got := p2aReview3RuntimeLedger(fixture.registry.retention)
			if got == baseline {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("reap ledger=%+v want %+v", got, baseline)
			}
			time.Sleep(time.Millisecond)
		}
		fixture.registry.retention.mu.Lock()
		_, oldPresent := fixture.registry.retention.generations[journalKey(fixture.previous)]
		_, firstPresent := fixture.registry.retention.generations[journalKey(first)]
		_, activePresent := fixture.registry.retention.generations[journalKey(second)]
		fixture.registry.retention.mu.Unlock()
		if oldPresent || firstPresent || !activePresent {
			t.Fatalf("reap generations: oldest=%v predecessor=%v active=%v", oldPresent, firstPresent, activePresent)
		}
	}
	reap(fixture.unit)
	for index := 0; index < 64; index++ {
		unit := &unifiedDevUnit{
			owner: fixture.effects.UnifiedDevPaneEffects, sessionID: fixture.unit.sessionID,
			done: make(chan struct{}), witnesses: append([]controlmode.PaneWitness(nil), fixture.unit.witnesses...),
		}
		fixture.effects.mu.Lock()
		fixture.effects.units[unit.sessionID] = unit
		fixture.effects.active[unit.sessionID] = journalKey(second)
		fixture.effects.mu.Unlock()
		reap(unit)
	}
	countsMu.Lock()
	oldCount, firstCount, activeCount := counts[journalKey(fixture.previous)], counts[journalKey(first)], counts[journalKey(second)]
	countsMu.Unlock()
	// Only the first unit death owns an eligible active route. The 64 synthetic
	// units installed below deliberately do not re-admit that route, so R14
	// requires their historical Disconnects to remain zero-reservation no-ops.
	if oldCount != 0 || firstCount != 0 || activeCount != 1 {
		t.Fatalf("disconnect reserve counts: oldest=%d predecessor=%d active=%d want 0/0/1", oldCount, firstCount, activeCount)
	}
}

func TestP2AReview3ExplicitFaultDisconnectsCurrentGeneration(t *testing.T) {
	fixture := newP2AFixture(t)
	baseline := p2aReview3RuntimeLedger(fixture.registry.retention)
	key := journalKey(fixture.previous)
	var reservations atomic.Int64
	fixture.registry.retention.setHook(func(point string, got unifiedjournal.PaneKey) {
		if point == "after_ingress_reserve" && got == key {
			reservations.Add(1)
		}
	})
	fixture.effects.reapFaultedUnitOnce(fixture.unit)
	if got := reservations.Load(); got != 1 {
		t.Fatalf("ordinary active-generation Disconnect reservations=%d want 1", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := p2aReview3RuntimeLedger(fixture.registry.retention); got == baseline {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("ordinary reap ledger=%+v want %+v", got, baseline)
		}
		time.Sleep(time.Millisecond)
	}
	fixture.effects.mu.Lock()
	_, unitLive := fixture.effects.units[fixture.unit.sessionID]
	_, active := fixture.effects.active[fixture.unit.sessionID]
	fixture.effects.mu.Unlock()
	if unitLive || active {
		t.Fatalf("ordinary reap retained provider authority: unit=%v active=%v", unitLive, active)
	}
}
