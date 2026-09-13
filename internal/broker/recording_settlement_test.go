package broker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/unifiedjournal"
)

func TestRecordingPublishedRotationIgnoresCallerCancellation(t *testing.T) {
	for _, when := range []string{"active_swapped", "pending_held"} {
		t.Run(when, func(t *testing.T) {
			f := newAdoptionFixture(t, 8)
			session := f.startPaneCommand(t, "published-cancel", "printf 'stable source\\n'; exec sleep 600")
			adoption, err := f.effects.AdoptSession(context.Background(), session)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var appends int
			f.registry.retention.setHook(func(point string, key unifiedjournal.PaneKey) {
				if point == "before_append" && key != adoption.Key {
					appends++
					if appends == 2 {
						close(entered)
						<-release
					}
				}
			})
			published := make(chan *unifiedDevRotation, 1)
			f.effects.rotationEdge = func(_ string, edge string) {
				if edge != "active_swapped" {
					return
				}
				f.effects.mu.Lock()
				rotation := f.effects.rotation
				f.effects.mu.Unlock()
				if _, _, _, stop, err := openSnapshotTailForTest(t, f.effects, session); err != nil {
					t.Errorf("published snapshot: %v", err)
				} else {
					stop()
				}
				rotation.holder.mu.Lock()
				err := rotation.holder.rotation.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: []byte("owned pending continuation")})
				rotation.holder.mu.Unlock()
				if err != nil {
					t.Error(err)
				}
				if when == "active_swapped" {
					cancel()
				}
				published <- rotation
			}
			done := make(chan error, 1)
			go func() { done <- f.effects.rotateSession(ctx, session) }()
			var rotation *unifiedDevRotation
			select {
			case rotation = <-published:
			case <-time.After(5 * time.Second):
				t.Fatal("publication not reached")
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("pending append did not hold")
			}
			if when == "pending_held" {
				cancel()
			}
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			f.registry.retention.mu.Lock()
			refs := f.registry.retention.generations[rotation.newKey].refs
			f.registry.retention.mu.Unlock()
			if refs == 0 {
				t.Fatal("held pending work lost its owner")
			}
			if _, _, _, stop, err := openSnapshotTailForTest(t, f.effects, session); err != nil {
				t.Fatal(err)
			} else {
				stop()
			}
			unblock()
			pollUntil(t, 5*time.Second, "published pending work settles", func() bool { f.effects.mu.Lock(); defer f.effects.mu.Unlock(); return f.effects.rotation == nil })
			if key, active := f.effects.paneKey(session); !active || key != rotation.newKey {
				t.Fatalf("caller cancellation revoked ready successor: active=%v key=%v", active, key)
			}
			if _, _, _, stop, err := openSnapshotTailForTest(t, f.effects, session); err != nil {
				t.Fatal(err)
			} else {
				stop()
			}
			fence, err := f.registry.retention.startDispatchFence()
			if err != nil {
				t.Fatal(err)
			}
			<-fence
			f.registry.retention.mu.Lock()
			refs = f.registry.retention.generations[rotation.newKey].refs
			f.registry.retention.mu.Unlock()
			if refs != 0 {
				t.Fatalf("released pending work retained %d references", refs)
			}
		})
	}
}

func TestRecordingRollbackSettlementKeepsDecoding(t *testing.T) {
	for _, lost := range []bool{false, true} {
		name := "preserved"
		if lost {
			name = "overflow_is_fatal"
		}
		t.Run(name, func(t *testing.T) { recordingRollbackSettlementKeepsDecoding(t, lost) })
	}
}

