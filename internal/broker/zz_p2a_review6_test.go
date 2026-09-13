package broker

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/unifiedjournal"
)

func TestP2AReview6HistoricalDisconnectRequiresRouterEligibility(t *testing.T) {
	fixture := newP2AFixture(t)
	previous := fixture.previous
	fixture.review4Die()
	current := fixture.review4Readopt(t, previous, 2)
	// review4Die publishes its Disconnect through the asynchronous retention
	// command plane. Quiesce that fixture-owned command before taking the R14
	// ledger baseline; otherwise its nine E credits can settle between the two
	// snapshots and masquerade as mutation by the historical Disconnect below.
	pollUntil(t, 5*time.Second, "prior Disconnect command settlement", func() bool {
		fixture.registry.retention.mu.Lock()
		defer fixture.registry.retention.mu.Unlock()
		return fixture.registry.retention.eUsed == 0
	})

	// Unit death retains immutable history. Make the historical witness the
	// exact retained admission, as it can be while the readopted router owns the
	// only eligible route. Exact-witness equality alone must not authorize it.
	fixture.registry.mu.Lock()
	coordinate := routeCoordinateKey(previous)
	retained := fixture.registry.admitted[coordinate]
	retained.witness = previous
	fixture.registry.admitted[coordinate] = retained
	session := fixture.registry.sessions[sessionKey(current.Session)]
	oldEligible := session != nil && session.router.UnifiedEligible(previous)
	currentEligible := session != nil && session.router.UnifiedEligible(current)
	fixture.registry.mu.Unlock()
	if oldEligible || !currentEligible {
		t.Fatalf("historical eligibility old=%v current=%v want false/true", oldEligible, currentEligible)
	}

	var baseline p2aReview3Ledger
	deadline := time.Now().Add(2 * time.Second)
	for {
		baseline = p2aReview3RuntimeLedger(fixture.registry.retention)
		if baseline.e == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("historical fixture did not quiesce: %+v", baseline)
		}
		time.Sleep(time.Millisecond)
	}
	fixture.registry.retention.mu.Lock()
	historicalBefore := fixture.registry.retention.generations[journalKey(previous)]
	historicalRefs := 0
	if historicalBefore != nil {
		historicalRefs = historicalBefore.refs
	}
	fixture.registry.retention.mu.Unlock()
	var reserves atomic.Int64
	fixture.registry.retention.setHook(func(point string, key unifiedjournal.PaneKey) {
		if point == "after_ingress_reserve" && key == journalKey(previous) {
			reserves.Add(1)
		}
	})
	if err := fixture.registry.ObservePane(controlmode.Observation{
		Kind: controlmode.ObservationDisconnect, Witness: previous,
	}); err != nil {
		t.Fatal(err)
	}
	if got := reserves.Load(); got != 0 {
		t.Fatalf("historical Disconnect reserved %d times want 0", got)
	}
	if got := p2aReview3RuntimeLedger(fixture.registry.retention); got != baseline {
		t.Fatalf("historical Disconnect ledger=%+v want %+v", got, baseline)
	}
	fixture.registry.retention.mu.Lock()
	historicalAfter := fixture.registry.retention.generations[journalKey(previous)]
	afterRefs := 0
	if historicalAfter != nil {
		afterRefs = historicalAfter.refs
	}
	fixture.registry.retention.mu.Unlock()
	if historicalAfter != historicalBefore || afterRefs != historicalRefs {
		t.Fatalf("historical Disconnect mutated generation before=%p/%d after=%p/%d", historicalBefore, historicalRefs, historicalAfter, afterRefs)
	}
}

func TestP2AReview6SnapshotReadDoesNotHoldSubscriberAuthority(t *testing.T) {
	fixture := newP2AFixture(t)
	key := journalKey(fixture.previous)
	entered := make(chan struct{})
	var once sync.Once
	fixture.effects.snapshotReadEdge = func() { once.Do(func() { close(entered) }) }
	defer func() { fixture.effects.snapshotReadEdge = nil }()

	fixture.effects.journalMu.Lock()
	type openResult struct {
		subscriber *unifiedDevSubscriber
		cancel     func()
		err        error
	}
	opened := make(chan openResult, 1)
	go func() {
		_, _, subscriber, cancel, err := openSnapshotTailForTest(t, fixture.effects, fixture.unit.sessionID)
		opened <- openResult{subscriber: subscriber, cancel: cancel, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		fixture.effects.journalMu.Unlock()
		t.Fatal("snapshot read did not reach blocked journal fence")
	}
	if !fixture.effects.subscriberMu.TryLock() {
		fixture.effects.journalMu.Unlock()
		t.Fatal("snapshot storage read held subscriberMu")
	}
	fixture.effects.subscriberMu.Unlock()

	// Exercise publication and typed close on a sibling subscriber. P2b tracks
	// the publication head for every key, so injecting an unjournaled event into
	// the candidate key would correctly force its registration retry forever;
	// the sibling proves the realm-wide subscriber authority remains live
	// without constructing that impossible journal/publication state.
	sideKey := key
	sideKey.Pane = "%review6-side"
	sideSubscriber := &unifiedDevSubscriber{data: make(chan unifiedjournal.Event, 1), done: make(chan struct{})}
	fixture.effects.subscriberMu.Lock()
	fixture.effects.subscribers[sideKey] = map[*unifiedDevSubscriber]struct{}{sideSubscriber: {}}
	fixture.effects.subscriberMu.Unlock()
	event := unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Sequence: 1, Start: 0, End: 1, Payload: []byte("x")}
	if err := fixture.effects.publishEvent(sideKey, event); err != nil {
		fixture.effects.journalMu.Unlock()
		t.Fatal(err)
	}
	select {
	case got, openForRelease := <-sideSubscriber.events():
		if openForRelease {
			sideSubscriber.releaseEvent(got)
		}
		if !bytes.Equal(got.Payload, event.Payload) {
			fixture.effects.journalMu.Unlock()
			t.Fatalf("published payload=%q want %q", got.Payload, event.Payload)
		}
	case <-time.After(2 * time.Second):
		fixture.effects.journalMu.Unlock()
		t.Fatal("live publication blocked behind snapshot storage read")
	}
	if closed := fixture.effects.closeSubscribers(sideKey, proto.SubscriberClosedLagged); closed != 1 {
		fixture.effects.journalMu.Unlock()
		t.Fatalf("typed close count=%d want 1", closed)
	}
	if _, open := <-sideSubscriber.events(); open {
		fixture.effects.journalMu.Unlock()
		t.Fatal("subscriber remained open")
	}
	if got := sideSubscriber.closeReason(); got != proto.SubscriberClosedLagged {
		fixture.effects.journalMu.Unlock()
		t.Fatalf("close reason=%q", got)
	}

	fixture.effects.journalMu.Unlock()
	select {
	case result := <-opened:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.cancel != nil {
			result.cancel()
		}
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot read did not resume")
	}
}

