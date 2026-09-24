package unifiedjournal

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// Without early supersession, a quota-filling stale
// generation could not reach its own cleanup. BeginReconstructedPane created
// the successor header, the broker journaled the bootstrap, and only Commit —
// after that append — swept the same-session stale generation and refunded
// its charges. A stale generation that itself held the last logical or
// physical byte of the realm therefore refused the one-byte successor
// bootstrap with ErrQuota, Commit was unreachable, and the session was
// permanently unadoptable.
//
// The contract pinned here: same-session supersession is transactional and
// happens BEFORE the successor is materialized. BeginReconstructedPane
// unlinks the session's slot-retired stale generations first, refunds their
// logical and physical charges only once unlink/ENOENT proves the bytes gone
// (the same proof-before-refund sweep Commit uses), and holds exactly the
// refunded amounts — bounded by the pane caps — as a supersession allowance
// reserved for this reservation alone: no other session can spend it, the
// successor's header and bootstrap draw it down record by record, and Commit
// or Abort releases whatever remains. An unlink that fails refunds nothing
// and grants nothing, so the successor fails closed exactly as before, with
// no charge or slot leaked; a stale generation of ANOTHER session grants no
// allowance at all.

const supersessionRed = "F2-SUPERSESSION-RED"

// supersessionKey is one server+session identity across control generations.
func supersessionKey(session string, generation uint64) PaneKey {
	return PaneKey{Server: "server-a", Session: session, ControlGeneration: generation, Window: "@1", Pane: "%1", Incarnation: "incarnation-a"}
}

// writeFullStaleBirth writes one birth generation for key holding exactly
// payload as committed output, closes the realm, and returns the file size
// the reopen will charge physically.
func writeFullStaleBirth(t *testing.T, options OpenOptions, key PaneKey, payload string) int64 {
	t.Helper()
	first, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatalf("%s open first realm: %v", supersessionRed, err)
	}
	if err := first.AdmitPane(key, Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("%s admit stale birth: %v", supersessionRed, err)
	}
	appendCommitted(t, first, key, payload)
	if err := first.Close(); err != nil {
		t.Fatalf("%s close first realm: %v", supersessionRed, err)
	}
	info, err := os.Lstat(storagePathsForTest(options, key).Journal)
	if err != nil {
		t.Fatalf("%s stat stale file: %v", supersessionRed, err)
	}
	return info.Size()
}

// requireLedgersSettled asserts both ledgers are exactly the sum of the pane
// charges the realm still holds, that nothing is reserved, and that the
// physical ledger equals the bytes actually on disk for every surviving
// generation: charged + reserved <= cap in both units, with reserved == 0.
func requireLedgersSettled(t *testing.T, label string, options OpenOptions, realm *Realm) {
	t.Helper()
	var logical, physical int64
	for key, pane := range realm.panes {
		logical += pane.realmCharge
		physical += pane.physical
		if pane.reservedCharge != 0 || pane.physicalReserved != 0 {
			t.Fatalf("%s %s: pane %q still reserves logical=%d physical=%d", supersessionRed, label, key.Session, pane.reservedCharge, pane.physicalReserved)
		}
		info, err := os.Lstat(storagePathsForTest(options, key).Journal)
		if err != nil {
			if pane.realmCharge == 0 && pane.physical == 0 && pane.slotRetired && errors.Is(err, os.ErrNotExist) {
				// A settled tombstone: fail-closed map entry, no bytes, no charge.
				continue
			}
			t.Fatalf("%s %s: pane %q gen %d has a charge but no file: %v", supersessionRed, label, key.Session, key.ControlGeneration, err)
		}
		if info.Size() != pane.physical {
			t.Fatalf("%s %s: pane %q gen %d physical=%d but file size=%d", supersessionRed, label, key.Session, key.ControlGeneration, pane.physical, info.Size())
		}
	}
	if realm.total != logical {
		t.Fatalf("%s %s: realm total=%d but pane charges sum to %d", supersessionRed, label, realm.total, logical)
	}
	if realm.physicalTotal != physical {
		t.Fatalf("%s %s: realm physicalTotal=%d but pane physical charges sum to %d", supersessionRed, label, realm.physicalTotal, physical)
	}
	if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
		t.Fatalf("%s %s: settled realm still reserves logical=%d physical=%d", supersessionRed, label, realm.reservedCharge, realm.physicalReserved)
	}
	if realm.total > options.RealmCapBytes {
		t.Fatalf("%s %s: logical total=%d exceeds cap=%d", supersessionRed, label, realm.total, options.RealmCapBytes)
	}
	if charged, reserved, capBytes := realm.PhysicalBudget(); charged+reserved > capBytes {
		t.Fatalf("%s %s: physical charged=%d reserved=%d exceeds cap=%d", supersessionRed, label, charged, reserved, capBytes)
	}
}

