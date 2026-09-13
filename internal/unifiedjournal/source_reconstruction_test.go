package unifiedjournal

import (
	"errors"
	"syscall"
	"testing"
)

// The broker passes its proven source directly into reconstruction ownership.
func reconstructSourceForTest(realm *Realm, key PaneKey, geometry Geometry, source SourceID) (*AdoptionReservation, error) {
	return realm.BeginReconstructedPaneForSource(key, geometry, source)
}

func TestSourceReconstructionAfterRecoveredRotationOverlap(t *testing.T) {
	for _, variant := range []string{"same-geometry", "new-refit-incarnation", "attributed-geometry-ambiguity", "both-geometry-ambiguous"} {
		for _, blocked := range []bool{false, true} {
			t.Run(variant+map[bool]string{false: "/cleanup", true: "/failed-cleanup-retry"}[blocked], func(t *testing.T) {
				options := rotationOptions(t)
				options.CompletePaneSlots = 2
				realm, err := OpenRealm(options)
				if err != nil {
					t.Fatal(err)
				}
				first, second, next := rotationKey("$source-reconstruction", 1), rotationKey("$source-reconstruction", 2), rotationKey("$source-reconstruction", 3)
				initial, successorGeometry, nextGeometry := Geometry{80, 24}, Geometry{80, 24}, Geometry{80, 24}
				if variant != "same-geometry" {
					next.Incarnation = "fresh-refit-capture"
					nextGeometry = Geometry{120, 40}
				}
				if variant == "attributed-geometry-ambiguity" || variant == "both-geometry-ambiguous" {
					second.Incarnation = "prior-refit-capture"
					successorGeometry = Geometry{120, 40}
				}
				if err := realm.AdmitPane(first, initial); err != nil {
					t.Fatal(err)
				}
				verifiedOutput(t, realm, first, []byte("old recording"))
				capacity, err := realm.BeginRotationCapacity(first)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := realm.BeginRotatedPane(second, successorGeometry, capacity); err != nil {
					t.Fatal(err)
				}
				verifiedOutput(t, realm, second, []byte("committed successor bootstrap"))
				// Simulate loss after committed successor data but before the
				// rotation reservation retires the predecessor.
				if err := realm.Close(); err != nil {
					t.Fatal(err)
				}
				options.StartupRecovery = true
				realm, err = OpenRealm(options)
				if err != nil {
					t.Fatal(err)
				}
				defer realm.Close()
				firstDecision, secondDecision := RecoveryKeepExact, RecoveryKeepExact
				if variant == "attributed-geometry-ambiguity" || variant == "both-geometry-ambiguous" {
					firstDecision = RecoveryRetainAmbiguous
				}
				if variant == "both-geometry-ambiguous" {
					secondDecision = RecoveryRetainAmbiguous
				}
				realm.ReconcileRecovered([]RecoveryDecision{{Key: first, Disposition: firstDecision}, {Key: second, Disposition: secondDecision}})
				source := SourceID{Realm: options.Realm, Server: first.Server, SocketPath: "/synthetic/reconstruction.sock", SocketDevice: 1, SocketInode: 2, ServerPID: 101, ServerStartTime: 102, Session: first.Session, Window: first.Window, Pane: first.Pane, PanePID: 201, PaneStartTime: 202}
				known := map[PaneKey]SourceID{second: source}
				if firstDecision == RecoveryKeepExact {
					known[first] = source
				}
				if err := realm.ConfigureSourceQuota(2, 8, known); err != nil {
					t.Fatal(err)
				}
				if firstDecision != RecoveryKeepExact {
					// Models the broker's later proof from explicit live host facts
					// and the recovered header's original geometry.
					if err := realm.AttributeRecoveredSource(first, source); err != nil {
						t.Fatal(err)
					}
				}
				logical, physical := realm.total, realm.physicalTotal
				if used, unknown, _, leases := realm.SourceUsage(source); used != 2 || unknown != 0 || leases != 2 {
					t.Fatal("fixture did not retain both source liabilities")
				}
				if realm.AvailableCompletePaneSlots() != 0 || !realm.HasRecoveredCaptureCandidate(first.Server, first.Session) {
					t.Fatal("proven recovered source cannot reach capture at full slot bound")
				}
				unlink := realm.ops.unlinkat
				if blocked {
					realm.ops.unlinkat = func(int, string) error { return syscall.EIO }
				}
				reservation, err := reconstructSourceForTest(realm, next, nextGeometry, source)
				if blocked {
					if !errors.Is(err, ErrSourceQuota) || reservation != nil {
						t.Fatalf("unproven cleanup admitted readiness: reservation=%v err=%v", reservation, err)
					}
					if realm.total != logical || realm.physicalTotal != physical || realm.panes[next] != nil || realm.reservedCharge != 0 || realm.physicalReserved != 0 {
						t.Fatal("failed cleanup changed byte charges or exposed successor")
					}
					if used, unknown, _, leases := realm.SourceUsage(source); used != 2 || unknown != 0 || leases != 2 {
						t.Fatal("failed cleanup refunded source ownership")
					}
					realm.ops.unlinkat = unlink
					reservation, err = reconstructSourceForTest(realm, next, nextGeometry, source)
				}
				if err != nil {
					t.Fatalf("proven recovered supersession cannot reconstruct: %v", err)
				}
				if realm.panes[first] != nil || realm.panes[second] != nil {
					t.Fatal("settled recovered files retained owners")
				}
				if used, unknown, _, leases := realm.SourceUsage(source); used != 1 || unknown != 0 || leases != 1 {
					t.Fatal("replacement did not own exactly one source allowance")
				}
				verifiedOutput(t, realm, next, []byte("fresh verified initial state"))
				reservation.Commit()
				if got, err := realm.InitialGeometry(next); err != nil || got != nextGeometry {
					t.Fatalf("replacement geometry=%v err=%v", got, err)
				}
				if _, err := realm.ReadCommittedEvents(next); err != nil {
					t.Fatalf("replacement is not readable: %v", err)
				}
			})
		}
	}
}

