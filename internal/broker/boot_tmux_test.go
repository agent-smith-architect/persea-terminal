package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func TestTmux34NoServerMessages(t *testing.T) {
	for _, test := range []struct {
		message string
		absent  bool
	}{
		{"couldn't create directory /tmp/tmux-123 (Read-only file system)", true},
		{"couldn't create directory /tmp/block/tmux-123 (Not a directory)", true},
		{"couldn't create directory /tmp/tmux-123 (Permission denied)", true},
		{"error connecting to /tmp/tmux-123/probe (No such file or directory)", true},
		{"error connecting to /tmp/tmux-123/probe (Connection refused)", true},
		{"no server running on /tmp/tmux-123/probe", true},
		{"failed to connect to server: Connection refused", true},
		{"directory /tmp/tmux-123 has unsafe permissions", false},
		{"couldn't read directory /tmp/tmux-123 (Permission denied)", false},
		{"couldn't read directory /tmp/tmux-123 (No such file or directory)", false},
		{"/tmp/tmux-123 is not a directory", false},
		{"error connecting to /tmp/tmux-123/probe (Permission denied)", false},
		{"error creating /tmp/tmux-123/probe (Read-only file system)", false},
		{"can't find session: No such file", false},
	} {
		t.Run(test.message, func(t *testing.T) {
			if got := tmuxNoServer(test.message); got != test.absent {
				t.Fatalf("tmux 3.4 message classified no_server=%v, want %v", got, test.absent)
			}
		})
	}
}

func bootCreationConfig(t *testing.T, server config.TmuxServer) config.Broker {
	t.Helper()
	cfg := unifiedE2E1CreationConfig(t, config.TmuxServer{Label: "main", SocketPath: filepath.Join(shortTempDir(t), "absent.sock")})
	cfg.Servers = []config.TmuxServer{server}
	return cfg
}

