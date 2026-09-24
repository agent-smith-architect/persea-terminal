package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

// adoptionFixture drives the real provider against a disposable tmux server
// with the real registry and retention runtime behind it, so adopted bytes
// travel the same path production bytes do: observer unit -> registry router
// -> retention batching -> journal realm -> committed events.
type adoptionFixture struct {
	disposable *disposable
	server     config.TmuxServer
	cfg        config.Broker
	runtimeDir string
	effects    *UnifiedDevPaneEffects
	registry   *paneRegistry
	cancel     context.CancelFunc
}

func newAdoptionFixture(t *testing.T, adoptionSlots int) *adoptionFixture {
	t.Helper()
	disposable := newDisposable(t)
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	runtimeDir := t.TempDir()
	cfg := unifiedAdoptionDevConfig(t, tmuxServer, runtimeDir, adoptionSlots)
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	registry := newPaneRegistry(effects)
	if registry.retention == nil {
		t.Fatal("adoption fixture requires the retained registry path")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = effects.RunObserver(ctx, registry) }()
	fixture := &adoptionFixture{
		disposable: disposable, server: tmuxServer, cfg: cfg, runtimeDir: runtimeDir,
		effects: effects, registry: registry, cancel: cancel,
	}
	t.Cleanup(func() {
		cancel()
		_ = registry.Close()
	})
	return fixture
}

// startPaneCommand creates a session whose only pane runs the given shell
// command directly, so pane output is exactly the command's output — no
// prompt, no typed-echo duplication.
func (fixture *adoptionFixture) startPaneCommand(t *testing.T, name, command string) string {
	t.Helper()
	fixture.disposable.run("new-session", "-d", "-s", name, "-x", "80", "-y", "24", command)
	return fixture.disposable.run("display-message", "-p", "-t", name+":", "#{session_id}")
}

func (fixture *adoptionFixture) paneID(t *testing.T, name string) string {
	t.Helper()
	return fixture.disposable.run("display-message", "-p", "-t", name+":", "#{pane_id}")
}

func (fixture *adoptionFixture) capture(t *testing.T, name string) string {
	t.Helper()
	out, err := tmuxCombinedOutput(fixture.disposable.path, "capture-pane", "-p", "-t", name+":")
	if err != nil {
		t.Fatalf("capture %s: %v %s", name, err, out)
	}
	return string(out)
}

func (fixture *adoptionFixture) journalBytes(t *testing.T, key unifiedjournal.PaneKey) []byte {
	t.Helper()
	fixture.effects.journalMu.Lock()
	events, err := fixture.effects.realm.ReadCommittedEvents(key)
	fixture.effects.journalMu.Unlock()
	if err != nil {
		t.Fatalf("read committed events: %v", err)
	}
	var data []byte
	for _, event := range events {
		if event.Kind == unifiedjournal.RecordOutput {
			data = append(data, event.Payload...)
		}
	}
	return data
}

func (fixture *adoptionFixture) journalFileCount(t *testing.T) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(fixture.runtimeDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "pane-") && strings.HasSuffix(entry.Name(), ".journal") {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk journal runtime: %v", err)
	}
	return count
}

