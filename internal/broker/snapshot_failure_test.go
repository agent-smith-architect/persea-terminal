package broker

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func TestSnapshotExecutionFailureWithLiveTargetIsSnapshotFailed(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	d := newDisposable(t)
	authority, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server = "local", d.tmux.Label
	authority.SessionID = d.run("display-message", "-p", "-t", "=alpha:", "#{session_id}")
	authority.SessionCreated, _ = strconv.ParseInt(d.run("display-message", "-p", "-t", "=alpha:", "#{session_created}"), 10, 64)

	fakeDir := shortTempDir(t)
	fakeTmux := filepath.Join(fakeDir, "tmux")
	script := "#!/bin/sh\nif [ \"$4\" = \"if-shell\" ]; then\n  exit 1\nfi\nexec " + shellQuote(tmuxPath) + " \"$@\"\n"
	if err := os.WriteFile(fakeTmux, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	s := &Server{config: config.Broker{Realm: "local", Servers: []config.TmuxServer{d.tmux}}}
	var wire bytes.Buffer
	s.snapshot(&lockedWriter{w: &wire}, proto.Control{Type: "snapshot", Authority: &authority, Depth: 20})
	frame, err := proto.ReadFrame(&wire)
	if err != nil || frame.Type != proto.FrameControl {
		t.Fatalf("snapshot response missing: %v", err)
	}
	control, err := proto.DecodeControl(frame.Payload)
	if err != nil || control.Type != "error" || control.Code != "snapshot_failed" {
		t.Fatalf("live-target execution failure = %+v %v, want snapshot_failed", control, err)
	}
}

func TestSnapshotTimeoutWithLiveTargetIsSnapshotFailed(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	d := newDisposable(t)
	authority, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server = "local", d.tmux.Label
	authority.SessionID = d.run("display-message", "-p", "-t", "=alpha:", "#{session_id}")
	authority.SessionCreated, _ = strconv.ParseInt(d.run("display-message", "-p", "-t", "=alpha:", "#{session_created}"), 10, 64)

	fakeDir := shortTempDir(t)
	fakeTmux := filepath.Join(fakeDir, "tmux")
	script := "#!/bin/sh\nif [ \"$4\" = \"if-shell\" ]; then\n  exec sleep 10\nfi\nexec " + shellQuote(tmuxPath) + " \"$@\"\n"
	if err := os.WriteFile(fakeTmux, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	s := &Server{config: config.Broker{Realm: "local", Servers: []config.TmuxServer{d.tmux}}}
	var wire bytes.Buffer
	s.snapshot(&lockedWriter{w: &wire}, proto.Control{Type: "snapshot", Authority: &authority, Depth: 20})
	frame, err := proto.ReadFrame(&wire)
	if err != nil || frame.Type != proto.FrameControl {
		t.Fatalf("snapshot response missing: %v", err)
	}
	control, err := proto.DecodeControl(frame.Payload)
	if err != nil || control.Type != "error" || control.Code != "snapshot_failed" {
		t.Fatalf("live-target timeout = %+v %v, want snapshot_failed", control, err)
	}
}

func TestSnapshotConfirmedVanishedPaneIsStaleTarget(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	d := newDisposable(t)
	d.run("split-window", "-h", "-t", "=alpha:", "sleep 600")
	authority, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server = "local", d.tmux.Label
	authority.SessionID = d.run("display-message", "-p", "-t", "=alpha:", "#{session_id}")
	authority.SessionCreated, _ = strconv.ParseInt(d.run("display-message", "-p", "-t", "=alpha:", "#{session_created}"), 10, 64)
	paneID := d.run("display-message", "-p", "-t", "=alpha:", "#{pane_id}")
	fakeDir := shortTempDir(t)
	fakeTmux := filepath.Join(fakeDir, "tmux")
	script := "#!/bin/sh\nif [ \"$4\" = \"if-shell\" ]; then\n  exec " + shellQuote(tmuxPath) + " -S " + shellQuote(d.path) + " kill-pane -t " + shellQuote(paneID) + "\nfi\nexec " + shellQuote(tmuxPath) + " \"$@\"\n"
	if err := os.WriteFile(fakeTmux, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	s := &Server{config: config.Broker{Realm: "local", Servers: []config.TmuxServer{d.tmux}}}
	var wire bytes.Buffer
	s.snapshot(&lockedWriter{w: &wire}, proto.Control{Type: "snapshot", Authority: &authority, Depth: 20})
	frame, err := proto.ReadFrame(&wire)
	if err != nil || frame.Type != proto.FrameControl {
		t.Fatalf("snapshot response missing: %v", err)
	}
	control, err := proto.DecodeControl(frame.Payload)
	if err != nil || control.Type != "error" || control.Code != "stale_target" {
		t.Fatalf("vanished pane response = %+v %v, want stale_target", control, err)
	}
}

func TestSnapshotConfirmedVanishedServerIsStaleTarget(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	d := newDisposable(t)
	authority, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server = "local", d.tmux.Label
	authority.SessionID = d.run("display-message", "-p", "-t", "=alpha:", "#{session_id}")
	authority.SessionCreated, _ = strconv.ParseInt(d.run("display-message", "-p", "-t", "=alpha:", "#{session_created}"), 10, 64)

	fakeDir := shortTempDir(t)
	fakeTmux := filepath.Join(fakeDir, "tmux")
	script := "#!/bin/sh\nif [ \"$4\" = \"if-shell\" ]; then\n  exec " + shellQuote(tmuxPath) + " -S " + shellQuote(d.path) + " kill-server\nfi\nexec " + shellQuote(tmuxPath) + " \"$@\"\n"
	if err := os.WriteFile(fakeTmux, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	s := &Server{config: config.Broker{Realm: "local", Servers: []config.TmuxServer{d.tmux}}}
	var wire bytes.Buffer
	s.snapshot(&lockedWriter{w: &wire}, proto.Control{Type: "snapshot", Authority: &authority, Depth: 20})
	frame, err := proto.ReadFrame(&wire)
	if err != nil || frame.Type != proto.FrameControl {
		t.Fatalf("snapshot response missing: %v", err)
	}
	control, err := proto.DecodeControl(frame.Payload)
	if err != nil || control.Type != "error" || control.Code != "stale_target" {
		t.Fatalf("vanished server response = %+v %v, want stale_target", control, err)
	}
}

func TestSnapshotServerVanishingDuringPaneRevalidationIsStaleTarget(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	d := newDisposable(t)
	authority, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server = "local", d.tmux.Label
	authority.SessionID = d.run("display-message", "-p", "-t", "=alpha:", "#{session_id}")
	authority.SessionCreated, _ = strconv.ParseInt(d.run("display-message", "-p", "-t", "=alpha:", "#{session_created}"), 10, 64)

	fakeDir := shortTempDir(t)
	fakeTmux := filepath.Join(fakeDir, "tmux")
	script := "#!/bin/sh\nif [ \"$4\" = \"if-shell\" ]; then\n  exit 1\nfi\nif [ \"$4\" = \"list-panes\" ]; then\n  " + shellQuote(tmuxPath) + " -S " + shellQuote(d.path) + " kill-server\nfi\nexec " + shellQuote(tmuxPath) + " \"$@\"\n"
	if err := os.WriteFile(fakeTmux, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	s := &Server{config: config.Broker{Realm: "local", Servers: []config.TmuxServer{d.tmux}}}
	var wire bytes.Buffer
	s.snapshot(&lockedWriter{w: &wire}, proto.Control{Type: "snapshot", Authority: &authority, Depth: 20})
	frame, err := proto.ReadFrame(&wire)
	if err != nil || frame.Type != proto.FrameControl {
		t.Fatalf("snapshot response missing: %v", err)
	}
	control, err := proto.DecodeControl(frame.Payload)
	if err != nil || control.Type != "error" || control.Code != "stale_target" {
		t.Fatalf("server vanished during pane revalidation = %+v %v, want stale_target", control, err)
	}
}
