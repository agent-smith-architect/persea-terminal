package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func tmuxTestCommand(t *testing.T, path string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	all := append([]string{"-S", path}, args...)
	out, err := exec.CommandContext(ctx, "tmux", all...).CombinedOutput()
	if err != nil {
		t.Fatalf("timeout-bound disposable tmux %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func readSnapshotWire(t *testing.T, wire *bytes.Buffer) (proto.Control, []byte) {
	t.Helper()
	first, err := proto.ReadFrame(wire)
	if err != nil || first.Type != proto.FrameControl {
		t.Fatalf("snapshot metadata missing: %v", err)
	}
	meta, err := proto.DecodeControl(first.Payload)
	if err != nil || meta.Type != "snapshot_ok" {
		t.Fatalf("snapshot metadata invalid: %+v %v", meta, err)
	}
	var data []byte
	for {
		frame, err := proto.ReadFrame(wire)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == proto.FrameData {
			data = append(data, frame.Payload...)
			continue
		}
		end, err := proto.DecodeControl(frame.Payload)
		if err != nil || end.Type != "snapshot_end" {
			t.Fatalf("snapshot terminator invalid: %+v %v", end, err)
		}
		return meta, data
	}
}

func TestSnapshotActualTmuxHistoryFidelityAndDepth(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "tmux.sock")
	conf := filepath.Join(dir, "tmux.conf")
	if err := os.WriteFile(conf, []byte("set -g history-limit 30000\nset -g status off\n"), 0600); err != nil {
		t.Fatal(err)
	}
	seed := `i=1; while [ "$i" -le 9000 ]; do printf 'PRE-%05d\n' "$i"; i=$((i+1)); done; printf '\033[31mANSI-RED\033[0m\n'; printf 'UNICODE-界-🙂-e\314\201-שלום\n'; printf 'WRAP-BEGIN-%090d-WRAP-END\n' 1; while [ "$i" -le 10050 ]; do printf 'PRE-%05d\n' "$i"; i=$((i+1)); done; printf '\033[?1049h\033[H\033[32mTUI-CURRENT\033[0m\n'; exec sleep 600`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	cmd := exec.CommandContext(ctx, "tmux", "-S", socket, "-f", conf, "new-session", "-d", "-s", "snap", "-x", "80", "-y", "24", "exec bash -c "+shellQuote(seed))
	out, err := cmd.CombinedOutput()
	cancel()
	if err != nil {
		t.Fatalf("start disposable tmux: %v: %s", err, out)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "tmux", "-S", socket, "kill-server").Run()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		state := tmuxTestCommand(t, socket, "display-message", "-p", "-t", "=snap:", "#{history_size}:#{alternate_on}")
		parts := strings.Split(state, ":")
		size, _ := strconv.Atoi(parts[0])
		if len(parts) == 2 && size >= 10000 && parts[1] == "1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("seed did not settle: %s", state)
		}
		time.Sleep(25 * time.Millisecond)
	}

	server := config.TmuxServer{Label: "test", SocketPath: socket}
	inc, err := incarnation(server)
	if err != nil {
		t.Fatal(err)
	}
	authority := inc
	authority.Realm, authority.Server = "local", "test"
	authority.SessionID = tmuxTestCommand(t, socket, "display-message", "-p", "-t", "=snap:", "#{session_id}")
	authority.SessionCreated, _ = strconv.ParseInt(tmuxTestCommand(t, socket, "display-message", "-p", "-t", "=snap:", "#{session_created}"), 10, 64)
	s := &Server{config: config.Broker{Realm: "local", Servers: []config.TmuxServer{server}}}

	var full bytes.Buffer
	s.snapshot(&lockedWriter{w: &full}, proto.Control{Type: "snapshot", Authority: &authority, Depth: SnapshotLineLimit + 1})
	meta, data := readSnapshotWire(t, &full)
	lines := bytes.Count(data, []byte{'\n'})
	if lines != SnapshotLineLimit+meta.Height {
		t.Fatalf("snapshot line cap = %d, want %d", lines, SnapshotLineLimit+meta.Height)
	}
	if !meta.Truncated || !meta.Alternate || meta.Width != 80 || meta.Height != 24 || !strings.HasPrefix(meta.Pane, "%") || meta.Depth != 5000 || meta.HistoryRows == nil || *meta.HistoryRows != 5000 {
		t.Fatalf("snapshot metadata mismatch: %+v", meta)
	}
	if delta := time.Now().UnixMilli() - meta.FrozenAt; delta < 0 || delta > 5000 {
		t.Fatalf("frozen_at not sane: delta=%d", delta)
	}
	text := string(data)
	for _, marker := range []string{"ANSI-RED", "\x1b[31m", "UNICODE-界-🙂-é-שלום", "WRAP-BEGIN-", "WRAP-END", "TUI-CURRENT"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("snapshot fidelity marker missing: %q", marker)
		}
	}
	first, last := -1, -1
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "PRE-") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(line, "PRE-"))
		if err != nil {
			continue
		}
		if first < 0 {
			first = n
		}
		if last >= 0 && n != last+1 {
			t.Fatalf("history ordering broke at %d -> %d", last, n)
		}
		last = n
	}
	if first < 1 || last < 10020 || last >= 10050 {
		t.Fatalf("history range unexpected: first=%d last=%d", first, last)
	}

	var shallow bytes.Buffer
	s.snapshot(&lockedWriter{w: &shallow}, proto.Control{Type: "snapshot", Authority: &authority, Depth: 20})
	shallowMeta, shallowData := readSnapshotWire(t, &shallow)
	if shallowMeta.Depth != 20 || bytes.Count(shallowData, []byte{'\n'}) > 20+meta.Height+1 {
		t.Fatalf("depth not respected: meta=%+v lines=%d", shallowMeta, bytes.Count(shallowData, []byte{'\n'}))
	}
	if !bytes.Contains(shallowData, []byte("TUI-CURRENT")) {
		t.Fatal("alternate-screen current rows missing from shallow snapshot")
	}
}

