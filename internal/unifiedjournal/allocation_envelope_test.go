package unifiedjournal

import (
	"runtime"
	"testing"
	"unsafe"
)

// This check uses exported allocator observations, never imports runtime
// internals. Above the largest exported small class, an8KiB page ceiling is
// conservative on the pinned linux/amd64 toolchain. A toolchain/shape change
// must requalify this ownership worksheet rather than silently reusing it.
func TestRecordingProjectionAllocationEnvelope(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("linux/amd64 ownership qualification")
	}
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	allocation := func(n int64) int64 {
		if n == 0 {
			return 0
		}
		for _, class := range stats.BySize {
			if int64(class.Size) >= n {
				return int64(class.Size)
			}
		}
		return (n + 8191) &^ 8191
	}
	event := allocation(int64(unsafe.Sizeof(eventPage{})))
	page := allocation(int64(unsafe.Sizeof(payloadPage{})))
	pending := allocation(int64(unsafe.Sizeof(pendingRecord{})))
	if event > 112 || page > 48 || pending > 176 || projectionPayloadPageBytes != 32<<10 {
		t.Fatalf("worksheet shape changed: event=%d page=%d pending=%d payload_page=%d", event, page, pending, projectionPayloadPageBytes)
	}
	var worstSize, worstAllocation int64
	worstRatio := float64(0)
	// Every full page satisfies the same4/3 inequality independently. This
	// exhaustive remainder check therefore also covers records larger than a
	// page, arbitrary payload mixtures, tiny records and geometry metadata.
	for n := int64(0); n <= projectionPayloadPageBytes; n++ {
		owned := event
		if n > 0 {
			owned += page + allocation(n)
		}
		physical := geometryRecordCost + n
		if 3*owned > 4*physical {
			t.Fatalf("projection bound failed n=%d owned=%d physical=%d", n, owned, physical)
		}
		// Recovery also accepts many appends with one final commit. Count only
		// the bytes that actually exist, not future per-record commit charges.
		if 5*owned > 12*(appendFixed+n) {
			t.Fatalf("recovery bound failed n=%d owned=%d", n, owned)
		}
		if ratio := float64(owned) / float64(physical); ratio > worstRatio {
			worstRatio, worstSize, worstAllocation = ratio, n, owned
		}
	}
	if 3*(page+allocation(projectionPayloadPageBytes)) > 4*projectionPayloadPageBytes {
		t.Fatal("full-page induction failed")
	}
	t.Logf("go=%s event=%d page=%d pending=%d worst_remainder=%d allocation=%d ratio=%.8f ordinary_projection_72MiB_ceiling=%d recovery_projection_72MiB_ceiling=%d production_pending_64_ceiling=%d", runtime.Version(), event, page, pending, worstSize, worstAllocation, worstRatio, 96<<20, ((72<<20)*12+4)/5, 64*pending)
}
