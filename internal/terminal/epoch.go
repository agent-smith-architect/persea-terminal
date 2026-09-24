package terminal

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Config struct {
	QuietInterval   time.Duration
	MaximumInterval time.Duration
	CutTimeout      time.Duration
	Clock           Clock
	HistoryRows     int
	HistoryRowsSet  bool
}

func (c Config) validate() error {
	if c.Clock == nil || c.QuietInterval <= 0 || c.MaximumInterval < c.QuietInterval || c.CutTimeout <= c.MaximumInterval {
		return ErrMalformed
	}
	if c.HistoryRowsSet && !ValidHistoryRows(c.HistoryRows) {
		return ErrMalformed
	}
	return nil
}

func (c Config) effectiveHistoryRows() int {
	if c.HistoryRowsSet {
		return c.HistoryRows
	}
	return DefaultHistoryRows
}

type activeCut struct {
	cut            *cut
	historyRows    int
	historyRequest *historyRequest
	cutIssued      bool
	captureReady   bool
	history        []string
	truncated      bool
	prepared       bool
	timer          Timer
	done           chan struct{}
}

type historyRequestPhase uint8

const (
	historyRequestQueued historyRequestPhase = iota + 1
	historyRequestIssued
	historyRequestDeferred
	historyRequestCommitted
	historyRequestSuperseded
)

type historyRequest struct {
	request uint64
	rows    int
	phase   historyRequestPhase
}

// supersedeBeforeIssue is the single recoverable rejection transition. Once
// exact-source CUT issuance starts, any discard or failure is terminal.
func (r *historyRequest) supersedeBeforeIssue() error {
	if r == nil || r.phase != historyRequestQueued {
		return ErrInvariant
	}
	r.phase = historyRequestSuperseded
	return nil
}

const MaxTransitionalModeRequests = 4

// AttachmentEffects binds one attachment-local Epoch to an opaque owner.
// Implementations perform lifecycle effects only and return ordinary errors.
type AttachmentEffects interface {
	BindAttachment(*Epoch) error
	ReleaseAttachment(*Epoch) error
}

// Epoch is the sole lifecycle owner. Adapters bind typed source, PTY,
// transport, and clock implementations, then call Start, PTYBytes,
// HandleFrame, Fault, and Finalize; they never construct protocol transitions.
type Epoch struct {
	source      *PinnedSource
	sourceID    string
	epoch       uint64
	protocol    *automaton
	scheduler   *scheduler
	egress      *egress
	input       *inputPump
	ingress     ingressFramer
	gate        *workGate
	lease       generationLease
	clock       Clock
	cutTimeout  time.Duration
	historyRows int
	attachments AttachmentEffects

	ctx       context.Context
	cancel    context.CancelFunc
	ownerDone chan struct{}
	done      chan struct{}

	mu                     sync.Mutex
	active                 *activeCut
	nextCut                uint64
	start                  *startAttempt
	terminal               *terminalResult
	transitionalInput      [][]byte
	transitionalInputBytes int
	transitionalModes      []Mode
	pendingHistory         *historyRequest
	lastHistoryRequest     uint64

	teardownOnce sync.Once
	teardownDone chan struct{}
	cleanupMu    sync.Mutex
	cleanup      cleanupState
}

func NewEpoch(ctx context.Context, source *PinnedSource, epoch uint64, writer FrameWriter, pty InterruptiblePTYWriter, config Config) (*Epoch, error) {
	return newEpoch(ctx, source, epoch, writer, pty, config, nil)
}

// NewEpochWithAttachmentEffects constructs an Epoch on an explicit production
// path that binds the child to its lifecycle owner.
func NewEpochWithAttachmentEffects(ctx context.Context, source *PinnedSource, epoch uint64, writer FrameWriter, pty InterruptiblePTYWriter, config Config, effects AttachmentEffects) (*Epoch, error) {
	return newEpoch(ctx, source, epoch, writer, pty, config, effects)
}

func newEpoch(ctx context.Context, source *PinnedSource, epoch uint64, writer FrameWriter, pty InterruptiblePTYWriter, config Config, effects AttachmentEffects) (*Epoch, error) {
	if ctx == nil || source == nil || epoch == 0 || writer == nil || pty == nil {
		return nil, ErrMalformed
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	sourceID, _, _ := source.publicIdentity()
	protocol, err := newAutomaton(sourceID, epoch)
	if err != nil {
		return nil, err
	}
	egress, err := newEgress(writer, config.CutTimeout)
	if err != nil {
		return nil, err
	}
	input, err := newInputPump(sourceID, epoch, pty)
	if err != nil {
		_ = egress.finishAndWait()
		return nil, err
	}
	scheduler, err := newScheduler(config.Clock, config.QuietInterval, config.MaximumInterval)
	if err != nil {
		_ = input.sealAndStop()
		_ = egress.finishAndWait()
		return nil, err
	}
	epochCtx, cancel := context.WithCancel(ctx)
	e := &Epoch{
		source: source, sourceID: sourceID, epoch: epoch, protocol: protocol,
		scheduler: scheduler, egress: egress, input: input, gate: newWorkGate(),
		lease: generationLease{source: sourceID, epoch: epoch, generation: 1},
		clock: config.Clock, cutTimeout: config.CutTimeout, historyRows: config.effectiveHistoryRows(), attachments: effects,
		ctx: epochCtx, cancel: cancel, ownerDone: make(chan struct{}), done: make(chan struct{}), teardownDone: make(chan struct{}),
	}
	if effects != nil {
		if err := effects.BindAttachment(e); err != nil {
			cancel()
			scheduler.Stop()
			inputErr := input.sealAndStop()
			egressErr := egress.finishAndWait()
			releaseErr := effects.ReleaseAttachment(e)
			return nil, errors.Join(err, inputErr, egressErr, releaseErr)
		}
	}
	go e.ownerLoop()
	return e, nil
}

// Done closes at the epoch's single terminal publication point.
func (e *Epoch) Done() <-chan struct{} { return e.done }

// Err returns the immutable public terminal error after Done closes.
func (e *Epoch) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.terminal == nil {
		return nil
	}
	return e.terminal.publicErr
}