func pollUntil(t *testing.T, limit time.Duration, what string, satisfied func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !satisfied() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var (
	adoptionScrollRegionRE = regexp.MustCompile(`\x1b\[[0-9]+;[0-9]+r`)
	adoptionCursorRE       = regexp.MustCompile(`\x1b\[([0-9]+);([0-9]+)H`)
)

// adoptionBootstrapParts splits a reconstructed bootstrap into its painted
// rows and the synthesis tail. The painted segment cannot contain an
// 'r'-final CSI: capture -e re-encodes cell attributes as SGR ('m'-final)
// only, so the first scroll-region sequence is the synthesis tail's start.
func adoptionBootstrapParts(t *testing.T, bootstrap []byte) (rows []string, tail string) {
	t.Helper()
	text := string(bootstrap)
	if !strings.HasPrefix(text, "\x1b[0m") {
		t.Fatalf("bootstrap does not open with an attribute reset: %q", text[:min(len(text), 16)])
	}
	text = strings.TrimPrefix(text, "\x1b[0m")
	location := adoptionScrollRegionRE.FindStringIndex(text)
	if location == nil {
		t.Fatal("bootstrap carries no scroll-region sequence")
	}
	painted := text[:location[0]]
	if painted == "" {
		return nil, text[location[0]:]
	}
	return strings.Split(painted, "\r\n"), text[location[0]:]
}

// splitAdoptionJournal separates the journal byte projection into the
// synthesized bootstrap and the live tail. The bootstrap's synthesis tail
// carries exactly two CUP sequences after the scroll-region set (position,
// then position restored after tab stops), followed by the pending prefix;
// this helper is valid only for panes whose own output contains no CUP.
func splitAdoptionJournal(t *testing.T, data []byte, pending string) (bootstrap, tail []byte) {
	t.Helper()
	region := adoptionScrollRegionRE.FindIndex(data)
	if region == nil {
		t.Fatal("journal projection carries no bootstrap scroll-region sequence")
	}
	cursors := adoptionCursorRE.FindAllIndex(data[region[1]:], -1)
	if len(cursors) < 2 {
		t.Fatalf("journal projection carries %d cursor sequences after the scroll region, want at least 2", len(cursors))
	}
	boundary := region[1] + cursors[1][1] + len(pending)
	if boundary > len(data) {
		t.Fatal("journal projection ends inside the pending prefix")
	}
	if string(data[boundary-len(pending):boundary]) != pending {
		t.Fatalf("bootstrap does not end with the pending prefix %q", pending)
	}
	return data[:boundary], data[boundary:]
}

// TestUnifiedAdoptionTextEquivalence is text-equivalence coverage plus the
// registration contract: the adopted bootstrap's painted rows equal tmux
// capture-pane ground truth byte for byte (SGR presence included), marker
// parity holds while streaming afterward, the generation is stamped
// reconstructed with the pane's geometry, re-adoption is an idempotent
// success, and the session is registered for guarded commands and
// snapshot+tail.
func TestUnifiedAdoptionTextEquivalence(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux adoption lifecycle")
	}
	fixture := newAdoptionFixture(t, 0)
	sessionID := fixture.startPaneCommand(t, "victim",
		`printf '\033[31mRED\033[0m plain\nsecond line\n'; exec sleep 600`)
	pollUntil(t, 5*time.Second, "pane content", func() bool {
		return strings.Contains(fixture.capture(t, "victim"), "second line")
	})

	adoption, err := fixture.effects.AdoptSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if adoption.Existing || adoption.Trimmed || adoption.SessionID != sessionID {
		t.Fatalf("adoption outcome: %+v", adoption)
	}

	var bootstrap []byte
	pollUntil(t, 5*time.Second, "bootstrap commit", func() bool {
		bootstrap = fixture.journalBytes(t, adoption.Key)
		return strings.Contains(string(bootstrap), "second line")
	})
	rows, _ := adoptionBootstrapParts(t, bootstrap)

	raw, err := tmuxCombinedOutput(fixture.disposable.path,
		"capture-pane", "-e", "-p", "-N", "-S", "-2000", "-E", "-", "-t", fixture.paneID(t, "victim"))
	if err != nil {
		t.Fatalf("ground truth capture: %v %s", err, raw)
	}
	truth := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(rows) != len(truth) {
		t.Fatalf("bootstrap rows=%d ground truth rows=%d", len(rows), len(truth))
	}
	for index := range rows {
		if rows[index] != truth[index] {
			t.Fatalf("row %d diverges:\n journal: %q\n tmux:    %q", index, rows[index], truth[index])
		}
	}

	fixture.effects.journalMu.Lock()
	origin, originErr := fixture.effects.realm.Origin(adoption.Key)
	geometry, geometryErr := fixture.effects.realm.InitialGeometry(adoption.Key)
	fixture.effects.journalMu.Unlock()
	if originErr != nil || origin != unifiedjournal.OriginReconstructed {
		t.Fatalf("origin=%q err=%v", origin, originErr)
	}
	if geometryErr != nil || geometry != (unifiedjournal.Geometry{Columns: 80, Rows: 24}) {
		t.Fatalf("geometry=%+v err=%v", geometry, geometryErr)
	}

	again, err := fixture.effects.AdoptSession(context.Background(), sessionID)
	if err != nil || !again.Existing || again.Key != adoption.Key {
		t.Fatalf("re-adoption is not idempotent: %+v err=%v", again, err)
	}

	fixture.effects.mu.Lock()
	_, unitRegistered := fixture.effects.units[sessionID]
	fixture.effects.mu.Unlock()
	if !unitRegistered {
		t.Fatal("adopted session has no registered unit for guarded commands")
	}
	events, initial, tailChannel, cancelTail, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil || len(events) == 0 || initial != (unifiedjournal.Geometry{Columns: 80, Rows: 24}) {
		t.Fatalf("snapshot+tail: events=%d initial=%+v err=%v", len(events), initial, err)
	}
	defer cancelTail()
	_ = tailChannel

	fixture.disposable.run("send-keys", "-t", "victim:", "PARITY_MARKER_XYZ", "Enter")
	pollUntil(t, 5*time.Second, "streamed marker", func() bool {
		return strings.Contains(string(fixture.journalBytes(t, adoption.Key)), "PARITY_MARKER_XYZ")
	})
	full := string(fixture.journalBytes(t, adoption.Key))
	if strings.Count(full, "PARITY_MARKER_XYZ") != 1 {
		t.Fatalf("streamed marker parity broken: %d occurrences", strings.Count(full, "PARITY_MARKER_XYZ"))
	}
	if strings.Contains(string(bootstrap), "PARITY_MARKER_XYZ") {
		t.Fatal("post-adoption marker leaked into the bootstrap")
	}
}

