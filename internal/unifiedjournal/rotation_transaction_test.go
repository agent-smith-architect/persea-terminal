package unifiedjournal

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
)

func rotationKey(session string, generation uint64) PaneKey {
	return PaneKey{
		Server: "server-a", Session: session, ControlGeneration: generation,
		Window: "@1", Pane: "%1", Incarnation: "incarnation-a",
	}
}

func rotationOptions(t *testing.T) OpenOptions {
	t.Helper()
	options := journalOptions(t)
	options.CompletePaneSlots = 2
	options.PhysicalCapBytes = 96 << 20
	return options
}

func admitRotationPredecessor(t *testing.T, realm *Realm, key PaneKey) {
	t.Helper()
	if err := realm.AdmitPane(key, Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("admit predecessor: %v", err)
	}
}

func appendCommittedOutput(t *testing.T, realm *Realm, key PaneKey, payload []byte) {
	t.Helper()
	record, err := realm.Append(key, payload)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := realm.Sync(key); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := realm.AdvanceCommitted(key, record); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestRotationCapacityAtomicallyHoldsSlotAndAllBudgets(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-capacity", 1)
	admitRotationPredecessor(t, realm, predecessor)

	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatalf("begin rotation capacity: %v", err)
	}
	if available := realm.AvailableCompletePaneSlots(); available != 0 {
		t.Fatalf("capacity did not reserve final slot: available=%d", available)
	}
	wantLogical := rotationPendingCapBytes + adoptionBootstrapCapBytes + rotationPendingCapBytes
	if realm.reservedCharge != wantLogical {
		t.Fatalf("logical reservation=%d want=%d", realm.reservedCharge, wantLogical)
	}
	wantPhysical := rotationReplayPhysical(rotationPendingCapBytes, rotationPendingCapRecords) +
		rotationHeaderPhysicalReserve + rotationReplayPhysical(adoptionBootstrapCapBytes, adoptionBootstrapCapRecords) +
		rotationReplayPhysical(rotationPendingCapBytes, rotationPendingCapRecords)
	if _, reserved, _ := realm.PhysicalBudget(); reserved != wantPhysical {
		t.Fatalf("physical reservation=%d want=%d", reserved, wantPhysical)
	}
	if _, err := realm.BeginRotationCapacity(predecessor); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("held unarmed capacity allowed a second reservation: %v", err)
	}

	capacity.Release()
	capacity.Release()
	if available := realm.AvailableCompletePaneSlots(); available != 0 {
		t.Fatalf("capacity release exposed rotation-only slot to ordinary admission: available=%d", available)
	}
	if realm.reservedCharge != 0 {
		t.Fatalf("capacity release leaked logical reservation=%d", realm.reservedCharge)
	}
	if _, reserved, _ := realm.PhysicalBudget(); reserved != 0 {
		t.Fatalf("capacity release leaked physical reservation=%d", reserved)
	}
}

func TestEligibilityWakeFollowsActualSlotAndProvenUnlinkRelease(t *testing.T) {
	wakes := 0
	options := rotationOptions(t)
	var realm *Realm
	options.EligibilityWake = func() {
		wakes++
		if realm != nil {
			_ = realm.AvailableCompletePaneSlots()
		}
	}
	var err error
	realm, err = openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()

	predecessor := rotationKey("$eligibility-release", 1)
	admitRotationPredecessor(t, realm, predecessor)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	capacity.Release()
	capacity.Release()
	if wakes != 1 {
		t.Fatalf("complete-pane release wakes=%d want 1", wakes)
	}

	// Manufacture a retired, already-slot-released generation so the next
	// supersession wake can come only from unlink-proved capacity, not a slot.
	realm.retention.enter()
	pane := realm.panes[predecessor]
	pane.slotRetired = true
	realm.retention.releaseCompletePane()
	realm.retention.leave()
	wakes = 0
	successor := rotationKey("$eligibility-release", 2)
	reservation, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if wakes != 1 {
		t.Fatalf("proven-unlink refund wakes=%d want 1", wakes)
	}
	reservation.Abort()
}

func TestPaneLogicalReportsTheLiveChargedUnit(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	key := rotationKey("$pane-logical", 1)
	admitRotationPredecessor(t, realm, key)
	if _, err := realm.Append(key, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if _, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	charged, cap := realm.PaneLogical(key)
	if want := int64(len("payload")) + geometryRecordCost; charged != want || cap != options.PaneCapBytes {
		t.Fatalf("pane logical=%d/%d want=%d/%d", charged, cap, want, options.PaneCapBytes)
	}
}

func TestBeginRotatedPaneConsumesCapacityWithoutFreshQuotaDecision(t *testing.T) {
	options := rotationOptions(t)
	predecessor := rotationKey("$rotation-consume", 1)
	geometry := Geometry{Columns: 80, Rows: 31}
	// Exactly R1+R2+R3: once held there is no unreserved logical byte from
	// which a post-composite Append could obtain a fresh admission.
	options.RealmCapBytes = 4 << 20
	predecessorHeader, err := json.Marshal(journalHeader{
		Version: journalVersion, Key: predecessor, BrokerIncarnation: options.BrokerIncarnation,
		GeometryInitial: &Geometry{Columns: 80, Rows: 24}, Origin: OriginBirth,
	})
	if err != nil {
		t.Fatal(err)
	}
	reservedPhysical := rotationReplayPhysical(rotationPendingCapBytes, rotationPendingCapRecords) +
		rotationHeaderPhysicalReserve + rotationReplayPhysical(adoptionBootstrapCapBytes, adoptionBootstrapCapRecords) +
		rotationReplayPhysical(rotationPendingCapBytes, rotationPendingCapRecords)
	options.PhysicalCapBytes = int64(8+len(predecessorHeader)) + reservedPhysical
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	successor := rotationKey("$rotation-consume", 2)
	admitRotationPredecessor(t, realm, predecessor)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, geometry, capacity)
	if err != nil {
		t.Fatalf("materialize rotated pane: %v", err)
	}
	if origin, err := realm.Origin(successor); err != nil || origin != OriginRotated {
		t.Fatalf("rotated origin=%q err=%v", origin, err)
	}
	if initial, err := realm.InitialGeometry(successor); err != nil || initial != geometry {
		t.Fatalf("rotated initial geometry=%v err=%v", initial, err)
	}
	capacity.Release() // consumed: must not return the slot or capacity.
	if available := realm.AvailableCompletePaneSlots(); available != 0 {
		t.Fatalf("consumed capacity was released by stale owner: available=%d", available)
	}

	// Exactly the bounded replay shape is already funded. Filling it while the
	// realm has no unreserved headroom must therefore not perform a new quota
	// decision.
	chunk := make([]byte, 64<<10)
	for index := int64(0); index < adoptionBootstrapCapRecords+rotationPendingCapRecords; index++ {
		if _, err := realm.Append(successor, chunk); err != nil {
			t.Fatalf("reserved replay record %d: %v", index, err)
		}
	}
	reservation.Commit()
	if available := realm.AvailableCompletePaneSlots(); available != 0 {
		t.Fatalf("rotation commit did not swap slots: available=%d", available)
	}
	if !realm.UnifiedEligible(successor) {
		t.Fatal("successor is not live after commit")
	}
	if _, ok := realm.panes[predecessor]; ok {
		t.Fatal("predecessor remained after successful unlink")
	}
	if realm.reservedCharge != 0 {
		t.Fatalf("commit leaked logical reservation=%d", realm.reservedCharge)
	}
	if _, reserved, _ := realm.PhysicalBudget(); reserved != 0 {
		t.Fatalf("commit leaked physical reservation=%d", reserved)
	}
}

