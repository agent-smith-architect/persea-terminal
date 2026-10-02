package broker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/terminal"
)

func TestRestoredShadowOwnerWitness(t *testing.T) {
	for _, inventory := range []bool{false, true} {
		for _, state := range []string{"absent", "removed", "live", "dead", "old-boot", "reused-pid", "empty", "extra-field", "newline", "invalid-boot", "uppercase-boot", "newline-boot", "leading-pid"} {
			t.Run(fmt.Sprintf("inventory=%t/%s", inventory, state), func(t *testing.T) {
				d := newDisposable(t)
				inc, err := readIncarnation(d.tmux)
				if err != nil {
					t.Fatal(err)
				}
				if inventory {
					if _, err := incarnation(d.tmux); err != nil {
						t.Fatal(err)
					}
				}
				owner, err := (procProbe{}).Witness(context.Background(), os.Getpid())
				if err != nil {
					t.Fatal(err)
				}
				boot := inc.BootID
				switch state {
				case "old-boot":
					boot = "00000000-0000-0000-0000-000000000000"
				case "reused-pid":
					owner.StartTime++
				case "dead":
					process := exec.Command("cat")
					input, err := process.StdinPipe()
					if err != nil {
						t.Fatal(err)
					}
					defer input.Close()
					if err := process.Start(); err != nil {
						t.Fatal(err)
					}
					owner, err = (procProbe{}).Witness(context.Background(), process.Process.Pid)
					_ = process.Process.Kill()
					_ = process.Wait()
					if err != nil {
						t.Fatal(err)
					}
				}
				value := fmt.Sprintf("%s:%d:%d", boot, owner.PID, owner.StartTime)
				switch state {
				case "empty":
					value = ""
				case "extra-field":
					value += ":extra"
				case "newline":
					value += "\n"
				case "invalid-boot":
					value = fmt.Sprintf("not-a-boot:%d:%d", owner.PID, owner.StartTime)
				case "uppercase-boot":
					value = fmt.Sprintf("AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA:%d:%d", owner.PID, owner.StartTime)
				case "newline-boot":
					value = fmt.Sprintf("%s\nextra:%d:%d", boot, owner.PID, owner.StartTime)
				case "leading-pid":
					value = fmt.Sprintf("%s:0%d:%d", boot, owner.PID, owner.StartTime)
				}
				name := attachmentShadowPrefix + strings.Repeat("9", 32)
				args := []string{"new-session", "-d", "-s", name}
				if state != "absent" {
					args = append(args, "-e", attachmentOwnerEnvironment+"="+value)
				}
				d.run(append(args, "sleep 600")...)
				id := d.run("display-message", "-p", "-t", name, "#{session_id}")
				if state == "removed" {
					d.run("set-environment", "-r", "-t", "="+id+":", attachmentOwnerEnvironment)
				}
				malformed := state == "empty" || state == "extra-field" || state == "newline" || state == "invalid-boot" || state == "uppercase-boot" || state == "newline-boot" || state == "leading-pid"
				logs := captureBrokerLogs(t)
				if inventory {
					got := (&Server{config: bootCreationConfig(t, d.tmux)}).inventoryServer(d.tmux, 10)
					if got.Status != "ok" || got.Error != "" {
						t.Fatalf("owner inspection inventory: %+v", got)
					}
				} else if _, err := incarnation(d.tmux); err != nil {
					t.Fatalf("owner inspection admission: %v", err)
				}
				wantPresent := state == "live" || malformed
				if present, err := tmuxSessionPresent(d.tmux, id); err != nil || present != wantPresent {
					t.Fatalf("owner %s: present=%t want=%t err=%v", state, present, wantPresent, err)
				}
				if malformed && (!strings.Contains(logs.String(), "event=orphan_shadow_skipped") || !strings.Contains(logs.String(), "invalid witness")) {
					t.Fatalf("uncertain owner skip was not explained: %q", logs.String())
				}
			})
		}
	}
}

func TestShadowOwnerEnvironmentReadFailures(t *testing.T) {
	for _, failure := range []string{"transport", "wrong-variable", "wrong-output", "missing"} {
		t.Run(failure, func(t *testing.T) {
			d := newDisposable(t)
			if _, err := incarnation(d.tmux); err != nil {
				t.Fatal(err)
			}
			name := attachmentShadowPrefix + strings.Repeat("8", 32)
			d.run("new-session", "-d", "-s", name, "sleep 600")
			id := d.run("display-message", "-p", "-t", name, "#{session_id}")
			body := "if args[4] == 'show-environment':\n"
			switch failure {
			case "missing":
				body += fmt.Sprintf("    subprocess.run([tmux, *args[:4], 'kill-session', '-t', %q], check=True)\n", "="+id)
			case "wrong-output":
				body += "    print('-PERSEA_SHADOW_OWNER_EXTRA')\n    sys.exit(0)\n"
			default:
				message := "lost owner transport"
				if failure == "wrong-variable" {
					message = "unknown variable: PERSEA_SHADOW_OWNER_EXTRA"
				}
				body += fmt.Sprintf("    sys.stderr.write(%q)\n    sys.exit(1)\n", message+"\n")
			}
			installShadowTmuxWrapper(t, body)
			logs := captureBrokerLogs(t)
			got := (&Server{config: bootCreationConfig(t, d.tmux)}).inventoryServer(d.tmux, 10)
			if failure == "missing" {
				if got.Status != "ok" || len(got.Sessions) != 1 || got.Sessions[0].Name != "alpha" {
					t.Fatalf("disappeared owner inspection hid healthy inventory: %+v", got)
				}
			} else {
				if (got.Status == "error") != (failure == "transport") {
					t.Fatalf("owner inspection failure policy: %+v", got)
				}
				if present, err := tmuxSessionPresent(d.tmux, id); err != nil || !present {
					t.Fatalf("uncertain owner was removed: present=%t err=%v", present, err)
				}
				if failure != "transport" && !strings.Contains(logs.String(), "event=orphan_shadow_skipped") {
					t.Fatalf("environment uncertainty was not logged: %q", logs.String())
				}
			}
		})
	}
}