func TestSnapshotOverCapRequestIsNotTruncatedWithoutActualLoss(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "small.sock")
	conf := filepath.Join(dir, "tmux.conf")
	if err := os.WriteFile(conf, []byte("set -g history-limit 100\nset -g status off\n"), 0600); err != nil {
		t.Fatal(err)
	}
	seed := `i=1; while [ "$i" -le 60 ]; do printf 'SMALL-%03d\n' "$i"; i=$((i+1)); done; exec sleep 600`
	tmuxTestCommand(t, socket, "-f", conf, "new-session", "-d", "-s", "small", "-x", "80", "-y", "24", "exec bash -c "+shellQuote(seed))
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })

	// Wait for the seed to stop writing, not merely to start. The old loop exited
	// the moment history_size became non-zero, so on a loaded machine it could read
	// 34 while the seed was still emitting 60 and then assert about truncation
	// against a count that was still moving. Identical in shape to the defect fixed
	// in TestSnapshotRetainsDepthHistoryRowsPlusVisibleScreen; that fix repaired the
	// instance and left the shape here. The seed execs `sleep 600` when it finishes,
	// so a count that stops changing is a count that will not change again.
	deadline := time.Now().Add(15 * time.Second)
	historySize, settled := 0, 0
	for settled < 2 {
		current, _ := strconv.Atoi(tmuxTestCommand(t, socket, "display-message", "-p", "-t", "=small:", "#{history_size}"))
		if current > 0 && current == historySize {
			settled++
		} else {
			settled = 0
		}
		historySize = current
		if time.Now().After(deadline) {
			t.Fatalf("small history did not settle: history_size=%d", historySize)
		}
		time.Sleep(25 * time.Millisecond)
	}
	server := config.TmuxServer{Label: "small", SocketPath: socket}
	authority, err := incarnation(server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server = "local", "small"
	authority.SessionID = tmuxTestCommand(t, socket, "display-message", "-p", "-t", "=small:", "#{session_id}")
	authority.SessionCreated, _ = strconv.ParseInt(tmuxTestCommand(t, socket, "display-message", "-p", "-t", "=small:", "#{session_created}"), 10, 64)
	s := &Server{config: config.Broker{Realm: "local", Servers: []config.TmuxServer{server}}}
	var wire bytes.Buffer
	s.snapshot(&lockedWriter{w: &wire}, proto.Control{Type: "snapshot", Authority: &authority, Depth: SnapshotLineLimit + 1})
	meta, _ := readSnapshotWire(t, &wire)
	if meta.Truncated || meta.Depth != SnapshotLineLimit || meta.HistoryRows == nil || *meta.HistoryRows != historySize {
		t.Fatalf("over-cap request claimed content loss: history_size=%d meta=%+v", historySize, meta)
	}
}