func TestSourceReconstructionCannotReclaimCurrentOrUnknownOwners(t *testing.T) {
	for _, kind := range []string{"current-with-historical-recovery-key", "unknown-recovered"} {
		t.Run(kind, func(t *testing.T) {
			options := rotationOptions(t)
			options.CompletePaneSlots = 4
			realm, err := OpenRealm(options)
			if err != nil {
				t.Fatal(err)
			}
			first, second, next := rotationKey("$protected-owner", 1), rotationKey("$protected-owner", 2), rotationKey("$protected-owner", 3)
			for _, key := range []PaneKey{first, second} {
				if err := realm.AdmitPane(key, Geometry{80, 24}); err != nil {
					t.Fatal(err)
				}
				verifiedOutput(t, realm, key, []byte("owned bytes"))
			}
			source := SourceID{Realm: options.Realm, Server: first.Server, SocketPath: "/synthetic/protected.sock", SocketDevice: 1, SocketInode: 2, ServerPID: 101, ServerStartTime: 102, Session: first.Session, Window: first.Window, Pane: first.Pane, PanePID: 201, PaneStartTime: 202}
			known := map[PaneKey]SourceID{first: source, second: source}
			if kind == "unknown-recovered" {
				if err := realm.Close(); err != nil {
					t.Fatal(err)
				}
				options.StartupRecovery = true
				realm, err = OpenRealm(options)
				if err != nil {
					t.Fatal(err)
				}
				realm.ReconcileRecovered(nil)
				known = nil
			} else {
				// A recovered key can remain in immutable history after that
				// old file was settled and a current file reused the key. The
				// current pane's live state must override that stale history.
				realm.recovered = []Recovery{{Key: first, Outcome: RecoveryKeptExact}, {Key: second, Outcome: RecoveryKeptExact}}
			}
			defer realm.Close()
			if err := realm.ConfigureSourceQuota(2, 8, known); err != nil {
				t.Fatal(err)
			}
			if realm.HasRecoveredCaptureCandidate(first.Server, first.Session) {
				t.Fatal("live or unknown owner granted recovered capture admission")
			}
			logical, physical, slots := realm.total, realm.physicalTotal, realm.AvailableCompletePaneSlots()
			reservation, err := realm.BeginReconstructedPaneForSource(next, Geometry{80, 24}, source)
			if reservation != nil || !errors.Is(err, ErrSourceQuota) {
				t.Fatalf("protected owner was reclaimed: reservation=%v err=%v", reservation, err)
			}
			realm.ReclaimRetiredSession(next)
			if realm.panes[first] == nil || realm.panes[second] == nil || realm.total != logical || realm.physicalTotal != physical || realm.AvailableCompletePaneSlots() != slots {
				t.Fatal("source refusal changed protected ownership")
			}
			if kind == "unknown-recovered" {
				if used, unknown, _, leases := realm.SourceUsage(source); used != 0 || unknown != 2 || leases != 2 {
					t.Fatal("unknown source liabilities were waived")
				}
				for _, key := range []PaneKey{first, second} {
					if err := realm.AttributeRecoveredSource(key, source); err != nil {
						t.Fatal(err)
					}
				}
				if !realm.HasRecoveredCaptureCandidate(first.Server, first.Session) {
					t.Fatal("explicit attribution did not permit recovered capture")
				}
				reservation, err = realm.BeginReconstructedPaneForSource(next, Geometry{80, 24}, source)
				if err != nil {
					t.Fatalf("explicit later attribution did not unblock reconstruction: %v", err)
				}
				reservation.Abort()
			} else {
				for _, key := range []PaneKey{first, second} {
					if _, err := realm.ReadCommittedEvents(key); err != nil {
						t.Fatalf("current recording lost readiness: %v", err)
					}
				}
			}
		})
	}
}
