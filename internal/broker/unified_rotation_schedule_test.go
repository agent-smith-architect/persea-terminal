package broker

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/proto"
	"persea-terminal/internal/unifiedjournal"
)

type rotationScheduleClock struct {
	now time.Time
}

func (clock *rotationScheduleClock) advance(delay time.Duration) { clock.now = clock.now.Add(delay) }

func rotationScheduleKey(session string, generation uint64) unifiedjournal.PaneKey {
	return unifiedjournal.PaneKey{
		Server: "default", Session: session, ControlGeneration: generation,
		Window: "@1", Pane: "%1", Incarnation: "incarnation",
	}
}

func newRotationScheduleEffects(clock *rotationScheduleClock, pressure map[unifiedjournal.PaneKey]unifiedRotationPressure, attempts *[]string, outcomes *[]error) *UnifiedDevPaneEffects {
	effects := &UnifiedDevPaneEffects{
		active:              make(map[string]unifiedjournal.PaneKey),
		rotationStates:      make(map[string]*unifiedRotationState),
		rotationWake:        make(chan struct{}, 1),
		rotationEligibility: make(chan struct{}, 1),
		rotationNow:         func() time.Time { return clock.now },
		rotationPressure: func(key unifiedjournal.PaneKey) (unifiedRotationPressure, error) {
			value, ok := pressure[key]
			if !ok {
				return unifiedRotationPressure{}, errors.New("missing pressure")
			}
			return value, nil
		},
	}
	effects.rotationAttempt = func(_ context.Context, session string) error {
		*attempts = append(*attempts, session)
		if len(*outcomes) == 0 {
			return nil
		}
		outcome := (*outcomes)[0]
		*outcomes = (*outcomes)[1:]
		return outcome
	}
	return effects
}

func TestUnifiedRotationTriggerThresholdEdgesAndPhysicalWinner(t *testing.T) {
	clock := &rotationScheduleClock{now: time.Unix(1_000, 0)}
	pressure := make(map[unifiedjournal.PaneKey]unifiedRotationPressure)
	var attempts []string
	var outcomes []error
	effects := newRotationScheduleEffects(clock, pressure, &attempts, &outcomes)

	logical := rotationScheduleKey("logical", 1)
	physical := rotationScheduleKey("physical", 1)
	effects.active["logical"] = logical
	effects.active["physical"] = physical
	pressure[logical] = unifiedRotationPressure{logical: 74, logicalCap: 100, physical: 59, physicalCap: 80}
	pressure[physical] = unifiedRotationPressure{logical: 1, logicalCap: 100, physical: 60, physicalCap: 80}

	effects.requestRotationEvaluation(logical)
	effects.requestRotationEvaluation(physical)
	effects.runRotationScheduler(context.Background())
	if !reflect.DeepEqual(attempts, []string{"physical"}) {
		t.Fatalf("attempts=%v want physical threshold winner", attempts)
	}
	if state := effects.rotationState("logical"); state != nil && state.armed {
		t.Fatalf("logical pane armed below exact HIGH edge: %+v", state)
	}
}

