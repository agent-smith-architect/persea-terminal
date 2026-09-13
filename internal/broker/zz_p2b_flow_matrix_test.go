package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/unifiedjournal"
)

func rotationJournalFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && len(entry.Name()) > len("pane-.journal") && entry.Name()[:5] == "pane-" {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

func assertRotationJournalFilesExclude(t *testing.T, root string, marker []byte) {
	t.Helper()
	files, err := rotationJournalFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(contents, marker) {
			t.Fatalf("forbidden decoded batch output reached journal file %s", path)
		}
	}
}

func rotationJournalDirectory(t *testing.T, root string) string {
	t.Helper()
	var directory string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && len(entry.Name()) > len("pane-.journal") && entry.Name()[:5] == "pane-" {
			directory = filepath.Dir(path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if directory == "" {
		t.Fatal("journal realm directory not found")
	}
	return directory
}

// The broker must attempt successor materialization once. A storage failure
// after capacity has become single-use arms 4R, restores the complete pending
// suffix once, and never retries BeginRotatedPane under a new key.
func TestUnifiedRotationMaterializationFailureSingleAttemptAndRestoresOnce(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	const marker = "MATERIALIZATION-FAILURE-PENDING-SUFFIX"
	sessionID := fixture.startPaneCommand(t, "rotation-materialization-failure", `sh -c 'stty -echo; printf "MATERIALIZATION-FAILURE-PREFIX\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	realmDirectory := rotationJournalDirectory(t, fixture.runtimeDir)
	var attempts atomic.Int32
	var captured *unifiedDevRotation
	fixture.effects.rotationEdge = func(_ string, edge string) {
		if edge != "successor_owner_held" {
			return
		}
		attempts.Add(1)
		fixture.effects.mu.Lock()
		rotation := fixture.effects.rotation
		fixture.effects.mu.Unlock()
		captured = rotation
		rotation.holder.mu.Lock()
		appendErr := rotation.holder.rotation.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: []byte(marker)})
		rotation.holder.mu.Unlock()
		if appendErr != nil {
			t.Errorf("append pending marker: %v", appendErr)
		}
		if chmodErr := os.Chmod(realmDirectory, 0o500); chmodErr != nil {
			t.Errorf("make journal realm read-only: %v", chmodErr)
		}
	}
	err = fixture.effects.rotateSession(ctx, sessionID)
	if chmodErr := os.Chmod(realmDirectory, 0o700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil || (!errors.Is(err, unifiedjournal.ErrStorage) && !errors.Is(err, unifiedjournal.ErrUnsafeRuntime)) {
		t.Fatalf("materialization failure error=%v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("successor materialization attempts=%d want 1", got)
	}
	secondKey := captured.newKey
	secondKey.ControlGeneration++
	fixture.effects.journalMu.Lock()
	second, secondErr := fixture.effects.realm.BeginRotatedPane(secondKey, unifiedjournal.Geometry{Columns: 80, Rows: 24}, captured.capacity)
	fixture.effects.journalMu.Unlock()
	if second != nil || !errors.Is(secondErr, unifiedjournal.ErrInvalidated) {
		t.Fatalf("second materialization result=%v error=%v want single-use refusal", second != nil, secondErr)
	}
	if count := bytes.Count(fixture.journalBytes(t, adoption.Key), []byte(marker)); count != 1 {
		t.Fatalf("4R pending replay count=%d want 1", count)
	}
	active, ok := fixture.effects.paneKey(sessionID)
	if !ok || active != adoption.Key {
		t.Fatalf("materialization failure active=%#v ok=%v want predecessor %#v", active, ok, adoption.Key)
	}
}

// A full pending buffer can overflow on one event while the same decoder Feed
// has a later event. The overflowing event and suffix are part of 4R and must
// be appended after the buffered flood exactly once, even when the control
// record was split across reads.
func TestUnifiedRotationPrePONROverflowRestoresSplitDecoderSuffixExactlyOnce(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "rotation-split-overflow", `sh -c 'stty -echo; printf "SPLIT-OVERFLOW-PREFIX\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	unit := fixture.effects.units[sessionID]
	old, found := unit.rotationWitness(adoption.Key)
	if !found {
		t.Fatal("missing predecessor witness")
	}
	next, err := unit.unusedRotationWitness(old)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := fixture.registry.retention.acquireGeometryOwner(adoption.Key)
	if err != nil {
		t.Fatal(err)
	}
	fixture.effects.journalMu.Lock()
	capacity, err := fixture.effects.realm.BeginRotationCapacity(adoption.Key)
	fixture.effects.journalMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	rotation := &unifiedDevRotation{
		session: sessionID, oldKey: adoption.Key, unit: unit, registry: fixture.registry,
		capacity: capacity, oldOwner: owner, holder: unit.holder, old: old,
		next: next, newKey: journalKey(next), flipped: true,
	}
	pending := &unifiedRotationPending{}
	flood := bytes.Repeat([]byte{'f'}, int(unifiedjournal.RotationPendingCapBytes))
	if err := pending.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: flood}); err != nil {
		t.Fatal(err)
	}
	rotation.holder.mu.Lock()
	rotation.holder.witness = next
	rotation.holder.committed = false
	rotation.holder.rotation = pending
	rotation.holder.mu.Unlock()

	decoder := controlmode.NewDecoder()
	stream := []byte(fmt.Sprintf("%%output %s EVT_A_ONLY\n%%output %s EVT_B_ONLY\n", old.Pane, old.Pane))
	cut := len(stream) / 2
	first, err := decoder.Feed(stream[:cut])
	if err != nil {
		t.Fatal(err)
	}
	second, err := decoder.Feed(stream[cut:])
	if err != nil {
		t.Fatal(err)
	}
	events := append(first, second...)
	if len(events) != 2 {
		t.Fatalf("split decoder events=%d want 2", len(events))
	}
	if err := rotation.restore(events); err != nil {
		t.Fatal(err)
	}
	fixture.registry.retention.releaseGeometryOwner(adoption.Key, owner)
	capacity.Release()
	committed := fixture.journalBytes(t, adoption.Key)
	for _, marker := range [][]byte{[]byte("EVT_A_ONLY"), []byte("EVT_B_ONLY")} {
		if count := bytes.Count(committed, marker); count != 1 {
			t.Fatalf("restored marker %q count=%d want 1", marker, count)
		}
	}
}

func TestUnifiedRotationFlowControlTripwireBeforeAndAfterPONR(t *testing.T) {
	signals := []controlmode.Event{
		{Kind: controlmode.EventPause, Name: "pause"},
		{Kind: controlmode.EventContinue, Name: "continue"},
		{Kind: controlmode.EventExtendedOutput, PaneID: "%1", Data: []byte("late")},
	}
	for _, afterPONR := range []bool{false, true} {
		for _, signal := range signals {
			name := fmt.Sprintf("kind_%d/post_ponr_%v", signal.Kind, afterPONR)
			t.Run(name, func(t *testing.T) {
				fixture := newAdoptionFixture(t, 4)
				sessionID := fixture.startPaneCommand(t, "rotation-flow-tripwire", `sh -c 'stty -echo; printf "ROTATION-FLOW-TRIPWIRE\n"; sleep 60'`)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				_, _, subscriber, stop, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				unit := fixture.effects.units[sessionID]
				old, found := unit.rotationWitness(adoption.Key)
				if !found {
					t.Fatal("missing predecessor witness")
				}
				probe := &unifiedDevRotation{session: sessionID, oldKey: adoption.Key, unit: unit, old: old, registry: fixture.registry, ponr: afterPONR}
				markers := []string{
					fmt.Sprintf("FLOW-COMPOSITE-%d-%v", signal.Kind, afterPONR),
					fmt.Sprintf("FLOW-BOUNDARY-%d-%v", signal.Kind, afterPONR),
				}
				batchFor := func(marker string) []byte {
					batch := []byte(fmt.Sprintf("%%output %s %s\n", adoption.Key.Pane, marker))
					switch signal.Kind {
					case controlmode.EventPause:
						batch = append(batch, []byte(fmt.Sprintf("%%pause %s\n", adoption.Key.Pane))...)
					case controlmode.EventContinue:
						batch = append(batch, []byte(fmt.Sprintf("%%continue %s\n", adoption.Key.Pane))...)
					case controlmode.EventExtendedOutput:
						batch = append(batch, []byte(fmt.Sprintf("%%extended-output %s 1 : late\n", adoption.Key.Pane))...)
					}
					return batch
				}
				compositeErr := exerciseRotationCompositeDecoderPath(t, ctx, unit, probe, batchFor(markers[0]))
				if !errors.Is(compositeErr, ErrUnifiedObserverFlowControl) {
					t.Fatalf("composite mixed flow-control batch error=%v", compositeErr)
				}
				boundaryErr := exerciseRotationBoundaryDecoderPath(ctx, unit, probe, batchFor(markers[1]))
				if !errors.Is(boundaryErr, ErrUnifiedObserverFlowControl) {
					t.Fatalf("boundary mixed flow-control batch error=%v", boundaryErr)
				}
				slotsBefore := fixture.effects.realm.AvailableCompletePaneSlots()
				var captured *unifiedDevRotation
				fixture.effects.rotationFault = func(_ string, stage string) error {
					if stage != "boundary_wait" {
						return nil
					}
					fixture.effects.mu.Lock()
					rotation := fixture.effects.rotation
					fixture.effects.mu.Unlock()
					if rotation == nil || rotation.ponr != afterPONR {
						return nil
					}
					captured = rotation
					return boundaryErr
				}
				err = fixture.effects.rotateSession(ctx, sessionID)
				if !errors.Is(err, ErrUnifiedObserverFlowControl) {
					t.Fatalf("rotation flow-control error=%v", err)
				}
				if afterPONR {
					if !errors.Is(err, ErrUnifiedRotateFatal) {
						t.Fatalf("post-PONR flow-control error=%v lacks fatal classification", err)
					}
				} else {
					if errors.Is(err, ErrUnifiedRotateFatal) {
						t.Fatalf("pre-PONR flow-control error=%v crossed the rotation PONR", err)
					}
					if captured == nil || captured.ponr || captured.registryCommitted {
						t.Fatalf("pre-PONR tripwire did not take 4R before the unit-fatal tripwire: rotation=%v ponr=%v committed=%v", captured != nil, captured != nil && captured.ponr, captured != nil && captured.registryCommitted)
					}
				}
				for {
					select {
					case event, open := <-subscriber.events():
						if open {
							subscriber.releaseEvent(event)
						}
						if !open {
							if reason := subscriber.closeReason(); reason != proto.SubscriberClosedGenerationFailed {
								t.Fatalf("flow-control tripwire close reason=%q", reason)
							}
							goto subscriberClosed
						}
						if bytes.Contains(event.Payload, []byte(markers[0])) || bytes.Contains(event.Payload, []byte(markers[1])) {
							t.Fatalf("forbidden batch output reached subscriber: %#v", event)
						}
					case <-time.After(10 * time.Second):
						t.Fatal("flow-control tripwire did not reap the observer unit")
					}
				}
			subscriberClosed:
				for _, marker := range markers {
					assertRotationJournalFilesExclude(t, fixture.runtimeDir, []byte(marker))
				}
				if !afterPONR {
					assertPrePONRFlowControlSettled(t, fixture, sessionID, captured)
					return
				}
				assertPostPONRFatalSettled(t, fixture, sessionID, captured, slotsBefore)
				return
			})
		}
	}
}

func TestRotationBoundaryFailureDrainsPublishedEffectsBeforeRollback(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "rotation-boundary-dispatch", `sh -c 'stty -echo; printf "ROTATION-BOUNDARY-DISPATCH\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, subscriber, stop, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	dispatchEntered := make(chan struct{})
	releaseDispatch := make(chan struct{})
	var blockOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseDispatch) }) }
	t.Cleanup(release)
	fixture.registry.retention.setHook(func(point string, key unifiedjournal.PaneKey) {
		if point != "before_downstream_write" || key == adoption.Key {
			return
		}
		blockOnce.Do(func() {
			close(dispatchEntered)
			<-releaseDispatch
		})
	})
	defer fixture.registry.retention.setHook(nil)

	var captured *unifiedDevRotation
	fixture.effects.rotationFault = func(_ string, stage string) error {
		if stage != "boundary_wait" {
			return nil
		}
		fixture.effects.mu.Lock()
		captured = fixture.effects.rotation
		fixture.effects.mu.Unlock()
		select {
		case <-dispatchEntered:
			return ErrUnifiedObserverFlowControl
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	result := make(chan error, 1)
	go func() { result <- fixture.effects.rotateSession(ctx, sessionID) }()

	select {
	case <-dispatchEntered:
	case err := <-result:
		t.Fatalf("rotation returned before successor publication reached the dispatcher: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-result:
		t.Fatalf("rotation returned before the queued successor effect settled: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-result; !errors.Is(err, ErrUnifiedObserverFlowControl) {
		t.Fatalf("rotation error=%v, want flow-control refusal", err)
	}
	if captured == nil || captured.ponr || captured.registryCommitted {
		t.Fatalf("rotation did not restore before the fatal tripwire: rotation=%v ponr=%v committed=%v", captured != nil, captured != nil && captured.ponr, captured != nil && captured.registryCommitted)
	}
	if reason := waitRotationSubscriberClose(t, subscriber); reason != proto.SubscriberClosedGenerationFailed {
		t.Fatalf("flow-control close reason=%q", reason)
	}
	assertPrePONRFlowControlSettled(t, fixture, sessionID, captured)
}

func TestRotationBoundaryManagerFailureDrainsPublishedEffectsBeforeRollback(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "rotation-boundary-manager-error", `sh -c 'stty -echo; printf "ROTATION-BOUNDARY-MANAGER-ERROR\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}

	// Start from an empty dispatcher. The production boundary then publishes
	// the injected storage-fault outcome before reporting its own error.
	drained, err := fixture.registry.retention.startDispatchFence()
	if err != nil {
		t.Fatal(err)
	}
	<-drained
	boundaryFailure := errors.New("injected rotation bootstrap boundary failure")
	fixture.registry.retention.options.stage = func(operation string, key unifiedjournal.PaneKey) error {
		if operation == "append" && key != adoption.Key {
			return boundaryFailure
		}
		return nil
	}
	dispatchEntered := make(chan struct{})
	releaseDispatch := make(chan struct{})
	var blockOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseDispatch) }) }
	t.Cleanup(release)
	observe := fixture.registry.retention.options.observe
	fixture.registry.retention.options.observe = func(event string, fields map[string]int64) {
		if event == "storage_fault" {
			blockOnce.Do(func() {
				close(dispatchEntered)
				<-releaseDispatch
			})
		}
		observe(event, fields)
	}

	var captured *unifiedDevRotation
	fixture.effects.rotationEdge = func(_ string, edge string) {
		if edge != "before_bootstrap_wait" {
			return
		}
		fixture.effects.mu.Lock()
		captured = fixture.effects.rotation
		fixture.effects.mu.Unlock()
	}
	result := make(chan error, 1)
	go func() { result <- fixture.effects.rotateSession(ctx, sessionID) }()

	select {
	case <-dispatchEntered:
	case err := <-result:
		t.Fatalf("rotation returned before boundary failure reached the dispatcher: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-result:
		t.Fatalf("rotation rolled back before boundary-owned effects settled: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-result; !errors.Is(err, unifiedjournal.ErrStorage) {
		t.Fatalf("rotation error=%v, want typed storage failure", err)
	}
	if captured == nil || captured.ponr || !captured.settled || captured.registryT != nil || captured.journal != nil {
		t.Fatalf("boundary failure settlement rotation=%v ponr=%v settled=%v registry=%v journal=%v",
			captured != nil, captured != nil && captured.ponr, captured != nil && captured.settled,
			captured != nil && captured.registryT != nil, captured != nil && captured.journal != nil)
	}
}

func TestRotationDispatchFenceCloseOrdering(t *testing.T) {
	for _, closeBeforeFence := range []bool{false, true} {
		name := "accepted_fence_before_close"
		if closeBeforeFence {
			name = "close_before_fence_rejected"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newAdoptionFixture(t, 4)
			sessionID := fixture.startPaneCommand(t, "rotation-fence-close-"+name, `sh -c 'stty -echo; printf "ROTATION-FENCE-CLOSE\n"; sleep 60'`)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			drained, err := fixture.registry.retention.startDispatchFence()
			if err != nil {
				t.Fatal(err)
			}
			<-drained

			appendEntered := make(chan struct{})
			releaseAppend := make(chan struct{})
			fenceEntered := make(chan struct{})
			releaseFence := make(chan struct{})
			var appendOnce, fenceOnce, releaseAppendOnce, releaseFenceOnce sync.Once
			var fencePublishes, downstreamWrites atomic.Int64
			releaseAppendFn := func() { releaseAppendOnce.Do(func() { close(releaseAppend) }) }
			releaseFenceFn := func() { releaseFenceOnce.Do(func() { close(releaseFence) }) }
			t.Cleanup(releaseAppendFn)
			t.Cleanup(releaseFenceFn)
			fixture.registry.retention.setHook(func(point string, key unifiedjournal.PaneKey) {
				switch {
				case point == "before_append" && key == adoption.Key:
					appendOnce.Do(func() {
						close(appendEntered)
						<-releaseAppend
					})
				case point == "before_dispatch_fence_publish":
					fencePublishes.Add(1)
					fenceOnce.Do(func() {
						close(fenceEntered)
						<-releaseFence
					})
				case point == "before_downstream_write" && key == adoption.Key:
					downstreamWrites.Add(1)
				}
			})
			defer fixture.registry.retention.setHook(nil)

			if err := fixture.registry.retention.WritePane(adoption.Key, []byte("REQUIRED-BEFORE-CLOSE")); err != nil {
				t.Fatal(err)
			}
			boundaryDone, err := fixture.registry.retention.startBoundary(adoption.Key, "rotation_bootstrap", false)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-appendEntered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			waitResult := make(chan error, 1)
			go func() {
				waitResult <- waitRotationBoundarySettlement(fixture.registry, boundaryDone, context.Canceled)
			}()

			closeResult := make(chan error, 1)
			if closeBeforeFence {
				fixture.cancel()
				go func() { closeResult <- fixture.registry.Close() }()
				pollUntil(t, 5*time.Second, "retention close admission", func() bool {
					fixture.registry.retention.mu.Lock()
					defer fixture.registry.retention.mu.Unlock()
					return fixture.registry.retention.closing
				})
				releaseAppendFn()
			} else {
				releaseAppendFn()
				select {
				case <-fenceEntered:
				case err := <-waitResult:
					t.Fatalf("accepted fence returned before publication: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				fixture.cancel()
				go func() { closeResult <- fixture.registry.Close() }()
				pollUntil(t, 5*time.Second, "retention close behind accepted fence", func() bool {
					fixture.registry.retention.mu.Lock()
					defer fixture.registry.retention.mu.Unlock()
					return fixture.registry.retention.closing
				})
				select {
				case err := <-waitResult:
					t.Fatalf("accepted fence completed before dispatcher release: %v", err)
				case err := <-closeResult:
					t.Fatalf("Close overtook accepted fence: %v", err)
				case <-time.After(100 * time.Millisecond):
				}
				releaseFenceFn()
			}

			waitErr := <-waitResult
			if !errors.Is(waitErr, context.Canceled) {
				t.Fatalf("boundary wait error=%v, want cancellation", waitErr)
			}
			if closeBeforeFence {
				if !errors.Is(waitErr, unifiedjournal.ErrInvalidated) {
					t.Fatalf("rejected fence error=%v, want teardown-owned invalidation", waitErr)
				}
				if got := fencePublishes.Load(); got != 0 {
					t.Fatalf("Close-before-admission published %d dispatcher fences", got)
				}
			} else if errors.Is(waitErr, unifiedjournal.ErrInvalidated) {
				t.Fatalf("accepted fence was rejected after admission: %v", waitErr)
			}
			if err := <-closeResult; err != nil {
				t.Fatal(err)
			}
			if got := downstreamWrites.Load(); got != 1 {
				t.Fatalf("required pre-Close effect dispatches=%d, want 1", got)
			}

			// Repeated and concurrent Close calls share the same completed result;
			// none may enqueue another close or touch the closed outbox.
			repeats := make(chan error, 3)
			for range 3 {
				go func() { repeats <- fixture.registry.Close() }()
			}
			for range 3 {
				if err := <-repeats; err != nil {
					t.Fatal(err)
				}
			}
			fixture.registry.retention.mu.Lock()
			queued := fixture.registry.retention.queue.len()
			closed := fixture.registry.retention.closed
			fixture.registry.retention.mu.Unlock()
			if queued != 0 || !closed {
				t.Fatalf("Close settlement queued=%d closed=%v", queued, closed)
			}
		})
	}
}

func TestRejectedOutputCloseAdmissionIsManagerOwned(t *testing.T) {
	for _, closeBeforeAdmission := range []bool{false, true} {
		name := "accepted_before_close"
		if closeBeforeAdmission {
			name = "close_before_admission"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newAdoptionFixture(t, 4)
			sessionID := fixture.startPaneCommand(t, "rejected-output-close-"+name, `sh -c 'stty -echo; printf "REJECTED-OUTPUT-CLOSE\n"; sleep 60'`)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			drained, err := fixture.registry.retention.startDispatchFence()
			if err != nil {
				t.Fatal(err)
			}
			<-drained

			fixture.effects.mu.Lock()
			unit := fixture.effects.units[sessionID]
			var witness controlmode.PaneWitness
			var found bool
			if unit != nil {
				witness, found = unit.rotationWitness(adoption.Key)
			}
			fixture.effects.mu.Unlock()
			if !found {
				t.Fatal("active adoption witness not found")
			}
			fixture.registry.mu.Lock()
			coordinate := routeCoordinateKey(witness)
			state, admitted := fixture.registry.admitted[coordinate]
			if !admitted {
				fixture.registry.mu.Unlock()
				t.Fatal("active adoption is not registry-admitted")
			}
			state.failedIncarnation = witness.Incarnation
			fixture.registry.admitted[coordinate] = state
			fixture.registry.failed = 1
			fixture.registry.mu.Unlock()
			fixture.registry.retention.mu.Lock()
			generation := fixture.registry.retention.generations[adoption.Key]
			if generation == nil {
				fixture.registry.retention.mu.Unlock()
				t.Fatal("active adoption generation not found")
			}
			generation.failed = true
			generation.retireRequested = true
			fixture.registry.retention.mu.Unlock()

			var discarded, published atomic.Int64
			fixture.effects.retentionObserve = func(event string, fields map[string]int64) {
				if event == "discarded_after_fault" && fields["discarded_bytes"] == int64(len("REJECTED")) {
					discarded.Add(1)
				}
			}
			admissionEntered := make(chan struct{})
			releaseAdmission := make(chan struct{})
			var admissionOnce, releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseAdmission) }) }
			t.Cleanup(release)
			fixture.registry.retention.setHook(func(point string, _ unifiedjournal.PaneKey) {
				switch point {
				case "before_rejected_output_admission":
					if closeBeforeAdmission {
						admissionOnce.Do(func() {
							close(admissionEntered)
							<-releaseAdmission
						})
					}
				case "before_rejected_output_publish":
					published.Add(1)
				}
			})
			defer fixture.registry.retention.setHook(nil)

			observeResult := make(chan error, 1)
			go func() {
				observeResult <- fixture.registry.ObservePane(controlmode.Observation{
					Kind: controlmode.ObservationOutput, Witness: witness, Data: []byte("REJECTED"),
				})
			}()
			if closeBeforeAdmission {
				select {
				case <-admissionEntered:
				case err := <-observeResult:
					t.Fatalf("rejected observation returned before admission edge: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				fixture.cancel()
				closeResult := make(chan error, 1)
				go func() { closeResult <- fixture.registry.Close() }()
				if err := <-closeResult; err != nil {
					t.Fatal(err)
				}
				release()
			}
			if err := <-observeResult; !errors.Is(err, unifiedjournal.ErrInvalidated) {
				t.Fatalf("rejected observation error=%v, want ErrInvalidated", err)
			}
			if closeBeforeAdmission {
				if got := published.Load(); got != 0 {
					t.Fatalf("teardown-owned rejected output published %d records", got)
				}
				if got := discarded.Load(); got != 0 {
					t.Fatalf("teardown-owned rejected output dispatched %d records", got)
				}
				return
			}
			settled, err := fixture.registry.retention.startDispatchFence()
			if err != nil {
				t.Fatal(err)
			}
			<-settled
			if got := published.Load(); got != 1 {
				t.Fatalf("accepted rejected-output publications=%d, want 1", got)
			}
			if got := discarded.Load(); got != 1 {
				t.Fatalf("accepted rejected-output observations=%d, want 1", got)
			}
		})
	}
}

