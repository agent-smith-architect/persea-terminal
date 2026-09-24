package unifiedjournal

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// failingUnlinkOps returns real journal ops whose unlinkat fails with EIO
// while *failing is true, and a pointer to that switch. Every other
// operation stays real: only the cleanup unlink is the injected fault.
func failingUnlinkOps() (journalOps, *bool) {
	failing := true
	ops := realJournalOps()
	realUnlink := ops.unlinkat
	ops.unlinkat = func(fd int, path string) error {
		if failing {
			return syscall.EIO
		}
		return realUnlink(fd, path)
	}
	return ops, &failing
}

func requireRetentionPathPresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("journal file whose unlink failed is not on disk: path=%q err=%v", path, err)
	}
}

// TestAdoptionAbortUnlinkFailureRetainsChargeAndTombstone is R1's Abort
// regression test: when the aborted reservation's unlink fails, the bytes are still
// on disk, so the realm charge must stay. The provisional slot is still
// retired — a failed adoption never holds admission capacity — but the
// invalidated tombstone and its charge survive until cleanup provably
// succeeds, and a later same-session supersession retries and refunds.
func TestAdoptionAbortUnlinkFailureRetainsChargeAndTombstone(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 2
	ops, failing := failingUnlinkOps()
	realm, err := openRealm(options, ops)
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
	if _, err := realm.Append(key, []byte("bootstrap")); err != nil {
		t.Fatalf("bootstrap append under reservation: %v", err)
	}
	bootstrapCharge := int64(len("bootstrap"))

	reservation.Abort()
	// The slot retires regardless of cleanup: the session must stay adoptable.
	if available := realm.AvailableCompletePaneSlots(); available != 1 {
		t.Fatalf("aborted reservation left availability=%d want 1", available)
	}
	if realm.total != bootstrapCharge {
		t.Fatalf("unlink failed but abort minted realm capacity: total=%d want=%d", realm.total, bootstrapCharge)
	}
	requireRetentionPathPresent(t, storagePathsForTest(options, key).Journal)
	if _, err := realm.Append(key, []byte("late-flight")); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("late append at an aborted key: %v, want invalidated", err)
	}
	// Abort stays idempotent with a retained charge.
	reservation.Abort()
	if realm.total != bootstrapCharge {
		t.Fatalf("double abort changed retained total=%d want %d", realm.total, bootstrapCharge)
	}

	// Once unlink can succeed, the next same-session supersession refunds the
	// retained tombstone and removes its file.
	*failing = false
	fresh := reservationKey("$1", 3)
	replacement, err := realm.BeginReconstructedPane(fresh, geometry)
	if err != nil {
		t.Fatalf("re-admission after failed-cleanup abort: %v", err)
	}
	replacement.Commit()
	if realm.total != 0 {
		t.Fatalf("healthy supersession did not refund retained charge: total=%d want 0", realm.total)
	}
	requireRetentionPathAbsent(t, storagePathsForTest(options, key).Journal)
	if !realm.UnifiedEligible(fresh) {
		t.Fatal("the committed re-adoption is not unified-eligible")
	}
}

// TestAdoptionCommitSupersessionUnlinkFailureRetainsStaleCharge is R1's
// Commit regression test: supersession of a slot-retired stale generation may
// refund its realm charge and drop its map entry only after the stale file is
// provably gone. On unlink failure the stale generation and its charge stay,
// and the next supersession for the session retries the cleanup.
func TestAdoptionCommitSupersessionUnlinkFailureRetainsStaleCharge(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 3
	first, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	geometry := Geometry{Columns: 80, Rows: 24}
	staleKey := reservationKey("$1", 2)
	reservation, err := first.BeginReconstructedPane(staleKey, geometry)
	if err != nil {
		t.Fatalf("first-run admission: %v", err)
	}
	reservation.Commit()
	if _, err := first.Append(staleKey, []byte("first-run-bootstrap")); err != nil {
		t.Fatalf("first-run bootstrap: %v", err)
	}
	staleCharge := int64(len("first-run-bootstrap"))
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	options.BrokerIncarnation = "broker-incarnation-b"
	ops, failing := failingUnlinkOps()
	reopened, err := openRealm(options, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.total != staleCharge {
		t.Fatalf("reopen reconstructed total=%d want %d", reopened.total, staleCharge)
	}

	freshKey := reservationKey("$1", 3)
	fresh, err := reopened.BeginReconstructedPane(freshKey, geometry)
	if err != nil {
		t.Fatalf("re-adoption admission: %v", err)
	}
	fresh.Commit()
	// Unlink failed: the stale bytes are still on disk, so the charge, the map
	// entry, and the file must all survive.
	if reopened.total != staleCharge {
		t.Fatalf("unlink failed but commit minted realm capacity: total=%d want=%d", reopened.total, staleCharge)
	}
	requireRetentionPathPresent(t, storagePathsForTest(options, staleKey).Journal)
	if origin, err := reopened.Origin(staleKey); err != nil || origin != OriginReconstructed {
		t.Fatalf("retained stale generation origin=%q err=%v, want reconstructed", origin, err)
	}
	if !reopened.UnifiedEligible(freshKey) {
		t.Fatal("the committed re-adoption is not unified-eligible")
	}

	// The next same-session supersession retries cleanup; with unlink healthy
	// it refunds and removes the stale generation.
	*failing = false
	retryKey := reservationKey("$1", 4)
	retry, err := reopened.BeginReconstructedPane(retryKey, geometry)
	if err != nil {
		t.Fatalf("retry admission: %v", err)
	}
	retry.Commit()
	if reopened.total != 0 {
		t.Fatalf("retried supersession did not refund stale charge: total=%d want 0", reopened.total)
	}
	requireRetentionPathAbsent(t, storagePathsForTest(options, staleKey).Journal)
	if _, err := reopened.Origin(staleKey); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("superseded stale origin err=%v, want invalidated", err)
	}
}

