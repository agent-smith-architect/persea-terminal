package unifiedjournal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"testing"
	"time"
)

// Keep the historical workload fixed: all historical appends share one commit,
// then measure only committing one more 64 KiB record.
func TestRecordingCommitWorkIndependentOfHistory(t *testing.T) {
	for _, prior := range []int{100, 1000, 10000, 32000} {
		t.Run(fmt.Sprint(prior), func(t *testing.T) {
			realm, err := OpenRealm(journalOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			defer realm.Close()
			key := journalKey("%growth", "synthetic-growth")
			admitJournalPane(t, realm, key)
			var record Record
			payload := bytes.Repeat([]byte{'h'}, 256)
			for i := 0; i < prior; i++ {
				record, err = realm.Append(key, payload)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := realm.Sync(key); err != nil {
				t.Fatal(err)
			}
			if err := realm.AdvanceCommitted(key, record); err != nil {
				t.Fatal(err)
			}
			readBytes := 0
			realRead := realm.ops.readAll
			realm.ops.readAll = func(reader io.Reader) ([]byte, error) {
				data, err := realRead(reader)
				readBytes += len(data)
				return data, err
			}
			realReadAt := realm.ops.readAt
			realm.ops.readAt = func(file *os.File, data []byte, offset int64) (int, error) {
				n, err := realReadAt(file, data, offset)
				readBytes += n
				return n, err
			}
			progressBefore := realm.VerificationProgress()
			record, err = realm.Append(key, bytes.Repeat([]byte{'n'}, 64<<10))
			if err != nil {
				t.Fatal(err)
			}
			if err := realm.Sync(key); err != nil {
				t.Fatal(err)
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			started := time.Now()
			err = realm.AdvanceCommitted(key, record)
			duration := time.Since(started)
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatal(err)
			}
			decoded := realm.VerificationProgress().DecodedRecords - progressBefore.DecodedRecords
			receipt, _ := json.Marshal(map[string]any{"prior_records": prior, "new_payload_bytes": 64 << 10, "read_bytes": readBytes, "decoded_records": decoded, "allocated_bytes": after.TotalAlloc - before.TotalAlloc, "duration_ns": duration.Nanoseconds(), "prefill": "all appends then one commit"})
			t.Log(string(receipt))
			if readBytes < (64<<10)+appendFixed+commitFixed {
				t.Errorf("required new-record readback was not observed: counted=%d", readBytes)
			}
			if readBytes > (64<<10)+4096 {
				t.Errorf("same new batch re-read %d bytes with %d old records; fixed suffix budget=%d", readBytes, prior, (64<<10)+4096)
			}
			if decoded != 1 {
				t.Errorf("decoded %d records for one new append", decoded)
			}
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 128<<10 {
				t.Errorf("new 64KiB commit allocated %d bytes", allocated)
			}
		})
	}
}

func TestVerificationFailureDoesNotPublishFrontier(t *testing.T) {
	realm, err := OpenRealm(journalOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	key := journalKey("%verify", "synthetic-failure")
	admitJournalPane(t, realm, key)
	record, err := realm.Append(key, []byte("unverified"))
	if err != nil {
		t.Fatal(err)
	}
	if err := realm.Sync(key); err != nil {
		t.Fatal(err)
	}
	realm.ops.readAll = func(io.Reader) ([]byte, error) { return nil, io.ErrUnexpectedEOF }
	realm.ops.readAt = func(*os.File, []byte, int64) (int, error) { return 0, io.ErrUnexpectedEOF }
	if err := realm.AdvanceCommitted(key, record); err == nil {
		t.Fatal("readback failure succeeded")
	}
	if got := realm.CommittedOffset(key); got != 0 {
		t.Errorf("published offset %d after failed verification", got)
	}
	if got := realm.CommittedSequence(key); got != 0 {
		t.Errorf("published sequence %d after failed verification", got)
	}
}
