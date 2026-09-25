// Unified observer spawning, recovery, and retirement.
package broker

import (
	"context"
	"errors"
	"fmt"
	"github.com/creack/pty"
	"os"
	"os/exec"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
	"strings"
	"time"
)

type terminalRetirement struct {
	retryReady bool
	running    bool
	stopped    bool
	unit       *unifiedDevUnit
	witness    controlmode.PaneWitness
	backoff    time.Duration
	cancel     func() bool
}

type observerSourceDisposition uint8

const (
	observerSourceAmbiguous observerSourceDisposition = iota + 1
	observerSourceTransportLost
	observerSourceOwnerGone
	observerSourceReplacement
)

type observerSourceDecision struct {
	disposition observerSourceDisposition
	replacement controlmode.PaneWitness
}

func classifyObserverRecheck(want controlmode.PaneWitness, current *controlmode.PaneWitness, sessionPresent, authoritative bool) observerSourceDecision {
	if current != nil {
		if *current == want {
			return observerSourceDecision{disposition: observerSourceTransportLost}
		}
		return observerSourceDecision{disposition: observerSourceReplacement, replacement: *current}
	}
	if authoritative && !sessionPresent {
		return observerSourceDecision{disposition: observerSourceOwnerGone}
	}
	return observerSourceDecision{disposition: observerSourceAmbiguous}
}

// sameRefitOwner compares only facts that a width change cannot alter. The
// public incarnation deliberately includes geometry, so comparing the full
// witness would misclassify the successfully resized pane as a replacement at
// the exact moment fatal cleanup needs an authoritative owner disposition.
func sameRefitOwner(before, after terminal.SourceWitness) bool {
	return before.Socket == after.Socket && before.Server == after.Server &&
		before.SessionID == after.SessionID && before.WindowID == after.WindowID &&
		before.PaneID == after.PaneID && before.Pane == after.Pane
}

// AdoptSession makes an EXISTING tmux session journal-active: a per-session
// observer unit attaches to it, captures its state atomically, and opens a
// fresh reconstructed journal generation whose bootstrap is the synthesized
// capture; live bytes stream after it exactly as for born sessions. Adopting
// an already-active session is an idempotent success. Every refusal is typed
// and fails closed.
func (effects *UnifiedDevPaneEffects) AdoptSession(ctx context.Context, sessionID string, requestedHistory ...int) (UnifiedAdoption, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	historyRows := adoptionHistoryCapRows
	if len(requestedHistory) > 1 {
		return UnifiedAdoption{}, ErrUnifiedAdoptHistory
	}
	if len(requestedHistory) == 1 {
		historyRows = requestedHistory[0]
	}
	if historyRows < 0 || historyRows > adoptionHistoryCapRows {
		return UnifiedAdoption{}, ErrUnifiedAdoptHistory
	}
	if !validSessionID(sessionID) {
		return UnifiedAdoption{}, ErrUnifiedAdoptSessionMissing
	}
	effects.mu.Lock()
	key, active := effects.active[sessionID]
	effects.mu.Unlock()
	if active {
		if !effects.recordingReady(key) {
			return UnifiedAdoption{}, errRecordingInitial
		}
		return UnifiedAdoption{SessionID: sessionID, Key: key, Existing: true}, nil
	}
	memory, err := effects.transients.acquire()
	if err != nil {
		return UnifiedAdoption{}, err
	}
	transferred := false
	defer func() {
		if !transferred {
			memory.done()
		}
	}()
	// The existence precheck turns both a missing session and a stopped tmux
	// server into the same typed refusal before anything is spawned. It is
	// advisory only: the session can still die before the unit attaches, and
	// the attach block's %error is the authoritative backstop.
	response := recordingResponse{owner: memory}
	preflight, stopPreflight := context.WithTimeout(ctx, SetupTimeout)
	err = recordingCommandOutput(preflight, effects.server, &response, "has-session", "-t", sessionID)
	stopPreflight()
	response.release()
	if err != nil {
		if ctx.Err() != nil {
			return UnifiedAdoption{}, ctx.Err()
		}
		if errors.Is(err, errRecordingTransients) {
			return UnifiedAdoption{}, errRecordingTransients
		}
		return UnifiedAdoption{}, ErrUnifiedAdoptSessionMissing
	}
	result := make(chan unifiedDevCommandResult, 1)
	request := unifiedDevCommand{
		memory:   memory,
		context:  ctx,
		server:   effects.server,
		args:     []string{"attach-session", "-f", "ignore-size", "-t", sessionID},
		adoption: &unifiedDevAdoption{sessionID: sessionID, historyRows: historyRows},
		done:     result,
	}
	select {
	case effects.spawns <- request:
		transferred = true
	case <-ctx.Done():
		return UnifiedAdoption{}, ctx.Err()
	case <-time.After(5 * time.Second):
		return UnifiedAdoption{}, errors.New("unified adoption observer unavailable")
	}
	select {
	case outcome := <-result:
		if outcome.err != nil {
			return UnifiedAdoption{}, outcome.err
		}
		return UnifiedAdoption{SessionID: outcome.session, Key: outcome.key, Trimmed: outcome.trimmed, Existing: outcome.existing}, nil
	case <-ctx.Done():
		return UnifiedAdoption{}, ctx.Err()
	case <-time.After(30 * time.Second):
		return UnifiedAdoption{}, errors.New("unified adoption timed out")
	}
}

