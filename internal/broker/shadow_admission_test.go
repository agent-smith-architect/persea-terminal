package broker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/config"
)

func TestIncarnationSweepsAreIndependent(t *testing.T) {
	a, b, c := newDisposable(t), newDisposable(t), newDisposable(t)
	if _, err := incarnation(b.tmux); err != nil {
		t.Fatal(err)
	}
	name := attachmentShadowPrefix + strings.Repeat("f", 32)
	c.run("new-session", "-d", "-s", name, "sleep 600")
	copyID := c.run("display-message", "-p", "-t", name, "#{session_id}")
	dir := shortTempDir(t)
	ready := shadowTestPipe(t, dir, "ready")
	release := shadowTestPipe(t, dir, "release")
	secondRead := shadowTestPipe(t, dir, "second-read")
	armed := filepath.Join(dir, "second")
	calls := filepath.Join(dir, "calls")
	installShadowTmuxWrapper(t, fmt.Sprintf(`if args[3] == %q:
    if args[4] == 'display-message' and os.path.exists(%q):
        with open(%q, 'w') as f:
            f.write('identity\n')
    if args[4] == 'list-sessions':
        with open(%q, 'a') as f:
            f.write('sweep\n')
        with open(%q, 'w') as f:
            f.write('blocked\n')
        fd = os.open(%q, os.O_RDONLY)
        os.read(fd, 1)
        os.close(fd)
`, a.path, armed, secondRead.Name(), calls, ready.Name(), release.Name()))
	type result struct {
		name  string
		err   error
		early bool
	}
	done := make(chan result, 4)
	var released atomic.Bool
	remaining := 0
	start := func(name string, server config.TmuxServer) {
		remaining++
		go func() {
			_, err := incarnation(server)
			done <- result{name, err, !released.Load()}
		}()
	}
	defer func() {
		released.Store(true)
		_, _ = release.Write([]byte{1, 1})
		for range remaining {
			select {
			case got := <-done:
				if got.err != nil {
					t.Errorf("%s: %v", got.name, got.err)
				}
				if strings.HasSuffix(got.name, " A") && got.early {
					t.Errorf("%s returned before its first sweep completed", got.name)
				}
			case <-time.After(5 * time.Second):
				t.Error("admission did not settle")
				return
			}
		}
		data, err := os.ReadFile(calls)
		if err != nil || strings.Count(string(data), "sweep\n") != 1 {
			t.Errorf("same incarnation swept more than once: %q err=%v", data, err)
		}
	}()
	start("first A", a.tmux)
	if got := shadowPipeLine(t, ready); got != "blocked" {
		t.Fatalf("first sweep barrier: %q", got)
	}
	if err := os.WriteFile(armed, nil, 0600); err != nil {
		t.Fatal(err)
	}
	start("second A", a.tmux)
	if got := shadowPipeLine(t, secondRead); got != "identity" {
		t.Fatalf("second identity barrier: %q", got)
	}
	start("admitted B", b.tmux)
	start("first C", c.tmux)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	want := map[string]bool{"admitted B": true, "first C": true}
	for len(want) != 0 {
		select {
		case got := <-done:
			remaining--
			if got.err != nil {
				t.Errorf("%s: %v", got.name, got.err)
			}
			if !want[got.name] {
				t.Fatalf("%s returned before its first sweep completed", got.name)
			}
			delete(want, got.name)
		case <-timer.C:
			t.Fatalf("blocked first sweep prevented unrelated admissions: %v", want)
		}
	}
	select {
	case got := <-done:
		remaining--
		t.Fatalf("%s returned before its first sweep completed", got.name)
	default:
	}
	if present, err := tmuxSessionPresent(c.tmux, copyID); err != nil || present {
		t.Fatalf("first C published before its sweep: present=%v err=%v", present, err)
	}
}