func TestP2AReview6SnapshotRegistrationPublishesConcurrentCommitExactlyOnce(t *testing.T) {
	fixture := newP2AFixture(t)
	marker := []byte("review6-after-snapshot")
	started := make(chan struct{})
	finished := make(chan error, 1)
	var once sync.Once
	fixture.effects.snapshotRegisterEdge = func() {
		once.Do(func() {
			close(started)
			go func() {
				err := fixture.registry.ObservePane(controlmode.Observation{
					Kind: controlmode.ObservationOutput, Witness: fixture.previous, Data: marker,
				})
				if err == nil {
					err = fixture.registry.retentionBoundary(fixture.previous, "review6_exact_once")
				}
				finished <- err
			}()
		})
	}
	defer func() { fixture.effects.snapshotRegisterEdge = nil }()

	events, _, subscriber, cancel, err := openSnapshotTailForTest(t, fixture.effects, fixture.unit.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	select {
	case <-started:
	default:
		t.Fatal("registration seam did not run")
	}
	for _, event := range events {
		if bytes.Equal(event.Payload, marker) {
			t.Fatal("concurrent post-snapshot commit appeared in snapshot")
		}
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent commit did not finish")
	}
	seen := 0
	deadline := time.After(2 * time.Second)
	for seen == 0 {
		select {
		case event, openForRelease := <-subscriber.events():
			if openForRelease {
				subscriber.releaseEvent(event)
			}
			if bytes.Equal(event.Payload, marker) {
				seen++
			}
		case <-deadline:
			t.Fatal("concurrent commit missing from live tail")
		}
	}
	select {
	case event, openForRelease := <-subscriber.events():
		if openForRelease {
			subscriber.releaseEvent(event)
		}
		if bytes.Equal(event.Payload, marker) {
			t.Fatal("concurrent commit duplicated in live tail")
		}
	case <-time.After(25 * time.Millisecond):
	}
}

func TestP2AReview6SnapshotRetriesAcrossRotationCommit(t *testing.T) {
	fixture := newP2AFixture(t)
	rotation := p2aReview5PrepareRotation(t, fixture)
	entered := make(chan struct{})
	var once sync.Once
	fixture.effects.snapshotReadEdge = func() { once.Do(func() { close(entered) }) }
	defer func() { fixture.effects.snapshotReadEdge = nil }()

	fixture.effects.journalMu.Lock()
	type openResult struct {
		events     []unifiedjournal.Event
		subscriber *unifiedDevSubscriber
		cancel     func()
		err        error
	}
	opened := make(chan openResult, 1)
	go func() {
		events, _, subscriber, cancel, err := openSnapshotTailForTest(t, fixture.effects, fixture.unit.sessionID)
		opened <- openResult{events: events, subscriber: subscriber, cancel: cancel, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		fixture.effects.journalMu.Unlock()
		t.Fatal("snapshot did not enter old-key read")
	}
	if disposition := rotation.txn.Commit(); disposition != paneRotationCommitted {
		fixture.effects.journalMu.Unlock()
		t.Fatalf("rotation disposition=%d", disposition)
	}
	fixture.effects.journalMu.Unlock()

	var result openResult
	select {
	case result = <-opened:
		if result.err != nil {
			t.Fatal(result.err)
		}
		defer result.cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot did not retry after active-key swap")
	}
	foundBootstrap := false
	for _, event := range result.events {
		if bytes.Equal(event.Payload, []byte("review4-bootstrap")) {
			foundBootstrap = true
		}
	}
	if !foundBootstrap {
		t.Fatal("retried snapshot did not come from successor")
	}
	fixture.effects.subscriberMu.Lock()
	_, oldRegistered := fixture.effects.subscribers[journalKey(fixture.previous)][result.subscriber]
	_, newRegistered := fixture.effects.subscribers[journalKey(rotation.next)][result.subscriber]
	fixture.effects.subscriberMu.Unlock()
	if oldRegistered || !newRegistered {
		t.Fatalf("snapshot registration old=%v new=%v want false/true", oldRegistered, newRegistered)
	}
	fixture.effects.journalMu.Lock()
	rotation.reservation.Commit()
	fixture.effects.journalMu.Unlock()
}
