package unifiedjournal

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"syscall"
	"testing"
)

func verificationRealm(t *testing.T) (*Realm, PaneKey) {
	t.Helper()
	realm, err := OpenRealm(journalOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { realm.Close() })
	key := journalKey("%verification", "synthetic-verification")
	admitJournalPane(t, realm, key)
	return realm, key
}

func verifiedOutput(t *testing.T, realm *Realm, key PaneKey, data []byte) Record {
	t.Helper()
	record, err := realm.Append(key, data)
	if err != nil {
		t.Fatal(err)
	}
	commitLast(t, realm, key, record)
	return record
}

func TestIncrementalVerificationChecksEveryNewRecord(t *testing.T) {
	mutations := map[string]struct{ frame, offset int }{
		"first payload": {0, appendFixed}, "first hash": {0, 38},
		"first kind": {0, 1}, "first sequence": {0, 9}, "first start": {0, 17},
		"first end": {0, 25}, "output geometry": {0, 33}, "payload length": {0, 29},
		"geometry type": {1, 1}, "geometry sequence": {1, 9}, "geometry columns": {1, 33},
		"geometry rows": {1, 37}, "geometry hash": {1, 38}, "geometry start": {1, 17},
		"second geometry": {2, 38}, "last output": {3, appendFixed},
		"commit kind": {4, 1}, "commit sequence": {4, 9}, "commit start": {4, 17},
		"commit end": {4, 25}, "commit hash": {4, 26}, "commit marker": {4, 0},
	}
	for name, mutation := range mutations {
		t.Run(name, func(t *testing.T) {
			realm, key := verificationRealm(t)
			prefix := verifiedOutput(t, realm, key, []byte("verified prefix"))
			pane := realm.panes[key]
			oldView, oldCursor := pane.verified.view, pane.verified.physical
			offsets := []int64{pane.stored}
			if _, err := realm.Append(key, []byte("first output")); err != nil {
				t.Fatal(err)
			}
			offsets = append(offsets, pane.stored)
			if _, err := realm.AppendGeometry(key, Geometry{80, 31}); err != nil {
				t.Fatal(err)
			}
			offsets = append(offsets, pane.stored)
			if _, err := realm.AppendGeometry(key, Geometry{80, 32}); err != nil {
				t.Fatal(err)
			}
			offsets = append(offsets, pane.stored)
			last, err := realm.Append(key, []byte("last output"))
			if err != nil {
				t.Fatal(err)
			}
			offsets = append(offsets, pane.stored)
			if err := realm.Sync(key); err != nil {
				t.Fatal(err)
			}
			position := offsets[mutation.frame] + int64(mutation.offset)
			realRead := realm.ops.readAt
			realm.ops.readAt = func(file *os.File, data []byte, offset int64) (int, error) {
				n, err := realRead(file, data, offset)
				if position >= offset && position < offset+int64(n) {
					data[position-offset] ^= 0x40
				}
				return n, err
			}
			if err := realm.AdvanceCommitted(key, last); !errors.Is(err, ErrCorruptJournal) {
				t.Fatalf("corrupt suffix result: %v", err)
			}
			if pane.verified.view != oldView || pane.verified.physical != oldCursor || realm.CommittedOffset(key) != prefix.End || realm.CommittedSequence(key) != prefix.Sequence {
				t.Fatal("failed suffix changed verified publication")
			}
			if realm.Reason(key) != ReasonCorruptJournal {
				t.Fatal("failure did not invalidate authority")
			}
			// Apply the same corruption to the full recovery input. Both paths
			// must use the same framing/type/hash acceptance rules.
			raw, err := os.ReadFile(journalPathFor(t, realm.options, key))
			if err != nil {
				t.Fatal(err)
			}
			raw[position] ^= 0x40
			parsed, parseErr := parseJournal(raw)
			if parseErr == nil {
				// A length that extends beyond EOF is a torn final append:
				// recovery keeps only its old prefix; live cannot claim completion.
				if name != "payload length" || parsed.committedSequence != prefix.Sequence {
					t.Fatalf("recovery unexpectedly accepted %s: seq=%d", name, parsed.committedSequence)
				}
			} else if !errors.Is(parseErr, ErrCorruptJournal) {
				t.Fatalf("recovery classification %v", parseErr)
			}
		})
	}
}

