package broker

import (
	"errors"
	"sort"
	"sync"
	"time"

	"persea-terminal/internal/unifiedjournal"
)

const (
	retentionDefaultPaneLimit    = 64
	retentionDefaultCommandLimit = 32768
	// The established 10,000 tiny-event burst needs 160,000 envelopes even
	// when dispatch has not caught up. Keep boundary/cleanup headroom as well.
	retentionDefaultEnvelopeLimit = 163840
	retentionDefaultIngressLimit  = 64 << 20
	retentionDefaultOutboxLimit   = 128 << 20
)

var errGeometryPauseOwned = errors.New("geometry pause already owned for pane")

type retentionTrialOptions struct {
	realm         *unifiedjournal.Realm
	journalMu     sync.Locker
	maxBatchBytes int
	maxFeedDelay  time.Duration
	now           func() time.Time
	after         func(time.Duration, func()) func() bool
	observe       func(string, map[string]int64)
	// retire settles a terminal generation's durable journal. False retains the
	// proof-bearing tombstone and asks the runtime to retry after a bounded
	// delay; true means unlink/ENOENT and ledger settlement are complete.
	retire func(unifiedjournal.PaneKey) bool
	// committed is an O(1), non-blocking scheduling notification. It runs only
	// after the durable commit succeeds; the retention manager never performs
	// pressure accounting or rotation work on its journal-drain path.
	committed func(unifiedjournal.PaneKey)
	// eligibilityWake is an identity-free, non-blocking hint that a resource
	// which can make an armed rotation runnable was actually released.
	eligibilityWake           func()
	maxPanes                  int
	maxCommands               int
	maxEnvelopes              int
	maxIngressBytes           int64
	maxOutboxBytes            int64
	stage                     func(string, unifiedjournal.PaneKey) error
	hook                      func(string, unifiedjournal.PaneKey)
	sourceRealm, sourceServer string
	sourceLimits              recordingSourceUsage
	admittedWrites            bool
}

type retentionTrialSource interface {
	retentionTrial() retentionTrialOptions
}

type retentionComponent struct {
	arrival     time.Time
	data        []byte
	reservation *retentionReservation
	final       bool
}

type retentionPaneRuntime struct {
	key       unifiedjournal.PaneKey
	writer    *unifiedjournal.Writer
	sequencer *unifiedjournal.Sequencer
	pending   []retentionComponent
	bytes     int
	held      []retentionComponent
	heldBytes int
	// paused belongs to the manager goroutine like the rest of this struct,
	// but it is the one piece of pane state observed from outside the
	// manager under runtime.mu (the geometry owner settle path reads it), so
	// it is written only through setPaused, under runtime.mu. A bare manager
	// write racing a mu-guarded read would be a data race.
	paused    bool
	failed    bool
	discarded int64
	timerGen  uint64
	timerDue  time.Time
	timerLive bool
}

type retentionGeneration struct {
	pendingHead, pendingTail *retentionReservation
	lastProgressMono         int64
	source                   *recordingSourceAccount
	sourceUsage              recordingSourceUsage
	initial                  *recordingInitialOperation
	writer                   *unifiedjournal.Writer
	// Retirement revokes admitted before cleanup may finish. Remember that
	// this exact writer once backed an admitted lifetime, not initial staging.
	writerAdmitted   bool
	key              unifiedjournal.PaneKey
	admitted         bool
	failed           bool
	faultPending     chan struct{}
	faultOrigin      string
	cutoff           uint64
	highestPublished uint64
	cleanupQueued    bool
	cleanupSpent     bool
	retireRequested  bool
	durableRequested bool
	// abandoned identifies an unadmitted rotation successor whose transaction
	// ended while runtime work still held a reference. It preserves fault
	// isolation until the last reference settles, then ordinary retirement
	// deletes the generation and refunds its lifetime P/Q ledger.
	abandoned        bool
	lifetimeReleased bool
	refs             int
	discarded        int64
}

type retentionReservation struct {
	previous, next *retentionReservation
	acceptedMono   int64
	runtime        *retentionTrialRuntime
	generation     *retentionGeneration
	q              int
	e              int
	b              int64
	o              int64
	parts          int
	doneParts      int
	released       bool
	failed         bool
}

type retentionCommandKind uint8

const (
	retentionCommandOutput retentionCommandKind = iota + 1
	retentionCommandBoundary
	retentionCommandDiscard
	retentionCommandCleanup
	retentionCommandClose
	retentionCommandGeometry
	retentionCommandDispatchFence
	retentionCommandRejectedOutput
	retentionCommandInitial
)

type retentionCommand struct {
	initial          *recordingInitialOperation
	acceptedUnixNano int64
	kind             retentionCommandKind
	ticket           uint64
	key              unifiedjournal.PaneKey
	payload          []byte
	arrival          time.Time
	bytes            int
	reason           string
	reservation      *retentionReservation
	done             chan error
	dispatched       chan struct{}
	retire           bool
	geometry         unifiedjournal.Geometry
	// geometryReservation is the journal capacity BeginGeometry took for this
	// geometry record before tmux was issued; processGeometry consumes it.
	geometryReservation *unifiedjournal.GeometryReservation
}

type retentionFeed struct {
	key      unifiedjournal.PaneKey
	payload  []byte
	start    int64
	end      int64
	sequence int64
	geometry unifiedjournal.Geometry
	feedSeq  uint64
}

type paneRangeEffects interface {
	WritePaneRange(unifiedjournal.PaneKey, []byte, int64, int64, int64) error
}

// paneGeometryEffects delivers a committed geometry event downstream on the same
// ordered stream as output.
type paneGeometryEffects interface {
	WritePaneGeometry(unifiedjournal.PaneKey, unifiedjournal.Event) error
}

type retentionEnvelope struct {
	sequence uint64
	// Every publication is one observation, feed, or settlement fence. Store
	// captured diagnostic values here; a callback map exists only while that
	// callback runs. Feed identity/payload storage is needed only for feeds.
	event    string
	fields   []retentionField
	feed     *retentionFeed
	parts    []*retentionReservation
	aborts   []*retentionReservation
	complete chan struct{}
}

type retentionField struct {
	name  string
	value int64
}

func captureRetentionFields(fields map[string]int64) []retentionField {
	values := make([]retentionField, 0, len(fields))
	for name, value := range fields {
		values = append(values, retentionField{name, value})
	}
	return values
}

func retentionCallbackFields(values []retentionField) map[string]int64 {
	fields := make(map[string]int64, len(values))
	for _, value := range values {
		fields[value.name] = value.value
	}
	return fields
}

type retentionTimerWake struct {
	generation uint64
	ack        chan struct{}
}

type retentionTrialRuntime struct {
	pendingMu       sync.Mutex
	pendingProgress map[unifiedjournal.PaneKey]recordingPendingSample
	progress        retentionProgress
	mu              sync.Mutex
	hookMu          sync.RWMutex
	options         retentionTrialOptions
	downstream      PaneEffects
	panes           map[unifiedjournal.PaneKey]*retentionPaneRuntime
	last            *unifiedjournal.PaneKey
	started         time.Time
	lastTimeNS      int64
	closed          bool
	closing         bool
	closeErr        error

	generations map[unifiedjournal.PaneKey]*retentionGeneration
	sources     map[recordingSourceKey]*recordingSourceAccount
	// geometryOwners is the shared per-pane admission gate for explicit Fits.
	// It lives on the runtime rather than an attachment issuer so a control
	// takeover cannot enqueue pause_start2 while the displaced attachment still
	// owns pause_start1's matching pause_end.
	geometryOwners    map[unifiedjournal.PaneKey]uint64
	nextGeometryOwner uint64
	queue             retentionCommandQueue
	nextTicket        uint64
	nextEffect        uint64
	qUsed             int
	eUsed             int
	pUsed             int
	bUsed             int64
	oUsed             int64

	wake           chan struct{}
	timerWake      chan retentionTimerWake
	managerDone    chan struct{}
	outbox         chan *retentionEnvelope
	dispatcherDone chan struct{}
	closeComplete  chan struct{}
	completeOnce   sync.Once

	timerCancel     func() bool
	timerExit       chan struct{}
	timerGeneration uint64
	timerDue        time.Time
	active          []retentionComponent
	commitFault     func(unifiedjournal.PaneKey, string, error) (bool, *retentionCommand)
	durableRetires  map[unifiedjournal.PaneKey]*durableRetirementState
}

type durableRetirementState struct {
	retryReady bool
	backoff    time.Duration
	running    bool
	cancel     func() bool
}

type retentionObservedWriteAhead struct {
	runtime *retentionTrialRuntime
	pane    *retentionPaneRuntime
}

