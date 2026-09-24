package broker

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

// rotationAccounting is the residue fingerprint a rotation transaction must
// leave untouched after Abort or refusal.
type rotationAccounting struct {
	generations         int
	pUsed, qUsed        int
	rotations           int
	rotationGenerations int
	broken              bool
	admitted            int
	sessions            int
	panes               int
}

func (fixture *rotationFixture) rotationAccounting() rotationAccounting {
	fixture.t.Helper()
	var out rotationAccounting
	fixture.registry.mu.Lock()
	out.rotations = len(fixture.registry.rotations)
	out.rotationGenerations = len(fixture.registry.rotationGenerations)
	out.broken = fixture.registry.broken
	out.admitted = len(fixture.registry.admitted)
	out.sessions = len(fixture.registry.sessions)
	out.panes = len(fixture.registry.panes)
	fixture.registry.retention.mu.Lock()
	out.generations = len(fixture.registry.retention.generations)
	out.pUsed = fixture.registry.retention.pUsed
	out.qUsed = fixture.registry.retention.qUsed
	fixture.registry.retention.mu.Unlock()
	fixture.registry.mu.Unlock()
	return out
}

func (fixture *rotationFixture) waitRotationAccounting(want rotationAccounting) rotationAccounting {
	fixture.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := fixture.rotationAccounting()
		if got == want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

func (fixture *rotationFixture) admitRotationPane(witness controlmode.PaneWitness) {
	fixture.t.Helper()
	base := fixture.effects.UnifiedDevPaneEffects
	reserveRecordingSourceForTest(fixture.t, base, journalKey(witness))
	base.journalMu.Lock()
	err := base.realm.AdmitPane(journalKey(witness), unifiedjournal.Geometry{Columns: 80, Rows: 24})
	base.journalMu.Unlock()
	if err != nil {
		fixture.t.Fatalf("admit %v journal: %v", witness, err)
	}
	if err := fixture.registry.AdmitPane(witness); err != nil {
		fixture.t.Fatalf("admit %v registry: %v", witness, err)
	}
	commitRecordingInitialForTest(fixture.t, fixture.registry, witness, nil)
}

func (fixture *rotationFixture) successorGeneration(next controlmode.PaneWitness) (present, admitted bool, refs int) {
	fixture.t.Helper()
	fixture.registry.retention.mu.Lock()
	defer fixture.registry.retention.mu.Unlock()
	generation := fixture.registry.retention.generations[journalKey(next)]
	if generation == nil {
		return false, false, 0
	}
	return true, generation.admitted, generation.refs
}

// F14: a post-Validate inconsistency must fault the successor and reap the
// unit. It must not trip the realm-wide breaker, which would destroy every
// other session's admission and refuse re-adoption of this one.
func TestRotationFatalCommitDoesNotTripRealmBreaker(t *testing.T) {
	fixture := newRotationFixture(t)
	other := controlmode.PaneWitness{
		Session: controlmode.SessionWitness{Server: "main", Session: "$rotation-other", ControlGeneration: 1},
		Window:  "@other", Pane: "%other", Incarnation: "2000,1,0",
	}
	fixture.admitRotationPane(other)
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
	// The same post-Validate inconsistency the builder's F14 test injects.
	fixture.registry.retention.mu.Lock()
	fixture.registry.retention.generations[journalKey(next)] = &retentionGeneration{key: journalKey(next)}
	fixture.registry.retention.mu.Unlock()
	txn.Commit()

	after := fixture.rotationAccounting()
	fixture.registry.mu.Lock()
	_, otherAdmitted := fixture.registry.admitted[routeCoordinateKey(other)]
	fixture.registry.mu.Unlock()
	if after.broken {
		t.Fatalf("F14 fatal reap tripped the realm breaker: registry.broken=true admitted=%d", after.admitted)
	}
	if !otherAdmitted {
		t.Fatal("F14 fatal reap destroyed an unrelated session's admission")
	}
	if err := fixture.registry.ObservePane(controlmode.Observation{
		Kind: controlmode.ObservationOutput, Witness: other, Data: []byte("other-still-live"),
	}); err != nil {
		t.Fatalf("unrelated session output after F14 fatal reap: %v", err)
	}
	fixture.abortReservation(reservation)
}

// The shadow router is a SESSION router and Commit installs it over the
// session's live router. A session with another admitted pane therefore
// cannot be rotated one pane at a time: the transaction must refuse, or the
// sibling must remain routable after Commit. It must never leave the sibling
// admitted-but-unroutable.
func TestRotationRotationRefusesOrCarriesSiblingPane(t *testing.T) {
	fixture := newRotationFixture(t)
	sibling := fixture.previous
	sibling.Pane = "%rotation-sibling"
	sibling.Incarnation = "1001,1,0"
	fixture.admitRotationPane(sibling)
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		if !errors.Is(err, ErrPaneRotationInvalid) {
			t.Fatalf("sibling refusal error = %v", err)
		}
		fixture.abortReservation(reservation)
		fixture.registry.mu.Lock()
		session := fixture.registry.sessions[sessionKey(fixture.previous.Session)]
		fixture.registry.mu.Unlock()
		if session != fixture.session || !session.router.UnifiedEligible(fixture.previous) || !session.router.UnifiedEligible(sibling) {
			t.Fatal("refusal disturbed the predecessor session router")
		}
		return
	}
	if err := txn.WriteBootstrap([]byte("sibling-bootstrap")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.boundary(next); err != nil {
		t.Fatal(err)
	}
	if err := txn.Validate(); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	fixture.effects.journalMu.Lock()
	reservation.Commit()
	fixture.effects.journalMu.Unlock()

	fixture.registry.mu.Lock()
	session := fixture.registry.sessions[sessionKey(sibling.Session)]
	_, siblingAdmitted := fixture.registry.admitted[routeCoordinateKey(sibling)]
	siblingRoutable := session != nil && session.router.UnifiedEligible(sibling)
	fixture.registry.mu.Unlock()
	if siblingAdmitted && !siblingRoutable {
		t.Fatalf("commit left the sibling pane admitted at generation %d but unroutable in the installed generation-%d router: split authority",
			sibling.Session.ControlGeneration, next.Session.ControlGeneration)
	}
	_ = fixture.registry.ObservePane(controlmode.Observation{
		Kind: controlmode.ObservationOutput, Witness: sibling, Data: []byte("sibling-output"),
	})
	fixture.registry.mu.Lock()
	successorEligible := session.router.UnifiedEligible(next)
	fixture.registry.mu.Unlock()
	if !successorEligible {
		t.Fatal("sibling output invalidated the freshly installed successor router")
	}
}

// Begin must reject every shape that is not "differs only in
// ControlGeneration", every predecessor that is not the live admitted
// witness, and an already-admitted successor route — all without residue.
func TestRotationBeginRejectsEveryNonRotationShapeWithoutResidue(t *testing.T) {
	fixture := newRotationFixture(t)
	before := fixture.rotationAccounting()
	shapes := map[string]func(*controlmode.PaneWitness){
		"same_generation":       func(*controlmode.PaneWitness) {},
		"window":                func(w *controlmode.PaneWitness) { w.Window = "@drift" },
		"pane":                  func(w *controlmode.PaneWitness) { w.Pane = "%drift" },
		"incarnation":           func(w *controlmode.PaneWitness) { w.Incarnation = "1000,2,0" },
		"server":                func(w *controlmode.PaneWitness) { w.Session.Server = "other" },
		"session_name":          func(w *controlmode.PaneWitness) { w.Session.Session = "$drift" },
		"generation_and_window": func(w *controlmode.PaneWitness) { w.Session.ControlGeneration = 2; w.Window = "@drift" },
		"generation_and_incarnation": func(w *controlmode.PaneWitness) {
			w.Session.ControlGeneration = 2
			w.Incarnation = "1000,2,0"
		},
	}
	for name, mutate := range shapes {
		next := fixture.previous
		mutate(&next)
		txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
		if txn != nil || !errors.Is(err, ErrPaneRotationInvalid) {
			t.Fatalf("shape %s: txn=%v err=%v", name, txn, err)
		}
		if after := fixture.rotationAccounting(); after != before {
			t.Fatalf("shape %s left residue: before=%+v after=%+v", name, before, after)
		}
	}
	stranger := fixture.previous
	stranger.Pane = "%never-admitted"
	strangerNext := stranger
	strangerNext.Session.ControlGeneration = 2
	if txn, err := fixture.registry.BeginPaneRotation(stranger, strangerNext); txn != nil || !errors.Is(err, ErrPaneRotationInvalid) {
		t.Fatalf("unadmitted predecessor: txn=%v err=%v", txn, err)
	}
	if after := fixture.rotationAccounting(); after != before {
		t.Fatalf("unadmitted predecessor left residue: before=%+v after=%+v", before, after)
	}
	if txn, err := fixture.registry.BeginPaneRotation(fixture.next(2), fixture.next(3)); txn != nil || !errors.Is(err, ErrPaneRotationInvalid) {
		t.Fatalf("unadmitted generation as predecessor: txn=%v err=%v", txn, err)
	}
	if after := fixture.rotationAccounting(); after != before {
		t.Fatalf("unadmitted generation left residue: before=%+v after=%+v", before, after)
	}

	// An already-admitted successor route is never a rotation target.
	next := fixture.next(2)
	fixture.admitRotationPane(next)
	afterAdmit := fixture.rotationAccounting()
	if txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next); txn != nil || !errors.Is(err, ErrPaneRotationInvalid) {
		t.Fatalf("admitted successor: txn=%v err=%v", txn, err)
	}
	if after := fixture.rotationAccounting(); after != afterAdmit {
		t.Fatalf("admitted successor left residue: before=%+v after=%+v", afterAdmit, after)
	}
}