func TestIncrementalVerificationBindsExpectedMetadata(t *testing.T) {
	for _, target := range []string{"earlier self-consistent output", "header identity", "header geometry", "header origin", "header broker"} {
		t.Run(target, func(t *testing.T) {
			realm, key := verificationRealm(t)
			pane := realm.panes[key]
			firstOffset := pane.stored
			if _, err := realm.Append(key, []byte("first")); err != nil {
				t.Fatal(err)
			}
			last, err := realm.Append(key, []byte("last"))
			if err != nil {
				t.Fatal(err)
			}
			if err := realm.Sync(key); err != nil {
				t.Fatal(err)
			}
			realRead := realm.ops.readAt
			var mutated []byte
			realm.ops.readAt = func(file *os.File, data []byte, offset int64) (int, error) {
				if mutated == nil {
					mutated, err = os.ReadFile(journalPathFor(t, realm.options, key))
					if err != nil {
						return 0, err
					}
					switch target {
					case "earlier self-consistent output":
						mutated[firstOffset+appendFixed] ^= 1
						hash := sha256.Sum256(mutated[firstOffset+appendFixed : firstOffset+appendFixed+5])
						copy(mutated[firstOffset+38:firstOffset+appendFixed], hash[:])
					case "header identity":
						position := bytes.Index(mutated[:firstOffset], []byte(key.Incarnation))
						mutated[position] ^= 1
					case "header geometry":
						position := bytes.Index(mutated[:firstOffset], []byte("\"columns\":80"))
						if position < 0 {
							t.Fatal("header geometry absent")
						}
						mutated[position+10] = '9'
					case "header origin":
						position := bytes.Index(mutated[:firstOffset], []byte("birth"))
						mutated[position] = 'x'
					case "header broker":
						position := bytes.Index(mutated[:firstOffset], []byte(realm.options.BrokerIncarnation))
						mutated[position] ^= 1
					}
				}
				n, err := realRead(file, data, offset)
				copy(data[:n], mutated[offset:offset+int64(n)])
				return n, err
			}
			if err := realm.AdvanceCommitted(key, last); !errors.Is(err, ErrCorruptJournal) {
				t.Fatalf("changed expected metadata accepted: %v", err)
			}
			if realm.CommittedSequence(key) != 0 || pane.verified.view != nil {
				t.Fatal("unverified initial metadata published")
			}
			if target == "earlier self-consistent output" {
				if _, err := parseJournal(mutated); err != nil {
					t.Fatalf("fixture must be internally valid, but different from submitted data: %v", err)
				}
			}
		})
	}
}