// RunObserver is the observer-unit supervisor. It owns the unit registry for
// the broker's lifetime: session births spawn units, unit exits are reaped
// without touching other sessions, and the broker goes down only when the
// supervisor itself stops. No control client is spawned at startup — the
// deprecated observer_session anchor is still validated by config but no
// longer observed, because every control connection belongs to exactly one
// observed session's unit.
func (effects *UnifiedDevPaneEffects) RunObserver(ctx context.Context, observer PaneObservationEffects) error {
	effects.mu.Lock()
	effects.observer = observer
	effects.observerCtx = ctx
	effects.observerStopped = false
	effects.mu.Unlock()
	defer effects.stopRotationScheduler()
	defer effects.stopTerminalRetirements()
	return effects.runSupervisedObserver(ctx)
}

// spawnUnit starts one observer unit whose control client's startup command is
// the caller-built session creation itself: the client is born observing the
// session it creates, so no pane byte can precede the unit's own stream.
// A spawn failure is returned to the caller; it never stops the supervisor.
func (effects *UnifiedDevPaneEffects) spawnUnit(ctx context.Context, request unifiedDevCommand) error {
	memory := request.memory
	if memory == nil {
		var err error
		memory, err = effects.transients.acquire()
		if err != nil {
			return err
		}
	}
	transferred := false
	defer func() {
		if !transferred {
			memory.done()
		}
	}()
	if err := request.spawnAdmissionError(); err != nil {
		return err
	}
	if request.server.Label != effects.server.Label {
		return errors.New("foreign unified server")
	}
	if request.adoption != nil {
		if err := effects.admitAdoptionSpawn(request); err != nil {
			if errors.Is(err, errUnifiedAdoptAnswered) {
				return nil
			}
			return err
		}
	}
	argv := append(tmuxArgv(request.server, "-C", "-f", "/dev/null"), request.args...)
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.Start(command)
	if err != nil {
		effects.abandonAdoption(request.adoption)
		return fmt.Errorf("start unified observer unit: %w", err)
	}
	if err := request.spawnAdmissionError(); err != nil {
		_ = command.Process.Kill()
		_ = ptmx.Close()
		_ = command.Wait()
		effects.abandonAdoption(request.adoption)
		return err
	}
	if err := unifiedDevDisableEcho(ptmx); err != nil {
		_ = ptmx.Close()
		transferred = true
		go func() { defer memory.done(); _ = command.Wait() }()
		effects.abandonAdoption(request.adoption)
		return fmt.Errorf("configure unified observer unit: %w", err)
	}
	unit := &unifiedDevUnit{
		memory: memory,
		owner:  effects, generation: effects.mintControlGeneration(request.adoption != nil),
		ptmx: ptmx, process: command, birth: request,
		commands: make(chan unifiedDevCommand), done: make(chan struct{}),
	}
	if request.birth != nil {
		request.birth.mu.Lock()
		request.birth.memory = memory
		request.birth.mu.Unlock()
	}
	if request.adoption != nil {
		unit.sessionID = request.adoption.sessionID
		if request.adoption.recovery != nil {
			unit.holder = request.adoption.holder
			unit.holder.mu.Lock()
			unit.holder.memory = memory
			unit.holder.mu.Unlock()
			unit.witnesses = append([]controlmode.PaneWitness(nil), request.adoption.witnesses...)
			request.adoption.recovery.unit = unit
			effects.mu.Lock()
			effects.units[unit.sessionID] = unit
			unit.sessionIdentity = unit.sessionID
			for _, witness := range unit.witnesses {
				if journalKey(witness) == request.adoption.recovery.oldKey {
					effects.panes[witness.Pane] = unit.holder
					break
				}
			}
			effects.mu.Unlock()
		}
	}
	memory.hold() // supervisor/reaper, independent of process/read-loop settlement
	effects.mu.Lock()
	if effects.supervised == nil {
		effects.supervised = make(map[*unifiedDevUnit]struct{})
	}
	unit.sessionIdentity = unit.sessionID
	effects.supervised[unit] = struct{}{}
	unit.progress.setStage(observerStageStarting)
	effects.mu.Unlock()
	transferred = true
	go unit.run(ctx)
	return nil
}

