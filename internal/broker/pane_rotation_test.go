package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

var errRotationInjected = errors.New("rotation injected failure")

type rotationTestEffects struct {
	*UnifiedDevPaneEffects
	mu        sync.Mutex
	failStage string
	failFeed  bool
	lockProbe func(string)
	observed  chan string
}

func (effects *rotationTestEffects) retentionTrial() retentionTrialOptions {
	options := effects.UnifiedDevPaneEffects.retentionTrial()
	options.stage = func(stage string, key unifiedjournal.PaneKey) error {
		effects.mu.Lock()
		fail := effects.failStage == stage
		probe := effects.lockProbe
		effects.mu.Unlock()
		if probe != nil {
			probe(stage)
		}
		if fail {
			return errRotationInjected
		}
		return nil
	}
	return options
}

func (effects *rotationTestEffects) WritePaneRange(key unifiedjournal.PaneKey, payload []byte, start, end, sequence int64) error {
	effects.mu.Lock()
	fail := effects.failFeed
	effects.mu.Unlock()
	if fail {
		return errRotationInjected
	}
	return effects.UnifiedDevPaneEffects.WritePaneRange(key, payload, start, end, sequence)
}

func (effects *rotationTestEffects) beginPaneRotationWitness(previous, next controlmode.PaneWitness) (paneRotationWitnessStage, error) {
	return effects.UnifiedDevPaneEffects.beginPaneRotationWitness(previous, next)
}

type rotationFixture struct {
	t           *testing.T
	effects     *rotationTestEffects
	registry    *paneRegistry
	previous    controlmode.PaneWitness
	unit        *unifiedDevUnit
	session     *sessionCoordinator
	generation  *retentionGeneration
	coordinator *paneCoordinator
	attachments string
	epoch       *terminal.Epoch
	subscriber  *unifiedDevSubscriber
	cancel      func()
}

func newRotationFixture(t *testing.T) *rotationFixture {
	return newRotationFixtureWithSlots(t, 16)
}

func newRotationFixtureWithSlots(t *testing.T, adoptionSlots int) *rotationFixture {
	t.Helper()
	base := newProjectionEffects(t, adoptionSlots)
	effects := &rotationTestEffects{UnifiedDevPaneEffects: base, observed: make(chan string, 256)}
	base.retentionObserve = func(event string, _ map[string]int64) {
		select {
		case effects.observed <- event:
		default:
		}
	}
	registry := newPaneRegistry(effects)
	base.observer = registry
	t.Cleanup(func() {
		_ = registry.Close()
		_ = base.realm.Close()
	})

	previous := controlmode.PaneWitness{
		Session: controlmode.SessionWitness{Server: "main", Session: "$rotation", ControlGeneration: 1},
		Window:  "@rotation", Pane: "%rotation", Incarnation: "1000,1,0",
	}
	reserveRecordingSourceForTest(t, base, journalKey(previous))
	base.journalMu.Lock()
	err := base.realm.AdmitPane(journalKey(previous), unifiedjournal.Geometry{Columns: 80, Rows: 24})
	base.journalMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.AdmitPane(previous); err != nil {
		t.Fatal(err)
	}
	commitRecordingInitialForTest(t, registry, previous, nil)
	unit := &unifiedDevUnit{
		owner: base, sessionID: previous.Session.Session,
		done: make(chan struct{}), witnesses: []controlmode.PaneWitness{previous},
	}
	base.mu.Lock()
	base.units[unit.sessionID] = unit
	base.active[unit.sessionID] = journalKey(previous)
	base.mu.Unlock()

	epoch := new(terminal.Epoch)
	registry.mu.Lock()
	session := registry.sessions[sessionKey(previous.Session)]
	coordinator := registry.panes[coordinateKey(previous)]
	coordinator.mu.Lock()
	coordinator.attachments[epoch] = struct{}{}
	attachments := fmt.Sprintf("%p", coordinator.attachments)
	coordinator.mu.Unlock()
	registry.mu.Unlock()
	registry.retention.mu.Lock()
	generation := registry.retention.generations[journalKey(previous)]
	registry.retention.mu.Unlock()

	_, _, subscriber, cancel, err := openSnapshotTailForTest(t, base, unit.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	return &rotationFixture{
		t: t, effects: effects, registry: registry, previous: previous, unit: unit,
		session: session, generation: generation,
		coordinator: coordinator, attachments: attachments, epoch: epoch,
		subscriber: subscriber, cancel: cancel,
	}
}

func (fixture *rotationFixture) next(generation uint64) controlmode.PaneWitness {
	next := fixture.previous
	next.Session.ControlGeneration = generation
	return next
}

func (fixture *rotationFixture) materialize(next controlmode.PaneWitness) *unifiedjournal.AdoptionReservation {
	fixture.t.Helper()
	reserveRecordingSourceForTest(fixture.t, fixture.effects.UnifiedDevPaneEffects, journalKey(next))
	fixture.effects.journalMu.Lock()
	reservation, err := fixture.effects.realm.BeginReconstructedPane(
		journalKey(next), unifiedjournal.Geometry{Columns: 80, Rows: 24},
	)
	fixture.effects.journalMu.Unlock()
	if err != nil {
		fixture.t.Fatalf("materialize successor: %v", err)
	}
	return reservation
}

func (fixture *rotationFixture) abortReservation(reservation *unifiedjournal.AdoptionReservation) {
	fixture.t.Helper()
	fixture.effects.journalMu.Lock()
	reservation.Abort()
	fixture.effects.journalMu.Unlock()
}

func (fixture *rotationFixture) boundary(witness controlmode.PaneWitness) error {
	fixture.t.Helper()
	return fixture.registry.retentionBoundary(witness, "rotation_bootstrap")
}

func (fixture *rotationFixture) waitObservation(want string) {
	fixture.t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-fixture.effects.observed:
			if event == want {
				return
			}
		case <-deadline:
			fixture.t.Fatalf("retention observation %q did not arrive", want)
		}
	}
}

