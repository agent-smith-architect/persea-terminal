package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

// This is an isolated allocation experiment, not the workload/admission lane.
// It holds real append/readback, pending input, publication, snapshot, decoder
// and response owners together. Manual feed time intentionally keeps the last
// partial batch of each large input live while earlier copies fill the outbox.
func TestRecordingCombinedOwnershipCalibration(t *testing.T) {
	if os.Getenv("PERSEA_RECORDING_COMBINED") != "1" {
		t.Skip("isolated combined ownership calibration")
	}
	for round := 0; round < 3; round++ {
		t.Run(strconv.Itoa(round), recordingCombinedOwnership)
	}
}

func recordingCombinedOwnership(t *testing.T) {
	recordingCombinedOwnershipShape(t, "")
}

// Legacy recovered generations are retained in their own realm partition;
// only newly admitted current generations supply unified reader snapshots.
func TestRecordingMixedRecoveryOwnershipCalibration(t *testing.T) {
	fixture := os.Getenv("PERSEA_RECORDING_MIXED_FIXTURE")
	if os.Getenv("PERSEA_RECORDING_COMBINED") != "1" || fixture == "" {
		t.Skip("isolated reachable mixed recovery ownership calibration")
	}
	for round := 0; round < 3; round++ {
		t.Run(strconv.Itoa(round), func(t *testing.T) { recordingCombinedOwnershipShape(t, fixture) })
	}
}

