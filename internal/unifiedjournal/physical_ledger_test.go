package unifiedjournal

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

const physicalRed = "RESIZE_REVIEW/PHYSICAL_LEDGER"

// physicalOptions pins both physical caps explicitly so the ledger under test
// is not the one derived from the test host's filesystem.
func physicalOptions(t *testing.T, panePhysical, realmPhysical int64) OpenOptions {
	t.Helper()
	options := journalOptions(t)
	options.PanePhysicalCapBytes = panePhysical
	options.PhysicalCapBytes = realmPhysical
	return options
}

// physicalJournalPath is the pane file's path without the single-pane
// storage-seam assertion journalPathFor carries; these contracts hold several
// panes at once.
func physicalJournalPath(options OpenOptions, key PaneKey) string {
	return storagePathsForTest(options, key).Journal
}

func journalFileSize(t *testing.T, options OpenOptions, key PaneKey) int64 {
	t.Helper()
	info, err := os.Stat(physicalJournalPath(options, key))
	if err != nil {
		t.Fatalf("%s stat journal: %v", physicalRed, err)
	}
	return info.Size()
}

// Tiny writes: the logical cap never binds, the physical cap does, the file
// never exceeds it, and the refusal is the quota refusal with the physical
// reason.
func TestPhysicalLedgerBoundsFramingHeavyPaneBeforeLogicalCap(t *testing.T) {
	options := physicalOptions(t, 8<<10, 1<<20)
	options.PaneCapBytes = 64 << 10
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", physicalRed, err)
	}
	defer realm.Close()
	key := journalKey("%1", "pane-inc-a")
	admitJournalPane(t, realm, key)
	records := 0
	var refusal error
	for records < 100000 {
		record, err := realm.Append(key, []byte{'x'})
		if err != nil {
			refusal = err
			break
		}
		commitLast(t, realm, key, record)
		records++
	}
	if refusal == nil {
		t.Fatalf("%s one-byte appends never met the physical cap", physicalRed)
	}
	if !errors.Is(refusal, ErrPhysicalQuota) || !errors.Is(refusal, ErrQuota) {
		t.Fatalf("%s refusal=%v want ErrPhysicalQuota wrapping ErrQuota", physicalRed, refusal)
	}
	pane := realm.panes[key]
	if pane.state != EligibilityUntrusted || pane.reason != ReasonPhysicalQuota {
		t.Fatalf("%s physical exhaustion did not fail closed: state=%v reason=%v", physicalRed, pane.state, pane.reason)
	}
	if pane.realmCharge != int64(records) || pane.realmCharge >= options.PaneCapBytes/4 {
		t.Fatalf("%s logical charge=%d: the logical cap bound first", physicalRed, pane.realmCharge)
	}
	size := journalFileSize(t, options, key)
	if size > options.PanePhysicalCapBytes || pane.physical > options.PanePhysicalCapBytes || size > pane.physical {
		t.Fatalf("%s file=%d charged=%d cap=%d: the file escaped the ledger", physicalRed, size, pane.physical, options.PanePhysicalCapBytes)
	}
	if _, err := realm.Append(key, []byte("later")); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("%s invalidated pane continued: %v", physicalRed, err)
	}
	// Geometry on the dead generation is refused for the same reason, and a
	// reservation reports the invalidation, not a fresh refusal.
	if _, err := realm.ReserveGeometry(key); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("%s reservation on a dead generation: %v", physicalRed, err)
	}
}

// Multiple panes share the realm's physical cap: each stays under its own
// pane cap, and the realm cap refuses the one that would cross it while the
// sum of files stays within the budget.
func TestPhysicalLedgerRealmCapCountsEveryPaneFile(t *testing.T) {
	options := physicalOptions(t, 1<<20, 24<<10)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", physicalRed, err)
	}
	defer realm.Close()
	a := journalKey("%1", "pane-inc-a")
	b := journalKey("%2", "pane-inc-b")
	admitJournalPane(t, realm, a)
	admitJournalPane(t, realm, b)
	payload := make([]byte, 1024)
	var refusal error
	for i := 0; i < 64 && refusal == nil; i++ {
		key := a
		if i%2 == 1 {
			key = b
		}
		record, err := realm.Append(key, payload)
		if err != nil {
			refusal = err
			break
		}
		commitLast(t, realm, key, record)
	}
	if !errors.Is(refusal, ErrPhysicalQuota) {
		t.Fatalf("%s realm physical cap never bound: %v", physicalRed, refusal)
	}
	charged, reserved, cap := realm.PhysicalBudget()
	sum := journalFileSize(t, options, a) + journalFileSize(t, options, b)
	if cap != options.PhysicalCapBytes || reserved != 0 || sum > cap || sum > charged {
		t.Fatalf("%s files=%d charged=%d reserved=%d cap=%d", physicalRed, sum, charged, reserved, cap)
	}
	if realm.panes[a].realmCharge >= options.PaneCapBytes/8 || realm.panes[b].realmCharge >= options.PaneCapBytes/8 {
		t.Fatalf("%s a logical cap bound instead of the realm physical cap", physicalRed)
	}
}

