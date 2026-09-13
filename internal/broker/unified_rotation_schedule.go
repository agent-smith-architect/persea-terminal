package broker

import (
	"context"
	"errors"
	"sort"
	"time"

	"persea-terminal/internal/unifiedjournal"
)

const (
	unifiedRotationHighNumerator   int64 = 3
	unifiedRotationHighDenominator int64 = 4
	unifiedRotationLowNumerator    int64 = 3
	unifiedRotationLowDenominator  int64 = 5
	unifiedRotationCadence               = 60 * time.Second
	unifiedRotationRetryInitial          = time.Second
	unifiedRotationRetryMaximum          = 5 * time.Minute
)

type unifiedRotationPressure struct {
	logical, logicalCap   int64
	physical, physicalCap int64
}

func (pressure unifiedRotationPressure) high() bool {
	return ratioAtLeast(pressure.logical, pressure.logicalCap, unifiedRotationHighNumerator, unifiedRotationHighDenominator) ||
		ratioAtLeast(pressure.physical, pressure.physicalCap, unifiedRotationHighNumerator, unifiedRotationHighDenominator)
}

func (pressure unifiedRotationPressure) belowLow() bool {
	return ratioBelow(pressure.logical, pressure.logicalCap, unifiedRotationLowNumerator, unifiedRotationLowDenominator) &&
		ratioBelow(pressure.physical, pressure.physicalCap, unifiedRotationLowNumerator, unifiedRotationLowDenominator)
}

func ratioAtLeast(value, cap, numerator, denominator int64) bool {
	return cap > 0 && value >= 0 && value*denominator >= cap*numerator
}

func ratioBelow(value, cap, numerator, denominator int64) bool {
	return cap > 0 && value >= 0 && value*denominator < cap*numerator
}

type unifiedRotationState struct {
	key               unifiedjournal.PaneKey
	armed             bool
	armedAt           time.Time
	lastSuccess       time.Time
	nextAttempt       time.Time
	backoff           time.Duration
	deferredAltScreen bool
}

// requestRotationEvaluation is the complete retention-drain responsibility:
// make one identity-free, non-blocking wake available. The key is deliberately
// not retained: the coordinator rescans every active/armed session, so a lost
// or coalesced wake cannot strand pressure and the journal drain never waits on
// provider authority. Accounting, tmux I/O and rotation stay off this path.
func (effects *UnifiedDevPaneEffects) requestRotationEvaluation(_ unifiedjournal.PaneKey) {
	effects.wakeRotationScheduler()
}

func (effects *UnifiedDevPaneEffects) wakeRotationScheduler() {
	if effects.rotationWake == nil {
		return
	}
	select {
	case effects.rotationWake <- struct{}{}:
	default:
	}
}

// requestRotationEligibility reports an identity-free resource transition.
// Only a release owner calls it, so every armed pane may retry promptly even
// when its prior eligibility refusal had reached maximum backoff.
func (effects *UnifiedDevPaneEffects) requestRotationEligibility() {
	select {
	case effects.rotationEligibility <- struct{}{}:
	default:
	}
	effects.wakeRotationScheduler()
}

func (effects *UnifiedDevPaneEffects) rotationState(session string) *unifiedRotationState {
	effects.mu.Lock()
	defer effects.mu.Unlock()
	state := effects.rotationStates[session]
	if state == nil {
		return nil
	}
	copy := *state
	return &copy
}

func (effects *UnifiedDevPaneEffects) readRotationPressure(key unifiedjournal.PaneKey) (unifiedRotationPressure, error) {
	effects.mu.Lock()
	pressure := effects.rotationPressure
	effects.mu.Unlock()
	if pressure != nil {
		return pressure(key)
	}
	effects.pressure.mu.Lock()
	defer effects.pressure.mu.Unlock()
	return effects.pressure.panes[key], nil
}

type unifiedRotationCandidate struct {
	session string
	key     unifiedjournal.PaneKey
	state   *unifiedRotationState
}

