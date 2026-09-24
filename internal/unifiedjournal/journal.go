// Package unifiedjournal implements the feature-off, volatile byte authority
// used by the unified-terminal canary. Journal payloads are never included in
// public status or error values.
package unifiedjournal

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
)

const (
	DefaultPaneCapBytes  int64 = 8 << 20
	DefaultRealmCapBytes int64 = 64 << 20

	// AdoptionBootstrapCapBytes and AdoptionBootstrapCapRecords are the fixed
	// successor-bootstrap replay shape required by the journal-rotation
	// contract. The broker coalesces 2 MiB into at most 32 64-KiB records.
	// Exporting both dimensions lets the broker and journal pin the same shape;
	// bytes alone cannot bound append+commit framing.
	AdoptionBootstrapCapBytes   int64 = 2 << 20
	AdoptionBootstrapCapRecords int64 = 32
	// RotationPendingCapBytes and RotationPendingCapRecords are the fixed
	// predecessor-rollback and successor-tail replay shape: 1 MiB in at most
	// 16 64-KiB records.
	RotationPendingCapBytes   int64 = 1 << 20
	RotationPendingCapRecords int64 = 16

	// Keep package-internal spellings while existing package tests and the
	// broker share these limits through the exported contract.
	adoptionBootstrapCapBytes   = AdoptionBootstrapCapBytes
	adoptionBootstrapCapRecords = AdoptionBootstrapCapRecords
	rotationPendingCapBytes     = RotationPendingCapBytes
	rotationPendingCapRecords   = RotationPendingCapRecords

	rotationHeaderPhysicalReserve int64 = 8 + headerLimit

	// Physical budget defaults. The logical caps above bound what a generation
	// MEANS (payload bytes and geometry events); they never bounded what it
	// OCCUPIES, because every output record carries appendFixed+commitFixed
	// bytes of framing the logical charge does not count. At one-byte writes
	// the 8 MiB logical pane allowance is about 1 GiB of file, and even the
	// measured production ratio (~2.7x) exhausts a 96 MiB runtime tmpfs before
	// the realm's logical cap. The physical ledger bounds occupancy directly.
	//
	// DefaultPanePhysicalMultiplier sizes a generation's on-disk allowance as
	// a multiple of its logical cap: 3x admits the measured production framing
	// ratio with the logical cap still the first to bind, while a framing-heavy
	// pane (tiny writes) is stopped long before it can reach the filesystem.
	DefaultPanePhysicalMultiplier int64 = 3
	// DefaultPhysicalFraction is the share of the runtime directory's
	// filesystem the realm may occupy when no explicit cap is configured; the
	// remainder is headroom for the filesystem's own metadata and any
	// co-tenant, so journal growth can never be what produces ENOSPC.
	DefaultPhysicalFraction = 0.75
	// DefaultPhysicalHeadroomBytes is the absolute minimum left free on that
	// filesystem, whichever of fraction or headroom is the tighter bound.
	DefaultPhysicalHeadroomBytes int64 = 8 << 20
)

var (
	ErrUnsafeRuntime  = errors.New("unsafe journal runtime")
	ErrQuota          = errors.New("journal quota exceeded")
	ErrInvalidated    = errors.New("journal authority invalidated")
	ErrInvalidRecord  = errors.New("invalid journal commit record")
	ErrCorruptJournal = errors.New("corrupt journal")
	ErrStorage        = errors.New("journal storage failure")
	// ErrRotationReplayRecordQuota is a typed, non-fatal refusal: the replay
	// still has byte capacity, but its exported record shape is exhausted.
	ErrRotationReplayRecordQuota = fmt.Errorf("%w: rotation replay record cap exceeded", ErrQuota)
	// ErrPhysicalQuota is the physical ledger's refusal. It wraps ErrQuota so
	// every caller that already refuses on the logical cap refuses identically
	// on the physical one: before any mutation, as one request's outcome.
	ErrPhysicalQuota = fmt.Errorf("%w: physical journal budget exhausted", ErrQuota)
)

type PaneKey struct {
	Server            string
	Session           string
	ControlGeneration uint64
	Window            string
	Pane              string
	Incarnation       string
}

// Geometry is a pane's cell grid. It is durable truth: the browser never
// supplies it, and no pane payload can produce one.
type Geometry struct {
	Columns int `json:"columns"`
	Rows    int `json:"rows"`
}

// maxJournalDimension is a structural sanity bound only. The product's narrower
// row-fit policy is enforced at the broker's unified-dev authority boundary,
// where the exact pane witness is available; the journal must not silently
// widen or narrow it.
const maxJournalDimension = 1000

func (geometry Geometry) valid() bool {
	return geometry.Columns >= 1 && geometry.Columns <= maxJournalDimension &&
		geometry.Rows >= 1 && geometry.Rows <= maxJournalDimension
}

// GenerationOrigin records how a generation's first bytes came to exist. A
// birth generation holds the pane's bytes from byte zero; a reconstructed one
// opens with a capture-equivalent bootstrap synthesized at adoption time; a
// rotated one is the transactionally swapped successor of a live generation.
// Origin never changes replay behavior; it is honesty metadata for
// projections. The set is closed: an unknown origin is a corrupt journal.
type GenerationOrigin string

const (
	OriginBirth         GenerationOrigin = "birth"
	OriginReconstructed GenerationOrigin = "reconstructed"
	// OriginRotated is deliberately a new member of the closed header set. An
	// old binary does not know it and therefore fails the generation closed;
	// rotation metadata is never guessed into birth or reconstruction.
	OriginRotated GenerationOrigin = "rotated"
)

// RecordKind types a committed record. The set is closed: an unknown kind is a
// corrupt journal, never a record to skip.
type RecordKind uint8

const (
	RecordOutput   RecordKind = 1
	RecordGeometry RecordKind = 2
)

type Record struct {
	Key      PaneKey
	Kind     RecordKind
	Sequence int64
	Start    int64
	End      int64
	Hash     [sha256.Size]byte
	Geometry Geometry
}

// Event is one element of the committed event projection. Live tail and replay
// use this same representation, so they cannot drift.
type Event struct {
	Kind     RecordKind
	Sequence int64
	Start    int64
	End      int64
	Geometry Geometry
	Payload  []byte
}

type OpenOptions struct {
	RuntimeDir        string
	Realm             string
	BrokerIncarnation string
	UID               int
	GID               int
	DirectoryMode     os.FileMode
	FileMode          os.FileMode
	PaneCapBytes      int64
	RealmCapBytes     int64
	// PhysicalCapBytes bounds the realm's on-disk footprint: headers, record
	// framing, payload, commit frames, and corrupt or torn files alike. Zero
	// derives it at open from the runtime directory's filesystem size as
	// min(size x PhysicalFraction, size - PhysicalHeadroomBytes).
	PhysicalCapBytes int64
	// PanePhysicalCapBytes bounds one generation's on-disk footprint. Zero
	// keeps PaneCapBytes x DefaultPanePhysicalMultiplier.
	PanePhysicalCapBytes int64
	// PhysicalFraction and PhysicalHeadroomBytes shape the derived realm cap;
	// zero keeps the package defaults. They are ignored when PhysicalCapBytes
	// is explicit.
	PhysicalFraction      float64
	PhysicalHeadroomBytes int64
	// CompletePaneSlots overrides the retention ledger's complete-pane slot cap
	// for this realm; zero keeps the default. Births and adoptions consume from
	// the same ledger, so this is the adoption-era sizing knob.
	CompletePaneSlots int64
	// EligibilityWake is an identity-free, non-blocking hint emitted after a
	// complete-pane slot or proven-unlink capacity refund becomes available.
	// It never runs under the journal's retention/accounting locks.
	EligibilityWake func()
	// StartupRecovery makes recovered files conservatively occupy complete-pane
	// slots until an authoritative inventory decision reconciles each one. It is
	// an internal broker policy switch, not operator configuration.
	StartupRecovery bool
	retention       *retentionPolicy
}

type RecoveryDisposition uint8

const (
	RecoveryKeepExact RecoveryDisposition = iota + 1
	RecoveryRetireAbsent
	RecoveryRetireReplacement
	RecoveryRetainAmbiguous
)

func (disposition RecoveryDisposition) String() string {
	switch disposition {
	case RecoveryKeepExact:
		return "keep_exact"
	case RecoveryRetireAbsent:
		return "retire_absent"
	case RecoveryRetireReplacement:
		return "retire_replacement"
	case RecoveryRetainAmbiguous:
		return "retain_ambiguous"
	default:
		return "invalid"
	}
}

type RecoveryOutcome string

const (
	RecoveryScanned           RecoveryOutcome = "scanned"
	RecoveryKeptExact         RecoveryOutcome = "kept_exact"
	RecoveryRetiredAbsent     RecoveryOutcome = "retired_absent"
	RecoveryRetiredStale      RecoveryOutcome = "retired_replacement"
	RecoveryRetainedAmbiguous RecoveryOutcome = "retained_ambiguous"
	RecoveryRetainedCorrupt   RecoveryOutcome = "retained_corrupt"
	RecoveryRetainedError     RecoveryOutcome = "retained_error"
)

type RecoveryDecision struct {
	Key         PaneKey
	Disposition RecoveryDisposition
}

type Recovery struct {
	Key              PaneKey
	Committed        int64
	Eligibility      EligibilityState
	Reason           InvalidationReason
	Decision         RecoveryDisposition
	Outcome          RecoveryOutcome
	Initial          Geometry
	Final            Geometry
	RetainedLogical  int64
	RetainedPhysical int64
	RetainedSlot     bool
	RetryNeeded      bool
}

type paneJournal struct {
	key                      PaneKey
	file                     *os.File
	end                      int64
	committed                int64
	verified                 verifiedCursor
	projectionReleased       bool
	headerHash               [sha256.Size]byte
	headerSize               int64
	pendingHead, pendingTail *pendingRecord
	pendingCount             int64
	last                     Record
	sequence                 int64
	committedSequence        int64
	initial                  Geometry
	recoveryFinal            Geometry
	// admitted records whether this generation ever received a durable birth
	// geometry. A generation that did not can still hold bytes, but it can never
	// serve unified replay, and it fails closed when reopened.
	admitted bool
	origin   GenerationOrigin
	stored   int64
	// geometryCharge is the realm charge a geometry record carries. Live
	// accounting bills geometryRecordCost per appended geometry record, and a
	// geometry record adds nothing to end, so end alone cannot rebuild the realm
	// charge after a reopen. Keeping this separate from stored is deliberate:
	// stored also counts the header and every OUTPUT frame's own framing, which
	// live realm accounting never charged.
	//
	// It accumulates during parsing rather than at the end of it, so a generation
	// that fails to parse still carries the charges for the appends that had
	// already validated when the failure was reached.
	geometryCharge int64
	// realmCharge is the cumulative charge this generation holds against the
	// realm total: output payload bytes plus the fixed cost of every geometry
	// record. It exists so an aborted adoption reservation and a superseded
	// stale generation can refund exactly what they charged, no more.
	realmCharge int64
	// slotRetired marks a generation that holds no complete-pane ledger slot:
	// any generation reopened at scan, whatever its origin (no recovered
	// generation is resumable by the broker that reopens it, so none may hold
	// admission capacity the session it belongs to still needs), or the
	// tombstone of an aborted adoption reservation. Retired generations remain
	// in the pane map so they stay attributable and fail closed, and they are
	// removed entirely when a new reconstructed admission for the same
	// server+session commits and their file is provably gone.
	slotRetired bool
	// The exact production writer publishes this only after fault cleanup and
	// its final runtime owner settle. It grants no slot or replay authority.
	failedOwnerSettled atomic.Bool
	state              EligibilityState
	reason             InvalidationReason
	corrupt            bool
	// physical is the generation's charge against the physical ledger: every
	// byte its file occupies or is about to occupy. Live accounting charges an
	// append's frame, payload AND the commit frame that will follow it before
	// the append is written, so a commit can never be the write that runs out
	// of room; a reopen charges the file's exact size, torn bytes included.
	// It is refunded only when an unlink proves the bytes gone.
	physical int64
	// physicalReserved and reservedCharge hold capacity a GeometryReservation
	// has taken ahead of its write, in physical and logical units. Both caps
	// treat reserved capacity as spent.
	physicalReserved int64
	reservedCharge   int64
	// adoptionLogical and adoptionPhysical are a provisional reconstructed
	// generation's supersession allowance: exactly the logical and physical
	// charges its own session's stale generations were proven gone for at
	// BeginReconstructedPane (unlink or ENOENT, never earlier), bounded by
	// the pane caps. Both are backed by a realm reservation and mirrored in
	// reservedCharge/physicalReserved, so no other generation can spend them;
	// the successor's own appends convert them into charge record by record,
	// and AdoptionReservation.Commit or Abort releases whatever remains.
	adoptionLogical  int64
	adoptionPhysical int64
	// A pre-composite rotation capacity holds rollback headroom on the live
	// predecessor and forward replay headroom on its provisional successor.
	// Keeping the two classes distinct ensures successor replay cannot consume
	// R1. Ordinary predecessor output remains on the ordinary path while the
	// capacity is merely held; only the explicit post-boundary arm transition
	// exposes R1 to the journal-side 4R rollback operation.
	rotationRollbackLogical  int64
	rotationRollbackPhysical int64
	rotationRollbackRecords  int64
	// rotationRollbackReserved records that R1 is still backed by a global
	// and pane reservation. It stays true from BeginRotationCapacity until
	// RotationCapacity.Release settles the remainder: arming exposes the credit
	// to the predecessor but never releases its backing, so no other pane can
	// take R1's room between the arm and the 4R replay. Each replay record
	// converts its held part from reservation into charge.
	rotationRollbackReserved bool
	// rotationRollbackArmed is the explicit post-boundary transition. Until it
	// is true, predecessor output is ordinary output and cannot spend R1.
	rotationRollbackArmed   bool
	rotationForwardLogical  int64
	rotationForwardPhysical int64
	rotationForwardRecords  int64
	rotationCapacityHeld    bool
}