// errUnifiedAdoptAnswered tells spawnUnit the supervisor already delivered the
// idempotent already-active success to the requester, so nothing spawns and no
// second reply may be sent. It never leaves the supervisor.
var errUnifiedAdoptAnswered = errors.New("unified adoption already answered")

// admitAdoptionSpawn is the lifecycle worker's adoption gate. Its single slot
// serializes the check-then-mark of the
// adopting set: two requests for the same session cannot interleave here.
func (effects *UnifiedDevPaneEffects) admitAdoptionSpawn(request unifiedDevCommand) error {
	sessionID := request.adoption.sessionID
	effects.mu.Lock()
	if recovery := request.adoption.recovery; recovery != nil {
		if effects.rotation != recovery || effects.active[sessionID] != recovery.oldKey || effects.units[sessionID] != nil {
			effects.mu.Unlock()
			return ErrUnifiedAdoptInProgress
		}
		if _, inProgress := effects.adopting[sessionID]; inProgress {
			effects.mu.Unlock()
			return ErrUnifiedAdoptInProgress
		}
		effects.adopting[sessionID] = struct{}{}
		effects.mu.Unlock()
		return nil
	}
	if key, active := effects.active[sessionID]; active {
		if !effects.recordingReady(key) {
			effects.mu.Unlock()
			return errRecordingInitial
		}
		effects.mu.Unlock()
		request.done <- unifiedDevCommandResult{session: sessionID, key: key, existing: true}
		return errUnifiedAdoptAnswered
	}
	if _, inProgress := effects.adopting[sessionID]; inProgress {
		effects.mu.Unlock()
		return ErrUnifiedAdoptInProgress
	}
	effects.mu.Unlock()
	// A proven non-live recovered source can reach capture even when it holds
	// every slot. Reconstruction rechecks identity and owns slot transfer and
	// proven unlink before admitting replacement bytes. This precheck itself
	// grants no reservation or readiness authority.
	effects.journalMu.Lock()
	slots := effects.realm.AvailableCompletePaneSlots()
	recovered := effects.realm.HasRecoveredCaptureCandidate(effects.server.Label, sessionID)
	recovered = recovered || effects.realm.HasSettledFailedCaptureCandidate(effects.server.Label, sessionID)
	effects.journalMu.Unlock()
	if slots < 1 && !recovered {
		return ErrUnifiedAdoptSlotsExhausted
	}
	effects.mu.Lock()
	effects.adopting[sessionID] = struct{}{}
	effects.mu.Unlock()
	return nil
}

// abandonAdoption releases the in-progress marker for an adoption whose unit
// could not be spawned. A running unit never calls this: its marker is
// released either at successful registration or by reapUnit.
func (effects *UnifiedDevPaneEffects) abandonAdoption(adoption *unifiedDevAdoption) {
	if adoption == nil {
		return
	}
	effects.mu.Lock()
	delete(effects.adopting, adoption.sessionID)
	effects.mu.Unlock()
}