// TestUnifiedAdoptionBoundaryMarkerConservation verifies: sequential
// markers stream across the adoption boundary and every marker survives
// exactly once — pre-boundary markers only through the capture, post-boundary
// markers only through the tail — in set equality with tmux's own capture as
// ground truth. A marker whose bytes straddle the boundary itself is counted
// through the seam join of the cursor row's text with the tail.
func TestUnifiedAdoptionBoundaryMarkerConservation(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux adoption lifecycle")
	}
	fixture := newAdoptionFixture(t, 0)
	const total = 300
	sessionID := fixture.startPaneCommand(t, "feed",
		`i=0; while [ "$i" -lt 300 ]; do i=$((i+1)); printf 'MRK_%04d\n' "$i"; sleep 0.01; done; exec sleep 600`)
	pollUntil(t, 10*time.Second, "the stream to start", func() bool {
		return strings.Contains(fixture.capture(t, "feed"), "MRK_0020")
	})

	adoption, err := fixture.effects.AdoptSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("adopt mid-stream: %v", err)
	}

	marker := regexp.MustCompile(`MRK_[0-9]{4}`)
	pollUntil(t, 60*time.Second, "the stream to finish", func() bool {
		return strings.Contains(fixture.capture(t, "feed"), fmt.Sprintf("MRK_%04d", total))
	})
	pollUntil(t, 10*time.Second, "the journal to catch up", func() bool {
		return strings.Contains(string(fixture.journalBytes(t, adoption.Key)), fmt.Sprintf("MRK_%04d", total))
	})

	raw, err := tmuxCombinedOutput(fixture.disposable.path,
		"capture-pane", "-p", "-N", "-S", "-", "-E", "-", "-t", fixture.paneID(t, "feed"))
	if err != nil {
		t.Fatalf("ground truth capture: %v %s", err, raw)
	}
	truth := map[string]int{}
	for _, hit := range marker.FindAllString(string(raw), -1) {
		truth[hit]++
	}
	if len(truth) != total {
		t.Fatalf("ground truth holds %d distinct markers, want %d", len(truth), total)
	}
	for hit, count := range truth {
		if count != 1 {
			t.Fatalf("ground truth marker %s appears %d times", hit, count)
		}
	}

	data := fixture.journalBytes(t, adoption.Key)
	bootstrap, tail := splitAdoptionJournal(t, data, "")
	rows, synthesisTail := adoptionBootstrapParts(t, bootstrap)

	// The seam join: the cursor row's text up to the cursor column, glued to
	// the tail, completes a marker whose bytes straddle the boundary. Matches
	// that end inside the fragment already live whole in the painted rows and
	// are not counted twice.
	cursors := adoptionCursorRE.FindAllStringSubmatch(synthesisTail, -1)
	if len(cursors) < 2 {
		t.Fatalf("synthesis tail carries %d cursor sequences, want 2", len(cursors))
	}
	cursorRow, cursorColumn := 0, 0
	if _, err := fmt.Sscanf(cursors[len(cursors)-1][1]+" "+cursors[len(cursors)-1][2], "%d %d", &cursorRow, &cursorColumn); err != nil {
		t.Fatalf("cursor parse: %v", err)
	}
	screenOffset := len(rows) - 24
	if screenOffset < 0 {
		screenOffset = 0
	}
	fragment := ""
	if index := screenOffset + cursorRow - 1; index >= 0 && index < len(rows) {
		row := rows[index]
		end := cursorColumn - 1
		if end > len(row) {
			end = len(row)
		}
		if end > 0 {
			fragment = row[:end]
		}
	}

	journal := map[string]int{}
	for _, hit := range marker.FindAllString(strings.Join(rows, "\n"), -1) {
		journal[hit]++
	}
	seam := fragment + string(tail)
	for _, location := range marker.FindAllStringIndex(seam, -1) {
		if location[1] > len(fragment) {
			journal[seam[location[0]:location[1]]]++
		}
	}

	for hit := range truth {
		if journal[hit] != 1 {
			t.Fatalf("marker %s crossed the boundary %d times, want exactly once", hit, journal[hit])
		}
	}
	for hit := range journal {
		if truth[hit] == 0 {
			t.Fatalf("journal invented marker %s", hit)
		}
	}
}