// Start admits one INITIAL or RECONNECT attempt. The admission transaction
// installs its WorkGate claim and replay owner before either becomes visible.
func (e *Epoch) Start(ctx context.Context, kind CutKind) error {
	if ctx == nil || (kind != CutInitial && kind != CutReconnect) {
		return notAdmittedResult(admissionMalformedRequest, ErrMalformed).publicError()
	}

	e.mu.Lock()
	if e.terminal != nil {
		result := alreadyTerminalResult(e.terminal)
		e.mu.Unlock()
		return result.publicError()
	}
	if e.start != nil {
		attempt := e.start
		if attempt.kind != kind {
			e.mu.Unlock()
			return notAdmittedResult(admissionConflictingAttempt, ErrOutOfState).publicError()
		}
		if attempt.phase == startPhasePrepared {
			result := attempt.result
			e.mu.Unlock()
			return result.publicError()
		}
		e.mu.Unlock()
		return e.waitStart(ctx, attempt).publicError()
	}
	if err := ctx.Err(); err != nil {
		e.mu.Unlock()
		return notAdmittedResult(admissionCallerPreCanceled, err).publicError()
	}
	if err := e.ctx.Err(); err != nil {
		request := e.requestTerminalLocked(canceledTerminalCause(err))
		e.mu.Unlock()
		return request.result.publicError()
	}
	claim, err := e.gate.claim("start-attempt")
	if err != nil {
		request := e.requestTerminalLocked(invariantTerminalCause(err))
		e.mu.Unlock()
		return request.result.publicError()
	}
	operationCtx, operationStop := context.WithCancel(e.ctx)
	attempt := &startAttempt{
		kind: kind, phase: startPhaseBinding, done: make(chan struct{}),
		ownership: pendingOwnership(), operationStop: operationStop,
	}
	e.start = attempt
	e.mu.Unlock()

	go e.watchStartOwner(attempt, ctx)
	e.runStartAttempt(operationCtx, attempt, claim)
	e.mu.Lock()
	result := e.lifecycleResultLocked(attempt)
	e.mu.Unlock()
	return result.publicError()
}

func (e *Epoch) waitStart(ctx context.Context, attempt *startAttempt) lifecycleResult {
	select {
	case <-attempt.done:
		e.mu.Lock()
		result := e.lifecycleResultLocked(attempt)
		e.mu.Unlock()
		return result
	case <-ctx.Done():
		e.mu.Lock()
		result := e.lifecycleResultLocked(attempt)
		if result.valid() {
			e.mu.Unlock()
			return result
		}
		err := ctx.Err()
		e.mu.Unlock()
		return notAdmittedResult(admissionCallerPreCanceled, err)
	}
}

func (e *Epoch) lifecycleResultLocked(attempt *startAttempt) lifecycleResult {
	if attempt != nil && attempt.result.valid() {
		return attempt.result
	}
	if e.terminal != nil {
		return alreadyTerminalResult(e.terminal)
	}
	return lifecycleResult{}
}

func (e *Epoch) watchStartOwner(attempt *startAttempt, owner context.Context) {
	select {
	case <-attempt.done:
		return
	case <-owner.Done():
	}
	e.mu.Lock()
	if e.start == attempt && e.terminal == nil &&
		attempt.phase != startPhasePrepared && attempt.phase != startPhaseTerminal {
		e.requestTerminalLocked(canceledTerminalCause(owner.Err()))
	}
	e.mu.Unlock()
}

func (e *Epoch) runStartAttempt(ctx context.Context, attempt *startAttempt, claim *workClaim) {
	defer claim.release()
	defer attempt.operationStop()

	outcome := e.source.bind(ctx, func(ids AttachmentIDs) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.transferStartOwnershipLocked(attempt, ids)
	})

	e.mu.Lock()
	if e.terminal != nil {
		e.mu.Unlock()
		return
	}
	proceed, failure := e.reconcileBindLocked(attempt, outcome)
	if attempt.pendingCause != nil {
		failure = *attempt.pendingCause
		proceed = false
	}
	if !proceed {
		e.requestTerminalLocked(failure)
		e.mu.Unlock()
		return
	}
	active, cause := e.prepareStartCutLocked(attempt)
	if cause.valid() {
		e.requestTerminalLocked(cause)
		e.mu.Unlock()
		return
	}
	lease := e.lease
	e.mu.Unlock()
	go e.watchCut(active, lease)

	outcomePrepare := e.runCut(ctx, active, attempt, lease)
	if outcomePrepare.kind == prepareFailed {
		e.mu.Lock()
		e.requestTerminalLocked(outcomePrepare.cause)
		e.mu.Unlock()
	}
	<-attempt.done
}

func (e *Epoch) transferStartOwnershipLocked(attempt *startAttempt, ids AttachmentIDs) error {
	if e.start != attempt || attempt == nil || attempt.phase != startPhaseBinding ||
		attempt.ownership.kind != ownershipPending || !ids.exact() {
		return ErrInvariant
	}
	attempt.ownership = exactOwnership(ids)
	return nil
}

func (e *Epoch) reconcileBindLocked(attempt *startAttempt, outcome bindOutcome) (bool, terminalCause) {
	if e.start != attempt || attempt == nil || attempt.phase != startPhaseBinding {
		return false, invariantTerminalCause(fmt.Errorf("BIND returned for a non-binding attempt"))
	}
	if !outcome.valid() {
		if attempt.ownership.kind == ownershipPending {
			attempt.ownership = noOwnership()
		}
		return false, invariantTerminalCause(fmt.Errorf("invalid BIND outcome"))
	}
	switch outcome.kind {
	case bindSucceededExact, bindFailedExact:
		if attempt.ownership.kind != ownershipExact || attempt.ownership.ids != outcome.ids {
			if outcome.ids.exact() && attempt.ownership.kind == ownershipPending {
				attempt.ownership = exactOwnership(outcome.ids)
			}
			return false, invariantTerminalCause(fmt.Errorf("BIND exact ownership transfer contradicted its outcome"))
		}
	case bindFailedNoOwner:
		if attempt.ownership.kind != ownershipPending {
			return false, invariantTerminalCause(fmt.Errorf("BIND no-owner outcome followed an exact transfer"))
		}
		attempt.ownership = noOwnership()
	case bindInvalid:
		if outcome.ids.exact() && attempt.ownership.kind == ownershipPending {
			attempt.ownership = exactOwnership(outcome.ids)
		} else if attempt.ownership.kind == ownershipPending {
			attempt.ownership = noOwnership()
		}
		return false, invariantTerminalCause(outcome.cause)
	default:
		if attempt.ownership.kind == ownershipPending {
			attempt.ownership = noOwnership()
		}
		return false, invariantTerminalCause(fmt.Errorf("unknown BIND outcome"))
	}
	if outcome.kind == bindSucceededExact {
		return true, terminalCause{}
	}
	return false, faultTerminalCause(outcome.cause)
}

