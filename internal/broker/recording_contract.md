# Recording commit, readiness, and resource contract

This is the contract for recording commit verification, readiness, resource
ownership, and lifecycle supervision. The existing batching, order, and authority
contracts continue to apply. A verified journal commit alone does not establish
that the broker has completed the initial-state readiness transaction.

## Verified committed views

PUJ2 remains the storage format. A successful commit publishes an immutable view
made from verified journal readback, including exact record kind, sequence,
offsets, geometry, payload hash and commit coverage. Every newly covered append
must be verified, including multiple appends committed by one commit record and
zero-payload geometry. A failed write, sync, read, or validation cannot publish
affected content. Ordinary commit work must depend on the new suffix and bounded
fixed state, not the size of the retained prefix. Payload and index storage must
be bounded and must preserve snapshots while their actual owners retain them.

An immutable view proves the bytes verified at commit time. Later mutation of
previously verified file bytes
need not be detected at the next unrelated commit. Full validation remains
mandatory on opening/recovery. File ownership and O_APPEND do not seal historical
bytes against every possible writer. Ordinary snapshots use the verified view;
they do not rescan disk, and no background scrubber is provided.

The implementation is described in [journal verification](../unifiedjournal/verification.md):
`Realm.AdvanceCommitted` publishes `verifiedCursor` only after `verifyCommitted`
checks the new suffix through `journalOps.readAt`. `parseJournal` shares the same
decoder for full recovery, and `ReadCommittedEvents` copies the immutable view.

## Exact initial-state success

Internal admission permits initial work; it does not permit ready publication.
The new operation result must bind the exact witness/PaneKey, operation identity,
required byte/chunk coverage, birth geometry, and successfully verified committed
sequence. Each required chunk must have succeeded under the same live authority.
An old failure, discarded chunk, cancellation or supersession remains a failure
even if a later empty generic boundary returns nil. Blank-looking terminal state
is valid if its modes, cursor, geometry and required initial records are present.

Gate adoption and creation success, active-generation publication, and
`openSnapshotTail` on this outcome. Preserve rotation's predecessor-seal point of
no return and its exact authority recheck. `startBoundary` settles manager work;
`startDispatchFence` settles dispatcher effects. Neither alone is a receipt for
the specified initial records. Do not change generic boundary idempotence merely
to use it as a receipt.

`recordingInitialOperation` is the broker's common readiness transaction.
`beginInitial` reserves the entire immutable initial state and queues one manager
command. The manager writes every required output chunk through
`Sequencer.WriteRecord`, which returns an exact record only after append, first
sync, `AdvanceCommitted` (commit write, second sync, verified readback), and feed
admission all succeed. The receipt binds operation and generation object
identity, PaneKey, captured source, birth geometry, chunk count, payload bytes
and digest, final sequence and final byte frontier. Each chunk is checked; a
zero-byte operation requires a real verified sequence-one output record.
Diagnostic stage counters never grant success.

`publishInitial` consumes the receipt under registry/runtime authority locks;
the publication checks the exact router witness, live generation and operation,
failure state, and current request interest. Creation and adoption publish active
only in their owning provider transition. Rotation consumes the same receipt in
its existing atomic provider/registry transition. `openSnapshotTail` checks this
proof both before reading and at final tail registration. The terminal protocol
admits Control only after that snapshot's PREPARE/READY transition reaches live.
An active-map entry or journal header alone grants none of those transitions.
The operation also binds its observer unit. The reader sets explicit atomic
transport failure before sending its error, so readiness cannot outrun a known
read failure simply because the manager-completion select case won. This is
lifecycle state, independent of diagnostic progress samples or loop-exit timing.

Creation verifies its observer flags and owns byte zero from its creating
control connection. When that ordered stream has no output, the same observer
executes the seven-block adoption capture to establish actual screen, modes,
cursor, pending parser prefix and geometry. Empty observed output is not itself
a blank-state proof. If raw byte-zero output arrives before the capture boundary
it remains authoritative; otherwise the verified capture supplies the initial
state. Adoption and rotation use their existing ordered capture boundaries.
Each publisher rechecks the external source and its current owning unit before
consuming the receipt; rotation also retains its exact pre/post-seal checks.

