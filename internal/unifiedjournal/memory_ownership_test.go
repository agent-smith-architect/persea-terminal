package unifiedjournal

import (
	"bytes"
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestRecordingIdentityReclamationAndExactWriter(t *testing.T) {
	realm, err := OpenRealm(rotationOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	key := rotationKey("$reused", 1)
	if _, err := realm.BindWriter(key); !errors.Is(err, ErrInvalidated) {
		t.Fatal("writer created missing admission")
	}
	var stale []*Writer
	for i := 0; i < 1000; i++ {
		reservation, err := realm.BeginReconstructedPane(key, Geometry{80, 24})
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		writer, err := realm.BindWriter(key)
		if err != nil {
			t.Fatal(err)
		}
		for _, prior := range stale {
			if _, err := prior.Append(key, []byte("stale")); !errors.Is(err, ErrInvalidated) {
				t.Fatal("old handle wrote into reused key")
			}
			if err := prior.Sync(key); !errors.Is(err, ErrInvalidated) {
				t.Fatal("old handle synced reused key")
			}
		}
		if i == 0 {
			stale = append(stale, writer)
		}
		record, err := writer.Append(key, []byte("snapshot"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Append(key, nil); !errors.Is(err, ErrInvalidRecord) {
			t.Fatal("production writer accumulated unverified appends")
		}
		if err := writer.Sync(key); err != nil {
			t.Fatal(err)
		}
		if err := writer.AdvanceCommitted(key, record); err != nil {
			t.Fatal(err)
		}
		snapshot, err := realm.ReadCommittedEvents(key)
		if err != nil {
			t.Fatal(err)
		}
		if realm.ReclaimRetired(key) {
			t.Fatal("live owner was reclaimed")
		}
		reservation.Abort()
		if _, err := realm.Append(key, []byte("late")); !errors.Is(err, ErrInvalidated) {
			t.Fatal("abort lost tombstone before quiescence")
		}
		if !realm.ReclaimRetired(key) || realm.IdentityCount() != 0 {
			t.Fatal("quiescent identity did not plateau at zero")
		}
		if _, err := writer.Append(key, nil); !errors.Is(err, ErrInvalidated) {
			t.Fatal("stale production handle recreated key")
		}
		if !bytes.Equal(snapshot[0].Payload, []byte("snapshot")) {
			t.Fatal("reclamation changed caller snapshot")
		}
		if writer.pane.verified.view != nil || writer.pane.pendingHead != nil {
			t.Fatal("stale handle retained projection")
		}
	}
	// Compatibility is deliberate and separate: low-level byte-only Append can
	// still create at an absent key, but the admitted writer cannot bind to it.
	if _, err := realm.Append(key, []byte("legacy low-level")); err != nil {
		t.Fatal(err)
	}
	if _, err := realm.BindWriter(key); !errors.Is(err, ErrInvalidated) {
		t.Fatal("unadmitted byte journal gained production authority")
	}
}

func TestRecordingIdentityRetainsFailedUnlinkAndReservation(t *testing.T) {
	realm, err := OpenRealm(rotationOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	key := rotationKey("$held", 1)
	if err := realm.AdmitPane(key, Geometry{80, 24}); err != nil {
		t.Fatal(err)
	}
	verifiedOutput(t, realm, key, []byte("owned"))
	geometry, err := realm.ReserveGeometry(key)
	if err != nil {
		t.Fatal(err)
	}
	if realm.RetirePane(key) || realm.ReclaimRetired(key) {
		t.Fatal("held reservation permitted retirement")
	}
	geometry.Release()
	unlink := realm.ops.unlinkat
	realm.ops.unlinkat = func(int, string) error { return syscall.EIO }
	if realm.RetirePane(key) || realm.ReclaimRetired(key) {
		t.Fatal("failed unlink refunded identity")
	}
	if realm.IdentityCount() != 1 || realm.panes[key].verified.view == nil {
		t.Fatal("failed unlink dropped retained owner")
	}
	realm.ops.unlinkat = unlink
	if !realm.RetirePane(key) || realm.IdentityCount() != 0 {
		t.Fatal("settled owner not reclaimed")
	}
}

func TestRecordingIdentityUnsettledCeiling(t *testing.T) {
	realm, err := OpenRealm(rotationOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	for i := 0; i < MaxRealmIdentities; i++ {
		key := rotationKey(fmt.Sprintf("$tombstone%d", i), 1)
		reservation, err := realm.BeginReconstructedPane(key, Geometry{80, 24})
		if err != nil {
			t.Fatal(err)
		}
		reservation.Abort() // no caller quiescence proof yet
	}
	if _, err := realm.BeginReconstructedPane(rotationKey("$overflow", 1), Geometry{80, 24}); !errors.Is(err, ErrQuota) {
		t.Fatalf("unsettled identity ceiling: %v", err)
	}
	for key := range realm.panes {
		if !realm.ReclaimRetired(key) {
			t.Fatal("settled tombstone was not reusable")
		}
	}
	if realm.IdentityCount() != 0 {
		t.Fatal("identity ceiling became permanent refusal")
	}
}

func TestRecordingSnapshotAllocationMatchesOwnedCopies(t *testing.T) {
	realm, err := OpenRealm(rotationOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	key := rotationKey("$snapshot", 1)
	if err := realm.AdmitPane(key, Geometry{80, 24}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		verifiedOutput(t, realm, key, []byte{byte(i)})
	}
	charge, count, err := realm.SnapshotAllocation(key)
	if err != nil || count != 1000 || charge < 73000 {
		t.Fatalf("snapshot charge=%d records=%d err=%v", charge, count, err)
	}
	events, err := realm.ReadCommittedEvents(key)
	if err != nil {
		t.Fatal(err)
	}
	for i, event := range events {
		if len(event.Payload) != 1 || cap(event.Payload) != 1 || event.Payload[0] != byte(i) {
			t.Fatal("copied event lost payload isolation")
		}
	}
	events[0].Payload[0] = 123
	again, err := realm.ReadCommittedEvents(key)
	if err != nil || again[0].Payload[0] != 0 {
		t.Fatal("caller copy mutated authoritative projection")
	}
}