func newRetentionTrialRuntime(effects PaneEffects) *retentionTrialRuntime {
	source, ok := effects.(retentionTrialSource)
	if !ok {
		return nil
	}
	options := source.retentionTrial()
	if options.realm == nil || options.maxFeedDelay != 16*time.Millisecond ||
		(options.maxBatchBytes != 64<<10 && options.maxBatchBytes != 256<<10) ||
		options.now == nil || options.after == nil || options.observe == nil {
		return nil
	}
	if options.maxPanes == 0 {
		options.maxPanes = retentionDefaultPaneLimit
	}
	if options.maxCommands == 0 {
		options.maxCommands = retentionDefaultCommandLimit
	}
	if options.maxEnvelopes == 0 {
		options.maxEnvelopes = retentionDefaultEnvelopeLimit
	}
	if options.maxIngressBytes == 0 {
		options.maxIngressBytes = retentionDefaultIngressLimit
	}
	if options.maxOutboxBytes == 0 {
		options.maxOutboxBytes = retentionDefaultOutboxLimit
	}
	if options.maxPanes < 1 || options.maxCommands < 2*options.maxPanes || options.maxEnvelopes < 1 ||
		options.maxIngressBytes < int64(options.maxBatchBytes) || options.maxOutboxBytes < int64(options.maxBatchBytes) {
		return nil
	}
	runtime := &retentionTrialRuntime{
		options: options, downstream: effects,
		panes:          make(map[unifiedjournal.PaneKey]*retentionPaneRuntime),
		generations:    make(map[unifiedjournal.PaneKey]*retentionGeneration),
		durableRetires: make(map[unifiedjournal.PaneKey]*durableRetirementState),
		geometryOwners: make(map[unifiedjournal.PaneKey]uint64),
		started:        options.now(), wake: make(chan struct{}, 1), timerWake: make(chan retentionTimerWake, 1),
		managerDone: make(chan struct{}), dispatcherDone: make(chan struct{}), closeComplete: make(chan struct{}),
		outbox: make(chan *retentionEnvelope, options.maxEnvelopes+options.maxPanes+2),
		queue:  newRetentionCommandQueue(options.maxCommands, options.maxPanes),
	}
	go runtime.dispatchLoop()
	go runtime.managerLoop()
	return runtime
}

func (runtime *retentionTrialRuntime) acquireGeometryOwner(key unifiedjournal.PaneKey) (uint64, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closing || runtime.closed {
		return 0, unifiedjournal.ErrInvalidated
	}
	if runtime.geometryOwners[key] != 0 {
		return 0, errGeometryPauseOwned
	}
	runtime.nextGeometryOwner++
	if runtime.nextGeometryOwner == 0 {
		runtime.nextGeometryOwner++
	}
	owner := runtime.nextGeometryOwner
	runtime.geometryOwners[key] = owner
	return owner, nil
}

func (runtime *retentionTrialRuntime) releaseGeometryOwner(key unifiedjournal.PaneKey, owner uint64) {
	if owner == 0 {
		return
	}
	released := false
	runtime.mu.Lock()
	if runtime.geometryOwners[key] == owner {
		delete(runtime.geometryOwners, key)
		released = true
	}
	runtime.mu.Unlock()
	if released && runtime.options.eligibilityWake != nil {
		runtime.options.eligibilityWake()
	}
}

func (runtime *retentionTrialRuntime) callHook(point string, key unifiedjournal.PaneKey) {
	runtime.hookMu.RLock()
	hook := runtime.options.hook
	runtime.hookMu.RUnlock()
	if hook != nil {
		hook(point, key)
	}
}

func (runtime *retentionTrialRuntime) setHook(hook func(string, unifiedjournal.PaneKey)) {
	runtime.hookMu.Lock()
	runtime.options.hook = hook
	runtime.hookMu.Unlock()
}

func retentionEnvelopeNeed(bytes, batch int) int {
	k := 1
	if bytes > 0 {
		k = (bytes+batch-1)/batch + 1
	}
	return 2 + 7*k
}

func (runtime *retentionTrialRuntime) ensureGeneration(key unifiedjournal.PaneKey, admitted bool) (*retentionGeneration, bool, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closing || runtime.closed {
		runtime.progress.rejected[recordingRejectClosed].Add(1)
		return nil, false, unifiedjournal.ErrInvalidated
	}
	if generation := runtime.generations[key]; generation != nil {
		// An abandoned or retiring generation remains in the table only to
		// isolate its already-admitted work until the last reference settles.
		// It is never a staging authority, even when the caller is not asking to
		// mark it admitted yet.
		if generation.abandoned || generation.retireRequested {
			return nil, false, unifiedjournal.ErrInvalidated
		}
		if admitted {
			if _, err := runtime.markAdmittedLocked(key, generation); err != nil {
				return nil, false, err
			}
		}
		return generation, false, nil
	}
	if runtime.pUsed >= runtime.options.maxPanes || runtime.qUsed+2 > runtime.options.maxCommands {
		if runtime.pUsed >= runtime.options.maxPanes {
			runtime.progress.rejected[recordingRejectGenerations].Add(1)
		} else {
			runtime.progress.rejected[recordingRejectCommands].Add(1)
		}
		return nil, false, unifiedjournal.ErrInvalidated
	}
	generation := &retentionGeneration{key: key, admitted: admitted}
	runtime.generations[key] = generation
	runtime.pUsed++
	runtime.qUsed += 2
	runtime.sourceChargeLocked(generation, 2, 0, 0, 0)
	runtime.publishAccountingLocked()
	return generation, true, nil
}

// generationAdmitted reports whether a registry admission is still backed by
// exact live runtime authority. Callers use it only while holding registry.mu,
// preserving registry -> retention order.
func (runtime *retentionTrialRuntime) generationAdmitted(key unifiedjournal.PaneKey) bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	generation := runtime.generations[key]
	return generation != nil && generation.admitted && !generation.abandoned && !generation.retireRequested && !generation.failed
}

// generationTerminalAdmitted reports an already-classified admitted lifetime.
// It is not ingress authority: callers use it only to preserve mandatory
// content-free fault observations and idempotent control completion without
// acquiring a reference or spending P/Q/E/B/O.
func (runtime *retentionTrialRuntime) generationTerminalAdmitted(key unifiedjournal.PaneKey) bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	generation := runtime.generations[key]
	return generation != nil && generation.admitted && !generation.abandoned && (generation.retireRequested || generation.failed)
}

func (runtime *retentionTrialRuntime) publishRejectedOutput(bytes int) {
	if bytes <= 0 {
		return
	}
	runtime.callHook("before_rejected_output_admission", unifiedjournal.PaneKey{})
	// This branch has no reservation to settle, but it is still an outbox
	// producer. Admit the observation through the manager so Close winning the
	// runtime.mu race owns teardown and no observer goroutine can send after the
	// manager closes the outbox.
	_ = runtime.enqueue(&retentionCommand{kind: retentionCommandRejectedOutput, bytes: bytes})
}

func (runtime *retentionTrialRuntime) releaseUnadmitted(key unifiedjournal.PaneKey, created bool) {
	if !created {
		return
	}
	runtime.mu.Lock()
	if generation := runtime.generations[key]; generation != nil && !generation.admitted && generation.refs == 0 {
		delete(runtime.generations, key)
		runtime.pUsed--
		runtime.qUsed -= 2
		runtime.sourceChargeLocked(generation, -2, 0, 0, 0)
		runtime.releaseSourceLocked(generation)
		runtime.publishAccountingLocked()
	}
	runtime.mu.Unlock()
}

// settleRotationAbort atomically proves that the predecessor remains the
// exact live, abortable generation captured by BeginPaneRotation and, only
// then, abandons the transaction-owned successor. The caller holds
// paneRegistry.mu, preserving registry -> retention lock order. A processed
// retiring seal changes predecessor state before its completion is observed;
// therefore the state test, not the driver's later markSealed call, owns PONR.
func (runtime *retentionTrialRuntime) settleRotationAbort(oldKey unifiedjournal.PaneKey, predecessor *retentionGeneration, newKey unifiedjournal.PaneKey, successor *retentionGeneration, created bool) bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	live := runtime.generations[oldKey]
	if live != predecessor || !predecessor.admitted || predecessor.retireRequested || predecessor.failed {
		return false
	}
	if created {
		generation := runtime.generations[newKey]
		// A bootstrap fault may already have retired the exact provisional
		// successor. Absence is settled cleanup, not drift; a different or
		// admitted generation is not transaction-owned and cannot be abandoned.
		if generation == nil {
			return true
		}
		if generation != successor || generation.admitted {
			return false
		}
		generation.abandoned = true
		generation.retireRequested = true
		runtime.tryRetireLocked(generation)
	}
	return true
}

// markAdmitted is the checked transition from provisional runtime authority to
// a registry-visible admission. An abandoned generation remains in the map
// while its last reference drains, but it is permanently ineligible for
// re-admission during that interval.
func (runtime *retentionTrialRuntime) markAdmitted(key unifiedjournal.PaneKey) (bool, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.markAdmittedLocked(key, nil)
}

// markAdmittedLocked returns whether this call changed the admission bit.
// When expected is non-nil the exact provisional generation must still own
// the key. Caller holds runtime.mu.
func (runtime *retentionTrialRuntime) markAdmittedLocked(key unifiedjournal.PaneKey, expected *retentionGeneration) (bool, error) {
	generation := runtime.generations[key]
	if generation == nil || expected != nil && generation != expected || generation.abandoned || generation.retireRequested || generation.failed {
		return false, unifiedjournal.ErrInvalidated
	}
	if generation.admitted {
		return false, nil
	}
	generation.admitted = true
	if generation.writer != nil {
		generation.writerAdmitted = true
	}
	return true, nil
}

// restoreUnadmitted rolls back only an admission bit installed by the caller
// before a later router admission failed. It never changes pre-existing
// admitted authority.
func (runtime *retentionTrialRuntime) restoreUnadmitted(key unifiedjournal.PaneKey, changed bool) {
	if !changed {
		return
	}
	runtime.mu.Lock()
	if generation := runtime.generations[key]; generation != nil {
		generation.admitted = false
		generation.writerAdmitted = false
		runtime.tryRetireLocked(generation)
	}
	runtime.mu.Unlock()
}

func (runtime *retentionTrialRuntime) reserve(key unifiedjournal.PaneKey, bytes int) (*retentionReservation, error) {
	runtime.callHook("before_reserve", key)
	return runtime.reserveWithAdmission(key, bytes, false, false)
}

// A content-free boundary belongs to an existing generation. In particular,
// cleanup may retire its owner after a caller's authority check; the boundary
// must not recreate that key with a new lifetime or without its source binding.
func (runtime *retentionTrialRuntime) reserveExisting(key unifiedjournal.PaneKey, bytes int) (*retentionReservation, error) {
	runtime.callHook("before_reserve", key)
	return runtime.reserveWithAdmission(key, bytes, false, true)
}

