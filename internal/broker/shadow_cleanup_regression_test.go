package broker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInventoryMarkerData(t *testing.T) {
	for _, value := range []string{"plain", "noise\tmarker", "noise\nmarker"} {
		for _, limit := range []int{10, 1, 0} {
			t.Run(fmt.Sprintf("%q/limit=%d", value, limit), func(t *testing.T) {
				d := newDisposable(t)
				d.tmux.Label = "main"
				d.run("new-session", "-d", "-s", "z-last", "sleep 600")
				for _, name := range []string{"alpha", "z-last"} {
					d.run("set-option", "-t", name, "@persea_client_id", value)
				}
				s := &Server{config: bootCreationConfig(t, d.tmux)}
				got := s.inventoryServer(d.tmux, limit)
				want := min(limit, 2)
				if got.Status != "ok" || !got.CanCreate || len(got.Sessions) != want {
					t.Fatalf("ordinary session marker data poisoned inventory: %+v", got)
				}
			})
		}
	}
}

func TestInventoryIgnoresMalformedRowsBeyondLimit(t *testing.T) {
	for _, limit := range []int{0, 1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			d := newDisposable(t)
			d.tmux.Label = "main"
			if _, err := incarnation(d.tmux); err != nil {
				t.Fatal(err)
			}
			installShadowTmuxWrapper(t, `if args[4] == 'list-sessions' and 'window_width' in args[-1]:
    result = subprocess.run([tmux, *args], check=True, stdout=subprocess.PIPE)
    sys.stdout.buffer.write(result.stdout)
    print('$99\tignored\tinvalid\t24\t0\t1\t1\t0\t1\t1\t1\t0\t0\t0')
    print('unparseable')
    sys.exit(0)
`)
			got := (&Server{config: bootCreationConfig(t, d.tmux)}).inventoryServer(d.tmux, limit)
			if got.Status != "ok" || !got.CanCreate || len(got.Sessions) != limit {
				t.Fatalf("row beyond limit poisoned inventory: %+v", got)
			}
		})
	}
}

func TestShadowCleanupRaceOutcomes(t *testing.T) {
	for _, admission := range []bool{false, true} {
		for _, change := range []string{"protected", "missing", "transport", "server", "stale"} {
			t.Run(fmt.Sprintf("admission=%t/%s", admission, change), func(t *testing.T) {
				d := newDisposable(t)
				d.tmux.Label = "main"
				if !admission {
					if _, err := incarnation(d.tmux); err != nil {
						t.Fatal(err)
					}
				}
				name := attachmentShadowPrefix + strings.Repeat("8", 32)
				d.run("new-session", "-d", "-s", name, "sleep 600")
				id := d.run("display-message", "-p", "-t", name, "#{session_id}")
				body := "if len(args) > 6 and args[4] == 'if-shell':\n"
				switch change {
				case "protected":
					body += "    subprocess.run([tmux, *args[:4], 'set-option', '-t', args[6], '@persea_client_id', 'became-protected'], check=True)\n"
				case "missing":
					body += "    subprocess.run([tmux, *args[:4], 'kill-session', '-t', args[6]], check=True)\n"
				case "transport":
					body += "    print('lost server', file=sys.stderr)\n    sys.exit(1)\n"
				case "server":
					body += "    print('no server running on fixture', file=sys.stderr)\n    sys.exit(1)\n"
				case "stale":
					body += "    args[7] = 'exit 1'\n"
				}
				installShadowTmuxWrapper(t, body)
				got := (&Server{config: bootCreationConfig(t, d.tmux)}).inventoryServer(d.tmux, 10)
				safe := change == "protected" || change == "missing"
				if safe && (got.Status != "ok" || !got.CanCreate || len(got.Sessions) == 0) {
					t.Fatalf("safe cleanup refusal hides healthy sessions: %+v", got)
				}
				if change == "missing" && len(got.Sessions) != 1 {
					t.Fatalf("disappeared candidate remained in inventory: %+v", got.Sessions)
				}
				if !safe && (got.Status == "ok" || got.CanCreate || len(got.Sessions) != 0) {
					t.Fatalf("cleanup failure was suppressed: %+v", got)
				}
				if present, err := tmuxSessionPresent(d.tmux, id); err != nil || present != (change != "missing") {
					t.Fatalf("cleanup race preservation: present=%t change=%s err=%v", present, change, err)
				}
			})
		}
	}
}

func TestRestoredEmptyMarkersMatchRemovalGuard(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		for _, option := range []string{"@persea_client_id", attachmentOwnerPIDOption, attachmentOwnerStartOption} {
			t.Run(fmt.Sprintf("inherited=%t/%s", inherited, option), func(t *testing.T) {
				d := newDisposable(t)
				name := attachmentShadowPrefix + strings.Repeat("7", 32)
				d.run("new-session", "-d", "-s", name, "sleep 600")
				id := d.run("display-message", "-p", "-t", name, "#{session_id}")
				inc, err := readIncarnation(d.tmux)
				if err != nil {
					t.Fatal(err)
				}
				shadow, ok := inspectRestoredShadow(d.tmux, id, inc)
				if !ok {
					t.Fatal("candidate not identified")
				}
				if inherited {
					d.run("set-option", "-g", option, "")
				} else {
					d.run("set-option", "-t", name, option, "")
				}
				if _, ok := inspectRestoredShadow(d.tmux, id, inc); !ok {
					t.Fatal("empty marker changed candidate classification")
				}
				if err := removeOrphanShadow(d.tmux, shadow); err != nil {
					t.Fatal(err)
				}
				if present, err := tmuxSessionPresent(d.tmux, id); err != nil || present {
					t.Fatalf("empty marker prevented cleanup: present=%t err=%v", present, err)
				}
			})
		}
	}
}

func TestAdmissionOrdinarySessionsUseConstantCommands(t *testing.T) {
	for _, count := range []int{1, 22, 100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			d := newDisposable(t)
			for i := 1; i < count; i++ {
				d.run("new-session", "-d", "-s", fmt.Sprintf("operator-%03d", i), "sleep 600")
			}
			log := filepath.Join(shortTempDir(t), "calls")
			installShadowTmuxWrapper(t, fmt.Sprintf("with open(%q, 'a') as f:\n    f.write(args[4] + '\\n')\n", log))
			if _, err := incarnation(d.tmux); err != nil {
				t.Fatal(err)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if string(calls) != "display-message\nlist-sessions\ndisplay-message\n" {
				t.Fatalf("admission command count grew with %d ordinary sessions: %q", count, calls)
			}
		})
	}
}

func TestMissingShadowDiagnostic(t *testing.T) {
	d := newDisposable(t)
	_, missing := tmuxOutput(d.tmux, "has-session", "-t", "=$999")
	var failure *tmuxCommandError
	if !errors.As(missing, &failure) || failure.message != "can't find session: $999" {
		t.Fatalf("unexpected tmux missing-session diagnostic: %v", missing)
	}
	err := removeOrphanShadow(d.tmux, orphanShadow{sessionID: "$999", name: attachmentShadowPrefix + strings.Repeat("6", 32)})
	if !errors.Is(err, errShadowMissing) {
		t.Fatalf("tmux missing-session diagnostic was not recognized: %v", err)
	}
}
