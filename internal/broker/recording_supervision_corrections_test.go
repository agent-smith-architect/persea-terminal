package broker

import (
	"context"
	"errors"
	"fmt"
	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
	"testing"
	"time"
)

// Models the publication gap after the initial receipt has completed, while
// its unit is still inside adoption and before process Kill reaches read EOF.
// No process is needed: cutoff is authoritative state, independent of Wait/EOF.
func TestRecordingSupervisionCorrectionCutoffBeforePublication(t *testing.T) {
	for _, kind := range []recordingInitialKind{recordingInitialBirth, recordingInitialAdoption} {
		for _, lateOwner := range []bool{false, true} {
			t.Run(fmt.Sprintf("kind-%d/late-owner-%v", kind, lateOwner), func(t *testing.T) { testCutoffBeforePublication(t, kind, lateOwner) })
		}
	}
}
func testCutoffBeforePublication(t *testing.T, kind recordingInitialKind, lateOwner bool) {
	_, registry, witness := newRecordingInitialFixture(t)
	op, beginErr := registry.beginInitial(kind, witness, recordingSourceForTest(witness), unifiedjournal.Geometry{Columns: 80, Rows: 24}, []byte("complete state"))
	if beginErr != nil {
		t.Fatal(beginErr)
	}
	<-op.done
	fence, err := registry.retention.startDispatchFence()
	if err != nil {
		t.Fatal(err)
	}
	<-fence
	unit := &unifiedDevUnit{done: make(chan struct{})}
	if !lateOwner {
		op.bindOwner(unit)
	}
	unit.progress.setStage(observerStageAdoption)
	unit.progress.stageMono.Store(recordingMonoNow() - int64(time.Second))
	effects := &UnifiedDevPaneEffects{
		observer:              registry,
		supervised:            map[*unifiedDevUnit]struct{}{unit: {}},
		supervisionStallLimit: time.Millisecond,
	}
	if _, err := op.result(); err != nil {
		t.Fatal(err)
	}
	effects.superviseRecording()
	if lateOwner {
		op.bindOwner(unit)
	}
	if !unit.supervisorFault.Load() {
		t.Fatal("supervisor did not cut off held initial owner")
	}
	if unit.readFailed.Load() {
		t.Fatal("probe must precede transport EOF")
	}
	if err := registry.publishInitial(op); err == nil {
		t.Fatal("authoritatively cut-off initial owner published success before read EOF")
	}
}

func TestRecordingSupervisionCorrectionLateRetirementAfterShutdown(t *testing.T) {
	for _, stage := range []string{"classification", "admission"} {
		t.Run(stage, func(t *testing.T) { testLateRetirementAfterShutdown(t, stage) })
	}
}
func testLateRetirementAfterShutdown(t *testing.T, stage string) {
	realm := openRetentionRealm(t, "late-retirement-shutdown")
	shadow := &retentionShadowEffects{realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock()}
	registry := newPaneRegistry(shadow)
	defer registry.Close()
	witness := retentionWitness("%late", "late-inc")
	witness.Session.Session = "$late"
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	registry.retention.mu.Lock()
	registry.retention.options.maxCommands = 0
	registry.retention.mu.Unlock()
	effects := &UnifiedDevPaneEffects{
		spawns: make(chan unifiedDevCommand), exits: make(chan *unifiedDevUnit),
		units: make(map[string]*unifiedDevUnit), active: make(map[string]unifiedjournal.PaneKey),
		panes: make(map[string]*unifiedDevBirth), rotationStates: make(map[string]*unifiedRotationState),
		rotationWake: make(chan struct{}, 1), rotationNow: time.Now,
	}
	entered, release := make(chan struct{}), make(chan struct{})
	effects.sessionPresence = func(config.TmuxServer, string) (bool, bool) {
		if stage == "classification" {
			close(entered)
			<-release
		}
		return false, true
	}
	if stage == "admission" {
		registry.retention.setHook(func(edge string, _ unifiedjournal.PaneKey) {
			if edge == "before_disconnect_reserve" {
				close(entered)
				<-release
			}
		})
	}
	memory, err := effects.transients.acquire()
	if err != nil {
		t.Fatal(err)
	}
	unit := &unifiedDevUnit{owner: effects, sessionID: "$late", memory: memory, witnesses: []controlmode.PaneWitness{witness}, done: make(chan struct{})}
	close(unit.done)
	effects.units[unit.sessionID] = unit
	effects.active[unit.sessionID] = journalKey(witness)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- effects.RunObserver(ctx, registry) }()
	effects.exits <- unit
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reap did not reach held classification")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunObserver did not stop independently")
	}
	if effects.observerRunContext().Err() == nil {
		t.Fatal("shutdown lost canceled run context")
	}
	effects.transients.mutex().Lock()
	heldOwners := effects.transients.units
	effects.transients.mutex().Unlock()
	if heldOwners != 1 || effects.lifecycle.snapshot().InFlight != 1 {
		t.Fatalf("held work refunded before settlement: units=%d lifecycle=%d", heldOwners, effects.lifecycle.snapshot().InFlight)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for effects.lifecycle.snapshot().InFlight != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	effects.mu.Lock()
	pending := len(effects.terminalRetires)
	ready := false
	for _, state := range effects.terminalRetires {
		ready = ready || state.retryReady
	}
	effects.mu.Unlock()
	effects.transients.mutex().Lock()
	owners := effects.transients.units
	effects.transients.mutex().Unlock()
	// Explicit cleanup of the falsifier's retained obligation, after observation.
	effects.stopTerminalRetirements()
	if pending != 0 || owners != 0 {
		t.Fatalf("late reap created owner after shutdown: pending=%d retryReady=%v chargedUnits=%d lifecycleInFlight=%d", pending, ready, owners, effects.lifecycle.snapshot().InFlight)
	}
}