// requireBudgetsWithinCap asserts charged + reserved <= cap in both units at
// an intermediate point of the transaction.
func requireBudgetsWithinCap(t *testing.T, label string, options OpenOptions, realm *Realm) {
	t.Helper()
	if realm.total+realm.reservedCharge > options.RealmCapBytes {
		t.Fatalf("%s %s: logical total=%d reserved=%d exceeds cap=%d", supersessionRed, label, realm.total, realm.reservedCharge, options.RealmCapBytes)
	}
	if charged, reserved, capBytes := realm.PhysicalBudget(); charged+reserved > capBytes {
		t.Fatalf("%s %s: physical charged=%d reserved=%d exceeds cap=%d", supersessionRed, label, charged, reserved, capBytes)
	}
}

func occupiedSlots(realm *Realm) int64 {
	realm.retention.mu.Lock()
	defer realm.retention.mu.Unlock()
	return realm.retention.reserved
}

// TestSupersessionLogicalFullStaleGenerationIsSupersededBeforeBootstrap uses
// a 64-byte realm filled by one recovered
// stale birth generation, a successor reservation, and a one-byte bootstrap.
// Without early supersession the bootstrap is refused with ErrQuota. The stale generation's
// bytes must instead be proven gone and refunded before the bootstrap, the
// refunded room must be the successor's alone, and Commit must settle both
// ledgers and the slot exactly.
func TestSupersessionLogicalFullStaleGenerationIsSupersededBeforeBootstrap(t *testing.T) {
	options := journalOptions(t)
	options.PaneCapBytes = 64
	options.RealmCapBytes = 64
	options.CompletePaneSlots = 4
	stale := supersessionKey("$1", 1)
	payload := string(make([]byte, 64))
	writeFullStaleBirth(t, options, stale, payload)

	options.BrokerIncarnation = "broker-incarnation-b"
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatalf("%s reopen: %v", supersessionRed, err)
	}
	defer realm.Close()
	if realm.total != 64 {
		t.Fatalf("%s recovered stale charge total=%d want 64", supersessionRed, realm.total)
	}
	successor := supersessionKey("$1", 2)
	reservation, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("%s begin successor: %v", supersessionRed, err)
	}
	// The stale generation is provably gone before any successor byte exists,
	// and its exact charge is held for this reservation, not returned to the
	// pool: the realm still reads full to everyone else.
	if _, err := os.Lstat(storagePathsForTest(options, stale).Journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s stale file after begin: err=%v want absent", supersessionRed, err)
	}
	if realm.panes[stale] != nil {
		t.Fatalf("%s stale generation still in the pane map after begin", supersessionRed)
	}
	if realm.total != 0 || realm.reservedCharge != 64 {
		t.Fatalf("%s after begin total=%d reserved=%d want 0/64", supersessionRed, realm.total, realm.reservedCharge)
	}
	requireBudgetsWithinCap(t, "after begin", options, realm)
	foreign := supersessionKey("$2", 1)
	if err := realm.AdmitPane(foreign, Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("%s admit foreign session: %v", supersessionRed, err)
	}
	if _, err := realm.Append(foreign, []byte("x")); !errors.Is(err, ErrQuota) {
		t.Fatalf("%s foreign session spent the supersession allowance: err=%v want quota", supersessionRed, err)
	}
	// The one-byte bootstrap the receipt refused now lands, drawing the
	// allowance down by exactly one byte.
	record, err := realm.Append(successor, []byte("b"))
	if err != nil {
		t.Fatalf("%s fresh bootstrap=%v total=%d cap=%d", supersessionRed, err, realm.total, options.RealmCapBytes)
	}
	commitLast(t, realm, successor, record)
	if realm.total != 1 || realm.reservedCharge != 63 {
		t.Fatalf("%s after bootstrap total=%d reserved=%d want 1/63", supersessionRed, realm.total, realm.reservedCharge)
	}
	requireBudgetsWithinCap(t, "after bootstrap", options, realm)

	reservation.Commit()
	requireLedgersSettled(t, "after commit", options, realm)
	if !realm.UnifiedEligible(successor) {
		t.Fatalf("%s committed successor is not unified-eligible", supersessionRed)
	}
	// Slot: the successor and the foreign birth occupy exactly two.
	if got := occupiedSlots(realm); got != 2 {
		t.Fatalf("%s occupied slots=%d want 2", supersessionRed, got)
	}
	// The released remainder is ordinary room again: the successor itself and
	// another session can both use it now. (The refused foreign append above
	// invalidated that generation, as every logical refusal does; a third
	// session stands in for it.)
	third := supersessionKey("$3", 1)
	if err := realm.AdmitPane(third, Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("%s admit third session: %v", supersessionRed, err)
	}
	if _, err := realm.Append(third, []byte("y")); err != nil {
		t.Fatalf("%s third-session append after commit: %v", supersessionRed, err)
	}
	if _, err := realm.Append(successor, []byte("c")); err != nil {
		t.Fatalf("%s successor append after commit: %v", supersessionRed, err)
	}
}