During rotation's sealed-predecessor/active-swap interval, the transaction owns
the predecessor generation even if its runtime slot has retired. A snapshot may
consume that predecessor's already-published receipt while the exact old router
and transaction remain authoritative. This does not mint successor readiness;
the atomic swap still requires the successor's healthy complete receipt. It
preserves gap-free attachment races at the existing point of no return.

Waits keep the observer consuming the control stream and hold no lock needed by
the work being awaited. Snapshot cut/tail registration retains its existing
consecutive-sequence and generation-race guarantees. A caller deadline ends its
wait; accepted work, cancellation and resource settlement have separate owners.
`settleInitialOwned` cancels readiness, waits for manager completion, uses funded
generation cleanup to drain accepted partial live batches, and awaits the
dispatcher fence before storage owners remove journals or release reservations.
If Close wins fence admission, dispatcher completion is the settlement proof.
Concurrent settlement is once-only. Logical rotation Abort remains nonblocking;
the rotation storage owner performs settlement before journal rollback. After
predecessor seal and before publication, cancellation follows fatal settlement
and cannot roll back or publish fresh readiness. Rotation's `requestInterest`
reads the receipt's publication bit under its runtime lock. That same readiness
publication ends caller authority across the remaining driver, including pending
output. Pending work then belongs to the recording lifecycle. Caller cancellation
cannot revoke it; storage, transport, continuation and shutdown failures still can.

Settlement submission and dependency waits are separate. `startInitialSettlement`
only submits once-owned cleanup after manager completion. `waitRecording` accepts
an explicit dependency owner: `recordingSettlementStream` for the observer driver,
or nil for an external caller whose observer runs independently. The stream owns
one decoder and retains the first secondary failure while waiting for accepted
work and its fence; it starts no extra task. Adoption's `finishAdoption` has one
failure owner through receipt, final source recheck and readiness publication.
Rotation carries that stream into its registry transaction, rollback and fatal
cleanup. Even the restored predecessor's boundary uses the stream owner. The
external creation commit/abort paths alone use blocking `settleInitial`.

Dependency waits return the original typed failure and any secondary stream
failure; the lifecycle driver decides rollback or fatal settlement. A cancelled
rotation that loses a continuation while draining (for example its bounded
pending buffer overflows) uses fatal settlement instead of restoring a predecessor
with a missing suffix. Known fatal reaping uses the explicit fault entry point;
it does not read a unit's loop-exit result before that result is published.

## Stable source budget identity

Construct a new internal quota identity from the configured realm/server and the
exact `terminal.SourceWitness` fields: socket path/device/inode; server PID/start
time; session, window and pane IDs; pane PID/start time. The per-broker lifetime
provides the boot boundary. A persisted or cross-broker identity would also need
the existing `proto.Authority.BootID`/UID/selector incarnation proof.

Exclude recording ControlGeneration, columns and rows. Do not derive this from
display names, a bare tmux session ID, or `PaneKey.Incarnation`: the latter is an
opaque hash from `startupSourceIncarnation` which includes initial geometry.
`refitGenerationIncarnation` already demonstrates why geometry changes require
special handling for journal identity, while quota identity must remain stable.

`recordingSourceKey` carries those exact comparable facts. `bindSource` attaches
it before initial payload reservation. A provisional rollover inherits its
predecessor's account; the explicit post-capture witness must match that account
before bootstrap. A replacement socket/server/pane process has separate identity
without erasing the previous source's unsettled realm charges. An unbound payload
is refused. Unexpected predecessor output after seal still invalidates the
pending rotation's existing route evidence and takes its local fatal commit path.

Runtime source counters mirror the existing reservation transitions, rather than
owning a second independent lifetime. The account includes current, successor,
failed and retiring generations. Its defaults are 16384 commands, 32768 reserved
envelopes, 8 MiB ingress and 16 MiB outbox bytes, each capped by the realm's limit.
The source command/envelope limits preserve the independently bounded source
burst allowance; they do not scale down mechanically with a smaller global pool.
The global limits are 32768 commands and 163840 reserved envelopes. Both scopes
must admit every reservation, including the existing 2 MiB initial and 1 MiB
pending bounds. These counters alone do not establish a combined heap budget. Generation
lifetime commands also count; cleanup keeps its existing funded reserve.