func TestUnifiedRotationTriggerHysteresisAndSuccessOnlyCadence(t *testing.T) {
	clock := &rotationScheduleClock{now: time.Unix(2_000, 0)}
	key := rotationScheduleKey("target", 1)
	pressure := map[unifiedjournal.PaneKey]unifiedRotationPressure{
		key: {logical: 75, logicalCap: 100, physical: 1, physicalCap: 100},
	}
	var attempts []string
	outcomes := []error{nil, errors.New("eligible later"), nil}
	effects := newRotationScheduleEffects(clock, pressure, &attempts, &outcomes)
	effects.active["target"] = key

	effects.requestRotationEvaluation(key)
	effects.runRotationScheduler(context.Background())
	if len(attempts) != 1 {
		t.Fatalf("first success attempts=%v", attempts)
	}

	// A successful rotation alone starts the cadence. Pressure between LOW and
	// HIGH retains the arm, but cannot run before the full 60 seconds.
	pressure[key] = unifiedRotationPressure{logical: 65, logicalCap: 100, physical: 1, physicalCap: 100}
	effects.requestRotationEvaluation(key)
	clock.advance(59 * time.Second)
	effects.runRotationScheduler(context.Background())
	if len(attempts) != 1 {
		t.Fatalf("success cadence fired early: %v", attempts)
	}
	clock.advance(time.Second)
	effects.runRotationScheduler(context.Background())
	if len(attempts) != 2 {
		t.Fatalf("success cadence did not fire at 60s: %v", attempts)
	}

	// Refusal consumes only bounded retry backoff, never another cadence window.
	clock.advance(time.Second)
	effects.runRotationScheduler(context.Background())
	if len(attempts) != 3 {
		t.Fatalf("refusal consumed cadence instead of retry backoff: %v", attempts)
	}
	pressure[key] = unifiedRotationPressure{logical: 59, logicalCap: 100, physical: 1, physicalCap: 100}
	effects.requestRotationEvaluation(key)
	effects.runRotationScheduler(context.Background())
	if state := effects.rotationState("target"); state != nil && state.armed {
		t.Fatalf("state remained armed below LOW: %+v", state)
	}
}

func TestUnifiedRotationTriggerWakeLossFairnessAndPerPaneRetry(t *testing.T) {
	clock := &rotationScheduleClock{now: time.Unix(3_000, 0)}
	pressure := make(map[unifiedjournal.PaneKey]unifiedRotationPressure)
	var attempts []string
	outcomes := []error{ErrUnifiedRotateAlternateScreen, nil, nil}
	effects := newRotationScheduleEffects(clock, pressure, &attempts, &outcomes)

	a := rotationScheduleKey("a", 1)
	b := rotationScheduleKey("b", 1)
	effects.active["a"], effects.active["b"] = a, b
	pressure[a] = unifiedRotationPressure{logical: 80, logicalCap: 100, physicalCap: 100}
	pressure[b] = unifiedRotationPressure{logical: 80, logicalCap: 100, physicalCap: 100}

	// Both notifications collapse into the capacity-one wake. The coordinator
	// still rescans all dirty/armed panes and deterministically chooses session a.
	effects.requestRotationEvaluation(b)
	effects.requestRotationEvaluation(a)
	effects.runRotationScheduler(context.Background())
	if !reflect.DeepEqual(attempts, []string{"a"}) {
		t.Fatalf("oldest/tie fairness attempts=%v", attempts)
	}
	if state := effects.rotationState("a"); state == nil || !state.armed || !state.lastSuccess.IsZero() {
		t.Fatalf("alternate-screen refusal consumed arm/cadence: %+v", state)
	}

	// a is backed off for one second, so b proceeds independently now.
	effects.runRotationScheduler(context.Background())
	if !reflect.DeepEqual(attempts, []string{"a", "b"}) {
		t.Fatalf("per-pane retry/fairness attempts=%v", attempts)
	}
	clock.advance(time.Second)
	effects.runRotationScheduler(context.Background())
	if !reflect.DeepEqual(attempts, []string{"a", "b", "a"}) {
		t.Fatalf("armed pane stranded after a lost wake: %v", attempts)
	}
}