// TestSupersessionPhysicalFullStaleGenerationIsSupersededBeforeHeader: the
// physical ledger is the one that binds. The stale file occupies the whole
// physical realm cap, so the successor header cannot be created without
// supersession. The refunded physical charge must carry the header and the
// bootstrap, and the ledger must settle to exactly the successor's file size.
func TestSupersessionPhysicalFullStaleGenerationIsSupersededBeforeHeader(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 3
	stale := supersessionKey("$1", 1)
	staleSize := writeFullStaleBirth(t, options, stale, "stale-output")
	options.PhysicalCapBytes = staleSize
	options.PanePhysicalCapBytes = staleSize

	options.BrokerIncarnation = "broker-incarnation-b"
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatalf("%s reopen: %v", supersessionRed, err)
	}
	defer realm.Close()
	if charged, _, capBytes := realm.PhysicalBudget(); charged != staleSize || capBytes != staleSize {
		t.Fatalf("%s recovered physical charged=%d cap=%d want %d/%d", supersessionRed, charged, capBytes, staleSize, staleSize)
	}
	successor := supersessionKey("$1", 2)
	reservation, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("%s begin successor on a physically full realm: %v", supersessionRed, err)
	}
	requireBudgetsWithinCap(t, "after begin", options, realm)
	if _, err := os.Lstat(storagePathsForTest(options, stale).Journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s stale file after begin: err=%v want absent", supersessionRed, err)
	}
	record, err := realm.Append(successor, []byte("b"))
	if err != nil {
		t.Fatalf("%s fresh bootstrap on a physically full realm: %v", supersessionRed, err)
	}
	commitLast(t, realm, successor, record)
	requireBudgetsWithinCap(t, "after bootstrap", options, realm)
	reservation.Commit()
	requireLedgersSettled(t, "after commit", options, realm)
	if got := journalFileSize(t, options, successor); realm.physicalTotal != got {
		t.Fatalf("%s physicalTotal=%d want successor file size %d", supersessionRed, realm.physicalTotal, got)
	}
}