func (fixture *rotationFixture) waitSuccessorQuiescent(next controlmode.PaneWitness) {
	fixture.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		fixture.registry.retention.mu.Lock()
		generation := fixture.registry.retention.generations[journalKey(next)]
		quiescent := generation == nil || generation.refs == 0
		fixture.registry.retention.mu.Unlock()
		if quiescent {
			return
		}
		if time.Now().After(deadline) {
			fixture.t.Fatal("successor runtime did not quiesce")
		}
		time.Sleep(time.Millisecond)
	}
}

func (fixture *rotationFixture) assertPredecessorLive(marker string, next controlmode.PaneWitness) {
	fixture.t.Helper()
	payload := []byte(marker)
	if err := fixture.registry.ObservePane(controlmode.Observation{
		Kind: controlmode.ObservationOutput, Witness: fixture.previous, Data: payload,
	}); err != nil {
		fixture.t.Fatalf("predecessor output: %v", err)
	}
	if err := fixture.registry.retentionBoundary(fixture.previous, "rotation_predecessor_probe"); err != nil {
		fixture.t.Fatalf("predecessor boundary: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event, ok := <-fixture.subscriber.events():
			if ok {
				fixture.subscriber.releaseEvent(event)
			}
			if !ok {
				fixture.t.Fatal("predecessor subscriber closed")
			}
			if event.Kind == unifiedjournal.RecordOutput && bytes.Equal(event.Payload, payload) {
				goto delivered
			}
		case <-deadline:
			fixture.t.Fatal("predecessor output was not delivered")
		}
	}

delivered:
	fixture.effects.journalMu.Lock()
	committed, err := fixture.effects.realm.ReadCommitted(journalKey(fixture.previous))
	fixture.effects.journalMu.Unlock()
	if err != nil || !bytes.Contains(committed, payload) {
		fixture.t.Fatalf("predecessor output not journaled: bytes=%q err=%v", committed, err)
	}

	fixture.registry.mu.Lock()
	session := fixture.registry.sessions[sessionKey(fixture.previous.Session)]
	oldAdmission, oldAdmitted := fixture.registry.admitted[routeCoordinateKey(fixture.previous)]
	_, newAdmitted := fixture.registry.admitted[routeCoordinateKey(next)]
	coordinator := fixture.registry.panes[coordinateKey(fixture.previous)]
	fixture.registry.mu.Unlock()
	if session != fixture.session || session == nil || !session.router.UnifiedEligible(fixture.previous) || session.router.UnifiedEligible(next) {
		fixture.t.Fatal("abort changed the predecessor router")
	}
	if !oldAdmitted || oldAdmission.witness != fixture.previous || newAdmitted {
		fixture.t.Fatal("abort changed admission authority")
	}
	if coordinator != fixture.coordinator {
		fixture.t.Fatal("abort replaced the shared coordinator")
	}
	coordinator.mu.Lock()
	attachmentIdentity := fmt.Sprintf("%p", coordinator.attachments)
	_, attached := coordinator.attachments[fixture.epoch]
	coordinator.mu.Unlock()
	if attachmentIdentity != fixture.attachments || !attached {
		fixture.t.Fatal("abort changed the shared attachment set")
	}

	fixture.registry.retention.mu.Lock()
	oldGeneration, oldRuntime := fixture.registry.retention.generations[journalKey(fixture.previous)]
	_, newRuntime := fixture.registry.retention.generations[journalKey(next)]
	generationCount := len(fixture.registry.retention.generations)
	fixture.registry.retention.mu.Unlock()
	if !oldRuntime || oldGeneration != fixture.generation || newRuntime || generationCount != 1 {
		fixture.t.Fatalf("abort runtime generations: old=%v new=%v count=%d", oldRuntime, newRuntime, generationCount)
	}
	if !slices.Equal(fixture.unit.witnesses, []controlmode.PaneWitness{fixture.previous}) {
		fixture.t.Fatalf("abort leaked staged witness: %#v", fixture.unit.witnesses)
	}
}