func TestRotationAbortLeavesPredecessorLiveAndRefundsSuccessorOnUnlinkProof(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-abort", 1)
	successor := rotationKey("$rotation-abort", 2)
	admitRotationPredecessor(t, realm, predecessor)
	appendCommittedOutput(t, realm, predecessor, []byte("predecessor-before"))
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := realm.Append(successor, []byte("discarded-bootstrap")); err != nil {
		t.Fatal(err)
	}
	reservation.Abort()
	reservation.Abort()
	if available := realm.AvailableCompletePaneSlots(); available != 0 {
		t.Fatalf("abort did not return successor slot: available=%d", available)
	}
	if !realm.UnifiedEligible(predecessor) {
		t.Fatal("abort changed predecessor eligibility")
	}
	appendCommittedOutput(t, realm, predecessor, []byte("predecessor-after"))
	capacity.Release()
	if _, err := realm.Append(successor, []byte("late")); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("aborted successor accepted late append: %v", err)
	}
	replay, err := realm.ReadCommitted(predecessor)
	if err != nil || string(replay) != "predecessor-beforepredecessor-after" {
		t.Fatalf("predecessor replay=%q err=%v", replay, err)
	}
	requireRetentionPathAbsent(t, storagePathsForTest(options, successor).Journal)
}

func TestRotationFatalSettlesSuccessorWithoutRollback(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-fatal", 1)
	successor := rotationKey("$rotation-fatal", 2)
	admitRotationPredecessor(t, realm, predecessor)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := realm.Append(successor, []byte("provisional-successor")); err != nil {
		t.Fatal(err)
	}
	reservation.Fatal()
	reservation.Fatal()
	capacity.Release()
	if realm.UnifiedEligible(predecessor) {
		t.Fatal("fatal settlement made the sealed predecessor eligible")
	}
	if _, err := realm.Append(predecessor, []byte("illegal-rollback")); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("fatal predecessor append error=%v", err)
	}
	if _, err := realm.Append(successor, []byte("late-successor")); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("fatal successor append error=%v", err)
	}
	if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
		t.Fatalf("fatal settlement leaked reservations logical=%d physical=%d", realm.reservedCharge, realm.physicalReserved)
	}
	if available := realm.AvailableCompletePaneSlots(); available != 1 {
		t.Fatalf("fatal settlement did not retire both sealed generations: available=%d", available)
	}
	requireRetentionPathAbsent(t, storagePathsForTest(options, successor).Journal)
}

func TestRotationFatalUnlinkFailureKeepsOnlyProvenCharge(t *testing.T) {
	options := rotationOptions(t)
	ops, _ := failingUnlinkOps()
	realm, err := openRealm(options, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-fatal-eio", 1)
	successor := rotationKey("$rotation-fatal-eio", 2)
	admitRotationPredecessor(t, realm, predecessor)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := realm.Append(successor, []byte("fatal-eio-survivor")); err != nil {
		t.Fatal(err)
	}
	logicalBefore := realm.total
	physicalBefore, _, _ := realm.PhysicalBudget()
	reservation.Fatal()
	capacity.Release()
	if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
		t.Fatalf("fatal EIO leaked reservations logical=%d physical=%d", realm.reservedCharge, realm.physicalReserved)
	}
	if available := realm.AvailableCompletePaneSlots(); available != 1 {
		t.Fatalf("fatal EIO did not retire both sealed generations: available=%d", available)
	}
	retained := realm.panes[successor]
	if retained == nil || !retained.slotRetired || retained.state != EligibilityUntrusted {
		t.Fatalf("fatal EIO did not retain a fail-closed tombstone: %#v", retained)
	}
	if realm.total != logicalBefore || realm.physicalTotal != physicalBefore {
		t.Fatalf("fatal EIO refunded unproven bytes logical=%d/%d physical=%d/%d", realm.total, logicalBefore, realm.physicalTotal, physicalBefore)
	}
	requireRetentionPathPresent(t, storagePathsForTest(options, successor).Journal)
}

func TestRotationFatalUnlinkENOENTIsProof(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-fatal-enoent", 1)
	successor := rotationKey("$rotation-fatal-enoent", 2)
	admitRotationPredecessor(t, realm, predecessor)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	path := storagePathsForTest(options, successor).Journal
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reservation.Fatal()
	capacity.Release()
	pane := realm.panes[successor]
	if pane == nil || !pane.slotRetired || pane.realmCharge != 0 || pane.physical != 0 {
		t.Fatalf("fatal ENOENT did not prove zero-charge retirement: %#v", pane)
	}
	if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
		t.Fatalf("fatal ENOENT leaked reservations logical=%d physical=%d", realm.reservedCharge, realm.physicalReserved)
	}
}