func (effects *UnifiedDevPaneEffects) startObserverRecovery(ctx context.Context, previous *unifiedDevUnit, key unifiedjournal.PaneKey) error {
	registry, ok := effects.observer.(*paneRegistry)
	if !ok || registry.retention == nil || previous == nil || previous.holder == nil {
		return ErrUnifiedRotateUnavailable
	}
	rotation := &unifiedDevRotation{session: previous.sessionID, oldKey: key, registry: registry}
	effects.mu.Lock()
	if effects.observerStopped || ctx.Err() != nil || effects.rotation != nil || effects.units[previous.sessionID] != nil || effects.active[previous.sessionID] != key {
		effects.mu.Unlock()
		return ErrUnifiedRotateUnavailable
	}
	effects.rotation = rotation
	effects.mu.Unlock()
	clear := func() {
		effects.mu.Lock()
		if effects.rotation == rotation {
			effects.rotation = nil
		}
		delete(effects.adopting, previous.sessionID)
		effects.mu.Unlock()
	}
	owner, err := registry.retention.acquireGeometryOwner(key)
	if err != nil {
		clear()
		return err
	}
	rotation.oldOwner = owner
	effects.journalMu.Lock()
	rotation.capacity, err = effects.realm.BeginRotationCapacity(key)
	effects.journalMu.Unlock()
	if err != nil {
		registry.retention.releaseGeometryOwner(key, owner)
		clear()
		return err
	}
	request := unifiedDevCommand{
		context: ctx,
		server:  effects.server,
		args:    []string{"attach-session", "-f", "ignore-size", "-t", previous.sessionID},
		adoption: &unifiedDevAdoption{
			sessionID: previous.sessionID, recovery: rotation, holder: previous.holder,
			witnesses: append([]controlmode.PaneWitness(nil), previous.witnesses...),
		},
		done: make(chan unifiedDevCommandResult, 1),
	}
	if err := effects.spawnUnit(ctx, request); err != nil {
		effects.releaseRotationCapacity(rotation.capacity)
		registry.retention.releaseGeometryOwner(key, owner)
		clear()
		return err
	}
	return nil
}

// reapUnit runs the unit-death protocol in the bounded lifecycle slot: mark the birth
// holder aborted so a racing commit fails closed, drop the session from the
// unit map, then distinguish a successfully inventoried missing tmux session
// from a control-client transport failure. Only the former owns terminal
// journal retirement. The supervisor and every other unit keep running.
func (effects *UnifiedDevPaneEffects) reapUnit(unit *unifiedDevUnit) {
	effects.reapUnitContext(effects.observerRunContext(), unit)
}

func (effects *UnifiedDevPaneEffects) reapUnitContext(ctx context.Context, unit *unifiedDevUnit) {
	unit.reapOnce.Do(func() {
		defer unit.memory.done()
		if unit.supervisorFault.Load() || errors.Is(unit.exitErr, ErrUnifiedObserverFlowControl) || errors.Is(unit.exitErr, ErrUnifiedRotateFatal) {
			effects.reapFaultedUnitOnce(unit)
			return
		}
		effects.reapUnitOnceContext(ctx, unit)
	})
}

// reapFaultedUnit is the synchronous post-fault entry point used when the
// caller already proved an internal invariant failure. Source inventory cannot
// turn that known fault into an ordinary reconnect merely because tmux remains
// alive.
func (effects *UnifiedDevPaneEffects) reapFaultedUnit(unit *unifiedDevUnit) {
	effects.reapFaultedUnitWithReason(unit, proto.SubscriberClosedGenerationFailed)
}

func (effects *UnifiedDevPaneEffects) reapFaultedUnitWithReason(unit *unifiedDevUnit, reason proto.SubscriberCloseReason) {
	unit.reapOnce.Do(func() { defer unit.memory.done(); effects.reapFaultedUnitOnceWithReason(unit, reason) })
}

func (effects *UnifiedDevPaneEffects) reapFaultedUnitOnce(unit *unifiedDevUnit) {
	effects.reapFaultedUnitOnceWithReason(unit, proto.SubscriberClosedGenerationFailed)
}

