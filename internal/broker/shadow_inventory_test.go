package broker

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"persea-terminal/internal/terminal"
)

func installShadowTmuxWrapper(t *testing.T, body string) {
	t.Helper()
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	dir := shortTempDir(t)
	script := "#!/usr/bin/python3\nimport os, sys, shlex, subprocess\nargs = sys.argv[1:]\ntmux = " + strconv.Quote(tmux) + "\n" + body + "\nos.execv(tmux, [tmux, *args])\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func shadowTestPipe(t *testing.T, dir, name string) *os.File {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func shadowPipeLine(t *testing.T, f *os.File) string {
	t.Helper()
	if err := f.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line)
}

func TestInventoryReapsLateRestoredShadows(t *testing.T) {
	for _, limit := range []int{10, 0} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			d := newDisposable(t)
			s := &Server{config: bootCreationConfig(t, d.tmux)}
			if _, err := incarnation(d.tmux); err != nil {
				t.Fatal(err)
			}
			name := attachmentShadowPrefix + strings.Repeat("f", 32)
			d.run("new-session", "-d", "-s", name, "sleep 600")
			id := d.run("display-message", "-p", "-t", name, "#{session_id}")
			ageRestoredShadow(t, d.tmux, id)
			got := s.inventoryServer(d.tmux, limit)
			if got.Status != "ok" || (limit > 0 && (len(got.Sessions) != 1 || got.Sessions[0].Name != "alpha")) {
				t.Fatalf("late restored inventory: %+v", got)
			}
			if present, err := tmuxSessionPresent(d.tmux, id); err != nil || present {
				t.Fatalf("late restored shadow survived inventory: present=%v err=%v", present, err)
			}
			if present, err := tmuxSessionPresent(d.tmux, "$0"); err != nil || !present {
				t.Fatalf("operator session lost: present=%v err=%v", present, err)
			}
		})
	}
}

