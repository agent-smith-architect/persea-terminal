package broker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

// Optional captured traces supplement the self-contained synthetic workloads.
var retentionEvidenceRoot = os.Getenv("PERSEA_RETENTION_EVIDENCE_ROOT")

const retentionFixtureSHA256 = "d62af25de3abd49f6ac6400cd4aba1af353b5f17e1bf78efb359e882b383112b"

type retentionWorkloadFile struct {
	Schema               string              `json:"schema"`
	SourceManifestSHA256 string              `json:"source_manifest_sha256"`
	Workloads            []retentionWorkload `json:"workloads"`
}

type retentionWorkload struct {
	ID                    string `json:"id"`
	SourceKind            string `json:"source_kind"`
	PayloadBytes          int    `json:"payload_bytes"`
	PayloadSHA256         string `json:"payload_sha256"`
	ChunkCount            int    `json:"chunk_count"`
	OrderedScheduleSHA256 string `json:"ordered_schedule_sha256"`
	IntervalNS            int64  `json:"interval_ns"`
	Records64K            int    `json:"records_64k"`
	Lengths64KSHA256      string `json:"lengths_64k_sha256"`
	Records256K           int    `json:"records_256k"`
	Lengths256KSHA256     string `json:"lengths_256k_sha256"`
}

type retentionCrashScenario struct {
	RuntimeDir string `json:"runtime_dir"`
	MarkerPath string `json:"marker_path"`
	Cut        string `json:"cut"`
	PayloadHex string `json:"payload_hex"`
}

type retentionTimer struct {
	at       time.Time
	fn       func()
	canceled bool
}

type retentionManualClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*retentionTimer
	changed chan struct{}
}

func newRetentionManualClock() *retentionManualClock {
	return &retentionManualClock{now: time.Unix(1_700_000_000, 0), changed: make(chan struct{}, 1)}
}

func (clock *retentionManualClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *retentionManualClock) After(delay time.Duration, fn func()) func() bool {
	clock.mu.Lock()
	timer := &retentionTimer{at: clock.now.Add(delay), fn: fn}
	clock.timers = append(clock.timers, timer)
	clock.mu.Unlock()
	select {
	case clock.changed <- struct{}{}:
	default:
	}
	return func() bool {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		if timer.canceled {
			return false
		}
		timer.canceled = true
		return true
	}
}

func (clock *retentionManualClock) waitTimerCount(count int) {
	for {
		clock.mu.Lock()
		reached := len(clock.timers) >= count
		clock.mu.Unlock()
		if reached {
			return
		}
		<-clock.changed
	}
}

func (clock *retentionManualClock) Advance(delta time.Duration) {
	target := clock.Now().Add(delta)
	for {
		clock.mu.Lock()
		var next *retentionTimer
		for _, timer := range clock.timers {
			if !timer.canceled && !timer.at.After(target) && (next == nil || timer.at.Before(next.at)) {
				next = timer
			}
		}
		if next == nil {
			clock.now = target
			clock.mu.Unlock()
			return
		}
		next.canceled = true
		clock.now = next.at
		clock.mu.Unlock()
		next.fn()
	}
}

type retentionShadowEffects struct {
	mu                 sync.Mutex
	run                func(context.Context, PaneObservationEffects) error
	realm              *unifiedjournal.Realm
	maxBytes           int
	maxDelay           time.Duration
	clock              *retentionManualClock
	beforeWrite        func()
	writeHook          func(unifiedjournal.PaneKey, []byte) error
	beforeObserve      func(string)
	hook               func(string, unifiedjournal.PaneKey)
	stage              func(string, unifiedjournal.PaneKey) error
	maxPanes           int
	maxCommands        int
	maxEnvelopes       int
	maxIngressBytes    int64
	maxOutboxBytes     int64
	feedChanged        chan struct{}
	observationChanged chan struct{}
	feeds              [][]byte
	feedKeys           []unifiedjournal.PaneKey
	observations       []string
	fields             []map[string]int64
}

type retentionBlockingPaneJournal struct {
	once     sync.Once
	started  chan struct{}
	release  chan struct{}
	delegate paneJournalEffect
}

func (journal *retentionBlockingPaneJournal) WritePane(key unifiedjournal.PaneKey, payload []byte) error {
	journal.once.Do(func() { close(journal.started) })
	<-journal.release
	return journal.delegate.WritePane(key, payload)
}

func (effects *retentionShadowEffects) RunObserver(ctx context.Context, observer PaneObservationEffects) error {
	return effects.run(ctx, observer)
}

func (effects *retentionShadowEffects) WritePane(key unifiedjournal.PaneKey, payload []byte) error {
	if effects.beforeWrite != nil {
		effects.beforeWrite()
	}
	if effects.writeHook != nil {
		if err := effects.writeHook(key, payload); err != nil {
			return err
		}
	}
	effects.mu.Lock()
	defer effects.mu.Unlock()
	effects.feedKeys = append(effects.feedKeys, key)
	effects.feeds = append(effects.feeds, append([]byte(nil), payload...))
	if effects.feedChanged != nil {
		select {
		case effects.feedChanged <- struct{}{}:
		default:
		}
	}
	return nil
}

func (effects *retentionShadowEffects) waitFeedCount(count int) {
	for {
		effects.mu.Lock()
		reached := len(effects.feeds) >= count
		changed := effects.feedChanged
		effects.mu.Unlock()
		if reached {
			return
		}
		<-changed
	}
}

func TestRetentionR5OrdinaryIngressReturnsBeforeBlockedFeed(t *testing.T) {
	realm := openRetentionRealm(t, "r5-bounded-ingress")
	clock := newRetentionManualClock()
	feedEntered := make(chan struct{})
	feedRelease := make(chan struct{})
	var once sync.Once
	effects := &retentionShadowEffects{
		realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock,
		beforeWrite: func() {
			once.Do(func() { close(feedEntered) })
			<-feedRelease
		},
	}
	registry := newPaneRegistry(effects)
	witness := retentionWitness("%r5-bounded", "r5-bounded-inc")
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	decoder := controlmode.NewDecoder()
	returned := make(chan error, 1)
	go func() {
		returned <- feedRetentionOutput(registry, decoder, witness, bytes.Repeat([]byte{'b'}, 64<<10))
	}()

	<-feedEntered
	if err := <-returned; err != nil {
		close(feedRelease)
		t.Fatal(err)
	}
	close(feedRelease)
	if err := registry.retention.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionR5SameCoordinateCallbackReentryIsLegal(t *testing.T) {
	testRetentionR5CallbackReentry(t, true)
}

func TestRetentionR5DifferentCoordinateCallbackReentryIsLegal(t *testing.T) {
	testRetentionR5CallbackReentry(t, false)
}

func testRetentionR5CallbackReentry(t *testing.T, sameCoordinate bool) {
	t.Helper()
	realm := openRetentionRealm(t, "r5-callback-reentry")
	clock := newRetentionManualClock()
	effects := &retentionShadowEffects{
		realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock,
	}
	registry := newPaneRegistry(effects)
	original := retentionWitness("%r5-reentry-a", "r5-reentry-a-inc")
	reentrant := original
	if !sameCoordinate {
		reentrant = retentionWitness("%r5-reentry-b", "r5-reentry-b-inc")
	}
	witnesses := []controlmode.PaneWitness{original}
	if !sameCoordinate {
		witnesses = append(witnesses, reentrant)
	}
	for _, witness := range witnesses {
		if err := registry.AdmitPane(witness); err != nil {
			t.Fatal(err)
		}
	}
	reentered := make(chan struct{})
	var once sync.Once
	effects.writeHook = func(unifiedjournal.PaneKey, []byte) error {
		var err error
		once.Do(func() {
			err = registry.ObservePane(controlmode.Observation{
				Witness: reentrant, Kind: controlmode.ObservationOutput, Data: []byte("reentered"),
			})
			close(reentered)
		})
		return err
	}
	decoder := controlmode.NewDecoder()
	returned := make(chan error, 1)
	go func() {
		returned <- feedRetentionOutput(registry, decoder, original, bytes.Repeat([]byte{'r'}, 64<<10))
	}()
	<-reentered
	if err := <-returned; err != nil {
		t.Fatal(err)
	}
	effects.writeHook = nil
	if err := registry.retention.Close(); err != nil {
		t.Fatal(err)
	}
	effects.mu.Lock()
	defer effects.mu.Unlock()
	if len(effects.feeds) != 2 || string(effects.feeds[1]) != "reentered" {
		t.Fatalf("callback reentry feeds = %q", effects.feeds)
	}
}

func TestRetentionR5SequentialPauseAndDurableEndFence(t *testing.T) {
	realm := openRetentionRealm(t, "r5-sequential-pause")
	clock := newRetentionManualClock()
	appendEntered := make(chan struct{})
	appendRelease := make(chan struct{})
	var appendOnce sync.Once
	effects := &retentionShadowEffects{
		realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock,
		stage: func(operation string, _ unifiedjournal.PaneKey) error {
			if operation == "append" {
				appendOnce.Do(func() {
					close(appendEntered)
					<-appendRelease
				})
			}
			return nil
		},
	}
	registry := newPaneRegistry(effects)
	witness := retentionWitness("%r5-pause", "r5-pause-inc")
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	if err := registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: witness, Label: "start"}); err != nil {
		t.Fatal(err)
	}
	if err := feedRetentionOutput(registry, controlmode.NewDecoder(), witness, []byte("held")); err != nil {
		t.Fatal(err)
	}
	effects.mu.Lock()
	feedsBeforeEnd := len(effects.feeds)
	effects.mu.Unlock()
	if feedsBeforeEnd != 0 {
		t.Fatalf("paused output reached downstream before pause-end fence: %d feeds", feedsBeforeEnd)
	}
	endDone := make(chan error, 1)
	go func() {
		endDone <- registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: witness, Label: "end"})
	}()
	<-appendEntered
	var earlyErr error
	early := false
	for attempt := 0; attempt < 10000 && !early; attempt++ {
		select {
		case earlyErr = <-endDone:
			early = true
		default:
			runtime.Gosched()
		}
	}
	if early {
		t.Fatalf("pause end returned before append/sync/commit fence: %v", earlyErr)
	}
	close(appendRelease)
	if err := <-endDone; err != nil {
		t.Fatal(err)
	}
	effects.waitObservation("pause_end_complete", 1)
	effects.mu.Lock()
	feedsAfterEnd := append([][]byte(nil), effects.feeds...)
	effects.mu.Unlock()
	if len(feedsAfterEnd) != 1 || string(feedsAfterEnd[0]) != "held" {
		t.Fatalf("pause-end durable fence feeds = %q", feedsAfterEnd)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionR5CompoundFeedFIFOAndQuiescentIdempotentClose(t *testing.T) {
	realm := openRetentionRealm(t, "r5-compound-close")
	clock := newRetentionManualClock()
	feedEntered := make(chan struct{})
	feedRelease := make(chan struct{})
	var blockFirst sync.Once
	effects := &retentionShadowEffects{
		realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock,
		beforeWrite: func() {
			blockFirst.Do(func() {
				close(feedEntered)
				<-feedRelease
			})
		},
	}
	registry := newPaneRegistry(effects)
	witness := retentionWitness("%r5-close", "r5-close-inc")
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	decoder := controlmode.NewDecoder()
	if err := feedRetentionOutput(registry, decoder, witness, bytes.Repeat([]byte{'f'}, 64<<10)); err != nil {
		t.Fatal(err)
	}
	<-feedEntered
	if err := feedRetentionOutput(registry, decoder, witness, []byte("g")); err != nil {
		t.Fatal(err)
	}
	closeResults := make(chan error, 4)
	for index := 0; index < cap(closeResults); index++ {
		go func() { closeResults <- registry.Close() }()
	}
	select {
	case err := <-closeResults:
		t.Fatalf("Close returned before claimed feed callback completed: %v", err)
	default:
	}
	close(feedRelease)
	for index := 0; index < cap(closeResults); index++ {
		if err := <-closeResults; err != nil {
			t.Fatal(err)
		}
	}
	effects.mu.Lock()
	feeds := append([][]byte(nil), effects.feeds...)
	var causal []string
	for _, event := range effects.observations {
		switch event {
		case "feed_start", "feed_complete", "storage_fault", "close":
			causal = append(causal, event)
		}
	}
	effects.mu.Unlock()
	if len(feeds) != 2 || len(feeds[0]) != 64<<10 || string(feeds[1]) != "g" {
		t.Fatalf("compound feed order = (%d, %q)", len(feeds[0]), feeds[1])
	}
	want := []string{"feed_start", "feed_complete", "feed_start", "feed_complete", "close"}
	if fmt.Sprint(causal) != fmt.Sprint(want) {
		t.Fatalf("compound callback FIFO = %v, want %v", causal, want)
	}
	if err := feedRetentionOutput(registry, controlmode.NewDecoder(), witness, []byte("late")); !errors.Is(err, unifiedjournal.ErrInvalidated) {
		t.Fatalf("post-Close ingress = %v, want ErrInvalidated", err)
	}
}

func TestRetentionR5PublishedPrefixAtFaultCutoffIsAttemptedOnce(t *testing.T) {
	realm := openRetentionRealm(t, "r5-published-cutoff")
	clock := newRetentionManualClock()
	firstEntered := make(chan struct{})
	firstRelease := make(chan struct{})
	releaseFirst := sync.OnceFunc(func() { close(firstRelease) })
	t.Cleanup(releaseFirst)
	secondPublished := make(chan struct{})
	var writeCount int
	var writeMu sync.Mutex
	var publishCount int
	var publishMu sync.Mutex
	effects := &retentionShadowEffects{
		realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock,
		writeHook: func(_ unifiedjournal.PaneKey, _ []byte) error {
			writeMu.Lock()
			writeCount++
			count := writeCount
			writeMu.Unlock()
			if count == 1 {
				close(firstEntered)
				<-firstRelease
				return errors.New("r5 first claimed feed failure")
			}
			return nil
		},
		hook: func(point string, _ unifiedjournal.PaneKey) {
			if point != "after_feed_publish" {
				return
			}
			publishMu.Lock()
			publishCount++
			if publishCount == 2 {
				close(secondPublished)
			}
			publishMu.Unlock()
		},
	}
	registry := newPaneRegistry(effects)
	witness := retentionWitness("%r5-cutoff", "r5-cutoff-inc")
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	decoder := controlmode.NewDecoder()
	if err := feedRetentionOutput(registry, decoder, witness, bytes.Repeat([]byte{'f'}, 64<<10)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first feed did not enter the write hook")
	}
	if err := feedRetentionOutput(registry, decoder, witness, bytes.Repeat([]byte{'g'}, 64<<10)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondPublished:
	case <-time.After(5 * time.Second):
		t.Fatal("second feed was not published while the first was blocked")
	}
	releaseFirst()
	// Close drains accepted work, so no unbounded feed wait can conceal a
	// missing attempt. The fault must remain visible after the drain.
	closed := make(chan error, 1)
	go func() { closed <- registry.Close() }()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("Close lost claimed feed failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not drain the published prefix")
	}
	effects.mu.Lock()
	feeds := append([][]byte(nil), effects.feeds...)
	effects.mu.Unlock()
	if len(feeds) != 1 || !bytes.Equal(feeds[0], bytes.Repeat([]byte{'g'}, 64<<10)) {
		t.Fatalf("published <=H feed attempts = %d/%q, want g exactly once", len(feeds), feeds)
	}
}

