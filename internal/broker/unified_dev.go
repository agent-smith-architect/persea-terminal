package broker

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"

	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

// Adoption refusals are typed so the projection layer can map each one to a
// distinct dashboard state. Every refusal fails closed: when any of these is
// returned, no journal generation, registry admission, or provider registration
// exists for the attempt.
var (
	ErrUnifiedAdoptSessionMissing = errors.New("unified adoption target session is unavailable")
	ErrUnifiedAdoptMultiWindow    = errors.New("unified adoption target has more than one window")
	ErrUnifiedAdoptMultiPane      = errors.New("unified adoption target has more than one pane")
	ErrUnifiedAdoptSlotsExhausted = errors.New("unified adoption slots are exhausted")
	ErrUnifiedAdoptInProgress     = errors.New("unified adoption is already in progress")
	ErrUnifiedAdoptUnstable       = errors.New("unified adoption capture did not stabilize")
	ErrUnifiedAdoptHistory        = errors.New("unified adoption history is outside its bounds")
	ErrUnifiedObserverFlowControl = errors.New("unified observer flow control violated byte authority")
)

// errUnifiedAdoptDrift is the internal PRE!=POST signal: the pane mutated (or a
// future tmux stopped draining a client's command line atomically) during the
// capture composite, so the attempt's capture is discarded and retried. It is
// never returned to a caller; exhausted retries surface ErrUnifiedAdoptUnstable.
var errUnifiedAdoptDrift = errors.New("unified adoption capture drifted")

const (
	// adoptionHistoryCapRows bounds how much scrollback the capture composite
	// requests. Deeper history stays in tmux and is not reconstructed.
	adoptionHistoryCapRows = proto.AdoptionHistoryMaxRows
	// adoptionBootstrapCapBytes bounds the synthesized bootstrap; oldest
	// history rows are trimmed first, then hidden normal rows if needed.
	// The trim is surfaced to the caller.
	// The pane generation's whole lifetime is 8 MiB, so a full bootstrap
	// spends a quarter of it.
	adoptionBootstrapCapBytes = int(unifiedjournal.AdoptionBootstrapCapBytes)
	// adoptionCaptureAttempts is the initial capture plus the PRE!=POST
	// version-drift retries. Measured on tmux 3.4: the composite is atomic by
	// construction (zero %output inside the span under flood), so a retry
	// fires only if a future tmux breaks that property.
	adoptionCaptureAttempts = 4
	// adoptionCompositeBlocks is the composite's sub-command count. Measured:
	// each semicolon sub-command produces its own %begin/%end block, so the
	// block count consumed must equal the sub-command count exactly.
	adoptionCompositeBlocks = 8
)

type unifiedDevCommand struct {
	spawnDeadline time.Time // only queued founding admission; never decoder cancellation
	// A founding caller transfers this existing owner with its spawn request.
	// It covers preflight command children before the observer process exists.
	memory *recordingUnitMemory
	// Runs on the decoder owner before its response storage is released.
	consumeResponse func(string) error
	context         context.Context
	birthCapture    *unifiedDevBirth
	server          config.TmuxServer
	args            []string
	birth           *unifiedDevBirth
	// adoption marks a founding request that attaches to an EXISTING session
	// instead of creating one. It mirrors the birth field: exactly one of the
	// two is set on a founding request, and the unit goroutine owns the whole
	// adoption sequence just as it owns birth.
	adoption *unifiedDevAdoption
	// rotation is a maintenance request executed on this session's existing
	// observer unit. It owns no second decoder or control client: the unit
	// goroutine consumes the capture boundary and every surrounding pane byte.
	rotation *unifiedDevRotation
	// blocks is how many %begin/%end command blocks this command occupies on the
	// control connection. Session birth occupies one. A guarded if-shell occupies
	// two: the submission, and the deferred success command tmux queues once the
	// guard passes — proven against real tmux 3.4 in the Phase-1 boundary test.
	// The completion of the LAST block is the sequence point; the first is not,
	// because pane output classified after it can still be pre-mutation output.
	blocks int
	done   chan unifiedDevCommandResult
}

// unifiedDevAdoption carries the identity of the session an adoption unit
// attaches to. Attachment is by session ID, never name: readiness
// exact-matches the $id field of %session-changed, and name targets are both
// ambiguous ("main" prefixing "main2") and subject to the measured
// `=name` targeting gotcha.
type unifiedDevAdoption struct {
	sessionID   string
	historyRows int
	recovery    *unifiedDevRotation
	holder      *unifiedDevBirth
	witnesses   []controlmode.PaneWitness
}

type unifiedDevCommandResult struct {
	session string
	key     unifiedjournal.PaneKey
	trimmed bool
	// existing marks the idempotent adoption outcome: the session was already
	// journal-active and no new generation was created.
	existing bool
	err      error
}

type unifiedDevSubscriber struct {
	lease *recordingReaderLease
	// cursor is a sequence, not a byte offset. Geometry events carry no payload,
	// so consecutive ones share a byte offset and a byte cursor cannot order them.
	cursor int64
	data   chan unifiedjournal.Event
	done   chan struct{}
	once   sync.Once
	// closedReason is the typed outcome of the provider removing this
	// subscriber. It is written under subscriberMu strictly before data is
	// closed, so a consumer that has observed the close reads it without a
	// lock. It stays empty when the subscriber's own cancel closed data: that
	// is the attachment ending on its own terms, not a verdict on it.
	closedReason proto.SubscriberCloseReason
	// verdict is closed, after closedReason is written and strictly before
	// data is closed, when the provider removes this subscriber with a typed
	// reason. It is the one signal of that removal a consumer can observe
	// WITHOUT receiving from data — which is what the attachment's bounded
	// close path needs, because the consumer of data is the goroutine that
	// may be parked inside a downstream write to a peer that has stopped
	// reading. Nil on a subscriber that was never given one; such a
	// subscriber has no bounded path and only the tail's own close.
	verdict chan struct{}
}

// events transfers one copied event to its consumer, which must call
// releaseEvent only after its final write/use returns. Removal drains queued
// events; it cannot settle an event already received by that consumer.
func (subscriber *unifiedDevSubscriber) events() <-chan unifiedjournal.Event { return subscriber.data }

// verdictSignal is closed once the provider has removed this subscriber with
// a typed reason; closeReason is valid after it is observed. Nil (never ready)
// on a subscriber without one.
func (subscriber *unifiedDevSubscriber) verdictSignal() <-chan struct{} { return subscriber.verdict }

// closeTyped records the provider's verdict and publishes it: reason first,
// then the verdict signal, then the tail. Caller holds subscriberMu and has
// already removed the subscriber from the registry, so this runs at most once.
func (subscriber *unifiedDevSubscriber) closeTyped(reason proto.SubscriberCloseReason) {
	subscriber.closedReason = reason
	if subscriber.verdict != nil {
		close(subscriber.verdict)
	}
	close(subscriber.data)
	subscriber.drainClosed()
}

// closeReason is valid only after events() has been observed closed. Empty
// means the subscriber cancelled itself; anything else is the provider's typed
// verdict, which the attachment must carry to the browser as its close reason.
func (subscriber *unifiedDevSubscriber) closeReason() proto.SubscriberCloseReason {
	return subscriber.closedReason
}

// UnifiedDevPaneEffects is the closed, single-target development provider.
// It supervises per-session control-mode observer units and owns the journal's
// atomic snapshot/tail registration; it is never constructed when the config
// block is absent.
type UnifiedDevPaneEffects struct {
	pressure              recordingPressure
	supervised            map[*unifiedDevUnit]struct{}
	supervisionStallLimit time.Duration
	exitRecords           recordingExitRing
	lifecycle             recordingStageProgress
	copies                recordingCopyBudget
	transients            recordingTransientBudget
	readers               recordingReaderBudget
	cfg                   config.Broker
	dev                   config.UnifiedTerminalDev
	server                config.TmuxServer
	realm                 *unifiedjournal.Realm
	spawns                chan unifiedDevCommand
	exits                 chan *unifiedDevUnit

	// generationSequence backs mintControlGeneration for units that need a
	// generation no earlier unit of this broker run has stamped.
	generationSequence atomic.Uint64

	mu       sync.Mutex
	observer PaneObservationEffects
	units    map[string]*unifiedDevUnit
	panes    map[string]*unifiedDevBirth
	active   map[string]unifiedjournal.PaneKey
	adopting map[string]struct{}
	// startupRecovery is the bounded typed outcome ledger consumed before the
	// provider can admit work. It contains one path-redacted entry per scanned
	// generation and is immutable after construction.
	startupRecovery []unifiedjournal.Recovery
	startupSources  map[unifiedjournal.PaneKey]unifiedjournal.SourceID
	rotation        *unifiedDevRotation
	// refitOperations is the bounded idempotency ledger for explicit width
	// generation changes. Entries live for this provider incarnation: retaining
	// a completed token is what makes a lost-success retry observationally the
	// same operation instead of a second tmux composite. The hard ceiling fails
	// new work closed rather than evicting a token and reopening ambiguity; a
	// provider Close/recreation is the bounded cleanup boundary.
	refitOperations map[unifiedRefitOperationKey]*unifiedRefitOperation
	// rotationStates is the pressure/cadence ledger for the automatic journal
	// rotation coordinator. Durable commits emit only an identity-free wake; the
	// observer/retention drain never takes effects.mu, performs accounting, or
	// submits a tmux command. The capacity-one wake is deliberately lossy because
	// every wake rescans every active and already-armed session.
	rotationStates      map[string]*unifiedRotationState
	rotationWake        chan struct{}
	rotationEligibility chan struct{}
	rotationTimer       func() bool
	rotationDue         time.Time
	rotationNow         func() time.Time
	rotationAfter       func(time.Duration, func()) func() bool
	// rotationPressure and rotationAttempt are bounded test seams. Production
	// leaves them nil and uses the journal ledger and rotateSession.
	rotationPressure func(unifiedjournal.PaneKey) (unifiedRotationPressure, error)
	rotationAttempt  func(context.Context, string) error
	// sessionPresence is a bounded test seam for the authoritative tmux
	// inventory query used when an observer process exits. Production leaves it
	// nil and requires a successful exact-ID inventory before declaring a
	// terminal disappearance.
	sessionPresence func(config.TmuxServer, string) (present, authoritative bool)
	observerCtx     context.Context
	observerStopped bool // effects.mu; shutdown closes retirement/recovery admission
	terminalRetires map[unifiedjournal.PaneKey]*terminalRetirement
	subscribers     map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}
	// publishedSequence is the last sequence offered to subscribers for each
	// generation. It lets snapshot registration detect a publication that won
	// the gap between its lock-free storage read and its final subscriberMu
	// registration, without ever doing storage I/O under subscriberMu.
	publishedSequence map[unifiedjournal.PaneKey]int64
	journalMu         recordingJournalLock
	subscriberMu      sync.Mutex

	// adoptionSpanOutputs counts %output events classified inside an adoption
	// composite's submission span. Measured zero on tmux 3.4 (the composite is
	// atomic by construction); the counter exists so the adoption regression test can pin
	// the measurement as a regression gate.
	adoptionSpanOutputs atomic.Int64
	// adoptionRetries counts PRE!=POST capture retries, the version-drift
	// guard's trip counter. Expected ~0 in production.
	adoptionRetries atomic.Int64
	// adoptionPostTamper is a test seam: it may corrupt the POST probe text for
	// a given attempt so the retry path is exercisable without real drift.
	adoptionPostTamper func(attempt int, post string) string
	// observerReadinessEdge is a test-only scheduling seam immediately after
	// readiness and before client-flag interrogation. It lets a real second
	// tmux client deterministically race the exact production boundary without
	// replacing any product decision or data path.
	observerReadinessEdge func(session string) error
	// geometryRefusal is a test seam: it may refuse a guarded resize before any
	// mutation, the way a not-yet-journal-ready target or an exhausted command
	// budget does, so the operational (non-closing) resize_failed path is
	// exercisable against a real tmux without racing the guard.
	geometryRefusal func(session string) error
	// resizeEdge is a test seam handed to the canonical resize transaction:
	// it is invoked at each post-issue edge ("issued", "witnessed", "sized")
	// so a suite can inflict a REAL failure there against a live tmux and
	// prove the attachment ends fatally rather than surviving as operational.
	resizeEdge func(session, edge string)
	// retentionObserve is a test seam: when set, the retention runtime's
	// observation stream is mirrored to it so a suite can prove that a
	// post-mutation resize failure faulted the generation.
	retentionObserve func(event string, fields map[string]int64)
	// observerDecoded is a test-only observation point installed before the
	// observer starts; it does not alter routing or settlement decisions.
	observerDecoded func(controlmode.Event)
	// unitReapEdge is a test-only deterministic edge after provider maps are
	// cleared and before the immutable witness snapshot is disconnected.
	unitReapEdge func(*unifiedDevUnit, []controlmode.PaneWitness)
	// rotationCommitEdge is a test-only scheduling seam while subscriberMu and
	// effects.mu both protect the active-key/subscriber authority swap.
	rotationCommitEdge func(string)
	// rotationEdge is a test-only scheduler seam at bounded, lock-free flow
	// edges. Production never installs it; it cannot alter product data.
	rotationEdge func(session, edge string)
	// rotationFault is the paired test-only failure seam. It runs only at an
	// ordered boundary-wait entry and lets causal tests inject the exact error
	// produced by a real bounded pending append without replacing settlement.
	rotationFault func(session, edge string) error
	// refitOperationEdge is a test-only scheduling seam after the immutable
	// idempotency record is published and before its sole owner executes. It
	// cannot alter production data; causal tests use it to hold the leader while
	// an exact duplicate proves it joins rather than reissues the operation.
	refitOperationEdge func(operation, edge string)
	// refitStageFault is a test-only failure seam at the closed post-PONR refit
	// intervals. Production leaves it nil. Returned errors are wrapped in the
	// same bounded stage/class envelope as real failures before they reach
	// settlement or logs.
	refitStageFault func(session string, stage unifiedRefitFailureStage) error
	// snapshotReadEdge and snapshotRegisterEdge are test-only scheduling seams
	// around the lock-free storage read and final fenced registration.
	snapshotReadEdge     func()
	snapshotRegisterEdge func()
	// subscriberCloseEdge counts attempted lock-held subscriber closes in tests.
	// It pins rotation's exact one-close-per-predecessor multiplicity; production
	// never installs it and the close itself remains idempotent.
	subscriberCloseEdge func(unifiedjournal.PaneKey, *unifiedDevSubscriber, proto.SubscriberCloseReason)
}