// Restart reconstruction: after a reopen the ledger equals the sum of the
// files' sizes exactly — header, framing, payload, commit frames, an
// uncommitted append, and a torn trailing record all included, for a parsed
// generation and a corrupt one alike.
func TestPhysicalLedgerReconstructsFromFileSizesIncludingTornRecords(t *testing.T) {
	options := physicalOptions(t, 1<<20, 8<<20)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", physicalRed, err)
	}
	a := journalKey("%1", "pane-inc-a")
	b := journalKey("%2", "pane-inc-b")
	admitJournalPane(t, realm, a)
	admitJournalPane(t, realm, b)
	for i := 0; i < 5; i++ {
		record, err := realm.Append(a, []byte("alpha-output"))
		if err != nil {
			t.Fatal(err)
		}
		commitLast(t, realm, a, record)
	}
	geometry, err := realm.AppendGeometry(a, Geometry{Columns: 80, Rows: 40})
	if err != nil {
		t.Fatal(err)
	}
	commitLast(t, realm, a, geometry)
	if _, err := realm.Append(a, []byte("uncommitted-tail")); err != nil {
		t.Fatal(err)
	}
	record, err := realm.Append(b, []byte("beta-output"))
	if err != nil {
		t.Fatal(err)
	}
	commitLast(t, realm, b, record)
	if err := realm.Close(); err != nil {
		t.Fatal(err)
	}
	// Tear b: a partial append frame after its last commit, then corrupt a's
	// tail so its parse fails outright. Both files still occupy their bytes.
	torn := []byte{appendMarker, byte(RecordOutput), 0, 0, 0, 0, 0}
	for _, tear := range []struct {
		key   PaneKey
		bytes []byte
	}{{b, torn}, {a, []byte("\xff\xfe\xfdgarbage that no parser accepts")}} {
		file, err := os.OpenFile(physicalJournalPath(options, tear.key), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(tear.bytes); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
	}
	sizeA, sizeB := journalFileSize(t, options, a), journalFileSize(t, options, b)

	reopened, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s reopen: %v", physicalRed, err)
	}
	defer reopened.Close()
	charged, reserved, _ := reopened.PhysicalBudget()
	if charged != sizeA+sizeB || reserved != 0 {
		t.Fatalf("%s reopened ledger=%d reserved=%d want %d (a=%d b=%d)", physicalRed, charged, reserved, sizeA+sizeB, sizeA, sizeB)
	}
	if got, _ := reopened.PanePhysical(a); got != sizeA {
		t.Fatalf("%s pane a physical=%d want file size %d", physicalRed, got, sizeA)
	}
	if got, _ := reopened.PanePhysical(b); got != sizeB {
		t.Fatalf("%s pane b physical=%d want file size %d", physicalRed, got, sizeB)
	}
	if reopened.panes[a].reason != ReasonCorruptJournal {
		t.Fatalf("%s corrupted a was not classified corrupt: reason=%v", physicalRed, reopened.panes[a].reason)
	}
	if reopened.panes[b].corrupt {
		t.Fatalf("%s a torn trailing append is outside committed authority, not corruption", physicalRed)
	}
	// A generation the reopen could not trust still occupies the realm's budget:
	// a new pane on this realm sees the reduced headroom.
	c := journalKey("%3", "pane-inc-c")
	reopened.options.PhysicalCapBytes = charged + 8 + headerLimit/8
	reopened.physicalCap = reopened.options.PhysicalCapBytes
	admitJournalPane(t, reopened, c)
	if _, err := reopened.Append(c, make([]byte, headerLimit/8)); !errors.Is(err, ErrPhysicalQuota) {
		t.Fatalf("%s surviving files did not count against a new pane: %v", physicalRed, err)
	}
}