// TestSupersessionBothLedgersFullStaleGenerationIsSuperseded: both caps sit
// exactly on the stale generation's charges.
func TestSupersessionBothLedgersFullStaleGenerationIsSuperseded(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 3
	options.PaneCapBytes = 64
	options.RealmCapBytes = 64
	stale := supersessionKey("$1", 1)
	staleSize := writeFullStaleBirth(t, options, stale, string(make([]byte, 64)))
	options.PhysicalCapBytes = staleSize
	options.PanePhysicalCapBytes = staleSize

	options.BrokerIncarnation = "broker-incarnation-b"
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatalf("%s reopen: %v", supersessionRed, err)
	}
	defer realm.Close()
	successor := supersessionKey("$1", 2)
	reservation, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("%s begin successor on a full realm: %v", supersessionRed, err)
	}
	requireBudgetsWithinCap(t, "after begin", options, realm)
	record, err := realm.Append(successor, []byte("b"))
	if err != nil {
		t.Fatalf("%s fresh bootstrap on a full realm: %v", supersessionRed, err)
	}
	commitLast(t, realm, successor, record)
	requireBudgetsWithinCap(t, "after bootstrap", options, realm)
	reservation.Commit()
	requireLedgersSettled(t, "after commit", options, realm)
	if got := occupiedSlots(realm); got != 1 {
		t.Fatalf("%s occupied slots=%d want 1", supersessionRed, got)
	}
}

// TestSupersessionUnlinkFailureGrantsNothingAndFailsClosed: proof before
// refund. While the stale unlink fails with EIO the bytes are still on disk,
// so nothing is refunded, nothing is held for the successor, and the
// bootstrap is refused exactly as before — with no charge or slot leaked by
// either the refusal or the Abort that follows. Once the unlink works again
// the next same-session admission supersedes both the stale generation and
// the aborted tombstone, and everything settles exactly.
func TestSupersessionUnlinkFailureGrantsNothingAndFailsClosed(t *testing.T) {
	options := journalOptions(t)
	options.PaneCapBytes = 64
	options.RealmCapBytes = 64
	options.CompletePaneSlots = 3
	stale := supersessionKey("$1", 1)
	writeFullStaleBirth(t, options, stale, string(make([]byte, 64)))

	options.BrokerIncarnation = "broker-incarnation-b"
	ops, failing := failingUnlinkOps()
	realm, err := openRealm(options, ops)
	if err != nil {
		t.Fatalf("%s reopen: %v", supersessionRed, err)
	}
	defer realm.Close()
	successor := supersessionKey("$1", 2)
	reservation, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("%s begin successor while unlink fails: %v", supersessionRed, err)
	}
	requireRetentionPathPresent(t, storagePathsForTest(options, stale).Journal)
	if realm.total != 64 || realm.reservedCharge != 0 {
		t.Fatalf("%s unproven removal changed the ledger: total=%d reserved=%d want 64/0", supersessionRed, realm.total, realm.reservedCharge)
	}
	if _, err := realm.Append(successor, []byte("b")); !errors.Is(err, ErrQuota) {
		t.Fatalf("%s bootstrap without proven room: err=%v want quota", supersessionRed, err)
	}
	reservation.Abort()
	// The aborted successor's own unlink fails too: its header stays charged
	// as a slot-retired tombstone, the stale generation keeps its charge, and
	// the provisional slot is returned.
	if got := occupiedSlots(realm); got != 0 {
		t.Fatalf("%s occupied slots after abort=%d want 0", supersessionRed, got)
	}
	if realm.total != 64 || realm.reservedCharge != 0 || realm.physicalReserved != 0 {
		t.Fatalf("%s after abort total=%d reserved=%d physicalReserved=%d", supersessionRed, realm.total, realm.reservedCharge, realm.physicalReserved)
	}
	requireBudgetsWithinCap(t, "after abort", options, realm)

	*failing = false
	next := supersessionKey("$1", 3)
	retry, err := realm.BeginReconstructedPane(next, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("%s begin after unlink recovered: %v", supersessionRed, err)
	}
	for _, gone := range []PaneKey{stale, successor} {
		if _, err := os.Lstat(storagePathsForTest(options, gone).Journal); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s gen %d file after retry begin: err=%v want absent", supersessionRed, gone.ControlGeneration, err)
		}
		if realm.panes[gone] != nil {
			t.Fatalf("%s gen %d still in the pane map after retry begin", supersessionRed, gone.ControlGeneration)
		}
	}
	record, err := realm.Append(next, []byte("b"))
	if err != nil {
		t.Fatalf("%s bootstrap after unlink recovered: %v", supersessionRed, err)
	}
	commitLast(t, realm, next, record)
	retry.Commit()
	requireLedgersSettled(t, "after retry commit", options, realm)
	if got := occupiedSlots(realm); got != 1 {
		t.Fatalf("%s occupied slots after retry commit=%d want 1", supersessionRed, got)
	}
}