// Initial publication is authoritative before the provider installs active[];
// a later cutoff must find this exact generation and seal attached input.
func TestRecordingSupervisionCorrectionPublicationBeforeRegistration(t *testing.T) {
	_, registry, witness := newRecordingInitialFixture(t)
	op := startRecordingInitialForTest(t, registry, witness, []byte("complete state"))
	<-op.done
	unit := &unifiedDevUnit{done: make(chan struct{})}
	op.bindOwner(unit)
	if err := registry.publishInitial(op); err != nil {
		t.Fatal(err)
	}
	if !registry.retention.initialReady(journalKey(witness)) {
		t.Fatal("published receipt not ready")
	}
	tx := &supervisionInputTransaction{witness: recordingSourceForTest(witness)}
	source, err := terminal.NewPinnedSource(tx.witness, tx, tx)
	if err != nil {
		t.Fatal(err)
	}
	writer := &supervisionInputWriter{entered: make(chan context.Context, 1), release: make(chan struct{})}
	close(writer.release)
	attachment := registry.attachmentEffects(witness.Session.Server, tx.witness)
	epoch, err := terminal.NewEpochWithAttachmentEffects(context.Background(), source, 1, supervisionFrameWriter{}, writer, terminal.Config{Clock: terminal.RealClock{}, QuietInterval: time.Second, MaximumInterval: 2 * time.Second, CutTimeout: 5 * time.Second}, attachment)
	if err != nil {
		t.Fatal(err)
	}
	tx.epoch = epoch
	defer epoch.Finalize(context.Background())
	if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	unit.progress.setStage(observerStageAdoption)
	unit.progress.stageMono.Store(recordingMonoNow() - int64(time.Second))
	effects := &UnifiedDevPaneEffects{observer: registry, supervised: map[*unifiedDevUnit]struct{}{unit: {}}, supervisionStallLimit: time.Millisecond}
	effects.superviseRecording()
	if registry.retention.initialReady(journalKey(witness)) {
		t.Fatal("cutoff left published generation ready")
	}
	select {
	case <-epoch.Done():
	default:
		t.Fatal("publication gap hid attachment from cutoff")
	}
	if err := attachment.BindAttachment(epoch); !errors.Is(err, errRecordingStalled) {
		t.Fatalf("late bind escaped: %v", err)
	}
	if unit.readFailed.Load() {
		t.Fatal("cutoff required EOF")
	}
}

func TestRecordingSupervisionCorrectionRotationPublicationOrder(t *testing.T) {
	for _, publishFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "cutoff_first", true: "publication_first"}[publishFirst], func(t *testing.T) {
			fixture := newP2AFixture(t)
			fixture.coordinator.mu.Lock()
			fixture.coordinator.attachments = nil // fixture's placeholder is not a constructed Epoch
			fixture.coordinator.mu.Unlock()
			next := fixture.next(2)
			reservation := fixture.materialize(next)
			defer fixture.abortReservation(reservation)
			txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
			if err != nil {
				t.Fatal(err)
			}
			if err := txn.WriteBootstrap([]byte("rotation complete")); err != nil {
				t.Fatal(err)
			}
			if err := fixture.boundary(next); err != nil {
				t.Fatal(err)
			}
			if err := txn.Validate(); err != nil {
				t.Fatal(err)
			}
			txn.initial.bindOwner(fixture.unit)
			if publishFirst && txn.Commit() != paneRotationCommitted {
				t.Fatal("publication did not win")
			}
			base := fixture.effects.UnifiedDevPaneEffects
			base.mu.Lock()
			base.supervised = map[*unifiedDevUnit]struct{}{fixture.unit: {}}
			base.supervisionStallLimit = time.Millisecond
			base.mu.Unlock()
			fixture.unit.progress.setStage(observerStageAdoption)
			fixture.unit.progress.stageMono.Store(recordingMonoNow() - int64(time.Second))
			base.superviseRecording()
			if !publishFirst && txn.Commit() == paneRotationCommitted {
				t.Fatal("rotation published after cutoff")
			}
			if fixture.registry.retention.initialReady(journalKey(next)) {
				t.Fatal("rotation remained ready after cutoff")
			}
		})
	}
}
