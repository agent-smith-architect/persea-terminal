package broker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/unifiedjournal"
)

// This is a preventive concurrency contract, not evidence that the original
// observer exit was a deadlock. It constructs the final-reference interleaving
// explicitly and requires snapshot readiness authority to remain available
// while durable retirement is waiting for journalMu.
func TestRecordingStaleCancellationReleasesAuthorityBeforeDurableIO(t *testing.T) {
	effects, registry, witness := newRecordingInitialFixture(t)
	commitRecordingInitialForTest(t, registry, witness, nil)
	runtime, key := registry.retention, journalKey(witness)
	reserved, route := make(chan struct{}), make(chan struct{})
	retiring := make(chan struct{})
	var calls atomic.Int64
	runtime.options.retire = func(key unifiedjournal.PaneKey) bool {
		if calls.Add(1) == 1 {
			close(retiring)
		}
		return effects.retirePane(key)
	}
	runtime.setHook(func(point string, got unifiedjournal.PaneKey) {
		if point == "after_ingress_reserve" && got == key {
			close(reserved)
			<-route
		}
	})
	done := make(chan error, 1)
	go func() {
		done <- registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationDisconnect, Witness: witness})
	}()
	select {
	case <-reserved:
	case <-time.After(2 * time.Second):
		t.Fatal("owner boundary did not reserve")
	}
	effects.journalMu.Lock()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(effects.journalMu.Unlock) }
	defer unblock()
	registry.mu.Lock()
	delete(registry.admitted, routeCoordinateKey(witness))
	runtime.mu.Lock()
	generation := runtime.generations[key]
	generation.admitted, generation.retireRequested, generation.durableRequested = false, true, true
	runtime.mu.Unlock()
	registry.mu.Unlock()
	close(route)
	select {
	case <-retiring:
	case <-time.After(2 * time.Second):
		t.Fatal("final-reference retirement did not start")
	}
	readiness := make(chan struct{})
	go func() { _ = effects.recordingReady(key); close(readiness) }()
	select {
	case <-readiness:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("stale cancellation held registry authority while waiting for journalMu")
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retirement did not settle")
	}
	if err := registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationDisconnect, Witness: witness}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("durable retirement repeated %d times", calls.Load())
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.generations[key] != nil || len(runtime.sources) != 0 || runtime.qUsed != 0 || runtime.eUsed != 0 {
		t.Fatal("once-only retirement retained reservation ownership")
	}
}
