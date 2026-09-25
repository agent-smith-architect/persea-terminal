package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"persea-terminal/internal/proto"
	"persea-terminal/internal/unifiedjournal"
)

// Pin the two capture views on a real private server before using them as
// reconstruction inputs. The pane waits on a file so no terminal input is needed.
func TestUnifiedAlternateTmuxCaptureViews(t *testing.T) {
	f := newAdoptionFixture(t, 4)
	f.startPaneCommand(t, "capture-normal", "exec sleep 600")
	quiet, err := tmuxCombinedOutput(f.disposable.path, "capture-pane", "-a", "-q", "-e", "-p", "-N", "-t", "capture-normal:")
	if err != nil || (string(quiet) != "" && string(quiet) != "\n") {
		t.Fatalf("absent saved screen: %q err=%v", quiet, err)
	}
	t.Logf("absent saved screen quiet response=%q", quiet)
	f.startPaneCommand(t, "capture-alt", `i=0; while [ "$i" -lt 35 ]; do printf 'NORMAL-%02d\r\n' "$i"; i=$((i+1)); done; printf '\033[7;9H\033[?1049h\033[2J\033[HALTERNATE\033[4;6H'; exec sleep 600`)
	pollUntil(t, 5*time.Second, "alternate content", func() bool { return strings.Contains(f.capture(t, "capture-alt"), "ALTERNATE") })
	pane := f.paneID(t, "capture-alt")
	capture := func(flags ...string) []string {
		args := append([]string{"capture-pane", "-p", "-N", "-S", "-", "-E", "-", "-t", pane}, flags...)
		out, err := tmuxCombinedOutput(f.disposable.path, args...)
		if err != nil {
			t.Fatalf("capture: %v: %s", err, out)
		}
		rows := parseAdoptionCapture(string(out))
		for i := range rows {
			rows[i] = strings.TrimRight(rows[i], " ")
		}
		return rows
	}
	active, saved := capture(), capture("-a")
	probe := f.disposable.run("display-message", "-p", "-t", pane, "#{history_size} #{alternate_on} #{alternate_saved_x} #{alternate_saved_y} #{cursor_x} #{cursor_y}")
	if probe != "12 1 8 6 5 3" {
		t.Fatalf("capture facts: %q", probe)
	}
	if len(active) != 36 || len(saved) != 24 {
		t.Fatalf("capture rows active=%d saved=%d", len(active), len(saved))
	}
	for i := 0; i < 12; i++ {
		if active[i] != fmt.Sprintf("NORMAL-%02d", i) {
			t.Fatalf("history row %d: %q", i, active[i])
		}
	}
	if active[12] != "ALTERNATE" || saved[0] != "NORMAL-12" || saved[22] != "NORMAL-34" || saved[23] != "" {
		t.Fatalf("visible screens active=%q saved=%q", active[12:], saved)
	}
	t.Logf("tmux capture: history=12 active rows=%d saved rows=%d; probe=%s", len(active), len(saved), probe)
}