func recordingRollbackSettlementKeepsDecoding(t *testing.T, lost bool) {
	marker := "SETTLEMENT-CONTINUATION"
	var decoded atomic.Bool
	f := newRecordingSettlementFixture(t, func(effects *UnifiedDevPaneEffects) {
		effects.observerDecoded = func(event controlmode.Event) {
			if event.Kind == controlmode.EventOutput && bytes.Contains(event.Data, []byte(marker)) {
				decoded.Store(true)
			}
		}
	})
	session := f.startPaneCommand(t, "rollback-decode", "exec /bin/sh")
	adoption, err := f.effects.AdoptSession(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	f.registry.retention.setHook(func(point string, key unifiedjournal.PaneKey) {
		if point == "before_downstream_write" && key != adoption.Key {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}
	})
	rotationReady := make(chan *unifiedDevRotation, 1)
	f.effects.rotationEdge = func(_ string, edge string) {
		if edge == "before_seal_start" {
			f.effects.mu.Lock()
			rotation := f.effects.rotation
			f.effects.mu.Unlock()
			rotationReady <- rotation
			cancel()
		}
	}
	if err := f.effects.rotateSession(ctx, session); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	rotation := <-rotationReady
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("successor feed did not hold")
	}
	op := rotation.registryT.initial
	pollUntil(t, time.Second, "cancelled initial settlement", func() bool {
		f.registry.retention.mu.Lock()
		defer f.registry.retention.mu.Unlock()
		return op.cancelled
	})
	if lost {
		rotation.holder.mu.Lock()
		rotation.holder.rotation.data = bytes.Repeat([]byte("x"), int(unifiedjournal.RotationPendingCapBytes))
		rotation.holder.mu.Unlock()
	}
	f.disposable.run("send-keys", "-t", session, "printf 'SETTLEMENT%s-CONTINUATION\\n' ''", "Enter")
	pollUntil(t, time.Second, "source emitted marker", func() bool { return strings.Contains(f.capture(t, session), marker) })
	pollUntil(t, time.Second, "rollback owner decoded marker while dispatcher held", decoded.Load)
	if !lost {
		pollUntil(t, time.Second, "rollback retained decoded marker", func() bool {
			rotation.holder.mu.Lock()
			defer rotation.holder.mu.Unlock()
			return rotation.holder.rotation != nil && bytes.Contains(rotation.holder.rotation.data, []byte(marker))
		})
	}
	f.registry.retention.mu.Lock()
	refs := op.generation.refs
	f.registry.retention.mu.Unlock()
	if refs == 0 {
		t.Fatal("held dispatcher lost its reservation")
	}
	unblock()
	pollUntil(t, 5*time.Second, "rollback settled", func() bool { f.effects.mu.Lock(); defer f.effects.mu.Unlock(); return f.effects.rotation == nil })
	assertRecordingInitialSettled(t, f.registry, op)
	if lost {
		if _, active := f.effects.paneKey(session); active {
			t.Fatal("lost continuation restored a ready predecessor")
		}
		select {
		case <-rotation.unit.done:
		case <-time.After(5 * time.Second):
			t.Fatal("fatal unit did not finish")
		}
		for _, cause := range []error{context.Canceled, ErrUnifiedRotatePendingOverflow, ErrUnifiedRotateFatal} {
			if !errors.Is(rotation.unit.exitErr, cause) {
				t.Fatalf("completed unit lost cause %v: %v", cause, rotation.unit.exitErr)
			}
		}
		later := errors.New("later failure")
		if err := rotation.settleFatal(later); !errors.Is(err, ErrUnifiedRotatePendingOverflow) || errors.Is(err, later) {
			t.Fatalf("idempotent fatal disposition lost original cause: %v", err)
		}
		return
	}
	if key, active := f.effects.paneKey(session); !active || key != adoption.Key {
		t.Fatal("healthy predecessor was not restored")
	}
	if !bytes.Contains(f.journalBytes(t, adoption.Key), []byte(marker)) {
		t.Fatal("rollback lost decoded continuation")
	}
}

func TestRecordingAdoptionLateFailureSettlementKeepsDecoding(t *testing.T) {
	for _, failure := range []string{"source", "provider_authority", "registry_authority"} {
		t.Run(failure, func(t *testing.T) { recordingAdoptionLateFailureSettlementKeepsDecoding(t, failure) })
	}
}

