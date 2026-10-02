package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"persea-terminal/internal/processprivacy"
	"persea-terminal/internal/terminal"
)

type shadowLogBuffer struct {
	sync.Mutex
	data bytes.Buffer
}

func (b *shadowLogBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.data.Write(p)
}

func (b *shadowLogBuffer) String() string {
	b.Lock()
	defer b.Unlock()
	return b.data.String()
}

func captureBrokerLogs(t *testing.T) *shadowLogBuffer {
	t.Helper()
	var logs shadowLogBuffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &logs
}

func TestPrivateShadowCreator(t *testing.T) {
	if os.Getenv("PERSEA_PRIVATE_SHADOW") != "1" {
		t.Skip("owned subprocess helper")
	}
	if err := processprivacy.DisableDumpability(); err != nil {
		t.Fatal(err)
	}
	owner, err := (procProbe{}).Witness(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("PERSEA_PRIVATE_OWNER_FILE"), []byte(fmt.Sprint(owner.StartTime)), 0600); err != nil {
		t.Fatal(err)
	}
	TestPausedShadowCreator(t)
}

func TestNonDumpableShadowOwner(t *testing.T) {
	for _, state := range []string{"running", "stopped", "marked", "exited"} {
		for _, admission := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admission=%t", state, admission), func(t *testing.T) {
				d := newDisposable(t)
				inc, err := readIncarnation(d.tmux)
				if err != nil {
					t.Fatal(err)
				}
				if !admission {
					if _, err := incarnation(d.tmux); err != nil {
						t.Fatal(err)
					}
				}
				dir := shortTempDir(t)
				ready, release := shadowTestPipe(t, dir, "ready"), shadowTestPipe(t, dir, "release")
				block := "printf '%s\\n' '#{session_name}' > " + shellQuote(ready.Name()) + "; read release < " + shellQuote(release.Name())
				d.run("set-hook", "-g", "after-new-session", "run-shell "+shellQuote(block))
				ownerFile := filepath.Join(dir, "owner")
				command := exec.Command(os.Args[0], "-test.run=^TestPrivateShadowCreator$", "-test.v")
				command.Env = append(os.Environ(), "PERSEA_PRIVATE_SHADOW=1", "PERSEA_SHADOW_TEST_SOCKET="+d.path, "PERSEA_PRIVATE_OWNER_FILE="+ownerFile)
				var output bytes.Buffer
				command.Stdout, command.Stderr = &output, &output
				if err := command.Start(); err != nil {
					t.Fatal(err)
				}
				reaped := false
				defer func() {
					_ = command.Process.Signal(syscall.SIGCONT)
					_, _ = fmt.Fprintln(release, "release")
					if !reaped {
						_ = command.Wait()
					}
					t.Logf("private creator: %s", output.String())
				}()
				name := shadowPipeLine(t, ready)
				id := d.run("display-message", "-p", "-t", name, "#{session_id}")
				start, err := os.ReadFile(ownerFile)
				if err != nil {
					t.Fatal(err)
				}
				want := fmt.Sprintf("%s=%s:%d:%s", attachmentOwnerEnvironment, inc.BootID, command.Process.Pid, start)
				if got := d.run("show-environment", "-t", "="+id+":", attachmentOwnerEnvironment); got != want {
					t.Fatalf("private creator has no birth witness: got=%q want=%q", got, want)
				}
				_, procErr := (procProbe{}).Witness(context.Background(), command.Process.Pid)
				t.Logf("private creator visibility: proc=%v kill0=%v", procErr, syscall.Kill(command.Process.Pid, 0))
				if procErr != nil && !errors.Is(procErr, os.ErrNotExist) {
					t.Fatal(procErr)
				}
				if state == "stopped" {
					if err := command.Process.Signal(syscall.SIGSTOP); err != nil {
						t.Fatal(err)
					}
					var status syscall.WaitStatus
					if pid, err := syscall.Wait4(command.Process.Pid, &status, syscall.WUNTRACED, nil); err != nil || pid != command.Process.Pid || !status.Stopped() {
						t.Fatalf("creator did not stop: %d %v %v", pid, status, err)
					}
				}
				if state == "marked" {
					d.run("set-option", "-t", name, "@persea_client_id", attachmentClientPrefix+strings.TrimPrefix(name, attachmentShadowPrefix))
					d.run("set-option", "-t", name, attachmentOwnerPIDOption, fmt.Sprint(command.Process.Pid))
					d.run("set-option", "-t", name, attachmentOwnerStartOption, string(start))
				}
				if state == "exited" {
					_ = command.Process.Kill()
					_ = command.Wait()
					reaped = true
				}
				if admission {
					if _, err := incarnation(d.tmux); err != nil {
						t.Fatal(err)
					}
				} else {
					got := (&Server{config: bootCreationConfig(t, d.tmux)}).inventoryServer(d.tmux, 10)
					if got.Status != "ok" || got.Error != "" {
						t.Fatal(got)
					}
				}
				if present, err := tmuxSessionPresent(d.tmux, id); err != nil || present != (state != "exited") {
					t.Fatalf("private %s creator shadow: present=%t err=%v", state, present, err)
				}
			})
		}
	}
}