func TestRotationAbortKeepsRollbackCreditUntilExplicitSettlement(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-abort-credit", 1)
	successor := rotationKey("$rotation-abort-credit", 2)
	admitRotationPredecessor(t, realm, predecessor)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	reservation.Abort()
	if _, err := realm.BeginRotationCapacity(predecessor); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("unsettled rollback credit allowed another rotation: %v", err)
	}
	if _, err := realm.Append(predecessor, make([]byte, rotationPendingCapBytes)); err != nil {
		t.Fatalf("spend rollback credit: %v", err)
	}
	capacity.Release()
	capacity.Release()
	pane := realm.panes[predecessor]
	if pane == nil || pane.reservedCharge != 0 || pane.physicalReserved != 0 ||
		pane.rotationCapacityHeld || pane.rotationRollbackLogical != 0 ||
		pane.rotationRollbackPhysical != 0 || pane.rotationRollbackRecords != 0 {
		t.Fatalf("rollback settlement leaked pane capacity: %#v", pane)
	}
	if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
		t.Fatalf("rollback settlement leaked realm capacity logical=%d physical=%d", realm.reservedCharge, realm.physicalReserved)
	}
	retry, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatalf("settled predecessor refused next rotation: %v", err)
	}
	retry.Release()
}

func TestRotationRollbackBeyondR1UsesOrdinaryRoom(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-rollback-overflow", 1)
	admitRotationPredecessor(t, realm, predecessor)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	if err := capacity.ArmRollback(); err != nil {
		t.Fatalf("arm rollback: %v", err)
	}
	if err := capacity.ArmRollback(); err != nil {
		t.Fatalf("idempotent arm: %v", err)
	}
	if reservation, err := realm.BeginRotatedPane(rotationKey("$rotation-rollback-overflow", 2), Geometry{Columns: 80, Rows: 24}, capacity); !errors.Is(err, ErrInvalidated) || reservation != nil {
		t.Fatalf("armed rollback capacity materialized a successor: reservation=%v err=%v", reservation, err)
	}
	if _, err := realm.Append(predecessor, make([]byte, rotationPendingCapBytes+1)); err != nil {
		t.Fatalf("rollback plus one ordinary byte: %v", err)
	}
	capacity.Release()
	if charged, _ := realm.PaneLogical(predecessor); charged != rotationPendingCapBytes+1 {
		t.Fatalf("rollback charge=%d want=%d", charged, rotationPendingCapBytes+1)
	}
	if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
		t.Fatalf("overflow settlement leaked reservations logical=%d physical=%d", realm.reservedCharge, realm.physicalReserved)
	}
}

func TestRotationReplayShapeExportsAndRecordCapRefusesWithoutInvalidation(t *testing.T) {
	if AdoptionBootstrapCapBytes != 2<<20 || AdoptionBootstrapCapRecords != 32 ||
		RotationPendingCapBytes != 1<<20 || RotationPendingCapRecords != 16 {
		t.Fatalf("exported replay shape drifted: bootstrap=%d/%d pending=%d/%d",
			AdoptionBootstrapCapBytes, AdoptionBootstrapCapRecords,
			RotationPendingCapBytes, RotationPendingCapRecords)
	}
	options := rotationOptions(t)
	options.CompletePaneSlots = 3
	options.PhysicalCapBytes = 8 << 20
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-record-cap", 1)
	successor := rotationKey("$rotation-record-cap", 2)
	filler := rotationKey("$rotation-record-filler", 1)
	admitRotationPredecessor(t, realm, predecessor)
	admitRotationPredecessor(t, realm, filler)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	charged, reserved, cap := realm.PhysicalBudget()
	slack := cap - charged - reserved
	if slack < int64(appendFixed+commitFixed) {
		t.Fatalf("no physical slack to consume: %d", slack)
	}
	if _, err := realm.Append(filler, make([]byte, slack-int64(appendFixed+commitFixed))); err != nil {
		t.Fatalf("consume physical slack: %v", err)
	}
	chunk := make([]byte, 64<<10)
	for index := int64(0); index < adoptionBootstrapCapRecords; index++ {
		if _, err := realm.Append(successor, chunk); err != nil {
			t.Fatalf("bootstrap record %d: %v", index, err)
		}
	}
	for index := 0; index < 15; index++ {
		if _, err := realm.Append(successor, chunk); err != nil {
			t.Fatalf("tail record %d: %v", index, err)
		}
	}
	if _, err := realm.Append(successor, make([]byte, 32<<10)); err != nil {
		t.Fatalf("tail record 16: %v", err)
	}
	if _, err := realm.Append(successor, make([]byte, 32<<10)); !errors.Is(err, ErrRotationReplayRecordQuota) || !errors.Is(err, ErrQuota) {
		t.Fatalf("17-record tail refusal=%v", err)
	}
	if !realm.UnifiedEligible(successor) {
		t.Fatalf("record-shape refusal invalidated successor: %v", realm.Reason(successor))
	}
	reservation.Abort()
	capacity.Release()
}

func TestRotationAbortUnlinkFailureRetainsOnlyProvenBytes(t *testing.T) {
	options := rotationOptions(t)
	ops, failing := failingUnlinkOps()
	realm, err := openRealm(options, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-abort-eio", 1)
	successor := rotationKey("$rotation-abort-eio", 2)
	admitRotationPredecessor(t, realm, predecessor)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := realm.Append(successor, []byte("surviving-bytes")); err != nil {
		t.Fatal(err)
	}
	logicalBefore := realm.total
	physicalBefore, _, _ := realm.PhysicalBudget()
	reservation.Abort()
	capacity.Release()
	if available := realm.AvailableCompletePaneSlots(); available != 0 {
		t.Fatalf("unlink failure retained successor slot: available=%d", available)
	}
	retained := realm.panes[successor]
	if retained == nil || !retained.slotRetired || retained.state != EligibilityUntrusted {
		t.Fatalf("unlink failure did not retain retired tombstone: %#v", retained)
	}
	if realm.total != logicalBefore || realm.physicalTotal != physicalBefore {
		t.Fatalf("unlink failure refunded unproven bytes logical=%d/%d physical=%d/%d", realm.total, logicalBefore, realm.physicalTotal, physicalBefore)
	}
	requireRetentionPathPresent(t, storagePathsForTest(options, successor).Journal)
	*failing = false
	retryKey := rotationKey("$rotation-abort-eio", 3)
	retryCapacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatalf("retry capacity: %v", err)
	}
	retry, err := realm.BeginRotatedPane(retryKey, Geometry{Columns: 80, Rows: 24}, retryCapacity)
	if err != nil {
		t.Fatalf("retry materialize: %v", err)
	}
	retry.Commit()
	requireRetentionPathAbsent(t, storagePathsForTest(options, successor).Journal)
	if _, present := realm.panes[successor]; present {
		t.Fatal("later rotation did not sweep abort's retained successor")
	}
}

