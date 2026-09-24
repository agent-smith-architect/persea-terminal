package unifiedjournal

import (
	"errors"
	"testing"
)

// Armed rollback credit must stay backed by the
// reservation until it is consumed or settled (packet 4R: "R1 guarantees
// both the capacity and the record count"). If arming releases the global
// reservation, a competitor can take the room and the replay then overdraws
// the realm caps — logical and physical.
func TestRotationArmedRollbackCreditStaysBackedByReservation(t *testing.T) {
	options := rotationOptions(t)
	options.CompletePaneSlots = 3
	const filled = 7 << 20
	// Logical realm cap = predecessor(7 MiB) + R1+R2+R3 (4 MiB) exactly.
	options.RealmCapBytes = filled + RotationPendingCapBytes + AdoptionBootstrapCapBytes + RotationPendingCapBytes
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-backed", 1)
	successor := rotationKey("$rotation-backed", 2)
	competitor := rotationKey("$rotation-backed-competitor", 1)
	admitRotationPredecessor(t, realm, predecessor)
	admitRotationPredecessor(t, realm, competitor)
	if _, err := realm.Append(predecessor, make([]byte, filled)); err != nil {
		t.Fatal(err)
	}
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	reservation.Abort() // arms R1
	// A competitor takes every byte of logical room the realm reports free.
	room := options.RealmCapBytes - realm.total - realm.reservedCharge
	if room <= 0 {
		t.Fatalf("no room to contest: %d", room)
	}
	if _, err := realm.Append(competitor, make([]byte, room)); err != nil {
		t.Fatalf("competitor append: %v", err)
	}
	// 4R replay from R1.
	_, replayErr := realm.Append(predecessor, make([]byte, RotationPendingCapBytes))
	capacity.Release()
	if realm.total > options.RealmCapBytes {
		t.Fatalf("realm logical ledger overdrawn: total=%d cap=%d (replay err=%v, competitor took %d)", realm.total, options.RealmCapBytes, replayErr, room)
	}
	if replayErr != nil {
		t.Fatalf("backed rollback replay refused: %v", replayErr)
	}
}

func TestRotationArmedRollbackCreditStaysBackedPhysically(t *testing.T) {
	options := rotationOptions(t)
	options.CompletePaneSlots = 3
	options.PhysicalCapBytes = 8 << 20
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-backed-phys", 1)
	successor := rotationKey("$rotation-backed-phys", 2)
	competitor := rotationKey("$rotation-backed-phys-competitor", 1)
	admitRotationPredecessor(t, realm, predecessor)
	admitRotationPredecessor(t, realm, competitor)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	reservation.Abort() // arms R1
	charged, reserved, cap := realm.PhysicalBudget()
	room := cap - charged - reserved
	if room <= int64(appendFixed+commitFixed) {
		t.Fatalf("no physical room to contest: %d", room)
	}
	if _, err := realm.Append(competitor, make([]byte, room-int64(appendFixed+commitFixed))); err != nil {
		t.Fatalf("competitor append: %v", err)
	}
	_, replayErr := realm.Append(predecessor, make([]byte, RotationPendingCapBytes))
	capacity.Release()
	if charged, _, cap := realm.PhysicalBudget(); charged > cap {
		t.Fatalf("realm physical ledger overdrawn: charged=%d cap=%d (replay err=%v)", charged, cap, replayErr)
	}
	if replayErr != nil {
		t.Fatalf("backed rollback replay refused: %v", replayErr)
	}
}

// After Abort, rollback credit remains reserved. The
// competitor may take exactly the non-R1 room; the replay then succeeds and
// the realm stays within its cap.
func TestRotationR1SurvivesAbortAndCompetitorTakesOnlyNonR1Room(t *testing.T) {
	options := rotationOptions(t)
	options.CompletePaneSlots = 4
	const filled = 7 << 20
	options.RealmCapBytes = filled + RotationPendingCapBytes + AdoptionBootstrapCapBytes + RotationPendingCapBytes + 16
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-r1-abort2", 1)
	successor := rotationKey("$rotation-r1-abort2", 2)
	control := rotationKey("$rotation-control2", 1)
	competitor := rotationKey("$rotation-competitor2", 1)
	admitRotationPredecessor(t, realm, predecessor)
	admitRotationPredecessor(t, realm, control)
	admitRotationPredecessor(t, realm, competitor)
	if _, err := realm.Append(predecessor, make([]byte, filled)); err != nil {
		t.Fatal(err)
	}
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := realm.Append(control, make([]byte, 17)); !errors.Is(err, ErrQuota) {
		t.Fatalf("control: competitor consumed reserved realm room while capacity live: %v", err)
	}
	reservation.Abort()
	// Non-R1 room after Abort: R2+R3 released (3 MiB) + 16 B slack.
	if _, err := realm.Append(competitor, make([]byte, AdoptionBootstrapCapBytes+RotationPendingCapBytes+16)); err != nil {
		t.Fatalf("competitor append of the non-R1 room: %v", err)
	}
	if _, err := realm.Append(competitor, []byte{1}); !errors.Is(err, ErrQuota) {
		t.Fatalf("competitor entered R1's room after Abort: %v", err)
	}
	if _, err := realm.Append(predecessor, make([]byte, RotationPendingCapBytes)); err != nil {
		t.Fatalf("rollback replay refused: %v", err)
	}
	capacity.Release()
	if realm.total > options.RealmCapBytes || realm.reservedCharge != 0 || realm.physicalReserved != 0 {
		t.Fatalf("after replay+release: total=%d cap=%d reserved=%d/%d", realm.total, options.RealmCapBytes, realm.reservedCharge, realm.physicalReserved)
	}
}

// T6 — coverage pin. ArmRollback discipline: idempotent; refused after
// materialization (Abort arms instead); pre-materialization arm then Release
// settles everything; a held-but-unarmed capacity cannot be armed by a stale
// object after Release.
func TestRotationArmRollbackDiscipline(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$rotation-arm", 1)
	admitRotationPredecessor(t, realm, predecessor)
	baseSlots, baseLogical, basePhysical := rotationLedger(realm)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	if err := capacity.ArmRollback(); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if err := capacity.ArmRollback(); err != nil {
		t.Fatalf("second arm: %v", err)
	}
	if _, err := realm.BeginRotatedPane(rotationKey("$rotation-arm", 2), Geometry{Columns: 80, Rows: 24}, capacity); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("materialized after arm: %v", err)
	}
	if _, err := realm.Append(predecessor, make([]byte, RotationPendingCapBytes/2)); err != nil {
		t.Fatalf("replay half of R1: %v", err)
	}
	capacity.Release()
	if err := capacity.ArmRollback(); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("stale arm after release: %v", err)
	}
	slots, logical, physical := rotationLedger(realm)
	pane := realm.panes[predecessor]
	if slots != baseSlots || logical != baseLogical || physical != basePhysical || pane.rotationCapacityHeld || pane.rotationRollbackArmed || pane.reservedCharge != 0 || pane.physicalReserved != 0 {
		t.Fatalf("arm+release drift: slots=%d/%d logical=%d/%d physical=%d/%d pane=%#v", slots, baseSlots, logical, baseLogical, physical, basePhysical, pane)
	}
	capacity, err = realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := realm.BeginRotatedPane(rotationKey("$rotation-arm", 3), Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if err := capacity.ArmRollback(); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("arm during active reservation: %v", err)
	}
	reservation.Abort()
	if err := capacity.ArmRollback(); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("arm after abort (already armed): %v", err)
	}
	capacity.Release()
}
