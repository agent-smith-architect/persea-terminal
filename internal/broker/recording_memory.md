# Recording memory qualification

Read this with the [ownership contract](recording_contract.md) and
[journal verification](../unifiedjournal/verification.md). Admission accounts
bound application-owned allocations. They do not make Go's collector, tmux,
the kernel, or a soft memory setting into hard allocation limits.

## Supported profile

| Scope | Capacity |
| --- | --- |
| Ordinary recorded sources | 8, plus one complete journal slot reserved for a successor |
| Runtime generations / journal identities | 64 / 128, including unsettled owners |
| Runtime Q / E | 32768 commands / 163840 envelope reservations |
| Per-source Q / E | 16384 / 32768, also subject to the global limits |
| Runtime B / O | 64 MiB input / 128 MiB feed credit; both charge the same accepted payload |
| Per-source B / O | 8 MiB / 16 MiB, shared by current and retiring generations |
| Command ring / outbox | 65667 / 163906 pointer cells |
| Shared reader and observer copies | 224 MiB |
| Reader / observer sublimits | 128 MiB / 128 MiB; full readers leave 96 MiB for observers |
| Founding/live/retiring observer units | 18, each with a 1536 KiB fixed allowance |
| Readers / public unified attachments | 64 / at most 27 after snapshot settlement |
| Reader tail | 64 buffered plus one in-flight event, at most 64 KiB per event |
| Journal logical / physical | 64 MiB / 72 MiB per realm, 8 MiB / 24 MiB per pane in the 96 MiB tmpfs profile |
| Service MemoryHigh / MemoryMax | 850 MiB / 1 GiB |

Physical limits are filesystem-derived. Tests on a larger host filesystem must
pin the 24/72 MiB profile; a test that silently inherits a larger filesystem
does not qualify this deployment. Increasing slots does not increase byte limits.
The ninth slot is a transactional reserve, not a ninth ordinary source.

The source Q/E limits are intentional independent shares. Reducing global E to
65536 rejected the established source-unbound 10000-small-output burst. E163840
preserves that workload while retaining the previous bounded source allowance.
A source-unbound benchmark does not promise that one production source can queue
10000 tiny events at once. Every real request must pass both source and global
admission, including lifecycle reservations.

## Runtime owner worksheet

The following arithmetic is for Go 1.26.5, linux/amd64. A toolchain or object
shape change requires requalification. Production code does not import Go runtime
internals. `TestRecordingProjectionAllocationEnvelope` checks the relevant
exported allocation classes and structure sizes; the isolated runtime calibration
prints the actual envelope, feed, field, command and reservation sizes.

For batch size K=65536, a zero-byte reservation costs E=9; a positive reservation
costs `9 + 7*ceil(n/K)`. At E163840 this gives at most 18204 live reservations,
10240 positive reservations and 23405 component/part occurrences. Global Q is
checked but is not the tighter bound on those live objects. Accounting refunds
B/O/E only after the manager has released Q and every part has actually settled.
B and O are incremented and refunded together, so their common live payload is
at most 64 MiB. Adding 64 and 128 MiB would count the same liability twice.

| Dominant retained owner | Conservative bytes |
| --- | ---: |
| Ring and outbox arrays | 1851392 |
| At most 36540 command objects | 9354240 |
| Result and shared-fence channels | 4376352 |
| Current and just-settled reservation objects | 3495168 |
| Parts/abort pointer arrays | 468100 |
| Pending/held component arrays and growth overlap | 10110960 |
| Two rounded copies of 64 MiB plus small-allocation slack | 168007675 |
| One assembly, just-settled feed and manager input tail | 8519680 |
| Qualified fixed maps, state, compact identities and first error | 2097152 |
| Envelopes, captured fields and typed feeds | 51326080 |
| Total | 259606799 (247.58 MiB; use 248 MiB) |

The supervision links and monotonic admission time increase each rounded reservation
allocation from 80 to 96 bytes. The E-derived current-plus-just-settled bound
increases by 582528 bytes; the rounded 248 MiB worksheet allowance still covers
this change. The existing fixed-state allowance also covers the two bounded
per-generation sample maps, fixed exit ring and one lifecycle worker. This is
an analytical shape update, not a new measured process high-water. No admission,
collector or service limit changes are implied.

This is a conservative runtime-only worksheet. It does not subtract GC or assume
that a refunded physical journal has consumed its outstanding callback copies.
Old keyless fences may fill the outbox while new reservations queue, so the full
physical outbox remains in the bound. Single-manager transfer, cleared held
prefixes and per-part failure settlement bound backing arrays without another
queue or lifetime mechanism.