type Realm struct {
	sourceQuota  *sourceJournalQuota
	verification verificationProgress
	options      OpenOptions
	ops          journalOps
	retention    *retentionLedger
	rootFD       int
	realmFD      int
	realmName    string
	panes        map[PaneKey]*paneJournal
	recovered    []Recovery
	total        int64
	// reservedCharge is the realm-wide logical capacity held by live
	// geometry reservations; physicalTotal, physicalReserved and the two caps
	// are the realm's physical ledger, in bytes on disk.
	reservedCharge   int64
	physicalTotal    int64
	physicalReserved int64
	physicalCap      int64
	panePhysicalCap  int64
	closed           bool
}

type journalHeader struct {
	Version           uint8
	Key               PaneKey
	BrokerIncarnation string
	GeometryInitial   *Geometry
	// Origin is absent in generations written before adoption existed; absent
	// means birth, because every such generation was a birth.
	Origin GenerationOrigin `json:",omitempty"`
}

type storagePaths struct {
	Directories []string
	Files       []string
	Journal     string
}

type journalOps struct {
	lstat        func(string) (os.FileInfo, error)
	open         func(string, int, uint32) (int, error)
	mkdirat      func(int, string, uint32) error
	openat       func(int, string, int, uint32) (int, error)
	fchmod       func(int, uint32) error
	dup          func(int) (int, error)
	fstat        func(int, *syscall.Stat_t) error
	fstatfs      func(int, *syscall.Statfs_t) error
	readAll      func(io.Reader) ([]byte, error)
	readAt       func(*os.File, []byte, int64) (int, error)
	readdirnames func(*os.File, int) ([]string, error)
	write        func(*os.File, []byte) (int, error)
	sync         func(*os.File) error
	seek         func(*os.File, int64, int) (int64, error)
	unlinkat     func(int, string) error
	close        func(*os.File) error
	closeFD      func(int) error
}

func realJournalOps() journalOps {
	return journalOps{
		lstat:        os.Lstat,
		open:         syscall.Open,
		mkdirat:      syscall.Mkdirat,
		openat:       syscall.Openat,
		fchmod:       syscall.Fchmod,
		dup:          syscall.Dup,
		fstat:        syscall.Fstat,
		fstatfs:      syscall.Fstatfs,
		readAll:      io.ReadAll,
		readAt:       func(file *os.File, data []byte, offset int64) (int, error) { return file.ReadAt(data, offset) },
		readdirnames: func(file *os.File, count int) ([]string, error) { return file.Readdirnames(count) },
		write:        func(file *os.File, data []byte) (int, error) { return file.Write(data) },
		sync:         func(file *os.File) error { return file.Sync() },
		seek:         func(file *os.File, offset int64, whence int) (int64, error) { return file.Seek(offset, whence) },
		unlinkat:     syscall.Unlinkat,
		close:        func(file *os.File) error { return file.Close() },
		closeFD:      syscall.Close,
	}
}

// PUJ2 is a typed event log. PUJ1 held raw byte records only and cannot express
// geometry, so it is not migrated, guessed at, or partially read: it fails
// closed and the legacy engine remains available.
var journalMagic = [4]byte{'P', 'U', 'J', '2'}

const journalVersion uint8 = 2

const (
	appendMarker byte = 0xa1
	commitMarker byte = 0xc1
	headerLimit       = 64 << 10

	// On-disk record layout, from the marker byte.
	//   append: marker kind sequence(8) start(8) end(8) length(4) columns(4) rows(4) hash(32) payload
	//   commit: marker kind sequence(8) start(8) end(8) hash(32)
	appendFixed = 1 + 1 + 8 + 8 + 8 + 4 + 4 + 4 + sha256.Size
	commitFixed = 1 + 1 + 8 + 8 + 8 + sha256.Size

	// geometryRecordCost charges a zero-payload geometry event against the pane
	// and realm budgets. Without it a generation could grow durable state without
	// bound while every payload byte stayed inside its cap.
	geometryRecordCost int64 = appendFixed + commitFixed
)

func rotationReplayPhysical(bytes, records int64) int64 {
	return bytes + records*geometryRecordCost
}

// geometryHash binds a geometry record's type, position and dimensions. The
// domain prefix means no output payload hash can ever collide into a geometry
// record's identity.
func geometryHash(sequence int64, geometry Geometry) [sha256.Size]byte {
	image := make([]byte, 0, 24+16)
	image = append(image, "persea-unified-journal/geometry\x00"...)
	var scratch [8]byte
	binary.BigEndian.PutUint64(scratch[:], uint64(sequence))
	image = append(image, scratch[:]...)
	binary.BigEndian.PutUint32(scratch[:4], uint32(geometry.Columns))
	image = append(image, scratch[:4]...)
	binary.BigEndian.PutUint32(scratch[:4], uint32(geometry.Rows))
	image = append(image, scratch[:4]...)
	return sha256.Sum256(image)
}

func OpenRealm(options OpenOptions) (*Realm, error) {
	return openRealm(options, realJournalOps())
}

