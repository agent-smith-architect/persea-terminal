package broker

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/unifiedjournal"
)

func TestRecordingProgressReadableDuringBlockedStages(t *testing.T) {
	for _, block := range []string{"append", "callback", "journal_lock"} {
		t.Run(block, func(t *testing.T) {
			realm := openRetentionRealm(t, "progress-"+block)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			pause := func() { once.Do(func() { close(entered) }); <-release }
			effects := &retentionShadowEffects{realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock()}
			if block == "append" {
				effects.stage = func(stage string, _ unifiedjournal.PaneKey) error {
					if stage == "append" {
						pause()
					}
					return nil
				}
			}
			if block == "callback" {
				effects.beforeWrite = pause
			}
			registry := newPaneRegistry(effects)
			witness := retentionWitness("%progress", "progress-inc")
			if err := registry.AdmitPane(witness); err != nil {
				t.Fatal(err)
			}
			if block == "journal_lock" {
				journalMu := new(sync.Mutex)
				registry.retention.options.journalMu = journalMu
				journalMu.Lock()
				registry.retention.setHook(func(stage string, _ unifiedjournal.PaneKey) {
					if stage == "before_append" {
						once.Do(func() { close(entered) })
					}
				})
				go func() { <-release; journalMu.Unlock() }()
			}
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(func() { unblock(); _ = registry.Close() })
			if err := feedRetentionOutput(registry, controlmode.NewDecoder(), witness, bytes.Repeat([]byte{'x'}, 64<<10)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("blocked stage never started")
			}
			// Hold the other mutable state lock too: the snapshot must use neither.
			registry.retention.mu.Lock()
			sampled := make(chan retentionProgressSnapshot, 1)
			go func() { sampled <- registry.retention.progressSnapshot() }()
			var sample retentionProgressSnapshot
			select {
			case sample = <-sampled:
			case <-time.After(time.Second):
				registry.retention.mu.Unlock()
				t.Fatal("snapshot depended on runtime lock")
			}
			registry.retention.mu.Unlock()
			if sample.AcceptedBytes != 64<<10 || sample.IngressBytes != 64<<10 || sample.Accepted == 0 {
				t.Fatalf("acceptance missing: %+v", sample)
			}
			if block == "callback" {
				if sample.Dispatch.InFlight != 1 || sample.Commit.Completed != 1 || sample.CommittedBytes != 64<<10 || sample.DeliveredBytes != 0 {
					t.Fatalf("blocked publication not visible: %+v", sample)
				}
			} else if sample.Append.InFlight != 1 || sample.Manager.InFlight != 1 {
				t.Fatalf("blocked write not visible: %+v", sample)
			}
			unblock()
			if err := registry.retention.Boundary(journalKey(witness), "explicit_flush"); err != nil {
				t.Fatal(err)
			}
			fence, err := registry.retention.startDispatchFence()
			if err != nil {
				t.Fatal(err)
			}
			<-fence
			settled := registry.retention.progressSnapshot()
			if settled.IngressBytes != 0 || settled.OutboxBytes != 0 || settled.Append.Completed != 1 || settled.Commit.Completed != 1 || settled.DeliveredBytes != 64<<10 {
				t.Fatalf("settlement missing: %+v", settled)
			}
		})
	}
}

func TestRecordingProgressClassifiesAdmissionFailure(t *testing.T) {
	realm := openRetentionRealm(t, "progress-rejection")
	effects := &retentionShadowEffects{realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(), maxIngressBytes: 64 << 10}
	registry := newPaneRegistry(effects)
	if registry.retention == nil {
		t.Fatal("retention fixture did not initialize")
	}
	defer registry.Close()
	witness := retentionWitness("%rejection", "rejection-inc")
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.retention.reserve(journalKey(witness), (64<<10)+1); !errors.Is(err, unifiedjournal.ErrInvalidated) {
		t.Fatalf("reservation = %v", err)
	}
	sample := registry.retention.progressSnapshot()
	if sample.Rejected[recordingRejectIngress] != 1 || sample.IngressBytes != 0 {
		t.Fatalf("rejection missing or spent bytes: %+v", sample)
	}
}

func TestObserverExitProgressRetainsStageAndClosedCause(t *testing.T) {
	for _, test := range []struct {
		err   error
		cause observerFailureCause
	}{
		{nil, observerCauseEOF}, {context.Canceled, observerCauseCancelled}, {context.DeadlineExceeded, observerCauseDeadline},
		{ErrUnifiedObserverFlowControl, observerCauseFlowControl}, {unifiedjournal.ErrInvalidated, observerCauseInvalidated},
		{unifiedjournal.ErrStorage, observerCauseStorage}, {errors.New("private error text"), observerCauseStageError},
	} {
		var progress observerProgress
		progress.stage.Store(uint32(observerStageAdoption))
		progress.recordExit(test.err)
		s := progress.snapshot()
		if s.ExitStage != observerStageAdoption || s.ExitCause != test.cause || s.ExitedUnixNano == 0 {
			t.Fatalf("incorrect exit: %+v", s)
		}
	}
}

func TestObserverExitProgressRecordedBeforeUnitDone(t *testing.T) {
	fixture := newAdoptionFixture(t, 0)
	session := fixture.startPaneCommand(t, "progress-exit", "printf 'synthetic exit fixture\\n'; exec sleep 600")
	if _, err := fixture.effects.AdoptSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	fixture.effects.mu.Lock()
	unit := fixture.effects.units[session]
	fixture.effects.mu.Unlock()
	if unit == nil {
		t.Fatal("adopted unit missing")
	}
	fixture.cancel()
	select {
	case <-unit.done:
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not exit")
	}
	sample := unit.progress.snapshot()
	if sample.ExitedUnixNano == 0 || sample.ExitCause != observerCauseCancelled {
		t.Fatalf("unit done without recorded cancellation: %+v", sample)
	}
}
