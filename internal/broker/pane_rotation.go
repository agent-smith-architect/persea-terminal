package broker

import (
	"errors"
	"fmt"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

var (
	ErrPaneRotationInvalid   = errors.New("pane rotation invalid")
	ErrPaneRotationBusy      = errors.New("pane rotation already settled")
	ErrPaneRotationBootstrap = errors.New("pane rotation bootstrap failed")
	ErrPaneRotationValidate  = errors.New("pane rotation validation failed")
	ErrPaneRotationFatal     = errors.New("pane rotation commit invariant failed")
)

// paneRotationWitnessStage keeps the successor witness outside unit.witnesses
// until the registry swap is committed. Implementations must allocate their
// committed representation during Begin so Commit remains an in-memory swap.
type paneRotationWitnessStage interface {
	validate() bool
	commit(*paneRotationTxn) paneRotationCommitDisposition
	abort()
	fatal(error, proto.SubscriberCloseReason)
}

// paneRotationCommitDisposition is closed: Commit either installed the whole
// successor atomically, already took the authoritative fatal path, or is
// observing a transaction that was explicitly aborted before the seal. It is
// not an error and never licenses rollback after the predecessor seal.
type paneRotationCommitDisposition uint8

const (
	paneRotationCommitFatal paneRotationCommitDisposition = iota
	paneRotationCommitted
	paneRotationAborted
)

type paneRotationWitnessOwner interface {
	beginPaneRotationWitness(controlmode.PaneWitness, controlmode.PaneWitness) (paneRotationWitnessStage, error)
}

// paneRotationTxn owns only the provisional shadow router and successor
// runtime generation. The physical-pane coordinator is deliberately borrowed:
// predecessor and successor have the same coordinateKey and share its
// attachment set.
type paneRotationTxn struct {
	registry *paneRegistry

	previous controlmode.PaneWitness
	next     controlmode.PaneWitness
	oldKey   unifiedjournal.PaneKey
	newKey   unifiedjournal.PaneKey

	session               *sessionCoordinator
	successorSession      *sessionCoordinator
	coordinator           *paneCoordinator
	predecessorGeneration *retentionGeneration
	successorGeneration   *retentionGeneration
	successorCreated      bool
	witness               paneRotationWitnessStage
	closeReason           proto.SubscriberCloseReason
	refit                 bool

	validated          bool
	bootstrapAttempted bool
	initial            *recordingInitialOperation
	settlement         recordingDependencyOwner
	initialSource      terminal.SourceWitness
	// sealed is set only by the unit-goroutine driver after the retiring
	// rotation_seal boundary returns successfully. Transaction methods are not
	// a general concurrent API; the unit goroutine orders Validate, seal, Commit.
	sealed      bool
	settled     bool
	disposition paneRotationCommitDisposition
}

func samePaneRotationCoordinate(previous, next controlmode.PaneWitness) bool {
	return previous.Session.Server == next.Session.Server &&
		previous.Session.Session == next.Session.Session &&
		previous.Window == next.Window && previous.Pane == next.Pane &&
		previous.Incarnation == next.Incarnation &&
		previous.Session.ControlGeneration != next.Session.ControlGeneration
}

func samePaneRefitCoordinate(previous, next controlmode.PaneWitness) bool {
	return previous.Session.Server == next.Session.Server &&
		previous.Session.Session == next.Session.Session &&
		previous.Window == next.Window && previous.Pane == next.Pane &&
		previous.Incarnation != "" && next.Incarnation != "" &&
		previous.Incarnation != next.Incarnation &&
		previous.Session.ControlGeneration != next.Session.ControlGeneration
}

// BeginPaneRotation constructs a reversible registry transaction. It installs
// no route, admission, coordinator or unit witness.
func (registry *paneRegistry) BeginPaneRotation(previous, next controlmode.PaneWitness) (*paneRotationTxn, error) {
	return registry.beginPaneRollover(previous, next, proto.SubscriberClosedGenerationRotated, false)
}

// BeginPaneRefit uses the same provisional registry transaction as automatic
// rotation but publishes the explicit width-refit close reason at the atomic
// active-key swap.
func (registry *paneRegistry) BeginPaneRefit(previous, next controlmode.PaneWitness) (*paneRotationTxn, error) {
	return registry.beginPaneRollover(previous, next, proto.SubscriberClosedGenerationRefit, true)
}

func (registry *paneRegistry) beginPaneRollover(previous, next controlmode.PaneWitness, closeReason proto.SubscriberCloseReason, refit bool) (*paneRotationTxn, error) {
	coordinateValid := samePaneRotationCoordinate(previous, next)
	if refit {
		coordinateValid = samePaneRefitCoordinate(previous, next)
	}
	if registry.retention == nil || !coordinateValid {
		return nil, ErrPaneRotationInvalid
	}
	owner, ok := registry.effects.(paneRotationWitnessOwner)
	if !ok {
		return nil, ErrPaneRotationInvalid
	}
	witness, err := owner.beginPaneRotationWitness(previous, next)
	if err != nil {
		return nil, errors.Join(ErrPaneRotationInvalid, err)
	}
	abortWitness := true
	defer func() {
		if abortWitness {
			witness.abort()
		}
	}()

	shadow := controlmode.NewSessionRouter(next.Session)
	if err := shadow.Admit(next); err != nil {
		return nil, errors.Join(ErrPaneRotationInvalid, err)
	}
	successorSession := &sessionCoordinator{witness: next.Session, router: shadow}
	newKey := journalKey(next)
	successor, created, err := registry.retention.ensureGeneration(newKey, false)
	if err != nil {
		return nil, errors.Join(ErrPaneRotationInvalid, err)
	}
	if !created {
		return nil, ErrPaneRotationInvalid
	}
	releaseSuccessor := true
	defer func() {
		if releaseSuccessor {
			registry.retention.releaseUnadmitted(newKey, created)
		}
	}()

	txn := &paneRotationTxn{
		registry: registry, previous: previous, next: next,
		oldKey: journalKey(previous), newKey: newKey,
		successorSession: successorSession, successorGeneration: successor,
		successorCreated: created, witness: witness, closeReason: closeReason, refit: refit,
	}
	registry.mu.Lock()
	valid := txn.captureAssumptionsLocked()
	oldRoute := routeCoordinateKey(previous)
	if valid && (registry.rotations[oldRoute] != nil || registry.rotationGenerations[newKey] != nil) {
		valid = false
	}
	if valid {
		registry.rotations[oldRoute] = txn
		registry.rotationGenerations[newKey] = txn
	}
	registry.mu.Unlock()
	if !valid {
		return nil, ErrPaneRotationInvalid
	}
	abortWitness = false
	releaseSuccessor = false
	return txn, nil
}

func (txn *paneRotationTxn) captureAssumptionsLocked() bool {
	registry := txn.registry
	if registry.closing || registry.closed || registry.broken {
		return false
	}
	session := registry.sessions[sessionKey(txn.previous.Session)]
	if session == nil || session.witness != txn.previous.Session || !session.router.UnifiedEligible(txn.previous) {
		return false
	}
	admission, admitted := registry.admitted[routeCoordinateKey(txn.previous)]
	if !admitted || admission.failedIncarnation != "" || admission.witness != txn.previous {
		return false
	}
	if _, exists := registry.admitted[routeCoordinateKey(txn.next)]; exists {
		return false
	}
	if !txn.singleSessionRouteLocked() {
		return false
	}
	coordinator := registry.panes[coordinateKey(txn.previous)]
	if coordinator == nil || !txn.successorCoordinateAvailableLocked(coordinator) {
		return false
	}
	registry.retention.mu.Lock()
	predecessor := registry.retention.generations[txn.oldKey]
	valid := predecessor != nil && predecessor.admitted && !predecessor.failed &&
		registry.retention.generations[txn.newKey] == txn.successorGeneration &&
		!txn.successorGeneration.admitted && !txn.successorGeneration.failed
	if valid {
		valid = registry.retention.inheritSourceLocked(predecessor, txn.successorGeneration)
	}
	registry.retention.mu.Unlock()
	if !valid {
		return false
	}
	txn.session = session
	txn.coordinator = coordinator
	txn.predecessorGeneration = predecessor
	if predecessor.initial != nil {
		// The provisional successor inherits exact source ownership. The
		// observer replaces this with its verified post-capture witness before
		// WriteBootstrap; structural callers retain the predecessor's facts.
		txn.initialSource = predecessor.initial.source
	}
	return true
}

func (txn *paneRotationTxn) successorCoordinateAvailableLocked(coordinator *paneCoordinator) bool {
	next := txn.registry.panes[coordinateKey(txn.next)]
	if txn.refit {
		return next == nil
	}
	return next == coordinator
}

// singleSessionRouteLocked pins the current single-pane rotation boundary.
// A session router is replaced as one authority at Commit, so accepting a
// sibling admission would silently install a one-pane shadow over live routes.
// Caller holds registry.mu.
func (txn *paneRotationTxn) singleSessionRouteLocked() bool {
	want := sessionKey(txn.previous.Session)
	session := txn.registry.sessions[want]
	if session == nil {
		return false
	}
	seen := 0
	for _, state := range txn.registry.admitted {
		if sessionKey(state.witness.Session) != want {
			continue
		}
		// Disconnect history remains admitted for diagnostics, but it is not
		// sibling authority once the current session router can no longer route
		// it. Rotation still requires exactly one live, router-eligible route.
		if !session.router.UnifiedEligible(state.witness) {
			continue
		}
		if state.witness != txn.previous {
			return false
		}
		seen++
	}
	return seen == 1
}

func (txn *paneRotationTxn) assumptionsHoldLocked() bool {
	registry := txn.registry
	if registry.closing || registry.closed || registry.broken ||
		registry.rotations[routeCoordinateKey(txn.previous)] != txn ||
		registry.rotationGenerations[txn.newKey] != txn ||
		registry.sessions[sessionKey(txn.previous.Session)] != txn.session ||
		registry.panes[coordinateKey(txn.previous)] != txn.coordinator ||
		!txn.successorCoordinateAvailableLocked(txn.coordinator) {
		return false
	}
	if !txn.session.router.UnifiedEligible(txn.previous) {
		return false
	}
	admission, admitted := registry.admitted[routeCoordinateKey(txn.previous)]
	if !admitted || admission.failedIncarnation != "" || admission.witness != txn.previous {
		return false
	}
	if _, exists := registry.admitted[routeCoordinateKey(txn.next)]; exists {
		return false
	}
	if !txn.singleSessionRouteLocked() {
		return false
	}
	registry.retention.mu.Lock()
	valid := registry.retention.generations[txn.oldKey] == txn.predecessorGeneration &&
		txn.predecessorGeneration.admitted && !txn.predecessorGeneration.failed &&
		registry.retention.generations[txn.newKey] == txn.successorGeneration &&
		!txn.successorGeneration.admitted && !txn.successorGeneration.failed
	registry.retention.mu.Unlock()
	return valid
}

// sealedAssumptionsHoldLocked is the post-rotation_seal commit predicate. The
// successful retiring boundary intentionally makes the predecessor generation
// unadmitted and may delete it before Commit; all provider-independent registry
// identities, admissions and the provisional successor must still be exact.
// Caller holds registry.mu.
func (txn *paneRotationTxn) sealedAssumptionsHoldLocked() bool {
	registry := txn.registry
	if registry.closing || registry.closed || registry.broken ||
		registry.rotations[routeCoordinateKey(txn.previous)] != txn ||
		registry.rotationGenerations[txn.newKey] != txn ||
		registry.sessions[sessionKey(txn.previous.Session)] != txn.session ||
		registry.panes[coordinateKey(txn.previous)] != txn.coordinator ||
		!txn.successorCoordinateAvailableLocked(txn.coordinator) ||
		!txn.session.router.UnifiedEligible(txn.previous) {
		return false
	}
	admission, admitted := registry.admitted[routeCoordinateKey(txn.previous)]
	if !admitted || admission.failedIncarnation != "" || admission.witness != txn.previous {
		return false
	}
	if _, exists := registry.admitted[routeCoordinateKey(txn.next)]; exists {
		return false
	}
	if !txn.singleSessionRouteLocked() {
		return false
	}
	registry.retention.mu.Lock()
	predecessor := registry.retention.generations[txn.oldKey]
	validPredecessor := predecessor == nil || (predecessor == txn.predecessorGeneration &&
		predecessor.retireRequested && !predecessor.admitted && !predecessor.failed)
	valid := validPredecessor && registry.retention.generations[txn.newKey] == txn.successorGeneration &&
		!txn.successorGeneration.admitted && !txn.successorGeneration.failed
	registry.retention.mu.Unlock()
	return valid
}

// markSealed records the successful 7a retirement boundary. The transaction is
// driven by the owning unit goroutine, which calls this only after the boundary
// result is green and immediately before Commit.
func (txn *paneRotationTxn) markSealed() {
	if txn != nil && txn.validated && !txn.settled {
		txn.sealed = true
	}
}

// WriteBootstrap sends server-authored bootstrap bytes straight to the
// successor generation through the existing shared coordinator. It never
// exposes the shadow router.
func (txn *paneRotationTxn) WriteBootstrap(payload []byte) error {
	if txn == nil || txn.settled || txn.bootstrapAttempted {
		return ErrPaneRotationBusy
	}
	txn.bootstrapAttempted = true
	var geometry unifiedjournal.Geometry
	var err error
	txn.registry.retention.withJournalLock(func() { geometry, err = txn.registry.retention.options.realm.InitialGeometry(txn.newKey) })
	if err != nil {
		return errors.Join(ErrPaneRotationBootstrap, err)
	}
	txn.coordinator.mu.Lock()
	txn.initial, err = txn.registry.retention.startInitial(recordingInitialRotation, txn.next, txn.initialSource, geometry, payload)
	txn.coordinator.mu.Unlock()
	if err != nil {
		return errors.Join(ErrPaneRotationBootstrap, err)
	}
	return nil
}

// Validate is the final fallible registry step. It performs no storage I/O and
// holds no lock while waiting for a retention boundary.
func (txn *paneRotationTxn) Validate() error {
	if txn == nil || txn.settled {
		return ErrPaneRotationBusy
	}
	if !txn.bootstrapAttempted || !txn.witness.validate() {
		return ErrPaneRotationValidate
	}
	if _, err := txn.initial.result(); err != nil {
		return errors.Join(ErrPaneRotationValidate, err)
	}
	txn.registry.mu.Lock()
	valid := txn.assumptionsHoldLocked()
	txn.registry.mu.Unlock()
	if !valid {
		return ErrPaneRotationValidate
	}
	txn.validated = true
	return nil
}

// Commit rechecks the exact receipt and ownership before bounded in-memory
// installation. A violated post-Validate assumption is fatal, so it faults the
// successor and reaps the unit rather than attempting a partial swap or
// returning an error past the seal.
func (txn *paneRotationTxn) Commit() paneRotationCommitDisposition {
	if txn == nil {
		return paneRotationCommitFatal
	}
	if txn.settled {
		return txn.disposition
	}
	return txn.witness.commit(txn)
}

// commitProviderLocked installs under effects.mu -> registry.mu ->
// retention.mu. providerValid and installWitness are both evaluated in the
// caller's same effects.mu interval, making provider death/sibling drift and
// witness publication indivisible from the registry swap.
func (txn *paneRotationTxn) commitProviderLocked(providerValid bool, installWitness func()) (paneRotationCommitDisposition, *retentionCommand) {
	registry := txn.registry
	registry.mu.Lock()
	valid := providerValid && txn.validated && txn.assumptionsHoldLocked()
	if txn.sealed {
		valid = providerValid && txn.validated && txn.sealedAssumptionsHoldLocked()
	}
	registry.retention.mu.Lock()
	if valid {
		_, err := txn.initial.resultLocked()
		valid = err == nil
	}
	if valid {
		_, err := registry.retention.markAdmittedLocked(txn.newKey, txn.successorGeneration)
		valid = err == nil
	}
	if !valid {
		// The fatal inconsistency is classified while the transaction still owns newKey. This keeps
		// the unadmitted successor fault local instead of falling through the
		// missing-admission path to the realm-wide breaker.
		var cleanup *retentionCommand
		// Initial settlement may already have retired this exact provisional owner.
		// A late fault must not create an unfunded replacement lifetime.
		if generation := registry.retention.generations[txn.newKey]; generation != nil && generation == txn.successorGeneration {
			var winner bool
			winner, cleanup = registry.retention.commitFaultLocked(txn.newKey, "rotation_commit", ErrPaneRotationFatal)
			if winner {
				registry.publishRetentionFailureLocked(txn.newKey)
			}
		}
		registry.retention.mu.Unlock()
		txn.settled = true
		txn.disposition = paneRotationCommitFatal
		txn.removeOwnershipLocked()
		registry.mu.Unlock()
		return paneRotationCommitFatal, cleanup
	}
	registry.sessions[sessionKey(txn.next.Session)] = txn.successorSession
	if txn.refit {
		delete(registry.panes, coordinateKey(txn.previous))
		registry.panes[coordinateKey(txn.next)] = txn.coordinator
	}
	delete(registry.admitted, routeCoordinateKey(txn.previous))
	registry.admitted[routeCoordinateKey(txn.next)] = paneAdmissionState{witness: txn.next}
	txn.initial.published = true
	registry.retention.mu.Unlock()
	installWitness()
	txn.settled = true
	txn.disposition = paneRotationCommitted
	txn.removeOwnershipLocked()
	registry.mu.Unlock()
	return paneRotationCommitted, nil
}

func (txn *paneRotationTxn) removeOwnershipLocked() {
	delete(txn.registry.rotations, routeCoordinateKey(txn.previous))
	delete(txn.registry.rotationGenerations, txn.newKey)
}

func (txn *paneRotationTxn) finishFatal(cleanup *retentionCommand, cause error) {
	if cleanup != nil {
		cleanup.done = make(chan error, 1)
		txn.registry.retention.appendCleanup(cleanup)
		_ = waitRecording(txn.settlement, nil, cleanup.done)
	}
	if txn.initial != nil {
		_ = settleInitialOwned(txn.initial, txn.registry, cause, txn.settlement)
	}
	reason := proto.SubscriberClosedGenerationFailed
	if txn.closeReason == proto.SubscriberClosedGenerationRefit {
		reason = proto.SubscriberClosedRefitFaulted
	}
	txn.witness.fatal(cause, reason)
}

// Fatal is the post-PONR settlement owner for a registry rotation that cannot
// reach Commit. It classifies the provisional successor while the transaction
// still owns it, removes all provisional routing authority, waits for the
// successor generation's funded cleanup, and then reaps the owning unit. It
// never invokes the pre-seal witness Abort path and is idempotent.
func (txn *paneRotationTxn) Fatal(cause error) paneRotationCommitDisposition {
	if txn == nil {
		return paneRotationCommitFatal
	}
	if txn.settled {
		return txn.disposition
	}
	registry := txn.registry
	registry.mu.Lock()
	registry.retention.mu.Lock()
	var cleanup *retentionCommand
	// Initial settlement may already have retired this exact provisional owner.
	// A late fault must not create an unfunded replacement lifetime.
	if generation := registry.retention.generations[txn.newKey]; generation != nil && generation == txn.successorGeneration {
		var winner bool
		winner, cleanup = registry.retention.commitFaultLocked(txn.newKey, "rotation_fatal", cause)
		if winner {
			registry.publishRetentionFailureLocked(txn.newKey)
		}
	}
	registry.retention.mu.Unlock()
	txn.settled = true
	txn.disposition = paneRotationCommitFatal
	txn.removeOwnershipLocked()
	registry.mu.Unlock()
	txn.finishFatal(cleanup, cause)
	return paneRotationCommitFatal
}

// Abort discards only transaction-owned provisional state. It returns true
// only when the transaction is (or was already) authoritatively aborted.
// After the retiring seal is submitted the predecessor runtime state owns the
// point of no return; markSealed is only the sequenced driver's record. Abort
// therefore refuses unless retention can still prove the exact predecessor is
// admitted, non-retiring and nonfailed. The unit-goroutine driver must then
// continue directly to Commit/fatal settlement.
// In particular Abort never creates, deletes or rewrites registry.panes or
// its attachment set.
func (txn *paneRotationTxn) Abort() bool {
	if txn == nil {
		return false
	}
	if txn.settled {
		return txn.disposition == paneRotationAborted
	}
	if txn.sealed {
		return false
	}
	txn.registry.mu.Lock()
	if !txn.registry.retention.settleRotationAbort(txn.oldKey, txn.predecessorGeneration, txn.newKey, txn.successorGeneration, txn.successorCreated) {
		txn.registry.mu.Unlock()
		return false
	}
	txn.settled = true
	txn.disposition = paneRotationAborted
	txn.removeOwnershipLocked()
	txn.registry.mu.Unlock()
	// Logical abandonment must not wait on accepted I/O. Its references stay
	// funded until the storage owner separately settles the initial operation.
	txn.initial.cancel()
	txn.witness.abort()
	return true
}

func (txn *paneRotationTxn) String() string {
	if txn == nil {
		return "pane rotation <nil>"
	}
	return fmt.Sprintf("pane rotation %s:%d->%d", txn.previous.Session.Session,
		txn.previous.Session.ControlGeneration, txn.next.Session.ControlGeneration)
}

type unifiedDevRotationWitnessStage struct {
	effects   *UnifiedDevPaneEffects
	unit      *unifiedDevUnit
	session   string
	previous  []controlmode.PaneWitness
	committed []controlmode.PaneWitness
	oldKey    unifiedjournal.PaneKey
}

func (effects *UnifiedDevPaneEffects) beginPaneRotationWitness(previous, next controlmode.PaneWitness) (paneRotationWitnessStage, error) {
	effects.mu.Lock()
	defer effects.mu.Unlock()
	unit := effects.units[previous.Session.Session]
	if unit == nil || unit.sessionID != previous.Session.Session || effects.active[unit.sessionID] != journalKey(previous) {
		return nil, ErrPaneRotationInvalid
	}
	if !unit.isLive() {
		return nil, ErrPaneRotationInvalid
	}
	found := false
	for _, witness := range unit.witnesses {
		if witness == next {
			return nil, ErrPaneRotationInvalid
		}
		// Older control generations are immutable reap history, not live
		// sibling authority. In the currently active generation, previous
		// must be the sole witness and must occur exactly once.
		if witness.Session.ControlGeneration != previous.Session.ControlGeneration {
			continue
		}
		if witness != previous || found {
			return nil, ErrPaneRotationInvalid
		}
		found = true
	}
	if !found {
		return nil, ErrPaneRotationInvalid
	}
	before := append([]controlmode.PaneWitness(nil), unit.witnesses...)
	after := make([]controlmode.PaneWitness, len(before)+1)
	copy(after, before)
	after[len(before)] = next
	return &unifiedDevRotationWitnessStage{
		effects: effects, unit: unit, session: unit.sessionID,
		previous: before, committed: after, oldKey: journalKey(previous),
	}, nil
}

func (stage *unifiedDevRotationWitnessStage) validate() bool {
	stage.effects.mu.Lock()
	defer stage.effects.mu.Unlock()
	return stage.validateLocked()
}

func (stage *unifiedDevRotationWitnessStage) validateLocked() bool {
	if stage.effects.units[stage.session] != stage.unit || stage.effects.active[stage.session] != stage.oldKey {
		return false
	}
	if !stage.unit.isLive() {
		return false
	}
	if len(stage.unit.witnesses) != len(stage.previous) {
		return false
	}
	for index := range stage.previous {
		if stage.unit.witnesses[index] != stage.previous[index] {
			return false
		}
	}
	return true
}

func (stage *unifiedDevRotationWitnessStage) commit(txn *paneRotationTxn) paneRotationCommitDisposition {
	// The active-key publication and predecessor-tail close are one observable
	// authority swap. Attachment registration takes the same outer lock before
	// resolving active, so it can bind only the complete predecessor state or
	// the complete successor state.
	stage.effects.subscriberMu.Lock()
	stage.effects.mu.Lock()
	providerValid := stage.validateLocked()
	disposition, cleanup := txn.commitProviderLocked(providerValid, func() {
		stage.unit.witnesses = stage.committed
		stage.effects.active[stage.session] = txn.newKey
	})
	if disposition == paneRotationCommitted {
		if edge := stage.effects.rotationCommitEdge; edge != nil {
			edge("after_install_before_close")
		}
		stage.effects.closeSubscribersLocked(stage.oldKey, txn.closeReason)
		delete(stage.effects.publishedSequence, stage.oldKey)
		if edge := stage.effects.rotationCommitEdge; edge != nil {
			edge("after_close_before_unlock")
		}
	}
	stage.effects.mu.Unlock()
	stage.effects.subscriberMu.Unlock()
	if disposition == paneRotationCommitFatal {
		txn.finishFatal(cleanup, ErrPaneRotationFatal)
	}
	return disposition
}

func (*unifiedDevRotationWitnessStage) abort() {}

func (stage *unifiedDevRotationWitnessStage) fatal(_ error, reason proto.SubscriberCloseReason) {
	if stage.unit.process != nil && stage.unit.process.Process != nil {
		_ = stage.unit.process.Process.Kill()
	}
	stage.effects.mu.Lock()
	live := stage.effects.units[stage.session] == stage.unit
	stage.effects.mu.Unlock()
	// If a real reap already removed provider authority, that in-flight reap
	// owns the immutable disconnect snapshot. Re-entering sync.Once from this
	// fatal path would only wait on the same authoritative cleanup.
	if live {
		stage.effects.reapFaultedUnitWithReason(stage.unit, reason)
	}
}
