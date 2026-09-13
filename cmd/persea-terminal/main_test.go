package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrokerRequiresUnifiedBeforeServing(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "disabled"}[disabled], func(t *testing.T) {
			dir := t.TempDir()
			value := map[string]any{"realm": "test", "front_uid": os.Getuid(), "servers": []map[string]any{{"label": "private", "socket_path": filepath.Join(dir, "absent-tmux.sock")}}}
			if disabled {
				value["unified_terminal_dev"] = map[string]any{"enabled": false}
			}
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "broker.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			socket := filepath.Join(dir, "absent", "broker.sock")
			err = run([]string{"broker", "--config", path, "--socket", socket})
			if err == nil || !strings.Contains(err.Error(), "requires enabled unified_terminal_dev") {
				t.Fatalf("unsupported serving config: %v", err)
			}
			if _, err := os.Stat(socket); !os.IsNotExist(err) {
				t.Fatalf("unsupported config created socket: %v", err)
			}
		})
	}
}

func TestFrontRequiresExplicitStaticDir(t *testing.T) {
	err := run([]string{"front", "--config", "missing.json"})
	if err == nil || !strings.Contains(err.Error(), "--static-dir") {
		t.Fatalf("run error = %v, want explicit static-dir requirement", err)
	}
}

func TestFrontRejectsRemovedListenFlag(t *testing.T) {
	if err := run([]string{"front", "--listen", "127.0.0.1:1", "--config", "x", "--static-dir", "ui/dist"}); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("removed --listen accepted: %v", err)
	}
}
