package broker

import (
	"context"
	"fmt"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
	"sync"
	"time"
)

const recordingSupervisionInterval = 50 * time.Millisecond
const recordingStallLimit = 2 * time.Second

var errRecordingStalled = fmt.Errorf("%w: recording work stalled", ErrUnifiedRotateFatal)

var unavailableObserverContext = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()

// A missing/stopped run never authorizes new source work. Retain cancellation
// across late cleanup rather than replacing it with a fresh background owner.
func (effects *UnifiedDevPaneEffects) observerRunContext() context.Context {
	effects.mu.Lock()
	defer effects.mu.Unlock()
	if effects.observerCtx == nil {
		return unavailableObserverContext
	}
	return effects.observerCtx
}

func (request unifiedDevCommand) spawnAdmissionError() error {
	if request.context != nil && request.context.Err() != nil {
		return request.context.Err()
	}
	if !request.spawnDeadline.IsZero() && !time.Now().Before(request.spawnDeadline) {
		return context.DeadlineExceeded
	}
	return nil
}

type recordingExitRecord struct {
	Key            unifiedjournal.PaneKey
	Generation     uint64
	Stage          observerFailureStage
	Cause          observerFailureCause
	SecondaryCause observerFailureCause
	ExitedMono     int64
}

// Oldest-first overwrite is deliberate: this is a fixed content-free outcome
// ring, not a history store and not an owner of units, processes or journals.
type recordingExitRing struct {
	mu      sync.Mutex
	records [2 * recordingObserverUnitLimit]recordingExitRecord
	next    uint64
}

func (effects *UnifiedDevPaneEffects) recordUnitExit(unit *unifiedDevUnit) {
	p := unit.progress.snapshot()
	record := recordingExitRecord{Generation: unit.generation, Stage: p.ExitStage, Cause: p.ExitCause, ExitedMono: recordingMonoNow()}
	record.Key = unifiedjournal.PaneKey{Server: unit.birth.server.Label, Session: unit.sessionID, ControlGeneration: unit.generation}
	if unit.supervisorFault.Load() {
		record.Stage = observerFailureStage(unit.supervisorStage.Load())
		record.Cause = observerCauseStalled
		record.SecondaryCause = p.ExitCause
	}
	if len(unit.witnesses) > 0 {
		record.Key = journalKey(unit.witnesses[len(unit.witnesses)-1])
	}
	if key := unit.supervisorKey.Load(); key != nil {
		record.Key = *key
	}
	ring := &effects.exitRecords
	ring.mu.Lock()
	ring.records[ring.next%uint64(len(ring.records))] = record
	ring.next++
	ring.mu.Unlock()
}
func (effects *UnifiedDevPaneEffects) recordingExitRecords() []recordingExitRecord {
	ring := &effects.exitRecords
	ring.mu.Lock()
	defer ring.mu.Unlock()
	start := uint64(0)
	if ring.next > uint64(len(ring.records)) {
		start = ring.next - uint64(len(ring.records))
	}
	result := make([]recordingExitRecord, 0, ring.next-start)
	for n := start; n < ring.next; n++ {
		result = append(result, ring.records[n%uint64(len(ring.records))])
	}
	return result
}

// The authoritative recording cutoff is state-only. It must never join a
// pending fault winner or wait for cleanup effects to reach the dispatcher.
func (registry *paneRegistry) superviseFault(key unifiedjournal.PaneKey) {
	registry.mu.Lock()
	registry.retention.mu.Lock()
	cleanup, coordinator := registry.superviseFaultLocked(key)
	registry.retention.mu.Unlock()
	registry.mu.Unlock()
	registry.finishSupervisionFault(cleanup, coordinator)
}

