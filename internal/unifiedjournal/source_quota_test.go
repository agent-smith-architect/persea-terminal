package unifiedjournal

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSourceAllowanceNormalRotationAndProvisionalTransfer(t *testing.T) {
	realm, err := OpenRealm(rotationOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	if err := realm.ConfigureSourceQuota(2, 8, nil); err != nil {
		t.Fatal(err)
	}
	source := SourceID{Realm: "rotation"}
	current := rotationKey("$source", 1)
	if err := realm.ReserveSource(current, source); err != nil {
		t.Fatal(err)
	}
	admitRotationPredecessor(t, realm, current)
	for generation := uint64(2); generation <= 5; generation++ {
		next := rotationKey(current.Session, generation)
		provisional := next
		provisional.Incarnation += "-before-refit"
		if err := realm.ReserveSource(provisional, source); err != nil {
			t.Fatal(err)
		}
		if err := realm.TransferSourceReservation(provisional, next); err != nil {
			t.Fatal(err)
		}
		if used, _, _, leases := realm.SourceUsage(source); used != 2 || leases != 2 {
			t.Fatalf("overlap used=%d leases=%d", used, leases)
		}
		capacity, err := realm.BeginRotationCapacity(current)
		if err != nil {
			t.Fatal(err)
		}
		reservation, err := realm.BeginRotatedPane(next, Geometry{80, 24}, capacity)
		if err != nil {
			t.Fatal(err)
		}
		verifiedOutput(t, realm, next, []byte("initial"))
		reservation.Commit()
		capacity.Release()
		if used, _, _, leases := realm.SourceUsage(source); used != 1 || leases != 1 {
			t.Fatalf("successful rotation retained allowance: used=%d leases=%d", used, leases)
		}
		current = next
	}
}

func TestSourceAllowanceFailedHeaderCannotBeCancelledOrForgottenAtRecovery(t *testing.T) {
	options := rotationOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	if err := realm.ConfigureSourceQuota(2, 8, nil); err != nil {
		t.Fatal(err)
	}
	source := SourceID{Realm: "failed-header"}
	first, second, third := rotationKey("$header", 1), rotationKey("$header", 2), rotationKey("$header", 3)
	if err := realm.ReserveSource(first, source); err != nil {
		t.Fatal(err)
	}
	realm.ops.write = func(*os.File, []byte) (int, error) { return 0, syscall.EIO }
	realm.ops.unlinkat = func(int, string) error { return syscall.EIO }
	if err := realm.AdmitPane(first, Geometry{80, 24}); !errors.Is(err, ErrStorage) {
		t.Fatalf("failed header admission=%v", err)
	}
	realm.CancelSourceReservation(first)
	if err := realm.ReserveSource(second, source); err != nil {
		t.Fatal(err)
	}
	if err := realm.ReserveSource(third, source); !errors.Is(err, ErrSourceQuota) {
		t.Fatal("failed header lost its held allowance")
	}
	if err := realm.Close(); err != nil {
		t.Fatal(err)
	}
	// An incomplete header cannot prove a PaneKey. The existing scan refuses
	// this runtime instead of treating the unidentifiable file as free space.
	reopened, err := OpenRealm(options)
	if err == nil {
		reopened.Close()
		t.Fatal("unidentifiable failed header minted capacity at recovery")
	}
}

func TestSourceAllowanceCancellationCannotRefundHeldCreation(t *testing.T) {
	realm, err := OpenRealm(rotationOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	if err := realm.ConfigureSourceQuota(2, 8, nil); err != nil {
		t.Fatal(err)
	}
	source := SourceID{Realm: "creation"}
	first, second, third := rotationKey("$claim", 1), rotationKey("$claim", 2), rotationKey("$claim", 3)
	for _, key := range []PaneKey{first, second} {
		if err := realm.ReserveSource(key, source); err != nil {
			t.Fatal(err)
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	open := realm.ops.openat
	realm.ops.openat = func(fd int, name string, flags int, mode uint32) (int, error) {
		if name == paneFileName(first) {
			close(entered)
			<-release
		}
		return open(fd, name, flags, mode)
	}
	done := make(chan error, 1)
	go func() { done <- realm.AdmitPane(first, Geometry{80, 24}) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("creation did not enter openat")
	}
	realm.CancelSourceReservation(first)
	if err := realm.ReserveSource(third, source); !errors.Is(err, ErrSourceQuota) {
		t.Fatalf("unsettled openat lost its allowance: %v", err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !realm.RetirePane(first) {
		t.Fatal("creation owner did not settle")
	}
	realm.CancelSourceReservation(second)
	if _, _, _, leases := realm.SourceUsage(source); leases != 0 {
		t.Fatal("creation/cancellation retained a lease")
	}
}

func TestSourceAllowanceRetainsFailedUnlinkAndRetriesBeforeAdmission(t *testing.T) {
	realm, err := OpenRealm(rotationOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	if err := realm.ConfigureSourceQuota(2, 8, nil); err != nil {
		t.Fatal(err)
	}
	source := SourceID{Realm: "unlink"}
	realUnlink := realm.ops.unlinkat
	realm.ops.unlinkat = func(int, string) error { return syscall.EIO }
	for generation := uint64(1); generation <= 2; generation++ {
		key := rotationKey("$failed-source", generation)
		if err := realm.ReserveSource(key, source); err != nil {
			t.Fatal(err)
		}
		if err := realm.AdmitPane(key, Geometry{80, 24}); err != nil {
			t.Fatal(err)
		}
		if realm.RetirePane(key) {
			t.Fatal("failed unlink reported settlement")
		}
		realm.CancelSourceReservation(key)
	}
	next := rotationKey("$failed-source", 3)
	if err := realm.ReserveSource(next, source); !errors.Is(err, ErrSourceQuota) {
		t.Fatalf("held old generations escaped source bound: %v", err)
	}
	realm.ReclaimRetiredSession(next)
	if used, _, _, _ := realm.SourceUsage(source); used != 2 {
		t.Fatal("failed retry released source allowance")
	}
	realm.ops.unlinkat = realUnlink
	realm.ReclaimRetiredSession(next)
	if err := realm.ReserveSource(next, source); err != nil {
		t.Fatal(err)
	}
	realm.CancelSourceReservation(next)
	realm.CancelSourceReservation(next)
	if used, unknown, _, leases := realm.SourceUsage(source); used != 0 || unknown != 0 || leases != 0 {
		t.Fatalf("proven cleanup/cancel retained ownership: %d/%d/%d", used, unknown, leases)
	}
}

func TestSourceAllowanceRecoveredUnknownCannotMintFreshAllowance(t *testing.T) {
	options := rotationOptions(t)
	options.CompletePaneSlots = 4
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatal(err)
	}
	first, second := rotationKey("$recovered-one", 1), rotationKey("$recovered-two", 1)
	for _, key := range []PaneKey{first, second} {
		if err := realm.AdmitPane(key, Geometry{80, 24}); err != nil {
			t.Fatal(err)
		}
		verifiedOutput(t, realm, key, []byte("surviving liability"))
	}
	if err := realm.Close(); err != nil {
		t.Fatal(err)
	}
	realm, err = OpenRealm(options)
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	known, unrelated := SourceID{Realm: "known"}, SourceID{Realm: "unrelated"}
	if err := realm.ConfigureSourceQuota(2, 8, map[PaneKey]SourceID{first: known}); err != nil {
		t.Fatal(err)
	}
	next := rotationKey("$recovered-one", 2)
	if err := realm.ReserveSource(next, known); !errors.Is(err, ErrSourceQuota) {
		t.Fatalf("unattributed file granted fresh allowance: %v", err)
	}
	if err := realm.AttributeRecoveredSource(second, unrelated); err != nil {
		t.Fatal(err)
	}
	if used, unknown, _, leases := realm.SourceUsage(known); used != 1 || unknown != 0 || leases != 2 {
		t.Fatal("attribution changed total file ownership")
	}
	if err := realm.ReserveSource(next, known); err != nil {
		t.Fatal(err)
	}
	if err := realm.AttributeRecoveredSource(second, known); !errors.Is(err, ErrInvalidated) {
		t.Fatal("recovered exact source could be rebound")
	}
	realm.CancelSourceReservation(next)
	if !realm.RetirePane(first) || !realm.RetirePane(second) {
		t.Fatal("recovery liabilities did not settle")
	}
	if used, unknown, _, leases := realm.SourceUsage(known); used != 0 || unknown != 0 || leases != 0 {
		t.Fatal("recovered source identity retained after unlink")
	}
}
