package unifiedjournal

import (
	"bytes"
	"os"
	"sync/atomic"
	"testing"
)

func TestSourceStartupOrphanRecoveryPersistentRoot(t *testing.T) {
	options := rotationOptions(t)
	options.CompletePaneSlots = 8
	seed, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	live := rotationKey("$startup-recovery-live", 1)
	absent := rotationKey("$startup-recovery-absent", 1)
	stale := rotationKey("$startup-recovery-live", 2)
	stale.Incarnation = "different-incarnation"
	ambiguous := rotationKey("$startup-recovery-other", 1)
	for _, key := range []PaneKey{live, absent, stale, ambiguous} {
		admitRotationPredecessor(t, seed, key)
		appendCommittedOutput(t, seed, key, []byte("startup-recovery persistent recovery shape"))
	}
	livePath := storagePathsForTest(options, live).Journal
	liveBytes, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	options.StartupRecovery = true
	restarted, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	if restarted.retention.reserved != 4 {
		t.Fatalf("startup scan slots=%d want 4 charged before inventory", restarted.retention.reserved)
	}
	outcomes := restarted.ReconcileRecovered([]RecoveryDecision{
		{Key: live, Disposition: RecoveryKeepExact},
		{Key: absent, Disposition: RecoveryRetireAbsent},
		{Key: stale, Disposition: RecoveryRetireReplacement},
		{Key: ambiguous, Disposition: RecoveryRetainAmbiguous},
	})
	if len(outcomes) != 4 {
		t.Fatalf("recovery ledger entries=%d want 4", len(outcomes))
	}
	want := map[PaneKey]struct {
		decision RecoveryDisposition
		outcome  RecoveryOutcome
		slot     bool
		retry    bool
	}{
		live:      {RecoveryKeepExact, RecoveryKeptExact, true, false},
		absent:    {RecoveryRetireAbsent, RecoveryRetiredAbsent, false, false},
		stale:     {RecoveryRetireReplacement, RecoveryRetiredStale, false, false},
		ambiguous: {RecoveryRetainAmbiguous, RecoveryRetainedAmbiguous, true, false},
	}
	for _, outcome := range outcomes {
		expect := want[outcome.Key]
		if outcome.Decision != expect.decision || outcome.Outcome != expect.outcome ||
			outcome.RetainedSlot != expect.slot || outcome.RetryNeeded != expect.retry {
			t.Fatalf("typed recovery outcome=%+v want=%+v", outcome, expect)
		}
		if expect.slot && (outcome.RetainedLogical == 0 || outcome.RetainedPhysical == 0) {
			t.Fatalf("retained generation lost charges: %+v", outcome)
		}
		if !expect.slot && (outcome.RetainedLogical != 0 || outcome.RetainedPhysical != 0) {
			t.Fatalf("retired generation retained charges: %+v", outcome)
		}
	}
	if restarted.panes[live] == nil || restarted.panes[ambiguous] == nil || restarted.panes[absent] != nil || restarted.panes[stale] != nil {
		t.Fatalf("typed recovery retained/deleted wrong generations: %+v", outcomes)
	}
	if restarted.retention.reserved != 2 {
		t.Fatalf("post-recovery slots=%d want live+ambiguous", restarted.retention.reserved)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, liveBytes) {
		t.Fatal("exact-live persistent journal changed during recovery")
	}

	repeated, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer repeated.Close()
	if repeated.retention.reserved != 2 {
		t.Fatalf("repeated startup minted slot capacity: reserved=%d", repeated.retention.reserved)
	}
	repeatedOutcomes := repeated.ReconcileRecovered([]RecoveryDecision{
		{Key: live, Disposition: RecoveryKeepExact},
		{Key: ambiguous, Disposition: RecoveryRetainAmbiguous},
	})
	if repeated.retention.reserved != 2 || len(repeated.panes) != 2 {
		t.Fatalf("repeated recovery drift slots=%d panes=%d", repeated.retention.reserved, len(repeated.panes))
	}
	for _, outcome := range repeatedOutcomes {
		if outcome.RetryNeeded || !outcome.RetainedSlot || outcome.RetainedLogical == 0 || outcome.RetainedPhysical == 0 {
			t.Fatalf("repeated recovery ledger drifted: %+v", outcome)
		}
	}
}