func TestSnapshotRetainsDepthHistoryRowsPlusVisibleScreen(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "depth.sock")
	conf := filepath.Join(dir, "tmux.conf")
	if err := os.WriteFile(conf, []byte("set -g history-limit 30000\nset -g status off\n"), 0600); err != nil {
		t.Fatal(err)
	}
	seed := `i=1; while [ "$i" -le 10050 ]; do printf 'ROW-%05d\n' "$i"; i=$((i+1)); done; exec sleep 600`
	tmuxTestCommand(t, socket, "-f", conf, "new-session", "-d", "-s", "depth", "-x", "80", "-y", "24", "exec bash -c "+shellQuote(seed))
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })
	// Wait for the seed to stop writing, not merely to pass a threshold. The old
	// loop exited at history_size >= 10000 while the seed was still emitting rows
	// up to 10050, so `oldest` below was computed from a count that kept growing
	// before the snapshot was taken, and the boundary row had already scrolled out
	// of the retained window. That only shows up on a machine slow enough for a
	// poll to land mid-write, which is why it passed locally and failed in CI.
	// The seed execs `sleep 600` when it finishes, so a count that stops changing
	// is a count that will not change again.
	deadline := time.Now().Add(15 * time.Second)
	historySize, settled := 0, 0
	for settled < 2 {
		current, _ := strconv.Atoi(tmuxTestCommand(t, socket, "display-message", "-p", "-t", "=depth:", "#{history_size}"))
		if current >= 10000 && current == historySize {
			settled++
		} else {
			settled = 0
		}
		historySize = current
		if time.Now().After(deadline) {
			t.Fatalf("depth seed did not settle: history_size=%d", historySize)
		}
		time.Sleep(25 * time.Millisecond)
	}
	server := config.TmuxServer{Label: "test", SocketPath: socket}
	authority, err := incarnation(server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server = "local", "test"
	authority.SessionID = tmuxTestCommand(t, socket, "display-message", "-p", "-t", "=depth:", "#{session_id}")
	authority.SessionCreated, _ = strconv.ParseInt(tmuxTestCommand(t, socket, "display-message", "-p", "-t", "=depth:", "#{session_created}"), 10, 64)
	var wire bytes.Buffer
	(&Server{config: config.Broker{Realm: "local", Servers: []config.TmuxServer{server}}}).snapshot(&lockedWriter{w: &wire}, proto.Control{Type: "snapshot", Authority: &authority, Depth: 5000})
	meta, data := readSnapshotWire(t, &wire)
	oldest := historySize - 5000 + 1
	if meta.HistoryRows == nil || *meta.HistoryRows != 5000 || bytes.Count(data, []byte{'\n'}) != 5000+meta.Height {
		t.Fatalf("depth semantics mismatch: meta=%+v rows=%d history_size=%d", meta, bytes.Count(data, []byte{'\n'}), historySize)
	}
	if !bytes.Contains(data, []byte(fmt.Sprintf("ROW-%05d", oldest))) || bytes.Contains(data, []byte(fmt.Sprintf("ROW-%05d", oldest-1))) || !bytes.Contains(data, []byte("ROW-10050")) {
		t.Fatalf("history boundary mismatch: oldest=%d older=%d", oldest, oldest-1)
	}
}

