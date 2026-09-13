package unifiedjournal

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"testing"
	"unsafe"
)

// Real append/readback allocations at the declared physical/logical capacity.
// Sync is disabled only in this allocation experiment: ordinary fault and
// ordering suites separately exercise durable sync. No production file is used.
func TestRecordingMemoryCalibrationProjection(t *testing.T) {
	if os.Getenv("PERSEA_RECORDING_MEMORY_CALIBRATION") != "1" {
		t.Skip("isolated capacity calibration")
	}
	for _, shape := range []string{"tiny", "geometry", "pending"} {
		t.Run(shape, func(t *testing.T) {
			options := rotationOptions(t)
			options.PaneCapBytes = 8 << 20
			options.RealmCapBytes = 64 << 20
			options.PanePhysicalCapBytes = 24 << 20
			options.PhysicalCapBytes = 72 << 20
			options.CompletePaneSlots = 9 // eight measured sources plus rotation reserve
			realm, err := OpenRealm(options)
			if err != nil {
				t.Fatal(err)
			}
			defer realm.Close()
			realm.ops.sync = func(*os.File) error { return nil }
			runtime.GC()
			var before, held runtime.MemStats
			runtime.ReadMemStats(&before)
			var records int64
			for i := 0; i < 8; i++ {
				key := rotationKey("$memory", uint64(i+1))
				if err := realm.AdmitPane(key, Geometry{80, 24}); err != nil {
					if errors.Is(err, ErrQuota) {
						break
					}
					t.Fatal(err)
				}
				for {
					var record Record
					if shape == "geometry" {
						record, err = realm.AppendGeometry(key, Geometry{80, 24})
					} else {
						record, err = realm.Append(key, []byte{'x'})
					}
					if errors.Is(err, ErrQuota) {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					records++
					if shape != "pending" {
						if err = realm.AdvanceCommitted(key, record); err != nil {
							t.Fatal(err)
						}
					}
				}
				logical, _, _ := realm.LogicalBudget()
				physical, _, _ := realm.PhysicalBudget()
				if logical >= 64<<20-geometryRecordCost || physical >= 72<<20-1024 {
					break
				}
			}
			runtime.GC()
			runtime.ReadMemStats(&held)
			var projected, pending, payload int64
			for _, pane := range realm.panes {
				pending += pane.pendingCount * AllocationCharge(int64(unsafe.Sizeof(pendingRecord{}))+8)
				for event := pane.verified.view; event != nil; event = event.previous {
					projected += AllocationCharge(int64(unsafe.Sizeof(eventPage{})) + 8)
					for page := event.payload; page != nil; page = page.previous {
						projected += AllocationCharge(int64(unsafe.Sizeof(payloadPage{}))+8) + AllocationCharge(int64(cap(page.data)))
						payload += int64(cap(page.data))
					}
				}
			}
			logical, _, _ := realm.LogicalBudget()
			physical, _, _ := realm.PhysicalBudget()
			result := map[string]any{"shape": shape, "records": records, "logical": logical, "physical": physical, "identities": len(realm.panes), "payload_capacity": payload, "projected_charge": projected, "pending_charge": pending, "held_heap_delta": int64(held.HeapAlloc) - int64(before.HeapAlloc), "total_alloc_delta": held.TotalAlloc - before.TotalAlloc, "heap_inuse": held.HeapInuse, "runtime_sys": held.Sys, "gc_count": held.NumGC - before.NumGC}
			encoded, _ := json.Marshal(result)
			t.Log(string(encoded))
			if shape == "geometry" {
				if logical < 63<<20 {
					t.Fatal("geometry did not reach declared logical capacity")
				}
			} else if physical < 71<<20 {
				t.Fatal("shape did not reach declared physical capacity")
			}
			if int64(held.HeapAlloc)-int64(before.HeapAlloc) > projected+pending+2<<20 {
				t.Fatal("projection allocation exceeds conservative charge")
			}
		})
	}
}
