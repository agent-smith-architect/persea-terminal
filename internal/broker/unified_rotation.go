package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

var (
	ErrUnifiedRotateUnavailable     = errors.New("unified rotation target is unavailable")
	ErrUnifiedRotateInProgress      = errors.New("unified rotation is already in progress")
	ErrUnifiedRotateAlternateScreen = errors.New("unified rotation target is on the alternate screen")
	ErrUnifiedRotateMultiWindow     = errors.New("unified rotation target has more than one window")
	ErrUnifiedRotateMultiPane       = errors.New("unified rotation target has more than one pane")
	ErrUnifiedRotateSlotsExhausted  = errors.New("unified rotation capacity is exhausted")
	ErrUnifiedRotateUnstable        = errors.New("unified rotation capture did not stabilize")
	ErrUnifiedRotatePendingOverflow = errors.New("unified rotation pending output exceeded its bounded shape")
	ErrUnifiedRotateFatal           = errors.New("unified rotation failed after sealing the predecessor")
	ErrUnifiedRefitMalformed        = errors.New("unified width refit request is malformed")
	ErrUnifiedRefitStale            = errors.New("unified width refit target is stale")
	ErrUnifiedRefitAlternateScreen  = errors.New("unified width refit target is on the alternate screen")
	ErrUnifiedRefitFatal            = errors.New("unified width refit failed after width mutation")
)

const rotationReplayBatchBytes = 64 << 10

const unifiedRefitOperationLimit = 128

type unifiedRefitFailureStage = proto.RefitFailureStage

const (
	refitFailureCapture          = proto.RefitFailureCapture
	refitFailureBindSuccessor    = proto.RefitFailureBindSuccessor
	refitFailureMaterialize      = proto.RefitFailureMaterialize
	refitFailureBeginRegistry    = proto.RefitFailureBeginRegistry
	refitFailureQueueBootstrap   = proto.RefitFailureQueueBootstrap
	refitFailureSubmitBoundary   = proto.RefitFailureSubmitBoundary
	refitFailureAwaitBoundary    = proto.RefitFailureAwaitBoundary
	refitFailureValidateRegistry = proto.RefitFailureValidateRegistry
	refitFailureSubmitSeal       = proto.RefitFailureSubmitSeal
	refitFailureAwaitSeal        = proto.RefitFailureAwaitSeal
	refitFailureCommitRegistry   = proto.RefitFailureCommitRegistry
	refitFailureCommitJournal    = proto.RefitFailureCommitJournal
	refitFailureQueuePending     = proto.RefitFailureQueuePending
	refitFailureSubmitPending    = proto.RefitFailureSubmitPending
	refitFailureAwaitPending     = proto.RefitFailureAwaitPending
)

type unifiedRefitFailureClass = proto.RefitFailureClass

const (
	refitFailureClassInvalidated = proto.RefitFailureInvalidated
	refitFailureClassCapacity    = proto.RefitFailureCapacity
	refitFailureClassStorage     = proto.RefitFailureStorage
	refitFailureClassObserver    = proto.RefitFailureObserver
	refitFailureClassInternal    = proto.RefitFailureInternal
)

type unifiedRefitStageError struct {
	stage unifiedRefitFailureStage
	class unifiedRefitFailureClass
	cause error
}

func (failure *unifiedRefitStageError) Error() string {
	return fmt.Sprintf("unified width refit failed at %s (%s)", failure.stage, failure.class)
}

func (failure *unifiedRefitStageError) Unwrap() error { return failure.cause }

func classifyUnifiedRefitFailure(cause error) unifiedRefitFailureClass {
	switch {
	case errors.Is(cause, unifiedjournal.ErrInvalidated):
		return refitFailureClassInvalidated
	case errors.Is(cause, unifiedjournal.ErrQuota), errors.Is(cause, unifiedjournal.ErrPhysicalQuota):
		return refitFailureClassCapacity
	case errors.Is(cause, unifiedjournal.ErrStorage):
		return refitFailureClassStorage
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded), errors.Is(cause, io.EOF):
		return refitFailureClassObserver
	default:
		return refitFailureClassInternal
	}
}

func (rotation *unifiedDevRotation) refitStageError(stage unifiedRefitFailureStage, cause error) error {
	if cause == nil || !rotation.refit {
		return cause
	}
	var staged *unifiedRefitStageError
	if errors.As(cause, &staged) {
		return cause
	}
	return &unifiedRefitStageError{stage: stage, class: classifyUnifiedRefitFailure(cause), cause: cause}
}

func (rotation *unifiedDevRotation) refitStage(stage unifiedRefitFailureStage) error {
	if !rotation.refit {
		return nil
	}
	rotation.refitCurrentStage = stage
	if rotation.unit == nil || rotation.unit.owner.refitStageFault == nil {
		return nil
	}
	return rotation.refitStageError(stage, rotation.unit.owner.refitStageFault(rotation.session, stage))
}

func (rotation *unifiedDevRotation) refitCurrentStageError(cause error) error {
	if cause == nil || !rotation.refit || !rotation.ponr {
		return cause
	}
	return rotation.refitStageError(rotation.refitCurrentStage, cause)
}

func refitFailureMetadata(err error) (proto.RefitFailureStage, proto.RefitFailureClass, bool) {
	var stage proto.RefitFailureStage
	var class proto.RefitFailureClass
	found := false
	valid := true
	var walk func(error)
	walk = func(current error) {
		if current == nil || !valid {
			return
		}
		if failure, ok := current.(*unifiedRefitStageError); ok {
			if !proto.IsRefitFailureStage(failure.stage) || !proto.IsRefitFailureClass(failure.class) ||
				(found && (stage != failure.stage || class != failure.class)) {
				valid = false
				return
			}
			stage, class, found = failure.stage, failure.class, true
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range wrapped.Unwrap() {
				walk(child)
			}
		case interface{ Unwrap() error }:
			walk(wrapped.Unwrap())
		}
	}
	walk(err)
	if !found || !valid {
		return "", "", false
	}
	return stage, class, true
}

type unifiedRefitOperationKey struct {
	authority proto.Authority
	operation string
}

type unifiedRefitRequest struct {
	source  terminal.SourceWitness
	columns int
	// rows is the requested row count as given: 0 means "keep the
	// predecessor's rows". It is part of the idempotency tuple.
	rows              int
	controlGeneration uint64
}

type unifiedRefitResult struct {
	successorSource string
	// rows is the row count the successor was built with (the request's rows,
	// or the predecessor's when none were requested).
	rows int
	err  error
}

// validRefitGeometry is the refit request policy: columns inside the explicit
// width band, and rows either omitted (0) or inside the same closed row policy
// the live rows-only Fit obeys. Reject, never clamp.
func validRefitGeometry(columns, rows int) bool {
	if columns < 20 || columns > 300 {
		return false
	}
	return rows == 0 || terminal.ValidVerticalFit(columns, rows)
}

// effectiveRefitRows resolves an omitted row request to the predecessor's rows.
func effectiveRefitRows(source terminal.SourceWitness, rows int) int {
	if rows > 0 {
		return rows
	}
	return source.Rows
}

type unifiedRefitOperation struct {
	request unifiedRefitRequest
	done    chan struct{}
	result  unifiedRefitResult
}

// unifiedRotationPending is the post-capture, pre-swap byte tail. It stores
// bytes rather than Observations: the witness is fixed by the transaction and
// a slice of per-byte Observation structs would leave heap and journal framing
// unbounded. Replaying in 64 KiB pieces pins the rotation flow reservation's exported
// one-MiB/sixteen-record shape.
type unifiedRotationPending struct {
	data   []byte
	memory *recordingUnitMemory
	charge int64
}

func (pending *unifiedRotationPending) append(observation controlmode.Observation) error {
	if observation.Kind != controlmode.ObservationOutput || len(observation.Data) == 0 {
		return nil
	}
	want := len(pending.data) + len(observation.Data)
	records := (want + rotationReplayBatchBytes - 1) / rotationReplayBatchBytes
	if int64(want) > unifiedjournal.RotationPendingCapBytes || int64(records) > unifiedjournal.RotationPendingCapRecords {
		return ErrUnifiedRotatePendingOverflow
	}
	if cap(pending.data) == 0 {
		cost := unifiedjournal.RotationPendingCapBytes
		if !pending.memory.Reserve(cost) {
			return errRecordingTransients
		}
		pending.charge = cost
		pending.data = make([]byte, 0, int(cost))
	}
	pending.data = append(pending.data, observation.Data...)
	return nil
}

type unifiedDevRotation struct {
	requestContext    context.Context
	initial           *recordingInitialOperation
	settlement        *recordingSettlementStream
	session           string
	oldKey            unifiedjournal.PaneKey
	unit              *unifiedDevUnit
	registry          *paneRegistry
	capacity          *unifiedjournal.RotationCapacity
	oldOwner          uint64
	newKey            unifiedjournal.PaneKey
	newOwner          uint64
	newHeld           bool
	holder            *unifiedDevBirth
	old               controlmode.PaneWitness
	next              controlmode.PaneWitness
	journal           *unifiedjournal.RotationReservation
	registryT         *paneRotationTxn
	flipped           bool
	ponr              bool
	registryCommitted bool
	journalCommitted  bool
	settled           bool
	refit             bool
	refitColumns      int
	refitRows         int
	refitOperation    string
	refitAuthority    proto.Authority
	refitSource       terminal.SourceWitness
	refitCurrentStage unifiedRefitFailureStage
	fatalCause        error
}