func TestRetentionR5SaturatedOrdinaryCapacityCannotStarveClose(t *testing.T) {
	realm := openRetentionRealm(t, "r5-saturated-close")
	clock := newRetentionManualClock()
	feedEntered := make(chan struct{})
	feedRelease := make(chan struct{})
	var once sync.Once
	effects := &retentionShadowEffects{
		realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock,
		maxPanes: 1, maxCommands: 3, maxEnvelopes: retentionEnvelopeNeed(64<<10, 64<<10),
		beforeWrite: func() {
			once.Do(func() {
				close(feedEntered)
				<-feedRelease
			})
		},
	}
	registry := newPaneRegistry(effects)
	witness := retentionWitness("%r5-saturated", "r5-saturated-inc")
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	if err := feedRetentionOutput(registry, controlmode.NewDecoder(), witness, bytes.Repeat([]byte{'s'}, 64<<10)); err != nil {
		t.Fatal(err)
	}
	<-feedEntered
	closeDone := make(chan error, 1)
	go func() { closeDone <- registry.Close() }()
	close(feedRelease)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
}

func TestRetentionR5CapacityFailFastAndRetirementReuse(t *testing.T) {
	realm := openRetentionRealm(t, "r5-cap-retire")
	clock := newRetentionManualClock()
	effects := &retentionShadowEffects{
		realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock,
		maxPanes: 1, maxCommands: 3, maxEnvelopes: 9,
	}
	registry := newPaneRegistry(effects)
	first := retentionWitness("%r5-cap-a", "r5-cap-a-inc")
	second := retentionWitness("%r5-cap-b", "r5-cap-b-inc")
	if err := registry.AdmitPane(first); err != nil {
		t.Fatal(err)
	}
	if err := registry.AdmitPane(second); !errors.Is(err, unifiedjournal.ErrInvalidated) {
		t.Fatalf("P+1 admission = %v, want fail-fast ErrInvalidated", err)
	}
	if err := registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationReplacement, Witness: first}); err != nil {
		t.Fatal(err)
	}
	var admitErr error
	for attempt := 0; attempt < 10000; attempt++ {
		admitErr = registry.AdmitPane(second)
		if admitErr == nil {
			break
		}
		if !errors.Is(admitErr, unifiedjournal.ErrInvalidated) {
			t.Fatal(admitErr)
		}
		runtime.Gosched()
	}
	if admitErr != nil {
		t.Fatalf("retired P credit was not reusable: %v", admitErr)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionR5FaultCASCleanupAndCutoff(t *testing.T) {
	for _, preFeedWins := range []bool{false, true} {
		name := "feed_wins"
		if preFeedWins {
			name = "pre_feed_wins"
		}
		t.Run(name, func(t *testing.T) { testRetentionR5FaultRace(t, preFeedWins) })
	}
}

func testRetentionR5FaultRace(t *testing.T, preFeedWins bool) {
	t.Helper()
	realm := openRetentionRealm(t, "r5-fault-race")
	clock := newRetentionManualClock()
	feedEntered := make(chan struct{})
	feedRelease := make(chan struct{})
	secondStage := make(chan struct{})
	stageRelease := make(chan struct{})
	winner := make(chan struct{})
	var winnerOnce sync.Once
	var feedOnce sync.Once
	var stageMu sync.Mutex
	appendCount := 0
	feedErr := errors.New("r5 feed fault")
	stageErr := errors.New("r5 pre-feed fault")
	effects := &retentionShadowEffects{
		realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock,
		writeHook: func(unifiedjournal.PaneKey, []byte) error {
			var err error
			feedOnce.Do(func() {
				close(feedEntered)
				<-feedRelease
				err = feedErr
			})
			return err
		},
		stage: func(operation string, _ unifiedjournal.PaneKey) error {
			if operation != "append" {
				return nil
			}
			stageMu.Lock()
			appendCount++
			count := appendCount
			stageMu.Unlock()
			if count != 2 {
				return nil
			}
			close(secondStage)
			if !preFeedWins {
				<-stageRelease
			}
			return stageErr
		},
		hook: func(point string, _ unifiedjournal.PaneKey) {
			if point == "after_fault_cas_winner" {
				winnerOnce.Do(func() { close(winner) })
			}
		},
	}
	registry := newPaneRegistry(effects)
	witness := retentionWitness("%r5-fault", "r5-fault-inc")
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	decoder := controlmode.NewDecoder()
	if err := feedRetentionOutput(registry, decoder, witness, bytes.Repeat([]byte{'a'}, 64<<10)); err != nil {
		t.Fatal(err)
	}
	<-feedEntered
	if err := feedRetentionOutput(registry, decoder, witness, bytes.Repeat([]byte{'b'}, 64<<10)); err != nil {
		t.Fatal(err)
	}
	<-secondStage
	if preFeedWins {
		<-winner
		close(feedRelease)
	} else {
		close(feedRelease)
		<-winner
		close(stageRelease)
	}
	effects.waitObservation("storage_fault", 2)
	effects.waitObservation("fault_cleanup", 1)
	if err := feedRetentionOutput(registry, decoder, witness, []byte("past-cutoff")); !errors.Is(err, unifiedjournal.ErrInvalidated) {
		t.Fatalf("past-cutoff output = %v, want ErrInvalidated", err)
	}
	effects.waitObservation("discarded_after_fault", 1)
	effects.mu.Lock()
	authoritative := 0
	var faultOrder []int64
	for index, event := range effects.observations {
		if event == "storage_fault" {
			faultOrder = append(faultOrder, effects.fields[index]["authoritative"])
			authoritative += int(effects.fields[index]["authoritative"])
		}
	}
	feedCount := len(effects.feeds)
	effects.mu.Unlock()
	if authoritative != 1 || feedCount != 0 {
		t.Fatalf("fault authority/feed cutoff = authority %d, feeds %d, order %v", authoritative, feedCount, faultOrder)
	}
	wantOrder := []int64{1, 0}
	if preFeedWins {
		wantOrder = []int64{0, 1}
	}
	if fmt.Sprint(faultOrder) != fmt.Sprint(wantOrder) {
		t.Fatalf("conditional result FIFO = %v, want %v", faultOrder, wantOrder)
	}
	if err := registry.Close(); err == nil {
		t.Fatal("Close lost authoritative asynchronous storage fault")
	}
}

func TestRetentionR5PreFeedResultDispatcherReentryAndDrainerProgress(t *testing.T) {
	realm := openRetentionRealm(t, "r5-prefeed-result")
	clock := newRetentionManualClock()
	callbackEntered := make(chan struct{})
	cleanupEntered := make(chan struct{})
	cleanupDone := make(chan struct{})
	callbackRelease := make(chan struct{})
	releaseCallback := sync.OnceFunc(func() { close(callbackRelease) })
	defer releaseCallback()
	var registry *paneRegistry
	witness := retentionWitness("%r5-prefeed", "r5-prefeed-inc")
	var callbackOnce sync.Once
	effects := &retentionShadowEffects{
		realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock,
		stage: func(operation string, _ unifiedjournal.PaneKey) error {
			if operation == "append" {
				return errors.New("r5 classified pre-feed fault")
			}
			return nil
		},
		hook: func(point string, _ unifiedjournal.PaneKey) {
			if point == "before_cleanup" {
				select {
				case <-cleanupEntered:
				default:
					close(cleanupEntered)
				}
			}
			if point == "after_cleanup" {
				close(cleanupDone)
			}
		},
		beforeObserve: func(event string) {
			if event != "storage_fault" {
				return
			}
			callbackOnce.Do(func() {
				close(callbackEntered)
				if err := registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: witness, Data: []byte("reenter")}); !errors.Is(err, unifiedjournal.ErrInvalidated) {
					t.Errorf("classified-result callback reentry: %v", err)
				}
				<-callbackRelease
			})
		},
	}
	registry = newPaneRegistry(effects)
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	returned := make(chan error, 1)
	go func() {
		returned <- feedRetentionOutput(registry, controlmode.NewDecoder(), witness, bytes.Repeat([]byte{'p'}, 64<<10))
	}()
	if err := <-returned; err != nil {
		t.Fatal(err)
	}
	<-callbackEntered
	select {
	case <-cleanupEntered:
		// The lane drainer continued through its elected cleanup while the sole
		// dispatcher remained inside the classified-result callback.
	case <-time.After(5 * time.Second):
		t.Fatal("pre-feed drainer handed off to or waited for the result callback")
	}
	releaseCallback()
	// Reentry can queue a discard before cleanup and retire the empty pane.
	// Cleanup still completes, but has no pane left to emit fault_cleanup for.
	<-cleanupDone
	if err := registry.Close(); err == nil {
		t.Fatal("Close lost classified pre-feed failure")
	}
}

func TestRetentionCorrectionTimerFeedCallbackCanReenterDurableFence(t *testing.T) {
	realm := openRetentionRealm(t, "timer-fence-reentry")
	clock := newRetentionManualClock()
	effects := &retentionShadowEffects{realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock}
	registry := newPaneRegistry(effects)
	witness := retentionWitness("%timer-fence", "timer-fence-inc")
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	reentered := make(chan error, 1)
	var once sync.Once
	effects.writeHook = func(unifiedjournal.PaneKey, []byte) error {
		once.Do(func() {
			reentered <- registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: witness, Label: "start"})
		})
		return nil
	}
	if err := feedRetentionOutput(registry, controlmode.NewDecoder(), witness, []byte{1}); err != nil {
		t.Fatal(err)
	}
	clock.waitTimerCount(1)
	advanceDone := make(chan struct{})
	go func() {
		clock.Advance(16 * time.Millisecond)
		close(advanceDone)
	}()
	select {
	case err := <-reentered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("timer flush manager waited on dispatcher while dispatcher reentered a durable same-pane fence")
	}
	select {
	case <-advanceDone:
	case <-time.After(time.Second):
		t.Fatal("timer callback did not signal and return after durable-fence reentry")
	}
	effects.writeHook = nil
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionCorrectionFinitePayloadAndLifetimeBudgets(t *testing.T) {
	t.Run("Q_must_fund_two_lifetime_credits_per_P", func(t *testing.T) {
		effects := &retentionShadowEffects{
			realm: openRetentionRealm(t, "invalid-lifetime-budget"), maxBytes: 64 << 10,
			maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(), maxPanes: 2, maxCommands: 3,
		}
		if registry := newPaneRegistry(effects); registry.retention != nil {
			_ = registry.Close()
			t.Fatal("runtime accepted Q < 2P lifetime reservation")
		}
	})
	t.Run("256MiB_rejected_before_router_copy", func(t *testing.T) {
		effects := &retentionShadowEffects{
			realm: openRetentionRealm(t, "finite-payload-budget"), maxBytes: 64 << 10,
			maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(),
		}
		registry := newPaneRegistry(effects)
		witness := retentionWitness("%payload-budget", "payload-budget-inc")
		if err := registry.AdmitPane(witness); err != nil {
			t.Fatal(err)
		}
		payload := make([]byte, 256<<20)
		err := registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: witness, Data: payload})
		if !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Fatalf("256MiB ingress error = %v, want fail-fast ErrInvalidated", err)
		}
		if err := registry.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRetentionCorrectionPostFaultIngressDoesNotMutateRouter(t *testing.T) {
	effects := &retentionShadowEffects{
		realm: openRetentionRealm(t, "post-fault-router"), maxBytes: 64 << 10,
		maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(),
		stage: func(operation string, _ unifiedjournal.PaneKey) error {
			if operation == "append" {
				return errors.New("classified pre-feed failure")
			}
			return nil
		},
	}
	registry := newPaneRegistry(effects)
	witness := retentionWitness("%post-fault", "post-fault-inc")
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	if err := feedRetentionOutput(registry, controlmode.NewDecoder(), witness, bytes.Repeat([]byte{'f'}, 64<<10)); err != nil {
		t.Fatal(err)
	}
	effects.waitObservation("storage_fault", 1)
	effects.waitObservation("fault_cleanup", 1)
	registry.mu.Lock()
	before := registry.sessions[sessionKey(witness.Session)].router.UnifiedEligible(witness)
	registry.mu.Unlock()
	if !before {
		t.Fatal("fault cleanup mutated router before the adversarial ingress")
	}
	err := registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationDisconnect, Witness: witness})
	if !errors.Is(err, unifiedjournal.ErrInvalidated) {
		t.Fatalf("post-fault disconnect = %v, want ErrInvalidated", err)
	}
	registry.mu.Lock()
	after := registry.sessions[sessionKey(witness.Session)].router.UnifiedEligible(witness)
	registry.mu.Unlock()
	if !after {
		t.Fatal("post-fault ingress mutated router before lane-visible cutoff rejection")
	}
	if err := registry.Close(); err == nil {
		t.Fatal("Close lost classified storage failure")
	}
}

