package unifiedjournal

import (
	"errors"
	"reflect"
	"syscall"
	"testing"
)

func TestSettledFailedSourceReconstruction(t *testing.T) {
	for _, variant := range []string{"settled", "unsettled", "wrong-source", "geometry-held", "rotation-held", "unlink-failure"} {
		t.Run(variant, func(t *testing.T) {
			options := rotationOptions(t)
			options.CompletePaneSlots = 2 // one ordinary owner and the existing successor reserve
			realm, err := OpenRealm(options)
			if err != nil {
				t.Fatal(err)
			}
			defer realm.Close()
			old, next := rotationKey("$failed", 1), rotationKey("$failed", 2)
			source := SourceID{Realm: options.Realm, Server: old.Server, SocketPath: "/synthetic/failed.sock", SocketDevice: 1, SocketInode: 2, ServerPID: 101, ServerStartTime: 102, Session: old.Session, Window: old.Window, Pane: old.Pane, PanePID: 201, PaneStartTime: 202}
			if err := realm.ConfigureSourceQuota(2, 8, nil); err != nil {
				t.Fatal(err)
			}
			if err := realm.ReserveSource(old, source); err != nil {
				t.Fatal(err)
			}
			if err := realm.AdmitPane(old, Geometry{80, 24}); err != nil {
				t.Fatal(err)
			}
			writer, err := realm.BindWriter(old)
			if err != nil {
				t.Fatal(err)
			}
			verifiedOutput(t, realm, old, []byte("retained committed history"))
			snapshot, err := realm.ReadCommittedEvents(old)
			if err != nil {
				t.Fatal(err)
			}
			snapshotCopy, err := realm.ReadCommittedEvents(old)
			if err != nil {
				t.Fatal(err)
			}
			pane := realm.panes[old]
			var releaseOwner func()
			if variant == "geometry-held" {
				hold, err := realm.ReserveGeometry(old)
				if err != nil {
					t.Fatal(err)
				}
				releaseOwner = hold.Release
			}
			if variant == "rotation-held" {
				hold, err := realm.BeginRotationCapacity(old)
				if err != nil {
					t.Fatal(err)
				}
				releaseOwner = hold.Release
			}
			invalidatePane(pane, ReasonQuota)
			logical, physical, slots := realm.total, realm.physicalTotal, realm.AvailableCompletePaneSlots()
			if slots != 0 {
				t.Fatal("fixture must fill ordinary slots")
			}
			// Interface detection also lets this behavioral test run against the
			// immutable pre-fix source: it reaches the old admission refusal.
			if variant != "unsettled" {
				if owner, ok := any(writer).(interface{ SettleFailedOwner() }); ok {
					owner.SettleFailedOwner()
				}
			}
			attemptSource := source
			if variant == "wrong-source" {
				attemptSource.PaneStartTime++
			}
			unlink := realm.ops.unlinkat
			if variant == "unlink-failure" {
				realm.ops.unlinkat = func(int, string) error { return syscall.EIO }
			}
			for attempt := 0; attempt < 2; attempt++ {
				reservation, err := realm.BeginReconstructedPaneForSource(next, Geometry{80, 24}, attemptSource)
				if variant == "settled" {
					if err != nil {
						t.Fatalf("settled exact source cannot reconstruct: %v", err)
					}
					verifiedOutput(t, realm, next, []byte("fresh captured state"))
					reservation.Commit()
					break
				}
				if reservation != nil || err == nil {
					t.Fatalf("unproven reconstruction admitted: reservation=%v err=%v", reservation, err)
				}
				if realm.panes[old] != pane || realm.panes[next] != nil || pane.slotRetired || realm.total != logical || realm.physicalTotal != physical || realm.AvailableCompletePaneSlots() != slots || pane.reason != ReasonQuota {
					t.Fatal("refusal changed old authority or retained liabilities")
				}
				if used, unknown, _, leases := realm.SourceUsage(source); used != 1 || unknown != 0 || leases != 1 {
					t.Fatal("refusal refunded source liability")
				}
			}
			if variant != "settled" {
				realm.ops.unlinkat = unlink
				if releaseOwner != nil {
					releaseOwner()
				}
				if owner, ok := any(writer).(interface{ SettleFailedOwner() }); ok {
					owner.SettleFailedOwner()
				}
				reservation, err := realm.BeginReconstructedPaneForSource(next, Geometry{80, 24}, source)
				if err != nil {
					t.Fatalf("released exact owner cannot retry: %v", err)
				}
				verifiedOutput(t, realm, next, []byte("fresh captured state"))
				reservation.Commit()
			}
			if realm.panes[old] != nil || realm.AvailableCompletePaneSlots() != slots || pane.reason != ReasonQuota {
				t.Fatal("slot transfer or first cause changed")
			}
			if used, unknown, _, leases := realm.SourceUsage(source); used != 1 || unknown != 0 || leases != 1 {
				t.Fatal("successor does not own exactly one source liability")
			}
			if !reflect.DeepEqual(snapshot, snapshotCopy) {
				t.Fatal("independently retained snapshot changed")
			}
			if _, err := writer.Append(old, []byte("stale")); !errors.Is(err, ErrInvalidated) {
				t.Fatalf("stale writer revived: %v", err)
			}
			if realm.reservedCharge != 0 || realm.physicalReserved != 0 {
				t.Fatal("successor retained provisional allowances")
			}
			// An old object-bound handle cannot mark the replacement lifetime
			// settled, even after the replacement independently becomes untrusted.
			invalidatePane(realm.panes[next], ReasonQuota)
			if owner, ok := any(writer).(interface{ SettleFailedOwner() }); ok {
				owner.SettleFailedOwner()
			}
			third := rotationKey(old.Session, 3)
			if reservation, err := realm.BeginReconstructedPaneForSource(third, Geometry{80, 24}, source); reservation != nil || !errors.Is(err, ErrQuota) {
				t.Fatalf("stale writer granted replacement settlement: reservation=%v err=%v", reservation, err)
			}
		})
	}
}
