package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/unifiedjournal"
)

func correctionRuntime(t *testing.T, label string) (*retentionTrialRuntime, *unifiedjournal.Realm) {
	t.Helper()
	realm := openRetentionRealm(t, label)
	effects := &retentionShadowEffects{realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock()}
	runtime := newRetentionTrialRuntime(effects)
	if runtime == nil {
		t.Fatal("runtime fixture unavailable")
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime, realm
}

func requireReleasedRecordingGauges(t *testing.T, runtime *retentionTrialRuntime) {
	t.Helper()
	s := runtime.progressSnapshot()
	if s.Generations != 0 || s.Commands != 0 || s.Envelopes != 0 || s.IngressBytes != 0 || s.OutboxBytes != 0 {
		t.Errorf("settled runtime retained gauges: generations=%d commands=%d envelopes=%d ingress=%d outbox=%d", s.Generations, s.Commands, s.Envelopes, s.IngressBytes, s.OutboxBytes)
	}
}

func TestRecordingCorrectionRetirementPublishesQuiescentGauges(t *testing.T) {
	for _, transition := range []string{"immediate", "durable", "close"} {
		t.Run(transition, func(t *testing.T) {
			runtime, realm := correctionRuntime(t, "correction-retire-"+transition)
			key := journalKey(retentionWitness("%retire", "retire-inc"))
			if _, _, err := runtime.ensureGeneration(key, true); err != nil {
				t.Fatal(err)
			}
			if transition == "durable" {
				if err := realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
					t.Fatal(err)
				}
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				runtime.options.retire = func(key unifiedjournal.PaneKey) bool { close(entered); <-release; return realm.RetirePane(key) }
				reservation, err := runtime.reserve(key, 0)
				if err != nil {
					t.Fatal(err)
				}
				done, err := runtime.submitBoundary(reservation, key, "terminal_gone", true)
				if err != nil {
					t.Fatal(err)
				}
				// Retirement may run on the manager before it sends done, or on
				// the dispatcher after the manager has released the command.
				// Release the held callback before waiting for either completion.
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("durable retire did not start")
				}
				runtime.mu.Lock()
				commandCharge := reservation.q
				s := runtime.progressSnapshot()
				runtime.mu.Unlock()
				if (commandCharge != 0 && commandCharge != 1) || s.Generations != 1 || s.Commands != 2+int64(commandCharge) {
					t.Errorf("held retirement lost ownership charge: generations=%d commands=%d command_charge=%d", s.Generations, s.Commands, commandCharge)
				}
				t.Logf("held retirement: generations=%d commands=%d boundary_command_charge=%d", s.Generations, s.Commands, commandCharge)
				unblock()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("durable boundary did not complete after retirement release")
				}
				pollUntil(t, time.Second, "durable owner and command release", func() bool {
					runtime.mu.Lock()
					defer runtime.mu.Unlock()
					return runtime.generations[key] == nil && reservation.q == 0
				})
			} else if transition == "immediate" {
				runtime.requestRetire(key)
			} else if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			// No subsequent command or unrelated traffic may refresh these gauges.
			requireReleasedRecordingGauges(t, runtime)
		})
	}
}

func TestRecordingCorrectionFaultCreationPublishesCharges(t *testing.T) {
	for _, path := range []string{"classify", "commit"} {
		t.Run(path, func(t *testing.T) {
			runtime, _ := correctionRuntime(t, "correction-fault-"+path)
			key := journalKey(retentionWitness("%missing", "missing-inc"))
			var winner bool
			if path == "classify" {
				winner, _ = runtime.classifyFault(key, "pre_feed", unifiedjournal.ErrStorage)
			} else {
				runtime.mu.Lock()
				winner, _ = runtime.commitFaultLocked(key, "pre_feed", unifiedjournal.ErrStorage)
				runtime.mu.Unlock()
			}
			if !winner {
				t.Fatal("fixture did not create fault ownership")
			}
			if s := runtime.progressSnapshot(); s.Commands != 2 {
				t.Errorf("funded fault owner invisible: commands=%d", s.Commands)
			}
		})
	}
}