// TestUnifiedAdoptionSplitSequenceSeam verifies: adoption taken while
// the pane is stalled mid-escape-sequence must journal the pending parser
// prefix as the bootstrap's last bytes, and the pane's continuation must land
// immediately after it, so the joined bytes form the complete valid sequence
// at the seam.
func TestUnifiedAdoptionSplitSequenceSeam(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux adoption lifecycle")
	}
	cases := []struct {
		name     string
		command  string
		pending  string
		joined   string
		finished string
	}{
		{
			name:     "mid-csi",
			command:  `printf 'X\033[38;5;14'; sleep 10; printf '1mCOLORED\033[0m\n'; exec sleep 600`,
			pending:  "\x1b[38;5;14",
			joined:   "\x1b[38;5;141mCOLORED",
			finished: "COLORED",
		},
		{
			name:     "mid-osc",
			command:  `printf 'Y\033]0;titlesta'; sleep 10; printf 'rt\007DONE\n'; exec sleep 600`,
			pending:  "\x1b]0;titlesta",
			joined:   "\x1b]0;titlestart\x07DONE",
			finished: "DONE",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAdoptionFixture(t, 0)
			sessionID := fixture.startPaneCommand(t, "stall", testCase.command)
			pollUntil(t, 5*time.Second, "the pane to stall mid-sequence", func() bool {
				out, err := tmuxCombinedOutput(fixture.disposable.path, "capture-pane", "-p", "-P", "-t", "stall:")
				return err == nil && strings.TrimSuffix(string(out), "\n") == testCase.pending
			})

			adoption, err := fixture.effects.AdoptSession(context.Background(), sessionID)
			if err != nil {
				t.Fatalf("adopt during the stall: %v", err)
			}

			var bootstrap []byte
			pollUntil(t, 5*time.Second, "bootstrap commit", func() bool {
				bootstrap = fixture.journalBytes(t, adoption.Key)
				return len(bootstrap) != 0
			})
			if !bytes.HasSuffix(bootstrap, []byte(testCase.pending)) {
				t.Fatalf("bootstrap does not end with the pending prefix %q", testCase.pending)
			}

			pollUntil(t, 20*time.Second, "the sequence to complete", func() bool {
				return strings.Contains(string(fixture.journalBytes(t, adoption.Key)), testCase.finished)
			})
			data := string(fixture.journalBytes(t, adoption.Key))
			if !strings.Contains(data, testCase.joined) {
				t.Fatalf("the seam did not join into the complete sequence %q", testCase.joined)
			}
			if strings.Count(data, testCase.pending) != 1 {
				t.Fatalf("the pending prefix appears %d times, want exactly once", strings.Count(data, testCase.pending))
			}
		})
	}
}