func TestRotationCommitUnlinkFailureRetiresSlotButRetainsCharges(t *testing.T) {
	options := rotationOptions(t)
	ops, failing := failingUnlinkOps()
	realm, err := openRealm(options, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-commit-eio", 1)
	successor := rotationKey("$rotation-commit-eio", 2)
	admitRotationPredecessor(t, realm, predecessor)
	appendCommittedOutput(t, realm, predecessor, []byte("old-charge"))
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	logicalBefore := realm.total
	physicalBefore, _, _ := realm.PhysicalBudget()
	reservation.Commit()
	if available := realm.AvailableCompletePaneSlots(); available != 0 {
		t.Fatalf("commit did not retire predecessor slot: available=%d", available)
	}
	retained := realm.panes[predecessor]
	if retained == nil || !retained.slotRetired {
		t.Fatalf("failed predecessor unlink not retained retired: %#v", retained)
	}
	if realm.total != logicalBefore || realm.physicalTotal != physicalBefore {
		t.Fatalf("failed predecessor unlink refunded charges logical=%d/%d physical=%d/%d", realm.total, logicalBefore, realm.physicalTotal, physicalBefore)
	}
	if !realm.UnifiedEligible(successor) {
		t.Fatal("successor not live after predecessor unlink failure")
	}
	*failing = false
	retryKey := rotationKey("$rotation-commit-eio", 3)
	retryCapacity, err := realm.BeginRotationCapacity(successor)
	if err != nil {
		t.Fatalf("retry capacity: %v", err)
	}
	retry, err := realm.BeginRotatedPane(retryKey, Geometry{Columns: 80, Rows: 24}, retryCapacity)
	if err != nil {
		t.Fatalf("retry materialize: %v", err)
	}
	retry.Commit()
	requireRetentionPathAbsent(t, storagePathsForTest(options, predecessor).Journal)
	if _, present := realm.panes[predecessor]; present {
		t.Fatal("later rotation did not sweep retained predecessor")
	}
}

func TestRotationN1FiftySwapsConserveSlotsAndCaps(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	current := rotationKey("$rotation-n1", 1)
	admitRotationPredecessor(t, realm, current)
	for generation := uint64(2); generation <= 51; generation++ {
		next := rotationKey("$rotation-n1", generation)
		capacity, err := realm.BeginRotationCapacity(current)
		if err != nil {
			t.Fatalf("rotation %d capacity: %v", generation-1, err)
		}
		reservation, err := realm.BeginRotatedPane(next, Geometry{Columns: 80, Rows: 24}, capacity)
		if err != nil {
			t.Fatalf("rotation %d materialize: %v", generation-1, err)
		}
		appendCommittedOutput(t, realm, next, []byte{byte(generation)})
		reservation.Commit()
		if available := realm.AvailableCompletePaneSlots(); available != 0 {
			t.Fatalf("rotation %d ordinary availability=%d want 0", generation-1, available)
		}
		if len(realm.panes) != 1 {
			t.Fatalf("rotation %d pane count=%d want 1", generation-1, len(realm.panes))
		}
		charged, cap := realm.PaneLogical(next)
		if charged > cap || realm.total > realm.options.RealmCapBytes {
			t.Fatalf("rotation %d exceeded logical cap pane=%d/%d realm=%d/%d", generation-1, charged, cap, realm.total, realm.options.RealmCapBytes)
		}
		current = next
	}
}

// rotationLedgersWithinCaps is the ledger-vs-cap invariant: charged plus
// reserved never exceeds the cap, on the realm and on every pane, in both
// units. Reserved is counted because a reservation is a promise the realm has
// already made; a ledger that is under cap only because it forgot a promise is
// exactly the overdraw C6 removed.
func rotationLedgersWithinCaps(t *testing.T, realm *Realm, stage string) {
	t.Helper()
	if realm.total+realm.reservedCharge > realm.options.RealmCapBytes {
		t.Fatalf("%s: realm logical ledger exceeds cap: total=%d reserved=%d cap=%d", stage, realm.total, realm.reservedCharge, realm.options.RealmCapBytes)
	}
	if charged, reserved, cap := realm.PhysicalBudget(); charged+reserved > cap {
		t.Fatalf("%s: realm physical ledger exceeds cap: charged=%d reserved=%d cap=%d", stage, charged, reserved, cap)
	}
	for key, pane := range realm.panes {
		if pane.realmCharge+pane.reservedCharge > realm.options.PaneCapBytes {
			t.Fatalf("%s: pane %v logical ledger exceeds cap: charged=%d reserved=%d cap=%d", stage, key, pane.realmCharge, pane.reservedCharge, realm.options.PaneCapBytes)
		}
		if pane.physical+pane.physicalReserved > realm.panePhysicalCap {
			t.Fatalf("%s: pane %v physical ledger exceeds cap: charged=%d reserved=%d cap=%d", stage, key, pane.physical, pane.physicalReserved, realm.panePhysicalCap)
		}
	}
}

func rotationSurvivingFileBytes(t *testing.T, options OpenOptions, keys ...PaneKey) int64 {
	t.Helper()
	var sum int64
	for _, key := range keys {
		info, err := os.Stat(storagePathsForTest(options, key).Journal)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		sum += info.Size()
	}
	return sum
}

// Every settlement path of a rotation capacity, under both a tight logical
// cap and a tight physical cap, with a competitor that takes every byte the
// realm reports free between the boundary and the 4R replay: the ledgers
// never exceed their caps at any step, the replay R1 guarantees is admitted,
// and each settlement leaves no reservation behind and a physical ledger equal
// to the surviving files.
func TestRotationLedgersNeverExceedCapsAcrossSettlementMatrix(t *testing.T) {
	type settlement struct {
		name        string
		materialize bool
		arm         bool  // pre-materialization ArmRollback
		abort       bool  // materialized: Abort (otherwise Commit)
		replay      int64 // bytes replayed into the predecessor after arm/abort
	}
	settlements := []settlement{
		{name: "held-release"},
		{name: "arm-replay-full", arm: true, replay: rotationPendingCapBytes},
		{name: "arm-replay-partial", arm: true, replay: rotationPendingCapBytes / 2},
		{name: "arm-release-unspent", arm: true},
		{name: "abort-replay-full", materialize: true, abort: true, replay: rotationPendingCapBytes},
		{name: "abort-replay-partial", materialize: true, abort: true, replay: rotationPendingCapBytes / 2},
		{name: "abort-release-unspent", materialize: true, abort: true},
		{name: "commit", materialize: true},
	}
	type budget struct {
		name   string
		filled int64
		adjust func(*OpenOptions)
	}
	budgets := []budget{
		{name: "logical-tight", filled: 7 << 20, adjust: func(options *OpenOptions) {
			// Realm cap = predecessor + R1 + R2 + R3 + 16 bytes of slack.
			options.RealmCapBytes = 7<<20 + rotationPendingCapBytes + adoptionBootstrapCapBytes + rotationPendingCapBytes + 16
		}},
		{name: "physical-tight", filled: 64 << 10, adjust: func(options *OpenOptions) {
			options.PhysicalCapBytes = 8 << 20
		}},
	}
	for _, budget := range budgets {
		for _, settle := range settlements {
			t.Run(budget.name+"/"+settle.name, func(t *testing.T) {
				options := rotationOptions(t)
				options.CompletePaneSlots = 3
				budget.adjust(&options)
				realm, err := openRealm(options, realJournalOps())
				if err != nil {
					t.Fatal(err)
				}
				defer realm.Close()
				predecessor := rotationKey("$rotation-ledger-matrix", 1)
				successor := rotationKey("$rotation-ledger-matrix", 2)
				competitor := rotationKey("$rotation-ledger-competitor", 1)
				keys := []PaneKey{predecessor, successor, competitor}
				admitRotationPredecessor(t, realm, predecessor)
				admitRotationPredecessor(t, realm, competitor)
				appendCommittedOutput(t, realm, predecessor, make([]byte, budget.filled))
				baseSlots := realm.retention.reserved
				rotationLedgersWithinCaps(t, realm, "before capacity")

				capacity, err := realm.BeginRotationCapacity(predecessor)
				if err != nil {
					t.Fatalf("capacity: %v", err)
				}
				rotationLedgersWithinCaps(t, realm, "after capacity")
				var reservation *RotationReservation
				if settle.materialize {
					reservation, err = realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
					if err != nil {
						t.Fatalf("materialize: %v", err)
					}
					rotationLedgersWithinCaps(t, realm, "after materialize")
				}
				if settle.arm {
					if err := capacity.ArmRollback(); err != nil {
						t.Fatalf("arm: %v", err)
					}
					rotationLedgersWithinCaps(t, realm, "after arm")
				}
				if settle.abort {
					reservation.Abort()
					rotationLedgersWithinCaps(t, realm, "after abort")
				}
				// The competitor takes every byte of ordinary room the realm reports
				// free in the binding unit, so any credit that is not truly backed by a
				// reservation is now unbacked.
				logicalRoom := options.RealmCapBytes - realm.total - realm.reservedCharge
				charged, reserved, cap := realm.PhysicalBudget()
				physicalRoom := cap - charged - reserved - int64(appendFixed+commitFixed)
				room := logicalRoom
				if physicalRoom < room {
					room = physicalRoom
				}
				if room <= 0 {
					t.Fatalf("no room to contest: logical=%d physical=%d", logicalRoom, physicalRoom)
				}
				// Every append is committed so the physical ledger, which charges the
				// commit frame up front, can be compared with the files at settlement.
				appendCommittedOutput(t, realm, competitor, make([]byte, room))
				rotationLedgersWithinCaps(t, realm, "after competitor")
				if settle.replay > 0 {
					record, err := realm.Append(predecessor, make([]byte, settle.replay))
					if err != nil {
						t.Fatalf("4R replay of %d bytes refused: %v", settle.replay, err)
					}
					if err := realm.Sync(predecessor); err != nil {
						t.Fatal(err)
					}
					if err := realm.AdvanceCommitted(predecessor, record); err != nil {
						t.Fatal(err)
					}
					rotationLedgersWithinCaps(t, realm, "after replay")
				}
				if settle.materialize && !settle.abort {
					appendCommittedOutput(t, realm, successor, make([]byte, adoptionBootstrapCapBytes/2))
					rotationLedgersWithinCaps(t, realm, "after successor bootstrap")
					reservation.Commit()
				}
				capacity.Release()
				rotationLedgersWithinCaps(t, realm, "after settlement")
				if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
					t.Fatalf("settlement left reservations: logical=%d physical=%d", realm.reservedCharge, realm.physicalReserved)
				}
				if charged, _, _ := realm.PhysicalBudget(); charged != rotationSurvivingFileBytes(t, options, keys...) {
					t.Fatalf("settlement: physical ledger=%d files=%d", charged, rotationSurvivingFileBytes(t, options, keys...))
				}
				if !realm.UnifiedEligible(competitor) {
					t.Fatalf("competitor lost eligibility: %v", realm.Reason(competitor))
				}
				live := predecessor
				wantSlots := baseSlots
				if settle.materialize && !settle.abort {
					live = successor
				}
				if !realm.UnifiedEligible(live) {
					t.Fatalf("live generation %v not eligible: %v", live, realm.Reason(live))
				}
				if realm.retention.reserved != wantSlots {
					t.Fatalf("settlement slots=%d want=%d", realm.retention.reserved, wantSlots)
				}
			})
		}
	}
}

