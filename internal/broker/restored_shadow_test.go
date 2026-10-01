package broker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func TestCreateRefusesReservedShadowNames(t *testing.T) {
	d := newDisposable(t)
	d.tmux.Label = "main"
	// Use a permissive policy so the reserved-name check owns the refusal.
	path := filepath.Join(shortTempDir(t), "broker.json")
	body := fmt.Sprintf(`{"realm":"test","front_uid":%d,"servers":[{"label":"main","socket_path":%q}],"session_create":{"enabled":true,"name_pattern":"^.*$"}}`, os.Getuid(), d.path)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadBroker(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{config: cfg}
	for _, suffix := range []string{strings.Repeat("a", 32), strings.Repeat("F", 32)} {
		name := attachmentShadowPrefix + suffix
		if !cfg.SessionCreate.AllowsName(name) {
			t.Fatal("test policy itself rejected the reserved name")
		}
		if got := unifiedE2E1Create(t, s, name); got.Type != "create_refused" || got.Code != "invalid_name" {
			t.Fatalf("reserved shadow name accepted: %+v", got)
		}
	}
	for _, name := range []string{attachmentShadowPrefix + "work", attachmentShadowPrefix + strings.Repeat("g", 32), attachmentShadowPrefix + strings.Repeat("a", 31)} {
		if got := unifiedE2E1Create(t, s, name); got.Type != "create_ok" {
			t.Fatalf("ordinary name was reserved: %+v", got)
		}
	}
}

func TestRestoredShadowRequiresAbsentOptionsDetachedSingleWindow(t *testing.T) {
	for _, mutation := range []string{"none", "client", "pid", "start", "empty-client", "empty-pid", "empty-start", "inherited", "windows", "attached", "ordinary-name"} {
		t.Run(mutation, func(t *testing.T) {
			d := newDisposable(t)
			name := attachmentShadowPrefix + strings.Repeat("a", 32)
			if mutation == "ordinary-name" {
				name = "operator-work"
			}
			d.run("new-session", "-d", "-s", name, "sleep 600")
			id := d.run("display-message", "-p", "-t", name, "#{session_id}")
			option := map[string]string{"client": "@persea_client_id", "pid": attachmentOwnerPIDOption, "start": attachmentOwnerStartOption}[strings.TrimPrefix(mutation, "empty-")]
			if option != "" {
				value := "marker"
				if strings.HasPrefix(mutation, "empty-") {
					value = ""
				}
				d.run("set-option", "-t", name, option, value)
			}
			switch mutation {
			case "inherited":
				d.run("set-option", "-g", "@persea_client_id", "")
			case "windows":
				d.run("new-window", "-d", "-t", name, "sleep 600")
			case "attached":
				w := recordingSourceWitness(t, d.path)
				if _, err := fmt.Fprintf(w.input, "switch-client -t %s\n", name); err != nil {
					t.Fatal(err)
				}
				w.roundtrip(t, "ATTACHED")
			}
			if _, err := incarnation(d.tmux); err != nil {
				t.Fatal(err)
			}
			present, err := tmuxSessionPresent(d.tmux, id)
			if err != nil || present != (mutation != "none") {
				t.Fatalf("restored cleanup %s: present=%v err=%v", mutation, present, err)
			}
		})
	}
}

func TestRestoredSweepPrecedesFirstUse(t *testing.T) {
	for _, entry := range []string{"inventory", "preview", "attachment", "create"} {
		t.Run(entry, func(t *testing.T) {
			d := newDisposable(t)
			d.tmux.Label = "main"
			s := &Server{config: bootCreationConfig(t, d.tmux)}
			for generation := 0; generation < 2; generation++ {
				if generation == 1 {
					w := recordingSourceWitness(t, d.path)
					d.run("kill-server")
					select {
					case <-w.done:
					case <-time.After(time.Second):
						t.Fatal("old server did not close its control client")
					}
					w.close()
					if got := s.inventoryServer(d.tmux, 10); got.Status != "stale_socket" && got.Status != "no_server" {
						t.Fatalf("stopped inventory: %+v", got)
					}
					d.run("new-session", "-d", "-s", "alpha", "sleep 600")
				}
				name := attachmentShadowPrefix + strings.Repeat("b", 32)
				d.run("new-session", "-d", "-s", name, "sleep 600")
				id := d.run("display-message", "-p", "-t", name, "#{session_id}")
				switch entry {
				case "inventory":
					got := s.inventoryServer(d.tmux, 10)
					if got.Status != "ok" || len(got.Sessions) != 1 || got.Sessions[0].Name != "alpha" {
						t.Fatalf("inventory published before sweep: %+v", got)
					}
				case "preview":
					got := controlResult(t, func(w *lockedWriter) { s.preview(w, proto.Control{ServerLabel: "main", SessionID: id}) })
					if got.Type != "preview_refused" || got.Code != "session_gone" {
						t.Fatalf("preview used restored copy: %+v", got)
					}
				case "attachment":
					if _, err := buildSourceWitness(context.Background(), d.tmux, "", id); err == nil {
						t.Fatal("attachment used restored copy")
					}
				case "create":
					if got := unifiedE2E1Create(t, s, "work"); got.Type != "create_ok" {
						t.Fatalf("create: %+v", got)
					}
				}
				if present, err := tmuxSessionPresent(d.tmux, id); err != nil || present {
					t.Fatalf("%s preceded sweep: present=%v err=%v", entry, present, err)
				}
			}
		})
	}
}

func TestAdmittedIncarnationIsNeverSweptAgain(t *testing.T) {
	d := newDisposable(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := incarnation(d.tmux)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Represents the interval in a live BIND before its owner options exist.
	name := attachmentShadowPrefix + strings.Repeat("c", 32)
	d.run("new-session", "-d", "-s", name, "sleep 600")
	id := d.run("display-message", "-p", "-t", name, "#{session_id}")
	if _, err := incarnation(d.tmux); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(d.path), "alias.sock")
	if err := os.Symlink(d.path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := incarnation(config.TmuxServer{Label: "alias", SocketPath: alias}); err != nil {
		t.Fatal(err)
	}
	if err := cleanupOrphanedShadows(config.Broker{Servers: []config.TmuxServer{d.tmux}}); err != nil {
		t.Fatal(err)
	}
	if present, err := tmuxSessionPresent(d.tmux, id); err != nil || !present {
		t.Fatalf("admitted incarnation was swept again: present=%v err=%v", present, err)
	}
}

func TestRestoredRemovalRechecksIdentity(t *testing.T) {
	for _, mutation := range []string{"client", "pid", "start", "name", "windows", "incarnation"} {
		t.Run(mutation, func(t *testing.T) {
			d := newDisposable(t)
			name := attachmentShadowPrefix + strings.Repeat("d", 32)
			d.run("new-session", "-d", "-s", name, "sleep 600")
			id := d.run("display-message", "-p", "-t", name, "#{session_id}")
			inc, err := readIncarnation(d.tmux)
			if err != nil {
				t.Fatal(err)
			}
			shadow, ok := inspectRestoredShadow(d.tmux, id, inc)
			if !ok {
				t.Fatal("restored candidate was not identified")
			}
			switch mutation {
			case "client", "pid", "start":
				option := map[string]string{"client": "@persea_client_id", "pid": attachmentOwnerPIDOption, "start": attachmentOwnerStartOption}[mutation]
				d.run("set-option", "-t", name, option, "changed")
			case "name":
				d.run("rename-session", "-t", name, "operator-work")
			case "windows":
				d.run("new-window", "-d", "-t", name, "sleep 600")
			case "incarnation":
				shadow.authority.ServerStart++
			}
			if err := removeOrphanShadow(d.tmux, shadow); err == nil {
				t.Fatal("changed restored candidate passed removal guard")
			}
			if present, err := tmuxSessionPresent(d.tmux, id); err != nil || !present {
				t.Fatalf("changed restored candidate removed: present=%v err=%v", present, err)
			}
		})
	}
}

func TestIncarnationReplacementCannotPublishUnsweptRows(t *testing.T) {
	for _, entry := range []string{"inventory", "preview"} {
		t.Run(entry, func(t *testing.T) {
			a, b := newDisposable(t), newDisposable(t)
			tmux, err := exec.LookPath("tmux")
			if err != nil {
				t.Fatal(err)
			}
			dir := shortTempDir(t)
			alias := filepath.Join(dir, "alias.sock")
			if err := os.Symlink(a.path, alias); err != nil {
				t.Fatal(err)
			}
			server := config.TmuxServer{Label: "main", SocketPath: alias}
			if _, err := incarnation(server); err != nil {
				t.Fatal(err)
			}
			name := attachmentShadowPrefix + strings.Repeat("e", 32)
			b.run("new-session", "-d", "-s", name, "sleep 600")
			id := b.run("display-message", "-p", "-t", name, "#{session_id}")
			command := "list-sessions"
			if entry == "preview" {
				command = "capture-pane"
			}
			script := "#!/bin/sh\nif [ \"$5\" = " + shellQuote(command) + " ]; then ln -sfn " + shellQuote(b.path) + " " + shellQuote(alias) + "; fi\nexec " + shellQuote(tmux) + " \"$@\"\n"
			if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			s := &Server{config: bootCreationConfig(t, server)}
			if entry == "inventory" {
				if got := s.inventoryServer(server, 10); got.Status != "error" || len(got.Sessions) != 0 || got.CanCreate {
					t.Fatalf("replacement inventory published before sweep: %+v", got)
				}
			} else {
				got := controlResult(t, func(w *lockedWriter) { s.preview(w, proto.Control{ServerLabel: "main", SessionID: "$0"}) })
				if got.Type != "preview_refused" || got.Code != "session_gone" {
					t.Fatalf("replacement preview published before sweep: %+v", got)
				}
			}
			if present, err := tmuxSessionPresent(b.tmux, id); err != nil || present {
				t.Fatalf("replacement incarnation was not swept: present=%v err=%v", present, err)
			}
		})
	}
}