The final diagnostic representation is a 112-byte envelope, 24-byte numeric field
and optional 160-byte feed. Eight captured fields occupy 192 bytes, so a mapped
diagnostic is at most 304 bytes. An output feed is 320 bytes and a geometry feed
352 bytes. The worksheet uses `304*(E+P+4) + 64*(floor(E/7)+2)`, conservatively
charging the larger optional feed only to funded feed occurrences. It never
assigns feed storage to every diagnostic. Field slices are exact-sized private
copies. Production producers are the explicit maps and field literals in
`retention_runtime.go`; none exceed eight fields, and all values are int64.
Callback maps exist only at the sole dispatcher; producer maps exist only at the
current manager/caller. Neither is retained in every queued envelope.

The 2 MiB fixed-state allowance is a qualified profile allowance, not a theorem
about every possible adversarial hash trajectory. Five actual runtime map types,
each with 64 live entries, were exercised with rolling, pinned-half and selected
hot-hash histories for two million transitions. They reached at most one
512-slot table; no directory split was observed. The allowance models two
1024-slot tables per map plus fixed objects and compact identity backing.
Deleting an entry does not imply shrinking a Go map. No periodic compactor was
added merely to force this allowance to match an estimate.

## Projection, readers and actual coexistence

Allocation-class enumeration covers every payload remainder through 32 KiB, then
uses the full-page inequality for larger records. Including event and payload
nodes, ordinary per-record commit projection is at most 4/3 of its physical
record cost. Valid dense recovery, with many appends and one commit, requires the
larger 12/5 bound. Tiny records and zero-byte geometry are included. Production
uses one unverified append per generation, at most 176 bytes of pending metadata
each, plus the single manager's current verification candidate. The low-level
multi-append API keeps its physical-ledger bound and compatibility; it does not
inherit the production one-pending assumption.

Retirement releases projection only after proven unlink. Snapshots are separate
copies: one event array and one payload slab, both reserved before allocation.
Readers can outlive the source generation and keep those copies charged through
the last backlog or in-flight write. Cancel, eviction and map removal do not
refund a held consumer. Snapshot copies therefore cannot be bounded by the
currently visible journal files.

Recovery marks clean surviving journals Legacy, not Continuous. A fresh broker
has no recovered reader leases. Production snapshot admission checks Continuous
eligibility before reserving/copying. A full recovered projection plus snapshots
read directly from those same Legacy journals is an allocation over-approximation,
not a reachable production state. Keep its GC result as a regression test rather than
using it to select service limits.

The mixed experiment retains 48 MiB of source-attributed Legacy records and
admits six current sources through the real nine-slot and 72 MiB physical rules.
Three completed post-commit rotation/fatal-cleanup transactions unlink predecessor
and successor journals while their earlier feeds remain blocked. Three current
sources retain almost-8-MiB partial input batches; the others queue input until
global B reaches 64 MiB. Real zero-byte boundary reservations fill E, alternating
control runs fill their actual object owners, and publication fills the complete
outbox. Current-only snapshot/tail readers fill the reader account. This proves
an important overlap; independent maxima still must not be added as if every
possible dense/current partition and source transition held at once.

The real PREPARE measurement parks `unifiedAttachmentFrameWriter.WriteFrame` at
its final downstream write. Maximum replay is 262144 bytes; the measured encoded
payload is 349686 bytes with capacity 352256. Framing writes a five-byte header
and the same payload, with no second full copy. Snapshot/backlog and encoder result
remain owned by their real call frames. Temporary strings, serializer buffers and
encoder-pool storage follow their actual Go lifetimes and remain in RSS until
the collector releases them. Keeping the entire cumulative 2 MiB PREPARE reserve
live as a synthetic byte array is a separate conservative lane, not a measurement
of those simultaneous live buffers.

## Reproducing the measurements

Use private disposable tmux and a fresh test process. Ordinary test runs skip the
capacity experiments. Useful gated tests are:

- `PERSEA_RECORDING_MEMORY_CALIBRATION=1`: outbox, serializer, projection and
  valid dense-recovery allocation tests.
- `PERSEA_RECORDING_COMBINED=1`: `TestRecordingCombinedOwnershipCalibration`.
  For the reachable mixed lane, also set `PERSEA_RECORDING_MIXED_FIXTURE` to a
  private two-pane dense fixture created by the recovery calibration. Set
  `PERSEA_RECORDING_PAD_READER_SCRATCH=1` only for the explicitly padded lane.
- `PERSEA_RECORDING_PROFILE=1`: `TestRecordingProfileEightSourceProduction` runs
  the unchanged nominal and burst manifest for sixty seconds each, using actual
  tmux chunking, decoder, source admission, journal and rotation paths.