// Live accounting is conservative: a charged byte is never more than the
// ledger says, and the ledger over-counts only by the pending commit frames
// it reserved ahead of their writes.
func TestPhysicalLedgerChargesAppendAndCommitBeforeWriting(t *testing.T) {
	options := physicalOptions(t, 1<<20, 8<<20)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", physicalRed, err)
	}
	defer realm.Close()
	key := journalKey("%1", "pane-inc-a")
	admitJournalPane(t, realm, key)
	pane := realm.panes[key]
	header := journalFileSize(t, options, key)
	if pane.physical != header {
		t.Fatalf("%s header charge=%d file=%d", physicalRed, pane.physical, header)
	}
	record, err := realm.Append(key, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	afterAppend := journalFileSize(t, options, key)
	if pane.physical != afterAppend+int64(commitFixed) {
		t.Fatalf("%s append charged %d, file=%d: the commit frame was not reserved with the append", physicalRed, pane.physical, afterAppend)
	}
	commitLast(t, realm, key, record)
	if pane.physical != journalFileSize(t, options, key) {
		t.Fatalf("%s after commit charged=%d file=%d", physicalRed, pane.physical, journalFileSize(t, options, key))
	}
	// A full physical pane refuses exactly when append+commit would not fit.
	room := options.PanePhysicalCapBytes - pane.physical
	exact := int(room) - appendFixed - commitFixed
	if _, err := realm.Append(key, make([]byte, exact+1)); !errors.Is(err, ErrPhysicalQuota) {
		t.Fatalf("%s one byte over the physical room was admitted: %v", physicalRed, err)
	}
}

// Forced ENOSPC at the write: the append fails closed with the storage
// reason, and the physical charge stays — the bytes may be on disk — so the
// ledger never under-counts what the filesystem holds.
func TestPhysicalLedgerKeepsChargeAcrossENOSPC(t *testing.T) {
	options := physicalOptions(t, 1<<20, 8<<20)
	ops := realJournalOps()
	fail := false
	ops.write = func(file *os.File, data []byte) (int, error) {
		if fail {
			return 0, syscall.ENOSPC
		}
		return file.Write(data)
	}
	realm, err := openRealm(options, ops)
	if err != nil {
		t.Fatalf("%s open: %v", physicalRed, err)
	}
	defer realm.Close()
	key := journalKey("%1", "pane-inc-a")
	admitJournalPane(t, realm, key)
	pane := realm.panes[key]
	before := pane.physical
	fail = true
	if _, err := realm.Append(key, []byte("doomed")); !errors.Is(err, ErrStorage) {
		t.Fatalf("%s ENOSPC append err=%v want ErrStorage", physicalRed, err)
	}
	if pane.reason != ReasonENOSPC || pane.state != EligibilityUntrusted {
		t.Fatalf("%s ENOSPC did not fail closed: state=%v reason=%v", physicalRed, pane.state, pane.reason)
	}
	if pane.physical != before+physicalAppendCost(len("doomed")) {
		t.Fatalf("%s ENOSPC refunded a charge the filesystem may still hold: before=%d after=%d", physicalRed, before, pane.physical)
	}
	charged, _, _ := realm.PhysicalBudget()
	if charged != pane.physical {
		t.Fatalf("%s realm total=%d pane=%d drifted", physicalRed, charged, pane.physical)
	}
}

