package broker

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

var errRecordingInitial = errors.New("initial recording state is not ready")

type recordingInitialKind uint8

const (
	recordingInitialBirth recordingInitialKind = iota + 1
	recordingInitialAdoption
	recordingInitialRotation
)

// The operation pointer is the non-reusable identity. Its generation pointer
// prevents a same-key replacement from consuming an earlier result. Payload is
// owned by the queued command; this retained proof contains no terminal bytes.
type recordingInitialOperation struct {
	runtime    *retentionTrialRuntime
	generation *retentionGeneration
	kind       recordingInitialKind
	witness    controlmode.PaneWitness
	source     terminal.SourceWitness
	geometry   unifiedjournal.Geometry
	bytes      int64
	chunks     int
	digest     [32]byte
	done       chan struct{}
	// The fields below are guarded by runtime.mu, including after done closes.
	cancelled      bool
	err            error
	receipt        *recordingInitialReceipt
	published      bool
	requestContext context.Context
	owner          *unifiedDevUnit
	// Once publishes the immutable settlement channel/error to every waiter.
	settlement     sync.Once
	settlementDone <-chan struct{}
	settlementErr  error
}

type recordingInitialReceipt struct {
	operation *recordingInitialOperation
	kind      recordingInitialKind
	source    terminal.SourceWitness
	key       unifiedjournal.PaneKey
	geometry  unifiedjournal.Geometry
	bytes     int64
	chunks    int
	digest    [32]byte
	sequence  int64
	end       int64
}

type recordingInitialObserver interface {
	beginInitial(recordingInitialKind, controlmode.PaneWitness, terminal.SourceWitness, unifiedjournal.Geometry, []byte) (*recordingInitialOperation, error)
	publishInitial(*recordingInitialOperation) error
	recordingRegistry() *paneRegistry
}

func (registry *paneRegistry) recordingRegistry() *paneRegistry { return registry }

func (effects *UnifiedDevPaneEffects) initialObserver() recordingInitialObserver {
	observer, _ := effects.observer.(recordingInitialObserver)
	return observer
}

// Creation with no observed bytes needs an actual state witness. Its own
// observer runs the same atomic capture as adoption; a zero-length buffer is
// never interpreted as a blank terminal. If raw byte-zero output arrives before
// the capture boundary it remains authoritative and the capture is unused.
func (birth *unifiedDevBirth) captureInitialIfEmpty() error {
	birth.mu.Lock()
	needed := !birth.aborted && len(birth.pending) == 0
	birth.mu.Unlock()
	if !needed {
		return nil
	}
	birth.owner.mu.Lock()
	unit := birth.owner.units[birth.witness.Session.Session]
	birth.owner.mu.Unlock()
	if unit == nil || unit.holder != birth {
		return errRecordingInitial
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan unifiedDevCommandResult, 1)
	request := unifiedDevCommand{server: birth.owner.server, birthCapture: birth, context: ctx, done: done}
	select {
	case unit.commands <- request:
	case <-unit.done:
		return errRecordingInitial
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case result := <-done:
		return result.err
	case <-unit.done:
		return errRecordingInitial
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (unit *unifiedDevUnit) captureBirthInitial(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error, request unifiedDevCommand) error {
	var batch controlmode.EventBatch
	defer batch.Release()
	birth := request.birthCapture
	if birth != unit.holder || birth == nil {
		return errRecordingInitial
	}
	birth.mu.Lock()
	haveOutput := len(birth.pending) != 0
	aborted := birth.aborted
	birth.mu.Unlock()
	if aborted {
		return errRecordingInitial
	}
	if haveOutput {
		return nil
	}
	if _, err := io.WriteString(unit.ptmx, adoptionCompositeLine(birth.witness.Pane)); err != nil {
		return err
	}
	response := recordingResponse{owner: unit.memory}
	defer response.release()
	responses := make([]string, 0, adoptionCompositeBlocks)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-request.context.Done():
			return request.context.Err()
		case err := <-readErr:
			if err == nil {
				return io.EOF
			}
			return err
		case chunk := <-read:
			events, err := decodeRotationEvents(decoder, chunk, &batch)
			if err != nil {
				return err
			}
			for index, event := range events {
				switch event.Kind {
				case controlmode.EventCommandResponse:
					if err := response.Write(event.Data); err != nil {
						return err
					}
				case controlmode.EventCommandError:
					return errRecordingInitial
				case controlmode.EventCommandEnd:
					responses = append(responses, response.String())
					response.Reset()
					if len(responses) != adoptionCompositeBlocks {
						continue
					}
					bootstrap, geometry, _, err := parseInitialCapture(responses, unit.process.Process.Pid, nil)
					if err != nil {
						return err
					}
					birth.mu.Lock()
					valid := !birth.aborted && !birth.streaming && birth.geometry == geometry
					if valid && len(birth.pending) == 0 {
						if err := birth.appendPendingLocked(bootstrap); err != nil {
							birth.mu.Unlock()
							return err
						}
					}
					birth.mu.Unlock()
					if !valid {
						return errRecordingInitial
					}
					for _, rest := range events[index+1:] {
						if err := unit.owner.consumeObserverEvent(rest); err != nil {
							return err
						}
					}
					return nil
				default:
					if err := unit.owner.consumeObserverEvent(event); err != nil {
						return err
					}
				}
			}
		}
	}
}