func (effects *UnifiedDevPaneEffects) reapFaultedUnitOnceWithReason(unit *unifiedDevUnit, reason proto.SubscriberCloseReason) {
	if unit.process != nil && unit.process.Process != nil {
		_ = unit.process.Process.Kill()
	}
	if unit.holder != nil {
		unit.holder.mu.Lock()
		unit.holder.aborted = true
		unit.holder.clearPendingLocked()
		unit.holder.mu.Unlock()
	}
	effects.subscriberMu.Lock()
	effects.mu.Lock()
	witnesses := append([]controlmode.PaneWitness(nil), unit.witnesses...)
	if unit.birth.adoption != nil {
		delete(effects.adopting, unit.sessionID)
	}
	if effects.units[unit.sessionID] != unit {
		effects.mu.Unlock()
		effects.subscriberMu.Unlock()
		return
	}
	delete(effects.units, unit.sessionID)
	delete(effects.rotationStates, unit.sessionID)
	activeKey, active := effects.active[unit.sessionID]
	var disconnects []controlmode.PaneWitness
	if active {
		for _, witness := range witnesses {
			if journalKey(witness) == activeKey {
				disconnects = append(disconnects, witness)
				delete(effects.active, unit.sessionID)
				break
			}
		}
	} else {
		disconnects = append(disconnects, witnesses...)
	}
	for _, witness := range witnesses {
		if effects.panes[witness.Pane] == unit.holder {
			delete(effects.panes, witness.Pane)
		}
		delete(effects.publishedSequence, journalKey(witness))
	}
	if active {
		effects.closeSubscribersLocked(activeKey, reason)
	}
	effects.mu.Unlock()
	effects.subscriberMu.Unlock()
	if edge := effects.unitReapEdge; edge != nil {
		edge(unit, append([]controlmode.PaneWitness(nil), witnesses...))
	}
	for _, witness := range disconnects {
		if effects.observer != nil {
			_ = effects.observer.ObservePane(controlmode.Observation{Kind: controlmode.ObservationDisconnect, Witness: witness})
		}
	}
}

const (
	terminalRetirementRetryInitial = 10 * time.Millisecond
	terminalRetirementRetryMaximum = time.Second
)

func (effects *UnifiedDevPaneEffects) tmuxSessionPresence(session string) (present, authoritative bool) {
	effects.mu.Lock()
	presence := effects.sessionPresence
	server := effects.server
	effects.mu.Unlock()
	if presence != nil {
		return presence(server, session)
	}
	rows, err := tmuxOutput(server, "list-sessions", "-F", "#{session_id}")
	if err != nil {
		return false, false
	}
	for _, row := range strings.Fields(rows) {
		if row == session {
			return true, true
		}
	}
	return false, true
}

func (effects *UnifiedDevPaneEffects) queueTerminalRetirement(ctx context.Context, unit *unifiedDevUnit, witness controlmode.PaneWitness) {
	key := journalKey(witness)
	effects.mu.Lock()
	if effects.observerStopped || ctx.Err() != nil {
		effects.mu.Unlock()
		return
	}
	if _, pending := effects.terminalRetires[key]; pending {
		effects.mu.Unlock()
		return
	}
	if effects.terminalRetires == nil {
		effects.terminalRetires = make(map[unifiedjournal.PaneKey]*terminalRetirement)
	}
	state := &terminalRetirement{unit: unit, witness: witness, backoff: terminalRetirementRetryInitial}
	unit.memory.hold()
	effects.terminalRetires[key] = state
	effects.mu.Unlock()
	effects.attemptTerminalRetirement(ctx, key)
}

func (effects *UnifiedDevPaneEffects) attemptTerminalRetirement(ctx context.Context, key unifiedjournal.PaneKey) {
	effects.mu.Lock()
	state := effects.terminalRetires[key]
	if state != nil && !state.running {
		state.running = true
	} else {
		state = nil
	}
	effects.mu.Unlock()
	if state == nil {
		return
	}
	if registry, ok := effects.observer.(*paneRegistry); ok {
		registry.retention.callHook("before_disconnect_reserve", key)
	}
	if effects.settleOwnerGone(state.unit, state.witness) == nil {
		effects.mu.Lock()
		delete(effects.terminalRetires, key)
		effects.mu.Unlock()
		state.unit.memory.done()
		return
	}
	select {
	case <-ctx.Done():
		effects.mu.Lock()
		delete(effects.terminalRetires, key)
		effects.mu.Unlock()
		state.unit.memory.done()
		return
	default:
	}
	effects.mu.Lock()
	state = effects.terminalRetires[key]
	if state == nil {
		effects.mu.Unlock()
		return
	}
	state.running = false
	if state.stopped {
		delete(effects.terminalRetires, key)
		effects.mu.Unlock()
		state.unit.memory.done()
		return
	}
	delay := state.backoff
	state.backoff *= 2
	if state.backoff > terminalRetirementRetryMaximum {
		state.backoff = terminalRetirementRetryMaximum
	}
	after := effects.rotationAfter
	if after == nil {
		after = func(delay time.Duration, fn func()) func() bool {
			timer := time.AfterFunc(delay, fn)
			return timer.Stop
		}
	}
	state.cancel = after(delay, func() {
		effects.mu.Lock()
		if effects.terminalRetires[key] == state && !state.stopped {
			state.cancel = nil
			state.retryReady = true
		}
		effects.mu.Unlock()
	})
	effects.mu.Unlock()
}