func TestRotationN13AtomicallyPreservesLastSlot(t *testing.T) {
	options := rotationOptions(t)
	options.CompletePaneSlots = 3
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	predecessor := rotationKey("$rotation-n13", 1)
	admitRotationPredecessor(t, realm, predecessor)

	start := make(chan struct{})
	type result struct {
		kind      string
		adoption  *AdoptionReservation
		capacity  *RotationCapacity
		err       error
		committed bool
	}
	results := make(chan result, 4)
	var wg sync.WaitGroup
	for index := 0; index < 2; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			reservation, err := realm.BeginReconstructedPane(rotationKey("$rotation-n13-adopt-"+string(rune('a'+index)), 1), Geometry{Columns: 80, Rows: 24})
			results <- result{kind: "adoption", adoption: reservation, err: err, committed: err == nil}
		}(index)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		err := realm.AdmitPane(rotationKey("$rotation-n13-birth", 1), Geometry{Columns: 80, Rows: 24})
		results <- result{kind: "birth", err: err, committed: err == nil}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		capacity, err := realm.BeginRotationCapacity(predecessor)
		results <- result{kind: "rotation", capacity: capacity, err: err}
	}()
	close(start)
	wg.Wait()
	close(results)
	nonRotationWinners := 0
	var rotation *RotationCapacity
	for outcome := range results {
		if outcome.kind == "rotation" {
			if outcome.err != nil {
				t.Fatalf("rotation lost reserved last slot: %v", outcome.err)
			}
			rotation = outcome.capacity
			continue
		}
		if outcome.committed {
			nonRotationWinners++
		}
		if outcome.adoption != nil {
			outcome.adoption.Abort()
		}
	}
	if nonRotationWinners > 1 {
		t.Fatalf("non-rotation admissions over-granted final reserve: winners=%d", nonRotationWinners)
	}
	rotation.Release()
	if err := realm.Close(); err != nil {
		t.Fatal(err)
	}

	options.BrokerIncarnation = "broker-incarnation-after-race"
	reopened, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if available := reopened.AvailableCompletePaneSlots(); available != options.CompletePaneSlots-1 {
		t.Fatalf("restart did not rebuild retired slot ledger: available=%d want=%d", available, options.CompletePaneSlots-1)
	}
}