// The derived realm cap comes from the runtime directory's filesystem,
// measured through the root descriptor: min(size x fraction, size - headroom).
// A filesystem that leaves no budget is an unsafe runtime, and an explicit cap
// never consults the filesystem.
func TestPhysicalLedgerDerivesRealmCapFromFilesystem(t *testing.T) {
	const unit = 4096
	statfs := func(blocks uint64, err error) func(int, *syscall.Statfs_t) error {
		return func(_ int, stat *syscall.Statfs_t) error {
			if err != nil {
				return err
			}
			*stat = syscall.Statfs_t{}
			stat.Blocks = blocks
			stat.Frsize = unit
			stat.Bsize = unit
			return nil
		}
	}
	t.Run("fraction binds", func(t *testing.T) {
		options := journalOptions(t)
		options.PhysicalHeadroomBytes = 1 << 20
		ops := realJournalOps()
		ops.fstatfs = statfs(96<<20/unit, nil)
		realm, err := openRealm(options, ops)
		if err != nil {
			t.Fatalf("%s open: %v", physicalRed, err)
		}
		defer realm.Close()
		if _, _, cap := realm.PhysicalBudget(); cap != int64(float64(96<<20)*DefaultPhysicalFraction) {
			t.Fatalf("%s derived cap=%d want 3/4 of 96 MiB", physicalRed, cap)
		}
	})
	t.Run("headroom binds", func(t *testing.T) {
		options := journalOptions(t)
		options.PhysicalFraction = 0.95
		options.PhysicalHeadroomBytes = 16 << 20
		ops := realJournalOps()
		ops.fstatfs = statfs(96<<20/unit, nil)
		realm, err := openRealm(options, ops)
		if err != nil {
			t.Fatalf("%s open: %v", physicalRed, err)
		}
		defer realm.Close()
		if _, _, cap := realm.PhysicalBudget(); cap != 80<<20 {
			t.Fatalf("%s derived cap=%d want 96 MiB - 16 MiB", physicalRed, cap)
		}
	})
	t.Run("no budget is unsafe", func(t *testing.T) {
		options := journalOptions(t)
		ops := realJournalOps()
		ops.fstatfs = statfs(1<<20/unit, nil)
		if _, err := openRealm(options, ops); !errors.Is(err, ErrUnsafeRuntime) {
			t.Fatalf("%s a 1 MiB filesystem under 8 MiB headroom opened: %v", physicalRed, err)
		}
	})
	t.Run("statfs failure is unsafe", func(t *testing.T) {
		options := journalOptions(t)
		ops := realJournalOps()
		ops.fstatfs = statfs(0, errors.New("injected statfs failure"))
		if _, err := openRealm(options, ops); !errors.Is(err, ErrUnsafeRuntime) {
			t.Fatalf("%s unmeasurable filesystem opened: %v", physicalRed, err)
		}
	})
	t.Run("explicit cap ignores the filesystem", func(t *testing.T) {
		options := journalOptions(t)
		options.PhysicalCapBytes = 5 << 20
		ops := realJournalOps()
		ops.fstatfs = statfs(0, errors.New("must not be consulted"))
		realm, err := openRealm(options, ops)
		if err != nil {
			t.Fatalf("%s open: %v", physicalRed, err)
		}
		defer realm.Close()
		if _, _, cap := realm.PhysicalBudget(); cap != 5<<20 {
			t.Fatalf("%s explicit cap=%d", physicalRed, cap)
		}
		if _, cap := realm.PanePhysical(journalKey("%9", "none")); cap != DefaultPaneCapBytes*DefaultPanePhysicalMultiplier {
			t.Fatalf("%s default pane physical cap=%d", physicalRed, cap)
		}
	})
	t.Run("tmpfs sized like production", func(t *testing.T) {
		// The deployed runtime directory is a 96 MiB tmpfs: the derived cap
		// leaves a quarter of it free, and a realm that would fill the logical
		// cap at the measured framing ratio is stopped by the physical one.
		options := journalOptions(t)
		ops := realJournalOps()
		ops.fstatfs = statfs(96<<20/unit, nil)
		realm, err := openRealm(options, ops)
		if err != nil {
			t.Fatalf("%s open: %v", physicalRed, err)
		}
		defer realm.Close()
		_, _, cap := realm.PhysicalBudget()
		if cap != 72<<20 || cap >= 96<<20-DefaultPhysicalHeadroomBytes {
			t.Fatalf("%s production-shaped cap=%d", physicalRed, cap)
		}
	})
}

// A header that no longer fits refuses the generation before a file exists.
func TestPhysicalLedgerRefusesHeaderWithoutRoom(t *testing.T) {
	options := physicalOptions(t, 1<<20, 64)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", physicalRed, err)
	}
	defer realm.Close()
	key := journalKey("%1", "pane-inc-a")
	if err := realm.AdmitPane(key, Geometry{Columns: 80, Rows: 24}); !errors.Is(err, ErrPhysicalQuota) {
		t.Fatalf("%s header admitted without room: %v", physicalRed, err)
	}
	if _, err := os.Stat(physicalJournalPath(options, key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s a refused header left a file: %v", physicalRed, err)
	}
	if _, err := realm.Append(key, []byte("x")); !errors.Is(err, ErrPhysicalQuota) {
		t.Fatalf("%s byte-only generation created without room: %v", physicalRed, err)
	}
}