// reserveAdmitted reserves against an existing admitted generation without
// ever materializing an absent key. Callers pair it with their own authority
// lock; the admission predicate and reservation are one retention-lock step.
func (runtime *retentionTrialRuntime) reserveAdmitted(key unifiedjournal.PaneKey, bytes int) (*retentionReservation, error) {
	return runtime.reserveWithAdmission(key, bytes, true, true)
}

func (runtime *retentionTrialRuntime) reserveWithAdmission(key unifiedjournal.PaneKey, bytes int, requireAdmitted, requireExisting bool) (*retentionReservation, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closing || runtime.closed {
		runtime.progress.rejected[recordingRejectClosed].Add(1)
		return nil, unifiedjournal.ErrInvalidated
	}
	generation := runtime.generations[key]
	if requireExisting && generation == nil || requireAdmitted && (generation == nil || !generation.admitted) {
		runtime.progress.rejected[recordingRejectAuthority].Add(1)
		return nil, unifiedjournal.ErrInvalidated
	}
	created := false
	if generation == nil {
		if runtime.pUsed >= runtime.options.maxPanes || runtime.qUsed+2 > runtime.options.maxCommands {
			if runtime.pUsed >= runtime.options.maxPanes {
				runtime.progress.rejected[recordingRejectGenerations].Add(1)
			} else {
				runtime.progress.rejected[recordingRejectCommands].Add(1)
			}
			return nil, unifiedjournal.ErrInvalidated
		}
		generation = &retentionGeneration{key: key}
		runtime.generations[key] = generation
		runtime.pUsed++
		runtime.qUsed += 2
		runtime.sourceChargeLocked(generation, 2, 0, 0, 0)
		created = true
	}
	// No new work may acquire refs or P/Q/E/B/O against an abandoned or
	// retiring generation. Existing reservations are the only authority that
	// can remain while its last reference drains.
	if generation.abandoned || generation.retireRequested {
		runtime.progress.rejected[recordingRejectAuthority].Add(1)
		return nil, unifiedjournal.ErrInvalidated
	}
	e := retentionEnvelopeNeed(bytes, runtime.options.maxBatchBytes)
	n := int64(bytes)
	if runtime.options.sourceRealm != "" && n > 0 && generation.source == nil || generation.source != nil && !generation.source.usage.plus(recordingSourceUsage{1, e, n, n}).fits(runtime.sourceLimit()) {
		if created {
			delete(runtime.generations, key)
			runtime.pUsed--
			runtime.qUsed -= 2
		}
		return nil, unifiedjournal.ErrSourceQuota
	}
	if n < 0 || n > runtime.options.maxIngressBytes || n > runtime.options.maxOutboxBytes ||
		runtime.qUsed+1 > runtime.options.maxCommands || runtime.eUsed+e > runtime.options.maxEnvelopes ||
		runtime.bUsed+n > runtime.options.maxIngressBytes || runtime.oUsed+n > runtime.options.maxOutboxBytes {
		switch {
		case n < 0 || n > runtime.options.maxIngressBytes || runtime.bUsed+n > runtime.options.maxIngressBytes:
			runtime.progress.rejected[recordingRejectIngress].Add(1)
		case n > runtime.options.maxOutboxBytes || runtime.oUsed+n > runtime.options.maxOutboxBytes:
			runtime.progress.rejected[recordingRejectOutbox].Add(1)
		case runtime.qUsed+1 > runtime.options.maxCommands:
			runtime.progress.rejected[recordingRejectCommands].Add(1)
		default:
			runtime.progress.rejected[recordingRejectEnvelopes].Add(1)
		}
		if created {
			delete(runtime.generations, key)
			runtime.pUsed--
			runtime.qUsed -= 2
		}
		return nil, unifiedjournal.ErrInvalidated
	}
	reservation := &retentionReservation{runtime: runtime, generation: generation, q: 1, e: e, b: n, o: n, parts: 1, failed: generation.failed}
	reservation.acceptedMono = recordingMonoNow()
	reservation.previous = generation.pendingTail
	if generation.pendingTail != nil {
		generation.pendingTail.next = reservation
	} else {
		generation.pendingHead = reservation
	}
	generation.pendingTail = reservation
	runtime.publishPendingLocked(generation)
	generation.refs++
	runtime.qUsed++
	runtime.eUsed += e
	runtime.bUsed += n
	runtime.oUsed += n
	runtime.sourceChargeLocked(generation, 1, e, n, n)
	runtime.publishAccountingLocked()
	return reservation, nil
}

func (runtime *retentionTrialRuntime) cancelReservation(reservation *retentionReservation) {
	if reservation == nil {
		return
	}
	ready := false
	runtime.mu.Lock()
	if !reservation.released {
		reservation.released = true
		reservation.unlinkPendingLocked()
		runtime.qUsed -= reservation.q
		runtime.eUsed -= reservation.e
		runtime.bUsed -= reservation.b
		runtime.oUsed -= reservation.o
		reservation.generation.refs--
		runtime.sourceChargeLocked(reservation.generation, -reservation.q, -reservation.e, -reservation.b, -reservation.o)
		ready = runtime.tryRetireLocked(reservation.generation)
		runtime.publishAccountingLocked()
	}
	runtime.mu.Unlock()
	if ready {
		runtime.retireDurable(reservation.generation.key)
	}
}

func (runtime *retentionTrialRuntime) releaseCommand(reservation *retentionReservation) {
	if reservation == nil {
		return
	}
	ready := false
	runtime.mu.Lock()
	if reservation.q != 0 {
		runtime.qUsed -= reservation.q
		runtime.sourceChargeLocked(reservation.generation, -reservation.q, 0, 0, 0)
		reservation.q = 0
		reservation.generation.lastProgressMono = recordingMonoNow()
		runtime.publishPendingLocked(reservation.generation)
		runtime.releaseCompletedReservationLocked(reservation)
		ready = runtime.tryRetireLocked(reservation.generation)
		runtime.publishAccountingLocked()
	}
	runtime.mu.Unlock()
	if ready {
		runtime.retireDurable(reservation.generation.key)
	}
}

func (runtime *retentionTrialRuntime) completePart(reservation *retentionReservation) {
	if reservation == nil {
		return
	}
	ready := false
	runtime.mu.Lock()
	if !reservation.released {
		reservation.doneParts++
		reservation.generation.lastProgressMono = recordingMonoNow()
		runtime.publishPendingLocked(reservation.generation)
		runtime.releaseCompletedReservationLocked(reservation)
		ready = runtime.tryRetireLocked(reservation.generation)
		runtime.publishAccountingLocked()
	}
	runtime.mu.Unlock()
	if ready {
		runtime.retireDurable(reservation.generation.key)
	}
}

// A command may still own the original input after a fast dispatcher settles
// its final copy. Both the manager and every published/discarded part must
// finish before any payload credit is reusable.
func (runtime *retentionTrialRuntime) releaseCompletedReservationLocked(reservation *retentionReservation) {
	if !reservation.released && reservation.q == 0 && reservation.doneParts >= reservation.parts {
		reservation.released = true
		reservation.unlinkPendingLocked()
		runtime.eUsed -= reservation.e
		runtime.bUsed -= reservation.b
		runtime.oUsed -= reservation.o
		reservation.generation.refs--
		runtime.sourceChargeLocked(reservation.generation, 0, -reservation.e, -reservation.b, -reservation.o)
	}
}

func (runtime *retentionTrialRuntime) publishAccountingLocked() {
	// No state -> outbox or telemetry callback edge: readers only load atomics.
	runtime.progress.commands.Store(int64(runtime.qUsed))
	runtime.progress.envelopes.Store(int64(runtime.eUsed))
	runtime.progress.generations.Store(int64(runtime.pUsed))
	runtime.progress.ingressBytes.Store(runtime.bUsed)
	runtime.progress.outboxBytes.Store(runtime.oUsed)
}

func (runtime *retentionTrialRuntime) tryRetireLocked(generation *retentionGeneration) bool {
	if generation == nil || generation.refs != 0 || generation.sourceUsage.commands != 2 || !generation.retireRequested {
		return false
	}
	if generation.failed && (!generation.cleanupQueued || !generation.cleanupSpent) {
		return false
	}
	// A durable retirement is not complete merely because runtime references
	// are quiescent. Keep the generation and its lifetime P/Q ledger as the
	// retry owner until Realm.RetirePane proves unlink or ENOENT. Otherwise an
	// EIO can strand a journal and its logical/physical charge after the only
	// runtime authority has already disappeared.
	if generation.durableRequested {
		return true
	}
	runtime.finishRetireLocked(generation)
	return false
}

func (runtime *retentionTrialRuntime) finishRetireLocked(generation *retentionGeneration) {
	if generation.failed && generation.writerAdmitted && !generation.abandoned && generation.cleanupSpent {
		generation.writer.SettleFailedOwner()
	}
	if !generation.lifetimeReleased {
		generation.lifetimeReleased = true
		runtime.pUsed--
		runtime.qUsed -= 2
		runtime.sourceChargeLocked(generation, -2, 0, 0, 0)
	}
	delete(runtime.generations, generation.key)
	runtime.releaseSourceLocked(generation)
	runtime.publishAccountingLocked()
}

func (runtime *retentionTrialRuntime) enqueue(command *retentionCommand) error {
	runtime.mu.Lock()
	if (runtime.closing || runtime.closed) && command.kind != retentionCommandClose && command.kind != retentionCommandCleanup {
		runtime.mu.Unlock()
		runtime.cancelReservation(command.reservation)
		return unifiedjournal.ErrInvalidated
	}
	if command.kind == retentionCommandRejectedOutput {
		if last := runtime.queue.back(); last.controlRun() {
			last.bytes += command.bytes
			runtime.mu.Unlock()
			return nil
		}
	}
	if !runtime.pushCommandLocked(command) {
		panic("retention: command capacity invariant violated")
	}
	runtime.mu.Unlock()
	runtime.signalManager()
	return nil
}