func openRealm(options OpenOptions, ops journalOps) (*Realm, error) {
	if options.RuntimeDir == "" || options.Realm == "" || options.BrokerIncarnation == "" ||
		options.DirectoryMode.Perm() == 0 || options.FileMode.Perm() == 0 ||
		options.PaneCapBytes <= 0 || options.RealmCapBytes <= 0 {
		return nil, ErrUnsafeRuntime
	}
	info, err := ops.lstat(options.RuntimeDir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != options.DirectoryMode.Perm() || !ownedBy(info, options.UID, options.GID) {
		return nil, ErrUnsafeRuntime
	}
	rootFD, err := ops.open(options.RuntimeDir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrUnsafeRuntime
	}
	if !safeFD(ops, rootFD, options.UID, options.GID, options.DirectoryMode, true) {
		ops.closeFD(rootFD)
		return nil, ErrUnsafeRuntime
	}
	realmName := realmDirectoryName(options.Realm)
	if err := ops.mkdirat(rootFD, realmName, uint32(options.DirectoryMode.Perm())); err != nil && !errors.Is(err, syscall.EEXIST) {
		ops.closeFD(rootFD)
		return nil, ErrUnsafeRuntime
	}
	realmFD, err := ops.openat(rootFD, realmName, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil || !safeFD(ops, realmFD, options.UID, options.GID, options.DirectoryMode, true) {
		if err == nil {
			ops.closeFD(realmFD)
		}
		ops.closeFD(rootFD)
		return nil, ErrUnsafeRuntime
	}
	realm := &Realm{options: options, ops: ops, rootFD: rootFD, realmFD: realmFD, realmName: realmName, panes: make(map[PaneKey]*paneJournal)}
	if err := realm.derivePhysicalBudget(); err != nil {
		realm.Close()
		return nil, err
	}
	if err := realm.scan(); err != nil {
		realm.Close()
		return nil, err
	}
	// Slot-retired generations own no complete-pane capacity: a recovered
	// generation can never be resumed, so charging it would permanently
	// consume a slot the session it belongs to still needs. Every generation
	// scan reopens is retired, so the ledger starts empty; the count is kept
	// as the one place the slot ledger is derived from pane state.
	occupied := 0
	for _, pane := range realm.panes {
		if !pane.slotRetired {
			occupied++
		}
	}
	realm.retention = newRetentionLedger(options, occupied)
	return realm, nil
}

// derivePhysicalBudget fixes the realm's physical caps at open. The pane cap
// is a multiple of the logical pane cap unless configured. The realm cap is
// explicit when configured; otherwise it is derived from the size of the
// filesystem the runtime directory lives on, read through the root directory
// descriptor so it is the same filesystem every journal file is created on:
// min(size x fraction, size - headroom). A filesystem too small to leave any
// budget after headroom is an unsafe runtime, not a zero-capacity realm.
func (realm *Realm) derivePhysicalBudget() error {
	options := realm.options
	realm.panePhysicalCap = options.PanePhysicalCapBytes
	if realm.panePhysicalCap <= 0 {
		realm.panePhysicalCap = options.PaneCapBytes * DefaultPanePhysicalMultiplier
	}
	if options.PhysicalCapBytes > 0 {
		realm.physicalCap = options.PhysicalCapBytes
		return nil
	}
	fraction := options.PhysicalFraction
	if fraction <= 0 || fraction > 1 {
		fraction = DefaultPhysicalFraction
	}
	headroom := options.PhysicalHeadroomBytes
	if headroom <= 0 {
		headroom = DefaultPhysicalHeadroomBytes
	}
	var stat syscall.Statfs_t
	if realm.ops.fstatfs == nil || realm.ops.fstatfs(realm.rootFD, &stat) != nil {
		return ErrUnsafeRuntime
	}
	unit := int64(stat.Frsize)
	if unit <= 0 {
		unit = int64(stat.Bsize)
	}
	if unit <= 0 || stat.Blocks == 0 {
		return ErrUnsafeRuntime
	}
	size := int64(stat.Blocks) * unit
	if size <= 0 {
		return ErrUnsafeRuntime
	}
	byFraction := int64(float64(size) * fraction)
	byHeadroom := size - headroom
	cap := byFraction
	if byHeadroom < cap {
		cap = byHeadroom
	}
	if cap <= 0 {
		return ErrUnsafeRuntime
	}
	realm.physicalCap = cap
	return nil
}

// physicalAppendCost is what one output append will occupy once committed:
// its frame, its payload, and the commit frame that makes it authoritative.
// Reserving the commit with the append is what keeps a commit from ever being
// the write that meets a full filesystem.
func physicalAppendCost(payload int) int64 {
	return int64(appendFixed) + int64(payload) + int64(commitFixed)
}

// physicalRoom reports whether cost fits inside both the pane's and the
// realm's remaining physical budget, with reserved capacity counted as spent.
func (realm *Realm) physicalRoom(pane *paneJournal, cost int64) bool {
	paneUsed := int64(0)
	if pane != nil {
		paneUsed = pane.physical + pane.physicalReserved
	}
	realmUsed := realm.physicalTotal + realm.physicalReserved
	return cost <= realm.panePhysicalCap-paneUsed && cost <= realm.physicalCap-realmUsed
}

// logicalRoom is the same test against the semantic caps.
func (realm *Realm) logicalRoom(pane *paneJournal, cost int64) bool {
	paneUsed := int64(0)
	if pane != nil {
		paneUsed = pane.realmCharge + pane.reservedCharge
	}
	return cost <= realm.options.PaneCapBytes-paneUsed && cost <= realm.options.RealmCapBytes-realm.total-realm.reservedCharge
}

func ownedBy(info os.FileInfo, uid, gid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid && int(stat.Gid) == gid
}

func safeFD(ops journalOps, fd, uid, gid int, mode os.FileMode, directory bool) bool {
	var stat syscall.Stat_t
	if fd < 0 || ops.fstat(fd, &stat) != nil || int(stat.Uid) != uid || int(stat.Gid) != gid || stat.Mode&0o7777 != uint32(mode.Perm()) {
		return false
	}
	typeMode := stat.Mode & syscall.S_IFMT
	if directory {
		return typeMode == syscall.S_IFDIR
	}
	return typeMode == syscall.S_IFREG
}

func realmDirectoryName(realm string) string {
	hash := sha256.Sum256([]byte(realm))
	return "realm-" + hex.EncodeToString(hash[:16])
}

func paneFileName(key PaneKey) string {
	encoded, _ := json.Marshal(key)
	hash := sha256.Sum256(encoded)
	return "pane-" + hex.EncodeToString(hash[:]) + ".journal"
}

func storagePathsForTest(options OpenOptions, key PaneKey) storagePaths {
	directory := filepath.Join(options.RuntimeDir, realmDirectoryName(options.Realm))
	journal := filepath.Join(directory, paneFileName(key))
	return storagePaths{Directories: []string{directory}, Files: []string{journal}, Journal: journal}
}

func (realm *Realm) scan() error {
	duplicate, err := realm.ops.dup(realm.realmFD)
	if err != nil {
		return ErrUnsafeRuntime
	}
	directory := os.NewFile(uintptr(duplicate), "journal-realm")
	names, err := realm.ops.readdirnames(directory, -1)
	realm.ops.close(directory)
	if err != nil {
		return ErrUnsafeRuntime
	}
	sort.Strings(names)
	if len(names) > MaxRealmIdentities {
		return ErrQuota
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "pane-") || !strings.HasSuffix(name, ".journal") || strings.Contains(name, "/") {
			return ErrUnsafeRuntime
		}
		pane, err := realm.openExisting(name)
		if err != nil {
			return err
		}
		if paneFileName(pane.key) != name {
			realm.ops.close(pane.file)
			return ErrUnsafeRuntime
		}
		if _, duplicate := realm.panes[pane.key]; duplicate {
			realm.ops.close(pane.file)
			return ErrUnsafeRuntime
		}
		realm.panes[pane.key] = pane
		// Rebuild exactly the charge live accounting made: output payload bytes,
		// which end already holds, plus the fixed reservation for every durable
		// geometry record. Anything else either mints capacity on restart or
		// invents a charge the realm never took.
		pane.realmCharge = pane.end + pane.geometryCharge
		realm.total += pane.realmCharge
		// The physical ledger rebuilds from the one fact a reopen can prove: the
		// file's size. Header, framing, payload, commit frames and torn or corrupt
		// bytes all occupy the filesystem identically, and a generation that
		// failed to parse occupies exactly as much as one that parsed.
		realm.physicalTotal += pane.physical
		// Every generation reopened here was written before this incarnation
		// and can never be resumed: openExisting classifies it legacy or
		// untrusted, never continuous, whatever its origin. It is stale by
		// construction, so it is retired from slot capacity at scan — a stale
		// birth generation would otherwise hold one of the complete-pane slots
		// for the life of the runtime directory, because nothing that came
		// after it could ever release it. The generation itself stays in the
		// map — origin-honest, charge-honest and fail-closed — until a new
		// reconstructed admission for its session commits and supersedes it.
		pane.slotRetired = !realm.options.StartupRecovery
		final := pane.recoveryFinal
		realm.recovered = append(realm.recovered, Recovery{
			Key: pane.key, Committed: pane.committed, Eligibility: pane.state,
			Reason: pane.reason, Outcome: RecoveryScanned, Initial: pane.initial,
			Final: final, RetainedLogical: pane.realmCharge,
			RetainedPhysical: pane.physical, RetainedSlot: !pane.slotRetired,
		})
	}
	return nil
}

func (realm *Realm) openExisting(name string) (*paneJournal, error) {
	fd, err := realm.ops.openat(realm.realmFD, name, syscall.O_RDWR|syscall.O_APPEND|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil || !safeFD(realm.ops, fd, realm.options.UID, realm.options.GID, realm.options.FileMode, false) {
		if err == nil {
			realm.ops.closeFD(fd)
		}
		return nil, ErrUnsafeRuntime
	}
	file := os.NewFile(uintptr(fd), name)
	contents, err := realm.ops.readAll(file)
	if err != nil {
		realm.ops.close(file)
		return nil, ErrStorage
	}
	pane, parseErr := parseJournal(contents)
	pane.file = file
	pane.physical = int64(len(contents))
	if _, err := realm.ops.seek(file, 0, io.SeekEnd); err != nil {
		realm.ops.close(file)
		return nil, ErrStorage
	}
	if parseErr != nil {
		pane.state = EligibilityUntrusted
		pane.reason = ReasonCorruptJournal
		pane.corrupt = true
		pane.verified = verifiedCursor{}
		pane.committed = 0
		pane.committedSequence = 0
	} else if !pane.admitted {
		// A generation that never carried a durable birth geometry cannot be
		// replayed: there is no size to admit it at. It fails closed exactly as a
		// corrupt one does, and legacy remains available.
		pane.state = EligibilityUntrusted
		pane.reason = ReasonCorruptJournal
		pane.corrupt = true
		pane.verified = verifiedCursor{}
		pane.committed = 0
		pane.committedSequence = 0
	} else {
		pane.state = EligibilityLegacy
		pane.reason = ReasonVolatileRestart
	}
	return pane, nil
}

func parseJournal(contents []byte) (*paneJournal, error) {
	pane := &paneJournal{}
	reader := bytes.NewReader(contents)
	header, size, digest, err := decodeHeader(reader)
	if err != nil {
		return pane, ErrCorruptJournal
	}
	if err := acceptHeader(pane, header); err != nil {
		return pane, err
	}
	pane.headerSize, pane.headerHash = size, digest
	cursor := verifiedCursor{physical: size}
	// Classification retains the last committed geometry even when a later
	// corrupt frame makes the generation unavailable for replay.
	defer func() {
		pane.recoveryFinal = pane.initial
		for page := cursor.view; page != nil; page = page.previous {
			if page.event.Kind == RecordGeometry {
				pane.recoveryFinal = page.event.Geometry
				break
			}
		}
	}()
	for reader.Len() > 0 {
		_, err := cursor.next(reader, pane.key)
		// Recovery must retain the charge of validated geometry even if a
		// later frame fails. This is independent of replay publication.
		pane.geometryCharge = cursor.geometryCharge
		pane.sequence, pane.stored = cursor.sequence, cursor.physical
		pane.committed, pane.committedSequence = cursor.committed, cursor.committedSequence
		if errors.Is(err, errTornAppend) {
			break
		}
		if err != nil {
			return pane, ErrCorruptJournal
		}
	}
	pane.end, pane.stored = cursor.end, cursor.physical
	// Drop the uncommitted projection, while retaining its parsed charges.
	cursor.tail = cursor.view
	pane.verified = cursor
	return pane, nil
}

// createPane writes the generation header. initial is nil only for a pane that
// was never admitted with a durable birth geometry: such a generation stays a
// byte-only journal, never becomes unified-eligible, and fails closed if it is
// ever reopened, because a header without GEOMETRY_INITIAL is not replayable.
func (realm *Realm) createPane(key PaneKey, initial *Geometry, origin GenerationOrigin) (*paneJournal, error) {
	return realm.createPaneWithCapacity(key, initial, origin, false)
}

// createPaneWithCapacity writes a header either through ordinary admission or
// as the pure consumer of a pre-composite rotation capacity. In the latter
// case the maximum header was already held in R2, so this function must not
// perform a second physical-cap decision. The caller converts that held
// maximum to the exact header charge only after successful materialization.
func (realm *Realm) createPaneWithCapacity(key PaneKey, initial *Geometry, origin GenerationOrigin, headerReserved bool) (*paneJournal, error) {
	header, err := realm.encodeJournalHeader(key, initial, origin)
	if err != nil {
		return nil, err
	}
	return realm.createPaneWithHeader(key, initial, origin, header, headerReserved)
}

// encodeJournalHeader is the exact on-disk header a generation opens with,
// framed. It is separate from materialization so a caller that must size the
// header's physical cost before deciding how to pay for it can.
func (realm *Realm) encodeJournalHeader(key PaneKey, initial *Geometry, origin GenerationOrigin) ([]byte, error) {
	headerBytes, err := json.Marshal(journalHeader{Version: journalVersion, Key: key, BrokerIncarnation: realm.options.BrokerIncarnation, GeometryInitial: initial, Origin: origin})
	if err != nil || len(headerBytes) > headerLimit {
		return nil, ErrStorage
	}
	header := make([]byte, 8+len(headerBytes))
	copy(header[:4], journalMagic[:])
	binary.BigEndian.PutUint32(header[4:8], uint32(len(headerBytes)))
	copy(header[8:], headerBytes)
	return header, nil
}

// createPaneWithHeader materializes a generation with an already-encoded
// header. headerReserved means the caller holds the header's physical cost
// already (a rotation's R2 maximum, or an adoption's supersession allowance)
// and converts it after the write; otherwise the header must fit ordinary
// physical room.
func (realm *Realm) createPaneWithHeader(key PaneKey, initial *Geometry, origin GenerationOrigin, header []byte, headerReserved bool) (*paneJournal, error) {
	if len(realm.panes) >= MaxRealmIdentities {
		return nil, ErrQuota
	}
	name := paneFileName(key)
	// The header is the generation's first physical cost and is reserved
	// before the file exists: a realm with no room for one more header refuses
	// the generation outright instead of creating a file it cannot grow.
	if !headerReserved && !realm.physicalRoom(nil, int64(len(header))) {
		return nil, ErrPhysicalQuota
	}
	if !realm.sourceClaimCreate(key) {
		return nil, ErrSourceQuota
	}
	fd, err := realm.ops.openat(realm.realmFD, name, syscall.O_RDWR|syscall.O_APPEND|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_CREAT|syscall.O_EXCL, uint32(realm.options.FileMode.Perm()))
	if err != nil && !errors.Is(err, syscall.EEXIST) {
		// Even failed creation must prove unlink/ENOENT before refunding its
		// claimed source allowance. EEXIST is unexpected existing ownership;
		// retain that liability for startup inventory rather than deleting it.
		realm.removePaneFile(key)
	}
	if err != nil || realm.ops.fchmod(fd, uint32(realm.options.FileMode.Perm())) != nil || !safeFD(realm.ops, fd, realm.options.UID, realm.options.GID, realm.options.FileMode, false) {
		if err == nil {
			realm.ops.closeFD(fd)
		}
		return nil, ErrUnsafeRuntime
	}
	file := os.NewFile(uintptr(fd), name)
	pane := &paneJournal{key: key, file: file, state: EligibilityContinuous, stored: int64(len(header)), origin: origin,
		headerSize: int64(len(header)), headerHash: sha256.Sum256(header)}
	// Charged before the write. A header that fails to write still leaves
	// whatever landed on the filesystem: the file is removed, and the charge
	// is refunded only if the unlink proves the bytes gone; otherwise it stays
	// in the realm total, unattributed but counted, until a reopen rebuilds
	// the ledger from the surviving file.
	pane.physical = int64(len(header))
	realm.physicalTotal += pane.physical
	if err := realm.writeAll(file, header); err != nil {
		realm.ops.close(file)
		if realm.removePaneFile(key) {
			realm.physicalTotal -= pane.physical
			realm.retention.releaseProvenCapacity(pane.physical > 0)
		}
		return nil, ErrStorage
	}
	if initial != nil {
		pane.initial = *initial
		pane.admitted = true
	}
	realm.panes[key] = pane
	return pane, nil
}

// AdmitPane opens a pane generation with its durable birth geometry. It is the
// only path that produces a unified-eligible journal, and it is deliberately
// not retroactive: a generation that already holds bytes cannot acquire a birth
// geometry after the fact, because the bytes before it were never sized.
func (realm *Realm) AdmitPane(key PaneKey, initial Geometry) error {
	return realm.admitPane(key, initial, OriginBirth)
}

// AdmitReconstructedPane opens an adopted pane generation whose first output
// is a synthesized capture-equivalent bootstrap rather than byte-zero pane
// output, committing the complete-pane charge immediately. Production
// adoption must not use it: the broker's adoption sequence has failure paths
// between admission and activation, and only BeginReconstructedPane lets those
// paths release the reservation instead of leaking the slot.
func (realm *Realm) AdmitReconstructedPane(key PaneKey, initial Geometry) error {
	reservation, err := realm.BeginReconstructedPane(key, initial)
	if err != nil {
		return err
	}
	reservation.Commit()
	return nil
}

// AdoptionReservation is a provisional reconstructed-pane admission. Begin
// reserves the slot, supersedes the session's stale generations by proof,
// and creates the generation so the bootstrap can be journaled; the durable
// complete-pane charge is committed only when the session activates
// (Commit), and every pre-commit failure aborts the reservation (Abort),
// releasing the slot and removing the generation's artifacts. Exactly one of
// Commit or Abort settles a reservation; later calls are no-ops.
type AdoptionReservation struct {
	realm *Realm
	key   PaneKey
	// held marks a reservation that still owns a provisional ledger slot and
	// a created generation. The idempotent already-live path holds nothing.
	held    bool
	settled bool
}

// BeginReconstructedPane provisionally opens an adopted pane generation. It is
// the only admission path that stamps origin=reconstructed. The returned
// reservation owns one complete-pane slot until it is settled: Commit makes
// the charge durable; Abort releases the slot and retires the generation so
// a failed adoption can never consume capacity.
//
// Same-session supersession is transactional and happens here, BEFORE the
// successor is materialized. A recovered stale generation that itself holds
// the realm's last logical or physical byte would otherwise refuse the
// successor's header or bootstrap, and the cleanup that refunds it — which
// used to run only at Commit, after the bootstrap — could never be reached:
// the session was permanently unadoptable. So, in order, under the retention
// interval:
//
//  1. the slot is reserved;
//  2. every slot-retired stale generation of the same server+session, whatever
//     its origin, is unlinked through the same proof-before-refund sweep
//     Commit runs, and its logical and physical charges are refunded only
//     when unlink or ENOENT proves the bytes gone — an unlink that fails
//     refunds nothing and grants nothing;
//  3. exactly the refunded amounts, bounded by the pane caps, are held back
//     as this reservation's supersession allowance: reserved on the realm so
//     no other session can spend them, spent only by this successor's header
//     and appends, and released by Commit or Abort. Charged plus reserved is
//     unchanged by the refund-and-hold, so both ledgers stay within cap at
//     every step;
//  4. the header is materialized, drawing its physical cost from the
//     allowance when the allowance can carry it and from ordinary room
//     otherwise. A header that fails to write releases the allowance and the
//     slot; bytes a failed unlink retains stay counted, exactly once.
//
// A stale generation of ANOTHER session is not this admission's to retire
// and grants no allowance. A crash after step 2 leaves the session with no
// generation until the next adoption, which starts from a fresh capture; a
// crash after step 4 leaves a successor the next scan retires and the next
// adoption supersedes. Nothing in the allowance is persisted.
func (realm *Realm) BeginReconstructedPane(key PaneKey, initial Geometry) (*AdoptionReservation, error) {
	return realm.beginReconstructedPane(key, initial, realm.reservedSource(key))
}

// BeginReconstructedPaneForSource admits a fresh capture under one existing
// reconstruction transaction. Recovered source owners are settled before the
// new source allowance is requested, so a crash with current+successor files
// cannot require a third allowance merely to reach proof-bearing cleanup.
func (realm *Realm) BeginReconstructedPaneForSource(key PaneKey, initial Geometry, source SourceID) (*AdoptionReservation, error) {
	if realm.sourceQuota != nil && (source == (SourceID{}) || source.Realm != realm.options.Realm || source.Server != key.Server || source.Session != key.Session || source.Window != key.Window || source.Pane != key.Pane) {
		return nil, ErrInvalidRecord
	}
	return realm.beginReconstructedPane(key, initial, source)
}

func (realm *Realm) beginReconstructedPane(key PaneKey, initial Geometry, source SourceID) (*AdoptionReservation, error) {
	realm.retention.enter()
	defer realm.retention.leave()
	if realm.closed {
		return nil, ErrInvalidated
	}
	if !initial.valid() {
		return nil, ErrInvalidRecord
	}
	if pane := realm.panes[key]; pane != nil {
		// Idempotent only for the identical live continuous generation: same
		// geometry AND same origin. A reused key with a different origin or a
		// stale generation is a re-admission of a different life and fails
		// closed; the caller mints the next generation.
		if pane.admitted && pane.initial == initial && pane.state == EligibilityContinuous && pane.origin == OriginReconstructed {
			if source != (SourceID{}) {
				if err := realm.ReserveSource(key, source); err != nil {
					return nil, err
				}
			}
			return &AdoptionReservation{realm: realm, key: key, settled: true}, nil
		}
		return nil, ErrInvalidated
	}
	failedLogical, failedPhysical, transferred, err := realm.transferSettledFailedForReconstructionLocked(key, source)
	if err != nil {
		return nil, err
	}
	if !transferred {
		transferred = realm.retireRecoveredForReconstructionLocked(key, source, true)
	}
	if !transferred {
		if err := realm.retention.reserveCompletePane(true); err != nil {
			return nil, err
		}
	}
	// Supersede by proof, then hold exactly what was proven gone for this
	// successor alone. The hold is taken in the same step as the refund so
	// the room never exists as ordinary room, not even for one instruction.
	allowanceLogical, allowancePhysical := realm.sweepRetiredSessionLocked(key)
	allowanceLogical += failedLogical
	allowancePhysical += failedPhysical
	if allowanceLogical > realm.options.PaneCapBytes {
		allowanceLogical = realm.options.PaneCapBytes
	}
	if allowancePhysical > realm.panePhysicalCap {
		allowancePhysical = realm.panePhysicalCap
	}
	realm.reservedCharge += allowanceLogical
	realm.physicalReserved += allowancePhysical
	releaseAllowance := func() {
		realm.reservedCharge -= allowanceLogical
		realm.physicalReserved -= allowancePhysical
		allowanceLogical, allowancePhysical = 0, 0
	}
	if source != (SourceID{}) {
		if err := realm.ReserveSource(key, source); err != nil {
			releaseAllowance()
			realm.retention.releaseCompletePane()
			return nil, err
		}
		// On failure before creation this releases the provisional lease. A
		// claimed/materialized file still requires unlink/ENOENT proof.
		defer realm.CancelSourceReservation(key)
	}
	header, err := realm.encodeJournalHeader(key, &initial, OriginReconstructed)
	if err != nil {
		releaseAllowance()
		realm.retention.releaseCompletePane()
		return nil, err
	}
	headerCost := int64(len(header))
	headerHeld := int64(0)
	if allowancePhysical >= headerCost {
		headerHeld = headerCost
	} else {
		// An allowance too small to carry the header is returned to ordinary
		// room in the same step, so the ordinary decision below sees every
		// byte the realm actually has; the logical allowance is unaffected.
		realm.physicalReserved -= allowancePhysical
		allowancePhysical = 0
	}
	chargedBefore := realm.physicalTotal
	pane, err := realm.createPaneWithHeader(key, &initial, OriginReconstructed, header, headerHeld > 0)
	if err != nil {
		// Bytes a failed header write left behind under a name the realm
		// cannot reuse are already counted in physicalTotal. If they were to
		// be paid from the allowance, convert exactly that much out of the
		// hold so they are counted once, as charge; then release the rest.
		// A generation that never came to exist holds nothing: this is a
		// pre-commit error path of the transaction, not a promised slot.
		if retained := realm.physicalTotal - chargedBefore; retained > 0 && headerHeld > 0 {
			if retained > headerHeld {
				retained = headerHeld
			}
			realm.physicalReserved -= retained
			allowancePhysical -= retained
		}
		releaseAllowance()
		realm.retention.releaseCompletePane()
		return nil, err
	}
	// Convert the header's exact cost out of the hold; the remainder is the
	// successor's to spend, mirrored on the pane so its own cap treats it as
	// already taken and the realm treats it as unavailable to anyone else.
	realm.physicalReserved -= headerHeld
	allowancePhysical -= headerHeld
	pane.adoptionLogical = allowanceLogical
	pane.adoptionPhysical = allowancePhysical
	pane.reservedCharge += allowanceLogical
	pane.physicalReserved += allowancePhysical
	return &AdoptionReservation{realm: realm, key: key, held: true}, nil
}

// retireRecoveredForReconstructionLocked settles only non-live startup owners.
// A complete explicit source association also covers geometry/refit changes and
// later attribution of an initially ambiguous inventory. Without that proof,
// legacy callers retain the stricter exact-recovery incarnation requirement.
// transferOne keeps one existing slot inside the reconstruction transaction;
// additional recovered slots are retired. Source and byte credits remain held
// until the ordinary sweep proves unlink. Current/actively held files are never
// selected merely because their source quota is full.
func (realm *Realm) retireRecoveredForReconstructionLocked(current PaneKey, source SourceID, transferOne bool) bool {
	transferred := false
	for _, recovery := range realm.recovered {
		key := recovery.Key
		if key == current || key.Server != current.Server || key.Session != current.Session || key.Window != current.Window || key.Pane != current.Pane {
			continue
		}
		if source != (SourceID{}) {
			if realm.reservedSource(key) != source {
				continue
			}
		} else if recovery.Outcome != RecoveryKeptExact || key.Incarnation != current.Incarnation {
			continue
		}
		pane := realm.panes[key]
		if pane == nil || pane.slotRetired || pane.state != EligibilityLegacy || pane.corrupt {
			continue
		}
		pane.slotRetired = true
		if transferred || !transferOne {
			realm.retention.retireCompletePane()
		} else {
			transferred = true
		}
	}
	return transferred
}

// HasRecoveredCaptureCandidate permits capture despite full slots only for a
// valid non-live recovered file with proven identity. Source association can
// survive ambiguous final geometry; it does not grant replay authority. The
// reconstruction transaction rechecks identity and owns all actual transfers.
func (realm *Realm) HasRecoveredCaptureCandidate(server, session string) bool {
	for _, recovery := range realm.recovered {
		if recovery.Key.Server != server || recovery.Key.Session != session {
			continue
		}
		pane := realm.panes[recovery.Key]
		if pane == nil || pane.slotRetired || pane.state != EligibilityLegacy || pane.corrupt {
			continue
		}
		if recovery.Outcome == RecoveryKeptExact || realm.reservedSource(recovery.Key) != (SourceID{}) {
			return true
		}
	}
	return false
}

func (realm *Realm) settledFailedCandidate(pane *paneJournal) bool {
	return pane != nil && pane.admitted && !pane.slotRetired &&
		pane.state == EligibilityUntrusted && !pane.corrupt && pane.failedOwnerSettled.Load() &&
		pane.reservedCharge == 0 && pane.physicalReserved == 0 &&
		pane.adoptionLogical == 0 && pane.adoptionPhysical == 0 &&
		!pane.rotationCapacityHeld && !pane.rotationRollbackReserved &&
		pane.rotationForwardLogical == 0 && pane.rotationForwardPhysical == 0 &&
		realm.reservedSource(pane.key) != (SourceID{})
}

// HasSettledFailedCaptureCandidate is advisory only. A completed failed writer
// may retain the last slot intentionally; fresh capture can reach the existing
// reconstruction transaction, which rechecks the complete current source.
func (realm *Realm) HasSettledFailedCaptureCandidate(server, session string) bool {
	for key, pane := range realm.panes {
		if key.Server == server && key.Session == session && realm.settledFailedCandidate(pane) {
			return true
		}
	}
	return false
}

// transferSettledFailedForReconstructionLocked supersedes one exact failed
// writer lifetime, not arbitrary untrusted history. Until unlink proves its
// file gone, even its slot remains charged. Snapshot objects retain their own
// independent projection/reader lifetimes; they are not live writer authority.
func (realm *Realm) transferSettledFailedForReconstructionLocked(current PaneKey, source SourceID) (logical, physical int64, transferred bool, err error) {
	if source == (SourceID{}) {
		return 0, 0, false, nil
	}
	var selected *paneJournal
	for key, pane := range realm.panes {
		if key == current || key.Server != current.Server || key.Session != current.Session || key.Window != current.Window || key.Pane != current.Pane ||
			!realm.settledFailedCandidate(pane) || realm.reservedSource(key) != source {
			continue
		}
		if selected == nil || key.ControlGeneration > selected.key.ControlGeneration {
			selected = pane
		}
	}
	if selected == nil {
		return 0, 0, false, nil
	}
	if selected.file != nil {
		_ = realm.ops.close(selected.file)
		selected.file = nil
	}
	if !realm.removePaneFile(selected.key) {
		return 0, 0, false, ErrQuota
	}
	logical, physical = selected.realmCharge, selected.physical
	realm.total -= logical
	realm.physicalTotal -= physical
	selected.realmCharge, selected.physical = 0, 0
	selected.slotRetired = true
	selected.releaseProjection()
	delete(realm.panes, selected.key)
	realm.retention.releaseProvenCapacity(logical > 0 || physical > 0)
	// The old complete slot stays reserved inside this transaction. Its byte
	// refund is immediately converted to the successor allowance by the caller.
	return logical, physical, true, nil
}

// releaseAdoptionAllowanceLocked returns a provisional successor's unspent
// supersession allowance to ordinary room. It is the settlement half of
// BeginReconstructedPane's hold and runs from Commit and Abort alike.
func releaseAdoptionAllowanceLocked(realm *Realm, pane *paneJournal) {
	if pane == nil {
		return
	}
	realm.reservedCharge -= pane.adoptionLogical
	realm.physicalReserved -= pane.adoptionPhysical
	pane.reservedCharge -= pane.adoptionLogical
	pane.physicalReserved -= pane.adoptionPhysical
	pane.adoptionLogical = 0
	pane.adoptionPhysical = 0
}

// removePaneFile removes a settled generation's journal file. True means the
// bytes are provably gone: the unlink succeeded or the file was already
// absent. Any other failure leaves the bytes possibly on disk, so callers
// must keep the generation's realm charge until a retry proves otherwise.
func (realm *Realm) removePaneFile(key PaneKey) bool {
	err := realm.ops.unlinkat(realm.realmFD, paneFileName(key))
	settled := err == nil || errors.Is(err, syscall.ENOENT)
	if settled {
		realm.sourceUnlinked(key)
	}
	return settled
}

// sweepRetiredSessionLocked retries proof-based cleanup for every retired
// generation of current's session except current itself. Both adoption and
// rotation use the same widened P1a rule, so birth, reconstructed, rotated,
// corrupt and prior unlink-failed generations cannot diverge by origin. It
// returns exactly the logical and physical charges it refunded, which is only
// ever what unlink or ENOENT proved gone.
func (realm *Realm) sweepRetiredSessionLocked(current PaneKey) (logicalRefunded, physicalRefunded int64) {
	for staleKey, stale := range realm.panes {
		if staleKey == current {
			continue
		}
		if !stale.slotRetired || staleKey.Server != current.Server || staleKey.Session != current.Session {
			continue
		}
		if stale.file != nil {
			_ = realm.ops.close(stale.file)
			stale.file = nil
		}
		if !realm.removePaneFile(staleKey) {
			continue
		}
		realm.total -= stale.realmCharge
		logicalRefunded += stale.realmCharge
		stale.realmCharge = 0
		realm.physicalTotal -= stale.physical
		physicalRefunded += stale.physical
		stale.physical = 0
		stale.releaseProjection()
		delete(realm.panes, staleKey)
	}
	realm.retention.releaseProvenCapacity(logicalRefunded > 0 || physicalRefunded > 0)
	return logicalRefunded, physicalRefunded
}

// RetirePane settles one terminal generation. The complete-pane slot is
// retired exactly once before unlink is attempted. A failed unlink retains the
// closed, slot-retired tombstone and both byte charges; the slot wake and the
// later proof-bearing byte-refund wake are therefore distinct.
//
// The bool reports terminal settlement: true means the generation is absent
// (including an idempotent retry); false means its file may still exist and a
// retry is required.
func (realm *Realm) RetirePane(key PaneKey) bool {
	realm.retention.enter()
	defer realm.retention.leave()
	if realm.closed {
		return true
	}
	pane := realm.panes[key]
	if pane == nil {
		return true
	}
	invalidatePane(pane, ReasonSourceReplacement)
	// A geometry or rotation transaction still owns these holds. Its actual
	// settlement must precede unlink and identity reuse.
	if pane.reservedCharge != 0 || pane.physicalReserved != 0 || pane.rotationCapacityHeld {
		return false
	}
	if pane.file != nil {
		_ = realm.ops.close(pane.file)
		pane.file = nil
	}
	if !pane.slotRetired {
		pane.slotRetired = true
		realm.retention.retireCompletePane()
	}
	if !realm.removePaneFile(key) {
		return false
	}
	released := pane.realmCharge > 0 || pane.physical > 0
	realm.total -= pane.realmCharge
	pane.realmCharge = 0
	realm.physicalTotal -= pane.physical
	pane.physical = 0
	pane.releaseProjection()
	delete(realm.panes, key)
	realm.retention.releaseProvenCapacity(released)
	return true
}

// Commit makes the reservation's complete-pane charge durable, releases the
// unspent supersession allowance to ordinary room, and retries the
// supersession of every slot-retired stale generation for the same
// server+session, whatever its origin — a birth generation recovered at
// scan, a reconstructed one, a corrupt or torn one, or an aborted adoption's
// tombstone: their files are unlinked and their realm and physical charges
// refunded, so at most one generation per adopted session owns capacity once
// cleanup completes. BeginReconstructedPane already ran this sweep before
// the successor existed; the retry here is for a generation whose unlink
// failed then. The sweep is keyed by identity, never by flag: the
// reservation's own key is excluded outright, and only a generation that
// already holds no slot is eligible. The refund follows proof: a stale
// generation whose unlink fails still has its bytes on disk, so it keeps its
// map entry and both charges, and the next same-session supersession retries
// the cleanup.
func (reservation *AdoptionReservation) Commit() {
	realm := reservation.realm
	realm.retention.enter()
	defer realm.retention.leave()
	if reservation.settled {
		return
	}
	reservation.settled = true
	if !reservation.held || realm.closed {
		return
	}
	reservation.held = false
	releaseAdoptionAllowanceLocked(realm, realm.panes[reservation.key])
	realm.sweepRetiredSessionLocked(reservation.key)
}

// Abort settles a failed adoption attempt: the provisional slot returns to
// the ledger and the pane entry stays behind as an invalidated, slot-retired
// tombstone so an append still in flight fails closed with ErrInvalidated
// instead of re-creating a birth generation at the aborted key. The realm
// charge the bootstrap already took is refunded only once the generation's
// file is provably gone: bytes a failed unlink leaves on disk must keep
// counting against the realm cap, and a reopen rebuilds exactly the retained
// charge from the surviving file. A retained tombstone is refunded and
// removed when a later reconstructed admission for the same session commits
// and its cleanup succeeds.
func (reservation *AdoptionReservation) Abort() {
	realm := reservation.realm
	realm.retention.enter()
	defer realm.retention.leave()
	if reservation.settled {
		return
	}
	reservation.settled = true
	if !reservation.held || realm.closed {
		return
	}
	reservation.held = false
	realm.retention.releaseCompletePane()
	pane := realm.panes[reservation.key]
	if pane == nil {
		return
	}
	releaseAdoptionAllowanceLocked(realm, pane)
	invalidatePane(pane, ReasonAdoptionAborted)
	pane.slotRetired = true
	if pane.file != nil {
		_ = realm.ops.close(pane.file)
		pane.file = nil
	}
	if !realm.removePaneFile(reservation.key) {
		return
	}
	realm.retention.releaseProvenCapacity(pane.realmCharge > 0 || pane.physical > 0)
	realm.total -= pane.realmCharge
	pane.realmCharge = 0
	realm.physicalTotal -= pane.physical
	pane.physical = 0
	pane.releaseProjection()
}

// RotationCapacity is the single pre-composite decision for a rotation. It
// atomically owns the temporary successor slot plus R1 predecessor rollback,
// R2 successor bootstrap and R3 successor-tail capacity. It is geometry
// independent: every quantity is a fixed worst-case bound. A successful
// BeginRotatedPane transfers S/R2/R3 but retains R1 here until Commit, or until
// the caller completes Abort's rollback replay and calls Release.
type RotationCapacity struct {
	realm       *Realm
	predecessor PaneKey
	held        bool
	// materializationAttempted is set the moment BeginRotatedPane hands the
	// capacity to storage. A failed attempt may leave header bytes charged
	// under a name the realm cannot reuse and may have converted part of the
	// header hold, so the capacity is single-use for materialization even on
	// failure. It stays eligible for the rollback path: ArmRollback, bounded replay
	// into the predecessor, then Release of the remaining holds.
	materializationAttempted bool
	// materialized means a RotationReservation owns the successor; active
	// means that reservation has not yet chosen Commit or Abort.
	materialized bool
	active       bool

	rollbackLogical   int64
	rollbackPhysical  int64
	rollbackRecords   int64
	successorLogical  int64
	successorPhysical int64
	successorRecords  int64
}

// BeginRotationCapacity atomically reserves every resource rotation may need
// before an external capture composite can mutate tmux. A refusal changes no
// journal state and returns the temporary slot before leaving the retention
// interval.
func (realm *Realm) BeginRotationCapacity(predecessor PaneKey) (*RotationCapacity, error) {
	realm.retention.enter()
	defer realm.retention.leave()
	if realm.closed {
		return nil, ErrInvalidated
	}
	pane := realm.panes[predecessor]
	if pane == nil || !pane.admitted || pane.state != EligibilityContinuous || pane.slotRetired || pane.rotationCapacityHeld {
		return nil, ErrInvalidated
	}
	if err := realm.retention.reserveCompletePane(false); err != nil {
		return nil, err
	}

	rollbackLogical := rotationPendingCapBytes
	rollbackPhysical := rotationReplayPhysical(rotationPendingCapBytes, rotationPendingCapRecords)
	successorLogical := adoptionBootstrapCapBytes + rotationPendingCapBytes
	successorPhysical := rotationHeaderPhysicalReserve +
		rotationReplayPhysical(adoptionBootstrapCapBytes, adoptionBootstrapCapRecords) +
		rotationReplayPhysical(rotationPendingCapBytes, rotationPendingCapRecords)
	if rollbackLogical > realm.options.PaneCapBytes-pane.realmCharge-pane.reservedCharge ||
		successorLogical > realm.options.PaneCapBytes ||
		rollbackLogical+successorLogical > realm.options.RealmCapBytes-realm.total-realm.reservedCharge {
		realm.retention.releaseCompletePane()
		return nil, ErrQuota
	}
	if rollbackPhysical > realm.panePhysicalCap-pane.physical-pane.physicalReserved ||
		successorPhysical > realm.panePhysicalCap ||
		rollbackPhysical+successorPhysical > realm.physicalCap-realm.physicalTotal-realm.physicalReserved {
		realm.retention.releaseCompletePane()
		return nil, ErrPhysicalQuota
	}

	pane.reservedCharge += rollbackLogical
	pane.physicalReserved += rollbackPhysical
	pane.rotationRollbackLogical = rollbackLogical
	pane.rotationRollbackPhysical = rollbackPhysical
	pane.rotationRollbackRecords = rotationPendingCapRecords
	pane.rotationRollbackReserved = true
	pane.rotationCapacityHeld = true
	realm.reservedCharge += rollbackLogical + successorLogical
	realm.physicalReserved += rollbackPhysical + successorPhysical
	return &RotationCapacity{
		realm: realm, predecessor: predecessor, held: true,
		rollbackLogical: rollbackLogical, rollbackPhysical: rollbackPhysical,
		rollbackRecords:  rotationPendingCapRecords,
		successorLogical: successorLogical, successorPhysical: successorPhysical,
		successorRecords: adoptionBootstrapCapRecords + rotationPendingCapRecords,
	}, nil
}

func (capacity *RotationCapacity) releaseLocked() {
	if capacity == nil || !capacity.held {
		return
	}
	// A stale owner cannot dismantle an active materialized transaction. Abort
	// first arms R1 as rollback credit; Commit settles it itself.
	if capacity.active {
		return
	}
	capacity.held = false
	realm := capacity.realm
	releaseRotationRollbackLocked(realm, realm.panes[capacity.predecessor])
	if !capacity.materialized {
		realm.reservedCharge -= capacity.successorLogical
		realm.physicalReserved -= capacity.successorPhysical
		realm.retention.releaseCompletePane()
	}
}

// Release settles an unconsumed pre-composite capacity, or the remainder of
// R1 after an aborted materialized rotation has replayed into its predecessor.
// It is idempotent. While a RotationReservation is active it is a no-op: only
// that reservation may choose Commit or Abort.
func (capacity *RotationCapacity) Release() {
	if capacity == nil {
		return
	}
	realm := capacity.realm
	realm.retention.enter()
	defer realm.retention.leave()
	capacity.releaseLocked()
}

// ArmRollback idempotently exposes a pre-materialization R1 reservation as
// predecessor-only rollback credit; the reservation keeps backing the credit
// until it is consumed or Release settles it. The caller invokes it only after
// the tmux boundary has flipped and 4R must replay into the predecessor. While
// merely held, R1 remains invisible to ordinary predecessor output.
func (capacity *RotationCapacity) ArmRollback() error {
	if capacity == nil {
		return ErrInvalidated
	}
	realm := capacity.realm
	realm.retention.enter()
	defer realm.retention.leave()
	if realm.closed || !capacity.held || capacity.materialized || capacity.active {
		return ErrInvalidated
	}
	pane := realm.panes[capacity.predecessor]
	if pane == nil || !pane.rotationCapacityHeld {
		return ErrInvalidated
	}
	armRotationRollbackLocked(realm, pane)
	return nil
}

// RotationReservation owns the materialized successor, its temporary slot,
// and every unconsumed R2/R3 hold. Its linked RotationCapacity retains R1.
// Commit swaps the slot and settles R1; Abort retires the successor slot and
// arms R1 for rollback until the caller releases the capacity. Exactly one
// reservation settlement takes effect.
type RotationReservation struct {
	realm       *Realm
	predecessor PaneKey
	successor   PaneKey
	capacity    *RotationCapacity
	held        bool
	committed   bool
	failed      bool
}

// BeginRotatedPane materializes a successor by consuming capacity already
// held. It performs no logical, physical or slot-capacity decision; its only
// failures are invalid identity/geometry and storage materialization.
func (realm *Realm) BeginRotatedPane(key PaneKey, initial Geometry, capacity *RotationCapacity) (*RotationReservation, error) {
	realm.retention.enter()
	defer realm.retention.leave()
	if realm.closed || capacity == nil || capacity.realm != realm || !capacity.held ||
		capacity.materializationAttempted || capacity.materialized || capacity.active {
		return nil, ErrInvalidated
	}
	if !initial.valid() {
		return nil, ErrInvalidRecord
	}
	predecessor := realm.panes[capacity.predecessor]
	if predecessor == nil || !predecessor.admitted || predecessor.state != EligibilityContinuous || predecessor.slotRetired ||
		!predecessor.rotationCapacityHeld || predecessor.rotationRollbackArmed {
		return nil, ErrInvalidated
	}
	if key.Server != capacity.predecessor.Server || key.Session != capacity.predecessor.Session ||
		key.ControlGeneration == capacity.predecessor.ControlGeneration {
		return nil, ErrInvalidated
	}
	if realm.panes[key] != nil {
		return nil, ErrInvalidated
	}
	capacity.materializationAttempted = true
	chargedBefore := realm.physicalTotal
	pane, err := realm.createPaneWithCapacity(key, &initial, OriginRotated, true)
	if err != nil {
		// A header that failed to write stays charged unless the cleanup unlink
		// proved its bytes gone. Those bytes are also still inside the header
		// hold this capacity carries, so convert the exact retained charge out
		// of the reservation here, in the same sequenced operation: the realm
		// counts them once, as charge, and physicalTotal is never refunded on
		// this path. R1 is untouched — this failure is post-boundary and the
		// 4R replay still needs it.
		if retained := realm.physicalTotal - chargedBefore; retained > 0 {
			realm.physicalReserved -= retained
			capacity.successorPhysical -= retained
		}
		return nil, err
	}

	// Convert R2's worst-case header hold into the exact physical charge that
	// createPaneWithCapacity just took. The remainder of R2 and all of R3 move
	// to the successor; their realm-wide reservation was already established.
	realm.physicalReserved -= rotationHeaderPhysicalReserve
	capacity.successorPhysical -= rotationHeaderPhysicalReserve
	pane.reservedCharge += capacity.successorLogical
	pane.physicalReserved += capacity.successorPhysical
	pane.rotationForwardLogical = capacity.successorLogical
	pane.rotationForwardPhysical = capacity.successorPhysical
	pane.rotationForwardRecords = capacity.successorRecords
	capacity.successorLogical = 0
	capacity.successorPhysical = 0
	capacity.successorRecords = 0
	capacity.materialized = true
	capacity.active = true
	return &RotationReservation{realm: realm, predecessor: capacity.predecessor, successor: key, capacity: capacity, held: true}, nil
}

func releaseRotationRollbackLocked(realm *Realm, pane *paneJournal) {
	if pane == nil || !pane.rotationCapacityHeld {
		return
	}
	if pane.rotationRollbackReserved {
		realm.reservedCharge -= pane.rotationRollbackLogical
		realm.physicalReserved -= pane.rotationRollbackPhysical
		pane.reservedCharge -= pane.rotationRollbackLogical
		pane.physicalReserved -= pane.rotationRollbackPhysical
	}
	pane.rotationRollbackLogical = 0
	pane.rotationRollbackPhysical = 0
	pane.rotationRollbackRecords = 0
	pane.rotationRollbackReserved = false
	pane.rotationRollbackArmed = false
	pane.rotationCapacityHeld = false
}

// armRotationRollbackLocked exposes the reserved R1 to the predecessor as
// bounded spendable rollback credit. The reservation itself is untouched:
// the credit stays backed on the pane and the realm, is decremented record by
// record as the replay consumes it, and its remainder is settled by
// RotationCapacity.Release. That is what makes the packet's Abort -> replay ->
// Release ordering race-safe: R1 guarantees the capacity, so no ordinary
// allocation on any pane can take that room before the replay lands, and only
// the predecessor may spend it.
func armRotationRollbackLocked(realm *Realm, pane *paneJournal) {
	if pane == nil || !pane.rotationCapacityHeld || !pane.rotationRollbackReserved {
		return
	}
	pane.rotationRollbackArmed = true
}

func releaseRotationPaneReservationsLocked(realm *Realm, pane *paneJournal) {
	if pane == nil {
		return
	}
	if pane.rotationCapacityHeld || pane.rotationRollbackLogical != 0 || pane.rotationRollbackPhysical != 0 || pane.rotationRollbackRecords != 0 {
		releaseRotationRollbackLocked(realm, pane)
	}
	if pane.rotationForwardLogical != 0 || pane.rotationForwardPhysical != 0 || pane.rotationForwardRecords != 0 {
		realm.reservedCharge -= pane.rotationForwardLogical
		realm.physicalReserved -= pane.rotationForwardPhysical
		pane.reservedCharge -= pane.rotationForwardLogical
		pane.physicalReserved -= pane.rotationForwardPhysical
		pane.rotationForwardLogical = 0
		pane.rotationForwardPhysical = 0
		pane.rotationForwardRecords = 0
	}
}

// Commit makes the successor's slot durable and transactionally retires the
// predecessor's. Byte ledgers are refunded only after unlink or ENOENT proves
// the predecessor bytes gone; unlink failure retains a slot-retired map entry
// and both charges for the next widened stale sweep.
func (reservation *RotationReservation) Commit() {
	if reservation == nil {
		return
	}
	realm := reservation.realm
	realm.retention.enter()
	defer realm.retention.leave()
	if !reservation.held {
		return
	}
	reservation.held = false
	reservation.committed = true
	if reservation.capacity != nil {
		reservation.capacity.active = false
		releaseRotationRollbackLocked(realm, realm.panes[reservation.predecessor])
		reservation.capacity.held = false
	}
	releaseRotationPaneReservationsLocked(realm, realm.panes[reservation.successor])
	if predecessor := realm.panes[reservation.predecessor]; predecessor != nil && !predecessor.slotRetired {
		predecessor.slotRetired = true
		invalidatePane(predecessor, ReasonSourceReplacement)
		realm.retention.releaseCompletePane()
	}
	realm.sweepRetiredSessionLocked(reservation.successor)
}

// FailCommitted retires the successor after Commit crossed the journal point
// of no return but before the broker made rotation_pending durable. Commit has
// already settled the temporary reservation and predecessor slot, so this is
// deliberately not Fatal: it owns the now-authoritative successor slot and
// refunds its charged bytes only after unlink (or ENOENT) proves the backing
// journal is gone. An unlink error leaves a bounded retired generation whose
// charge remains represented until a later sweep can prove removal.
func (reservation *RotationReservation) FailCommitted() {
	if reservation == nil {
		return
	}
	realm := reservation.realm
	realm.retention.enter()
	defer realm.retention.leave()
	if !reservation.committed && !reservation.failed {
		return
	}
	reservation.failed = true
	reservation.committed = false
	pane := realm.panes[reservation.successor]
	if pane == nil {
		return
	}
	invalidatePane(pane, ReasonSourceReplacement)
	if !pane.slotRetired {
		pane.slotRetired = true
		realm.retention.releaseCompletePane()
	}
	if pane.file != nil {
		_ = realm.ops.close(pane.file)
		pane.file = nil
	}
	if !realm.removePaneFile(reservation.successor) {
		return
	}
	realm.retention.releaseProvenCapacity(pane.realmCharge > 0 || pane.physical > 0)
	realm.total -= pane.realmCharge
	pane.realmCharge = 0
	realm.physicalTotal -= pane.physical
	pane.physical = 0
	pane.releaseProjection()
	delete(realm.panes, reservation.successor)
}

// Abort returns the successor slot, invalidates and tombstones only the
// successor generation, and releases R2/R3. It leaves R1 owned by the
// RotationCapacity so the 4R restore replay can consume it before Release
// settles the unspent remainder. The predecessor remains live throughout.
func (reservation *RotationReservation) Abort() {
	if reservation == nil {
		return
	}
	realm := reservation.realm
	realm.retention.enter()
	defer realm.retention.leave()
	if !reservation.held {
		return
	}
	reservation.held = false
	if reservation.capacity != nil {
		reservation.capacity.active = false
	}
	armRotationRollbackLocked(realm, realm.panes[reservation.predecessor])
	releaseRotationPaneReservationsLocked(realm, realm.panes[reservation.successor])
	realm.retention.releaseCompletePane()
	pane := realm.panes[reservation.successor]
	if pane == nil {
		return
	}
	invalidatePane(pane, ReasonAdoptionAborted)
	pane.slotRetired = true
	if pane.file != nil {
		_ = realm.ops.close(pane.file)
		pane.file = nil
	}
	if !realm.removePaneFile(reservation.successor) {
		return
	}
	realm.retention.releaseProvenCapacity(pane.realmCharge > 0 || pane.physical > 0)
	realm.total -= pane.realmCharge
	pane.realmCharge = 0
	realm.physicalTotal -= pane.physical
	pane.physical = 0
	pane.releaseProjection()
}

// Fatal settles a materialized rotation after the predecessor crossed its
// retiring point of no return. Unlike Abort it never arms R1 and never makes
// predecessor output writable again: it refunds every unspent reservation,
// returns the temporary successor slot, and retires the provisional successor.
// It is idempotent and is the only journal settlement allowed after PONR.
func (reservation *RotationReservation) Fatal() {
	if reservation == nil {
		return
	}
	realm := reservation.realm
	realm.retention.enter()
	defer realm.retention.leave()
	if !reservation.held {
		return
	}
	reservation.held = false
	if reservation.capacity != nil {
		reservation.capacity.active = false
		predecessor := realm.panes[reservation.predecessor]
		if predecessor != nil {
			invalidatePane(predecessor, ReasonSourceReplacement)
			// Fatal is beyond the retiring seal, so neither generation may
			// inherit this complete-pane slot. Leaving the predecessor counted
			// would leak one slot across each fatal/re-adoption cycle.
			if !predecessor.slotRetired {
				predecessor.slotRetired = true
				realm.retention.releaseCompletePane()
			}
		}
		releaseRotationRollbackLocked(realm, predecessor)
		reservation.capacity.held = false
	}
	releaseRotationPaneReservationsLocked(realm, realm.panes[reservation.successor])
	realm.retention.releaseCompletePane()
	pane := realm.panes[reservation.successor]
	if pane == nil {
		return
	}
	invalidatePane(pane, ReasonSourceReplacement)
	pane.slotRetired = true
	if pane.file != nil {
		_ = realm.ops.close(pane.file)
		pane.file = nil
	}
	if !realm.removePaneFile(reservation.successor) {
		return
	}
	realm.retention.releaseProvenCapacity(pane.realmCharge > 0 || pane.physical > 0)
	realm.total -= pane.realmCharge
	pane.realmCharge = 0
	realm.physicalTotal -= pane.physical
	pane.physical = 0
	pane.releaseProjection()
}

func (realm *Realm) admitPane(key PaneKey, initial Geometry, origin GenerationOrigin) error {
	realm.retention.enter()
	defer realm.retention.leave()
	if realm.closed {
		return ErrInvalidated
	}
	if !initial.valid() {
		return ErrInvalidRecord
	}
	if pane := realm.panes[key]; pane != nil {
		// Idempotent only for the identical live continuous generation: same
		// geometry AND same origin. A reused key with a different origin is a
		// re-admission of a different life and fails closed.
		if pane.admitted && pane.initial == initial && pane.state == EligibilityContinuous && pane.origin == origin {
			return nil
		}
		return ErrInvalidated
	}
	if err := realm.retention.reserveCompletePane(true); err != nil {
		return err
	}
	if _, err := realm.createPane(key, &initial, origin); err != nil {
		realm.retention.releaseCompletePane()
		return err
	}
	return nil
}

// Origin reports how the generation began. A pre-adoption header without the
// field reads as birth, because every generation written before adoption
// existed was a birth.
func (realm *Realm) Origin(key PaneKey) (GenerationOrigin, error) {
	pane := realm.panes[key]
	if pane == nil {
		return "", ErrInvalidated
	}
	if pane.corrupt || pane.reason == ReasonCorruptJournal {
		return "", ErrCorruptJournal
	}
	if pane.origin == "" {
		return OriginBirth, nil
	}
	return pane.origin, nil
}

// InitialGeometry returns the generation's durable birth geometry.
func (realm *Realm) InitialGeometry(key PaneKey) (Geometry, error) {
	pane := realm.panes[key]
	if pane == nil {
		return Geometry{}, ErrInvalidated
	}
	if pane.corrupt || pane.reason == ReasonCorruptJournal {
		return Geometry{}, ErrCorruptJournal
	}
	if !pane.admitted {
		return Geometry{}, ErrInvalidated
	}
	return pane.initial, nil
}

// ReadCommittedEvents is the committed event projection. Initial snapshot and
// live tail are built from this one representation, so they cannot drift.
func (realm *Realm) ReadCommittedEvents(key PaneKey) ([]Event, error) {
	pane, err := realm.committedEventsPane(key)
	if err != nil {
		return nil, err
	}
	events := make([]Event, pane.committedSequence)
	// One caller-owned payload allocation avoids a separate rounded allocation
	// for every tiny record. Full slice expressions prevent one event from
	// extending into another event's bytes.
	payload := make([]byte, pane.committed)
	for page := pane.verified.view; page != nil; page = page.previous {
		event := page.event
		if event.Kind == RecordOutput {
			event.Payload = payload[event.Start:event.End:event.End]
			copyEventPayload(event.Payload, page)
		}
		events[event.Sequence-1] = event
	}
	return events, nil
}

func (realm *Realm) committedEventsPane(key PaneKey) (*paneJournal, error) {
	pane := realm.panes[key]
	if pane == nil {
		return nil, ErrInvalidated
	}
	if pane.corrupt || pane.reason == ReasonCorruptJournal {
		return nil, ErrCorruptJournal
	}
	if pane.projectionReleased {
		return nil, ErrInvalidated
	}
	if !pane.admitted {
		return nil, ErrInvalidated
	}
	return pane, nil
}

// CommittedSequence is the sequence projection of the committed frontier. Byte
// offsets cannot serve that role once zero-length records exist.
func (realm *Realm) CommittedSequence(key PaneKey) int64 {
	if pane := realm.panes[key]; pane != nil {
		return pane.committedSequence
	}
	return 0
}

func (realm *Realm) Append(key PaneKey, payload []byte) (Record, error) {
	realm.retention.enter()
	defer realm.retention.leave()
	if realm.closed {
		return Record{}, ErrInvalidated
	}
	pane := realm.panes[key]
	if pane != nil && pane.state != EligibilityContinuous {
		return Record{}, ErrInvalidated
	}
	if pane == nil {
		if err := realm.retention.reserveCompletePane(true); err != nil {
			return Record{}, err
		}
		var err error
		pane, err = realm.createPane(key, nil, OriginBirth)
		if err != nil {
			realm.retention.releaseCompletePane()
			return Record{}, err
		}
	}
	// Two ledgers, two units, both checked before anything is written.
	//
	// The logical cap bounds the pane's live charge — output payload plus the
	// fixed cost of every geometry record — the same quantity the realm cap
	// bounds in aggregate and the one a reopen rebuilds from the parsed file.
	// The physical cap bounds what the file occupies — this append's frame,
	// its payload, and the commit frame that will follow — the quantity a
	// reopen rebuilds from the file's size. Each budget is measured in the
	// unit it is spent in; a budget measured in one unit while spent in
	// another lies about its own headroom, and that lie is how a logical cap
	// let framing overhead reach the filesystem.
	logicalCost := int64(len(payload))
	var heldLogical, heldPhysical, heldRecords *int64
	holdReserved := false
	forwardCredit := false
	if pane.rotationForwardLogical > 0 {
		heldLogical = &pane.rotationForwardLogical
		heldPhysical = &pane.rotationForwardPhysical
		heldRecords = &pane.rotationForwardRecords
		holdReserved = true
		forwardCredit = true
	} else if pane.rotationCapacityHeld && pane.rotationRollbackArmed &&
		pane.rotationRollbackLogical > 0 && pane.rotationRollbackRecords > 0 {
		// Only ArmRollback or RotationReservation.Abort makes R1 visible to
		// predecessor Append. When either the byte or record dimension is
		// exhausted, later predecessor output falls through to ordinary room.
		heldLogical = &pane.rotationRollbackLogical
		heldPhysical = &pane.rotationRollbackPhysical
		heldRecords = &pane.rotationRollbackRecords
		holdReserved = pane.rotationRollbackReserved
	} else if pane.adoptionLogical > 0 || pane.adoptionPhysical > 0 {
		// A provisional reconstructed successor spends its supersession
		// allowance first: exactly the charges its session's stale
		// generations were proven gone for, held for it alone by
		// BeginReconstructedPane. The allowance is byte-exact in both units
		// and has no record shape; once a unit is exhausted the remainder of
		// a record falls through to ordinary room.
		heldLogical = &pane.adoptionLogical
		heldPhysical = &pane.adoptionPhysical
		holdReserved = true
	}
	recordLimited := forwardCredit && heldLogical != nil && *heldLogical > 0 && *heldRecords == 0
	if recordLimited {
		return Record{}, ErrRotationReplayRecordQuota
	}
	logicalHeld := int64(0)
	physicalHeld := int64(0)
	// A record-shaped hold (rotation) is spendable while it has records left;
	// the byte-exact adoption allowance is spendable while it has bytes left.
	drawsHeld := heldLogical != nil && (heldRecords == nil || *heldRecords > 0)
	if drawsHeld {
		logicalHeld = logicalCost
		if logicalHeld > *heldLogical {
			logicalHeld = *heldLogical
		}
	}
	logicalExtra := logicalCost - logicalHeld
	if !realm.logicalRoom(pane, logicalExtra) {
		invalidatePane(pane, ReasonQuota)
		return Record{}, ErrQuota
	}
	cost := physicalAppendCost(len(payload))
	if drawsHeld {
		physicalHeld = cost
		if heldRecords != nil {
			// A record-shaped hold (rotation R2/R3, R1) is sized per replay
			// record: its physical slice for one record is the held payload
			// plus that one record's framing, never more, so a record that
			// spills past the logical hold pays its spill's framing from
			// ordinary room.
			if maxForHeldPayload := logicalHeld + int64(appendFixed+commitFixed); physicalHeld > maxForHeldPayload {
				physicalHeld = maxForHeldPayload
			}
		}
		// The adoption allowance is two independent, proof-derived ledgers
		// whose physical hold is spent up to this append's exact
		// physical cost whatever the logical hold contributed. A stale
		// generation that was physical-full from framing while holding
		// little payload refunds exactly that shape, and coupling the two
		// would refuse a bootstrap whose whole cost is inside the hold.
		if physicalHeld > *heldPhysical {
			physicalHeld = *heldPhysical
		}
	}
	physicalExtra := cost - physicalHeld
	if !realm.physicalRoom(pane, physicalExtra) {
		invalidatePane(pane, ReasonPhysicalQuota)
		return Record{}, ErrPhysicalQuota
	}
	// Convert the held part of this record's cost into actual charge. A
	// rotation record beyond the exported shape can use only ordinary room;
	// if the held bytes make that room unavailable, it is a typed refusal and
	// never invalidates the generation.
	if drawsHeld {
		*heldLogical -= logicalHeld
		*heldPhysical -= physicalHeld
		if heldRecords != nil {
			(*heldRecords)--
		}
	}
	if holdReserved {
		pane.reservedCharge -= logicalHeld
		realm.reservedCharge -= logicalHeld
		pane.physicalReserved -= physicalHeld
		realm.physicalReserved -= physicalHeld
	}
	record := Record{
		Key: key, Kind: RecordOutput, Sequence: pane.sequence + 1,
		Start: pane.end, End: pane.end + int64(len(payload)), Hash: sha256.Sum256(payload),
	}
	// Charged before the write and never refunded on failure: a torn frame
	// occupies the filesystem as surely as a whole one.
	pane.physical += cost
	realm.physicalTotal += cost
	if err := realm.writeRecord(pane, record, payload); err != nil {
		return Record{}, err
	}
	pane.end = record.End
	pane.realmCharge += logicalCost
	realm.total += logicalCost
	return record, nil
}

// GeometryReservation holds, ahead of its write, every unit of journal
// capacity one geometry record and its commit will consume: the logical
// record cost against the pane and realm caps, and the physical append+commit
// cost against the pane and realm physical caps. It exists so a caller that
// must know BEFORE mutating an external system (tmux) whether the journal will
// accept the geometry can find out: a refused reservation changes nothing,
// and a held one cannot be consumed by any other append.
//
// Exactly one of AppendReservedGeometry or Release settles a reservation;
// later calls are no-ops.
type GeometryReservation struct {
	realm *Realm
	key   PaneKey
	held  bool
}

// ReserveGeometry takes a geometry reservation for key. It refuses with
// ErrQuota (logical) or ErrPhysicalQuota (physical, which wraps ErrQuota)
// WITHOUT invalidating the generation: nothing was written and the journal's
// durable truth is intact, so the refusal is one request's outcome. Output
// that later meets the same cap fails closed as it always has.
func (realm *Realm) ReserveGeometry(key PaneKey) (*GeometryReservation, error) {
	realm.retention.enter()
	defer realm.retention.leave()
	if realm.closed {
		return nil, ErrInvalidated
	}
	pane := realm.panes[key]
	if pane == nil || !pane.admitted || pane.state != EligibilityContinuous {
		return nil, ErrInvalidated
	}
	if !realm.logicalRoom(pane, geometryRecordCost) {
		return nil, ErrQuota
	}
	if !realm.physicalRoom(pane, geometryRecordCost) {
		return nil, ErrPhysicalQuota
	}
	pane.reservedCharge += geometryRecordCost
	realm.reservedCharge += geometryRecordCost
	pane.physicalReserved += geometryRecordCost
	realm.physicalReserved += geometryRecordCost
	return &GeometryReservation{realm: realm, key: key, held: true}, nil
}

// releaseLocked returns the reserved capacity to both ledgers. The caller
// holds the realm's admission order.
func (reservation *GeometryReservation) releaseLocked() {
	if !reservation.held {
		return
	}
	reservation.held = false
	realm := reservation.realm
	realm.reservedCharge -= geometryRecordCost
	realm.physicalReserved -= geometryRecordCost
	if pane := realm.panes[reservation.key]; pane != nil {
		pane.reservedCharge -= geometryRecordCost
		pane.physicalReserved -= geometryRecordCost
	}
}

// Release refunds an unconsumed reservation. Safe to call after the
// reservation was consumed or released.
func (reservation *GeometryReservation) Release() {
	if reservation == nil {
		return
	}
	realm := reservation.realm
	realm.retention.enter()
	defer realm.retention.leave()
	reservation.releaseLocked()
}

// Held reports whether the reservation still holds capacity.
func (reservation *GeometryReservation) Held() bool {
	return reservation != nil && reservation.held
}

// AppendReservedGeometry appends a typed geometry event against capacity the
// reservation already holds. It consumes the reservation whatever the
// outcome, performs no cap check — the reservation IS the check, and nothing
// else can have spent what it held — and otherwise behaves exactly as
// AppendGeometry.
func (realm *Realm) AppendReservedGeometry(reservation *GeometryReservation, geometry Geometry) (Record, error) {
	realm.retention.enter()
	defer realm.retention.leave()
	if reservation == nil || reservation.realm != realm || !reservation.held {
		return Record{}, ErrInvalidRecord
	}
	reservation.releaseLocked()
	if realm.closed {
		return Record{}, ErrInvalidated
	}
	pane := realm.panes[reservation.key]
	if pane == nil || !pane.admitted || pane.state != EligibilityContinuous {
		return Record{}, ErrInvalidated
	}
	if !geometry.valid() {
		return Record{}, ErrInvalidRecord
	}
	return realm.appendGeometryLocked(pane, geometry)
}

// AppendGeometry appends a typed geometry event. It carries no payload, so it is
// charged a fixed record cost against the same budgets: durable growth must stay
// bounded whether it comes from bytes or from events.
func (realm *Realm) AppendGeometry(key PaneKey, geometry Geometry) (Record, error) {
	realm.retention.enter()
	defer realm.retention.leave()
	if realm.closed {
		return Record{}, ErrInvalidated
	}
	pane := realm.panes[key]
	if pane == nil || !pane.admitted {
		return Record{}, ErrInvalidated
	}
	if pane.state != EligibilityContinuous {
		return Record{}, ErrInvalidated
	}
	if !geometry.valid() {
		return Record{}, ErrInvalidRecord
	}
	// Charged against the pane's live charge, exactly as output is. This check
	// once measured the file size instead: a busy pane's framing overhead (128
	// bytes per output record, never charged) then exceeded the cap long before
	// its payload did, so the first explicit Fit on a long-lived session was
	// refused as over quota — and invalidated the generation — while output
	// still had headroom. A budget has one unit — and the file's occupancy has
	// its own budget, the physical ledger, checked beside it.
	if !realm.logicalRoom(pane, geometryRecordCost) {
		invalidatePane(pane, ReasonQuota)
		return Record{}, ErrQuota
	}
	if !realm.physicalRoom(pane, geometryRecordCost) {
		invalidatePane(pane, ReasonPhysicalQuota)
		return Record{}, ErrPhysicalQuota
	}
	return realm.appendGeometryLocked(pane, geometry)
}

// appendGeometryLocked writes one geometry record whose capacity has already
// been admitted, charging both ledgers. The physical cost of a geometry
// record is its frame plus its commit frame, which is also its logical cost.
func (realm *Realm) appendGeometryLocked(pane *paneJournal, geometry Geometry) (Record, error) {
	record := Record{
		Key: pane.key, Kind: RecordGeometry, Sequence: pane.sequence + 1,
		Start: pane.end, End: pane.end, Hash: geometryHash(pane.sequence+1, geometry), Geometry: geometry,
	}
	pane.physical += geometryRecordCost
	realm.physicalTotal += geometryRecordCost
	if err := realm.writeRecord(pane, record, nil); err != nil {
		return Record{}, err
	}
	pane.realmCharge += geometryRecordCost
	realm.total += geometryRecordCost
	return record, nil
}

// PhysicalBudget reports the realm's physical ledger in bytes on disk: the
// capacity charged to generations (including corrupt and slot-retired ones
// whose files survive), the capacity held by live geometry reservations, and
// the realm cap fixed at open.
func (realm *Realm) PhysicalBudget() (charged, reserved, cap int64) {
	return realm.physicalTotal, realm.physicalReserved, realm.physicalCap
}

// LogicalBudget reports the realm's semantic-byte ledger. It mirrors
// PhysicalBudget so lifecycle regression tests can prove terminal cleanup returned
// both independent durable ledgers to their exact baseline.
func (realm *Realm) LogicalBudget() (charged, reserved, cap int64) {
	return realm.total, realm.reservedCharge, realm.options.RealmCapBytes
}

// PanePhysical reports one generation's physical charge and the per-pane
// physical cap. A key with no generation reports zero charge.
func (realm *Realm) PanePhysical(key PaneKey) (charged, cap int64) {
	if pane := realm.panes[key]; pane != nil {
		charged = pane.physical
	}
	return charged, realm.panePhysicalCap
}

// PaneLogical reports one generation's charged semantic bytes and the
// per-pane cap. Reservations are deliberately reported separately from
// charge: this accessor is the durable amount used by rotation policy, while
// admission itself accounts for both charged and reserved capacity.
func (realm *Realm) PaneLogical(key PaneKey) (charged, cap int64) {
	if pane := realm.panes[key]; pane != nil {
		charged = pane.realmCharge
	}
	return charged, realm.options.PaneCapBytes
}

// VisitRotationPressure visits the existing bounded generation identities.
// Like the other ledger accessors, the caller owns journal serialization.
// The callback must only copy values; it must not perform I/O or reenter Realm.
func (realm *Realm) VisitRotationPressure(visit func(PaneKey, int64, int64, int64, int64)) {
	for key, pane := range realm.panes {
		visit(key, pane.realmCharge, realm.options.PaneCapBytes, realm.physicalTotal, realm.physicalCap)
	}
}

func (realm *Realm) writeRecord(pane *paneJournal, record Record, payload []byte) error {
	if err := realm.writeAll(pane.file, encodeAppend(record, payload)); err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			invalidatePane(pane, ReasonENOSPC)
		} else {
			invalidatePane(pane, ReasonCorruptJournal)
		}
		return ErrStorage
	}
	pane.sequence = record.Sequence
	pane.last = record
	pending := &pendingRecord{record: record}
	if pane.pendingTail == nil {
		pane.pendingHead = pending
	} else {
		pane.pendingTail.next = pending
	}
	pane.pendingTail = pending
	pane.pendingCount++
	pane.stored += int64(appendFixed + len(payload))
	return nil
}