type unifiedDevBirth struct {
	memory        *recordingUnitMemory
	pendingCharge int64
	owner         *UnifiedDevPaneEffects
	server        string
	name          string

	mu               sync.Mutex
	witness          controlmode.PaneWitness
	source           terminal.SourceWitness
	geometry         unifiedjournal.Geometry
	initial          *recordingInitialOperation
	streaming        bool
	observerVerified bool
	pending          []controlmode.Observation
	ready            chan struct{}
	readyOnce        sync.Once
	committed        bool
	aborted          bool
	rotation         *unifiedRotationPending
}

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

// unifiedDevUnit is one per-session control-mode observer. Measured on tmux
// 3.4 (evidence: 2026-08-19_controlmode_output_reach): a control client
// receives %output ONLY for the session it is attached to, so observing a
// session requires a client of its own. Each unit owns its control-client
// process, PTY, decoder feed, read loop, and command channel; the session it
// observes is the session its startup command created, which is why it owns
// byte zero by construction.
type unifiedDevUnit struct {
	supervisorFault atomic.Bool
	supervisorStage atomic.Uint32
	supervisorKey   atomic.Pointer[unifiedjournal.PaneKey]
	sessionIdentity string // effects.mu; available before completed-loop fields
	memory          *recordingUnitMemory
	progress        observerProgress
	owner           *UnifiedDevPaneEffects
	// generation is the ControlGeneration this unit stamps on every witness it
	// builds. Birth units keep the historical generation 1; adoption units must
	// mint fresh (see mintControlGeneration).
	generation uint64
	ptmx       *os.File
	process    *exec.Cmd
	birth      unifiedDevCommand
	commands   chan unifiedDevCommand
	// done closes when the unit's loop has exited; command submitters select on
	// it so a dead unit refuses instead of swallowing requests.
	done       chan struct{}
	readFailed atomic.Bool
	reapOnce   sync.Once
	exitErr    error

	// Unit-goroutine state. The supervisor reads these only after receiving the
	// unit on the exits channel, which orders the memory.
	sessionID string
	holder    *unifiedDevBirth
	witnesses []controlmode.PaneWitness
	delivered bool
	ready     bool
}

// NewUnifiedDevPaneEffects constructs the provider only for an enabled,
// validated development block.
// unifiedJournalCaps are the durable budgets the unified journal realm opens
// with. They are the journal's own defaults and nothing configures them; the
// package variable exists so the broker suite can shrink a cap and drive a real
// pane to it in seconds, which is how the geometry-cap accounting regression is
// proven end to end against a live tmux rather than only at the journal layer.
// The physical pair is the on-disk ledger's budget: zero keeps the journal's
// derivation (pane: a multiple of the logical pane cap; realm: a fraction of
// the runtime directory's filesystem, measured at open).
var unifiedJournalCaps = struct{ pane, realm, panePhysical, realmPhysical int64 }{
	pane: unifiedjournal.DefaultPaneCapBytes, realm: unifiedjournal.DefaultRealmCapBytes,
}

func NewUnifiedDevPaneEffects(cfg config.Broker) (*UnifiedDevPaneEffects, error) {
	if cfg.UnifiedTerminalDev == nil || !cfg.UnifiedTerminalDev.Enabled {
		return nil, errors.New("unified terminal development path is disabled")
	}
	dev := *cfg.UnifiedTerminalDev
	var server config.TmuxServer
	found := false
	for _, candidate := range cfg.Servers {
		if candidate.Label == dev.Server {
			server, found = candidate, true
			break
		}
	}
	if !found {
		return nil, errors.New("unified terminal development server is unavailable")
	}
	effects := &UnifiedDevPaneEffects{
		cfg: cfg, dev: dev, server: server,
		spawns: make(chan unifiedDevCommand), exits: make(chan *unifiedDevUnit),
		units: make(map[string]*unifiedDevUnit), panes: make(map[string]*unifiedDevBirth),
		active:              make(map[string]unifiedjournal.PaneKey),
		adopting:            make(map[string]struct{}),
		refitOperations:     make(map[unifiedRefitOperationKey]*unifiedRefitOperation),
		rotationStates:      make(map[string]*unifiedRotationState),
		rotationWake:        make(chan struct{}, 1),
		rotationEligibility: make(chan struct{}, 1),
		rotationNow:         time.Now,
		rotationAfter: func(delay time.Duration, fn func()) func() bool {
			timer := time.AfterFunc(delay, fn)
			return timer.Stop
		},
		terminalRetires: make(map[unifiedjournal.PaneKey]*terminalRetirement),
		subscribers:     make(map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}),
	}
	effects.readers.copies = &effects.copies
	effects.transients.copies = &effects.copies
	realm, err := unifiedjournal.OpenRealm(unifiedjournal.OpenOptions{
		RuntimeDir: dev.RuntimeDir, Realm: cfg.Realm, BrokerIncarnation: fmt.Sprintf("%d", os.Getpid()),
		UID: os.Getuid(), GID: os.Getgid(), DirectoryMode: 0o700, FileMode: 0o600,
		PaneCapBytes: unifiedJournalCaps.pane, RealmCapBytes: unifiedJournalCaps.realm,
		PanePhysicalCapBytes: unifiedJournalCaps.panePhysical, PhysicalCapBytes: unifiedJournalCaps.realmPhysical,
		CompletePaneSlots: int64(dev.AdoptionSlots),
		EligibilityWake:   effects.requestRotationEligibility,
		StartupRecovery:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("open unified development journal: %w", err)
	}
	effects.realm = realm
	effects.journalMu.publish = effects.publishPressure
	effects.reconcileStartupJournals()
	if err := realm.ConfigureSourceQuota(2, retentionDefaultPaneLimit, effects.startupSources); err != nil {
		realm.Close()
		return nil, fmt.Errorf("configure recording source allowances: %w", err)
	}
	effects.publishPressure()
	return effects, nil
}

type startupPaneWitness struct {
	server, session, window, pane string
	socket                        terminal.SocketIdentity
	serverProcess                 terminal.ProcessWitness
	paneProcess                   terminal.ProcessWitness
	geometry                      unifiedjournal.Geometry
}

func (witness startupPaneWitness) incarnation(initial unifiedjournal.Geometry) string {
	return startupSourceIncarnation(witness.socket, witness.serverProcess, witness.session, witness.window, witness.pane, witness.paneProcess, initial.Columns, initial.Rows)
}