func (runtime *retentionTrialRuntime) signalManager() {
	select {
	case runtime.wake <- struct{}{}:
	default:
	}
}

func (runtime *retentionTrialRuntime) WritePane(key unifiedjournal.PaneKey, payload []byte) error {
	reservation, err := runtime.reserve(key, len(payload))
	if err != nil {
		return err
	}
	return runtime.submitOutput(reservation, key, payload)
}

func (runtime *retentionTrialRuntime) submitOutput(reservation *retentionReservation, key unifiedjournal.PaneKey, payload []byte) error {
	if reservation.failed {
		return runtime.enqueue(&retentionCommand{kind: retentionCommandDiscard, key: key, bytes: len(payload), reason: "discarded_after_fault", reservation: reservation})
	}
	return runtime.enqueue(&retentionCommand{kind: retentionCommandOutput, key: key, payload: payload, arrival: runtime.options.now(), reservation: reservation})
}

func (runtime *retentionTrialRuntime) Boundary(key unifiedjournal.PaneKey, reason string) error {
	reservation, err := runtime.reserveExisting(key, 0)
	if err != nil {
		// Faulted admitted generations have already consumed their funded
		// cleanup. Later content-free control boundaries are idempotent; they
		// must not recreate the generation or acquire any bounded resource.
		if errors.Is(err, unifiedjournal.ErrInvalidated) && runtime.generationTerminalAdmitted(key) {
			return nil
		}
		return err
	}
	done, err := runtime.submitBoundary(reservation, key, reason, false)
	if err != nil {
		return err
	}
	return <-done
}

func (runtime *retentionTrialRuntime) startBoundary(key unifiedjournal.PaneKey, reason string, retire bool) (<-chan error, error) {
	reservation, err := runtime.reserveExisting(key, 0)
	if err != nil {
		return nil, err
	}
	return runtime.submitBoundary(reservation, key, reason, retire)
}

func (runtime *retentionTrialRuntime) submitBoundary(reservation *retentionReservation, key unifiedjournal.PaneKey, reason string, retire bool) (<-chan error, error) {
	done := make(chan error, 1)
	if err := runtime.enqueue(&retentionCommand{kind: retentionCommandBoundary, key: key, reason: reason, reservation: reservation, done: done, retire: retire}); err != nil {
		return nil, err
	}
	return done, nil
}

// startDispatchFence completes after all previously accepted effects. Adjacent
// fences and keyless rejection diagnostics share one pending control run. A
// fence may also cover later diagnostics in that run, but never crosses an
// accepted ordinary command. No waiter list or per-request outbox slot exists.
// Closing is the only refusal: the reserved queue capacity includes one run
// between each funded command and at both ends, even at ordinary saturation.
func (runtime *retentionTrialRuntime) startDispatchFence() (<-chan struct{}, error) {
	runtime.mu.Lock()
	if runtime.closing || runtime.closed {
		runtime.mu.Unlock()
		return nil, unifiedjournal.ErrInvalidated
	}
	if last := runtime.queue.back(); last.controlRun() {
		if last.dispatched == nil {
			last.dispatched = make(chan struct{})
		}
		dispatched := last.dispatched
		runtime.mu.Unlock()
		return dispatched, nil
	}
	dispatched := make(chan struct{})
	if !runtime.pushCommandLocked(&retentionCommand{kind: retentionCommandDispatchFence, dispatched: dispatched}) {
		panic("retention: reserved fence capacity invariant violated")
	}
	runtime.mu.Unlock()
	runtime.signalManager()
	return dispatched, nil
}

func (runtime *retentionTrialRuntime) Discard(key unifiedjournal.PaneKey, bytes int, reason string) error {
	reservation, err := runtime.reserve(key, 0)
	if err != nil {
		return err
	}
	return runtime.submitDiscard(reservation, key, bytes, reason)
}

func (runtime *retentionTrialRuntime) submitDiscard(reservation *retentionReservation, key unifiedjournal.PaneKey, bytes int, reason string) error {
	return runtime.enqueue(&retentionCommand{kind: retentionCommandDiscard, key: key, bytes: bytes, reason: reason, reservation: reservation})
}

// rejectReservation publishes required post-cutoff discard control without
// leaving the ephemeral runtime generation behind. Sticky ineligibility is
// owned by the registry coordinate record, not this runtime generation.
func (runtime *retentionTrialRuntime) rejectReservation(reservation *retentionReservation, key unifiedjournal.PaneKey, bytes int) {
	if bytes != 0 {
		if err := runtime.enqueue(&retentionCommand{
			kind: retentionCommandDiscard, key: key, bytes: bytes, reason: "discarded_after_fault",
			reservation: reservation, retire: true,
		}); err == nil {
			return
		}
	}
	runtime.mu.Lock()
	if reservation != nil && reservation.generation != nil {
		reservation.generation.admitted = false
		reservation.generation.retireRequested = true
	}
	runtime.mu.Unlock()
	runtime.cancelReservation(reservation)
}

func (runtime *retentionTrialRuntime) Close() error {
	if runtime == nil {
		return nil
	}
	runtime.mu.Lock()
	if runtime.closed {
		done := runtime.closeComplete
		runtime.mu.Unlock()
		<-done
		return runtime.closeErr
	}
	if !runtime.closing {
		runtime.closing = true
		if !runtime.pushCommandLocked(&retentionCommand{kind: retentionCommandClose}) {
			panic("retention: exhausted reserved close capacity")
		}
	}
	done := runtime.closeComplete
	runtime.mu.Unlock()
	runtime.signalManager()
	<-runtime.managerDone
	<-runtime.dispatcherDone
	runtime.mu.Lock()
	runtime.closed = true
	err := runtime.closeErr
	runtime.mu.Unlock()
	runtime.completeOnce.Do(func() { close(runtime.closeComplete) })
	<-done
	return err
}

func (runtime *retentionTrialRuntime) managerLoop() {
	defer close(runtime.managerDone)
	runtime.publishObservation("reconnect", map[string]int64{"queue_depth": 0, "queue_bytes": 0}, nil)
	for {
		runtime.processDurableRetries()
		for {
			command := runtime.popCommand()
			if command == nil {
				break
			}
			if command.kind == retentionCommandClose {
				if runtime.processClose() {
					close(runtime.outbox)
					return
				}
				continue
			}
			runtime.processCommand(command)
		}
		runtime.rearmTimer()
		select {
		case <-runtime.wake:
		case wake := <-runtime.timerWake:
			if wake.generation == runtime.timerGeneration {
				runtime.timerCancel = nil
				runtime.timerExit = nil
				runtime.timerDue = time.Time{}
				runtime.processDueTimers()
			}
			close(wake.ack)
		}
	}
}

func (runtime *retentionTrialRuntime) popCommand() *retentionCommand {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.queue.len() == 0 {
		return nil
	}
	command := runtime.queue.pop()
	runtime.progress.dequeued.Add(1)
	if runtime.queue.len() == 0 {
		runtime.progress.oldestQueued.Store(0)
	} else {
		runtime.progress.oldestQueued.Store(runtime.queue.front().acceptedUnixNano)
	}
	return command
}

func (runtime *retentionTrialRuntime) processCommand(command *retentionCommand) {
	started := runtime.progress.manager.begin()
	runtime.progress.activeAccepted.Store(command.acceptedUnixNano)
	defer func() {
		runtime.progress.manager.finish(started, false)
		runtime.progress.activeAccepted.Store(0)
	}()
	switch command.kind {
	case retentionCommandOutput:
		runtime.processOutput(command)
	case retentionCommandBoundary:
		runtime.processBoundary(command)
	case retentionCommandDiscard:
		runtime.processDiscard(command)
	case retentionCommandCleanup:
		runtime.processCleanup(command)
	case retentionCommandGeometry:
		runtime.processGeometry(command)
	case retentionCommandDispatchFence, retentionCommandRejectedOutput:
		if command.bytes != 0 {
			runtime.callHook("before_rejected_output_publish", unifiedjournal.PaneKey{})
			runtime.publishAbortObservation("discarded_after_fault", map[string]int64{"discarded_bytes": int64(command.bytes)}, nil)
		}
		if command.dispatched != nil {
			runtime.callHook("before_dispatch_fence_publish", unifiedjournal.PaneKey{})
			runtime.publishEnvelope(&retentionEnvelope{complete: command.dispatched})
		}
	case retentionCommandInitial:
		runtime.processInitial(command)
	}
	runtime.releaseCommand(command.reservation)
}

// setPaused is the only writer of pane.paused. It runs on the manager, which
// never holds runtime.mu while processing a command, and publishes the write
// under runtime.mu so an observer that reads the flag under the same mutex
// from another goroutine is ordered after it.
func (runtime *retentionTrialRuntime) setPaused(pane *retentionPaneRuntime, paused bool) {
	runtime.mu.Lock()
	pane.paused = paused
	runtime.mu.Unlock()
}

func (runtime *retentionTrialRuntime) pane(key unifiedjournal.PaneKey) *retentionPaneRuntime {
	if pane := runtime.panes[key]; pane != nil {
		return pane
	}
	pane := &retentionPaneRuntime{key: key}
	pane.sequencer = unifiedjournal.NewSequencer(
		retentionObservedWriteAhead{runtime: runtime, pane: pane},
		func(key unifiedjournal.PaneKey, record unifiedjournal.Record, payload []byte) error {
			return runtime.publishFeed(pane, key, payload, record.Start, record.End, record.Sequence)
		},
	)
	runtime.panes[key] = pane
	return pane
}