func (e *Epoch) prepareStartCutLocked(attempt *startAttempt) (*activeCut, terminalCause) {
	if e.start != attempt || attempt.phase != startPhaseBinding || attempt.pendingCause != nil ||
		attempt.ownership.kind != ownershipExact || e.active != nil || e.protocol.state != stateAwaitPrepare {
		return nil, invariantTerminalCause(fmt.Errorf("illegal INITIAL/RECONNECT CUT transition"))
	}
	if err := e.scheduler.BeginExternal(); err != nil {
		return nil, faultTerminalCause(err)
	}
	e.nextCut++
	marker := e.markerPayload(e.nextCut, attempt.ownership.ids)
	cut, err := newCut(e.nextCut, attempt.kind, marker)
	if err != nil {
		return nil, invariantTerminalCause(err)
	}
	adoption := cut.adoptPreMarker(&attempt.replay)
	if !adoption.valid() || adoption.kind != replayAdopted {
		return nil, invariantTerminalCause(adoption.cause)
	}
	active := &activeCut{cut: cut, historyRows: e.historyRows, timer: e.clock.NewTimer(e.cutTimeout), done: make(chan struct{})}
	e.active = active
	attempt.phase = startPhaseCutting
	return active, terminalCause{}
}

// PTYBytes is the only PTY-output route and the sole ingress-framer owner.
func (e *Epoch) PTYBytes(data []byte) error {
	if len(data) == 0 || len(data) > MaxEgressBytes {
		return ErrMalformed
	}
	e.mu.Lock()
	if e.terminal != nil {
		err := e.terminal.publicErr
		e.mu.Unlock()
		return err
	}
	if e.active == nil && !e.bindingIngressLocked() && (e.protocol.state != stateLive || e.protocol.cut == 0) {
		e.mu.Unlock()
		return ErrOutOfState
	}
	if e.start != nil && e.start.phase == startPhaseBinding && e.start.pendingCause != nil {
		err := e.start.pendingCause.diagnostic
		e.mu.Unlock()
		return err
	}
	events, ingressErr := e.ingress.feed(data)
	var internalErr error
	var activity bool
	for _, event := range events {
		if event.kind == ingressPrivateMarker {
			if e.active == nil {
				internalErr = fmt.Errorf("reserved Persea marker outside active cut")
			} else {
				internalErr = e.active.cut.acceptMarker(event.payload)
				if internalErr == nil {
					prepare := e.maybePublishPrepareLocked()
					switch prepare.kind {
					case prepareFailed:
						request := e.requestTerminalLocked(prepare.cause)
						if request.kind == terminalRequestResolved {
							internalErr = request.result.publicError()
						} else {
							internalErr = prepare.cause.diagnostic
						}
					case prepareAlreadyTerminal:
						internalErr = prepare.result.publicError()
					}
				}
			}
		} else {
			var eventActivity bool
			eventActivity, internalErr = e.routeOrdinaryLocked(event.payload)
			activity = activity || eventActivity
		}
		if internalErr != nil {
			break
		}
	}
	if internalErr == nil {
		internalErr = ingressErr
	}
	if internalErr != nil {
		request := e.requestTerminalLocked(faultTerminalCause(internalErr))
		if request.kind == terminalRequestResolved {
			internalErr = request.result.publicError()
		}
	}
	e.mu.Unlock()
	if internalErr != nil {
		return internalErr
	}
	// A journal-backed writer already owns ordered output and replay. Scheduling
	// presentation captures here races its generation refit against this epoch's
	// old geometry witness. Explicit initial and row cuts still use the scheduler.
	if activity && !e.egress.writerOwnsOutput {
		if err := e.scheduler.Activity(); err != nil {
			e.terminate(faultTerminalCause(err))
			return e.Err()
		}
	}
	return nil
}

func (e *Epoch) bindingIngressLocked() bool {
	return e.start != nil && e.start.phase == startPhaseBinding && e.start.result.kind == lifecycleResultInvalid
}

func (e *Epoch) routeOrdinaryLocked(data []byte) (bool, error) {
	if e.active != nil {
		route, err := e.active.cut.feedOrdinary(data)
		if err != nil {
			return false, err
		}
		if len(route.live) > 0 {
			frame := e.liveFrameLocked(e.protocol.cut, route.live)
			next := *e.protocol
			if err = next.apply(fromServer, frame); err == nil {
				if err = e.egress.enqueue(frame); err == nil {
					*e.protocol = next
				}
			}
			if err != nil {
				return false, err
			}
		}
		prepare := e.maybePublishPrepareLocked()
		switch prepare.kind {
		case prepareFailed:
			request := e.requestTerminalLocked(prepare.cause)
			if request.kind == terminalRequestResolved {
				return false, request.result.publicError()
			}
			return false, prepare.cause.diagnostic
		case prepareAlreadyTerminal:
			return false, prepare.result.publicError()
		}
		return route.activity, nil
	}
	if e.bindingIngressLocked() {
		if e.start.pendingCause != nil {
			return false, e.start.pendingCause.diagnostic
		}
		if len(data) > ReplayByteCap-len(e.start.replay) {
			return false, ErrSaturated
		}
		e.start.replay = append(e.start.replay, data...)
		return false, nil
	}
	if e.protocol.state != stateLive || e.protocol.cut == 0 {
		return false, ErrOutOfState
	}
	frame := e.liveFrameLocked(e.protocol.cut, data)
	next := *e.protocol
	if err := next.apply(fromServer, frame); err != nil {
		return false, err
	}
	if err := e.egress.enqueue(frame); err != nil {
		return false, err
	}
	*e.protocol = next
	return true, nil
}

func (e *Epoch) liveFrameLocked(cut uint64, data []byte) Frame {
	return Frame{Version: ProtocolVersion, Type: FrameLive, Source: e.sourceID, Epoch: e.epoch, Cut: cut, Data: append([]byte(nil), data...)}
}

