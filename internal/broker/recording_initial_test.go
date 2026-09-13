package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

// Filesystem-only provider fixtures still supply the same complete source
// identity required at production admission; no tmux process is contacted.
func recordingSourceForTest(witness controlmode.PaneWitness) terminal.SourceWitness {
	return terminal.SourceWitness{
		Socket:    terminal.SocketIdentity{Path: "/synthetic/recording.sock", Device: 1, Inode: 2},
		Server:    terminal.ProcessWitness{PID: 101, StartTime: 102},
		SessionID: witness.Session.Session, WindowID: witness.Window, PaneID: witness.Pane,
		Pane:        terminal.ProcessWitness{PID: 201, StartTime: 202},
		Incarnation: witness.Incarnation, Columns: 80, Rows: 24,
	}
}

func reserveRecordingSourceForTest(t *testing.T, effects *UnifiedDevPaneEffects, key unifiedjournal.PaneKey) {
	t.Helper()
	witness := controlmode.PaneWitness{Session: controlmode.SessionWitness{Server: key.Server, Session: key.Session, ControlGeneration: key.ControlGeneration}, Window: key.Window, Pane: key.Pane, Incarnation: key.Incarnation}
	if err := effects.reserveJournalSource(key, recordingSourceForTest(witness)); err != nil {
		t.Fatal(err)
	}
}

func commitRecordingInitialForTest(t *testing.T, registry *paneRegistry, witness controlmode.PaneWitness, payload []byte) *recordingInitialOperation {
	t.Helper()
	var geometry unifiedjournal.Geometry
	var err error
	registry.retention.withJournalLock(func() { geometry, err = registry.retention.options.realm.InitialGeometry(journalKey(witness)) })
	if err != nil {
		t.Fatal(err)
	}
	op, err := registry.beginInitial(recordingInitialBirth, witness, recordingSourceForTest(witness), geometry, payload)
	if err != nil {
		t.Fatal(err)
	}
	<-op.done
	if err := registry.publishInitial(op); err != nil {
		t.Fatal(err)
	}
	dispatched, err := registry.retention.startDispatchFence()
	if err != nil {
		t.Fatal(err)
	}
	<-dispatched
	return op
}

func prepareRecordingProviderForTest(t *testing.T, effects *UnifiedDevPaneEffects, keys ...unifiedjournal.PaneKey) {
	t.Helper()
	registry := newPaneRegistry(effects)
	effects.observer = registry
	t.Cleanup(func() { _ = registry.Close() })
	for _, key := range keys {
		witness := controlmode.PaneWitness{Session: controlmode.SessionWitness{Server: key.Server, Session: key.Session, ControlGeneration: key.ControlGeneration}, Window: key.Window, Pane: key.Pane, Incarnation: key.Incarnation}
		if err := registry.AdmitPane(witness); err != nil {
			t.Fatal(err)
		}
		commitRecordingInitialForTest(t, registry, witness, nil)
	}
}