func TestRetentionCorrectionPauseEndPartitionsHeldBatches(t *testing.T) {
	effects := &retentionShadowEffects{
		realm: openRetentionRealm(t, "pause-partition"), maxBytes: 64 << 10,
		maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(),
	}
	registry := newPaneRegistry(effects)
	witness := retentionWitness("%pause-partition", "pause-partition-inc")
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	if err := registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: witness, Label: "start"}); err != nil {
		t.Fatal(err)
	}
	decoder := controlmode.NewDecoder()
	for _, value := range []byte{'a', 'b'} {
		if err := feedRetentionOutput(registry, decoder, witness, bytes.Repeat([]byte{value}, 64<<10)); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: witness, Label: "end"}); err != nil {
		t.Fatal(err)
	}
	effects.waitObservation("pause_end_complete", 1)
	effects.mu.Lock()
	feeds := append([][]byte(nil), effects.feeds...)
	effects.mu.Unlock()
	lengths := make([]int, len(feeds))
	for index := range feeds {
		lengths[index] = len(feeds[index])
	}
	if len(feeds) != 2 || lengths[0] != 64<<10 || lengths[1] != 64<<10 {
		t.Fatalf("pause-end feed sizes = %v, want [65536 65536]", lengths)
	}
	if feeds[0][0] != 'a' || feeds[1][0] != 'b' {
		t.Fatalf("pause-end feed order = %q/%q", feeds[0][:1], feeds[1][:1])
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionCorrectionFailedGenerationRetiresAndReusesCapacity(t *testing.T) {
	cleanupDone := make(chan struct{})
	var cleanupOnce sync.Once
	effects := &retentionShadowEffects{
		realm: openRetentionRealm(t, "failed-generation-retirement"), maxBytes: 64 << 10,
		maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(), maxPanes: 1, maxCommands: 3,
		stage: func(operation string, _ unifiedjournal.PaneKey) error {
			if operation == "append" {
				return errors.New("retirement pre-feed failure")
			}
			return nil
		},
		hook: func(point string, _ unifiedjournal.PaneKey) {
			if point == "after_cleanup" {
				cleanupOnce.Do(func() { close(cleanupDone) })
			}
		},
	}
	registry := newPaneRegistry(effects)
	failed := retentionWitness("%failed-capacity", "failed-capacity-inc")
	replacement := retentionWitness("%replacement-capacity", "replacement-capacity-inc")
	if err := registry.AdmitPane(failed); err != nil {
		t.Fatal(err)
	}
	if err := feedRetentionOutput(registry, controlmode.NewDecoder(), failed, bytes.Repeat([]byte{'x'}, 64<<10)); err != nil {
		t.Fatal(err)
	}
	<-cleanupDone
	effects.waitObservation("storage_fault", 1)
	effects.waitObservation("fault_cleanup", 1)
	var admitErr error
	for attempt := 0; attempt < 10000; attempt++ {
		admitErr = registry.AdmitPane(replacement)
		if admitErr == nil {
			break
		}
		runtime.Gosched()
	}
	if admitErr != nil {
		t.Fatalf("failed generation did not release/reuse P/Q after result+cleanup: %v", admitErr)
	}
	if err := registry.Close(); err == nil {
		t.Fatal("Close lost retired generation storage failure")
	}
}

func (effects *retentionShadowEffects) BeginSessionBirth(string, string) (SessionBirthEffects, error) {
	return retentionBirthEffects{}, nil
}

// retentionTrial is deliberately test-private. The unchanged candidate ignores
// it, so the first test revision compiles and fails on immediate WritePane calls.
func (effects *retentionShadowEffects) retentionTrial() retentionTrialOptions {
	effects.mu.Lock()
	if effects.feedChanged == nil {
		effects.feedChanged = make(chan struct{}, 1)
	}
	if effects.observationChanged == nil {
		effects.observationChanged = make(chan struct{}, 1)
	}
	effects.mu.Unlock()
	return retentionTrialOptions{
		realm: effects.realm, maxBatchBytes: effects.maxBytes, maxFeedDelay: effects.maxDelay,
		now: effects.clock.Now, after: effects.clock.After, hook: effects.hook, stage: effects.stage,
		maxPanes: effects.maxPanes, maxCommands: effects.maxCommands, maxEnvelopes: effects.maxEnvelopes,
		maxIngressBytes: effects.maxIngressBytes, maxOutboxBytes: effects.maxOutboxBytes,
		observe: func(event string, fields map[string]int64) {
			if effects.beforeObserve != nil {
				effects.beforeObserve(event)
			}
			effects.mu.Lock()
			defer effects.mu.Unlock()
			effects.observations = append(effects.observations, event)
			copyFields := make(map[string]int64, len(fields))
			for key, value := range fields {
				copyFields[key] = value
			}
			effects.fields = append(effects.fields, copyFields)
			select {
			case effects.observationChanged <- struct{}{}:
			default:
			}
		},
	}
}

func (effects *retentionShadowEffects) waitObservation(event string, count int) {
	for {
		effects.mu.Lock()
		seen := 0
		for _, candidate := range effects.observations {
			if candidate == event {
				seen++
			}
		}
		changed := effects.observationChanged
		effects.mu.Unlock()
		if seen >= count {
			return
		}
		<-changed
	}
}

type retentionBirthEffects struct{}

func (retentionBirthEffects) CommitSessionBirth(string) error { return nil }
func (retentionBirthEffects) AbortSessionBirth(error) error   { return nil }

func openRetentionRealm(t *testing.T, label string) *unifiedjournal.Realm {
	t.Helper()
	realm, _ := openRetentionRealmWithCaps(t, label, 32<<20, 64<<20)
	return realm
}

func openRetentionRealmWithCaps(t *testing.T, label string, paneCap, realmCap int64) (*unifiedjournal.Realm, string) {
	t.Helper()
	runtimeDir := shortTempDir(t)
	realm, err := unifiedjournal.OpenRealm(unifiedjournal.OpenOptions{
		RuntimeDir: runtimeDir, Realm: label, BrokerIncarnation: "retention-batching-test",
		UID: os.Getuid(), GID: os.Getgid(), DirectoryMode: 0o700, FileMode: 0o600,
		PaneCapBytes: paneCap, RealmCapBytes: realmCap,
	})
	if err != nil {
		t.Fatalf("open retention realm: %v", err)
	}
	t.Cleanup(func() { _ = realm.Close() })
	return realm, runtimeDir
}

func serveRetentionTrial(t *testing.T, effects *retentionShadowEffects) {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join(shortTempDir(t), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Broker{Realm: "retention-test", FrontUID: uint32(os.Getuid()), FrontUIDConfigured: true}
	if err := ServeWithPaneEffects(listener, cfg, effects); err != nil {
		t.Fatalf("serve retention trial: %v", err)
	}
}

func retentionWitness(pane, incarnation string) controlmode.PaneWitness {
	return controlmode.PaneWitness{
		Session: controlmode.SessionWitness{Server: "test", Session: "$0", ControlGeneration: 1},
		Window:  "@0", Pane: pane, Incarnation: incarnation,
	}
}

func feedRetentionOutput(observer PaneObservationEffects, decoder *controlmode.Decoder, witness controlmode.PaneWitness, payload []byte) error {
	wire := append([]byte("%output "+witness.Pane+" "), retentionEncodeControlPayload(payload)...)
	wire = append(wire, '\n')
	events, err := decoder.Feed(wire)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Kind != controlmode.EventOutput && event.Kind != controlmode.EventExtendedOutput {
			continue
		}
		if err := observer.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: witness, Data: event.Data}); err != nil {
			return err
		}
	}
	return nil
}

func retentionEncodeControlPayload(payload []byte) []byte {
	encoded := make([]byte, 0, len(payload)*4)
	for _, value := range payload {
		encoded = append(encoded, '\\', '0'+((value>>6)&7), '0'+((value>>3)&7), '0'+(value&7))
	}
	return encoded
}

func TestRetentionBatchingNaturalIntegratedPath(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	clock := newRetentionManualClock()
	effects := &retentionShadowEffects{realm: openRetentionRealm(t, "natural-red"), maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock}
	decoded := 0
	effects.run = func(ctx context.Context, observer PaneObservationEffects) error {
		d := newDisposable(t)
		d.run("kill-session", "-t", "alpha")
		d.run("new-session", "-d", "-s", "alpha", "-x", "80", "-y", "24", "sh")
		cmd := exec.CommandContext(ctx, "tmux", "-S", d.path, "-CC", "attach-session", "-r", "-t", "alpha")
		terminal, err := pty.Start(cmd)
		if err != nil {
			return err
		}
		defer terminal.Close()
		go func() {
			time.Sleep(100 * time.Millisecond)
			d.run("send-keys", "-t", "alpha:", "-l", "printf one; printf two; printf three")
			d.run("send-keys", "-t", "alpha:", "Enter")
		}()
		decoder := controlmode.NewDecoder()
		witness := retentionWitness("%0", "natural")
		if err := observer.AdmitPane(witness); err != nil {
			return err
		}
		reader := bufio.NewReader(terminal)
		for decoded < 2 {
			line, readErr := reader.ReadBytes('\n')
			if readErr != nil {
				return readErr
			}
			line = bytes.TrimSuffix(line, []byte{'\r', '\n'})
			if marker := bytes.IndexByte(line, '%'); marker >= 0 {
				line = line[marker:]
			}
			if !bytes.HasPrefix(line, []byte("%output ")) && !bytes.HasPrefix(line, []byte("%extended-output ")) {
				continue
			}
			events, decodeErr := decoder.Feed(append(line, '\n'))
			if decodeErr != nil {
				return decodeErr
			}
			for _, event := range events {
				if event.Kind == controlmode.EventOutput || event.Kind == controlmode.EventExtendedOutput {
					decoded++
					if err := observer.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: witness, Data: event.Data}); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	serveRetentionTrial(t, effects)
	if decoded < 2 {
		t.Fatalf("real tmux -CC decoded only %d deliveries", decoded)
	}
	effects.mu.Lock()
	feeds := len(effects.feeds)
	effects.mu.Unlock()
	if feeds != 1 {
		t.Fatalf("decoded deliveries immediately called WritePane: decoded=%d feeds=%d; want one close-flushed real per-pane batch", decoded, feeds)
	}
}

func TestRetentionBatchingBoundaryFaultAndCloseIntegration(t *testing.T) {
	clock := newRetentionManualClock()
	effects := &retentionShadowEffects{realm: openRetentionRealm(t, "boundaries"), maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock}
	effects.run = func(_ context.Context, observer PaneObservationEffects) error {
		registry := observer.(*paneRegistry)
		decoder := controlmode.NewDecoder()
		left, right := retentionWitness("%0", "left"), retentionWitness("%1", "right")
		closing := retentionWitness("%2", "closing")
		if err := observer.AdmitPane(left); err != nil {
			return err
		}
		if err := observer.AdmitPane(right); err != nil {
			return err
		}
		if err := feedRetentionOutput(observer, decoder, left, []byte{1, 2}); err != nil {
			return err
		}
		if err := feedRetentionOutput(observer, decoder, right, []byte{3, 4}); err != nil {
			return err
		}
		if err := observer.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: right, Label: "start"}); err != nil {
			return err
		}
		if err := feedRetentionOutput(observer, decoder, right, []byte{5}); err != nil {
			return err
		}
		if err := observer.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: right, Label: "end"}); err != nil {
			return err
		}
		if err := registry.retentionBoundary(right, "storage_fault"); err != nil {
			return err
		}
		if err := feedRetentionOutput(observer, decoder, right, []byte{6}); !errors.Is(err, unifiedjournal.ErrInvalidated) {
			return fmt.Errorf("post-fault output = %v, want ErrInvalidated", err)
		}
		if err := registry.retentionBoundary(right, "explicit_flush"); err != nil {
			return err
		}
		if err := registry.retentionBoundary(right, "cut_checkpoint"); err != nil {
			return err
		}
		if err := registry.retentionBoundary(right, "terminal_handoff"); err != nil {
			return err
		}
		if err := observer.AdmitPane(closing); err != nil {
			return err
		}
		if err := observer.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: closing, Label: "start"}); err != nil {
			return err
		}
		return feedRetentionOutput(observer, decoder, closing, []byte{7})
	}
	serveRetentionTrial(t, effects)
	effects.mu.Lock()
	defer effects.mu.Unlock()
	if got := bytes.Join(effects.feeds, nil); !bytes.Equal(got, []byte{1, 2, 3, 4, 5, 7}) {
		t.Fatalf("boundary/fault feed = %v", got)
	}
	joined := strings.Join(effects.observations, ",")
	for _, required := range []string{"decode_arrival", "flush_start", "append_write_complete", "append_sync_complete", "commit_write_complete", "commit_sync_complete", "committed_offset", "feed_start", "feed_complete", "pause_start", "pause_end", "discarded_after_fault", "close"} {
		if !strings.Contains(joined, required) {
			t.Errorf("missing content-free observation %q in %s", required, joined)
		}
	}
	pauseEndFlush := false
	closeFlush := false
	for index, event := range effects.observations {
		if event != "flush_start" {
			continue
		}
		pauseEndFlush = pauseEndFlush || effects.fields[index]["reason"] == 6
		closeFlush = closeFlush || effects.fields[index]["reason"] == 12
	}
	if !pauseEndFlush || !closeFlush {
		t.Fatalf("missing pause-end/close boundary flush: pause_end=%t close=%t", pauseEndFlush, closeFlush)
	}
	order := []string{"append_write_complete", "append_sync_complete", "commit_write_complete", "commit_sync_complete", "committed_offset", "feed_start", "feed_complete"}
	previous := -1
	for _, event := range order {
		index := indexString(effects.observations, event)
		if index <= previous {
			t.Errorf("durability/feed order %v", effects.observations)
			break
		}
		previous = index
	}
}

func TestRetentionBatchingTimerPaneIncarnationPauseFaultAndClose(t *testing.T) {
	clock := newRetentionManualClock()
	effects := &retentionShadowEffects{realm: openRetentionRealm(t, "timer-boundaries"), maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock}
	effects.run = func(_ context.Context, observer PaneObservationEffects) error {
		registry := observer.(*paneRegistry)
		decoder := controlmode.NewDecoder()
		first := retentionWitness("%0", "first")
		second := retentionWitness("%1", "second")
		if err := observer.AdmitPane(first); err != nil {
			return err
		}
		if err := observer.AdmitPane(second); err != nil {
			return err
		}
		if err := feedRetentionOutput(observer, decoder, first, []byte{1}); err != nil {
			return err
		}
		clock.waitTimerCount(1)
		clock.Advance(15 * time.Millisecond)
		effects.mu.Lock()
		beforeTimer := len(effects.feeds)
		effects.mu.Unlock()
		if beforeTimer != 0 {
			return fmt.Errorf("timer flushed early")
		}
		clock.Advance(time.Millisecond)
		effects.waitFeedCount(1)
		effects.mu.Lock()
		afterTimer := len(effects.feeds)
		effects.mu.Unlock()
		if afterTimer != 1 {
			return fmt.Errorf("timer did not flush at the 16ms boundary")
		}
		if err := feedRetentionOutput(observer, decoder, first, []byte{2}); err != nil {
			return err
		}
		if err := feedRetentionOutput(observer, decoder, second, []byte{3}); err != nil {
			return err
		}
		if err := registry.retentionBoundary(second, "incarnation_change"); err != nil {
			return err
		}
		if err := feedRetentionOutput(observer, decoder, second, []byte{31}); err != nil {
			return err
		}
		if err := observer.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: second, Label: "start"}); err != nil {
			return err
		}
		if err := feedRetentionOutput(observer, decoder, second, []byte{4}); err != nil {
			return err
		}
		clock.Advance(64 * time.Millisecond)
		if err := observer.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: second, Label: "end"}); err != nil {
			return err
		}
		if err := registry.retentionBoundary(second, "storage_fault"); err != nil {
			return err
		}
		if err := feedRetentionOutput(observer, decoder, second, []byte{5}); !errors.Is(err, unifiedjournal.ErrInvalidated) {
			return fmt.Errorf("post-fault output = %v, want ErrInvalidated", err)
		}
		return nil
	}
	serveRetentionTrial(t, effects)
	effects.mu.Lock()
	defer effects.mu.Unlock()
	if got := bytes.Join(effects.feeds, nil); !bytes.Equal(got, []byte{1, 2, 3, 31, 4}) {
		t.Fatalf("timer/boundary feed=%v", got)
	}
	if len(effects.feedKeys) != len(effects.feeds) || effects.feedKeys[0].Incarnation != "first" || effects.feedKeys[len(effects.feedKeys)-1].Incarnation != "second" {
		t.Fatalf("full PaneKey/incarnation isolation lost: %+v", effects.feedKeys)
	}
	joined := strings.Join(effects.observations, ",")
	for _, required := range []string{"pause_hold", "pause_end", "storage_fault", "discarded_after_fault", "close"} {
		if !strings.Contains(joined, required) {
			t.Errorf("missing %s observation", required)
		}
	}
	pauseFlush := false
	for index, event := range effects.observations {
		if event == "flush_start" && effects.fields[index]["reason"] == 5 {
			pauseFlush = true
		}
	}
	if !pauseFlush {
		t.Error("pause start did not establish its own flush boundary")
	}
}

