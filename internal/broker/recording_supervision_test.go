package broker

import (
	"bytes"
	"context"
	"errors"
	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/unifiedjournal"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestRecordingSupervisionHeldOwnerCutoff(t *testing.T) {
	for _, stage := range []string{"append", "sync", "commit", "callback"} {
		t.Run(stage, func(t *testing.T) {
			realm := openRetentionRealm(t, "supervise-"+stage)
			entered, release := make(chan struct{}), make(chan struct{})
			var enterOnce, releaseOnce sync.Once
			block := func() { enterOnce.Do(func() { close(entered) }); <-release }
			shadow := &retentionShadowEffects{realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock()}
			if stage == "callback" {
				shadow.beforeWrite = block
			} else {
				shadow.stage = func(s string, _ unifiedjournal.PaneKey) error {
					if s == stage {
						block()
					}
					return nil
				}
			}
			registry := newPaneRegistry(shadow)
			defer func() { releaseOnce.Do(func() { close(release) }); _ = registry.Close() }()
			witness := retentionWitness("%supervision", "supervision-inc")
			if err := registry.AdmitPane(witness); err != nil {
				t.Fatal(err)
			}
			if err := feedRetentionOutput(registry, controlmode.NewDecoder(), witness, bytes.Repeat([]byte{'x'}, 64<<10)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("owner not held")
			}
			samples := registry.retention.pendingSamples()
			if len(samples) != 1 || samples[0].oldestMono == 0 {
				t.Fatalf("missing accepted owner: %+v", samples)
			}
			registry.retention.mu.Lock()
			independent := make(chan []recordingPendingSample, 1)
			go func() { independent <- registry.retention.pendingSamples() }()
			select {
			case <-independent:
			case <-time.After(time.Second):
				registry.retention.mu.Unlock()
				t.Fatal("sample joined runtime lock")
			}
			registry.retention.mu.Unlock()
			effects := &UnifiedDevPaneEffects{observer: registry, supervisionStallLimit: time.Nanosecond}
			effects.superviseRecording()
			registry.mu.Lock()
			failed := registry.failedIncarnationLocked(witness)
			registry.mu.Unlock()
			if !failed {
				t.Fatal("held owner did not get authoritative cutoff")
			}
			if s := registry.retention.progressSnapshot(); s.IngressBytes != 64<<10 || s.OutboxBytes != 64<<10 {
				t.Fatalf("cutoff released held credits: %+v", s)
			}
			if s := registry.retention.pendingSamples(); len(s) != 1 || s[0].oldestMono != samples[0].oldestMono {
				t.Fatalf("ownership age reset: %+v", s)
			}
			if err := feedRetentionOutput(registry, controlmode.NewDecoder(), witness, []byte("late")); err == nil {
				t.Fatal("late output accepted")
			}
			releaseOnce.Do(func() { close(release) })
			if err := registry.Close(); !errors.Is(err, errRecordingStalled) {
				t.Fatalf("fault cause not retained: %v", err)
			}
			if s := registry.retention.progressSnapshot(); s.IngressBytes != 0 || s.OutboxBytes != 0 {
				t.Fatalf("owner did not settle: %+v", s)
			}
		})
	}
}

func TestRecordingSupervisionPressureIndependent(t *testing.T) {
	realm := openRetentionRealm(t, "pressure-independent")
	effects := &UnifiedDevPaneEffects{realm: realm}
	effects.journalMu.publish = effects.publishPressure
	key := journalKey(retentionWitness("%pressure", "pressure-inc"))
	effects.journalMu.Lock()
	if _, err := realm.Append(key, []byte("abc")); err != nil {
		effects.journalMu.Unlock()
		t.Fatal(err)
	}
	effects.journalMu.Unlock()
	effects.journalMu.Lock()
	result := make(chan unifiedRotationPressure, 1)
	go func() { p, _ := effects.readRotationPressure(key); result <- p }()
	select {
	case p := <-result:
		if p.logical != 3 {
			t.Errorf("lost owner publication: %+v", p)
		}
	case <-time.After(time.Second):
		t.Error("pressure reader joined journal lock")
	}
	effects.journalMu.Unlock()
}

