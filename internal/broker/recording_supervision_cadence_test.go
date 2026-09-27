package broker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/unifiedjournal"
)

// Every kind of recording state keeps the supervisor at its working cadence;
// only a provider holding none of it is idle.
func TestRecordingSupervisionIdleCountsEveryRecordingOwner(t *testing.T) {
	if !(&UnifiedDevPaneEffects{}).supervisionIdle() {
		t.Fatal("an empty provider is not idle")
	}
	owners := map[string]func(*UnifiedDevPaneEffects){
		"supervised unit": func(e *UnifiedDevPaneEffects) {
			e.supervised = map[*unifiedDevUnit]struct{}{{}: {}}
		},
		"unit":     func(e *UnifiedDevPaneEffects) { e.units = map[string]*unifiedDevUnit{"$1": {}} },
		"birth":    func(e *UnifiedDevPaneEffects) { e.panes = map[string]*unifiedDevBirth{"$1": {}} },
		"active":   func(e *UnifiedDevPaneEffects) { e.active = map[string]unifiedjournal.PaneKey{"$1": {}} },
		"adoption": func(e *UnifiedDevPaneEffects) { e.adopting = map[string]struct{}{"$1": {}} },
		"terminal retirement": func(e *UnifiedDevPaneEffects) {
			e.terminalRetires = map[unifiedjournal.PaneKey]*terminalRetirement{{}: {}}
		},
	}
	for name, add := range owners {
		effects := &UnifiedDevPaneEffects{}
		add(effects)
		if effects.supervisionIdle() {
			t.Fatalf("a provider holding a %s is idle", name)
		}
	}

	realm := openRetentionRealm(t, "idle-generation")
	shadow := &retentionShadowEffects{realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock()}
	registry := newPaneRegistry(shadow)
	defer registry.Close()
	effects := &UnifiedDevPaneEffects{observer: registry}
	if !effects.supervisionIdle() {
		t.Fatal("a provider whose registry holds no generation is not idle")
	}
	if err := registry.AdmitPane(retentionWitness("%idle", "idle-inc")); err != nil {
		t.Fatal(err)
	}
	if effects.supervisionIdle() {
		t.Fatal("a provider holding a retention generation is idle")
	}
}

// The supervisor ticks at its idle interval while nothing is recorded, returns
// to its working interval when recording state appears, even unannounced, and
// slows down again once that state is gone.
func TestRecordingSupervisionSlowsWhenNothingIsRecorded(t *testing.T) {
	const fast, idle = 2 * time.Millisecond, 100 * time.Millisecond
	var ticks atomic.Int64
	effects := &UnifiedDevPaneEffects{supervisionInterval: fast, supervisionIdleInterval: idle, supervisionTick: func() { ticks.Add(1) }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- effects.runSupervisedObserver(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	countOver := func(window time.Duration) int64 {
		start := ticks.Load()
		deadline := time.NewTimer(window)
		defer deadline.Stop()
		<-deadline.C
		return ticks.Load() - start
	}
	// An idle window of 3.5 idle intervals holds at most a few ticks; the
	// working cadence would deliver about 175.
	if n := countOver(7 * idle / 2); n > 6 {
		t.Fatalf("%d ticks in an idle window; want the idle cadence", n)
	}

	unit := &unifiedDevUnit{done: make(chan struct{})}
	effects.mu.Lock()
	effects.supervised = map[*unifiedDevUnit]struct{}{unit: {}}
	effects.mu.Unlock()
	appeared := time.Now()
	base := ticks.Load()
	pollUntil(t, 5*time.Second, "the working cadence", func() bool { return ticks.Load()-base >= 50 })
	if elapsed := time.Since(appeared); elapsed > idle+50*fast+time.Second {
		t.Fatalf("the working cadence resumed only after %v", elapsed)
	}

	effects.mu.Lock()
	delete(effects.supervised, unit)
	effects.mu.Unlock()
	// One working tick after the removal re-evaluates the cadence.
	base = ticks.Load()
	pollUntil(t, 5*time.Second, "a tick after the removal", func() bool { return ticks.Load()-base >= 2 })
	if n := countOver(7 * idle / 2); n > 6 {
		t.Fatalf("%d ticks after recording state was gone; want the idle cadence again", n)
	}
}

// A real provider returns to the idle cadence once its only recorded session
// ends: every owner the supervisor watches is released.
func TestRecordingSupervisionReturnsToIdleAfterTheSessionEnds(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux adoption lifecycle")
	}
	fixture := newAdoptionFixture(t, 0)
	pollUntil(t, 5*time.Second, "an idle provider before any recording", fixture.effects.supervisionIdle)
	sessionID := fixture.startPaneCommand(t, "idle-cycle", "sleep 600")
	if _, err := fixture.effects.AdoptSession(context.Background(), sessionID); err != nil {
		t.Fatal(err)
	}
	if fixture.effects.supervisionIdle() {
		t.Fatal("a provider recording a session is idle")
	}
	fixture.disposable.run("kill-session", "-t", sessionID)
	pollUntil(t, 10*time.Second, "an idle provider after the session ended", fixture.effects.supervisionIdle)
}
