package unifiedjournal

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeCraftedJournal writes a raw PUJ2 file for a key, exactly as a past
// broker would have: magic, header length, header JSON, no records. It builds
// the realm directory with the realm's own modes so OpenRealm accepts it.
func writeCraftedJournal(t *testing.T, options OpenOptions, key PaneKey, header []byte) {
	t.Helper()
	paths := storagePathsForTest(options, key)
	if err := os.MkdirAll(filepath.Dir(paths.Journal), options.DirectoryMode.Perm()); err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 8+len(header))
	copy(frame[:4], journalMagic[:])
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(header)))
	copy(frame[8:], header)
	if err := os.WriteFile(paths.Journal, frame, options.FileMode.Perm()); err != nil {
		t.Fatal(err)
	}
}

// TestAdoptionOriginAbsentReadsAsBirth pins backward compatibility: a
// generation written before adoption existed carries no Origin field, and it
// opens as a birth, because every such generation was one.
func TestAdoptionOriginAbsentReadsAsBirth(t *testing.T) {
	options := journalOptions(t)
	key := journalKey("%1", "incarnation-a")
	header, err := json.Marshal(journalHeader{
		Version: journalVersion, Key: key, BrokerIncarnation: "previous-broker",
		GeometryInitial: &Geometry{Columns: 80, Rows: 24},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(header) == "" || errors.Is(err, ErrCorruptJournal) {
		t.Fatal("unreachable")
	}
	// The absent-field shape is the contract under test.
	if json.Valid(header) && string(header) != "" {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(header, &raw); err != nil {
			t.Fatal(err)
		}
		if _, present := raw["Origin"]; present {
			t.Fatal("the crafted pre-adoption header must not carry an Origin field")
		}
	}
	writeCraftedJournal(t, options, key, header)

	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatalf("open realm over a pre-adoption generation: %v", err)
	}
	defer realm.Close()
	origin, err := realm.Origin(key)
	if err != nil || origin != OriginBirth {
		t.Fatalf("pre-adoption origin=%q err=%v, want birth", origin, err)
	}
	if realm.UnifiedEligible(key) {
		t.Fatal("a reopened generation must fail closed for unified replay")
	}
}

// TestAdoptionOriginUnknownIsCorrupt pins the closed set: an origin outside
// birth|reconstructed is a corrupt journal, never a value to skip.
func TestAdoptionOriginUnknownIsCorrupt(t *testing.T) {
	options := journalOptions(t)
	key := journalKey("%2", "incarnation-b")
	header, err := json.Marshal(journalHeader{
		Version: journalVersion, Key: key, BrokerIncarnation: "previous-broker",
		GeometryInitial: &Geometry{Columns: 80, Rows: 24}, Origin: GenerationOrigin("resurrected"),
	})
	if err != nil {
		t.Fatal(err)
	}
	writeCraftedJournal(t, options, key, header)

	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatalf("a corrupt generation must not deny the whole realm: %v", err)
	}
	defer realm.Close()
	if _, err := realm.Origin(key); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("unknown origin error=%v, want corrupt journal", err)
	}
	if realm.UnifiedEligible(key) {
		t.Fatal("a corrupt-origin generation is unified-eligible")
	}
}