func recordingAdoptionLateFailureSettlementKeepsDecoding(t *testing.T, failure string) {
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var decoded atomic.Bool
	marker := "late-source-continuation"
	f := newRecordingSettlementFixture(t, func(effects *UnifiedDevPaneEffects) {
		effects.retentionObserve = func(event string, _ map[string]int64) {
			if event == "feed_complete" {
				enteredOnce.Do(func() { close(entered); <-release })
			}
		}
		effects.observerDecoded = func(event controlmode.Event) {
			if event.Kind == controlmode.EventOutput && bytes.Contains(event.Data, []byte(marker)) {
				decoded.Store(true)
			}
		}
	})
	gate := filepath.Join(t.TempDir(), "emit")
	emit := "while [ ! -e '" + gate + "' ]; do sleep 0.01; done; printf '" + marker + "\\n'; exec sleep 600"
	session := f.startPaneCommand(t, "late-adoption", "printf 'initial state\\n'; "+emit)
	ready, resume := make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	resumeRecheck := func() { resumeOnce.Do(func() { close(resume) }) }
	defer resumeRecheck()
	f.registry.retention.setHook(func(point string, _ unifiedjournal.PaneKey) {
		if point == "adoption_initial_complete" {
			close(ready)
			<-resume
		}
	})
	done := make(chan error, 1)
	go func() { _, err := f.effects.AdoptSession(context.Background(), session); done <- err }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("initial receipt not reached")
	}
	<-entered
	f.effects.mu.Lock()
	var holder *unifiedDevBirth
	for _, candidate := range f.effects.panes {
		holder = candidate
	}
	f.effects.mu.Unlock()
	if holder == nil {
		t.Fatal("missing initial holder")
	}
	op := holder.initial
	switch failure {
	case "source":
		f.disposable.run("respawn-pane", "-k", "-t", session+":", emit)
	case "provider_authority":
		f.effects.mu.Lock()
		f.effects.panes[holder.witness.Pane] = &unifiedDevBirth{owner: f.effects, witness: holder.witness, aborted: true}
		f.effects.mu.Unlock()
	case "registry_authority":
		f.registry.mu.Lock()
		state := f.registry.admitted[routeCoordinateKey(holder.witness)]
		state.witness.Incarnation = "replacement"
		f.registry.admitted[routeCoordinateKey(holder.witness)] = state
		f.registry.mu.Unlock()
	}
	resumeRecheck()
	pollUntil(t, time.Second, "late source failure requests settlement", func() bool {
		f.registry.retention.mu.Lock()
		defer f.registry.retention.mu.Unlock()
		return op.cancelled
	})
	if err := os.WriteFile(gate, []byte("emit"), 0600); err != nil {
		t.Fatal(err)
	}
	pollUntil(t, time.Second, "source emitted marker", func() bool { return strings.Contains(f.capture(t, session), marker) })
	pollUntil(t, time.Second, "late adoption failure decoded marker while dispatcher held", decoded.Load)
	f.registry.retention.mu.Lock()
	refs := op.generation.refs
	f.registry.retention.mu.Unlock()
	if refs == 0 {
		t.Fatal("held dispatcher lost ownership")
	}
	unblock()
	if err := <-done; err == nil {
		t.Fatal("failed adoption succeeded")
	}
	select {
	case <-op.owner.done:
	case <-time.After(5 * time.Second):
		t.Fatal("failed unit did not settle")
	}
	assertRecordingInitialSettled(t, f.registry, op)
}

func assertRecordingInitialSettled(t *testing.T, registry *paneRegistry, op *recordingInitialOperation) {
	t.Helper()
	fence, err := registry.retention.startDispatchFence()
	if err != nil {
		t.Fatal(err)
	}
	<-fence
	registry.retention.mu.Lock()
	refs := op.generation.refs
	registry.retention.mu.Unlock()
	if refs != 0 {
		t.Fatalf("settled initial operation retained %d references", refs)
	}
}

func newRecordingSettlementFixture(t *testing.T, configure func(*UnifiedDevPaneEffects)) *adoptionFixture {
	t.Helper()
	disposable := newDisposable(t)
	server := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	runtimeDir := t.TempDir()
	cfg := unifiedAdoptionDevConfig(t, server, runtimeDir, 8)
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configure(effects)
	registry := newPaneRegistry(effects)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = effects.RunObserver(ctx, registry) }()
	t.Cleanup(func() { cancel(); _ = registry.Close() })
	return &adoptionFixture{disposable: disposable, server: server, cfg: cfg, runtimeDir: runtimeDir, effects: effects, registry: registry, cancel: cancel}
}
