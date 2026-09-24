package unifiedjournal

import (
	"errors"
	"syscall"
	"testing"
)

func TestRotationCommittedSuccessorFailureSettlesByUnlinkProof(t *testing.T) {
	for _, test := range []struct {
		name      string
		unlinkErr error
	}{
		{name: "unlink"},
		{name: "enoent", unlinkErr: syscall.ENOENT},
		{name: "eio", unlinkErr: syscall.EIO},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := rotationOptions(t)
			options.CompletePaneSlots = 3
			ops := realJournalOps()
			realUnlink := ops.unlinkat
			failSuccessor := false
			ops.unlinkat = func(dirfd int, name string) error {
				if failSuccessor {
					return test.unlinkErr
				}
				return realUnlink(dirfd, name)
			}
			realm, err := openRealm(options, ops)
			if err != nil {
				t.Fatal(err)
			}
			defer realm.Close()
			predecessor := rotationKey("$committed-failure", 1)
			successor := rotationKey("$committed-failure", 2)
			admitRotationPredecessor(t, realm, predecessor)
			capacity, err := realm.BeginRotationCapacity(predecessor)
			if err != nil {
				t.Fatal(err)
			}
			reservation, err := realm.BeginRotatedPane(successor, Geometry{Columns: 80, Rows: 24}, capacity)
			if err != nil {
				t.Fatal(err)
			}
			appendCommittedOutput(t, realm, successor, []byte("successor-bootstrap"))
			reservation.Commit()
			afterCommitSlots := realm.AvailableCompletePaneSlots()

			failSuccessor = test.unlinkErr != nil
			reservation.FailCommitted()
			pane := realm.panes[successor]
			if pane != nil && (!pane.slotRetired || pane.state == EligibilityContinuous) {
				t.Fatalf("failed successor remained eligible: %#v", pane)
			}
			if got := realm.AvailableCompletePaneSlots(); got != afterCommitSlots+1 {
				t.Fatalf("after failure slots=%d want %d", got, afterCommitSlots+1)
			}
			if test.unlinkErr == nil || errors.Is(test.unlinkErr, syscall.ENOENT) {
				if pane != nil || realm.total != 0 || realm.physicalTotal != 0 {
					t.Fatalf("unlink-proven successor residue pane=%#v logical=%d physical=%d", pane, realm.total, realm.physicalTotal)
				}
				reservation.FailCommitted() // idempotent after proof
				return
			}

			// EIO cannot justify a refund. The retired generation remains fully
			// charged until a later sweep obtains unlink proof.
			if pane == nil || realm.total == 0 || realm.physicalTotal == 0 {
				t.Fatalf("EIO lost retained ledger pane=%#v logical=%d physical=%d", pane, realm.total, realm.physicalTotal)
			}
			failSuccessor = false
			reservation.FailCommitted()
			if realm.panes[successor] != nil || realm.total != 0 || realm.physicalTotal != 0 {
				t.Fatalf("EIO retry did not converge pane=%#v logical=%d physical=%d", realm.panes[successor], realm.total, realm.physicalTotal)
			}
		})
	}
}
