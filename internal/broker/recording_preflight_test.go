package broker

import (
	"context"
	"errors"
	"testing"
)

func TestRecordingFoundingPreflightUsesExistingOwnerAdmission(t *testing.T) {
	effects := newProjectionEffects(t, 9)
	var owners []*recordingUnitMemory
	for i := 0; i < recordingObserverUnitLimit; i++ {
		owner, err := effects.transients.acquire()
		if err != nil {
			t.Fatal(err)
		}
		owners = append(owners, owner)
	}
	defer func() {
		for _, owner := range owners {
			owner.done()
		}
	}()
	// A missing session would produce a different refusal if its subprocess
	// ran before founding admission. The full owner pool must refuse first.
	if _, err := effects.AdoptSession(context.Background(), "$999999"); !errors.Is(err, errRecordingTransients) {
		t.Fatal("preflight ran outside founding admission", err)
	}
	owners[len(owners)-1].done()
	owners = owners[:len(owners)-1]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := effects.AdoptSession(ctx, "$999999"); !errors.Is(err, context.Canceled) {
		t.Fatal("preflight lost caller cancellation", err)
	}
	effects.transients.mutex().Lock()
	units := effects.transients.units
	effects.transients.mutex().Unlock()
	if units != recordingObserverUnitLimit-1 {
		t.Fatal("failed preflight retained or double-refunded its owner", units)
	}
}