// A read-only capture cannot promise arbitrary saved-grid restoration after
// a shrink: two distinct saved grids have identical public capture inputs.
// Replay must still open both and restore the same explicit approximation.
func TestUnifiedAlternateTmuxSavedGridCaptureLimit(t *testing.T) {
	f := newAdoptionFixture(t, 4)
	leave := filepath.Join(t.TempDir(), "leave")
	var captures, restored []string
	var keys []unifiedjournal.PaneKey
	for index, suffix := range []string{"A", "B"} {
		name := fmt.Sprintf("saved-grid-%d", index)
		text := strings.Repeat("x", 65) + suffix + strings.Repeat("y", 14)
		f.startPaneCommand(t, name, `printf '`+text+`\r\n\033[H\033[?1049h\033[2J\033[HSAME'; while [ ! -f `+shellQuote(leave)+` ]; do sleep 0.02; done; printf '\033[?1049l'; exec sleep 600`)
		pollUntil(t, 5*time.Second, "saved grid ready", func() bool { return strings.Contains(f.capture(t, name), "SAME") })
		f.disposable.run("resize-window", "-t", name+":", "-x", "53", "-y", "24")
		out, err := tmuxCombinedOutput(f.disposable.path, "capture-pane", "-a", "-p", "-e", "-N", "-t", name+":")
		if err != nil {
			t.Fatal(err)
		}
		captures = append(captures, string(out))
		adoption, err := f.effects.AdoptSession(context.Background(), f.disposable.run("display-message", "-p", "-t", name+":", "#{session_id}"))
		if err != nil {
			t.Fatalf("adopt clipped saved screen: %v", err)
		}
		keys = append(keys, adoption.Key)
		assertAlternateReplay(t, f, name, adoption.Key, true)
	}
	if captures[0] != captures[1] || strings.Contains(captures[0], "A") || strings.Contains(captures[1], "B") {
		t.Fatalf("saved captures no longer hide the clipped cells: %q", captures)
	}
	if err := os.WriteFile(leave, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for index := range captures {
		name := fmt.Sprintf("saved-grid-%d", index)
		pollUntil(t, 5*time.Second, "saved grid restored", func() bool {
			return f.disposable.run("display-message", "-p", "-t", name+":", "#{alternate_on}") == "0"
		})
		restored = append(restored, f.capture(t, name))
		wantRestored := make([]string, 24)
		wantRestored[0] = strings.Repeat("x", 12) + []string{"A", "B"}[index] + strings.Repeat("y", 14)
		if restored[index] != strings.Join(wantRestored, "\n")+"\n" {
			t.Fatalf("tmux clipped-cell restoration changed: %q", restored[index])
		}
		pollUntil(t, 5*time.Second, "clipped saved exit committed", func() bool { return bytes.Contains(f.journalBytes(t, keys[index]), []byte("\x1b[?1049l")) })
		replay := replayAlternateJournal(t, f, keys[index])
		// Both indistinguishable captures must restore the same explicit
		// approximation: the exposed 53 cells, then 23 blank hard rows.
		want := make([]string, 24)
		want[0] = strings.Repeat("x", 53)
		for i := range replay.Lines {
			replay.Lines[i] = strings.TrimRight(replay.Lines[i], " ")
		}
		if !reflect.DeepEqual(replay.Lines, want) || replay.Type != "normal" || replay.X != 0 || replay.Y != 0 {
			t.Fatalf("clipped saved display approximation: %+v", replay)
		}
		t.Logf("clipped saved display %d restored by tmux: %q", index, restored[index])
	}
	if restored[0] == restored[1] || !strings.Contains(restored[0], "A") || !strings.Contains(restored[1], "B") {
		t.Fatalf("tmux no longer restores clipped saved cells: %q", restored)
	}
}

func TestUnifiedAlternateWithoutSavedCursor(t *testing.T) {
	f := newAdoptionFixture(t, 4)
	leave := filepath.Join(t.TempDir(), "leave")
	session := f.startPaneCommand(t, "unsaved-cursor", `printf 'NORMAL\033[?1047h\033[2J\033[HALTERNATE\033[4;6H'; while [ ! -f `+shellQuote(leave)+` ]; do sleep 0.02; done; printf '\033[?1047l'; exec sleep 600`)
	pollUntil(t, 5*time.Second, "unsaved alternate ready", func() bool { return strings.Contains(f.capture(t, "unsaved-cursor"), "ALTERNATE") })
	probe := f.disposable.run("display-message", "-p", "-t", session+":", "#{alternate_on} #{alternate_saved_x} #{alternate_saved_y}")
	t.Logf("alternate without cursor save: %s", probe)
	adoption, err := f.effects.AdoptSession(context.Background(), session)
	if err != nil {
		t.Fatalf("adopt alternate without cursor save: %v", err)
	}
	assertAlternateReplay(t, f, session, adoption.Key, true)
	if err := os.WriteFile(leave, nil, 0600); err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 5*time.Second, "unsaved alternate exit committed", func() bool { return bytes.Contains(f.journalBytes(t, adoption.Key), []byte("\x1b[?1047l")) })
	assertAlternateReplay(t, f, session, adoption.Key, false)
}

type alternateReplay struct {
	Lines []string
	X, Y  int
	Type  string
}