func (runtime *retentionTrialRuntime) processOutput(command *retentionCommand) {
	runtime.processTimersThrough(command.arrival)
	if runtime.last != nil && *runtime.last != command.key {
		if previous := runtime.panes[*runtime.last]; previous != nil {
			_ = runtime.flushPane(previous, "pane_change")
		}
	}
	keyCopy := command.key
	runtime.last = &keyCopy
	pane := runtime.pane(command.key)
	runtime.publishObservation("decode_arrival", map[string]int64{
		"component_bytes": int64(len(command.payload)), "queue_depth": int64(len(pane.pending)), "queue_bytes": int64(pane.bytes),
	}, nil)
	if runtime.generationFailed(command.key) {
		pane.discarded += int64(len(command.payload))
		runtime.publishObservation("discarded_after_fault", map[string]int64{"discarded_bytes": pane.discarded}, []*retentionReservation{command.reservation})
		return
	}
	runtime.enqueueComponent(pane, command.payload, command.arrival, command.reservation)
}

func (runtime *retentionTrialRuntime) processTimersThrough(at time.Time) {
	keys := make([]unifiedjournal.PaneKey, 0)
	for key, pane := range runtime.panes {
		if pane.timerLive && !pane.timerDue.After(at) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return retentionKeyLess(keys[i], keys[j]) })
	for _, key := range keys {
		pane := runtime.panes[key]
		pane.timerLive = false
		_ = runtime.flushPane(pane, "timer")
	}
}

func (runtime *retentionTrialRuntime) enqueueComponent(pane *retentionPaneRuntime, payload []byte, arrival time.Time, reservation *retentionReservation) {
	if len(payload) == 0 {
		runtime.publishObservation("feed_complete", map[string]int64{"batch_bytes": 0}, []*retentionReservation{reservation})
		return
	}
	parts := (pane.bytes + len(payload) + runtime.options.maxBatchBytes - 1) / runtime.options.maxBatchBytes
	if parts < 1 {
		parts = 1
	}
	runtime.mu.Lock()
	reservation.parts = parts
	runtime.mu.Unlock()
	data := payload
	part := 0
	for len(data) != 0 {
		if pane.bytes == 0 && !pane.paused {
			pane.timerGen++
			pane.timerDue = arrival.Add(runtime.options.maxFeedDelay)
			pane.timerLive = true
		}
		available := runtime.options.maxBatchBytes - pane.bytes
		take := len(data)
		if take > available {
			take = available
		}
		part++
		piece := retentionComponent{
			arrival: arrival, data: data[:take], reservation: reservation, final: part == parts,
		}
		if pane.paused {
			pane.held = append(pane.held, piece)
			pane.heldBytes += take
			runtime.publishObservation("pause_hold", map[string]int64{"held_components": int64(len(pane.held)), "held_bytes": int64(pane.heldBytes)}, nil)
		} else {
			pane.pending = append(pane.pending, piece)
			pane.bytes += take
		}
		data = data[take:]
		if !pane.paused && pane.bytes == runtime.options.maxBatchBytes {
			if runtime.flushPane(pane, "size") != nil {
				// flushPane published an ordered abort for this component.
				// Earlier chunks may still be owned by a blocked dispatcher;
				// credit survives until every published/discarded part and this
				// manager command have actually settled.
				remaining := (len(data) + runtime.options.maxBatchBytes - 1) / runtime.options.maxBatchBytes
				data = nil
				if remaining != 0 {
					runtime.publishDiscardParts("discarded_after_fault", reservation, remaining)
				}
				return
			}
		}
	}
}

// drainHeld moves held pause components into pending under the batch-size
// ceiling, flushing at the boundary exactly like ordinary ingress. Both the
// pause_end boundary and close-time flushAll drain through here so a paused
// pane can never coalesce two accepted deliveries into one oversize feed.
func (runtime *retentionTrialRuntime) drainHeld(pane *retentionPaneRuntime, reason string) error {
	held := pane.held
	pane.held = nil
	pane.heldBytes = 0
	var err error
	for index, component := range held {
		held[index] = retentionComponent{}
		if pane.bytes != 0 && pane.bytes+len(component.data) > runtime.options.maxBatchBytes && err == nil {
			err = runtime.flushPane(pane, reason)
		}
		if err != nil {
			remaining := append([]retentionComponent{component}, held[index+1:]...)
			clear(held[index+1:])
			component = retentionComponent{}
			runtime.publishAbortObservation("discarded_after_fault", nil, remaining)
			return err
		}
		pane.pending = append(pane.pending, component)
		pane.bytes += len(component.data)
		if pane.bytes == runtime.options.maxBatchBytes && err == nil {
			err = runtime.flushPane(pane, reason)
		}
	}
	return err
}

func (runtime *retentionTrialRuntime) flushPane(pane *retentionPaneRuntime, reason string) error {
	if pane == nil || pane.bytes == 0 {
		return nil
	}
	pane.timerLive = false
	payload := make([]byte, 0, pane.bytes)
	oldest := runtime.options.now()
	components := pane.pending
	for index, component := range components {
		payload = append(payload, component.data...)
		if index == 0 || component.arrival.Before(oldest) {
			oldest = component.arrival
		}
	}
	pane.pending = nil
	pane.bytes = 0
	runtime.publishObservation("flush_start", map[string]int64{
		"reason": flushReasonCode(reason), "batch_bytes": int64(len(payload)), "batch_components": int64(len(components)),
		"oldest_delay_ns": nonnegativeDuration(runtime.options.now().Sub(oldest)), "queue_depth": 0, "queue_bytes": 0,
	}, nil)
	runtime.active = components
	err := pane.sequencer.Write(pane.key, payload)
	runtime.active = nil
	if err == nil {
		return nil
	}
	pane.failed = true
	winner, cleanup := runtime.classifyFault(pane.key, "pre_feed", err)
	stopReason := runtime.realmReason(pane.key)
	fields := map[string]int64{"batch_bytes": int64(len(payload)), "storage_reason": int64(stopReason)}
	if stopReason == unifiedjournal.ReasonQuota || stopReason == unifiedjournal.ReasonPhysicalQuota {
		fields["cap_winner"] = 1
	}
	if !winner {
		fields["authoritative"] = 0
	} else {
		fields["authoritative"] = 1
	}
	// The classified result is published before its elected cleanup. The sole
	// dispatcher may therefore invoke the result callback without allowing the
	// later cleanup record to overtake it, while this drainer remains free to
	// continue processing accepted tickets.
	runtime.publishAbortObservation("storage_fault", fields, components)
	if cleanup != nil {
		runtime.appendCleanup(cleanup)
	}
	return err
}

func (runtime *retentionTrialRuntime) publishFeed(pane *retentionPaneRuntime, key unifiedjournal.PaneKey, payload []byte, start, end, sequence int64) error {
	runtime.callHook("before_feed_publish", key)
	runtime.mu.Lock()
	generation := runtime.generations[key]
	if generation == nil || generation.failed {
		runtime.mu.Unlock()
		runtime.publishAbortObservation("discarded_after_fault", map[string]int64{"discarded_bytes": int64(len(payload))}, runtime.active)
		return nil
	}
	generation.highestPublished++
	feedSeq := generation.highestPublished
	runtime.mu.Unlock()
	parts := make([]*retentionReservation, 0, len(runtime.active))
	for _, component := range runtime.active {
		parts = append(parts, component.reservation)
	}
	runtime.publishEnvelope(&retentionEnvelope{
		feed:   &retentionFeed{key: key, payload: append([]byte(nil), payload...), start: start, end: end, sequence: sequence, feedSeq: feedSeq},
		fields: []retentionField{{"batch_bytes", int64(len(payload))}, {"committed_offset", end}},
		parts:  parts,
	})
	runtime.callHook("after_feed_publish", key)
	return nil
}

// reserveSlot takes a bounded, non-blocking command reservation ahead of time.
// The geometry barrier reserves everything it needs to finish OR fail closed
// before it holds anything, so releasing the hold can never be denied for want
// of capacity — the failure mode that would leave a pane permanently silent.
func (runtime *retentionTrialRuntime) reserveSlot(key unifiedjournal.PaneKey) (*retentionReservation, error) {
	return runtime.reserve(key, 0)
}

func (runtime *retentionTrialRuntime) releaseUnusedSlot(reservation *retentionReservation) {
	runtime.cancelReservation(reservation)
}

// submitGeometry queues a typed geometry commit against an already-taken
// reservation. It never blocks the caller on capacity and never touches the
// observer loop.
func (runtime *retentionTrialRuntime) submitGeometry(reservation *retentionReservation, journalReservation *unifiedjournal.GeometryReservation, key unifiedjournal.PaneKey, geometry unifiedjournal.Geometry) (<-chan error, error) {
	done := make(chan error, 1)
	if err := runtime.enqueue(&retentionCommand{kind: retentionCommandGeometry, key: key, geometry: geometry, reservation: reservation, geometryReservation: journalReservation, done: done}); err != nil {
		return nil, err
	}
	return done, nil
}