// TestSupersessionUnlinkFailureOnPhysicallyFullRealmRefusesTheHeader: with
// the physical ledger full and the unlink failing there is no proven room
// for the header, so Begin itself refuses with the physical quota, creates
// nothing, holds nothing, and returns the slot.
func TestSupersessionUnlinkFailureOnPhysicallyFullRealmRefusesTheHeader(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 3
	stale := supersessionKey("$1", 1)
	staleSize := writeFullStaleBirth(t, options, stale, "stale-output")
	options.PhysicalCapBytes = staleSize
	options.PanePhysicalCapBytes = staleSize

	options.BrokerIncarnation = "broker-incarnation-b"
	ops, _ := failingUnlinkOps()
	realm, err := openRealm(options, ops)
	if err != nil {
		t.Fatalf("%s reopen: %v", supersessionRed, err)
	}
	defer realm.Close()
	successor := supersessionKey("$1", 2)
	if _, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24}); !errors.Is(err, ErrPhysicalQuota) {
		t.Fatalf("%s begin on a physically full realm with unproven removal: err=%v want physical quota", supersessionRed, err)
	}
	if realm.panes[successor] != nil {
		t.Fatalf("%s refused begin left a successor entry", supersessionRed)
	}
	if _, err := os.Lstat(storagePathsForTest(options, successor).Journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s refused begin left a successor file: err=%v", supersessionRed, err)
	}
	if got := occupiedSlots(realm); got != 0 {
		t.Fatalf("%s occupied slots after refused begin=%d want 0", supersessionRed, got)
	}
	requireLedgersSettled(t, "after refused begin", options, realm)
}

