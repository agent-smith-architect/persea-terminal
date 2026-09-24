package unifiedjournal

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

// staleShape is one way a first-incarnation generation can be left on disk
// for the next incarnation's scan to recover. Every shape is a generation the
// dead broker owned and the live one can never resume, so every shape must
// be slot-retired at scan and superseded by the next same-session adoption.
type staleShape struct {
	name string
	// write leaves the generation on the first realm; the caller closes it.
	write func(t *testing.T, realm *Realm, key PaneKey)
	// mutate damages the closed file the way a crash or a bad disk would.
	mutate func(t *testing.T, path string)
	// origin is the header origin the reopened generation reports, when its
	// header is still readable.
	origin GenerationOrigin
	// corrupt is whether the reopen classifies it as a corrupt journal.
	corrupt bool
}

func appendCommitted(t *testing.T, realm *Realm, key PaneKey, payload string) {
	t.Helper()
	record, err := realm.Append(key, []byte(payload))
	if err != nil {
		t.Fatalf("append %q: %v", payload, err)
	}
	commitLast(t, realm, key, record)
}

func appendRaw(t *testing.T, path string, tail []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(tail); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// staleShapes are the recovered-generation shapes N15 names: born,
// reconstructed, rotated, torn, corrupt, and the byte-only birth that Append's
// implicit create produces.
func staleShapes() []staleShape {
	geometry := Geometry{Columns: 80, Rows: 24}
	return []staleShape{
		{
			name: "born",
			write: func(t *testing.T, realm *Realm, key PaneKey) {
				if err := realm.AdmitPane(key, geometry); err != nil {
					t.Fatalf("admit born: %v", err)
				}
				appendCommitted(t, realm, key, "born-output")
			},
			origin: OriginBirth,
		},
		{
			name: "reconstructed",
			write: func(t *testing.T, realm *Realm, key PaneKey) {
				if err := realm.AdmitReconstructedPane(key, geometry); err != nil {
					t.Fatalf("admit reconstructed: %v", err)
				}
				appendCommitted(t, realm, key, "reconstructed-bootstrap")
			},
			origin: OriginReconstructed,
		},
		{
			name: "rotated",
			write: func(t *testing.T, realm *Realm, key PaneKey) {
				predecessor := key
				predecessor.ControlGeneration ^= 1 << 63
				if err := realm.AdmitPane(predecessor, geometry); err != nil {
					t.Fatalf("admit rotation predecessor: %v", err)
				}
				capacity, err := realm.BeginRotationCapacity(predecessor)
				if err != nil {
					t.Fatalf("reserve rotation capacity: %v", err)
				}
				reservation, err := realm.BeginRotatedPane(key, geometry, capacity)
				if err != nil {
					capacity.Release()
					t.Fatalf("begin rotated pane: %v", err)
				}
				appendCommitted(t, realm, key, "rotated-output")
				reservation.Commit()
			},
			origin: OriginRotated,
		},
		{
			name: "torn",
			write: func(t *testing.T, realm *Realm, key PaneKey) {
				if err := realm.AdmitPane(key, geometry); err != nil {
					t.Fatalf("admit born: %v", err)
				}
				appendCommitted(t, realm, key, "committed-before-tear")
				if _, err := realm.Append(key, []byte("uncommitted-tail")); err != nil {
					t.Fatalf("uncommitted append: %v", err)
				}
			},
			// A partial append frame after the last record: outside committed
			// authority, not corruption, and it still occupies the file.
			mutate: func(t *testing.T, path string) {
				appendRaw(t, path, []byte{appendMarker, byte(RecordOutput), 0, 0, 0, 0, 0})
			},
			origin: OriginBirth,
		},
		{
			name: "corrupt",
			write: func(t *testing.T, realm *Realm, key PaneKey) {
				if err := realm.AdmitPane(key, geometry); err != nil {
					t.Fatalf("admit born: %v", err)
				}
				appendCommitted(t, realm, key, "committed-before-garbage")
			},
			mutate: func(t *testing.T, path string) {
				appendRaw(t, path, []byte("\xff\xfe\xfdgarbage that no parser accepts"))
			},
			origin:  OriginBirth,
			corrupt: true,
		},
		{
			// Append's implicit create: a birth generation with no birth
			// geometry, which reopens untrusted. It is still a birth that held
			// a slot.
			name: "byte-only-birth",
			write: func(t *testing.T, realm *Realm, key PaneKey) {
				appendCommitted(t, realm, key, "byte-only-output")
			},
			origin:  OriginBirth,
			corrupt: true,
		},
	}
}

// writeStaleGeneration leaves one generation of the given shape on disk under
// the first broker incarnation. The realm is closed cleanly; a crash is N2's
// case.
func writeStaleGeneration(t *testing.T, options OpenOptions, shape staleShape, key PaneKey) {
	t.Helper()
	first, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	shape.write(t, first, key)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if shape.mutate != nil {
		shape.mutate(t, storagePathsForTest(options, key).Journal)
	}
}

// requireStaleRetiredAtScan asserts the scan half of N15 for one recovered
// generation: it is slot-retired, it is attributed with its origin (or as
// corrupt), its physical charge is exactly its file size, and the realm
// total is exactly the sum of the recovered realm charges.
func requireStaleRetiredAtScan(t *testing.T, options OpenOptions, realm *Realm, shape staleShape, key PaneKey) (realmCharge, physical int64) {
	t.Helper()
	stale := realm.panes[key]
	if stale == nil {
		t.Fatalf("%s: recovered generation missing from the pane map", shape.name)
	}
	if !stale.slotRetired {
		t.Fatalf("%s: recovered generation still holds a complete-pane slot", shape.name)
	}
	if stale.state == EligibilityContinuous || realm.UnifiedEligible(key) {
		t.Fatalf("%s: recovered generation is resumable: state=%v", shape.name, stale.state)
	}
	if stale.corrupt != shape.corrupt {
		t.Fatalf("%s: corrupt=%v want %v", shape.name, stale.corrupt, shape.corrupt)
	}
	if !shape.corrupt {
		if origin, err := realm.Origin(key); err != nil || origin != shape.origin {
			t.Fatalf("%s: origin=%q err=%v want %q", shape.name, origin, err, shape.origin)
		}
	}
	info, err := os.Lstat(storagePathsForTest(options, key).Journal)
	if err != nil {
		t.Fatalf("%s: stale file: %v", shape.name, err)
	}
	if stale.physical != info.Size() {
		t.Fatalf("%s: physical=%d want file size %d", shape.name, stale.physical, info.Size())
	}
	var sum int64
	for _, pane := range realm.panes {
		sum += pane.realmCharge
	}
	if realm.total != sum {
		t.Fatalf("%s: realm total=%d but pane charges sum to %d", shape.name, realm.total, sum)
	}
	return stale.realmCharge, stale.physical
}

// TestWidenedCleanupRetiresAndSupersedesEveryOrigin is N15's core: for every
// recovered shape, one ordinary slot is still free after scan (the leak
// symptom was availability 0 and an ErrQuota adoption refusal), and the next
// same-session adoption commit supersedes the stale generation — file
// unlinked, map entry gone, realm and physical charges refunded exactly —
// while a stale generation of another session is left alone.
func TestWidenedCleanupRetiresAndSupersedesEveryOrigin(t *testing.T) {
	for _, shape := range staleShapes() {
		t.Run(shape.name, func(t *testing.T) {
			options := journalOptions(t)
			options.CompletePaneSlots = 3
			staleKey := reservationKey("$1", 1)
			foreignKey := reservationKey("$2", 1)
			first, err := openRealm(options, realJournalOps())
			if err != nil {
				t.Fatal(err)
			}
			shape.write(t, first, staleKey)
			// The foreign control is always a born generation: the leak the
			// packet names is a birth holding a slot, and it must survive a
			// different session's supersession untouched.
			if err := first.AdmitPane(foreignKey, Geometry{Columns: 80, Rows: 24}); err != nil {
				t.Fatalf("admit foreign control: %v", err)
			}
			appendCommitted(t, first, foreignKey, "foreign-output")
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			if shape.mutate != nil {
				shape.mutate(t, storagePathsForTest(options, staleKey).Journal)
			}

			// One ordinary slot plus the rotation reserve on reopen: with either
			// recovered generation still holding a slot ordinary admission would
			// already be over its limit.
			options.CompletePaneSlots = 2
			options.BrokerIncarnation = "broker-incarnation-b"
			reopened, err := openRealm(options, realJournalOps())
			if err != nil {
				t.Fatalf("reopen over %s: %v", shape.name, err)
			}
			defer reopened.Close()
			staleCharge, stalePhysical := requireStaleRetiredAtScan(t, options, reopened, shape, staleKey)
			foreignCharge, foreignPhysical := requireStaleRetiredAtScan(t, options, reopened, staleShapes()[0], foreignKey)
			if available := reopened.AvailableCompletePaneSlots(); available != 1 {
				t.Fatalf("%s: post-scan availability=%d want 1", shape.name, available)
			}
			totalBefore := reopened.total
			physicalBefore, _, _ := reopened.PhysicalBudget()

			freshKey := reservationKey("$1", 2)
			geometry := Geometry{Columns: 80, Rows: 24}
			reservation, err := reopened.BeginReconstructedPane(freshKey, geometry)
			if err != nil {
				t.Fatalf("%s: adoption on the single ordinary slot refused: %v", shape.name, err)
			}
			if _, err := reopened.Append(freshKey, []byte("fresh-bootstrap")); err != nil {
				t.Fatalf("%s: bootstrap: %v", shape.name, err)
			}
			reservation.Commit()

			requireRetentionPathAbsent(t, storagePathsForTest(options, staleKey).Journal)
			if _, present := reopened.panes[staleKey]; present {
				t.Fatalf("%s: superseded generation still in the pane map", shape.name)
			}
			if _, err := reopened.Origin(staleKey); !errors.Is(err, ErrInvalidated) {
				t.Fatalf("%s: superseded origin err=%v want invalidated", shape.name, err)
			}
			bootstrap := int64(len("fresh-bootstrap"))
			if reopened.total != totalBefore-staleCharge+bootstrap {
				t.Fatalf("%s: total=%d want %d (before=%d stale=%d bootstrap=%d)", shape.name, reopened.total, totalBefore-staleCharge+bootstrap, totalBefore, staleCharge, bootstrap)
			}
			freshPhysical, _ := reopened.PanePhysical(freshKey)
			if physical, _, _ := reopened.PhysicalBudget(); physical != physicalBefore-stalePhysical+freshPhysical {
				t.Fatalf("%s: physical=%d want %d (before=%d stale=%d fresh=%d)", shape.name, physical, physicalBefore-stalePhysical+freshPhysical, physicalBefore, stalePhysical, freshPhysical)
			}
			// The foreign session's stale birth is not this commit's to sweep.
			requireRetentionPathPresent(t, storagePathsForTest(options, foreignKey).Journal)
			if foreign := reopened.panes[foreignKey]; foreign == nil || !foreign.slotRetired || foreign.realmCharge != foreignCharge || foreign.physical != foreignPhysical {
				t.Fatalf("%s: foreign stale birth touched by supersession: %+v", shape.name, foreign)
			}
			if !reopened.UnifiedEligible(freshKey) {
				t.Fatalf("%s: committed adoption is not unified-eligible", shape.name)
			}
			if available := reopened.AvailableCompletePaneSlots(); available != 0 {
				t.Fatalf("%s: post-commit availability=%d want 0", shape.name, available)
			}
		})
	}
}

// TestWidenedCleanupRetainsChargesUntilUnlinkOrENOENT is N15's proof-before-
// refund half for a stale BIRTH generation, the origin the old sweep never
// reached. On unlink EIO the stale generation keeps its file, its map entry,
// its realm charge and its physical charge, and stays slot-retired; a later
// same-session supersession retries and refunds once unlink succeeds. ENOENT
// is proof too: a file that vanished underneath the ledger is refunded on
// the first sweep.
func TestWidenedCleanupRetainsChargesUntilUnlinkOrENOENT(t *testing.T) {
	t.Run("eio-then-retry", func(t *testing.T) {
		options := journalOptions(t)
		options.CompletePaneSlots = 3
		staleKey := reservationKey("$1", 1)
		writeStaleGeneration(t, options, staleShapes()[0], staleKey)

		options.BrokerIncarnation = "broker-incarnation-b"
		ops, failing := failingUnlinkOps()
		reopened, err := openRealm(options, ops)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		staleCharge, stalePhysical := requireStaleRetiredAtScan(t, options, reopened, staleShapes()[0], staleKey)
		geometry := Geometry{Columns: 80, Rows: 24}

		firstKey := reservationKey("$1", 2)
		first, err := reopened.BeginReconstructedPane(firstKey, geometry)
		if err != nil {
			t.Fatalf("first adoption: %v", err)
		}
		if _, err := reopened.Append(firstKey, []byte("first-bootstrap")); err != nil {
			t.Fatal(err)
		}
		first.Commit()
		firstPhysical, _ := reopened.PanePhysical(firstKey)
		// Unlink failed: the bytes are still on disk, so nothing is refunded
		// and nothing is forgotten. The slot stays retired regardless.
		requireRetentionPathPresent(t, storagePathsForTest(options, staleKey).Journal)
		stale := reopened.panes[staleKey]
		if stale == nil || !stale.slotRetired || stale.realmCharge != staleCharge || stale.physical != stalePhysical {
			t.Fatalf("EIO supersession changed the retained stale birth: %+v", stale)
		}
		if reopened.total != staleCharge+int64(len("first-bootstrap")) {
			t.Fatalf("EIO supersession minted realm capacity: total=%d want %d", reopened.total, staleCharge+int64(len("first-bootstrap")))
		}
		if physical, _, _ := reopened.PhysicalBudget(); physical != stalePhysical+firstPhysical {
			t.Fatalf("EIO supersession minted physical capacity: %d want %d", physical, stalePhysical+firstPhysical)
		}
		if available := reopened.AvailableCompletePaneSlots(); available != 1 {
			t.Fatalf("availability after EIO=%d want 1 (only the committed adoption holds a slot)", available)
		}

		// The next same-session supersession retries. The first adoption is
		// live and holds its slot, so it is not sweepable; only the retained
		// stale birth is refunded.
		*failing = false
		secondKey := reservationKey("$1", 3)
		second, err := reopened.BeginReconstructedPane(secondKey, geometry)
		if err != nil {
			t.Fatalf("second adoption: %v", err)
		}
		if _, err := reopened.Append(secondKey, []byte("second-bootstrap")); err != nil {
			t.Fatal(err)
		}
		second.Commit()
		requireRetentionPathAbsent(t, storagePathsForTest(options, staleKey).Journal)
		if _, present := reopened.panes[staleKey]; present {
			t.Fatal("retried supersession left the stale birth in the map")
		}
		requireRetentionPathPresent(t, storagePathsForTest(options, firstKey).Journal)
		if !reopened.UnifiedEligible(firstKey) || !reopened.UnifiedEligible(secondKey) {
			t.Fatal("a live committed adoption was swept as stale")
		}
		if reopened.total != int64(len("first-bootstrap")+len("second-bootstrap")) {
			t.Fatalf("retried supersession total=%d want %d", reopened.total, len("first-bootstrap")+len("second-bootstrap"))
		}
		secondPhysical, _ := reopened.PanePhysical(secondKey)
		if physical, _, _ := reopened.PhysicalBudget(); physical != firstPhysical+secondPhysical {
			t.Fatalf("retried supersession physical=%d want %d", physical, firstPhysical+secondPhysical)
		}
	})

	t.Run("enoent-is-proof", func(t *testing.T) {
		options := journalOptions(t)
		options.CompletePaneSlots = 2
		staleKey := reservationKey("$1", 1)
		writeStaleGeneration(t, options, staleShapes()[0], staleKey)

		options.BrokerIncarnation = "broker-incarnation-b"
		reopened, err := openRealm(options, realJournalOps())
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		requireStaleRetiredAtScan(t, options, reopened, staleShapes()[0], staleKey)
		// The file vanishes underneath the ledger; the charge must stay until
		// a sweep proves it gone, and ENOENT at that sweep is the proof.
		if err := os.Remove(storagePathsForTest(options, staleKey).Journal); err != nil {
			t.Fatal(err)
		}
		if reopened.total == 0 {
			t.Fatal("removing the file must not itself refund anything")
		}
		freshKey := reservationKey("$1", 2)
		reservation, err := reopened.BeginReconstructedPane(freshKey, Geometry{Columns: 80, Rows: 24})
		if err != nil {
			t.Fatal(err)
		}
		reservation.Commit()
		if _, present := reopened.panes[staleKey]; present {
			t.Fatal("ENOENT did not count as proof: stale birth still in the map")
		}
		freshPhysical, _ := reopened.PanePhysical(freshKey)
		if physical, _, _ := reopened.PhysicalBudget(); reopened.total != 0 || physical != freshPhysical {
			t.Fatalf("ENOENT supersession left total=%d physical=%d want 0/%d", reopened.total, physical, freshPhysical)
		}
	})
}

// TestWidenedCleanupSurvivesRepeatedRestart pins convergence: a mixed set of
// stale generations — born, reconstructed, torn, corrupt — reopened under
// three successive incarnations is slot-retired every time, holds exactly
// the same charges every time (no drift in either ledger), and is superseded
// in one sweep by the first adoption that commits for the session.
func TestWidenedCleanupSurvivesRepeatedRestart(t *testing.T) {
	options := journalOptions(t)
	shapes := staleShapes()
	options.CompletePaneSlots = int64(len(shapes)) + 1 // room to write every shape while preserving rotation's slot
	keys := make(map[string]PaneKey, len(shapes))
	first, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	for index, shape := range shapes {
		key := reservationKey("$1", uint64(index+1))
		keys[shape.name] = key
		shape.write(t, first, key)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for _, shape := range shapes {
		if shape.mutate != nil {
			shape.mutate(t, storagePathsForTest(options, keys[shape.name]).Journal)
		}
	}

	// Fewer configured slots than stale generations on every reopen: any
	// recovered generation still holding a slot would overflow the ledger.
	options.CompletePaneSlots = 2
	var expectedTotal, expectedPhysical int64
	for restart := 1; restart <= 3; restart++ {
		options.BrokerIncarnation = fmt.Sprintf("broker-incarnation-%d", restart)
		reopened, err := openRealm(options, realJournalOps())
		if err != nil {
			t.Fatalf("restart %d: %v", restart, err)
		}
		var total, physical int64
		for _, shape := range shapes {
			charge, size := requireStaleRetiredAtScan(t, options, reopened, shape, keys[shape.name])
			total += charge
			physical += size
		}
		charged, _, _ := reopened.PhysicalBudget()
		if reopened.total != total || charged != physical {
			t.Fatalf("restart %d: ledgers total=%d/%d physical=%d/%d", restart, reopened.total, total, charged, physical)
		}
		if restart == 1 {
			expectedTotal, expectedPhysical = total, physical
		} else if total != expectedTotal || physical != expectedPhysical {
			t.Fatalf("restart %d drifted: total=%d want %d physical=%d want %d", restart, total, expectedTotal, physical, expectedPhysical)
		}
		if available := reopened.AvailableCompletePaneSlots(); available != 1 {
			t.Fatalf("restart %d: availability=%d want 1 plus the rotation reserve with %d stale generations", restart, available, len(shapes))
		}
		if recovered := reopened.Recovered(); len(recovered) != len(shapes) {
			t.Fatalf("restart %d: recovered %d generations want %d", restart, len(recovered), len(shapes))
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}

	options.BrokerIncarnation = "broker-incarnation-final"
	final, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	freshKey := reservationKey("$1", 100)
	reservation, err := final.BeginReconstructedPane(freshKey, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := final.Append(freshKey, []byte("fresh-bootstrap")); err != nil {
		t.Fatal(err)
	}
	reservation.Commit()
	for _, shape := range shapes {
		requireRetentionPathAbsent(t, storagePathsForTest(options, keys[shape.name]).Journal)
		if _, present := final.panes[keys[shape.name]]; present {
			t.Fatalf("%s survived the sweep", shape.name)
		}
	}
	if len(final.panes) != 1 {
		t.Fatalf("pane map holds %d entries after the sweep, want only the fresh generation", len(final.panes))
	}
	freshPhysical, _ := final.PanePhysical(freshKey)
	if charged, _, _ := final.PhysicalBudget(); final.total != int64(len("fresh-bootstrap")) || charged != freshPhysical {
		t.Fatalf("post-sweep ledgers total=%d physical=%d want %d/%d", final.total, charged, len("fresh-bootstrap"), freshPhysical)
	}
}

// TestWidenedCleanupExcludesTheCurrentKeyByIdentity pins the exclusion
// clause. No natural path can slot-retire the committing generation, so the
// flag is forced here to leave the identity exclusion as the only thing
// standing between the reservation and its own sweep: the commit must keep
// its file, its eligibility and its charges.
func TestWidenedCleanupExcludesTheCurrentKeyByIdentity(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 2
	staleKey := reservationKey("$1", 1)
	writeStaleGeneration(t, options, staleShapes()[0], staleKey)

	options.BrokerIncarnation = "broker-incarnation-b"
	reopened, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	freshKey := reservationKey("$1", 2)
	reservation, err := reopened.BeginReconstructedPane(freshKey, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Append(freshKey, []byte("fresh-bootstrap")); err != nil {
		t.Fatal(err)
	}
	fresh := reopened.panes[freshKey]
	fresh.slotRetired = true
	freshPhysical := fresh.physical
	reservation.Commit()
	fresh.slotRetired = false

	requireRetentionPathPresent(t, storagePathsForTest(options, freshKey).Journal)
	if reopened.panes[freshKey] != fresh || !reopened.UnifiedEligible(freshKey) {
		t.Fatal("the committing generation was swept by its own supersession")
	}
	if fresh.realmCharge != int64(len("fresh-bootstrap")) || fresh.physical != freshPhysical {
		t.Fatalf("the committing generation's charges were refunded: realm=%d physical=%d", fresh.realmCharge, fresh.physical)
	}
	requireRetentionPathAbsent(t, storagePathsForTest(options, staleKey).Journal)
	if _, err := reopened.Append(freshKey, []byte("after-commit")); err != nil {
		t.Fatalf("append to the committed generation: %v", err)
	}
}

// TestStrandedBirthGenerationIsSupersededAfterCrash is N2's journal half for
// this phase: a born session whose broker dies without any cleanup — an
// uncommitted tail still in flight — leaves a birth generation the next
// incarnation cannot resume. That stranded generation must not hold a slot,
// and the next adoption of the session supersedes it and refunds exactly the
// charges the scan attributed to it. (N2's first half, rotating a born
// session, needs the rotation flow rotation transaction and is not exercised here.)
func TestStrandedBirthGenerationIsSupersededAfterCrash(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 2
	staleKey := reservationKey("$1", 1)
	crashed, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	// Descriptor hygiene only: Close after the fact runs no ledger cleanup,
	// so the reopen below still sees exactly what the crash left behind.
	defer crashed.Close()
	if err := crashed.AdmitPane(staleKey, Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	appendCommitted(t, crashed, staleKey, "committed-before-crash")
	if _, err := crashed.Append(staleKey, []byte("in-flight-at-crash")); err != nil {
		t.Fatal(err)
	}
	if available := crashed.AvailableCompletePaneSlots(); available != 0 {
		t.Fatalf("live born generation holds no slot: availability=%d", available)
	}

	options.BrokerIncarnation = "broker-incarnation-b"
	reopened, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	staleCharge, stalePhysical := requireStaleRetiredAtScan(t, options, reopened, staleShapes()[0], staleKey)
	// The scan charges what live accounting charged: every parsed append's
	// payload, committed or not.
	if staleCharge != int64(len("committed-before-crash")+len("in-flight-at-crash")) {
		t.Fatalf("stranded birth charge=%d want %d", staleCharge, len("committed-before-crash")+len("in-flight-at-crash"))
	}
	if committed, err := reopened.ReadCommitted(staleKey); err != nil || string(committed) != "committed-before-crash" {
		t.Fatalf("stranded birth committed prefix=%q err=%v", committed, err)
	}
	if available := reopened.AvailableCompletePaneSlots(); available != 1 {
		t.Fatalf("stranded birth generation holds the only ordinary slot: availability=%d", available)
	}
	if err := reopened.AdmitPane(staleKey, Geometry{Columns: 80, Rows: 24}); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("stranded birth re-admitted: %v", err)
	}

	freshKey := reservationKey("$1", 2)
	reservation, err := reopened.BeginReconstructedPane(freshKey, Geometry{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("adoption after crash refused: %v", err)
	}
	if _, err := reopened.Append(freshKey, []byte("fresh-bootstrap")); err != nil {
		t.Fatal(err)
	}
	reservation.Commit()
	requireRetentionPathAbsent(t, storagePathsForTest(options, staleKey).Journal)
	if _, present := reopened.panes[staleKey]; present {
		t.Fatal("stranded birth generation survived the adoption commit")
	}
	freshPhysical, _ := reopened.PanePhysical(freshKey)
	charged, _, _ := reopened.PhysicalBudget()
	if reopened.total != int64(len("fresh-bootstrap")) || charged != freshPhysical {
		t.Fatalf("post-supersession ledgers total=%d physical=%d want %d/%d (stale was %d/%d)", reopened.total, charged, len("fresh-bootstrap"), freshPhysical, staleCharge, stalePhysical)
	}
	if available := reopened.AvailableCompletePaneSlots(); available != 0 {
		t.Fatalf("post-commit availability=%d want 0", available)
	}
}
