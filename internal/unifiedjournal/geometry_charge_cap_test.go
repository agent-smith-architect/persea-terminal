package unifiedjournal

import (
	"errors"
	"os"
	"testing"
)

const geometryCapRed = "RESIZE_DEAD_END/GEOMETRY_CAP_MEASURES_LIVE_CHARGE"

// TestGeometryCapMeasuresLiveChargeNotFileSize reproduces the production
// arithmetic behind the operator's resize_failed dead end: a long-lived pane
// whose journal FILE had grown past the pane cap on framing overhead alone
// (128 bytes per output record, never charged) while its live charge — the
// quantity output appends are budgeted on — still had headroom. The geometry
// cap check measured the file, so the first explicit Fit was refused as over
// quota and invalidated the generation. A budget has one unit: geometry and
// output must be refused at the same charge.
func TestGeometryCapMeasuresLiveChargeNotFileSize(t *testing.T) {
	options := journalOptions(t)
	options.PaneCapBytes = 64 << 10
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", geometryCapRed, err)
	}
	key := journalKey("%1", "pane-inc-a")
	admitJournalPane(t, realm, key)

	// One-byte output records until the file alone exceeds the pane cap. The
	// charge stays a few hundred bytes: every record is almost all framing.
	records := 0
	for realm.panes[key].stored <= options.PaneCapBytes {
		record, err := realm.Append(key, []byte{'x'})
		if err != nil {
			t.Fatalf("%s append record %d: %v", geometryCapRed, records, err)
		}
		commitLast(t, realm, key, record)
		records++
	}
	pane := realm.panes[key]
	if pane.realmCharge != int64(records) || pane.realmCharge >= options.PaneCapBytes/8 {
		t.Fatalf("%s charge=%d records=%d: the fixture did not separate file size from charge", geometryCapRed, pane.realmCharge, records)
	}
	info, err := os.Stat(journalPathFor(t, options, key))
	if err != nil || info.Size() <= options.PaneCapBytes {
		t.Fatalf("%s journal file size=%d must exceed the cap %d: %v", geometryCapRed, info.Size(), options.PaneCapBytes, err)
	}

	// The measured failure: a geometry record with charge headroom to spare.
	geometry, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: 48})
	if err != nil {
		t.Fatalf("%s geometry refused with %d bytes of charge headroom (file=%d cap=%d): %v",
			geometryCapRed, options.PaneCapBytes-pane.realmCharge, info.Size(), options.PaneCapBytes, err)
	}
	commitLast(t, realm, key, geometry)
	if pane.state != EligibilityContinuous {
		t.Fatalf("%s the refused fit must not have invalidated the generation: state=%v reason=%v", geometryCapRed, pane.state, pane.reason)
	}
	if _, err := realm.Append(key, []byte("output after fit")); err != nil {
		t.Fatalf("%s output after a fit on a framing-heavy journal: %v", geometryCapRed, err)
	}

	// The cap still bounds the charge, in the same unit for both record kinds:
	// fill to one geometry record below the cap, then geometry fits exactly
	// once and output of one byte is refused — and refusal is by charge, not by
	// how many framed bytes the file happens to hold.
	remaining := options.PaneCapBytes - pane.realmCharge - geometryRecordCost
	if remaining <= 0 {
		t.Fatalf("%s fixture left no room to fill: remaining=%d", geometryCapRed, remaining)
	}
	fill, err := realm.Append(key, make([]byte, remaining))
	if err != nil {
		t.Fatalf("%s fill to the cap: %v", geometryCapRed, err)
	}
	commitLast(t, realm, key, fill)
	if pane.realmCharge != options.PaneCapBytes-geometryRecordCost {
		t.Fatalf("%s charge=%d want=%d", geometryCapRed, pane.realmCharge, options.PaneCapBytes-geometryRecordCost)
	}
	last, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: 30})
	if err != nil {
		t.Fatalf("%s the last geometry record inside the cap: %v", geometryCapRed, err)
	}
	commitLast(t, realm, key, last)
	if pane.realmCharge != options.PaneCapBytes {
		t.Fatalf("%s charge=%d want the cap %d", geometryCapRed, pane.realmCharge, options.PaneCapBytes)
	}
	if _, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: 31}); !errors.Is(err, ErrQuota) {
		t.Fatalf("%s geometry past the cap err=%v want=%v", geometryCapRed, err, ErrQuota)
	}
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", geometryCapRed, err)
	}

	// A reopen rebuilds exactly the live charge, so the verdict survives a
	// broker restart: still at the cap, still refused.
	reopened, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s reopen: %v", geometryCapRed, err)
	}
	defer reopened.Close()
	if reopened.panes[key].realmCharge != options.PaneCapBytes {
		t.Fatalf("%s reopened charge=%d want=%d", geometryCapRed, reopened.panes[key].realmCharge, options.PaneCapBytes)
	}
}