func TestRetentionBatchingStaleTimerCannotFlushNewGeneration(t *testing.T) {
	clock := newRetentionManualClock()
	callbackStarted := make(chan struct{})
	callbackRelease := make(chan struct{})
	callbackExited := make(chan struct{})
	var callbackOnce sync.Once
	effects := &retentionShadowEffects{
		realm: openRetentionRealm(t, "stale-timer"), maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock,
		feedChanged: make(chan struct{}, 1),
		hook: func(point string, _ unifiedjournal.PaneKey) {
			if point == "timer_callback_enter" {
				callbackOnce.Do(func() {
					close(callbackStarted)
					<-callbackRelease
				})
			}
			if point == "timer_callback_exit" {
				select {
				case <-callbackExited:
				default:
					close(callbackExited)
				}
			}
		},
	}
	runtime := newRetentionTrialRuntime(effects)
	key := journalKey(retentionWitness("%0", "stale-timer"))
	if err := runtime.WritePane(key, []byte{1}); err != nil {
		t.Fatal(err)
	}
	clock.waitTimerCount(1)
	clock.mu.Lock()
	clock.timers[0].canceled = true
	firstCallback := clock.timers[0].fn
	clock.mu.Unlock()
	go firstCallback()
	<-callbackStarted
	if err := runtime.WritePane(key, bytes.Repeat([]byte{2}, 64<<10)); err != nil {
		t.Fatal(err)
	}
	effects.waitFeedCount(1)
	effects.mu.Lock()
	lengths := make([]int, len(effects.feeds))
	for index := range effects.feeds {
		lengths[index] = len(effects.feeds[index])
	}
	effects.mu.Unlock()
	if fmt.Sprint(lengths) != "[65536]" {
		t.Fatalf("stale generation flushed new remainder: feed lengths=%v, want [65536]", lengths)
	}
	close(callbackRelease)
	<-callbackExited
	clock.waitTimerCount(2)
	clock.Advance(16 * time.Millisecond)
	effects.waitFeedCount(2)
	effects.mu.Lock()
	lengths = lengths[:0]
	for index := range effects.feeds {
		lengths = append(lengths, len(effects.feeds[index]))
	}
	effects.mu.Unlock()
	if fmt.Sprint(lengths) != "[65536 1]" {
		t.Fatalf("current generation did not flush remainder: feed lengths=%v, want [65536 1]", lengths)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionBatchingRealRegistryIncarnationChangeFlushesOldPrefix(t *testing.T) {
	clock := newRetentionManualClock()
	effects := &retentionShadowEffects{realm: openRetentionRealm(t, "real-incarnation-change"), maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock}
	registry := newPaneRegistry(effects)
	decoder := controlmode.NewDecoder()
	oldWitness := retentionWitness("%0", "old")
	newWitness := retentionWitness("%0", "new")
	if err := registry.AdmitPane(oldWitness); err != nil {
		t.Fatal(err)
	}
	if err := feedRetentionOutput(registry, decoder, oldWitness, []byte{7}); err != nil {
		t.Fatal(err)
	}
	if err := feedRetentionOutput(registry, decoder, newWitness, []byte{8}); err != nil {
		t.Fatal(err)
	}
	effects.waitFeedCount(1)
	effects.waitObservation("discarded_after_fault", 1)

	effects.mu.Lock()
	defer effects.mu.Unlock()
	if len(effects.feeds) != 1 || !bytes.Equal(effects.feeds[0], []byte{7}) || effects.feedKeys[0].Incarnation != "old" {
		t.Fatalf("incarnation change feeds=%v keys=%+v, want old valid prefix only", effects.feeds, effects.feedKeys)
	}
	boundary := -1
	discard := -1
	for index, event := range effects.observations {
		if event == "flush_start" && effects.fields[index]["reason"] == flushReasonCode("incarnation_change") {
			boundary = index
		}
		if event == "discarded_after_fault" {
			discard = index
		}
	}
	if boundary < 0 || discard < 0 || boundary >= discard {
		t.Fatalf("incarnation ordering observations=%v boundary=%d discard=%d", effects.observations, boundary, discard)
	}

	t.Run("close_race", func(t *testing.T) {
		clock := newRetentionManualClock()
		raceEffects := &retentionShadowEffects{realm: openRetentionRealm(t, "incarnation-close-race"), maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock}
		raceRegistry := newPaneRegistry(raceEffects)
		oldWitness := retentionWitness("%0", "race-old")
		newWitness := retentionWitness("%0", "race-new")
		if err := raceRegistry.AdmitPane(oldWitness); err != nil {
			t.Fatal(err)
		}
		if err := feedRetentionOutput(raceRegistry, controlmode.NewDecoder(), oldWitness, []byte{11}); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		observeDone := make(chan error, 1)
		closeDone := make(chan error, 1)
		go func() {
			<-start
			observeDone <- feedRetentionOutput(raceRegistry, controlmode.NewDecoder(), newWitness, []byte{12})
		}()
		go func() {
			<-start
			closeDone <- raceRegistry.retention.Close()
		}()
		close(start)
		if err := <-observeDone; err != nil && !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Fatalf("incarnation observation racing Close error=%v", err)
		}
		if err := <-closeDone; err != nil {
			t.Fatal(err)
		}
		raceEffects.mu.Lock()
		defer raceEffects.mu.Unlock()
		if len(raceEffects.feeds) != 1 || !bytes.Equal(raceEffects.feeds[0], []byte{11}) || raceEffects.feedKeys[0].Incarnation != "race-old" {
			t.Fatalf("incarnation/close race feeds=%v keys=%+v, want old prefix exactly once", raceEffects.feeds, raceEffects.feedKeys)
		}
	})
}

func TestRetentionBatchingAdmissionIncarnationChangeFlushesOldPrefix(t *testing.T) {
	clock := newRetentionManualClock()
	effects := &retentionShadowEffects{realm: openRetentionRealm(t, "admission-incarnation-change"), maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock}
	registry := newPaneRegistry(effects)
	decoder := controlmode.NewDecoder()
	oldWitness := retentionWitness("%0", "old-admitted")
	newWitness := retentionWitness("%0", "new-admitted")
	if err := registry.AdmitPane(oldWitness); err != nil {
		t.Fatal(err)
	}
	if err := feedRetentionOutput(registry, decoder, oldWitness, []byte{7}); err != nil {
		t.Fatal(err)
	}
	if err := registry.AdmitPane(newWitness); !errors.Is(err, controlmode.ErrMissedFirstByte) {
		t.Fatalf("new-incarnation admission error=%v, want %v", err, controlmode.ErrMissedFirstByte)
	}
	if err := feedRetentionOutput(registry, decoder, newWitness, []byte{8}); err != nil {
		t.Fatal(err)
	}
	effects.waitFeedCount(1)
	effects.waitObservation("discarded_after_fault", 1)

	registry.mu.Lock()
	_, admitted := registry.admitted[routeCoordinateKey(newWitness)]
	_, coordinated := registry.panes[coordinateKey(newWitness)]
	registry.mu.Unlock()
	effects.mu.Lock()
	if len(effects.feeds) != 1 || !bytes.Equal(effects.feeds[0], []byte{7}) || effects.feedKeys[0].Incarnation != "old-admitted" {
		effects.mu.Unlock()
		t.Fatalf("admission incarnation change feeds=%v keys=%+v, want old valid prefix only", effects.feeds, effects.feedKeys)
	}
	boundary := -1
	discard := -1
	boundaries := 0
	for index, event := range effects.observations {
		if event == "flush_start" && effects.fields[index]["reason"] == flushReasonCode("incarnation_change") {
			boundary = index
			boundaries++
		}
		if event == "discarded_after_fault" {
			discard = index
		}
	}
	effects.mu.Unlock()
	if admitted || coordinated {
		t.Fatalf("rejected new witness retained: admitted=%t coordinated=%t", admitted, coordinated)
	}
	if boundaries != 1 || boundary < 0 || discard < 0 || boundary >= discard {
		t.Fatalf("admission incarnation ordering: boundaries=%d boundary=%d discard=%d", boundaries, boundary, discard)
	}

	t.Run("close_error_race", func(t *testing.T) {
		clock := newRetentionManualClock()
		feedStarted := make(chan struct{})
		feedRelease := make(chan struct{})
		raceEffects := &retentionShadowEffects{
			realm: openRetentionRealm(t, "admission-incarnation-close-race"), maxBytes: 64 << 10,
			maxDelay: 16 * time.Millisecond, clock: clock,
			beforeWrite: func() {
				close(feedStarted)
				<-feedRelease
			},
		}
		raceRegistry := newPaneRegistry(raceEffects)
		oldWitness := retentionWitness("%0", "race-old-admitted")
		newWitness := retentionWitness("%0", "race-new-admitted")
		if err := raceRegistry.AdmitPane(oldWitness); err != nil {
			t.Fatal(err)
		}
		if err := feedRetentionOutput(raceRegistry, controlmode.NewDecoder(), oldWitness, []byte{11}); err != nil {
			t.Fatal(err)
		}
		admitDone := make(chan error, 1)
		go func() { admitDone <- raceRegistry.AdmitPane(newWitness) }()
		<-feedStarted
		closeStarted := make(chan struct{})
		closeDone := make(chan error, 1)
		go func() {
			close(closeStarted)
			closeDone <- raceRegistry.retention.Close()
		}()
		<-closeStarted
		close(feedRelease)
		if err := <-admitDone; !errors.Is(err, controlmode.ErrMissedFirstByte) {
			t.Fatalf("racing admission error=%v, want %v", err, controlmode.ErrMissedFirstByte)
		}
		if err := <-closeDone; err != nil {
			t.Fatal(err)
		}
		if err := feedRetentionOutput(raceRegistry, controlmode.NewDecoder(), newWitness, []byte{12}); !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Fatalf("post-race invalid output error=%v, want %v", err, unifiedjournal.ErrInvalidated)
		}
		raceEffects.mu.Lock()
		defer raceEffects.mu.Unlock()
		if len(raceEffects.feeds) != 1 || !bytes.Equal(raceEffects.feeds[0], []byte{11}) || raceEffects.feedKeys[0].Incarnation != "race-old-admitted" {
			t.Fatalf("admission/close race feeds=%v keys=%+v, want old prefix exactly once", raceEffects.feeds, raceEffects.feedKeys)
		}
		boundaries := 0
		for index, event := range raceEffects.observations {
			if event == "flush_start" && raceEffects.fields[index]["reason"] == flushReasonCode("incarnation_change") {
				boundaries++
			}
		}
		if boundaries != 1 {
			t.Fatalf("admission/close race incarnation boundaries=%d, want 1", boundaries)
		}
	})
}

func TestRetentionBatchingIncarnationTransitionsWaitForAcceptedOldWrite(t *testing.T) {
	type transitionResult struct {
		err       error
		committed int64
	}
	for _, variant := range []string{"admit", "observe"} {
		t.Run(variant, func(t *testing.T) {
			clock := newRetentionManualClock()
			appendStarted := make(chan struct{})
			appendRelease := make(chan struct{})
			var appendOnce sync.Once
			effects := &retentionShadowEffects{
				realm: openRetentionRealm(t, "accepted-old-"+variant), maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock,
				hook: func(point string, _ unifiedjournal.PaneKey) {
					if point == "before_append" {
						appendOnce.Do(func() {
							close(appendStarted)
							<-appendRelease
						})
					}
				},
			}
			registry := newPaneRegistry(effects)
			oldWitness := retentionWitness("%0", "accepted-old")
			newWitness := retentionWitness("%0", "transition-new")
			if err := registry.AdmitPane(oldWitness); err != nil {
				t.Fatal(err)
			}
			oldDone := make(chan error, 1)
			go func() {
				oldDone <- feedRetentionOutput(registry, controlmode.NewDecoder(), oldWitness, []byte{7})
			}()
			if err := <-oldDone; err != nil {
				t.Fatal(err)
			}

			transitionStarted := make(chan struct{})
			transitionDone := make(chan transitionResult, 1)
			go func() {
				close(transitionStarted)
				var err error
				if variant == "admit" {
					err = registry.AdmitPane(newWitness)
				} else {
					err = feedRetentionOutput(registry, controlmode.NewDecoder(), newWitness, []byte{8})
				}
				result := transitionResult{err: err, committed: effects.realm.CommittedOffset(journalKey(oldWitness))}
				transitionDone <- result
			}()
			<-transitionStarted
			<-appendStarted
			var result transitionResult
			returnedBeforeOldWrite := false
			select {
			case result = <-transitionDone:
				returnedBeforeOldWrite = true
			default:
			}

			close(appendRelease)
			if !returnedBeforeOldWrite {
				result = <-transitionDone
			}
			if variant == "admit" {
				if !errors.Is(result.err, controlmode.ErrMissedFirstByte) {
					t.Fatalf("admission transition error=%v, want %v", result.err, controlmode.ErrMissedFirstByte)
				}
			} else if result.err != nil {
				t.Fatalf("observation transition error=%v", result.err)
			}
			effects.waitFeedCount(1)
			if err := feedRetentionOutput(registry, controlmode.NewDecoder(), newWitness, []byte{9}); err != nil {
				t.Fatal(err)
			}
			if err := registry.retention.Close(); err != nil {
				t.Fatal(err)
			}
			effects.mu.Lock()
			finalFeeds := len(effects.feeds)
			boundaries := 0
			for index, event := range effects.observations {
				if event == "flush_start" && effects.fields[index]["reason"] == flushReasonCode("incarnation_change") {
					boundaries++
				}
			}
			var finalPayload []byte
			if finalFeeds != 0 {
				finalPayload = append([]byte(nil), effects.feeds[0]...)
			}
			effects.mu.Unlock()
			if returnedBeforeOldWrite || result.committed != 1 || boundaries != 1 {
				t.Fatalf("%s transition passed accepted old write: returned_early=%t committed_at_return=%d incarnation_boundaries=%d final_feeds=%d", variant, returnedBeforeOldWrite, result.committed, boundaries, finalFeeds)
			}
			if finalFeeds != 1 || !bytes.Equal(finalPayload, []byte{7}) {
				t.Fatalf("%s transition final feeds=%d payload=%v, want old byte exactly once", variant, finalFeeds, finalPayload)
			}
		})
	}
}

func TestRetentionBatchingCloseIsTerminalForBoundaryAndDiscard(t *testing.T) {
	pathSnapshot := func(t *testing.T, root string) string {
		t.Helper()
		var paths []string
		if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			paths = append(paths, relative+":"+info.Mode().String())
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return strings.Join(paths, "\n")
	}
	newClosedRuntime := func(t *testing.T, label string) (*retentionTrialRuntime, *retentionShadowEffects, string) {
		t.Helper()
		clock := newRetentionManualClock()
		realm, runtimeDir := openRetentionRealmWithCaps(t, label, 32<<20, 64<<20)
		effects := &retentionShadowEffects{realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock}
		runtime := newRetentionTrialRuntime(effects)
		if err := runtime.Close(); err != nil {
			t.Fatal(err)
		}
		return runtime, effects, runtimeDir
	}
	assertUnchanged := func(t *testing.T, runtime *retentionTrialRuntime, effects *retentionShadowEffects, runtimeDir string, panes, observations int, paths string) {
		t.Helper()
		if len(runtime.panes) != panes || runtime.last != nil {
			t.Fatalf("closed runtime allocated state: panes=%d last=%v", len(runtime.panes), runtime.last)
		}
		if after := pathSnapshot(t, runtimeDir); after != paths {
			t.Fatalf("closed runtime allocated path: before=%q after=%q", paths, after)
		}
		effects.mu.Lock()
		defer effects.mu.Unlock()
		if len(effects.observations) != observations {
			t.Fatalf("closed runtime published observation: before=%d after=%d events=%v", observations, len(effects.observations), effects.observations)
		}
	}

	t.Run("boundary", func(t *testing.T) {
		runtime, effects, runtimeDir := newClosedRuntime(t, "closed-boundary")
		beforePanes, beforeObservations := len(runtime.panes), len(effects.observations)
		beforePaths := pathSnapshot(t, runtimeDir)
		err := runtime.Boundary(journalKey(retentionWitness("%0", "closed")), "pause_start")
		if !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Fatalf("Boundary after Close error=%v, want %v", err, unifiedjournal.ErrInvalidated)
		}
		assertUnchanged(t, runtime, effects, runtimeDir, beforePanes, beforeObservations, beforePaths)
	})

	t.Run("discard", func(t *testing.T) {
		runtime, effects, runtimeDir := newClosedRuntime(t, "closed-discard")
		beforePanes, beforeObservations := len(runtime.panes), len(effects.observations)
		beforePaths := pathSnapshot(t, runtimeDir)
		discarder, ok := any(runtime).(interface {
			Discard(unifiedjournal.PaneKey, int, string) error
		})
		if !ok {
			runtime.Discard(journalKey(retentionWitness("%0", "closed")), 1, "decode_fault")
			t.Error("Discard after Close has no terminal error result")
		} else if err := discarder.Discard(journalKey(retentionWitness("%0", "closed")), 1, "decode_fault"); !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Fatalf("Discard after Close error=%v, want %v", err, unifiedjournal.ErrInvalidated)
		}
		assertUnchanged(t, runtime, effects, runtimeDir, beforePanes, beforeObservations, beforePaths)
	})

	t.Run("slow_feed_close_race", func(t *testing.T) {
		clock := newRetentionManualClock()
		realm, runtimeDir := openRetentionRealmWithCaps(t, "slow-feed-close", 32<<20, 64<<20)
		feedStarted := make(chan struct{})
		feedRelease := make(chan struct{})
		effects := &retentionShadowEffects{
			realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock,
			beforeWrite: func() {
				close(feedStarted)
				<-feedRelease
			},
		}
		runtime := newRetentionTrialRuntime(effects)
		key := journalKey(retentionWitness("%0", "slow-close"))
		if err := runtime.WritePane(key, []byte{9}); err != nil {
			t.Fatal(err)
		}
		closeDone := make(chan error, 1)
		go func() { closeDone <- runtime.Close() }()
		<-feedStarted
		select {
		case err := <-closeDone:
			t.Fatalf("Close returned before slow committed feed completed: %v", err)
		default:
		}
		boundaryDone := make(chan error, 1)
		discardDone := make(chan error, 1)
		go func() { boundaryDone <- runtime.Boundary(key, "pause_start") }()
		go func() { discardDone <- runtime.Discard(key, 1, "decode_fault") }()
		close(feedRelease)
		if err := <-closeDone; err != nil {
			t.Fatal(err)
		}
		if err := <-boundaryDone; !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Fatalf("Boundary racing slow Close error=%v, want %v", err, unifiedjournal.ErrInvalidated)
		}
		if err := <-discardDone; !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Fatalf("Discard racing slow Close error=%v, want %v", err, unifiedjournal.ErrInvalidated)
		}
		effects.mu.Lock()
		if len(effects.feeds) != 1 || !bytes.Equal(effects.feeds[0], []byte{9}) {
			t.Fatalf("slow close feeds=%v, want one committed prefix", effects.feeds)
		}
		effects.mu.Unlock()
		paths := pathSnapshot(t, runtimeDir)
		panes, observations := len(runtime.panes), len(effects.observations)
		if len(runtime.panes) != panes || pathSnapshot(t, runtimeDir) != paths || len(effects.observations) != observations {
			t.Fatal("racing calls changed terminal closed state")
		}
	})
}