// Caller holds registry.mu and retention.mu, after any provider lock. This is
// also the publication ordering point for an initial owner not yet registered
// in the provider maps. Only the returned effects run after all state unlocks.
func (registry *paneRegistry) superviseFaultLocked(key unifiedjournal.PaneKey) (*retentionCommand, *paneCoordinator) {
	generation := registry.retention.generations[key]
	state, admitted := registry.admitted[routeCoordinateJournalKey(key)]
	current := admitted && state.witness.Incarnation == key.Incarnation
	if generation == nil || !current && registry.rotationGenerations[key] == nil {
		return nil, nil
	}
	_, cleanup := registry.commitRetentionFaultLocked(key, "supervisor", errRecordingStalled)
	coordinator := registry.panes[paneCoordinateKey{server: key.Server, session: key.Session, window: key.Window, pane: key.Pane, incarnation: key.Incarnation}]
	if !current {
		coordinator = nil
	}
	return cleanup, coordinator
}

func (registry *paneRegistry) finishSupervisionFault(cleanup *retentionCommand, coordinator *paneCoordinator) {
	if cleanup != nil {
		registry.retention.appendCleanup(cleanup)
	}
	if coordinator == nil {
		return
	}
	coordinator.mu.Lock()
	coordinator.failure = errRecordingStalled
	epochs := make([]*terminal.Epoch, 0, len(coordinator.attachments))
	for epoch := range coordinator.attachments {
		epochs = append(epochs, epoch)
	}
	coordinator.mu.Unlock()
	for _, epoch := range epochs {
		_ = epoch.Fault(errRecordingStalled)
	}
}

func (effects *UnifiedDevPaneEffects) superviseRecording() {
	limit := effects.supervisionStallLimit
	if limit <= 0 {
		limit = recordingStallLimit
	}
	now := recordingMonoNow()
	effects.mu.Lock()
	registry, _ := effects.observer.(*paneRegistry)
	units := make([]*unifiedDevUnit, 0, len(effects.supervised))
	for unit := range effects.supervised {
		units = append(units, unit)
	}
	effects.mu.Unlock()
	stalled := make(map[unifiedjournal.PaneKey]bool)
	if registry != nil && registry.retention != nil {
		for _, sample := range registry.retention.pendingSamples() {
			if now-sample.oldestMono >= int64(limit) && now-sample.progressMono >= int64(limit) {
				registry.superviseFault(sample.key)
				stalled[sample.key] = true
			}
		}
	}
	for _, unit := range units {
		select {
		case <-unit.done:
			continue
		default:
		}
		stage := observerFailureStage(unit.progress.stage.Load())
		since := unit.progress.stageMono.Load()
		failed := stage != observerStageStream && since != 0 && now-since >= int64(limit)
		effects.mu.Lock()
		key, active := effects.active[unit.sessionIdentity]
		current := effects.units[unit.sessionIdentity] == unit
		if current && active && stalled[key] {
			failed = true
		}
		if !failed || unit.supervisorFault.Load() {
			effects.mu.Unlock()
			continue
		}
		if registry != nil && registry.retention != nil {
			registry.mu.Lock()
			registry.retention.mu.Lock()
		}
		// Initial publication takes the same retention lock, including rotation
		// commit. Set cutoff here, not after an unlocked current/active snapshot.
		unit.supervisorStage.Store(uint32(stage))
		if current && active {
			cutoffKey := key
			unit.supervisorKey.Store(&cutoffKey)
		}
		unit.supervisorFault.Store(true)
		type faultEffect struct {
			cleanup     *retentionCommand
			coordinator *paneCoordinator
		}
		var faults []faultEffect
		if registry != nil && registry.retention != nil {
			for candidate, generation := range registry.retention.generations {
				initial := generation.initial
				if !(current && active && candidate == key) && (initial == nil || initial.owner != unit) {
					continue
				}
				cleanup, coordinator := registry.superviseFaultLocked(candidate)
				if cleanup != nil || coordinator != nil {
					faults = append(faults, faultEffect{cleanup, coordinator})
					if !current || !active {
						cutoffKey := candidate
						unit.supervisorKey.Store(&cutoffKey)
					}
				}
			}
			registry.retention.mu.Unlock()
			registry.mu.Unlock()
		}
		effects.mu.Unlock()
		for _, fault := range faults {
			registry.finishSupervisionFault(fault.cleanup, fault.coordinator)
		}
		// Kill isolates the tmux observer even when its Go owner is inside a held
		// callback. No lease, queued work or process-Wait ownership is released.
		if unit.process != nil && unit.process.Process != nil {
			_ = unit.process.Process.Kill()
		}
	}
}