func TestIncrementalReadWriteAndSyncFaults(t *testing.T) {
	for _, fault := range []string{"short reads", "no progress", "short EOF", "read error", "read full error", "append short write", "commit short write", "append sync", "commit sync"} {
		t.Run(fault, func(t *testing.T) {
			realm, key := verificationRealm(t)
			prefix := verifiedOutput(t, realm, key, []byte("prefix"))
			oldView, oldCursor := realm.panes[key].verified.view, realm.panes[key].verified.physical
			realRead, realWrite, realSync := realm.ops.readAt, realm.ops.write, realm.ops.sync
			realm.ops.readAt = func(file *os.File, data []byte, offset int64) (int, error) {
				switch fault {
				case "short reads":
					if len(data) > 3 {
						data = data[:3]
					}
				case "no progress":
					return 0, nil
				case "short EOF":
					if offset >= oldCursor+appendFixed {
						return 0, io.EOF
					}
				case "read error":
					return 0, syscall.EIO
				case "read full error":
					n, _ := realRead(file, data, offset)
					return n, syscall.EIO
				}
				return realRead(file, data, offset)
			}
			failedWrite := false
			realm.ops.write = func(file *os.File, data []byte) (int, error) {
				if failedWrite {
					return 0, nil
				}
				if fault == "append short write" && data[0] == appendMarker || fault == "commit short write" && data[0] == commitMarker {
					failedWrite = true
					return realWrite(file, data[:len(data)/2])
				}
				return realWrite(file, data)
			}
			syncCalls := 0
			realm.ops.sync = func(file *os.File) error {
				syncCalls++
				if fault == "append sync" && syncCalls == 1 || fault == "commit sync" && syncCalls == 2 {
					return syscall.EIO
				}
				return realSync(file)
			}
			feeds := 0
			sequencer := NewSequencer(realm, func(PaneKey, Record, []byte) error { feeds++; return nil })
			err := sequencer.Write(key, bytes.Repeat([]byte{'n'}, projectionPayloadPageBytes+1))
			if fault == "short reads" {
				if err != nil || feeds != 1 {
					t.Fatalf("legal partial reads failed: err=%v feeds=%d", err, feeds)
				}
				return
			}
			if err == nil || feeds != 0 || !sequencer.Failed() {
				t.Fatalf("fault escaped: err=%v feeds=%d", err, feeds)
			}
			pane := realm.panes[key]
			if realm.CommittedSequence(key) != prefix.Sequence || realm.CommittedOffset(key) != prefix.End || pane.verified.view != oldView || pane.verified.physical != oldCursor {
				t.Fatal("failed I/O published a frontier")
			}
			if !errors.Is(sequencer.Write(key, []byte("retry")), ErrInvalidated) {
				t.Fatal("failed sequencer was not sticky")
			}
		})
	}
}