// TestSupersessionFailedSuccessorMaterializationReleasesEverything: the
// header write fails after the stale generation was already superseded.
// Nothing may be held or leaked: the allowance is released, the slot
// returned, and the failed header's bytes are either provably gone or
// counted exactly once as retained charge.
func TestSupersessionFailedSuccessorMaterializationReleasesEverything(t *testing.T) {
	for _, unlinkFails := range []bool{false, true} {
		name := "header-unlinked"
		if unlinkFails {
			name = "header-retained"
		}
		t.Run(name, func(t *testing.T) {
			options := journalOptions(t)
			options.PaneCapBytes = 64
			options.RealmCapBytes = 64
			options.CompletePaneSlots = 3
			stale := supersessionKey("$1", 1)
			writeFullStaleBirth(t, options, stale, string(make([]byte, 64)))

			options.BrokerIncarnation = "broker-incarnation-b"
			ops := realJournalOps()
			realWrite := ops.write
			failWrites := false
			ops.write = func(file *os.File, data []byte) (int, error) {
				if failWrites {
					return 0, syscall.EIO
				}
				return realWrite(file, data)
			}
			realUnlink := ops.unlinkat
			unlinkSuccessorFails := false
			ops.unlinkat = func(fd int, path string) error {
				if unlinkSuccessorFails && path == paneFileName(supersessionKey("$1", 2)) {
					return syscall.EIO
				}
				return realUnlink(fd, path)
			}
			realm, err := openRealm(options, ops)
			if err != nil {
				t.Fatalf("%s reopen: %v", supersessionRed, err)
			}
			defer realm.Close()
			successor := supersessionKey("$1", 2)
			failWrites = true
			unlinkSuccessorFails = unlinkFails
			if _, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24}); !errors.Is(err, ErrStorage) {
				t.Fatalf("%s begin with a failing header write: err=%v want storage", supersessionRed, err)
			}
			failWrites = false
			unlinkSuccessorFails = false
			if got := occupiedSlots(realm); got != 0 {
				t.Fatalf("%s occupied slots after failed materialization=%d want 0", supersessionRed, got)
			}
			if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
				t.Fatalf("%s failed materialization left reservations logical=%d physical=%d", supersessionRed, realm.reservedCharge, realm.physicalReserved)
			}
			// The stale generation was superseded by proof before the header
			// was attempted: its bytes are gone and refunded either way.
			if _, err := os.Lstat(storagePathsForTest(options, stale).Journal); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s stale file after failed materialization: err=%v want absent", supersessionRed, err)
			}
			if realm.total != 0 {
				t.Fatalf("%s logical total after failed materialization=%d want 0", supersessionRed, realm.total)
			}
			info, statErr := os.Lstat(storagePathsForTest(options, successor).Journal)
			if unlinkFails {
				// Retained: the file is on disk and counted exactly once.
				if statErr != nil {
					t.Fatalf("%s retained failed header is not on disk: %v", supersessionRed, statErr)
				}
				// The charge taken before the write is retained as the
				// conservative bound: never below what is on disk, never
				// refunded without unlink proof.
				if realm.physicalTotal <= 0 || realm.physicalTotal < info.Size() {
					t.Fatalf("%s retained header physicalTotal=%d file size %d", supersessionRed, realm.physicalTotal, info.Size())
				}
			} else {
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("%s failed header file: err=%v want absent", supersessionRed, statErr)
				}
				if realm.physicalTotal != 0 {
					t.Fatalf("%s physicalTotal after unlinked failed header=%d want 0", supersessionRed, realm.physicalTotal)
				}
			}
			requireBudgetsWithinCap(t, "after failed materialization", options, realm)
			// The session stays adoptable: the next generation admits and
			// bootstraps on ordinary room.
			next := supersessionKey("$1", 3)
			retry, err := realm.BeginReconstructedPane(next, Geometry{Columns: 80, Rows: 24})
			if err != nil {
				t.Fatalf("%s begin after failed materialization: %v", supersessionRed, err)
			}
			record, err := realm.Append(next, []byte("b"))
			if err != nil {
				t.Fatalf("%s bootstrap after failed materialization: %v", supersessionRed, err)
			}
			commitLast(t, realm, next, record)
			retry.Commit()
			if unlinkFails {
				// A retained failed header under a name the realm cannot
				// attribute is not a pane entry; it stays counted until a reopen
				// rebuilds the ledger from the surviving file.
				if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
					t.Fatalf("%s retained-header commit left reservations", supersessionRed)
				}
				requireBudgetsWithinCap(t, "after retry commit", options, realm)
			} else {
				requireLedgersSettled(t, "after retry commit", options, realm)
			}
		})
	}
}

