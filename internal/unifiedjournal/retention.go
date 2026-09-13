package unifiedjournal

import "sync"

// Eight ordinary sources retain one additional slot for a rotation successor.
const defaultCompletePaneSlots int64 = 9

type retentionPolicy struct {
	maximumCompletePanes int64
	completePaneUnits    int64
	completeRealmUnits   int64
}

// retentionLedger serializes append admission in request order and retains one
// complete pane slot for every admitted pane. Ordinary admissions preserve
// the final slot for rotation. A committed slot otherwise survives failures
// and denials; releaseCompletePane returns only a provisional adoption/rotation
// slot or the predecessor side of a committed rotation swap.
type retentionLedger struct {
	mu          sync.Mutex
	ready       *sync.Cond
	next        uint64
	serving     uint64
	reserved    int64
	limit       int64
	wake        func()
	wakePending bool
}

func newRetentionLedger(options OpenOptions, occupied int) *retentionLedger {
	policy := retentionPolicy{
		maximumCompletePanes: defaultCompletePaneSlots,
		completePaneUnits:    1,
		completeRealmUnits:   defaultCompletePaneSlots,
	}
	if options.CompletePaneSlots > 0 {
		policy.maximumCompletePanes = options.CompletePaneSlots
		policy.completeRealmUnits = options.CompletePaneSlots
	}
	if options.retention != nil {
		policy = *options.retention
	}
	limit := policy.maximumCompletePanes
	if limit < 0 || policy.completePaneUnits <= 0 || policy.completeRealmUnits < 0 {
		limit = 0
	} else if completeSlots := policy.completeRealmUnits / policy.completePaneUnits; completeSlots < limit {
		limit = completeSlots
	}
	ledger := &retentionLedger{reserved: int64(occupied), limit: limit, wake: options.EligibilityWake}
	ledger.ready = sync.NewCond(&ledger.mu)
	return ledger
}

func (ledger *retentionLedger) enter() {
	ledger.mu.Lock()
	ticket := ledger.next
	ledger.next++
	for ticket != ledger.serving {
		ledger.ready.Wait()
	}
	ledger.mu.Unlock()
}

func (ledger *retentionLedger) leave() {
	ledger.mu.Lock()
	ledger.serving++
	ledger.ready.Broadcast()
	wake := ledger.wakePending
	ledger.wakePending = false
	ledger.mu.Unlock()
	if wake && ledger.wake != nil {
		ledger.wake()
	}
}

// reserveCompletePane atomically reserves one complete-pane slot. Ordinary
// births, reconstructed admissions and implicit append-created panes pass
// reserveLast=true so the final slot stays available for a rotation swap.
// Rotation alone passes false: it already owns a live predecessor slot and
// must be able to reserve the temporary successor slot that makes the swap
// transactional.
func (ledger *retentionLedger) reserveCompletePane(reserveLast bool) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	limit := ledger.limit
	if reserveLast {
		limit--
	}
	if limit < 0 || ledger.reserved >= limit {
		return ErrQuota
	}
	ledger.reserved++
	return nil
}

// releaseCompletePane returns one slot whose transaction has proved it no
// longer owns a live admission: an aborted provisional generation, or the
// predecessor half of a committed rotation swap.
func (ledger *retentionLedger) releaseCompletePane() {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.reserved > 0 {
		ledger.reserved--
		ledger.wakePending = true
	}
}

// retireCompletePane returns the slot owned by a terminal generation. Slot
// capacity and byte capacity are independent: the slot becomes reusable once
// the ordered terminal boundary has settled, even when unlink still needs a
// retry. leave publishes this slot wake after the new reservation count is
// visible. A later proof-bearing byte refund publishes its own wake.
func (ledger *retentionLedger) retireCompletePane() bool {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.reserved == 0 {
		return false
	}
	ledger.reserved--
	ledger.wakePending = true
	return true
}

// releaseProvenCapacity records a physical/logical refund proved by unlink or
// ENOENT. The coalesced wake is delivered by leave, after accounting unlocks.
func (ledger *retentionLedger) releaseProvenCapacity(released bool) {
	if !released {
		return
	}
	ledger.mu.Lock()
	ledger.wakePending = true
	ledger.mu.Unlock()
}

func (ledger *retentionLedger) availableCompletePanes() int64 {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	// Project ordinary admission headroom, not the rotation-only final slot.
	// This remains advisory UX; reserveCompletePane is the authority.
	limit := ledger.limit - 1
	if limit < 0 || ledger.reserved >= limit {
		return 0
	}
	return limit - ledger.reserved
}