func parseInitialCapture(responses []string, observerPID int, tamper func(string) string) ([]byte, unifiedjournal.Geometry, bool, error) {
	invalid := func(err error) ([]byte, unifiedjournal.Geometry, bool, error) {
		return nil, unifiedjournal.Geometry{}, false, err
	}
	if len(responses) != adoptionCompositeBlocks {
		return invalid(errRecordingInitial)
	}
	if err := rejectObserverClientInventory(responses[0], observerPID); err != nil {
		return invalid(err)
	}
	if strings.TrimSpace(responses[1]) != "" {
		return invalid(errRecordingInitial)
	}
	pre, post := strings.TrimSpace(responses[2]), strings.TrimSpace(responses[6])
	if tamper != nil {
		post = tamper(post)
	}
	probe, err := parseAdoptionProbe(pre)
	if err != nil {
		return invalid(err)
	}
	switch {
	case probe.alternate != 0:
		return invalid(ErrUnifiedAdoptAlternateScreen)
	case probe.windows != 1:
		return invalid(ErrUnifiedAdoptMultiWindow)
	case probe.panes != 1:
		return invalid(ErrUnifiedAdoptMultiPane)
	}
	if pre != post {
		return invalid(errUnifiedAdoptDrift)
	}
	modes, err := parseAdoptionModes(strings.TrimSpace(responses[5]))
	if err != nil {
		return invalid(err)
	}
	// Exactly one LF belongs to the command response, not the parser prefix.
	pending := strings.TrimSuffix(responses[4], "\n")
	bootstrap, trimmed, err := synthesizeAdoptionBootstrap(parseAdoptionCapture(responses[3]), pending, probe.cursorX, probe.cursorY, modes)
	if err != nil {
		return invalid(err)
	}
	return bootstrap, unifiedjournal.Geometry{Columns: modes.columns, Rows: modes.rows}, trimmed, nil
}