func TestEveryNonRotationAdmissionPreservesFinalSlot(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-last-slot", 1)
	admitRotationPredecessor(t, realm, predecessor)
	if _, err := realm.BeginReconstructedPane(rotationKey("$denied-adoption", 1), Geometry{Columns: 80, Rows: 24}); !errors.Is(err, ErrQuota) {
		t.Fatalf("reconstructed admission used final slot: %v", err)
	}
	if err := realm.AdmitPane(rotationKey("$denied-birth", 1), Geometry{Columns: 80, Rows: 24}); !errors.Is(err, ErrQuota) {
		t.Fatalf("birth admission used final slot: %v", err)
	}
	if _, err := realm.Append(rotationKey("$denied-implicit", 1), []byte("x")); !errors.Is(err, ErrQuota) {
		t.Fatalf("implicit append admission used final slot: %v", err)
	}
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatalf("rotation could not use final slot: %v", err)
	}
	capacity.Release()
}

func TestRotatedOriginReopensAndUnknownOriginFailsClosed(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	predecessor := rotationKey("$rotation-origin", 1)
	successor := rotationKey("$rotation-origin", 2)
	admitRotationPredecessor(t, realm, predecessor)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	appendCommittedOutput(t, realm, successor, []byte("rotated"))
	reservation.Commit()
	if err := realm.Close(); err != nil {
		t.Fatal(err)
	}
	options.BrokerIncarnation = "broker-incarnation-reopen"
	reopened, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if origin, err := reopened.Origin(successor); err != nil || origin != OriginRotated {
		t.Fatalf("reopened origin=%q err=%v", origin, err)
	}
	charged, cap := reopened.PaneLogical(successor)
	if charged != int64(len("rotated")) || charged > cap {
		t.Fatalf("reopened logical=%d/%d want=%d", charged, cap, len("rotated"))
	}

	// The origin field remains a closed set. This is the same switch by which
	// a pre-rotation binary rejects `rotated`: no unknown value is guessed into
	// birth or reconstruction.
	unknownHeader, err := json.Marshal(journalHeader{
		Version: journalVersion, Key: rotationKey("$unknown-origin", 1),
		BrokerIncarnation: "broker-incarnation-unknown", GeometryInitial: &Geometry{Columns: 80, Rows: 24},
		Origin: GenerationOrigin("future-origin"),
	})
	if err != nil {
		t.Fatal(err)
	}
	unknown := make([]byte, 8+len(unknownHeader))
	copy(unknown[:4], journalMagic[:])
	binary.BigEndian.PutUint32(unknown[4:8], uint32(len(unknownHeader)))
	copy(unknown[8:], unknownHeader)
	if pane, err := parseJournal(unknown); !errors.Is(err, ErrCorruptJournal) || pane == nil {
		t.Fatalf("unknown origin did not fail closed: pane=%#v err=%v", pane, err)
	}
}