func encodeAppend(record Record, payload []byte) []byte {
	frame := make([]byte, appendFixed+len(payload))
	frame[0] = appendMarker
	frame[1] = byte(record.Kind)
	binary.BigEndian.PutUint64(frame[2:10], uint64(record.Sequence))
	binary.BigEndian.PutUint64(frame[10:18], uint64(record.Start))
	binary.BigEndian.PutUint64(frame[18:26], uint64(record.End))
	binary.BigEndian.PutUint32(frame[26:30], uint32(len(payload)))
	binary.BigEndian.PutUint32(frame[30:34], uint32(record.Geometry.Columns))
	binary.BigEndian.PutUint32(frame[34:38], uint32(record.Geometry.Rows))
	copy(frame[38:38+sha256.Size], record.Hash[:])
	copy(frame[appendFixed:], payload)
	return frame
}

func encodeCommit(record Record) []byte {
	frame := make([]byte, commitFixed)
	frame[0] = commitMarker
	frame[1] = byte(record.Kind)
	binary.BigEndian.PutUint64(frame[2:10], uint64(record.Sequence))
	binary.BigEndian.PutUint64(frame[10:18], uint64(record.Start))
	binary.BigEndian.PutUint64(frame[18:26], uint64(record.End))
	copy(frame[26:], record.Hash[:])
	return frame
}