// HandleFrame is the sole browser-input route. Public request preconditions
// remain public; failures after an admitted component operation are terminal.
func (e *Epoch) HandleFrame(frame Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	if e.terminal != nil {
		err := e.terminal.publicErr
		e.mu.Unlock()
		return err
	}
	if frame.Source != e.sourceID || frame.Epoch != e.epoch {
		e.mu.Unlock()
		return ErrStale
	}
	var internalErr error
	var complete, retry bool
	switch frame.Type {
	case FrameReady:
		if e.active == nil || !e.active.prepared || e.active.cut.ID != frame.Cut {
			e.mu.Unlock()
			return ErrStale
		}
		next := *e.protocol
		if err := next.apply(fromBrowser, frame); err != nil {
			e.mu.Unlock()
			return err
		}
		commit := Frame{Version: ProtocolVersion, Type: FrameCommit, Source: e.sourceID, Epoch: e.epoch, Cut: frame.Cut}
		if internalErr = next.apply(fromServer, commit); internalErr != nil {
			break
		}
		held := e.active.cut.heldBytes()
		frames := []Frame{commit}
		if len(held) > 0 {
			frames = append(frames, e.liveFrameLocked(frame.Cut, held))
		}
		if internalErr = e.releaseTransitionalLocked(&next, &frames); internalErr != nil {
			break
		}
		if internalErr = e.egress.enqueueBatch(frames); internalErr != nil {
			clear(held)
			break
		}
		*e.protocol = next
		if len(e.transitionalInput) > 0 {
			if internalErr = e.input.enqueueBatch(e.sourceID, e.epoch, e.transitionalInput); internalErr != nil {
				break
			}
		}
		e.clearTransitionalLocked()
		if e.active.cut.Kind == CutHistory {
			if e.active.historyRequest == nil || e.active.historyRequest.phase != historyRequestIssued {
				internalErr = ErrInvariant
				break
			}
			e.active.historyRequest.phase = historyRequestCommitted
			e.historyRows = e.active.historyRows
		}
		e.active.cut.discardHeld()
		e.finishActiveLocked()
		complete = true
	case FrameDefer:
		if e.active == nil || !e.active.prepared || e.active.cut.ID != frame.Cut {
			e.mu.Unlock()
			return ErrStale
		}
		if e.active.cut.Kind != CutOngoing && e.active.cut.Kind != CutHistory {
			internalErr = ErrReopenRequired
			break
		}
		next := *e.protocol
		if err := next.apply(fromBrowser, frame); err != nil {
			e.mu.Unlock()
			return err
		}
		held := e.active.cut.heldBytes()
		frames := []Frame{}
		if len(held) > 0 {
			frames = append(frames, e.liveFrameLocked(frame.Cut, held))
		}
		if internalErr = e.releaseTransitionalLocked(&next, &frames); internalErr != nil {
			clear(held)
			break
		}
		if len(frames) > 0 {
			if internalErr = e.egress.enqueueBatch(frames); internalErr != nil {
				clear(held)
				break
			}
		}
		*e.protocol = next
		if len(e.transitionalInput) > 0 {
			if internalErr = e.input.enqueueBatch(e.sourceID, e.epoch, e.transitionalInput); internalErr != nil {
				break
			}
		}
		e.clearTransitionalLocked()
		deferredKind := e.active.cut.Kind
		if deferredKind == CutHistory {
			if e.active.historyRequest == nil || e.active.historyRequest.phase != historyRequestIssued {
				internalErr = ErrInvariant
				break
			}
			e.active.historyRequest.phase = historyRequestDeferred
		}
		e.active.cut.discardHeld()
		e.finishActiveLocked()
		complete, retry = true, deferredKind == CutOngoing
	case FrameModeRequest:
		if e.transitionalOngoingLocked() {
			if len(e.transitionalModes) >= MaxTransitionalModeRequests {
				internalErr = ErrSaturated
				break
			}
			e.transitionalModes = append(e.transitionalModes, frame.Mode)
			break
		}
		next := *e.protocol
		if err := next.apply(fromBrowser, frame); err != nil {
			e.mu.Unlock()
			return err
		}
		mode := Frame{Version: ProtocolVersion, Type: FrameMode, Source: e.sourceID, Epoch: e.epoch, Mode: frame.Mode}
		if internalErr = next.apply(fromServer, mode); internalErr != nil {
			break
		}
		if internalErr = e.input.setMode(frame.Mode); internalErr != nil {
			break
		}
		if internalErr = e.egress.enqueue(mode); internalErr != nil {
			_ = e.input.setMode(ModeObserve)
			break
		}
		*e.protocol = next
	case FrameInput:
		if e.transitionalOngoingLocked() {
			if e.protocol.mode != ModeControl {
				e.mu.Unlock()
				return ErrObserveOnly
			}
			if len(e.transitionalInput) >= MaxInputFrames || len(frame.Data) > MaxInputBytes-e.transitionalInputBytes {
				internalErr = ErrSaturated
				break
			}
			e.transitionalInput = append(e.transitionalInput, append([]byte(nil), frame.Data...))
			e.transitionalInputBytes += len(frame.Data)
			break
		}
		next := *e.protocol
		if err := next.apply(fromBrowser, frame); err != nil {
			e.mu.Unlock()
			return err
		}
		internalErr = e.input.enqueue(frame.Source, frame.Epoch, frame.Data)
	default:
		e.mu.Unlock()
		return ErrOutOfState
	}
	if internalErr != nil {
		request := e.requestTerminalLocked(faultTerminalCause(internalErr))
		if request.kind == terminalRequestResolved {
			internalErr = request.result.publicError()
		}
	}
	e.mu.Unlock()
	if internalErr != nil {
		return internalErr
	}
	if complete {
		if err := e.scheduler.Complete(retry && !e.egress.writerOwnsOutput); err != nil {
			e.terminate(faultTerminalCause(err))
			return e.Err()
		}
		go e.beginPendingHistory()
	}
	return nil
}

