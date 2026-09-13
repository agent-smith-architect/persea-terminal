package broker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This opt-in gate uses one private source server across three finite floods.
// A completed producer receipt and captured tail marker precede each recovery
// sample. RSS alone cannot distinguish live output from allocator retention.
func TestRecordingSupervisionSourceStallRecovery(t *testing.T) {
	if os.Getenv("PERSEA_RECORDING_SOURCE_STALL") != "1" {
		t.Skip("private source stall/recovery memory gate")
	}
	type hold struct {
		entered, release chan struct{}
		once             sync.Once
	}
	holds := make(chan *hold, 3)
	f := newRecordingSettlementFixture(t, func(effects *UnifiedDevPaneEffects) {
		// Exercise the production deadline: a shorter synthetic-unit deadline
		// can fault legitimate history reconstruction under the race detector.
		effects.supervisionStallLimit = 2 * time.Second
		effects.observerReadinessEdge = func(string) error {
			h := <-holds
			if h == nil {
				return nil
			}
			close(h.entered)
			<-h.release
			return context.Canceled
		}
	})
	session := f.startPaneCommand(t, "source-stall", "stty -echo -opost; exec /bin/sh")
	serverPID, err := strconv.Atoi(f.disposable.run("display-message", "-p", "#{pid}"))
	if err != nil {
		t.Fatal(err)
	}
	producerPID, err := strconv.Atoi(f.disposable.run("display-message", "-p", "-t", "="+session+":", "#{pane_pid}"))
	if err != nil {
		t.Fatal(err)
	}
	baseline := recordingSourceFacts(t, f.disposable, session)
	witness := recordingSourceWitness(t, f.disposable.path)
	defer witness.close()
	cgroup, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", serverPID))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("source_gate socket=%s broker_pid=%d source_pid=%d pane_pid=%d witness_pid=%d source_cgroup=%q", f.disposable.path, os.Getpid(), serverPID, producerPID, witness.cmd.Process.Pid, strings.TrimSpace(string(cgroup)))
	probe := recordingStartNativeProbe(t, serverPID)
	defer probe.stop()
	probe.sample("baseline_before_stalls")
	producer := filepath.Join(t.TempDir(), "finite-producer.py")
	const script = `import os, sys, time
for i in range(128):
    data = b'x' * 65536
    while data:
        n = os.write(1, data)
        data = data[n:]
    time.sleep(0.005)
data = ('\n' + sys.argv[2] + '\n').encode()
while data:
    n = os.write(1, data)
    data = data[n:]
with open(sys.argv[1], 'w') as receipt:
    receipt.write('DONE 8388608 %d\n' % os.getpid())
`
	if err := os.WriteFile(producer, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	var settled []int64
	var observerPIDs []int
	var producerPIDs []int
	for cycle := 0; cycle < 3; cycle++ {
		h := &hold{entered: make(chan struct{}), release: make(chan struct{})}
		defer h.once.Do(func() { close(h.release) })
		holds <- h
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := f.effects.AdoptSession(ctx, session); done <- err }()
		select {
		case <-h.entered:
		case err := <-done:
			cancel()
			t.Fatalf("cycle %d adoption ended before held edge: %v", cycle, err)
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("founding observer did not enter readiness edge")
		}
		pid := recordingSourceObserverPID(t, f.disposable, session)
		observerPIDs = append(observerPIDs, pid)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("caller cancellation: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancelled caller remained blocked")
		}
		receipt := filepath.Join(t.TempDir(), "producer-done")
		marker := fmt.Sprintf("SOURCE_STALL_DONE_%d", cycle)
		command := "/usr/bin/python3 " + shellQuote(producer) + " " + shellQuote(receipt) + " " + shellQuote(marker)
		producerStarted := time.Now()
		f.disposable.run("send-keys", "-l", "-t", "="+session+":", command)
		f.disposable.run("send-keys", "-t", "="+session+":", "Enter")
		var completedProducerPID int
		var lastProducerSample time.Time
		pollUntil(t, 8*time.Second, "finite producer stop receipt and source tail marker", func() bool {
			if time.Since(lastProducerSample) >= 100*time.Millisecond {
				probe.sample(fmt.Sprintf("cycle_%d_producing", cycle))
				lastProducerSample = time.Now()
			}
			data, err := os.ReadFile(receipt)
			fields := strings.Fields(string(data))
			if err != nil || len(fields) != 3 || fields[0] != "DONE" || fields[1] != "8388608" {
				return false
			}
			completedProducerPID, err = strconv.Atoi(fields[2])
			if err != nil || completedProducerPID <= 0 {
				return false
			}
			if _, err := os.Stat(fmt.Sprintf("/proc/%d", completedProducerPID)); !os.IsNotExist(err) {
				return false
			}
			out, err := tmuxCombinedOutput(f.disposable.path, "capture-pane", "-p", "-t", "="+session+":")
			return err == nil && strings.Contains(string(out), marker)
		})
		producerElapsed := time.Since(producerStarted)
		producerPIDs = append(producerPIDs, completedProducerPID)
		t.Logf("source_producer cycle=%d started_elapsed_seconds=%.6f stopped_elapsed_seconds=%.6f receipt_and_tail_elapsed_seconds=%.6f payload_bytes=8388608 payload_bytes_per_second=%.3f", cycle, producerStarted.Sub(probe.started).Seconds(), time.Since(probe.started).Seconds(), producerElapsed.Seconds(), float64(8<<20)/producerElapsed.Seconds())
		witness.roundtrip(t, fmt.Sprintf("WITNESS_STALLED_%d", cycle))
		probe.sample(fmt.Sprintf("cycle_%d_producer_stopped_callback_held", cycle))
		// The cutoff must sever the upstream connection even when arbitrary
		// callback code cannot return. Actual Wait/lease settlement is later.
		deadline := time.Now().Add(3 * time.Second)
		for recordingSourceClientPresent(t, f.disposable, pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		attached := recordingSourceClientPresent(t, f.disposable, pid)
		if attached {
			t.Errorf("cycle %d: cancelled held observer pid=%d still buffers source output after cutoff deadline", cycle, pid)
		}
		f.effects.transients.mutex().Lock()
		unitsHeld := f.effects.transients.units
		f.effects.transients.mutex().Unlock()
		if unitsHeld != 1 {
			t.Errorf("cycle %d: held callback owner units=%d, want 1 until actual settlement", cycle, unitsHeld)
		}
		t.Logf("source_cutoff cycle=%d producer_started_elapsed_seconds=%.6f observed_elapsed_seconds=%.6f observer_pid=%d still_attached=%t callback_released=false held_units=%d", cycle, producerStarted.Sub(probe.started).Seconds(), time.Since(probe.started).Seconds(), pid, attached, unitsHeld)
		h.once.Do(func() { close(h.release) })
		pollUntil(t, 5*time.Second, "observer process Wait and owner release", func() bool {
			f.effects.transients.mutex().Lock()
			units := f.effects.transients.units
			f.effects.transients.mutex().Unlock()
			_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
			return units == 0 && os.IsNotExist(err)
		})
		pollUntil(t, 5*time.Second, "founding map removal before retry", func() bool {
			f.effects.mu.Lock()
			defer f.effects.mu.Unlock()
			_, adopting := f.effects.adopting[session]
			return f.effects.units[session] == nil && !adopting
		})
		if got := recordingSourceFacts(t, f.disposable, session); got != baseline {
			t.Errorf("cycle %d: source geometry/options changed\nbefore=%s\nafter=%s", cycle, baseline, got)
		}
		var lo, hi int64
		for sample := 0; sample < 4; sample++ {
			witness.roundtrip(t, fmt.Sprintf("WITNESS_RECOVERED_%d_%d", cycle, sample))
			_, _, private, _, _, _, err := recordingClientMemory([]int{serverPID})
			if err != nil {
				t.Fatal(err)
			}
			if sample == 0 {
				lo = private
			}
			lo, hi = min(lo, private), max(hi, private)
			probe.sample(fmt.Sprintf("cycle_%d_settled_%d", cycle, sample))
			time.Sleep(150 * time.Millisecond)
		}
		settled = append(settled, hi)
		if hi-lo > 2<<20 {
			t.Errorf("cycle %d: stopped and disconnected source not settled: private min=%d max=%d", cycle, lo, hi)
		}
		t.Logf("source_cycle=%d producer_bytes=8388608 receipt=complete producer_pid=%d producer_pid_absent=true tail_marker=captured observer_pid=%d actual_wait=complete units=0 source_private_min=%d source_private_max=%d", cycle, completedProducerPID, pid, lo, hi)
	}
	// A bounded finite experiment is not a universal memory guarantee. Reuse
	// of the same server after disconnected observers exposes cycle growth.
	if settled[2]-settled[1] > 4<<20 {
		t.Errorf("settled source private memory continues growing after warmup: %v", settled)
	}
	// A fresh authoritative recorder must really record after the failed
	// founders have settled; successful client reconnection alone is weaker.
	holds <- nil
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer recoveryCancel()
	adoption, err := f.effects.AdoptSession(recoveryCtx, session)
	if err != nil {
		t.Fatalf("recording recovery after repeated stalls: %v", err)
	}
	recoveredPID := recordingSourceObserverPID(t, f.disposable, session)
	observerPIDs = append(observerPIDs, recoveredPID)
	f.disposable.run("send-keys", "-l", "-t", "="+session+":", "printf '\\nSOURCE_RECORDER_RECOVERED\\n'")
	f.disposable.run("send-keys", "-t", "="+session+":", "Enter")
	pollUntil(t, 5*time.Second, "fresh authoritative recording output", func() bool {
		return bytes.Contains(f.journalBytes(t, adoption.Key), []byte("SOURCE_RECORDER_RECOVERED"))
	})
	if got := recordingSourceFacts(t, f.disposable, session); got != baseline {
		t.Error("source geometry/options changed during successful recorder recovery")
	}
	t.Logf("source_recording_recovery observer_pid=%d authoritative_marker=committed", recoveredPID)
	witness.close()
	f.cancel()
	pollUntil(t, 5*time.Second, "recovered observer process Wait and owner release", func() bool {
		f.effects.transients.mutex().Lock()
		units := f.effects.transients.units
		f.effects.transients.mutex().Unlock()
		_, err := os.Stat(fmt.Sprintf("/proc/%d", recoveredPID))
		return units == 0 && os.IsNotExist(err)
	})
	if _, err := tmuxCombinedOutput(f.disposable.path, "kill-server"); err != nil {
		t.Fatal(err)
	}
	for _, pid := range append(append(observerPIDs, producerPIDs...), witness.cmd.Process.Pid) {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); !os.IsNotExist(err) {
			t.Errorf("owned client pid=%d still exists after cleanup: %v", pid, err)
		}
	}
	for _, pid := range []int{serverPID, producerPID} {
		pollUntil(t, 5*time.Second, "private source process termination", func() bool {
			data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if os.IsNotExist(err) {
				return true
			}
			// Daemonized tmux and its pane belong to the host reaper. A zombie
			// has no RSS/FD/output owner, but its PID is explicitly reported.
			end := strings.LastIndexByte(string(data), ')')
			return err == nil && end >= 0 && strings.HasPrefix(string(data)[end+1:], " Z")
		})
		_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
		t.Logf("source_cleanup pid=%d absent=%t terminated=true", pid, os.IsNotExist(err))
	}
	t.Logf("source_settled_private_cycles=%v owned_client_cleanup=complete", settled)
}