// TestUnifiedAdoptionRefusalsAreTypedAndArtifactFree verifies: every
// ineligible target refuses with its own typed error and leaves no journal
// file, no active projection, no in-progress marker, and no registered unit.
func TestUnifiedAdoptionRefusalsAreTypedAndArtifactFree(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux adoption lifecycle")
	}
	fixture := newAdoptionFixture(t, 0)

	assertRefusal := func(t *testing.T, sessionID string, want error) {
		t.Helper()
		before := fixture.journalFileCount(t)
		_, err := fixture.effects.AdoptSession(context.Background(), sessionID)
		if !errors.Is(err, want) {
			t.Fatalf("refusal type: got %v want %v", err, want)
		}
		if after := fixture.journalFileCount(t); after != before {
			t.Fatalf("refusal left journal artifacts: %d -> %d files", before, after)
		}
		fixture.effects.mu.Lock()
		_, active := fixture.effects.active[sessionID]
		fixture.effects.mu.Unlock()
		if active {
			t.Fatal("refused session is journal-active")
		}
		pollUntil(t, 5*time.Second, "the in-progress marker to clear", func() bool {
			fixture.effects.mu.Lock()
			defer fixture.effects.mu.Unlock()
			_, adopting := fixture.effects.adopting[sessionID]
			_, unit := fixture.effects.units[sessionID]
			return !adopting && !unit
		})
	}

	t.Run("alternate-screen", func(t *testing.T) {
		sessionID := fixture.startPaneCommand(t, "altscreen", `printf '\033[?1049h'; exec sleep 600`)
		pollUntil(t, 5*time.Second, "the alternate screen", func() bool {
			return fixture.disposable.run("display-message", "-p", "-t", "altscreen:", "#{alternate_on}") == "1"
		})
		assertRefusal(t, sessionID, ErrUnifiedAdoptAlternateScreen)
	})

	t.Run("multi-pane", func(t *testing.T) {
		sessionID := fixture.startPaneCommand(t, "twopane", "sleep 600")
		fixture.disposable.run("split-window", "-t", "twopane:", "sleep 600")
		assertRefusal(t, sessionID, ErrUnifiedAdoptMultiPane)
	})

	t.Run("multi-window", func(t *testing.T) {
		sessionID := fixture.startPaneCommand(t, "twowin", "sleep 600")
		fixture.disposable.run("new-window", "-t", "twowin:", "sleep 600")
		assertRefusal(t, sessionID, ErrUnifiedAdoptMultiWindow)
	})

	t.Run("missing-session", func(t *testing.T) {
		assertRefusal(t, "$4242", ErrUnifiedAdoptSessionMissing)
	})

	t.Run("in-progress", func(t *testing.T) {
		sessionID := fixture.startPaneCommand(t, "busy", "sleep 600")
		fixture.effects.mu.Lock()
		fixture.effects.adopting[sessionID] = struct{}{}
		fixture.effects.mu.Unlock()
		_, err := fixture.effects.AdoptSession(context.Background(), sessionID)
		if !errors.Is(err, ErrUnifiedAdoptInProgress) {
			t.Fatalf("concurrent adoption type: %v", err)
		}
		fixture.effects.mu.Lock()
		delete(fixture.effects.adopting, sessionID)
		fixture.effects.mu.Unlock()
	})

	t.Run("exhausted-slots", func(t *testing.T) {
		scarce := newAdoptionFixture(t, 2)
		firstID := scarce.startPaneCommand(t, "first", "sleep 600")
		if _, err := scarce.effects.AdoptSession(context.Background(), firstID); err != nil {
			t.Fatalf("first adoption within the slot budget: %v", err)
		}
		secondID := scarce.startPaneCommand(t, "second", "sleep 600")
		before := scarce.journalFileCount(t)
		_, err := scarce.effects.AdoptSession(context.Background(), secondID)
		if !errors.Is(err, ErrUnifiedAdoptSlotsExhausted) {
			t.Fatalf("slot exhaustion type: %v", err)
		}
		if after := scarce.journalFileCount(t); after != before {
			t.Fatalf("slot refusal created journal artifacts: %d -> %d files", before, after)
		}
	})
}

// TestUnifiedAdoptionCompositeAtomicityUnderFlood verifies: under
// unthrottled pane flood, repeated adoption composites classify ZERO %output
// events inside any submission span — the measured tmux 3.4 atomicity as a
// regression gate — and the PRE!=POST retry path is exercisable through the
// test seam without real version drift.
func TestUnifiedAdoptionCompositeAtomicityUnderFlood(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux adoption flood")
	}
	const floodCommand = `while :; do echo FLOOD_LINE_OF_CONTINUOUS_OUTPUT; done`

	t.Run("flood", func(t *testing.T) {
		fixture := newAdoptionFixture(t, 64)
		const composites = 25
		for index := 0; index < composites; index++ {
			name := fmt.Sprintf("flood%02d", index)
			sessionID := fixture.startPaneCommand(t, name, floodCommand)
			adoption, err := fixture.effects.AdoptSession(context.Background(), sessionID)
			if err != nil {
				t.Fatalf("adoption %d under flood: %v", index, err)
			}
			if adoption.Existing {
				t.Fatalf("adoption %d reported an existing generation", index)
			}
			// The flood is killed right after its capture so the journal realm
			// never accumulates 25 live floods; the span counter has already
			// judged the composite by then.
			fixture.disposable.run("kill-session", "-t", sessionID)
		}
		if outputs := fixture.effects.adoptionSpanOutputs.Load(); outputs != 0 {
			t.Fatalf("%d %%output events were classified inside adoption submission spans, want 0", outputs)
		}
		if retries := fixture.effects.adoptionRetries.Load(); retries != 0 {
			t.Fatalf("%d PRE!=POST retries under flood, want 0", retries)
		}
	})

	t.Run("tamper-retry", func(t *testing.T) {
		fixture := newAdoptionFixture(t, 0)
		fixture.effects.adoptionPostTamper = func(attempt int, post string) string {
			if attempt == 1 {
				return post + " tampered"
			}
			return post
		}
		sessionID := fixture.startPaneCommand(t, "tampered", floodCommand)
		adoption, err := fixture.effects.AdoptSession(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("adoption through the tampered first attempt: %v", err)
		}
		if retries := fixture.effects.adoptionRetries.Load(); retries != 1 {
			t.Fatalf("tampered POST caused %d retries, want exactly 1", retries)
		}
		if outputs := fixture.effects.adoptionSpanOutputs.Load(); outputs != 0 {
			t.Fatalf("%d %%output events inside spans across the retry, want 0", outputs)
		}
		pollUntil(t, 5*time.Second, "the retried bootstrap commit", func() bool {
			return len(fixture.journalBytes(t, adoption.Key)) != 0
		})
	})
}

