package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/unifiedjournal"
)

func TestSourceHardCapUsesCommittedLogicalBytes(t *testing.T) {
	const logicalCap = int64(128 << 10)
	shapes := []struct {
		name string
		next func(int64) int
	}{
		{name: "one_byte", next: func(int64) int { return 1 }},
		{name: "64_KiB", next: func(remaining int64) int {
			if remaining < 64<<10 {
				return int(remaining)
			}
			return 64 << 10
		}},
		{name: "mixed_records", next: func(remaining int64) int {
			for _, size := range []int{3, 1021, 17 << 10, 64 << 10, 4093} {
				if int64(size) <= remaining {
					return size
				}
			}
			return int(remaining)
		}},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			runtimeDir := shortTempDir(t)
			realm, err := unifiedjournal.OpenRealm(unifiedjournal.OpenOptions{
				RuntimeDir: runtimeDir, Realm: "hard-cap-" + shape.name, BrokerIncarnation: "hard-cap",
				UID: os.Getuid(), GID: os.Getgid(), DirectoryMode: 0o700, FileMode: 0o600,
				PaneCapBytes: logicalCap, RealmCapBytes: 2 * logicalCap,
				PanePhysicalCapBytes: 256 << 20, PhysicalCapBytes: 512 << 20,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer realm.Close()
			key := rotationScheduleKey("$hard-cap-"+shape.name, 1)
			if err := realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
				t.Fatal(err)
			}

			remaining := logicalCap - 1
			records := 0
			for remaining > 0 {
				size := shape.next(remaining)
				if size <= 0 || int64(size) > remaining {
					t.Fatalf("invalid split size %d for remaining %d", size, remaining)
				}
				if _, err := realm.Append(key, bytes.Repeat([]byte{'x'}, size)); err != nil {
					t.Fatalf("append cap-1 record %d: %v", records, err)
				}
				remaining -= int64(size)
				records++
			}
			if err := realm.Sync(key); err != nil {
				t.Fatalf("commit cap-1: %v", err)
			}
			effects := &UnifiedDevPaneEffects{realm: realm}
			// This direct-ledger fixture is its own journal owner.
			effects.publishPressure()
			pressureAtMinusOne, err := effects.readRotationPressure(key)
			if err != nil {
				t.Fatal(err)
			}
			if pressureAtMinusOne.logical != logicalCap-1 || pressureAtMinusOne.logicalCap != logicalCap {
				t.Fatalf("committed cap-1 pressure=%d/%d", pressureAtMinusOne.logical, pressureAtMinusOne.logicalCap)
			}

			pending := &unifiedRotationPending{}
			if err := pending.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: bytes.Repeat([]byte{'p'}, 64<<10)}); err != nil {
				t.Fatal(err)
			}
			pressureWithPending, err := effects.readRotationPressure(key)
			if err != nil {
				t.Fatal(err)
			}
			if pressureWithPending.logical != pressureAtMinusOne.logical {
				t.Fatalf("pending bytes advanced logical pressure: before=%d after=%d pending=%d", pressureAtMinusOne.logical, pressureWithPending.logical, len(pending.data))
			}

			if _, err := realm.Append(key, []byte{'y'}); err != nil {
				t.Fatalf("append exact cap: %v", err)
			}
			if err := realm.Sync(key); err != nil {
				t.Fatalf("commit exact cap: %v", err)
			}
			effects.publishPressure()
			pressureAtCap, err := effects.readRotationPressure(key)
			if err != nil {
				t.Fatal(err)
			}
			physicalAtCap, _ := realm.PanePhysical(key)
			if pressureAtCap.logical != logicalCap || physicalAtCap <= pressureAtCap.logical {
				t.Fatalf("cap accounting logical=%d physical=%d want logical=%d and framing-heavy physical>logical", pressureAtCap.logical, physicalAtCap, logicalCap)
			}

			requested := int64(1)
			if _, err := realm.Append(key, bytes.Repeat([]byte{'z'}, int(requested))); !errors.Is(err, unifiedjournal.ErrQuota) {
				t.Fatalf("first-over append=%v want ErrQuota", err)
			}
			logicalAfterRefusal, _ := realm.PaneLogical(key)
			physicalAfterRefusal, _ := realm.PanePhysical(key)
			if logicalAfterRefusal != logicalCap || physicalAfterRefusal != physicalAtCap || realm.Reason(key) != unifiedjournal.ReasonQuota {
				t.Fatalf("refused request changed committed driver: logical=%d physical=%d reason=%d", logicalAfterRefusal, physicalAfterRefusal, realm.Reason(key))
			}
			events, err := realm.ReadCommittedEvents(key)
			if err != nil || len(events) != 0 {
				t.Fatalf("invalidated hard-cap projection exposed output: events=%d err=%v", len(events), err)
			}
			if _, err := realm.Append(key, []byte{'w'}); !errors.Is(err, unifiedjournal.ErrInvalidated) {
				t.Fatalf("post-cap append=%v want ErrInvalidated", err)
			}
			t.Logf("rotationLF-F4A receipt split=%s records=%d cap-1=%d cap=%d requested_first_over=%d pending=%d physical=%d",
				shape.name, records, pressureAtMinusOne.logical, pressureAtCap.logical, requested, len(pending.data), physicalAtCap)
		})
	}
}