// Validate is the only fallible step; it must actually fail on every
// condition Commit would otherwise install over: a dead unit, a drifted
// witness set, a moved active key, and a bootstrap that was never written.
func TestRotationValidateRefusesDeadUnitDriftedWitnessAndMissingBootstrap(t *testing.T) {
	cases := map[string]func(*rotationFixture, *paneRotationTxn){
		"missing_bootstrap": func(*rotationFixture, *paneRotationTxn) {},
		"unit_death": func(fixture *rotationFixture, txn *paneRotationTxn) {
			fixture.writeBootstrap(txn)
			close(fixture.unit.done)
		},
		"witness_drift": func(fixture *rotationFixture, txn *paneRotationTxn) {
			fixture.writeBootstrap(txn)
			extra := fixture.previous
			extra.Pane = "%adopted-later"
			base := fixture.effects.UnifiedDevPaneEffects
			base.mu.Lock()
			fixture.unit.witnesses = append(fixture.unit.witnesses, extra)
			base.mu.Unlock()
		},
		"active_key_drift": func(fixture *rotationFixture, txn *paneRotationTxn) {
			fixture.writeBootstrap(txn)
			base := fixture.effects.UnifiedDevPaneEffects
			base.mu.Lock()
			base.active[fixture.unit.sessionID] = journalKey(fixture.next(99))
			base.mu.Unlock()
		},
		"unit_replaced": func(fixture *rotationFixture, txn *paneRotationTxn) {
			fixture.writeBootstrap(txn)
			base := fixture.effects.UnifiedDevPaneEffects
			base.mu.Lock()
			base.units[fixture.unit.sessionID] = &unifiedDevUnit{owner: base, sessionID: fixture.unit.sessionID, done: make(chan struct{})}
			base.mu.Unlock()
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newRotationFixture(t)
			before := fixture.rotationAccounting()
			witnessesBefore := append([]controlmode.PaneWitness(nil), fixture.unit.witnesses...)
			next := fixture.next(2)
			reservation := fixture.materialize(next)
			txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
			if err != nil {
				t.Fatal(err)
			}
			arrange(fixture, txn)
			if err := txn.Validate(); !errors.Is(err, ErrPaneRotationValidate) {
				t.Fatalf("Validate error = %v, want ErrPaneRotationValidate", err)
			}
			fixture.registry.mu.Lock()
			session := fixture.registry.sessions[sessionKey(fixture.previous.Session)]
			_, successorAdmitted := fixture.registry.admitted[routeCoordinateKey(next)]
			fixture.registry.mu.Unlock()
			if session != fixture.session || successorAdmitted {
				t.Fatal("a refused Validate installed the successor")
			}
			txn.Abort()
			fixture.abortReservation(reservation)
			fixture.waitSuccessorQuiescent(next)
			// Restore fixture-side drift so the residue comparison is about the
			// transaction, not about the arrangement.
			base := fixture.effects.UnifiedDevPaneEffects
			base.mu.Lock()
			base.units[fixture.unit.sessionID] = fixture.unit
			base.active[fixture.unit.sessionID] = journalKey(fixture.previous)
			base.mu.Unlock()
			if !slices.Equal(fixture.unit.witnesses, witnessesBefore) && name != "witness_drift" {
				t.Fatalf("unit.witnesses changed: %#v", fixture.unit.witnesses)
			}
			fixture.unit.witnesses = witnessesBefore
			after := fixture.waitRotationAccounting(before)
			if after != before {
				t.Fatalf("residue after refused Validate + Abort: before=%+v after=%+v", before, after)
			}
		})
	}
}

