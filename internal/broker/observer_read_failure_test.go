package broker

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/unifiedjournal"
)

func TestObserverReadFailureBroadcast(t *testing.T) {
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer output.Close()
	read := make(chan []byte)
	result := newObserverReadResult()
	done := make(chan struct{})
	defer close(done)
	var failed atomic.Bool
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		unifiedDevReadLoop(input, read, result, done, &failed)
	}()
	wrote := make(chan error, 1)
	go func() {
		_, err := output.Write([]byte("before\r\n"))
		_ = output.Close()
		wrote <- err
	}()
	select {
	case chunk := <-read:
		if string(chunk) != "before\n" {
			t.Fatalf("final output = %q", chunk)
		}
	case <-time.After(time.Second):
		t.Fatal("reader did not deliver output before termination")
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("reader did not finish")
	}
	if !failed.Load() {
		t.Fatal("read failure did not fence publication")
	}
	for observer := 0; observer < 3; observer++ {
		select {
		case <-result.done():
			if !errors.Is(result.err, io.EOF) {
				t.Fatalf("observer %d read error = %v", observer, result.err)
			}
		default:
			t.Fatalf("observer %d lost durable reader termination", observer)
		}
	}
}

func TestObserverWaitersRetainReadFailure(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "commands")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cause := errors.New("terminal observer read failure")
	result := newObserverReadResult()
	result.finish(cause)
	owner := &UnifiedDevPaneEffects{panes: make(map[string]*unifiedDevBirth)}
	holder := &unifiedDevBirth{owner: owner}
	unit := &unifiedDevUnit{owner: owner, holder: holder, ptmx: file}
	witness := controlmode.PaneWitness{Pane: "%1"}
	tests := []struct {
		name string
		wait func(context.Context, *controlmode.Decoder) error
	}{
		{"birth", func(ctx context.Context, decoder *controlmode.Decoder) error {
			return unit.finishBirth(ctx, decoder, nil, result)
		}},
		{"readiness", func(ctx context.Context, decoder *controlmode.Decoder) error {
			return unit.awaitReady(ctx, decoder, nil, result)
		}},
		{"command", func(ctx context.Context, decoder *controlmode.Decoder) error {
			return unit.finishCommand(ctx, decoder, nil, result, unifiedDevCommand{blocks: 1})
		}},
		{"adoption_attach", func(ctx context.Context, decoder *controlmode.Decoder) error {
			return unit.finishAdoptionAttach(ctx, decoder, nil, result)
		}},
		{"adoption_capture", func(ctx context.Context, decoder *controlmode.Decoder) error {
			unit.birth.adoption = &unifiedDevAdoption{}
			_, _, _, err := unit.submitAdoptionComposite(ctx, decoder, nil, result, holder, &witness, 1)
			return err
		}},
		{"birth_capture", func(ctx context.Context, decoder *controlmode.Decoder) error {
			return unit.captureBirthInitial(ctx, decoder, nil, result, unifiedDevCommand{context: ctx, birthCapture: holder})
		}},
		{"rotation_capture", func(ctx context.Context, decoder *controlmode.Decoder) error {
			_, _, err := unit.submitRotationComposite(ctx, decoder, nil, result, &unifiedDevRotation{old: witness}, 1)
			return err
		}},
		{"rotation_boundary", func(ctx context.Context, decoder *controlmode.Decoder) error {
			return unit.awaitRotationBoundary(ctx, decoder, nil, result, nil, &unifiedDevRotation{})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for attempt := 0; attempt < 2; attempt++ {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				decoder := controlmode.NewDecoder()
				err := test.wait(ctx, decoder)
				cancel()
				decoder.Release()
				if !errors.Is(err, cause) {
					t.Fatalf("wait %d lost shared reader failure: %v", attempt, err)
				}
			}
		})
	}
}