func startupSourceIncarnation(socket terminal.SocketIdentity, server terminal.ProcessWitness, session, window, pane string, process terminal.ProcessWitness, columns, rows int) string {
	identity := fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%d\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d", socket.Path, socket.Device, socket.Inode, server.PID, server.StartTime, session, window, pane, process.PID, process.StartTime, columns, rows)
	sum := sha256.Sum256([]byte(identity))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// startupPaneInventory takes one tmux list-panes inventory for the configured
// server, then derives the same exact source identity used at attachment. A
// failed row/process witness makes the whole inventory unavailable; callers
// retain every recovered file rather than making per-row guesses.
func (effects *UnifiedDevPaneEffects) startupPaneInventory(ctx context.Context) (inventory []startupPaneWitness, release func(), resultErr error) {
	owner, err := effects.transients.acquire()
	if err != nil {
		return nil, func() {}, err
	}
	response := &recordingResponse{owner: owner}
	release = func() { response.release(); owner.done() }
	defer func() {
		if resultErr != nil {
			release()
			release = func() {}
		}
	}()
	commandCtx, cancel := context.WithTimeout(ctx, SetupTimeout)
	defer cancel()
	socketPath, err := resolveSocketPath(effects.server)
	if err != nil {
		return nil, release, err
	}
	info, err := os.Stat(socketPath)
	if err != nil {
		return nil, release, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, release, errors.New("startup inventory socket stat unavailable")
	}
	inc, err := incarnation(effects.server)
	if err != nil {
		return nil, release, err
	}
	probe := procProbe{}
	serverWitness, err := probe.Witness(ctx, inc.ServerPID)
	if err != nil {
		return nil, release, err
	}
	const format = "#{session_id}\t#{window_id}\t#{pane_id}\t#{pane_pid}\t#{pane_width}\t#{pane_height}"
	err = recordingCommandOutput(commandCtx, effects.server, response, "list-panes", "-a", "-F", format)
	if err != nil {
		return nil, release, err
	}
	lines := strings.Split(strings.TrimSpace(response.String()), "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		return nil, release, nil
	}
	result := make([]startupPaneWitness, 0, len(lines))
	for _, line := range lines {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 6 {
			return nil, release, errors.New("startup inventory returned an invalid row")
		}
		panePID, e1 := strconv.Atoi(fields[3])
		columns, e2 := strconv.Atoi(fields[4])
		rows, e3 := strconv.Atoi(fields[5])
		if e1 != nil || e2 != nil || e3 != nil {
			return nil, release, errors.New("startup inventory returned invalid numbers")
		}
		paneWitness, err := probe.Witness(ctx, panePID)
		if err != nil {
			return nil, release, err
		}
		result = append(result, startupPaneWitness{
			server: effects.server.Label, session: fields[0], window: fields[1], pane: fields[2],
			socket:        terminal.SocketIdentity{Path: socketPath, Device: uint64(st.Dev), Inode: st.Ino},
			serverProcess: serverWitness, paneProcess: paneWitness,
			geometry: unifiedjournal.Geometry{Columns: columns, Rows: rows},
		})
	}
	return result, release, nil
}

func classifyStartupRecovery(recovery unifiedjournal.Recovery, inventory []startupPaneWitness, authoritative bool, server string) unifiedjournal.RecoveryDisposition {
	if !authoritative || recovery.Key.Server != server {
		return unifiedjournal.RecoveryRetainAmbiguous
	}
	var candidates []startupPaneWitness
	for _, live := range inventory {
		if live.server == recovery.Key.Server && live.session == recovery.Key.Session {
			candidates = append(candidates, live)
		}
	}
	if len(candidates) == 0 {
		return unifiedjournal.RecoveryRetireAbsent
	}
	if len(candidates) != 1 {
		return unifiedjournal.RecoveryRetainAmbiguous
	}
	live := candidates[0]
	if live.window != recovery.Key.Window || live.pane != recovery.Key.Pane || live.incarnation(recovery.Initial) != recovery.Key.Incarnation {
		return unifiedjournal.RecoveryRetireReplacement
	}
	if live.geometry != recovery.Final {
		return unifiedjournal.RecoveryRetainAmbiguous
	}
	return unifiedjournal.RecoveryKeepExact
}

func startupRecoveryRecord(outcome unifiedjournal.Recovery) string {
	return fmt.Sprintf("component=broker event=startup_recovery decision=%q result=%q server=%q session=%q window=%q pane=%q control_generation=%d retained_logical_bytes=%d retained_physical_bytes=%d retained_slot=%t retry_needed=%t",
		outcome.Decision.String(), outcome.Outcome, outcome.Key.Server, outcome.Key.Session,
		outcome.Key.Window, outcome.Key.Pane, outcome.Key.ControlGeneration,
		outcome.RetainedLogical, outcome.RetainedPhysical, outcome.RetainedSlot, outcome.RetryNeeded)
}

func recordStartupRecovery(outcome unifiedjournal.Recovery) {
	brokerLogf("%s", startupRecoveryRecord(outcome))
}

func (effects *UnifiedDevPaneEffects) reconcileStartupJournals() {
	recovered := effects.realm.Recovered()
	if len(recovered) == 0 {
		return
	}
	var maximumGeneration uint64
	for _, recovery := range recovered {
		if recovery.Key.ControlGeneration > maximumGeneration {
			maximumGeneration = recovery.Key.ControlGeneration
		}
	}
	if maximumGeneration > 1 {
		effects.generationSequence.Store(maximumGeneration - 1)
	}
	inventory, releaseInventory, err := effects.startupPaneInventory(context.Background())
	defer releaseInventory()
	decisions := make([]unifiedjournal.RecoveryDecision, 0, len(recovered))
	effects.startupSources = make(map[unifiedjournal.PaneKey]unifiedjournal.SourceID)
	for _, recovery := range recovered {
		disposition := classifyStartupRecovery(recovery, inventory, err == nil, effects.server.Label)
		decisions = append(decisions, unifiedjournal.RecoveryDecision{Key: recovery.Key, Disposition: disposition})
		// Source ownership survives geometry drift even when replay is
		// ambiguous. Association uses the same immutable witness proof and
		// original header geometry; it grants no replay or readiness authority.
		if err == nil && recovery.Key.Server == effects.server.Label {
			for _, live := range inventory {
				if live.server == recovery.Key.Server && live.session == recovery.Key.Session && live.window == recovery.Key.Window && live.pane == recovery.Key.Pane && live.incarnation(recovery.Initial) == recovery.Key.Incarnation {
					identity := recordingSourceKey{effects.cfg.Realm, effects.server.Label, live.socket, live.serverProcess, strings.Clone(live.session), strings.Clone(live.window), strings.Clone(live.pane), live.paneProcess}
					effects.startupSources[recovery.Key] = identity.journalID()
				}
			}
		}
	}
	outcomes := effects.realm.ReconcileRecovered(decisions)
	effects.startupRecovery = append([]unifiedjournal.Recovery(nil), outcomes...)
	for _, outcome := range outcomes {
		recordStartupRecovery(outcome)
	}
}

func (effects *UnifiedDevPaneEffects) retentionTrial() retentionTrialOptions {
	return retentionTrialOptions{
		admittedWrites: true,
		sourceRealm:    effects.cfg.Realm, sourceServer: effects.server.Label,
		realm: effects.realm, journalMu: &effects.journalMu, maxBatchBytes: 64 << 10, maxFeedDelay: 16 * time.Millisecond,
		now: time.Now, after: func(delay time.Duration, fn func()) func() bool {
			timer := time.AfterFunc(delay, fn)
			return timer.Stop
		},
		observe: func(event string, fields map[string]int64) {
			if observe := effects.retentionObserve; observe != nil {
				observe(event, fields)
			}
		},
		retire:          effects.retirePane,
		committed:       effects.requestRotationEvaluation,
		eligibilityWake: effects.requestRotationEligibility,
	}
}

// retirePane serializes terminal-generation settlement with every journal
// append and pressure read. False is retained unlink authority: the retention
// runtime retries it and the realm keeps both durable charges until proof.
func (effects *UnifiedDevPaneEffects) retirePane(key unifiedjournal.PaneKey) bool {
	effects.journalMu.Lock()
	defer effects.journalMu.Unlock()
	return effects.realm.RetirePane(key)
}

// mintControlGeneration issues the ControlGeneration a new observer unit stamps
// on the witnesses it builds. A born session is always a fresh journal pane
// (its key is new by pane identity and incarnation), so birth units keep the
// historical generation 1. An adoption unit must mint fresh: the journal's
// AdmitPane is idempotent only for a live continuous generation, and the
// session router invalidates on any generation mismatch, so re-adoption under
// a reused generation would fail closed against the previous unit's journal
// instead of opening a new one.
func (effects *UnifiedDevPaneEffects) mintControlGeneration(fresh bool) uint64 {
	if !fresh {
		return 1
	}
	return effects.generationSequence.Add(1) + 1
}

func (effects *UnifiedDevPaneEffects) BeginSessionBirth(server, name string) (SessionBirthEffects, error) {
	if server != effects.dev.Server || name != effects.dev.Session {
		return nil, nil
	}
	birth := &unifiedDevBirth{owner: effects, server: server, name: name, ready: make(chan struct{})}
	return birth, nil
}

// dashboardLaunch is now only the configured target's CREATE affordance: it
// projects create while no session carries the configured name, and nothing
// once one does — an existing target is served by the per-session projection
// (projectSession), which is where open/adoptable/blocked now live.
func (effects *UnifiedDevPaneEffects) dashboardLaunch(server string, sessions []proto.Session) *proto.UnifiedDevLaunch {
	if !effects.dev.Enabled || server != effects.dev.Server {
		return nil
	}
	for i := range sessions {
		if sessions[i].Name == effects.dev.Session {
			return nil
		}
	}
	return &proto.UnifiedDevLaunch{State: proto.UnifiedDevLaunchCreate, Name: effects.dev.Session}
}

// unifiedSessionFacts carries the per-row eligibility fields measured by the
// same list-sessions call that produced the inventory row. On tmux 3.4 the
// window/pane formats expand against each session's active window and pane
// under `list-sessions -F`, which is exactly sufficient: the eligible class's
// active pane IS its only pane. Valid=false means the fields did not parse and
// the projection fails closed to unavailable for that row only.
type unifiedSessionFacts struct {
	Alternate, Panes, Windows int
	Valid                     bool
}

// projectSession classifies one inventory row into the closed per-session
// unified state set. Journal-active wins outright and reports its generation
// origin; everything else is judged from the row's measured eligibility, then
// slot budget. Every unclassifiable input fails closed to unavailable.
func (effects *UnifiedDevPaneEffects) projectSession(server, sessionID string, facts unifiedSessionFacts) *proto.UnifiedSessionState {
	if !effects.dev.Enabled {
		return nil
	}
	if server != effects.dev.Server {
		return &proto.UnifiedSessionState{State: proto.UnifiedSessionBlockedForeignServer}
	}
	effects.mu.Lock()
	key, active := effects.active[sessionID]
	effects.mu.Unlock()
	if active {
		if effects.realm == nil {
			return &proto.UnifiedSessionState{State: proto.UnifiedSessionUnavailable}
		}
		effects.journalMu.Lock()
		origin, err := effects.realm.Origin(key)
		effects.journalMu.Unlock()
		if err != nil {
			return &proto.UnifiedSessionState{State: proto.UnifiedSessionUnavailable}
		}
		projectedOrigin := string(origin)
		if origin == unifiedjournal.OriginRotated {
			// Rotation is a durable reconstruction of the same browser-visible
			// terminal. Keep the journal's typed origin internal: the public
			// inventory vocabulary intentionally has only birth and reconstructed.
			projectedOrigin = proto.UnifiedOriginReconstructed
		}
		return &proto.UnifiedSessionState{State: proto.UnifiedSessionOpen, Origin: projectedOrigin}
	}
	if !facts.Valid {
		return &proto.UnifiedSessionState{State: proto.UnifiedSessionUnavailable}
	}
	// Eligibility before slots, matching the adoption sequence: an ineligible
	// pane shows its real blocker even when the slot budget is also gone.
	switch {
	case facts.Windows != 1:
		return &proto.UnifiedSessionState{State: proto.UnifiedSessionBlockedMultiWindow}
	case facts.Panes != 1:
		return &proto.UnifiedSessionState{State: proto.UnifiedSessionBlockedMultiPane}
	}
	if effects.realm == nil {
		return &proto.UnifiedSessionState{State: proto.UnifiedSessionUnavailable}
	}
	effects.journalMu.Lock()
	slots := effects.realm.AvailableCompletePaneSlots()
	effects.journalMu.Unlock()
	if slots < 1 {
		return &proto.UnifiedSessionState{State: proto.UnifiedSessionSlotsExhausted}
	}
	return &proto.UnifiedSessionState{State: proto.UnifiedSessionAdoptable}
}

// UnifiedAdoption is the provider-level outcome of adopting an existing
// session into the unified journal.
type UnifiedAdoption struct {
	SessionID string
	Key       unifiedjournal.PaneKey
	// Trimmed reports that the reconstructed bootstrap dropped its oldest
	// history rows to fit the bootstrap byte cap; the projection layer can
	// surface a shallow reconstruction honestly.
	Trimmed bool
	// Existing marks the idempotent path: the session was already
	// journal-active and this call created nothing.
	Existing bool
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

func (birth *unifiedDevBirth) CreateSession(server config.TmuxServer, args []string) (string, error) {
	if server.Label != birth.server || len(args) < 2 {
		return "", errors.New("invalid unified session birth request")
	}
	memory, err := birth.owner.transients.acquire()
	if err != nil {
		return "", err
	}
	transferred := false
	defer func() {
		if !transferred {
			memory.done()
		}
	}()
	result := make(chan unifiedDevCommandResult, 1)
	select {
	case birth.owner.spawns <- unifiedDevCommand{spawnDeadline: time.Now().Add(5 * time.Second), memory: memory, server: server, args: append([]string(nil), args...), birth: birth, done: result}:
		transferred = true
	case <-time.After(5 * time.Second):
		return "", errors.New("unified session birth observer unavailable")
	}
	select {
	case outcome := <-result:
		return outcome.session, outcome.err
	case <-time.After(5 * time.Second):
		return "", errors.New("unified session birth timed out")
	}
}

func (birth *unifiedDevBirth) CommitSessionBirth(sessionID string) error {
	if !birth.memory.Reserve(int64(adoptionBootstrapCapBytes)) {
		return errRecordingTransients
	}
	defer birth.memory.Release(int64(adoptionBootstrapCapBytes))
	select {
	case <-birth.ready:
	case <-time.After(2 * time.Second):
		return errors.New("unified session birth observer is not ready")
	}
	if err := birth.captureInitialIfEmpty(); err != nil {
		return err
	}
	birth.mu.Lock()
	if birth.aborted || birth.committed || birth.streaming || birth.witness.Session.Session != sessionID || len(birth.pending) == 0 {
		birth.mu.Unlock()
		return errors.New("unified session birth has no byte-zero output")
	}
	observer := birth.owner.initialObserver()
	if observer == nil {
		birth.mu.Unlock()
		return errRecordingInitial
	}
	registry := observer.recordingRegistry()
	geometry := birth.geometry
	if geometry.Columns == 0 {
		birth.owner.journalMu.Lock()
		geometry, _ = birth.owner.realm.InitialGeometry(journalKey(birth.witness))
		birth.owner.journalMu.Unlock()
	}
	size := 0
	for _, observation := range birth.pending {
		size += len(observation.Data)
	}
	payload := make([]byte, 0, size)
	for _, observation := range birth.pending {
		if observation.Witness != birth.witness || observation.Kind != controlmode.ObservationOutput {
			birth.mu.Unlock()
			return errRecordingInitial
		}
		payload = append(payload, observation.Data...)
	}
	op, err := observer.beginInitial(recordingInitialBirth, birth.witness, birth.source, geometry, payload)
	if err != nil {
		birth.mu.Unlock()
		return err
	}
	birth.initial = op
	birth.clearPendingLocked()
	birth.streaming = true
	birth.mu.Unlock()
	birth.owner.mu.Lock()
	initialUnit := birth.owner.units[sessionID]
	birth.owner.mu.Unlock()
	op.bindOwner(initialUnit)
	<-op.done
	if _, err := op.result(); err != nil {
		return settleInitial(op, registry, err)
	}
	if err := birth.verifyInitialSource(context.Background()); err != nil {
		return settleInitial(op, registry, err)
	}
	err = func() error {
		birth.mu.Lock()
		defer birth.mu.Unlock()
		if birth.aborted || birth.initial != op {
			return errRecordingInitial
		}
		key := journalKey(birth.witness)
		birth.owner.mu.Lock()
		defer birth.owner.mu.Unlock()
		unit := birth.owner.units[sessionID]
		if unit == nil || unit != initialUnit || unit.holder != birth || !unit.isLive() {
			return errRecordingInitial
		}
		if err := observer.publishInitial(op); err != nil {
			return err
		}
		birth.committed = true
		birth.owner.active[sessionID] = key
		return nil
	}()
	if err != nil {
		return settleInitial(op, registry, err)
	}
	return nil
}

func (birth *unifiedDevBirth) AbortSessionBirth(error) error {
	birth.mu.Lock()
	if birth.committed {
		birth.mu.Unlock()
		return nil
	}
	birth.aborted = true
	birth.clearPendingLocked()
	pane := birth.witness.Pane
	op := birth.initial
	birth.mu.Unlock()
	if op != nil {
		if observer := birth.owner.initialObserver(); observer != nil {
			_ = settleInitial(op, observer.recordingRegistry(), errRecordingInitial)
		}
	}
	if pane != "" {
		birth.owner.mu.Lock()
		if birth.owner.panes[pane] == birth {
			delete(birth.owner.panes, pane)
		}
		birth.owner.mu.Unlock()
	}
	return nil
}

func (birth *unifiedDevBirth) observe(data []byte) error {
	birth.mu.Lock()
	defer birth.mu.Unlock()
	if birth.aborted {
		return unifiedjournal.ErrInvalidated
	}
	if !birth.committed && birth.rotation != nil {
		return birth.rotation.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: birth.witness, Data: data})
	}
	if !birth.streaming && !birth.committed {
		if err := birth.appendPendingLocked(data); err != nil {
			return err
		}
		if birth.observerVerified {
			birth.readyOnce.Do(func() { close(birth.ready) })
		}
		return nil
	}
	// ObservePane synchronously reserves and copies accepted data. This caller
	// keeps the decoder batch alive through that copy; no intermediate clone.
	return birth.owner.observer.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: birth.witness, Data: data})
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

// run drives one unit to completion and hands the carcass to the supervisor.
// The founding request is always answered exactly once, even when the unit
// dies before its startup block completes.
func (unit *unifiedDevUnit) run(ctx context.Context) {
	defer unit.memory.done()
	err := unit.serve(ctx)
	if unit.supervisorFault.Load() && err == nil {
		err = errRecordingStalled
	}
	unit.progress.recordExit(err)
	unit.exitErr = err
	if !unit.delivered {
		if err == nil {
			err = errors.New("unified session birth observer unavailable")
		}
		unit.birth.done <- unifiedDevCommandResult{err: err}
	}
	close(unit.done)
	unit.owner.recordUnitExit(unit)
	// The unit is over, so its control client dies with it. Closing the PTY
	// master is not enough: a client that is still healthily attached — an
	// adoption refused after a successful attach is the common case — survives
	// the close, and waiting on it would strand this carcass forever and with
	// it the reap that releases the session for the next attempt.
	if unit.process.Process != nil {
		_ = unit.process.Process.Kill()
	}
	_ = unit.process.Wait()
	unit.owner.mu.Lock()
	delete(unit.owner.supervised, unit)
	unit.owner.mu.Unlock()
	select {
	case unit.owner.exits <- unit:
	case <-ctx.Done():
		unit.reapOnce.Do(func() { unit.memory.done() })
	}
}

