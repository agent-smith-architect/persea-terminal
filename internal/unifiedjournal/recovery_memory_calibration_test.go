package unifiedjournal

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"testing"
)

// The low-level API can append many records before advancing one final
// commit. Recovered physical bytes therefore cannot be priced as if every
// tiny record had its own commit frame. Build that valid dense file shape
// directly so construction does not retain an unrelated pending-owner graph.
func TestRecordingMemoryCalibrationDenseRecovery(t *testing.T) {
	if os.Getenv("PERSEA_RECORDING_MEMORY_CALIBRATION") != "1" {
		t.Skip("isolated recovery capacity calibration")
	}
	options := rotationOptions(t)
	if path := os.Getenv("PERSEA_RECORDING_DENSE_FIXTURE"); path != "" {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal("dense fixture must be a new private directory", err)
		}
		options.RuntimeDir = path
	}
	options.PaneCapBytes, options.RealmCapBytes = 8<<20, 64<<20
	options.PanePhysicalCapBytes, options.PhysicalCapBytes = 24<<20, 72<<20
	options.CompletePaneSlots = 9
	densePanes := 3
	if value := os.Getenv("PERSEA_RECORDING_DENSE_PANES"); value != "" {
		var err error
		densePanes, err = strconv.Atoi(value)
		if err != nil || densePanes < 1 || densePanes > 3 {
			t.Fatal("dense fixture pane count", value)
		}
	}
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatal(err)
	}
	var records int64
	for i := 0; i < densePanes; i++ {
		key := rotationKey("$dense-memory", uint64(i+1))
		if err := realm.AdmitPane(key, Geometry{80, 24}); err != nil {
			t.Fatal(err)
		}
		pane := realm.panes[key]
		count := (int64(24<<20) - pane.headerSize - commitFixed) / int64(appendFixed+1)
		var last Record
		buffer := make([]byte, 0, 64<<10)
		for sequence := int64(1); sequence <= count; sequence++ {
			last = Record{Key: key, Kind: RecordOutput, Sequence: sequence, Start: sequence - 1, End: sequence, Hash: sha256.Sum256([]byte{'x'})}
			buffer = append(buffer, encodeAppend(last, []byte{'x'})...)
			if len(buffer) > 63<<10 {
				if _, err := pane.file.Write(buffer); err != nil {
					t.Fatal(err)
				}
				buffer = buffer[:0]
			}
		}
		buffer = append(buffer, encodeCommit(last)...)
		if _, err := pane.file.Write(buffer); err != nil {
			t.Fatal(err)
		}
		records += count
	}
	if err := realm.Close(); err != nil {
		t.Fatal(err)
	}
	realm = nil
	runtime.GC()
	var before, held runtime.MemStats
	runtime.ReadMemStats(&before)
	recovered, err := OpenRealm(options)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	runtime.GC()
	runtime.ReadMemStats(&held)
	var decoded int64
	for _, pane := range recovered.panes {
		decoded += pane.verified.committedSequence
	}
	physical, _, capacity := recovered.PhysicalBudget()
	if physical < int64(densePanes*24-1)<<20 || decoded != records {
		t.Fatal("recovery did not reach declared dense shape", physical, decoded, records)
	}
	ceiling := (physical*12+4)/5 + 2<<20
	if int64(held.HeapAlloc)-int64(before.HeapAlloc) > ceiling {
		t.Fatal("recovery exceeded rounded owner inequality")
	}
	result, _ := json.Marshal(map[string]any{"shape": "dense_multiappend_recovery", "records": records, "physical": physical, "capacity": capacity, "held_heap_delta": int64(held.HeapAlloc) - int64(before.HeapAlloc), "heap_inuse": held.HeapInuse, "runtime_sys": held.Sys, "go_accounted": held.Sys - held.HeapReleased, "projection_ceiling": ceiling})
	t.Log(string(result))
	runtime.KeepAlive(recovered)
}