func TestInventorySkipsLiveShadowsWithoutExtraCalls(t *testing.T) {
	d := newDisposable(t)
	s := &Server{config: bootCreationConfig(t, d.tmux)}
	if _, err := incarnation(d.tmux); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(shortTempDir(t), "calls")
	installShadowTmuxWrapper(t, fmt.Sprintf("with open(%q, 'a') as f:\n    f.write(args[4] + '\\n')\n", log))
	calls := func() string {
		t.Helper()
		if err := os.WriteFile(log, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if got := s.inventoryServer(d.tmux, 10); got.Status != "ok" {
			t.Fatalf("inventory: %+v", got)
		}
		b, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	baseline := calls()
	name := attachmentShadowPrefix + strings.Repeat("a", 32)
	d.run("new-session", "-d", "-s", name, "sleep 600")
	id := d.run("display-message", "-p", "-t", name, "#{session_id}")
	ageRestoredShadow(t, d.tmux, id)
	owner, err := (procProbe{}).Witness(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	setShadowOwner(t, d, name, attachmentClientPrefix+strings.Repeat("a", 32), owner.PID, owner.StartTime)
	if got := calls(); got != baseline {
		t.Fatalf("live shadow added tmux calls: baseline=%q live=%q", baseline, got)
	}
	if present, err := tmuxSessionPresent(d.tmux, id); err != nil || !present {
		t.Fatalf("live shadow was removed: present=%v err=%v", present, err)
	}
}

func TestInventoryProtectsShadowBirthUntilCommandReturns(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			d := newDisposable(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			authority, err := incarnation(d.tmux)
			if err != nil {
				t.Fatal(err)
			}
			detail, err := details(d.tmux, "$0")
			if err != nil {
				t.Fatal(err)
			}
			authority.SessionID, authority.SessionCreated = detail.ID, detail.Created
			witness, err := buildSourceWitness(ctx, d.tmux, authority.BootID, detail.ID)
			if err != nil {
				t.Fatal(err)
			}
			dir := shortTempDir(t)
			ready, release := shadowTestPipe(t, dir, "ready"), shadowTestPipe(t, dir, "release")
			// Split the real birth list at the unmarked interval. Inventory runs
			// before the remaining commands, or before a failed client returns.
			body := fmt.Sprintf(`if len(args) > 8 and args[4] == 'if-shell' and args[8].startswith('new-session '):
    first, rest = args[8].split(' ; ', 1)
    words = shlex.split(first)
    name = words[words.index('-s') + 1]
    subprocess.run([tmux, *args[:4], *words], check=True)
    with open(%q, 'w') as f:
        f.write(name + '\n')
    with open(%q) as f:
        outcome = f.readline().strip()
    if outcome == 'failure':
        sys.exit(1)
    args[8] = rest
`, ready.Name(), release.Name())
			installShadowTmuxWrapper(t, body)
			delayed := newDelayedPTY()
			delayed.setConsumer(func([]byte) error { return nil }, func(error) {})
			txn := &tmuxPinnedTransaction{server: d.tmux, bootID: authority.BootID, authority: authority, pty: delayed}
			type result struct {
				bound terminal.TransactionResult
				err   error
			}
			done, finished := make(chan result, 1), make(chan struct{})
			go func() {
				defer close(finished)
				bound, err := txn.RunPinned(ctx, terminal.TransactionRequest{Action: terminal.ActionBind, Witness: witness})
				done <- result{bound, err}
			}()
			var once sync.Once
			unblock := func() { once.Do(func() { _, _ = fmt.Fprintln(release, outcome) }) }
			defer func() {
				unblock()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("birth command did not finish")
				}
			}()
			name := shadowPipeLine(t, ready)
			id := d.run("display-message", "-p", "-t", name, "#{session_id}")
			ageRestoredShadow(t, d.tmux, id)
			if out := d.run("show-options", "-Aq", "-t", "="+id+":", "@persea_client_id"); out != "" {
				t.Fatalf("birth fixture already marked: %q", out)
			}
			s := &Server{config: bootCreationConfig(t, d.tmux)}
			if got := s.inventoryServer(d.tmux, 10); got.Status != "ok" {
				t.Fatalf("inventory during birth: %+v", got)
			}
			if present, err := tmuxSessionPresent(d.tmux, id); err != nil || !present {
				t.Fatalf("in-flight shadow removed: present=%v err=%v", present, err)
			}
			if outcome != "timeout" {
				unblock()
			}
			var got result
			select {
			case got = <-done:
			case <-time.After(shadowBirthTimeout + time.Second):
				t.Fatal("birth command exceeded its cleanup safety bound")
			}
			if (got.err == nil) != (outcome == "success") {
				t.Fatalf("birth %s: %v", outcome, got.err)
			}
			if attachmentShadowPending(name) {
				t.Fatal("completed birth still marked in-flight")
			}
			if outcome == "success" {
				defer func() {
					_, _ = txn.RunPinned(context.Background(), terminal.TransactionRequest{Action: terminal.ActionCleanup, Witness: witness, Attachment: got.bound.Attachment})
				}()
			}
			if inv := s.inventoryServer(d.tmux, 10); inv.Status != "ok" {
				t.Fatalf("inventory after birth: %+v", inv)
			}
			if present, err := tmuxSessionPresent(d.tmux, id); err != nil || present != (outcome == "success") {
				t.Fatalf("birth %s cleanup: present=%v err=%v", outcome, present, err)
			}
		})
	}
}

func TestSlowIncarnationReadDoesNotBlockAdmittedServer(t *testing.T) {
	a, b := newDisposable(t), newDisposable(t)
	for _, d := range []*disposable{a, b} {
		if _, err := incarnation(d.tmux); err != nil {
			t.Fatal(err)
		}
	}
	dir := shortTempDir(t)
	ready, release := shadowTestPipe(t, dir, "ready"), shadowTestPipe(t, dir, "release")
	body := fmt.Sprintf(`if args[3] == %q and args[4] == 'display-message':
    with open(%q, 'w') as f:
        f.write('blocked\n')
    with open(%q) as f:
        f.readline()
`, a.path, ready.Name(), release.Name())
	installShadowTmuxWrapper(t, body)
	blocked := make(chan error, 1)
	go func() { _, err := incarnation(a.tmux); blocked <- err }()
	defer func() {
		_, _ = fmt.Fprintln(release, "release")
		select {
		case err := <-blocked:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("blocked lookup did not finish")
		}
	}()
	if got := shadowPipeLine(t, ready); got != "blocked" {
		t.Fatalf("lookup barrier: %q", got)
	}
	other := make(chan error, 1)
	go func() { _, err := incarnation(b.tmux); other <- err }()
	select {
	case err := <-other:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Error("blocked tmux read prevented another admitted server lookup")
	}
}