func (unit *unifiedDevUnit) serve(ctx context.Context) error {
	defer unit.ptmx.Close()
	read := make(chan []byte, 16)
	readErr := make(chan error, 1)
	unit.memory.hold()
	go func() {
		defer unit.memory.done()
		unifiedDevReadLoop(unit.ptmx, read, readErr, unit.done, &unit.readFailed)
	}()
	decoder := controlmode.NewBudgetedDecoder(unit.memory)
	defer decoder.Release()
	if unit.birth.adoption != nil {
		unit.progress.setStage(observerStageAttach)
		if err := unit.finishAdoptionAttach(ctx, decoder, read, readErr); err != nil {
			return err
		}
		unit.progress.setStage(observerStageReady)
		if err := unit.awaitReady(ctx, decoder, read, readErr); err != nil {
			return err
		}
		if edge := unit.owner.observerReadinessEdge; edge != nil {
			if err := edge(unit.sessionID); err != nil {
				return err
			}
		}
		unit.progress.setStage(observerStageFlags)
		if err := unit.verifyObserverClientFlags(ctx, decoder, read, readErr); err != nil {
			return err
		}
		if recovery := unit.birth.adoption.recovery; recovery != nil {
			unit.progress.setStage(observerStageRotation)
			if err := unit.runRotation(ctx, decoder, read, readErr, recovery); err != nil {
				return err
			}
			unit.delivered = true
			unit.owner.mu.Lock()
			delete(unit.owner.adopting, unit.sessionID)
			unit.owner.mu.Unlock()
		} else {
			unit.progress.setStage(observerStageAdoption)
			if err := unit.runAdoption(ctx, decoder, read, readErr); err != nil {
				return err
			}
		}
	} else {
		unit.progress.setStage(observerStageBirth)
		if err := unit.finishBirth(ctx, decoder, read, readErr); err != nil {
			return err
		}
		unit.progress.setStage(observerStageReady)
		if err := unit.awaitReady(ctx, decoder, read, readErr); err != nil {
			return err
		}
		if edge := unit.owner.observerReadinessEdge; edge != nil {
			if err := edge(unit.sessionID); err != nil {
				return err
			}
		}
		unit.progress.setStage(observerStageFlags)
		if err := unit.verifyObserverClientFlags(ctx, decoder, read, readErr); err != nil {
			return err
		}
	}
	if unit.birth.birth != nil {
		birth := unit.birth.birth
		birth.mu.Lock()
		birth.observerVerified = true
		birth.readyOnce.Do(func() { close(birth.ready) })
		birth.mu.Unlock()
	}
	for {
		unit.progress.setStage(observerStageStream)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			unit.progress.setStage(observerStageRead)
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				return ctx.Err()
			}
			return fmt.Errorf("unified control observer: %w", err)
		case request := <-unit.commands:
			unit.progress.setStage(observerStageCommand)
			if request.server.Label != unit.owner.server.Label {
				request.done <- unifiedDevCommandResult{err: fmt.Errorf("%w: foreign unified server", errGeometryNotIssued)}
				continue
			}
			if request.birthCapture != nil {
				err := unit.captureBirthInitial(ctx, decoder, read, readErr, request)
				request.done <- unifiedDevCommandResult{err: err}
				if err != nil {
					return err
				}
				continue
			}
			if request.rotation != nil {
				unit.progress.setStage(observerStageRotation)
				err := unit.runRotation(ctx, decoder, read, readErr, request.rotation)
				request.done <- unifiedDevCommandResult{err: err}
				if errors.Is(err, ErrUnifiedRotateFatal) || errors.Is(err, ErrUnifiedObserverFlowControl) {
					return err
				}
				continue
			}
			line := make([]string, len(request.args))
			for index, argument := range request.args {
				line[index] = shellQuote(argument)
			}
			if _, err := io.WriteString(unit.ptmx, strings.Join(line, " ")+"\n"); err != nil {
				request.done <- unifiedDevCommandResult{err: err}
				continue
			}
			if err := unit.finishCommand(ctx, decoder, read, readErr, request); err != nil {
				request.done <- unifiedDevCommandResult{err: err}
			}
		case chunk := <-read:
			unit.progress.setStage(observerStageDecode)
			if err := unit.owner.consumeObserverEvents(decoder, chunk); err != nil {
				return err
			}
		}
	}
}

// unifiedDevReadLoop normalizes the control PTY's CR-LF line endings into the
// byte stream the decoder consumes. It stops feeding once the unit is done, so
// a dead unit cannot strand this goroutine on a full channel.
func unifiedDevReadLoop(ptmx *os.File, read chan<- []byte, readErr chan<- error, done <-chan struct{}, failed *atomic.Bool) {
	buffer := make([]byte, 64<<10)
	pendingCR := false
	for {
		n, err := ptmx.Read(buffer)
		if err != nil {
			// Transport health is independent of consuming the error channel.
			// A ready manager result cannot outrun a known observer read failure.
			failed.Store(true)
		}
		if n > 0 {
			chunk := make([]byte, 0, n+1)
			for _, value := range buffer[:n] {
				if pendingCR {
					if value == '\n' {
						chunk = append(chunk, '\n')
						pendingCR = false
						continue
					}
					chunk = append(chunk, '\r')
					pendingCR = false
				}
				if value == '\r' {
					pendingCR = true
				} else {
					chunk = append(chunk, value)
				}
			}
			if len(chunk) != 0 {
				select {
				case read <- chunk:
				case <-done:
					return
				}
			}
		}
		if err != nil {
			readErr <- err
			return
		}
	}
}

func unifiedDevDisableEcho(file *os.File) error {
	var state syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&state)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	state.Lflag &^= syscall.ECHO
	_, _, errno = syscall.Syscall6(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&state)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// finishBirth consumes the unit's startup command block. Because the startup
// command IS the session creation, tmux wraps the printed session identity in
// the connection's first %begin/%end block (measured: n3_born.txt), and any
// pane output can only follow that block on this same stream.
func (unit *unifiedDevUnit) finishBirth(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error) error {
	var batch controlmode.EventBatch
	defer batch.Release()
	effects := unit.owner
	request := unit.birth
	response := recordingResponse{owner: unit.memory}
	defer response.release()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case chunk := <-read:
			events, err := batch.Feed(decoder, chunk)
			if err != nil {
				return fmt.Errorf("decode unified create response: %w", err)
			}
			for index, event := range events {
				switch event.Kind {
				case controlmode.EventCommandResponse:
					if err := response.Write(event.Data); err != nil {
						return err
					}
				case controlmode.EventCommandError:
					// The rest of the decoded chunk still carries this
					// connection's live events; dropping it would silently
					// lose pane bytes, so route it onward before failing.
					for _, remainder := range events[index+1:] {
						if err := effects.consumeObserverEvent(remainder); err != nil {
							return err
						}
					}
					return errors.New("tmux rejected unified session birth")
				case controlmode.EventCommandEnd:
					sessionID := strings.Clone(strings.TrimSpace(response.String()))
					if !validSessionID(sessionID) {
						return errors.New("unified session birth returned invalid identity")
					}
					source, err := buildSourceWitness(ctx, effects.server, "", sessionID)
					if err != nil {
						return err
					}
					witness := controlmode.PaneWitness{
						Session: controlmode.SessionWitness{Server: effects.server.Label, Session: sessionID, ControlGeneration: unit.generation},
						Window:  source.WindowID, Pane: source.PaneID, Incarnation: source.Incarnation,
					}
					// The generation's birth geometry is durable truth from byte
					// zero. Without it the journal is byte-only and can never serve
					// unified replay, so admission happens before the pane is
					// observable, not after its first output.
					if err := effects.reserveJournalSource(journalKey(witness), source); err != nil {
						return err
					}
					effects.journalMu.Lock()
					admitErr := effects.realm.AdmitPane(journalKey(witness), unifiedjournal.Geometry{Columns: source.Columns, Rows: source.Rows})
					effects.journalMu.Unlock()
					if admitErr != nil {
						effects.realm.CancelSourceReservation(journalKey(witness))
						return admitErr
					}
					if err := effects.observer.AdmitPane(witness); err != nil {
						return err
					}
					request.birth.mu.Lock()
					request.birth.witness = witness
					request.birth.source = source
					request.birth.geometry = unifiedjournal.Geometry{Columns: source.Columns, Rows: source.Rows}
					request.birth.mu.Unlock()
					unit.sessionID = sessionID
					unit.holder = request.birth
					unit.witnesses = append(unit.witnesses, witness)
					effects.mu.Lock()
					effects.panes[witness.Pane] = request.birth
					effects.units[sessionID] = unit
					unit.sessionIdentity = sessionID
					effects.mu.Unlock()
					unit.delivered = true
					request.done <- unifiedDevCommandResult{session: sessionID}
					// The readiness notification can share the decoded chunk
					// with the creation's own %end, so the remainder must be
					// consumed through the readiness watch or it is lost.
					for _, remainder := range events[index+1:] {
						if err := unit.consumeReadyEvent(remainder); err != nil {
							return err
						}
					}
					return nil
				default:
					if err := effects.consumeObserverEvent(event); err != nil {
						return err
					}
				}
			}
		}
	}
}

// consumeReadyEvent routes one decoded event onward while watching for the
// unit's readiness notification: an exact match on the $id field of
// %session-changed, never a name substring, which would let one session's
// readiness satisfy another's ("main" matching "main2"). Everything else keeps
// its decoded order through the routing map, so byte-zero output cannot be
// dropped while readiness is pending.
func (unit *unifiedDevUnit) consumeReadyEvent(event controlmode.Event) error {
	if !unit.ready && unit.sessionID != "" && event.Kind == controlmode.EventNotification && event.Name == "session-changed" {
		fields := strings.Fields(event.Args)
		if len(fields) != 0 && fields[0] == unit.sessionID {
			unit.ready = true
			return nil
		}
	}
	return unit.owner.consumeObserverEvent(event)
}

// awaitReady blocks until the readiness watch has seen the observed session's
// own %session-changed, which may already have arrived inside the creation
// chunk consumed by finishBirth.
func (unit *unifiedDevUnit) awaitReady(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error) error {
	var batch controlmode.EventBatch
	defer batch.Release()
	for {
		if unit.ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case chunk := <-read:
			events, err := batch.Feed(decoder, chunk)
			if err != nil {
				return fmt.Errorf("decode unified observer bootstrap: %w", err)
			}
			for _, event := range events {
				if err := unit.consumeReadyEvent(event); err != nil {
					return err
				}
			}
		}
	}
}

// verifyObserverClientFlags interrogates this unit's own control client on its
// ordered stream after readiness and before any reconstructed capture. tmux's
// pause-after and no-output flags are additive and another same-server client
// may set them at any time, so this is an early rejection gate rather than a
// lasting proof. The adoption composite repeats the check at its boundary.
func (unit *unifiedDevUnit) verifyObserverClientFlags(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error) error {
	request := unifiedDevCommand{
		args:            []string{"list-clients", "-F", "#{client_pid}|#{client_flags}"},
		consumeResponse: func(raw string) error { return rejectObserverClientInventory(raw, unit.process.Process.Pid) },
		blocks:          1,
		done:            make(chan unifiedDevCommandResult, 1),
	}
	line := make([]string, len(request.args))
	for index, argument := range request.args {
		line[index] = shellQuote(argument)
	}
	if _, err := io.WriteString(unit.ptmx, strings.Join(line, " ")+"\n"); err != nil {
		return err
	}
	if err := unit.finishCommand(ctx, decoder, read, readErr, request); err != nil {
		return err
	}
	return (<-request.done).err
}

func rejectObserverClientInventory(raw string, observerPID int) error {
	matches := 0
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "|", 2)
		if len(fields) != 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid != observerPID {
			continue
		}
		matches++
		if err := rejectObserverClientFlags(fields[1]); err != nil {
			return err
		}
	}
	if matches != 1 {
		return fmt.Errorf("%w: observer client identity count %d", ErrUnifiedObserverFlowControl, matches)
	}
	return nil
}

func rejectObserverClientFlags(raw string) error {
	flags := strings.TrimSpace(raw)
	if strings.ContainsAny(flags, " \t\r\n") {
		return fmt.Errorf("%w: malformed client flags", ErrUnifiedObserverFlowControl)
	}
	for _, flag := range strings.Split(flags, ",") {
		if flag == "no-output" || flag == "pause-after" || strings.HasPrefix(flag, "pause-after=") {
			return fmt.Errorf("%w: dangerous client flag %s", ErrUnifiedObserverFlowControl, flag)
		}
	}
	return nil
}

// finishAdoptionAttach consumes the unit's startup attach block. The attach
// prints nothing; tmux reports an unavailable target as %error on this same
// block (measured: the message text arrives as the block's response line), and
// the attach is the block's only command, so %error here IS the typed
// missing-session refusal — no message parsing is needed or wanted.
func (unit *unifiedDevUnit) finishAdoptionAttach(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error) error {
	var batch controlmode.EventBatch
	defer batch.Release()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case chunk := <-read:
			events, err := batch.Feed(decoder, chunk)
			if err != nil {
				return fmt.Errorf("decode unified adoption attach: %w", err)
			}
			for index, event := range events {
				switch event.Kind {
				case controlmode.EventCommandResponse:
					// Response text here can only be an error message; the
					// %error that follows carries the judgement.
				case controlmode.EventCommandError:
					for _, remainder := range events[index+1:] {
						if err := unit.owner.consumeObserverEvent(remainder); err != nil {
							return err
						}
					}
					return ErrUnifiedAdoptSessionMissing
				case controlmode.EventCommandEnd:
					// Readiness may share the decoded chunk with the attach's
					// own %end; the remainder is consumed through the
					// readiness watch so it cannot be lost.
					for _, remainder := range events[index+1:] {
						if err := unit.consumeReadyEvent(remainder); err != nil {
							return err
						}
					}
					return nil
				default:
					if err := unit.consumeReadyEvent(event); err != nil {
						return err
					}
				}
			}
		}
	}
}