// TestUnifiedAdoptionRestartFailsClosedAndReadoptable is restart coverage in the broker
// half: after a simulated broker restart over a surviving runtime directory,
// the adopted generation is never resumed as live — it reopens fail-closed and
// origin-honest — the provider starts with no active sessions, and the same
// session adopts again into a FRESH generation past the stale one.
func TestUnifiedAdoptionRestartFailsClosedAndReadoptable(t *testing.T) {
	runUnifiedAdoptionRestartReadoptable(t, 0)
}

// TestUnifiedAdoptionRestartOneSlotReadoptable is the one-slot restart regression test
// pinned as a regression gate: with exactly one ordinary-admission slot plus
// the rotation reserve, the stale reconstructed generation a restart leaves
// behind must not keep the ordinary slot charged — the same session re-adopts
// on the first attempt, or F1 has regressed into "unified adoption slots are
// exhausted".
func TestUnifiedAdoptionRestartOneSlotReadoptable(t *testing.T) {
	runUnifiedAdoptionRestartReadoptable(t, 2)
}

func runUnifiedAdoptionRestartReadoptable(t *testing.T, adoptionSlots int) {
	t.Helper()
	if testing.Short() {
		t.Skip("real tmux adoption lifecycle")
	}
	disposable := newDisposable(t)
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	runtimeDir := t.TempDir()
	cfg := unifiedAdoptionDevConfig(t, tmuxServer, runtimeDir, adoptionSlots)

	first, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstRegistry := newPaneRegistry(first)
	firstCtx, firstCancel := context.WithCancel(context.Background())
	go func() { _ = first.RunObserver(firstCtx, firstRegistry) }()

	disposable.run("new-session", "-d", "-s", "survivor", "-x", "80", "-y", "24",
		`printf 'RESTART_MARKER\n'; exec sleep 600`)
	sessionID := disposable.run("display-message", "-p", "-t", "survivor:", "#{session_id}")
	adoption, err := first.AdoptSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	pollUntil(t, 5*time.Second, "the first bootstrap commit", func() bool {
		first.journalMu.Lock()
		events, readErr := first.realm.ReadCommittedEvents(adoption.Key)
		first.journalMu.Unlock()
		return readErr == nil && len(events) != 0
	})

	// The simulated restart: the run dies, the runtime directory survives.
	firstCancel()
	if err := firstRegistry.Close(); err != nil {
		t.Fatalf("close first registry: %v", err)
	}
	first.journalMu.Lock()
	_ = first.realm.Close()
	first.journalMu.Unlock()

	second, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.active) != 0 {
		t.Fatalf("restarted provider resumed %d active sessions, want none", len(second.active))
	}
	second.journalMu.Lock()
	eligible := second.realm.UnifiedEligible(adoption.Key)
	origin, originErr := second.realm.Origin(adoption.Key)
	second.journalMu.Unlock()
	if eligible {
		t.Fatal("a reopened adopted generation is unified-eligible: stale resume")
	}
	if originErr != nil || origin != unifiedjournal.OriginReconstructed {
		t.Fatalf("reopened origin=%q err=%v", origin, originErr)
	}

	secondRegistry := newPaneRegistry(second)
	secondCtx, secondCancel := context.WithCancel(context.Background())
	defer secondCancel()
	go func() { _ = second.RunObserver(secondCtx, secondRegistry) }()
	t.Cleanup(func() { _ = secondRegistry.Close() })

	readopted, err := second.AdoptSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("re-adoption after restart: %v", err)
	}
	if readopted.Existing || readopted.Key == adoption.Key {
		t.Fatalf("re-adoption did not mint a fresh generation: %+v vs %+v", readopted.Key, adoption.Key)
	}
	if readopted.Key.ControlGeneration <= adoption.Key.ControlGeneration {
		t.Fatalf("re-adoption generation %d did not move past the stale %d",
			readopted.Key.ControlGeneration, adoption.Key.ControlGeneration)
	}
	pollUntil(t, 5*time.Second, "the re-adopted bootstrap commit", func() bool {
		second.journalMu.Lock()
		events, readErr := second.realm.ReadCommittedEvents(readopted.Key)
		second.journalMu.Unlock()
		if readErr != nil {
			return false
		}
		var data []byte
		for _, event := range events {
			data = append(data, event.Payload...)
		}
		return strings.Contains(string(data), "RESTART_MARKER")
	})
}

