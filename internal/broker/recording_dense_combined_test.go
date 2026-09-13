package broker

import (
	"bytes"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/unifiedjournal"
)

// This conservative allocation lane combines the densest valid recovered
// projection with full queued input/publication and provider-copy budgets.
// It deliberately over-approximates simultaneous source admission: a full
// recovered realm cannot promise new source admission. The separate private
// tmux profile tests the supported workload and admission behavior.
func TestRecordingDenseCombinedOwnershipCalibration(t *testing.T) {
	fixture := os.Getenv("PERSEA_RECORDING_DENSE_FIXTURE")
	if os.Getenv("PERSEA_RECORDING_COMBINED") != "1" || fixture == "" {
		t.Skip("isolated dense combined ownership calibration; generate the journal fixture first")
	}
	for round := 0; round < 3; round++ {
		t.Run(strconv.Itoa(round), func(t *testing.T) {
			realm, err := unifiedjournal.OpenRealm(unifiedjournal.OpenOptions{
				RuntimeDir: fixture, Realm: "realm-a", BrokerIncarnation: "broker-incarnation-a",
				UID: os.Geteuid(), GID: os.Getegid(), DirectoryMode: 0o700, FileMode: 0o600,
				PaneCapBytes: 8 << 20, RealmCapBytes: 64 << 20,
				PanePhysicalCapBytes: 24 << 20, PhysicalCapBytes: 72 << 20, CompletePaneSlots: 9,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer realm.Close()
			effects := &UnifiedDevPaneEffects{realm: realm}
			effects.readers.copies, effects.transients.copies = &effects.copies, &effects.copies
			managerEntered, managerRelease := make(chan struct{}), make(chan struct{})
			dispatchEntered, dispatchRelease := make(chan struct{}), make(chan struct{})
			var managerOnce, dispatchOnce, releaseOnce sync.Once
			clock := newRetentionManualClock()
			options := retentionTrialOptions{
				realm: realm, journalMu: &effects.journalMu,
				maxBatchBytes: 64 << 10, maxFeedDelay: 16 * time.Millisecond, now: clock.Now, after: clock.After,
				hook: func(point string, _ unifiedjournal.PaneKey) {
					if point == "before_dispatch_fence_publish" {
						managerOnce.Do(func() { close(managerEntered) })
						<-managerRelease
					}
				},
				observe: func(string, map[string]int64) {
					dispatchOnce.Do(func() { close(dispatchEntered) })
					<-dispatchRelease
				},
			}
			provider := &recordingProfileEffects{recordingMemoryEffects: &recordingMemoryEffects{UnifiedDevPaneEffects: effects, options: options}}
			registry := newPaneRegistry(provider)
			trial := registry.retention
			defer func() {
				releaseOnce.Do(func() { close(managerRelease); close(dispatchRelease) })
				_ = registry.Close()
			}()
			trial.publishObservation("dense_hold", nil, nil)
			<-dispatchEntered
			if _, err := trial.startDispatchFence(); err != nil {
				t.Fatal(err)
			}
			<-managerEntered
			for i := 0; i < 8; i++ {
				key := journalKey(retentionWitness("%"+strconv.Itoa(i+100), "dense-queued-"+strconv.Itoa(i)))
				if _, _, err := trial.ensureGeneration(key, true); err != nil {
					t.Fatal(err)
				}
				if err := trial.WritePane(key, bytes.Repeat([]byte{'q'}, 8<<20)); err != nil {
					t.Fatal(err)
				}
			}
			fields := map[string]int64{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6, "g": 7, "h": 8}
			for len(trial.outbox) < cap(trial.outbox) {
				trial.publishObservation("dense_full", fields, nil)
			}
			var leases []*recordingReaderLease
			var snapshots [][]unifiedjournal.Event
			defer func() {
				for i, lease := range leases {
					if i < len(snapshots) {
						snapshots[i] = nil
					}
					lease.releaseSnapshot()
					lease.detach()
				}
			}()
			var records int64
			recoveries := realm.Recovered()
			if len(recoveries) != 3 {
				t.Fatal("wrong dense fixture generation count", len(recoveries))
			}
			// Four actual copies retain three distinct projections plus a second
			// subscriber to the first generation.
			for _, recovered := range append(recoveries, recoveries[0]) {
				size, count, err := realm.SnapshotAllocation(recovered.Key)
				if err != nil {
					t.Fatal(err)
				}
				lease, err := effects.readers.acquire(size)
				if err != nil {
					t.Fatal(err)
				}
				leases = append(leases, lease)
				events, err := realm.ReadCommittedEvents(recovered.Key)
				if err != nil {
					t.Fatal(err)
				}
				snapshots = append(snapshots, events)
				records += count
			}
			empty, err := effects.readers.acquire(0)
			if err != nil {
				t.Fatal(err)
			}
			leases = append(leases, empty)
			// Fill real bounded per-reader tail owners, including their existing
			// minimum allocation credit, without inventing one oversized event.
			var padding [][]byte
			for _, lease := range leases {
				for effects.readers.snapshot().Bytes < recordingReaderBytes {
					room := int64(recordingReaderBytes) - effects.readers.snapshot().Bytes
					size := min(room, 64<<10)
					if !lease.reserveEvent(size) {
						break
					}
					padding = append(padding, bytes.Repeat([]byte{'r'}, int(size)))
					defer lease.releaseEvent(size)
				}
			}
			physical, reserved, capacity := realm.PhysicalBudget()
			if physical < 71<<20 || records < 1_000_000 || trial.bUsed != 64<<20 || len(trial.outbox) != retentionDefaultEnvelopeLimit+retentionDefaultPaneLimit+2 || effects.readers.snapshot().Bytes != recordingReaderBytes {
				t.Fatal("dense owner saturation incomplete", physical, records, trial.bUsed, len(trial.outbox), effects.readers.snapshot())
			}
			t.Logf("dense_owners records=%d outbox=%d ingress=%d reader=%d physical=%d physical_reserved=%d physical_cap=%d", records, len(trial.outbox), trial.bUsed, effects.readers.snapshot().Bytes, physical, reserved, capacity)
			recordingCalibrateCopyChurn(t, effects, snapshots)
			runtime.KeepAlive(padding)
		})
	}
}