func TestRetentionBatchingCapWinnerUsesRealFileAccounting(t *testing.T) {
	clock := newRetentionManualClock()
	realm, runtimeDir := openRetentionRealmWithCaps(t, "real-cap", 512, 1024)
	effects := &retentionShadowEffects{realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: clock}
	effects.run = func(_ context.Context, observer PaneObservationEffects) error {
		registry := observer.(*paneRegistry)
		witness := retentionWitness("%0", "cap")
		if err := observer.AdmitPane(witness); err != nil {
			return err
		}
		if err := feedRetentionOutput(observer, controlmode.NewDecoder(), witness, bytes.Repeat([]byte{7}, 1024)); err != nil {
			return err
		}
		return registry.retentionBoundary(witness, "explicit_flush")
	}
	listener, err := net.Listen("unix", filepath.Join(shortTempDir(t), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Broker{Realm: "retention-test", FrontUID: uint32(os.Getuid()), FrontUIDConfigured: true}
	if err := ServeWithPaneEffects(listener, cfg, effects); !errors.Is(err, unifiedjournal.ErrStorage) {
		t.Fatalf("cap flush error=%v", err)
	}
	effects.mu.Lock()
	if len(effects.feeds) != 0 {
		t.Fatalf("failed cap batch leaked %d feeds", len(effects.feeds))
	}
	capObserved := false
	for index, event := range effects.observations {
		if event == "storage_fault" && effects.fields[index]["cap_winner"] == 1 {
			capObserved = true
		}
	}
	effects.mu.Unlock()
	if !capObserved {
		t.Fatal("real quota did not report content-free cap winner")
	}
	logical, allocated := retentionFileAccounting(t, runtimeDir)
	t.Logf("file_receipt logical_bytes=%d allocated_st_blocks_x512=%d", logical, allocated)
	if logical == 0 || allocated == 0 {
		t.Fatalf("real file accounting logical=%d allocated=%d", logical, allocated)
	}
}

func retentionFileAccounting(t *testing.T, runtimeDir string) (int64, int64) {
	t.Helper()
	var logical, allocated int64
	if err := filepath.Walk(runtimeDir, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode().IsRegular() && strings.HasSuffix(info.Name(), ".journal") {
			logical += info.Size()
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				allocated += stat.Blocks * 512
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return logical, allocated
}

func TestRetentionBatchingCrashCommittedPrefix(t *testing.T) {
	if os.Getenv("PERSEA_RETENTION_CRASH_HELPER") == "1" {
		retentionCrashHelper(t)
		return
	}
	if _, err := exec.LookPath("strace"); err != nil {
		t.Skip("strace unavailable")
	}
	for _, cut := range []string{"before_append", "after_append_write", "after_append_sync", "after_commit_write_before_sync", "after_commit_sync"} {
		t.Run(cut, func(t *testing.T) {
			runtimeDir := shortTempDir(t)
			markerPath := filepath.Join(runtimeDir, "cut.ready")
			scenario := retentionCrashScenario{RuntimeDir: runtimeDir, MarkerPath: markerPath, Cut: cut, PayloadHex: "010203040506"}
			scenarioBytes, _ := json.Marshal(scenario)
			scenarioPath := filepath.Join(runtimeDir, "scenario.json")
			if err := os.WriteFile(scenarioPath, scenarioBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			tracePath := filepath.Join(runtimeDir, "syscalls.log")
			traceArgs := []string{"-f", "-o", tracePath, "-e", "trace=write,fsync,close"}
			if cut == "after_commit_write_before_sync" {
				traceArgs = append(traceArgs, "-e", "inject=fsync:delay_enter=30s:when=2")
			}
			traceArgs = append(traceArgs, os.Args[0], "-test.run", "^TestRetentionBatchingCrashCommittedPrefix$")
			cmd := exec.Command("strace", traceArgs...)
			cmd.Env = append(os.Environ(), "PERSEA_RETENTION_CRASH_HELPER=1", "PERSEA_RETENTION_CRASH_SCENARIO="+scenarioPath)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(markerPath); err == nil {
					break
				}
				if time.Now().After(deadline) {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					t.Fatal("crash helper did not reach cut")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if cut == "after_commit_write_before_sync" {
				time.Sleep(100 * time.Millisecond)
			}
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			if receiptDir := os.Getenv("PERSEA_RETENTION_CRASH_RECEIPT_DIR"); receiptDir != "" {
				trace, err := os.ReadFile(tracePath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(receiptDir, cut+".syscalls.log"), trace, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := unifiedjournal.OpenRealm(retentionCrashOptions(runtimeDir))
			if err != nil {
				t.Fatal(err)
			}
			committed, _ := reopened.ReadCommitted(journalKey(retentionWitness("%0", "crash")))
			_ = reopened.Close()
			want := ""
			if cut == "after_commit_write_before_sync" || cut == "after_commit_sync" {
				want = scenario.PayloadHex
			}
			if got := hex.EncodeToString(committed); got != want {
				t.Fatalf("reopened prefix=%q want=%q", got, want)
			}
			t.Logf("crash_receipt cut=%s reopened_committed_bytes=%d expected_committed_bytes=%d", cut, len(committed), len(want)/2)
		})
	}
}

type retentionCrashWriteAhead struct {
	realm    *unifiedjournal.Realm
	scenario retentionCrashScenario
}

func (ahead retentionCrashWriteAhead) reach(cut string) {
	if ahead.scenario.Cut != cut {
		return
	}
	_ = os.WriteFile(ahead.scenario.MarkerPath, []byte(cut), 0o600)
	select {}
}

func (ahead retentionCrashWriteAhead) Append(key unifiedjournal.PaneKey, payload []byte) (unifiedjournal.Record, error) {
	ahead.reach("before_append")
	record, err := ahead.realm.Append(key, payload)
	if err == nil {
		ahead.reach("after_append_write")
	}
	return record, err
}

func (ahead retentionCrashWriteAhead) Sync(key unifiedjournal.PaneKey) error {
	err := ahead.realm.Sync(key)
	if err == nil {
		ahead.reach("after_append_sync")
	}
	return err
}

func (ahead retentionCrashWriteAhead) AdvanceCommitted(key unifiedjournal.PaneKey, record unifiedjournal.Record) error {
	if ahead.scenario.Cut == "after_commit_write_before_sync" {
		_ = os.WriteFile(ahead.scenario.MarkerPath, []byte(ahead.scenario.Cut), 0o600)
	}
	err := ahead.realm.AdvanceCommitted(key, record)
	if err == nil {
		ahead.reach("after_commit_sync")
	}
	return err
}

func retentionCrashOptions(runtimeDir string) unifiedjournal.OpenOptions {
	return unifiedjournal.OpenOptions{RuntimeDir: runtimeDir, Realm: "crash", BrokerIncarnation: "crash-helper", UID: os.Getuid(), GID: os.Getgid(), DirectoryMode: 0o700, FileMode: 0o600, PaneCapBytes: 1 << 20, RealmCapBytes: 2 << 20}
}

func retentionCrashHelper(t *testing.T) {
	data, err := os.ReadFile(os.Getenv("PERSEA_RETENTION_CRASH_SCENARIO"))
	if err != nil {
		t.Fatal(err)
	}
	var scenario retentionCrashScenario
	if err := json.Unmarshal(data, &scenario); err != nil {
		t.Fatal(err)
	}
	payload, err := hex.DecodeString(scenario.PayloadHex)
	if err != nil {
		t.Fatal(err)
	}
	realm, err := unifiedjournal.OpenRealm(retentionCrashOptions(scenario.RuntimeDir))
	if err != nil {
		t.Fatal(err)
	}
	ahead := retentionCrashWriteAhead{realm: realm, scenario: scenario}
	key := journalKey(retentionWitness("%0", "crash"))
	// A generation must carry its durable birth geometry before its first byte,
	// or the crash-recovered journal is byte-only and fails unified closed.
	if err := realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	if err := unifiedjournal.NewSequencer(ahead, func(unifiedjournal.PaneKey, unifiedjournal.Record, []byte) error { return nil }).Write(key, payload); err != nil {
		t.Fatal(err)
	}
	t.Fatal("crash helper passed selected cut")
}

func loadRetentionWorkloadFile(t *testing.T) retentionWorkloadFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "unifiedjournal", "testdata", "retention_workloads.json"))
	if err != nil {
		t.Fatal(err)
	}
	requireRetentionSHA(t, data, retentionFixtureSHA256, "fixture")
	var fixture retentionWorkloadFile
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Schema != "issue25-retention-workloads-v1" || len(fixture.Workloads) != 19 {
		t.Fatalf("fixture identity/count = %q/%d", fixture.Schema, len(fixture.Workloads))
	}
	return fixture
}

func indexString(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return -1
}

func TestRetentionBatchingTwoArmNineteenWorkloadMatrix(t *testing.T) {
	fixture := loadRetentionWorkloadFile(t)
	for _, maxBytes := range []int{64 << 10, 256 << 10} {
		for _, workload := range fixture.Workloads {
			maxBytes, workload := maxBytes, workload
			t.Run(fmt.Sprintf("%dk/%s", maxBytes>>10, workload.ID), func(t *testing.T) {
				chunks := retentionWorkloadChunks(t, workload)
				payload := bytes.Join(chunks, nil)
				requireRetentionSHA(t, payload, workload.PayloadSHA256, "payload")
				if len(payload) != workload.PayloadBytes || len(chunks) != workload.ChunkCount {
					t.Fatalf("input shape = %d bytes/%d chunks", len(payload), len(chunks))
				}
				requireRetentionSHA(t, retentionScheduleBytes(chunks, workload.IntervalNS), workload.OrderedScheduleSHA256, "schedule")
				clock := newRetentionManualClock()
				realm, runtimeDir := openRetentionRealmWithCaps(t, workload.ID+strconv.Itoa(maxBytes), 64<<20, 128<<20)
				effects := &retentionShadowEffects{realm: realm, maxBytes: maxBytes, maxDelay: 16 * time.Millisecond, clock: clock}
				key := journalKey(retentionWitness("%0", "matrix"))
				effects.run = func(_ context.Context, observer PaneObservationEffects) error {
					registry := observer.(*paneRegistry)
					witness := retentionWitness("%0", "matrix")
					if err := observer.AdmitPane(witness); err != nil {
						return err
					}
					decoder := controlmode.NewDecoder()
					for index, chunk := range chunks {
						if index > 0 {
							clock.Advance(time.Duration(workload.IntervalNS))
						}
						if err := feedRetentionOutput(observer, decoder, witness, chunk); err != nil {
							t.Logf("first failed chunk=%d error=%v accounting=%+v", index, err, registry.retention.progressSnapshot())
							return err
						}
					}
					return registry.retentionBoundary(witness, "terminal_handoff")
				}
				serveRetentionTrial(t, effects)
				effects.mu.Lock()
				feed := bytes.Join(effects.feeds, nil)
				lengthText := make([]string, len(effects.feeds))
				for index, item := range effects.feeds {
					lengthText[index] = strconv.Itoa(len(item))
				}
				records := len(effects.feeds)
				effects.mu.Unlock()
				if !bytes.Equal(feed, payload) {
					t.Fatalf("replay/model input mismatch")
				}
				committed, err := effects.realm.ReadCommitted(key)
				if err != nil || !bytes.Equal(committed, payload) {
					t.Fatalf("committed prefix mismatch: %v", err)
				}
				wantRecords, wantLengths := workload.Records64K, workload.Lengths64KSHA256
				if maxBytes == 256<<10 {
					wantRecords, wantLengths = workload.Records256K, workload.Lengths256KSHA256
				}
				if records != wantRecords {
					t.Fatalf("records=%d want=%d", records, wantRecords)
				}
				requireRetentionSHA(t, []byte(strings.Join(lengthText, ",")), wantLengths, "batch lengths")
				logical, allocated := retentionFileAccounting(t, runtimeDir)
				t.Logf("matrix_receipt arm_bytes=%d chunks=%d payload_bytes=%d batches=%d committed_bytes=%d logical_bytes=%d allocated_st_blocks_x512=%d", maxBytes, len(chunks), len(payload), records, len(committed), logical, allocated)
			})
		}
	}
}

func retentionScheduleBytes(chunks [][]byte, intervalNS int64) []byte {
	parts := make([]string, len(chunks))
	for index, chunk := range chunks {
		sum := sha256.Sum256(chunk)
		parts[index] = fmt.Sprintf("%d:%d:%x", int64(index)*intervalNS, len(chunk), sum)
	}
	return []byte(strings.Join(parts, "|"))
}

func requireRetentionSHA(t *testing.T, data []byte, want, label string) {
	t.Helper()
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("%s sha256=%s want=%s", label, got, want)
	}
}

func retentionWorkloadChunks(t *testing.T, workload retentionWorkload) [][]byte {
	t.Helper()
	switch workload.SourceKind {
	case "gate3c":
		if retentionEvidenceRoot == "" {
			t.Skip("optional captured trace: set PERSEA_RETENTION_EVIDENCE_ROOT")
		}
		stem := strings.TrimPrefix(workload.ID, "G3C-")
		stem = strings.ReplaceAll(stem, "mouse-focus", "mouse_focus")
		at := strings.LastIndexByte(stem, '-')
		stem = stem[:at] + "-run-" + stem[at+1:]
		root := filepath.Join(retentionEvidenceRoot, "2026-08-13_unified-terminal-gate3c")
		raw, err := os.ReadFile(filepath.Join(root, stem+".journal.bin"))
		if err != nil {
			t.Skipf("frozen Gate3C source unavailable: %v", err)
		}
		frameData, err := os.ReadFile(filepath.Join(root, stem+".frames.json"))
		if err != nil {
			t.Fatal(err)
		}
		var frames []struct {
			Start, End, Bytes int
			SHA256            string `json:"sha256"`
		}
		if err := json.Unmarshal(frameData, &frames); err != nil {
			t.Fatal(err)
		}
		chunks := make([][]byte, len(frames))
		for index, frame := range frames {
			if frame.Start < 0 || frame.End < frame.Start || frame.End > len(raw) || frame.End-frame.Start != frame.Bytes {
				t.Fatalf("bad frame %d", index)
			}
			chunks[index] = append([]byte(nil), raw[frame.Start:frame.End]...)
			requireRetentionSHA(t, chunks[index], frame.SHA256, "frame")
		}
		return chunks
	case "gate5":
		if retentionEvidenceRoot == "" {
			t.Skip("optional captured trace: set PERSEA_RETENTION_EVIDENCE_ROOT")
		}
		raw, err := os.ReadFile(filepath.Join(retentionEvidenceRoot, "2026-08-12_unified-terminal-gate5b", "workload.journal.bin"))
		if err != nil {
			t.Skipf("frozen Gate5 source unavailable: %v", err)
		}
		var chunks [][]byte
		for offset := 0; offset < len(raw); offset += 64 << 10 {
			end := offset + 64<<10
			if end > len(raw) {
				end = len(raw)
			}
			chunks = append(chunks, raw[offset:end])
		}
		return chunks
	case "ordinary-v1":
		chunks := make([][]byte, 0, 120)
		commands := []string{"pwd", "git status --short", "ls -1 ai_todo | head -5"}
		for index := 0; index < 120; index++ {
			text := fmt.Sprintf("\x1b[32moperator@lab\x1b[0m:\x1b[34m~/fixture_ops_init\x1b[0m$ %s\r\nordinary-%03d /workspace/examples/synthetic/shell-demo01\r\nstatus=%d elapsed=%.1fs\r\n", commands[index%3], index, index%4, float64(index)*0.1)
			chunks = append(chunks, retentionONLCR([]byte(text)))
		}
		return chunks
	case "build-v1":
		var chunks [][]byte
		var pending bytes.Buffer
		for index := 0; index < 30000; index++ {
			fmt.Fprintf(&pending, "[build] compile package_%03d/unit_%05d.go: ok cache=%d elapsed=%dms sha=%08x\r\n", index%240, index, index%7, index%997, index)
			if pending.Len() >= 65536 {
				chunks = append(chunks, retentionONLCR(pending.Bytes()))
				pending.Reset()
			}
		}
		if pending.Len() > 0 {
			chunks = append(chunks, retentionONLCR(pending.Bytes()))
		}
		return chunks
	case "repaint-v1":
		chunks := [][]byte{[]byte("\x1b[?1049h\x1b[?25l\x1b[2J")}
		for frame := 0; frame < 600; frame++ {
			var out bytes.Buffer
			out.WriteString("\x1b[H")
			for row := 0; row < 40; row++ {
				source := fmt.Sprintf(" frame=%04d row=%02d ", frame, row) + strings.Repeat("cpu=42% task=journal-replay | ", 4)
				if len(source) > 118 {
					source = source[:118]
				}
				source += strings.Repeat(" ", 118-len(source))
				if row%2 == 0 {
					out.WriteString("\x1b[38;5;39m")
				} else {
					out.WriteString("\x1b[38;5;214m")
				}
				out.WriteString(source)
				if row != 39 {
					out.WriteString("\r\n")
				}
			}
			chunks = append(chunks, retentionONLCR(out.Bytes()))
		}
		return append(chunks, retentionONLCR([]byte("\x1b[0m\x1b[?25h\x1b[?1049l")))
	case "sustained-v1":
		source := []byte(strings.Repeat("sustained-output-0123456789abcdef", 4))
		line := append(append([]byte(nil), source[:127]...), '\n')
		block := retentionONLCR(bytes.Repeat(line, 128))
		chunks := make([][]byte, 1024)
		for index := range chunks {
			chunks[index] = block
		}
		return chunks
	case "one-byte-fast-v1", "one-byte-sixty-v1":
		chunks := make([][]byte, workload.ChunkCount)
		for index := range chunks {
			chunks[index] = []byte{'x'}
		}
		return chunks
	default:
		t.Fatalf("unknown source kind %q", workload.SourceKind)
		return nil
	}
}

func retentionONLCR(input []byte) []byte {
	output := make([]byte, 0, len(input)+bytes.Count(input, []byte{'\n'}))
	start := 0
	for index, value := range input {
		if value != '\n' {
			continue
		}
		output = append(output, input[start:index]...)
		output = append(output, '\r', '\n')
		start = index + 1
	}
	return append(output, input[start:]...)
}

func TestRetentionDefaultRunRemainsFeatureOff(t *testing.T) {
	sentinel := errors.New("feature-off listener stop")
	listener := &retentionFeatureOffListener{err: sentinel}
	before := runtime.NumGoroutine()
	err := Serve(listener, config.Broker{Realm: "feature-off", FrontUID: uint32(os.Getuid()), FrontUIDConfigured: true})
	if !errors.Is(err, sentinel) {
		t.Fatalf("default Serve error = %v, want listener sentinel", err)
	}
	if listener.accepts != 1 {
		t.Fatalf("default Serve accept calls = %d, want 1", listener.accepts)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("default Serve constructed retained feature machinery: goroutines %d -> %d", before, after)
	}
}

type retentionFeatureOffListener struct {
	err     error
	accepts int
}

func (listener *retentionFeatureOffListener) Accept() (net.Conn, error) {
	listener.accepts++
	return nil, listener.err
}

func (*retentionFeatureOffListener) Close() error   { return nil }
func (*retentionFeatureOffListener) Addr() net.Addr { return retentionFeatureOffAddr("feature-off") }

type retentionFeatureOffAddr string

func (addr retentionFeatureOffAddr) Network() string { return string(addr) }
func (addr retentionFeatureOffAddr) String() string  { return string(addr) }

// Paused oversize closure, bounded failed tombstones, and reservation fault races.

// Close while a pane is paused with two held ceiling-sized deliveries must
// partition the drain exactly like pause_end; one concatenated oversize feed
// violates the accepted batch arm.
func TestRetentionMetaCloseDrainsPausedHeldWithinBatchCeiling(t *testing.T) {
	effects := &retentionShadowEffects{
		realm: openRetentionRealm(t, "close-paused-oversize"), maxBytes: 64 << 10,
		maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(),
	}
	registry := newPaneRegistry(effects)
	witness := retentionWitness("%close-paused", "close-paused-inc")
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	if err := registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationPause, Witness: witness, Label: "start"}); err != nil {
		t.Fatal(err)
	}
	decoder := controlmode.NewDecoder()
	for _, value := range []byte{'a', 'b'} {
		if err := feedRetentionOutput(registry, decoder, witness, bytes.Repeat([]byte{value}, 64<<10)); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	effects.mu.Lock()
	feeds := append([][]byte(nil), effects.feeds...)
	effects.mu.Unlock()
	lengths := make([]int, len(feeds))
	for index := range feeds {
		lengths[index] = len(feeds[index])
	}
	if len(feeds) != 2 || lengths[0] != 64<<10 || lengths[1] != 64<<10 {
		t.Fatalf("close-time paused drain feed sizes = %v, want [65536 65536]", lengths)
	}
	if feeds[0][0] != 'a' || feeds[1][0] != 'b' {
		t.Fatalf("close-time paused drain order = %q/%q", feeds[0][:1], feeds[1][:1])
	}
}

// A retired failed generation must release its runtime entry and P/Q credits,
// while the existing admitted-coordinate record keeps the exact incarnation
// sticky until a different incarnation is successfully admitted. Repeating
// at P=1 proves that neither runtime tombstones nor rejection ephemerals grow.
func TestRetentionMetaFailedGenerationRetirementBoundsTombstones(t *testing.T) {
	const failures = 32
	cleanups := make(chan struct{}, failures)
	effects := &retentionShadowEffects{
		realm: openRetentionRealm(t, "tombstone-bound"), maxBytes: 64 << 10,
		maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(), maxPanes: 1, maxCommands: 3,
		stage: func(operation string, _ unifiedjournal.PaneKey) error {
			if operation == "append" {
				return errors.New("tombstone pre-feed failure")
			}
			return nil
		},
		hook: func(point string, _ unifiedjournal.PaneKey) {
			if point == "after_cleanup" {
				select {
				case cleanups <- struct{}{}:
				default:
				}
			}
		},
	}
	registry := newPaneRegistry(effects)
	decoder := controlmode.NewDecoder()
	for index := 0; index < failures; index++ {
		witness := retentionWitness("%tomb", fmt.Sprintf("tomb-inc-%d", index))
		if err := registry.AdmitPane(witness); err != nil {
			t.Fatalf("different incarnation %d was not admitted after paired retirement: %v", index, err)
		}
		registry.mu.Lock()
		admitted := registry.admitted[routeCoordinateKey(witness)]
		registry.mu.Unlock()
		if admitted.failedIncarnation != "" {
			t.Fatalf("different successfully admitted incarnation %d retained marker %q", index, admitted.failedIncarnation)
		}
		if err := feedRetentionOutput(registry, decoder, witness, bytes.Repeat([]byte{'t'}, 64<<10)); err != nil {
			t.Fatal(err)
		}
		<-cleanups
		effects.waitObservation("storage_fault", index+1)
		for attempt := 0; attempt < 10000; attempt++ {
			registry.retention.mu.Lock()
			retained := len(registry.retention.generations)
			panes := registry.retention.pUsed
			commands := registry.retention.qUsed
			registry.retention.mu.Unlock()
			if retained == 0 && panes == 0 && commands == 0 {
				break
			}
			if attempt == 9999 {
				t.Fatalf("iteration %d retained runtime generations/P/Q = %d/%d/%d", index, retained, panes, commands)
			}
			runtime.Gosched()
		}
		if err := registry.AdmitPane(witness); !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Fatalf("same failed incarnation %d admission = %v, want ErrInvalidated", index, err)
		}
		if err := registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationDisconnect, Witness: witness}); !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Fatalf("same failed incarnation %d observation = %v, want ErrInvalidated", index, err)
		}
		replacement := retentionWitness("%tomb", fmt.Sprintf("observed-only-inc-%d", index))
		if err := registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationDisconnect, Witness: replacement}); err != nil {
			t.Fatalf("different unadmitted incarnation %d observation = %v", index, err)
		}
		if err := registry.AdmitPane(witness); !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Fatalf("observing replacement cleared failed incarnation %d: %v", index, err)
		}
		registry.mu.Lock()
		state := registry.admitted[routeCoordinateKey(witness)]
		eligible := registry.sessions[sessionKey(witness.Session)].router.UnifiedEligible(witness)
		coordinates := len(registry.admitted)
		registry.mu.Unlock()
		if state.failedIncarnation != witness.Incarnation || !eligible || coordinates != 1 {
			t.Fatalf("iteration %d marker/router/coordinates = %q/%t/%d", index, state.failedIncarnation, eligible, coordinates)
		}
	}
	if err := registry.Close(); err == nil {
		t.Fatal("Close lost classified storage failures")
	}
	registry.mu.Lock()
	coordinates := len(registry.admitted)
	registry.mu.Unlock()
	if coordinates != 0 {
		t.Fatalf("registry Close retained %d admitted-coordinate markers", coordinates)
	}
}