func TestRecordingSettlementRetainsReadyReadFailure(t *testing.T) {
	cause := errors.New("reader failed as dependency returned")
	for _, dependency := range []string{"done", "result"} {
		t.Run(dependency, func(t *testing.T) {
			readResult := newObserverReadResult()
			stream := &recordingSettlementStream{readResult: readResult}
			// Only the dependency can win the select. Publish failure after it
			// returns, before the deferred reader check, without another waiter.
			stream.dependencyReturned = func() {
				stream.dependencyReturned = nil
				readResult.finish(cause)
			}
			done, result := make(chan struct{}), make(chan error, 1)
			if dependency == "done" {
				close(done)
			} else {
				result <- nil
			}
			if err := stream.wait(done, result); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(stream.err, cause) {
				t.Fatalf("ready dependency hid reader failure: %v", stream.err)
			}
			// A later dependency wait keeps the first failure without spinning.
			closeAgain := make(chan struct{})
			close(closeAgain)
			if err := stream.wait(closeAgain, nil); err != nil || !errors.Is(stream.err, cause) {
				t.Fatalf("later settlement lost read failure: result=%v failure=%v", err, stream.err)
			}
			select {
			case <-readResult.done():
			default:
				t.Fatal("settlement acknowledgement cleared the shared result")
			}
		})
	}
}