// Resize is the sole geometry mutation route. It accepts only an effective
// Control request at a committed LIVE cut, pauses scheduled cuts, performs one
// exact-incarnation tmux resize, then publishes a candidate RESIZE cut.
func (e *Epoch) Resize(ctx context.Context, frame Frame) error {
	if ctx == nil || frame.Type != FrameResize {
		return ErrMalformed
	}
	if err := frame.Validate(); err != nil {
		return err
	}
	claim, err := e.gate.claim("resize")
	if err != nil {
		if terminal := e.Err(); terminal != nil {
			return terminal
		}
		return err
	}
	defer claim.release()

	e.mu.Lock()
	if e.terminal != nil {
		err := e.terminal.publicErr
		e.mu.Unlock()
		return err
	}
	if frame.Source != e.sourceID || frame.Epoch != e.epoch || e.active != nil ||
		e.protocol.state != stateLive || e.protocol.mode != ModeControl ||
		e.start == nil || e.start.ownership.kind != ownershipExact {
		e.mu.Unlock()
		return ErrOutOfState
	}
	next := *e.protocol
	if err := next.apply(fromBrowser, frame); err != nil {
		e.mu.Unlock()
		return err
	}
	if err := e.scheduler.BeginExternal(); err != nil {
		request := e.requestTerminalLocked(faultTerminalCause(err))
		e.mu.Unlock()
		return request.result.publicError()
	}
	e.nextCut++
	ids := e.start.ownership.ids
	marker := e.markerPayload(e.nextCut, ids)
	cut, err := newCut(e.nextCut, CutResize, marker)
	if err != nil {
		request := e.requestTerminalLocked(invariantTerminalCause(err))
		e.mu.Unlock()
		return request.result.publicError()
	}
	active := &activeCut{cut: cut, historyRows: e.historyRows, timer: e.clock.NewTimer(e.cutTimeout), done: make(chan struct{})}
	e.active = active
	*e.protocol = next
	lease := e.lease
	e.mu.Unlock()
	go e.watchCut(active, lease)

	if err := e.source.resize(ctx, ids, frame.Columns, frame.Rows); err != nil {
		if IsResizeRefusal(err) {
			return e.abandonResize(active, lease, err)
		}
		// Not a proven pre-issue refusal: the geometry command was issued, or
		// its outcome is unknown, or the source is dead. tmux may hold the new
		// geometry while this epoch's cut sequence and the durable journal do
		// not; keeping the epoch live would publish held output under a
		// geometry that is no longer true. The attachment is retired, and a
		// reopen re-mints against whatever tmux actually holds.
		e.mu.Lock()
		request := e.requestTerminalLocked(faultTerminalCause(err))
		e.mu.Unlock()
		return request.result.publicError()
	}
	prepare := e.runCut(ctx, active, nil, lease)
	if prepare.kind == prepareFailed {
		e.mu.Lock()
		request := e.requestTerminalLocked(prepare.cause)
		e.mu.Unlock()
		return request.result.publicError()
	}
	if prepare.kind == prepareAlreadyTerminal {
		return prepare.result.publicError()
	}
	return nil
}

// abandonResize retires a RESIZE cut whose tmux transaction was refused before
// it was issued. Its only caller has proven that with a typed *ResizeRefusal:
// tmux, the attachment PTY and the journal are exactly as they were. The cut
// was never issued either — no marker was written and no capture taken — so
// there is nothing to reconcile: the bytes the pending cut was holding back are
// released as LIVE output of the cut still in force, the scheduler's external
// hold ends, and the epoch carries on exactly as before the request. A refused
// Fit is one request's outcome, not a fault of the attachment. A resize that
// may have applied never reaches here: `active.cutIssued` records only the
// protocol cut marker and proves nothing about tmux, which is why the
// certainty comes from the source's typed outcome and not from this cut.
func (e *Epoch) abandonResize(active *activeCut, lease generationLease, cause error) error {
	e.mu.Lock()
	if e.terminal != nil {
		err := e.terminal.publicErr
		e.mu.Unlock()
		return err
	}
	if lease.validate(e.lease) != nil || e.active != active || active.cutIssued {
		request := e.requestTerminalLocked(invariantTerminalCause(fmt.Errorf("RESIZE abandonment contradicted its active cut")))
		e.mu.Unlock()
		return request.result.publicError()
	}
	released := active.cut.preMarker()
	if len(released) > 0 {
		frame := e.liveFrameLocked(e.protocol.cut, released)
		next := *e.protocol
		err := next.apply(fromServer, frame)
		if err == nil {
			err = e.egress.enqueue(frame)
		}
		if err != nil {
			request := e.requestTerminalLocked(faultTerminalCause(err))
			e.mu.Unlock()
			return request.result.publicError()
		}
		*e.protocol = next
	}
	active.cut.discardAll()
	e.finishActiveLocked()
	e.mu.Unlock()
	if err := e.scheduler.Complete(len(released) > 0 && !e.egress.writerOwnsOutput); err != nil {
		e.terminate(faultTerminalCause(err))
		return e.Err()
	}
	return fmt.Errorf("%w: %w", ErrResizeFailed, cause)
}

// RequestHistory records one read-only desired depth on the bound epoch. All
// recoverable decisions happen while its request is still pre-CUT; exact-source
// CUT issuance transfers the request to the terminal failure domain.
func (e *Epoch) RequestHistory(frame Frame) error {
	if frame.Type != FrameHistory {
		return ErrMalformed
	}
	if err := frame.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	if e.terminal != nil {
		err := e.terminal.publicErr
		e.mu.Unlock()
		return err
	}
	if frame.Source != e.sourceID || frame.Epoch != e.epoch {
		e.mu.Unlock()
		return ErrStale
	}
	next := *e.protocol
	if err := next.apply(fromBrowser, frame); err != nil {
		e.mu.Unlock()
		return err
	}
	if frame.Request <= e.lastHistoryRequest {
		activeMatch := e.active != nil && e.active.historyRequest != nil &&
			e.active.historyRequest.request == frame.Request && e.active.historyRequest.rows == frame.HistoryRows
		pendingMatch := e.pendingHistory != nil &&
			e.pendingHistory.request == frame.Request && e.pendingHistory.rows == frame.HistoryRows
		e.mu.Unlock()
		if activeMatch || pendingMatch {
			return nil
		}
		return ErrStale
	}
	request := &historyRequest{request: frame.Request, rows: frame.HistoryRows, phase: historyRequestQueued}
	if e.pendingHistory != nil {
		if err := e.pendingHistory.supersedeBeforeIssue(); err != nil {
			request := e.requestTerminalLocked(invariantTerminalCause(err))
			e.mu.Unlock()
			return request.result.publicError()
		}
	}
	e.pendingHistory = request
	e.lastHistoryRequest = frame.Request
	e.mu.Unlock()
	go e.beginPendingHistory()
	return nil
}