// recordExplicitRefitSuccess applies the same completion-time cadence floor as
// an automatic rotation. It is called only after rotation_pending is durable;
// capacity-release wakes may accelerate a refusal backoff but never bypass
// this successful-rollover floor.
func (effects *UnifiedDevPaneEffects) recordExplicitRefitSuccess(session string, key unifiedjournal.PaneKey) {
	effects.mu.Lock()
	nowFn := effects.rotationNow
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	state := effects.rotationStates[session]
	if state == nil {
		state = &unifiedRotationState{}
		effects.rotationStates[session] = state
	}
	state.key = key
	state.lastSuccess = now
	state.backoff = 0
	state.nextAttempt = now.Add(unifiedRotationCadence)
	state.deferredAltScreen = false
	effects.mu.Unlock()
}

func (effects *UnifiedDevPaneEffects) rotationEvaluationSet() []unifiedRotationCandidate {
	effects.mu.Lock()
	defer effects.mu.Unlock()

	wanted := make(map[string]unifiedjournal.PaneKey, len(effects.active))
	for session, key := range effects.active {
		wanted[session] = key
	}
	for session, state := range effects.rotationStates {
		key, active := effects.active[session]
		if !active {
			delete(effects.rotationStates, session)
			continue
		}
		if state.armed {
			wanted[session] = key
		}
	}

	result := make([]unifiedRotationCandidate, 0, len(wanted))
	for session, key := range wanted {
		result = append(result, unifiedRotationCandidate{session: session, key: key})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].session < result[j].session })
	return result
}

func (effects *UnifiedDevPaneEffects) updateRotationPressure(candidate unifiedRotationCandidate, pressure unifiedRotationPressure, now time.Time) {
	effects.mu.Lock()
	defer effects.mu.Unlock()
	if current, ok := effects.active[candidate.session]; !ok || current != candidate.key {
		return
	}
	state := effects.rotationStates[candidate.session]
	if state == nil {
		state = &unifiedRotationState{}
		effects.rotationStates[candidate.session] = state
	}
	if state.key != (unifiedjournal.PaneKey{}) && state.key != candidate.key {
		state.deferredAltScreen = false
	}
	state.key = candidate.key
	if pressure.high() {
		if !state.armed {
			state.armed = true
			state.armedAt = now
			state.backoff = 0
			state.nextAttempt = now
			if cadence := state.lastSuccess.Add(unifiedRotationCadence); !state.lastSuccess.IsZero() && cadence.After(state.nextAttempt) {
				state.nextAttempt = cadence
			}
		}
		return
	}
	if state.armed && pressure.belowLow() {
		state.armed = false
		state.backoff = 0
		state.nextAttempt = time.Time{}
		state.deferredAltScreen = false
	}
}

func (effects *UnifiedDevPaneEffects) dueRotation(now time.Time) (string, bool) {
	effects.mu.Lock()
	defer effects.mu.Unlock()
	type dueState struct {
		session string
		state   *unifiedRotationState
	}
	var due []dueState
	for session, state := range effects.rotationStates {
		if !state.armed || state.nextAttempt.After(now) {
			continue
		}
		if current, ok := effects.active[session]; !ok || current != state.key {
			continue
		}
		due = append(due, dueState{session: session, state: state})
	}
	if len(due) == 0 {
		return "", false
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].state.armedAt.Equal(due[j].state.armedAt) {
			return due[i].session < due[j].session
		}
		return due[i].state.armedAt.Before(due[j].state.armedAt)
	})
	return due[0].session, true
}