| Owner | Acquire/transfer | Release |
| --- | --- | --- |
| Runtime generation | `ensureGeneration`, then `bindSource` or exact predecessor inheritance | Last reservation reference **and** last manager command, then existing retirement proof |
| Ordinary Q/E/B/O reservation | `reserveWithAdmission` atomically checks realm and source totals | Q at manager completion; E/B/O only after Q and every published or discarded part settle |
| Source account | First bound generation, at most the realm's 64 live generation owners | Last generation after all its credits settle; Close clears quiescent runtime state |
| Durable source allowance | `Realm.ReserveSource`, before header creation; reconstruction settles proven recovered owners before reserving its replacement; refit transfers an unwritten lease to the final key | Cancellation only before creation is claimed; after claim, proven unlink/ENOENT, including failed creation |

Durable ownership uses the same explicit tuple in `unifiedjournal.SourceID`, with
no serialization, hashing, new file format or parsing of opaque incarnations.
Each source may reserve two complete journal-generation allowances: current and
successor. Rollback stays within the predecessor's existing reservation. This
conservatively covers logical bytes, physical framing, slots and projection
metadata already bounded per generation; their existing realm ledgers remain
authoritative and unchanged. A held old file deliberately prevents a third
generation. Successful rotation and proven retirement regain the allowance.
Creation claims the lease before `openat`, so concurrent cancellation cannot
refund capacity from an unsettled syscall. An existing unexpected filename stays
charged conservatively until recovery or unlink proves settlement.

Startup configures at most 128 held/provisional leases for 64 sources. Proven
inventory matches associate surviving journals with their exact current source,
including geometry drift that makes replay ambiguous. This association grants
no replay authority. A valid non-live associated file may pass the full-slot
capture precheck; unknown and live current files may not.
Later association recomputes the existing header incarnation from explicit source
facts and that header's original geometry; it never parses the opaque value.
Every unknown recovered file consumes one allowance against **every** prospective
source until attribution or unlink. Attribution transfers existing ownership and
creates no allowance. Two unknown liabilities therefore quarantine new admission.
`BeginReconstructedPaneForSource` owns recovered supersession, slot transfer,
unlink, byte allowance and replacement source reservation in one existing
journal transaction. It selects only valid non-live recovered files with the
exact source association. It keeps one old slot, retires additional old slots,
and proves unlink before reserving the replacement source allowance and creating
its header. Failed unlink preserves both byte and source charges and refuses
readiness; the next attempt retries the same settled owners. A failed current
writer becomes a non-live reconstruction candidate only when its exact runtime
generation settles fault cleanup and every reservation/initial/dispatch owner.
Its object-bound writer records that fact without refunding a slot or bytes.
Fresh reconstruction rechecks the complete source identity and geometry/rotation
holds, then transfers that specific failed owner's slot only after proven
unlink. Runtime absence or an untrusted journal alone grants no authority.
Independently retained snapshots keep their existing lifetime; they neither
authorize reconstruction nor require a global zero-reader gate. Live current
files are never removed to create quota room. Legacy reconstruction and
`ReclaimRetiredSession` use the stricter existing exact-recovery incarnation
guard. The latter also retries terminal-generation unlink on ordinary quota
refusal. No path refunds before unlink/ENOENT. If startup exceeds the bounded
inventory, startup refuses; after storage recovery, restart/reconciliation can
prove and settle old files.

## Bounded global command queue

`retentionCommandQueue` is a fixed pointer ring. Push and pop perform one cell
write apiece; pop clears its reference. Commands preserve their original ticket
and acceptance age. The ordinary global FIFO, compound feed ordering, 64 KiB /
16 ms batching, single manager, single dispatcher and acknowledged timer model
are unchanged.

Content-free `Boundary` and `startBoundary` commands reserve only an existing
generation under the retention lock. They return `ErrInvalidated` for an absent
or retired key without creating a new lifetime, source binding, or P/Q/E/B/O
charge. The registry's failed-incarnation recheck preserves its idempotent
terminal-boundary outcome when fault cleanup races reservation. Explicit
generation creation, admitted payload reservations, and Close remain distinct.

