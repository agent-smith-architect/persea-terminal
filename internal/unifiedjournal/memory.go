package unifiedjournal

import "unsafe"

// AllocationCharge is a conservative allocation size, not a heap measurement.
// Small allocations use a power-of-two ceiling, covering Go size classes;
// larger allocations round to runtime page size. Pointer-bearing callers add
// their allocator header before calling. This deliberately does not discount
// memory because a recent heap sample happened to be small.
func AllocationCharge(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	if bytes > 32<<10 {
		return (bytes + (8 << 10) - 1) &^ ((8 << 10) - 1)
	}
	charge := int64(16)
	for charge < bytes {
		charge *= 2
	}
	return charge
}

// SnapshotAllocation must be read under the same journal serialization as
// ReadCommittedEvents. It describes the latter's two allocations before they
// happen, including a pointer-array allocator header.
func (realm *Realm) SnapshotAllocation(key PaneKey) (bytes, records int64, err error) {
	pane, err := realm.committedEventsPane(key)
	if err != nil {
		return 0, 0, err
	}
	records = pane.committedSequence
	if records != 0 {
		bytes = AllocationCharge(records*int64(unsafe.Sizeof(Event{})) + 8)
	}
	return bytes + AllocationCharge(pane.committed), records, nil
}