func assertPrePONRFlowControlSettled(t *testing.T, fixture *adoptionFixture, sessionID string, rotation *unifiedDevRotation) {
	t.Helper()
	pollUntil(t, 10*time.Second, "pre-PONR flow-control unit reap", func() bool {
		fixture.effects.mu.Lock()
		defer fixture.effects.mu.Unlock()
		return fixture.effects.units[sessionID] == nil
	})
	if rotation == nil || !rotation.settled || rotation.ponr || rotation.registryCommitted || rotation.registryT != nil || rotation.journal != nil {
		t.Fatalf("pre-PONR flow-control settlement rotation=%v settled=%v ponr=%v committed=%v registry=%v journal=%v",
			rotation != nil, rotation != nil && rotation.settled, rotation != nil && rotation.ponr,
			rotation != nil && rotation.registryCommitted, rotation != nil && rotation.registryT != nil, rotation != nil && rotation.journal != nil)
	}
	rotation.holder.mu.Lock()
	pending := rotation.holder.rotation
	rotation.holder.mu.Unlock()
	if pending != nil {
		t.Fatal("pre-PONR flow-control settlement retained holder output")
	}
	fixture.registry.mu.Lock()
	_, routeHeld := fixture.registry.rotations[routeCoordinateKey(rotation.old)]
	_, generationHeld := fixture.registry.rotationGenerations[rotation.newKey]
	fixture.registry.mu.Unlock()
	if routeHeld || generationHeld {
		t.Fatalf("pre-PONR flow-control settlement retained provisional state route=%v generation=%v", routeHeld, generationHeld)
	}
	fixture.registry.retention.mu.Lock()
	newGeneration := fixture.registry.retention.generations[rotation.newKey]
	fixture.registry.retention.mu.Unlock()
	if newGeneration != nil {
		t.Fatal("pre-PONR flow-control settlement retained successor generation authority")
	}
	fixture.effects.mu.Lock()
	_, active := fixture.effects.active[sessionID]
	fixture.effects.mu.Unlock()
	if active {
		t.Fatal("pre-PONR fatal tripwire left an active generation")
	}
	if out, err := tmuxCombinedOutput(fixture.disposable.path, "has-session", "-t", sessionID); err != nil {
		t.Fatalf("pre-PONR fatal tripwire destroyed the tmux session: %v %s", err, out)
	}
	if state := fixture.effects.projectSession("main", sessionID, unifiedSessionFacts{Windows: 1, Panes: 1, Valid: true}); state == nil || state.State != "adoptable" {
		t.Fatalf("pre-PONR fatal tripwire state=%+v, want adoptable", state)
	}
}

