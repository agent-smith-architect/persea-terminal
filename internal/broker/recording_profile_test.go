package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/unifiedjournal"
)

type recordingProfileLane struct {
	ID       string                     `json:"id"`
	Sources  int                        `json:"sources"`
	Rate     int64                      `json:"events_per_second_per_source"`
	Phase    int64                      `json:"source_phase_ns"`
	Size     int                        `json:"payload_bytes"`
	ANSI     bool                       `json:"ansi"`
	Segments []recordingWorkloadSegment `json:"segments"`
}

type recordingProfileManifest struct {
	Seed      int `json:"seed"`
	Execution struct {
		Duration int64 `json:"quick_duration_ns"`
	} `json:"execution"`
	Lanes []recordingProfileLane `json:"lanes"`
}

func recordingProfile(t *testing.T, path, id string) (recordingProfileManifest, recordingProfileLane) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sum := fmt.Sprintf("%x", sha256.Sum256(data)); sum != "d0d4f3551c63289538127de42a4c6edff7e3c76fc010168275115ca8e5bd4000" {
		t.Fatal("workload changed", sum)
	}
	var manifest recordingProfileManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, lane := range manifest.Lanes {
		if lane.ID == id {
			return manifest, lane
		}
	}
	t.Fatal("missing workload lane", id)
	return manifest, recordingProfileLane{}
}

func recordingProfilePayload(seed, source, index int, lane recordingProfileLane) []byte {
	data := make([]byte, lane.Size)
	for i := range data {
		data[i] = byte(32 + (seed+source*17+index*31+i*13)%95)
	}
	if lane.ANSI && index%4 == 0 {
		copy(data, []byte{27, '[', '2', 'K', '\r'})
	}
	copy(data[len(data)-2:], []byte{'\r', '\n'})
	return data
}