func TestUnifiedRotationTriggerComesFromDurableCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux retention commit")
	}
	saved := unifiedJournalCaps
	unifiedJournalCaps.pane = 64 << 10
	unifiedJournalCaps.realm = 8 << 20
	t.Cleanup(func() { unifiedJournalCaps = saved })

	fixture := newAdoptionFixture(t, 2)
	attempted := make(chan string, 1)
	fixture.effects.mu.Lock()
	fixture.effects.rotationAttempt = func(_ context.Context, session string) error {
		select {
		case attempted <- session:
		default:
		}
		return ErrUnifiedRotateAlternateScreen
	}
	fixture.effects.mu.Unlock()
	sessionID := fixture.startPaneCommand(t, "trigger", "exec sh")
	if _, err := fixture.effects.AdoptSession(context.Background(), sessionID); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	fixture.disposable.run("send-keys", "-t", "trigger:",
		"head -c 52000 /dev/zero | tr '\\0' x; printf '\\nTRIGGER_COMMITTED\\n'", "Enter")

	select {
	case got := <-attempted:
		if got != sessionID {
			t.Fatalf("rotation session=%q want %q", got, sessionID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("durable retention commit did not wake automatic rotation")
	}
}

func TestUnifiedRotationTriggerBackoffCapsWithoutDisarming(t *testing.T) {
	clock := &rotationScheduleClock{now: time.Unix(4_000, 0)}
	key := rotationScheduleKey("deferred", 1)
	pressure := map[unifiedjournal.PaneKey]unifiedRotationPressure{
		key: {logical: 80, logicalCap: 100, physicalCap: 100},
	}
	var attempts []string
	outcomes := make([]error, 12)
	for index := range outcomes {
		outcomes[index] = ErrUnifiedRotateAlternateScreen
	}
	effects := newRotationScheduleEffects(clock, pressure, &attempts, &outcomes)
	effects.active["deferred"] = key
	effects.requestRotationEvaluation(key)
	for index := 0; index < 12; index++ {
		effects.runRotationScheduler(context.Background())
		state := effects.rotationState("deferred")
		if state == nil || !state.armed || !state.lastSuccess.IsZero() {
			t.Fatalf("attempt %d consumed arm/cadence: %+v", index, state)
		}
		if state.backoff > unifiedRotationRetryMaximum {
			t.Fatalf("attempt %d backoff=%s exceeds cap", index, state.backoff)
		}
		clock.now = state.nextAttempt
	}
	if got := effects.rotationState("deferred").backoff; got != unifiedRotationRetryMaximum {
		t.Fatalf("backoff=%s want capped %s", got, unifiedRotationRetryMaximum)
	}
}

func TestUnifiedRotationCadenceAndBackoffStartAtAttemptCompletion(t *testing.T) {
	for _, item := range []struct {
		name    string
		outcome error
		wait    time.Duration
	}{
		{name: "success", wait: unifiedRotationCadence},
		{name: "failure", outcome: ErrUnifiedRotateAlternateScreen, wait: unifiedRotationRetryInitial},
	} {
		t.Run(item.name, func(t *testing.T) {
			started := time.Unix(20_000, 0)
			clock := &rotationScheduleClock{now: started}
			key := rotationScheduleKey(item.name, 1)
			pressure := map[unifiedjournal.PaneKey]unifiedRotationPressure{
				key: {logical: 80, logicalCap: 100, physicalCap: 100},
			}
			var attempts []string
			effects := newRotationScheduleEffects(clock, pressure, &attempts, nil)
			effects.active[item.name] = key
			effects.rotationAttempt = func(context.Context, string) error {
				attempts = append(attempts, item.name)
				clock.advance(45 * time.Second)
				return item.outcome
			}

			effects.requestRotationEvaluation(key)
			effects.runRotationScheduler(context.Background())
			completed := started.Add(45 * time.Second)
			state := effects.rotationState(item.name)
			if state == nil || !state.nextAttempt.Equal(completed.Add(item.wait)) {
				t.Fatalf("next attempt=%v want completion %v + %v; state=%+v", state.nextAttempt, completed, item.wait, state)
			}
			clock.now = started.Add(item.wait)
			effects.runRotationScheduler(context.Background())
			if len(attempts) != 1 {
				t.Fatalf("attempt repeated on start-based deadline: %v", attempts)
			}
		})
	}
}

func TestEligibilityWakeDuringSuccessCannotBypassCadence(t *testing.T) {
	started := time.Unix(30_000, 0)
	clock := &rotationScheduleClock{now: started}
	key := rotationScheduleKey("cadence-release", 1)
	pressure := map[unifiedjournal.PaneKey]unifiedRotationPressure{
		key: {logical: 80, logicalCap: 100, physicalCap: 100},
	}
	var attempts []string
	var outcomes []error
	effects := newRotationScheduleEffects(clock, pressure, &attempts, &outcomes)
	effects.active[key.Session] = key
	effects.rotationAttempt = func(context.Context, string) error {
		attempts = append(attempts, key.Session)
		if len(attempts) == 1 {
			clock.advance(45 * time.Second)
			// Resource releases coalesce while the successful attempt settles.
			for range 4 {
				effects.requestRotationEligibility()
			}
		}
		return nil
	}

	effects.requestRotationEvaluation(key)
	effects.runRotationScheduler(context.Background())
	completed := started.Add(45 * time.Second)
	effects.runRotationScheduler(context.Background())
	if !reflect.DeepEqual(attempts, []string{key.Session}) {
		t.Fatalf("eligibility wake bypassed post-success cadence: attempts=%v", attempts)
	}
	state := effects.rotationState(key.Session)
	if state == nil || !state.nextAttempt.Equal(completed.Add(unifiedRotationCadence)) {
		t.Fatalf("eligibility wake moved cadence deadline: state=%+v", state)
	}
	clock.now = completed.Add(unifiedRotationCadence - time.Nanosecond)
	effects.runRotationScheduler(context.Background())
	if len(attempts) != 1 {
		t.Fatalf("eligibility wake fired before cadence deadline: attempts=%v", attempts)
	}
	clock.now = completed.Add(unifiedRotationCadence)
	effects.runRotationScheduler(context.Background())
	if !reflect.DeepEqual(attempts, []string{key.Session, key.Session}) {
		t.Fatalf("cadence deadline did not release successor attempt: attempts=%v", attempts)
	}
}

func TestExplicitWidthRefitSuccessSharesRotationCadenceFloor(t *testing.T) {
	completed := time.Unix(34_000, 0)
	clock := &rotationScheduleClock{now: completed}
	key := rotationScheduleKey("explicit-refit-cadence", 2)
	pressure := map[unifiedjournal.PaneKey]unifiedRotationPressure{
		key: {logical: 80, logicalCap: 100, physicalCap: 100},
	}
	var attempts []string
	var outcomes []error
	effects := newRotationScheduleEffects(clock, pressure, &attempts, &outcomes)
	effects.active[key.Session] = key
	effects.rotationStates[key.Session] = &unifiedRotationState{key: key, armed: true}

	effects.recordExplicitRefitSuccess(key.Session, key)
	for range 4 {
		effects.requestRotationEligibility()
	}
	effects.runRotationScheduler(context.Background())
	if len(attempts) != 0 {
		t.Fatalf("capacity wake bypassed explicit-refit cadence: attempts=%v", attempts)
	}
	state := effects.rotationState(key.Session)
	if state == nil || !state.lastSuccess.Equal(completed) || !state.nextAttempt.Equal(completed.Add(unifiedRotationCadence)) {
		t.Fatalf("explicit refit did not publish completion-time cadence: %+v", state)
	}
	clock.now = completed.Add(unifiedRotationCadence)
	effects.runRotationScheduler(context.Background())
	if !reflect.DeepEqual(attempts, []string{key.Session}) {
		t.Fatalf("explicit-refit cadence did not release at the floor: attempts=%v", attempts)
	}
}

func TestGeometryOwnerReleaseWakesEligibilityOnlyForExactOwner(t *testing.T) {
	clock := &rotationScheduleClock{now: time.Unix(35_000, 0)}
	key := rotationScheduleKey("owner", 1)
	pressure := map[unifiedjournal.PaneKey]unifiedRotationPressure{
		key: {logical: 80, logicalCap: 100, physicalCap: 100},
	}
	var attempts []string
	outcomes := []error{}
	effects := newRotationScheduleEffects(clock, pressure, &attempts, &outcomes)
	effects.active[key.Session] = key
	effects.rotationStates[key.Session] = &unifiedRotationState{
		key: key, armed: true, backoff: unifiedRotationRetryMaximum,
		nextAttempt: clock.now.Add(unifiedRotationRetryMaximum),
	}
	runtime := &retentionTrialRuntime{
		options:        retentionTrialOptions{eligibilityWake: effects.requestRotationEligibility},
		geometryOwners: make(map[unifiedjournal.PaneKey]uint64),
	}
	owner, err := runtime.acquireGeometryOwner(key)
	if err != nil {
		t.Fatal(err)
	}
	runtime.releaseGeometryOwner(key, owner+1)
	effects.runRotationScheduler(context.Background())
	if len(attempts) != 0 {
		t.Fatalf("non-owner release caused an attempt: %v", attempts)
	}
	runtime.releaseGeometryOwner(key, owner)
	runtime.releaseGeometryOwner(key, owner)
	effects.runRotationScheduler(context.Background())
	if !reflect.DeepEqual(attempts, []string{key.Session}) {
		t.Fatalf("exact geometry-owner release did not promptly attempt: %v", attempts)
	}
}

func TestEligibilityReleaseInterruptsMaximumBackoffWithoutBusyLoop(t *testing.T) {
	clock := &rotationScheduleClock{now: time.Unix(40_000, 0)}
	key := rotationScheduleKey("resource-wait", 1)
	pressure := map[unifiedjournal.PaneKey]unifiedRotationPressure{
		key: {logical: 80, logicalCap: 100, physicalCap: 100},
	}
	var attempts []string
	outcomes := []error{}
	effects := newRotationScheduleEffects(clock, pressure, &attempts, &outcomes)
	effects.active[key.Session] = key
	effects.rotationStates[key.Session] = &unifiedRotationState{
		key: key, armed: true, backoff: unifiedRotationRetryMaximum,
		nextAttempt: clock.now.Add(unifiedRotationRetryMaximum),
	}

	effects.requestRotationEligibility()
	effects.runRotationScheduler(context.Background())
	if !reflect.DeepEqual(attempts, []string{key.Session}) {
		t.Fatalf("prompt release attempt=%v", attempts)
	}
	// A coalesced wake without another owning release cannot bypass cadence.
	effects.wakeRotationScheduler()
	effects.runRotationScheduler(context.Background())
	if len(attempts) != 1 {
		t.Fatalf("non-release wake caused a busy loop: %v", attempts)
	}
}

func TestAlternateScreenDeferralClearsBelowLowAndOnSessionReplacement(t *testing.T) {
	clock := &rotationScheduleClock{now: time.Unix(50_000, 0)}
	oldKey := rotationScheduleKey("replace", 1)
	newKey := rotationScheduleKey("replace", 2)
	pressure := map[unifiedjournal.PaneKey]unifiedRotationPressure{}
	var attempts []string
	outcomes := []error{}
	effects := newRotationScheduleEffects(clock, pressure, &attempts, &outcomes)
	effects.active[oldKey.Session] = oldKey
	effects.rotationStates[oldKey.Session] = &unifiedRotationState{key: oldKey, armed: true, deferredAltScreen: true}
	effects.updateRotationPressure(unifiedRotationCandidate{session: oldKey.Session, key: oldKey}, unifiedRotationPressure{logical: 59, logicalCap: 100, physicalCap: 100}, clock.now)
	if state := effects.rotationState(oldKey.Session); state.armed || state.deferredAltScreen {
		t.Fatalf("below-LOW state retained deferral: %+v", state)
	}

	effects.mu.Lock()
	effects.active[newKey.Session] = newKey
	effects.rotationStates[newKey.Session] = &unifiedRotationState{key: oldKey, armed: true, deferredAltScreen: true}
	effects.mu.Unlock()
	effects.updateRotationPressure(unifiedRotationCandidate{session: newKey.Session, key: newKey}, unifiedRotationPressure{logical: 80, logicalCap: 100, physicalCap: 100}, clock.now)
	if state := effects.rotationState(newKey.Session); state.key != newKey || state.deferredAltScreen {
		t.Fatalf("session replacement retained stale deferral: %+v", state)
	}
}

func TestUnitReapClearsAlternateScreenDeferral(t *testing.T) {
	effects := &UnifiedDevPaneEffects{
		units: make(map[string]*unifiedDevUnit), panes: make(map[string]*unifiedDevBirth),
		active: make(map[string]unifiedjournal.PaneKey), rotationStates: make(map[string]*unifiedRotationState),
		publishedSequence: make(map[unifiedjournal.PaneKey]int64),
	}
	unit := &unifiedDevUnit{sessionID: "reaped", birth: unifiedDevCommand{}}
	effects.units[unit.sessionID] = unit
	effects.rotationStates[unit.sessionID] = &unifiedRotationState{armed: true, deferredAltScreen: true}
	effects.reapUnitOnce(unit)
	if state := effects.rotationState(unit.sessionID); state != nil {
		t.Fatalf("unit reap retained rotation deferral: %+v", state)
	}
}

func TestUnifiedRotationCommitWakeNeverWaitsForProviderAuthority(t *testing.T) {
	effects := &UnifiedDevPaneEffects{rotationWake: make(chan struct{}, 1)}
	effects.mu.Lock()
	done := make(chan struct{})
	go func() {
		effects.requestRotationEvaluation(rotationScheduleKey("blocked-provider", 1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		effects.mu.Unlock()
		t.Fatal("durable commit wake waited for provider authority")
	}
	effects.mu.Unlock()
	select {
	case <-effects.rotationWake:
	default:
		t.Fatal("lock-free commit wake was lost")
	}
}

func TestUnifiedRotationPressureAndCapacitySettlementShareJournalFence(t *testing.T) {
	realm := openRetentionRealm(t, "rotation-pressure-settlement-fence")
	key := rotationScheduleKey("$pressure-fence", 1)
	if err := realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	effects := &UnifiedDevPaneEffects{realm: realm}
	for attempt := 0; attempt < 200; attempt++ {
		effects.journalMu.Lock()
		capacity, err := realm.BeginRotationCapacity(key)
		effects.journalMu.Unlock()
		if err != nil {
			t.Fatalf("attempt %d capacity: %v", attempt, err)
		}
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			if _, err := effects.readRotationPressure(key); err != nil {
				t.Errorf("attempt %d pressure: %v", attempt, err)
			}
		}()
		go func() {
			defer wait.Done()
			<-start
			effects.releaseRotationCapacity(capacity)
		}()
		close(start)
		wait.Wait()
	}
}

func TestUnifiedRotationAutomaticRealTmuxTriggerToSealAndForcedReopen(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux automatic rotation")
	}
	saved := unifiedJournalCaps
	unifiedJournalCaps.pane = 8 << 20
	unifiedJournalCaps.realm = 64 << 20
	unifiedJournalCaps.panePhysical = 16 << 20
	unifiedJournalCaps.realmPhysical = 96 << 20
	t.Cleanup(func() { unifiedJournalCaps = saved })

	fixture := newAdoptionFixture(t, 4)
	type edgeMeasurement struct {
		at                                    time.Time
		charge, physicalCharged, physicalHeld int64
		physicalCap                           int64
		bootstrapBytes, bootstrapRecords      int64
		pendingBytes, pendingRecords          int64
		bootstrapErr                          error
	}
	var measureMu sync.Mutex
	var measuredKey unifiedjournal.PaneKey
	edges := make(map[string]edgeMeasurement)
	var attemptErr error
	attemptDone := make(chan struct{})
	var attemptDoneOnce sync.Once
	fixture.effects.mu.Lock()
	fixture.effects.rotationAttempt = func(ctx context.Context, session string) error {
		err := fixture.effects.rotateSession(ctx, session)
		measureMu.Lock()
		attemptErr = err
		measureMu.Unlock()
		attemptDoneOnce.Do(func() { close(attemptDone) })
		return err
	}
	fixture.effects.rotationEdge = func(session, edge string) {
		measurement := edgeMeasurement{at: time.Now()}
		measureMu.Lock()
		key := measuredKey
		measureMu.Unlock()
		fixture.effects.mu.Lock()
		rotation := fixture.effects.rotation
		fixture.effects.mu.Unlock()

		if key == (unifiedjournal.PaneKey{}) {
			return
		}
		fixture.effects.journalMu.Lock()
		measurement.charge, _ = fixture.effects.realm.PaneLogical(key)
		measurement.physicalCharged, measurement.physicalHeld, measurement.physicalCap = fixture.effects.realm.PhysicalBudget()
		if rotation != nil && rotation.newKey != (unifiedjournal.PaneKey{}) {
			var events []unifiedjournal.Event
			events, measurement.bootstrapErr = fixture.effects.realm.ReadCommittedEvents(rotation.newKey)
			if measurement.bootstrapErr == nil {
				measurement.bootstrapRecords = int64(len(events))
				for _, event := range events {
					measurement.bootstrapBytes += int64(len(event.Payload))
				}
			}
		}
		fixture.effects.journalMu.Unlock()
		// holder_flipped is emitted from inside holder.mu; all later edges are
		// outside it and may sample the bounded pending replay safely.
		if edge != "holder_flipped" && rotation != nil && rotation.holder != nil {
			rotation.holder.mu.Lock()
			if rotation.holder.rotation != nil {
				measurement.pendingBytes = int64(len(rotation.holder.rotation.data))
				measurement.pendingRecords = (measurement.pendingBytes + rotationReplayBatchBytes - 1) / rotationReplayBatchBytes
			}
			rotation.holder.mu.Unlock()
		}
		measureMu.Lock()
		edges[edge] = measurement
		measureMu.Unlock()
	}
	fixture.effects.mu.Unlock()

	sessionID := fixture.startPaneCommand(t, "automatic", "exec sh")
	adoption, err := fixture.effects.AdoptSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	measureMu.Lock()
	measuredKey = adoption.Key
	measureMu.Unlock()
	_, _, predecessor, cancelPredecessor, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatalf("open predecessor: %v", err)
	}
	defer cancelPredecessor()
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for deliveredForTest := range predecessor.events() {
			predecessor.releaseEvent(deliveredForTest)
		}
	}()

	fixture.disposable.run("send-keys", "-t", "automatic:",
		`awk 'BEGIN { for (i=0; i<102000; i++) printf "AUTO-%06d-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n", i; fflush() }'`, "Enter")

	var successor unifiedjournal.PaneKey
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		fixture.effects.mu.Lock()
		successor = fixture.effects.active[sessionID]
		fixture.effects.mu.Unlock()
		if successor != (unifiedjournal.PaneKey{}) && successor != adoption.Key {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if successor == (unifiedjournal.PaneKey{}) || successor == adoption.Key {
		fixture.effects.journalMu.Lock()
		charge, cap := fixture.effects.realm.PaneLogical(adoption.Key)
		fixture.effects.journalMu.Unlock()
		measureMu.Lock()
		seen := make(map[string]edgeMeasurement, len(edges))
		for edge, measurement := range edges {
			seen[edge] = measurement
		}
		observedErr := attemptErr
		measureMu.Unlock()
		t.Fatalf("automatic successor absent: logical=%d/%d err=%v edges=%v capture_tail=%q", charge, cap, observedErr, seen, fixture.capture(t, "automatic"))
	}
	if reason := waitRotationSubscriberClose(t, predecessor); reason != proto.SubscriberClosedGenerationRotated {
		t.Fatalf("predecessor close=%q want %q", reason, proto.SubscriberClosedGenerationRotated)
	}
	<-drainDone
	select {
	case <-attemptDone:
	case <-time.After(10 * time.Second):
		t.Fatal("automatic rotation attempt did not settle")
	}

	measureMu.Lock()
	seen := make(map[string]edgeMeasurement, len(edges))
	for edge, measurement := range edges {
		seen[edge] = measurement
	}
	measureMu.Unlock()
	trigger, triggerOK := seen["trigger_selected"]
	sealed, sealedOK := seen["predecessor_sealed"]
	if !triggerOK || !sealedOK {
		t.Fatalf("missing trigger/seal edges: %v", seen)
	}
	if duration := sealed.at.Sub(trigger.at); duration < 0 || duration > 5*time.Second {
		t.Fatalf("trigger-to-seal duration=%s exceeds bounded proxy", duration)
	}
	if admitted := sealed.charge - trigger.charge; admitted < 0 || admitted > unifiedjournal.RotationPendingCapBytes {
		t.Fatalf("predecessor in-flight bytes=%d outside [0,%d]", admitted, unifiedjournal.RotationPendingCapBytes)
	} else {
		var peakCharged, peakHeld, physicalCap int64
		for _, measurement := range seen {
			if measurement.physicalCharged > peakCharged {
				peakCharged = measurement.physicalCharged
			}
			if measurement.physicalHeld > peakHeld {
				peakHeld = measurement.physicalHeld
			}
			if measurement.physicalCap > physicalCap {
				physicalCap = measurement.physicalCap
			}
		}
		if sealed.bootstrapErr != nil {
			t.Fatalf("successor bootstrap measurement: %v", sealed.bootstrapErr)
		}
		if sealed.bootstrapBytes > unifiedjournal.AdoptionBootstrapCapBytes || sealed.bootstrapRecords > unifiedjournal.AdoptionBootstrapCapRecords {
			t.Fatalf("successor bootstrap bytes=%d/%d records=%d/%d", sealed.bootstrapBytes, unifiedjournal.AdoptionBootstrapCapBytes, sealed.bootstrapRecords, unifiedjournal.AdoptionBootstrapCapRecords)
		}
		if sealed.pendingBytes > unifiedjournal.RotationPendingCapBytes || sealed.pendingRecords > unifiedjournal.RotationPendingCapRecords {
			t.Fatalf("successor pending bytes=%d/%d records=%d/%d", sealed.pendingBytes, unifiedjournal.RotationPendingCapBytes, sealed.pendingRecords, unifiedjournal.RotationPendingCapRecords)
		}
		if peakCharged+peakHeld > physicalCap {
			t.Fatalf("peak physical charged=%d held=%d cap=%d", peakCharged, peakHeld, physicalCap)
		}
		t.Logf("automatic trigger duration=%s predecessor_inflight=%d peak_physical_charged=%d peak_physical_held=%d physical_cap=%d bootstrap_bytes=%d bootstrap_records=%d pending_bytes=%d pending_records=%d", sealed.at.Sub(trigger.at), admitted, peakCharged, peakHeld, physicalCap, sealed.bootstrapBytes, sealed.bootstrapRecords, sealed.pendingBytes, sealed.pendingRecords)
	}

	events, _, successorTail, cancelSuccessor, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatalf("open successor: %v", err)
	}
	defer cancelSuccessor()
	if got := bytes.Count(rotationSnapshotBytes(events), []byte("AUTO-101999")); got != 1 {
		t.Fatalf("successor authoritative bootstrap count=%d want 1", got)
	}
	fixture.disposable.run("send-keys", "-t", "automatic:", "printf 'AUTO-POST\\n'", "Enter")
	select {
	case event, openForRelease := <-successorTail.events():
		if openForRelease {
			successorTail.releaseEvent(event)
		}
		if event.Kind != unifiedjournal.RecordOutput || !bytes.Contains(event.Payload, []byte("AUTO-POST")) {
			t.Fatalf("successor tail=%+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("successor did not deliver post-rotation sentinel")
	}
}