func TestPausedShadowCreator(t *testing.T) {
	socket := os.Getenv("PERSEA_SHADOW_TEST_SOCKET")
	if socket == "" {
		t.Skip("owned subprocess helper")
	}
	server := config.TmuxServer{Label: "main", SocketPath: socket}
	ctx := context.Background()
	authority, err := incarnation(server)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := details(server, "$0")
	if err != nil {
		t.Fatal(err)
	}
	authority.SessionID, authority.SessionCreated = detail.ID, detail.Created
	witness, err := buildSourceWitness(ctx, server, authority.BootID, detail.ID)
	if err != nil {
		t.Fatal(err)
	}
	delayed := newDelayedPTY()
	delayed.setConsumer(func([]byte) error { return nil }, func(error) {})
	txn := &tmuxPinnedTransaction{server: server, bootID: authority.BootID, authority: authority, pty: delayed}
	_, err = txn.RunPinned(ctx, terminal.TransactionRequest{Action: terminal.ActionBind, Witness: witness})
	t.Logf("birth returned after resume: %v", err)
}

func TestPausedCreatorProtectsShadowBeyondFormerGrace(t *testing.T) {
	d := newDisposable(t)
	inc, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	dir := shortTempDir(t)
	ready, release := shadowTestPipe(t, dir, "ready"), shadowTestPipe(t, dir, "release")
	block := "printf '%s\\n' '#{session_name}' > " + shellQuote(ready.Name()) + "; read release < " + shellQuote(release.Name())
	d.run("set-hook", "-g", "after-new-session", "run-shell "+shellQuote(block))
	command := exec.Command(os.Args[0], "-test.run=^TestPausedShadowCreator$", "-test.v")
	command.Env = append(os.Environ(), "PERSEA_SHADOW_TEST_SOCKET="+d.path)
	log, err := os.Create(filepath.Join(dir, "creator.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Signal(syscall.SIGCONT)
		_, _ = fmt.Fprintln(release, "release")
		if err := command.Wait(); err != nil {
			t.Errorf("creator helper: %v", err)
		}
		data, _ := os.ReadFile(log.Name())
		t.Logf("creator result: %s", data)
	}()
	name := shadowPipeLine(t, ready)
	if err := command.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	var state syscall.WaitStatus
	if pid, err := syscall.Wait4(command.Process.Pid, &state, syscall.WUNTRACED, nil); err != nil || pid != command.Process.Pid || !state.Stopped() {
		t.Fatalf("creator did not stop: pid=%d state=%v err=%v", pid, state, err)
	}
	owner, err := (procProbe{}).Witness(context.Background(), command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	id := d.run("display-message", "-p", "-t", name, "#{session_id}")
	want := fmt.Sprintf("%s=%s:%d:%d", attachmentOwnerEnvironment, inc.BootID, owner.PID, owner.StartTime)
	if got := d.run("show-environment", "-t", "="+id+":", attachmentOwnerEnvironment); got != want {
		t.Fatalf("first session hook had no creation-time witness: got=%q want=%q", got, want)
	}
	if got := d.run("show-options", "-Aq", "-t", "="+id+":", "@persea_client_id"); got != "" {
		t.Fatalf("birth already has client marker: %q", got)
	}
	server := &Server{config: bootCreationConfig(t, d.tmux)}
	if got := server.inventoryServer(d.tmux, 10); got.Status != "ok" {
		t.Fatal(got)
	}
	// The elapsed interval is the regression's subject: exceed the former
	// twelve-second protection bound while the owner remains suspended.
	interval := time.NewTimer(13 * time.Second)
	defer interval.Stop()
	<-interval.C
	if got := server.inventoryServer(d.tmux, 10); got.Status != "ok" {
		t.Fatal(got)
	}
	if present, err := tmuxSessionPresent(d.tmux, id); err != nil || !present {
		t.Fatalf("another broker removed the suspended creator's shadow: present=%t err=%v", present, err)
	}
	if got := d.run("show-options", "-Aq", "-t", "="+id+":", "@persea_client_id"); got != "" {
		t.Fatalf("suspended birth advanced: %q", got)
	}
	t.Logf("stopped creator %s retains unmarked shadow beyond 13 seconds", strconv.Itoa(owner.PID))
}