func recordingSourceFacts(t *testing.T, d *disposable, session string) string {
	t.Helper()
	target := "=" + session + ":"
	return d.run("display-message", "-p", "-t", target, "#{pane_width}x#{pane_height}|#{window_width}x#{window_height}") + "\n" + d.run("show-options", "-A", "-t", session) + "\n" + d.run("show-options", "-w", "-A", "-t", target)
}

func recordingSourceObserverPID(t *testing.T, d *disposable, session string) int {
	t.Helper()
	for _, line := range strings.Split(d.run("list-clients", "-F", "#{client_pid}|#{session_id}"), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) == 2 && fields[1] == session {
			pid, err := strconv.Atoi(fields[0])
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
	}
	t.Fatal("held observer client absent before cancellation")
	return 0
}

func recordingSourceClientPresent(t *testing.T, d *disposable, pid int) bool {
	t.Helper()
	for _, field := range strings.Fields(d.run("list-clients", "-F", "#{client_pid}")) {
		if field == strconv.Itoa(pid) {
			return true
		}
	}
	return false
}

type recordingSourceControlWitness struct {
	cmd   *exec.Cmd
	input io.WriteCloser
	lines chan string
	done  chan struct{}
	once  sync.Once
}

func recordingSourceWitness(t *testing.T, socket string) *recordingSourceControlWitness {
	t.Helper()
	w := &recordingSourceControlWitness{cmd: exec.Command("tmux", "-S", socket, "-C", "attach-session", "-f", "read-only,ignore-size", "-t", "alpha"), lines: make(chan string, 256), done: make(chan struct{})}
	var err error
	w.input, err = w.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := w.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(w.done)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			select {
			case w.lines <- scanner.Text():
			default:
			}
		}
	}()
	t.Cleanup(w.close)
	w.roundtrip(t, "WITNESS_READY")
	return w
}

func (w *recordingSourceControlWitness) roundtrip(t *testing.T, marker string) {
	t.Helper()
	start := time.Now()
	if _, err := fmt.Fprintf(w.input, "display-message -p %s\n", marker); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-w.lines:
			if line == marker {
				t.Logf("other_client_roundtrip marker=%s elapsed_us=%d", marker, time.Since(start).Microseconds())
				return
			}
		case <-w.done:
			t.Fatal("other client exited during source stall/recovery")
		case <-timer.C:
			t.Fatal("other client unresponsive during source stall/recovery")
		}
	}
}

func (w *recordingSourceControlWitness) close() {
	w.once.Do(func() {
		_ = w.input.Close()
		_ = w.cmd.Process.Kill()
		<-w.done
		_ = w.cmd.Wait()
	})
}