func recordingCombinedOwnershipShape(t *testing.T, recoveredFixture string) {
	d := newDisposable(t)
	server := config.TmuxServer{Label: "main", SocketPath: d.path}
	cfg := unifiedAdoptionDevConfig(t, server, t.TempDir(), 9)
	if recoveredFixture != "" {
		cfg.Realm, cfg.UnifiedTerminalDev.RuntimeDir = "realm-a", recoveredFixture
	}
	oldCaps := unifiedJournalCaps
	unifiedJournalCaps.panePhysical, unifiedJournalCaps.realmPhysical = 24<<20, 72<<20
	effects, err := NewUnifiedDevPaneEffects(cfg)
	unifiedJournalCaps = oldCaps
	if err != nil {
		t.Fatal(err)
	}
	clock := newRetentionManualClock()
	options := effects.retentionTrial()
	options.now, options.after = clock.Now, clock.After
	// Filesystem fixtures supply complete synthetic host facts to the real
	// source admission checks. The production lane separately reads real tmux
	// host witnesses and runs the unchanged workload.
	var holding atomic.Bool
	var holdManager atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	managerEntered, managerRelease := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	var managerOnce sync.Once
	options.hook = func(point string, _ unifiedjournal.PaneKey) {
		if point == "before_dispatch_fence_publish" && holdManager.Load() {
			managerOnce.Do(func() { close(managerEntered) })
			<-managerRelease
		}
	}
	options.observe = func(string, map[string]int64) {
		if holding.Load() {
			once.Do(func() { close(entered) })
			<-release
		}
	}
	provider := &recordingProfileEffects{recordingMemoryEffects: &recordingMemoryEffects{UnifiedDevPaneEffects: effects, options: options}}
	registry := newPaneRegistry(provider)
	effects.observer = registry
	trial := registry.retention
	defer func() {
		releaseOnce.Do(func() { close(managerRelease); close(release) })
		_ = registry.Close()
		_ = effects.realm.Close()
	}()
	sources, writingSources, inputSize := 8, 8, (8<<20)-1024
	if recoveredFixture != "" {
		sources, writingSources, inputSize = 6, 3, (8<<20)-(64<<10)-1
		if len(effects.realm.Recovered()) < 2 {
			t.Fatal("missing retained dense recovery partition")
		}
		for _, recovered := range effects.realm.Recovered() {
			if recovered.Key.Server != "server-a" {
				continue // prior current generations are retired by startup inventory
			}
			// Model completed source attribution without granting legacy replay
			// authority. Unknown owners correctly blocked every new source in
			// the first regression run of this mixed-state fixture.
			id := unifiedjournal.SourceID{Realm: cfg.Realm, Server: recovered.Key.Server, Session: recovered.Key.Session, Window: recovered.Key.Window, Pane: recovered.Key.Pane, SocketPath: "/synthetic/dense-source", SocketDevice: 1, SocketInode: 1, ServerPID: 1, ServerStartTime: 1, PanePID: 2, PaneStartTime: 2}
			if err := effects.realm.AttributeRecoveredSource(recovered.Key, id); err != nil {
				t.Fatal(err)
			}
			if effects.realm.UnifiedEligible(recovered.Key) {
				t.Fatal("source attribution granted legacy replay")
			}
		}
	}
	oldInput := 0
	if recoveredFixture != "" {
		holding.Store(true)
		trial.publishObservation("calibration_hold", map[string]int64{"held": 1}, nil)
		<-entered
		// A failed post-commit rotation can unlink both old and successor
		// journals while old feed callbacks still own their copies. Exercise
		// those exact transactions before reusing their ordinary slots.
		for i := 0; i < 3; i++ {
			oldInput += recordingCalibrateRetiredFeed(t, effects, registry, i)
		}
	}
	keys := make([]unifiedjournal.PaneKey, sources)
	for i := range keys {
		witness := retentionWitness("%"+strconv.Itoa(i+1), "combined-"+strconv.Itoa(i))
		witness.Session.Server, witness.Session.Session = "main", "$"+strconv.Itoa(i+1)
		key := journalKey(witness)
		reserveRecordingSourceForTest(t, effects, key)
		if err := effects.realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
			t.Fatal(err)
		}
		if err := registry.AdmitPane(witness); err != nil {
			t.Fatal(err)
		}
		recordingCalibrationInitial(t, registry, witness)
		effects.active[key.Session] = key
		keys[i] = key
	}
	runtime.GC()
	recordingMemorySample(t, "warmed")
	if recoveredFixture == "" {
		holding.Store(true)
		trial.publishObservation("calibration_hold", map[string]int64{"held": 1}, nil)
		<-entered
	}
	totalInput := oldInput + writingSources*inputSize
	for _, key := range keys[:writingSources] {
		if err := trial.WritePane(key, bytes.Repeat([]byte{'p'}, inputSize)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for recoveredFixture == "" {
		trial.mu.Lock()
		done := trial.qUsed == 2*sources
		used := trial.bUsed
		trial.mu.Unlock()
		if done {
			if used != int64(totalInput) {
				t.Fatal("payload credit did not reach target", used)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("manager did not reach held partial batches")
		}
		time.Sleep(time.Millisecond)
	}
	if recoveredFixture != "" {
		// Pending inputs for the other three current sources consume their
		// source credits before the manager can append into the almost-full
		// physical partition. Already published feeds retain their credits.
		holdManager.Store(true)
		if _, err := trial.startDispatchFence(); err != nil {
			t.Fatal(err)
		}
		<-managerEntered
		for _, key := range keys[writingSources:] {
			size := min(8<<20, retentionDefaultIngressLimit-totalInput)
			if err := trial.WritePane(key, bytes.Repeat([]byte{'q'}, size)); err != nil {
				t.Fatal(err)
			}
			totalInput += size
		}
		if trial.bUsed != retentionDefaultIngressLimit {
			t.Fatal("old feeds and new source owners did not fill B", trial.bUsed)
		}
		// Held bytes do not exclude content-free control reservations. Fill
		// the remaining E capacity through actual ordinary boundary commands,
		// alternating their coalesced control runs, while the manager is held.
		// This allocates command objects and result/fence channels, not merely
		// the already-present ring's pointers.
		controls := 0
		for {
			advanced := false
			for _, key := range keys {
				if _, err := trial.startBoundary(key, "calibration_pending", false); err != nil {
					if errors.Is(err, unifiedjournal.ErrSourceQuota) || errors.Is(err, unifiedjournal.ErrInvalidated) {
						continue
					}
					t.Fatal(err)
				}
				if _, err := trial.startDispatchFence(); err != nil {
					t.Fatal(err)
				}
				controls++
				advanced = true
			}
			if !advanced {
				break
			}
		}
		if trial.eUsed+9 <= retentionDefaultEnvelopeLimit || controls == 0 {
			t.Fatal("remaining metadata capacity did not saturate", controls, trial.eUsed)
		}
		t.Logf("queued_control_reservations=%d Q=%d E=%d ring_cells=%d", controls, trial.qUsed, trial.eUsed, len(trial.queue.cells))
	}
	fields := map[string]int64{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6, "g": 7, "h": 8}
	for len(trial.outbox) < cap(trial.outbox) {
		trial.publishObservation("calibration_full", fields, nil)
	}
	if cap(trial.outbox) != retentionDefaultEnvelopeLimit+retentionDefaultPaneLimit+2 || len(trial.outbox) != cap(trial.outbox) {
		t.Fatal("outbox capacity mismatch")
	}
	var snapshots [][]unifiedjournal.Event
	var tails []*unifiedDevSubscriber
	var stops []func()
	defer func() {
		for i := range tails {
			snapshots[i] = nil
			stops[i]()
		}
	}()
	// Four duplicated sources and four single readers leave one single-source
	// tail for exact remaining-capacity saturation without fanout artifacts.
	readerIndexes := []int{0, 1, 2, 3, 0, 1, 2, 3, 4, 5, 6, 7}
	if recoveredFixture != "" {
		readerIndexes = []int{0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 2, 3, 4, 3, 5}
	}
	for _, index := range readerIndexes {
		if os.Getenv("PERSEA_RECORDING_PAD_READER_SCRATCH") == "1" {
			events, _, tail, cancel, err := effects.openSnapshotTail(keys[index].Session)
			if err != nil {
				t.Fatal(err)
			}
			snapshots, tails = append(snapshots, events), append(tails, tail)
			stops = append(stops, func() { tail.releaseSnapshot(); cancel() })
			continue
		}
		writer, stop := recordingCalibratePrepare(t, effects, keys[index].Session)
		// The parked WriteFrame owns the replay and serialized bytes. Its
		// backlog borrows the already-funded snapshot index and payload slab.
		snapshots, tails, stops = append(snapshots, writer.backlog), append(tails, writer.tail), append(stops, stop)
	}
	last := tails[len(tails)-1]
	sequence := last.cursor
	for {
		room := int64(recordingReaderBytes) - effects.readers.snapshot().Bytes
		if room == 0 {
			break
		}
		if room < recordingTailNodeBytes {
			t.Fatal("remaining capacity is below the minimum allocation", room)
		}
		sequence++
		size := min(room-recordingTailNodeBytes, 64<<10)
		for unifiedjournal.AllocationCharge(size)+recordingTailNodeBytes > room {
			size /= 2
		}
		size = unifiedjournal.AllocationCharge(size)
		if err := effects.publishEvent(keys[sources-1], unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Sequence: sequence, Payload: bytes.Repeat([]byte{'t'}, int(size))}); err != nil {
			t.Fatal(err)
		}
		if last.closeReason() != "" && effects.readers.snapshot().Bytes < recordingReaderBytes {
			t.Fatal("tail filled before byte ceiling")
		}
	}
	logical, _, _ := effects.realm.LogicalBudget()
	physical, reserved, physicalCap := effects.realm.PhysicalBudget()
	t.Logf("owners outbox=%d ingress=%d reader=%d logical=%d physical=%d physical_reserved=%d physical_cap=%d tmpfs_ceiling=%d recovered=%t", len(trial.outbox), totalInput, effects.readers.snapshot().Bytes, logical, physical, reserved, physicalCap, 96<<20, recoveredFixture != "")
	recordingCalibrateCopyChurn(t, effects, snapshots)
}

type recordingPrepareWire struct {
	entered chan struct{}
	release chan struct{}
	bytes   int
	backing int
	replay  int
}

func (wire *recordingPrepareWire) Write(data []byte) (int, error) {
	if len(data) > 5 && data[0] == '{' {
		wire.bytes, wire.backing = len(data), cap(data)
		marker := []byte(`"replay":"`)
		if start := bytes.Index(data, marker); start >= 0 {
			start += len(marker)
			if end := bytes.IndexByte(data[start:], '"'); end >= 0 {
				wire.replay = end / 4 * 3
				for i := start + end - 1; i >= start && data[i] == '='; i-- {
					wire.replay--
				}
			}
		}
		close(wire.entered)
		<-wire.release
	}
	return len(data), nil
}

// Park the real PREPARE at the final wire write. The framing layer passes the
// encoder's payload directly after a five-byte header; this sink makes no
// payload copy. Encoder temporaries and pool storage remain subject to their
// actual Go lifetimes and are included in the process/GC measurements.
func recordingCalibratePrepare(t *testing.T, effects *UnifiedDevPaneEffects, session string) (*unifiedAttachmentFrameWriter, func()) {
	t.Helper()
	wire := &recordingPrepareWire{entered: make(chan struct{}), release: make(chan struct{})}
	writer := &unifiedAttachmentFrameWriter{provider: effects, session: session, downstream: &attachmentFrameWriter{wire: &lockedWriter{w: wire}}}
	frame := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FramePrepare, Source: "calibration", Epoch: 1, Cut: 1, Kind: terminal.CutInitial, Columns: 80, Rows: 24}
	raw := unifiedE2E1RawFrame(t, frame)
	done := make(chan error, 1)
	go func() { done <- writer.WriteFrame(context.Background(), raw) }()
	select {
	case <-wire.entered:
	case err := <-done:
		t.Fatal("PREPARE did not reach its wire write", err)
	case <-time.After(10 * time.Second):
		t.Fatal("PREPARE did not reach its wire write")
	}
	t.Logf("prepare session=%s replay_bytes=%d encoded_bytes=%d encoded_capacity=%d framing_payload_copies=0 snapshot_backlog_events=%d", session, wire.replay, wire.bytes, wire.backing, len(writer.backlog))
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(wire.release)
			if err := <-done; err != nil {
				t.Error("PREPARE failed after release", err)
			}
			if err := writer.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	return writer, stop
}

