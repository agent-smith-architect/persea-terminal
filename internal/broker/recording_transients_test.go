package broker

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
)

func TestRecordingTransientOwnerSurvivesEveryActualHolder(t *testing.T) {
	budget := &recordingTransientBudget{limit: recordingUnitBytes + 1024}
	owner, err := budget.acquire()
	if err != nil {
		t.Fatal(err)
	}
	owner.hold() // read loop
	owner.hold() // reaper
	if !owner.Reserve(1024) {
		t.Fatal("dynamic admission")
	}
	owner.done() // map removal/reaper is not process or read settlement
	owner.done() // process wait has returned, read still owns its channel
	if _, err := budget.acquire(); !errors.Is(err, errRecordingTransients) {
		t.Fatal("early permit reuse")
	}
	owner.done()
	if budget.units != 1 || budget.bytes != recordingUnitBytes+1024 {
		t.Fatal("copied data outlives last active owner")
	}
	owner.Release(1024)
	if budget.units != 0 || budget.bytes != 0 {
		t.Fatal("settled permit leaked")
	}
	owner, err = budget.acquire()
	if err != nil {
		t.Fatal(err)
	}
	owner.done()
}

func TestRecordingFoundingPermitSurvivesCallerTimeout(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	fixture := newRecordingSettlementFixture(t, func(effects *UnifiedDevPaneEffects) {
		effects.transients.limit = recordingUnitBytes + (1 << 20)
		effects.observerReadinessEdge = func(string) error { close(entered); <-release; return nil }
	})
	first := fixture.startPaneCommand(t, "permit-first", "sleep 60")
	second := fixture.startPaneCommand(t, "permit-second", "sleep 60")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := fixture.effects.AdoptSession(ctx, first); done <- err }()
	<-entered
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatal(err)
	}
	fixture.effects.mu.Lock()
	founding := fixture.effects.units[first] == nil
	fixture.effects.mu.Unlock()
	if !founding {
		close(release)
		t.Fatal("fixture crossed founding boundary")
	}
	if _, err := fixture.effects.AdoptSession(context.Background(), second); !errors.Is(err, errRecordingTransients) {
		close(release)
		t.Fatal("caller timeout returned the founding permit", err)
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		fixture.effects.transients.mutex().Lock()
		units, held := fixture.effects.transients.units, fixture.effects.transients.bytes
		fixture.effects.transients.mutex().Unlock()
		if units == 0 && held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("actual process/read/reaper settlement retained units=%d bytes=%d", units, held)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRecordingTransientFoundingUnitsHaveIndependentCountBound(t *testing.T) {
	budget := &recordingTransientBudget{}
	owners := make([]*recordingUnitMemory, 0, recordingObserverUnitLimit)
	for i := 0; i < recordingObserverUnitLimit; i++ {
		owner, err := budget.acquire()
		if err != nil {
			t.Fatal(i, err)
		}
		owners = append(owners, owner)
	}
	if _, err := budget.acquire(); !errors.Is(err, errRecordingTransients) {
		t.Fatal("unbounded founding units")
	}
	for _, owner := range owners {
		owner.done()
	}
	if budget.units != 0 || budget.bytes != 0 {
		t.Fatal("permits did not settle")
	}
}

func TestRecordingCaptureReservesBeforeBuilderGrowth(t *testing.T) {
	budget := &recordingTransientBudget{limit: recordingUnitBytes + 1024}
	owner, _ := budget.acquire()
	defer owner.done()
	response := recordingResponse{owner: owner}
	defer response.release()
	data := bytes.Repeat([]byte{'x'}, 64<<10)
	allocations := testing.AllocsPerRun(10, func() {
		if err := response.Write(data); !errors.Is(err, errRecordingTransients) {
			t.Fatal(err)
		}
	})
	if response.builder.Cap() != 0 || response.bytes != 0 || allocations != 0 {
		t.Fatalf("unfunded builder: cap=%d charge=%d allocations=%g", response.builder.Cap(), response.bytes, allocations)
	}
}

func TestRecordingBirthPendingTinyReadsAndReaperSettlement(t *testing.T) {
	budget := &recordingTransientBudget{}
	owner, _ := budget.acquire()
	birth := &unifiedDevBirth{memory: owner}
	for i := 0; i < 10000; i++ {
		if err := birth.appendPendingLocked([]byte{'x'}); err != nil {
			t.Fatal(err)
		}
	}
	if len(birth.pending) != 1 || len(birth.pending[0].Data) != 10000 {
		t.Fatal("tiny reads amplified metadata")
	}
	if err := birth.appendPendingLocked(make([]byte, adoptionBootstrapCapBytes-10000)); err != nil {
		t.Fatal(err)
	}
	if err := birth.appendPendingLocked([]byte{'x'}); !errors.Is(err, errRecordingTransients) {
		t.Fatal("initial cap changed", err)
	}
	owner.done()
	if budget.units != 1 || budget.bytes <= recordingUnitBytes {
		t.Fatal("pending bytes lost their actual owner")
	}
	birth.clearPendingLocked()
	if budget.bytes != 0 || budget.units != 0 {
		t.Fatal("pending settlement leaked")
	}
}

func TestRecordingRotationPendingCreditTransfersToConsumer(t *testing.T) {
	budget := &recordingTransientBudget{}
	owner, _ := budget.acquire()
	pending := &unifiedRotationPending{memory: owner}
	if err := pending.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: make([]byte, 1<<20)}); err != nil {
		t.Fatal(err)
	}
	if err := pending.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: []byte{'x'}}); !errors.Is(err, ErrUnifiedRotatePendingOverflow) {
		t.Fatal(err)
	}
	owner.done()
	if budget.bytes != recordingUnitBytes+(1<<20) {
		t.Fatal("pending lost its consumer charge")
	}
	pending.release()
	if budget.bytes != 0 {
		t.Fatal("pending charge leak")
	}
}
