package unifiedjournal

import (
	"errors"
	"testing"
)

func reservationKey(session string, generation uint64) PaneKey {
	return PaneKey{
		Server: "server-a", Session: session, ControlGeneration: generation,
		Window: "@1", Pane: "%1", Incarnation: "incarnation-a",
	}
}

// TestAdoptionReservationAbortReleasesSlotAndArtifacts is F1's journal half
// for the failed-adoption leak: a reservation aborted before activation
// returns its slot, refunds the realm charge its bootstrap already took,
// removes the generation's file, and leaves a fail-closed tombstone so an
// append still in flight cannot re-create a birth generation at the key.
func TestAdoptionReservationAbortReleasesSlotAndArtifacts(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 2
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	geometry := Geometry{Columns: 80, Rows: 24}
	key := reservationKey("$1", 2)

	reservation, err := realm.BeginReconstructedPane(key, geometry)
	if err != nil {
		t.Fatalf("begin reconstructed: %v", err)
	}
	if available := realm.AvailableCompletePaneSlots(); available != 0 {
		t.Fatalf("provisional reservation left availability=%d want 0", available)
	}
	// The bootstrap lands before the failure, exactly as the broker journals
	// it between admission and activation.
	if _, err := realm.Append(key, []byte("bootstrap-bytes")); err != nil {
		t.Fatalf("bootstrap append under reservation: %v", err)
	}
	before := realm.total

	reservation.Abort()
	if available := realm.AvailableCompletePaneSlots(); available != 1 {
		t.Fatalf("aborted reservation left availability=%d want 1", available)
	}
	if realm.total != before-int64(len("bootstrap-bytes")) {
		t.Fatalf("aborted reservation left realm total=%d want %d", realm.total, before-int64(len("bootstrap-bytes")))
	}
	requireRetentionPathAbsent(t, storagePathsForTest(options, key).Journal)
	if _, err := realm.Append(key, []byte("late-flight")); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("late append at an aborted key: %v, want invalidated", err)
	}
	// Abort is idempotent and the slot is genuinely reusable.
	reservation.Abort()
	if available := realm.AvailableCompletePaneSlots(); available != 1 {
		t.Fatalf("double abort changed availability=%d want 1", available)
	}
	fresh := reservationKey("$1", 3)
	replacement, err := realm.BeginReconstructedPane(fresh, geometry)
	if err != nil {
		t.Fatalf("re-admission after abort: %v", err)
	}
	replacement.Commit()
	if !realm.UnifiedEligible(fresh) {
		t.Fatal("the recovered slot did not admit a live generation")
	}
}

// TestAdoptionReservationCommitSupersedesStaleGeneration is F1's journal half
// for the restart leak: a reconstructed generation reopened under a new
// broker incarnation is slot-retired at scan (the session stays adoptable
// with one ordinary-admission slot plus the rotation reserve), stays
// origin-honest until then, and is
// superseded — file unlinked, charge refunded — when a fresh reconstructed
// admission for the same session commits. A stale generation of a DIFFERENT
// session is untouched.
func TestAdoptionReservationCommitSupersedesStaleGeneration(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 3
	first, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	geometry := Geometry{Columns: 80, Rows: 24}
	staleKey := reservationKey("$1", 2)
	otherKey := reservationKey("$2", 2)
	for _, key := range []PaneKey{staleKey, otherKey} {
		reservation, err := first.BeginReconstructedPane(key, geometry)
		if err != nil {
			t.Fatalf("first-run admission of %q: %v", key.Session, err)
		}
		reservation.Commit()
		if _, err := first.Append(key, []byte("first-run-bootstrap")); err != nil {
			t.Fatalf("first-run bootstrap of %q: %v", key.Session, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	options.CompletePaneSlots = 2
	options.BrokerIncarnation = "broker-incarnation-b"
	reopened, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// Both stale generations are slot-retired at scan: with one configured
	// ordinary slot the ledger still has capacity, while the final configured
	// slot remains reserved for rotation.
	if available := reopened.AvailableCompletePaneSlots(); available != 1 {
		t.Fatalf("reopened availability=%d want 1", available)
	}
	if origin, err := reopened.Origin(staleKey); err != nil || origin != OriginReconstructed {
		t.Fatalf("stale origin=%q err=%v before supersession", origin, err)
	}

	freshKey := reservationKey("$1", 3)
	totalBefore := reopened.total
	reservation, err := reopened.BeginReconstructedPane(freshKey, geometry)
	if err != nil {
		t.Fatalf("re-adoption admission: %v", err)
	}
	// The same-session stale generation is superseded by proof
	// at Begin, before the successor exists — file, map entry, and charge —
	// and the refunded charge is held for this reservation until Commit
	// releases it (adoption_supersession_capacity_test.go pins the hold).
	requireRetentionPathAbsent(t, storagePathsForTest(options, staleKey).Journal)
	if _, err := reopened.Origin(staleKey); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("superseded stale origin err=%v, want invalidated", err)
	}
	if reopened.total != totalBefore-int64(len("first-run-bootstrap")) {
		t.Fatalf("supersession refunded to total=%d want %d", reopened.total, totalBefore-int64(len("first-run-bootstrap")))
	}
	if reopened.reservedCharge != int64(len("first-run-bootstrap")) {
		t.Fatalf("supersession held reserved=%d want %d", reopened.reservedCharge, len("first-run-bootstrap"))
	}
	reservation.Commit()
	if reopened.reservedCharge != 0 {
		t.Fatalf("commit left reserved=%d want 0", reopened.reservedCharge)
	}
	// The other session's stale generation is not this admission's to retire.
	if origin, err := reopened.Origin(otherKey); err != nil || origin != OriginReconstructed {
		t.Fatalf("foreign stale origin=%q err=%v after supersession", origin, err)
	}
	if !reopened.UnifiedEligible(freshKey) {
		t.Fatal("the committed re-adoption is not unified-eligible")
	}
	// The committed charge occupies the single ordinary slot durably.
	if available := reopened.AvailableCompletePaneSlots(); available != 0 {
		t.Fatalf("post-commit availability=%d want 0", available)
	}
}