func (effects *UnifiedDevPaneEffects) stopTerminalRetirements() {
	effects.mu.Lock()
	effects.observerStopped = true
	for key, state := range effects.terminalRetires {
		if state.cancel != nil {
			state.cancel()
		}
		state.stopped = true
		if !state.running {
			delete(effects.terminalRetires, key)
			state.unit.memory.done()
		}
	}
	// Keep the original (now canceled) run context available to late owners.
	effects.mu.Unlock()
}

func (effects *UnifiedDevPaneEffects) classifyObserverSource(ctx context.Context, witness controlmode.PaneWitness) observerSourceDecision {
	if effects.sessionPresence != nil {
		present, authoritative := effects.tmuxSessionPresence(witness.Session.Session)
		if present && authoritative {
			current := witness
			return classifyObserverRecheck(witness, &current, true, true)
		}
		return classifyObserverRecheck(witness, nil, present, authoritative)
	}
	source, err := buildSourceWitness(ctx, effects.server, "", witness.Session.Session)
	if err == nil {
		current := controlmode.PaneWitness{Session: witness.Session, Window: source.WindowID, Pane: source.PaneID, Incarnation: source.Incarnation}
		return classifyObserverRecheck(witness, &current, true, true)
	}
	present, authoritative := effects.tmuxSessionPresence(witness.Session.Session)
	return classifyObserverRecheck(witness, nil, present, authoritative)
}

// classifyRefitSource is the cleanup authority for every post-PONR refit
// failure, before and after registry/journal commit. Exact changed-width owner
// and ambiguous inventory retain both possible generations; only proved
// owner-gone or a stable-identity replacement may authorize cleanup.
func (effects *UnifiedDevPaneEffects) classifyRefitSource(ctx context.Context, before terminal.SourceWitness, witness controlmode.PaneWitness) observerSourceDecision {
	if before.Validate() != nil {
		return effects.classifyObserverSource(ctx, witness)
	}
	current, err := buildSourceWitness(ctx, effects.server, "", before.SessionID)
	if err == nil {
		if sameRefitOwner(before, current) {
			return observerSourceDecision{disposition: observerSourceTransportLost}
		}
		return observerSourceDecision{
			disposition: observerSourceReplacement,
			replacement: controlmode.PaneWitness{
				Session: witness.Session, Window: current.WindowID, Pane: current.PaneID, Incarnation: current.Incarnation,
			},
		}
	}
	present, authoritative := effects.tmuxSessionPresence(before.SessionID)
	if authoritative && !present {
		return observerSourceDecision{disposition: observerSourceOwnerGone}
	}
	return observerSourceDecision{disposition: observerSourceAmbiguous}
}

func (effects *UnifiedDevPaneEffects) settleOwnerGone(unit *unifiedDevUnit, witness controlmode.PaneWitness) error {
	key := journalKey(witness)
	effects.subscriberMu.Lock()
	effects.mu.Lock()
	if effects.units[unit.sessionID] != unit || effects.active[unit.sessionID] != key {
		effects.mu.Unlock()
		effects.subscriberMu.Unlock()
		return nil
	}
	registry, production := effects.observer.(*paneRegistry)
	var reservation *retentionReservation
	var err error
	if production {
		reservation, err = registry.beginOwnerGone(witness)
		if err != nil {
			effects.mu.Unlock()
			effects.subscriberMu.Unlock()
			return err
		}
		if reservation == nil {
			effects.mu.Unlock()
			effects.subscriberMu.Unlock()
			return nil
		}
	}
	delete(effects.units, unit.sessionID)
	delete(effects.active, unit.sessionID)
	delete(effects.adopting, unit.sessionID)
	delete(effects.rotationStates, unit.sessionID)
	delete(effects.publishedSequence, key)
	for _, current := range unit.witnesses {
		if effects.panes[current.Pane] == unit.holder {
			delete(effects.panes, current.Pane)
		}
	}
	effects.mu.Unlock()
	effects.closeSubscribersLocked(key, proto.SubscriberClosedGenerationFailed)
	effects.subscriberMu.Unlock()
	if production {
		return registry.finishOwnerGone(witness, reservation)
	}
	if effects.observer == nil {
		return errors.New("terminal owner retirement observer unavailable")
	}
	return effects.observer.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOwnerGone, Witness: witness})
}

