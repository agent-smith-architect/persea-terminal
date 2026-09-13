package unifiedjournal

import (
	"errors"
	"os"
	"testing"
)

// Post-ship F2 correction PSF-R1 (advisor adjudication of 6cabafd,
// 2026-08-27): the supersession allowance is two independent ledgers, and
// Append must spend each in its own unit. The first F2 cut reused rotation's
// record-shaped drawdown, which caps the physical slice of one record to
// the logical slice plus one record's framing. That coupling is exact for a
// rotation record (R2/R3 are sized per record) but wrong for proof-derived
// adoption capacity: a stale generation that is physical-full from framing
// while holding little payload — 128 committed one-byte records — refunds a
// large physical allowance and a small logical one, and the successor's
// bootstrap was refused with ErrPhysicalQuota although its exact physical
// cost was inside the physical hold.
//
// Pinned here: the physical allowance is consumed up to this append's exact
// physical cost regardless of how much of the logical allowance the append
// uses; the logical allowance is consumed in bytes of payload; any payload
// beyond the logical allowance still meets the ordinary logical caps, and
// any physical cost beyond the physical allowance still meets the ordinary
// physical caps; and Commit and Abort settle both units on their own.
// Rotation's record-shaped holds keep their coupling (the P1b suite runs
// unchanged alongside).

const ledgersRed = "PSF-R1-LEDGERS-RED"

// writeFramingHeavyStaleBirth writes one birth generation holding records
// committed one-byte records — the advisor's framing-heavy shape — and
// returns the file size the reopen will charge physically.
func writeFramingHeavyStaleBirth(t *testing.T, options OpenOptions, key PaneKey, records int) int64 {
	t.Helper()
	first, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatalf("%s open first realm: %v", ledgersRed, err)
	}
	if err := first.AdmitPane(key, Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("%s admit stale birth: %v", ledgersRed, err)
	}
	for i := 0; i < records; i++ {
		appendCommitted(t, first, key, "x")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("%s close first realm: %v", ledgersRed, err)
	}
	info, err := os.Lstat(storagePathsForTest(options, key).Journal)
	if err != nil {
		t.Fatalf("%s stat stale file: %v", ledgersRed, err)
	}
	return info.Size()
}

// TestSupersessionPhysicalAllowanceIsSpentIndependentlyOfLogical is the
// advisor's receipt verbatim: 128 one-byte records, the physical cap equal
// to that stale file, and a 256-byte successor bootstrap. At 6cabafd the
// bootstrap is refused with ErrPhysicalQuota because the physical slice
// was capped to the 128-byte logical slice plus one record's framing.
func TestSupersessionPhysicalAllowanceIsSpentIndependentlyOfLogical(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 3
	stale := supersessionKey("$1", 1)
	staleSize := writeFramingHeavyStaleBirth(t, options, stale, 128)
	options.PhysicalCapBytes = staleSize

	options.BrokerIncarnation = "broker-incarnation-b"
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatalf("%s reopen: %v", ledgersRed, err)
	}
	defer realm.Close()
	if realm.total != 128 || realm.physicalTotal != staleSize {
		t.Fatalf("%s recovered total=%d physicalTotal=%d want 128/%d", ledgersRed, realm.total, realm.physicalTotal, staleSize)
	}
	successor := supersessionKey("$1", 2)
	reservation, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("%s begin successor: %v", ledgersRed, err)
	}
	pane := realm.panes[successor]
	headerCost := pane.physical
	if pane.adoptionLogical != 128 || pane.adoptionPhysical != staleSize-headerCost {
		t.Fatalf("%s allowance logical=%d physical=%d want 128/%d", ledgersRed, pane.adoptionLogical, pane.adoptionPhysical, staleSize-headerCost)
	}
	if realm.reservedCharge != 128 || realm.physicalReserved != staleSize-headerCost {
		t.Fatalf("%s realm reserved logical=%d physical=%d want 128/%d", ledgersRed, realm.reservedCharge, realm.physicalReserved, staleSize-headerCost)
	}
	requireBudgetsWithinCap(t, "after begin", options, realm)

	payload := make([]byte, 256)
	physicalBefore := pane.adoptionPhysical
	record, err := realm.Append(successor, payload)
	if err != nil {
		charged, reserved, capBytes := realm.PhysicalBudget()
		t.Fatalf("%s successor could not spend its independent physical allowance: %v (logical hold=%d physical hold=%d physical total=%d reserved=%d cap=%d)",
			ledgersRed, err, pane.adoptionLogical, pane.adoptionPhysical, charged, reserved, capBytes)
	}
	commitLast(t, realm, successor, record)
	// Each ledger settled in its own unit: the logical allowance gave its
	// 128 bytes and the remaining 128 bytes of payload came from ordinary
	// logical room; the physical allowance gave this append's exact physical
	// cost, no less.
	if pane.adoptionLogical != 0 {
		t.Fatalf("%s logical allowance after a 256-byte append=%d want 0", ledgersRed, pane.adoptionLogical)
	}
	if want := physicalBefore - physicalAppendCost(len(payload)); pane.adoptionPhysical != want {
		t.Fatalf("%s physical allowance after append=%d want %d (exact physical cost %d drawn)", ledgersRed, pane.adoptionPhysical, want, physicalAppendCost(len(payload)))
	}
	if realm.total != 256 || realm.reservedCharge != 0 {
		t.Fatalf("%s after append total=%d reserved=%d want 256/0", ledgersRed, realm.total, realm.reservedCharge)
	}
	if realm.physicalTotal != headerCost+physicalAppendCost(len(payload)) || realm.physicalReserved != pane.adoptionPhysical {
		t.Fatalf("%s after append physicalTotal=%d physicalReserved=%d want %d/%d", ledgersRed, realm.physicalTotal, realm.physicalReserved, headerCost+physicalAppendCost(len(payload)), pane.adoptionPhysical)
	}
	requireBudgetsWithinCap(t, "after append", options, realm)

	reservation.Commit()
	requireLedgersSettled(t, "after commit", options, realm)
	if got := journalFileSize(t, options, successor); realm.physicalTotal != got {
		t.Fatalf("%s physicalTotal=%d want successor file size %d", ledgersRed, realm.physicalTotal, got)
	}
	if got := occupiedSlots(realm); got != 1 {
		t.Fatalf("%s occupied slots=%d want 1", ledgersRed, got)
	}
}