func replayAlternateJournal(t *testing.T, f *adoptionFixture, key unifiedjournal.PaneKey) alternateReplay {
	t.Helper()
	f.effects.journalMu.Lock()
	geometry, err := f.effects.realm.InitialGeometry(key)
	f.effects.journalMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return replayAlternateBytes(t, geometry, f.journalBytes(t, key))
}

func replayAlternateBytes(t *testing.T, geometry unifiedjournal.Geometry, data []byte) alternateReplay {
	t.Helper()
	input, err := json.Marshal(struct {
		Columns int    `json:"columns"`
		Rows    int    `json:"rows"`
		Data    []byte `json:"data"`
	}{geometry.Columns, geometry.Rows, data})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", "../../ui/test/alternate_replay_oracle.cjs")
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("headless xterm replay (run npm ci in ui first): %v: %s", err, out)
	}
	var replay alternateReplay
	if err := json.Unmarshal(out, &replay); err != nil {
		t.Fatalf("oracle: %v: %s", err, out)
	}
	return replay
}

func assertAlternateReplay(t *testing.T, f *adoptionFixture, session string, key unifiedjournal.PaneKey, alternate bool) {
	t.Helper()
	replay := replayAlternateJournal(t, f, key)
	args := []string{"capture-pane", "-p", "-t", session + ":"}
	wantType := "alternate"
	if !alternate {
		args = append(args, "-S", "-")
		wantType = "normal"
	}
	truth, err := tmuxCombinedOutput(f.disposable.path, args...)
	if err != nil {
		t.Fatal(err)
	}
	// tmux -N can include allocated trailing blank cells. Compare display text
	// with the same blank-cell normalization on both sides, retaining every row.
	normalize := func(text string) string {
		rows := strings.Split(text, "\n")
		for i := range rows {
			rows[i] = strings.TrimRight(rows[i], " ")
		}
		return strings.Join(rows, "\n")
	}
	if got := strings.Join(replay.Lines, "\n") + "\n"; normalize(got) != normalize(string(truth)) {
		t.Fatalf("replay %s differs:\n xterm=%q\n tmux =%q", wantType, got, truth)
	}
	cursor := f.disposable.run("display-message", "-p", "-t", session+":", "#{cursor_x} #{cursor_y}")
	if replay.Type != wantType || fmt.Sprintf("%d %d", replay.X, replay.Y) != cursor {
		t.Fatalf("replay type=%s cursor=%d,%d want %s %s", replay.Type, replay.X, replay.Y, wantType, cursor)
	}
}

func TestUnifiedAlternateReconstruction(t *testing.T) {
	for _, operation := range []string{"adopt", "rotate", "refit", "refit-shrink"} {
		t.Run(operation, func(t *testing.T) {
			f := newAdoptionFixture(t, 4)
			leave := filepath.Join(t.TempDir(), "leave")
			session := f.startPaneCommand(t, "alternate-replay", `i=0; while [ "$i" -lt 35 ]; do printf 'NORMAL-%02d\r\n' "$i"; i=$((i+1)); done; printf '\033[7;9H\033[?1049h\033[2J\033[H\033[31mALTERNATE\033[0m\033[24;1HBOTTOM\033[4;6H'; while [ ! -f `+shellQuote(leave)+` ]; do sleep 0.02; done; printf '\033[?1049l'; exec sleep 600`)
			pollUntil(t, 5*time.Second, "alternate ready", func() bool { return strings.Contains(f.capture(t, "alternate-replay"), "BOTTOM") })
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			adoption, err := f.effects.AdoptSession(ctx, session)
			if err != nil {
				t.Fatalf("adopt alternate screen: %v", err)
			}
			key := adoption.Key
			if operation == "rotate" {
				if err := f.effects.rotateSession(ctx, session); err != nil {
					t.Fatalf("rotate alternate screen: %v", err)
				}
			} else if strings.HasPrefix(operation, "refit") {
				detail, err := details(f.server, session)
				if err != nil {
					t.Fatal(err)
				}
				authority, err := incarnation(f.server)
				if err != nil {
					t.Fatal(err)
				}
				authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = f.cfg.Realm, f.server.Label, detail.ID, detail.Created
				source, err := buildSourceWitness(ctx, f.server, authority.BootID, session)
				if err != nil {
					t.Fatal(err)
				}
				columns := 97
				if operation == "refit-shrink" {
					columns = 53
				}
				if err := f.effects.refitSession(ctx, authority, source, columns, strings.Repeat("a", 43)); err != nil {
					t.Fatalf("refit alternate screen: %v", err)
				}
			}
			if operation != "adopt" {
				var ok bool
				key, ok = f.effects.paneKey(session)
				if !ok || key == adoption.Key {
					t.Fatal("no successor")
				}
			}
			projection := f.effects.projectSession(f.server.Label, session, unifiedSessionFacts{Alternate: 1, Panes: 1, Windows: 1, Valid: true})
			if projection.State != proto.UnifiedSessionOpen || projection.Detail != "" {
				t.Fatalf("alternate projection: %+v", projection)
			}
			assertAlternateReplay(t, f, session, key, true)
			if err := os.WriteFile(leave, nil, 0600); err != nil {
				t.Fatal(err)
			}
			pollUntil(t, 5*time.Second, "alternate exit committed", func() bool { return bytes.Contains(f.journalBytes(t, key), []byte("\x1b[?1049l")) })
			assertAlternateReplay(t, f, session, key, false)
		})
	}
}

