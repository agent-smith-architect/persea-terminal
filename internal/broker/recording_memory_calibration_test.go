package broker

import (
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
	"unsafe"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

type recordingMemoryEffects struct {
	*UnifiedDevPaneEffects
	options retentionTrialOptions
}

func (effects *recordingMemoryEffects) retentionTrial() retentionTrialOptions          { return effects.options }
func (effects *recordingMemoryEffects) WritePane(unifiedjournal.PaneKey, []byte) error { return nil }

// Run alone in a fresh test process. It reaches the declared outbox capacity
// with the actual publication allocator and holds its real dispatcher. It
// measures ownership allocation; it is not a journal or workload throughput
// qualification and does not claim every owner maximum can coexist.
func TestRecordingMemoryCalibrationOutbox(t *testing.T) {
	if os.Getenv("PERSEA_RECORDING_MEMORY_CALIBRATION") != "1" {
		t.Skip("isolated capacity calibration")
	}
	for _, capacity := range []int{262144, retentionDefaultEnvelopeLimit} {
		t.Run(stringSize(capacity), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			clock := newRetentionManualClock()
			effects := &recordingMemoryEffects{UnifiedDevPaneEffects: &UnifiedDevPaneEffects{}, options: retentionTrialOptions{
				realm: openRetentionRealm(t, "outbox-memory"), maxBatchBytes: 64 << 10, maxFeedDelay: 16 * time.Millisecond,
				now: clock.Now, after: clock.After, maxCommands: 32768, maxEnvelopes: capacity,
				observe: func(string, map[string]int64) { once.Do(func() { close(entered); <-release }) },
			}}
			runtime.GC()
			var before, allocated, held runtime.MemStats
			runtime.ReadMemStats(&before)
			trial := newRetentionTrialRuntime(effects)
			trial.publishObservation("held", map[string]int64{"one": 1}, nil)
			<-entered
			fields := map[string]int64{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8}
			for len(trial.outbox) < cap(trial.outbox) {
				trial.publishObservation("calibration", fields, nil)
			}
			if len(trial.outbox) != cap(trial.outbox) {
				t.Fatal("calibration failed to reach declared outbox capacity")
			}
			runtime.ReadMemStats(&allocated)
			runtime.GC()
			runtime.ReadMemStats(&held)
			result := map[string]any{"owner": "full_outbox", "envelopes": cap(trial.outbox), "held_heap_delta": int64(held.HeapAlloc) - int64(before.HeapAlloc), "total_alloc_delta": allocated.TotalAlloc - before.TotalAlloc,
				"heap_inuse": held.HeapInuse, "runtime_sys": held.Sys, "gc_count": held.NumGC - before.NumGC,
				"envelope_struct": unsafe.Sizeof(retentionEnvelope{}), "feed_struct": unsafe.Sizeof(retentionFeed{}), "field_struct": unsafe.Sizeof(retentionField{}), "reservation_struct": unsafe.Sizeof(retentionReservation{}), "command_struct": unsafe.Sizeof(retentionCommand{}),
				"conservative_diagnostic_envelope_bytes": 320, "ring_cells": len(trial.queue.cells)}
			encoded, _ := json.Marshal(result)
			t.Log(string(encoded))
			if held.HeapAlloc-before.HeapAlloc > uint64(cap(trial.outbox))*320+8<<20 {
				t.Error("allocation exceeds conservative envelope worksheet")
			}
			close(release)
			if err := trial.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func stringSize(size int) string {
	if size == 262144 {
		return "baseline262144"
	}
	return "candidate" + strconv.Itoa(size)
}

func TestRecordingMemoryCalibrationEncoding(t *testing.T) {
	if os.Getenv("PERSEA_RECORDING_MEMORY_CALIBRATION") != "1" {
		t.Skip("isolated capacity calibration")
	}
	for _, size := range []int{64 << 10, 256 << 10} {
		frame := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameLive, Source: "calibration", Epoch: 1, Cut: 1, Data: make([]byte, size)}
		limit := uint64(recordingWriterBytes)
		if size == 256<<10 {
			frame.Type = terminal.FramePrepare
			frame.Kind = terminal.CutInitial
			frame.Columns = 80
			frame.Rows = 24
			frame.Replay = frame.Data
			frame.Data = nil
			frame.History = []string{}
			limit = recordingPrepareBytes
		}
		for i := 0; i < 10; i++ {
			if _, err := attachmentwire.Encode(frame, attachmentwire.ServerToBrowser); err != nil {
				t.Fatal(err)
			}
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		const repeats = 100
		var encoded []byte
		for i := 0; i < repeats; i++ {
			var err error
			encoded, err = attachmentwire.Encode(frame, attachmentwire.ServerToBrowser)
			if err != nil {
				t.Fatal(err)
			}
		}
		runtime.ReadMemStats(&after)
		allocated := (after.TotalAlloc - before.TotalAlloc) / repeats
		t.Logf("owner=encoding payload=%d wire=%d total_alloc_per_call=%d reserved=%d", size, len(encoded), allocated, limit)
		if allocated > limit {
			t.Fatal("serializer reserve smaller than total allocation per call")
		}
		runtime.KeepAlive(encoded)
	}
}