// Initial readiness is the typed committed receipt, independent of a held
// downstream callback. Unlike the general fixture helper, this one deliberately
// does not wait for a dispatch fence after that receipt.
func recordingCalibrationInitial(t *testing.T, registry *paneRegistry, witness controlmode.PaneWitness) {
	t.Helper()
	op := startRecordingInitialForTest(t, registry, witness, nil)
	<-op.done
	if err := registry.publishInitial(op); err != nil {
		t.Fatal(err)
	}
}

func recordingCalibrateRetiredFeed(t *testing.T, effects *UnifiedDevPaneEffects, registry *paneRegistry, index int) int {
	t.Helper()
	witness := retentionWitness("%old"+strconv.Itoa(index), "combined-old-"+strconv.Itoa(index))
	witness.Session.Server, witness.Session.Session = "main", "$old"+strconv.Itoa(index)
	key := journalKey(witness)
	reserveRecordingSourceForTest(t, effects, key)
	if err := effects.realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	recordingCalibrationInitial(t, registry, witness)
	memory, err := effects.transients.acquire()
	if err != nil {
		t.Fatal(err)
	}
	unit := &unifiedDevUnit{owner: effects, memory: memory, sessionID: witness.Session.Session, done: make(chan struct{}), witnesses: []controlmode.PaneWitness{witness}}
	defer unit.reapOnce.Do(memory.done)
	effects.units[unit.sessionID], effects.active[unit.sessionID] = unit, key
	const size = 6 << 20 // leave the real 1MiB predecessor rollback reserve
	if err := registry.retention.WritePane(key, bytes.Repeat([]byte{'o'}, size)); err != nil {
		t.Fatal(err)
	}
	boundary, err := registry.retention.startBoundary(key, "calibration_old", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-boundary; err != nil {
		t.Fatal(err)
	}
	capacity, err := effects.realm.BeginRotationCapacity(key)
	if err != nil {
		t.Fatal("real rotation capacity refused the old-feed shape", err)
	}
	next := witness
	next.Session.ControlGeneration++
	nextKey := journalKey(next)
	reserveRecordingSourceForTest(t, effects, nextKey)
	journal, err := effects.realm.BeginRotatedPane(nextKey, unifiedjournal.Geometry{Columns: 80, Rows: 24}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	txn, err := registry.BeginPaneRotation(witness, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := txn.WriteBootstrap(nil); err != nil {
		t.Fatal(err)
	}
	<-txn.initial.done
	if err := txn.Validate(); err != nil {
		t.Fatal(err)
	}
	seal, err := registry.retention.startBoundary(key, "rotation_seal", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-seal; err != nil {
		t.Fatal(err)
	}
	txn.markSealed()
	if disposition := txn.Commit(); disposition != paneRotationCommitted {
		t.Fatal("real registry commit refused the old-feed shape", disposition)
	}
	journal.Commit()
	rotation := &unifiedDevRotation{unit: unit, session: unit.sessionID, registry: registry, old: witness, next: next, oldKey: key, newKey: nextKey, journal: journal, capacity: capacity, ponr: true, registryCommitted: true, journalCommitted: true}
	if err := rotation.settleFatal(errors.New("calibration failure after journal commit")); !errors.Is(err, ErrUnifiedRotateFatal) {
		t.Fatal(err)
	}
	if _, err := effects.realm.Origin(key); !errors.Is(err, unifiedjournal.ErrInvalidated) {
		t.Fatal("old journal was not unlinked", err)
	}
	if _, err := effects.realm.Origin(nextKey); !errors.Is(err, unifiedjournal.ErrInvalidated) {
		t.Fatal("failed successor was not unlinked", err)
	}
	registry.retention.mu.Lock()
	generation := registry.retention.generations[key]
	held := generation != nil && generation.refs > 0 && generation.source != nil && generation.source.usage.ingress == size
	registry.retention.mu.Unlock()
	if !held {
		t.Fatal("journal cleanup refunded old feed ownership")
	}
	return size
}

func recordingCalibrateCopyChurn(t *testing.T, effects *UnifiedDevPaneEffects, snapshots [][]unifiedjournal.Event) {
	var owners []*recordingUnitMemory
	var fixed [][]byte
	// Snapshot admission also retains PREPARE/writer scratch. These readers
	// deliberately hold their copied snapshots without running a writer, so
	// materialize the complete already-charged scratch allowance. This is a
	// conservative allocation envelope, not a claim that every byte of an
	// encoder's cumulative allocation remains live simultaneously.
	var readerFixed []byte
	if os.Getenv("PERSEA_RECORDING_PAD_READER_SCRATCH") == "1" {
		readerFixed = make([]byte, effects.readers.snapshot().Readers*(recordingWriterBytes+recordingReaderFloor+recordingPrepareBytes))
	}
	for offset := 0; offset < len(readerFixed); offset += 4096 {
		readerFixed[offset] = 1
	}
	for i := 0; i < recordingObserverUnitLimit; i++ {
		owner, err := effects.transients.acquire()
		if err != nil {
			t.Fatal(err)
		}
		owners = append(owners, owner)
		// The isolated owner has no live read loop. Materialize its entire
		// charged fixed allowance as touched backing so the measured peak
		// includes that conservative owner term as well as dynamic copies.
		backing := make([]byte, recordingUnitBytes)
		for offset := 0; offset < len(backing); offset += 4096 {
			backing[offset] = 1
		}
		fixed = append(fixed, backing)
	}
	budgeted := controlmode.NewBudgetedDecoder(owners[0])
	var batch controlmode.EventBatch
	chunk := bytes.Repeat([]byte{'x'}, 64<<10)
	prefix := []byte("%output %1 ")
	if _, err := batch.Feed(budgeted, prefix); err != nil {
		t.Fatal(err)
	}
	for remaining := (16 << 20) - len(prefix); remaining > 0; {
		n := min(remaining, len(chunk))
		if _, err := batch.Feed(budgeted, chunk[:n]); err != nil {
			t.Fatal(err)
		}
		remaining -= n
	}
	if _, err := batch.Feed(budgeted, []byte{'\n'}); err != nil {
		t.Fatal(err)
	}
	response := recordingResponse{owner: owners[1]}
	for response.Write(chunk) == nil {
	}
	defer func() {
		response.release()
		batch.Release()
		budgeted.Release()
		for _, owner := range owners {
			owner.done()
		}
	}()
	shared := effects.readers.copies
	shared.mu.Lock()
	copies := shared.bytes
	shared.mu.Unlock()
	if recordingCopyBytes-copies > 1<<20 {
		t.Fatal("combined copy ceiling not reached", copies)
	}

	recordingMemorySample(t, "simultaneous_allocated")
	runtime.GC()
	recordingMemorySample(t, "simultaneous_live")
	// Repeated decoder/capture allocations under unchanged held maxima reveal
	// GC/native plateaus and limiter behavior; no budget is refunded by GC.
	started := time.Now()
	var churnBytes int64
	for i := 0; i < 100; i++ {
		response.release()
		for response.Write(chunk) == nil {
			churnBytes += int64(len(chunk))
		}
		if i%10 == 0 {
			recordingMemorySample(t, "churn")
		}
	}
	t.Logf("churn_bytes=%d churn_elapsed_ns=%d", churnBytes, time.Since(started).Nanoseconds())
	recordingMemorySample(t, "churn_end")
	runtime.KeepAlive(snapshots)
	runtime.KeepAlive(batch)
	runtime.KeepAlive(response)
	runtime.KeepAlive(fixed)
	runtime.KeepAlive(readerFixed)
}

func recordingMemorySample(t *testing.T, stage string) {
	t.Helper()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	values := map[string]any{"stage": stage, "go": runtime.Version(), "heap_alloc": m.HeapAlloc, "heap_inuse": m.HeapInuse, "heap_sys": m.HeapSys, "heap_released": m.HeapReleased, "stack_inuse": m.StackInuse, "runtime_sys": m.Sys, "go_accounted": m.Sys - m.HeapReleased, "gc_count": m.NumGC, "gc_pause_ns": m.PauseTotalNs, "gc_cpu_fraction": m.GCCPUFraction, "gomemlimit": os.Getenv("GOMEMLIMIT")}
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) == nil {
		values["process_cpu_ns"] = usage.Utime.Nano() + usage.Stime.Nano()
		values["process_maxrss"] = usage.Maxrss * 1024
	}
	if data, err := os.ReadFile("/proc/self/smaps_rollup"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					switch fields[0] {
					case "Rss:", "Pss:", "Private_Dirty:", "Private_Clean:", "Anonymous:":
						values["proc_"+strings.TrimSuffix(fields[0], ":")] = n * 1024
					}
				}
			}
		}
	}
	samples := []metrics.Sample{{Name: "/gc/limiter/last-enabled:gc-cycle"}, {Name: "/cpu/classes/gc/total:cpu-seconds"}, {Name: "/cpu/classes/total:cpu-seconds"}}
	metrics.Read(samples)
	for _, sample := range samples {
		switch sample.Value.Kind() {
		case metrics.KindUint64:
			values[sample.Name] = sample.Value.Uint64()
		case metrics.KindFloat64:
			values[sample.Name] = sample.Value.Float64()
		}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(fmt.Errorf("memory sample: %w", err))
	}
	t.Log(string(encoded))
}
