package broker

import (
	"errors"

	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

// A source account survives generation changes. The explicit immutable host
// facts are its identity; geometry and both opaque/public incarnations are not.
type recordingSourceKey struct {
	Realm, Server         string
	Socket                terminal.SocketIdentity
	ServerProcess         terminal.ProcessWitness
	Session, Window, Pane string
	PaneProcess           terminal.ProcessWitness
}

func recordingSourceIdentity(realm, server string, source terminal.SourceWitness) (recordingSourceKey, error) {
	key := recordingSourceKey{realm, server, source.Socket, source.Server, source.SessionID, source.WindowID, source.PaneID, source.Pane}
	if realm == "" || server == "" || source.Socket.Path == "" || source.Socket.Device == 0 || source.Socket.Inode == 0 || source.Server.PID <= 0 || source.Server.StartTime == 0 || source.SessionID == "" || source.WindowID == "" || source.PaneID == "" || source.Pane.PID <= 0 || source.Pane.StartTime == 0 {
		return recordingSourceKey{}, errRecordingInitial
	}
	return key, nil
}

func (key recordingSourceKey) journalID() unifiedjournal.SourceID {
	return unifiedjournal.SourceID{
		Realm: key.Realm, Server: key.Server,
		SocketPath: key.Socket.Path, SocketDevice: key.Socket.Device, SocketInode: key.Socket.Inode,
		ServerPID: key.ServerProcess.PID, ServerStartTime: key.ServerProcess.StartTime,
		Session: key.Session, Window: key.Window, Pane: key.Pane,
		PanePID: key.PaneProcess.PID, PaneStartTime: key.PaneProcess.StartTime,
	}
}

type recordingSourceUsage struct {
	commands, envelopes int
	ingress, outbox     int64
}
type recordingSourceAccount struct {
	key         recordingSourceKey
	generations int
	usage       recordingSourceUsage
}

// Preserve the supported per-source burst allowance independently of the
// aggregate pool: a source may borrow spare capacity but cannot monopolize it.
// The aggregate and source checks must both succeed. The 8 MiB ingress allowance
// includes a 2 MiB bootstrap and 1 MiB rollback/pending capture with room for
// accepted current-generation work. These are admission limits, not a heap sum.
func (runtime *retentionTrialRuntime) sourceLimit() recordingSourceUsage {
	limit := runtime.options.sourceLimits
	if limit.commands == 0 {
		limit.commands = min(runtime.options.maxCommands, 16384)
	}
	if limit.envelopes == 0 {
		limit.envelopes = min(runtime.options.maxEnvelopes, 32768)
	}
	if limit.ingress == 0 {
		limit.ingress = min(runtime.options.maxIngressBytes, retentionDefaultIngressLimit/8)
	}
	if limit.outbox == 0 {
		limit.outbox = min(runtime.options.maxOutboxBytes, retentionDefaultOutboxLimit/8)
	}
	return limit
}

func (usage recordingSourceUsage) fits(limit recordingSourceUsage) bool {
	return usage.commands <= limit.commands && usage.envelopes <= limit.envelopes && usage.ingress <= limit.ingress && usage.outbox <= limit.outbox
}

func (usage recordingSourceUsage) plus(other recordingSourceUsage) recordingSourceUsage {
	return recordingSourceUsage{usage.commands + other.commands, usage.envelopes + other.envelopes, usage.ingress + other.ingress, usage.outbox + other.outbox}
}

// sourceChargeLocked mirrors an existing reservation transition, never a new
// lifetime. Provisional generations retain their usage until exact binding.
func (runtime *retentionTrialRuntime) sourceChargeLocked(generation *retentionGeneration, commands, envelopes int, ingress, outbox int64) {
	delta := recordingSourceUsage{commands, envelopes, ingress, outbox}
	generation.sourceUsage = generation.sourceUsage.plus(delta)
	if generation.source != nil {
		generation.source.usage = generation.source.usage.plus(delta)
	}
}

func (runtime *retentionTrialRuntime) bindSource(key unifiedjournal.PaneKey, source terminal.SourceWitness) error {
	if runtime.options.sourceRealm == "" {
		return nil
	}
	identity, err := recordingSourceIdentity(runtime.options.sourceRealm, key.Server, source)
	if err != nil || key.Server != runtime.options.sourceServer || key.Session != source.SessionID || key.Window != source.WindowID || key.Pane != source.PaneID {
		return errRecordingInitial
	}
	generation, created, err := runtime.ensureGeneration(key, false)
	if err != nil {
		return err
	}
	runtime.mu.Lock()
	if runtime.generations[key] != generation || runtime.closing || runtime.closed || generation.failed || generation.abandoned || generation.lifetimeReleased {
		runtime.mu.Unlock()
		runtime.releaseUnadmitted(key, created)
		return unifiedjournal.ErrInvalidated
	}
	if generation.source != nil {
		valid := generation.source.key == identity
		runtime.mu.Unlock()
		if !valid {
			return errRecordingInitial
		}
		return nil
	}
	account := runtime.sources[identity]
	if account == nil {
		if len(runtime.sources) >= runtime.options.maxPanes {
			runtime.mu.Unlock()
			runtime.releaseUnadmitted(key, created)
			return unifiedjournal.ErrSourceQuota
		}
		account = &recordingSourceAccount{key: identity}
	}
	if !account.usage.plus(generation.sourceUsage).fits(runtime.sourceLimit()) {
		runtime.mu.Unlock()
		runtime.releaseUnadmitted(key, created)
		return unifiedjournal.ErrSourceQuota
	}
	if runtime.sources == nil {
		runtime.sources = make(map[recordingSourceKey]*recordingSourceAccount)
	}
	runtime.sources[identity] = account
	account.generations++
	account.usage = account.usage.plus(generation.sourceUsage)
	generation.source = account
	runtime.mu.Unlock()
	return nil
}

func (runtime *retentionTrialRuntime) releaseSourceLocked(generation *retentionGeneration) {
	if account := generation.source; account != nil {
		account.generations--
		generation.source = nil
		if account.generations == 0 {
			delete(runtime.sources, account.key)
		}
	}
}

// A validated rollover borrows the predecessor's exact source account while
// provisional. startInitial still requires the new explicit witness to match
// that binding; geometry and control-generation changes cannot mint credit.
func (runtime *retentionTrialRuntime) inheritSourceLocked(predecessor, successor *retentionGeneration) bool {
	if runtime.options.sourceRealm == "" {
		return true
	}
	account := predecessor.source
	if account == nil {
		return false
	}
	if successor.source != nil {
		return successor.source == account
	}
	if !account.usage.plus(successor.sourceUsage).fits(runtime.sourceLimit()) {
		return false
	}
	account.generations++
	account.usage = account.usage.plus(successor.sourceUsage)
	successor.source = account
	return true
}

// journalSourceIdentity proves recovery association without admitting another
// generation. Reconstruction can therefore settle its old owners inside the
// existing journal transaction before requesting a fresh allowance.
func (effects *UnifiedDevPaneEffects) journalSourceIdentity(key unifiedjournal.PaneKey, source terminal.SourceWitness) (unifiedjournal.SourceID, error) {
	if !effects.realm.SourceQuotaEnabled() {
		return unifiedjournal.SourceID{}, nil
	}
	identity, err := recordingSourceIdentity(effects.cfg.Realm, effects.server.Label, source)
	if err != nil {
		return unifiedjournal.SourceID{}, err
	}
	id := identity.journalID()
	for _, recovery := range effects.startupRecovery {
		if recovery.Key.Server == key.Server && recovery.Key.Session == source.SessionID && recovery.Key.Window == source.WindowID && recovery.Key.Pane == source.PaneID && recovery.Key.Incarnation == startupSourceIncarnation(source.Socket, source.Server, source.SessionID, source.WindowID, source.PaneID, source.Pane, recovery.Initial.Columns, recovery.Initial.Rows) {
			if err := effects.realm.AttributeRecoveredSource(recovery.Key, id); err != nil {
				return unifiedjournal.SourceID{}, err
			}
		}
	}
	return id, nil
}

// Birth and rotation reserve before mutating the live source. Adoption instead
// passes the proven identity to BeginReconstructedPaneForSource, whose existing
// owner settles recovered liabilities and acquires the new allowance atomically.
func (effects *UnifiedDevPaneEffects) reserveJournalSource(key unifiedjournal.PaneKey, source terminal.SourceWitness) error {
	id, err := effects.journalSourceIdentity(key, source)
	if err != nil {
		return err
	}
	err = effects.realm.ReserveSource(key, id)
	if errors.Is(err, unifiedjournal.ErrSourceQuota) {
		// A previous failed unlink must be retryable before a new allowance
		// exists. Reuse the same terminal-generation sweep as admission and
		// rotation; live or still-unlinkable files retain their full charge.
		effects.journalMu.Lock()
		effects.realm.ReclaimRetiredSession(key)
		effects.journalMu.Unlock()
		err = effects.realm.ReserveSource(key, id)
	}
	return err
}