For realm command limit Q and live generation limit P, `2*(Q+P+1)+1` cells cover
all producers. Ordinary commands have Q reservations, each live generation funds
one cleanup, and Close owns one marker. Between those commands and at the ends,
one adjacent control run combines keyless rejected-byte diagnostics and a shared
dispatcher fence. Alternating failed sources cannot create additional runs.
Funded commands separate runs, preserving accepted work order. Rejected-byte
totals remain exact; diagnostic callback granularity becomes one callback per run.
A fence guarantees completion of everything accepted before it, and may also
cover later diagnostics within its run. There is no per-request waiter list.

Closing is the only fence admission refusal. Ordinary saturation cannot exhaust
the reserved ring; capacity failure is an internal invariant violation, not a
second error silently routed to `dispatcherDone`. The fixed outbox, one manager
command and one dispatcher envelope also bound control-run storage when old fence
effects are held across generation retirement/reuse. No helper goroutines or
unbounded fallback queues supply capacity.

Each outbox envelope stores immutable numeric diagnostic fields captured at
publication and, when present, one typed feed. The dispatcher creates the public
callback map when delivering that envelope. Producer mutation and callback
mutation cannot change another observation. Diagnostics keep their original FIFO
position relative to compound feeds; callbacks still precede their feed, and
their actual return remains part of reservation settlement. Production diagnostic
producers use at most eight numeric fields. The representation does not impose a
new schema limit or add a second queue.

A failed later flush discards only the unpublished parts of its reservation.
Earlier feeds keep their payload and source/global credits through the blocked
callback. Failed held tails are cleared as they settle instead of assembling an
oversized failure batch. The runtime retains its first close cause, without an
ever-growing chain of retired-generation errors.

## Reader and snapshot ownership

`recordingReaderBudget` admits at most 64 readers and 128 MiB of charged reader
allocations across the provider. These are aggregate admission limits, not a
claim that the whole broker fits the combined target below. `openSnapshotTail`
reserves the copied event index, payload slab and serializer allowances before
calling `ReadCommittedEvents`, under the existing journal/snapshot seam. Array
charges include allocator rounding. A failed admission allocates no snapshot.

Each admitted reader reserves 528 KiB for its writer and fixed queue state,
a 64 KiB + 128 byte first-event allowance, and
an additional 2 MiB through PREPARE/backlog settlement.
PREPARE assembles at most 16 KiB of replay; a LIVE write carries at most 16 KiB
of output per encoding. The reserves above leave headroom beyond these bounds.
The initial event index and payload slab remain charged while any backlog suffix
can retain them. `releaseSnapshot` means the final consumer has finished; cancelling
the tail alone does not provide that proof.

The linked subscriber queue and events in flight share a 4 MiB + 64 KiB
allocation limit per reader, within the aggregate budget. Every node costs 128
bytes, covering its event, link, allocator header and size-class rounding;
payload allocation is charged separately. There is no independent event-count
limit: even zero-payload geometry costs a node, so at most 33,280 events can be
owned. A maximal 64 KiB output costs 65,664 bytes, allowing 64 such events.
Between PREPARE and COMMIT the writer does not drain the tail; thousands of
small records can fit without a count-only eviction. No records are coalesced,
so sequence-gap detection and geometry boundaries retain their exact order.
Both `subscriber_closed` and `subscriber_close_cut` log the eviction limit.
Publication reserves before
copying; receipt transfers ownership to the writer without refund. The writer
releases the event only after its actual write returns. Eviction and cancellation
close and drain buffered events, refunding only the events actually drained. A
concurrently dequeued event remains charged, including during a blocked write.
Reader count and fixed allowances release after detach, snapshot settlement and
the last event release. Closing a socket or removing a map entry is not used as
an allocation-settlement proof.

The public unified attachment acquires this same lease before constructing its
Epoch or reading its outer frame body. Its additional 4 MiB allowance covers
the existing attachment work and transport owners; this permits at most 27 such
attachments admitted sequentially with small snapshots: the twenty-eighth
funds its PREPARE workspace but cannot fund its 4 MiB attachment allowance.
The 27 settled attachments charge about 123.613 MiB before additional
tail/snapshot charges. There is no second independently refundable attachment
budget. Epoch work, PTY reader and process Wait each keep
their existing owner hold after cancellation or a timed-out Finalize.