// These two tests preserve the real adoption and creation witnesses from the
// baseline falsifiers. Their hold is a storage dependency, not a timing guess.
func TestRecordingFalsifierAdoptionWaitsForInitialCommit(t *testing.T) {
	f := newAdoptionFixture(t, 0)
	session := f.startPaneCommand(t, "recording-initial", "printf 'synthetic initial state\\n'; exec sleep 600")
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	f.registry.retention.options.stage = func(stage string, _ unifiedjournal.PaneKey) error {
		if stage == "append" {
			once.Do(func() { close(entered) })
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { _, err := f.effects.AdoptSession(context.Background(), session); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("initial append did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("adoption completed while initial append held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, _, _, cancel, err := openSnapshotTailForTest(t, f.effects, session); err == nil {
		cancel()
		t.Fatal("initializing generation opened a snapshot")
	}
	var openers sync.WaitGroup
	for i := 0; i < 8; i++ {
		openers.Add(1)
		go func() {
			defer openers.Done()
			if _, _, _, stop, err := openSnapshotTailForTest(t, f.effects, session); err == nil {
				stop()
				t.Error("concurrent initializing reopen succeeded")
			}
		}()
	}
	openers.Wait()
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adoption did not settle")
	}
}

func TestRecordingFalsifierCreationWaitsForInitialCommit(t *testing.T) {
	realm := openRetentionRealm(t, "initial-birth")
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	effects := &retentionShadowEffects{realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(), stage: func(stage string, _ unifiedjournal.PaneKey) error {
		if stage == "append" {
			once.Do(func() { close(entered) })
			<-release
		}
		return nil
	}}
	registry := newPaneRegistry(effects)
	defer func() { unblock(); _ = registry.Close() }()
	witness := retentionWitness("%birth", "synthetic-birth")
	if err := realm.AdmitPane(journalKey(witness), unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	owner := &UnifiedDevPaneEffects{observer: registry, realm: realm, active: make(map[string]unifiedjournal.PaneKey)}
	birth := &unifiedDevBirth{owner: owner, witness: witness, ready: make(chan struct{}), pending: []controlmode.Observation{{Kind: controlmode.ObservationOutput, Witness: witness, Data: bytes.Repeat([]byte{'b'}, 64<<10)}}}
	owner.units = map[string]*unifiedDevUnit{witness.Session.Session: {holder: birth, done: make(chan struct{})}}
	close(birth.ready)
	done := make(chan error, 1)
	go func() { done <- birth.CommitSessionBirth(witness.Session.Session) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("initial append did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("creation completed while initial append held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("creation did not settle")
	}
}

// A real journal, registry, manager, and dispatcher; only the external tmux
// source is absent in these operation/ownership tests.
func newRecordingInitialFixture(t *testing.T, configure ...func(*UnifiedDevPaneEffects)) (*UnifiedDevPaneEffects, *paneRegistry, controlmode.PaneWitness) {
	t.Helper()
	effects := newProjectionEffects(t, 8)
	for _, apply := range configure {
		apply(effects)
	}
	registry := newPaneRegistry(effects)
	effects.observer = registry
	witness := retentionWitness("%initial", "initial-source")
	witness.Session.Server = effects.server.Label
	reserveRecordingSourceForTest(t, effects, journalKey(witness))
	effects.journalMu.Lock()
	err := effects.realm.AdmitPane(journalKey(witness), unifiedjournal.Geometry{Columns: 80, Rows: 24})
	effects.journalMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return effects, registry, witness
}

func startRecordingInitialForTest(t *testing.T, registry *paneRegistry, witness controlmode.PaneWitness, payload []byte) *recordingInitialOperation {
	t.Helper()
	op, err := registry.beginInitial(recordingInitialBirth, witness, recordingSourceForTest(witness), unifiedjournal.Geometry{Columns: 80, Rows: 24}, payload)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestRecordingFalsifierLaterBoundaryIsNotInitialReceipt(t *testing.T) {
	for _, stage := range []string{"append", "sync", "commit"} {
		for failedChunk := 1; failedChunk <= 3; failedChunk++ {
			t.Run(fmt.Sprintf("%s/chunk-%d", stage, failedChunk), func(t *testing.T) {
				effects, registry, witness := newRecordingInitialFixture(t)
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				count := 0
				registry.retention.options.stage = func(got string, _ unifiedjournal.PaneKey) error {
					if got == stage {
						count++
						if count == failedChunk {
							close(entered)
							<-release
							return errors.New("required initial chunk failed")
						}
					}
					return nil
				}
				op := startRecordingInitialForTest(t, registry, witness, bytes.Repeat([]byte("x"), 2*(64<<10)+7))
				select {
				case <-entered:
				case <-time.After(2 * time.Second):
					t.Fatal("required stage did not start")
				}
				if _, err := op.result(); err == nil {
					t.Fatal("held required chunk produced a receipt")
				}
				if registry.retention.initialReady(journalKey(witness)) {
					t.Fatal("held operation became ready")
				}
				later, err := registry.retention.startBoundary(journalKey(witness), "explicit_flush", false)
				if err != nil {
					t.Fatal(err)
				}
				unblock()
				<-op.done
				if err := <-later; err != nil {
					t.Fatalf("generic empty boundary changed semantics: %v", err)
				}
				if _, err := op.result(); err == nil {
					t.Fatal("later boundary redeemed the failed initial operation")
				}
				if err := registry.publishInitial(op); err == nil {
					t.Fatal("failed initial state published")
				}
				effects.journalMu.Lock()
				sequence := effects.realm.CommittedSequence(journalKey(witness))
				effects.journalMu.Unlock()
				if sequence != int64(failedChunk-1) {
					t.Fatalf("verified prefix=%d want %d", sequence, failedChunk-1)
				}
			})
		}
	}
}

func TestRecordingInitialReceiptRechecksExactCurrentAuthority(t *testing.T) {
	for _, change := range []string{"cancel", "deadline", "transport", "generation", "operation", "witness", "failure", "coverage", "source", "geometry", "sequence"} {
		t.Run(change, func(t *testing.T) {
			_, registry, witness := newRecordingInitialFixture(t)
			op := startRecordingInitialForTest(t, registry, witness, []byte("complete state"))
			<-op.done
			if _, err := op.result(); err != nil {
				t.Fatal(err)
			}
			runtime := registry.retention
			switch change {
			case "cancel":
				op.cancel()
			case "deadline":
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				op.bindContext(ctx)
			case "transport":
				unit := &unifiedDevUnit{done: make(chan struct{})}
				op.bindOwner(unit)
				unit.readFailed.Store(true)
			case "generation":
				runtime.mu.Lock()
				old := runtime.generations[journalKey(witness)]
				copy := *old
				runtime.generations[journalKey(witness)] = &copy
				runtime.mu.Unlock()
				defer func() { runtime.mu.Lock(); runtime.generations[journalKey(witness)] = old; runtime.mu.Unlock() }()
			case "operation":
				runtime.mu.Lock()
				old := op.generation.initial
				op.generation.initial = &recordingInitialOperation{runtime: runtime, generation: op.generation}
				runtime.mu.Unlock()
				defer func() { runtime.mu.Lock(); op.generation.initial = old; runtime.mu.Unlock() }()
			case "witness":
				registry.mu.Lock()
				state := registry.admitted[routeCoordinateKey(witness)]
				state.witness.Incarnation = "replacement"
				registry.admitted[routeCoordinateKey(witness)] = state
				registry.mu.Unlock()
			case "failure":
				runtime.mu.Lock()
				op.generation.failed = true
				runtime.mu.Unlock()
			case "coverage":
				runtime.mu.Lock()
				op.receipt.chunks++
				runtime.mu.Unlock()
			case "source":
				runtime.mu.Lock()
				op.receipt.source.PaneID = "replacement"
				runtime.mu.Unlock()
			case "geometry":
				runtime.mu.Lock()
				op.receipt.geometry.Rows++
				runtime.mu.Unlock()
			case "sequence":
				runtime.mu.Lock()
				op.receipt.sequence++
				runtime.mu.Unlock()
			}
			if err := registry.publishInitial(op); err == nil {
				t.Fatalf("%s allowed stale success", change)
			}
		})
	}
}

// This faults real on-disk bytes after the broker adapter's append/sync and
// before AdvanceCommitted. No stage hook supplies the error: journal readback
// must detect it, the adapter must propagate it, and the operation must refuse.
func TestRecordingInitialActualAdapterRejectsVerificationFailure(t *testing.T) {
	effects, registry, witness := newRecordingInitialFixture(t)
	corrupted := false
	registry.retention.options.stage = func(stage string, _ unifiedjournal.PaneKey) error {
		if stage != "commit" {
			return nil
		}
		files, err := rotationJournalFiles(effects.dev.RuntimeDir)
		if err != nil {
			return err
		}
		if len(files) != 1 {
			return fmt.Errorf("journal files=%d", len(files))
		}
		file, err := os.OpenFile(files[0], os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		_, err = file.WriteAt([]byte{'!'}, info.Size()-1)
		corrupted = err == nil
		return err
	}
	op := startRecordingInitialForTest(t, registry, witness, []byte("verified state"))
	<-op.done
	if !corrupted {
		t.Fatal("actual journal corruption did not complete")
	}
	if _, err := op.result(); err == nil {
		t.Fatal("corrupt real journal produced an initial receipt")
	}
	if err := registry.publishInitial(op); err == nil {
		t.Fatal("verification failure published readiness")
	}
	registry.retention.withJournalLock(func() {
		if sequence := effects.realm.CommittedSequence(journalKey(witness)); sequence != 0 {
			t.Errorf("corrupt initial journal published sequence %d", sequence)
		}
	})
	effects.subscriberMu.Lock()
	got := effects.publishedSequence[journalKey(witness)]
	effects.subscriberMu.Unlock()
	if got != 0 {
		t.Fatalf("corrupt initial journal reached subscriber feed at %d", got)
	}
}

func TestRecordingAdoptionLosesInterestOrSourceWhileInitialHeld(t *testing.T) {
	for _, failure := range []string{"cancel", "deadline", "unit death", "source replacement"} {
		t.Run(failure, func(t *testing.T) {
			f := newAdoptionFixture(t, 0)
			session := f.startPaneCommand(t, "held-source", "printf 'initial state\\n'; exec sleep 600")
			entered, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			f.registry.retention.options.stage = func(stage string, _ unifiedjournal.PaneKey) error {
				if stage == "append" {
					once.Do(func() { close(entered); <-release })
				}
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			if failure == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
			}
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := f.effects.AdoptSession(ctx, session); done <- err }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("initial append did not enter")
			}
			var op *recordingInitialOperation
			f.registry.retention.mu.Lock()
			for _, generation := range f.registry.retention.generations {
				if generation.key.Session == session {
					op = generation.initial
				}
			}
			f.registry.retention.mu.Unlock()
			if op == nil {
				t.Fatal("held generation has no initial operation")
			}
			switch failure {
			case "cancel":
				cancel()
			case "deadline":
				<-ctx.Done()
			case "unit death":
				pid, err := strconv.Atoi(strings.TrimSpace(f.disposable.run("list-clients", "-t", session, "-F", "#{client_pid}")))
				if err != nil {
					t.Fatal(err)
				}
				process, err := os.FindProcess(pid)
				if err != nil {
					t.Fatal(err)
				}
				if err := process.Kill(); err != nil {
					t.Fatal(err)
				}
			case "source replacement":
				f.disposable.run("respawn-pane", "-k", "-t", session+":", "exec sleep 600")
			}
			f.registry.retention.mu.Lock()
			refs := op.generation.refs
			f.registry.retention.mu.Unlock()
			if refs == 0 || f.journalFileCount(t) != 1 {
				t.Fatal("held work lost its references or provisional journal")
			}
			if _, _, _, stop, err := openSnapshotTailForTest(t, f.effects, session); err == nil {
				stop()
				t.Fatal("failed pending adoption opened a snapshot")
			}
			unblock()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("failed pending adoption returned success")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("failed adoption did not settle")
			}
			pollUntil(t, 5*time.Second, "provisional adoption settlement", func() bool { return f.journalFileCount(t) == 0 })
			f.registry.retention.mu.Lock()
			published, refs := op.published, op.generation.refs
			f.registry.retention.mu.Unlock()
			if published || refs != 0 {
				t.Fatalf("late readiness or unsettled ownership: published=%v refs=%d", published, refs)
			}
		})
	}
}

func TestRecordingInitialWaitDecodesWhileStorageHeld(t *testing.T) {
	effects, registry, witness := newRecordingInitialFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	registry.retention.options.stage = func(stage string, _ unifiedjournal.PaneKey) error {
		if stage == "append" {
			once.Do(func() { close(entered); <-release })
		}
		return nil
	}
	op := startRecordingInitialForTest(t, registry, witness, []byte("\x1b["))
	<-entered
	holder := &unifiedDevBirth{owner: effects, witness: witness, streaming: true, initial: op}
	effects.panes[witness.Pane] = holder
	unit := &unifiedDevUnit{owner: effects, holder: holder, done: make(chan struct{}), birth: unifiedDevCommand{}}
	read, readErr := make(chan []byte), make(chan error)
	done := make(chan error, 1)
	go func() { done <- unit.awaitInitial(context.Background(), controlmode.NewDecoder(), read, readErr, op) }()
	// Split the tmux control record itself, while also completing an ANSI
	// prefix that belongs to the initial byte stream.
	read <- []byte("%out")
	read <- []byte("put " + witness.Pane + " 31mcontinued\\033[0m\n")
	pollUntil(t, time.Second, "decode behind held initial append", func() bool {
		registry.retention.mu.Lock()
		defer registry.retention.mu.Unlock()
		for _, command := range queuedRetentionCommands(&registry.retention.queue) {
			if command.kind == retentionCommandOutput {
				return true
			}
		}
		return false
	})
	select {
	case err := <-done:
		t.Fatalf("wait completed early: %v", err)
	default:
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := registry.retention.Boundary(journalKey(witness), "explicit_flush"); err != nil {
		t.Fatal(err)
	}
	effects.journalMu.Lock()
	events, err := effects.realm.ReadCommittedEvents(journalKey(witness))
	effects.journalMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var payload []byte
	for i, event := range events {
		if event.Sequence != int64(i+1) {
			t.Fatal("nonconsecutive sequence")
		}
		payload = append(payload, event.Payload...)
	}
	if string(payload) != "\x1b[31mcontinued\x1b[0m" {
		t.Fatalf("split sequence lost or duplicated: %q", payload)
	}
}

func TestRecordingInitialCancellationRetainsDispatcherOwnership(t *testing.T) {
	for _, closing := range []bool{false, true} {
		t.Run(fmt.Sprintf("close-wins-%v", closing), func(t *testing.T) {
			recordingInitialCancellationRetainsDispatcherOwnership(t, closing)
		})
	}
}

func TestRecordingInitialCancellationKeepsDecodingAcceptedStream(t *testing.T) {
	effects, registry, witness := newRecordingInitialFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	registry.retention.options.stage = func(stage string, _ unifiedjournal.PaneKey) error {
		if stage == "append" {
			once.Do(func() { close(entered); <-release })
		}
		return nil
	}
	op := startRecordingInitialForTest(t, registry, witness, []byte("initial"))
	<-entered
	holder := &unifiedDevBirth{owner: effects, witness: witness, streaming: true, initial: op}
	effects.panes[witness.Pane] = holder
	unit := &unifiedDevUnit{owner: effects, holder: holder, done: make(chan struct{})}
	read, readErr := make(chan []byte), make(chan error)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- unit.awaitInitial(ctx, controlmode.NewDecoder(), read, readErr, op) }()
	cancel()
	pollUntil(t, time.Second, "cancelled readiness", func() bool {
		registry.retention.mu.Lock()
		defer registry.retention.mu.Unlock()
		return op.cancelled
	})
	for _, chunk := range []string{"%out", "put " + witness.Pane + " continuation\\033[0m\n"} {
		select {
		case read <- []byte(chunk):
		case <-time.After(time.Second):
			t.Fatal("cancelled observer stopped consuming its control stream")
		}
	}
	pollUntil(t, time.Second, "decoded continuation after cancellation", func() bool {
		registry.retention.mu.Lock()
		defer registry.retention.mu.Unlock()
		for _, command := range queuedRetentionCommands(&registry.retention.queue) {
			if command.kind == retentionCommandOutput {
				return true
			}
		}
		return false
	})
	unblock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := registry.publishInitial(op); err == nil {
		t.Fatal("cancelled stream produced late readiness")
	}
}

func recordingInitialCancellationRetainsDispatcherOwnership(t *testing.T, closing bool) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	_, registry, witness := newRecordingInitialFixture(t, func(effects *UnifiedDevPaneEffects) {
		effects.retentionObserve = func(event string, _ map[string]int64) {
			if event == "feed_complete" {
				once.Do(func() { close(entered); <-release })
			}
		}
	})
	registry.retention.options.maxFeedDelay = time.Hour
	op := startRecordingInitialForTest(t, registry, witness, []byte("owned"))
	<-op.done
	<-entered
	completed := registry.retention.progress.manager.completed.Load()
	if err := registry.retention.WritePane(journalKey(witness), []byte("accepted pending tail")); err != nil {
		t.Fatal(err)
	}
	pollUntil(t, time.Second, "accepted tail reaches manager", func() bool { return registry.retention.progress.manager.completed.Load() > completed })
	var closed chan error
	if closing {
		closed = make(chan error, 1)
		go func() { closed <- registry.Close() }()
		pollUntil(t, time.Second, "Close owns accepted work", func() bool {
			registry.retention.mu.Lock()
			defer registry.retention.mu.Unlock()
			return registry.retention.closing
		})
	}
	done := make(chan error, 1)
	duplicate := make(chan error, 1)
	go func() { done <- settleInitial(op, registry, context.Canceled) }()
	go func() { duplicate <- settleInitial(op, registry, context.Canceled) }()
	pollUntil(t, time.Second, "cancelled operation", func() bool { registry.retention.mu.Lock(); defer registry.retention.mu.Unlock(); return op.cancelled })
	select {
	case <-done:
		t.Fatal("cancellation released a held dispatcher effect")
	case <-time.After(50 * time.Millisecond):
	}
	registry.retention.mu.Lock()
	refs := op.generation.refs
	registry.retention.mu.Unlock()
	if refs == 0 {
		t.Fatal("accepted work lost its reservation before dispatch")
	}
	if err := registry.publishInitial(op); err == nil {
		t.Fatal("late completion published cancelled state")
	}
	unblock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-duplicate; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if closed != nil {
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	}
	registry.retention.mu.Lock()
	refs = op.generation.refs
	b, o := registry.retention.bUsed, registry.retention.oUsed
	registry.retention.mu.Unlock()
	if refs != 0 || b != 0 || o != 0 {
		t.Fatalf("settlement refs/B/O=%d/%d/%d", refs, b, o)
	}
}

func TestRecordingInitialBlankRecordAndSnapshotGate(t *testing.T) {
	effects, registry, witness := newRecordingInitialFixture(t)
	key := journalKey(witness)
	effects.active[witness.Session.Session] = key
	if _, _, _, cancel, err := openSnapshotTailForTest(t, effects, witness.Session.Session); err == nil {
		cancel()
		t.Fatal("header-only generation was ready")
	}
	op := commitRecordingInitialForTest(t, registry, witness, nil)
	receipt, err := op.result()
	if err != nil {
		t.Fatal(err)
	}
	if receipt.bytes != 0 || receipt.chunks != 1 || receipt.sequence != 1 {
		t.Fatalf("blank initial proof=%+v", receipt)
	}
	events, geometry, _, cancel, err := openSnapshotTailForTest(t, effects, witness.Session.Session)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if len(events) != 1 || events[0].Sequence != 1 || len(events[0].Payload) != 0 || geometry != (unifiedjournal.Geometry{Columns: 80, Rows: 24}) {
		t.Fatalf("blank state=%+v geometry=%+v", events, geometry)
	}
}

func TestRecordingSilentCreationCapturesActualBlankState(t *testing.T) {
	f := newAdoptionFixture(t, 8)
	name := f.effects.dev.Session
	effects, err := f.effects.BeginSessionBirth(f.server.Label, name)
	if err != nil {
		t.Fatal(err)
	}
	birth := effects.(*unifiedDevBirth)
	session, err := birth.CreateSession(f.server, []string{"new-session", "-f", "ignore-size", "-P", "-F", "#{session_id}", "-s", name, "-x", "80", "-y", "24", "exec sleep 600"})
	if err != nil {
		t.Fatal(err)
	}
	if err := birth.CommitSessionBirth(session); err != nil {
		t.Fatal(err)
	}
	events, geometry, _, cancel, err := openSnapshotTailForTest(t, f.effects, session)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	var payload []byte
	for _, event := range events {
		payload = append(payload, event.Payload...)
	}
	// The silent program contributes no bytes. These records come from the
	// ordered capture's actual mode, cursor and screen witness.
	if len(payload) == 0 || !bytes.Contains(payload, []byte("\x1b[")) || geometry != (unifiedjournal.Geometry{Columns: 80, Rows: 24}) {
		t.Fatalf("missing captured blank state: bytes=%d geometry=%+v", len(payload), geometry)
	}
	birth.mu.Lock()
	op := birth.initial
	verified := birth.observerVerified
	birth.mu.Unlock()
	if !verified || op == nil || op.kind != recordingInitialBirth || op.source.Socket.Path == "" {
		t.Fatal("blank creation lost its observer/source/operation proof")
	}
}

func TestRecordingRotationCancellationBeforeAndAfterSeal(t *testing.T) {
	for _, edge := range []string{"before_seal_start", "before_registry_commit"} {
		t.Run(edge, func(t *testing.T) {
			f := newAdoptionFixture(t, 8)
			session := f.startPaneCommand(t, "cancel-initial-rotation", "printf 'stable source\\n'; exec sleep 600")
			adoption, err := f.effects.AdoptSession(context.Background(), session)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reached := make(chan struct{})
			var rotation *unifiedDevRotation
			var initial *recordingInitialOperation
			f.effects.rotationEdge = func(_ string, got string) {
				if got == edge {
					f.effects.mu.Lock()
					rotation = f.effects.rotation
					initial = rotation.registryT.initial
					f.effects.mu.Unlock()
					cancel()
					close(reached)
				}
			}
			err = f.effects.rotateSession(ctx, session)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel outcome=%v", err)
			}
			select {
			case <-reached:
			case <-time.After(3 * time.Second):
				t.Fatal("rotation did not reach cancellation edge")
			}
			pollUntil(t, 5*time.Second, "rotation cancellation settlement", func() bool { f.effects.mu.Lock(); defer f.effects.mu.Unlock(); return f.effects.rotation == nil })
			key, active := f.effects.paneKey(session)
			if edge == "before_seal_start" {
				if !active || key != adoption.Key {
					t.Fatal("pre-seal cancellation lost predecessor")
				}
				if _, _, _, stop, err := openSnapshotTailForTest(t, f.effects, session); err != nil {
					t.Fatal(err)
				} else {
					stop()
				}
			} else {
				if active {
					t.Fatal("post-seal cancellation published a generation")
				}
				if !rotation.ponr || rotation.registryCommitted {
					t.Fatal("post-seal cancellation crossed readiness publication")
				}
			}
			if initial == nil || initial.published {
				t.Fatal("cancelled operation minted readiness")
			}
		})
	}
}