func TestRecordingWorkloadPinsTailBudget(t *testing.T) {
	data, err := os.ReadFile("testdata/recording-workload-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Lanes  []recordingProfileLane `json:"lanes"`
		Limits map[string]int64       `json:"current_limits"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Lanes) == 0 {
		t.Fatal("workload has no lanes")
	}
	recordingProfile(t, "testdata/recording-workload-v1.json", manifest.Lanes[0].ID)
	for name, want := range map[string]int64{
		"subscriber_tail_bytes":                  recordingTailBytes,
		"subscriber_node_bytes":                  recordingTailNodeBytes,
		"subscriber_maximum_zero_payload_events": recordingTailBytes / recordingTailNodeBytes,
		"subscriber_writer_bytes":                recordingWriterBytes,
		"subscriber_first_event_bytes":           recordingReaderFloor,
	} {
		if got := manifest.Limits[name]; got != want {
			t.Errorf("%s=%d want %d", name, got, want)
		}
	}
	if _, exists := manifest.Limits["subscriber_event_slots"]; exists {
		t.Fatal("workload retains an independent event-count bound")
	}
}

func recordingProfileSchedule(manifest recordingProfileManifest, lane recordingProfileLane, source int, emit func(int, int64, []byte)) int {
	segments := lane.Segments
	if len(segments) == 0 {
		segments = []recordingWorkloadSegment{{0, manifest.Execution.Duration, lane.Rate, lane.Phase}}
	}
	index := 0
	for _, segment := range segments {
		for at := segment.StartNS + int64(source)*segment.PhaseNS; at < segment.EndNS; at += int64(time.Second) / segment.Rate {
			emit(index, at, recordingProfilePayload(manifest.Seed, source, index, lane))
			index++
		}
	}
	return index
}

// The child is the source process inside its private tmux pane. Each source
// writes the frozen manifest's bytes/times; the actual tmux decoder chooses
// event chunking. No per-feed result list pollutes the memory measurement.
func TestRecordingProfileSourceProcess(t *testing.T) {
	if os.Getenv("PERSEA_RECORDING_SOURCE") == "" {
		return
	}
	source, err := strconv.Atoi(os.Getenv("PERSEA_RECORDING_SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, lane := recordingProfile(t, os.Getenv("PERSEA_RECORDING_MANIFEST"), os.Getenv("PERSEA_RECORDING_LANE"))
	gate := os.Getenv("PERSEA_RECORDING_GATE")
	var start int64
	for {
		data, err := os.ReadFile(gate)
		if err == nil {
			start, err = strconv.ParseInt(string(data), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	maximumLate := time.Duration(0)
	count := recordingProfileSchedule(manifest, lane, source, func(index int, at int64, payload []byte) {
		target := time.Unix(0, start+at)
		time.Sleep(time.Until(target))
		maximumLate = max(maximumLate, time.Since(target))
		if _, err := os.Stdout.Write(payload); err != nil {
			t.Fatal(err)
		}
	})
	receipt, _ := json.Marshal(map[string]any{"source": source, "events": count, "max_source_late_ns": maximumLate.Nanoseconds()})
	if err := os.WriteFile(gate+".done-"+strconv.Itoa(source), receipt, 0600); err != nil {
		t.Fatal(err)
	}
	// Remain alive so terminal retirement cannot delete the qualified journal.
	time.Sleep(3 * time.Minute)
	os.Exit(0)
}

func TestRecordingProfileEightSourceProduction(t *testing.T) {
	if os.Getenv("PERSEA_RECORDING_PROFILE") != "1" {
		t.Skip("60-second real tmux capacity profile")
	}
	for _, id := range []string{"nominal_plain", "bounded_burst"} {
		t.Run(id, func(t *testing.T) { recordingProfileProduction(t, id) })
	}
}

func recordingProfileProduction(t *testing.T, id string) {
	manifestPath, _ := filepath.Abs("testdata/recording-workload-v1.json")
	manifest, lane := recordingProfile(t, manifestPath, id)
	disposable := newDisposable(t)
	server := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	cfg := unifiedAdoptionDevConfig(t, server, t.TempDir(), 0)
	// The disposable host filesystem is larger than the service's 96 MiB
	// tmpfs. Pin its derived 24/72 MiB physical ledgers without enlarging them.
	oldCaps := unifiedJournalCaps
	unifiedJournalCaps.panePhysical, unifiedJournalCaps.realmPhysical = 24<<20, 72<<20
	effects, err := NewUnifiedDevPaneEffects(cfg)
	unifiedJournalCaps = oldCaps
	if err != nil {
		t.Fatal(err)
	}
	options := effects.retentionTrial()
	if retentionDefaultCommandLimit != 32768 || retentionDefaultEnvelopeLimit != 163840 || recordingObserverUnitLimit != 18 {
		t.Fatal("production profile changed without qualification")
	}
	if _, _, capacity := effects.realm.PhysicalBudget(); capacity != 72<<20 {
		t.Fatal("unexpected production physical capacity", capacity)
	}
	wrapped := &recordingMemoryEffects{UnifiedDevPaneEffects: effects, options: options}
	// Forward the production feed; the allocation-only wrapper above normally
	// discards it, so use the dedicated forwarding wrapper here.
	provider := &recordingProfileEffects{recordingMemoryEffects: wrapped}
	var mu sync.Mutex
	seen := map[string]hash.Hash{}
	lengths := map[string]int64{}
	effects.observerDecoded = func(event controlmode.Event) {
		if event.Kind != controlmode.EventOutput {
			return
		}
		mu.Lock()
		if digest := seen[event.PaneID]; digest != nil {
			_, _ = digest.Write(event.Data)
			lengths[event.PaneID] += int64(len(event.Data))
		}
		mu.Unlock()
	}
	registry := newPaneRegistry(provider)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = effects.RunObserver(ctx, registry) }()
	t.Cleanup(func() { cancel(); _ = registry.Close() })
	fixture := &adoptionFixture{disposable: disposable, server: server, cfg: cfg, effects: effects, registry: registry, cancel: cancel}
	gate := filepath.Join(t.TempDir(), "start")
	binary, _ := os.Executable()
	sessions := make([]string, lane.Sources)
	keys := make([]unifiedjournal.PaneKey, lane.Sources)
	wantDigests := make([]string, lane.Sources)
	wantBytes := make([]int64, lane.Sources)
	readerCtx, stopReaders := context.WithCancel(ctx)
	var readers sync.WaitGroup
	readerErrors := make(chan error, lane.Sources)
	defer func() {
		stopReaders()
		readers.Wait()
		close(readerErrors)
		for err := range readerErrors {
			t.Error(err)
		}
	}()
	for source := 0; source < lane.Sources; source++ {
		command := "stty -echo -opost; exec env PERSEA_RECORDING_SOURCE=" + strconv.Itoa(source) + " PERSEA_RECORDING_MANIFEST=" + shellQuote(manifestPath) + " PERSEA_RECORDING_LANE=" + shellQuote(id) + " PERSEA_RECORDING_GATE=" + shellQuote(gate) + " " + shellQuote(binary) + " -test.run=^TestRecordingProfileSourceProcess$"
		session := fixture.startPaneCommand(t, "profile-"+strconv.Itoa(source), command)
		adopted, err := effects.AdoptSession(ctx, session)
		if err != nil {
			t.Fatalf("source %d admission: %v", source, err)
		}
		sessions[source], keys[source] = session, adopted.Key
		mu.Lock()
		seen[adopted.Key.Pane] = sha256.New()
		mu.Unlock()
		expected := sha256.New()
		count := recordingProfileSchedule(manifest, lane, source, func(_ int, _ int64, data []byte) { _, _ = expected.Write(data); wantBytes[source] += int64(len(data)) })
		wantDigests[source] = hex.EncodeToString(expected.Sum(nil))
		if id == "bounded_burst" && count != 2760 {
			t.Fatal("burst schedule changed", count)
		}
		readers.Add(1)
		go func(session string) {
			defer readers.Done()
			if err := drainRecordingProfileReader(readerCtx, effects, session); err != nil {
				readerErrors <- err
			}
		}(session)
	}
	if got := effects.realm.AvailableCompletePaneSlots(); got != 0 {
		t.Fatal("eight ordinary sources must leave only the reserved successor slot", got)
	}
	start := time.Now().Add(100 * time.Millisecond)
	if err := os.WriteFile(gate, []byte(strconv.FormatInt(start.UnixNano(), 10)), 0600); err != nil {
		t.Fatal(err)
	}
	rotationDone := make(chan error, 1)
	go func() {
		time.Sleep(time.Until(start.Add(2 * time.Second)))
		rotationDone <- effects.rotateSession(ctx, sessions[0])
	}()
	deadline := start.Add(time.Duration(manifest.Execution.Duration) + 10*time.Second)
	for {
		complete := true
		mu.Lock()
		for source, key := range keys {
			complete = complete && lengths[key.Pane] == wantBytes[source]
		}
		mu.Unlock()
		if complete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("source/decoder completion timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-rotationDone; err != nil {
		t.Fatal("overlapping rotation", err)
	}
	for source := range sessions {
		var receipt struct {
			Events      int   `json:"events"`
			MaximumLate int64 `json:"max_source_late_ns"`
		}
		data, err := os.ReadFile(gate + ".done-" + strconv.Itoa(source))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		if int64(receipt.Events*lane.Size) != wantBytes[source] {
			t.Fatal("source receipt differs from frozen workload")
		}
		t.Logf("source=%d events=%d max_source_late_ns=%d", source, receipt.Events, receipt.MaximumLate)
	}
	for _, session := range sessions {
		key, _ := effects.paneKey(session)
		done, err := registry.retention.startBoundary(key, "profile_drain", false)
		if err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	fence, err := registry.retention.startDispatchFence()
	if err != nil {
		t.Fatal(err)
	}
	<-fence
	mu.Lock()
	for source, key := range keys {
		if got := hex.EncodeToString(seen[key.Pane].Sum(nil)); got != wantDigests[source] {
			t.Errorf("source%d decoded hash=%s want=%s bytes=%d", source, got, wantDigests[source], lengths[key.Pane])
		}
	}
	mu.Unlock()
	for source, session := range sessions {
		key, ok := effects.paneKey(session)
		if !ok || !effects.recordingReady(key) {
			t.Errorf("source%d lost readiness", source)
		}
		if err := effects.rotateSession(ctx, session); err != nil {
			t.Errorf("source%d final rotation: %v", source, err)
		}
	}
	charged, reserved, capacity := effects.realm.PhysicalBudget()
	if reserved != 0 || capacity != 72<<20 || charged > capacity {
		t.Errorf("physical profile charged=%d reserved=%d cap=%d", charged, reserved, capacity)
	}
	effects.transients.mutex().Lock()
	limits := registry.retention.sourceLimit()
	t.Logf("lane=%s sources=8 slots=9 Q=%d E=%d sourceQ=%d sourceE=%d source_bytes=%v transient_peak=%d transient_now=%d units=%d physical=%d reserved=%d", id, retentionDefaultCommandLimit, retentionDefaultEnvelopeLimit, limits.commands, limits.envelopes, wantBytes, effects.transients.peak, effects.transients.bytes, effects.transients.units, charged, reserved)
	effects.transients.mutex().Unlock()
	effects.mu.Lock()
	var clientPIDs []int
	for _, unit := range effects.units {
		if unit.process != nil && unit.process.Process != nil {
			clientPIDs = append(clientPIDs, unit.process.Process.Pid)
		}
	}
	effects.mu.Unlock()
	var clientRSS, clientHighWater int64
	for _, pid := range clientPIDs {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				n, _ := strconv.ParseInt(fields[1], 10, 64)
				switch fields[0] {
				case "VmRSS:":
					clientRSS += n * 1024
				case "VmHWM:":
					clientHighWater += n * 1024
				}
			}
		}
	}
	t.Logf("observer_clients=%d client_rss=%d client_sum_high_water=%d", len(clientPIDs), clientRSS, clientHighWater)
}

// The manifest requires one reader per source. Rotation deliberately terminates
// its old tail; the same reader reopens snapshot+tail on the successor.
func drainRecordingProfileReader(ctx context.Context, effects *UnifiedDevPaneEffects, session string) error {
	for ctx.Err() == nil {
		events, _, tail, cancel, err := effects.openSnapshotTail(session)
		if err != nil {
			return err
		}
		events = nil
		_ = events
		tail.releaseSnapshot()
		for {
			select {
			case <-ctx.Done():
				cancel()
				return nil
			case <-tail.events():
				event, ok := tail.receive()
				if !ok {
					cancel()
					if tail.closeReason() != proto.SubscriberClosedGenerationRotated {
						return fmt.Errorf("profile reader: %s", tail.closeReason())
					}
					goto reopen
				}
				bytes := recordingEventBytes(event)
				event = unifiedjournal.Event{}
				tail.lease.releaseEvent(bytes)
			}
		}
	reopen:
	}
	return nil
}

type recordingProfileEffects struct{ *recordingMemoryEffects }

func (effects *recordingProfileEffects) WritePaneRange(key unifiedjournal.PaneKey, payload []byte, start, end, sequence int64) error {
	return effects.UnifiedDevPaneEffects.WritePaneRange(key, payload, start, end, sequence)
}