func TestRotationRepeatedReopenCannotMintLogicalCapacity(t *testing.T) {
	options := rotationOptions(t)
	options.RealmCapBytes = 5 << 20
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	predecessor := rotationKey("$rotation-reopen", 1)
	successor := rotationKey("$rotation-reopen", 2)
	admitRotationPredecessor(t, realm, predecessor)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 1<<20)
	appendCommittedOutput(t, realm, successor, payload)
	reservation.Commit()
	if err := realm.Close(); err != nil {
		t.Fatal(err)
	}

	for generation := 0; generation < 3; generation++ {
		options.BrokerIncarnation = "broker-incarnation-reopen-" + string(rune('a'+generation))
		reopened, err := openRealm(options, realJournalOps())
		if err != nil {
			t.Fatal(err)
		}
		charged, _ := reopened.PaneLogical(successor)
		if charged != int64(len(payload)) || reopened.total != int64(len(payload)) {
			t.Fatalf("reopen %d minted capacity pane=%d realm=%d", generation, charged, reopened.total)
		}
		freshKey := rotationKey("$rotation-reopen-cap", uint64(generation+10))
		admission, err := reopened.BeginReconstructedPane(freshKey, Geometry{Columns: 80, Rows: 24})
		if err != nil {
			t.Fatalf("reopen %d cap probe admission: %v", generation, err)
		}
		if _, err := reopened.Append(freshKey, make([]byte, (4<<20)+1)); !errors.Is(err, ErrQuota) {
			t.Fatalf("reopen %d minted realm capacity: append err=%v want quota", generation, err)
		}
		admission.Abort()
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRotationCapacityMaterializationFailureKeepsCapacityReleasable(t *testing.T) {
	options := rotationOptions(t)
	ops := realJournalOps()
	realOpenat := ops.openat
	failCreate := false
	ops.openat = func(fd int, path string, flags int, mode uint32) (int, error) {
		if failCreate && flags&syscall.O_CREAT != 0 {
			return -1, syscall.EIO
		}
		return realOpenat(fd, path, flags, mode)
	}
	realm, err := openRealm(options, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-materialize", 1)
	admitRotationPredecessor(t, realm, predecessor)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	failCreate = true
	if reservation, err := realm.BeginRotatedPane(rotationKey("$rotation-materialize", 2), Geometry{Columns: 80, Rows: 24}, capacity); err == nil || reservation != nil {
		t.Fatalf("materialization failure returned reservation=%v err=%v", reservation, err)
	}
	capacity.Release()
	if available := realm.AvailableCompletePaneSlots(); available != 0 {
		t.Fatalf("failed materialization leaked slot: available=%d", available)
	}
	if !realm.UnifiedEligible(predecessor) {
		t.Fatal("failed materialization changed predecessor")
	}
	if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
		t.Fatalf("failed materialization leaked capacity logical=%d physical=%d", realm.reservedCharge, realm.physicalReserved)
	}
}

// P1B-R1 (correction C7). A successor header that fails to write is charged
// before the write and refunded only when the cleanup unlink proves the bytes
// gone. When the unlink also fails, the retained charge must be converted out
// of the header hold the capacity still carries, inside the same sequenced
// operation, so the same bytes are never both charged and reserved. The
// capacity is single-use for materialization after any storage attempt, and
// stays eligible for the F5 path: ArmRollback, bounded replay, Release.
func TestRotationFailedMaterializationKeepsPhysicalLedgerWithinCap(t *testing.T) {
	cases := []struct {
		name      string
		land      bool // the header reaches the file before the write reports failure
		torn      bool // only half of the header reaches the file
		unlinkEIO bool // cleanup unlink fails, so the bytes cannot be proven gone
	}{
		{name: "header-write-fails-unlink-proves-gone"},
		{name: "header-landed-unlink-eio", land: true, unlinkEIO: true},
		{name: "header-torn-unlink-eio", torn: true, unlinkEIO: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options := rotationOptions(t)
			ops := realJournalOps()
			predecessor := rotationKey("$rotation-failed-materialize", 1)
			successor := rotationKey("$rotation-failed-materialize", 2)
			successorName := paneFileName(successor)
			realWrite, realUnlink := ops.write, ops.unlinkat
			failHeader, failUnlink := false, false
			headerSize := int64(0)
			ops.write = func(file *os.File, data []byte) (int, error) {
				if !failHeader || file.Name() != successorName {
					return realWrite(file, data)
				}
				if headerSize == 0 {
					headerSize = int64(len(data))
				}
				switch {
				case tc.land:
					realWrite(file, data)
				case tc.torn:
					realWrite(file, data[:len(data)/2])
				}
				return 0, syscall.EIO
			}
			ops.unlinkat = func(fd int, name string) error {
				if failUnlink && name == successorName {
					return syscall.EIO
				}
				return realUnlink(fd, name)
			}
			realm, err := openRealm(options, ops)
			if err != nil {
				t.Fatal(err)
			}
			defer realm.Close()
			admitRotationPredecessor(t, realm, predecessor)
			appendCommittedOutput(t, realm, predecessor, make([]byte, 4<<10))
			// Exactly S+R1+R2+R3 plus one ordinary one-byte append of physical room.
			chargedBefore, _, _ := realm.PhysicalBudget()
			rollbackPhysical := rotationReplayPhysical(rotationPendingCapBytes, rotationPendingCapRecords)
			successorPhysical := rotationHeaderPhysicalReserve +
				rotationReplayPhysical(adoptionBootstrapCapBytes, adoptionBootstrapCapRecords) +
				rotationReplayPhysical(rotationPendingCapBytes, rotationPendingCapRecords)
			realm.physicalCap = chargedBefore + rollbackPhysical + successorPhysical + physicalAppendCost(1)
			baseSlots := realm.retention.reserved
			capacity, err := realm.BeginRotationCapacity(predecessor)
			if err != nil {
				t.Fatalf("capacity with exact room refused: %v", err)
			}
			_, reservedBefore, _ := realm.PhysicalBudget()

			failHeader, failUnlink = true, tc.unlinkEIO
			reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
			failHeader = false
			if reservation != nil || !errors.Is(err, ErrStorage) {
				t.Fatalf("failed materialization returned reservation=%v err=%v", reservation, err)
			}
			if headerSize == 0 {
				t.Fatal("header write was never attempted")
			}
			charged, reserved, cap := realm.PhysicalBudget()
			wantRetained := int64(0)
			if tc.unlinkEIO {
				wantRetained = headerSize
			}
			if retained := charged - chargedBefore; retained != wantRetained {
				t.Fatalf("retained physical charge=%d want=%d (unlink proof=%v)", retained, wantRetained, !tc.unlinkEIO)
			}
			if charged+reserved > cap {
				t.Fatalf("failed materialization crossed physical cap: used=%d cap=%d excess=%d", charged+reserved, cap, charged+reserved-cap)
			}
			if reserved != reservedBefore-wantRetained || capacity.successorPhysical != successorPhysical-wantRetained {
				t.Fatalf("header hold not converted by the retained charge: realm reserved=%d want=%d capacity successor=%d want=%d", reserved, reservedBefore-wantRetained, capacity.successorPhysical, successorPhysical-wantRetained)
			}
			// The one ordinary byte the predecessor owns is still appendable before 4R.
			appendCommittedOutput(t, realm, predecessor, []byte{1})
			if !realm.UnifiedEligible(predecessor) {
				t.Fatalf("predecessor lost eligibility: %v", realm.Reason(predecessor))
			}
			// A storage attempt is single-use, whatever key the retry names.
			if retry, err := realm.BeginRotatedPane(rotationKey("$rotation-failed-materialize", 3), Geometry{Columns: 80, Rows: 24}, capacity); retry != nil || !errors.Is(err, ErrInvalidated) {
				t.Fatalf("second materialization attempt on a spent capacity: reservation=%v err=%v", retry, err)
			}
			// F5: arm R1, replay the bounded tail into the predecessor, release the rest.
			if err := capacity.ArmRollback(); err != nil {
				t.Fatalf("arm after failed materialization: %v", err)
			}
			record, err := realm.Append(predecessor, make([]byte, rotationPendingCapBytes))
			if err != nil {
				t.Fatalf("4R replay refused after failed materialization: %v", err)
			}
			if err := realm.Sync(predecessor); err != nil {
				t.Fatal(err)
			}
			if err := realm.AdvanceCommitted(predecessor, record); err != nil {
				t.Fatal(err)
			}
			capacity.Release()
			capacity.Release()
			rotationLedgersWithinCaps(t, realm, "after release")
			if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
				t.Fatalf("release left reservations logical=%d physical=%d", realm.reservedCharge, realm.physicalReserved)
			}
			if realm.retention.reserved != baseSlots {
				t.Fatalf("release slots=%d want=%d", realm.retention.reserved, baseSlots)
			}
			if !realm.UnifiedEligible(predecessor) {
				t.Fatalf("predecessor not live after settlement: %v", realm.Reason(predecessor))
			}
			charged, _, _ = realm.PhysicalBudget()
			files := rotationSurvivingFileBytes(t, options, predecessor, successor)
			if wantCharged := chargedBefore + wantRetained + physicalAppendCost(1) + physicalAppendCost(int(rotationPendingCapBytes)); charged != wantCharged {
				t.Fatalf("settled physical ledger=%d want=%d", charged, wantCharged)
			}
			if charged < files || (!tc.torn && charged != files) {
				t.Fatalf("settled physical ledger=%d files=%d (torn=%v)", charged, files, tc.torn)
			}
			// A reopen rebuilds the physical ledger from the surviving files only. A
			// torn orphan header cannot be attributed to a key, so the scan refuses
			// the realm rather than guess; either way nothing is minted.
			if err := realm.Close(); err != nil {
				t.Fatal(err)
			}
			options.BrokerIncarnation = "broker-incarnation-failed-materialize-reopen"
			reopened, err := openRealm(options, realJournalOps())
			if err != nil {
				if !tc.torn {
					t.Fatalf("reopen after failed materialization: %v", err)
				}
				return
			}
			defer reopened.Close()
			if rebuilt, _, _ := reopened.PhysicalBudget(); rebuilt != files {
				t.Fatalf("reopen rebuilt physical ledger=%d files=%d", rebuilt, files)
			}
		})
	}
}

