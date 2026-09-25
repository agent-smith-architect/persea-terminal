package broker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// All dimensions and coordinates here are measured against real tmux. The
// saved display stays 80x24 while the active display takes the new geometry.
func TestUnifiedAlternateResizedSavedScreen(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		columns, rows, savedX, savedY int
		wantHistory, wantX, wantY     int
		approximateCursor             bool
	}{
		{"height-shrink", 80, 12, 8, 6, 12, 8, 6, false},
		{"height-shrink-bottom", 80, 12, 8, 23, 24, 8, 11, false},
		{"height-growth", 80, 30, 8, 6, 6, 8, 12, false},
		{"height-growth-blanks", 80, 40, 8, 6, 0, 8, 18, false},
		{"width-shrink", 53, 24, 8, 6, 12, 8, 6, false},
		{"width-growth", 97, 24, 8, 6, 12, 8, 6, false},
		// The cursor sits in unallocated blank cells. Width reflow snaps it
		// to the empty line's end in tmux; captures cannot expose allocation
		// or original width. Reconstruction explicitly clamps to (52,11).
		{"saved-cursor-outside", 53, 12, 79, 23, 24, 0, 11, true},
	} {
		for _, operation := range []string{"adopt", "rotate", "refit"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				f := newAdoptionFixture(t, 4)
				leave := filepath.Join(t.TempDir(), "leave")
				command := fmt.Sprintf(`i=0; while [ "$i" -lt 35 ]; do printf 'NORMAL-%%02d\r\n' "$i"; i=$((i+1)); done; printf '\033[%d;%dH\033[?1049h\033[2J\033[HALTERNATE\033[10;1HACTIVE-TENTH\033[4;6H'; while [ ! -f %s ]; do sleep 0.02; done; printf '\033[?1049l'; exec sleep 600`, tc.savedY+1, tc.savedX+1, shellQuote(leave))
				session := f.startPaneCommand(t, "resized", command)
				pollUntil(t, 5*time.Second, "alternate ready", func() bool { return strings.Contains(f.capture(t, session), "ACTIVE-TENTH") })
				columns := tc.columns
				if operation == "refit" {
					columns--
				}
				f.disposable.run("resize-window", "-t", session+":", "-x", strconv.Itoa(columns), "-y", strconv.Itoa(tc.rows))
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				adoption, err := f.effects.AdoptSession(ctx, session)
				if err != nil {
					t.Fatalf("adopt resized alternate: %v", err)
				}
				key := adoption.Key
				if operation == "rotate" {
					if err := f.effects.rotateSession(ctx, session); err != nil {
						t.Fatalf("rotate resized alternate: %v", err)
					}
				} else if operation == "refit" {
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
					if err := f.effects.refitSessionGeometry(ctx, authority, source, tc.columns, tc.rows, strings.Repeat("b", 43)); err != nil {
						t.Fatalf("refit resized alternate: %v", err)
					}
				}
				if operation != "adopt" {
					var ok bool
					key, ok = f.effects.paneKey(session)
					if !ok || key == adoption.Key {
						t.Fatal("no successor")
					}
				}
				assertAlternateReplay(t, f, session, key, true)
				saved, err := tmuxCombinedOutput(f.disposable.path, "capture-pane", "-a", "-p", "-t", session+":")
				if err != nil {
					t.Fatal(err)
				}
				if len(parseAdoptionCapture(string(saved))) != 24 {
					t.Fatal("saved height changed")
				}
				if err := os.WriteFile(leave, nil, 0600); err != nil {
					t.Fatal(err)
				}
				pollUntil(t, 5*time.Second, "exit committed", func() bool { return bytes.Contains(f.journalBytes(t, key), []byte("\x1b[?1049l")) })
				facts := f.disposable.run("display-message", "-p", "-t", session+":", "#{history_size} #{cursor_x} #{cursor_y}")
				wantFacts := fmt.Sprintf("%d %d %d", tc.wantHistory, tc.wantX, tc.wantY)
				if facts != wantFacts {
					t.Fatalf("tmux restore facts=%s want %s", facts, wantFacts)
				}
				t.Logf("saved=80x24 current=%dx%d restored history,x,y=%s", tc.columns, tc.rows, facts)
				if !tc.approximateCursor {
					assertAlternateReplay(t, f, session, key, false)
				} else {
					replay := replayAlternateJournal(t, f, key)
					truth, err := tmuxCombinedOutput(f.disposable.path, "capture-pane", "-p", "-S", "-", "-t", session+":")
					if err != nil {
						t.Fatal(err)
					}
					want := parseAdoptionCapture(string(truth))
					for i := range want {
						want[i] = strings.TrimRight(want[i], " ")
					}
					for i := range replay.Lines {
						replay.Lines[i] = strings.TrimRight(replay.Lines[i], " ")
					}
					if !reflect.DeepEqual(replay.Lines, want) || replay.Type != "normal" || replay.X != 52 || replay.Y != 11 {
						t.Fatalf("clamped saved cursor approximation: %+v", replay)
					}
				}
			})
		}
	}
}
