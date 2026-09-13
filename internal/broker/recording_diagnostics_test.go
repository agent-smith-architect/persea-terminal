package broker

import (
	"sync"
	"testing"
	"time"
)

func TestRecordingDiagnosticValuesAndHeldCallbackSettlement(t *testing.T) {
	barrier, releaseBarrier := make(chan struct{}), make(chan struct{})
	callback, releaseCallback := make(chan struct{}), make(chan struct{})
	var barrierOnce, callbackOnce sync.Once
	defer barrierOnce.Do(func() { close(releaseBarrier) })
	defer callbackOnce.Do(func() { close(releaseCallback) })
	clock := newRetentionManualClock()
	var values []int64
	effects := &recordingMemoryEffects{UnifiedDevPaneEffects: &UnifiedDevPaneEffects{}, options: retentionTrialOptions{
		realm: openRetentionRealm(t, "diagnostic-values"), maxBatchBytes: 64 << 10, maxFeedDelay: 16 * time.Millisecond,
		now: clock.Now, after: clock.After,
		observe: func(event string, fields map[string]int64) {
			switch event {
			case "barrier":
				close(barrier)
				<-releaseBarrier
			case "owned":
				values = append(values, fields["authoritative"])
				fields["authoritative"] = -1 // callback owns its ordinary mutable map
				close(callback)
				<-releaseCallback
			case "after":
				values = append(values, fields["authoritative"])
			}
		},
	}}
	trial := newRetentionTrialRuntime(effects)
	defer func() {
		barrierOnce.Do(func() { close(releaseBarrier) })
		callbackOnce.Do(func() { close(releaseCallback) })
		_ = trial.Close()
	}()
	trial.publishObservation("barrier", nil, nil)
	<-barrier
	reservation, err := trial.reserve(journalKey(retentionWitness("%0", "diagnostic-values")), 1)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]int64{"authoritative": 1}
	trial.publishObservation("owned", fields, []*retentionReservation{reservation})
	trial.releaseCommand(reservation)
	fields["authoritative"] = 2
	trial.publishObservation("after", fields, nil)
	delete(fields, "authoritative")
	barrierOnce.Do(func() { close(releaseBarrier) })
	<-callback
	trial.mu.Lock()
	held := trial.bUsed
	trial.mu.Unlock()
	if held != 1 {
		t.Fatal("callback refunded its owner before returning", held)
	}
	callbackOnce.Do(func() { close(releaseCallback) })
	fence, err := trial.startDispatchFence()
	if err != nil {
		t.Fatal(err)
	}
	<-fence
	if len(values) != 2 || values[0] != 1 || values[1] != 2 {
		t.Fatal("capture-time values or callback isolation changed", values)
	}
	trial.mu.Lock()
	remaining := trial.bUsed
	trial.mu.Unlock()
	if remaining != 0 {
		t.Fatal("completed callback kept its owner", remaining)
	}
}