func TestRotationCapacityRefusalIsAtomicAcrossLogicalAndPhysicalBudgets(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *Realm, PaneKey)
		options func(*testing.T) OpenOptions
		want    error
	}{
		{
			name:    "R1-logical",
			options: func(t *testing.T) OpenOptions { return rotationOptions(t) },
			prepare: func(t *testing.T, realm *Realm, key PaneKey) {
				if _, err := realm.Append(key, make([]byte, (7<<20)+1)); err != nil {
					t.Fatalf("fill predecessor: %v", err)
				}
			},
			want: ErrQuota,
		},
		{
			name: "R2-R3-physical",
			options: func(t *testing.T) OpenOptions {
				options := rotationOptions(t)
				options.PanePhysicalCapBytes = 3 << 20
				return options
			},
			prepare: func(*testing.T, *Realm, PaneKey) {},
			want:    ErrPhysicalQuota,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := test.options(t)
			realm, err := openRealm(options, realJournalOps())
			if err != nil {
				t.Fatal(err)
			}
			defer realm.Close()
			predecessor := rotationKey("$rotation-refusal-"+test.name, 1)
			admitRotationPredecessor(t, realm, predecessor)
			test.prepare(t, realm, predecessor)
			logicalBefore := realm.reservedCharge
			physicalBefore := realm.physicalReserved
			if capacity, err := realm.BeginRotationCapacity(predecessor); !errors.Is(err, test.want) || capacity != nil {
				t.Fatalf("capacity=%v err=%v want=%v", capacity, err, test.want)
			}
			if realm.reservedCharge != logicalBefore || realm.physicalReserved != physicalBefore {
				t.Fatalf("refusal mutated reservations logical=%d/%d physical=%d/%d", realm.reservedCharge, logicalBefore, realm.physicalReserved, physicalBefore)
			}
			if realm.panes[predecessor].rotationCapacityHeld {
				t.Fatal("refusal left predecessor capacity ownership")
			}
			if available := realm.AvailableCompletePaneSlots(); available != 0 {
				t.Fatalf("refusal exposed or consumed ordinary capacity: available=%d", available)
			}
		})
	}
}

func TestRotationAbortUnlinkENOENTStillRefunds(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-abort-enoent", 1)
	successor := rotationKey("$rotation-abort-enoent", 2)
	admitRotationPredecessor(t, realm, predecessor)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	path := storagePathsForTest(options, successor).Journal
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reservation.Abort()
	capacity.Release()
	if pane := realm.panes[successor]; pane == nil || !pane.slotRetired || pane.realmCharge != 0 || pane.physical != 0 {
		t.Fatalf("ENOENT proof did not leave a zero-charge fail-closed tombstone: %#v", pane)
	}
}
