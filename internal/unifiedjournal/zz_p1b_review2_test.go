package unifiedjournal

// Re-adjudication experiments for 51dd40f "Make rotation rollback capacity
// spendable". Review-only.

import (
	"errors"
	"testing"
)

// T1 — FINDING candidate. Packet §2.3 gate 6 / §3.3: the capacity is acquired
// BEFORE the composite reaches the PTY and the holder starts buffering only at
// the boundary flip (step 3c). Ordinary predecessor output therefore reaches
// the journal while the capacity is held and un-armed. It must be charged to
// ordinary room, must not spend R1, and must never be record-limited.
func TestReviewOrdinaryPredecessorOutputDoesNotSpendR1WhileHeld(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$review-ordinary-held", 1)
	admitRotationPredecessor(t, realm, predecessor)
	// 2 MiB free: R1 (1 MiB) plus 1 MiB of ordinary room for the window.
	if _, err := realm.Append(predecessor, make([]byte, options.PaneCapBytes-2*RotationPendingCapBytes)); err != nil {
		t.Fatal(err)
	}
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	defer capacity.Release()
	// Pre-boundary ordinary output: 16 tiny writes, then a 17th.
	for index := 0; index < int(RotationPendingCapRecords); index++ {
		if _, err := realm.Append(predecessor, []byte{byte(index)}); err != nil {
			t.Fatalf("ordinary record %d refused while capacity held: %v", index, err)
		}
	}
	pane := realm.panes[predecessor]
	if pane.rotationRollbackLogical != RotationPendingCapBytes || pane.rotationRollbackRecords != RotationPendingCapRecords {
		t.Fatalf("FINDING: ordinary pre-boundary output spent R1: bytes=%d/%d records=%d/%d", pane.rotationRollbackLogical, RotationPendingCapBytes, pane.rotationRollbackRecords, RotationPendingCapRecords)
	}
	if _, err := realm.Append(predecessor, []byte("17")); err != nil {
		t.Fatalf("FINDING: 17th ordinary record refused with ordinary room available: %v (live=%v)", err, realm.UnifiedEligible(predecessor))
	}
}

// T2 — FINDING candidate. After Abort the credit is armed; the 4R replay may
// legally use all 16 records with few bytes. Ordinary output resumes on the
// predecessor as soon as the holder is re-committed (4R), i.e. BEFORE
// capacity.Release(); it must fall through to ordinary room, not be refused.
func TestReviewArmedCreditRecordExhaustionDoesNotBlockOrdinaryOutput(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$review-armed", 1)
	successor := rotationKey("$review-armed", 2)
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
	// 4R replay: a legal tail shape of 16 one-byte records.
	for index := 0; index < int(RotationPendingCapRecords); index++ {
		if _, err := realm.Append(predecessor, []byte{byte(index)}); err != nil {
			t.Fatalf("rollback record %d refused: %v", index, err)
		}
	}
	// Holder re-committed; the session's ordinary output resumes before Release.
	if _, err := realm.Append(predecessor, []byte("ordinary")); err != nil {
		t.Fatalf("FINDING: ordinary output refused after the armed credit's records ran out: %v (live=%v)", err, realm.UnifiedEligible(predecessor))
	}
	capacity.Release()
	if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
		t.Fatalf("settlement leaked logical=%d physical=%d", realm.reservedCharge, realm.physicalReserved)
	}
}

// T3 — coverage pin (expected PASS). The typed record refusal wraps ErrQuota
// and does not invalidate; a rotation-unrelated pane is never record-limited.
func TestReviewRecordCapNeverTouchesUnrelatedPanes(t *testing.T) {
	options := rotationOptions(t)
	options.CompletePaneSlots = 3
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$review-unrelated", 1)
	other := rotationKey("$review-unrelated-other", 1)
	admitRotationPredecessor(t, realm, predecessor)
	admitRotationPredecessor(t, realm, other)
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	defer capacity.Release()
	for index := 0; index < 40; index++ {
		if _, err := realm.Append(other, []byte{1}); err != nil {
			t.Fatalf("unrelated pane record %d refused: %v", index, err)
		}
	}
	if !errors.Is(ErrRotationReplayRecordQuota, ErrQuota) {
		t.Fatal("typed record refusal does not wrap ErrQuota")
	}
}

// T4 — FINDING candidate (regression vs 49a7b03). A capacity that retains R1
// after materialization must still refuse a SECOND BeginRotatedPane: S/R2/R3
// were transferred to the first successor, so a second materialization would
// admit a generation with no slot, no header room check and no forward credit.
func TestReviewCapacityCannotMaterializeTwice(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	predecessor := rotationKey("$review-twice", 1)
	admitRotationPredecessor(t, realm, predecessor)
	geometry := Geometry{Columns: 80, Rows: 24}
	capacity, err := realm.BeginRotationCapacity(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	first, err := realm.BeginRotatedPane(rotationKey("$review-twice", 2), geometry, capacity)
	if err != nil {
		t.Fatal(err)
	}
	slotsBefore, _, _ := reviewLedger(realm)
	second, err := realm.BeginRotatedPane(rotationKey("$review-twice", 3), geometry, capacity)
	if err == nil || second != nil {
		slots, _, _ := reviewLedger(realm)
		live := 0
		for _, pane := range realm.panes {
			if pane.state == EligibilityContinuous && !pane.slotRetired {
				live++
			}
		}
		t.Fatalf("FINDING: consumed capacity materialized a second successor: err=%v slots=%d (before %d) live-generations=%d", err, slots, slotsBefore, live)
	}
	first.Commit()
}
