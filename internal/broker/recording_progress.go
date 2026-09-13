package broker

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"persea-terminal/internal/unifiedjournal"
)

// These samples are diagnostic hints, never authority or a coherent ledger.
// Each owner writes fixed-size atomics directly; a blocked callback, journal
// operation, or state lock cannot block a reader. No event queue or log sink is
// involved, so there is no telemetry backlog or terminal content to retain.
type recordingStageSnapshot struct {
	StartedMonoNS, FinishedMonoNS                     int64
	StartedUnixNano, FinishedUnixNano, LastDurationNS int64
	Attempts, Completed, Failures, InFlight           uint64
}

type recordingStageProgress struct {
	startedMono, finishedMono               atomic.Int64
	started, finished, duration             atomic.Int64
	attempts, completed, failures, inFlight atomic.Uint64
}

func (p *recordingStageProgress) begin() time.Time {
	now := time.Now()
	p.startedMono.Store(recordingMonoNow())
	p.started.Store(now.UnixNano())
	p.inFlight.Store(1)
	p.attempts.Add(1)
	return now
}

func (p *recordingStageProgress) finish(start time.Time, failed bool) {
	if failed {
		p.failures.Add(1)
	}
	p.duration.Store(time.Since(start).Nanoseconds())
	p.finished.Store(time.Now().UnixNano())
	p.finishedMono.Store(recordingMonoNow())
	p.completed.Add(1)
	p.inFlight.Store(0)
}

func (p *recordingStageProgress) snapshot() recordingStageSnapshot {
	return recordingStageSnapshot{p.startedMono.Load(), p.finishedMono.Load(), p.started.Load(), p.finished.Load(), p.duration.Load(), p.attempts.Load(), p.completed.Load(), p.failures.Load(), p.inFlight.Load()}
}

type recordingRejection uint8

const (
	recordingRejectClosed recordingRejection = iota
	recordingRejectAuthority
	recordingRejectGenerations
	recordingRejectCommands
	recordingRejectEnvelopes
	recordingRejectIngress
	recordingRejectOutbox
	recordingRejectCount
)

type retentionProgress struct {
	committedBytes, deliveredBytes                              atomic.Uint64
	accepted, dequeued                                          atomic.Uint64
	acceptedBytes                                               atomic.Uint64
	oldestQueued, activeAccepted                                atomic.Int64
	commands, envelopes, generations, ingressBytes, outboxBytes atomic.Int64
	rejected                                                    [recordingRejectCount]atomic.Uint64
	manager, append, sync, commit, dispatch                     recordingStageProgress
}

type retentionProgressSnapshot struct {
	SampledMonoNS, OldestPendingMonoNS                          int64
	CommittedBytes, DeliveredBytes                              uint64
	SampledUnixNano                                             int64
	Accepted, Dequeued, AcceptedBytes                           uint64
	OldestQueuedUnixNano, ActiveAcceptedUnixNano                int64
	Commands, Envelopes, Generations, IngressBytes, OutboxBytes int64
	Rejected                                                    [recordingRejectCount]uint64
	Manager, Append, Sync, Commit, Dispatch                     recordingStageSnapshot
}

func (runtime *retentionTrialRuntime) recordAcceptedLocked(command *retentionCommand) {
	command.acceptedUnixNano = time.Now().UnixNano()
	runtime.progress.accepted.Add(1)
	runtime.progress.acceptedBytes.Add(uint64(len(command.payload)))
	if runtime.queue.len() == 1 {
		runtime.progress.oldestQueued.Store(command.acceptedUnixNano)
	}
}

func (runtime *retentionTrialRuntime) progressSnapshot() retentionProgressSnapshot {
	p := &runtime.progress
	s := retentionProgressSnapshot{
		SampledMonoNS:  recordingMonoNow(),
		CommittedBytes: p.committedBytes.Load(), DeliveredBytes: p.deliveredBytes.Load(),
		SampledUnixNano: time.Now().UnixNano(), Accepted: p.accepted.Load(), Dequeued: p.dequeued.Load(), AcceptedBytes: p.acceptedBytes.Load(),
		OldestQueuedUnixNano: p.oldestQueued.Load(), ActiveAcceptedUnixNano: p.activeAccepted.Load(),
		Commands: p.commands.Load(), Envelopes: p.envelopes.Load(), Generations: p.generations.Load(), IngressBytes: p.ingressBytes.Load(), OutboxBytes: p.outboxBytes.Load(),
		Manager: p.manager.snapshot(), Append: p.append.snapshot(), Sync: p.sync.snapshot(), Commit: p.commit.snapshot(), Dispatch: p.dispatch.snapshot(),
	}
	for i := range s.Rejected {
		s.Rejected[i] = p.rejected[i].Load()
	}
	for _, pending := range runtime.pendingSamples() {
		if s.OldestPendingMonoNS == 0 || pending.oldestMono < s.OldestPendingMonoNS {
			s.OldestPendingMonoNS = pending.oldestMono
		}
	}
	return s
}

// The unit keeps a closed cause and exact high-level failure stage even after
// serve has returned and process.Wait/reaping has not settled. Free-form errors
// remain private existing values and are never copied into these snapshots.
type observerFailureStage uint32

const (
	observerStageStarting observerFailureStage = iota
	observerStageAttach
	observerStageReady
	observerStageFlags
	observerStageAdoption
	observerStageBirth
	observerStageRotation
	observerStageStream
	observerStageRead
	observerStageDecode
	observerStageCommand
)

type observerFailureCause uint32

const (
	observerCauseNone observerFailureCause = iota
	observerCauseEOF
	observerCauseCancelled
	observerCauseDeadline
	observerCauseFlowControl
	observerCauseInvalidated
	observerCauseStorage
	observerCauseStageError
	observerCauseStalled
)

type observerProgress struct {
	stageMono            atomic.Int64
	stage                atomic.Uint32
	exitStage, exitCause atomic.Uint32
	exitedUnixNano       atomic.Int64
}

func (p *observerProgress) setStage(stage observerFailureStage) {
	if observerFailureStage(p.stage.Load()) != stage || p.stageMono.Load() == 0 {
		p.stageMono.Store(recordingMonoNow())
		p.stage.Store(uint32(stage))
	}
}

type observerProgressSnapshot struct {
	Stage, ExitStage observerFailureStage
	ExitCause        observerFailureCause
	ExitedUnixNano   int64
}

func (p *observerProgress) snapshot() observerProgressSnapshot {
	return observerProgressSnapshot{observerFailureStage(p.stage.Load()), observerFailureStage(p.exitStage.Load()), observerFailureCause(p.exitCause.Load()), p.exitedUnixNano.Load()}
}
func (p *observerProgress) recordExit(err error) {
	cause := observerCauseStageError
	switch {
	case err == nil || errors.Is(err, io.EOF):
		cause = observerCauseEOF
	case errors.Is(err, context.Canceled):
		cause = observerCauseCancelled
	case errors.Is(err, context.DeadlineExceeded):
		cause = observerCauseDeadline
	case errors.Is(err, ErrUnifiedObserverFlowControl):
		cause = observerCauseFlowControl
	case errors.Is(err, unifiedjournal.ErrInvalidated):
		cause = observerCauseInvalidated
	case errors.Is(err, unifiedjournal.ErrStorage), errors.Is(err, unifiedjournal.ErrCorruptJournal):
		cause = observerCauseStorage
	}
	p.exitStage.Store(p.stage.Load())
	p.exitCause.Store(uint32(cause))
	p.exitedUnixNano.Store(time.Now().UnixNano())
}
