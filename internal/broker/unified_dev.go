// Unified provider configuration, startup inventory, and session projection.
package broker

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

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