func (fixture *rotationFixture) requireRetry(generation uint64) {
	fixture.t.Helper()
	next := fixture.next(generation)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		fixture.t.Fatalf("retry begin: %v", err)
	}
	if err := txn.WriteBootstrap([]byte("retry-bootstrap")); err != nil {
		fixture.t.Fatalf("retry bootstrap: %v", err)
	}
	if err := fixture.boundary(next); err != nil {
		fixture.t.Fatalf("retry boundary: %v", err)
	}
	if err := txn.Validate(); err != nil {
		fixture.t.Fatalf("retry validate: %v", err)
	}
	if disposition := txn.Commit(); disposition != paneRotationCommitted {
		fixture.t.Fatalf("retry commit disposition=%d", disposition)
	}
	fixture.effects.journalMu.Lock()
	reservation.Commit()
	fixture.effects.journalMu.Unlock()

	fixture.registry.mu.Lock()
	session := fixture.registry.sessions[sessionKey(next.Session)]
	_, oldAdmitted := fixture.registry.admitted[routeCoordinateKey(fixture.previous)]
	newAdmission, newAdmitted := fixture.registry.admitted[routeCoordinateKey(next)]
	coordinator := fixture.registry.panes[coordinateKey(next)]
	fixture.registry.mu.Unlock()
	if session == nil || !session.router.UnifiedEligible(next) || oldAdmitted || !newAdmitted || newAdmission.witness != next {
		fixture.t.Fatal("retry did not install exactly the successor authority")
	}
	if coordinator != fixture.coordinator {
		fixture.t.Fatal("retry replaced the shared coordinator")
	}
	if !slices.Equal(fixture.unit.witnesses, []controlmode.PaneWitness{fixture.previous, next}) {
		fixture.t.Fatalf("commit did not install the staged witness: %#v", fixture.unit.witnesses)
	}
}