func (e *Epoch) beginPendingHistory() {
	claim, err := e.gate.claim("history-cut")
	if err != nil {
		if e.Err() == nil {
			e.terminate(faultTerminalCause(err))
		}
		return
	}
	defer claim.release()

	e.mu.Lock()
	if e.terminal != nil || e.pendingHistory == nil {
		e.mu.Unlock()
		return
	}
	if e.active != nil || e.protocol.state != stateLive || e.start == nil || e.start.ownership.kind != ownershipExact {
		e.mu.Unlock()
		return
	}
	request := e.pendingHistory
	if request.phase != historyRequestQueued {
		terminalRequest := e.requestTerminalLocked(invariantTerminalCause(fmt.Errorf("pending history request crossed its pre-CUT boundary")))
		e.mu.Unlock()
		_ = terminalRequest
		return
	}
	if err := e.scheduler.BeginExternal(); err != nil {
		terminalRequest := e.requestTerminalLocked(faultTerminalCause(err))
		e.mu.Unlock()
		_ = terminalRequest
		return
	}
	e.pendingHistory = nil
	e.nextCut++
	marker := e.markerPayload(e.nextCut, e.start.ownership.ids)
	cut, err := newCut(e.nextCut, CutHistory, marker)
	if err != nil {
		terminalRequest := e.requestTerminalLocked(invariantTerminalCause(err))
		e.mu.Unlock()
		_ = terminalRequest
		return
	}
	active := &activeCut{
		cut: cut, historyRows: request.rows, historyRequest: request,
		timer: e.clock.NewTimer(e.cutTimeout), done: make(chan struct{}),
	}
	e.active = active
	lease := e.lease
	e.mu.Unlock()
	go e.watchCut(active, lease)

	prepare := e.runCut(e.ctx, active, nil, lease)
	if prepare.kind == prepareFailed {
		e.mu.Lock()
		e.requestTerminalLocked(prepare.cause)
		e.mu.Unlock()
	}
}

func (e *Epoch) transitionalOngoingLocked() bool {
	return e.active != nil && e.active.prepared && (e.active.cut.Kind == CutOngoing || e.active.cut.Kind == CutHistory) &&
		(e.protocol.state == statePrepared || e.protocol.state == stateReady)
}

// releaseTransitionalLocked moves an ONGOING cut's server-owned input into the
// PTY pump only after the same cut has returned to LIVE. The input batch is
// reserved atomically, and mode requests are serialized after it. A requested
// downgrade is explicitly retried when applying it would revoke admitted input.
func (e *Epoch) releaseTransitionalLocked(next *automaton, frames *[]Frame) error {
	if next == nil || next.state != stateLive {
		return ErrInvariant
	}
	if len(e.transitionalInput) > 0 {
		if err := e.input.canEnqueueBatch(e.sourceID, e.epoch, e.transitionalInput); err != nil {
			return err
		}
	}
	for _, requested := range e.transitionalModes {
		if requested == ModeObserve && len(e.transitionalInput) > 0 {
			*frames = append(*frames, Frame{Version: ProtocolVersion, Type: FrameMode, Source: e.sourceID, Epoch: e.epoch, Mode: next.mode, Reason: "retry_after_cut"})
			continue
		}
		request := Frame{Version: ProtocolVersion, Type: FrameModeRequest, Source: e.sourceID, Epoch: e.epoch, Mode: requested}
		if err := next.apply(fromBrowser, request); err != nil {
			return err
		}
		mode := Frame{Version: ProtocolVersion, Type: FrameMode, Source: e.sourceID, Epoch: e.epoch, Mode: requested}
		if err := next.apply(fromServer, mode); err != nil {
			return err
		}
		if err := e.input.setMode(requested); err != nil {
			return err
		}
		*frames = append(*frames, mode)
	}
	return nil
}

func (e *Epoch) clearTransitionalLocked() {
	for i := range e.transitionalInput {
		clear(e.transitionalInput[i])
	}
	e.transitionalInput = nil
	e.transitionalInputBytes = 0
	e.transitionalModes = nil
}

// Fault acknowledges a typed terminal request without waiting on a possibly
// synchronous caller's own WorkGate claim.
func (e *Epoch) Fault(cause error) error {
	e.mu.Lock()
	e.requestTerminalLocked(faultTerminalCause(cause))
	e.mu.Unlock()
	return nil
}

// Finalize joins the one teardown and may retry only its immutable exact-ID
// cleanup obligation.
func (e *Epoch) Finalize(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	var cause terminalCause
	if parentErr := e.ctx.Err(); parentErr != nil {
		cause = canceledTerminalCause(parentErr)
	} else if finishErr := e.finalizeCauseLocked(); finishErr != nil {
		cause = faultTerminalCause(finishErr)
	} else {
		cause = orderlyTerminalCause()
	}
	request := e.requestTerminalLocked(cause)
	e.mu.Unlock()
	if request.kind == terminalRequestDeferred {
		select {
		case <-request.wait.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-e.teardownDone:
	case <-ctx.Done():
		return ctx.Err()
	}

	e.cleanupMu.Lock()
	defer e.cleanupMu.Unlock()
	if !e.cleanup.complete && e.cleanup.err != nil && !e.cleanup.reported {
		e.cleanup.reported = true
		return errors.Join(e.cleanup.teardownErr, e.cleanup.err)
	}
	if !e.cleanup.complete {
		e.mu.Lock()
		ownership := e.terminal.ownership
		e.mu.Unlock()
		e.executeCleanupLocked(ctx, ownership)
	}
	if e.cleanup.complete {
		return e.cleanup.teardownErr
	}
	return errors.Join(e.cleanup.teardownErr, e.cleanup.err)
}

func (e *Epoch) finalizeCauseLocked() error {
	if cause := e.ctx.Err(); cause != nil {
		return cause
	}
	return e.finishIngressLocked()
}

func (e *Epoch) finishIngressLocked() error {
	events, err := e.ingress.finish()
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.kind != ingressOrdinary {
			return fmt.Errorf("private marker completed at ingress EOF")
		}
		if _, err := e.routeOrdinaryLocked(event.payload); err != nil {
			return err
		}
	}
	return nil
}

func (e *Epoch) terminate(cause terminalCause) {
	e.mu.Lock()
	e.requestTerminalLocked(cause)
	e.mu.Unlock()
}

func (e *Epoch) ownerLoop() {
	defer close(e.ownerDone)
	for {
		select {
		case <-e.ctx.Done():
			e.terminate(canceledTerminalCause(e.ctx.Err()))
			return
		case err := <-e.input.failures():
			e.terminate(faultTerminalCause(err))
			return
		case err := <-e.egress.failures():
			e.terminate(faultTerminalCause(err))
			return
		case <-e.scheduler.Due():
			if err := e.beginOngoingCut(e.ctx); err != nil {
				e.terminate(faultTerminalCause(err))
				return
			}
		}
	}
}