func exerciseRotationCompositeDecoderPath(t *testing.T, ctx context.Context, unit *unifiedDevUnit, rotation *unifiedDevRotation, batch []byte) error {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	probeUnit := &unifiedDevUnit{owner: unit.owner, ptmx: writer}
	read := make(chan []byte, 1)
	read <- batch
	_, _, err = probeUnit.submitRotationComposite(ctx, controlmode.NewDecoder(), read, make(chan error), rotation, 1)
	return err
}

func exerciseRotationBoundaryDecoderPath(ctx context.Context, unit *unifiedDevUnit, rotation *unifiedDevRotation, batch []byte) error {
	read := make(chan []byte)
	done := make(chan error, 1)
	go func() {
		read <- batch
		done <- nil
	}()
	return unit.awaitRotationBoundary(ctx, controlmode.NewDecoder(), read, make(chan error), done, rotation)
}

func TestRotationBoundaryFailureWaitsForSubmittedBoundary(t *testing.T) {
	for _, test := range []struct {
		name  string
		hook  bool
		batch []byte
	}{
		{name: "injected_failure", hook: true},
		{name: "decoded_flow_control", batch: []byte("%extended-output %1 1 : late\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			owner := &UnifiedDevPaneEffects{}
			if test.hook {
				owner.rotationFault = func(string, string) error { return ErrUnifiedObserverFlowControl }
			}
			unit := &unifiedDevUnit{owner: owner}
			read := make(chan []byte)
			boundaryDone := make(chan error, 1)
			returned := make(chan error, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			go func() {
				returned <- unit.awaitRotationBoundary(ctx, controlmode.NewDecoder(), read, make(chan error), boundaryDone, &unifiedDevRotation{session: "boundary-order"})
			}()
			if !test.hook {
				read <- test.batch
			}
			select {
			case err := <-returned:
				t.Fatalf("failure returned before submitted boundary settled: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			boundaryDone <- nil
			if err := <-returned; !errors.Is(err, ErrUnifiedObserverFlowControl) {
				t.Fatalf("settled failure=%v, want flow-control fault", err)
			}
		})
	}
}

func TestUnifiedRotationUnitDeathAtEverySequenceEdgeCanRestart(t *testing.T) {
	edges := []string{
		"capacity_reserved", "before_composite_write", "holder_flipped",
		"successor_owner_held", "successor_materialized", "bootstrap_durable",
		"registry_validated", "predecessor_sealed", "active_swapped",
		"journal_committed", "pending_durable",
	}
	for _, wantedEdge := range edges {
		t.Run(wantedEdge, func(t *testing.T) {
			fixture := newAdoptionFixture(t, 4)
			sessionID := fixture.startPaneCommand(t, "rotation-unit-death", `sh -c 'stty -echo; printf "ROTATION-UNIT-DEATH\n"; sleep 60'`)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := fixture.effects.AdoptSession(ctx, sessionID); err != nil {
				t.Fatal(err)
			}
			var killed atomic.Bool
			reapDone := make(chan struct{})
			fixture.effects.rotationEdge = func(_ string, edge string) {
				if edge != wantedEdge || !killed.CompareAndSwap(false, true) {
					return
				}
				fixture.effects.mu.Lock()
				unit := fixture.effects.units[sessionID]
				fixture.effects.mu.Unlock()
				if unit == nil || unit.process == nil || unit.process.Process == nil {
					t.Errorf("edge %s has no live observer process", edge)
					return
				}
				go func() {
					fixture.effects.reapFaultedUnit(unit)
					_ = unit.process.Process.Kill()
					close(reapDone)
				}()
			}
			_ = fixture.effects.rotateSession(ctx, sessionID)
			if !killed.Load() {
				t.Fatalf("edge %s was not reached", wantedEdge)
			}
			select {
			case <-reapDone:
			case <-time.After(10 * time.Second):
				t.Fatal("timed out waiting for adversarial reap")
			}
			pollUntil(t, 10*time.Second, "dead rotation unit removal", func() bool {
				fixture.effects.mu.Lock()
				defer fixture.effects.mu.Unlock()
				return fixture.effects.units[sessionID] == nil && fixture.effects.rotation == nil
			})
			if _, err := fixture.effects.AdoptSession(ctx, sessionID); err != nil {
				t.Fatalf("restart after unit death at %s: %v", wantedEdge, err)
			}
		})
	}
}

// This is the wire-level P2D-F1 shape. Five peers keep draining and receive
// every pre-rotation sentinel before their typed rotation verdict. One peer is
// wedged on the triggering write, is cut once, then one re-mint reconstructs
// that event and receives the post-reconnect sentinel once.
func TestUnifiedRotationSixAttachmentTransportShape(t *testing.T) {
	fixture := newAdoptionFixture(t, 8)
	const sessionName = "rotation-six-attachments"
	sessionID := fixture.startPaneCommand(t, sessionName, `sh -c 'stty -echo; while IFS= read -r line; do printf "%s\n" "$line"; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	attachments := make([]*b1Attachment, 6)
	for index := range attachments {
		attachments[index] = b1Open(t, fixture.effects, sessionID, fmt.Sprintf("rotation-wire-%d", index))
	}
	pre := []byte("[rotation-pre]")
	fixture.disposable.run("send-keys", "-l", "-t", sessionName+":", string(pre))
	fixture.disposable.run("send-keys", "-t", sessionName+":", "Enter")
	for index := 0; index < 5; index++ {
		attachment := attachments[index]
		pollUntil(t, 5*time.Second, "draining pre sentinel", func() bool {
			live, _, _, _ := attachment.snapshot()
			return bytes.Count(live, pre) == 1
		})
	}
	attachments[5].wedge()
	trigger := []byte("[rotation-trigger]")
	fixture.disposable.run("send-keys", "-l", "-t", sessionName+":", string(trigger))
	fixture.disposable.run("send-keys", "-t", sessionName+":", "Enter")
	for index := 0; index < 5; index++ {
		attachment := attachments[index]
		pollUntil(t, 5*time.Second, "draining trigger sentinel", func() bool {
			live, _, _, _ := attachment.snapshot()
			return bytes.Count(live, trigger) == 1
		})
	}
	if err := fixture.effects.rotateSession(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		attachment := attachments[index]
		pollUntil(t, 5*time.Second, "draining typed rotation verdict", func() bool {
			_, controls, _, _ := attachment.snapshot()
			return len(controls) == 1
		})
		live, controls, _, _ := attachment.snapshot()
		if bytes.Count(live, pre) != 1 || bytes.Count(live, trigger) != 1 || controls[0].Code != string(proto.SubscriberClosedGenerationRotated) {
			t.Fatalf("draining attachment %d live=%q controls=%+v", index, live, controls)
		}
	}
	select {
	case <-attachments[5].ended:
	case <-time.After(unifiedSubscriberCloseGrace + 5*time.Second):
		t.Fatal("wedged rotation attachment was not cut")
	}

	newKey, ok := fixture.effects.paneKey(sessionID)
	if !ok || newKey == adoption.Key {
		t.Fatalf("rotation successor key=%#v ok=%v", newKey, ok)
	}
	reminted := b1Open(t, fixture.effects, sessionID, "rotation-wire-reminted")
	replay := reminted.replayBytes()
	if bytes.Count(replay, pre) != 1 || bytes.Count(replay, trigger) != 1 {
		t.Fatalf("reminted replay pre=%d trigger=%d", bytes.Count(replay, pre), bytes.Count(replay, trigger))
	}
	post := []byte("[rotation-post-reconnect]")
	b1Commit(t, fixture.effects, newKey, post)
	pollUntil(t, 5*time.Second, "post-reconnect sentinel", func() bool {
		live, _, _, _ := reminted.snapshot()
		return bytes.Count(live, post) == 1
	})
	if live, _, _, _ := reminted.snapshot(); bytes.Count(live, post) != 1 {
		t.Fatalf("post-reconnect sentinel count=%d", bytes.Count(live, post))
	}
}