// A disconnect whose reservation snapshot predates the fault CAS must be
// rejected by a fresh lane-visible check before any router mutation. The
// before_fault_cas hook parks the winner until the adversarial ObservePane
// has passed reserve with a clean snapshot; after_ingress_reserve then admits
// the CAS and waits for it, so the observation continues toward the router
// only after the fault is authoritative.
func TestRetentionMetaReserveToRouterFaultRaceRejectsBeforeMutation(t *testing.T) {
	reserveDone := make(chan struct{})
	casDone := make(chan struct{})
	trapArmed := make(chan struct{})
	var casGate, reserveGate sync.Once
	effects := &retentionShadowEffects{
		realm: openRetentionRealm(t, "reserve-router-race"), maxBytes: 64 << 10,
		maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(),
		stage: func(operation string, _ unifiedjournal.PaneKey) error {
			if operation == "append" {
				return errors.New("race pre-feed failure")
			}
			return nil
		},
		hook: func(point string, _ unifiedjournal.PaneKey) {
			switch point {
			case "before_fault_cas":
				casGate.Do(func() { <-reserveDone })
			case "after_fault_cas_winner":
				select {
				case <-casDone:
				default:
					close(casDone)
				}
			case "after_ingress_reserve":
				select {
				case <-trapArmed:
					reserveGate.Do(func() {
						close(reserveDone)
						<-casDone
					})
				default:
				}
			}
		},
	}
	registry := newPaneRegistry(effects)
	witness := retentionWitness("%race-router", "race-router-inc")
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	if err := feedRetentionOutput(registry, controlmode.NewDecoder(), witness, bytes.Repeat([]byte{'r'}, 64<<10)); err != nil {
		t.Fatal(err)
	}
	// Only the adversarial disconnect below may spring the reserve trap; the
	// triggering feed above must pass its own reserve hook unimpeded.
	close(trapArmed)
	// The manager is parked in before_fault_cas until the adversarial
	// disconnect has reserved with a clean snapshot.
	err := registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationDisconnect, Witness: witness})
	if !errors.Is(err, unifiedjournal.ErrInvalidated) {
		t.Fatalf("post-CAS disconnect = %v, want ErrInvalidated", err)
	}
	registry.mu.Lock()
	eligible := registry.sessions[sessionKey(witness.Session)].router.UnifiedEligible(witness)
	registry.mu.Unlock()
	if !eligible {
		t.Fatal("post-fault ingress mutated router before lane-visible cutoff rejection")
	}
	if err := registry.Close(); err == nil {
		t.Fatal("Close lost classified storage failure")
	}
}

// Failed-coordinate authority has the same finite owner bound as live pane
// generations. With P=1, the first exact failure remains sticky; a second
// distinct failure exhausts that marker capacity and permanently fails the
// retained registry closed instead of growing another tombstone.
func TestRetentionMetaDistinctFailedCoordinatesTripBoundedBreaker(t *testing.T) {
	const attempts = 32
	cleanups := make(chan struct{}, attempts)
	effects := &retentionShadowEffects{
		realm: openRetentionRealm(t, "distinct-marker-bound"), maxBytes: 64 << 10,
		maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(), maxPanes: 1, maxCommands: 3,
		stage: func(operation string, _ unifiedjournal.PaneKey) error {
			if operation == "append" {
				return errors.New("distinct marker failure")
			}
			return nil
		},
		hook: func(point string, _ unifiedjournal.PaneKey) {
			if point == "after_cleanup" {
				cleanups <- struct{}{}
			}
		},
	}
	registry := newPaneRegistry(effects)
	decoder := controlmode.NewDecoder()
	witnesses := []controlmode.PaneWitness{
		retentionWitness("%distinct-0", "distinct-inc-0"),
		retentionWitness("%distinct-1", "distinct-inc-1"),
	}
	for index, witness := range witnesses {
		if err := registry.AdmitPane(witness); err != nil {
			t.Fatalf("admit distinct authoritative failure %d: %v", index, err)
		}
		if err := feedRetentionOutput(registry, decoder, witness, bytes.Repeat([]byte{'d'}, 64<<10)); err != nil {
			t.Fatal(err)
		}
		<-cleanups
		effects.waitObservation("storage_fault", index+1)
		waitRetentionRuntimeRetired(t, registry, index)
		registry.mu.Lock()
		coordinates := len(registry.admitted)
		failed := 0
		for _, state := range registry.admitted {
			if state.failedIncarnation != "" {
				failed++
			}
		}
		registry.mu.Unlock()
		if coordinates > effects.maxPanes || failed > effects.maxPanes {
			t.Fatalf("failure %d escaped F=P: coordinates=%d markers=%d P=%d", index, coordinates, failed, effects.maxPanes)
		}
	}

	registry.mu.Lock()
	sessionsBefore, panesBefore := len(registry.sessions), len(registry.panes)
	registry.mu.Unlock()
	for index := 2; index < attempts; index++ {
		candidate := retentionWitness(fmt.Sprintf("%%distinct-%d", index), fmt.Sprintf("distinct-inc-%d", index))
		if err := registry.AdmitPane(candidate); !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Fatalf("post-overflow admission %d = %v, want ErrInvalidated", index, err)
		}
		if err := registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: candidate, Data: []byte("late")}); !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Fatalf("post-overflow observation %d = %v, want ErrInvalidated", index, err)
		}
	}
	for index, witness := range witnesses {
		if err := registry.AdmitPane(witness); !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Fatalf("old failed admission %d after overflow = %v", index, err)
		}
		if err := registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationDisconnect, Witness: witness}); !errors.Is(err, unifiedjournal.ErrInvalidated) {
			t.Fatalf("old failed observation %d after overflow = %v", index, err)
		}
	}
	registry.mu.Lock()
	coordinates := len(registry.admitted)
	sessionsAfter, panesAfter := len(registry.sessions), len(registry.panes)
	broken, failed := registry.broken, registry.failed
	registry.mu.Unlock()
	if coordinates > effects.maxPanes || !broken || failed != 0 || sessionsAfter != sessionsBefore || panesAfter != panesBefore {
		t.Fatalf("post-overflow coordinates/broken/markers/sessions/panes = %d/%t/%d/%d/%d, before sessions/panes=%d/%d", coordinates, broken, failed, sessionsAfter, panesAfter, sessionsBefore, panesBefore)
	}
	waitRetentionRuntimeRetired(t, registry, attempts)
	if err := registry.Close(); err == nil {
		t.Fatal("Close lost distinct classified storage failures")
	}
	registry.mu.Lock()
	coordinates = len(registry.admitted)
	broken, failed = registry.broken, registry.failed
	registry.mu.Unlock()
	if coordinates != 0 || broken || failed != 0 {
		t.Fatalf("Close retained coordinates/breaker/markers = %d/%t/%d", coordinates, broken, failed)
	}
}

