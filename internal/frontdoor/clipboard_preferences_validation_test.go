package frontdoor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClipboardPreferencesPersistedGrammar(t *testing.T) {
	for name, body := range map[string]string{
		"missing-version":  `{"default_retention_seconds":1800,"revision":1}`,
		"missing-policy":   `{"version":1,"revision":1}`,
		"missing-revision": `{"version":1,"default_retention_seconds":1800}`,
		"null-policy":      `{"version":1,"default_retention_seconds":null,"revision":1}`,
		"null-version":     `{"version":null,"default_retention_seconds":1800,"revision":1}`,
		"null-revision":    `{"version":1,"default_retention_seconds":1800,"revision":null}`,
		"duplicate-policy": `{"version":1,"default_retention_seconds":0,"default_retention_seconds":1800,"revision":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := shortTestDir(t)
			path := filepath.Join(dir, "clipboard-preferences.json")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if store, err := openClipboardPreferencesStore(filepath.Join(dir, "snippets.json")); err == nil {
				_ = store.file.fs.(rootAliasFS).root.Close()
				t.Fatal("malformed persisted preferences accepted")
			}
			unchanged, err := os.ReadFile(path)
			if err != nil || string(unchanged) != body {
				t.Fatal("refused preferences were rewritten", err)
			}
		})
	}
}