func controlResult(t *testing.T, run func(*lockedWriter)) proto.Control {
	t.Helper()
	var output bytes.Buffer
	run(&lockedWriter{w: &output})
	frame, err := proto.ReadFrame(&output)
	if err != nil {
		t.Fatal(err)
	}
	result, err := proto.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestMissingSocketDirectoryIsStoppedServer(t *testing.T) {
	base := shortTempDir(t)
	block := filepath.Join(base, "block")
	if err := os.WriteFile(block, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// An ENOTDIR parent is uncreatable even as root. The production EROFS
	// spelling is pinned separately above.
	t.Setenv("TMUX_TMPDIR", block)
	server := config.TmuxServer{Label: "main", SocketName: "boot-" + filepath.Base(base)}
	cfg := bootCreationConfig(t, server)
	if _, err := tmuxOutput(server, "list-sessions"); !errors.Is(err, errNoServer) || !strings.Contains(err.Error(), "(Not a directory)") {
		t.Fatalf("uncreatable socket directory is not a stopped server: %v", err)
	}
	if _, err := incarnation(server); !errors.Is(err, errNoServer) {
		t.Fatalf("incarnation: %v", err)
	}
	if err := cleanupOrphanedShadows(cfg); err != nil {
		t.Fatalf("boot cleanup failed: %v", err)
	}
	s := &Server{config: cfg}
	if inventory := s.inventoryServer(server, 10); inventory.Status != "no_server" || inventory.CanCreate || len(inventory.Sessions) != 0 {
		t.Fatalf("boot inventory = %+v", inventory)
	}
	if result := unifiedE2E1Create(t, s, "work"); result.Type != "create_refused" || result.Code != "no_server" {
		t.Fatalf("boot creation = %+v", result)
	}
	if result := controlResult(t, func(w *lockedWriter) { s.preview(w, proto.Control{ServerLabel: "main", SessionID: "$0"}) }); result.Code != "server_unavailable" {
		t.Fatalf("boot preview = %+v", result)
	}
	if _, err := buildSourceWitness(context.Background(), server, "", "$0"); !errors.Is(err, errNoServer) {
		t.Fatalf("boot attachment witness = %v", err)
	}
	blockedSocket := filepath.Join(base, "broker.sock")
	if err := os.Mkdir(blockedSocket, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blockedSocket, "keep"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	var pathErr *os.PathError
	if err := Run(blockedSocket, cfg); !errors.As(err, &pathErr) || pathErr.Op != "remove" {
		t.Fatalf("boot did not reach listener setup: %v", err)
	}
}

func TestSocketDirectoryFileRemainsFatal(t *testing.T) {
	base := shortTempDir(t)
	t.Setenv("TMUX_TMPDIR", base)
	if err := os.WriteFile(filepath.Join(base, fmt.Sprintf("tmux-%d", os.Getuid())), nil, 0600); err != nil {
		t.Fatal(err)
	}
	server := config.TmuxServer{Label: "main", SocketName: "boot-" + filepath.Base(base)}
	cfg := bootCreationConfig(t, server)
	if _, err := incarnation(server); err == nil || errors.Is(err, errNoServer) || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("socket directory file was treated as absent: %v", err)
	}
	if err := cleanupOrphanedShadows(cfg); err == nil {
		t.Fatal("unsafe startup cleanup succeeded")
	}
	if got := (&Server{config: cfg}).inventoryServer(server, 10); got.Status != "error" || got.CanCreate {
		t.Fatalf("unsafe inventory = %+v", got)
	}
}

func TestTmuxClientsCannotStartServer(t *testing.T) {
	for _, control := range []bool{false, true} {
		t.Run(fmt.Sprint(control), func(t *testing.T) {
			server := config.TmuxServer{Label: "main", SocketPath: filepath.Join(shortTempDir(t), "tmux.sock")}
			t.Cleanup(func() { _, _ = tmuxCombinedOutput(server.SocketPath, "kill-server") })
			args := []string{"new-session", "-d", "-s", "forbidden", "sleep 600"}
			if control {
				args = append([]string{"-C"}, args...)
			}
			argv := tmuxArgv(server, args...)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
			if err == nil || !strings.Contains(string(output), "No such file or directory") {
				t.Fatalf("broker client could start a server: %v %s", err, output)
			}
			if _, err := os.Stat(server.SocketPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("broker client left a server socket: %v", err)
			}
		})
	}
}

func TestCreateRequiresRunningServerBeforeBirth(t *testing.T) {
	server := config.TmuxServer{Label: "main", SocketPath: filepath.Join(shortTempDir(t), "tmux.sock")}
	birth := &unifiedE2E1CreatorBirth{sessionID: "$91"}
	s := &Server{config: bootCreationConfig(t, server), birth: &unifiedE2E1BirthOwner{target: "work", birth: birth}}
	result := unifiedE2E1Create(t, s, "work")
	if result.Type != "create_refused" || result.Code != "no_server" || birth.create != 0 {
		t.Fatalf("creation without a server reached birth: result=%+v creates=%d", result, birth.create)
	}
	if got := s.inventoryServer(server, 10); got.Status != "no_server" || got.CanCreate {
		t.Fatalf("stopped server offers creation: %+v", got)
	}
	d := &disposable{t: t, tmux: server, path: server.SocketPath}
	d.run("new-session", "-d", "-s", "keeper", "sleep 600")
	t.Cleanup(func() { _, _ = tmuxCombinedOutput(d.path, "kill-server") })
	if got := s.inventoryServer(server, 10); got.Status != "ok" || !got.CanCreate {
		t.Fatalf("running server does not offer creation: %+v", got)
	}
	s.birth = nil
	if result := unifiedE2E1Create(t, s, "work"); result.Type != "create_ok" {
		t.Fatalf("creation on running server failed: %+v", result)
	}
}

type bootBirthEffect func(string, string) (SessionBirthEffects, error)

func (f bootBirthEffect) beginSessionBirth(server, name string) (SessionBirthEffects, error) {
	return f(server, name)
}

func TestCreateReportsServerLostAfterPreflight(t *testing.T) {
	d := newDisposable(t)
	d.tmux.Label = "main"
	w := recordingSourceWitness(t, d.path)
	birth := &unifiedE2E1CreatorBirth{createErr: errors.New("observer connection closed")}
	s := &Server{config: bootCreationConfig(t, d.tmux)}
	s.birth = bootBirthEffect(func(string, string) (SessionBirthEffects, error) {
		d.run("kill-server")
		select {
		case <-w.done:
		case <-time.After(time.Second):
			t.Fatal("server did not close its control client")
		}
		w.close()
		return birth, nil
	})
	if result := unifiedE2E1Create(t, s, "work"); result.Type != "create_refused" || result.Code != "no_server" || birth.create != 1 || birth.abort != 1 {
		t.Fatalf("server loss after preflight = %+v; create=%d abort=%d", result, birth.create, birth.abort)
	}
}

func TestObserverSpawnGatesIncarnation(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprint(running), func(t *testing.T) {
			effects := newProjectionEffects(t, 0)
			effects.server.SocketPath = filepath.Join(shortTempDir(t), "tmux.sock")
			id := "$0"
			if running {
				d := newDisposable(t)
				effects.server.SocketPath = d.path
				name := attachmentShadowPrefix + strings.Repeat("f", 32)
				d.run("new-session", "-d", "-s", name, "sleep 600")
				id = d.run("display-message", "-p", "-t", name, "#{session_id}")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer func() {
				cancel()
				effects.mu.Lock()
				var units []*unifiedDevUnit
				for unit := range effects.supervised {
					units = append(units, unit)
				}
				effects.mu.Unlock()
				for _, unit := range units {
					select {
					case <-unit.done:
					case <-time.After(time.Second):
						t.Error("test observer did not stop")
					}
				}
			}()
			err := effects.spawnUnit(ctx, unifiedDevCommand{server: effects.server, args: []string{"attach-session", "-f", "ignore-size", "-t", id}, done: make(chan unifiedDevCommandResult, 1)})
			if !running && !errors.Is(err, errNoServer) {
				t.Fatalf("observer spawn accepted a stopped server: %v", err)
			}
			if running {
				if err != nil {
					t.Fatal(err)
				}
				if present, err := tmuxSessionPresent(effects.server, id); err != nil || present {
					t.Fatalf("observer connected before restored sweep: present=%v err=%v", present, err)
				}
			}
		})
	}
}
