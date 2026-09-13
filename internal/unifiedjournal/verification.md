# Verified journal projection

PUJ2 is the durable record format. `AdvanceCommitted` preserves append → sync →
commit-marker → sync ordering, then reads the exact new physical extent through
the injectable `journalOps.readAt` operation. `verifiedCursor` is separate from
the append position and all capacity charges. The first verification reads and
binds the header's exact encoding, key, broker incarnation, origin, and birth
geometry. Subsequent verifications start after the last verified commit marker.

`decodeHeader`, `acceptHeader`, and `verifiedCursor.next` implement the common
recovery and incremental acceptance rules. Every new append must have valid
framing, type, consecutive sequence, offsets, typed geometry, and payload hash.
Live verification also compares each record with its submitted append metadata.
Several appends may share one commit; all must verify. A valid final record alone
does not prove earlier records. The final commit must cover the exact requested
record and physical boundary.

Verification builds private candidate state. It publishes the committed frontier
and immutable view only after every check succeeds, then releases pending append
metadata. On failure the previous frontier and view remain unchanged and the
generation fails closed. Existing write-ahead sequencers feed only after success
and retain sticky failures. Realm operations still require their caller's existing
serialization; diagnostic atomics do not replace it or grant commit authority.

## Storage and lifetime

The committed index is a backward-linked chain of immutable event pages. Each
event owns a backward-linked chain of payload pages, each at most 32 KiB; small
payload pages hold exactly their input length. Appending links new pages to the
existing chain without copying it, growing a historical pointer array, or keeping
a second historical record map. Sequence numbers provide the ordered snapshot
index, including zero-byte output and geometry events.

Before verification, each successful append owns one pending metadata node.
These nodes are bounded by the existing physical ledger's per-record charge;
zero-byte output cannot evade that charge. Successful verification releases the
pending nodes. Failed generations keep their resources until their real lifecycle
owner releases them. Verified payload and index references belong to the pane;
retirement removes that ownership only under existing unlink and slot rules.
Adoption abort and rotation abort/fatal retain identity tombstones after unlink.
Once unlink is proven, these tombstones release projection and pending metadata;
committed-view reads explicitly return `ErrInvalidated`, with corruption errors
retaining precedence. Failed unlink keeps ownership and charges until retry.
`MaxRealmIdentities` bounds live, recovered and unsettled objects at 128. After
actual operation quiescence, `ReclaimRetired` removes only proven-unlinked objects
whose projection, pending metadata and all byte/reservation holds have settled.
The broker calls this assertion after its manager/dispatcher settlement boundary.
Existing caller-owned snapshot copies remain valid.

Production callers bind `Writer` to the exact admitted pane object. An old handle
cannot create a missing generation or write to a new object at a reused key.
This handle allows one unverified append at a time, matching the existing broker
sequencer's complete append/sync/verify/feed operation per 64 KiB batch or initial
chunk. `Realm.Append` deliberately remains the low-level compatibility API:
multiple appends can share one commit and each must still verify. The caller
must not assert quiescence for low-level key-based work that remains outstanding.

`ProjectionUsage` reports retained payload capacity, event/payload structure bytes,
and pending metadata structure bytes. It does not include allocator size-class
rounding, the fixed pane/cursor structures, or snapshot copies. Those remain
distinct owners in the combined broker budget. During verification both pending
metadata and candidate pages exist; admission must count that transient overlap.
No physical or logical capacity charge is refunded merely because verification
failed. This API does not establish an aggregate heap budget by itself.

`SnapshotAllocation` calculates the copied event array and contiguous payload
slab charge from the verified record count and byte frontier without copying.
`AllocationCharge` conservatively rounds small allocations to powers of two and
large allocations to 8 KiB pages; pointer-bearing arrays include a header allowance.
`ReadCommittedEvents` makes those caller-owned copies with isolated payload slice
capacities. A caller can retain or modify a snapshot without changing authority.
The broker reserves before copying and holds the charge through the real final
consumer, including retained backlog. Low-level snapshot callers own their own
admission policy. Snapshot work is proportional to history; ordinary commit
readback, index allocation and payload copying depend only on the new suffix and
fixed header work at the first commit. Ordinary attachment never scans the file.

## Recovery and integrity

Full validation remains mandatory on open/recovery. A torn final append is outside
committed authority and is ignored; a torn commit or invalid complete frame fails
closed. Fully validated geometry appends keep their logical charge even when a
later frame is corrupt. Recovery also preserves the last committed geometry in
its classification. Uncommitted payload pages are discarded after their charges
are reconstructed. Recovered generations remain legacy or untrusted under the
existing lifecycle policy and cannot resume as continuous writers.

The live view proves the bytes read back at commit time. Later changes to already
verified file bytes need not be detected by the next unrelated commit; its new
view still contains the original verified bytes. Full recovery detects that later
corruption. Private storage and `O_APPEND` do not authenticate or seal historical
bytes against every writer. Per-attachment scans and background scrubbing are not
part of this contract.

Initial-state receipts, readiness gates, aggregate source/reader admission,
independent lifecycle supervision, and post-reap diagnostics belong to the
[recording contract](../broker/recording_contract.md).
