package broker

// Provenance: Fable re-check of M11 P2a candidate ffb3c32
// (docs: 2026-08-27_p2a_adjudication.md, "Re-check (ffb3c32)"). Independent
// slot-conservation scenario across rotations, unit death and re-adoption;
// the 7b→7c reap interval; reap of a never-activated unit; R10 ingress pins.

import (
	"errors"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/unifiedjournal"
)

// review4Rotate drives the frozen mainline (§3.3 7-pre..7d) for the fixture's
// current witness and returns the committed successor witness. swapActive is
// retained for the independent review fixture's original call shape; the
// corrected Commit now owns active-key publication atomically, so the extra
// assignment is deliberately idempotent.
func (fixture *p2aFixture) review4Rotate(t *testing.T, previous controlmode.PaneWitness, generation uint64, swapActive bool) controlmode.PaneWitness {
	t.Helper()
	next := previous
	next.Session.ControlGeneration = generation
	reserveRecordingSourceForTest(t, fixture.effects.UnifiedDevPaneEffects, journalKey(next))
	fixture.effects.journalMu.Lock()
	reservation, err := fixture.effects.realm.BeginReconstructedPane(journalKey(next), unifiedjournal.Geometry{Columns: 80, Rows: 24})
	fixture.effects.journalMu.Unlock()
	if err != nil {
		t.Fatalf("materialize %d: %v", generation, err)
	}
	txn, err := fixture.registry.BeginPaneRotation(previous, next)
	if err != nil {
		t.Fatalf("begin rotation %d->%d: %v", previous.Session.ControlGeneration, generation, err)
	}
	if err := txn.WriteBootstrap([]byte("bootstrap")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.registry.retentionBoundary(next, "rotation_bootstrap"); err != nil {
		t.Fatal(err)
	}
	if err := txn.Validate(); err != nil {
		t.Fatalf("validate %d: %v", generation, err)
	}
	sealed, err := fixture.registry.retention.startBoundary(journalKey(previous), "rotation_seal", true)
	if err != nil {
		t.Fatalf("seal %d: %v", generation, err)
	}
	select {
	case err := <-sealed:
		if err != nil {
			t.Fatalf("seal %d: %v", generation, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("seal did not complete")
	}
	txn.markSealed()
	if disposition := txn.Commit(); disposition != paneRotationCommitted {
		t.Fatalf("commit %d disposition=%d", generation, disposition)
	}
	fixture.effects.journalMu.Lock()
	reservation.Commit()
	// This registry-only fixture uses an adoption reservation. Production's
	// RotationReservation.Commit owns the predecessor's proven retirement.
	fixture.effects.realm.RetirePane(journalKey(previous))
	fixture.effects.journalMu.Unlock()
	if swapActive {
		fixture.effects.mu.Lock()
		fixture.effects.active[fixture.unit.sessionID] = journalKey(next)
		fixture.effects.mu.Unlock()
	}
	return next
}

func (fixture *p2aFixture) review4Retention() (generations, pUsed, qUsed int) {
	fixture.registry.retention.mu.Lock()
	defer fixture.registry.retention.mu.Unlock()
	return len(fixture.registry.retention.generations), fixture.registry.retention.pUsed, fixture.registry.retention.qUsed
}

func (fixture *p2aFixture) review4WaitRetention(want [3]int) [3]int {
	deadline := time.Now().Add(2 * time.Second)
	for {
		g, p, q := fixture.review4Retention()
		got := [3]int{g, p, q}
		if got == want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

// review4Revive installs a fresh unit for the session on its current active
// witness (the builder's re-reap shape, but the following rotations mint new
// generation numbers rather than re-reaping one fixed key).
func (fixture *p2aFixture) review4Revive(witness controlmode.PaneWitness) {
	base := fixture.effects.UnifiedDevPaneEffects
	unit := &unifiedDevUnit{owner: base, sessionID: witness.Session.Session, done: make(chan struct{}), witnesses: []controlmode.PaneWitness{witness}}
	base.mu.Lock()
	base.units[unit.sessionID] = unit
	base.active[unit.sessionID] = journalKey(witness)
	base.mu.Unlock()
	fixture.unit = unit
}

// review4Readopt models the ordinary lifecycle after a unit death: a new unit
// for the same session with a freshly minted control generation on the same
// physical pane, admitted through the registry like birth/adoption.
func (fixture *p2aFixture) review4Readopt(t *testing.T, previous controlmode.PaneWitness, generation uint64) controlmode.PaneWitness {
	t.Helper()
	witness := previous
	witness.Session.ControlGeneration = generation
	base := fixture.effects.UnifiedDevPaneEffects
	// The fixture's Disconnect preserves historical runtime accounting. The
	// source is nevertheless dead, so settle its durable journal as the real
	// owner-reap path does before admitting the replacement capture.
	base.journalMu.Lock()
	base.realm.RetirePane(journalKey(previous))
	base.journalMu.Unlock()
	reserveRecordingSourceForTest(t, base, journalKey(witness))
	base.journalMu.Lock()
	err := base.realm.AdmitPane(journalKey(witness), unifiedjournal.Geometry{Columns: 80, Rows: 24})
	base.journalMu.Unlock()
	if err != nil {
		t.Fatalf("readopt journal %d: %v", generation, err)
	}
	if err := fixture.registry.AdmitPane(witness); err != nil {
		t.Fatalf("readopt registry %d: %v", generation, err)
	}
	commitRecordingInitialForTest(t, fixture.registry, witness, nil)
	fixture.review4Revive(witness)
	return witness
}

func (fixture *p2aFixture) review4Die() {
	close(fixture.unit.done)
	fixture.effects.reapFaultedUnit(fixture.unit)
}

// review4CountReserves records the builder's own ingress measurement point.
func (fixture *p2aFixture) review4CountReserves() (func(unifiedjournal.PaneKey) int, func()) {
	counts := make(map[unifiedjournal.PaneKey]int)
	var mu sync.Mutex
	fixture.registry.retention.setHook(func(point string, key unifiedjournal.PaneKey) {
		if point == "after_ingress_reserve" {
			mu.Lock()
			counts[key]++
			mu.Unlock()
		}
	})
	return func(key unifiedjournal.PaneKey) int {
			mu.Lock()
			defer mu.Unlock()
			return counts[key]
		}, func() {
			fixture.registry.retention.setHook(nil)
		}
}

// Slot conservation over the lifecycle P2b will produce: the session is
// re-adopted with a fresh control generation, rotated twice, and its unit
// dies — 64 times with monotonically fresh generation numbers. Re-admission
// of a fresh generation is base behaviour (see the diagnostic below), so the
// pin is the rotation-attributable delta: from the snapshot taken right after
// each re-adoption, two rotations plus the death must return the ledger to
// exactly that snapshot, with one Disconnect on the active successor and none
// on either retired predecessor.
func TestP2AReview4SlotsConservedAcrossRotationsAndDeaths(t *testing.T) {
	savedCaps := unifiedJournalCaps
	unifiedJournalCaps.panePhysical = 64 << 20
	unifiedJournalCaps.realmPhysical = 1 << 30
	t.Cleanup(func() { unifiedJournalCaps = savedCaps })
	fixture := newP2AFixtureWithSlots(t, 256)
	fixture.registry.retention.mu.Lock()
	fixture.registry.retention.options.maxPanes = 256
	fixture.registry.retention.mu.Unlock()
	witness := fixture.previous
	generation := uint64(1)
	count, stop := fixture.review4CountReserves()
	defer stop()
	for repetition := 0; repetition < 64; repetition++ {
		if repetition > 0 {
			generation++
			witness = fixture.review4Readopt(t, witness, generation)
		}
		g, p, q := fixture.review4Retention()
		snapshot := [3]int{g, p, q}
		predecessor := witness
		witness = fixture.review4Rotate(t, witness, generation+1, true)
		middle := witness
		witness = fixture.review4Rotate(t, witness, generation+2, true)
		generation += 2
		if got := fixture.review4WaitRetention(snapshot); got != snapshot {
			t.Fatalf("repetition %d after two rotations: retention=%v want snapshot %v", repetition, got, snapshot)
		}
		fixture.review4Die()
		if got := fixture.review4WaitRetention(snapshot); got != snapshot {
			t.Fatalf("repetition %d after unit death: retention=%v want snapshot %v (rotation slot leak)", repetition, got, snapshot)
		}
		if count(journalKey(predecessor)) != 0 || count(journalKey(middle)) != 0 || count(journalKey(witness)) != 1 {
			t.Fatalf("repetition %d disconnect reserves: predecessor=%d middle=%d active=%d want 0/0/1",
				repetition, count(journalKey(predecessor)), count(journalKey(middle)), count(journalKey(witness)))
		}
		fixture.registry.mu.Lock()
		broken := fixture.registry.broken
		fixture.registry.mu.Unlock()
		if broken {
			t.Fatalf("repetition %d: realm breaker tripped", repetition)
		}
	}
	g, p, q := fixture.review4Retention()
	t.Logf("after 64 repetitions (63 fresh-generation re-adoptions): generations=%d pUsed=%d qUsed=%d", g, p, q)
}

// Diagnostic: re-admitting the same session on the same pane with a fresh
// control generation after a death, with and without a rotation in between.
// Growth that appears in both columns belongs to the base admission path
// (a Disconnect submits a retire=false "reconnect" boundary and AdmitPane of
// a fresh generation never retires the dead one — neither is in P2a's diff);
// growth that appears only with rotation would be P2a's.
func TestP2AReview4ReadoptionWithFreshGenerationAccounting(t *testing.T) {
	growth := map[bool][3]int{}
	for _, rotate := range []bool{false, true} {
		fixture := newP2AFixture(t)
		witness := fixture.previous
		generation := uint64(1)
		var history [][3]int
		for repetition := 0; repetition < 4; repetition++ {
			if rotate {
				witness = fixture.review4Rotate(t, witness, generation+1, true)
				generation++
			}
			fixture.review4Die()
			generation++
			witness = fixture.review4Readopt(t, witness, generation)
			g, p, q := fixture.review4Retention()
			history = append(history, [3]int{g, p, q})
		}
		t.Logf("rotate=%v retention after each readoption (generations,pUsed,qUsed): %v", rotate, history)
		growth[rotate] = history[len(history)-1]
		if rotate {
			// A retiring predecessor's final reference is settled by the retention
			// dispatcher. Compare the quiescent accounting shape, not a transient
			// snapshot taken between boundary completion and reference release.
			growth[rotate] = fixture.review4WaitRetention(growth[false])
		}
	}
	if growth[true] != growth[false] {
		t.Fatalf("rotation changes re-adoption accounting: with rotation %v, without %v", growth[true], growth[false])
	}
}

// The corrected post-Commit state installs the successor witness, admission,
// active key and predecessor-subscriber close atomically. A unit death at the
// first observable point after Commit must disconnect the successor route.
func TestP2AReview4ReapBetweenCommitAndActiveSwapDisconnectsSuccessor(t *testing.T) {
	fixture := newP2AFixture(t)
	count, stop := fixture.review4CountReserves()
	defer stop()
	successor := fixture.review4Rotate(t, fixture.previous, 2, false)
	fixture.review4Die()
	fixture.registry.mu.Lock()
	_, admitted := fixture.registry.admitted[routeCoordinateKey(successor)]
	session := fixture.registry.sessions[sessionKey(successor.Session)]
	routable := session != nil && session.router.UnifiedEligible(successor)
	fixture.registry.mu.Unlock()
	t.Logf("reap before active swap: successor disconnect reserves=%d predecessor=%d admitted=%v routable=%v",
		count(journalKey(successor)), count(journalKey(fixture.previous)), admitted, routable)
	if count(journalKey(successor)) != 1 {
		t.Fatalf("successor received %d Disconnects from the reap, want 1 (reap keyed on stale effects.active)", count(journalKey(successor)))
	}
}

// A unit that dies after registry admission but before it became the
// session's active key (birth: AdmitPane precedes CommitSessionBirth's active
// assignment). Before ffb3c32 the reap disconnected every witness; now only
// the captured active key. The admitted route must not be left routable with
// no unit behind it.
func TestP2AReview4ReapOfNeverActivatedUnitDisconnectsItsAdmission(t *testing.T) {
	fixture := newP2AFixture(t)
	count, stop := fixture.review4CountReserves()
	defer stop()
	base := fixture.effects.UnifiedDevPaneEffects
	base.mu.Lock()
	delete(base.active, fixture.unit.sessionID)
	base.mu.Unlock()
	fixture.review4Die()
	fixture.registry.mu.Lock()
	session := fixture.registry.sessions[sessionKey(fixture.previous.Session)]
	_, admitted := fixture.registry.admitted[routeCoordinateKey(fixture.previous)]
	routable := session != nil && session.router.UnifiedEligible(fixture.previous)
	fixture.registry.mu.Unlock()
	t.Logf("never-activated unit reap: disconnect reserves=%d admitted=%v routable=%v", count(journalKey(fixture.previous)), admitted, routable)
	if count(journalKey(fixture.previous)) != 1 {
		t.Fatal("reap of a never-activated unit emitted no Disconnect for its admitted witness (Disconnect suppressed)")
	}
}

// R10 pins: after a pre-seal Abort with the bootstrap writer still holding a
// reference, no new staging, reservation or write may attach to the abandoned
// successor, and it retires to baseline once the writer settles.
func TestP2AReview4AbandonedSuccessorRefusesAllIngress(t *testing.T) {
	fixture := newP2AFixture(t)
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
		if _, _, refs := fixture.reviewSuccessorGeneration(next); refs > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bootstrap write never took a successor reference")
		}
		time.Sleep(time.Millisecond)
	}
	if !txn.Abort() {
		t.Fatal("pre-seal Abort refused")
	}
	_, _, ensureErr := fixture.registry.retention.ensureGeneration(journalKey(next), false)
	_, reserveErr := fixture.registry.retention.reserve(journalKey(next), 4)
	writeErr := fixture.registry.retention.WritePane(journalKey(next), []byte("late"))
	_, beginErr := fixture.registry.BeginPaneRotation(fixture.previous, next)
	releaseOnce.Do(func() { close(release) })
	fixture.effects.mu.Lock()
	fixture.effects.lockProbe = nil
	fixture.effects.mu.Unlock()
	fixture.waitSuccessorQuiescent(next)
	got := fixture.review4WaitRetention([3]int{1, 1, 2})
	for name, err := range map[string]error{"ensureGeneration": ensureErr, "reserve": reserveErr, "WritePane": writeErr} {
		if !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Errorf("%s on abandoned successor = %v, want ErrInvalidated", name, err)
		}
	}
	if !errors.Is(beginErr, ErrPaneRotationInvalid) {
		t.Errorf("Begin on abandoned successor key = %v", beginErr)
	}
	if got != [3]int{1, 1, 2} {
		t.Fatalf("retention after abandoned successor settled = %v, want [1 1 2]", got)
	}
	fixture.abortReservation(reservation)
}

// After a unit death the session comes back with a fresh control generation
// (birth/adoption always mint one). The dead generation's admitted entry is
// never deleted by a Disconnect, so a single-route predicate that counts every
// admitted entry of the session — live or dead — refuses every later rotation
// of that session for the rest of the broker's life.
func TestP2AReview4RotationStillPossibleAfterDeathAndReadoption(t *testing.T) {
	fixture := newP2AFixture(t)
	fixture.review4Die()
	witness := fixture.review4Readopt(t, fixture.previous, 2)
	next := witness
	next.Session.ControlGeneration = 3
	reservation := fixture.materialize(next)
	defer fixture.abortReservation(reservation)
	txn, err := fixture.registry.BeginPaneRotation(witness, next)
	fixture.registry.mu.Lock()
	session := fixture.registry.sessions[sessionKey(witness.Session)]
	type entry struct {
		generation uint64
		eligible   bool
	}
	var entries []entry
	for _, state := range fixture.registry.admitted {
		if sessionKey(state.witness.Session) == sessionKey(witness.Session) {
			entries = append(entries, entry{state.witness.Session.ControlGeneration, session != nil && session.router.UnifiedEligible(state.witness)})
		}
	}
	fixture.registry.mu.Unlock()
	t.Logf("admitted entries for the session after death+readoption (generation, live in current router): %v", entries)
	if err != nil {
		t.Fatalf("rotation after death+readoption refused: %v", err)
	}
	if !txn.Abort() {
		t.Fatal("abort of the probe transaction refused")
	}
}