func TestPaneRotationN9FailureMatrixPreservesPredecessor(t *testing.T) {
	checkpoints := []string{"provisional_creation", "bootstrap_append", "bootstrap_sync", "bootstrap_publication", "bootstrap_boundary"}
	for _, checkpoint := range checkpoints {
		for _, unitDeath := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/death=%v", checkpoint, unitDeath), func(t *testing.T) {
				fixture := newRotationFixture(t)
				next := fixture.next(2)
				reservation := fixture.materialize(next)
				txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
				if err != nil {
					t.Fatal(err)
				}

				switch checkpoint {
				case "provisional_creation":
					// The injected failure is immediately after Begin has created
					// both provisional identities and before bootstrap I/O.
				case "bootstrap_append", "bootstrap_sync", "bootstrap_publication", "bootstrap_boundary":
					fixture.effects.mu.Lock()
					switch checkpoint {
					case "bootstrap_append":
						fixture.effects.failStage = "append"
					case "bootstrap_sync":
						fixture.effects.failStage = "sync"
					case "bootstrap_publication":
						fixture.effects.failFeed = true
					}
					fixture.effects.mu.Unlock()
					if err := txn.WriteBootstrap([]byte("bootstrap-" + checkpoint)); err != nil {
						t.Fatalf("enqueue bootstrap: %v", err)
					}
					boundaryErr := fixture.boundary(next)
					switch checkpoint {
					case "bootstrap_append", "bootstrap_sync":
						// A later generic boundary may settle an empty queue. The
						// exact initial operation must retain the earlier failure.
						if _, initialErr := txn.initial.result(); initialErr == nil {
							t.Fatalf("injected %s produced a successful initial receipt", checkpoint)
						}
						fixture.waitObservation("storage_fault")
					case "bootstrap_publication":
						fixture.waitObservation("storage_fault")
					case "bootstrap_boundary":
						if boundaryErr != nil {
							t.Fatalf("reach bootstrap boundary: %v", boundaryErr)
						}
						fixture.waitObservation("feed_complete")
					}
				}
				fixture.effects.mu.Lock()
				fixture.effects.failStage = ""
				fixture.effects.failFeed = false
				fixture.effects.mu.Unlock()
				var releaseReap chan struct{}
				var reaped chan struct{}
				var reapSnapshot []controlmode.PaneWitness
				if unitDeath {
					reapEntered := make(chan struct{})
					releaseReap = make(chan struct{})
					reaped = make(chan struct{})
					close(fixture.unit.done)
					fixture.effects.unitReapEdge = func(_ *unifiedDevUnit, witnesses []controlmode.PaneWitness) {
						reapSnapshot = append([]controlmode.PaneWitness(nil), witnesses...)
						close(reapEntered)
						<-releaseReap
					}
					go func() {
						fixture.effects.reapUnitContext(context.Background(), fixture.unit)
						close(reaped)
					}()
					<-reapEntered
				}
				txn.Abort()
				fixture.abortReservation(reservation)
				fixture.assertPredecessorLive("OLD-"+checkpoint, next)
				if unitDeath {
					close(releaseReap)
					<-reaped
					fixture.effects.mu.Lock()
					_, unitLive := fixture.effects.units[fixture.unit.sessionID]
					_, active := fixture.effects.active[fixture.unit.sessionID]
					fixture.effects.mu.Unlock()
					if unitLive || active || !slices.Equal(reapSnapshot, []controlmode.PaneWitness{fixture.previous}) {
						t.Fatal("unit-death abort did not return the session to adoptable")
					}
					// A fresh observer fixture proves retry is still possible after
					// the ordinary death/reap lifecycle.
					newRotationFixture(t).requireRetry(3)
				} else {
					fixture.requireRetry(3)
				}
			})
		}
	}
}

func TestPaneRotationAbortWithLiveSuccessorReferenceIsolatesFaultAndRefundsLifetime(t *testing.T) {
	fixture := newRotationFixture(t)
	next := fixture.next(2)
	runtime := fixture.registry.retention
	runtime.mu.Lock()
	beforeP, beforeQ, beforeE, beforeB, beforeO := runtime.pUsed, runtime.qUsed, runtime.eUsed, runtime.bUsed, runtime.oUsed
	runtime.mu.Unlock()

	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}
	held, err := runtime.reserve(journalKey(next), 1)
	if err != nil {
		t.Fatalf("hold successor reference: %v", err)
	}

	// Pause the authoritative fault after it announces intent but before it can
	// acquire registry.mu. Abort must leave enough successor-local authority for
	// that exact racing fault, without retaining transaction ownership.
	faultEntered := make(chan struct{})
	allowFault := make(chan struct{})
	runtime.setHook(func(point string, key unifiedjournal.PaneKey) {
		if point == "before_fault_cas" && key == journalKey(next) {
			close(faultEntered)
			<-allowFault
		}
	})
	type faultResult struct {
		winner  bool
		cleanup *retentionCommand
	}
	faulted := make(chan faultResult, 1)
	go func() {
		winner, cleanup := runtime.classifyFault(journalKey(next), "rotation_abort_race", errRotationInjected)
		faulted <- faultResult{winner: winner, cleanup: cleanup}
	}()
	<-faultEntered
	txn.Abort()
	if disposition := txn.Commit(); disposition == paneRotationCommitFatal || disposition == paneRotationCommitted {
		t.Fatalf("Commit after Abort disposition=%d, want distinct aborted settlement", disposition)
	}
	close(allowFault)
	result := <-faulted
	if !result.winner || result.cleanup == nil {
		t.Fatalf("racing successor fault winner=%v cleanup=%v", result.winner, result.cleanup != nil)
	}
	runtime.appendCleanup(result.cleanup)

	fixture.registry.mu.Lock()
	broken := fixture.registry.broken
	_, predecessorAdmitted := fixture.registry.admitted[routeCoordinateKey(fixture.previous)]
	rotations := len(fixture.registry.rotations)
	ownedGenerations := len(fixture.registry.rotationGenerations)
	fixture.registry.mu.Unlock()
	if broken || !predecessorAdmitted || rotations != 0 || ownedGenerations != 0 {
		t.Fatalf("Abort fault isolation: broken=%v predecessor=%v rotations=%d generations=%d", broken, predecessorAdmitted, rotations, ownedGenerations)
	}
	runtime.mu.Lock()
	successor := runtime.generations[journalKey(next)]
	abandoned := successor != nil && !successor.admitted && successor.retireRequested && successor.refs == 1
	runtime.mu.Unlock()
	if !abandoned {
		t.Fatal("Abort did not retain one referenced successor as isolated retirement authority")
	}

	runtime.cancelReservation(held)
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.mu.Lock()
		_, present := runtime.generations[journalKey(next)]
		p, q, e, b, o := runtime.pUsed, runtime.qUsed, runtime.eUsed, runtime.bUsed, runtime.oUsed
		runtime.mu.Unlock()
		if !present {
			if p != beforeP || q != beforeQ || e != beforeE || b != beforeB || o != beforeO {
				t.Fatalf("abandoned successor ledger: got P/Q/E/B/O=%d/%d/%d/%d/%d want %d/%d/%d/%d/%d", p, q, e, b, o, beforeP, beforeQ, beforeE, beforeB, beforeO)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("abandoned successor did not retire after its last reference and cleanup")
		}
		time.Sleep(time.Millisecond)
	}
	fixture.assertPredecessorLive("abort-race-predecessor", next)
}