// TestSupersessionRestartReconstructsExactLedgers: a crash between the
// bootstrap and Commit. The reopen finds no stale generation (it was
// superseded by proof at Begin), recovers the uncommitted successor as a
// slot-retired generation charged exactly by its file, holds nothing (the
// allowance is volatile and never persisted), and the next admission
// supersedes that recovered successor in turn.
func TestSupersessionRestartReconstructsExactLedgers(t *testing.T) {
	options := journalOptions(t)
	options.PaneCapBytes = 64
	options.RealmCapBytes = 64
	options.CompletePaneSlots = 3
	stale := supersessionKey("$1", 1)
	writeFullStaleBirth(t, options, stale, string(make([]byte, 64)))

	options.BrokerIncarnation = "broker-incarnation-b"
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatalf("%s reopen: %v", supersessionRed, err)
	}
	successor := supersessionKey("$1", 2)
	if _, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("%s begin successor: %v", supersessionRed, err)
	}
	record, err := realm.Append(successor, []byte("bootstrap"))
	if err != nil {
		t.Fatalf("%s bootstrap: %v", supersessionRed, err)
	}
	commitLast(t, realm, successor, record)
	// Crash: the reservation is never settled.
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close mid-adoption: %v", supersessionRed, err)
	}

	options.BrokerIncarnation = "broker-incarnation-c"
	restarted, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatalf("%s restart: %v", supersessionRed, err)
	}
	defer restarted.Close()
	if restarted.panes[stale] != nil {
		t.Fatalf("%s superseded stale generation came back at restart", supersessionRed)
	}
	recovered := restarted.panes[successor]
	if recovered == nil || !recovered.slotRetired {
		t.Fatalf("%s uncommitted successor not recovered slot-retired: %+v", supersessionRed, recovered)
	}
	if restarted.total != int64(len("bootstrap")) {
		t.Fatalf("%s restarted total=%d want %d", supersessionRed, restarted.total, len("bootstrap"))
	}
	if got := occupiedSlots(restarted); got != 0 {
		t.Fatalf("%s restarted occupied slots=%d want 0", supersessionRed, got)
	}
	requireLedgersSettled(t, "after restart", options, restarted)

	next := supersessionKey("$1", 3)
	retry, err := restarted.BeginReconstructedPane(next, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("%s begin after restart: %v", supersessionRed, err)
	}
	if restarted.panes[successor] != nil {
		t.Fatalf("%s recovered successor was not superseded at begin", supersessionRed)
	}
	record, err = restarted.Append(next, []byte("b"))
	if err != nil {
		t.Fatalf("%s bootstrap after restart: %v", supersessionRed, err)
	}
	commitLast(t, restarted, next, record)
	retry.Commit()
	requireLedgersSettled(t, "after restart commit", options, restarted)
	if got := occupiedSlots(restarted); got != 1 {
		t.Fatalf("%s occupied slots after restart commit=%d want 1", supersessionRed, got)
	}
}

// TestSupersessionForeignSessionFullGenerationGrantsNoAllowance: a stale
// generation of ANOTHER session fills the realm. It is not this admission's
// to retire, so it grants nothing: the successor is refused exactly as an
// ordinary full realm refuses it, the foreign bytes and charges stay, and no
// reservation is left behind.
func TestSupersessionForeignSessionFullGenerationGrantsNoAllowance(t *testing.T) {
	t.Run("logical", func(t *testing.T) {
		options := journalOptions(t)
		options.PaneCapBytes = 64
		options.RealmCapBytes = 64
		options.CompletePaneSlots = 3
		foreign := supersessionKey("$2", 1)
		writeFullStaleBirth(t, options, foreign, string(make([]byte, 64)))
		options.BrokerIncarnation = "broker-incarnation-b"
		realm, err := openRealm(options, realJournalOps())
		if err != nil {
			t.Fatalf("%s reopen: %v", supersessionRed, err)
		}
		defer realm.Close()
		successor := supersessionKey("$1", 2)
		reservation, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24})
		if err != nil {
			t.Fatalf("%s begin: %v", supersessionRed, err)
		}
		if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
			t.Fatalf("%s foreign stale granted an allowance logical=%d physical=%d", supersessionRed, realm.reservedCharge, realm.physicalReserved)
		}
		requireRetentionPathPresent(t, storagePathsForTest(options, foreign).Journal)
		if _, err := realm.Append(successor, []byte("b")); !errors.Is(err, ErrQuota) {
			t.Fatalf("%s bootstrap against a foreign-full realm: err=%v want quota", supersessionRed, err)
		}
		reservation.Abort()
		if realm.total != 64 || realm.panes[foreign] == nil {
			t.Fatalf("%s foreign stale charge disturbed: total=%d present=%v", supersessionRed, realm.total, realm.panes[foreign] != nil)
		}
		requireLedgersSettled(t, "after abort", options, realm)
		if got := occupiedSlots(realm); got != 0 {
			t.Fatalf("%s occupied slots=%d want 0", supersessionRed, got)
		}
	})
	t.Run("physical", func(t *testing.T) {
		options := journalOptions(t)
		options.CompletePaneSlots = 3
		foreign := supersessionKey("$2", 1)
		size := writeFullStaleBirth(t, options, foreign, "foreign-output")
		options.PhysicalCapBytes = size
		options.PanePhysicalCapBytes = size
		options.BrokerIncarnation = "broker-incarnation-b"
		realm, err := openRealm(options, realJournalOps())
		if err != nil {
			t.Fatalf("%s reopen: %v", supersessionRed, err)
		}
		defer realm.Close()
		successor := supersessionKey("$1", 2)
		if _, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24}); !errors.Is(err, ErrPhysicalQuota) {
			t.Fatalf("%s begin against a foreign physically full realm: err=%v want physical quota", supersessionRed, err)
		}
		requireRetentionPathPresent(t, storagePathsForTest(options, foreign).Journal)
		requireLedgersSettled(t, "after refused begin", options, realm)
		if got := occupiedSlots(realm); got != 0 {
			t.Fatalf("%s occupied slots=%d want 0", supersessionRed, got)
		}
	})
}

