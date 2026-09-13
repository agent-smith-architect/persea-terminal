package broker

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const recordingNativeProducer = `import os, sys, time
receipt = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
def emit(data):
    offset = 0
    while offset < len(data):
        offset += os.write(1, data[offset:])
emit(b'f' * 2097152)
os.write(receipt, b'I')
while True:
    emit(b'f' * 8192)
    os.write(receipt, b'.')
    time.sleep(0.1)
`

func recordingNativeReceipts(t *testing.T, paths []string) []int64 {
	t.Helper()
	counts := make([]int64, len(paths))
	for i, path := range paths {
		info, err := os.Stat(path)
		if err != nil || info.Size() < 1 {
			t.Fatalf("source %d has no completed initial-write receipt: %v", i, err)
		}
		counts[i] = info.Size() - 1
	}
	return counts
}

func recordingWaitNativeInitial(t *testing.T, paths []string, timeout time.Duration) []int64 {
	t.Helper()
	started := time.Now()
	for {
		ready := 0
		for _, path := range paths {
			if info, err := os.Stat(path); err == nil && info.Size() >= 2 {
				ready++
			}
		}
		if ready == len(paths) {
			t.Logf("native_initial_receipts sources=%d initial_bytes_per_source=%d elapsed_seconds=%.6f", ready, 2<<20, time.Since(started).Seconds())
			return recordingNativeReceipts(t, paths)
		}
		if time.Since(started) > timeout {
			t.Fatalf("initial output receipts incomplete: ready=%d required=%d elapsed=%s", ready, len(paths), time.Since(started))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type recordingNativeProbe struct {
	t                     *testing.T
	server                int
	started               time.Time
	stopCh, done          chan struct{}
	once                  sync.Once
	mu                    sync.Mutex
	seen                  map[int]string
	peakChildren, peakFDs int
	peakHelpers           int
	peakSourceRSS         int64
	peakSourcePrivate     int64
	peakSourcePTE         int64
}

func recordingStartNativeProbe(t *testing.T, server int) *recordingNativeProbe {
	p := &recordingNativeProbe{t: t, server: server, started: time.Now(), stopCh: make(chan struct{}), done: make(chan struct{}), seen: make(map[int]string)}
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			p.children()
			select {
			case <-p.stopCh:
				return
			case <-ticker.C:
			}
		}
	}()
	return p
}

// Read each thread's direct children: os/exec may fork from any Go OS thread.
// Server-spawned producers and guard shells belong to the external source tree.
func (p *recordingNativeProbe) children() {
	tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", os.Getpid()))
	if err != nil {
		return
	}
	ids := make(map[int]bool)
	for _, task := range tasks {
		data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", os.Getpid(), task.Name()))
		for _, field := range strings.Fields(string(data)) {
			if pid, err := strconv.Atoi(field); err == nil && pid != p.server {
				ids[pid] = true
			}
		}
	}
	current := make(map[int]string)
	fds, helpers := 0, 0
	for pid := range ids {
		comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err != nil || !strings.HasPrefix(string(comm), "tmux") {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		args := strings.ReplaceAll(string(data), "\x00", " ")
		role := "helper"
		if strings.Contains(args, " -C ") {
			role = "control"
		} else if strings.Contains(args, " attach-session ") {
			role = "epoch"
		}
		if role == "helper" {
			helpers++
		}
		current[pid] = role
		entries, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		fds += len(entries)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for pid, role := range current {
		p.seen[pid] = role
	}
	if len(current) > p.peakChildren || helpers > p.peakHelpers || fds > p.peakFDs {
		p.peakChildren, p.peakHelpers, p.peakFDs = max(p.peakChildren, len(current)), max(p.peakHelpers, helpers), max(p.peakFDs, fds)
		p.t.Logf("native_child_high_water elapsed_seconds=%.6f live=%d helpers=%d fds=%d pids_and_roles=%v", time.Since(p.started).Seconds(), len(current), helpers, fds, current)
	}
}

func (p *recordingNativeProbe) sample(phase string) {
	p.children()
	rss, _, private, _, _, pte, err := recordingClientMemory([]int{p.server})
	if err != nil {
		p.t.Logf("external_source_server phase=%s pid=%d read_error=%q", phase, p.server, err)
		return
	}
	entries, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", p.server))
	children := recordingNativeProcessChildren(p.server)
	p.mu.Lock()
	p.peakSourceRSS, p.peakSourcePrivate, p.peakSourcePTE = max(p.peakSourceRSS, rss), max(p.peakSourcePrivate, private), max(p.peakSourcePTE, pte)
	p.mu.Unlock()
	p.t.Logf("external_source_server phase=%s elapsed_seconds=%.6f pid=%d rss=%d private=%d page_tables=%d fds=%d direct_children=%d", phase, time.Since(p.started).Seconds(), p.server, rss, private, pte, len(entries), children)
}

func recordingNativeProcessChildren(pid int) int {
	data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	return len(strings.Fields(string(data)))
}

func (p *recordingNativeProbe) stop() {
	p.once.Do(func() { close(p.stopCh) })
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	var remaining []int
	for pid := range p.seen {
		if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err == nil {
			remaining = append(remaining, pid)
		}
	}
	sort.Ints(remaining)
	// Each unit can overlap its control client with a source-witness helper
	// before journal admission. Attachments can also overlap their own helper.
	// This is scoped planning arithmetic, not a measured simultaneous count
	// or a universal process bound; held founders here stop before that query.
	scopedChildren := 2*recordingObserverUnitLimit + 2*27 + 1
	p.t.Logf("native_process_summary observed_pids=%d peak_children=%d peak_helpers=%d peak_fds=%d remaining_observed_pids=%v peak_source_rss=%d peak_source_private=%d peak_source_page_tables=%d analytical_scoped_child_count=%d harness_probe_child_allowance=1", len(p.seen), p.peakChildren, p.peakHelpers, p.peakFDs, remaining, p.peakSourceRSS, p.peakSourcePrivate, p.peakSourcePTE, scopedChildren)
}