// processGeometry commits one typed GEOMETRY record at the current sequence and
// publishes it downstream. It runs on the manager loop, between the pause_start
// that stopped publication and the pause_end that resumes it, so the record
// lands after every byte published before it and before every byte released
// after it.
func (runtime *retentionTrialRuntime) processGeometry(command *retentionCommand) {
	pane := runtime.pane(command.key)
	err := runtime.flushPane(pane, "geometry")
	if err == nil && runtime.generationFailed(command.key) {
		err = unifiedjournal.ErrInvalidated
	}
	var record unifiedjournal.Record
	if err == nil {
		appendStarted := runtime.progress.append.begin()
		runtime.withJournalLock(func() {
			// A reserved geometry spends capacity taken before tmux was issued;
			// an unreserved one (no BeginGeometry ahead of it) admits itself.
			appendAttempted := false
			err = runtime.geometryStorageStep("append", command.key, func() error {
				appendAttempted = true
				var appendErr error
				if command.geometryReservation != nil {
					record, appendErr = runtime.options.realm.AppendReservedGeometry(command.geometryReservation, command.geometry)
				} else {
					record, appendErr = runtime.options.realm.AppendGeometry(command.key, command.geometry)
				}
				return appendErr
			})
			// The injected pre-write failure has not transferred this capacity
			// to AppendReservedGeometry, so its existing owner must release it.
			if !appendAttempted && command.geometryReservation != nil {
				command.geometryReservation.Release()
			}
			runtime.progress.append.finish(appendStarted, err != nil)
			if err == nil {
				err = runtime.measureGeometryStep("sync", &runtime.progress.sync, command.key, func() error { return runtime.options.realm.Sync(command.key) })
			}
			if err == nil {
				err = runtime.measureGeometryStep("commit", &runtime.progress.commit, command.key, func() error { return runtime.options.realm.AdvanceCommitted(command.key, record) })
			}
		})
	} else if command.geometryReservation != nil {
		runtime.withJournalLock(command.geometryReservation.Release)
	}
	if err != nil {
		// A geometry the journal did not accept must never reach the browser, and
		// the generation cannot continue pretending its durable truth is intact.
		winner, cleanup := runtime.classifyFault(command.key, "geometry", err)
		if cleanup != nil {
			runtime.appendCleanup(cleanup)
		}
		fields := map[string]int64{"columns": int64(command.geometry.Columns), "rows": int64(command.geometry.Rows)}
		if winner {
			fields["authoritative"] = 1
		}
		runtime.publishObservation("geometry_fault", fields, []*retentionReservation{command.reservation})
		if command.done != nil {
			command.done <- err
		}
		return
	}
	runtime.publishEnvelope(&retentionEnvelope{
		feed: &retentionFeed{
			key: command.key, start: record.Start, end: record.End,
			sequence: record.Sequence, geometry: command.geometry,
		},
		fields: []retentionField{{"columns", int64(command.geometry.Columns)}, {"rows", int64(command.geometry.Rows)}, {"committed_offset", record.End}},
		parts:  []*retentionReservation{command.reservation},
	})
	if command.done != nil {
		command.done <- nil
	}
}

// Geometry retains its compound journal lock. The append span starts before
// that lock; subsequent spans measure their own storage step without wrapping
// the output sequencer (which already measures its append/sync/commit calls).
func (runtime *retentionTrialRuntime) measureGeometryStep(name string, progress *recordingStageProgress, key unifiedjournal.PaneKey, operation func() error) (err error) {
	started := progress.begin()
	defer func() { progress.finish(started, err != nil) }()
	return runtime.geometryStorageStep(name, key, operation)
}

func (runtime *retentionTrialRuntime) geometryStorageStep(name string, key unifiedjournal.PaneKey, operation func() error) error {
	runtime.callHook("before_geometry_"+name, key)
	if stage := runtime.options.stage; stage != nil {
		if err := stage(name, key); err != nil {
			return err
		}
	}
	return operation()
}

func (runtime *retentionTrialRuntime) processBoundary(command *retentionCommand) {
	pane := runtime.pane(command.key)
	var err error
	switch command.reason {
	case "pause_start":
		err = runtime.flushPane(pane, command.reason)
		if err == nil {
			runtime.callHook("before_pause_start_pause", command.key)
			runtime.setPaused(pane, true)
			runtime.callHook("after_pause_start_pause", command.key)
			runtime.publishObservation("pause_start", map[string]int64{"held_bytes": int64(pane.heldBytes)}, []*retentionReservation{command.reservation})
		} else {
			runtime.publishObservation("boundary_error", map[string]int64{"reason": flushReasonCode(command.reason)}, []*retentionReservation{command.reservation})
		}
	case "pause_end":
		err = runtime.flushPane(pane, command.reason)
		runtime.callHook("before_pause_end_unpause", command.key)
		runtime.setPaused(pane, false)
		runtime.callHook("after_pause_end_unpause", command.key)
		runtime.publishObservation("pause_end", map[string]int64{"held_bytes": int64(pane.heldBytes)}, nil)
		if drainErr := runtime.drainHeld(pane, command.reason); err == nil {
			err = drainErr
		}
		if err == nil {
			err = runtime.flushPane(pane, command.reason)
		}
		runtime.publishObservation("pause_end_complete", map[string]int64{"held_bytes": 0}, []*retentionReservation{command.reservation})
	case "storage_fault", "decode_fault", "geometry_fault":
		err = runtime.flushPane(pane, command.reason)
		winner, cleanup := runtime.classifyFault(command.key, command.reason, nil)
		if cleanup != nil {
			runtime.appendCleanup(cleanup)
		}
		fields := map[string]int64{"discarded_bytes": pane.discarded}
		if winner {
			fields["authoritative"] = 1
		}
		runtime.publishObservation(command.reason, fields, []*retentionReservation{command.reservation})
	default:
		err = runtime.flushPane(pane, command.reason)
		runtime.publishObservation("boundary_complete", map[string]int64{"reason": flushReasonCode(command.reason)}, []*retentionReservation{command.reservation})
	}
	// Retirement is explicit on the command. Production incarnation changes
	// submit retire=true; a content-only boundary bearing that diagnostic
	// reason must not silently revoke later ingress.
	if command.reason == "terminal_gone" {
		runtime.mu.Lock()
		if generation := runtime.generations[command.key]; generation != nil {
			generation.durableRequested = true
		}
		runtime.mu.Unlock()
	}
	if command.reason == "terminal_handoff" || command.retire {
		runtime.requestRetire(command.key)
	}
	if command.done != nil {
		command.done <- err
	}
}

const (
	durableRetirementRetryDelay   = 100 * time.Millisecond
	durableRetirementRetryMaximum = 5 * time.Second
)

// retireDurable owns the proof-bearing retry state after runtime authority has
// been revoked. Exactly one attempt or timer exists per key. Backoff is capped,
// and close cancels every outstanding timer, so an unlink fault cannot create
// an unbounded detached timer tree or outlive its manager.
func (runtime *retentionTrialRuntime) retireDurable(key unifiedjournal.PaneKey) {
	runtime.mu.Lock()
	if runtime.closing || runtime.closed || runtime.options.retire == nil {
		runtime.mu.Unlock()
		return
	}
	state := runtime.durableRetires[key]
	if state == nil {
		if runtime.durableRetires == nil {
			runtime.durableRetires = make(map[unifiedjournal.PaneKey]*durableRetirementState)
		}
		state = &durableRetirementState{backoff: durableRetirementRetryDelay}
		runtime.durableRetires[key] = state
	}
	if state.running || state.cancel != nil {
		runtime.mu.Unlock()
		return
	}
	state.running = true
	runtime.mu.Unlock()

	settled := runtime.options.retire(key)
	runtime.mu.Lock()
	state = runtime.durableRetires[key]
	if state == nil {
		runtime.mu.Unlock()
		return
	}
	state.running = false
	if settled {
		delete(runtime.durableRetires, key)
		if generation := runtime.generations[key]; generation != nil && generation.durableRequested && generation.refs == 0 && generation.retireRequested && (!generation.failed || generation.cleanupQueued && generation.cleanupSpent) {
			generation.durableRequested = false
			runtime.finishRetireLocked(generation)
		}
		runtime.mu.Unlock()
		return
	}
	if runtime.closing || runtime.closed {
		delete(runtime.durableRetires, key)
		runtime.mu.Unlock()
		return
	}
	delay := state.backoff
	state.backoff *= 2
	if state.backoff > durableRetirementRetryMaximum {
		state.backoff = durableRetirementRetryMaximum
	}
	state.cancel = runtime.options.after(delay, func() {
		runtime.mu.Lock()
		current := runtime.durableRetires[key]
		if current != state || runtime.closing || runtime.closed {
			runtime.mu.Unlock()
			return
		}
		current.cancel = nil
		current.retryReady = true
		runtime.mu.Unlock()
		runtime.signalManager()
	})
	runtime.mu.Unlock()
}

func (runtime *retentionTrialRuntime) processDiscard(command *retentionCommand) {
	discarded := int64(command.bytes)
	if pane := runtime.panes[command.key]; pane != nil {
		pane.discarded += int64(command.bytes)
		discarded = pane.discarded
	} else {
		runtime.mu.Lock()
		if generation := runtime.generations[command.key]; generation != nil {
			generation.discarded += int64(command.bytes)
			discarded = generation.discarded
		}
		runtime.mu.Unlock()
	}
	event := command.reason
	if event == "" || event == "decode_fault" {
		event = "discarded_after_fault"
	}
	runtime.publishObservation(event, map[string]int64{"discarded_bytes": discarded}, []*retentionReservation{command.reservation})
	if command.retire {
		runtime.requestRetire(command.key)
	}
}