func (rotation *unifiedDevRotation) requestError() error {
	if ctx := rotation.requestInterest(); ctx != nil {
		return ctx.Err()
	}
	return nil
}

// The receipt's publication is also the end of caller authority. Pending work
// after that point belongs to the recording lifecycle, even if the caller left.
func (rotation *unifiedDevRotation) requestInterest() context.Context {
	if op := rotation.initial; op != nil {
		op.runtime.mu.Lock()
		published := op.published
		op.runtime.mu.Unlock()
		if published {
			return nil
		}
	}
	return rotation.requestContext
}

func validRefitOperation(value string) bool {
	if len(value) != 43 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// refitSession is the explicit width-generation idempotency boundary. The
// immutable operation is recorded before current-source validation: an exact
// lost-success replay therefore observes the original settled result rather
// than being misclassified as stale after the successor became active.
func (effects *UnifiedDevPaneEffects) refitSession(ctx context.Context, authority proto.Authority, source terminal.SourceWitness, columns int, operation string) error {
	_, err := effects.refitSessionResult(ctx, authority, source, columns, operation)
	return err
}

// refitSessionGeometry is refitSession with an explicit row request; rows 0
// keeps the predecessor's rows.
func (effects *UnifiedDevPaneEffects) refitSessionGeometry(ctx context.Context, authority proto.Authority, source terminal.SourceWitness, columns, rows int, operation string) error {
	_, _, err := effects.refitSessionResultGeometry(ctx, authority, source, columns, rows, operation)
	return err
}

// settledRefitOperation is the broker-facing idempotency lookup. It is safe to
// call before tmux witness reconstruction: immutable authority plus operation
// token address the provider-lifetime record, and the recorded column request
// prevents a token from being reused for a different tuple.
func (effects *UnifiedDevPaneEffects) settledRefitOperation(authority proto.Authority, columns int, operation string) (unifiedRefitResult, bool) {
	return effects.settledRefitOperationGeometry(authority, columns, 0, operation)
}

// settledRefitOperationGeometry is settledRefitOperation for the full
// columns×rows tuple: a token replayed with different rows is a different
// request and is refused as malformed.
func (effects *UnifiedDevPaneEffects) settledRefitOperationGeometry(authority proto.Authority, columns, rows int, operation string) (unifiedRefitResult, bool) {
	if !validRefitGeometry(columns, rows) || !validRefitOperation(operation) {
		return unifiedRefitResult{err: ErrUnifiedRefitMalformed}, true
	}
	key := unifiedRefitOperationKey{authority: authority, operation: operation}
	effects.mu.Lock()
	recorded := effects.refitOperations[key]
	effects.mu.Unlock()
	if recorded == nil {
		return unifiedRefitResult{}, false
	}
	<-recorded.done
	if columns != recorded.request.columns || rows != recorded.request.rows {
		return unifiedRefitResult{err: ErrUnifiedRefitMalformed}, true
	}
	return recorded.result, true
}

// refitSessionResult places the idempotency lookup above current-source
// validation. A broker retry necessarily rebuilds the now-current witness, so
// a completed operation must be found by immutable authority+token before the
// changed geometry can be mistaken for a malformed new request.
func (effects *UnifiedDevPaneEffects) refitSessionResult(ctx context.Context, authority proto.Authority, source terminal.SourceWitness, columns int, operation string) (string, error) {
	successor, _, err := effects.refitSessionResultGeometry(ctx, authority, source, columns, 0, operation)
	return successor, err
}

// refitSessionResultGeometry is refitSessionResult with an explicit row
// request. It returns the successor source and the rows the successor was
// built with (the request's, or the predecessor's when rows is 0).
func (effects *UnifiedDevPaneEffects) refitSessionResultGeometry(ctx context.Context, authority proto.Authority, source terminal.SourceWitness, columns, rows int, operation string) (string, int, error) {
	if !validRefitGeometry(columns, rows) || !validRefitOperation(operation) {
		return "", 0, ErrUnifiedRefitMalformed
	}
	key := unifiedRefitOperationKey{authority: authority, operation: operation}
	effects.mu.Lock()
	if effects.refitOperations == nil {
		effects.refitOperations = make(map[unifiedRefitOperationKey]*unifiedRefitOperation)
	}
	if recorded := effects.refitOperations[key]; recorded != nil {
		effects.mu.Unlock()
		<-recorded.done
		result := recorded.result
		request := recorded.request
		exactOriginal := source == request.source
		exactSuccessor := result.err == nil && result.successorSource != "" &&
			source.Incarnation == result.successorSource && sameRefitOwner(request.source, source)
		if columns != request.columns || rows != request.rows || (!exactOriginal && !exactSuccessor) {
			return "", 0, ErrUnifiedRefitMalformed
		}
		return result.successorSource, result.rows, result.err
	}
	if len(effects.refitOperations) >= unifiedRefitOperationLimit {
		effects.mu.Unlock()
		return "", 0, ErrUnifiedRotateInProgress
	}
	// A refit is a width change by definition; rows-only changes belong to the
	// live rows-only Fit on the attachment protocol.
	if columns == source.Columns {
		effects.mu.Unlock()
		return "", 0, ErrUnifiedRefitMalformed
	}
	oldKey := effects.active[source.SessionID]
	request := unifiedRefitRequest{source: source, columns: columns, rows: rows, controlGeneration: oldKey.ControlGeneration}
	recorded := &unifiedRefitOperation{request: request, done: make(chan struct{})}
	effects.refitOperations[key] = recorded
	effects.mu.Unlock()
	if edge := effects.refitOperationEdge; edge != nil {
		edge(operation, "recorded")
	}

	appliedRows := effectiveRefitRows(source, rows)
	var successorSource string
	resultErr := effects.executeRefitSession(ctx, authority, source, columns, appliedRows, operation, request.controlGeneration, &successorSource)
	effects.mu.Lock()
	recorded.result = unifiedRefitResult{successorSource: successorSource, rows: appliedRows, err: resultErr}
	close(recorded.done)
	effects.mu.Unlock()
	return successorSource, appliedRows, resultErr
}

// executeRefitSession is the single execution side of the recorded operation.
// The caller has already revalidated the authority and source; this final
// admission binds the operation to the exact active generation and reserves
// both the geometry owner and complete rotation capacity before any tmux
// command can be queued.
func (effects *UnifiedDevPaneEffects) executeRefitSession(ctx context.Context, authority proto.Authority, source terminal.SourceWitness, columns, rows int, operation string, controlGeneration uint64, successorSource *string) error {
	registry, ok := effects.observer.(*paneRegistry)
	if !ok || registry.retention == nil || !effects.forServer(authority.Server) {
		return ErrUnifiedRotateUnavailable
	}
	// The public source incarnation includes geometry. A preceding explicit
	// rows-only Fit therefore changes source.Incarnation without replacing the
	// pane owner or the active journal generation. Snapshot the active key, read
	// its durable birth geometry without effects.mu, then revalidate the key and
	// owner below before installing the rotation. Re-deriving the incarnation
	// from the current socket/process identity and the generation's birth
	// geometry admits only that exact owner; a respawn or replacement still
	// fails closed.
	effects.mu.Lock()
	unit := effects.units[source.SessionID]
	oldKey, active := effects.active[source.SessionID]
	_, adopting := effects.adopting[source.SessionID]
	if effects.rotation != nil {
		effects.mu.Unlock()
		return ErrUnifiedRotateInProgress
	}
	var old controlmode.PaneWitness
	found := false
	if unit != nil {
		old, found = unit.rotationWitness(oldKey)
	}
	if unit == nil || !active || adopting || authority.SessionID != source.SessionID || oldKey.ControlGeneration != controlGeneration ||
		!found || old.Session.Session != source.SessionID || old.Window != source.WindowID || old.Pane != source.PaneID {
		effects.mu.Unlock()
		return ErrUnifiedRefitStale
	}
	effects.mu.Unlock()
	expectedIncarnation, incarnationErr := effects.refitGenerationIncarnation(oldKey, source)
	if incarnationErr != nil {
		return ErrUnifiedRefitStale
	}

	effects.mu.Lock()
	unit = effects.units[source.SessionID]
	currentKey, current := effects.active[source.SessionID]
	_, adopting = effects.adopting[source.SessionID]
	if effects.rotation != nil {
		effects.mu.Unlock()
		return ErrUnifiedRotateInProgress
	}
	if unit == nil || !current || currentKey != oldKey || adopting || authority.SessionID != source.SessionID || oldKey.ControlGeneration != controlGeneration {
		effects.mu.Unlock()
		return ErrUnifiedRefitStale
	}
	old, found = unit.rotationWitness(oldKey)
	if !found || old.Session.Session != source.SessionID || old.Window != source.WindowID || old.Pane != source.PaneID || old.Incarnation != expectedIncarnation {
		effects.mu.Unlock()
		return ErrUnifiedRefitStale
	}
	rotation := &unifiedDevRotation{
		session: source.SessionID, oldKey: oldKey, unit: unit, registry: registry,
		refit: true, refitColumns: columns, refitRows: rows, refitOperation: operation,
		refitAuthority: authority, refitSource: source,
	}
	effects.rotation = rotation
	effects.mu.Unlock()

	clear := func() {
		effects.mu.Lock()
		if effects.rotation == rotation {
			effects.rotation = nil
		}
		effects.mu.Unlock()
	}
	owner, err := registry.retention.acquireGeometryOwner(oldKey)
	if err != nil {
		clear()
		return errors.Join(ErrUnifiedRotateInProgress, err)
	}
	rotation.oldOwner = owner
	effects.journalMu.Lock()
	capacity, capacityErr := effects.realm.BeginRotationCapacity(oldKey)
	effects.journalMu.Unlock()
	if capacityErr != nil {
		registry.retention.releaseGeometryOwner(oldKey, owner)
		clear()
		if errors.Is(capacityErr, unifiedjournal.ErrQuota) || errors.Is(capacityErr, unifiedjournal.ErrPhysicalQuota) {
			return errors.Join(ErrUnifiedRotateSlotsExhausted, capacityErr)
		}
		return errors.Join(ErrUnifiedRotateUnavailable, capacityErr)
	}
	rotation.capacity = capacity
	result := make(chan unifiedDevCommandResult, 1)
	rotation.requestContext = ctx
	request := unifiedDevCommand{server: effects.server, rotation: rotation, done: result}
	select {
	case unit.commands <- request:
	case <-unit.done:
		effects.releaseRotationCapacity(capacity)
		registry.retention.releaseGeometryOwner(oldKey, owner)
		clear()
		return ErrUnifiedRotateUnavailable
	case <-ctx.Done():
		effects.releaseRotationCapacity(capacity)
		registry.retention.releaseGeometryOwner(oldKey, owner)
		clear()
		return ctx.Err()
	case <-time.After(5 * time.Second):
		effects.releaseRotationCapacity(capacity)
		registry.retention.releaseGeometryOwner(oldKey, owner)
		clear()
		return ErrUnifiedRotateUnavailable
	}
	select {
	case outcome := <-result:
		if outcome.err == nil && successorSource != nil {
			*successorSource = rotation.next.Incarnation
		}
		return outcome.err
	case <-unit.done:
		select {
		case outcome := <-result:
			if outcome.err == nil && successorSource != nil {
				*successorSource = rotation.next.Incarnation
			}
			return outcome.err
		default:
			return ErrUnifiedRotateUnavailable
		}
	}
}

// rotateSession is the test/future-trigger entry point. It makes every
// capacity decision before the unit writes the capture composite, and hands
// accepted work to the sole observer goroutine exactly as issueGuarded does.
func (effects *UnifiedDevPaneEffects) rotateSession(ctx context.Context, session string) error {
	registry, ok := effects.observer.(*paneRegistry)
	if !ok || registry.retention == nil {
		return ErrUnifiedRotateUnavailable
	}
	effects.mu.Lock()
	unit := effects.units[session]
	oldKey, active := effects.active[session]
	_, adopting := effects.adopting[session]
	if effects.rotation != nil {
		effects.mu.Unlock()
		return ErrUnifiedRotateInProgress
	}
	if unit == nil || !active || adopting {
		effects.mu.Unlock()
		return ErrUnifiedRotateUnavailable
	}
	rotation := &unifiedDevRotation{session: session, oldKey: oldKey, unit: unit, registry: registry}
	effects.rotation = rotation
	effects.mu.Unlock()

	clear := func() {
		effects.mu.Lock()
		if effects.rotation == rotation {
			effects.rotation = nil
		}
		effects.mu.Unlock()
	}
	owner, err := registry.retention.acquireGeometryOwner(oldKey)
	if err != nil {
		clear()
		return errors.Join(ErrUnifiedRotateInProgress, err)
	}
	rotation.oldOwner = owner
	effects.journalMu.Lock()
	capacity, capacityErr := effects.realm.BeginRotationCapacity(oldKey)
	effects.journalMu.Unlock()
	if capacityErr != nil {
		registry.retention.releaseGeometryOwner(oldKey, owner)
		clear()
		if errors.Is(capacityErr, unifiedjournal.ErrQuota) || errors.Is(capacityErr, unifiedjournal.ErrPhysicalQuota) {
			return errors.Join(ErrUnifiedRotateSlotsExhausted, capacityErr)
		}
		return errors.Join(ErrUnifiedRotateUnavailable, capacityErr)
	}
	rotation.capacity = capacity
	if edge := effects.rotationEdge; edge != nil {
		edge(session, "capacity_reserved")
	}
	result := make(chan unifiedDevCommandResult, 1)
	rotation.requestContext = ctx
	request := unifiedDevCommand{server: effects.server, rotation: rotation, done: result}
	select {
	case unit.commands <- request:
		// The unit now owns rollback, reservation settlement and owner release.
	case <-unit.done:
		effects.releaseRotationCapacity(capacity)
		registry.retention.releaseGeometryOwner(oldKey, owner)
		clear()
		return ErrUnifiedRotateUnavailable
	case <-ctx.Done():
		effects.releaseRotationCapacity(capacity)
		registry.retention.releaseGeometryOwner(oldKey, owner)
		clear()
		return ctx.Err()
	case <-time.After(5 * time.Second):
		effects.releaseRotationCapacity(capacity)
		registry.retention.releaseGeometryOwner(oldKey, owner)
		clear()
		return ErrUnifiedRotateUnavailable
	}
	select {
	case outcome := <-result:
		return outcome.err
	case <-unit.done:
		select {
		case outcome := <-result:
			return outcome.err
		default:
			return ErrUnifiedRotateUnavailable
		}
	case <-ctx.Done():
		// The unit still owns settlement. Cancellation before publication must
		// restore before the seal, or settle fatally after that point of no return.
		return ctx.Err()
	}
}

func (unit *unifiedDevUnit) runRotation(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error, rotation *unifiedDevRotation) (err error) {
	// The returned bootstrap can overlap its queued runtime copy until this
	// rotation settles, after the capture command's response owner has left.
	effects := unit.owner
	rotation.settlement = &recordingSettlementStream{unit: unit, decoder: decoder, read: read, readErr: readErr}
	defer func() {
		var retained *rotationOverflow
		if errors.As(err, &retained) {
			defer func() { retained.events = nil; retained.batch.Release() }()
		}
		// A refit sets a closed stage before the conservative width PONR and
		// advances it before every later fallible interval. Wrap here as the final
		// product-side fence so a newly-added return cannot bypass fatal
		// settlement's safe attribution.
		if rotation.refit && rotation.ponr && !rotation.settled {
			if err == nil {
				err = ErrUnifiedRefitFatal
			}
			err = rotation.refitCurrentStageError(err)
		}
		if !rotation.settled {
			var overflow *rotationOverflow
			_ = errors.As(err, &overflow)
			var extra []controlmode.Event
			if overflow != nil {
				extra = overflow.events
			}
			if restoreErr := rotation.restore(extra, err); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
		}
		if rotation.newHeld {
			rotation.registry.retention.releaseGeometryOwner(rotation.newKey, rotation.newOwner)
			rotation.newHeld = false
		}
		rotation.registry.retention.releaseGeometryOwner(rotation.oldKey, rotation.oldOwner)
		effects.releaseRotationCapacity(rotation.capacity)
		effects.mu.Lock()
		if effects.rotation == rotation {
			effects.rotation = nil
		}
		effects.mu.Unlock()
	}()

	if !unit.memory.Reserve(int64(adoptionBootstrapCapBytes)) {
		return errRecordingTransients
	}
	defer unit.memory.Release(int64(adoptionBootstrapCapBytes))
	if rotation.unit != unit || rotation.session != unit.sessionID {
		return ErrUnifiedRotateUnavailable
	}
	if err := rotation.requestError(); err != nil {
		return err
	}
	source, sourceErr := buildSourceWitness(ctx, effects.server, "", rotation.session)
	if sourceErr != nil {
		return errors.Join(ErrUnifiedRotateUnavailable, sourceErr)
	}
	if rotation.refit && source != rotation.refitSource {
		return ErrUnifiedRefitStale
	}
	expectedIncarnation, incarnationErr := effects.refitGenerationIncarnation(rotation.oldKey, source)
	if incarnationErr != nil {
		return ErrUnifiedRotateUnavailable
	}
	old, found := unit.rotationWitness(rotation.oldKey)
	if !found || source.WindowID != old.Window || source.PaneID != old.Pane || expectedIncarnation != old.Incarnation {
		return ErrUnifiedRotateUnavailable
	}
	rotation.old = old
	rotation.holder = unit.holder
	if rotation.holder == nil {
		return ErrUnifiedRotateUnavailable
	}
	if rotation.refit {
		alternate, probeErr := unit.queryRefitAlternate(ctx, decoder, read, readErr, old.Pane)
		if probeErr != nil {
			return probeErr
		}
		if alternate {
			return ErrUnifiedRefitAlternateScreen
		}
	}
	next, nextErr := unit.unusedRotationWitness(old)
	if nextErr != nil {
		return nextErr
	}
	rotation.next = next
	rotation.newKey = journalKey(next)
	if err := effects.reserveJournalSource(rotation.newKey, source); err != nil {
		return err
	}
	sourceReservationKey := rotation.newKey
	defer func() { effects.realm.CancelSourceReservation(sourceReservationKey) }()

	var bootstrap []byte
	var geometry unifiedjournal.Geometry
	attempts := adoptionCaptureAttempts
	if rotation.refit {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			effects.adoptionRetries.Add(1)
			select {
			case <-time.After(time.Duration(attempt) * 25 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		bootstrap, geometry, err = unit.submitRotationComposite(ctx, decoder, read, readErr, rotation, attempt)
		if errors.Is(err, errUnifiedAdoptDrift) {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	if !rotation.flipped {
		return ErrUnifiedRotateUnstable
	}
	if rotation.refit {
		if err = rotation.refitStage(refitFailureBindSuccessor); err != nil {
			return err
		}
		// The tmux incarnation includes geometry. Rebuild it after the guarded
		// width composite and bind the successor generation to that exact public
		// identity before any journal or registry materialization. Control
		// generation was pre-reserved; this is identity completion, not a second
		// successor attempt.
		landed, witnessErr := buildSourceWitness(ctx, effects.server, rotation.refitAuthority.BootID, rotation.session)
		if witnessErr != nil || !sameRefitOwner(rotation.refitSource, landed) || landed.Columns != rotation.refitColumns || landed.Rows != rotation.refitRows {
			return errors.Join(ErrUnifiedRefitFatal, witnessErr)
		}
		provisional := rotation.next
		rotation.next.Window = landed.WindowID
		rotation.next.Pane = landed.PaneID
		rotation.next.Incarnation = landed.Incarnation
		rotation.holder.mu.Lock()
		if rotation.holder.aborted || rotation.holder.committed || rotation.holder.rotation == nil || rotation.holder.witness != provisional {
			rotation.holder.mu.Unlock()
			return ErrUnifiedRefitFatal
		}
		rotation.holder.witness = rotation.next
		rotation.holder.mu.Unlock()
		rotation.newKey = journalKey(rotation.next)
		if err := effects.realm.TransferSourceReservation(sourceReservationKey, rotation.newKey); err != nil {
			return err
		}
		sourceReservationKey = rotation.newKey
		effects.journalMu.Lock()
		_, originErr := effects.realm.Origin(rotation.newKey)
		effects.journalMu.Unlock()
		if !errors.Is(originErr, unifiedjournal.ErrInvalidated) {
			return errors.Join(ErrUnifiedRefitFatal, ErrUnifiedRotateUnstable)
		}
	}

	if err = rotation.refitStage(refitFailureMaterialize); err != nil {
		return err
	}
	rotation.newOwner, err = rotation.registry.retention.acquireGeometryOwner(rotation.newKey)
	if err != nil {
		return errors.Join(ErrUnifiedRotateInProgress, err)
	}
	rotation.newHeld = true
	if edge := effects.rotationEdge; edge != nil {
		edge(rotation.session, "successor_owner_held")
	}
	// BeginRotatedPane is deliberately attempted exactly once. Candidate keys
	// were proven absent before the composite; any materialization failure owns
	// R1 and goes directly through ArmRollback/restore, never a retry.
	effects.journalMu.Lock()
	rotation.journal, err = effects.realm.BeginRotatedPane(rotation.newKey, geometry, rotation.capacity)
	effects.journalMu.Unlock()
	if err != nil {
		return err
	}
	if edge := effects.rotationEdge; edge != nil {
		edge(rotation.session, "successor_materialized")
	}
	if rotation.refit {
		if err = rotation.refitStage(refitFailureBeginRegistry); err != nil {
			return err
		}
		rotation.registryT, err = rotation.registry.BeginPaneRefit(old, rotation.next)
	} else {
		rotation.registryT, err = rotation.registry.BeginPaneRotation(old, rotation.next)
	}
	if err != nil {
		return rotation.refitStageError(refitFailureBeginRegistry, err)
	}
	rotation.registryT.settlement = rotation.settlement
	if err = rotation.refitStage(refitFailureQueueBootstrap); err != nil {
		return err
	}
	rotation.registryT.initialSource = source
	if err = rotation.registryT.WriteBootstrap(bootstrap); err != nil {
		return rotation.refitStageError(refitFailureQueueBootstrap, err)
	}
	rotation.registryT.initial.bindContext(rotation.requestContext)
	rotation.registryT.initial.bindOwner(unit)
	rotation.initial = rotation.registryT.initial
	if err = rotation.refitStage(refitFailureSubmitBoundary); err != nil {
		return err
	}
	bootstrapDone, boundaryErr := rotation.registry.retention.startBoundary(rotation.newKey, "rotation_bootstrap", false)
	if boundaryErr != nil {
		return rotation.refitStageError(refitFailureSubmitBoundary, rotation.waitBoundarySettlement(nil, boundaryErr))
	}
	if edge := effects.rotationEdge; edge != nil {
		edge(rotation.session, "before_bootstrap_wait")
	}
	if err = rotation.refitStage(refitFailureAwaitBoundary); err != nil {
		return err
	}
	if err = unit.awaitRotationBoundary(ctx, decoder, read, readErr, bootstrapDone, rotation); err != nil {
		return rotation.refitStageError(refitFailureAwaitBoundary, err)
	}
	if edge := effects.rotationEdge; edge != nil {
		edge(rotation.session, "bootstrap_durable")
	}
	currentSource, sourceErr := buildSourceWitness(ctx, effects.server, "", rotation.session)
	if sourceErr != nil || !sameRefitOwner(source, currentSource) || currentSource.Columns != geometry.Columns || currentSource.Rows != geometry.Rows {
		return errors.Join(ErrUnifiedRotateUnavailable, sourceErr)
	}
	if err = rotation.refitStage(refitFailureValidateRegistry); err != nil {
		return err
	}
	if err = rotation.registryT.Validate(); err != nil {
		return rotation.refitStageError(refitFailureValidateRegistry, err)
	}
	if edge := effects.rotationEdge; edge != nil {
		edge(rotation.session, "registry_validated")
	}
	return unit.commitRotation(ctx, decoder, read, readErr, rotation)
}

// refitGenerationIncarnation binds a current physical source to an existing
// journal generation without treating an explicit rows-only geometry change
// as source replacement. The durable initial geometry is part of the old
// generation's identity; every non-geometry owner fact comes from a fresh tmux
// and /proc witness.
func (effects *UnifiedDevPaneEffects) refitGenerationIncarnation(key unifiedjournal.PaneKey, source terminal.SourceWitness) (string, error) {
	effects.journalMu.Lock()
	initial, err := effects.realm.InitialGeometry(key)
	effects.journalMu.Unlock()
	if err != nil {
		return "", err
	}
	return startupSourceIncarnation(source.Socket, source.Server, source.SessionID, source.WindowID, source.PaneID, source.Pane, initial.Columns, initial.Rows), nil
}

func (unit *unifiedDevUnit) commitRotation(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error, rotation *unifiedDevRotation) (err error) {
	effects := unit.owner
	if err = rotation.refitStage(refitFailureSubmitSeal); err != nil {
		return err
	}
	if edge := effects.rotationEdge; edge != nil {
		edge(rotation.session, "before_seal_start")
	}
	if err := rotation.requestError(); err != nil {
		return err
	}
	sealDone, sealErr := rotation.registry.retention.startBoundary(rotation.oldKey, "rotation_seal", true)
	if sealErr != nil {
		return errors.Join(ErrUnifiedRotateFatal, sealErr)
	}
	rotation.ponr = true
	if edge := effects.rotationEdge; edge != nil {
		edge(rotation.session, "before_seal_wait")
	}
	if err = rotation.refitStage(refitFailureAwaitSeal); err != nil {
		return err
	}
	if err = unit.awaitRotationBoundary(ctx, decoder, read, readErr, sealDone, rotation); err != nil {
		return rotation.refitStageError(refitFailureAwaitSeal, errors.Join(ErrUnifiedRotateFatal, err))
	}
	if edge := effects.rotationEdge; edge != nil {
		edge(rotation.session, "predecessor_sealed")
	}
	rotation.registryT.markSealed()
	if edge := effects.rotationEdge; edge != nil {
		edge(rotation.session, "before_registry_commit")
	}
	if err = rotation.refitStage(refitFailureCommitRegistry); err != nil {
		return err
	}
	if err := rotation.requestError(); err != nil {
		return errors.Join(ErrUnifiedRotateFatal, err)
	}
	disposition := rotation.registryT.Commit()
	if disposition != paneRotationCommitted {
		// A fatal disposition has already reaped provider authority; an
		// impossible already-aborted disposition is equally non-authoritative.
		// Neither is rollback permission after the predecessor seal: do not swap
		// active, close subscribers, or settle the journal reservation.
		return rotation.refitStageError(refitFailureCommitRegistry, ErrUnifiedRotateFatal)
	}
	rotation.registryCommitted = true
	if edge := effects.rotationEdge; edge != nil {
		edge(rotation.session, "registry_committed")
	}
	// Registry Commit owns the active-key publication and predecessor-tail
	// close as one subscriberMu -> effects.mu -> registry.mu -> retention.mu
	// interval. Reaching this edge therefore means both 7b and 7c are complete.
	if edge := effects.rotationEdge; edge != nil {
		edge(rotation.session, "active_swapped")
	}

	if err = rotation.refitStage(refitFailureCommitJournal); err != nil {
		return err
	}
	effects.journalMu.Lock()
	rotation.journal.Commit()
	effects.journalMu.Unlock()
	rotation.journalCommitted = true
	if edge := effects.rotationEdge; edge != nil {
		edge(rotation.session, "journal_committed")
	}
	rotation.registryT = nil
	unit.generation = rotation.next.Session.ControlGeneration
	if err = rotation.refitStage(refitFailureQueuePending); err != nil {
		return err
	}
	if err = rotation.commitPending(rotation.next); err != nil {
		return rotation.refitStageError(refitFailureQueuePending, errors.Join(ErrUnifiedRotateFatal, err))
	}
	if err = rotation.refitStage(refitFailureSubmitPending); err != nil {
		return err
	}
	pendingDone, pendingErr := rotation.registry.retention.startBoundary(rotation.newKey, "rotation_pending", false)
	if pendingErr != nil {
		return rotation.refitStageError(refitFailureSubmitPending, errors.Join(ErrUnifiedRotateFatal, pendingErr))
	}
	if edge := effects.rotationEdge; edge != nil {
		edge(rotation.session, "before_pending_wait")
	}
	if err = rotation.refitStage(refitFailureAwaitPending); err != nil {
		return err
	}
	if err = unit.awaitRotationBoundary(ctx, decoder, read, readErr, pendingDone, rotation); err != nil {
		return rotation.refitStageError(refitFailureAwaitPending, errors.Join(ErrUnifiedRotateFatal, err))
	}
	if edge := effects.rotationEdge; edge != nil {
		edge(rotation.session, "pending_durable")
	}
	rotation.journal = nil
	rotation.settled = true
	if rotation.refit {
		effects.recordExplicitRefitSuccess(rotation.session, rotation.newKey)
	}
	return nil
}

func (unit *unifiedDevUnit) rotationWitness(key unifiedjournal.PaneKey) (controlmode.PaneWitness, bool) {
	for _, witness := range unit.witnesses {
		if journalKey(witness) == key {
			return witness, true
		}
	}
	return controlmode.PaneWitness{}, false
}

// unusedRotationWitness preflights collisions before materialization. This is
// not a capacity decision and is followed by exactly one BeginRotatedPane.
func (unit *unifiedDevUnit) unusedRotationWitness(old controlmode.PaneWitness) (controlmode.PaneWitness, error) {
	for tries := 0; tries < 16; tries++ {
		next := old
		next.Session.ControlGeneration = unit.owner.mintControlGeneration(true)
		key := journalKey(next)
		unit.owner.journalMu.Lock()
		_, err := unit.owner.realm.Origin(key)
		unit.owner.journalMu.Unlock()
		if errors.Is(err, unifiedjournal.ErrInvalidated) {
			return next, nil
		}
	}
	return controlmode.PaneWitness{}, ErrUnifiedRotateUnstable
}

// queryRefitAlternate is a pre-PONR policy interrogation on the unit's own
// ordered observer stream. Output arriving around it is routed normally; only
// the exact command response is consumed here. A refused alternate-screen
// refit therefore mutates no tmux geometry, cadence, subscriber or generation.
func (unit *unifiedDevUnit) queryRefitAlternate(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error, pane string) (bool, error) {
	var batch controlmode.EventBatch
	defer batch.Release()
	line := "display-message -p -t " + shellQuote(pane) + " " + shellQuote("#{alternate_on}") + "\n"
	if _, err := io.WriteString(unit.ptmx, line); err != nil {
		return false, err
	}
	response := recordingResponse{owner: unit.memory}
	defer response.release()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case err := <-readErr:
			return false, err
		case chunk := <-read:
			events, err := decodeRotationEvents(decoder, chunk, &batch)
			if err != nil {
				return false, err
			}
			for index, event := range events {
				switch event.Kind {
				case controlmode.EventCommandResponse:
					if err := response.Write(event.Data); err != nil {
						return false, err
					}
				case controlmode.EventCommandError:
					return false, ErrUnifiedRotateUnavailable
				case controlmode.EventCommandEnd:
					for _, rest := range events[index+1:] {
						if err := unit.owner.consumeObserverEvent(rest); err != nil {
							return false, err
						}
					}
					value := strings.TrimSpace(response.String())
					if value != "0" && value != "1" {
						return false, ErrUnifiedRotateUnavailable
					}
					return value == "1", nil
				case controlmode.EventCommandBegin:
				default:
					if err := unit.owner.consumeObserverEvent(event); err != nil {
						return false, err
					}
				}
			}
		}
	}
}

func tmuxControlCommand(args []string) string {
	quoted := make([]string, len(args))
	for index, arg := range args {
		quoted[index] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

// refitCompositeLine resizes the guarded pane to columns×rows in the same
// control-mode line as the adoption composite; rows is the resolved target
// (the request's, or the predecessor's when none were requested).
func refitCompositeLine(authority proto.Authority, source terminal.SourceWitness, columns, rows int) string {
	return tmuxControlCommand(guardedResizeArgs(authority, source, columns, rows)) + " ; " + strings.TrimSuffix(adoptionCompositeLine(source.PaneID), "\n") + "\n"
}

func (unit *unifiedDevUnit) submitRotationComposite(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error, rotation *unifiedDevRotation, attempt int) ([]byte, unifiedjournal.Geometry, error) {
	var batch controlmode.EventBatch
	defer batch.Release()
	if edge := unit.owner.rotationEdge; edge != nil {
		edge(rotation.session, "before_composite_write")
	}
	line := adoptionCompositeLine(rotation.old.Pane)
	blocks := adoptionCompositeBlocks
	if rotation.refit {
		line = refitCompositeLine(rotation.refitAuthority, rotation.refitSource, rotation.refitColumns, rotation.refitRows)
		blocks += 2 // guarded if-shell response plus its queued geometry-mutation response
		rotation.ponr = true
		if err := rotation.refitStage(refitFailureCapture); err != nil {
			return nil, unifiedjournal.Geometry{}, err
		}
	}
	if _, err := io.WriteString(unit.ptmx, line); err != nil {
		return nil, unifiedjournal.Geometry{}, err
	}
	remaining := blocks
	responses := make([]string, 0, blocks)
	response := recordingResponse{owner: unit.memory}
	defer response.release()
	began := false
	for {
		select {
		case <-ctx.Done():
			return nil, unifiedjournal.Geometry{}, ctx.Err()
		case err := <-readErr:
			return nil, unifiedjournal.Geometry{}, err
		case chunk := <-read:
			events, err := decodeRotationEvents(decoder, chunk, &batch)
			if err != nil {
				return nil, unifiedjournal.Geometry{}, fmt.Errorf("decode unified rotation composite: %w", err)
			}
			for index, event := range events {
				switch event.Kind {
				case controlmode.EventCommandResponse:
					if err := response.Write(event.Data); err != nil {
						return nil, unifiedjournal.Geometry{}, err
					}
				case controlmode.EventCommandError:
					for _, rest := range events[index+1:] {
						if consumeErr := unit.owner.consumeObserverEvent(rest); consumeErr != nil {
							return nil, unifiedjournal.Geometry{}, consumeErr
						}
					}
					return nil, unifiedjournal.Geometry{}, ErrUnifiedRotateUnavailable
				case controlmode.EventCommandEnd:
					responses = append(responses, response.String())
					response.Reset()
					remaining--
					if remaining != 0 {
						continue
					}
					bootstrap, geometry, judgeErr := unit.commitRotationCapture(responses, rotation, attempt)
					if judgeErr != nil {
						for _, rest := range events[index+1:] {
							if consumeErr := unit.owner.consumeObserverEvent(rest); consumeErr != nil {
								return nil, unifiedjournal.Geometry{}, consumeErr
							}
						}
						return nil, unifiedjournal.Geometry{}, judgeErr
					}
					for restIndex := index + 1; restIndex < len(events); restIndex++ {
						if consumeErr := unit.owner.consumeObserverEvent(events[restIndex]); consumeErr != nil {
							if errors.Is(consumeErr, ErrUnifiedRotatePendingOverflow) {
								return nil, unifiedjournal.Geometry{}, &rotationOverflow{events: events[restIndex:], batch: batch.Take()}
							}
							return nil, unifiedjournal.Geometry{}, consumeErr
						}
					}
					return bootstrap, geometry, nil
				case controlmode.EventCommandBegin:
					began = true
				default:
					if began && (event.Kind == controlmode.EventOutput || event.Kind == controlmode.EventExtendedOutput) {
						unit.owner.adoptionSpanOutputs.Add(1)
					}
					if consumeErr := unit.owner.consumeObserverEvent(event); consumeErr != nil {
						return nil, unifiedjournal.Geometry{}, consumeErr
					}
				}
			}
		}
	}
}

func (unit *unifiedDevUnit) commitRotationCapture(responses []string, rotation *unifiedDevRotation, attempt int) ([]byte, unifiedjournal.Geometry, error) {
	offset := 0
	if rotation.refit {
		offset = 2
		if len(responses) != adoptionCompositeBlocks+offset || strings.TrimSpace(responses[0]) != "" || strings.TrimSpace(responses[1]) != "" {
			return nil, unifiedjournal.Geometry{}, errors.New("unified width refit composite returned an invalid guarded command shape")
		}
	} else if len(responses) != adoptionCompositeBlocks {
		return nil, unifiedjournal.Geometry{}, errors.New("unified rotation composite returned an invalid block count")
	}
	responses = responses[offset:]
	if err := rejectObserverClientInventory(responses[0], unit.process.Process.Pid); err != nil {
		return nil, unifiedjournal.Geometry{}, err
	}
	if strings.TrimSpace(responses[1]) != "" {
		return nil, unifiedjournal.Geometry{}, errors.New("unified rotation pane normalization returned unexpected output")
	}
	pre := strings.TrimSpace(responses[2])
	post := strings.TrimSpace(responses[6])
	if unit.owner.adoptionPostTamper != nil {
		post = unit.owner.adoptionPostTamper(attempt, post)
	}
	probe, err := parseAdoptionProbe(pre)
	if err != nil {
		return nil, unifiedjournal.Geometry{}, err
	}
	switch {
	case probe.alternate != 0:
		return nil, unifiedjournal.Geometry{}, ErrUnifiedRotateAlternateScreen
	case probe.windows != 1:
		return nil, unifiedjournal.Geometry{}, ErrUnifiedRotateMultiWindow
	case probe.panes != 1:
		return nil, unifiedjournal.Geometry{}, ErrUnifiedRotateMultiPane
	}
	if pre != post {
		return nil, unifiedjournal.Geometry{}, errUnifiedAdoptDrift
	}
	modes, err := parseAdoptionModes(strings.TrimSpace(responses[5]))
	if err != nil {
		return nil, unifiedjournal.Geometry{}, err
	}
	if rotation.refit && (modes.columns != rotation.refitColumns || modes.rows != rotation.refitRows) {
		return nil, unifiedjournal.Geometry{}, ErrUnifiedRefitFatal
	}
	rows := parseAdoptionCapture(responses[3])
	pending := strings.TrimSuffix(responses[4], "\n")
	bootstrap, _, err := synthesizeAdoptionBootstrap(rows, pending, probe.cursorX, probe.cursorY, modes)
	if err != nil {
		return nil, unifiedjournal.Geometry{}, err
	}
	holder := rotation.holder
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if holder.aborted || !holder.committed || holder.witness != rotation.old || holder.rotation != nil {
		return nil, unifiedjournal.Geometry{}, ErrUnifiedRotateUnavailable
	}
	holder.witness = rotation.next
	holder.committed = false
	holder.rotation = &unifiedRotationPending{memory: unit.memory}
	rotation.flipped = true
	if edge := unit.owner.rotationEdge; edge != nil {
		edge(rotation.session, "holder_flipped")
	}
	return bootstrap, unifiedjournal.Geometry{Columns: modes.columns, Rows: modes.rows}, nil
}

type rotationOverflow struct {
	events []controlmode.Event
	batch  controlmode.EventBatch
}

func (*rotationOverflow) Error() string { return ErrUnifiedRotatePendingOverflow.Error() }
func (*rotationOverflow) Unwrap() error { return ErrUnifiedRotatePendingOverflow }

func (unit *unifiedDevUnit) awaitRotationBoundary(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error, done <-chan error, rotation *unifiedDevRotation) error {
	var batch controlmode.EventBatch
	defer batch.Release()
	if rotation.settlement == nil {
		rotation.settlement = &recordingSettlementStream{unit: unit, decoder: decoder, read: read, readErr: readErr}
	}
	if fault := unit.owner.rotationFault; fault != nil {
		if err := fault(rotation.session, "boundary_wait"); err != nil {
			return rotation.waitBoundarySettlement(done, err)
		}
	}
	var callerDone <-chan struct{}
	if interest := rotation.requestInterest(); interest != nil {
		callerDone = interest.Done()
	}
	for {
		select {
		case err := <-done:
			if requestErr := rotation.requestError(); requestErr != nil {
				return rotation.cancelAndSettleBoundary(nil, errors.Join(err, requestErr))
			}
			if err == nil {
				return nil
			}
			return rotation.waitBoundarySettlement(nil, err)
		case <-ctx.Done():
			return rotation.cancelAndSettleBoundary(done, ctx.Err())
		case <-callerDone:
			if err := rotation.requestError(); err != nil {
				return rotation.cancelAndSettleBoundary(done, err)
			}
			callerDone = nil
		case err := <-readErr:
			rotation.settlement.readErr = nil
			if err == nil {
				err = io.EOF
			}
			return rotation.waitBoundarySettlement(done, err)
		case chunk := <-read:
			events, err := decodeRotationEvents(decoder, chunk, &batch)
			if err != nil {
				rotation.settlement.read = nil
				return rotation.waitBoundarySettlement(done, err)
			}
			for index, event := range events {
				if err := unit.owner.consumeObserverEvent(event); err != nil {
					if errors.Is(err, ErrUnifiedRotatePendingOverflow) {
						return rotation.waitBoundarySettlement(done, &rotationOverflow{events: events[index:], batch: batch.Take()})
					}
					return rotation.waitBoundarySettlement(done, err)
				}
			}
		}
	}
}

func (rotation *unifiedDevRotation) cancelAndSettleBoundary(done <-chan error, cause error) error {
	if rotation.initial != nil {
		rotation.initial.cancel()
	}
	return rotation.waitBoundarySettlement(done, cause)
}

func (rotation *unifiedDevRotation) waitBoundarySettlement(done <-chan error, cause error) error {
	if done != nil {
		cause = errors.Join(cause, rotation.waitDependency(nil, done))
	}
	if rotation.initial != nil {
		// Initial recording may fail and revoke the successor before its
		// boundary is submitted. Keep that typed cause after its work settles.
		cause = errors.Join(cause, rotation.waitDependency(rotation.initial.done, nil))
		_, initialErr := rotation.initial.result()
		cause = errors.Join(cause, initialErr)
	}
	if rotation.registry != nil && rotation.registry.retention != nil {
		dispatched, err := rotation.registry.retention.startDispatchFence()
		if err != nil {
			dispatched = rotation.registry.retention.dispatcherDone
		}
		rotation.waitDependency(dispatched, nil)
		cause = errors.Join(cause, err)
	}
	if rotation.settlement != nil && rotation.settlement.err != nil {
		// Waits report outcomes; the rotation driver owns rollback or fatal
		// settlement. Preserve the original typed cause at this boundary.
		cause = errors.Join(cause, rotation.settlement.err)
	}
	return cause
}

func (rotation *unifiedDevRotation) waitDependency(done <-chan struct{}, result <-chan error) error {
	if rotation.settlement != nil {
		return rotation.settlement.wait(done, result)
	}
	return waitRecording(nil, done, result)
}

// External callers use the same boundary/fence ordering without owning a
// decoder. Observer drivers call the method with their stream owner installed.
func waitRotationBoundarySettlement(registry *paneRegistry, done <-chan error, cause error) error {
	rotation := &unifiedDevRotation{registry: registry}
	return rotation.waitBoundarySettlement(done, cause)
}

// decodeRotationEvents rejects the complete decoded batch before routing any
// member. Our observer never requests tmux flow control, so a pause, continue,
// or extended-output record invalidates every ordinary output that arrived in
// the same read. This all-or-nothing pre-scan is shared by both the composite
// and boundary phases.
func decodeRotationEvents(decoder *controlmode.Decoder, chunk []byte, batches ...*controlmode.EventBatch) ([]controlmode.Event, error) {
	var events []controlmode.Event
	var err error
	if len(batches) == 0 {
		events, err = decoder.Feed(chunk) // unbudgeted protocol fixtures
	} else {
		events, err = batches[0].Feed(decoder, chunk)
	}
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if err := observerFlowControlFault(event); err != nil {
			return nil, err
		}
	}
	return events, nil
}

func (rotation *unifiedDevRotation) restore(extra []controlmode.Event, failures ...error) error {
	if rotation.settled {
		return nil
	}
	failure := error(ErrUnifiedRotateFatal)
	if len(failures) != 0 && failures[0] != nil {
		failure = failures[0]
	}
	if rotation.ponr {
		return rotation.settleFatal(failure)
	}
	if rotation.settlement != nil && rotation.settlement.err != nil {
		return rotation.settleFatal(errors.Join(failure, rotation.settlement.err))
	}
	if !rotation.abortRegistry() {
		// A refused Abort proves the registry transaction crossed its seal even
		// if a future caller lost the local PONR bit. Treat it as authoritative
		// fatal settlement; never continue into journal Abort or rollback replay.
		rotation.ponr = true
		return rotation.settleFatal(failure)
	}
	if rotation.settlement != nil && rotation.settlement.err != nil {
		return rotation.settleFatal(errors.Join(failure, rotation.settlement.err))
	}
	if !rotation.flipped {
		rotation.settled = true
		return nil
	}
	if rotation.journal != nil {
		rotation.ownerJournalAbort()
	} else {
		rotation.unit.owner.journalMu.Lock()
		err := rotation.capacity.ArmRollback()
		rotation.unit.owner.journalMu.Unlock()
		if err != nil {
			rotation.settled = true
			return errors.Join(ErrUnifiedRotateFatal, err)
		}
	}
	holder := rotation.holder
	holder.mu.Lock()
	var pending []byte
	if holder.rotation != nil {
		defer holder.rotation.release()
		pending = holder.rotation.data
	}
	holder.rotation = nil
	holder.witness = rotation.old
	holder.committed = true
	holder.mu.Unlock()
	if err := rotation.writePending(rotation.old, pending); err != nil {
		rotation.settled = true
		return errors.Join(ErrUnifiedRotateFatal, err)
	}
	for _, event := range extra {
		if event.Kind == controlmode.EventOutput && len(event.Data) != 0 {
			if err := rotation.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: rotation.old, Data: event.Data}); err != nil {
				rotation.settled = true
				return errors.Join(ErrUnifiedRotateFatal, err)
			}
		}
	}
	if edge := rotation.unit.owner.rotationEdge; edge != nil {
		edge(rotation.session, "before_restore_wait")
	}
	done, err := rotation.registry.retention.startBoundary(rotation.oldKey, "rotation_restore", false)
	if err == nil {
		err = rotation.waitDependency(nil, done)
	}
	if rotation.settlement != nil && rotation.settlement.err != nil {
		return rotation.settleFatal(errors.Join(failure, err, rotation.settlement.err))
	}
	if err != nil {
		rotation.settled = true
		return errors.Join(ErrUnifiedRotateFatal, err)
	}
	rotation.unit.owner.releaseRotationCapacity(rotation.capacity)
	rotation.settled = true
	return nil
}

// releaseRotationCapacity shares journalMu with pressure snapshots. The Realm
// APIs are caller-synchronized; without this fence an automatic scheduler can
// read the physical ledger while rollback settlement mutates it.
func (effects *UnifiedDevPaneEffects) releaseRotationCapacity(capacity *unifiedjournal.RotationCapacity) {
	if capacity == nil {
		return
	}
	effects.journalMu.Lock()
	capacity.Release()
	effects.journalMu.Unlock()
}

// settleFatal owns every post-PONR terminal edge. Registry and journal
// transactions are explicitly settled before the unit is reaped; ordinary
// Abort/4R is forbidden because the predecessor seal is authoritative. The
// method is idempotent so a Commit-classified fatal inconsistency and the driver's deferred
// terminal path converge on the same cleanup without double settlement.
func (rotation *unifiedDevRotation) settleFatal(cause error) error {
	if rotation.settled {
		return rotation.fatalResult(rotation.fatalCause)
	}
	if cause == nil {
		cause = rotation.fatalError()
	}
	if rotation.refit {
		cause = rotation.refitCurrentStageError(cause)
	}
	rotation.fatalCause = cause
	var sourceDecision observerSourceDecision
	if rotation.refit {
		ctx := rotation.unit.owner.observerRunContext()
		// Source disposition is the first post-PONR cleanup decision. A dead
		// control transport is not owner disappearance; when tmux still names the
		// exact owner (or cannot answer authoritatively), retain all uncertain
		// durable state and its accounting rather than manufacturing deletion
		// authority from the observer failure.
		sourceDecision = rotation.unit.owner.classifyRefitSource(ctx, rotation.refitSource, rotation.old)
		if sourceDecision.disposition == observerSourceTransportLost || sourceDecision.disposition == observerSourceAmbiguous {
			rotation.retainUncertainRefitFailure(cause)
			return rotation.fatalResult(cause)
		}
		// A pre-registry refit reservation still owns the provisional
		// successor's transferred R2/R3 holds. Settle it before registry Fatal
		// can reap and unlink that successor; otherwise the pane disappears
		// before RotationReservation.Fatal can return those holds, orphaning
		// bounded capacity. Uncertain ownership returned above deliberately
		// retains the reservation instead.
		if !rotation.registryCommitted && rotation.journal != nil {
			rotation.unit.owner.journalMu.Lock()
			rotation.journal.Fatal()
			rotation.unit.owner.journalMu.Unlock()
			rotation.journal = nil
		}
	}
	if rotation.registryCommitted {
		rotation.failCommittedSuccessor(cause)
	} else if rotation.registryT != nil {
		rotation.registryT.Fatal(cause)
		rotation.registryT = nil
		if rotation.refit {
			rotation.failRefitPredecessor(cause, sourceDecision)
		} else {
			// Fatal registry settlement synchronously removes provider authority.
			// Only after no fresh attach can bind the invalid predecessor do we end
			// every existing tail with the one terminal generation-failed outcome.
			effects := rotation.unit.owner
			effects.subscriberMu.Lock()
			effects.closeSubscribersLocked(rotation.oldKey, rotation.fatalCloseReason())
			effects.subscriberMu.Unlock()
		}
	} else if rotation.refit {
		rotation.failRefitPredecessor(cause, sourceDecision)
	}
	if rotation.journal != nil {
		rotation.unit.owner.journalMu.Lock()
		if rotation.journalCommitted {
			rotation.journal.FailCommitted()
		} else {
			rotation.journal.Fatal()
		}
		rotation.unit.owner.journalMu.Unlock()
		rotation.journal = nil
	}
	if rotation.holder != nil {
		rotation.holder.mu.Lock()
		rotation.holder.rotation.release()
		rotation.holder.rotation = nil
		rotation.holder.aborted = true
		rotation.holder.mu.Unlock()
	}
	if rotation.unit != nil {
		if rotation.unit.process != nil && rotation.unit.process.Process != nil {
			_ = rotation.unit.process.Process.Kill()
		}
		rotation.unit.owner.reapFaultedUnitWithReason(rotation.unit, rotation.fatalCloseReason())
	}
	// The provisional successor is absent from the unit witness snapshot until
	// commit. If its initial-operation lifetime already settled, terminal reap
	// cannot discover its retained journal through a runtime generation. Only
	// here, after source disposition, journal transaction and witness settlement,
	// transfer that exact key to the existing unlink/retry owner. Uncertain refits
	// returned above and retain their durable data.
	rotation.registry.retention.mu.Lock()
	retiredSuccessor := rotation.initial != nil && rotation.initial.generation != nil &&
		rotation.initial.generation.key == rotation.newKey && rotation.initial.generation.lifetimeReleased &&
		rotation.registry.retention.generations[rotation.newKey] == nil
	rotation.registry.retention.mu.Unlock()
	if retiredSuccessor {
		rotation.registry.retention.retireDurable(rotation.newKey)
	}
	rotation.unit.owner.journalMu.Lock()
	rotation.unit.owner.realm.ReclaimRetired(rotation.newKey)
	rotation.unit.owner.journalMu.Unlock()
	rotation.settled = true
	return rotation.fatalResult(cause)
}

// retainUncertainRefitFailure is the terminal owner for exact-owner transport
// loss and ambiguous source inventory after the width composite crossed PONR.
// It removes every route/attachment surface and marks both possible durable
// generations failed, but deliberately enqueues no cleanup and settles no
// journal reservation: only a later authoritative Close/reconciliation may
// release the retained slot and logical/physical charge.
func (rotation *unifiedDevRotation) retainUncertainRefitFailure(cause error) {
	effects := rotation.unit.owner
	registry := rotation.registry
	effects.subscriberMu.Lock()
	effects.mu.Lock()
	registry.mu.Lock()
	registry.retention.mu.Lock()
	keys := []unifiedjournal.PaneKey{rotation.oldKey}
	if rotation.newKey != (unifiedjournal.PaneKey{}) && rotation.newKey != rotation.oldKey {
		keys = append(keys, rotation.newKey)
	}
	for _, key := range keys {
		// Registry Commit may already have retired the predecessor. Uncertainty
		// retains every generation that still exists; it must never recreate a
		// historical key and consume a fresh pane slot merely to mark it failed.
		if _, exists := registry.retention.generations[key]; !exists {
			continue
		}
		winner, _ := registry.retention.commitFaultLocked(key, "refit_post_ponr_uncertain", cause)
		if winner {
			registry.publishRetentionFailureLocked(key)
		}
	}
	if rotation.registryT != nil {
		rotation.registryT.settled = true
		rotation.registryT.disposition = paneRotationCommitFatal
		rotation.registryT.removeOwnershipLocked()
		rotation.registryT = nil
	}
	registry.retention.mu.Unlock()
	registry.mu.Unlock()

	delete(effects.active, rotation.session)
	delete(effects.units, rotation.session)
	delete(effects.adopting, rotation.session)
	delete(effects.rotationStates, rotation.session)
	for _, witness := range rotation.unit.witnesses {
		if effects.panes[witness.Pane] == rotation.unit.holder {
			delete(effects.panes, witness.Pane)
		}
		delete(effects.publishedSequence, journalKey(witness))
	}
	effects.closeSubscribersLocked(rotation.oldKey, proto.SubscriberClosedRefitFaulted)
	if rotation.newKey != (unifiedjournal.PaneKey{}) {
		effects.closeSubscribersLocked(rotation.newKey, proto.SubscriberClosedRefitFaulted)
	}
	effects.mu.Unlock()
	effects.subscriberMu.Unlock()

	if rotation.holder != nil {
		rotation.holder.mu.Lock()
		rotation.holder.rotation.release()
		rotation.holder.rotation = nil
		rotation.holder.aborted = true
		rotation.holder.clearPendingLocked()
		rotation.holder.mu.Unlock()
	}
	// This terminal path already performed the complete provider reap while
	// retaining storage. Prevent the ordinary unit-death path from synthesizing
	// a Disconnect that would turn uncertainty into cleanup authority. Claim
	// reap ownership before signalling the process; the wait goroutine is
	// otherwise allowed to win this exact interval and recreate the retired
	// predecessor generation.
	rotation.unit.reapOnce.Do(func() { rotation.unit.memory.done() })
	if rotation.unit.process != nil && rotation.unit.process.Process != nil {
		_ = rotation.unit.process.Process.Kill()
	}
	rotation.settled = true
}

func (rotation *unifiedDevRotation) fatalError() error {
	if rotation.refit {
		return ErrUnifiedRefitFatal
	}
	return ErrUnifiedRotateFatal
}

// fatalResult preserves typed causes for internal errors.Is/As on every fatal
// return, including repeated dispositions. Public errors and subscriber reasons
// remain closed; refit causes retain their existing stage/class wrapper.
func (rotation *unifiedDevRotation) fatalResult(cause error) error {
	if cause == nil {
		return rotation.fatalError()
	}
	return errors.Join(rotation.fatalError(), cause)
}

func (rotation *unifiedDevRotation) fatalCloseReason() proto.SubscriberCloseReason {
	if rotation.refit {
		return proto.SubscriberClosedRefitFaulted
	}
	return proto.SubscriberClosedGenerationFailed
}

// failRefitPredecessor owns the interval after the guarded width composite
// may have reached tmux but before a successor registry transaction exists.
// The predecessor can no longer be replay authority: make it ineligible and
// remove active attachment authority before ending its tails with the typed
// terminal refit fault. No old generation is revived and no realm breaker is
// involved.
func (rotation *unifiedDevRotation) failRefitPredecessor(cause error, decision observerSourceDecision) {
	effects := rotation.unit.owner
	effects.subscriberMu.Lock()
	effects.mu.Lock()
	if active, ok := effects.active[rotation.session]; ok && active == rotation.oldKey {
		delete(effects.active, rotation.session)
	}
	registry := rotation.registry
	registry.mu.Lock()
	if state, ok := registry.admitted[routeCoordinateKey(rotation.old)]; ok && state.witness == rotation.old {
		if session := registry.sessions[sessionKey(rotation.old.Session)]; session != nil && session.router.UnifiedEligible(rotation.old) {
			observation := controlmode.Observation{Kind: controlmode.ObservationOwnerGone, Witness: rotation.old}
			if decision.disposition == observerSourceReplacement {
				observation.Kind = controlmode.ObservationReplacement
				observation.Replacement = decision.replacement
			}
			session.router.Observe(observation)
		}
	}
	registry.retention.mu.Lock()
	var cleanup *retentionCommand
	retiredLifetime := registry.retention.generations[rotation.oldKey] == nil
	// Proved owner disappearance/replacement authorizes durable retirement,
	// but not optimistic accounting release. The generation remains the exact
	// retry owner until Realm.RetirePane proves unlink or ENOENT.
	if !retiredLifetime {
		var winner bool
		winner, cleanup = registry.retention.commitFaultLocked(rotation.oldKey, "refit_post_ponr", cause)
		generation := registry.retention.generations[rotation.oldKey]
		generation.durableRequested = true
		if winner {
			registry.publishRetentionFailureLocked(rotation.oldKey)
		}
	}
	registry.retention.mu.Unlock()
	registry.mu.Unlock()
	effects.closeSubscribersLocked(rotation.oldKey, proto.SubscriberClosedRefitFaulted)
	effects.mu.Unlock()
	effects.subscriberMu.Unlock()
	if retiredLifetime {
		// The retiring seal can refund the predecessor runtime lifetime before
		// this terminal callback arrives. Its captured journal key still owns
		// any durable liability; reuse the unlink retry owner without inventing
		// a fresh generation or refunding its already-returned P/Q a second time.
		registry.retention.retireDurable(rotation.oldKey)
	}
	if cleanup != nil {
		cleanup.done = make(chan error, 1)
		registry.retention.appendCleanup(cleanup)
		_ = rotation.waitDependency(nil, cleanup.done)
	}
}

// failCommittedSuccessor owns the interval after the registry swap and before
// rotation_pending is durable. It removes attachment authority before making
// the exact successor runtime fault visible, closes every successor tail with
// a terminal typed outcome, and only then lets the ordinary unit reap remove
// provider/publication bookkeeping. Journal unlink settlement is kept in the
// caller so no storage I/O occurs while subscriberMu is held.
func (rotation *unifiedDevRotation) failCommittedSuccessor(cause error) {
	effects := rotation.unit.owner
	effects.subscriberMu.Lock()
	effects.mu.Lock()
	if active, ok := effects.active[rotation.session]; ok && active == rotation.newKey {
		delete(effects.active, rotation.session)
	}

	registry := rotation.registry
	registry.mu.Lock()
	if state, ok := registry.admitted[routeCoordinateKey(rotation.next)]; ok && state.witness == rotation.next {
		if session := registry.sessions[sessionKey(rotation.next.Session)]; session != nil && session.router.UnifiedEligible(rotation.next) {
			session.router.Observe(controlmode.Observation{
				Kind:        controlmode.ObservationReplacement,
				Witness:     rotation.next,
				Replacement: rotation.next,
			})
		}
	}
	registry.retention.mu.Lock()
	var cleanup *retentionCommand
	// The completed initial operation retains the exact committed lifetime even
	// after registryT is released. Observer cleanup may have retired it first.
	if generation := registry.retention.generations[rotation.newKey]; generation != nil && rotation.initial != nil && generation == rotation.initial.generation {
		var winner bool
		winner, cleanup = registry.retention.commitFaultLocked(rotation.newKey, "rotation_committed", cause)
		if winner {
			registry.publishRetentionFailureLocked(rotation.newKey)
		}
	}
	registry.retention.mu.Unlock()
	registry.mu.Unlock()

	effects.closeSubscribersLocked(rotation.newKey, rotation.fatalCloseReason())
	effects.mu.Unlock()
	effects.subscriberMu.Unlock()

	if cleanup != nil {
		cleanup.done = make(chan error, 1)
		registry.retention.appendCleanup(cleanup)
		_ = rotation.waitDependency(nil, cleanup.done)
	}
}

func (rotation *unifiedDevRotation) abortRegistry() bool {
	if rotation.registryT == nil {
		return true
	}
	if !rotation.registryT.Abort() {
		return false
	}
	// Abort transfers no storage ownership. Drain accepted work and the
	// dispatcher before restore is allowed to unlink the provisional journal.
	if rotation.registryT.initial != nil {
		_ = settleInitialOwned(rotation.registryT.initial, rotation.registry, ErrPaneRotationBusy, rotation.registryT.settlement)
	}
	rotation.registryT = nil
	return true
}

func (rotation *unifiedDevRotation) ownerJournalAbort() {
	rotation.unit.owner.journalMu.Lock()
	rotation.journal.Abort()
	rotation.unit.owner.realm.ReclaimRetired(rotation.newKey)
	rotation.unit.owner.journalMu.Unlock()
	rotation.journal = nil
}

func (rotation *unifiedDevRotation) writePending(witness controlmode.PaneWitness, pending []byte) error {
	for len(pending) != 0 {
		count := len(pending)
		if count > rotationReplayBatchBytes {
			count = rotationReplayBatchBytes
		}
		if err := rotation.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: witness, Data: pending[:count]}); err != nil {
			return err
		}
		pending = pending[count:]
	}
	return nil
}

func (rotation *unifiedDevRotation) commitPending(witness controlmode.PaneWitness) error {
	holder := rotation.holder
	holder.mu.Lock()
	var pending []byte
	if holder.rotation != nil {
		defer holder.rotation.release()
		pending = holder.rotation.data
	}
	records := (len(pending) + rotationReplayBatchBytes - 1) / rotationReplayBatchBytes
	if int64(len(pending)) > unifiedjournal.RotationPendingCapBytes || int64(records) > unifiedjournal.RotationPendingCapRecords {
		holder.mu.Unlock()
		return ErrUnifiedRotatePendingOverflow
	}
	holder.rotation = nil
	holder.committed = true
	holder.mu.Unlock()
	return rotation.writePending(witness, pending)
}