// startInitial admits one complete initial state atomically into the ordinary
// bounded manager queue. Initial state has its own command because a generic
// boundary cannot prove that each earlier output was accepted rather than
// discarded. Subsequent live output remains FIFO behind this command.
func (runtime *retentionTrialRuntime) startInitial(kind recordingInitialKind, witness controlmode.PaneWitness, source terminal.SourceWitness, geometry unifiedjournal.Geometry, payload []byte) (*recordingInitialOperation, error) {
	key := journalKey(witness)
	if kind < recordingInitialBirth || kind > recordingInitialRotation || geometry.Columns <= 0 || geometry.Rows <= 0 || runtime.options.realm == nil {
		return nil, errRecordingInitial
	}
	if err := runtime.bindSource(key, source); err != nil {
		return nil, err
	}
	reservation, err := runtime.reserve(key, len(payload))
	if err != nil {
		return nil, err
	}
	chunks := (len(payload) + runtime.options.maxBatchBytes - 1) / runtime.options.maxBatchBytes
	if chunks == 0 {
		chunks = 1
	}
	op := &recordingInitialOperation{runtime: runtime, generation: reservation.generation, kind: kind, witness: witness, source: source, geometry: geometry, bytes: int64(len(payload)), chunks: chunks, digest: sha256.Sum256(payload), done: make(chan struct{})}
	runtime.mu.Lock()
	generation := runtime.generations[key]
	valid := generation == reservation.generation && !generation.failed && generation.initial == nil && !generation.retireRequested && !generation.abandoned
	if valid {
		generation.initial = op
		reservation.parts = chunks
	}
	runtime.mu.Unlock()
	if !valid {
		runtime.cancelReservation(reservation)
		return nil, errRecordingInitial
	}
	command := &retentionCommand{kind: retentionCommandInitial, key: key, payload: append([]byte(nil), payload...), initial: op, reservation: reservation}
	if err := runtime.enqueue(command); err != nil {
		runtime.finishInitial(op, nil, err)
		return nil, err
	}
	return op, nil
}

func (op *recordingInitialOperation) healthyLocked() bool {
	if op == nil || op.cancelled || op.runtime.closing || op.runtime.closed {
		return false
	}
	if op.owner != nil && !op.owner.isLive() {
		return false
	}
	if !op.published && op.requestContext != nil && op.requestContext.Err() != nil {
		return false
	}
	g := op.runtime.generations[journalKey(op.witness)]
	return g == op.generation && g != nil && g.initial == op && !g.failed && !g.retireRequested && !g.abandoned
}

func (op *recordingInitialOperation) resultLocked() (*recordingInitialReceipt, error) {
	if !op.healthyLocked() || op.err != nil || op.receipt == nil {
		return nil, errors.Join(errRecordingInitial, op.err)
	}
	return op.receiptLocked()
}

func (op *recordingInitialOperation) receiptLocked() (*recordingInitialReceipt, error) {
	if op == nil || op.cancelled || op.err != nil || op.receipt == nil {
		return nil, errRecordingInitial
	}
	r := op.receipt
	if r.operation != op || r.kind != op.kind || r.source != op.source || r.key != journalKey(op.witness) || r.geometry != op.geometry || r.bytes != op.bytes || r.chunks != op.chunks || r.digest != op.digest || r.sequence != int64(op.chunks) || r.end != op.bytes {
		return nil, errRecordingInitial
	}
	return r, nil
}

func (op *recordingInitialOperation) result() (*recordingInitialReceipt, error) {
	if op == nil {
		return nil, errRecordingInitial
	}
	op.runtime.mu.Lock()
	defer op.runtime.mu.Unlock()
	return op.resultLocked()
}

func (op *recordingInitialOperation) cancel() {
	if op != nil {
		op.runtime.mu.Lock()
		op.cancelled = true
		op.runtime.mu.Unlock()
	}
}

func (op *recordingInitialOperation) bindContext(ctx context.Context) {
	if op != nil {
		op.runtime.mu.Lock()
		op.requestContext = ctx
		op.runtime.mu.Unlock()
	}
}

func (op *recordingInitialOperation) bindOwner(unit *unifiedDevUnit) {
	op.runtime.mu.Lock()
	op.owner = unit
	op.runtime.mu.Unlock()
}

func (runtime *retentionTrialRuntime) initialReady(key unifiedjournal.PaneKey) bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	g := runtime.generations[key]
	if g == nil || !g.admitted || g.initial == nil || !g.initial.published {
		return false
	}
	_, err := g.initial.resultLocked()
	return err == nil
}