func TestRecordingCorrectionDispatchFailureIndependentOfCleanupElection(t *testing.T) {
	for _, priorStorageFault := range []bool{false, true} {
		t.Run(fmt.Sprintf("prior_storage_%t", priorStorageFault), func(t *testing.T) {
			realm := openRetentionRealm(t, "correction-dispatch")
			feedEntered, feedRelease, cleanupRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var enterOnce, feedOnce, cleanupOnce sync.Once
			var failures, winners, cleanups atomic.Int64
			unblockFeed := func() { feedOnce.Do(func() { close(feedRelease) }) }
			unblockCleanup := func() { cleanupOnce.Do(func() { close(cleanupRelease) }) }
			effects := &retentionShadowEffects{realm: realm, maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(), writeHook: func(unifiedjournal.PaneKey, []byte) error {
				enterOnce.Do(func() { close(feedEntered) })
				<-feedRelease
				failures.Add(1)
				return errors.New("synthetic delivery failure")
			}, hook: func(point string, _ unifiedjournal.PaneKey) {
				if point == "after_fault_cas_winner" {
					winners.Add(1)
				}
				if point == "before_cleanup" {
					cleanups.Add(1)
					<-cleanupRelease
				}
			}}
			registry := newPaneRegistry(effects)
			defer func() { unblockFeed(); unblockCleanup(); _ = registry.Close() }()
			witness := retentionWitness("%dispatch", "dispatch-inc")
			if err := registry.AdmitPane(witness); err != nil {
				t.Fatal(err)
			}
			runtime, key := registry.retention, journalKey(witness)
			if err := runtime.WritePane(key, bytes.Repeat([]byte{'a'}, 64<<10)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-feedEntered:
			case <-time.After(time.Second):
				t.Fatal("first delivery did not block")
			}
			if err := runtime.WritePane(key, bytes.Repeat([]byte{'b'}, 64<<10)); err != nil {
				t.Fatal(err)
			}
			if err := runtime.Boundary(key, "explicit_flush"); err != nil {
				t.Fatal(err)
			}
			fence, err := runtime.startDispatchFence()
			if err != nil {
				t.Fatal(err)
			}
			if priorStorageFault {
				winner, cleanup := runtime.classifyFault(key, "pre_feed", unifiedjournal.ErrStorage)
				if !winner || cleanup == nil {
					t.Fatal("injected storage fault did not own cleanup")
				}
				runtime.appendCleanup(cleanup)
			}
			unblockFeed()
			select {
			case <-fence:
			case <-time.After(time.Second):
				t.Fatal("queued failed deliveries did not settle")
			}
			s := runtime.progressSnapshot()
			if failures.Load() != 2 || winners.Load() != 1 || s.Dispatch.Failures != 2 {
				t.Errorf("delivery failures=%d, fault winners=%d, reported failed envelopes=%d; want 2,1,2", failures.Load(), winners.Load(), s.Dispatch.Failures)
			}
			unblockCleanup()
			_ = registry.Close()
			if cleanups.Load() != 1 {
				t.Errorf("cleanup elected %d times, want once", cleanups.Load())
			}
		})
	}
}

func TestRecordingCorrectionGeometryStorageSpans(t *testing.T) {
	for _, stage := range []string{"append", "sync", "commit"} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fail_%t", stage, fail), func(t *testing.T) {
				h := newGeometryBarrierHarness(t, "correction-geometry-"+stage)
				runtime := h.registry.retention
				if err := runtime.WritePane(h.key, []byte("output")); err != nil {
					t.Fatal(err)
				}
				if err := runtime.Boundary(h.key, "explicit_flush"); err != nil {
					t.Fatal(err)
				}
				fence, err := runtime.startDispatchFence()
				if err != nil {
					t.Fatal(err)
				}
				<-fence
				ticket, err := h.issuer.BeginGeometry(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				runtime.setHook(func(point string, _ unifiedjournal.PaneKey) {
					if point == "before_geometry_"+stage {
						close(entered)
						<-release
					}
				})
				if fail {
					runtime.options.stage = func(name string, _ unifiedjournal.PaneKey) error {
						if name == stage {
							return unifiedjournal.ErrStorage
						}
						return nil
					}
				}
				done := make(chan error, 1)
				go func() { done <- ticket.Commit(context.Background(), 80, 37) }()
				var outcome error
				select {
				case <-entered:
					s := runtime.progressSnapshot()
					span := map[string]recordingStageSnapshot{"append": s.Append, "sync": s.Sync, "commit": s.Commit}[stage]
					if span.InFlight != 1 || span.Attempts != 2 || span.Completed != 1 {
						t.Errorf("held geometry %s omitted or double-counted: %+v", stage, span)
					}
					unblock()
					select {
					case outcome = <-done:
					case <-time.After(time.Second):
						t.Fatal("geometry did not settle after release")
					}
				case outcome = <-done:
					t.Errorf("geometry finished without entering advertised %s storage span", stage)
				case <-time.After(time.Second):
					t.Fatal("geometry neither entered storage nor completed")
				}
				if fail != (outcome != nil) {
					t.Errorf("geometry failure=%t outcome=%v", fail, outcome)
				}
				fence, err = runtime.startDispatchFence()
				if err != nil {
					t.Fatal(err)
				}
				<-fence
				s := runtime.progressSnapshot()
				stages := []recordingStageSnapshot{s.Append, s.Sync, s.Commit}
				failedIndex := map[string]int{"append": 0, "sync": 1, "commit": 2}[stage]
				for index, span := range stages {
					wantAttempts, wantFailures := uint64(2), uint64(0)
					if fail && index > failedIndex {
						wantAttempts = 1
					}
					if fail && index == failedIndex {
						wantFailures = 1
					}
					if span.Attempts != wantAttempts || span.Completed != wantAttempts || span.Failures != wantFailures || span.InFlight != 0 {
						t.Errorf("stage %d: %+v want attempts/completed=%d failures=%d", index, span, wantAttempts, wantFailures)
					}
				}
				if s.CommittedBytes != 6 || s.DeliveredBytes != 6 {
					t.Errorf("geometry changed payload byte counters: committed=%d delivered=%d", s.CommittedBytes, s.DeliveredBytes)
				}
			})
		}
	}
}

func TestRecordingCorrectionStartupEOFClassification(t *testing.T) {
	for _, readFailure := range []error{io.EOF, fmt.Errorf("wrapped read: %w", io.EOF)} {
		unit := &unifiedDevUnit{}
		unit.progress.stage.Store(uint32(observerStageReady))
		readErrors := make(chan error, 1)
		readErrors <- readFailure
		err := unit.awaitReady(context.Background(), controlmode.NewDecoder(), make(chan []byte), readErrors)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("startup did not return EOF: %v", err)
		}
		unit.progress.recordExit(err)
		s := unit.progress.snapshot()
		if s.ExitCause != observerCauseEOF || s.ExitStage != observerStageReady {
			t.Errorf("known startup EOF lost cause/stage: %+v", s)
		}
	}
}
