package unifiedjournal

import (
	"errors"
	"testing"
)

func fillRotationPane(t *testing.T, realm *Realm, key PaneKey, size int64) {
	t.Helper()
	if _, err := realm.Append(key, make([]byte, size)); err != nil {
		t.Fatalf("fill %v with %d bytes: %v", key, size, err)
	}
}

func rotationLedger(realm *Realm) (slots int64, logical, physical int64) {
	realm.retention.mu.Lock()
	slots = realm.retention.reserved
	realm.retention.mu.Unlock()
	return slots, realm.reservedCharge, realm.physicalReserved
}

// On pre-materialization abort, rollback must replay the bounded tail into
// the predecessor using its reserved capacity before releasing that capacity.
// A predecessor with exactly the reserved headroom must remain eligible.
func TestRotationR1FundsRollbackReplayWhileCapacityHeld(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-r1-held", 1)
	admitRotationPredecessor(t, realm, predecessor)
	// Leave exactly rotationPendingCapBytes of logical headroom.
	fillRotationPane(t, realm, predecessor, options.PaneCapBytes-rotationPendingCapBytes)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatalf("capacity with exactly R1 headroom refused: %v", err)
	}
	defer capacity.Release()
	if err := capacity.ArmRollback(); err != nil {
		t.Fatalf("arm pre-materialization rollback: %v", err)
	}
	// 4R: replay the bounded tail into the predecessor. Packet: "R1 guarantees
	// both the capacity and the record count for it".
	if _, err := realm.Append(predecessor, make([]byte, rotationPendingCapBytes)); err != nil {
		t.Fatalf("rollback replay of exactly R1 bytes refused while R1 held: %v (predecessor eligible=%v reason=%v)", err, realm.UnifiedEligible(predecessor), realm.Reason(predecessor))
	}
	if !realm.UnifiedEligible(predecessor) {
		t.Fatalf("predecessor not live after rollback replay: reason=%v", realm.Reason(predecessor))
	}
}

func TestRotationCrashMidRotationOfBornSessionIsSupersededOnReopen(t *testing.T) {
	options := rotationOptions(t)
	options.CompletePaneSlots = 2
	predecessor := rotationKey("$rotation-crash", 1)
	successor := rotationKey("$rotation-crash", 2)
	crashed, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer crashed.Close() // descriptor hygiene only; no ledger cleanup runs
	admitRotationPredecessor(t, crashed, predecessor)
	appendCommittedOutput(t, crashed, predecessor, []byte("born-committed"))
	capacity, err := crashed.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crashed.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 30}, capacity); err != nil {
		t.Fatal(err)
	}
	appendCommittedOutput(t, crashed, successor, []byte("rotated-bootstrap"))
	if _, err := crashed.Append(successor, []byte("in-flight")); err != nil {
		t.Fatal(err)
	}
	// crash: no Commit, no Abort.

	options.BrokerIncarnation = "broker-incarnation-after-crash"
	reopened, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, key := range []PaneKey{predecessor, successor} {
		pane := reopened.panes[key]
		if pane == nil || !pane.slotRetired || pane.state == EligibilityContinuous {
			t.Fatalf("%v not retired at scan: %#v", key, pane)
		}
		if _, err := reopened.BeginRotationCapacity(key); !errors.Is(err, ErrInvalidated) {
			t.Fatalf("recovered %v accepted a rotation capacity: %v", key, err)
		}
		if _, err := reopened.Append(key, []byte("late")); !errors.Is(err, ErrInvalidated) {
			t.Fatalf("recovered %v accepted an append: %v", key, err)
		}
	}
	if origin, err := reopened.Origin(successor); err != nil || origin != OriginRotated {
		t.Fatalf("successor origin=%q err=%v", origin, err)
	}
	if committed, err := reopened.ReadCommitted(successor); err != nil || string(committed) != "rotated-bootstrap" {
		t.Fatalf("successor committed prefix=%q err=%v", committed, err)
	}
	if available := reopened.AvailableCompletePaneSlots(); available != 1 {
		t.Fatalf("retired generations hold ordinary slots: available=%d", available)
	}
	staleLogical := reopened.total
	if staleLogical != int64(len("born-committed")+len("rotated-bootstrap")+len("in-flight")) {
		t.Fatalf("scan charge=%d", staleLogical)
	}
	fresh := rotationKey("$rotation-crash", 3)
	adoption, err := reopened.BeginReconstructedPane(fresh, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("adoption after crash refused: %v", err)
	}
	if _, err := reopened.Append(fresh, []byte("fresh")); err != nil {
		t.Fatal(err)
	}
	adoption.Commit()
	for _, key := range []PaneKey{predecessor, successor} {
		requireRetentionPathAbsent(t, storagePathsForTest(options, key).Journal)
		if _, present := reopened.panes[key]; present {
			t.Fatalf("%v survived the adoption sweep", key)
		}
	}
	freshPhysical, _ := reopened.PanePhysical(fresh)
	charged, _, _ := reopened.PhysicalBudget()
	if reopened.total != int64(len("fresh")) || charged != freshPhysical {
		t.Fatalf("ledgers after sweep total=%d physical=%d want %d/%d", reopened.total, charged, len("fresh"), freshPhysical)
	}
}
