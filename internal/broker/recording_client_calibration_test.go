package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This is an external-process measurement, not a per-process memory bound.
// The active observer plus held founding observers coexist with all actually
// admitted Epoch clients; slow readers then exercise the real shutdown path.
func TestRecordingNativeClientOverlapCalibration(t *testing.T) {
	if os.Getenv("PERSEA_RECORDING_CLIENTS") != "1" {
		t.Skip("isolated native-client sustained-stall calibration")
	}
	entered := make(chan string, recordingObserverUnitLimit)
	release := make(chan struct{})
	var once sync.Once
	var active string
	f := newRecordingSettlementFixture(t, func(effects *UnifiedDevPaneEffects) {
		effects.observerReadinessEdge = func(session string) error {
			// The first session supplies authoritative snapshots and live tails.
			if session == active {
				return nil
			}
			entered <- session
			<-release
			return context.Canceled
		}
	})
	defer once.Do(func() { close(release) })
	active = f.startPaneCommand(t, "native-active", "stty -echo -opost; exec /bin/sh")
	if _, err := f.effects.AdoptSession(context.Background(), active); err != nil {
		t.Fatal(err)
	}
	serverPID, err := strconv.Atoi(f.disposable.run("display-message", "-p", "#{pid}"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native_fixture_socket=%s broker_pid=%d source_server_pid=%d observer_limit=%d", f.disposable.path, os.Getpid(), serverPID, recordingObserverUnitLimit)
	probe := recordingStartNativeProbe(t, serverPID)
	defer probe.stop()
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &Server{config: f.cfg, panes: f.registry, unified: f.effects}
	go func() { _ = server.accept(listener) }()
	d, err := details(f.server, active)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(f.server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = f.cfg.Realm, "main", d.ID, d.Created
	var attachments []net.Conn
	defer func() {
		for _, conn := range attachments {
			_ = conn.Close()
		}
	}()
	// PREPARE is released between opens. The permanent per-Epoch and writer
	// owners admit 27; a twenty-eighth still needs its PREPARE workspace.
	for i := 0; i < 27; i++ {
		conn, _ := unifiedE2E1OpenAttachment(t, listener.Addr().String(), authority)
		attachments = append(attachments, conn)
	}
	if lease, err := f.effects.readers.acquireAttachment(); err == nil {
		lease.detach()
		t.Fatal("unexpected twenty-eighth attachment admission")
	}
	var sessions, progress []string
	for i := 1; i < recordingObserverUnitLimit; i++ {
		session := f.startPaneCommand(t, "native-founder-"+strconv.Itoa(i), "stty -echo -opost; exec /bin/sh")
		sessions = append(sessions, session)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := f.effects.AdoptSession(ctx, session); done <- err }()
		select {
		case got := <-entered:
			if got != session {
				t.Fatal(got, session)
			}
		case err := <-done:
			cancel()
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("founding edge timeout")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		progress = append(progress, filepath.Join(t.TempDir(), "progress"))
	}
	// Every held client exists before any producer starts. Each receipt byte is
	// appended only after a full write: I acknowledges the initial 2 MiB, and
	// each following byte acknowledges one 8 KiB sustained output block.
	producer := filepath.Join(t.TempDir(), "native-producer.py")
	if err := os.WriteFile(producer, []byte(recordingNativeProducer), 0600); err != nil {
		t.Fatal(err)
	}
	for i, session := range sessions {
		command := "exec /usr/bin/python3 " + shellQuote(producer) + " " + shellQuote(progress[i])
		f.disposable.run("send-keys", "-t", "="+session+":", command, "Enter")
	}
	readPIDs := func() []int {
		var pids []int
		for _, field := range strings.Fields(f.disposable.run("list-clients", "-F", "#{client_pid}")) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				t.Fatal(err)
			}
			pids = append(pids, pid)
		}
		return pids
	}
	if pids := readPIDs(); len(pids) != recordingObserverUnitLimit+len(attachments) {
		t.Fatal("co-reachable client population", len(pids))
	}
	baseline := recordingWaitNativeInitial(t, progress, 60*time.Second)
	previous := append([]int64(nil), baseline...)
	measurementStart, previousAt := time.Now(), time.Now()
	var initialPrivate, finalPrivate, peakPrivate, peakRSS, peakPTE, peakFiles int64
	for sample := 0; sample < 30; sample++ {
		time.Sleep(time.Second)
		now := time.Now()
		current := recordingNativeReceipts(t, progress)
		var total int64
		minimum := int64(^uint64(0) >> 1)
		deltas := make([]int64, len(current))
		for i, n := range current {
			deltas[i] = (n - previous[i]) * 8192
			total += deltas[i]
			minimum = min(minimum, deltas[i])
			if deltas[i] <= 0 {
				t.Errorf("source %d failed sustained window %d: before=%d after=%d elapsed=%s", i, sample+1, previous[i], n, now.Sub(previousAt))
			}
		}
		t.Logf("native_receipt_window=%d elapsed_seconds=%.6f source_bytes=%v minimum_source_bytes=%d total_bytes=%d minimum_source_bytes_per_second=%.3f total_bytes_per_second=%.3f", sample+1, now.Sub(previousAt).Seconds(), deltas, minimum, total, float64(minimum)/now.Sub(previousAt).Seconds(), float64(total)/now.Sub(previousAt).Seconds())
		previous, previousAt = current, now
		pids := readPIDs()
		if len(pids) != recordingObserverUnitLimit+len(attachments) {
			t.Fatal("client left before sustained measurement", len(pids))
		}
		rss, pss, private, maximumPrivate, files, pte, err := recordingClientMemory(pids)
		if err != nil {
			t.Fatal(err)
		}
		if sample == 0 {
			initialPrivate = private
		}
		finalPrivate = private
		peakPrivate, peakRSS, peakPTE, peakFiles = max(peakPrivate, private), max(peakRSS, rss), max(peakPTE, pte), max(peakFiles, files)
		t.Logf("native_stall_second=%d clients=%d sum_rss=%d sum_pss=%d sum_private=%d maximum_private=%d mapped_file_union=%d page_tables=%d", sample+1, len(pids), rss, pss, private, maximumPrivate, files, pte)
		recordingMemorySample(t, "native_sustained_stall")
		probe.sample("native_sustained_stall")
	}
	var totalProgress int64
	minimumProgress := int64(^uint64(0) >> 1)
	for i, n := range previous {
		minimumProgress = min(minimumProgress, n-baseline[i])
		totalProgress += n - baseline[i]
	}
	t.Logf("native_sustained_source_progress elapsed_seconds=%.6f minimum_iterations=%d total_iterations=%d bytes_per_iteration=%d sources=%d initial_bytes_per_source=%d", previousAt.Sub(measurementStart).Seconds(), minimumProgress, totalProgress, 8192, len(progress), 2<<20)
	t.Logf("native_client_summary peak_private=%d peak_rss=%d peak_page_tables=%d peak_file_union=%d private_slope_bytes_per_second=%.3f", peakPrivate, peakRSS, peakPTE, peakFiles, float64(finalPrivate-initialPrivate)/time.Since(measurementStart).Seconds())
	// The peers stop reading. Enough authoritative output fills the existing
	// reader admission and exercises subscriber eviction, write cancellation,
	// Epoch finalization, PTY drain and the actual tmux Wait settlement.
	f.disposable.run("send-keys", "-t", "="+active+":", "head -c4194304 /dev/zero | tr '\\000' a", "Enter")
	probe.sample("authoritative_overload_started")
	deadline := time.Now().Add(10 * time.Second)
	for {
		pids := readPIDs()
		f.effects.readers.mutex().Lock()
		readers, held := f.effects.readers.readers, f.effects.readers.bytes
		f.effects.readers.mutex().Unlock()
		t.Logf("native_teardown clients=%d readers=%d reader_bytes=%d", len(pids), readers, held)
		recordingMemorySample(t, "native_teardown")
		if readers == 0 && len(pids) == recordingObserverUnitLimit {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("actual attachment settlement did not refund after overload: clients=%d readers=%d reader_bytes=%d", len(pids), readers, held)
			break
		}
		probe.sample("attachment_overload_settlement")
		time.Sleep(250 * time.Millisecond)
	}
	for _, conn := range attachments {
		_ = conn.Close()
	}
	probe.sample("peer_connections_closed")
	deadline = time.Now().Add(10 * time.Second)
	for {
		f.effects.readers.mutex().Lock()
		readers, held := f.effects.readers.readers, f.effects.readers.bytes
		f.effects.readers.mutex().Unlock()
		if readers == 0 {
			t.Logf("native_connection_settlement readers=%d reader_bytes=%d", readers, held)
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("connection shutdown retained owners: readers=%d reader_bytes=%d", readers, held)
			break
		}
		probe.sample("peer_connection_settlement")
		time.Sleep(250 * time.Millisecond)
	}
	once.Do(func() { close(release) })
	probe.sample("founder_consumers_released")
	deadline = time.Now().Add(10 * time.Second)
	for {
		f.effects.transients.mutex().Lock()
		units := f.effects.transients.units
		f.effects.transients.mutex().Unlock()
		if units == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("actual founding settlement did not refund: units=%d", units)
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := f.effects.AdoptSession(context.Background(), active); err != nil {
		t.Error("reader overload stopped authoritative recording", err)
	}
	f.cancel()
	deadline = time.Now().Add(10 * time.Second)
	for {
		f.effects.transients.mutex().Lock()
		units, held := f.effects.transients.units, f.effects.transients.bytes
		f.effects.transients.mutex().Unlock()
		if units == 0 {
			t.Logf("native_observer_shutdown units=%d held_bytes=%d", units, held)
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("observer shutdown retained owners: units=%d held_bytes=%d", units, held)
			break
		}
		probe.sample("observer_shutdown_settlement")
		time.Sleep(250 * time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		probe.sample("post_broker_client_close")
		time.Sleep(time.Second)
	}
	t.Logf("native_post_close_client_pids=%v", readPIDs())
	probe.sample("before_private_source_shutdown")
	if out, err := tmuxCombinedOutput(f.disposable.path, "kill-server"); err != nil {
		t.Errorf("private source shutdown: %v %s", err, out)
	}
	t.Logf("native_private_source_shutdown_requested pid=%d", serverPID)
}

func TestRecordingFoundingClientCapacityCalibration(t *testing.T) {
	if os.Getenv("PERSEA_RECORDING_CLIENTS") != "1" {
		t.Skip("isolated founding-client capacity calibration")
	}
	entered := make(chan string, recordingObserverUnitLimit)
	release := make(chan struct{})
	var once sync.Once
	f := newRecordingSettlementFixture(t, func(effects *UnifiedDevPaneEffects) {
		effects.observerReadinessEdge = func(session string) error { entered <- session; <-release; return context.Canceled }
	})
	defer once.Do(func() { close(release) })
	var sessions []string
	for i := 0; i < recordingObserverUnitLimit; i++ {
		session := f.startPaneCommand(t, "founding-memory-"+strconv.Itoa(i), "stty -echo -opost; exec /bin/sh")
		sessions = append(sessions, session)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := f.effects.AdoptSession(ctx, session); done <- err }()
		select {
		case got := <-entered:
			if got != session {
				t.Fatal(got, session)
			}
		case err := <-done:
			cancel()
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("founding edge timeout")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if _, err := f.effects.transients.acquire(); !errors.Is(err, errRecordingTransients) {
		t.Fatal("founding count exceeded", err)
	}
	// All callers are gone while actual processes/readers remain. Their
	// channels now fill with real tmux output under their fixed unit reserves.
	for _, session := range sessions {
		f.disposable.run("send-keys", "-t", "="+session+":", "head -c2097152 /dev/zero | tr '\\000' f", "Enter")
	}
	var leases []*recordingReaderLease
	var snapshots [][]byte
	for i := 0; i < 8; i++ {
		size := int64(recordingReaderBytes/8 - recordingWriterBytes - recordingReaderFloor - recordingPrepareBytes)
		lease, err := f.effects.readers.acquire(size)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
		snapshots = append(snapshots, bytes.Repeat([]byte{'s'}, int(size)))
	}
	defer func() {
		for i, lease := range leases {
			snapshots[i] = nil
			lease.releaseSnapshot()
			lease.detach()
		}
	}()
	var pids []int
	for _, field := range strings.Fields(f.disposable.run("list-clients", "-F", "#{client_pid}")) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	if len(pids) != recordingObserverUnitLimit {
		t.Fatal("wrong live client population", len(pids))
	}
	for sample := 0; sample < 4; sample++ {
		time.Sleep(250 * time.Millisecond)
		rss, pss, private, maximumPrivate, files, pte, err := recordingClientMemory(pids)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("clients=%d sum_rss=%d sum_pss=%d sum_private=%d maximum_private=%d mapped_file_union=%d page_tables=%d", len(pids), rss, pss, private, maximumPrivate, files, pte)
		recordingMemorySample(t, "founding_clients_full_readers")
	}
	once.Do(func() { close(release) })
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.effects.transients.mutex().Lock()
		units, held := f.effects.transients.units, f.effects.transients.bytes
		f.effects.transients.mutex().Unlock()
		if units == 0 && held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("actual founder settlement did not refund", units, held)
		}
		time.Sleep(time.Millisecond)
	}
}

type recordingMapping struct{ start, end uint64 }

// Summed RSS repeats shared library pages. A conservative physical estimate
// instead keeps every private page plus the union of every file mapping, even
// nonresident mapped pages. Page tables are reported separately.
func recordingClientMemory(pids []int) (rss, pss, private, maximumPrivate, files, pte int64, err error) {
	mappings := make(map[string][]recordingMapping)
	for _, pid := range pids {
		var data []byte
		data, err = os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
		if err != nil {
			return
		}
		var own int64
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			n, _ := strconv.ParseInt(fields[1], 10, 64)
			n *= 1024
			switch fields[0] {
			case "Rss:":
				rss += n
			case "Pss:":
				pss += n
			case "Private_Clean:", "Private_Dirty:":
				private += n
				own += n
			}
		}
		maximumPrivate = max(maximumPrivate, own)
		data, err = os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 1 && fields[0] == "VmPTE:" {
				n, _ := strconv.ParseInt(fields[1], 10, 64)
				pte += n * 1024
			}
		}
		data, err = os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 5 || fields[4] == "0" {
				continue
			}
			span := strings.Split(fields[0], "-")
			if len(span) != 2 {
				continue
			}
			start, e1 := strconv.ParseUint(span[0], 16, 64)
			end, e2 := strconv.ParseUint(span[1], 16, 64)
			offset, e3 := strconv.ParseUint(fields[2], 16, 64)
			if e1 != nil || e2 != nil || e3 != nil {
				err = errors.New("invalid process mapping")
				return
			}
			identity := fields[3] + ":" + fields[4]
			mappings[identity] = append(mappings[identity], recordingMapping{offset, offset + end - start})
		}
	}
	for _, ranges := range mappings {
		sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
		var end uint64
		for _, span := range ranges {
			if span.end > end {
				files += int64(span.end - max(end, span.start))
				end = span.end
			}
		}
	}
	return
}