// TestAdoptionHeaderlessStillFailsClosed re-pins the existing discipline in
// the adoption era. A headerless file cannot be attributed to any pane, so it
// fails closed at the realm boundary: the realm refuses to open over it. A
// header that parses but names a geometry-less generation stays attributable
// and fails closed per pane instead.
func TestAdoptionHeaderlessStillFailsClosed(t *testing.T) {
	t.Run("unattributable", func(t *testing.T) {
		options := journalOptions(t)
		key := journalKey("%3", "incarnation-c")
		paths := storagePathsForTest(options, key)
		if err := os.MkdirAll(filepath.Dir(paths.Journal), options.DirectoryMode.Perm()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.Journal, []byte("PUJ2 without a header"), options.FileMode.Perm()); err != nil {
			t.Fatal(err)
		}
		if _, err := openRealm(options, realJournalOps()); !errors.Is(err, ErrUnsafeRuntime) {
			t.Fatalf("headerless realm open error=%v, want unsafe runtime", err)
		}
	})
	t.Run("geometry-less", func(t *testing.T) {
		options := journalOptions(t)
		key := journalKey("%3", "incarnation-c")
		header, err := json.Marshal(journalHeader{Version: journalVersion, Key: key, BrokerIncarnation: "previous-broker"})
		if err != nil {
			t.Fatal(err)
		}
		writeCraftedJournal(t, options, key, header)
		realm, err := openRealm(options, realJournalOps())
		if err != nil {
			t.Fatalf("an attributable geometry-less generation must not deny the realm: %v", err)
		}
		defer realm.Close()
		if _, err := realm.Origin(key); !errors.Is(err, ErrCorruptJournal) {
			t.Fatalf("geometry-less origin error=%v, want corrupt journal", err)
		}
		if realm.UnifiedEligible(key) {
			t.Fatal("a geometry-less generation is unified-eligible")
		}
	})
}

// TestAdoptionReconstructedRoundTripFailsClosedOnReopen checks that a
// reconstructed generation keeps its origin across a reopen, is never
// resumed as live, and its key refuses re-admission — a fresh generation is
// the only way forward.
func TestAdoptionReconstructedRoundTripFailsClosedOnReopen(t *testing.T) {
	options := journalOptions(t)
	key := journalKey("%4", "incarnation-d")
	geometry := Geometry{Columns: 80, Rows: 24}

	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	if err := realm.AdmitReconstructedPane(key, geometry); err != nil {
		t.Fatalf("admit reconstructed: %v", err)
	}
	if origin, err := realm.Origin(key); err != nil || origin != OriginReconstructed {
		t.Fatalf("live origin=%q err=%v", origin, err)
	}
	// Idempotent for the identical live generation, closed for a birth
	// re-admission of the same key: origin is part of the identity judgement.
	if err := realm.AdmitReconstructedPane(key, geometry); err != nil {
		t.Fatalf("identical reconstructed re-admission: %v", err)
	}
	if err := realm.AdmitPane(key, geometry); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("birth re-admission over a reconstructed generation: %v, want invalidated", err)
	}
	if err := realm.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.UnifiedEligible(key) {
		t.Fatal("a reopened reconstructed generation is unified-eligible: stale resume")
	}
	if origin, err := reopened.Origin(key); err != nil || origin != OriginReconstructed {
		t.Fatalf("reopened origin=%q err=%v", origin, err)
	}
	if err := reopened.AdmitReconstructedPane(key, geometry); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("re-admission of a stale reconstructed key: %v, want invalidated", err)
	}
}

// TestCompletePaneSlotsOptionSizesTheLedger pins the adoption-era sizing knob:
// CompletePaneSlots overrides the default cap, availability reports the
// remaining budget, and exhaustion refuses with the quota error before any
// admission side effect.
func TestCompletePaneSlotsOptionSizesTheLedger(t *testing.T) {
	options := journalOptions(t)
	options.CompletePaneSlots = 3
	realm, err := openRealm(options, realJournalOps())
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	geometry := Geometry{Columns: 80, Rows: 24}
	if available := realm.AvailableCompletePaneSlots(); available != 2 {
		t.Fatalf("fresh availability=%d want 2", available)
	}
	if err := realm.AdmitPane(journalKey("%5", "incarnation-e"), geometry); err != nil {
		t.Fatal(err)
	}
	if err := realm.AdmitReconstructedPane(journalKey("%6", "incarnation-f"), geometry); err != nil {
		t.Fatal(err)
	}
	if available := realm.AvailableCompletePaneSlots(); available != 0 {
		t.Fatalf("exhausted availability=%d want 0", available)
	}
	if err := realm.AdmitReconstructedPane(journalKey("%7", "incarnation-g"), geometry); !errors.Is(err, ErrQuota) {
		t.Fatalf("over-budget admission error=%v, want quota", err)
	}
	if realm.UnifiedEligible(journalKey("%7", "incarnation-g")) {
		t.Fatal("a refused admission left an eligible generation")
	}
}