func TestSnapshotPaneMetadataMatchesCapturedPaneDuringActiveSwitches(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "pane.sock")
	conf := filepath.Join(dir, "tmux.conf")
	if err := os.WriteFile(conf, []byte("set -g status off\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tmuxTestCommand(t, socket, "-f", conf, "new-session", "-d", "-s", "panes", "-x", "80", "-y", "24", "exec bash -c "+shellQuote("printf 'PANE-A-DISTINCTIVE\\n'; exec sleep 600"))
	tmuxTestCommand(t, socket, "split-window", "-h", "-t", "=panes:", "exec bash -c "+shellQuote("printf 'PANE-B-DISTINCTIVE\\n'; exec sleep 600"))
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })
	panes := strings.Split(tmuxTestCommand(t, socket, "list-panes", "-t", "=panes:", "-F", "#{pane_id}"), "\n")
	if len(panes) != 2 {
		t.Fatalf("want two panes, got %q", panes)
	}
	server := config.TmuxServer{Label: "test", SocketPath: socket}
	authority, err := incarnation(server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server = "local", "test"
	authority.SessionID = tmuxTestCommand(t, socket, "display-message", "-p", "-t", "=panes:", "#{session_id}")
	authority.SessionCreated, _ = strconv.ParseInt(tmuxTestCommand(t, socket, "display-message", "-p", "-t", "=panes:", "#{session_created}"), 10, 64)
	s := &Server{config: config.Broker{Realm: "local", Servers: []config.TmuxServer{server}}}
	done := make(chan struct{})
	var switches sync.WaitGroup
	switches.Add(1)
	go func() {
		defer switches.Done()
		for {
			for _, pane := range panes {
				select {
				case <-done:
					return
				default:
					_ = exec.Command("tmux", "-S", socket, "select-pane", "-t", pane).Run()
				}
			}
		}
	}()
	t.Cleanup(func() { close(done); switches.Wait() })
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		var wire bytes.Buffer
		s.snapshot(&lockedWriter{w: &wire}, proto.Control{Type: "snapshot", Authority: &authority, Depth: 20})
		meta, data := readSnapshotWire(t, &wire)
		seen[meta.Pane] = true
		switch meta.Pane {
		case panes[0]:
			if !bytes.Contains(data, []byte("PANE-A-DISTINCTIVE")) || bytes.Contains(data, []byte("PANE-B-DISTINCTIVE")) {
				t.Fatalf("pane %s metadata did not match A capture", meta.Pane)
			}
		case panes[1]:
			if !bytes.Contains(data, []byte("PANE-B-DISTINCTIVE")) || bytes.Contains(data, []byte("PANE-A-DISTINCTIVE")) {
				t.Fatalf("pane %s metadata did not match B capture", meta.Pane)
			}
		default:
			t.Fatalf("unexpected pane metadata %q", meta.Pane)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("active switching did not exercise both panes: %v", seen)
	}
}

func TestGuardedSnapshotVanishedPaneFailsClosedAsStale(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "vanished.sock")
	tmuxTestCommand(t, socket, "new-session", "-d", "-s", "vanished", "exec sleep 600")
	tmuxTestCommand(t, socket, "split-window", "-h", "-t", "=vanished:", "exec sleep 600")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })
	server := config.TmuxServer{Label: "test", SocketPath: socket}
	authority, err := incarnation(server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server = "local", "test"
	authority.SessionID = tmuxTestCommand(t, socket, "display-message", "-p", "-t", "=vanished:", "#{session_id}")
	authority.SessionCreated, _ = strconv.ParseInt(tmuxTestCommand(t, socket, "display-message", "-p", "-t", "=vanished:", "#{session_created}"), 10, 64)
	paneID := tmuxTestCommand(t, socket, "display-message", "-p", "-t", "=vanished:", "#{pane_id}")
	tmuxTestCommand(t, socket, "kill-pane", "-t", paneID)
	if _, err := guardedSnapshot(server, authority, paneID, 20); !errors.Is(err, errStaleTarget) {
		t.Fatalf("vanished pane error = %v, want stale target", err)
	}
}