func (runtime *retentionTrialRuntime) classifyFault(key unifiedjournal.PaneKey, origin string, cause error) (bool, *retentionCommand) {
	runtime.mu.Lock()
	generation := runtime.generations[key]
	if generation == nil {
		generation = &retentionGeneration{key: key}
		runtime.generations[key] = generation
		runtime.qUsed += 2
		runtime.sourceChargeLocked(generation, 2, 0, 0, 0)
		runtime.publishAccountingLocked()
	}
	if generation.failed {
		runtime.mu.Unlock()
		runtime.callHook("after_fault_cas_loser", key)
		return false, nil
	}
	if generation.faultPending != nil {
		pending := generation.faultPending
		runtime.mu.Unlock()
		<-pending
		runtime.callHook("after_fault_cas_loser", key)
		return false, nil
	}
	generation.faultPending = make(chan struct{})
	runtime.mu.Unlock()

	// faultPending is intent, not authority. It prevents an already-queued
	// registry mutation from crossing the short interval before the registry
	// and runtime can commit the authoritative failure as one transaction.
	runtime.callHook("before_fault_cas", key)
	var winner bool
	var cleanup *retentionCommand
	if runtime.commitFault != nil {
		winner, cleanup = runtime.commitFault(key, origin, cause)
	} else {
		runtime.mu.Lock()
		winner, cleanup = runtime.commitFaultLocked(key, origin, cause)
		runtime.mu.Unlock()
	}
	if winner {
		runtime.callHook("after_fault_cas_winner", key)
	} else {
		runtime.callHook("after_fault_cas_loser", key)
	}
	return winner, cleanup
}

// commitFaultLocked installs runtime authority. The registry-backed path calls
// it while holding registry.mu followed by runtime.mu, then publishes either
// the exact marker or the bounded breaker before releasing either lock.
func (runtime *retentionTrialRuntime) commitFaultLocked(key unifiedjournal.PaneKey, origin string, cause error) (bool, *retentionCommand) {
	generation := runtime.generations[key]
	if generation == nil {
		generation = &retentionGeneration{key: key}
		runtime.generations[key] = generation
		runtime.qUsed += 2
		runtime.sourceChargeLocked(generation, 2, 0, 0, 0)
		runtime.publishAccountingLocked()
	}
	if generation.failed {
		return false, nil
	}
	generation.failed = true
	generation.faultOrigin = origin
	generation.cutoff = generation.highestPublished
	generation.cleanupQueued = true
	cleanup := &retentionCommand{kind: retentionCommandCleanup, key: key, reason: origin}
	if cause != nil && runtime.closeErr == nil {
		// Close reports the first recording failure. A join chain across
		// retired generations would retain unbounded lifetime history.
		runtime.closeErr = cause
	}
	if generation.faultPending != nil {
		close(generation.faultPending)
		generation.faultPending = nil
	}
	return true, cleanup
}

func (runtime *retentionTrialRuntime) pendingFault(key unifiedjournal.PaneKey) <-chan struct{} {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if generation := runtime.generations[key]; generation != nil {
		return generation.faultPending
	}
	return nil
}

func (runtime *retentionTrialRuntime) appendCleanup(command *retentionCommand) {
	runtime.mu.Lock()
	if !runtime.pushCommandLocked(command) {
		panic("retention: exhausted funded cleanup capacity")
	}
	runtime.mu.Unlock()
	runtime.signalManager()
}

func (runtime *retentionTrialRuntime) processCleanup(command *retentionCommand) {
	runtime.callHook("before_cleanup", command.key)
	pane := runtime.panes[command.key]
	if pane != nil {
		components := append([]retentionComponent(nil), pane.pending...)
		components = append(components, pane.held...)
		pane.pending = nil
		pane.held = nil
		pane.bytes = 0
		pane.heldBytes = 0
		runtime.setPaused(pane, false)
		pane.failed = true
		pane.timerLive = false
		for _, component := range components {
			pane.discarded += int64(len(component.data))
		}
		if len(components) != 0 {
			runtime.publishAbortObservation("discarded_after_fault", map[string]int64{"discarded_bytes": pane.discarded}, components)
		} else {
			runtime.publishObservation("fault_cleanup", map[string]int64{"discarded_bytes": pane.discarded}, nil)
		}
		delete(runtime.panes, command.key)
		if runtime.last != nil && *runtime.last == command.key {
			runtime.last = nil
		}
	}
	ready := false
	runtime.mu.Lock()
	if generation := runtime.generations[command.key]; generation != nil {
		generation.cleanupSpent = true
		generation.retireRequested = true
		ready = runtime.tryRetireLocked(generation)
	}
	runtime.mu.Unlock()
	if ready {
		runtime.retireDurable(command.key)
	}
	runtime.callHook("after_cleanup", command.key)
	if command.done != nil {
		command.done <- nil
	}
}

func (runtime *retentionTrialRuntime) generationFailed(key unifiedjournal.PaneKey) bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	generation := runtime.generations[key]
	return generation != nil && generation.failed
}

func (runtime *retentionTrialRuntime) requestRetire(key unifiedjournal.PaneKey) {
	if pane := runtime.panes[key]; pane != nil && len(pane.pending) == 0 && len(pane.held) == 0 && !pane.timerLive {
		delete(runtime.panes, key)
		if runtime.last != nil && *runtime.last == key {
			runtime.last = nil
		}
	}
	ready := false
	runtime.mu.Lock()
	if generation := runtime.generations[key]; generation != nil {
		generation.retireRequested = true
		generation.admitted = false
		ready = runtime.tryRetireLocked(generation)
	}
	runtime.mu.Unlock()
	if ready {
		runtime.retireDurable(key)
	}
}

func (runtime *retentionTrialRuntime) processClose() bool {
	runtime.flushAll("close")
	barrier := make(chan struct{})
	runtime.publishEnvelope(&retentionEnvelope{complete: barrier})
	<-barrier
	runtime.mu.Lock()
	if runtime.queue.len() != 0 {
		if !runtime.pushCommandLocked(&retentionCommand{kind: retentionCommandClose}) {
			panic("retention: exhausted reserved close capacity")
		}
		runtime.mu.Unlock()
		return false
	}
	runtime.mu.Unlock()
	final := make(chan struct{})
	runtime.publishEnvelope(&retentionEnvelope{
		event: "close", fields: []retentionField{{"pane_count", int64(len(runtime.panes))}},
		complete: final,
	})
	<-final
	runtime.stopTimer()
	runtime.mu.Lock()
	for key, retirement := range runtime.durableRetires {
		if retirement.cancel != nil {
			retirement.cancel()
		}
		delete(runtime.durableRetires, key)
	}
	for _, generation := range runtime.generations {
		generation.retireRequested = true
		generation.cleanupSpent = generation.cleanupSpent || !generation.failed
		generation.cleanupQueued = generation.cleanupQueued || !generation.failed
	}
	runtime.generations = make(map[unifiedjournal.PaneKey]*retentionGeneration)
	runtime.sources = make(map[recordingSourceKey]*recordingSourceAccount)
	runtime.panes = make(map[unifiedjournal.PaneKey]*retentionPaneRuntime)
	runtime.qUsed = 0
	runtime.eUsed = 0
	runtime.pUsed = 0
	runtime.bUsed = 0
	runtime.oUsed = 0
	runtime.publishAccountingLocked()
	runtime.closed = true
	runtime.last = nil
	runtime.mu.Unlock()
	return true
}

func (runtime *retentionTrialRuntime) flushAll(reason string) {
	keys := make([]unifiedjournal.PaneKey, 0, len(runtime.panes))
	for key := range runtime.panes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return retentionKeyLess(keys[i], keys[j]) })
	for _, key := range keys {
		pane := runtime.panes[key]
		if pane.paused && !pane.failed {
			runtime.setPaused(pane, false)
			if err := runtime.drainHeld(pane, reason); err != nil {
				runtime.closeErr = errors.Join(runtime.closeErr, err)
			}
		}
		if err := runtime.flushPane(pane, reason); err != nil {
			runtime.closeErr = errors.Join(runtime.closeErr, err)
		}
	}
}

func (runtime *retentionTrialRuntime) publishObservation(event string, fields map[string]int64, parts []*retentionReservation) {
	runtime.publishEnvelope(&retentionEnvelope{
		event: event, fields: captureRetentionFields(fields),
		parts: parts,
	})
}

func (runtime *retentionTrialRuntime) publishAbortObservation(event string, fields map[string]int64, components []retentionComponent) {
	aborts := make([]*retentionReservation, 0, len(components))
	for index, component := range components {
		if component.reservation == nil {
			continue
		}
		// Each occurrence is one part, including repeated parts of one
		// reservation. Discarding one cannot refund a later held part.
		aborts = append(aborts, component.reservation)
		components[index] = retentionComponent{}
	}
	runtime.publishEnvelope(&retentionEnvelope{
		event: event, fields: captureRetentionFields(fields),
		aborts: aborts,
	})
}

func (runtime *retentionTrialRuntime) publishDiscardParts(event string, reservation *retentionReservation, count int) {
	parts := make([]*retentionReservation, count)
	for index := range parts {
		parts[index] = reservation
	}
	runtime.publishEnvelope(&retentionEnvelope{
		event: event, aborts: parts,
	})
}

func (runtime *retentionTrialRuntime) publishEnvelope(envelope *retentionEnvelope) {
	runtime.mu.Lock()
	runtime.nextEffect++
	envelope.sequence = runtime.nextEffect
	runtime.mu.Unlock()
	runtime.outbox <- envelope
}

func (runtime *retentionTrialRuntime) dispatchLoop() {
	defer close(runtime.dispatcherDone)
	for envelope := range runtime.outbox {
		started := runtime.progress.dispatch.begin()
		var cleanup *retentionCommand
		failed := false
		if envelope.event != "" {
			runtime.options.observe(envelope.event, retentionCallbackFields(envelope.fields))
		}
		if envelope.feed != nil {
			cleanup, failed = runtime.dispatchFeed(*envelope.feed, retentionCallbackFields(envelope.fields))
		}
		for _, reservation := range envelope.parts {
			runtime.completePart(reservation)
		}
		for _, reservation := range envelope.aborts {
			runtime.completePart(reservation)
		}
		if cleanup != nil {
			runtime.appendCleanup(cleanup)
		}
		if envelope.complete != nil {
			close(envelope.complete)
		}
		runtime.progress.dispatch.finish(started, failed)
	}
}