func TestTerminalRetirementRetainsEIOChargeAndRetriesToProof(t *testing.T) {
	options := rotationOptions(t)
	var wakes atomic.Int64
	type wakeReceipt struct{ logical, physical, slots int64 }
	var receipts []wakeReceipt
	var realm *Realm
	options.EligibilityWake = func() {
		wakes.Add(1)
		receipts = append(receipts, wakeReceipt{logical: realm.total, physical: realm.physicalTotal, slots: realm.retention.reserved})
	}
	ops, failing := failingUnlinkOps()
	var err error
	realm, err = openRealm(options, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()

	key := rotationKey("$terminal-retire-eio", 1)
	admitRotationPredecessor(t, realm, key)
	appendCommittedOutput(t, realm, key, []byte("terminal-retirement-charge"))
	logicalBefore := realm.total
	physicalBefore, _, _ := realm.PhysicalBudget()
	reservedBefore := realm.retention.reserved
	path := storagePathsForTest(options, key).Journal

	if settled := realm.RetirePane(key); settled {
		t.Fatal("EIO retirement reported unlink proof")
	}
	retained := realm.panes[key]
	if retained == nil || !retained.slotRetired || retained.realmCharge != logicalBefore || retained.physical != physicalBefore {
		t.Fatalf("EIO retirement lost proof-bearing charge: %#v", retained)
	}
	if realm.total != logicalBefore || realm.physicalTotal != physicalBefore || realm.retention.reserved != reservedBefore-1 {
		t.Fatalf("EIO retirement refunded before proof logical=%d/%d physical=%d/%d slots=%d/%d", realm.total, logicalBefore, realm.physicalTotal, physicalBefore, realm.retention.reserved, reservedBefore)
	}
	if wakes.Load() != 1 {
		t.Fatalf("EIO retirement slot wakes=%d want 1", wakes.Load())
	}
	if len(receipts) != 1 || receipts[0] != (wakeReceipt{logical: logicalBefore, physical: physicalBefore, slots: reservedBefore - 1}) {
		t.Fatalf("slot wake observed wrong published counters: %+v", receipts)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("EIO retirement did not retain file: %v", err)
	}

	*failing = false
	if settled := realm.RetirePane(key); !settled {
		t.Fatal("retry did not prove terminal retirement")
	}
	if _, present := realm.panes[key]; present {
		t.Fatal("proved retirement retained pane authority")
	}
	if realm.total != 0 || realm.physicalTotal != 0 {
		t.Fatalf("proved retirement retained ledgers logical=%d physical=%d", realm.total, realm.physicalTotal)
	}
	if realm.retention.reserved != 0 {
		t.Fatalf("proved retirement retained complete-pane slots: %d", realm.retention.reserved)
	}
	if wakes.Load() != 2 {
		t.Fatalf("proved retirement wakes=%d want slot+byte wakes", wakes.Load())
	}
	if len(receipts) != 2 || receipts[1] != (wakeReceipt{slots: reservedBefore - 1}) {
		t.Fatalf("byte wake preceded refund publication: %+v", receipts)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("proved retirement file err=%v want ENOENT", err)
	}
}

func TestSourceStartupRecoveryLedgerRetains1294ByteResidueUntilProof(t *testing.T) {
	options := rotationOptions(t)
	options.CompletePaneSlots = 2
	seed, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	key := rotationKey("$size", 917)
	admitRotationPredecessor(t, seed, key)
	appendCommittedOutput(t, seed, key, make([]byte, 916))
	path := storagePathsForTest(options, key).Journal
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 1294 {
		t.Fatalf("residue size=%d want live evidence shape 1294", info.Size())
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	options.StartupRecovery = true
	ops, failing := failingUnlinkOps()
	for start := 1; start <= 2; start++ {
		restarted, err := openRealm(options, ops)
		if err != nil {
			t.Fatal(err)
		}
		outcomes := restarted.ReconcileRecovered([]RecoveryDecision{{Key: key, Disposition: RecoveryRetireAbsent}})
		if len(outcomes) != 1 {
			t.Fatalf("start %d outcomes=%d", start, len(outcomes))
		}
		outcome := outcomes[0]
		if outcome.Decision != RecoveryRetireAbsent || outcome.Outcome != RecoveryRetainedError ||
			outcome.RetainedPhysical != 1294 || outcome.RetainedLogical == 0 || outcome.RetainedSlot || !outcome.RetryNeeded {
			t.Fatalf("start %d typed residue outcome=%+v", start, outcome)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("start %d lost unproved residue: %v", start, err)
		}
		if start == 2 {
			*failing = false
			settled := restarted.ReconcileRecovered([]RecoveryDecision{{Key: key, Disposition: RecoveryRetireAbsent}})
			if len(settled) != 1 || settled[0].Outcome != RecoveryRetiredAbsent || settled[0].RetryNeeded ||
				settled[0].RetainedLogical != 0 || settled[0].RetainedPhysical != 0 || settled[0].RetainedSlot {
				t.Fatalf("proved retry outcome=%+v", settled)
			}
		}
		if err := restarted.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("proved residue still present: %v", err)
	}
}

func TestTerminalRetirementENOENTIsProofAndIdempotent(t *testing.T) {
	options := rotationOptions(t)
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	key := rotationKey("$terminal-retire-enoent", 1)
	admitRotationPredecessor(t, realm, key)
	path := storagePathsForTest(options, key).Journal
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !realm.RetirePane(key) || !realm.RetirePane(key) {
		t.Fatal("ENOENT/idempotent retirement did not settle")
	}
	if realm.total != 0 || realm.physicalTotal != 0 || realm.retention.reserved != 0 {
		t.Fatalf("ENOENT retirement ledger logical=%d physical=%d slots=%d", realm.total, realm.physicalTotal, realm.retention.reserved)
	}
}
