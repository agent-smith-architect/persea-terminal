package broker

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/unifiedjournal"
)

func TestRecordingPartialFailureKeepsEarlierWriterCharged(t *testing.T) {
	recordingPartialFailure(t, false)
}

func TestRecordingRetiredFaultsKeepBoundedCloseDiagnostic(t *testing.T) {
	effects := &retentionShadowEffects{realm: openRetentionRealm(t, "fault-history"), maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock()}
	trial := newRetentionTrialRuntime(effects)
	defer trial.Close()
	first := errors.New("first durable recording failure")
	for round := 0; round < 16; round++ {
		for i := 0; i < retentionDefaultPaneLimit; i++ {
			key := journalKey(retentionWitness(fmt.Sprintf("%%%d", i), fmt.Sprintf("fault-%d-%d", round, i)))
			if _, _, err := trial.ensureGeneration(key, true); err != nil {
				t.Fatal(err)
			}
			cause := error(first)
			if round != 0 || i != 0 {
				cause = fmt.Errorf("later generation failure %d/%d", round, i)
			}
			_, cleanup := trial.classifyFault(key, "storage_fault", cause)
			trial.appendCleanup(cleanup)
			trial.requestRetire(key)
		}
		fence, err := trial.startDispatchFence()
		if err != nil {
			t.Fatal(err)
		}
		<-fence
		trial.mu.Lock()
		generations, p, q, e := len(trial.generations), trial.pUsed, trial.qUsed, trial.eUsed
		trial.mu.Unlock()
		if generations != 0 || p != 0 || q != 0 || e != 0 {
			t.Fatal("retired fault generation remained owned", generations, p, q, e)
		}
	}
	if err := trial.Close(); !errors.Is(err, first) || err.Error() != first.Error() {
		t.Fatal("Close retained lifetime error history instead of its first diagnostic", err)
	}
}

func TestRecordingHeldDrainFailureDiscardsTailBeforeRefund(t *testing.T) {
	recordingPartialFailure(t, true)
}

func recordingPartialFailure(t *testing.T, paused bool) {
	entered, release, failed, processed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	cleanupRelease := make(chan struct{})
	var once sync.Once
	writes := 0
	effects := &retentionShadowEffects{
		realm: openRetentionRealm(t, "partial-failure"), maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(),
		beforeWrite: func() { once.Do(func() { close(entered); <-release }) },
		stage: func(stage string, _ unifiedjournal.PaneKey) error {
			if stage == "append" {
				writes++
				if writes == 2 {
					close(failed)
					return errors.New("second append failed")
				}
			}
			return nil
		},
		hook: func(stage string, _ unifiedjournal.PaneKey) {
			if stage == "before_cleanup" {
				close(processed)
				<-cleanupRelease
			}
		},
	}
	trial := newRetentionTrialRuntime(effects)
	defer func() { close(release); close(cleanupRelease); _ = trial.Close() }()
	key := journalKey(retentionWitness("%partial", "partial"))
	if paused {
		if _, _, err := trial.ensureGeneration(key, true); err != nil {
			t.Fatal(err)
		}
		if err := trial.Boundary(key, "pause_start"); err != nil {
			t.Fatal(err)
		}
	}
	if err := trial.WritePane(key, make([]byte, 3*64<<10)); err != nil {
		t.Fatal(err)
	}
	if paused {
		if _, err := trial.startBoundary(key, "pause_end", false); err != nil {
			t.Fatal(err)
		}
	}
	for _, ready := range []<-chan struct{}{entered, failed, processed} {
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			t.Fatal("owner edge timeout")
		}
	}
	trial.mu.Lock()
	bytes, envelopes := trial.bUsed, trial.eUsed
	trial.mu.Unlock()
	if bytes != 3*64<<10 || envelopes == 0 {
		t.Fatalf("earlier blocked writer refunded by later failure: bytes=%d envelopes=%d", bytes, envelopes)
	}
	// The manager is held at cleanup. A failed drain must already have
	// discarded the unprocessed tail, so cleanup cannot assemble an oversized
	// final batch after the first error or retain its uncredited payload.
	if paused {
		pane := trial.panes[key]
		if pane.bytes != 0 || pane.heldBytes != 0 || len(pane.pending) != 0 || len(pane.held) != 0 {
			t.Fatalf("failed drain retained tail: pending=%d/%d held=%d/%d", pane.bytes, len(pane.pending), pane.heldBytes, len(pane.held))
		}
	}
}
