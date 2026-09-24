package broker

// ---------------------------------------------------------------------------
// ISSUE25 — canceled BeginGeometry must never strand the journal pause.
//
// BeginGeometry queues a non-cancelable pause_start and waits for its result.
// A request context that cancels in that window still returns a typed
// operational refusal — but the queued pause must be paired with a pause_end
// that something owns, or the late pause lands with no ticket to release it
// and the pane holds output forever behind a Fit the epoch recorded as a
// non-mutation. The deterministic test exercises that window; the stress test
// pins that the pairing survives repeated races against output, later Fits on
// the same pane, and the journal lock being held across the queue/ack window.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

// A typed pre-issue refusal promises that the attachment and journal runtime
// remain usable. Cancel after pause_start is queued but before it is
// acknowledged: BeginGeometry must not return a typed refusal while that
// queued command can later strand the pane in the paused state.
func TestCanceledBeginGeometryCannotStrandPause(t *testing.T) {
	harness := newGeometryBarrierHarness(t, "canceled-begin")
	runtime := harness.registry.retention

	// Hold the manager in pause_start's flush after it has consumed the command.
	harness.journalMu.Lock()
	locked := true
	defer func() {
		if locked {
			harness.journalMu.Unlock()
		}
	}()
	// Give pause_start something to flush. Without pending output it can
	// acknowledge before reaching the journal lock, which tests a different
	// (post-ack) cancellation shape.
	if err := runtime.WritePane(harness.key, []byte("PENDING")); err != nil {
		t.Fatalf("queue pending output: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		ticket geometryTicket
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		ticket, err := harness.issuer.BeginGeometry(ctx)
		resultCh <- result{ticket: ticket, err: err}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.mu.Lock()
		used := runtime.qUsed
		runtime.mu.Unlock()
		if used >= 6 { // generation floor (2), commit, release, fault, pause_start
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("BeginGeometry never queued pause_start")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	got := <-resultCh
	if got.ticket != nil || !terminal.IsResizeRefusal(got.err) {
		t.Fatalf("canceled BeginGeometry ticket=%T err=%v, want typed refusal", got.ticket, got.err)
	}

	harness.journalMu.Unlock()
	locked = false
	deadline = time.Now().Add(5 * time.Second)
	lastUsed := -1
	for {
		runtime.mu.Lock()
		used := runtime.qUsed
		runtime.mu.Unlock()
		lastUsed = used
		if used == 2 { // the generation's lifetime/cleanup floor remains
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pause_start did not settle: qUsed=%d output=%q", lastUsed, harness.down.outputText())
		}
		time.Sleep(time.Millisecond)
	}

	if err := runtime.WritePane(harness.key, []byte("AFTER-CANCEL")); err != nil {
		t.Fatalf("write after typed refusal: %v", err)
	}
	// Flush accepted output and wait for its dispatch, without changing pause
	// ownership. A fixed sleep can mistake a delayed worker for a stranded pause.
	if err := runtime.Boundary(harness.key, "cancel_probe"); err != nil {
		t.Fatalf("flush after typed refusal: %v", err)
	}
	dispatched, err := runtime.startDispatchFence()
	if err != nil {
		t.Fatalf("dispatch after typed refusal: %v", err)
	}
	select {
	case <-dispatched:
	case <-time.After(5 * time.Second):
		t.Fatal("output dispatch after typed refusal did not settle")
	}
	if got := harness.down.outputText(); got != "PENDINGAFTER-CANCEL" {
		// Restore the disposable harness before reporting the RED so its cleanup
		// does not wait on the deliberately stranded hold.
		if err := runtime.Boundary(harness.key, "pause_end"); err != nil {
			t.Fatalf("typed resize refusal stranded the journal pause and recovery failed: published=%q recovery=%v", got, err)
		}
		harness.down.awaitOutput(t, "manual recovery", "PENDINGAFTER-CANCEL")
		t.Fatalf("typed resize refusal stranded the journal pause: published=%q want=%q", got, "PENDINGAFTER-CANCEL")
	}

	// The refusal was operational, so the same generation must still carry a
	// full Fit: the pane is not paused, no slot leaked, and a committed
	// geometry still lands between the output that precedes and follows it.
	ticket, err := harness.issuer.BeginGeometry(context.Background())
	if err != nil {
		t.Fatalf("BeginGeometry after canceled begin: %v", err)
	}
	if err := runtime.WritePane(harness.key, []byte("held")); err != nil {
		t.Fatalf("write during post-cancel fit: %v", err)
	}
	if err := ticket.Commit(context.Background(), 80, 41); err != nil {
		t.Fatalf("commit after canceled begin: %v", err)
	}
	harness.down.awaitOutput(t, "post-cancel fit", "PENDINGAFTER-CANCELheld")
	var order []string
	for _, event := range harness.down.snapshot() {
		if event.Kind == unifiedjournal.RecordGeometry {
			order = append(order, "GEOMETRY")
			continue
		}
		order = append(order, string(event.Payload))
	}
	want := []string{"PENDING", "AFTER-CANCEL", "GEOMETRY", "held"}
	if len(order) != len(want) {
		t.Fatalf("post-cancel published order=%v want=%v", order, want)
	}
	for index := range want {
		if order[index] != want[index] {
			t.Fatalf("post-cancel published order=%v want=%v", order, want)
		}
	}
}

// A second attachment can begin a Fit after its own gate admits it while a
// displaced attachment is still unwinding a canceled BeginGeometry. The
// canceled begin must enqueue its pause_end ahead of that later pause_start;
// otherwise end1 resumes publication inside barrier2.
func TestAdvisorCanceledBeginCannotEndInsideConcurrentSamePaneBegin(t *testing.T) {
	h := newGeometryBarrierHarness(t, "advisor-same-pane-cancel-interleave")
	runtime := h.registry.retention

	h.journalMu.Lock()
	locked := true
	defer func() {
		if locked {
			h.journalMu.Unlock()
		}
	}()
	if err := runtime.WritePane(h.key, []byte("PENDING")); err != nil {
		t.Fatalf("queue pending output: %v", err)
	}

	type beginResult struct {
		ticket geometryTicket
		err    error
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	first := make(chan beginResult, 1)
	go func() {
		ticket, err := h.issuer.BeginGeometry(ctx1)
		first <- beginResult{ticket: ticket, err: err}
	}()
	waitQUsed := func(want int, label string) {
		deadline := time.Now().Add(5 * time.Second)
		for {
			runtime.mu.Lock()
			used := runtime.qUsed
			runtime.mu.Unlock()
			if used >= want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: qUsed=%d want-at-least=%d", label, used, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitQUsed(6, "first pause_start")

	// A distinct attachment issuer for the same pane must be refused before it
	// can reserve slots or queue start2. This is the exact start1,start2,end1
	// ordering the shared owner excludes.
	otherIssuer := &unifiedGeometryIssuer{provider: h.provider, registry: h.registry, session: "$0"}
	second := make(chan beginResult, 1)
	go func() {
		ticket, err := otherIssuer.BeginGeometry(context.Background())
		second <- beginResult{ticket: ticket, err: err}
	}()
	got2 := <-second
	if got2.ticket != nil || !terminal.IsResizeRefusal(got2.err) || !errors.Is(got2.err, errGeometryPauseOwned) {
		t.Fatalf("concurrent begin ticket=%T err=%v, want geometry-owner refusal", got2.ticket, got2.err)
	}
	runtime.mu.Lock()
	usedAfterRefusal := runtime.qUsed
	queuedStarts := 0
	for _, command := range queuedRetentionCommands(&runtime.queue) {
		if command.key == h.key && command.reason == "pause_start" {
			queuedStarts++
		}
	}
	runtime.mu.Unlock()
	if usedAfterRefusal != 6 || queuedStarts != 0 {
		t.Fatalf("concurrent refusal touched runtime: qUsed=%d queued pause_start=%d", usedAfterRefusal, queuedStarts)
	}

	cancel1()
	got1 := <-first
	if got1.ticket != nil || !terminal.IsResizeRefusal(got1.err) {
		t.Fatalf("first begin ticket=%T err=%v, want typed refusal", got1.ticket, got1.err)
	}

	// Cancellation enqueued end1 while still owning the pane. A later issuer
	// can now acquire ownership, but its start3 must land after end1 in FIFO.
	third := make(chan beginResult, 1)
	go func() {
		ticket, err := otherIssuer.BeginGeometry(context.Background())
		third <- beginResult{ticket: ticket, err: err}
	}()
	h.journalMu.Unlock()
	locked = false
	got3 := <-third
	if got3.err != nil || got3.ticket == nil {
		t.Fatalf("later begin ticket=%T err=%v", got3.ticket, got3.err)
	}
	defer got3.ticket.Release()

	// Wait until end1 has consumed its reservation. The second ticket still
	// owns commit/release/fault, so the generation floor plus those slots is 5.
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.mu.Lock()
		used := runtime.qUsed
		runtime.mu.Unlock()
		if used == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("end1 did not settle: qUsed=%d", used)
		}
		time.Sleep(time.Millisecond)
	}

	if err := runtime.WritePane(h.key, []byte("LEAK")); err != nil {
		t.Fatalf("write inside second barrier: %v", err)
	}
	if err := runtime.Boundary(h.key, "advisor_probe"); err != nil {
		t.Fatalf("probe boundary: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := h.down.outputText(); strings.Contains(got, "LEAK") {
		t.Fatalf("canceled begin's pause_end resumed publication inside later barrier: output=%q", got)
	}
	got3.ticket.Release()
	h.down.awaitOutput(t, "release after later barrier", "PENDINGLEAK")
}

// --- stress: pause ownership under repeated cancellation races --------------

type cancelStressDownstream struct {
	mu     sync.Mutex
	events map[unifiedjournal.PaneKey][]unifiedjournal.Event
}

func (downstream *cancelStressDownstream) WritePane(unifiedjournal.PaneKey, []byte) error {
	return errors.New("unified feed range metadata is required")
}

func (downstream *cancelStressDownstream) WritePaneRange(key unifiedjournal.PaneKey, payload []byte, start, end, sequence int64) error {
	return downstream.record(key, unifiedjournal.Event{
		Kind: unifiedjournal.RecordOutput, Sequence: sequence, Start: start, End: end,
		Payload: append([]byte(nil), payload...),
	})
}

func (downstream *cancelStressDownstream) WritePaneGeometry(key unifiedjournal.PaneKey, event unifiedjournal.Event) error {
	return downstream.record(key, event)
}

func (downstream *cancelStressDownstream) record(key unifiedjournal.PaneKey, event unifiedjournal.Event) error {
	downstream.mu.Lock()
	downstream.events[key] = append(downstream.events[key], event)
	downstream.mu.Unlock()
	return nil
}

func (downstream *cancelStressDownstream) snapshot(key unifiedjournal.PaneKey) []unifiedjournal.Event {
	downstream.mu.Lock()
	defer downstream.mu.Unlock()
	return append([]unifiedjournal.Event(nil), downstream.events[key]...)
}

func (downstream *cancelStressDownstream) outputText(key unifiedjournal.PaneKey) string {
	var builder strings.Builder
	for _, event := range downstream.snapshot(key) {
		if event.Kind == unifiedjournal.RecordOutput {
			builder.Write(event.Payload)
		}
	}
	return builder.String()
}

type cancelStressCounts struct {
	mu     sync.Mutex
	counts map[string]int
}

func (counts *cancelStressCounts) observe(event string, _ map[string]int64) {
	counts.mu.Lock()
	counts.counts[event]++
	counts.mu.Unlock()
}

func (counts *cancelStressCounts) get(event string) int {
	counts.mu.Lock()
	defer counts.mu.Unlock()
	return counts.counts[event]
}

type cancelStressEffects struct {
	realm      *unifiedjournal.Realm
	journalMu  *sync.Mutex
	downstream *cancelStressDownstream
	counts     *cancelStressCounts
	stage      func(string, unifiedjournal.PaneKey) error
	hook       func(string, unifiedjournal.PaneKey)
}

func (effects cancelStressEffects) RunObserver(ctx context.Context, _ PaneObservationEffects) error {
	<-ctx.Done()
	return ctx.Err()
}

func (effects cancelStressEffects) WritePane(key unifiedjournal.PaneKey, payload []byte) error {
	return effects.downstream.WritePane(key, payload)
}

func (effects cancelStressEffects) WritePaneRange(key unifiedjournal.PaneKey, payload []byte, start, end, sequence int64) error {
	return effects.downstream.WritePaneRange(key, payload, start, end, sequence)
}

func (effects cancelStressEffects) WritePaneGeometry(key unifiedjournal.PaneKey, event unifiedjournal.Event) error {
	return effects.downstream.WritePaneGeometry(key, event)
}

func (effects cancelStressEffects) BeginSessionBirth(string, string) (SessionBirthEffects, error) {
	return nil, nil
}

func (effects cancelStressEffects) retentionTrial() retentionTrialOptions {
	return retentionTrialOptions{
		realm: effects.realm, journalMu: effects.journalMu, maxBatchBytes: 64 << 10, maxFeedDelay: 16 * time.Millisecond,
		now: time.Now,
		after: func(delay time.Duration, fn func()) func() bool {
			timer := time.AfterFunc(delay, fn)
			return timer.Stop
		},
		observe: effects.counts.observe,
		stage:   effects.stage,
		hook:    effects.hook,
	}
}

// A cancellation may win the caller's select just before pause_start reports
// that its flush failed. The caller gets the pre-issue refusal, but the late
// storage verdict must still fail the generation closed, enqueue its harmless
// unpaused pause_end, and release the per-pane owner for teardown/reopen.
func TestCanceledBeginLateSettledFailureFailsClosedAndReleasesOwner(t *testing.T) {
	realm := openRetentionRealm(t, "cancel-late-pause-failure")
	downstream := &cancelStressDownstream{events: make(map[unifiedjournal.PaneKey][]unifiedjournal.Event)}
	counts := &cancelStressCounts{counts: make(map[string]int)}
	var journalMu sync.Mutex
	var stageMu sync.Mutex
	failAppend := true
	effects := cancelStressEffects{
		realm: realm, journalMu: &journalMu, downstream: downstream, counts: counts,
		stage: func(operation string, _ unifiedjournal.PaneKey) error {
			stageMu.Lock()
			defer stageMu.Unlock()
			if operation == "append" && failAppend {
				failAppend = false
				return errors.New("forced late pause flush failure")
			}
			return nil
		},
	}
	registry := newPaneRegistry(effects)
	runtime := registry.retention
	if runtime == nil {
		t.Fatal("retention runtime unavailable")
	}
	t.Cleanup(func() { _ = registry.Close() })

	witness := retentionWitness("%0", "cancel-late-pause-failure-incarnation")
	key := journalKey(witness)
	if err := realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("admit pane: %v", err)
	}
	registry.mu.Lock()
	registry.admitted[routeCoordinateKey(witness)] = paneAdmissionState{witness: witness}
	registry.mu.Unlock()
	provider := &UnifiedDevPaneEffects{
		realm: realm, active: map[string]unifiedjournal.PaneKey{"$0": key},
		subscribers: make(map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}),
	}
	issuer := &unifiedGeometryIssuer{provider: provider, registry: registry, session: "$0"}

	journalMu.Lock()
	locked := true
	defer func() {
		if locked {
			journalMu.Unlock()
		}
	}()
	if err := runtime.WritePane(key, []byte("FAIL-ME")); err != nil {
		t.Fatalf("queue pending output: %v", err)
	}
	runtime.markAdmitted(key)
	ctx, cancel := context.WithCancel(context.Background())
	type beginResult struct {
		ticket geometryTicket
		err    error
	}
	result := make(chan beginResult, 1)
	go func() {
		ticket, err := issuer.BeginGeometry(ctx)
		result <- beginResult{ticket: ticket, err: err}
	}()
	waitGeometryOwnerState(t, runtime, key, true, 6)
	cancel()
	got := <-result
	if got.ticket != nil || !terminal.IsResizeRefusal(got.err) || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("canceled begin ticket=%T err=%v, want context refusal", got.ticket, got.err)
	}

	journalMu.Unlock()
	locked = false
	deadline := time.Now().Add(5 * time.Second)
	for {
		registry.mu.Lock()
		sticky := registry.broken || registry.failedIncarnationLocked(witness)
		registry.mu.Unlock()
		runtime.mu.Lock()
		generation := runtime.generations[key]
		failed := generation == nil || generation.failed
		owner := runtime.geometryOwners[key]
		pane := runtime.panes[key]
		paused := pane != nil && pane.paused
		runtime.mu.Unlock()
		if failed && sticky && owner == 0 && !paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("late failure did not settle fail-closed: failed=%v sticky=%v owner=%d paused=%v", failed, sticky, owner, paused)
		}
		time.Sleep(time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for counts.get("boundary_error") == 0 && counts.get("storage_fault") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("late pause failure produced no fail-closed observation")
		}
		time.Sleep(time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for counts.get("pause_end_complete") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("late failed pause did not execute its unpaused pause_end")
		}
		time.Sleep(time.Millisecond)
	}
}

// If pause_start's failure settles while cancellation is being observed, the
// buffered result wins the nested probe. That path is fatal (not operational),
// owes no pause_end because the pause never applied, and still releases the
// shared owner.
func TestCanceledBeginSettledRaceFailureIsFatalAndReleasesOwner(t *testing.T) {
	realm := openRetentionRealm(t, "cancel-settled-pause-failure")
	downstream := &cancelStressDownstream{events: make(map[unifiedjournal.PaneKey][]unifiedjournal.Event)}
	counts := &cancelStressCounts{counts: make(map[string]int)}
	var journalMu sync.Mutex
	var stageMu sync.Mutex
	failAppend := true
	cancelObserved := make(chan struct{})
	allowProbe := make(chan struct{})
	var observedOnce sync.Once
	effects := cancelStressEffects{
		realm: realm, journalMu: &journalMu, downstream: downstream, counts: counts,
		stage: func(operation string, _ unifiedjournal.PaneKey) error {
			stageMu.Lock()
			defer stageMu.Unlock()
			if operation == "append" && failAppend {
				failAppend = false
				return errors.New("forced settled pause flush failure")
			}
			return nil
		},
		hook: func(point string, _ unifiedjournal.PaneKey) {
			if point != "geometry_cancel_observed" {
				return
			}
			observedOnce.Do(func() { close(cancelObserved) })
			<-allowProbe
		},
	}
	registry := newPaneRegistry(effects)
	runtime := registry.retention
	if runtime == nil {
		t.Fatal("retention runtime unavailable")
	}
	t.Cleanup(func() { _ = registry.Close() })

	witness := retentionWitness("%0", "cancel-settled-pause-failure-incarnation")
	key := journalKey(witness)
	if err := realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("admit pane: %v", err)
	}
	registry.mu.Lock()
	registry.admitted[routeCoordinateKey(witness)] = paneAdmissionState{witness: witness}
	registry.mu.Unlock()
	provider := &UnifiedDevPaneEffects{
		realm: realm, active: map[string]unifiedjournal.PaneKey{"$0": key},
		subscribers: make(map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}),
	}
	issuer := &unifiedGeometryIssuer{provider: provider, registry: registry, session: "$0"}

	journalMu.Lock()
	locked := true
	defer func() {
		if locked {
			journalMu.Unlock()
		}
	}()
	if err := runtime.WritePane(key, []byte("FAIL-SETTLED")); err != nil {
		t.Fatalf("queue pending output: %v", err)
	}
	runtime.markAdmitted(key)
	ctx, cancel := context.WithCancel(context.Background())
	type beginResult struct {
		ticket geometryTicket
		err    error
	}
	result := make(chan beginResult, 1)
	go func() {
		ticket, err := issuer.BeginGeometry(ctx)
		result <- beginResult{ticket: ticket, err: err}
	}()
	waitGeometryOwnerState(t, runtime, key, true, 6)
	cancel()
	select {
	case <-cancelObserved:
	case <-time.After(5 * time.Second):
		t.Fatal("BeginGeometry did not reach cancellation settled-race edge")
	}

	journalMu.Unlock()
	locked = false
	deadline := time.Now().Add(5 * time.Second)
	for {
		registry.mu.Lock()
		sticky := registry.broken || registry.failedIncarnationLocked(witness)
		registry.mu.Unlock()
		if sticky && counts.get("boundary_error") > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pause failure did not settle before cancellation probe")
		}
		time.Sleep(time.Millisecond)
	}
	close(allowProbe)
	got := <-result
	if got.ticket != nil || got.err == nil || terminal.IsResizeRefusal(got.err) {
		t.Fatalf("settled failure ticket=%T err=%v, want fatal generation verdict", got.ticket, got.err)
	}
	waitGeometryOwnerState(t, runtime, key, false, 0)
	if counts.get("pause_end_complete") != 0 {
		t.Fatalf("settled failed pause unexpectedly enqueued pause_end: count=%d", counts.get("pause_end_complete"))
	}
}