// adoptionObserverTamper wraps the real registry router so a test can fail
// exactly one adoption step — registry admission or the bootstrap injection —
// on the production path without touching product code. An armed failure
// fires once; everything else is forwarded to the real registry.
type adoptionObserverTamper struct {
	inner *paneRegistry

	mu         sync.Mutex
	failAdmit  error
	failOutput error
}

func (tamper *adoptionObserverTamper) recordingRegistry() *paneRegistry { return tamper.inner }
func (tamper *adoptionObserverTamper) publishInitial(op *recordingInitialOperation) error {
	return tamper.inner.publishInitial(op)
}
func (tamper *adoptionObserverTamper) beginInitial(kind recordingInitialKind, witness controlmode.PaneWitness, source terminal.SourceWitness, geometry unifiedjournal.Geometry, payload []byte) (*recordingInitialOperation, error) {
	tamper.mu.Lock()
	err := tamper.failOutput
	tamper.failOutput = nil
	tamper.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return tamper.inner.beginInitial(kind, witness, source, geometry, payload)
}

func (tamper *adoptionObserverTamper) AdmitPane(witness controlmode.PaneWitness) error {
	tamper.mu.Lock()
	err := tamper.failAdmit
	tamper.failAdmit = nil
	tamper.mu.Unlock()
	if err != nil {
		return err
	}
	return tamper.inner.AdmitPane(witness)
}

func (tamper *adoptionObserverTamper) ObservePane(observation controlmode.Observation) error {
	if observation.Kind == controlmode.ObservationOutput {
		tamper.mu.Lock()
		err := tamper.failOutput
		tamper.failOutput = nil
		tamper.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return tamper.inner.ObservePane(observation)
}

func newTamperedAdoptionFixture(t *testing.T, adoptionSlots int) (*adoptionFixture, *adoptionObserverTamper) {
	t.Helper()
	disposable := newDisposable(t)
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	runtimeDir := t.TempDir()
	cfg := unifiedAdoptionDevConfig(t, tmuxServer, runtimeDir, adoptionSlots)
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	registry := newPaneRegistry(effects)
	if registry.retention == nil {
		t.Fatal("adoption fixture requires the retained registry path")
	}
	tamper := &adoptionObserverTamper{inner: registry}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = effects.RunObserver(ctx, tamper) }()
	fixture := &adoptionFixture{
		disposable: disposable, server: tmuxServer, cfg: cfg, runtimeDir: runtimeDir,
		effects: effects, registry: registry, cancel: cancel,
	}
	t.Cleanup(func() {
		cancel()
		_ = registry.Close()
	})
	return fixture, tamper
}

// TestUnifiedAdoptionFailureReleasesSlot is F1's failed-first-adoption pin:
// when registry admission or the bootstrap injection fails AFTER the journal
// reservation is taken, the reservation is aborted — the single ordinary
// slot returns to the ledger while the rotation reserve stays protected, no
// journal artifact survives, and the SAME session then adopts successfully.
func TestUnifiedAdoptionFailureReleasesSlot(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux adoption lifecycle")
	}
	for _, vector := range []struct {
		name string
		arm  func(tamper *adoptionObserverTamper, err error)
	}{
		{name: "registry-admission", arm: func(tamper *adoptionObserverTamper, err error) {
			tamper.mu.Lock()
			tamper.failAdmit = err
			tamper.mu.Unlock()
		}},
		{name: "bootstrap-injection", arm: func(tamper *adoptionObserverTamper, err error) {
			tamper.mu.Lock()
			tamper.failOutput = err
			tamper.mu.Unlock()
		}},
	} {
		t.Run(vector.name, func(t *testing.T) {
			fixture, tamper := newTamperedAdoptionFixture(t, 2)
			sessionID := fixture.startPaneCommand(t, "leaky", `printf 'LEAK_MARKER\n'; exec sleep 600`)
			injected := errors.New("injected adoption failure")
			vector.arm(tamper, injected)
			if _, err := fixture.effects.AdoptSession(context.Background(), sessionID); !errors.Is(err, injected) {
				t.Fatalf("tampered adoption error=%v, want the injected failure", err)
			}
			fixture.effects.journalMu.Lock()
			available := fixture.effects.realm.AvailableCompletePaneSlots()
			fixture.effects.journalMu.Unlock()
			if available != 1 {
				t.Fatalf("failed adoption left availability=%d want 1", available)
			}
			if count := fixture.journalFileCount(t); count != 0 {
				t.Fatalf("failed adoption left %d journal files, want 0", count)
			}
			// The same session adopts cleanly on the recovered slot. The dead
			// unit's reap can still be in flight, so in-progress refusals are
			// retried until it lands.
			var adoption UnifiedAdoption
			pollUntil(t, 5*time.Second, "re-adoption on the recovered slot", func() bool {
				result, err := fixture.effects.AdoptSession(context.Background(), sessionID)
				if errors.Is(err, ErrUnifiedAdoptInProgress) {
					return false
				}
				if err != nil {
					t.Fatalf("re-adoption after tampered failure: %v", err)
				}
				adoption = result
				return true
			})
			pollUntil(t, 5*time.Second, "the re-adopted bootstrap commit", func() bool {
				return bytes.Contains(fixture.journalBytes(t, adoption.Key), []byte("LEAK_MARKER"))
			})
		})
	}
}