func TestPaneRotationBeginRejectsProviderSiblingWithoutAllocatingSuccessor(t *testing.T) {
	fixture := newRotationFixture(t)
	next := fixture.next(2)
	sibling := fixture.previous
	sibling.Pane = "%rotation-provider-sibling"
	sibling.Incarnation = "1000,3,0"
	fixture.effects.mu.Lock()
	fixture.unit.witnesses = append(fixture.unit.witnesses, sibling)
	fixture.effects.mu.Unlock()
	fixture.registry.retention.mu.Lock()
	beforeP, beforeQ := fixture.registry.retention.pUsed, fixture.registry.retention.qUsed
	fixture.registry.retention.mu.Unlock()

	if txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next); txn != nil || !errors.Is(err, ErrPaneRotationInvalid) {
		t.Fatalf("Begin with provider sibling = (%v, %v), want typed refusal", txn, err)
	}
	fixture.registry.mu.Lock()
	rotations := len(fixture.registry.rotations)
	ownedGenerations := len(fixture.registry.rotationGenerations)
	fixture.registry.mu.Unlock()
	fixture.registry.retention.mu.Lock()
	_, successorPresent := fixture.registry.retention.generations[journalKey(next)]
	afterP, afterQ := fixture.registry.retention.pUsed, fixture.registry.retention.qUsed
	fixture.registry.retention.mu.Unlock()
	if rotations != 0 || ownedGenerations != 0 || successorPresent || afterP != beforeP || afterQ != beforeQ {
		t.Fatalf("provider-sibling refusal residue: rotations=%d owned=%d successor=%v P/Q=%d/%d want %d/%d", rotations, ownedGenerations, successorPresent, afterP, afterQ, beforeP, beforeQ)
	}
}