func TestVerifiedProjectionIntegrityAndOwnership(t *testing.T) {
	realm, key := verificationRealm(t)
	payload := bytes.Repeat([]byte("0123456789"), 10000)
	first := verifiedOutput(t, realm, key, payload)
	oldView := realm.panes[key].verified.view
	snapshot, err := realm.ReadCommittedEvents(key)
	if err != nil {
		t.Fatal(err)
	}
	// A separate writer changes historical disk bytes after they were verified.
	file, err := os.OpenFile(journalPathFor(t, realm.options, key), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteAt([]byte{'X'}, realm.panes[key].headerSize+appendFixed)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	verifiedOutput(t, realm, key, []byte("suffix"))
	if realm.panes[key].verified.view.previous != oldView {
		t.Fatal("ordinary append rebuilt historical index")
	}
	got, err := realm.ReadCommitted(key)
	if err != nil || !bytes.Equal(got[:first.End], payload) || !bytes.Equal(snapshot[0].Payload, payload) {
		t.Fatal("historical immutable view changed")
	}
	got[0], snapshot[0].Payload[0] = 'Y', 'Z'
	again, err := realm.ReadCommitted(key)
	if err != nil || !bytes.Equal(again[:first.End], payload) {
		t.Fatal("snapshot recipient mutated journal authority")
	}
	retained, metadata, records := realm.ProjectionUsage(key)
	if retained != int64(len(payload)+6) || records != 2 || metadata <= 0 || realm.panes[key].pendingCount != 0 {
		t.Fatalf("projection accounting=%d/%d/%d", retained, metadata, records)
	}
	for page := oldView.payload; page != nil; page = page.previous {
		if cap(page.data) > projectionPayloadPageBytes {
			t.Fatal("unbounded payload page")
		}
	}
	raw, err := os.ReadFile(journalPathFor(t, realm.options, key))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseJournal(raw); !errors.Is(err, ErrCorruptJournal) {
		t.Fatal("full recovery missed historical corruption")
	}
}

func TestIncrementalAndRecoveryProjectionAgree(t *testing.T) {
	realm, key := verificationRealm(t)
	// Independent input/expected model: expected events use only this fixture,
	// never a returned Record, parser cursor, or live/recovered projection.
	type input struct {
		kind     RecordKind
		size     int
		fill     byte
		geometry Geometry
	}
	batches := [][]input{
		{
			{kind: RecordOutput, size: 0},
			{kind: RecordOutput, size: 0},
			{kind: RecordGeometry, geometry: Geometry{80, 25}},
			{kind: RecordOutput, size: 1, fill: 'a'},
		},
		{
			{kind: RecordOutput, size: 32767, fill: 'b'},
			{kind: RecordGeometry, geometry: Geometry{80, 26}},
			{kind: RecordOutput, size: 127, fill: 'c'},
			{kind: RecordOutput, size: 0},
			{kind: RecordGeometry, geometry: Geometry{80, 27}},
		},
		{
			{kind: RecordOutput, size: 32768, fill: 'd'},
			{kind: RecordOutput, size: 32769, fill: 'e'},
			{kind: RecordGeometry, geometry: Geometry{80, 28}},
			{kind: RecordOutput, size: 0},
			{kind: RecordOutput, size: 0},
		},
	}
	outputSizes := make(map[int]int)
	previousZeroOutput, consecutiveZeroOutputs := false, false
	var expected []Event
	var expectedBytes []byte
	var expectedOffset int64
	expectedGeometry := Geometry{80, 24}
	geometryAfterOutput, outputAfterGeometry, multiAppend := false, false, false
	for batchIndex, batch := range batches {
		multiAppend = multiAppend || len(batch) > 1
		var record Record
		for _, item := range batch {
			event := Event{Kind: item.kind, Sequence: int64(len(expected) + 1), Start: expectedOffset, End: expectedOffset, Geometry: item.geometry}
			var appendErr error
			switch item.kind {
			case RecordOutput:
				event.Payload = bytes.Repeat([]byte{item.fill}, item.size)
				event.End += int64(item.size)
				expectedBytes = append(expectedBytes, event.Payload...)
				outputSizes[item.size]++
				consecutiveZeroOutputs = consecutiveZeroOutputs || (previousZeroOutput && item.size == 0)
				previousZeroOutput = item.size == 0
				outputAfterGeometry = outputAfterGeometry || geometryAfterOutput
				record, appendErr = realm.Append(key, append([]byte(nil), event.Payload...))
			case RecordGeometry:
				geometryAfterOutput = geometryAfterOutput || len(outputSizes) > 0
				previousZeroOutput = false
				expectedGeometry = item.geometry
				record, appendErr = realm.AppendGeometry(key, item.geometry)
			default:
				t.Fatalf("unknown fixture kind %d", item.kind)
			}
			if appendErr != nil {
				t.Fatal(appendErr)
			}
			expected = append(expected, event)
			expectedOffset = event.End
		}
		commitLast(t, realm, key, record)
		raw, err := os.ReadFile(journalPathFor(t, realm.options, key))
		if err != nil {
			t.Fatal(err)
		}
		// Open an actual copied journal through the recovery boundary. The
		// live writer remains open so later batches exercise incremental state.
		fresh := freshRuntime(t, realm.options)
		writeJournalCopy(t, fresh, key, raw)
		recovery, err := OpenRealm(fresh)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { recovery.Close() })
		for name, view := range map[string]*Realm{"live": realm, "recovered": recovery} {
			events, err := view.ReadCommittedEvents(key)
			if err != nil || !reflect.DeepEqual(events, expected) {
				t.Fatalf("batch %d %s events differ from independent fixture: err=%v", batchIndex, name, err)
			}
			data, err := view.ReadCommitted(key)
			if err != nil || !bytes.Equal(data, expectedBytes) || view.CommittedSequence(key) != int64(len(expected)) || view.CommittedOffset(key) != expectedOffset {
				t.Fatalf("batch %d %s bytes/frontier differ from independent fixture: err=%v", batchIndex, name, err)
			}
		}
		classification := recovery.Recovered()
		if len(classification) != 1 || classification[0].Initial != (Geometry{80, 24}) || classification[0].Final != expectedGeometry || classification[0].Committed != expectedOffset || classification[0].Eligibility != EligibilityLegacy || classification[0].Reason != ReasonVolatileRestart {
			t.Fatalf("batch %d recovery classification differs from fixture: %+v", batchIndex, classification)
		}
	}
	for size, wantCount := range map[int]int{0: 5, 1: 1, 127: 1, 32767: 1, 32768: 1, 32769: 1} {
		if outputSizes[size] != wantCount {
			t.Errorf("fixture emitted %d OUTPUT records of size %d, want %d", outputSizes[size], size, wantCount)
		}
	}
	if !consecutiveZeroOutputs {
		t.Error("fixture never emitted consecutive zero-byte OUTPUT records")
	}
	if !geometryAfterOutput || !outputAfterGeometry || !multiAppend {
		t.Error("fixture must interleave OUTPUT/GEOMETRY and commit multiple appends together")
	}
}