func (runtime *retentionTrialRuntime) finishInitial(op *recordingInitialOperation, receipt *recordingInitialReceipt, err error) {
	runtime.mu.Lock()
	if err == nil && !op.healthyLocked() {
		err = errRecordingInitial
	}
	op.err = err
	if err == nil {
		op.receipt = receipt
	}
	close(op.done)
	runtime.mu.Unlock()
}

func (runtime *retentionTrialRuntime) processInitial(command *retentionCommand) {
	op, key := command.initial, command.key
	pane := runtime.pane(key)
	var err error
	runtime.mu.Lock()
	valid := op.healthyLocked()
	runtime.mu.Unlock()
	if !valid || pane.bytes != 0 || pane.heldBytes != 0 || pane.paused {
		err = errRecordingInitial
	}
	if err == nil {
		runtime.withJournalLock(func() {
			geometry, readErr := runtime.options.realm.InitialGeometry(key)
			if readErr != nil || geometry != op.geometry || runtime.options.realm.CommittedSequence(key) != 0 || !runtime.options.realm.UnifiedEligible(key) {
				err = errors.Join(errRecordingInitial, readErr)
			}
		})
	}
	var last unifiedjournal.Record
	completed := 0
	for chunk, start := 0, 0; err == nil && chunk < op.chunks; chunk++ {
		end := start + runtime.options.maxBatchBytes
		if end > len(command.payload) {
			end = len(command.payload)
		}
		part := command.payload[start:end]
		runtime.active = []retentionComponent{{data: part, reservation: command.reservation, final: chunk+1 == op.chunks}}
		last, err = pane.sequencer.WriteRecord(key, part)
		runtime.active = nil
		if err == nil && (last.Key != key || last.Kind != unifiedjournal.RecordOutput || last.Sequence != int64(chunk+1) || last.Start != int64(start) || last.End != int64(end) || last.Hash != sha256.Sum256(part)) {
			err = errRecordingInitial
		}
		runtime.mu.Lock()
		valid = op.healthyLocked()
		runtime.mu.Unlock()
		if err == nil && !valid {
			err = errRecordingInitial
		}
		start = end
		if err == nil {
			completed++
		}
	}
	if err != nil {
		_, cleanup := runtime.classifyFault(key, "initial", err)
		runtime.publishDiscardParts("storage_fault", command.reservation, op.chunks-completed)
		if cleanup != nil {
			runtime.appendCleanup(cleanup)
		}
		runtime.finishInitial(op, nil, err)
		return
	}
	runtime.finishInitial(op, &recordingInitialReceipt{operation: op, kind: op.kind, source: op.source, key: key, geometry: op.geometry, bytes: op.bytes, chunks: op.chunks, digest: op.digest, sequence: last.Sequence, end: last.End}, nil)
}

func (registry *paneRegistry) beginInitial(kind recordingInitialKind, witness controlmode.PaneWitness, source terminal.SourceWitness, geometry unifiedjournal.Geometry, payload []byte) (*recordingInitialOperation, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.retention == nil || registry.closing || registry.closed || registry.broken || !registry.disconnectAuthorityLocked(witness) || registry.failedIncarnationLocked(witness) {
		return nil, errRecordingInitial
	}
	return registry.retention.startInitial(kind, witness, source, geometry, payload)
}