func (e *Epoch) beginOngoingCut(ctx context.Context) error {
	claim, err := e.gate.claim("ongoing-cut")
	if err != nil {
		if terminal := e.Err(); terminal != nil {
			return terminal
		}
		return err
	}
	defer claim.release()
	e.mu.Lock()
	if e.terminal != nil {
		err := e.terminal.publicErr
		e.mu.Unlock()
		return err
	}
	if e.active != nil || e.protocol.state != stateLive || e.start == nil || e.start.ownership.kind != ownershipExact {
		request := e.requestTerminalLocked(invariantTerminalCause(fmt.Errorf("illegal ONGOING CUT transition")))
		e.mu.Unlock()
		return request.result.publicError()
	}
	e.nextCut++
	marker := e.markerPayload(e.nextCut, e.start.ownership.ids)
	cut, newErr := newCut(e.nextCut, CutOngoing, marker)
	if newErr != nil {
		request := e.requestTerminalLocked(invariantTerminalCause(newErr))
		e.mu.Unlock()
		return request.result.publicError()
	}
	active := &activeCut{cut: cut, historyRows: e.historyRows, timer: e.clock.NewTimer(e.cutTimeout), done: make(chan struct{})}
	e.active = active
	lease := e.lease
	e.mu.Unlock()
	go e.watchCut(active, lease)

	prepare := e.runCut(ctx, active, nil, lease)
	if prepare.kind == prepareFailed {
		e.mu.Lock()
		request := e.requestTerminalLocked(prepare.cause)
		e.mu.Unlock()
		return request.result.publicError()
	}
	if prepare.kind == prepareAlreadyTerminal {
		return prepare.result.publicError()
	}
	return nil
}

func (e *Epoch) runCut(ctx context.Context, active *activeCut, attempt *startAttempt, lease generationLease) prepareOutcome {
	var ids AttachmentIDs
	e.mu.Lock()
	if attempt != nil {
		ids = attempt.ownership.ids
	} else if e.start != nil && e.start.ownership.kind == ownershipExact {
		ids = e.start.ownership.ids
	}
	e.mu.Unlock()
	e.mu.Lock()
	if e.active != active || active.cutIssued {
		e.mu.Unlock()
		return prepareOutcome{kind: prepareFailed, cause: invariantTerminalCause(fmt.Errorf("CUT issuance contradicted its active owner"))}
	}
	active.cutIssued = true
	if active.historyRequest != nil {
		if active.historyRequest.phase != historyRequestQueued {
			e.mu.Unlock()
			return prepareOutcome{kind: prepareFailed, cause: invariantTerminalCause(fmt.Errorf("history CUT issuance crossed a non-queued request"))}
		}
		active.historyRequest.phase = historyRequestIssued
	}
	e.mu.Unlock()
	outcome := e.source.cut(ctx, ids, active.cut.ID, active.cut.Kind, active.cut.markerPayload, active.historyRows)
	if !outcome.valid() {
		return prepareOutcome{kind: prepareFailed, cause: invariantTerminalCause(fmt.Errorf("invalid CUT outcome"))}
	}
	if outcome.kind == cutFailed {
		return prepareOutcome{kind: prepareFailed, cause: faultTerminalCause(outcome.cause)}
	}
	rows, truncated, err := boundedOffscreenRows(outcome.capture.Capture, active.historyRows)
	if err != nil {
		return prepareOutcome{kind: prepareFailed, cause: faultTerminalCause(err)}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.terminal != nil {
		return prepareOutcome{kind: prepareAlreadyTerminal, result: alreadyTerminalResult(e.terminal)}
	}
	if lease.validate(e.lease) != nil || e.active != active {
		return prepareOutcome{kind: prepareFailed, cause: invariantTerminalCause(fmt.Errorf("CUT completion lost its active owner"))}
	}
	active.captureReady, active.history, active.truncated = true, rows, truncated
	if attempt != nil {
		if e.start != attempt || attempt.phase != startPhaseCutting {
			return prepareOutcome{kind: prepareFailed, cause: invariantTerminalCause(fmt.Errorf("CUT completion contradicted its start attempt"))}
		}
		attempt.phase = startPhaseAwaitingPrepare
	}
	return e.maybePublishPrepareLocked()
}

func (e *Epoch) maybePublishPrepareLocked() prepareOutcome {
	if e.terminal != nil {
		return prepareOutcome{kind: prepareAlreadyTerminal, result: alreadyTerminalResult(e.terminal)}
	}
	active := e.active
	if active == nil {
		return prepareOutcome{kind: prepareFailed, cause: invariantTerminalCause(fmt.Errorf("PREPARE has no active cut"))}
	}
	if active.prepared || !active.captureReady || !active.cut.markerFound() {
		return prepareOutcome{kind: prepareWaiting}
	}
	if active.cut.Kind == CutInitial || active.cut.Kind == CutReconnect {
		if e.start == nil || e.start.phase != startPhaseAwaitingPrepare || e.start.pendingCause != nil ||
			e.start.ownership.kind != ownershipExact {
			return prepareOutcome{kind: prepareFailed, cause: invariantTerminalCause(fmt.Errorf("PREPARE contradicted its start attempt"))}
		}
	}
	_, columns, rows := e.source.publicIdentity()
	frame := Frame{
		Version: ProtocolVersion, Type: FramePrepare, Source: e.sourceID, Epoch: e.epoch,
		Cut: active.cut.ID, Kind: active.cut.Kind,
		Columns: columns, Rows: rows,
	}
	if !e.egress.writerOwnsOutput {
		frame.History, frame.Replay = append([]string(nil), active.history...), active.cut.preMarker()
		frame.Truncated = active.truncated
	}
	if active.cut.Kind == CutHistory {
		if active.historyRequest == nil || active.historyRequest.phase != historyRequestIssued {
			return prepareOutcome{kind: prepareFailed, cause: invariantTerminalCause(fmt.Errorf("HISTORY PREPARE has no issued request"))}
		}
		frame.Request = active.historyRequest.request
		frame.EffectiveHistoryRows = active.historyRows
	}
	next := *e.protocol
	if err := next.apply(fromServer, frame); err != nil {
		return prepareOutcome{kind: prepareFailed, cause: faultTerminalCause(err)}
	}
	if err := e.egress.enqueue(frame); err != nil {
		return prepareOutcome{kind: prepareFailed, cause: faultTerminalCause(err)}
	}
	*e.protocol = next
	active.prepared = true
	if active.cut.Kind == CutInitial || active.cut.Kind == CutReconnect {
		e.start.result = preparedResult()
		e.start.phase = startPhasePrepared
		close(e.start.done)
	}
	return prepareOutcome{kind: preparePublished}
}

func (e *Epoch) markerPayload(cut uint64, ids AttachmentIDs) []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%s", e.sourceID, e.epoch, cut, ids.ShadowSessionID, ids.ClientID)))
	return []byte(markerNamespace + base64.RawStdEncoding.EncodeToString(sum[:]))
}