// A geometry reservation is the pre-issue admission the resize transaction
// relies on: refused without invalidating, held against every other append on
// both ledgers, refunded on release, consumed exactly once.
func TestGeometryReservationHoldsBothLedgersAndNeverInvalidates(t *testing.T) {
	options := physicalOptions(t, 1<<20, 8<<20)
	options.PaneCapBytes = 4 * geometryRecordCost
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", physicalRed, err)
	}
	defer realm.Close()
	key := journalKey("%1", "pane-inc-a")
	other := journalKey("%2", "pane-inc-b")
	admitJournalPane(t, realm, key)
	admitJournalPane(t, realm, other)
	pane := realm.panes[key]

	// Logical refusal leaves the generation continuous.
	fill, err := realm.Append(key, make([]byte, options.PaneCapBytes-geometryRecordCost+1))
	if err != nil {
		t.Fatal(err)
	}
	commitLast(t, realm, key, fill)
	if _, err := realm.ReserveGeometry(key); !errors.Is(err, ErrQuota) || errors.Is(err, ErrPhysicalQuota) {
		t.Fatalf("%s logical refusal=%v", physicalRed, err)
	}
	if pane.state != EligibilityContinuous {
		t.Fatalf("%s a refused reservation invalidated the generation: %v", physicalRed, pane.reason)
	}

	// Physical refusal likewise, on a realm with logical room to spare.
	wide := physicalOptions(t, 1<<20, 8<<20)
	wide.PanePhysicalCapBytes = 0
	wide.PaneCapBytes = 1 << 20
	tight, err := OpenRealm(wide)
	if err != nil {
		t.Fatal(err)
	}
	defer tight.Close()
	admitJournalPane(t, tight, key)
	tight.physicalCap = tight.panes[key].physical + geometryRecordCost - 1
	if _, err := tight.ReserveGeometry(key); !errors.Is(err, ErrPhysicalQuota) {
		t.Fatalf("%s physical refusal=%v", physicalRed, err)
	}
	if tight.panes[key].state != EligibilityContinuous {
		t.Fatalf("%s a physical refusal invalidated the generation", physicalRed)
	}

	// A held reservation is spent capacity for everyone: the realm's logical
	// and physical headroom shrink by it, for this pane and for others.
	roomy := physicalOptions(t, 1<<20, 0)
	roomy.PaneCapBytes = 1 << 20
	shared, err := OpenRealm(roomy)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	admitJournalPane(t, shared, key)
	admitJournalPane(t, shared, other)
	headerA, headerB := shared.panes[key].physical, shared.panes[other].physical
	shared.physicalCap = headerA + headerB + geometryRecordCost + physicalAppendCost(4)
	reservation, err := shared.ReserveGeometry(key)
	if err != nil || !reservation.Held() {
		t.Fatalf("%s reservation: %v", physicalRed, err)
	}
	if _, reserved, _ := shared.PhysicalBudget(); reserved != geometryRecordCost {
		t.Fatalf("%s reserved=%d", physicalRed, reserved)
	}
	if _, err := shared.Append(other, make([]byte, 5)); !errors.Is(err, ErrPhysicalQuota) {
		t.Fatalf("%s another pane consumed reserved physical capacity: %v", physicalRed, err)
	}
	shared.panes[other].state, shared.panes[other].reason = EligibilityContinuous, ReasonNone
	record, err := shared.Append(other, make([]byte, 4))
	if err != nil {
		t.Fatalf("%s append within the unreserved remainder: %v", physicalRed, err)
	}
	commitLast(t, shared, other, record)
	// Consumed exactly once: the record lands, the reserve is gone, the charge
	// is what the reserve held, and a second consumption is refused.
	geometry, err := shared.AppendReservedGeometry(reservation, Geometry{Columns: 80, Rows: 48})
	if err != nil {
		t.Fatalf("%s reserved geometry: %v", physicalRed, err)
	}
	commitLast(t, shared, key, geometry)
	charged, reserved, cap := shared.PhysicalBudget()
	if reserved != 0 || charged != cap || reservation.Held() {
		t.Fatalf("%s after consumption charged=%d reserved=%d cap=%d held=%t", physicalRed, charged, reserved, cap, reservation.Held())
	}
	if _, err := shared.AppendReservedGeometry(reservation, Geometry{Columns: 80, Rows: 48}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("%s a consumed reservation was honoured twice: %v", physicalRed, err)
	}
	reservation.Release()
	if _, reserved, _ := shared.PhysicalBudget(); reserved != 0 {
		t.Fatalf("%s release after consumption refunded capacity: reserved=%d", physicalRed, reserved)
	}
	if shared.panes[key].reservedCharge != 0 || shared.reservedCharge != 0 {
		t.Fatalf("%s logical reserve leaked: pane=%d realm=%d", physicalRed, shared.panes[key].reservedCharge, shared.reservedCharge)
	}

	// Released unconsumed: everything refunded.
	again, err := shared.ReserveGeometry(key)
	if !errors.Is(err, ErrPhysicalQuota) {
		t.Fatalf("%s a full realm granted a reservation: %v", physicalRed, err)
	}
	shared.physicalCap += geometryRecordCost
	again, err = shared.ReserveGeometry(key)
	if err != nil {
		t.Fatal(err)
	}
	again.Release()
	again.Release()
	if _, reserved, _ := shared.PhysicalBudget(); reserved != 0 || shared.reservedCharge != 0 {
		t.Fatalf("%s release did not refund: physical=%d logical=%d", physicalRed, reserved, shared.reservedCharge)
	}
}