func (fixture *rotationFixture) writeBootstrap(txn *paneRotationTxn) {
	fixture.t.Helper()
	if err := txn.WriteBootstrap([]byte("rotation-bootstrap")); err != nil {
		fixture.t.Fatal(err)
	}
	if err := fixture.boundary(txn.next); err != nil {
		fixture.t.Fatal(err)
	}
}

// Abort at every pre-PONR stage — including immediately after Validate, the
// last abortable moment — must leave the registry and retention accounting
// bit-identical, and a retry must then commit. The in-flight stage aborts
// while the bootstrap write still holds a reference on the successor
// generation; the generation must still be released once that reference
// drops, or every such abort leaks a pane slot.
func TestRotationAbortLeavesZeroResidueAtEveryStage(t *testing.T) {
	for _, stage := range []string{"begin", "bootstrap_in_flight", "bootstrap_durable", "validated"} {
		t.Run(stage, func(t *testing.T) {
			fixture := newRotationFixture(t)
			before := fixture.rotationAccounting()
			next := fixture.next(2)
			reservation := fixture.materialize(next)
			txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
			if err != nil {
				t.Fatal(err)
			}
			var release chan struct{}
			switch stage {
			case "begin":
			case "bootstrap_in_flight":
				release = make(chan struct{})
				var once sync.Once
				fixture.effects.mu.Lock()
				fixture.effects.lockProbe = func(stage string) {
					if stage == "append" {
						once.Do(func() { <-release })
					}
				}
				fixture.effects.mu.Unlock()
				if err := txn.WriteBootstrap([]byte("in-flight")); err != nil {
					t.Fatal(err)
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
			case "bootstrap_durable":
				fixture.writeBootstrap(txn)
			case "validated":
				fixture.writeBootstrap(txn)
				if err := txn.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			txn.Abort()
			if release != nil {
				close(release)
				fixture.effects.mu.Lock()
				fixture.effects.lockProbe = nil
				fixture.effects.mu.Unlock()
			}
			fixture.abortReservation(reservation)
			fixture.waitSuccessorQuiescent(next)
			deadline := time.Now().Add(2 * time.Second)
			for {
				present, _, _ := fixture.successorGeneration(next)
				if !present {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("stage %s: successor generation %v survived Abort with zero references", stage, journalKey(next))
				}
				time.Sleep(time.Millisecond)
			}
			after := fixture.rotationAccounting()
			if after != before {
				t.Fatalf("stage %s residue: before=%+v after=%+v", stage, before, after)
			}
			fixture.assertPredecessorLive("OLD-"+stage, next)
			fixture.requireRetry(3)
		})
	}
}

// Settlement semantics: one bootstrap per transaction, Busy after settle,
// Commit marks the successor generation admitted, ownership is released, and
// a settled transaction is inert on every later call.
func TestRotationSettleSemanticsAndSuccessorAdmission(t *testing.T) {
	fixture := newRotationFixture(t)
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := txn.WriteBootstrap([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := txn.WriteBootstrap([]byte("second")); !errors.Is(err, ErrPaneRotationBusy) {
		t.Fatalf("second bootstrap error = %v, want ErrPaneRotationBusy", err)
	}
	if err := fixture.boundary(next); err != nil {
		t.Fatal(err)
	}
	if err := txn.Validate(); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	fixture.effects.journalMu.Lock()
	reservation.Commit()
	fixture.effects.journalMu.Unlock()

	present, admitted, _ := fixture.successorGeneration(next)
	if !present || !admitted {
		t.Fatalf("commit did not mark the successor generation admitted: present=%v admitted=%v", present, admitted)
	}
	after := fixture.rotationAccounting()
	if after.rotations != 0 || after.rotationGenerations != 0 {
		t.Fatalf("commit left transaction ownership: %+v", after)
	}
	if err := txn.Validate(); !errors.Is(err, ErrPaneRotationBusy) {
		t.Fatalf("Validate after commit = %v", err)
	}
	if err := txn.WriteBootstrap([]byte("late")); !errors.Is(err, ErrPaneRotationBusy) {
		t.Fatalf("WriteBootstrap after commit = %v", err)
	}
	txn.Commit()
	txn.Abort()
	if again := fixture.rotationAccounting(); again != after {
		t.Fatalf("settled transaction mutated state: before=%+v after=%+v", after, again)
	}
	fixture.registry.mu.Lock()
	session := fixture.registry.sessions[sessionKey(next.Session)]
	_, oldAdmitted := fixture.registry.admitted[routeCoordinateKey(fixture.previous)]
	fixture.registry.mu.Unlock()
	if session != txn.successorSession || oldAdmitted {
		t.Fatal("settled Abort disturbed the committed successor authority")
	}
	// The successor accepts output through the shared coordinator.
	if err := fixture.registry.ObservePane(controlmode.Observation{
		Kind: controlmode.ObservationOutput, Witness: next, Data: []byte("NEW-after-commit"),
	}); err != nil {
		t.Fatalf("successor output: %v", err)
	}
	if err := fixture.boundary(next); err != nil {
		t.Fatal(err)
	}
	fixture.effects.journalMu.Lock()
	committed, err := fixture.effects.realm.ReadCommitted(journalKey(next))
	fixture.effects.journalMu.Unlock()
	if err != nil || !bytes.Contains(committed, []byte("first")) || !bytes.Contains(committed, []byte("NEW-after-commit")) {
		t.Fatalf("successor journal: bytes=%q err=%v", committed, err)
	}
	if bytes.Contains(committed, []byte("second")) {
		t.Fatal("refused second bootstrap reached the successor journal")
	}
}

// Concurrent Begins for one predecessor admit exactly one transaction, and
// every loser leaves no provisional generation, ownership, or accounting.
func TestRotationConcurrentBeginsAdmitExactlyOne(t *testing.T) {
	fixture := newRotationFixture(t)
	before := fixture.rotationAccounting()
	const fanout = 6
	for round := 0; round < 12; round++ {
		nexts := make([]controlmode.PaneWitness, fanout)
		for index := range nexts {
			nexts[index] = fixture.next(uint64(1000 + round*fanout + index))
		}
		// This test isolates concurrent registry claims without materialized
		// journals. Production materializes before Begin; integration tests
		// exercise that ordering and the single-successor source allowance.
		txns := make([]*paneRotationTxn, fanout)
		errs := make([]error, fanout)
		var start, done sync.WaitGroup
		start.Add(1)
		for index := range nexts {
			done.Add(1)
			go func(index int) {
				defer done.Done()
				start.Wait()
				txns[index], errs[index] = fixture.registry.BeginPaneRotation(fixture.previous, nexts[index])
			}(index)
		}
		start.Done()
		done.Wait()
		winners := 0
		for index := range txns {
			if txns[index] != nil {
				winners++
				continue
			}
			if !errors.Is(errs[index], ErrPaneRotationInvalid) {
				t.Fatalf("round %d loser %d error = %v", round, index, errs[index])
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: %d concurrent transactions admitted, want exactly 1", round, winners)
		}
		during := fixture.rotationAccounting()
		if during.rotations != 1 || during.rotationGenerations != 1 || during.generations != before.generations+1 || during.pUsed != before.pUsed+1 {
			t.Fatalf("round %d: losers left residue while the winner was open: %+v (baseline %+v)", round, during, before)
		}
		for index := range txns {
			if txns[index] != nil {
				txns[index].Abort()
			}
		}
		if after := fixture.rotationAccounting(); after != before {
			t.Fatalf("round %d residue: before=%+v after=%+v", round, before, after)
		}
	}
	fixture.assertPredecessorLive("OLD-after-contention", fixture.next(2))
}

// The live paths that share the coordinator — predecessor output,
// predecessor boundaries, attachment bind/release, and an unrelated
// session's output — run concurrently with the whole transaction. Nothing
// observes the successor before Commit; the coordinator pointer never
// changes; every predecessor byte accepted before Commit is journaled to the
// predecessor; the successor accepts output only after Commit.
func TestRotationLivePathsAcrossTransactionUnderRace(t *testing.T) {
	fixture := newRotationFixture(t)
	other := controlmode.PaneWitness{
		Session: controlmode.SessionWitness{Server: "main", Session: "$rotation-race-other", ControlGeneration: 1},
		Window:  "@race", Pane: "%race", Incarnation: "3000,1,0",
	}
	fixture.admitRotationPane(other)
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}

	stopPredecessor := make(chan struct{})
	stopAll := make(chan struct{})
	var predecessorWriters, everyone sync.WaitGroup
	var acceptedMu sync.Mutex
	var accepted [][]byte
	predecessorWriters.Add(1)
	go func() {
		defer predecessorWriters.Done()
		for index := 0; ; index++ {
			select {
			case <-stopPredecessor:
				return
			default:
			}
			payload := []byte(fmt.Sprintf("PRE-%04d|", index))
			if err := fixture.registry.ObservePane(controlmode.Observation{
				Kind: controlmode.ObservationOutput, Witness: fixture.previous, Data: payload,
			}); err == nil {
				acceptedMu.Lock()
				accepted = append(accepted, payload)
				acceptedMu.Unlock()
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	predecessorWriters.Add(1)
	go func() {
		defer predecessorWriters.Done()
		for {
			select {
			case <-stopPredecessor:
				return
			default:
			}
			_ = fixture.registry.retentionBoundary(fixture.previous, "race_probe")
			time.Sleep(time.Millisecond)
		}
	}()
	everyone.Add(1)
	go func() {
		defer everyone.Done()
		effects := fixture.registry.attachmentEffects("main", terminal.SourceWitness{
			SessionID: fixture.previous.Session.Session, WindowID: fixture.previous.Window,
			PaneID: fixture.previous.Pane, Incarnation: fixture.previous.Incarnation,
		})
		for {
			select {
			case <-stopAll:
				return
			default:
			}
			epoch := new(terminal.Epoch)
			if err := effects.BindAttachment(epoch); err != nil {
				t.Errorf("bind: %v", err)
				return
			}
			fixture.registry.mu.Lock()
			coordinator := fixture.registry.panes[coordinateKey(fixture.previous)]
			successor := fixture.registry.panes[coordinateKey(next)]
			fixture.registry.mu.Unlock()
			if coordinator != fixture.coordinator || successor != fixture.coordinator {
				t.Errorf("coordinator identity changed during transaction")
				return
			}
			_ = effects.ReleaseAttachment(epoch)
		}
	}()
	everyone.Add(1)
	go func() {
		defer everyone.Done()
		for index := 0; ; index++ {
			select {
			case <-stopAll:
				return
			default:
			}
			if err := fixture.registry.ObservePane(controlmode.Observation{
				Kind: controlmode.ObservationOutput, Witness: other, Data: []byte(fmt.Sprintf("OTHER-%d|", index)),
			}); err != nil {
				t.Errorf("unrelated session output during rotation: %v", err)
				return
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()

	if err := txn.WriteBootstrap([]byte("RACE-BOOTSTRAP|")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.boundary(next); err != nil {
		t.Fatal(err)
	}
	if err := txn.Validate(); err != nil {
		t.Fatalf("Validate under concurrent live traffic: %v", err)
	}
	close(stopPredecessor)
	predecessorWriters.Wait()
	txn.Commit()
	fixture.effects.journalMu.Lock()
	reservation.Commit()
	fixture.effects.journalMu.Unlock()
	close(stopAll)
	everyone.Wait()

	if err := fixture.registry.ObservePane(controlmode.Observation{
		Kind: controlmode.ObservationOutput, Witness: next, Data: []byte("POST-COMMIT|"),
	}); err != nil {
		t.Fatalf("successor output after commit: %v", err)
	}
	if err := fixture.boundary(next); err != nil {
		t.Fatal(err)
	}
	if err := fixture.registry.retentionBoundary(fixture.previous, "race_flush"); err != nil {
		t.Fatal(err)
	}
	fixture.effects.journalMu.Lock()
	oldBytes, oldErr := fixture.effects.realm.ReadCommitted(journalKey(fixture.previous))
	newBytes, newErr := fixture.effects.realm.ReadCommitted(journalKey(next))
	fixture.effects.journalMu.Unlock()
	if oldErr != nil || newErr != nil {
		t.Fatalf("read committed: old=%v new=%v", oldErr, newErr)
	}
	acceptedMu.Lock()
	defer acceptedMu.Unlock()
	if len(accepted) == 0 {
		t.Fatal("no predecessor output was accepted during the transaction")
	}
	for _, payload := range accepted {
		if !bytes.Contains(oldBytes, payload) {
			t.Fatalf("accepted predecessor payload %q missing from the predecessor journal", payload)
		}
	}
	if bytes.Contains(oldBytes, []byte("RACE-BOOTSTRAP|")) || bytes.Contains(newBytes, []byte("PRE-")) {
		t.Fatal("bytes crossed generations")
	}
	if !bytes.Contains(newBytes, []byte("RACE-BOOTSTRAP|")) {
		t.Fatalf("successor journal lacks the bootstrap: %q", newBytes)
	}
	if !bytes.Contains(newBytes, []byte("POST-COMMIT|")) {
		t.Fatal("successor journal lacks post-commit output")
	}
	fixture.registry.mu.Lock()
	coordinator := fixture.registry.panes[coordinateKey(next)]
	fixture.registry.mu.Unlock()
	if coordinator != fixture.coordinator {
		t.Fatal("commit replaced the shared coordinator")
	}
}

// Seam pin for rotation flow: a successor-witness observation that reaches the
// registry BEFORE Commit is never journaled to the successor, and the
// transaction can no longer commit (Validate refuses). This is why the
// rotation flow must buffer post-boundary bytes until after Commit.
func TestRotationPreCommitSuccessorObservationNeverReachesSuccessorAndBlocksCommit(t *testing.T) {
	fixture := newRotationFixture(t)
	before := fixture.rotationAccounting()
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}
	fixture.writeBootstrap(txn)
	leakErr := fixture.registry.ObservePane(controlmode.Observation{
		Kind: controlmode.ObservationOutput, Witness: next, Data: []byte("LEAK-BEFORE-COMMIT|"),
	})
	_ = fixture.boundary(next)
	fixture.effects.journalMu.Lock()
	successorBytes, readErr := fixture.effects.realm.ReadCommitted(journalKey(next))
	fixture.effects.journalMu.Unlock()
	if readErr != nil {
		t.Fatalf("read successor: %v", readErr)
	}
	if bytes.Contains(successorBytes, []byte("LEAK-BEFORE-COMMIT|")) {
		t.Fatalf("pre-commit successor observation reached the successor journal (observe err=%v)", leakErr)
	}
	validateErr := txn.Validate()
	fixture.registry.mu.Lock()
	session := fixture.registry.sessions[sessionKey(fixture.previous.Session)]
	predecessorRoutable := session != nil && session.router.UnifiedEligible(fixture.previous)
	fixture.registry.mu.Unlock()
	t.Logf("pre-commit successor observation: observe err=%v validate err=%v predecessor routable=%v", leakErr, validateErr, predecessorRoutable)
	if predecessorRoutable {
		if validateErr != nil {
			t.Fatalf("predecessor untouched but Validate refused: %v", validateErr)
		}
	} else if !errors.Is(validateErr, ErrPaneRotationValidate) {
		t.Fatalf("predecessor router invalidated but Validate = %v; Commit would install over a dead predecessor", validateErr)
	}
	txn.Abort()
	fixture.abortReservation(reservation)
	fixture.waitSuccessorQuiescent(next)
	after := fixture.rotationAccounting()
	if present, _, _ := fixture.successorGeneration(next); present || after.rotations != 0 || after.rotationGenerations != 0 || after.pUsed != before.pUsed {
		t.Fatalf("successor residue after abort: present=%v before=%+v after=%+v", present, before, after)
	}
}