// TestSupersessionAllowanceIsBoundedByThePaneCaps: several same-session
// stale generations whose charges together exceed the pane cap. Every one is
// superseded and refunded by proof, but the allowance held for the successor
// is bounded by what one generation may ever hold; the excess is ordinary
// room again immediately.
func TestSupersessionAllowanceIsBoundedByThePaneCaps(t *testing.T) {
	options := journalOptions(t)
	options.PaneCapBytes = 32
	options.RealmCapBytes = 96
	options.CompletePaneSlots = 4
	first := supersessionKey("$1", 1)
	second := supersessionKey("$1", 2)
	realm0, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []PaneKey{first, second} {
		if err := realm0.AdmitPane(key, Geometry{Columns: 80, Rows: 24}); err != nil {
			t.Fatal(err)
		}
		appendCommitted(t, realm0, key, string(make([]byte, 32)))
	}
	if err := realm0.Close(); err != nil {
		t.Fatal(err)
	}
	options.BrokerIncarnation = "broker-incarnation-b"
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatalf("%s reopen: %v", supersessionRed, err)
	}
	defer realm.Close()
	if realm.total != 64 {
		t.Fatalf("%s recovered total=%d want 64", supersessionRed, realm.total)
	}
	successor := supersessionKey("$1", 3)
	reservation, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("%s begin: %v", supersessionRed, err)
	}
	if realm.total != 0 || realm.reservedCharge != 32 {
		t.Fatalf("%s after begin total=%d reserved=%d want 0/32 (allowance bounded by the pane cap)", supersessionRed, realm.total, realm.reservedCharge)
	}
	for _, gone := range []PaneKey{first, second} {
		if realm.panes[gone] != nil {
			t.Fatalf("%s gen %d not superseded", supersessionRed, gone.ControlGeneration)
		}
	}
	// A sibling can use the excess (96 - 32 held = 64 of ordinary room).
	foreign := supersessionKey("$2", 1)
	if err := realm.AdmitPane(foreign, Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	appendCommitted(t, realm, foreign, string(make([]byte, 32)))
	// The successor draws its allowance to its pane cap and no further.
	if _, err := realm.Append(successor, make([]byte, 32)); err != nil {
		t.Fatalf("%s successor bootstrap within the allowance: %v", supersessionRed, err)
	}
	if _, err := realm.Append(successor, []byte("x")); !errors.Is(err, ErrQuota) {
		t.Fatalf("%s successor exceeded its pane cap: err=%v want quota", supersessionRed, err)
	}
	reservation.Abort()
	if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
		t.Fatalf("%s abort left reservations logical=%d physical=%d", supersessionRed, realm.reservedCharge, realm.physicalReserved)
	}
	requireLedgersSettled(t, "after abort", options, realm)
}
