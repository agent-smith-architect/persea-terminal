package broker

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/unifiedjournal"
)

func TestRecordingProfileFullInitialAndContinuation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	effects, registry, witness := newRecordingInitialFixture(t, func(effects *UnifiedDevPaneEffects) {
		effects.retentionObserve = func(string, map[string]int64) { once.Do(func() { close(entered); <-release }) }
	})
	defer releaseOnce.Do(func() { close(release) })
	memory, err := effects.transients.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer memory.done()
	if !memory.Reserve(2 << 20) {
		t.Fatal("initial capture allowance refused")
	}
	initial := bytes.Repeat([]byte{'i'}, 2<<20)
	defer memory.Release(2 << 20)
	op := startRecordingInitialForTest(t, registry, witness, initial)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher hold did not start")
	}
	select {
	case <-op.done:
	case <-time.After(3 * time.Second):
		t.Fatal("full initial state did not commit")
	}
	if err := registry.publishInitial(op); err != nil {
		t.Fatal(err)
	}
	pending := &unifiedRotationPending{memory: memory}
	defer pending.release()
	continuation := bytes.Repeat([]byte{'c'}, 1<<20)
	if err := pending.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: continuation}); err != nil {
		t.Fatal(err)
	}
	rotation := &unifiedDevRotation{registry: registry, holder: &unifiedDevBirth{rotation: pending, memory: memory}}
	if err := rotation.commitPending(witness); err != nil {
		t.Fatal(err)
	}
	boundary, err := registry.retention.startBoundary(journalKey(witness), "rotation_pending", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-boundary; err != nil {
		t.Fatal(err)
	}
	registry.retention.mu.Lock()
	b, e := registry.retention.bUsed, registry.retention.eUsed
	registry.retention.mu.Unlock()
	if b != 3<<20 || e < 233+256 || e > retentionDefaultEnvelopeLimit {
		t.Fatal("full bootstrap/continuation owners did not fit or settled early", b, e)
	}
	releaseOnce.Do(func() { close(release) })
	fence, err := registry.retention.startDispatchFence()
	if err != nil {
		t.Fatal(err)
	}
	<-fence
	effects.journalMu.Lock()
	events, err := effects.realm.ReadCommittedEvents(journalKey(witness))
	effects.journalMu.Unlock()
	if err != nil || len(events) != 48 {
		t.Fatal("full profile record coverage", len(events), err)
	}
	for i, event := range events {
		want := byte('i')
		if i >= 32 {
			want = 'c'
		}
		if event.Kind != unifiedjournal.RecordOutput || len(event.Payload) != 64<<10 || !bytes.Equal(event.Payload, bytes.Repeat([]byte{want}, 64<<10)) {
			t.Fatal("bootstrap/continuation byte order", i)
		}
	}
}