func (runtime *retentionTrialRuntime) dispatchFeed(effect retentionFeed, fields map[string]int64) (*retentionCommand, bool) {
	runtime.options.observe("feed_start", fields)
	runtime.callHook("before_downstream_write", effect.key)
	var err error
	if effect.geometry != (unifiedjournal.Geometry{}) {
		if geometryEffects, ok := runtime.downstream.(paneGeometryEffects); ok {
			err = geometryEffects.WritePaneGeometry(effect.key, unifiedjournal.Event{
				Kind: unifiedjournal.RecordGeometry, Sequence: effect.sequence,
				Start: effect.start, End: effect.end, Geometry: effect.geometry,
			})
		} else {
			err = errors.New("unified geometry delivery is unavailable")
		}
	} else if ranged, ok := runtime.downstream.(paneRangeEffects); ok {
		err = ranged.WritePaneRange(effect.key, effect.payload, effect.start, effect.end, effect.sequence)
	} else {
		err = runtime.downstream.WritePane(effect.key, effect.payload)
	}
	if err == nil {
		runtime.progress.deliveredBytes.Add(uint64(len(effect.payload)))
		runtime.options.observe("feed_complete", fields)
		return nil, false
	}
	winner, cleanup := runtime.classifyFault(effect.key, "feed", err)
	result := cloneRetentionFields(fields)
	if winner {
		result["authoritative"] = 1
	} else {
		result["authoritative"] = 0
	}
	runtime.options.observe("storage_fault", result)
	return cleanup, true
}

func (runtime *retentionTrialRuntime) processDueTimers() {
	now := runtime.options.now()
	keys := make([]unifiedjournal.PaneKey, 0)
	for key, pane := range runtime.panes {
		if pane.timerLive && !pane.timerDue.After(now) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return retentionKeyLess(keys[i], keys[j]) })
	for _, key := range keys {
		pane := runtime.panes[key]
		pane.timerLive = false
		_ = runtime.flushPane(pane, "timer")
	}
}

func (runtime *retentionTrialRuntime) rearmTimer() {
	var due time.Time
	for _, pane := range runtime.panes {
		if pane.timerLive && (due.IsZero() || pane.timerDue.Before(due)) {
			due = pane.timerDue
		}
	}
	if due == runtime.timerDue {
		return
	}
	runtime.stopTimer()
	if due.IsZero() {
		return
	}
	runtime.timerGeneration++
	generation := runtime.timerGeneration
	exit := make(chan struct{})
	runtime.timerExit = exit
	runtime.timerDue = due
	delay := due.Sub(runtime.options.now())
	if delay < 0 {
		delay = 0
	}
	runtime.timerCancel = runtime.options.after(delay, func() {
		runtime.callHook("timer_callback_enter", unifiedjournal.PaneKey{})
		ack := make(chan struct{})
		runtime.timerWake <- retentionTimerWake{generation: generation, ack: ack}
		<-ack
		close(exit)
		runtime.callHook("timer_callback_exit", unifiedjournal.PaneKey{})
	})
}

func (runtime *retentionTrialRuntime) stopTimer() {
	if runtime.timerCancel == nil {
		return
	}
	cancel := runtime.timerCancel
	exit := runtime.timerExit
	runtime.timerCancel = nil
	runtime.timerExit = nil
	runtime.timerDue = time.Time{}
	if cancel() {
		return
	}
	select {
	case wake := <-runtime.timerWake:
		close(wake.ack)
		<-exit
	case <-exit:
	}
}

func (sink retentionObservedWriteAhead) Append(key unifiedjournal.PaneKey, payload []byte) (record unifiedjournal.Record, resultErr error) {
	started := sink.runtime.progress.append.begin()
	defer func() { sink.runtime.progress.append.finish(started, resultErr != nil) }()
	sink.runtime.callHook("before_append", key)
	if sink.runtime.options.stage != nil {
		if err := sink.runtime.options.stage("append", key); err != nil {
			return unifiedjournal.Record{}, err
		}
	}
	record, err := sink.runtime.realmAppend(key, payload)
	if err == nil {
		sink.runtime.publishObservation("append_write_complete", map[string]int64{"payload_bytes": int64(len(payload)), "end_offset": record.End}, nil)
	}
	return record, err
}

func (sink retentionObservedWriteAhead) Sync(key unifiedjournal.PaneKey) (resultErr error) {
	started := sink.runtime.progress.sync.begin()
	defer func() { sink.runtime.progress.sync.finish(started, resultErr != nil) }()
	sink.runtime.callHook("before_sync", key)
	if sink.runtime.options.stage != nil {
		if err := sink.runtime.options.stage("sync", key); err != nil {
			return err
		}
	}
	err := sink.runtime.realmSync(key)
	if err == nil {
		sink.runtime.publishObservation("append_sync_complete", map[string]int64{"end_offset": sink.runtime.realmEndOffset(key)}, nil)
	}
	return err
}

func (sink retentionObservedWriteAhead) AdvanceCommitted(key unifiedjournal.PaneKey, record unifiedjournal.Record) (resultErr error) {
	started := sink.runtime.progress.commit.begin()
	defer func() { sink.runtime.progress.commit.finish(started, resultErr != nil) }()
	sink.runtime.callHook("before_commit", key)
	if sink.runtime.options.stage != nil {
		if err := sink.runtime.options.stage("commit", key); err != nil {
			return err
		}
	}
	committed, err := sink.runtime.realmAdvanceCommitted(key, record)
	if err == nil {
		sink.runtime.progress.committedBytes.Add(uint64(record.End - record.Start))
		sink.runtime.publishObservation("commit_write_complete", map[string]int64{"committed_offset": committed}, nil)
		sink.runtime.publishObservation("commit_sync_complete", map[string]int64{"committed_offset": committed}, nil)
		sink.runtime.publishObservation("committed_offset", map[string]int64{"committed_offset": committed}, nil)
		if committed := sink.runtime.options.committed; committed != nil {
			committed(key)
		}
	}
	return err
}

func (runtime *retentionTrialRuntime) withJournalLock(fn func()) {
	if runtime.options.journalMu != nil {
		runtime.options.journalMu.Lock()
		defer runtime.options.journalMu.Unlock()
	}
	fn()
}

func (runtime *retentionTrialRuntime) realmAppend(key unifiedjournal.PaneKey, payload []byte) (record unifiedjournal.Record, err error) {
	runtime.withJournalLock(func() {
		if !runtime.options.admittedWrites {
			record, err = runtime.options.realm.Append(key, payload)
			return
		}
		pane := runtime.pane(key)
		if pane.writer == nil {
			pane.writer, err = runtime.options.realm.BindWriter(key)
			if err != nil {
				return
			}
			runtime.mu.Lock()
			if generation := runtime.generations[key]; generation != nil {
				generation.writer = pane.writer
				generation.writerAdmitted = generation.admitted
			}
			runtime.mu.Unlock()
		}
		record, err = pane.writer.Append(key, payload)
	})
	return record, err
}

func (runtime *retentionTrialRuntime) realmSync(key unifiedjournal.PaneKey) (err error) {
	runtime.withJournalLock(func() {
		if runtime.options.admittedWrites {
			err = runtime.pane(key).writer.Sync(key)
		} else {
			err = runtime.options.realm.Sync(key)
		}
	})
	return err
}

func (runtime *retentionTrialRuntime) realmEndOffset(key unifiedjournal.PaneKey) (offset int64) {
	runtime.withJournalLock(func() { offset = runtime.options.realm.EndOffset(key) })
	return offset
}

func (runtime *retentionTrialRuntime) realmAdvanceCommitted(key unifiedjournal.PaneKey, record unifiedjournal.Record) (committed int64, err error) {
	runtime.withJournalLock(func() {
		if runtime.options.admittedWrites {
			err = runtime.pane(key).writer.AdvanceCommitted(key, record)
		} else {
			err = runtime.options.realm.AdvanceCommitted(key, record)
		}
		if err == nil {
			committed = runtime.options.realm.CommittedOffset(key)
		}
	})
	return committed, err
}

func (runtime *retentionTrialRuntime) realmReason(key unifiedjournal.PaneKey) (reason unifiedjournal.InvalidationReason) {
	runtime.withJournalLock(func() { reason = runtime.options.realm.Reason(key) })
	return reason
}

func cloneRetentionFields(fields map[string]int64) map[string]int64 {
	copyFields := make(map[string]int64, len(fields))
	for key, value := range fields {
		copyFields[key] = value
	}
	return copyFields
}

func nonnegativeDuration(duration time.Duration) int64 {
	if duration < 0 {
		return 0
	}
	return int64(duration)
}

func flushReasonCode(reason string) int64 {
	switch reason {
	case "size":
		return 1
	case "timer":
		return 2
	case "pane_change":
		return 3
	case "incarnation_change":
		return 4
	case "pause_start":
		return 5
	case "pause_end":
		return 6
	case "reconnect":
		return 7
	case "storage_fault":
		return 8
	case "decode_fault":
		return 9
	case "explicit_flush":
		return 10
	case "cut_checkpoint":
		return 11
	case "close":
		return 12
	case "terminal_handoff":
		return 13
	case "geometry_fault":
		return 14
	case "terminal_gone":
		return 15
	default:
		return 0
	}
}

func retentionKeyLess(left, right unifiedjournal.PaneKey) bool {
	if left.Server != right.Server {
		return left.Server < right.Server
	}
	if left.Session != right.Session {
		return left.Session < right.Session
	}
	if left.ControlGeneration != right.ControlGeneration {
		return left.ControlGeneration < right.ControlGeneration
	}
	if left.Window != right.Window {
		return left.Window < right.Window
	}
	if left.Pane != right.Pane {
		return left.Pane < right.Pane
	}
	return left.Incarnation < right.Incarnation
}