- `PERSEA_RECORDING_CLIENTS=1`: `TestRecordingNativeClientOverlapCalibration`
  holds 18 control clients and 27 Epoch clients, records sustained source receipts,
  overlapping helper children, per-process memory and actual final reap.

Record exact source and compiled binary hashes before execution, command, cwd,
environment, timestamps and exit status. Keep failed variants and the unchanged
manifest hash. Repeated warmed saturation reports actual owner gauges, heap/RSS
high-water, Go accounted memory, stacks, collector CPU/limiter state and allocation
throughput. `GOMEMLIMIT` is only a soft GC policy; do not subtract garbage or pool
storage from RSS before actual reclamation. Include the full 96 MiB tmpfs term
and separately qualified native/file/kernel charges in the service envelope.

## Calibrated service setting

The prepared unified broker unit uses `GOMEMLIMIT=528MiB`, retaining the existing
850 MiB MemoryHigh and 1 GiB MemoryMax. With all 18 unit allowances and actual
blocked PREPARE writers, three warmed mixed rounds reached a 619544576-byte broker
RSS high-water (590.84 MiB). Retained heap was 430.50–430.59 MB across rounds;
each round also churned 491520000 capture bytes in 5.24–5.46 seconds. Collector
limiter activation occurred in this deliberately saturated lane and remains a
reported performance limit. Ordinary saturation reached 553881600 bytes without
limiter activation. The unchanged real eight-source nominal and burst lanes both
passed; source scheduling lateness stayed below 3.6 ms in that run. This is source
timing, not browser paint latency.

A separate delegated-cgroup experiment placed the broker and its children in the
service group from process birth, and its private source tmux server in another
group. It observed 18 control plus 27 Epoch clients and overlapping helper work.
The service peak, including its broker, was 124.242 MiB. Largest sampled native
private memory was 0.727 MiB; service kernel high-water was 17.426 MiB, including
page tables and kernel stacks. Those observations remain unchanged. The fixture
holds founding observers before their source-witness query, so its measured
child high-water does not include every analytically possible helper overlap.

An existing observer client can coexist with its unit's `buildSourceWitness`
tmux helper. Independent units execute those queries before journal-slot
admission, so the nine complete slots do not serialize them. The scoped planning
count includes each such helper as well as attachment and supervisor work:

| Native owner | Scoped count |
| --- | ---: |
| Observer/founding client | 18 |
| Concurrent source-witness helper per unit | 18 |
| Attachment client | 27 |
| Attachment transaction/cleanup helper | 27 |
| Separate supervisor/helper allowance | 1 |
| Total | 91 |

Using the existing sampled per-process and mapping/kernel terms gives
`91*0.75 + 14 + 32 = 114.25 MiB` before margin. The corrected external planning
allowance is 128 MiB, leaving 13.75 MiB above that subtotal. Shared mappings count
once; the kernel term retains broker page-table/stack charges without adding
them a second time. This supersedes the earlier 73-child/112-MiB allowance.

The corrected planning envelope is `590.84375 + 128 + 96 = 814.84375 MiB`,
leaving 35.15625 MiB to MemoryHigh and 209.15625 MiB to MemoryMax. This is a
measured broker high-water plus a revised analytical allowance and full tmpfs;
it is not a newly measured simultaneous peak or a universal process/RSS bound.
The application owner limits, sampled native memory, analytical overlap and soft
collector policy remain separate. Integrated Stage C/4 cgroup, lag/intermittent
network, source-health and sustained qualification is mandatory before production
acceptance. Toolchain, native build, geometry/mode or supported-profile changes
must repeat the relevant qualification. The padded cumulative-scratch
lane and the unreachable full-Legacy-plus-Legacy-snapshot lane remain recorded
as conservative GC regression tests; neither is hidden by subtracting uncollected bytes.

Native clients require separate accounting from their source tmux server.
The held-client test has shown flat broker-child private memory while the private
source server retained more than a GiB of anonymous memory. Post-close anonymous
pages alone do not distinguish outstanding buffers from allocator high-water.
The source-stall gate now exercises independent observer isolation, stopped finite
producers, repeated recovery, a responsive second client, unchanged source facts
and actual PID/lease settlement. It samples source peaks and repeated settled
windows. Stable post-stop memory together with absent observer processes and
successful resumed recording distinguishes surviving source state/allocator
capacity from a still-live observer backlog, without uniquely attributing heap
pages. Finite single-source cycles are not sustained-load or network qualification.
End-to-end verification must cover the source server under the complete
multiple-source workload, slow/intermittent network paths, and sustained load.