When explicitly paired with `unifiedAttachmentFrameWriter`, the attachment's
ordinary tmux output is suppressed before capture/JSON/egress allocation. The
journal snapshot and tail already supply its output. The exact cut marker,
barrier, source/geometry, input, control and READY/COMMIT authorities stay in
Epoch. Classic attachments retain their capture and egress behavior. Suppression
requires the paired output-owning writer, so it cannot silently create an
attachment without an authoritative output source.

## Observer and capture ownership

The provider's reader and observer accounts share a 224 MiB copy ceiling under
one mutex. Readers may charge at most 128 MiB, leaving 96 MiB for observer work;
observer work may borrow unused reader space up to its own 128 MiB ceiling.
Acquisition and refund update both scopes at the original owner's existing edge.
The margin admits the maximum decoder record together with the supported initial
and continuation owners; exceeding the remaining capacity is an explicit
recording-capacity failure, not silent truncation or an uncharged allocation.

There are 18 founding/live/retiring observer unit owners: twice the eight-source
profile plus its spare complete slot. Each reserves 1536 KiB for its read buffer,
bounded normalized-read channel, producer/consumer chunks and fixed state.
Adoption reserves before its `has-session` subprocess; creation reserves before
argument copying or spawn. A replacement transfers its founding owner into the
existing unit lifecycle. Map removal, PTY close and sent kill signals do not
release it before the read goroutine and actual process Wait have settled.
Repeated replacement cannot bypass a stalled predecessor; exhaustion refuses
admission until an owner settles. The existing observer supervisor owns one bounded lifecycle work slot as described below.

The budgeted decoder reserves retained buffer growth and batch copies before
allocation. `EventBatch` owns its decoded data until Release, or Take explicitly
transfers that ownership. The 16 MiB record limit, 2 MiB initial state and 1 MiB
rotation continuation remain valid. Capture/control responses reserve builder
growth, parse indexes and dependent bootstrap copies before appending. Their
command owner remains through syscall/output-copy completion and the final parse
consumer. Command reset starts another response block; it is not a refund.

## Admitted writers and retired identities

Production `retentionTrial` selects `admittedWrites`; the manager binds one
`unifiedjournal.Writer` to the exact admitted pane object and retains that handle.
A missing key, failed generation or later object reusing the same key cannot be
written through that handle. Low-level `Realm.Append` keeps its explicit
create-on-first-write and multiple-appends-per-commit compatibility.

The production handle permits one unverified append per generation. This makes
the existing sequencer lifetime explicit: `enqueueComponent` retains the 64 KiB /
16 ms batching policy; `flushPane` writes the completed batch through
`Sequencer.WriteRecord`, which performs append, sync, verified commit and feed
before returning. Initial state also calls `WriteRecord` separately for each
64 KiB chunk, including all 32 chunks of a maximum 2 MiB bootstrap. Geometry
flushes preceding output, then appends, syncs and verifies on the same manager.
Every newly committed record is still verified. The guard adds no lock, queue,
goroutine, cross-generation dependency or different scheduling policy.

`MaxRealmIdentities` bounds live, recovered and unsettled journal objects at 128,
separately from complete-pane admission. `ReclaimRetired` is an explicit caller
assertion of operation quiescence after manager/dispatcher settlement. It removes
only a retired object with proven unlink, released projection/pending state, and
no remaining physical/logical, adoption, geometry or rotation reservations.
Failed unlink or an unsettled reservation retains the identity and its charges.
Production adoption and rotation cleanup invoke it after their existing settlement
boundaries. Caller-owned snapshot copies remain valid; stale exact writer handles
remain invalid even after a deliberate new admission at the same key.

## Combined resource budget

The workload manifest separates target budgets from enforced limits. Account
for actual retained objects and their owners before using a target budget as
an admission threshold. Include:

- Journal tmpfs pages and metadata, current/retiring overlap, committed payload
  pages and record indexes; count each shared page once for its actual lifetime.
- Accepted output and callback payloads, command/envelope structures, source and
  realm reservations, and separately funded lifecycle/fault/cleanup work.