// TestSupersessionAbortSettlesBothAllowancesIndependently: the same shape,
// partially spent in both units, then aborted. Both reservations go to zero
// on their own, the tombstone is unlinked and refunded, and nothing leaks.
func TestSupersessionAbortSettlesBothAllowancesIndependently(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 3
	stale := supersessionKey("$1", 1)
	staleSize := writeFramingHeavyStaleBirth(t, options, stale, 128)
	options.PhysicalCapBytes = staleSize

	options.BrokerIncarnation = "broker-incarnation-b"
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatalf("%s reopen: %v", ledgersRed, err)
	}
	defer realm.Close()
	successor := supersessionKey("$1", 2)
	reservation, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("%s begin successor: %v", ledgersRed, err)
	}
	pane := realm.panes[successor]
	// Spend part of each unit: 64 of 128 logical bytes, one record's exact
	// physical cost of the physical hold.
	record, err := realm.Append(successor, make([]byte, 64))
	if err != nil {
		t.Fatalf("%s bootstrap: %v", ledgersRed, err)
	}
	commitLast(t, realm, successor, record)
	if pane.adoptionLogical != 64 || pane.adoptionPhysical <= 0 {
		t.Fatalf("%s partially spent allowance logical=%d physical=%d", ledgersRed, pane.adoptionLogical, pane.adoptionPhysical)
	}
	reservation.Abort()
	if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
		t.Fatalf("%s abort left reservations logical=%d physical=%d", ledgersRed, realm.reservedCharge, realm.physicalReserved)
	}
	if realm.total != 0 || realm.physicalTotal != 0 {
		t.Fatalf("%s abort left charges total=%d physicalTotal=%d", ledgersRed, realm.total, realm.physicalTotal)
	}
	requireLedgersSettled(t, "after abort", options, realm)
	if got := occupiedSlots(realm); got != 0 {
		t.Fatalf("%s occupied slots=%d want 0", ledgersRed, got)
	}
}