// runAdoption owns the whole adoption sequence on the unit goroutine, exactly
// as the unit goroutine owns birth: witness resolution, the buffered holder,
// the atomic capture composite with its drift retries, journal admission, the
// synthesized bootstrap, and registration. It answers the founding request
// itself only on success; every failure propagates so the unit's death
// protocol delivers it exactly once.
func (unit *unifiedDevUnit) runAdoption(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error) error {
	effects := unit.owner
	sessionID := unit.birth.adoption.sessionID
	source, err := buildSourceWitness(ctx, effects.server, "", sessionID)
	if err != nil {
		return errors.Join(ErrUnifiedAdoptSessionMissing, err)
	}
	witness := controlmode.PaneWitness{
		Session: controlmode.SessionWitness{Server: effects.server.Label, Session: sessionID, ControlGeneration: unit.generation},
		Window:  source.WindowID, Pane: source.PaneID, Incarnation: source.Incarnation,
	}
	holder := &unifiedDevBirth{owner: effects, memory: unit.memory, server: effects.server.Label, ready: make(chan struct{}), witness: witness, source: source}
	for attempt := 1; attempt <= adoptionCaptureAttempts; attempt++ {
		if attempt > 1 {
			effects.adoptionRetries.Add(1)
			select {
			case <-time.After(time.Duration(attempt) * 25 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		reservation, key, trimmed, err := unit.submitAdoptionComposite(ctx, decoder, read, readErr, holder, &witness, attempt)
		if errors.Is(err, errUnifiedAdoptDrift) {
			continue
		}
		if err != nil {
			return err
		}
		return unit.finishAdoption(ctx, decoder, read, readErr, holder, reservation, key, trimmed)
	}
	return ErrUnifiedAdoptUnstable
}

// The accepted adoption has one failure owner across receipt, source recheck,
// authority validation and publication. Every unsuccessful exit drains its
// stream through settlement before the provisional journal can be removed.
func (unit *unifiedDevUnit) finishAdoption(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error, holder *unifiedDevBirth, reservation *unifiedjournal.AdoptionReservation, key unifiedjournal.PaneKey, trimmed bool) (err error) {
	effects := unit.owner
	stream := &recordingSettlementStream{unit: unit, decoder: decoder, read: read, readErr: readErr}
	published := false
	defer func() {
		if !published {
			err = stream.abortAdoption(holder, reservation, err)
		}
	}()
	if err := stream.awaitInitial(ctx, holder.initial); err != nil {
		return err
	}
	effects.initialObserver().recordingRegistry().retention.callHook("adoption_initial_complete", key)
	if err := holder.verifyInitialSource(ctx); err != nil {
		return err
	}
	witness := holder.witness
	sessionID := witness.Session.Session
	unit.holder = holder
	unit.witnesses = append(unit.witnesses, witness)
	effects.mu.Lock()
	if effects.panes[witness.Pane] != holder || !unit.isLive() || (unit.birth.context != nil && unit.birth.context.Err() != nil) {
		effects.mu.Unlock()
		return errRecordingInitial
	}
	if err := effects.initialObserver().publishInitial(holder.initial); err != nil {
		effects.mu.Unlock()
		return err
	}
	holder.mu.Lock()
	holder.committed = true
	holder.mu.Unlock()
	effects.units[sessionID] = unit
	unit.sessionIdentity = sessionID
	effects.active[sessionID] = key
	delete(effects.adopting, sessionID)
	published = true
	effects.mu.Unlock()
	// Active registration is the activation boundary; the provisional
	// complete-pane reservation now becomes a durable charge.
	effects.journalMu.Lock()
	reservation.Commit()
	effects.journalMu.Unlock()
	unit.delivered = true
	unit.birth.done <- unifiedDevCommandResult{session: sessionID, key: key, trimmed: trimmed}
	return nil
}

func (stream *recordingSettlementStream) abortAdoption(holder *unifiedDevBirth, reservation *unifiedjournal.AdoptionReservation, cause error) error {
	holder.mu.Lock()
	holder.aborted = true
	holder.mu.Unlock()
	if holder.initial != nil {
		cause = stream.settleInitial(holder.initial, stream.unit.owner.initialObserver().recordingRegistry(), cause)
	}
	stream.unit.owner.journalMu.Lock()
	reservation.Abort()
	stream.unit.owner.realm.ReclaimRetired(journalKey(holder.witness))
	stream.unit.owner.journalMu.Unlock()
	// Receipt failure can precede registration on the unit, so its reaper
	// does not yet own this provisional holder. Release it with the abort.
	stream.unit.owner.mu.Lock()
	if stream.unit.owner.panes[holder.witness.Pane] == holder {
		delete(stream.unit.owner.panes, holder.witness.Pane)
	}
	stream.unit.owner.mu.Unlock()
	return cause
}

// submitAdoptionComposite runs one capture attempt: it registers the buffering
// holder, submits the composite as ONE line, consumes its seven blocks, and at
// the final %end — the boundary — admits the initial operation ON THIS GOROUTINE
// before events[index+1:] are consumed, so output decoded behind the boundary
// finds the holder streaming and ordered after its bootstrap. Ready publication
// separately waits for the exact operation receipt. On any failure
// the holder is released and its buffer discarded before the decoded remainder
// is routed onward.
func (unit *unifiedDevUnit) submitAdoptionComposite(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error, holder *unifiedDevBirth, witness *controlmode.PaneWitness, attempt int) (*unifiedjournal.AdoptionReservation, unifiedjournal.PaneKey, bool, error) {
	var batch controlmode.EventBatch
	defer batch.Release()
	effects := unit.owner
	// The holder is registered BEFORE the composite is submitted: every
	// %output this connection delivers before the boundary is buffered and
	// discarded there, because its effect is already inside the capture; the
	// gap where a byte could fall between capture and live flow does not
	// exist.
	effects.mu.Lock()
	effects.panes[witness.Pane] = holder
	effects.mu.Unlock()
	release := func() {
		effects.mu.Lock()
		if effects.panes[witness.Pane] == holder {
			delete(effects.panes, witness.Pane)
		}
		effects.mu.Unlock()
		holder.mu.Lock()
		holder.clearPendingLocked()
		holder.mu.Unlock()
	}
	if _, err := io.WriteString(unit.ptmx, adoptionCompositeLine(witness.Pane, unit.birth.adoption.historyRows)); err != nil {
		release()
		return nil, unifiedjournal.PaneKey{}, false, err
	}
	remaining := adoptionCompositeBlocks
	responses := make([]string, 0, adoptionCompositeBlocks)
	response := recordingResponse{owner: unit.memory}
	defer response.release()
	// The submission span opens at the composite's first %begin, not at the
	// PTY write: flood output emitted before tmux processes the line is still
	// in flight on the stream and precedes the span, and its effect is inside
	// the capture exactly like every other pre-boundary byte.
	began := false
	for {
		select {
		case <-ctx.Done():
			release()
			return nil, unifiedjournal.PaneKey{}, false, ctx.Err()
		case err := <-readErr:
			release()
			return nil, unifiedjournal.PaneKey{}, false, err
		case chunk := <-read:
			events, err := batch.Feed(decoder, chunk)
			if err != nil {
				release()
				return nil, unifiedjournal.PaneKey{}, false, fmt.Errorf("decode unified adoption composite: %w", err)
			}
			// Judge the entire decoded chunk before its final command boundary can
			// open a generation. A tripwire record sharing that read with %end
			// must fault the unit before BeginReconstructedPane or AdmitPane.
			for _, event := range events {
				if err := observerFlowControlFault(event); err != nil {
					release()
					return nil, unifiedjournal.PaneKey{}, false, err
				}
			}
			for index, event := range events {
				switch event.Kind {
				case controlmode.EventCommandResponse:
					if err := response.Write(event.Data); err != nil {
						release()
						return nil, unifiedjournal.PaneKey{}, false, err
					}
				case controlmode.EventCommandError:
					// The target vanished mid-composite: the only rejection
					// these sub-commands can draw. Route the decoded
					// remainder onward before failing, as every consumer on
					// this connection must.
					release()
					for _, rest := range events[index+1:] {
						if err := effects.consumeObserverEvent(rest); err != nil {
							return nil, unifiedjournal.PaneKey{}, false, err
						}
					}
					return nil, unifiedjournal.PaneKey{}, false, ErrUnifiedAdoptSessionMissing
				case controlmode.EventCommandEnd:
					responses = append(responses, response.String())
					response.Reset()
					remaining--
					if remaining != 0 {
						continue
					}
					reservation, key, trimmed, err := unit.commitAdoption(responses, holder, witness, attempt)
					if err != nil {
						release()
					}
					for _, rest := range events[index+1:] {
						if consumeErr := effects.consumeObserverEvent(rest); consumeErr != nil {
							// The attempt cannot activate: even after a
							// successful boundary flip, a dead consumer means
							// this unit dies before registration, so the
							// provisional charge is released with it.
							if reservation != nil {
								stream := &recordingSettlementStream{unit: unit, decoder: decoder, read: read, readErr: readErr}
								consumeErr = stream.abortAdoption(holder, reservation, consumeErr)
								release()
							}
							return nil, unifiedjournal.PaneKey{}, false, consumeErr
						}
					}
					return reservation, key, trimmed, err
				case controlmode.EventCommandBegin:
					began = true
				default:
					if began && (event.Kind == controlmode.EventOutput || event.Kind == controlmode.EventExtendedOutput) {
						// Measured zero on tmux 3.4: the composite is atomic
						// by construction. The counter turns the measurement
						// into a regression gate.
						effects.adoptionSpanOutputs.Add(1)
					}
					if err := effects.consumeObserverEvent(event); err != nil {
						release()
						return nil, unifiedjournal.PaneKey{}, false, err
					}
				}
			}
		}
	}
}

// commitAdoption judges one attempt's seven block responses at the boundary and,
// when they hold, opens the reconstructed generation: a provisional journal
// reservation first, then registry admission, then the synthesized bootstrap
// as ordinary output, and only then the boundary flip that discards the
// holder's pre-boundary buffer and commits it to live flow. The returned
// reservation still holds the complete-pane charge provisionally: the caller
// commits it at activation, and every failure between here and activation
// aborts it — a failed adoption never consumes a slot.
func (unit *unifiedDevUnit) commitAdoption(responses []string, holder *unifiedDevBirth, witness *controlmode.PaneWitness, attempt int) (*unifiedjournal.AdoptionReservation, unifiedjournal.PaneKey, bool, error) {
	effects := unit.owner
	var tamper func(string) string
	if effects.adoptionPostTamper != nil {
		tamper = func(post string) string { return effects.adoptionPostTamper(attempt, post) }
	}
	bootstrap, geometry, trimmed, err := parseInitialCapture(responses, unit.process.Process.Pid, tamper)
	if err != nil {
		return nil, unifiedjournal.PaneKey{}, false, err
	}
	var key unifiedjournal.PaneKey
	var reservation *unifiedjournal.AdoptionReservation
	for tries := 0; tries < 16 && reservation == nil; tries++ {
		key = journalKey(*witness)
		source, err := effects.journalSourceIdentity(key, holder.source)
		if err != nil {
			return nil, unifiedjournal.PaneKey{}, false, err
		}
		effects.journalMu.Lock()
		provisional, admitErr := effects.realm.BeginReconstructedPaneForSource(key, geometry, source)
		effects.journalMu.Unlock()
		switch {
		case admitErr == nil:
			reservation = provisional
		case errors.Is(admitErr, unifiedjournal.ErrQuota):
			return nil, unifiedjournal.PaneKey{}, false, ErrUnifiedAdoptSlotsExhausted
		case errors.Is(admitErr, unifiedjournal.ErrInvalidated):
			// A surviving journal file from an earlier broker run occupies
			// this generation's key: the realm outlives active state whenever
			// its runtime directory does. A stale generation is never
			// resumed; the next generation is minted and admission retried so
			// the session stays adoptable across restarts. The stale
			// generation itself is slot-retired at scan and superseded when
			// this admission commits, so it never consumes capacity for good.
			unit.generation = effects.mintControlGeneration(true)
			witness.Session.ControlGeneration = unit.generation
		default:
			return nil, unifiedjournal.PaneKey{}, false, admitErr
		}
	}
	if reservation == nil {
		return nil, unifiedjournal.PaneKey{}, false, errors.New("unified adoption found no unused generation")
	}
	abort := func() {
		effects.journalMu.Lock()
		reservation.Abort()
		effects.realm.ReclaimRetired(key)
		effects.journalMu.Unlock()
	}
	holder.mu.Lock()
	holder.witness = *witness
	holder.mu.Unlock()
	if err := effects.observer.AdmitPane(*witness); err != nil {
		abort()
		return nil, unifiedjournal.PaneKey{}, false, err
	}
	observer := effects.initialObserver()
	if observer == nil {
		abort()
		return nil, unifiedjournal.PaneKey{}, false, errRecordingInitial
	}
	op, err := observer.beginInitial(recordingInitialAdoption, *witness, holder.source, geometry, bootstrap)
	if err != nil {
		abort()
		return nil, unifiedjournal.PaneKey{}, false, err
	}
	op.bindContext(unit.birth.context)
	op.bindOwner(unit)
	// The boundary flip. Pre-boundary buffered output is discarded because its
	// effect is inside the capture; everything after it flows to the journal
	// on the same ordered path, behind the bootstrap.
	holder.mu.Lock()
	holder.clearPendingLocked()
	holder.initial = op
	holder.geometry = geometry
	holder.streaming = true
	holder.mu.Unlock()
	return reservation, key, trimmed, nil
}

// adoptionCompositeLine is the atomic capture composite: eight semicolon
// sub-commands submitted as ONE control-mode line, which tmux 3.4 drains in a
// single command-queue run without processing pane reads (measured by the
// adoption regression test). The first command rechecks this observer's
// flags and the second normalizes its target subscription to pane:on before
// PRE/capture. This is intentionally honest rather than complete tripwire
// coverage: pane:off and no-output are signal-less in tmux, so the readiness
// flag interrogation plus this boundary normalization cover those states.
// The PRE and POST probes carry every field
// whose mutation could split the capture — history_limit included so at-cap
// saturation stays visible. The capture keeps hard row boundaries (-N, and
// deliberately no -J): the wrap bit is unrecoverable either way and columns
// never change after attach. The -P sub-command recovers the pending parser
// prefix — an escape sequence the pane terminal has consumed but not
// completed — without which a capture taken mid-sequence journals the
// continuation without its prefix and corrupts the parser at the seam.
func adoptionCompositeLine(pane string, requestedHistory ...int) string {
	historyRows := adoptionHistoryCapRows
	if len(requestedHistory) == 1 {
		historyRows = requestedHistory[0]
	}
	const probe = "#{history_size} #{history_limit} #{cursor_x} #{cursor_y} #{alternate_on} #{window_panes} #{session_windows} #{?alternate_on,#{alternate_saved_x},0} #{?alternate_on,#{alternate_saved_y},0}"
	// pane_tabs is last because it is the only field that can be empty.
	const modes = "#{cursor_flag} #{insert_flag} #{keypad_cursor_flag} #{keypad_flag} #{origin_flag} #{wrap_flag} #{mouse_standard_flag} #{mouse_button_flag} #{mouse_all_flag} #{mouse_utf8_flag} #{mouse_sgr_flag} #{scroll_region_upper} #{scroll_region_lower} #{pane_width} #{pane_height} #{pane_tabs}"
	target := shellQuote(pane)
	return strings.Join([]string{
		"list-clients -F " + shellQuote("#{client_pid}|#{client_flags}"),
		"refresh-client -A " + shellQuote(pane+":on"),
		"display-message -p -t " + target + " " + shellQuote(probe),
		"capture-pane -e -p -N -S -" + strconv.Itoa(historyRows) + " -E - -t " + target,
		// -q makes the absent saved screen an empty successful block. Both
		// captures stay in this command-queue run, inside the drift probes.
		"capture-pane -a -q -e -p -N -t " + target,
		"capture-pane -p -P -t " + target,
		"display-message -p -t " + target + " " + shellQuote(modes),
		"display-message -p -t " + target + " " + shellQuote(probe),
	}, " ; ") + "\n"
}

type adoptionProbe struct {
	historySize, historyLimit, cursorX, cursorY, alternate, panes, windows int
	savedX, savedY                                                         int
}

func parseAdoptionProbe(text string) (adoptionProbe, error) {
	invalid := errors.New("unified adoption probe returned an invalid shape")
	fields := strings.Fields(text)
	if len(fields) != 9 {
		return adoptionProbe{}, invalid
	}
	values := make([]int, len(fields))
	for index, field := range fields {
		value, err := strconv.Atoi(field)
		if err != nil || value < 0 {
			return adoptionProbe{}, invalid
		}
		values[index] = value
	}
	return adoptionProbe{
		historySize: values[0], historyLimit: values[1],
		cursorX: values[2], cursorY: values[3],
		alternate: values[4], panes: values[5], windows: values[6],
		savedX: values[7], savedY: values[8],
	}, nil
}

type adoptionModes struct {
	cursorVisible, insert, keypadCursor, keypad, origin, wrap bool
	mouseStandard, mouseButton, mouseAll, mouseUTF8, mouseSGR bool
	scrollTop, scrollBottom, columns, rows                    int
	tabs                                                      []int
}

func parseAdoptionModes(text string) (adoptionModes, error) {
	invalid := errors.New("unified adoption mode probe returned an invalid shape")
	fields := strings.Fields(text)
	// 15 fields when the pane has no tab stops at all; 16 otherwise.
	if len(fields) != 15 && len(fields) != 16 {
		return adoptionModes{}, invalid
	}
	flags := make([]bool, 11)
	for index := range flags {
		switch fields[index] {
		case "0":
		case "1":
			flags[index] = true
		default:
			return adoptionModes{}, invalid
		}
	}
	numbers := make([]int, 4)
	for index := range numbers {
		value, err := strconv.Atoi(fields[11+index])
		if err != nil || value < 0 {
			return adoptionModes{}, invalid
		}
		numbers[index] = value
	}
	modes := adoptionModes{
		cursorVisible: flags[0], insert: flags[1], keypadCursor: flags[2], keypad: flags[3],
		origin: flags[4], wrap: flags[5],
		mouseStandard: flags[6], mouseButton: flags[7], mouseAll: flags[8], mouseUTF8: flags[9], mouseSGR: flags[10],
		scrollTop: numbers[0], scrollBottom: numbers[1], columns: numbers[2], rows: numbers[3],
	}
	if modes.columns < 1 || modes.rows < 1 || modes.scrollBottom < modes.scrollTop || modes.scrollBottom >= modes.rows {
		return adoptionModes{}, invalid
	}
	if len(fields) == 16 {
		for _, column := range strings.Split(fields[15], ",") {
			value, err := strconv.Atoi(column)
			if err != nil || value < 0 {
				return adoptionModes{}, invalid
			}
			if value >= modes.columns {
				// A stop beyond the current width can survive a shrink; it is
				// unreachable and dropped rather than refused.
				continue
			}
			modes.tabs = append(modes.tabs, value)
		}
	}
	return modes, nil
}

// parseAdoptionCapture splits the capture block into rows. Every row arrives
// as one response line; the final line's terminator belongs to the block
// framing, not to the last row.
func parseAdoptionCapture(response string) []string {
	if response == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(response, "\n"), "\n")
}

type adoptionAlternate struct {
	rows             []string
	cursorX, cursorY int
}

// tmux keeps the saved grid at its original height until alternate exit.
// Shrink discards bottom rows below the saved cursor first, then scrolls the
// remaining overflow into history. Growth pulls history into view, then pads.
// hscrolled (history eligible for growth), old width and wrap/allocation flags
// are not exposed. Use available history and hard captured rows; cells clipped
// by capture cannot be recovered. These hidden-screen limits must not prevent
// opening the exact visible alternate display.
func fitAdoptionAlternate(history []string, saved adoptionAlternate, modes adoptionModes) ([]string, int, int) {
	x := min(max(saved.cursorX, 0), modes.columns-1)
	y := min(max(saved.cursorY, 0), max(0, len(saved.rows)-1))
	display := saved.rows
	padding := 0
	if len(display) > modes.rows {
		shrink := len(display) - modes.rows
		drop := min(shrink, len(display)-1-y)
		display = display[:len(display)-drop]
		y -= shrink - drop
	} else {
		growth := modes.rows - len(display)
		pull := min(growth, len(history))
		y += pull
		padding = growth - pull
	}
	rows := make([]string, 0, len(history)+len(display)+padding)
	rows = append(rows, history...)
	rows = append(rows, display...)
	rows = append(rows, make([]string, padding)...)
	return rows, x, min(max(y, 0), modes.rows-1)
}

// synthesizeAdoptionBootstrap renders the captured pane state as one byte
// sequence a fresh terminal of the captured geometry replays into the
// capture-equivalent visible screen, with a fitted hidden normal screen when
// alternate mode is active. The emit order is pinned: attribute reset, rows
// in order (history scrolls through naturally, CRLF between rows and none
// after the last), saved normal cursor and alternate display when active,
// then DECSTBM — which homes the cursor — then DECOM per the
// captured origin flag, then CUP with region-relative coordinates iff origin
// mode is on, then the remaining modes, then tab stops (whose HTS placement
// moves the cursor by column, so the captured position is restored again
// after them), and the captured pending parser prefix LAST, immediately
// before live bytes. G0/G1 designation, arbitrary saved DECSC state, cursor style, and
// the SGR live at the seam are unreadable on tmux 3.4 and reset to defaults:
// the hidden normal display also has the resize limits described above.
func synthesizeAdoptionBootstrap(rows []string, pending string, cursorX, cursorY int, modes adoptionModes, alternate ...adoptionAlternate) ([]byte, bool, error) {
	var switchScreen strings.Builder
	if len(alternate) > 0 {
		saved := alternate[0]
		if len(alternate) != 1 || len(rows) < modes.rows {
			return nil, false, errors.New("unified adoption saved screen has an invalid shape")
		}
		// Ordinary capture is normal history followed by the alternate view.
		// Seed the saved normal display before entering 1049, so a later exit
		// restores it and its cursor. CUP paints the alternate rows without
		// scrolling either buffer, including a full-width bottom row.
		visible := rows[len(rows)-modes.rows:]
		var savedX, savedY int
		rows, savedX, savedY = fitAdoptionAlternate(rows[:len(rows)-modes.rows], saved, modes)
		fmt.Fprintf(&switchScreen, "\x1b[%d;%dH\x1b[?1049h\x1b[0m", savedY+1, savedX+1)
		for index, line := range visible {
			fmt.Fprintf(&switchScreen, "\x1b[%d;1H%s", index+1, line)
		}
	}
	set := func(builder *strings.Builder, on bool, enable, disable string) {
		if on {
			builder.WriteString(enable)
		} else {
			builder.WriteString(disable)
		}
	}
	row := cursorY + 1
	if modes.origin {
		row = cursorY - modes.scrollTop + 1
		if row < 1 {
			row = 1
		}
	}
	cursor := fmt.Sprintf("\x1b[%d;%dH", row, cursorX+1)
	var tail strings.Builder
	fmt.Fprintf(&tail, "\x1b[%d;%dr", modes.scrollTop+1, modes.scrollBottom+1)
	set(&tail, modes.origin, "\x1b[?6h", "\x1b[?6l")
	tail.WriteString(cursor)
	set(&tail, modes.wrap, "\x1b[?7h", "\x1b[?7l")
	set(&tail, modes.cursorVisible, "\x1b[?25h", "\x1b[?25l")
	set(&tail, modes.insert, "\x1b[4h", "\x1b[4l")
	set(&tail, modes.keypadCursor, "\x1b[?1h", "\x1b[?1l")
	set(&tail, modes.keypad, "\x1b=", "\x1b>")
	set(&tail, modes.mouseStandard, "\x1b[?1000h", "\x1b[?1000l")
	set(&tail, modes.mouseButton, "\x1b[?1002h", "\x1b[?1002l")
	set(&tail, modes.mouseAll, "\x1b[?1003h", "\x1b[?1003l")
	set(&tail, modes.mouseUTF8, "\x1b[?1005h", "\x1b[?1005l")
	set(&tail, modes.mouseSGR, "\x1b[?1006h", "\x1b[?1006l")
	tail.WriteString("\x1b[3g")
	for _, column := range modes.tabs {
		fmt.Fprintf(&tail, "\x1b[%dG\x1bH", column+1)
	}
	tail.WriteString(cursor)
	tail.WriteString(pending)

	const head = "\x1b[0m"
	size := len(head) + switchScreen.Len() + tail.Len()
	for _, line := range rows {
		size += len(line) + 2
	}
	if len(rows) != 0 {
		size -= 2
	}
	trimmed := false
	// Preserve the visible screen. Under alternate mode the normal display is
	// hidden: after exhausting history, replace its oldest rows with blanks as
	// needed. Keep row positions and the saved cursor, never trim alternate rows.
	for size > adoptionBootstrapCapBytes && len(rows) > modes.rows {
		size -= len(rows[0]) + 2
		rows = rows[1:]
		trimmed = true
	}
	if len(alternate) != 0 {
		for index := 0; size > adoptionBootstrapCapBytes && index < len(rows); index++ {
			size -= len(rows[index])
			rows[index] = ""
			trimmed = true
		}
	}
	if size > adoptionBootstrapCapBytes {
		return nil, false, errors.New("unified adoption screen exceeds the bootstrap cap")
	}
	bootstrap := make([]byte, 0, size)
	bootstrap = append(bootstrap, head...)
	for index, line := range rows {
		if index != 0 {
			bootstrap = append(bootstrap, '\r', '\n')
		}
		bootstrap = append(bootstrap, line...)
	}
	bootstrap = append(bootstrap, switchScreen.String()...)
	bootstrap = append(bootstrap, tail.String()...)
	return bootstrap, trimmed, nil
}

// finishCommand consumes an already-issued command's blocks on the unit's own
// control connection, using the same discipline as finishBirth: the command
// response accumulates and is never dispatched as pane output, and every other
// event keeps its decoded order, including the remainder of the chunk that
// carried the final %end or a rejection.
//
// A refused guard is not distinguishable here. tmux reports a guarded if-shell
// whose guard evaluates false as an ordinary %end with an empty response, so
// this returns success and the caller's exact witness recheck is the only
// rejection detector. That is a property of tmux, established by the Phase-1
// proof, not an assumption.
func (unit *unifiedDevUnit) finishCommand(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error, request unifiedDevCommand) error {
	var batch controlmode.EventBatch
	defer batch.Release()
	remaining := request.blocks
	if remaining < 1 {
		return errors.New("unified observer command requires a block count")
	}
	response := recordingResponse{owner: unit.memory}
	defer response.release()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case chunk := <-read:
			events, err := batch.Feed(decoder, chunk)
			if err != nil {
				return fmt.Errorf("decode unified observer command: %w", err)
			}
			for index, event := range events {
				switch event.Kind {
				case controlmode.EventCommandResponse:
					if err := response.Write(event.Data); err != nil {
						return err
					}
				case controlmode.EventCommandError:
					// Same-connection live pane bytes can share the decoded
					// chunk with a rejection; consume the remainder before
					// returning so none of them are silently dropped.
					for _, rest := range events[index+1:] {
						if err := unit.owner.consumeObserverEvent(rest); err != nil {
							return err
						}
					}
					return errors.New("tmux rejected a unified observer command")
				case controlmode.EventCommandEnd:
					remaining--
					if remaining != 0 {
						continue
					}
					if request.consumeResponse != nil {
						if err := request.consumeResponse(response.String()); err != nil {
							return err
						}
					}
					request.done <- unifiedDevCommandResult{}
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

func (effects *UnifiedDevPaneEffects) consumeObserverEvents(decoder *controlmode.Decoder, chunk []byte) error {
	var batch controlmode.EventBatch
	defer batch.Release()
	events, err := batch.Feed(decoder, chunk)
	if err != nil {
		return fmt.Errorf("decode unified observer control: %w", err)
	}
	for _, event := range events {
		if err := effects.consumeObserverEvent(event); err != nil {
			return err
		}
	}
	return nil
}

// observerFlowControlFault is the closed signal tripwire for the delivery-mode
// assumption. Persea never requests tmux flow control on an observer unit, so
// pause, continue, or any delayed extended-output proves that byte delivery is
// no longer the ordered ordinary stream the journal is authorized to record.
// pane:off and no-output themselves emit no reliable signal; readiness flag
// interrogation and adoption's pane:on normalization are their separate,
// necessarily point-in-time coverage.
func observerFlowControlFault(event controlmode.Event) error {
	switch event.Kind {
	case controlmode.EventPause:
		return fmt.Errorf("%w: %%pause", ErrUnifiedObserverFlowControl)
	case controlmode.EventContinue:
		return fmt.Errorf("%w: %%continue", ErrUnifiedObserverFlowControl)
	case controlmode.EventExtendedOutput:
		return fmt.Errorf("%w: %%extended-output", ErrUnifiedObserverFlowControl)
	default:
		return nil
	}
}

func (effects *UnifiedDevPaneEffects) consumeObserverEvent(event controlmode.Event) error {
	if effects.observerDecoded != nil {
		effects.observerDecoded(event)
	}
	if err := observerFlowControlFault(event); err != nil {
		return err
	}
	if event.Kind != controlmode.EventOutput {
		return nil
	}
	effects.mu.Lock()
	birth := effects.panes[event.PaneID]
	effects.mu.Unlock()
	if birth == nil {
		return nil
	}
	return birth.observe(event.Data)
}

func (effects *UnifiedDevPaneEffects) WritePane(key unifiedjournal.PaneKey, payload []byte) error {
	return errors.New("unified feed range metadata is required")
}

func (effects *UnifiedDevPaneEffects) WritePaneRange(key unifiedjournal.PaneKey, payload []byte, start, end, sequence int64) error {
	if start < 0 || end < start || end-start != int64(len(payload)) || sequence < 1 {
		return errors.New("invalid unified feed range")
	}
	return effects.publishEvent(key, unifiedjournal.Event{
		Kind: unifiedjournal.RecordOutput, Sequence: sequence, Start: start, End: end, Payload: payload,
	})
}

// WritePaneGeometry delivers a committed geometry event on the same ordered
// stream as output, which is the only way a subscriber can see it in the
// position the journal committed it at.
func (effects *UnifiedDevPaneEffects) WritePaneGeometry(key unifiedjournal.PaneKey, event unifiedjournal.Event) error {
	if event.Kind != unifiedjournal.RecordGeometry || event.Sequence < 1 {
		return errors.New("invalid unified geometry event")
	}
	return effects.publishEvent(key, event)
}

// publishEvent fans one committed event out to every live subscriber. A
// subscriber that has already advanced past this sequence is skipped; one that
// is behind it has missed an event and is closed rather than fed a gap; one
// whose buffer is full is wedged and is evicted rather than blocking the
// publication path — publication holds the realm-wide subscriber lock, so a
// single stalled reader must never hold back every other pane's live tail.
// Both closures are one typed outcome, subscriber_lagged, delivered through
// closeSubscriberLocked: the attachment that owns the subscriber ends with
// that reason, the browser classifies it reconnectable, and the client is
// rebuilt from snapshot+tail — which includes the very event that evicted it.
func (effects *UnifiedDevPaneEffects) publishEvent(key unifiedjournal.PaneKey, event unifiedjournal.Event) error {
	effects.subscriberMu.Lock()
	defer effects.subscriberMu.Unlock()
	// Only admitted runtime generations call publishEvent. Record their last
	// publication under subscriberMu; rotation and unit reap remove the old
	// generation's entry while holding the same lock, so this remains bounded
	// without consulting provider authority on the dispatch hot path.
	if effects.publishedSequence == nil {
		effects.publishedSequence = make(map[unifiedjournal.PaneKey]int64)
	}
	if event.Sequence > effects.publishedSequence[key] {
		effects.publishedSequence[key] = event.Sequence
	}
	for subscriber := range effects.subscribers[key] {
		if subscriber.cursor >= event.Sequence {
			continue
		}
		if subscriber.cursor != event.Sequence-1 {
			effects.closeSubscriberLocked(key, subscriber, proto.SubscriberClosedLagged)
			continue
		}
		if len(subscriber.data) == cap(subscriber.data) || !subscriber.lease.reserveEvent(recordingEventBytes(event)) {
			effects.closeSubscriberLocked(key, subscriber, proto.SubscriberClosedLagged)
			continue
		}
		delivered := event
		if len(event.Payload) != 0 {
			delivered.Payload = make([]byte, len(event.Payload))
			copy(delivered.Payload, event.Payload)
		}
		select {
		case subscriber.data <- delivered:
			subscriber.cursor = event.Sequence
		case <-subscriber.done:
			subscriber.releaseEvent(delivered)
			if effects.removeSubscriberLocked(key, subscriber) {
				close(subscriber.data)
				subscriber.drainClosed()
			}
		default:
			subscriber.releaseEvent(delivered)
			effects.closeSubscriberLocked(key, subscriber, proto.SubscriberClosedLagged)
		}
	}
	return nil
}

// removeSubscriberLocked removes one subscriber without publishing a provider
// verdict. It is used only when the subscriber has already cancelled itself.
// Caller holds subscriberMu.
func (effects *UnifiedDevPaneEffects) removeSubscriberLocked(key unifiedjournal.PaneKey, subscriber *unifiedDevSubscriber) bool {
	bucket := effects.subscribers[key]
	if _, present := bucket[subscriber]; !present {
		return false
	}
	delete(bucket, subscriber)
	if len(bucket) == 0 {
		delete(effects.subscribers, key)
	}
	return true
}

// closeSubscriberLocked is the one way the provider removes a subscriber it
// did not merely see cancel itself. It records the typed reason and only then
// closes the tail, so the consumer that observes the close can read the reason
// without a lock and carry it to the browser. Caller holds subscriberMu. A
// subscriber that is no longer registered has already been closed by its own
// cancel and is left alone: the channel is closed at most once.
func (effects *UnifiedDevPaneEffects) closeSubscriberLocked(key unifiedjournal.PaneKey, subscriber *unifiedDevSubscriber, reason proto.SubscriberCloseReason) {
	if edge := effects.subscriberCloseEdge; edge != nil {
		edge(key, subscriber, reason)
	}
	if !effects.removeSubscriberLocked(key, subscriber) {
		return
	}
	if !proto.IsSubscriberCloseReason(reason) {
		// A reason outside the closed set would reach the browser as an
		// unknown code and dead-end there; never invent one at a call site.
		reason = proto.SubscriberClosedLagged
	}
	subscriber.closeTyped(reason)
}

// closeSubscribersLocked closes every present subscriber and unconditionally
// removes its bucket. The unconditional delete is load-bearing: cancellation
// may have emptied a bucket immediately before a fatal settlement. Caller
// holds subscriberMu.
func (effects *UnifiedDevPaneEffects) closeSubscribersLocked(key unifiedjournal.PaneKey, reason proto.SubscriberCloseReason) int {
	closed := 0
	for subscriber := range effects.subscribers[key] {
		effects.closeSubscriberLocked(key, subscriber, reason)
		closed++
	}
	delete(effects.subscribers, key)
	return closed
}

// closeSubscribers removes every subscriber of one pane key with one typed
// reason. This is the shape a deliberate, provider-initiated close takes —
// journal rotation closes a predecessor generation's subscribers with
// generation_rotated through exactly this call — and returns how many
// subscribers it ended.
func (effects *UnifiedDevPaneEffects) closeSubscribers(key unifiedjournal.PaneKey, reason proto.SubscriberCloseReason) int {
	effects.subscriberMu.Lock()
	defer effects.subscriberMu.Unlock()
	return effects.closeSubscribersLocked(key, reason)
}

// openSnapshotTail returns the committed event projection and a live tail of the
// same representation. Snapshot and tail cannot drift, because they are the same
// events read at one instant under one lock.
//
// The returned tail is the subscriber itself: its events() channel closes when
// either side removes it, and closeReason() then tells the consumer whether
// that was its own cancel (empty) or the provider's typed verdict, which the
// attachment must end with.
func (effects *UnifiedDevPaneEffects) openSnapshotTail(sessionID string) ([]unifiedjournal.Event, unifiedjournal.Geometry, *unifiedDevSubscriber, func(), error) {
	return effects.openSnapshotTailWithLease(sessionID, nil)
}

func (effects *UnifiedDevPaneEffects) openSnapshotTailWithLease(sessionID string, attachment *recordingReaderLease) ([]unifiedjournal.Event, unifiedjournal.Geometry, *unifiedDevSubscriber, func(), error) {
	releaseAttempt := func(lease *recordingReaderLease) {
		lease.releaseSnapshot()
		if attachment == nil {
			lease.detach()
		}
	}
	// Storage reads deliberately run without subscriberMu. journalMu remains
	// held through final registration, so every commit is either in the snapshot
	// or reaches the registered tail. The final section re-resolves active and
	// compares the publication head while every publisher and rotation swap is
	// excluded; a winner in either race retries instead of registering a gap.
	const registrationAttempts = 128
	for attempt := 0; attempt < registrationAttempts; attempt++ {
		effects.subscriberMu.Lock()
		effects.mu.Lock()
		key, ok := effects.active[sessionID]
		effects.mu.Unlock()
		effects.subscriberMu.Unlock()
		if !ok {
			return nil, unifiedjournal.Geometry{}, nil, nil, errors.New("unified target is not journal-ready")
		}
		if !effects.recordingReady(key) {
			// The active generation can swap between lookup and receipt check.
			// Retry against its successor instead of reporting a transient gap.
			effects.mu.Lock()
			current, present := effects.active[sessionID]
			effects.mu.Unlock()
			if present && current != key {
				continue
			}
			return nil, unifiedjournal.Geometry{}, nil, nil, errRecordingInitial
		}

		if edge := effects.snapshotReadEdge; edge != nil {
			edge()
		}
		effects.journalMu.Lock()
		var events []unifiedjournal.Event
		var lease *recordingReaderLease
		var err error
		if !effects.realm.UnifiedEligible(key) {
			err = unifiedjournal.ErrInvalidated
		} else {
			var bytes int64
			bytes, _, err = effects.realm.SnapshotAllocation(key)
			if err == nil {
				if attachment == nil {
					lease, err = effects.readers.acquire(bytes)
				} else {
					lease = attachment
					err = lease.reserveSnapshot(bytes)
				}
			}
			if err == nil {
				events, err = effects.realm.ReadCommittedEvents(key)
			}
		}
		var initial unifiedjournal.Geometry
		if err == nil {
			initial, err = effects.realm.InitialGeometry(key)
		}
		cursor := int64(0)
		if len(events) != 0 {
			cursor = events[len(events)-1].Sequence
		}
		if edge := effects.snapshotRegisterEdge; edge != nil {
			edge()
		}

		effects.subscriberMu.Lock()
		effects.mu.Lock()
		activeKey, active := effects.active[sessionID]
		rotatingOld := effects.rotation != nil && effects.rotation.session == sessionID && effects.rotation.oldKey == key
		effects.mu.Unlock()
		if err != nil {
			releaseAttempt(lease)
			effects.subscriberMu.Unlock()
			effects.journalMu.Unlock()
			if active && (activeKey != key || rotatingOld) {
				time.Sleep(time.Millisecond)
				continue
			}
			return nil, unifiedjournal.Geometry{}, nil, nil, err
		}
		if !active {
			releaseAttempt(lease)
			effects.subscriberMu.Unlock()
			effects.journalMu.Unlock()
			return nil, unifiedjournal.Geometry{}, nil, nil, errors.New("unified target is not journal-ready")
		}
		if activeKey != key || effects.publishedSequence[key] > cursor {
			releaseAttempt(lease)
			effects.subscriberMu.Unlock()
			effects.journalMu.Unlock()
			time.Sleep(time.Millisecond)
			continue
		}
		if !effects.recordingReady(key) {
			releaseAttempt(lease)
			effects.subscriberMu.Unlock()
			effects.journalMu.Unlock()
			if rotatingOld {
				time.Sleep(time.Millisecond)
				continue
			}
			return nil, unifiedjournal.Geometry{}, nil, nil, errRecordingInitial
		}
		subscriber := &unifiedDevSubscriber{lease: lease, cursor: cursor, data: make(chan unifiedjournal.Event, recordingTailSlots), done: make(chan struct{}), verdict: make(chan struct{})}
		if effects.subscribers[key] == nil {
			effects.subscribers[key] = make(map[*unifiedDevSubscriber]struct{})
		}
		effects.subscribers[key][subscriber] = struct{}{}
		effects.subscriberMu.Unlock()
		effects.journalMu.Unlock()
		cancel := func() {
			subscriber.once.Do(func() { close(subscriber.done) })
			effects.subscriberMu.Lock()
			if effects.removeSubscriberLocked(key, subscriber) {
				close(subscriber.data)
				subscriber.drainClosed()
			}
			subscriber.lease.detach()
			effects.subscriberMu.Unlock()
		}
		return events, initial, subscriber, cancel, nil
	}
	return nil, unifiedjournal.Geometry{}, nil, nil, errors.New("unified snapshot registration did not stabilize")
}

// issueGuarded writes an already-guarded tmux command to the owning unit's own
// control connection and waits for its blocks to complete. It is the only way
// this product issues a geometry command for an observed pane: one connection,
// one boundary, no second observer and no polling. A session without a live
// unit refuses instead of queueing against a dead connection.
func (effects *UnifiedDevPaneEffects) issueGuarded(ctx context.Context, session string, args []string, blocks int) error {
	effects.mu.Lock()
	unit := effects.units[session]
	effects.mu.Unlock()
	if unit == nil {
		return fmt.Errorf("%w: unified observer is unavailable", errGeometryNotIssued)
	}
	result := make(chan unifiedDevCommandResult, 1)
	// Until the unit accepts the command nothing has been written to the
	// control connection, so every failure to hand it over is a proven
	// non-submission. Once accepted, the unit owns the write, and any later
	// failure or timeout is an unknown transport outcome — never a refusal.
	select {
	case unit.commands <- unifiedDevCommand{server: effects.server, args: append([]string(nil), args...), blocks: blocks, done: result}:
	case <-unit.done:
		return fmt.Errorf("%w: unified observer is unavailable", errGeometryNotIssued)
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", errGeometryNotIssued, ctx.Err())
	case <-time.After(5 * time.Second):
		return fmt.Errorf("%w: unified observer is unavailable", errGeometryNotIssued)
	}
	select {
	case outcome := <-result:
		return outcome.err
	case <-unit.done:
		return errors.New("unified observer is unavailable")
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return errors.New("unified observer command timed out")
	}
}

func (effects *UnifiedDevPaneEffects) paneKey(sessionID string) (unifiedjournal.PaneKey, bool) {
	effects.mu.Lock()
	defer effects.mu.Unlock()
	key, ok := effects.active[sessionID]
	return key, ok
}

// unifiedGeometryIssuer is the seam attachment.go's canonical resize transaction
// issues through for the configured unified-dev target.
type unifiedGeometryIssuer struct {
	provider *UnifiedDevPaneEffects
	registry *paneRegistry
	session  string
}

// guardedResizeBlocks is how many command blocks the guarded if-shell occupies on
// the control connection: the submission, and the deferred success command tmux
// queues once the guard passes. Established against real tmux by the Phase-1
// boundary proof; the completion of the last block is sequence N.
const guardedResizeBlocks = 2

// BeginGeometry reserves every capacity one geometry mutation needs, before
// any command reaches tmux, and types each failure under the RESIZE
// guarantee. The default is fatal: only a rejection that leaves this
// attachment's authority intact — a command budget with no slot to spare, a
// journal cap with no room for the record — is typed terminal.RefuseResize,
// because the operator can simply try again. A target that is no longer
// journal-active, a runtime that is gone, a pause that faulted the
// generation, or a journal that has invalidated this generation are verdicts
// on the attachment: nothing was mutated, but nothing later could succeed, so
// the attachment closes and a reopen re-mints.
func (issuer *unifiedGeometryIssuer) BeginGeometry(ctx context.Context) (geometryTicket, error) {
	if refuse := issuer.provider.geometryRefusal; refuse != nil {
		if err := refuse(issuer.session); err != nil {
			return nil, terminal.RefuseResize(err)
		}
	}
	key, ok := issuer.provider.paneKey(issuer.session)
	if !ok {
		return nil, errors.New("unified target is not journal-active")
	}
	runtime := issuer.registry.retention
	if runtime == nil {
		return nil, errors.New("unified retention runtime is unavailable")
	}
	owner, err := runtime.acquireGeometryOwner(key)
	if err != nil {
		if errors.Is(err, errGeometryPauseOwned) {
			return nil, terminal.RefuseResize(err)
		}
		return nil, terminal.RefuseResize(fmt.Errorf("geometry command budget: %w", err))
	}
	ownerHeld := true
	releaseOwner := func() {
		if ownerHeld {
			runtime.releaseGeometryOwner(key, owner)
			ownerHeld = false
		}
	}
	commitSlot, err := runtime.reserveSlot(key)
	if err != nil {
		releaseOwner()
		return nil, terminal.RefuseResize(fmt.Errorf("geometry command budget: %w", err))
	}
	releaseSlot, err := runtime.reserveSlot(key)
	if err != nil {
		runtime.releaseUnusedSlot(commitSlot)
		releaseOwner()
		return nil, terminal.RefuseResize(fmt.Errorf("geometry command budget: %w", err))
	}
	// faultSlot is the third reservation: if the command is issued and never
	// reaches a durable commit, the generation must be faulted (tmux may hold a
	// geometry the journal does not), and that fault must never be refused for
	// want of a command slot.
	faultSlot, err := runtime.reserveSlot(key)
	if err != nil {
		runtime.releaseUnusedSlot(commitSlot)
		runtime.releaseUnusedSlot(releaseSlot)
		releaseOwner()
		return nil, terminal.RefuseResize(fmt.Errorf("geometry command budget: %w", err))
	}
	releaseAll := func() {
		runtime.releaseUnusedSlot(commitSlot)
		runtime.releaseUnusedSlot(releaseSlot)
		runtime.releaseUnusedSlot(faultSlot)
		releaseOwner()
	}
	// startBoundary, never the blocking Boundary: the observer loop must keep
	// reading the control stream throughout, and a hold that stops ingestion
	// would deadlock the very command it is waiting for.
	started, err := runtime.startBoundary(key, "pause_start", false)
	if err != nil {
		releaseAll()
		return nil, terminal.RefuseResize(fmt.Errorf("geometry command budget: %w", err))
	}
	select {
	case err := <-started:
		if err != nil {
			// The pause flushed this pane's pending output and the journal
			// refused it: the generation has just failed closed. Not a refusal
			// of this request — the attachment's authority is gone.
			releaseAll()
			return nil, fmt.Errorf("geometry pause faulted the generation: %w", err)
		}
	case <-ctx.Done():
		// The pause_start is already queued and cannot be recalled: from the
		// moment startBoundary accepted it, something must own the matching
		// pause_end, or the late pause lands with no ticket to release it and
		// the pane holds output forever behind a "refused" Fit. Ownership here
		// is positional, not temporal: the manager queue is FIFO, so the
		// pause_end enqueued below — before this request returns — lands
		// strictly after its own pause_start AND strictly before any later
		// Fit's pause_start. Waiting for the pause to settle first and only
		// then submitting the end (from a detached completion) would open the
		// opposite hazard: a subsequent Fit's pause_start could interleave
		// (start1, start2, end1), and the late end would resume publication in
		// the middle of the new barrier. The caller still gets the typed
		// operational refusal — nothing reached tmux.
		// The existing runtime hook is test-only and lets the settled-race
		// regression test hold this exact edge until pause_start publishes its result.
		runtime.callHook("geometry_cancel_observed", key)
		select {
		case err := <-started:
			// The pause settled while cancellation was being observed. Honor
			// the settled verdict deterministically instead of racing on it:
			// a failed pause is the same fatal generation verdict as the
			// uncanceled path, with every slot returned — no pause applied,
			// so no pause_end is owed.
			if err != nil {
				releaseAll()
				return nil, fmt.Errorf("geometry pause faulted the generation: %w", err)
			}
		default:
			// Not yet settled. If the pause later applies, the queued
			// pause_end resumes it in order; if its flush fails first, the
			// pause never applies (pane.paused is never set, the fault is
			// classified on the manager loop) and the pause_end drains as a
			// harmless boundary on an unpaused pane. Either way nothing is
			// stranded, and a fatal verdict, when there is one, reaches the
			// caller on its next attempt.
		}
		// The release slot was reserved before the pause was queued for
		// exactly this submission, so ending the pause can never be denied
		// for want of capacity. submitBoundary fails only when the runtime is
		// closing: enqueue has then canceled the release reservation itself,
		// the FIFO queue still settles the already-accepted pause_start ahead
		// of the close command, and close tears down every pane — ownership
		// never dangles. No result is awaited: the buffered done channel
		// stalls nothing, and the strand window is precisely a manager
		// blocked mid-flush, which a canceled request must not wait behind.
		_, _ = runtime.submitBoundary(releaseSlot, key, "pause_end", false)
		// A future owner may enqueue only after this matching end is already in
		// the manager FIFO. If submission failed, runtime close owns teardown and
		// rejects every future owner.
		releaseOwner()
		runtime.releaseUnusedSlot(commitSlot)
		runtime.releaseUnusedSlot(faultSlot)
		return nil, terminal.RefuseResize(ctx.Err())
	}
	ticket := &unifiedGeometryTicketState{
		provider: issuer.provider, runtime: runtime, key: key,
		commitSlot: commitSlot, releaseSlot: releaseSlot, faultSlot: faultSlot,
		geometryOwner: owner,
	}
	ownerHeld = false
	// The journal's own capacity — logical record cost AND physical
	// append+commit cost — is reserved here, after the pause boundary has
	// flushed this pane's pending output (so the charge it is measured against
	// is current) and before any command reaches tmux. A journal that would
	// refuse the geometry refuses NOW, with tmux untouched, as one request's
	// outcome; once the command is issued, Commit cannot meet a cap. A journal
	// that has already invalidated this generation is not refusing a request:
	// it has no authority left to record one.
	var reservation *unifiedjournal.GeometryReservation
	var reserveErr error
	runtime.withJournalLock(func() {
		reservation, reserveErr = issuer.provider.realm.ReserveGeometry(key)
	})
	if reserveErr != nil {
		ticket.Release()
		if errors.Is(reserveErr, unifiedjournal.ErrQuota) {
			return nil, terminal.RefuseResize(reserveErr)
		}
		return nil, fmt.Errorf("geometry journal reservation: %w", reserveErr)
	}
	ticket.reservation = reservation
	return ticket, nil
}

type unifiedGeometryTicketState struct {
	provider      *UnifiedDevPaneEffects
	runtime       *retentionTrialRuntime
	key           unifiedjournal.PaneKey
	commitSlot    *retentionReservation
	releaseSlot   *retentionReservation
	faultSlot     *retentionReservation
	reservation   *unifiedjournal.GeometryReservation
	geometryOwner uint64
	// issued records that the guarded command reached the observer, or that
	// its outcome is unknown; committed records a durable geometry record.
	// Issued without committed is the post-mutation state: Release faults the
	// generation so no further output is published under a geometry the
	// journal cannot vouch for.
	issued    bool
	committed bool
	released  bool
}

func (ticket *unifiedGeometryTicketState) Issue(ctx context.Context, args []string) error {
	err := ticket.provider.issueGuarded(ctx, ticket.key.Session, args, guardedResizeBlocks)
	if err == nil || !errors.Is(err, errGeometryNotIssued) {
		ticket.issued = true
	}
	return err
}

func (ticket *unifiedGeometryTicketState) Commit(ctx context.Context, columns, rows int) error {
	reservation := ticket.reservation
	ticket.reservation = nil
	done, err := ticket.runtime.submitGeometry(ticket.commitSlot, reservation, ticket.key, unifiedjournal.Geometry{Columns: columns, Rows: rows})
	if err != nil {
		ticket.commitSlot = nil
		ticket.runtime.withJournalLock(reservation.Release)
		return err
	}
	ticket.commitSlot = nil
	select {
	case err := <-done:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	ticket.committed = true
	ticket.Release()
	return nil
}

// Release resumes publication exactly once, in the original order, whether the
// mutation committed, was refused, or failed. Without it an ordinary authority
// rejection would leave the pane holding output until a reservation failed and
// the generation silently died. An unconsumed journal reservation is refunded
// here too, so a refused or failed mutation leaves no capacity orphaned.
//
// A command that was issued but never durably committed faults the generation
// first: tmux may now hold a geometry the journal's last record contradicts,
// and every byte published after it would be rendered under the wrong grid.
// The fault is a queued boundary on its own pre-reserved slot, so it lands
// before the pause ends and cannot be refused for capacity. A commit failure
// has already faulted the generation on the manager loop; the second fault
// loses the CAS and is inert.
func (ticket *unifiedGeometryTicketState) Release() {
	if ticket.released {
		return
	}
	ticket.released = true
	if ticket.commitSlot != nil {
		ticket.runtime.releaseUnusedSlot(ticket.commitSlot)
		ticket.commitSlot = nil
	}
	if ticket.reservation != nil {
		reservation := ticket.reservation
		ticket.reservation = nil
		ticket.runtime.withJournalLock(reservation.Release)
	}
	if ticket.faultSlot != nil {
		faultSlot := ticket.faultSlot
		ticket.faultSlot = nil
		if ticket.issued && !ticket.committed {
			if done, err := ticket.runtime.submitBoundary(faultSlot, ticket.key, "geometry_fault", false); err == nil {
				select {
				case <-done:
				case <-time.After(10 * time.Second):
				}
			}
		} else {
			ticket.runtime.releaseUnusedSlot(faultSlot)
		}
	}
	done, err := ticket.runtime.submitBoundary(ticket.releaseSlot, ticket.key, "pause_end", false)
	ticket.releaseSlot = nil
	// The owner is released after pause_end is enqueued, never merely after
	// pause_start settles. That makes the unpaused-drain path structural: no
	// other same-pane pause owner can exist ahead of this end in the FIFO.
	ticket.runtime.releaseGeometryOwner(ticket.key, ticket.geometryOwner)
	ticket.geometryOwner = 0
	if err != nil {
		return
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

// forServer reports whether this provider observes the given server label.
func (effects *UnifiedDevPaneEffects) forServer(server string) bool {
	return effects.dev.Enabled && effects.dev.Server == server
}

// allows gates unified-dev attachment. The journal-active arm is consulted by
// session ID — the key `active` uses — so adopted sessions are attachable and
// a not-yet-observed session cannot ride a name. The configured-target arm
// keeps the development target itself admissible; name here is the revalidated
// session's exact current name (an equality check, never tmux prefix
// targeting), and an unobserved target still fails downstream at snapshot
// time rather than being served a dead journal.
func (effects *UnifiedDevPaneEffects) allows(server, sessionID, name string) bool {
	if !effects.forServer(server) {
		return false
	}
	effects.mu.Lock()
	_, active := effects.active[sessionID]
	effects.mu.Unlock()
	return active || name == effects.dev.Session
}

var _ PaneEffects = (*UnifiedDevPaneEffects)(nil)
var _ sessionBirthCreator = (*unifiedDevBirth)(nil)