- Snapshot builders, copied bytes or pinned pages, metadata and cancellation
  lifetime; aggregate and per-subscriber queued bytes, reader count and copies.
- Observer/control buffers, bounded lifecycle tasks, diagnostic state, stacks,
  allocator and GC headroom. Include tmpfs in cgroup memory, not only Go heap.

Current live journal limits are 8 MiB logical per pane and 64 MiB per realm;
physical limits derive independently. Deployment has a 96 MiB journal tmpfs,
850 MiB MemoryHigh, and 1 GiB MemoryMax. Runtime maxima are 64 generation
owners, 32768 commands, 163840 envelopes, 64 MiB ingress and 128 MiB outbox.
The fixed command ring has 65667 cells and the outbox has 163906 cells, including
their existing funded cleanup/close reserves. Complete journal admission defaults
to nine slots: eight ordinary sources and one successor reserve. Current,
recovered and retiring files retain their own physical/logical/source charges
until proven unlink. These independent maxima are not proof that their combined memory use fits.
The reader budget above covers copied snapshots, queued/in-flight events and
writer allowances. Config permits up to 256 front connections; that count cannot
substitute for reader admission. Observer buffers and capture/decoder copies,
verification candidates, allocator/GC behavior and fixed runtime structures still
require combined calibration. The detailed ownership worksheet and reproducible
measurement method are in [recording memory](recording_memory.md). A soft Go
memory setting is a GC policy, never an ownership cap or proof of total service
memory. The original 640 MiB planning target is not a product limit or a supported
safety claim.

## Progress, response, and eventual settlement

Independent progress samples must remain readable through a held journal lock,
storage operation or dispatcher callback. Detecting a stalled stage or expiring
a caller deadline does not cancel an arbitrary syscall/callback. No finite
cleanup guarantee is made without an actual cancellation/isolation contract.
Do not release credits, delete owned files, or accept late readiness while work
still owns them. Prove safe response and settlement as distinct tests.

The recording manager, dispatcher, verification owner and observer units publish
constant-size progress values. Monotonic process-relative nanoseconds drive
elapsed-time decisions; Unix timestamps remain diagnostic compatibility fields.
Each reservation keeps its original admission time through queued, pending,
held-batch and dispatched work. A bounded per-generation sample contains the
oldest surviving reservation and latest command/part completion. Intrusive links
are removed only at the existing actual release edge, so transfer never resets
age. Sample reads use a separate short state lock, never runtime/journal locks,
queue submission, storage I/O or callback completion. Idle generations have no
pending sample. Independently loaded stage values are diagnostic hints, not a
coherent accounting transaction or readiness authority.

Journal serialization publishes logical and physical ledger values before its
owner unlocks. Publication visits only the existing bounded journal identities,
retains one scalar sample per identity, and performs no I/O. Pressure reads copy
one sample under a separate short lock; the monotonic publication age remains
available while journal serialization is held. The last complete publication is
usable during a held operation, without pretending it contains unfinished work.

RunObserver samples every 50 ms. The initial local stall policy is two seconds
with pending accepted work and no command/part completion for that generation,
or two seconds in a non-idle local observer stage (startup, decoder delivery or
control/rotation work). Stream idle is exempt. Ordinary browser/network writes
are owned and bounded by the existing reader path and do not drive this cutoff.
These thresholds are a local failure-response policy, not syscall cancellation,
a source-memory cap or an end-to-end network deadline. A caller timeout and a
recording stall remain different outcomes.

The existing registry fault linearization is shared after an exact current or
owned-rotation generation check. Provider, registry and retention state locks order
initial publication against unit cutoff, including completed receipts before
provider registration. Cutoff first revokes publication; publication first exposes
its exact owned initial generation to fault and attachment sealing. Stale retired
samples cannot poison a successor.
The winner publishes its already-funded cleanup without joining a blocked fault
winner, journal owner or dispatcher. Exact attachment coordinators seal future
binds and call Epoch.Fault after releasing registry/coordinator locks. Terminal
publication seals and cancels the existing input pump before Done is visible;
queued input is discarded, future input refused, and a held active write retains
its credit until return. A successful partial write after cancellation does not
start another write. Join/close remain teardown work, never cutoff work.

