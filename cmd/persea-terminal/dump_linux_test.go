//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestUnifiedBrokerDumpabilityFailurePrecedesJournalOpen(t *testing.T) {
	runtimeDir := t.TempDir() + "/journal"
	configPath := t.TempDir() + "/broker.json"
	value := map[string]any{
		"realm":     "test",
		"front_uid": os.Getuid(),
		"servers":   []map[string]any{{"label": "default", "socket_name": "default"}},
		"session_create": map[string]any{
			"enabled": true, "servers": []string{"default"},
		},
		"unified_terminal_dev": map[string]any{
			"enabled": true, "server": "default", "session": "unified-dev",
			"observer_session": "observer-main", "runtime_dir": runtimeDir,
		},
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("dumpability sentinel")
	old := setBrokerDumpability
	setBrokerDumpability = func() error { return want }
	t.Cleanup(func() { setBrokerDumpability = old })
	err = run([]string{"broker", "--socket", t.TempDir() + "/broker.sock", "--config", configPath})
	if !errors.Is(err, want) {
		t.Fatalf("run error = %v, want %v", err, want)
	}
	if _, err := os.Stat(runtimeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal runtime was touched before dumpability failure: %v", err)
	}
}

func TestDisableProcessDumpabilityInChild(t *testing.T) {
	if os.Getenv("PERSEA_DUMPABILITY_CHILD") == "1" {
		if err := disableProcessDumpability(); err != nil {
			t.Fatal(err)
		}
		value, err := processDumpability()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("DUMPABLE=%d\n", value)
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestDisableProcessDumpabilityInChild$")
	command.Env = append(os.Environ(), "PERSEA_DUMPABILITY_CHILD=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("dumpability child: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "DUMPABLE=0") {
		t.Fatalf("dumpability child output = %q", output)
	}
}
