package unifiedjournal

// Writer binds production write authority to one admitted pane object. A key
// alone is insufficient after quiescent identity reclamation: a stale caller
// must neither create a missing generation nor write to a new object at a
// reused key. Calls use the Realm's existing external serialization.
type Writer struct {
	realm *Realm
	pane  *paneJournal
}

// SettleFailedOwner records the existing writer owner's final fault settlement.
// The caller must have revoked ingress and settled every command, reservation,
// initial operation and cleanup reference. This does not release any journal
// liability. The atomic handoff avoids taking the journal lock under the
// retention owner lock; a stale handle can mark only its original pane object.
func (writer *Writer) SettleFailedOwner() {
	if writer != nil && writer.pane != nil {
		writer.pane.failedOwnerSettled.Store(true)
	}
}

func (realm *Realm) BindWriter(key PaneKey) (*Writer, error) {
	pane := realm.panes[key]
	if pane == nil || !pane.admitted || pane.state != EligibilityContinuous || realm.closed {
		return nil, ErrInvalidated
	}
	return &Writer{realm: realm, pane: pane}, nil
}

func (writer *Writer) valid(key PaneKey) bool {
	return writer != nil && writer.pane != nil && writer.pane.key == key &&
		!writer.realm.closed && writer.realm.panes[key] == writer.pane &&
		writer.pane.admitted && writer.pane.state == EligibilityContinuous
}

func (writer *Writer) Append(key PaneKey, payload []byte) (Record, error) {
	if !writer.valid(key) {
		return Record{}, ErrInvalidated
	}
	// The production sequencer settles each record before submitting another.
	// Keep that ownership bound explicit; batched low-level Append remains a
	// separate API and can still commit several appends with one marker.
	if writer.pane.pendingCount != 0 {
		return Record{}, ErrInvalidRecord
	}
	return writer.realm.Append(key, payload)
}

func (writer *Writer) Sync(key PaneKey) error {
	if !writer.valid(key) {
		return ErrInvalidated
	}
	return writer.realm.Sync(key)
}

func (writer *Writer) AdvanceCommitted(key PaneKey, record Record) error {
	if !writer.valid(key) {
		return ErrInvalidated
	}
	return writer.realm.AdvanceCommitted(key, record)
}

// ReclaimRetired forgets a proven-unlinked tombstone after the caller has
// settled all operations that could use its key. Broker transaction owners
// call it after manager and dispatcher settlement. Writer handles remain
// invalid even if a later explicit admission reuses the key. Low-level Append
// retains its deliberate create-on-first-write compatibility; its callers
// must not use this quiescence assertion while they still have pending work.
func (realm *Realm) ReclaimRetired(key PaneKey) bool {
	pane := realm.panes[key]
	if pane == nil {
		return true
	}
	if !pane.slotRetired || pane.file != nil || !pane.projectionReleased ||
		pane.realmCharge != 0 || pane.physical != 0 || pane.reservedCharge != 0 || pane.physicalReserved != 0 ||
		pane.adoptionLogical != 0 || pane.adoptionPhysical != 0 || pane.rotationCapacityHeld ||
		pane.rotationRollbackReserved || pane.rotationForwardLogical != 0 || pane.rotationForwardPhysical != 0 {
		return false
	}
	delete(realm.panes, key)
	return true
}

func (realm *Realm) IdentityCount() int { return len(realm.panes) }

// Includes live, recovered and unsettled tombstones. Reclamation, not eviction,
// regains this capacity. This is separate from complete-pane write admission.
const MaxRealmIdentities = 128