func TestSynthesizeAdoptionAlternateState(t *testing.T) {
	modes := adoptionModes{columns: 8, rows: 2, scrollBottom: 1, wrap: true, cursorVisible: true, tabs: []int{4}}
	active := []string{"HISTORY", "ALT-TOP", "ALT-LAST"}
	saved := adoptionAlternate{rows: []string{"NORMAL", "SAVED"}, cursorX: 2, cursorY: 1}
	bootstrap, trimmed, err := synthesizeAdoptionBootstrap(active, "\x1b[", 3, 0, modes, saved)
	if err != nil || trimmed {
		t.Fatalf("synthesize: trimmed=%t err=%v", trimmed, err)
	}
	want := "\x1b[0mHISTORY\r\nNORMAL\r\nSAVED\x1b[2;3H\x1b[?1049h\x1b[0m\x1b[1;1HALT-TOP\x1b[2;1HALT-LAST\x1b[1;2r"
	if !strings.HasPrefix(string(bootstrap), want) || !strings.HasSuffix(string(bootstrap), "\x1b[1;4H\x1b[") {
		t.Fatalf("two-screen order/pending prefix: %q", bootstrap)
	}
	if active[1] != "ALT-TOP" {
		t.Fatal("synthesis modified capture input")
	}
	for _, resized := range []adoptionAlternate{
		{}, {rows: []string{"short"}}, {rows: []string{"1", "2", "3"}},
		{rows: saved.rows, cursorY: 2}, {rows: saved.rows, cursorX: -1},
		{rows: saved.rows, cursorX: 1<<32 - 1, cursorY: 1<<32 - 1},
	} {
		if _, _, err := synthesizeAdoptionBootstrap(active, "", 0, 0, modes, resized); err != nil {
			t.Fatalf("refused resized saved state: %+v: %v", resized, err)
		}
	}
	if _, _, err := synthesizeAdoptionBootstrap(active[:1], "", 0, 0, modes, saved); err == nil {
		t.Fatal("accepted incomplete alternate display")
	}
	// tmux retains the old saved X across a width shrink. It is legitimate
	// captured state; CUP resolves it within the reconstruction's geometry.
	saved.cursorX = 79
	if _, _, err := synthesizeAdoptionBootstrap(active, "", 0, 0, modes, saved); err != nil {
		t.Fatalf("refused saved cursor from a wider screen: %v", err)
	}
}