func (e *Epoch) finishActiveLocked() {
	if e.active == nil {
		return
	}
	e.active.timer.Stop()
	close(e.active.done)
	e.active = nil
}

func (e *Epoch) requestTerminalLocked(cause terminalCause) terminalRequest {
	if e.terminal != nil {
		return terminalRequest{kind: terminalRequestResolved, result: alreadyTerminalResult(e.terminal)}
	}
	if !cause.valid() {
		cause = invariantTerminalCause(fmt.Errorf("invalid terminal cause"))
	}
	if e.start != nil && e.start.phase == startPhaseBinding && e.start.ownership.kind == ownershipPending {
		if e.start.pendingCause == nil {
			copyCause := cause
			e.start.pendingCause = &copyCause
			e.ingress.discard()
			clear(e.start.replay)
			e.start.replay = nil
			e.start.operationStop()
		}
		return terminalRequest{kind: terminalRequestDeferred, wait: e.start}
	}
	if e.start != nil && e.start.pendingCause != nil {
		cause = *e.start.pendingCause
	}
	result := e.publishTerminalLocked(cause)
	return terminalRequest{kind: terminalRequestResolved, result: result}
}

// publishTerminalLocked is the only terminalResult assignment, Done close, END
// publication, and teardown launch site. The complete result is immutable.
func (e *Epoch) publishTerminalLocked(cause terminalCause) lifecycleResult {
	if e.terminal != nil {
		return alreadyTerminalResult(e.terminal)
	}
	ownership := noOwnership()
	if e.start != nil {
		ownership = e.start.ownership
	}
	if ownership.kind == ownershipPending {
		return lifecycleResult{}
	}
	if !ownership.valid() {
		ownership = noOwnership()
		cause = invariantTerminalCause(fmt.Errorf("invalid terminal ownership"))
	}
	if !cause.valid() {
		cause = invariantTerminalCause(fmt.Errorf("invalid terminal cause"))
	}

	e.ingress.discard()
	e.clearTransitionalLocked()
	if e.start != nil {
		clear(e.start.replay)
		e.start.replay = nil
	}
	e.lease.generation++
	if e.active != nil {
		e.active.cut.discardAll()
		e.finishActiveLocked()
	}

	endReason := cause.endReason()
	end := Frame{Version: ProtocolVersion, Type: FrameEnd, Source: e.sourceID, Epoch: e.epoch, Reason: endReason}
	next := *e.protocol
	var endErr error
	if err := next.apply(fromServer, end); err != nil {
		endErr = err
	} else if err := e.egress.enqueue(end); err != nil {
		endErr = err
	} else {
		*e.protocol = next
	}
	result := &terminalResult{
		cause: cause, publicErr: errors.Join(ErrClosed, cause.diagnostic),
		endReason: endReason, ownership: ownership, endErr: endErr,
	}
	if !result.valid() {
		fallback := invariantTerminalCause(fmt.Errorf("constructed invalid terminal result"))
		result = &terminalResult{
			cause: fallback, publicErr: errors.Join(ErrClosed, fallback.diagnostic),
			endReason: fallback.endReason(), ownership: ownership, endErr: endErr,
		}
	}
	e.terminal = result
	// Input cutoff belongs to publication, not to teardown progress. Sealing
	// cancels admission; teardown retains and joins any active writer.
	e.input.seal()
	if e.start != nil && e.start.result.kind == lifecycleResultInvalid {
		e.start.result = terminalLifecycleResult(result)
		e.start.phase = startPhaseTerminal
		close(e.start.done)
	}
	close(e.done)
	e.cancel()
	e.teardownOnce.Do(func() { go e.teardown() })
	return terminalLifecycleResult(result)
}

func (e *Epoch) teardown() {
	defer close(e.teardownDone)
	e.scheduler.Stop()
	inputErr := e.input.sealAndStop()
	<-e.ownerDone
	<-e.gate.seal()
	egressErr := e.egress.finishAndWait()

	e.mu.Lock()
	terminal := e.terminal
	e.mu.Unlock()
	e.cleanupMu.Lock()
	teardownErr := errors.Join(terminal.endErr, inputErr, egressErr)
	cleanupCtx, cancel := context.WithTimeout(context.Background(), e.cutTimeout)
	e.executeCleanupLocked(cleanupCtx, terminal.ownership)
	cancel()
	if e.attachments != nil {
		teardownErr = errors.Join(teardownErr, e.attachments.ReleaseAttachment(e))
	}
	e.cleanup.teardownErr = teardownErr
	e.cleanupMu.Unlock()
}

func (e *Epoch) executeCleanupLocked(ctx context.Context, ownership attachmentOwnership) {
	if e.cleanup.complete {
		return
	}
	switch ownership.kind {
	case ownershipNone:
		e.cleanup.err = nil
		e.cleanup.complete = true
	case ownershipExact:
		e.cleanup.err = e.source.cleanupAttachment(ctx, ownership.ids)
		if e.cleanup.err == nil {
			e.cleanup.complete = true
		}
	default:
		e.cleanup.err = ErrInvariant
	}
}

func (e *Epoch) watchCut(active *activeCut, lease generationLease) {
	select {
	case <-active.done:
		return
	case <-e.ctx.Done():
		return
	case <-active.timer.C():
	}
	e.mu.Lock()
	if e.terminal == nil && lease.validate(e.lease) == nil && e.active == active {
		e.requestTerminalLocked(faultTerminalCause(fmt.Errorf("%w: cut %d deadline exhausted", ErrReopenRequired, active.cut.ID)))
	}
	e.mu.Unlock()
}