func waitGeometryOwnerState(t *testing.T, runtime *retentionTrialRuntime, key unifiedjournal.PaneKey, owned bool, minQ int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.mu.Lock()
		owner := runtime.geometryOwners[key]
		used := runtime.qUsed
		runtime.mu.Unlock()
		if (owner != 0) == owned && used >= minQ {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("geometry owner state owned=%v qUsed>=%d: owner=%d qUsed=%d", owned, minQ, owner, used)
		}
		time.Sleep(time.Millisecond)
	}
}

// Multiple attachment issuers race the same pane each round. Exactly one may
// own the pause lifecycle; every other contender is refused before reserving a
// slot. Alternating rounds cancel that owner at the queue/ack edge or let it
// return a ticket, proving both paths pair start/end, preserve held output, and
// restore the generation floor.
func TestConcurrentSamePaneGeometryOwnerStress(t *testing.T) {
	const (
		rounds     = 18
		contenders = 12
	)
	h := newGeometryBarrierHarness(t, "same-pane-owner-stress")
	runtime := h.registry.retention
	expected := ""

	for round := 0; round < rounds; round++ {
		h.journalMu.Lock()
		pre := fmt.Sprintf("r%d-pre;", round)
		if err := runtime.WritePane(h.key, []byte(pre)); err != nil {
			h.journalMu.Unlock()
			t.Fatalf("round %d pre write: %v", round, err)
		}

		type result struct {
			ticket geometryTicket
			err    error
		}
		start := make(chan struct{})
		results := make(chan result, contenders)
		cancels := make([]context.CancelFunc, contenders)
		for contender := 0; contender < contenders; contender++ {
			ctx, cancel := context.WithCancel(context.Background())
			cancels[contender] = cancel
			issuer := &unifiedGeometryIssuer{provider: h.provider, registry: h.registry, session: "$0"}
			go func() {
				<-start
				ticket, err := issuer.BeginGeometry(ctx)
				results <- result{ticket: ticket, err: err}
			}()
		}
		close(start)
		waitGeometryOwnerState(t, runtime, h.key, true, 6)

		for loser := 0; loser < contenders-1; loser++ {
			select {
			case got := <-results:
				if got.ticket != nil || !terminal.IsResizeRefusal(got.err) || !errors.Is(got.err, errGeometryPauseOwned) {
					h.journalMu.Unlock()
					t.Fatalf("round %d loser %d ticket=%T err=%v, want owner refusal", round, loser, got.ticket, got.err)
				}
			case <-time.After(5 * time.Second):
				h.journalMu.Unlock()
				t.Fatalf("round %d did not refuse %d concurrent contenders", round, contenders-1)
			}
		}

		if round%2 == 0 {
			for _, cancel := range cancels {
				cancel()
			}
			got := <-results
			if got.ticket != nil || !terminal.IsResizeRefusal(got.err) || errors.Is(got.err, errGeometryPauseOwned) {
				h.journalMu.Unlock()
				t.Fatalf("round %d owner cancellation ticket=%T err=%v", round, got.ticket, got.err)
			}
			h.journalMu.Unlock()
			expected += pre
		} else {
			h.journalMu.Unlock()
			got := <-results
			if got.err != nil || got.ticket == nil {
				t.Fatalf("round %d winning begin ticket=%T err=%v", round, got.ticket, got.err)
			}
			expected += pre
			h.down.awaitOutput(t, fmt.Sprintf("round %d pre", round), expected)
			held := fmt.Sprintf("r%d-held;", round)
			if err := runtime.WritePane(h.key, []byte(held)); err != nil {
				t.Fatalf("round %d held write: %v", round, err)
			}
			if err := runtime.Boundary(h.key, "owner_stress_probe"); err != nil {
				t.Fatalf("round %d probe: %v", round, err)
			}
			h.down.requireOutputStaysAt(t, fmt.Sprintf("round %d barrier", round), expected, 20*time.Millisecond)
			got.ticket.Release()
			expected += held
		}
		for _, cancel := range cancels {
			cancel()
		}
		h.down.awaitOutput(t, fmt.Sprintf("round %d release", round), expected)

		deadline := time.Now().Add(5 * time.Second)
		for {
			runtime.mu.Lock()
			used := runtime.qUsed
			owner := runtime.geometryOwners[h.key]
			pane := runtime.panes[h.key]
			paused := pane != nil && pane.paused
			runtime.mu.Unlock()
			if used == 2 && owner == 0 && !paused {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d did not settle: qUsed=%d owner=%d paused=%v", round, used, owner, paused)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// Many panes on one runtime, each cycling Fits whose contexts cancel at random
// points around the pause_start queue/ack edge, with output racing every
// barrier and the shared journal lock intermittently held to widen the
// unacknowledged window. Terminal obligations: every pane publishes everything
// it wrote in exactly write order (so no pane ends paused and no held output
// leaks mid-barrier), every committed geometry precedes the output produced
// under its barrier, applied pauses and completed releases balance, and the
// runtime returns to its per-generation slot floor.
func TestCanceledBeginGeometryStressKeepsPauseOwnership(t *testing.T) {
	const (
		stressPanes      = 6
		stressIterations = 24
	)
	realm := openRetentionRealm(t, "cancel-stress")
	downstream := &cancelStressDownstream{events: make(map[unifiedjournal.PaneKey][]unifiedjournal.Event)}
	counts := &cancelStressCounts{counts: make(map[string]int)}
	var journalMu sync.Mutex
	runtime := newRetentionTrialRuntime(cancelStressEffects{
		realm: realm, journalMu: &journalMu, downstream: downstream, counts: counts,
	})
	if runtime == nil {
		t.Fatal("retention runtime unavailable")
	}
	t.Cleanup(func() { _ = runtime.Close() })

	provider := &UnifiedDevPaneEffects{
		realm: realm, active: make(map[string]unifiedjournal.PaneKey),
		subscribers: make(map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}),
	}
	registry := &paneRegistry{closeDone: make(chan struct{})}
	registry.retention = runtime
	keys := make([]unifiedjournal.PaneKey, stressPanes)
	issuers := make([]*unifiedGeometryIssuer, stressPanes)
	for pane := 0; pane < stressPanes; pane++ {
		session := fmt.Sprintf("$%d", pane)
		keys[pane] = journalKey(retentionWitness(fmt.Sprintf("%%%d", pane), fmt.Sprintf("cancel-stress-%d", pane)))
		if err := realm.AdmitPane(keys[pane], unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
			t.Fatalf("admit pane %d: %v", pane, err)
		}
		provider.active[session] = keys[pane]
		issuers[pane] = &unifiedGeometryIssuer{provider: provider, registry: registry, session: session}
	}

	// The jitter holder recreates the stranded-pause window under load: while
	// it owns the journal lock, every queued pause_start sits consumed by the
	// manager but unacknowledged, which is exactly where cancellations must not
	// abandon release ownership.
	stopJitter := make(chan struct{})
	var jitter sync.WaitGroup
	jitter.Add(1)
	go func() {
		defer jitter.Done()
		for {
			select {
			case <-stopJitter:
				return
			default:
			}
			journalMu.Lock()
			time.Sleep(time.Duration(200+rand.Intn(1800)) * time.Microsecond)
			journalMu.Unlock()
			time.Sleep(time.Duration(rand.Intn(500)) * time.Microsecond)
		}
	}()

	expected := make([]strings.Builder, stressPanes)
	var drivers sync.WaitGroup
	for pane := 0; pane < stressPanes; pane++ {
		drivers.Add(1)
		go func(pane int) {
			defer drivers.Done()
			issuer := issuers[pane]
			key := keys[pane]
			write := func(chunk string) bool {
				if err := runtime.WritePane(key, []byte(chunk)); err != nil {
					t.Errorf("pane %d write %q: %v", pane, chunk, err)
					return false
				}
				expected[pane].WriteString(chunk)
				return true
			}
			for iteration := 0; iteration < stressIterations; iteration++ {
				if !write(fmt.Sprintf("p%d-i%d-pre;", pane, iteration)) {
					return
				}
				if iteration%3 == 2 {
					// A full commit cycle: the geometry record must land ahead
					// of the output produced under its barrier, even right
					// after canceled attempts on the same pane.
					ticket, err := issuer.BeginGeometry(context.Background())
					if err != nil {
						t.Errorf("pane %d iteration %d begin: %v", pane, iteration, err)
						return
					}
					if !write(fmt.Sprintf("p%d-i%d-mid;", pane, iteration)) {
						ticket.Release()
						return
					}
					if err := ticket.Commit(context.Background(), 80, 24+iteration%16); err != nil {
						t.Errorf("pane %d iteration %d commit: %v", pane, iteration, err)
						return
					}
					continue
				}
				// A cancellation racing the pause_start queue/ack edge. Either
				// the cancel won — typed refusal, no ticket, and the pairing
				// below still ends the pause — or the begin won and the normal
				// deferred Release path runs.
				ctx, cancel := context.WithCancel(context.Background())
				go func() {
					time.Sleep(time.Duration(rand.Intn(1500)) * time.Microsecond)
					cancel()
				}()
				ticket, err := issuer.BeginGeometry(ctx)
				if err != nil {
					if !terminal.IsResizeRefusal(err) {
						t.Errorf("pane %d iteration %d: canceled begin returned untyped error: %v", pane, iteration, err)
						cancel()
						return
					}
				} else {
					if !write(fmt.Sprintf("p%d-i%d-mid;", pane, iteration)) {
						ticket.Release()
						cancel()
						return
					}
					ticket.Release()
				}
				cancel()
			}
			write(fmt.Sprintf("p%d-final", pane))
		}(pane)
	}
	drivers.Wait()
	close(stopJitter)
	jitter.Wait()

	// No pane may end paused: everything written must publish, in write order.
	deadline := time.Now().Add(15 * time.Second)
	for pane := 0; pane < stressPanes; pane++ {
		want := expected[pane].String()
		for downstream.outputText(keys[pane]) != want {
			if time.Now().After(deadline) {
				t.Fatalf("pane %d ended stranded: published=%q want=%q", pane, downstream.outputText(keys[pane]), want)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	// Every committed geometry must precede the output produced under its
	// barrier — a late pause_end from a canceled begin resuming publication in
	// the middle of a later Fit would publish the mid chunk first.
	for pane := 0; pane < stressPanes; pane++ {
		events := downstream.snapshot(keys[pane])
		for iteration := 2; iteration < stressIterations; iteration += 3 {
			rows := 24 + iteration%16
			geometryIndex, midIndex := -1, -1
			for index, event := range events {
				if event.Kind == unifiedjournal.RecordGeometry && event.Geometry.Rows == rows && geometryIndex < 0 {
					geometryIndex = index
				}
				if event.Kind == unifiedjournal.RecordOutput &&
					string(event.Payload) == fmt.Sprintf("p%d-i%d-mid;", pane, iteration) {
					midIndex = index
				}
			}
			if geometryIndex < 0 || midIndex < 0 || geometryIndex > midIndex {
				t.Fatalf("pane %d iteration %d: geometry did not precede its barrier's output: geometry=%d mid=%d",
					pane, iteration, geometryIndex, midIndex)
			}
		}
	}

	// Slot balance and pause pairing: the runtime returns to its generation
	// floor and every applied pause_start was completed by exactly one
	// pause_end.
	deadline = time.Now().Add(10 * time.Second)
	for {
		runtime.mu.Lock()
		used := runtime.qUsed
		runtime.mu.Unlock()
		starts, ends := counts.get("pause_start"), counts.get("pause_end_complete")
		if used == 2*stressPanes && starts == ends {
			if starts == 0 {
				t.Fatal("stress never applied a pause")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slots or pauses did not settle: qUsed=%d want=%d pause_start=%d pause_end_complete=%d",
				used, 2*stressPanes, starts, ends)
		}
		time.Sleep(2 * time.Millisecond)
	}
	for _, event := range []string{"boundary_error", "storage_fault", "geometry_fault"} {
		if count := counts.get(event); count != 0 {
			t.Fatalf("stress observed %d %s events", count, event)
		}
	}
}