// runRotationScheduler performs at most one attempt. It is called only by the
// observer supervisor, which is the realm-wide sequenced rotation owner.
func (effects *UnifiedDevPaneEffects) runRotationScheduler(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	effects.mu.Lock()
	nowFn := effects.rotationNow
	effects.mu.Unlock()
	now := nowFn()
	select {
	case <-effects.rotationEligibility:
		effects.mu.Lock()
		for _, state := range effects.rotationStates {
			eligibleAt := now
			if !state.lastSuccess.IsZero() {
				cadenceAt := state.lastSuccess.Add(unifiedRotationCadence)
				if cadenceAt.After(eligibleAt) {
					eligibleAt = cadenceAt
				}
			}
			if state.armed && state.nextAttempt.After(eligibleAt) {
				state.nextAttempt = eligibleAt
			}
		}
		effects.mu.Unlock()
	default:
	}
	for _, candidate := range effects.rotationEvaluationSet() {
		pressure, err := effects.readRotationPressure(candidate.key)
		if err != nil {
			continue
		}
		effects.updateRotationPressure(candidate, pressure, now)
	}

	session, ok := effects.dueRotation(now)
	if !ok {
		effects.armRotationTimer(now)
		return
	}
	effects.mu.Lock()
	attempt := effects.rotationAttempt
	selectedState := effects.rotationStates[session]
	if selectedState == nil {
		effects.mu.Unlock()
		return
	}
	selectedKey := selectedState.key
	effects.mu.Unlock()
	if attempt == nil {
		attempt = effects.rotateSession
	}
	if edge := effects.rotationEdge; edge != nil {
		edge(session, "trigger_selected")
	}
	err := attempt(ctx, session)
	if ctx.Err() != nil {
		return
	}
	// Cadence and retry are measured from settlement, never selection. A slow
	// composite must receive the same full quiet period as a fast one.
	completedAt := nowFn()
	if edge := effects.rotationEdge; edge != nil {
		if err == nil {
			edge(session, "trigger_succeeded")
		} else {
			edge(session, "trigger_deferred")
		}
	}

	effects.mu.Lock()
	state := effects.rotationStates[session]
	if state != nil && state == selectedState && (err == nil || state.key == selectedKey) {
		if err == nil {
			state.lastSuccess = completedAt
			state.backoff = 0
			state.nextAttempt = completedAt.Add(unifiedRotationCadence)
			state.deferredAltScreen = false
			if key, active := effects.active[session]; active {
				state.key = key
			}
		} else {
			if errors.Is(err, ErrUnifiedRotateAlternateScreen) {
				state.deferredAltScreen = true
			}
			if state.backoff == 0 {
				state.backoff = unifiedRotationRetryInitial
			} else {
				state.backoff *= 2
				if state.backoff > unifiedRotationRetryMaximum {
					state.backoff = unifiedRotationRetryMaximum
				}
			}
			state.nextAttempt = completedAt.Add(state.backoff)
		}
	}
	effects.mu.Unlock()
	// One attempt may have shared its capacity-one wake with other already-due
	// panes. Re-wake unconditionally; a lone refused pane merely arms its bounded
	// retry timer, while another due pane cannot be stranded by wake coalescing.
	effects.wakeRotationScheduler()
	effects.armRotationTimer(completedAt)
}

func (effects *UnifiedDevPaneEffects) armRotationTimer(now time.Time) {
	effects.mu.Lock()
	defer effects.mu.Unlock()
	var due time.Time
	for _, state := range effects.rotationStates {
		if !state.armed || state.nextAttempt.IsZero() || !state.nextAttempt.After(now) {
			continue
		}
		if due.IsZero() || state.nextAttempt.Before(due) {
			due = state.nextAttempt
		}
	}
	if due.Equal(effects.rotationDue) {
		return
	}
	if effects.rotationTimer != nil {
		effects.rotationTimer()
		effects.rotationTimer = nil
	}
	effects.rotationDue = due
	if due.IsZero() || effects.rotationAfter == nil {
		return
	}
	effects.rotationTimer = effects.rotationAfter(due.Sub(now), effects.wakeRotationScheduler)
}

func (effects *UnifiedDevPaneEffects) stopRotationScheduler() {
	effects.mu.Lock()
	defer effects.mu.Unlock()
	if effects.rotationTimer != nil {
		effects.rotationTimer()
		effects.rotationTimer = nil
	}
	effects.rotationDue = time.Time{}
}