func TestSynthesizeAdoptionAlternateCap(t *testing.T) {
	modes := adoptionModes{columns: 80, rows: 2, scrollBottom: 1}
	saved := adoptionAlternate{rows: []string{"NORMAL-TOP", "NORMAL-BOTTOM"}}
	active := []string{strings.Repeat("h", adoptionBootstrapCapBytes), "ALT-TOP", "ALT-BOTTOM"}
	bootstrap, trimmed, err := synthesizeAdoptionBootstrap(active, "", 0, 0, modes, saved)
	if err != nil || !trimmed || len(bootstrap) > adoptionBootstrapCapBytes {
		t.Fatalf("bounded synthesis: bytes=%d trimmed=%t err=%v", len(bootstrap), trimmed, err)
	}
	for _, marker := range []string{"NORMAL-TOP", "NORMAL-BOTTOM", "ALT-TOP", "ALT-BOTTOM"} {
		if !bytes.Contains(bootstrap, []byte(marker)) {
			t.Fatalf("trimmed retained row %s", marker)
		}
	}
	for _, largeSaved := range []bool{false, true} {
		active = []string{"ALT-TOP", "ALT-BOTTOM"}
		saved.rows = []string{"NORMAL-TOP", "NORMAL-BOTTOM"}
		if largeSaved {
			saved.rows[0] = strings.Repeat("s", adoptionBootstrapCapBytes)
		} else {
			active[0] = strings.Repeat("a", adoptionBootstrapCapBytes)
		}
		bootstrap, trimmed, err := synthesizeAdoptionBootstrap(active, "", 0, 0, modes, saved)
		if largeSaved {
			if err != nil || !trimmed || len(bootstrap) > adoptionBootstrapCapBytes {
				t.Fatalf("hidden normal rows blocked reconstruction: trimmed=%t err=%v", trimmed, err)
			}
			for _, marker := range []string{"NORMAL-BOTTOM", "ALT-TOP", "ALT-BOTTOM"} {
				if !bytes.Contains(bootstrap, []byte(marker)) {
					t.Fatalf("trimmed retained row %s", marker)
				}
			}
			geometry := unifiedjournal.Geometry{Columns: 80, Rows: 2}
			visible := replayAlternateBytes(t, geometry, bootstrap)
			if !reflect.DeepEqual(visible.Lines, []string{"ALT-TOP", "ALT-BOTTOM"}) || visible.Type != "alternate" || visible.X != 0 || visible.Y != 0 {
				t.Fatalf("cap damaged alternate display: %+v", visible)
			}
			restored := replayAlternateBytes(t, geometry, append(bootstrap, []byte("\x1b[?1049l")...))
			if !reflect.DeepEqual(restored.Lines, []string{"", "NORMAL-BOTTOM"}) || restored.Type != "normal" || restored.X != 0 || restored.Y != 0 {
				t.Fatalf("cap hidden-row approximation: %+v", restored)
			}
		} else if err == nil {
			t.Fatal("trimmed visible alternate screen instead of refusing")
		}
	}
}

func TestInitialAlternateCaptureDriftAndShape(t *testing.T) {
	probe := "1 2000 3 0 1 1 1 2 1\n"
	responses := []string{"42|control-mode\n", "", probe, "HISTORY\nALT-TOP\nALT-LAST\n", "NORMAL\nSAVED\n", "\x1b[\n", "1 0 0 0 0 1 0 0 0 0 0 0 1 8 2 4\n", probe}
	bootstrap, geometry, _, err := parseInitialCapture(responses, 42, nil)
	if err != nil || geometry.Columns != 8 || geometry.Rows != 2 || !bytes.HasSuffix(bootstrap, []byte("\x1b[")) {
		t.Fatalf("initial alternate: geometry=%+v err=%v", geometry, err)
	}
	for _, field := range []int{0, 2, 3, 4, 7, 8} {
		_, _, _, err := parseInitialCapture(responses, 42, func(post string) string {
			parts := strings.Fields(post)
			parts[field] = "9"
			return strings.Join(parts, " ")
		})
		if !errors.Is(err, errUnifiedAdoptDrift) {
			t.Fatalf("field %d drift accepted: %v", field, err)
		}
	}
	bad := append([]string(nil), responses...)
	bad[3] = "short\n"
	if _, _, _, err := parseInitialCapture(bad, 42, nil); err == nil {
		t.Fatal("accepted incomplete active screen")
	}
}