func (realm *Realm) writeAll(file *os.File, data []byte) error {
	for len(data) != 0 {
		written, err := realm.ops.write(file, data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func (realm *Realm) Sync(key PaneKey) error {
	pane := realm.panes[key]
	if pane == nil || pane.state != EligibilityContinuous || realm.closed {
		return ErrInvalidated
	}
	if err := realm.ops.sync(pane.file); err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			invalidatePane(pane, ReasonENOSPC)
		} else {
			invalidatePane(pane, ReasonCorruptJournal)
		}
		return ErrStorage
	}
	return nil
}

func (realm *Realm) AdvanceCommitted(key PaneKey, record Record) error {
	pane := realm.panes[key]
	if pane == nil || pane.state != EligibilityContinuous || realm.closed {
		return ErrInvalidated
	}
	if pane.last != record || record.Key != key || record.Sequence != pane.sequence ||
		record.Sequence <= pane.committedSequence || record.End != pane.end {
		invalidatePane(pane, ReasonCorruptJournal)
		return ErrInvalidRecord
	}
	if err := realm.writeAll(pane.file, encodeCommit(record)); err != nil || realm.ops.sync(pane.file) != nil {
		invalidatePane(pane, ReasonENOSPC)
		return ErrStorage
	}
	pane.stored += commitFixed
	verified, err := realm.verifyCommitted(pane, record)
	if err != nil {
		invalidatePane(pane, ReasonCorruptJournal)
		return ErrCorruptJournal
	}
	// The caller serializes journal operations. No observer can see a new
	// frontier before its immutable readback view has passed verification.
	pane.verified = verified
	pane.committed = verified.committed
	pane.committedSequence = verified.committedSequence
	pane.pendingHead, pane.pendingTail, pane.pendingCount = nil, nil, 0
	return nil
}

func invalidatePane(pane *paneJournal, reason InvalidationReason) {
	if pane.state != EligibilityUntrusted {
		pane.state = EligibilityUntrusted
		pane.reason = reason
	}
}

func (realm *Realm) CommittedOffset(key PaneKey) int64 {
	if pane := realm.panes[key]; pane != nil {
		return pane.committed
	}
	return 0
}

func (realm *Realm) EndOffset(key PaneKey) int64 {
	if pane := realm.panes[key]; pane != nil {
		return pane.end
	}
	return 0
}

func (realm *Realm) Eligibility(key PaneKey) EligibilityState {
	if pane := realm.panes[key]; pane != nil {
		return pane.state
	}
	return EligibilityLegacy
}

func (realm *Realm) Reason(key PaneKey) InvalidationReason {
	if pane := realm.panes[key]; pane != nil {
		return pane.reason
	}
	return ReasonNone
}

// AvailableCompletePaneSlots reports advisory ordinary-admission headroom;
// the rotation-only final slot is excluded. It remains useful for projection
// and UX, but reserveCompletePane is the only admission authority, so races in
// this read can only make a later admission refuse, never over-grant.
func (realm *Realm) AvailableCompletePaneSlots() int64 {
	return realm.retention.availableCompletePanes()
}

// UnifiedEligible requires both a live continuous generation and a durable birth
// geometry. Bytes alone are not enough: replay cannot size a terminal it was
// never told the birth geometry of.
func (realm *Realm) UnifiedEligible(key PaneKey) bool {
	pane := realm.panes[key]
	return pane != nil && pane.state == EligibilityContinuous && pane.admitted
}

func (realm *Realm) ReadCommitted(key PaneKey) ([]byte, error) {
	pane := realm.panes[key]
	if pane == nil {
		return nil, nil
	}
	if pane.corrupt || pane.reason == ReasonCorruptJournal {
		return nil, ErrCorruptJournal
	}
	if pane.projectionReleased {
		return nil, ErrInvalidated
	}
	data := make([]byte, pane.committed)
	for page := pane.verified.view; page != nil; page = page.previous {
		if page.event.Kind == RecordOutput {
			copyEventPayload(data[page.event.Start:page.event.End], page)
		}
	}
	return data, nil
}

func (realm *Realm) Recovered() []Recovery {
	return append([]Recovery(nil), realm.recovered...)
}

// ReconcileRecovered applies one closed, typed inventory decision per scanned
// generation. Files are already charged before this runs. Only an
// authoritative absent/replacement decision may enter proof-bearing
// retirement; ambiguity and corrupt identity retain both the file and charge.
func (realm *Realm) ReconcileRecovered(decisions []RecoveryDecision) []Recovery {
	byKey := make(map[PaneKey]RecoveryDisposition, len(decisions))
	for _, decision := range decisions {
		byKey[decision.Key] = decision.Disposition
	}
	for index := range realm.recovered {
		recovery := &realm.recovered[index]
		pane := realm.panes[recovery.Key]
		if pane == nil {
			continue
		}
		disposition := byKey[recovery.Key]
		if disposition == 0 {
			disposition = RecoveryRetainAmbiguous
		}
		recovery.Decision = disposition
		if pane.corrupt || pane.reason == ReasonCorruptJournal {
			recovery.Outcome = RecoveryRetainedCorrupt
		} else {
			switch disposition {
			case RecoveryKeepExact:
				recovery.Outcome = RecoveryKeptExact
			case RecoveryRetireAbsent, RecoveryRetireReplacement:
				if realm.RetirePane(recovery.Key) {
					if disposition == RecoveryRetireAbsent {
						recovery.Outcome = RecoveryRetiredAbsent
					} else {
						recovery.Outcome = RecoveryRetiredStale
					}
				} else {
					recovery.Outcome = RecoveryRetainedError
				}
			case RecoveryRetainAmbiguous:
				recovery.Outcome = RecoveryRetainedAmbiguous
			default:
				recovery.Outcome = RecoveryRetainedError
			}
		}
		pane = realm.panes[recovery.Key]
		if pane == nil {
			recovery.RetainedLogical = 0
			recovery.RetainedPhysical = 0
			recovery.RetainedSlot = false
		} else {
			recovery.RetainedLogical = pane.realmCharge
			recovery.RetainedPhysical = pane.physical
			recovery.RetainedSlot = !pane.slotRetired
		}
		recovery.RetryNeeded = recovery.Outcome == RecoveryRetainedError
	}
	return realm.Recovered()
}

func (realm *Realm) Close() error {
	if realm == nil {
		return nil
	}
	if realm.retention != nil {
		realm.retention.enter()
		defer realm.retention.leave()
	}
	if realm.closed {
		return nil
	}
	realm.closed = true
	failed := false
	for _, pane := range realm.panes {
		if pane.file != nil && realm.ops.close(pane.file) != nil {
			failed = true
		}
	}
	if realm.realmFD >= 0 && realm.ops.closeFD(realm.realmFD) != nil {
		failed = true
	}
	if realm.rootFD >= 0 && realm.ops.closeFD(realm.rootFD) != nil {
		failed = true
	}
	if failed {
		return ErrStorage
	}
	return nil
}