func TestSharedDecoderPreservesTornTailClassification(t *testing.T) {
	realm, key := verificationRealm(t)
	verifiedOutput(t, realm, key, []byte("prefix"))
	base, err := os.ReadFile(journalPathFor(t, realm.options, key))
	if err != nil {
		t.Fatal(err)
	}
	record, err := realm.AppendGeometry(key, Geometry{80, 45})
	if err != nil {
		t.Fatal(err)
	}
	appendFrame, commitFrame := encodeAppend(record, nil), encodeCommit(record)
	for cut := 0; cut <= len(appendFrame)+len(commitFrame); cut++ {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			raw := append(append([]byte(nil), base...), append(appendFrame, commitFrame...)[:cut]...)
			parsed, err := parseJournal(raw)
			wantCorrupt := cut > len(appendFrame) && cut < len(appendFrame)+len(commitFrame)
			if errors.Is(err, ErrCorruptJournal) != wantCorrupt {
				t.Fatalf("cut=%d classification=%v wantCorrupt=%v", cut, err, wantCorrupt)
			}
			if cut >= len(appendFrame) && parsed.geometryCharge != geometryRecordCost {
				t.Fatal("validated geometry lost its charge")
			}
			if err == nil && cut < len(appendFrame)+len(commitFrame) && parsed.committedSequence != 1 {
				t.Fatal("uncommitted geometry became visible")
			}
		})
	}
	// Nonconsecutive sequence cannot disappear behind a self-consistent hash.
	bad := append([]byte(nil), appendFrame...)
	binary.BigEndian.PutUint64(bad[2:10], 44)
	if _, err := parseJournal(append(append([]byte(nil), base...), bad...)); !errors.Is(err, ErrCorruptJournal) {
		t.Fatal("sequence gap accepted")
	}
}

func TestCorruptRecoveryKeepsCommittedGeometryClassification(t *testing.T) {
	realm, key := verificationRealm(t)
	record, err := realm.AppendGeometry(key, Geometry{80, 47})
	if err != nil {
		t.Fatal(err)
	}
	commitLast(t, realm, key, record)
	raw, err := os.ReadFile(journalPathFor(t, realm.options, key))
	if err != nil {
		t.Fatal(err)
	}
	fresh := freshRuntime(t, realm.options)
	writeJournalCopy(t, fresh, key, append(raw, 0xff))
	recovered, err := OpenRealm(fresh)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	entries := recovered.Recovered()
	if len(entries) != 1 || entries[0].Final != (Geometry{80, 47}) || entries[0].Reason != ReasonCorruptJournal || entries[0].Committed != 0 {
		t.Fatalf("corrupt recovery classification=%+v", entries)
	}
}