// TestAdoptionCleanupFailureReopenReconstructsRetainedCharge pins the
// durability half of R1: a retained tombstone's charge is exactly what a
// reopen rebuilds from its surviving file, across repeated reopens, and the
// tombstone is slot-retired at scan so it never consumes admission capacity.
func TestAdoptionCleanupFailureReopenReconstructsRetainedCharge(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 2
	ops, _ := failingUnlinkOps()
	realm, err := openRealm(options, ops)
	if err != nil {
		t.Fatal(err)
	}
	geometry := Geometry{Columns: 80, Rows: 24}
	key := reservationKey("$1", 2)
	reservation, err := realm.BeginReconstructedPane(key, geometry)
	if err != nil {
		t.Fatalf("begin reconstructed: %v", err)
	}
	if _, err := realm.Append(key, []byte("bootstrap")); err != nil {
		t.Fatalf("bootstrap append under reservation: %v", err)
	}
	bootstrapCharge := int64(len("bootstrap"))
	reservation.Abort()
	if realm.total != bootstrapCharge {
		t.Fatalf("abort with failed unlink retained total=%d want %d", realm.total, bootstrapCharge)
	}
	if err := realm.Close(); err != nil {
		t.Fatal(err)
	}

	// Repeated reopens converge: the surviving file reconstructs exactly the
	// retained charge, the tombstone is slot-retired at scan, and it fails
	// closed rather than resuming.
	for reopenIndex := 0; reopenIndex < 2; reopenIndex++ {
		reopened, err := openRealm(options, realJournalOps())
		if err != nil {
			t.Fatalf("reopen %d: %v", reopenIndex, err)
		}
		if reopened.total != bootstrapCharge {
			t.Fatalf("reopen %d reconstructed total=%d want %d", reopenIndex, reopened.total, bootstrapCharge)
		}
		if available := reopened.AvailableCompletePaneSlots(); available != 1 {
			t.Fatalf("reopen %d availability=%d want 1", reopenIndex, available)
		}
		if _, err := reopened.Append(key, []byte("resume")); !errors.Is(err, ErrInvalidated) {
			t.Fatalf("reopen %d resumed a retained tombstone: %v, want invalidated", reopenIndex, err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatalf("close reopen %d: %v", reopenIndex, err)
		}
	}

	// A healthy supersession after reopen finally refunds and removes it.
	final, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	fresh := reservationKey("$1", 3)
	replacement, err := final.BeginReconstructedPane(fresh, geometry)
	if err != nil {
		t.Fatalf("post-reopen re-admission: %v", err)
	}
	replacement.Commit()
	if final.total != 0 {
		t.Fatalf("post-reopen supersession did not refund: total=%d want 0", final.total)
	}
	requireRetentionPathAbsent(t, storagePathsForTest(options, key).Journal)
}

// TestAdoptionCleanupFailureRealmCapCountsRetainedCharge pins the safety
// property R1 protects: a retained charge keeps counting against the realm
// cap, so a failed cleanup can never mint append capacity.
func TestAdoptionCleanupFailureRealmCapCountsRetainedCharge(t *testing.T) {
	options := journalOptions(t)
	options.PaneCapBytes = 1024
	options.RealmCapBytes = 1024
	ops, _ := failingUnlinkOps()
	realm, err := openRealm(options, ops)
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
	retained := make([]byte, 600)
	if _, err := realm.Append(key, retained); err != nil {
		t.Fatalf("bootstrap append under reservation: %v", err)
	}
	reservation.Abort()
	if realm.total != int64(len(retained)) {
		t.Fatalf("abort with failed unlink retained total=%d want %d", realm.total, len(retained))
	}

	// 600 retained + 500 requested exceeds the 1024 realm cap: the retained
	// bytes must deny it.
	if _, err := realm.Append(journalKey("%denied", "cap-inc-a"), make([]byte, 500)); !errors.Is(err, ErrQuota) {
		t.Fatalf("append into retained-charge headroom: %v, want quota", err)
	}
	// A request that fits under cap WITH the retained charge still succeeds.
	if _, err := realm.Append(journalKey("%fits", "cap-inc-b"), make([]byte, 400)); err != nil {
		t.Fatalf("append within honest headroom: %v", err)
	}
	if realm.total != int64(len(retained))+400 {
		t.Fatalf("post-append total=%d want %d", realm.total, len(retained)+400)
	}
}