// TestSynthesizeAdoptionBootstrapOrderPin pins the synthesis emit order byte
// for byte: attribute reset, rows with CRLF between and none after the last,
// DECSTBM, then DECOM, then region-relative CUP (origin mode on), the mode
// set, tab stops via CHA+HTS, the cursor restored after the tab movement, and
// the pending prefix LAST.
func TestSynthesizeAdoptionBootstrapOrderPin(t *testing.T) {
	modes := adoptionModes{
		cursorVisible: true, insert: false, keypadCursor: true, keypad: false,
		origin: true, wrap: false,
		mouseStandard: false, mouseButton: false, mouseAll: true, mouseUTF8: false, mouseSGR: true,
		scrollTop: 2, scrollBottom: 20, columns: 80, rows: 24,
		tabs: []int{4, 9},
	}
	bootstrap, trimmed, err := synthesizeAdoptionBootstrap([]string{"alpha", "beta"}, "\x1b]0;t", 3, 5, modes)
	if err != nil || trimmed {
		t.Fatalf("synthesis: trimmed=%t err=%v", trimmed, err)
	}
	expected := "\x1b[0m" +
		"alpha\r\nbeta" +
		"\x1b[3;21r" +
		"\x1b[?6h" +
		"\x1b[4;4H" +
		"\x1b[?7l" + "\x1b[?25h" + "\x1b[4l" + "\x1b[?1h" + "\x1b>" +
		"\x1b[?1000l" + "\x1b[?1002l" + "\x1b[?1003h" + "\x1b[?1005l" + "\x1b[?1006h" +
		"\x1b[3g" + "\x1b[5G\x1bH" + "\x1b[10G\x1bH" +
		"\x1b[4;4H" +
		"\x1b]0;t"
	if string(bootstrap) != expected {
		t.Fatalf("synthesis order diverged:\n got:  %q\n want: %q", bootstrap, expected)
	}
}

// TestSynthesizeAdoptionBootstrapTrimsOldestHistoryFirst pins the bootstrap
// cap: oldest history rows are trimmed first, the visible screen is never
// trimmed, and the trim is reported.
func TestSynthesizeAdoptionBootstrapTrimsOldestHistoryFirst(t *testing.T) {
	modes := adoptionModes{cursorVisible: true, wrap: true, scrollTop: 0, scrollBottom: 23, columns: 80, rows: 24}
	rows := make([]string, 2100)
	for index := range rows {
		rows[index] = fmt.Sprintf("row%05d%s", index, strings.Repeat("a", 1000))
	}
	bootstrap, trimmed, err := synthesizeAdoptionBootstrap(rows, "", 0, 23, modes)
	if err != nil {
		t.Fatalf("synthesis: %v", err)
	}
	if !trimmed {
		t.Fatal("an over-cap bootstrap was not reported as trimmed")
	}
	if len(bootstrap) > adoptionBootstrapCapBytes {
		t.Fatalf("bootstrap is %d bytes, cap is %d", len(bootstrap), adoptionBootstrapCapBytes)
	}
	text := string(bootstrap)
	if strings.Contains(text, "row00000") {
		t.Fatal("the oldest history row survived the trim")
	}
	for index := 2100 - 24; index < 2100; index++ {
		if !strings.Contains(text, fmt.Sprintf("row%05d", index)) {
			t.Fatalf("screen row %d was trimmed", index)
		}
	}
}