func TestPaneRotationHistoricalWitnessDoesNotBlockNextSinglePaneRotation(t *testing.T) {
	fixture := newRotationFixture(t)
	second := fixture.next(2)
	secondReservation := fixture.materialize(second)
	first, err := fixture.registry.BeginPaneRotation(fixture.previous, second)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.WriteBootstrap([]byte("second-generation")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.boundary(second); err != nil {
		t.Fatal(err)
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
	if disposition := first.Commit(); disposition != paneRotationCommitted {
		t.Fatalf("first rotation disposition=%d", disposition)
	}
	fixture.effects.journalMu.Lock()
	secondReservation.Commit()
	// Production RotationReservation.Commit retires its predecessor; this
	// registry fixture materialized via an adoption reservation instead.
	fixture.effects.realm.RetirePane(journalKey(fixture.previous))
	fixture.effects.journalMu.Unlock()
	fixture.effects.mu.Lock()
	fixture.effects.active[fixture.unit.sessionID] = journalKey(second)
	fixture.effects.mu.Unlock()

	third := second
	third.Session.ControlGeneration = 3
	thirdReservation := fixture.materialize(third)
	next, err := fixture.registry.BeginPaneRotation(second, third)
	if err != nil {
		t.Fatalf("historical predecessor witness blocked the next rotation: %v", err)
	}
	next.Abort()
	fixture.abortReservation(thirdReservation)
	if !slices.Equal(fixture.unit.witnesses, []controlmode.PaneWitness{fixture.previous, second}) {
		t.Fatalf("second Abort changed committed witness history: %#v", fixture.unit.witnesses)
	}
}

func TestPaneRotationN9ConsecutiveAbortsNeverStageWitnesses(t *testing.T) {
	fixture := newRotationFixture(t)
	before := append([]controlmode.PaneWitness(nil), fixture.unit.witnesses...)
	for index := 0; index < 32; index++ {
		next := fixture.next(uint64(100 + index))
		reservation := fixture.materialize(next)
		txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
		if err != nil {
			t.Fatalf("abort %d begin: %v", index, err)
		}
		txn.Abort()
		fixture.abortReservation(reservation)
		if !slices.Equal(fixture.unit.witnesses, before) {
			t.Fatalf("abort %d leaked witness: %#v", index, fixture.unit.witnesses)
		}
	}
}

func TestPaneRotationValidateThenInconsistencyFatallyReapsWithoutSwap(t *testing.T) {
	fixture := newRotationFixture(t)
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := txn.WriteBootstrap([]byte("fatal-bootstrap")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.boundary(next); err != nil {
		t.Fatal(err)
	}
	if err := txn.Validate(); err != nil {
		t.Fatal(err)
	}
	fixture.registry.retention.mu.Lock()
	fixture.registry.retention.generations[journalKey(next)] = &retentionGeneration{key: journalKey(next)}
	fixture.registry.retention.mu.Unlock()
	if disposition := txn.Commit(); disposition != paneRotationCommitFatal {
		t.Fatalf("inconsistent commit disposition=%d", disposition)
	}

	fixture.registry.mu.Lock()
	session := fixture.registry.sessions[sessionKey(fixture.previous.Session)]
	_, successorAdmitted := fixture.registry.admitted[routeCoordinateKey(next)]
	fixture.registry.mu.Unlock()
	fixture.effects.mu.Lock()
	_, unitLive := fixture.effects.units[fixture.unit.sessionID]
	_, active := fixture.effects.active[fixture.unit.sessionID]
	fixture.effects.mu.Unlock()
	if session == txn.successorSession || successorAdmitted {
		t.Fatal("fatal commit inconsistency partially installed the successor")
	}
	if unitLive || active {
		t.Fatal("fatal commit inconsistency did not reap the unit")
	}
	if !slices.Equal(fixture.unit.witnesses, []controlmode.PaneWitness{fixture.previous}) {
		t.Fatal("fatal commit inconsistency installed the successor witness")
	}
	fixture.abortReservation(reservation)
}

func validatedPaneRotation(t *testing.T, fixture *rotationFixture, next controlmode.PaneWitness) (*paneRotationTxn, *unifiedjournal.AdoptionReservation) {
	t.Helper()
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := txn.WriteBootstrap([]byte("post-validate-bootstrap")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.boundary(next); err != nil {
		t.Fatal(err)
	}
	if err := txn.Validate(); err != nil {
		t.Fatal(err)
	}
	return txn, reservation
}

func (fixture *rotationFixture) assertFatalCommitEdgePreservesPredecessor(txn *paneRotationTxn, next controlmode.PaneWitness) {
	fixture.t.Helper()
	fixture.waitSuccessorQuiescent(next)

	fixture.effects.mu.Lock()
	_, unitLive := fixture.effects.units[fixture.unit.sessionID]
	activeKey, active := fixture.effects.active[fixture.unit.sessionID]
	fixture.effects.mu.Unlock()
	if unitLive || active {
		fixture.t.Fatalf("fatal commit retained provider authority: unit=%v active=%v key=%#v", unitLive, active, activeKey)
	}

	fixture.registry.mu.Lock()
	session := fixture.registry.sessions[sessionKey(fixture.previous.Session)]
	_, successorAdmitted := fixture.registry.admitted[routeCoordinateKey(next)]
	coordinator := fixture.registry.panes[coordinateKey(fixture.previous)]
	broken := fixture.registry.broken
	fixture.registry.mu.Unlock()
	// A fatal inconsistency may reap the owning unit and thereby disconnect its predecessor route,
	// but the provisional successor fault remains local: it must not trip the
	// realm breaker or install any successor authority.
	if session != fixture.session || successorAdmitted || broken {
		fixture.t.Fatalf("fatal commit changed registry authority: session=%v successor=%v broken=%v", session == fixture.session, successorAdmitted, broken)
	}
	if coordinator != fixture.coordinator {
		fixture.t.Fatal("fatal commit replaced the shared coordinator")
	}
	coordinator.mu.Lock()
	attachmentIdentity := fmt.Sprintf("%p", coordinator.attachments)
	_, attached := coordinator.attachments[fixture.epoch]
	coordinator.mu.Unlock()
	if attachmentIdentity != fixture.attachments || !attached {
		fixture.t.Fatal("fatal commit changed shared attachment identity or membership")
	}

	fixture.registry.retention.mu.Lock()
	oldGeneration, oldRuntime := fixture.registry.retention.generations[journalKey(fixture.previous)]
	successorGeneration, successorRuntime := fixture.registry.retention.generations[journalKey(next)]
	successorAdmittedRuntime := successorRuntime && successorGeneration.admitted
	fixture.registry.retention.mu.Unlock()
	if !oldRuntime || oldGeneration != fixture.generation || (successorRuntime && successorGeneration != txn.successorGeneration) || successorAdmittedRuntime {
		fixture.t.Fatalf("fatal commit runtime generations: old=%v successor=%v successor_admitted=%v", oldRuntime, successorRuntime, successorAdmittedRuntime)
	}
}

func TestPaneRotationPostValidateProviderReapNeverInstallsSuccessor(t *testing.T) {
	fixture := newRotationFixture(t)
	next := fixture.next(2)
	txn, reservation := validatedPaneRotation(t, fixture, next)

	var snapshot []controlmode.PaneWitness
	fixture.effects.unitReapEdge = func(_ *unifiedDevUnit, witnesses []controlmode.PaneWitness) {
		snapshot = append([]controlmode.PaneWitness(nil), witnesses...)
	}
	fixture.effects.sessionPresence = func(config.TmuxServer, string) (bool, bool) { return false, true }
	fixture.effects.reapUnitContext(context.Background(), fixture.unit)
	if disposition := txn.Commit(); disposition != paneRotationCommitFatal {
		t.Fatalf("provider-reap commit disposition=%d", disposition)
	}
	if !slices.Equal(snapshot, []controlmode.PaneWitness{fixture.previous}) {
		t.Fatalf("reap witness snapshot=%#v", snapshot)
	}

	fixture.registry.mu.Lock()
	session := fixture.registry.sessions[sessionKey(next.Session)]
	_, admitted := fixture.registry.admitted[routeCoordinateKey(next)]
	fixture.registry.mu.Unlock()
	if session == txn.successorSession || admitted {
		t.Fatalf("post-Validate provider reap installed successor: session=%v admitted=%v", session == txn.successorSession, admitted)
	}
	fixture.abortReservation(reservation)
}

func TestPaneRotationPostValidateSiblingDriftNeverPartiallyCommits(t *testing.T) {
	fixture := newRotationFixture(t)
	next := fixture.next(2)
	txn, reservation := validatedPaneRotation(t, fixture, next)
	sibling := fixture.previous
	sibling.Pane = "%rotation-sibling"
	sibling.Incarnation = "1000,2,0"
	if err := fixture.registry.AdmitPane(sibling); err != nil {
		t.Fatal(err)
	}
	fixture.effects.mu.Lock()
	fixture.unit.witnesses = append(fixture.unit.witnesses, sibling)
	fixture.effects.mu.Unlock()
	entered := make(chan struct{})
	var snapshot []controlmode.PaneWitness
	fixture.effects.unitReapEdge = func(_ *unifiedDevUnit, witnesses []controlmode.PaneWitness) {
		snapshot = append([]controlmode.PaneWitness(nil), witnesses...)
		close(entered)
	}
	if disposition := txn.Commit(); disposition != paneRotationCommitFatal {
		t.Fatalf("sibling-drift commit disposition=%d", disposition)
	}
	<-entered
	fixture.assertFatalCommitEdgePreservesPredecessor(txn, next)

	fixture.registry.mu.Lock()
	session := fixture.registry.sessions[sessionKey(next.Session)]
	_, successorAdmitted := fixture.registry.admitted[routeCoordinateKey(next)]
	_, siblingAdmitted := fixture.registry.admitted[routeCoordinateKey(sibling)]
	siblingRoutable := session != nil && session.router.UnifiedEligible(sibling)
	fixture.registry.mu.Unlock()
	if session == txn.successorSession || successorAdmitted {
		t.Fatalf("post-Validate sibling drift partially committed: successor=%v sibling_admitted=%v sibling_routable=%v", successorAdmitted, siblingAdmitted, siblingRoutable)
	}
	if !slices.Equal(snapshot, []controlmode.PaneWitness{fixture.previous, sibling}) {
		t.Fatalf("sibling-drift reap snapshot=%#v", snapshot)
	}
	fixture.abortReservation(reservation)
}

func TestPaneRotationFatalDispositionStopsEveryDownstreamMutation(t *testing.T) {
	fixture := newRotationFixture(t)
	next := fixture.next(2)
	txn, reservation := validatedPaneRotation(t, fixture, next)
	fixture.effects.mu.Lock()
	delete(fixture.effects.active, fixture.unit.sessionID)
	fixture.effects.mu.Unlock()

	activeSwaps, subscriberCloses, reservationCommits := 0, 0, 0
	disposition := txn.Commit()
	if disposition == paneRotationCommitted {
		activeSwaps++
		subscriberCloses++
		reservationCommits++
	}
	if disposition != paneRotationCommitFatal || activeSwaps != 0 || subscriberCloses != 0 || reservationCommits != 0 {
		t.Fatalf("fatal downstream callbacks: disposition=%d active=%d close=%d reservation=%d", disposition, activeSwaps, subscriberCloses, reservationCommits)
	}
	fixture.registry.mu.Lock()
	coordinator := fixture.registry.panes[coordinateKey(fixture.previous)]
	_, successorAdmitted := fixture.registry.admitted[routeCoordinateKey(next)]
	fixture.registry.mu.Unlock()
	if coordinator != fixture.coordinator || successorAdmitted {
		t.Fatal("fatal disposition changed shared coordinator or successor admission")
	}
	coordinator.mu.Lock()
	attachmentIdentity := fmt.Sprintf("%p", coordinator.attachments)
	_, attached := coordinator.attachments[fixture.epoch]
	coordinator.mu.Unlock()
	if attachmentIdentity != fixture.attachments || !attached {
		t.Fatal("fatal disposition changed shared attachment membership")
	}
	fixture.abortReservation(reservation)
}

func TestPaneRotationLockContractLeavesSubscriberMutexOutsideStorageAndBoundary(t *testing.T) {
	fixture := newRotationFixture(t)
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	checked := map[string]bool{}
	fixture.effects.mu.Lock()
	fixture.effects.lockProbe = func(stage string) {
		if !fixture.effects.subscriberMu.TryLock() {
			t.Errorf("subscriberMu held during %s storage I/O", stage)
			return
		}
		fixture.effects.subscriberMu.Unlock()
		mu.Lock()
		checked[stage] = true
		mu.Unlock()
	}
	fixture.effects.mu.Unlock()
	if err := txn.WriteBootstrap([]byte("lock-contract")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- fixture.boundary(next) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bootstrap boundary deadlocked")
	}
	mu.Lock()
	for _, stage := range []string{"append", "sync", "commit"} {
		if !checked[stage] {
			t.Errorf("storage stage %s was not exercised", stage)
		}
	}
	mu.Unlock()
	txn.Abort()
	fixture.abortReservation(reservation)
}

func TestPaneRotationRejectsInvalidAndConcurrentTransactionsTyped(t *testing.T) {
	fixture := newRotationFixture(t)
	invalid := fixture.previous
	invalid.Pane = "%other"
	if _, err := fixture.registry.BeginPaneRotation(fixture.previous, invalid); !errors.Is(err, ErrPaneRotationInvalid) {
		t.Fatalf("invalid shape error = %v", err)
	}
	first := fixture.next(2)
	firstReservation := fixture.materialize(first)
	firstTxn, err := fixture.registry.BeginPaneRotation(fixture.previous, first)
	if err != nil {
		t.Fatal(err)
	}
	second := fixture.next(3)
	// A second concurrent rollover is refused before any third journal can
	// spend a source allowance. Registry refusal needs only its witness.
	if _, err := fixture.registry.BeginPaneRotation(fixture.previous, second); !errors.Is(err, ErrPaneRotationInvalid) {
		t.Fatalf("concurrent rotation error = %v", err)
	}
	firstTxn.Abort()
	fixture.abortReservation(firstReservation)
}
