package broker

import (
	"persea-terminal/internal/unifiedjournal"
	"sync"
	"time"
)

var recordingMonotonicOrigin = time.Now()

func recordingMonoNow() int64 { return time.Since(recordingMonotonicOrigin).Nanoseconds() + 1 }

// Journal owners publish only after their operation returns, before releasing
// serialization. Readers never join journal serialization or perform I/O.
type recordingJournalLock struct {
	sync.Mutex
	publish func()
}

func (lock *recordingJournalLock) Unlock() {
	if lock.publish != nil {
		lock.publish()
	}
	lock.Mutex.Unlock()
}

type recordingPressure struct {
	mu          sync.Mutex
	panes       map[unifiedjournal.PaneKey]unifiedRotationPressure
	sampledMono int64
}

func (effects *UnifiedDevPaneEffects) pressureSample(key unifiedjournal.PaneKey) (unifiedRotationPressure, time.Duration) {
	p := &effects.pressure
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.panes[key], time.Duration(recordingMonoNow() - p.sampledMono)
}

func (effects *UnifiedDevPaneEffects) publishPressure() {
	p := &effects.pressure
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.panes == nil {
		p.panes = make(map[unifiedjournal.PaneKey]unifiedRotationPressure)
	}
	clear(p.panes)
	effects.realm.VisitRotationPressure(func(key unifiedjournal.PaneKey, logical, logicalCap, physical, physicalCap int64) {
		p.panes[key] = unifiedRotationPressure{logical: logical, logicalCap: logicalCap, physical: physical, physicalCap: physicalCap}
	})
	p.sampledMono = recordingMonoNow()
}

func (reservation *retentionReservation) unlinkPendingLocked() {
	generation := reservation.generation
	if reservation.previous != nil {
		reservation.previous.next = reservation.next
	} else if generation.pendingHead == reservation {
		generation.pendingHead = reservation.next
	}
	if reservation.next != nil {
		reservation.next.previous = reservation.previous
	} else if generation.pendingTail == reservation {
		generation.pendingTail = reservation.previous
	}
	reservation.previous, reservation.next = nil, nil
	reservation.runtime.publishPendingLocked(generation)
}

type recordingPendingSample struct {
	key          unifiedjournal.PaneKey
	oldestMono   int64
	progressMono int64
}

func (runtime *retentionTrialRuntime) publishPendingLocked(generation *retentionGeneration) {
	runtime.pendingMu.Lock()
	defer runtime.pendingMu.Unlock()
	if runtime.pendingProgress == nil {
		runtime.pendingProgress = make(map[unifiedjournal.PaneKey]recordingPendingSample)
	}
	if generation.pendingHead == nil {
		delete(runtime.pendingProgress, generation.key)
		return
	}
	runtime.pendingProgress[generation.key] = recordingPendingSample{generation.key, generation.pendingHead.acceptedMono, generation.lastProgressMono}
}

func (runtime *retentionTrialRuntime) pendingSamples() []recordingPendingSample {
	runtime.pendingMu.Lock()
	defer runtime.pendingMu.Unlock()
	result := make([]recordingPendingSample, 0, len(runtime.pendingProgress))
	for _, sample := range runtime.pendingProgress {
		result = append(result, sample)
	}
	return result
}

// Retry timer callbacks only mark existing obligations and wake the manager.
// All retry I/O belongs to that existing owner, never to a timer goroutine.
func (runtime *retentionTrialRuntime) processDurableRetries() {
	runtime.mu.Lock()
	var due []unifiedjournal.PaneKey
	if !runtime.closing && !runtime.closed {
		for key, state := range runtime.durableRetires {
			if state.retryReady {
				state.retryReady = false
				due = append(due, key)
			}
		}
	}
	runtime.mu.Unlock()
	for _, key := range due {
		runtime.retireDurable(key)
	}
}