func waitRetentionRuntimeRetired(t *testing.T, registry *paneRegistry, iteration int) {
	t.Helper()
	for attempt := 0; attempt < 10000; attempt++ {
		registry.retention.mu.Lock()
		retained := len(registry.retention.generations)
		panes := registry.retention.pUsed
		commands := registry.retention.qUsed
		registry.retention.mu.Unlock()
		if retained == 0 && panes == 0 && commands == 0 {
			return
		}
		if attempt == 9999 {
			t.Fatalf("iteration %d retained runtime generations/P/Q = %d/%d/%d", iteration, retained, panes, commands)
		}
		runtime.Gosched()
	}
}

// An admission already queued on registry.mu before a first fault attempts
// publication must see the completed marker transaction before it can touch
// the live router. A successful transactional replacement may replace the old
// authority; a failed admission must leave it exact and eligible.
func TestRetentionMetaQueuedAdmissionCannotOvertakeFaultPublication(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		appendEntered := make(chan struct{})
		appendRelease := make(chan struct{})
		faultAttempted := make(chan struct{})
		var appendOnce, faultOnce sync.Once
		effects := &retentionShadowEffects{
			realm: openRetentionRealm(t, fmt.Sprintf("queued-fault-admit-%d", iteration)), maxBytes: 64 << 10,
			maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(), maxPanes: 2, maxCommands: 5,
			stage: func(operation string, _ unifiedjournal.PaneKey) error {
				if operation != "append" {
					return nil
				}
				appendOnce.Do(func() { close(appendEntered); <-appendRelease })
				return errors.New("queued fault")
			},
			hook: func(point string, _ unifiedjournal.PaneKey) {
				if point == "before_fault_cas" {
					faultOnce.Do(func() { close(faultAttempted) })
				}
			},
		}
		registry := newPaneRegistry(effects)
		old := retentionWitness("%queued-race", fmt.Sprintf("old-%d", iteration))
		replacement := retentionWitness("%queued-race", fmt.Sprintf("new-%d", iteration))
		if err := registry.AdmitPane(old); err != nil {
			t.Fatal(err)
		}
		if err := feedRetentionOutput(registry, controlmode.NewDecoder(), old, bytes.Repeat([]byte{'q'}, 64<<10)); err != nil {
			t.Fatal(err)
		}
		<-appendEntered

		registry.mu.Lock()
		admitDone := make(chan error, 1)
		go func() { admitDone <- registry.AdmitPane(replacement) }()
		newKey := journalKey(replacement)
		for {
			registry.retention.mu.Lock()
			created := registry.retention.generations[newKey] != nil
			registry.retention.mu.Unlock()
			if created {
				break
			}
			runtime.Gosched()
		}
		close(appendRelease)
		<-faultAttempted
		registry.mu.Unlock()
		admitErr := <-admitDone
		effects.waitObservation("storage_fault", 1)
		effects.waitObservation("fault_cleanup", 1)

		registry.mu.Lock()
		state, present := registry.admitted[routeCoordinateKey(old)]
		session := registry.sessions[sessionKey(old.Session)]
		oldEligible := session != nil && session.router.UnifiedEligible(old)
		newEligible := session != nil && session.router.UnifiedEligible(replacement)
		registry.mu.Unlock()
		if admitErr == nil {
			if !present || state.witness != replacement || state.failedIncarnation != "" || oldEligible || !newEligible {
				t.Fatalf("iteration %d successful replacement was not atomic: present=%t witness=%q marker=%q old_eligible=%t new_eligible=%t", iteration, present, state.witness.Incarnation, state.failedIncarnation, oldEligible, newEligible)
			}
		} else if !present || state.witness != old || state.failedIncarnation != old.Incarnation || !oldEligible || newEligible {
			t.Fatalf("iteration %d failed admission %v overtook fault publication: present=%t witness=%q marker=%q old_eligible=%t new_eligible=%t", iteration, admitErr, present, state.witness.Incarnation, state.failedIncarnation, oldEligible, newEligible)
		}
		_ = registry.Close()
	}
}

func TestRetentionMetaCapacityRejectedReplacementPreservesOldAuthority(t *testing.T) {
	appendEntered := make(chan struct{})
	appendRelease := make(chan struct{})
	var once sync.Once
	effects := &retentionShadowEffects{
		realm: openRetentionRealm(t, "capacity-rejected-replacement"), maxBytes: 64 << 10,
		maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(), maxPanes: 1, maxCommands: 3,
		stage: func(operation string, _ unifiedjournal.PaneKey) error {
			if operation == "append" {
				once.Do(func() { close(appendEntered); <-appendRelease })
				return errors.New("capacity replacement fault")
			}
			return nil
		},
	}
	registry := newPaneRegistry(effects)
	old := retentionWitness("%capacity-replace", "capacity-old")
	replacement := retentionWitness("%capacity-replace", "capacity-new")
	if err := registry.AdmitPane(old); err != nil {
		t.Fatal(err)
	}
	if err := feedRetentionOutput(registry, controlmode.NewDecoder(), old, bytes.Repeat([]byte{'c'}, 64<<10)); err != nil {
		t.Fatal(err)
	}
	<-appendEntered
	if err := registry.AdmitPane(replacement); !errors.Is(err, unifiedjournal.ErrInvalidated) {
		t.Fatalf("capacity-rejected replacement = %v, want ErrInvalidated", err)
	}
	registry.mu.Lock()
	state, present := registry.admitted[routeCoordinateKey(old)]
	oldEligible := registry.sessions[sessionKey(old.Session)].router.UnifiedEligible(old)
	registry.mu.Unlock()
	if !present || state.witness != old || state.failedIncarnation != "" || !oldEligible {
		t.Fatalf("capacity rejection mutated old authority: present=%t witness=%q marker=%q eligible=%t", present, state.witness.Incarnation, state.failedIncarnation, oldEligible)
	}
	close(appendRelease)
	effects.waitObservation("storage_fault", 1)
	effects.waitObservation("fault_cleanup", 1)
	registry.mu.Lock()
	state, present = registry.admitted[routeCoordinateKey(old)]
	oldEligible = registry.sessions[sessionKey(old.Session)].router.UnifiedEligible(old)
	registry.mu.Unlock()
	if !present || state.witness != old || state.failedIncarnation != old.Incarnation || !oldEligible {
		t.Fatalf("fault publication lost old authority after rejected replacement: present=%t witness=%q marker=%q eligible=%t", present, state.witness.Incarnation, state.failedIncarnation, oldEligible)
	}
	_ = registry.Close()
}

// ---------------------------------------------------------------------------
// ISSUE25 — geometry barrier release.
//
// The barrier is installed BEFORE the guarded command is issued, so every path
// out of that command — success, refusal, fault — has to release it. Nothing
// about a refused resize is exceptional: an operator is expected to click again
// after one. If a refusal left the hold in place, the pane would keep accreting
// held output until a reservation failed and the generation died, from a request
// the design calls "rejected without mutation". That silent stall is what these
// regression tests exist to catch.
// ---------------------------------------------------------------------------

const geometryBarrierRed = "ISSUE25/GEOMETRY_BARRIER/CANDIDATE_RED"

type geometryBarrierDownstream struct {
	mu        sync.Mutex
	events    []unifiedjournal.Event
	delivered chan struct{}
}

func (downstream *geometryBarrierDownstream) WritePane(unifiedjournal.PaneKey, []byte) error {
	return errors.New("unified feed range metadata is required")
}

func (downstream *geometryBarrierDownstream) WritePaneRange(key unifiedjournal.PaneKey, payload []byte, start, end, sequence int64) error {
	return downstream.record(unifiedjournal.Event{
		Kind: unifiedjournal.RecordOutput, Sequence: sequence, Start: start, End: end,
		Payload: append([]byte(nil), payload...),
	})
}

func (downstream *geometryBarrierDownstream) WritePaneGeometry(key unifiedjournal.PaneKey, event unifiedjournal.Event) error {
	return downstream.record(event)
}

func (downstream *geometryBarrierDownstream) record(event unifiedjournal.Event) error {
	downstream.mu.Lock()
	downstream.events = append(downstream.events, event)
	downstream.mu.Unlock()
	select {
	case downstream.delivered <- struct{}{}:
	default:
	}
	return nil
}

func (downstream *geometryBarrierDownstream) snapshot() []unifiedjournal.Event {
	downstream.mu.Lock()
	defer downstream.mu.Unlock()
	return append([]unifiedjournal.Event(nil), downstream.events...)
}

func (downstream *geometryBarrierDownstream) outputText() string {
	var builder strings.Builder
	for _, event := range downstream.snapshot() {
		if event.Kind == unifiedjournal.RecordOutput {
			builder.Write(event.Payload)
		}
	}
	return builder.String()
}