// One slow lifecycle operation may own the slot. Requests that cannot enter
// are refused or remain in existing bounded owner channels; no task goroutine
// is created per wake. The unit decoder and settlement driver stay in unit.run.
func (effects *UnifiedDevPaneEffects) runSupervisedObserver(ctx context.Context) error {
	jobs := make(chan func(), 1)
	defer close(jobs)
	completed := make(chan struct{}, 1)
	go func() {
		for job := range jobs {
			job()
			select {
			case completed <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(recordingSupervisionInterval)
	defer ticker.Stop()
	busy := false
	rotationPending := false
	preferRotation := false
	var reaps []*unifiedDevUnit
	var spawns []unifiedDevCommand
	start := func(job func()) {
		busy = true
		started := effects.lifecycle.begin()
		jobs <- func() { defer effects.lifecycle.finish(started, false); job() }
	}
	for {
		if !busy {
			if len(reaps) > 0 {
				unit := reaps[0]
				reaps[0] = nil
				reaps = reaps[1:]
				start(func() { effects.reapUnitContext(ctx, unit) })
			} else if key, ok := effects.takeTerminalRetry(); ok {
				start(func() { effects.attemptTerminalRetirement(ctx, key) })
			} else if rotationPending && (preferRotation || len(spawns) == 0) {
				rotationPending = false
				preferRotation = false
				start(func() { effects.runRotationScheduler(ctx) })
			} else if len(spawns) > 0 {
				request := spawns[0]
				spawns[0] = unifiedDevCommand{}
				spawns = spawns[1:]
				preferRotation = true
				start(func() {
					if err := effects.spawnUnit(ctx, request); err != nil {
						request.done <- unifiedDevCommandResult{err: err}
					}
				})
			}
		}
		select {
		case <-ctx.Done():
			for _, request := range spawns {
				request.memory.done()
				request.done <- unifiedDevCommandResult{err: ctx.Err()}
			}
			for _, unit := range reaps {
				unit.reapOnce.Do(func() { unit.memory.done() })
			}
			return ctx.Err()
		case <-ticker.C:
			kept := spawns[:0]
			for _, request := range spawns {
				if err := request.spawnAdmissionError(); err != nil {
					request.memory.done()
					request.done <- unifiedDevCommandResult{err: err}
				} else {
					kept = append(kept, request)
				}
			}
			clear(spawns[len(kept):])
			spawns = kept
			effects.superviseRecording()
		case <-completed:
			busy = false
		case <-effects.rotationWake:
			rotationPending = true
		case request := <-effects.spawns:
			if busy {
				if len(spawns) < recordingObserverUnitLimit {
					spawns = append(spawns, request)
				} else {
					request.memory.done()
					request.done <- unifiedDevCommandResult{err: errRecordingTransients}
				}
			} else {
				preferRotation = true
				start(func() {
					if err := effects.spawnUnit(ctx, request); err != nil {
						request.done <- unifiedDevCommandResult{err: err}
					}
				})
			}
		case unit := <-effects.exits:
			reaps = append(reaps, unit)
		}
	}
}

func (effects *UnifiedDevPaneEffects) takeTerminalRetry() (unifiedjournal.PaneKey, bool) {
	effects.mu.Lock()
	defer effects.mu.Unlock()
	for key, state := range effects.terminalRetires {
		if state.retryReady && !state.running && !state.stopped {
			state.retryReady = false
			return key, true
		}
	}
	return unifiedjournal.PaneKey{}, false
}
