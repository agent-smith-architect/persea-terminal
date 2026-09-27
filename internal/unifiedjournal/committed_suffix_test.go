package unifiedjournal

import (
	"bytes"
	"errors"
	"runtime"
	"testing"
	"unsafe"
)

// suffixFixture commits output and geometry records, including consecutive
// geometry that shares one byte offset, and returns the full projection.
func suffixFixture(t *testing.T) (*Realm, PaneKey, []Event) {
	t.Helper()
	realm, err := OpenRealm(rotationOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = realm.Close() })
	key := rotationKey("$suffix", 1)
	if err := realm.AdmitPane(key, Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	geometry := func(rows int) {
		record, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: rows})
		if err != nil {
			t.Fatal(err)
		}
		commitLast(t, realm, key, record)
	}
	geometry(30)
	verifiedOutput(t, realm, key, []byte("alpha"))
	verifiedOutput(t, realm, key, bytes.Repeat([]byte("b"), 300))
	geometry(31)
	geometry(32)
	verifiedOutput(t, realm, key, []byte("gamma"))
	verifiedOutput(t, realm, key, bytes.Repeat([]byte("d"), 1000))
	geometry(33)
	events, err := realm.ReadCommittedEvents(key)
	if err != nil {
		t.Fatal(err)
	}
	return realm, key, events
}

func cursorAfter(events []Event, sequence int64) CommittedCursor {
	if sequence == 0 {
		return CommittedCursor{}
	}
	return CommittedCursor{Sequence: sequence, Offset: events[sequence-1].End}
}

// Every cursor and bound returns exactly the next records of the full
// projection: the longest run within the bound, never less than one record,
// and SuffixAllocation describes that same read.
func TestCommittedSuffixMatchesTheProjectionFromEveryCursor(t *testing.T) {
	realm, key, all := suffixFixture(t)
	for after := int64(0); after <= int64(len(all)); after++ {
		cursor := cursorAfter(all, after)
		for _, maxBytes := range []int64{0, 1, 5, 80, 160, 299, 300, 305, 460, 1000, 1500, 1 << 20} {
			got, err := realm.ReadCommittedEventsAfter(key, cursor, maxBytes)
			if err != nil {
				t.Fatalf("after=%d max=%d: %v", after, maxBytes, err)
			}
			// The bound counts payload and event index alike.
			want := int64(0)
			for last := after + 1; last <= int64(len(all)); last++ {
				cost := all[last-1].End - cursor.Offset + (last-after)*int64(unsafe.Sizeof(Event{}))
				if last > after+1 && cost > maxBytes {
					break
				}
				want = last - after
			}
			if int64(len(got)) != want {
				t.Fatalf("after=%d max=%d: %d records, want %d", after, maxBytes, len(got), want)
			}
			payload := int64(0)
			for index, event := range got {
				if !sameEvent(event, all[after+int64(index)]) {
					t.Fatalf("after=%d max=%d record %d = %+v, want %+v", after, maxBytes, index, event, all[after+int64(index)])
				}
				if cap(event.Payload) != len(event.Payload) {
					t.Fatalf("after=%d record %d payload can grow into its neighbour", after, index)
				}
				payload += int64(len(event.Payload))
			}
			charge, records, err := realm.SuffixAllocation(key, cursor, maxBytes)
			if err != nil || records != want {
				t.Fatalf("after=%d max=%d allocation records=%d err=%v, want %d", after, maxBytes, records, err, want)
			}
			wantCharge := int64(0)
			if want != 0 {
				wantCharge = AllocationCharge(want*int64(unsafe.Sizeof(Event{}))+8) + AllocationCharge(payload)
			}
			if charge != wantCharge {
				t.Fatalf("after=%d max=%d allocation charge=%d, want %d", after, maxBytes, charge, wantCharge)
			}
		}
	}
}

// A run of records without payload is bounded by its event index: geometry
// alone cannot make one round copy an arbitrarily large index.
func TestCommittedSuffixBoundsARunOfGeometryByItsIndex(t *testing.T) {
	realm, err := OpenRealm(rotationOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	key := rotationKey("$suffix-geometry", 1)
	if err := realm.AdmitPane(key, Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	for rows := 25; rows < 25+64; rows++ {
		record, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: rows})
		if err != nil {
			t.Fatal(err)
		}
		commitLast(t, realm, key, record)
	}
	index := int64(unsafe.Sizeof(Event{}))
	got, err := realm.ReadCommittedEventsAfter(key, CommittedCursor{}, 10*index)
	if err != nil || len(got) != 10 {
		t.Fatalf("a %d-byte bound read %d geometry records (err=%v), want 10", 10*index, len(got), err)
	}
}

// A cursor that is not a committed record boundary is refused, never read
// from: a byte offset alone cannot order geometry, and a stale or foreign
// cursor must not silently skip or repeat records.
func TestCommittedSuffixRefusesACursorThatIsNotARecordBoundary(t *testing.T) {
	realm, key, all := suffixFixture(t)
	frontier := int64(len(all))
	for name, cursor := range map[string]CommittedCursor{
		"beyond the frontier":         {Sequence: frontier + 1, Offset: all[frontier-1].End},
		"frontier at a wrong offset":  {Sequence: frontier, Offset: all[frontier-1].End - 1},
		"inside an output record":     {Sequence: 2, Offset: all[1].End - 1},
		"geometry run at next offset": {Sequence: 4, Offset: all[5].End},
		"negative sequence":           {Sequence: -1},
	} {
		if _, err := realm.ReadCommittedEventsAfter(key, cursor, 1<<20); !errors.Is(err, ErrCursorMismatch) {
			t.Errorf("%s: read err=%v, want ErrCursorMismatch", name, err)
		}
		if _, _, err := realm.SuffixAllocation(key, cursor, 1<<20); !errors.Is(err, ErrCursorMismatch) {
			t.Errorf("%s: allocation err=%v, want ErrCursorMismatch", name, err)
		}
	}
}

// The read copies: a caller mutating its suffix cannot change the projection.
func TestCommittedSuffixReturnsCallerOwnedCopies(t *testing.T) {
	realm, key, all := suffixFixture(t)
	got, err := realm.ReadCommittedEventsAfter(key, cursorAfter(all, 1), 1<<20)
	if err != nil || len(got) == 0 {
		t.Fatalf("read=%d err=%v", len(got), err)
	}
	got[0].Payload[0] = 'X'
	again, err := realm.ReadCommittedEvents(key)
	if err != nil || again[1].Payload[0] != 'a' {
		t.Fatal("caller copy mutated the authoritative projection")
	}
}

// A read after a long history allocates for the suffix only: reading the whole
// projection and discarding its prefix would repeat the history's work and
// allocation on every catch-up round.
func TestCommittedSuffixAllocatesOnlyTheSuffix(t *testing.T) {
	realm, err := OpenRealm(rotationOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	key := rotationKey("$suffix-work", 1)
	if err := realm.AdmitPane(key, Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("h"), 64<<10)
	for i := 0; i < 64; i++ {
		verifiedOutput(t, realm, key, chunk)
	}
	cursor := CommittedCursor{Sequence: 64, Offset: 64 * int64(len(chunk))}
	verifiedOutput(t, realm, key, []byte("suffix"))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := realm.ReadCommittedEventsAfter(key, cursor, 1<<20)
	runtime.ReadMemStats(&after)
	if err != nil || len(got) != 1 || string(got[0].Payload) != "suffix" {
		t.Fatalf("suffix=%+v err=%v", got, err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 256<<10 {
		t.Fatalf("a %d-byte suffix after 4 MiB of history allocated %d bytes", len("suffix"), allocated)
	}
}
