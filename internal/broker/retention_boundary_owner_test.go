package broker

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"persea-terminal/internal/unifiedjournal"
)

type boundaryOwnerLedger struct {
	p, q, e          int
	b, o             int64
	generations      map[unifiedjournal.PaneKey]*retentionGeneration
	usage            map[unifiedjournal.PaneKey]recordingSourceUsage
	refs             map[unifiedjournal.PaneKey]int
	sourceIdentities map[recordingSourceKey]*recordingSourceAccount
	sources          map[recordingSourceKey]recordingSourceAccount
}

func captureBoundaryOwnerLedger(runtime *retentionTrialRuntime) boundaryOwnerLedger {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	result := boundaryOwnerLedger{p: runtime.pUsed, q: runtime.qUsed, e: runtime.eUsed, b: runtime.bUsed, o: runtime.oUsed,
		generations: make(map[unifiedjournal.PaneKey]*retentionGeneration), usage: make(map[unifiedjournal.PaneKey]recordingSourceUsage), refs: make(map[unifiedjournal.PaneKey]int),
		sourceIdentities: make(map[recordingSourceKey]*recordingSourceAccount), sources: make(map[recordingSourceKey]recordingSourceAccount)}
	for key, generation := range runtime.generations {
		result.generations[key], result.usage[key], result.refs[key] = generation, generation.sourceUsage, generation.refs
	}
	for key, account := range runtime.sources {
		result.sourceIdentities[key], result.sources[key] = account, *account
	}
	return result
}

func TestRetentionBoundaryAbsentOrRetiredOwnerDoesNotAllocate(t *testing.T) {
	for _, retired := range []bool{false, true} {
		for _, asynchronous := range []bool{false, true} {
			name := "absent"
			if retired {
				name = "retired"
			}
			if asynchronous {
				name += "/start"
			} else {
				name += "/blocking"
			}
			t.Run(name, func(t *testing.T) {
				fixture := newRotationFixture(t)
				next := fixture.next(2)
				if retired {
					reservation := fixture.materialize(next)
					txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
					if err != nil {
						t.Fatal(err)
					}
					if !txn.Abort() {
						t.Fatal("rotation did not abort")
					}
					fixture.abortReservation(reservation)
				}
				runtime := fixture.registry.retention
				before := captureBoundaryOwnerLedger(runtime)
				if before.generations[journalKey(next)] != nil {
					t.Fatal("fixture has a live successor")
				}
				var err error
				if asynchronous {
					var done <-chan error
					done, err = runtime.startBoundary(journalKey(next), "missing_owner", false)
					if done != nil {
						<-done
					}
				} else {
					err = runtime.Boundary(journalKey(next), "missing_owner")
				}
				after := captureBoundaryOwnerLedger(runtime)
				if !errors.Is(err, unifiedjournal.ErrInvalidated) || !reflect.DeepEqual(before, after) {
					t.Fatalf("missing owner boundary err=%v; ledger before=%+v after=%+v", err, before, after)
				}
			})
		}
	}
}

func TestRetentionBoundaryFaultBetweenRegistryCheckAndReserveDoesNotResurrect(t *testing.T) {
	fixture := newRotationFixture(t)
	runtime := fixture.registry.retention
	key := journalKey(fixture.previous)
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	runtime.setHook(func(point string, candidate unifiedjournal.PaneKey) {
		if point == "before_reserve" && candidate == key {
			close(entered)
			<-release
		}
	})
	returned := make(chan error, 1)
	go func() { returned <- fixture.registry.retentionBoundary(fixture.previous, "racing_terminal_boundary") }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("boundary did not reach reservation")
	}
	winner, cleanup := runtime.classifyFault(key, "boundary_owner_test", errRotationInjected)
	if !winner || cleanup == nil {
		t.Fatal("fault did not own cleanup")
	}
	runtime.appendCleanup(cleanup)
	fence, err := runtime.startDispatchFence()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-fence:
	case <-time.After(2 * time.Second):
		t.Fatal("actual fault cleanup did not settle")
	}
	before := captureBoundaryOwnerLedger(runtime)
	if before.generations[key] != nil {
		t.Fatal("fault cleanup did not retire original generation")
	}
	close(release)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("sticky failed-incarnation boundary lost idempotence: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("boundary did not return")
	}
	after := captureBoundaryOwnerLedger(runtime)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("boundary resurrected retired owner: before=%+v after=%+v", before, after)
	}
}