func TestRecordingSupervisionLifecycleSlot(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	effects := &UnifiedDevPaneEffects{
		spawns: make(chan unifiedDevCommand), exits: make(chan *unifiedDevUnit),
		active:         map[string]unifiedjournal.PaneKey{"$1": {Session: "$1"}},
		rotationStates: make(map[string]*unifiedRotationState), rotationWake: make(chan struct{}, 1),
		rotationNow: time.Now, rotationPressure: func(unifiedjournal.PaneKey) (unifiedRotationPressure, error) {
			once.Do(func() { close(entered) })
			<-release
			return unifiedRotationPressure{}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- effects.RunObserver(ctx, nil) }()
	defer func() { cancel(); close(release) }()
	effects.wakeRotationScheduler()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("lifecycle slot not entered")
	}
	reply := make(chan unifiedDevCommandResult, 1)
	queuedContext, stopQueued := context.WithCancel(context.Background())
	queuedReply := make(chan unifiedDevCommandResult, 1)
	memory, err := effects.transients.acquire()
	if err != nil {
		t.Fatal(err)
	}
	effects.spawns <- unifiedDevCommand{context: queuedContext, memory: memory, done: queuedReply}
	stopQueued()
	select {
	case r := <-queuedReply:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("queued cancel: %v", r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled pending spawn retained behind held job")
	}
	effects.transients.mutex().Lock()
	owners := effects.transients.units
	effects.transients.mutex().Unlock()
	if owners != 0 {
		t.Fatal("canceled pending founding owner not refunded")
	}
	if effects.lifecycle.snapshot().InFlight != 1 {
		t.Fatal("held lifecycle snapshot unavailable")
	}
	for n := 0; n < recordingObserverUnitLimit; n++ {
		select {
		case effects.spawns <- unifiedDevCommand{done: make(chan unifiedDevCommandResult, 1)}:
		case <-time.After(time.Second):
			t.Fatal("bounded queue admission blocked")
		}
	}
	select {
	case effects.spawns <- unifiedDevCommand{done: reply}:
	case <-time.After(time.Second):
		t.Fatal("held lifecycle blocked request intake")
	}
	select {
	case r := <-reply:
		if !errors.Is(r.err, errRecordingTransients) {
			t.Fatalf("busy admission: %v", r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("held lifecycle blocked admission refusal")
	}
	if len(effects.recordingExitRecords()) != 0 {
		t.Fatal("unexpected result")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("held lifecycle blocked shutdown")
	}
}

func TestRecordingSupervisionIdleAndProgressHealthy(t *testing.T) {
	realm := openRetentionRealm(t, "healthy-progress")
	shadow := &retentionShadowEffects{realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock()}
	registry := newPaneRegistry(shadow)
	defer registry.Close()
	witness := retentionWitness("%healthy", "healthy-inc")
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	effects := &UnifiedDevPaneEffects{observer: registry, supervisionStallLimit: time.Second}
	effects.superviseRecording()
	reservation, err := registry.retention.reserveAdmitted(journalKey(witness), 10)
	if err != nil {
		t.Fatal(err)
	}
	registry.retention.mu.Lock()
	reservation.acceptedMono = recordingMonoNow() - int64(time.Hour)
	reservation.generation.lastProgressMono = recordingMonoNow()
	registry.retention.publishPendingLocked(reservation.generation)
	registry.retention.mu.Unlock()
	effects.superviseRecording()
	registry.mu.Lock()
	failed := registry.failedIncarnationLocked(witness)
	registry.mu.Unlock()
	if failed {
		t.Fatal("old but progressing owner was faulted")
	}
	registry.retention.cancelReservation(reservation)
	effects.superviseRecording()
}

func TestRecordingSupervisionReservationBound(t *testing.T) {
	size := unsafe.Sizeof(retentionReservation{})
	if size > 96 {
		t.Fatalf("reservation shape needs worksheet requalification: size=%d", size)
	}
	t.Logf("reservation size=%d maximum live=18204 current+just-settled=%d bytes; actual allocation checked by paired benchmark", size, 2*18204*size)
}

var recordingReservationBenchmark *retentionReservation

func BenchmarkRecordingSupervisionReservation(b *testing.B) {
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		recordingReservationBenchmark = &retentionReservation{q: 1}
	}
}

func TestRecordingSupervisionRetainedExitAfterOwnerRemoval(t *testing.T) {
	realm := openRetentionRealm(t, "exit-dispatch-held")
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce sync.Once
	shadow := &retentionShadowEffects{realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(), beforeWrite: func() { enteredOnce.Do(func() { close(entered) }); <-release }}
	registry := newPaneRegistry(shadow)
	defer func() { close(release); _ = registry.Close() }()
	witness := retentionWitness("%exit", "exit-inc")
	witness.Session.Session = "$1"
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	if err := feedRetentionOutput(registry, controlmode.NewDecoder(), witness, bytes.Repeat([]byte{'x'}, 64<<10)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("dispatcher not held")
	}
	effects := &UnifiedDevPaneEffects{units: make(map[string]*unifiedDevUnit), panes: make(map[string]*unifiedDevBirth), active: make(map[string]unifiedjournal.PaneKey)}
	effects.sessionPresence = func(config.TmuxServer, string) (bool, bool) { return false, false }
	// Exercise real reap map removal without keeping a retired unit pointer.
	func() {
		unit := &unifiedDevUnit{owner: effects, sessionID: "$1", generation: 23, done: make(chan struct{})}
		unit.witnesses = []controlmode.PaneWitness{witness}
		unit.progress.setStage(observerStageRead)
		unit.progress.recordExit(context.DeadlineExceeded)
		effects.units[unit.sessionID] = unit
		close(unit.done)
		effects.recordUnitExit(unit)
		effects.reapUnit(unit)
	}()
	if len(effects.units) != 0 {
		t.Fatal("owner map still retains unit")
	}
	records := effects.recordingExitRecords()
	if len(records) != 1 || records[0].Generation != 23 || records[0].Cause != observerCauseDeadline || records[0].Stage != observerStageRead {
		t.Fatalf("lost completed outcome: %+v", records)
	}
	if records[0].Key != journalKey(witness) {
		t.Fatal("lost exact source/generation")
	}
	for n := 0; n < 100; n++ {
		unit := &unifiedDevUnit{generation: uint64(n)}
		effects.recordUnitExit(unit)
	}
	records = effects.recordingExitRecords()
	if len(records) != 2*recordingObserverUnitLimit || records[len(records)-1].Generation != 99 {
		t.Fatal("exit ring not bounded/ordered")
	}
}
