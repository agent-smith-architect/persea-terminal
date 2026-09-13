package unifiedjournal

import (
	"fmt"
	"sync"
)

// SourceID carries exact comparable host facts supplied by the broker. Journal
// records still contain only PaneKey; this is process-local admission ownership.
// Geometry, ControlGeneration and opaque incarnations are deliberately absent.
type SourceID struct {
	Realm, Server             string
	SocketPath                string
	SocketDevice, SocketInode uint64
	ServerPID                 int
	ServerStartTime           uint64
	Session, Window, Pane     string
	PanePID                   int
	PaneStartTime             uint64
}

var ErrSourceQuota = fmt.Errorf("%w: source recording allowance exhausted", ErrQuota)

// ReclaimRetiredSession retries proof-bearing cleanup for legacy quota-first
// callers. It shares reconstruction's strict recovered-owner guard and never
// retires a live generation. Source-aware adoption uses the single transaction
// in BeginReconstructedPaneForSource to retain slot/byte transfer guarantees.
// The broker holds its journal lock, as for other Realm I/O methods.
func (realm *Realm) ReclaimRetiredSession(key PaneKey) {
	realm.retention.enter()
	defer realm.retention.leave()
	if !realm.closed {
		realm.retireRecoveredForReconstructionLocked(key, SourceID{}, false)
		realm.sweepRetiredSessionLocked(key)
	}
}

func (realm *Realm) SourceQuotaEnabled() bool { return realm.sourceQuota != nil }

func (realm *Realm) reservedSource(key PaneKey) SourceID {
	if quota := realm.sourceQuota; quota != nil {
		quota.mu.Lock()
		defer quota.mu.Unlock()
		return quota.leases[key].source
	}
	return SourceID{}
}

type sourceJournalLease struct {
	source       SourceID
	materialized bool
}

type sourceJournalQuota struct {
	mu                    sync.Mutex
	perSource, maxSources int
	leases                map[PaneKey]sourceJournalLease
	used                  map[SourceID]int
	unknown               int
}

// ConfigureSourceQuota runs once during startup, while the caller owns the
// realm. Every surviving file reserves a complete generation allowance. Unknown
// files conservatively reduce every source's available allowance until exact
// attribution or proven unlink. The journal's byte/slot ledgers are unchanged.
func (realm *Realm) ConfigureSourceQuota(perSource, maxSources int, known map[PaneKey]SourceID) error {
	if realm.sourceQuota != nil || perSource < 2 || maxSources < 1 {
		return ErrInvalidRecord
	}
	quota := &sourceJournalQuota{perSource: perSource, maxSources: maxSources, leases: make(map[PaneKey]sourceJournalLease), used: make(map[SourceID]int)}
	for key, pane := range realm.panes {
		if pane.projectionReleased && pane.physical == 0 {
			continue
		}
		id := known[key]
		quota.leases[key] = sourceJournalLease{source: id, materialized: true}
		if id == (SourceID{}) {
			quota.unknown++
		} else {
			quota.used[id]++
		}
	}
	if len(quota.leases) > perSource*maxSources || len(quota.used) > maxSources {
		return ErrSourceQuota
	}
	realm.sourceQuota = quota
	return nil
}

// ReserveSource is independent of journal I/O. The full allowance is reserved
// before header creation; repeated calls for the same exact binding are inert.
func (realm *Realm) ReserveSource(key PaneKey, id SourceID) error {
	quota := realm.sourceQuota
	if quota == nil {
		return nil
	}
	quota.mu.Lock()
	defer quota.mu.Unlock()
	if id == (SourceID{}) {
		return ErrInvalidRecord
	}
	if lease, exists := quota.leases[key]; exists {
		if lease.source != id {
			return ErrInvalidated
		}
		return nil
	}
	if quota.used[id]+quota.unknown >= quota.perSource || len(quota.leases) >= quota.perSource*quota.maxSources || quota.used[id] == 0 && len(quota.used) >= quota.maxSources {
		return ErrSourceQuota
	}
	quota.leases[key] = sourceJournalLease{source: id}
	quota.used[id]++
	return nil
}

// AttributeRecoveredSource transfers an existing unknown liability after the
// broker proves its exact source. It never creates a lease or permits a write.
func (realm *Realm) AttributeRecoveredSource(key PaneKey, id SourceID) error {
	quota := realm.sourceQuota
	if quota == nil {
		return nil
	}
	quota.mu.Lock()
	defer quota.mu.Unlock()
	lease, exists := quota.leases[key]
	if !exists {
		return nil
	}
	if id == (SourceID{}) || lease.source != (SourceID{}) && lease.source != id {
		return ErrInvalidated
	}
	if lease.source == id {
		return nil
	}
	if quota.used[id] == 0 && len(quota.used) >= quota.maxSources {
		return ErrSourceQuota
	}
	quota.unknown--
	quota.used[id]++
	lease.source = id
	quota.leases[key] = lease
	return nil
}

// TransferSourceReservation completes a geometry-dependent journal key without
// acquiring a second allowance. Only an unwritten reservation may move.
func (realm *Realm) TransferSourceReservation(from, to PaneKey) error {
	quota := realm.sourceQuota
	if quota == nil || from == to {
		return nil
	}
	quota.mu.Lock()
	defer quota.mu.Unlock()
	lease, exists := quota.leases[from]
	if !exists || lease.materialized {
		return ErrInvalidated
	}
	if _, exists := quota.leases[to]; exists {
		return ErrInvalidated
	}
	delete(quota.leases, from)
	quota.leases[to] = lease
	return nil
}

// Claim creation before entering openat. Cancellation can then never refund
// the allowance while the file-creation syscall is still unsettled.
func (realm *Realm) sourceClaimCreate(key PaneKey) bool {
	quota := realm.sourceQuota
	if quota == nil {
		return true
	}
	quota.mu.Lock()
	defer quota.mu.Unlock()
	lease, exists := quota.leases[key]
	if !exists || lease.source == (SourceID{}) || lease.materialized {
		return false
	}
	lease.materialized = true
	quota.leases[key] = lease
	return true
}

func (quota *sourceJournalQuota) releaseLocked(key PaneKey) {
	lease, exists := quota.leases[key]
	if !exists {
		return
	}
	delete(quota.leases, key)
	if lease.source == (SourceID{}) {
		quota.unknown--
		return
	}
	quota.used[lease.source]--
	if quota.used[lease.source] == 0 {
		delete(quota.used, lease.source)
	}
}

func (realm *Realm) sourceUnlinked(key PaneKey) {
	if quota := realm.sourceQuota; quota != nil {
		quota.mu.Lock()
		quota.releaseLocked(key)
		quota.mu.Unlock()
	}
}

// CancelSourceReservation refunds only a header that never existed. Once a
// file exists, removePaneFile's unlink/ENOENT proof owns the release.
func (realm *Realm) CancelSourceReservation(key PaneKey) {
	if quota := realm.sourceQuota; quota != nil {
		quota.mu.Lock()
		if lease, exists := quota.leases[key]; exists && !lease.materialized {
			quota.releaseLocked(key)
		}
		quota.mu.Unlock()
	}
}

func (realm *Realm) SourceUsage(id SourceID) (used, unknown, limit, identities int) {
	if quota := realm.sourceQuota; quota != nil {
		quota.mu.Lock()
		defer quota.mu.Unlock()
		return quota.used[id], quota.unknown, quota.perSource, len(quota.leases)
	}
	return
}