func TestRecordingInitialRetainsPreviouslyObservedReadFailure(t *testing.T) {
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
	unit := &unifiedDevUnit{owner: effects, done: make(chan struct{})}
	cause := errors.New("initial observer read failure")
	result := newObserverReadResult()
	result.finish(cause)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := unit.awaitReady(ctx, controlmode.NewDecoder(), nil, result); !errors.Is(err, cause) {
		t.Fatalf("first waiter: %v", err)
	}
	returned := make(chan error, 1)
	go func() { returned <- unit.awaitInitial(ctx, controlmode.NewDecoder(), nil, result, op) }()
	// Cancellation is recorded before accepted storage can settle.
	for {
		registry.retention.mu.Lock()
		cancelled := op.cancelled
		registry.retention.mu.Unlock()
		if cancelled {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("initial wait did not observe the retained failure")
		default:
			runtime.Gosched()
		}
	}
	unblock()
	select {
	case err := <-returned:
		if !errors.Is(err, cause) {
			t.Fatalf("initial wait lost prior read failure: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("initial failure did not settle")
	}
	if err := registry.publishInitial(op); err == nil {
		t.Fatal("failed initial wait published readiness")
	}
}

func TestObserverReadFailureAfterRotationRecovers(t *testing.T) {
	for _, duringRotation := range []bool{false, true} {
		name := "ordinary_loop"
		if duringRotation {
			name = "rotation_then_ordinary_loop"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newAdoptionFixture(t, 4)
			inject := scriptObserverReadFailure(fixture.effects)
			reaped := make(chan *unifiedDevUnit, 1)
			fixture.effects.unitReapEdge = func(unit *unifiedDevUnit, _ []controlmode.PaneWitness) {
				select {
				case reaped <- unit:
				default:
				}
			}
			recovered := make(chan struct{}, 1)
			fixture.effects.rotationEdge = func(_ string, edge string) {
				if duringRotation && edge == "before_composite_write" {
					inject()
				}
				if edge == "pending_durable" {
					select {
					case recovered <- struct{}{}:
					default:
					}
				}
			}
			session := fixture.startPaneCommand(t, "read-failure", "exec cat")
			adopted, err := fixture.effects.AdoptSession(context.Background(), session)
			if err != nil {
				t.Fatal(err)
			}
			fixture.effects.mu.Lock()
			victim := fixture.effects.units[session]
			fixture.effects.mu.Unlock()
			if duringRotation {
				err := fixture.effects.rotateSession(context.Background(), session)
				if !errors.Is(err, syscall.EIO) || errors.Is(err, ErrUnifiedRotateFatal) {
					t.Fatalf("rotation must first return the nonfatal read error: %v", err)
				}
			} else {
				inject()
			}
			select {
			case unit := <-reaped:
				if unit != victim || !errors.Is(unit.exitErr, syscall.EIO) || unit.progress.snapshot().ExitStage != observerStageRead {
					t.Fatalf("read failure missed the ordinary reap path: same=%t error=%v progress=%+v", unit == victim, unit.exitErr, unit.progress.snapshot())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("reader failure was consumed before the ordinary loop could reap")
			}
			select {
			case <-recovered:
			case <-time.After(2 * time.Second):
				t.Fatal("transport loss did not publish a recovered successor")
			}
			fixture.effects.mu.Lock()
			successor, unit := fixture.effects.active[session], fixture.effects.units[session]
			fixture.effects.mu.Unlock()
			if successor == adopted.Key || successor == (unifiedjournal.PaneKey{}) || unit == nil || unit == victim {
				t.Fatalf("recovery did not replace the dead observer: successor=%+v unit=%p", successor, unit)
			}
		})
	}
}

func scriptObserverReadFailure(effects *UnifiedDevPaneEffects) func() {
	fail, failed := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	effects.observerReadLoop = func(unit *unifiedDevUnit, read chan []byte, result *observerReadResult) {
		if first.Swap(true) {
			unifiedDevReadLoop(unit.ptmx, read, result, unit.done, &unit.readFailed)
			return
		}
		// Relay the real startup. At the command edge, the scripted
		// terminal read replaces further bytes, so capture cannot win.
		source := make(chan []byte, 16)
		sourceResult := newObserverReadResult()
		unit.memory.hold()
		go func() {
			defer unit.memory.done()
			unifiedDevReadLoop(unit.ptmx, source, sourceResult, unit.done, &unit.readFailed)
		}()
		publishFailure := func() {
			for {
				select {
				case <-read:
				default:
					unit.readFailed.Store(true)
					result.finish(syscall.EIO)
					close(failed)
					return
				}
			}
		}
		for {
			select {
			case <-fail:
				publishFailure()
				return
			case chunk := <-source:
				select {
				case read <- chunk:
				case <-fail:
					publishFailure()
					return
				case <-unit.done:
					return
				}
			case <-sourceResult.done():
				result.finish(sourceResult.err)
				return
			case <-unit.done:
				return
			}
		}
	}
	var once sync.Once
	return func() { once.Do(func() { close(fail); <-failed }) }
}

func TestObserverReadFailureKeepsFatalRotationPolicy(t *testing.T) {
	for _, edge := range []string{"successor_materialized", "predecessor_sealed"} {
		t.Run(edge, func(t *testing.T) {
			fixture := newAdoptionFixture(t, 4)
			inject := scriptObserverReadFailure(fixture.effects)
			fixture.effects.rotationEdge = func(_ string, at string) {
				if at == edge {
					inject()
				}
			}
			if edge == "successor_materialized" {
				// Reject bootstrap first, then observe transport loss while its
				// accepted work settles. This is a secondary failure, not a
				// race between the boundary result and the read-failure arm.
				fixture.effects.rotationFault = func(string, string) error {
					return errors.New("bootstrap publication rejected")
				}
			}
			session := fixture.startPaneCommand(t, "fatal-read-failure", "exec cat")
			adoption, err := fixture.effects.AdoptSession(context.Background(), session)
			if err != nil {
				t.Fatal(err)
			}
			fixture.effects.mu.Lock()
			unit := fixture.effects.units[session]
			fixture.effects.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := fixture.effects.rotateSession(ctx, session); !errors.Is(err, ErrUnifiedRotateFatal) {
				t.Fatalf("unsafe rotation resumed after %s read failure: %v", edge, err)
			}
			select {
			case <-unit.done:
			case <-time.After(2 * time.Second):
				t.Fatal("fatal read failure left its observer registered")
			}
			fixture.effects.mu.Lock()
			active := fixture.effects.active[session]
			fixture.effects.mu.Unlock()
			if active != (unifiedjournal.PaneKey{}) || fixture.effects.recordingReady(adoption.Key) {
				t.Fatalf("fatal rotation retained attachment authority: %+v", active)
			}
		})
	}
}