The supervisor can kill only its own observer process to isolate an upstream tmux
backlog while its Go owner remains held. It does not wait for the held callback,
claim the process was reaped, or release any native/read/decoder owner. All
accepted work still settles through the initial-state driver and retention
owners. Original typed errors remain private and preserved; no late ready result
can restore a generation after the authoritative fault.

`TestRecordingNativeClientOverlapCalibration` measures this process/owner
distinction. Its transient peak is 45 native clients: 27 attachment clients,
one active observer and 17 founders held in readiness callbacks. Under pressure
the supervisor removes exactly those 17 founding client identities while all
28 others survive. The sustained measurement checks the surviving identities,
producer progress and memory for 30 seconds; all 18 observer units remain charged
while the callbacks are held. Attachment overload then leaves only the active
native observer. Releasing the callbacks settles the 17 founding owners, leaving
exactly the active unit; final shutdown must settle every owner and native client.
The transient peak is not a promise of 45 permanently unread native clients.

Slow rotation, spawn, source classification, reap and terminal-retirement retries
use one lifecycle work slot. RunObserver remains the decision owner and continues
sampling, accepting bounded requests and reporting admission refusal. At most 18
pending founding descriptors are retained; production transfers the already-funded
founding owner before enqueue. Descriptors contain existing bounded arguments and
identity, not output payload. Overflow refuses immediately. Reaps have priority;
spawn and rotation turns alternate when both are pending. One rotation remains
active. Canceled adoption requests and expired queued birth requests are rejected
before spawn and rechecked after native spawn; active native work retains its
lease through actual Wait. Shutdown refunds pending descriptors only; a held
active job remains owned until it returns. Late rotation completion checks its
exact state token and cannot rearm a stopped observer.

Durable retry timers mark the existing obligation and wake the one retention
manager. Terminal retry timers mark the existing obligation for the lifecycle
slot. Neither callback executes retirement I/O, and duplicate wakes coalesce.
A terminal-retirement record holds its unit lease through actual running
settlement; shutdown cannot refund an active retry. Retirement admission checks
the stopped flag under the provider lock before retaining a unit reference. Late
classification/reap carries its original canceled run context; shutdown retains
that context and cannot resurrect retry or recovery work using Background. The original manager,
dispatcher and batching timer acknowledgement bound remain unchanged.

The unit publishes its closed result after its loop has settled and before
process Wait. A fixed 36-entry, oldest-overwritten ring retains only the closed
cause, stage, exact PaneKey/control generation and monotonic exit time. A
supervisor cutoff also retains the first settled secondary closed cause. The
ring owns no unit, process, journal or callback and remains retrievable after
actual owner-map removal, independently of blocked dispatch. Unknown error text
is mapped to the closed generic stage-error cause.

Registry decisions still complete under registry.mu and perform cancellation
only after unlock. No state-lock edge may call journal I/O or join a callback.
The progress/cutoff paths preserve receipt, publication, cancellation
or rollback authority. Safe cutoff and actual once-only settlement are separate
behavioral gates.

## Reproducible qualification

Use `testdata/recording-workload-v1.json` with synthetic payloads and private tmux.
The burst calibration lane has explicit half-open schedule segments, phasing
relative to each segment start and continuous per-source payload indexes. Its
one-second 400-event/s segment plus 59 seconds at 40 events/s emits 22,080 events
across eight sources. `TestRecordingCorrectionBurstWorkloadSchedule` validates
the transition, counts, source phases and payload-index continuity.
History-work tests count positional reads. Adoption, creation, and
later-empty-boundary tests in `recording_initial_test.go` check the typed receipt
while preserving generic empty-boundary success. Real journal I/O faults at append,
first sync, commit write, second sync and readback are composed with broker
held/failed completion tests. A broker test also corrupts actual initial journal
bytes before commit to prove real adapter/readback failure propagation without
an intermediate feed or readiness publication. This composition does not claim
five independently injected broker end-to-end I/O cases.

Unit checks do not establish browser paint latency, bounded total memory, or
end-to-end performance. Validate those with the manifest's nominal and overload
workloads, fault injection, sustained runs, and the full repository test suite.