// publishInitial is called while the provider's exact holder/unit is frozen.
// The nested order matches rotation: provider -> registry -> retention.
func (registry *paneRegistry) publishInitial(op *recordingInitialOperation) error {
	if op == nil {
		return errRecordingInitial
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.closing || registry.closed || registry.broken || !registry.disconnectAuthorityLocked(op.witness) || registry.failedIncarnationLocked(op.witness) {
		return errRecordingInitial
	}
	registry.retention.mu.Lock()
	defer registry.retention.mu.Unlock()
	if _, err := op.resultLocked(); err != nil {
		return err
	}
	op.published = true
	return nil
}

// startInitialSettlement only submits once-owned cleanup. Its caller has
// already waited for op.done through the correct dependency owner.
func startInitialSettlement(op *recordingInitialOperation, registry *paneRegistry, cause error) <-chan struct{} {
	op.settlement.Do(func() {
		// The generation's funded cleanup drains every accepted live component,
		// including partial batches queued while the initial write was held.
		// Cleanup cannot depend on an ordinary command reservation being free.
		registry.mu.Lock()
		runtime := registry.retention
		runtime.mu.Lock()
		var cleanup *retentionCommand
		if !runtime.closing && !runtime.closed && runtime.generations[journalKey(op.witness)] == op.generation && !op.generation.failed {
			winner, command := runtime.commitFaultLocked(journalKey(op.witness), "initial_cancel", cause)
			cleanup = command
			if winner {
				registry.publishRetentionFailureLocked(journalKey(op.witness))
			}
		}
		runtime.mu.Unlock()
		registry.mu.Unlock()
		if cleanup != nil {
			runtime.appendCleanup(cleanup)
		}
		dispatched, err := runtime.startDispatchFence()
		if err == nil {
			op.settlementDone = dispatched
		} else {
			// Close winning fence admission owns all already-accepted effects.
			// Its dispatcher completion is the remaining settlement authority.
			op.settlementDone = runtime.dispatcherDone
		}
		op.settlementErr = err
	})
	return op.settlementDone
}

// settleInitial is for external creation callers; their observer keeps running.
func settleInitial(op *recordingInitialOperation, registry *paneRegistry, cause error) error {
	return settleInitialOwned(op, registry, cause, nil)
}

// A nil owner is an external caller whose observer runs on another goroutine.
// Observer lifecycle drivers must supply their explicit stream owner.
type recordingDependencyOwner interface {
	wait(<-chan struct{}, <-chan error) error
}

func waitRecording(owner recordingDependencyOwner, done <-chan struct{}, result <-chan error) error {
	if owner != nil {
		return owner.wait(done, result)
	}
	select {
	case <-done:
		return nil
	case err := <-result:
		return err
	}
}

func settleInitialOwned(op *recordingInitialOperation, registry *paneRegistry, cause error, owner recordingDependencyOwner) error {
	op.cancel()
	waitRecording(owner, op.done, nil)
	waitRecording(owner, startInitialSettlement(op, registry, cause), nil)
	return errors.Join(cause, op.settlementErr)
}

func (unit *unifiedDevUnit) isLive() bool {
	if unit.supervisorFault.Load() || unit.readFailed.Load() {
		return false
	}
	select {
	case <-unit.done:
		return false
	default:
		return true
	}
}

func (birth *unifiedDevBirth) verifyInitialSource(ctx context.Context) error {
	if birth.initial == nil || birth.initial.source != birth.source || birth.initial.witness != birth.witness {
		return errRecordingInitial
	}
	// Synthetic registry fixtures have no external source. Production births
	// and adoptions always capture it before admission.
	if birth.source.Socket.Path == "" {
		return nil
	}
	now, err := buildSourceWitness(ctx, birth.owner.server, "", birth.witness.Session.Session)
	if err != nil {
		return err
	}
	if now != birth.source {
		return errRecordingInitial
	}
	return nil
}

func (effects *UnifiedDevPaneEffects) recordingReady(key unifiedjournal.PaneKey) bool {
	observer := effects.initialObserver()
	if observer == nil {
		return false
	}
	registry := observer.recordingRegistry()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.retention == nil || registry.closing || registry.closed || registry.broken {
		return false
	}
	registry.retention.mu.Lock()
	defer registry.retention.mu.Unlock()
	g := registry.retention.generations[key]
	// A sealed predecessor remains a valid snapshot source until the atomic
	// active-key swap. The rotation transaction retains its exact generation
	// and successful initial proof even if the runtime's retirement removed
	// the ordinary admission. This grants no fresh successor readiness.
	txn := registry.rotations[routeCoordinateJournalKey(key)]
	retiring := txn != nil && !txn.settled && txn.oldKey == key && txn.predecessorGeneration != nil
	if retiring {
		g = txn.predecessorGeneration
	}
	if g == nil || (!g.admitted && !retiring) || g.failed || g.initial == nil || !g.initial.published || !registry.disconnectAuthorityLocked(g.initial.witness) {
		return false
	}
	if retiring {
		owner := g.initial.owner
		if txn.initial != nil && txn.initial.owner != nil {
			owner = txn.initial.owner
		}
		if owner != nil && !owner.isLive() {
			return false
		}
		_, err := g.initial.receiptLocked()
		return err == nil
	}
	_, err := g.initial.resultLocked()
	return err == nil
}

func (unit *unifiedDevUnit) awaitInitial(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error, op *recordingInitialOperation) error {
	stream := &recordingSettlementStream{unit: unit, decoder: decoder, read: read, readErr: readErr}
	return stream.awaitInitial(ctx, op)
}

func (stream *recordingSettlementStream) awaitInitial(ctx context.Context, op *recordingInitialOperation) error {
	var batch controlmode.EventBatch
	defer batch.Release()
	unit := stream.unit
	observer := unit.owner.initialObserver()
	if observer == nil {
		return errRecordingInitial
	}
	registry := observer.recordingRegistry()
	var callerDone <-chan struct{}
	if unit.birth.context != nil {
		callerDone = unit.birth.context.Done()
	}
	for {
		select {
		case <-op.done:
			if ctx.Err() != nil {
				return stream.settleInitial(op, registry, ctx.Err())
			}
			if unit.birth.context != nil && unit.birth.context.Err() != nil {
				return stream.settleInitial(op, registry, unit.birth.context.Err())
			}
			_, err := op.result()
			if err != nil {
				return stream.settleInitial(op, registry, err)
			}
			return nil
		case <-ctx.Done():
			return stream.settleInitial(op, registry, ctx.Err())
		case <-callerDone:
			return stream.settleInitial(op, registry, unit.birth.context.Err())
		case err := <-stream.readErr:
			if err == nil {
				err = io.EOF
			}
			stream.readErr = nil
			return stream.settleInitial(op, registry, err)
		case chunk := <-stream.read:
			events, err := decodeRotationEvents(stream.decoder, chunk, &batch)
			if err != nil {
				stream.read = nil
				return stream.settleInitial(op, registry, err)
			}
			for _, event := range events {
				if err := unit.owner.consumeObserverEvent(event); err != nil {
					return stream.settleInitial(op, registry, err)
				}
			}
		}
	}
}

// Cancellation ends publication interest, not this unit's stream ownership.
// Keep draining and decoding while the accepted manager command and then its
// dispatcher effects settle. Errors are already terminal; no further event can
// restore readiness. This uses the owning goroutine, not an unbounded task.
func (stream *recordingSettlementStream) settleInitial(op *recordingInitialOperation, registry *paneRegistry, cause error) error {
	err := settleInitialOwned(op, registry, cause, stream)
	return errors.Join(err, stream.err)
}

// One observer-owned stream spans initial completion and failure settlement.
// Its dependency waits retain the first secondary failure so rollback cannot
// restore a generation whose continuation was lost while work settled.
type recordingSettlementStream struct {
	unit    *unifiedDevUnit
	decoder *controlmode.Decoder
	read    <-chan []byte
	readErr <-chan error
	err     error
}

func (stream *recordingSettlementStream) fail(err error) {
	if stream.err == nil {
		stream.err = err
	}
}

func (stream *recordingSettlementStream) wait(done <-chan struct{}, result <-chan error) error {
	var batch controlmode.EventBatch
	defer batch.Release()
	for {
		select {
		case <-done:
			return nil
		case err := <-result:
			return err
		case err := <-stream.readErr:
			if err == nil {
				err = io.EOF
			}
			stream.fail(err)
			stream.readErr = nil
		case chunk := <-stream.read:
			events, err := decodeRotationEvents(stream.decoder, chunk, &batch)
			if err != nil {
				stream.fail(err)
				stream.read = nil
				continue
			}
			for _, event := range events {
				stream.fail(stream.unit.owner.consumeObserverEvent(event))
			}
		}
	}
}