func TestSourceFatalHardCapSettlement(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux hard-cap settlement")
	}
	saved := unifiedJournalCaps
	unifiedJournalCaps.pane = 256 << 10
	unifiedJournalCaps.realm = 8 << 20
	unifiedJournalCaps.panePhysical = 64 << 20
	unifiedJournalCaps.realmPhysical = 128 << 20
	t.Cleanup(func() { unifiedJournalCaps = saved })

	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "hard-cap-hard", "sleep 600")
	adoption, err := fixture.effects.AdoptSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	_, _, subscriber, cancelSubscriber, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatalf("open subscriber: %v", err)
	}
	defer cancelSubscriber()
	fixture.effects.mu.Lock()
	unit := fixture.effects.units[sessionID]
	var witness controlmode.PaneWitness
	if unit != nil && len(unit.witnesses) == 1 {
		witness = unit.witnesses[0]
	}
	fixture.effects.mu.Unlock()
	if witness == (controlmode.PaneWitness{}) {
		t.Fatal("adopted observer witness unavailable")
	}
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for deliveredForTest := range subscriber.events() {
			subscriber.releaseEvent(deliveredForTest)
		}
	}()

	var closeCount atomic.Int64
	var edgeMu sync.Mutex
	var edges []string
	fixture.effects.subscriberCloseEdge = func(key unifiedjournal.PaneKey, _ *unifiedDevSubscriber, reason proto.SubscriberCloseReason) {
		if key == adoption.Key && reason == proto.SubscriberClosedGenerationFailed {
			closeCount.Add(1)
		}
	}
	fixture.effects.rotationEdge = func(session, edge string) {
		if session != sessionID {
			return
		}
		edgeMu.Lock()
		edges = append(edges, edge)
		edgeMu.Unlock()
	}
	// Adoption can leave a final control-mode byte queued behind its capture
	// boundary. Settle that real byte before deriving headroom from the journal.
	if err := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: witness, Label: "start"}); err != nil {
		t.Fatalf("initial settle boundary: %v", err)
	}
	if err := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: witness, Label: "end"}); err != nil {
		t.Fatalf("initial settle release: %v", err)
	}

	fixture.effects.journalMu.Lock()
	before, cap := fixture.effects.realm.PaneLogical(adoption.Key)
	slotsAfterAdoption := fixture.effects.realm.AvailableCompletePaneSlots()
	fixture.effects.journalMu.Unlock()
	if before >= cap {
		t.Fatalf("bootstrap charge=%d already reached cap=%d", before, cap)
	}
	headroom := cap - before
	if err := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: witness, Data: bytes.Repeat([]byte{'X'}, int(headroom-1))}); err != nil {
		t.Fatalf("cap-1 output: %v", err)
	}
	if err := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: witness, Label: "start"}); err != nil {
		fixture.effects.journalMu.Lock()
		got, _ := fixture.effects.realm.PaneLogical(adoption.Key)
		reason := fixture.effects.realm.Reason(adoption.Key)
		fixture.effects.journalMu.Unlock()
		t.Fatalf("cap-1 boundary: %v logical=%d/%d before=%d headroom=%d reason=%d", err, got, cap, before, headroom, reason)
	}
	if err := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: witness, Label: "end"}); err != nil {
		t.Fatalf("cap-1 release: %v", err)
	}
	fixture.effects.journalMu.Lock()
	committedAtMinusOne, _ := fixture.effects.realm.PaneLogical(adoption.Key)
	fixture.effects.journalMu.Unlock()
	if committedAtMinusOne != cap-1 {
		t.Fatalf("runtime cap-1 charge=%d want %d", committedAtMinusOne, cap-1)
	}
	if err := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: witness, Data: []byte{'Y'}}); err != nil {
		t.Fatalf("exact-cap output: %v", err)
	}
	if err := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: witness, Label: "start"}); err != nil {
		t.Fatalf("exact-cap boundary: %v", err)
	}
	if err := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: witness, Label: "end"}); err != nil {
		t.Fatalf("exact-cap release: %v", err)
	}
	fixture.effects.journalMu.Lock()
	committedAtCap, _ := fixture.effects.realm.PaneLogical(adoption.Key)
	physicalAtCap, _ := fixture.effects.realm.PanePhysical(adoption.Key)
	fixture.effects.journalMu.Unlock()

	fixture.effects.requestRotationEvaluation(adoption.Key)
	time.Sleep(50 * time.Millisecond)
	if err := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: witness, Data: []byte{'Z'}}); err != nil {
		t.Fatalf("first-over submission: %v", err)
	}
	firstOverErr := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: witness, Label: "start"})
	if firstOverErr == nil {
		t.Fatal("first-over boundary succeeded")
	}
	// The real unit returns the same classified storage error from its
	// ObservePane call and the supervisor invokes this one reap owner. This test
	// drove that call from the registry seam, so invoke the identical owner
	// synchronously instead of depending on another tmux byte to wake it.
	fixture.effects.reapUnit(unit)
	if reason := waitRotationSubscriberClose(t, subscriber); reason != proto.SubscriberClosedGenerationFailed {
		t.Fatalf("hard-cap close=%q want %q", reason, proto.SubscriberClosedGenerationFailed)
	}
	<-drainDone
	settled := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fixture.effects.subscriberMu.Lock()
		_, bucket := fixture.effects.subscribers[adoption.Key]
		fixture.effects.subscriberMu.Unlock()
		fixture.effects.mu.Lock()
		_, unit := fixture.effects.units[sessionID]
		rotation := fixture.effects.rotation
		fixture.effects.mu.Unlock()
		fixture.effects.journalMu.Lock()
		logical, reservedLogical, _ := fixture.effects.realm.LogicalBudget()
		physical, reservedPhysical, _ := fixture.effects.realm.PhysicalBudget()
		slots := fixture.effects.realm.AvailableCompletePaneSlots()
		fixture.effects.journalMu.Unlock()
		if !bucket && !unit && rotation == nil && logical == cap && reservedLogical == 0 && physical == physicalAtCap && reservedPhysical == 0 && slots == slotsAfterAdoption && fixture.journalFileCount(t) == 1 {
			settled = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !settled {
		fixture.effects.subscriberMu.Lock()
		_, bucket := fixture.effects.subscribers[adoption.Key]
		fixture.effects.subscriberMu.Unlock()
		fixture.effects.mu.Lock()
		_, unit := fixture.effects.units[sessionID]
		rotation := fixture.effects.rotation
		_, active := fixture.effects.active[sessionID]
		fixture.effects.mu.Unlock()
		fixture.effects.journalMu.Lock()
		logical, reservedLogical, _ := fixture.effects.realm.LogicalBudget()
		physical, reservedPhysical, _ := fixture.effects.realm.PhysicalBudget()
		slots := fixture.effects.realm.AvailableCompletePaneSlots()
		fixture.effects.journalMu.Unlock()
		fixture.registry.retention.mu.Lock()
		generation := fixture.registry.retention.generations[adoption.Key]
		var runtimeState string
		if generation != nil {
			runtimeState = fmt.Sprintf("failed=%v refs=%d queued=%v spent=%v retire=%v durable=%v lifetime=%v",
				generation.failed, generation.refs, generation.cleanupQueued, generation.cleanupSpent, generation.retireRequested, generation.durableRequested, generation.lifetimeReleased)
		}
		fixture.registry.retention.mu.Unlock()
		t.Fatalf("hard-cap settlement timeout: bucket=%v unit=%v active=%v rotation=%v logical=%d+%d want=%d physical=%d+%d want=%d slots=%d want=%d files=%d runtime={%s} first_over=%v",
			bucket, unit, active, rotation != nil, logical, reservedLogical, cap, physical, reservedPhysical, physicalAtCap, slots, slotsAfterAdoption, fixture.journalFileCount(t), runtimeState, firstOverErr)
	}

	if closeCount.Load() != 1 {
		t.Fatalf("typed terminal close count=%d want 1", closeCount.Load())
	}
	if _, _, _, _, err := openSnapshotTailForTest(t, fixture.effects, sessionID); err == nil {
		t.Fatal("hard-cap generation still accepted a fresh subscriber")
	}
	edgeMu.Lock()
	seenEdges := append([]string(nil), edges...)
	edgeMu.Unlock()
	for _, edge := range seenEdges {
		if edge == "successor_owner_held" || edge == "successor_materialized" || edge == "bootstrap_durable" || edge == "active_swapped" {
			t.Fatalf("hard-cap path reached successor edge %q: %v", edge, seenEdges)
		}
	}
	if committedAtCap != cap {
		t.Fatalf("pre-fault committed charge=%d want cap=%d", committedAtCap, cap)
	}
	t.Logf("rotationLF-F4B receipt cap-1=%d logical=%d/%d physical=%d first_over=%v close=%q close_count=%d successor_edges=%v retained_slots=%d retained_logical=%d retained_physical=%d",
		committedAtMinusOne, committedAtCap, cap, physicalAtCap, firstOverErr, proto.SubscriberClosedGenerationFailed, closeCount.Load(), seenEdges, slotsAfterAdoption, cap, physicalAtCap)
}