// TestSupersessionPayloadBeyondLogicalAllowanceMeetsTheLogicalCap: a large
// physical allowance lends no logical room. Payload beyond the 128-byte
// logical allowance is charged against the ordinary logical caps: a realm
// cap of 192 admits exactly 192 bytes (128 held + 64 ordinary) and refuses
// the 193rd, whatever the physical hold still has.
func TestSupersessionPayloadBeyondLogicalAllowanceMeetsTheLogicalCap(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 3
	options.PaneCapBytes = 192
	options.RealmCapBytes = 192
	stale := supersessionKey("$1", 1)
	staleSize := writeFramingHeavyStaleBirth(t, options, stale, 128)
	options.PhysicalCapBytes = staleSize

	options.BrokerIncarnation = "broker-incarnation-b"
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatalf("%s reopen: %v", ledgersRed, err)
	}
	defer realm.Close()
	successor := supersessionKey("$1", 2)
	reservation, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("%s begin successor: %v", ledgersRed, err)
	}
	pane := realm.panes[successor]
	record, err := realm.Append(successor, make([]byte, 192))
	if err != nil {
		t.Fatalf("%s 192-byte append (128 held + 64 ordinary): %v", ledgersRed, err)
	}
	commitLast(t, realm, successor, record)
	if pane.adoptionLogical != 0 || realm.total != 192 || realm.reservedCharge != 0 {
		t.Fatalf("%s after 192 bytes logical allowance=%d total=%d reserved=%d want 0/192/0", ledgersRed, pane.adoptionLogical, realm.total, realm.reservedCharge)
	}
	if pane.adoptionPhysical <= physicalAppendCost(1) {
		t.Fatalf("%s the physical hold should still carry another record: %d", ledgersRed, pane.adoptionPhysical)
	}
	if _, err := realm.Append(successor, []byte("y")); !errors.Is(err, ErrQuota) || errors.Is(err, ErrPhysicalQuota) {
		t.Fatalf("%s payload beyond the logical cap: err=%v want the logical quota refusal", ledgersRed, err)
	}
	reservation.Abort()
	requireLedgersSettled(t, "after abort", options, realm)
}

// TestSupersessionCostBeyondPhysicalAllowanceMeetsThePhysicalCap: a large
// logical allowance lends no physical room. With the physical cap on the
// stale file and the logical caps wide, a record whose exact physical cost
// consumes the physical hold lands, and the next record — still inside the
// logical allowance — is refused by the ordinary physical cap.
func TestSupersessionCostBeyondPhysicalAllowanceMeetsThePhysicalCap(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 3
	stale := supersessionKey("$1", 1)
	// One 4 KiB record: logical 4096, physical a little more than that.
	staleSize := writeFullStaleBirth(t, options, stale, string(make([]byte, 4096)))
	options.PhysicalCapBytes = staleSize

	options.BrokerIncarnation = "broker-incarnation-b"
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatalf("%s reopen: %v", ledgersRed, err)
	}
	defer realm.Close()
	successor := supersessionKey("$1", 2)
	reservation, err := realm.BeginReconstructedPane(successor, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("%s begin successor: %v", ledgersRed, err)
	}
	pane := realm.panes[successor]
	// A payload sized so this record's exact physical cost equals the
	// physical hold: it lands, drawing the hold to zero, while the logical
	// allowance still has room.
	fit := int(pane.adoptionPhysical) - appendFixed - commitFixed
	if fit <= 0 || int64(fit) >= pane.adoptionLogical {
		t.Fatalf("%s shape: physical hold=%d logical hold=%d fit=%d", ledgersRed, pane.adoptionPhysical, pane.adoptionLogical, fit)
	}
	record, err := realm.Append(successor, make([]byte, fit))
	if err != nil {
		t.Fatalf("%s record consuming the exact physical hold: %v", ledgersRed, err)
	}
	commitLast(t, realm, successor, record)
	if pane.adoptionPhysical != 0 || pane.adoptionLogical != 4096-int64(fit) {
		t.Fatalf("%s after the fitting record physical hold=%d logical hold=%d want 0/%d", ledgersRed, pane.adoptionPhysical, pane.adoptionLogical, 4096-int64(fit))
	}
	requireBudgetsWithinCap(t, "after the fitting record", options, realm)
	if _, err := realm.Append(successor, []byte("y")); !errors.Is(err, ErrPhysicalQuota) {
		t.Fatalf("%s cost beyond the physical cap: err=%v want the physical quota refusal", ledgersRed, err)
	}
	reservation.Abort()
	requireLedgersSettled(t, "after abort", options, realm)
}