func (downstream *geometryBarrierDownstream) awaitOutput(t *testing.T, label, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if downstream.outputText() == want {
			return
		}
		select {
		case <-downstream.delivered:
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("%s %s: published=%q want=%q", geometryBarrierRed, label, downstream.outputText(), want)
}

func (downstream *geometryBarrierDownstream) requireOutputStaysAt(t *testing.T, label, want string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if got := downstream.outputText(); got != want {
			t.Fatalf("%s %s: barrier published %q, want it held at %q", geometryBarrierRed, label, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type geometryBarrierHarness struct {
	provider  *UnifiedDevPaneEffects
	registry  *paneRegistry
	issuer    *unifiedGeometryIssuer
	key       unifiedjournal.PaneKey
	down      *geometryBarrierDownstream
	realm     *unifiedjournal.Realm
	journalMu sync.Mutex
}

func newGeometryBarrierHarness(t *testing.T, label string) *geometryBarrierHarness {
	t.Helper()
	realm := openRetentionRealm(t, label)
	key := journalKey(retentionWitness("%0", label+"-incarnation"))
	if err := realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("%s admit pane: %v", geometryBarrierRed, err)
	}
	harness := &geometryBarrierHarness{
		key: key, realm: realm,
		down: &geometryBarrierDownstream{delivered: make(chan struct{}, 64)},
	}
	harness.provider = &UnifiedDevPaneEffects{
		realm: realm, active: map[string]unifiedjournal.PaneKey{"$0": key},
		subscribers: make(map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}),
	}
	registry := &paneRegistry{closeDone: make(chan struct{})}
	registry.retention = newRetentionTrialRuntimeWithDownstream(t, realm, &harness.journalMu, harness.down)
	harness.registry = registry
	harness.issuer = &unifiedGeometryIssuer{provider: harness.provider, registry: registry, session: "$0"}
	return harness
}

func newRetentionTrialRuntimeWithDownstream(t *testing.T, realm *unifiedjournal.Realm, journalMu *sync.Mutex, downstream *geometryBarrierDownstream) *retentionTrialRuntime {
	t.Helper()
	runtime := newRetentionTrialRuntime(geometryBarrierEffects{
		realm: realm, journalMu: journalMu, downstream: downstream,
	})
	if runtime == nil {
		t.Fatalf("%s retention runtime unavailable", geometryBarrierRed)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime
}

type geometryBarrierEffects struct {
	realm      *unifiedjournal.Realm
	journalMu  *sync.Mutex
	downstream *geometryBarrierDownstream
}

func (effects geometryBarrierEffects) RunObserver(ctx context.Context, _ PaneObservationEffects) error {
	<-ctx.Done()
	return ctx.Err()
}

func (effects geometryBarrierEffects) WritePane(key unifiedjournal.PaneKey, payload []byte) error {
	return effects.downstream.WritePane(key, payload)
}

func (effects geometryBarrierEffects) WritePaneRange(key unifiedjournal.PaneKey, payload []byte, start, end, sequence int64) error {
	return effects.downstream.WritePaneRange(key, payload, start, end, sequence)
}

func (effects geometryBarrierEffects) WritePaneGeometry(key unifiedjournal.PaneKey, event unifiedjournal.Event) error {
	return effects.downstream.WritePaneGeometry(key, event)
}

func (effects geometryBarrierEffects) BeginSessionBirth(string, string) (SessionBirthEffects, error) {
	return nil, nil
}

func (effects geometryBarrierEffects) retentionTrial() retentionTrialOptions {
	return retentionTrialOptions{
		realm: effects.realm, journalMu: effects.journalMu, maxBatchBytes: 64 << 10, maxFeedDelay: 16 * time.Millisecond,
		now: time.Now,
		after: func(delay time.Duration, fn func()) func() bool {
			timer := time.AfterFunc(delay, fn)
			return timer.Stop
		},
		observe: func(string, map[string]int64) {},
	}
}

func TestUnifiedGeometryBarrierReleasesOnRefusedResize(t *testing.T) {
	harness := newGeometryBarrierHarness(t, "geometry-barrier-refused")
	runtime := harness.registry.retention

	if err := runtime.WritePane(harness.key, []byte("before")); err != nil {
		t.Fatalf("%s write before: %v", geometryBarrierRed, err)
	}
	harness.down.awaitOutput(t, "pre-barrier", "before")

	ticket, err := harness.issuer.BeginGeometry(context.Background())
	if err != nil {
		t.Fatalf("%s begin geometry: %v", geometryBarrierRed, err)
	}
	for _, chunk := range []string{"held-1", "held-2", "held-3"} {
		if err := runtime.WritePane(harness.key, []byte(chunk)); err != nil {
			t.Fatalf("%s write %s: %v", geometryBarrierRed, chunk, err)
		}
	}
	harness.down.requireOutputStaysAt(t, "during barrier", "before", 300*time.Millisecond)

	// This is the whole point: the guarded command was refused, so the caller
	// returns an error and releases the ticket without ever committing.
	ticket.Release()
	harness.down.awaitOutput(t, "after refusal", "beforeheld-1held-2held-3")

	for _, event := range harness.down.snapshot() {
		if event.Kind == unifiedjournal.RecordGeometry {
			t.Fatalf("%s a refused resize committed a geometry event: %+v", geometryBarrierRed, event)
		}
	}
	events, err := harness.realm.ReadCommittedEvents(harness.key)
	if err != nil {
		t.Fatalf("%s read committed events: %v", geometryBarrierRed, err)
	}
	for _, event := range events {
		if event.Kind == unifiedjournal.RecordGeometry {
			t.Fatalf("%s a refused resize left durable geometry: %+v", geometryBarrierRed, event)
		}
	}

	// Releasing twice must not double-release the reservation or the hold.
	ticket.Release()

	// And a subsequent valid Fit must still succeed on the same generation.
	second, err := harness.issuer.BeginGeometry(context.Background())
	if err != nil {
		t.Fatalf("%s second begin geometry after a refusal: %v", geometryBarrierRed, err)
	}
	if err := runtime.WritePane(harness.key, []byte("after")); err != nil {
		t.Fatalf("%s write after: %v", geometryBarrierRed, err)
	}
	if err := second.Commit(context.Background(), 80, 43); err != nil {
		t.Fatalf("%s commit after a refusal: %v", geometryBarrierRed, err)
	}
	harness.down.awaitOutput(t, "after commit", "beforeheld-1held-2held-3after")

	published := harness.down.snapshot()
	geometryIndex, afterIndex := -1, -1
	for index, event := range published {
		if event.Kind == unifiedjournal.RecordGeometry && geometryIndex < 0 {
			geometryIndex = index
		}
		if event.Kind == unifiedjournal.RecordOutput && string(event.Payload) == "after" {
			afterIndex = index
		}
	}
	if geometryIndex < 0 || afterIndex < 0 || geometryIndex > afterIndex {
		t.Fatalf("%s geometry was not published before the output that follows it: geometry=%d after=%d events=%+v",
			geometryBarrierRed, geometryIndex, afterIndex, published)
	}
	if published[geometryIndex].Geometry != (unifiedjournal.Geometry{Columns: 80, Rows: 43}) {
		t.Fatalf("%s published geometry=%+v", geometryBarrierRed, published[geometryIndex].Geometry)
	}
}

func TestUnifiedGeometryBarrierOrdersCommittedGeometryBetweenOutput(t *testing.T) {
	harness := newGeometryBarrierHarness(t, "geometry-barrier-order")
	runtime := harness.registry.retention

	if err := runtime.WritePane(harness.key, []byte("old-output")); err != nil {
		t.Fatalf("%s write old: %v", geometryBarrierRed, err)
	}
	harness.down.awaitOutput(t, "old output", "old-output")

	ticket, err := harness.issuer.BeginGeometry(context.Background())
	if err != nil {
		t.Fatalf("%s begin geometry: %v", geometryBarrierRed, err)
	}
	// Output produced while the command is in flight is exactly what must land
	// after the committed geometry, never before it.
	if err := runtime.WritePane(harness.key, []byte("new-output")); err != nil {
		t.Fatalf("%s write new: %v", geometryBarrierRed, err)
	}
	if err := ticket.Commit(context.Background(), 80, 37); err != nil {
		t.Fatalf("%s commit: %v", geometryBarrierRed, err)
	}
	harness.down.awaitOutput(t, "released", "old-outputnew-output")

	published := harness.down.snapshot()
	var order []string
	for _, event := range published {
		if event.Kind == unifiedjournal.RecordGeometry {
			order = append(order, "GEOMETRY")
			continue
		}
		order = append(order, string(event.Payload))
	}
	want := []string{"old-output", "GEOMETRY", "new-output"}
	if len(order) != len(want) {
		t.Fatalf("%s published order=%v want=%v", geometryBarrierRed, order, want)
	}
	for index := range want {
		if order[index] != want[index] {
			t.Fatalf("%s published order=%v want=%v", geometryBarrierRed, order, want)
		}
	}

	// The same ordering has to be durable, not just live.
	events, err := harness.realm.ReadCommittedEvents(harness.key)
	if err != nil {
		t.Fatalf("%s read committed events: %v", geometryBarrierRed, err)
	}
	var durable []string
	for _, event := range events {
		if event.Kind == unifiedjournal.RecordGeometry {
			durable = append(durable, "GEOMETRY")
			continue
		}
		durable = append(durable, string(event.Payload))
	}
	if len(durable) != len(want) {
		t.Fatalf("%s durable order=%v want=%v", geometryBarrierRed, durable, want)
	}
	for index := range want {
		if durable[index] != want[index] {
			t.Fatalf("%s durable order=%v want=%v", geometryBarrierRed, durable, want)
		}
	}
}

// --- the resize transaction's own release obligation ------------------------

type recordingGeometryIssuer struct {
	issueErr error
	ticket   *recordingGeometryTicket
}

type recordingGeometryTicket struct {
	issued   int
	commits  int
	releases int
	issueErr error
}

func (issuer *recordingGeometryIssuer) BeginGeometry(context.Context) (geometryTicket, error) {
	issuer.ticket = &recordingGeometryTicket{issueErr: issuer.issueErr}
	return issuer.ticket, nil
}

func (ticket *recordingGeometryTicket) Issue(context.Context, []string) error {
	ticket.issued++
	return ticket.issueErr
}

func (ticket *recordingGeometryTicket) Commit(context.Context, int, int) error {
	ticket.commits++
	return nil
}

func (ticket *recordingGeometryTicket) Release() { ticket.releases++ }

// TestGuardedResizeAlwaysReleasesItsGeometryTicket drives the canonical resize
// transaction itself. It is the regression test for the mutant "barrier installed but
// never released on the resize-failure path": the transaction owns the release
// obligation, and no caller can be trusted to remember it.
func TestGuardedResizeAlwaysReleasesItsGeometryTicket(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux guarded resize")
	}
	disposable := newDisposable(t)
	server := config.TmuxServer{Label: "test", SocketPath: disposable.path}
	sessionID := disposable.run("list-sessions", "-f", "#{==:#{session_name},alpha}", "-F", "#{session_id}")
	authority, err := incarnation(server)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := details(server, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = "test", server.Label, detail.ID, detail.Created
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	witness, err := buildSourceWitness(ctx, server, "", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(shortTempDir(t), "attachment-pty")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	for _, item := range []struct {
		name     string
		issueErr error
		rows     int
	}{
		{name: "refused command", issueErr: errors.New("guard refused"), rows: witness.Rows + 5},
		{name: "witness mismatch after command", rows: witness.Rows + 5},
	} {
		t.Run(item.name, func(t *testing.T) {
			issuer := &recordingGeometryIssuer{issueErr: item.issueErr}
			txn := &tmuxPinnedTransaction{server: server, authority: authority, pty: newDelayedPTY(), issuer: issuer}
			ids := terminal.AttachmentIDs{ShadowSessionID: "$999", ClientID: "client-geometry-release"}
			txn.client = &attachmentClient{ids: ids, file: file}
			_, err := txn.resize(ctx, terminal.TransactionRequest{
				Action: terminal.ActionResize, Witness: witness, Attachment: ids,
				Columns: witness.Columns, Rows: item.rows,
			})
			if err == nil {
				t.Fatalf("%s a resize that could not complete reported success", geometryBarrierRed)
			}
			if issuer.ticket == nil {
				t.Fatalf("%s no geometry ticket was taken", geometryBarrierRed)
			}
			if issuer.ticket.releases != 1 {
				t.Fatalf("%s ticket released %d times on the %s path, want exactly 1", geometryBarrierRed, issuer.ticket.releases, item.name)
			}
			if issuer.ticket.commits != 0 {
				t.Fatalf("%s a failed resize committed geometry %d times", geometryBarrierRed, issuer.ticket.commits)
			}
			after, err := buildSourceWitness(ctx, server, "", sessionID)
			if err != nil || after.Columns != witness.Columns || after.Rows != witness.Rows {
				t.Fatalf("%s a failed resize moved the geometry to %dx%d: %v", geometryBarrierRed, after.Columns, after.Rows, err)
			}
		})
	}
}

// TestGuardedResizeFailurePathsReleaseHeldOutputExactlyOnce is receipt C.
//
// The earlier release test proved the transaction calls Release exactly once on
// each failure path, using a recording ticket. That is necessary and not
// sufficient: it says nothing about the pane bytes the real barrier was holding
// while the command was in flight. This drives the real retention barrier, the
// real journal, and the real tmuxPinnedTransaction, and asks the question that
// actually matters to an operator — after a resize that did not happen, does my
// terminal keep printing?
//
// Both reachable post-barrier failures are covered:
//
//	command error      the issuer's write to the observer connection fails
//	witness mismatch   the command "succeeded" but the geometry did not move,
//	                   which is exactly what a refused guard looks like on a
//	                   control connection, where no exit status comes back
//
// In both, held output must be published exactly once, in the order the pane
// wrote it, with one release and no deadlock, and no geometry may be committed.
func TestGuardedResizeFailurePathsReleaseHeldOutputExactlyOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux guarded resize failure paths")
	}
	for _, item := range []struct {
		name     string
		issueErr error
	}{
		{name: "command error on the observer connection", issueErr: errors.New("observer command failed")},
		{name: "witness mismatch after an apparently clean command", issueErr: nil},
	} {
		t.Run(item.name, func(t *testing.T) {
			harness := newGeometryBarrierHarness(t, "barrier-failure-"+strings.ReplaceAll(item.name, " ", "-"))
			runtime := harness.registry.retention

			// Output published before the barrier exists.
			for _, chunk := range []string{"pre-1", "pre-2"} {
				if err := runtime.WritePane(harness.key, []byte(chunk)); err != nil {
					t.Fatalf("%s write %s: %v", geometryBarrierRed, chunk, err)
				}
			}
			harness.down.awaitOutput(t, "pre-barrier", "pre-1pre-2")

			disposable := newDisposable(t)
			server := config.TmuxServer{Label: "test", SocketPath: disposable.path}
			sessionID := disposable.run("list-sessions", "-f", "#{==:#{session_name},alpha}", "-F", "#{session_id}")
			authority, err := incarnation(server)
			if err != nil {
				t.Fatal(err)
			}
			detail, err := details(server, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = "test", server.Label, detail.ID, detail.Created
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			witness, err := buildSourceWitness(ctx, server, "", sessionID)
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.CreateTemp(shortTempDir(t), "attachment-pty")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()

			// The real issuer and the real ticket. Only the act of writing to the
			// observer connection is replaced, because this test has no observer:
			// a nil issueErr means "the command completed and changed nothing",
			// which is byte-for-byte what tmux reports for a refused guard.
			issuer := &barrierFailureIssuer{
				inner:    &unifiedGeometryIssuer{provider: harness.provider, registry: harness.registry, session: "$0"},
				issueErr: item.issueErr,
				held: func() {
					for _, chunk := range []string{"held-1", "held-2", "held-3"} {
						if err := runtime.WritePane(harness.key, []byte(chunk)); err != nil {
							t.Errorf("%s write %s while held: %v", geometryBarrierRed, chunk, err)
						}
					}
					harness.down.requireOutputStaysAt(t, "while the command is in flight", "pre-1pre-2", 250*time.Millisecond)
				},
			}
			txn := &tmuxPinnedTransaction{server: server, authority: authority, pty: newDelayedPTY(), issuer: issuer}
			ids := terminal.AttachmentIDs{ShadowSessionID: "$999", ClientID: "client-barrier-failure"}
			txn.client = &attachmentClient{ids: ids, file: file}

			_, resizeErr := txn.resize(ctx, terminal.TransactionRequest{
				Action: terminal.ActionResize, Witness: witness, Attachment: ids,
				Columns: witness.Columns, Rows: witness.Rows + 7,
			})
			if resizeErr == nil {
				t.Fatalf("%s a resize that never took effect reported success", geometryBarrierRed)
			}
			if issuer.begins != 1 {
				t.Fatalf("%s barrier installed %d times, want exactly 1", geometryBarrierRed, issuer.begins)
			}

			// The whole point: held bytes are published, exactly once, in the
			// order the pane wrote them, without any further prompting.
			harness.down.awaitOutput(t, "after the failure", "pre-1pre-2held-1held-2held-3")

			// awaitOutput above already required the concatenation to equal
			// "pre-1pre-2held-1held-2held-3" exactly, which is what proves both
			// order and exactly-once: a duplicate or a reordering changes that
			// string. The runtime batches components into feeds, so the count of
			// downstream events is a property of batching, not of the barrier,
			// and is deliberately not asserted. What is asserted is that nothing
			// geometric rode along and that no chunk was delivered twice.
			published := harness.down.snapshot()
			text := harness.down.outputText()
			for _, event := range published {
				if event.Kind == unifiedjournal.RecordGeometry {
					t.Fatalf("%s a failed resize published a geometry event: %+v", geometryBarrierRed, event)
				}
			}
			for _, chunk := range []string{"pre-1", "pre-2", "held-1", "held-2", "held-3"} {
				if count := strings.Count(text, chunk); count != 1 {
					t.Fatalf("%s chunk %q was published %d times: %q", geometryBarrierRed, chunk, count, text)
				}
			}
			events, err := harness.realm.ReadCommittedEvents(harness.key)
			if err != nil {
				t.Fatalf("%s read committed events: %v", geometryBarrierRed, err)
			}
			for _, event := range events {
				if event.Kind == unifiedjournal.RecordGeometry {
					t.Fatalf("%s a failed resize left durable geometry: %+v", geometryBarrierRed, event)
				}
			}
			after, err := buildSourceWitness(ctx, server, "", sessionID)
			if err != nil || after.Columns != witness.Columns || after.Rows != witness.Rows {
				t.Fatalf("%s a failed resize moved the geometry to %dx%d: %v", geometryBarrierRed, after.Columns, after.Rows, err)
			}

			// The generation is still usable: a later valid Fit commits and its
			// geometry is ordered after everything released above.
			recovery, err := issuer.inner.BeginGeometry(ctx)
			if err != nil {
				t.Fatalf("%s the generation could not take a second barrier: %v", geometryBarrierRed, err)
			}
			if err := runtime.WritePane(harness.key, []byte("after")); err != nil {
				t.Fatalf("%s write after: %v", geometryBarrierRed, err)
			}
			if err := recovery.Commit(ctx, witness.Columns, witness.Rows+7); err != nil {
				t.Fatalf("%s the generation could not commit after a failure: %v", geometryBarrierRed, err)
			}
			harness.down.awaitOutput(t, "after recovery", "pre-1pre-2held-1held-2held-3after")
			recovered := harness.down.snapshot()
			geometryAt, afterAt := -1, -1
			for index, event := range recovered {
				if event.Kind == unifiedjournal.RecordGeometry && geometryAt < 0 {
					geometryAt = index
				}
				if event.Kind == unifiedjournal.RecordOutput && string(event.Payload) == "after" {
					afterAt = index
				}
			}
			if geometryAt < 0 || afterAt < 0 || geometryAt > afterAt {
				t.Fatalf("%s recovery geometry was not ordered before the output that follows it: geometry=%d after=%d",
					geometryBarrierRed, geometryAt, afterAt)
			}
		})
	}
}

// barrierFailureIssuer wraps the real issuer so the test can write pane output
// while the guarded command is nominally in flight, and can fail that command
// without needing a live observer connection.
type barrierFailureIssuer struct {
	inner    *unifiedGeometryIssuer
	issueErr error
	held     func()
	begins   int
}

func (issuer *barrierFailureIssuer) BeginGeometry(ctx context.Context) (geometryTicket, error) {
	ticket, err := issuer.inner.BeginGeometry(ctx)
	if err != nil {
		return nil, err
	}
	issuer.begins++
	return &barrierFailureTicket{inner: ticket, issuer: issuer}, nil
}

type barrierFailureTicket struct {
	inner  geometryTicket
	issuer *barrierFailureIssuer
}

func (ticket *barrierFailureTicket) Issue(ctx context.Context, args []string) error {
	if ticket.issuer.held != nil {
		ticket.issuer.held()
	}
	return ticket.issuer.issueErr
}

func (ticket *barrierFailureTicket) Commit(ctx context.Context, columns, rows int) error {
	return ticket.inner.Commit(ctx, columns, rows)
}

func (ticket *barrierFailureTicket) Release() { ticket.inner.Release() }
