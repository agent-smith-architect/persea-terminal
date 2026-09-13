package unifiedjournal

import (
	"bytes"
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestProjectionOwnershipFollowsProvenTombstoneUnlink(t *testing.T) {
	for _, kind := range []string{"adoption abort", "rotation abort", "rotation fatal"} {
		for _, unlink := range []string{"success", "enoent", "failure then retry"} {
			t.Run(kind+"/"+unlink, func(t *testing.T) {
				options := rotationOptions(t)
				realm, err := OpenRealm(options)
				if err != nil {
					t.Fatal(err)
				}
				defer realm.Close()
				key := rotationKey("$projection-ownership", 2)
				var settle func()
				if kind == "adoption abort" {
					reservation, err := realm.BeginReconstructedPane(key, Geometry{80, 24})
					if err != nil {
						t.Fatal(err)
					}
					settle = reservation.Abort
				} else {
					predecessor := rotationKey(key.Session, 1)
					admitRotationPredecessor(t, realm, predecessor)
					capacity, err := realm.BeginRotationCapacity(predecessor)
					if err != nil {
						t.Fatal(err)
					}
					defer capacity.Release()
					reservation, err := realm.BeginRotatedPane(key, Geometry{80, 24}, capacity)
					if err != nil {
						t.Fatal(err)
					}
					settle = reservation.Abort
					if kind == "rotation fatal" {
						settle = reservation.Fatal
					}
				}
				payload := bytes.Repeat([]byte{'v'}, 64<<10)
				verifiedOutput(t, realm, key, payload)
				snapshot, err := realm.ReadCommittedEvents(key)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := realm.Append(key, []byte("pending")); err != nil {
					t.Fatal(err)
				}
				pane := realm.panes[key]
				beforePayload, beforeMetadata, _ := realm.ProjectionUsage(key)
				beforePhysical, beforeLogical := pane.physical, pane.realmCharge
				if beforePayload == 0 || beforeMetadata == 0 || pane.pendingCount == 0 {
					t.Fatal("fixture has no retained ownership")
				}
				realUnlink := realm.ops.unlinkat
				realm.ops.unlinkat = func(fd int, name string) error {
					if unlink == "failure then retry" {
						return syscall.EIO
					}
					err := realUnlink(fd, name)
					if unlink == "enoent" && err == nil {
						return syscall.ENOENT
					}
					return err
				}
				settle()
				settledSlots := realm.AvailableCompletePaneSlots()
				settle()
				if realm.AvailableCompletePaneSlots() != settledSlots {
					t.Fatal("idempotent settlement changed slots")
				}
				if pane.state != EligibilityUntrusted || !pane.slotRetired {
					t.Fatal("lost tombstone identity/classification")
				}
				if unlink == "failure then retry" {
					gotPayload, gotMetadata, _ := realm.ProjectionUsage(key)
					if gotPayload != beforePayload || gotMetadata != beforeMetadata || pane.physical != beforePhysical || pane.realmCharge != beforeLogical {
						t.Fatal("failed unlink prematurely released an owner or charge")
					}
					realm.ops.unlinkat = realUnlink
					if !realm.RetirePane(key) || realm.panes[key] != nil {
						t.Fatal("unlink retry did not settle generation")
					}
				} else {
					if realm.panes[key] != pane {
						t.Fatal("settlement removed required tombstone")
					}
					gotPayload, gotMetadata, _ := realm.ProjectionUsage(key)
					if gotPayload != 0 || gotMetadata != 0 || pane.pendingHead != nil || pane.pendingTail != nil || pane.verified.view != nil {
						t.Errorf("proven unlink retained projection/pending ownership: payload=%d metadata=%d", gotPayload, gotMetadata)
					}
					if _, err := realm.ReadCommitted(key); !errors.Is(err, ErrInvalidated) {
						t.Errorf("released byte view=%v, want invalidated", err)
					}
					if _, err := realm.ReadCommittedEvents(key); !errors.Is(err, ErrInvalidated) {
						t.Errorf("released event view=%v, want invalidated", err)
					}
					if pane.physical != 0 || pane.realmCharge != 0 {
						t.Fatal("proven unlink failed to refund charges")
					}
				}
				if !bytes.Equal(snapshot[0].Payload, payload) {
					t.Fatal("settlement invalidated caller-owned snapshot")
				}
				if unlink != "failure then retry" {
					if _, err := realm.Append(key, []byte("late")); !errors.Is(err, ErrInvalidated) {
						t.Fatalf("tombstone accepted late append: %v", err)
					}
				}
			})
		}
	}
}

func TestReleasedProjectionPreservesCorruptionPrecedence(t *testing.T) {
	realm, err := OpenRealm(journalOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	key := reservationKey("$corrupt-projection", 2)
	reservation, err := realm.BeginReconstructedPane(key, Geometry{80, 24})
	if err != nil {
		t.Fatal(err)
	}
	verifiedOutput(t, realm, key, []byte("verified"))
	record, err := realm.Append(key, []byte("failed"))
	if err != nil {
		t.Fatal(err)
	}
	if err := realm.Sync(key); err != nil {
		t.Fatal(err)
	}
	realm.ops.readAt = func(*os.File, []byte, int64) (int, error) { return 0, syscall.EIO }
	if !errors.Is(realm.AdvanceCommitted(key, record), ErrCorruptJournal) {
		t.Fatal("fixture did not corrupt generation")
	}
	reservation.Abort()
	if payload, metadata, _ := realm.ProjectionUsage(key); payload != 0 || metadata != 0 {
		t.Errorf("corrupt tombstone retains projection=%d/%d", payload, metadata)
	}
	if _, err := realm.ReadCommitted(key); !errors.Is(err, ErrCorruptJournal) {
		t.Errorf("corrupt byte view precedence=%v", err)
	}
	if _, err := realm.ReadCommittedEvents(key); !errors.Is(err, ErrCorruptJournal) {
		t.Errorf("corrupt event view precedence=%v", err)
	}
}
