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

// A cursor that is not a committed position is refused, never read from: a
// byte offset alone cannot order geometry, and a stale or foreign cursor must
// not silently skip or repeat records.
func TestCommittedSuffixRefusesACursorThatIsNotAPosition(t *testing.T) {
	realm, key, all := suffixFixture(t)
	frontier := int64(len(all))
	for name, cursor := range map[string]CommittedCursor{
		"beyond the frontier":         {Sequence: frontier + 1, Offset: all[frontier-1].End},
		"frontier at a wrong offset":  {Sequence: frontier, Offset: all[frontier-1].End - 1},
		"inside its own last record":  {Sequence: 2, Offset: all[1].End - 1},
		"past the next output record": {Sequence: 2, Offset: all[2].End},
		"after geometry, inside next": {Sequence: 3, Offset: all[3].Start + 1},
		"inside a record two ahead":   {Sequence: 2, Offset: all[5].Start + 1},
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

// A page whose last frame ended inside an output record resumes from that
// byte: the read returns the rest of the record, then the projection after it,
// and SuffixAllocation charges exactly that.
func TestCommittedSuffixResumesInsideAnOutputRecord(t *testing.T) {
	realm, key, all := suffixFixture(t)
	for _, next := range all {
		if next.Kind != RecordOutput {
			continue
		}
		for _, offset := range []int64{next.Start + 1, (next.Start + next.End) / 2, next.End - 1} {
			cursor := CommittedCursor{Sequence: next.Sequence - 1, Offset: offset}
			if err := realm.ValidCursor(key, cursor); err != nil {
				t.Fatalf("%+v: %v", cursor, err)
			}
			got, err := realm.ReadCommittedEventsAfter(key, cursor, 1<<20)
			if err != nil || int64(len(got)) != int64(len(all))-cursor.Sequence {
				t.Fatalf("%+v: %d records err=%v", cursor, len(got), err)
			}
			rest := next.Payload[offset-next.Start:]
			if got[0].Sequence != next.Sequence || got[0].Start != offset || got[0].End != next.End || !bytes.Equal(got[0].Payload, rest) || cap(got[0].Payload) != len(rest) {
				t.Fatalf("%+v: first = %+v, want the rest of record %d", cursor, got[0], next.Sequence)
			}
			payload := int64(len(rest))
			for index, event := range got[1:] {
				if !sameEvent(event, all[next.Sequence+int64(index)]) {
					t.Fatalf("%+v: record %d = %+v", cursor, index+1, event)
				}
				payload += int64(len(event.Payload))
			}
			charge, records, err := realm.SuffixAllocation(key, cursor, 1<<20)
			want := AllocationCharge(int64(len(got))*int64(unsafe.Sizeof(Event{}))+8) + AllocationCharge(payload)
			if err != nil || records != int64(len(got)) || charge != want {
				t.Fatalf("%+v: allocation %d/%d err=%v, want %d/%d", cursor, charge, records, err, want, len(got))
			}
		}
	}
}

// Each generation names its stream afresh, so a position from one is refused
// by any other generation, even one with the same key; a generation reopened
// by a later broker has none, so no position outlives the broker.
func TestCommittedStreamIsFreshForEveryGeneration(t *testing.T) {
	options := rotationOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatal(err)
	}
	key := rotationKey("$stream", 1)
	if err := realm.AdmitPane(key, Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	verifiedOutput(t, realm, key, []byte("alpha"))
	first, err := realm.Stream(key)
	if err != nil || len(first) < 26 {
		t.Fatalf("stream %q err=%v", first, err)
	}
	other, otherKey, _ := suffixFixture(t)
	if second, err := other.Stream(otherKey); err != nil || second == first || len(second) != len(first) {
		t.Fatalf("another generation's stream %q (first %q) err=%v", second, first, err)
	}
	if err := realm.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRealm(options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if stream, err := reopened.Stream(key); err == nil && stream != "" {
		t.Fatalf("a reopened generation kept a stream: %q", stream)
	}
}

// GeometryAt is the geometry a reader has applied after a record: the newest
// geometry at or before it, else the birth geometry.
func TestGeometryAtFollowsTheProjection(t *testing.T) {
	realm, key, all := suffixFixture(t)
	want := Geometry{Columns: 80, Rows: 24}
	for sequence := int64(0); sequence <= int64(len(all)); sequence++ {
		if sequence > 0 && all[sequence-1].Kind == RecordGeometry {
			want = all[sequence-1].Geometry
		}
		if got, err := realm.GeometryAt(key, sequence); err != nil || got != want {
			t.Fatalf("after %d: %+v err=%v, want %+v", sequence, got, err, want)
		}
	}
	if _, err := realm.GeometryAt(key, int64(len(all))+1); !errors.Is(err, ErrCursorMismatch) {
		t.Fatalf("beyond the frontier: %v", err)
	}
}