func (effects *UnifiedDevPaneEffects) reapUnitOnce(unit *unifiedDevUnit) {
	effects.reapUnitOnceContext(effects.observerRunContext(), unit)
}

func (effects *UnifiedDevPaneEffects) reapUnitOnceContext(ctx context.Context, unit *unifiedDevUnit) {
	effects.mu.Lock()
	if unit.birth.adoption != nil {
		delete(effects.adopting, unit.sessionID)
	}
	witnesses := append([]controlmode.PaneWitness(nil), unit.witnesses...)
	unitOwned := effects.units[unit.sessionID] == unit
	activeKey, active := effects.active[unit.sessionID]
	effects.mu.Unlock()
	if !unitOwned {
		return
	}
	if edge := effects.unitReapEdge; edge != nil {
		edge(unit, append([]controlmode.PaneWitness(nil), witnesses...))
	}
	var current controlmode.PaneWitness
	for _, witness := range witnesses {
		if !active || journalKey(witness) == activeKey {
			current = witness
			break
		}
	}
	if current == (controlmode.PaneWitness{}) {
		if unit.holder != nil {
			unit.holder.mu.Lock()
			unit.holder.aborted = true
			unit.holder.clearPendingLocked()
			unit.holder.mu.Unlock()
		}
		effects.mu.Lock()
		if effects.units[unit.sessionID] == unit {
			delete(effects.units, unit.sessionID)
			delete(effects.adopting, unit.sessionID)
			delete(effects.rotationStates, unit.sessionID)
			for _, witness := range witnesses {
				if effects.panes[witness.Pane] == unit.holder {
					delete(effects.panes, witness.Pane)
				}
			}
		}
		effects.mu.Unlock()
		return
	}
	decision := effects.classifyObserverSource(ctx, current)
	if decision.disposition == observerSourceOwnerGone {
		if unit.holder != nil {
			unit.holder.mu.Lock()
			unit.holder.aborted = true
			unit.holder.clearPendingLocked()
			unit.holder.mu.Unlock()
		}
		effects.queueTerminalRetirement(ctx, unit, current)
		return
	}
	if decision.disposition != observerSourceTransportLost && unit.holder != nil {
		unit.holder.mu.Lock()
		unit.holder.aborted = true
		unit.holder.clearPendingLocked()
		unit.holder.mu.Unlock()
	}

	replacement := decision.disposition == observerSourceReplacement
	recoveryFailed := unit.birth.adoption != nil && unit.birth.adoption.recovery != nil && !unit.birth.adoption.recovery.journalCommitted
	effects.subscriberMu.Lock()
	effects.mu.Lock()
	if effects.units[unit.sessionID] != unit || effects.active[unit.sessionID] != activeKey {
		effects.mu.Unlock()
		effects.subscriberMu.Unlock()
		return
	}
	delete(effects.units, unit.sessionID)
	delete(effects.adopting, unit.sessionID)
	delete(effects.rotationStates, unit.sessionID)
	for _, witness := range witnesses {
		if effects.panes[witness.Pane] == unit.holder {
			delete(effects.panes, witness.Pane)
		}
	}
	if decision.disposition != observerSourceTransportLost || recoveryFailed {
		delete(effects.active, unit.sessionID)
		delete(effects.publishedSequence, activeKey)
	}
	effects.mu.Unlock()
	effects.subscriberMu.Unlock()
	// Replacement retirement can perform storage I/O and therefore runs with no
	// provider or subscriber lock held. It removes router/retention authority
	// before the affected attachments receive their terminal provider verdict.
	if replacement && effects.observer != nil {
		_ = effects.observer.ObservePane(controlmode.Observation{Kind: controlmode.ObservationReplacement, Witness: current, Replacement: decision.replacement})
	}
	effects.closeSubscribers(activeKey, proto.SubscriberClosedGenerationFailed)
	if decision.disposition == observerSourceTransportLost && !recoveryFailed {
		if err := effects.startObserverRecovery(ctx, unit, activeKey); err != nil {
			effects.subscriberMu.Lock()
			effects.mu.Lock()
			if effects.active[unit.sessionID] == activeKey && effects.units[unit.sessionID] == nil {
				delete(effects.active, unit.sessionID)
				delete(effects.publishedSequence, activeKey)
			}
			effects.mu.Unlock()
			effects.subscriberMu.Unlock()
		}
	}
}