func TestUncertainOwnerProcessIsSkipped(t *testing.T) {
	for _, marked := range []bool{false, true} {
		for _, result := range []string{"proc-error", "kill-error", "kill-eperm", "kill-success"} {
			t.Run(fmt.Sprintf("marked=%t/%s", marked, result), func(t *testing.T) {
				d := newDisposable(t)
				inc, err := readIncarnation(d.tmux)
				if err != nil {
					t.Fatal(err)
				}
				owner, err := (procProbe{}).Witness(context.Background(), os.Getpid())
				if err != nil {
					t.Fatal(err)
				}
				name := attachmentShadowPrefix + strings.Repeat("b", 32)
				d.run("new-session", "-d", "-t", "alpha", "-s", name, "-e", fmt.Sprintf("%s=%s:%d:%d", attachmentOwnerEnvironment, inc.BootID, owner.PID, owner.StartTime))
				id := d.run("display-message", "-p", "-t", name, "#{session_id}")
				if marked {
					setShadowOwner(t, d, name, attachmentClientPrefix+strings.Repeat("b", 32), owner.PID, owner.StartTime)
				}
				oldRead, oldSignal := readOwnerProcess, signalOwnerProcess
				t.Cleanup(func() { readOwnerProcess, signalOwnerProcess = oldRead, oldSignal })
				readOwnerProcess = func(context.Context, int) (terminal.ProcessWitness, error) {
					if result == "proc-error" {
						return terminal.ProcessWitness{}, syscall.EACCES
					}
					return terminal.ProcessWitness{}, os.ErrNotExist
				}
				signalOwnerProcess = func(pid int, signal syscall.Signal) error {
					if pid != owner.PID || signal != 0 {
						t.Errorf("unsafe process probe: %d %d", pid, signal)
					}
					switch result {
					case "kill-eperm":
						return syscall.EPERM
					case "kill-success":
						return nil
					default:
						return syscall.EINVAL
					}
				}
				logs := captureBrokerLogs(t)
				if _, err := incarnation(d.tmux); err != nil {
					t.Fatalf("uncertain candidate blocked admission: %v", err)
				}
				got := (&Server{config: bootCreationConfig(t, d.tmux)}).inventoryServer(d.tmux, 10)
				if got.Status != "ok" || got.Error != "" {
					t.Fatalf("uncertain candidate hid healthy inventory: %+v", got)
				}
				if present, err := tmuxSessionPresent(d.tmux, id); err != nil || !present {
					t.Fatalf("uncertain owner lost shadow: present=%t err=%v", present, err)
				}
				if strings.HasSuffix(result, "error") && (!strings.Contains(logs.String(), "event=orphan_shadow_skipped") || !strings.Contains(logs.String(), "process:")) {
					t.Fatalf("uncertain process was not explained: %q", logs.String())
				}
			})
		}
	}
}
