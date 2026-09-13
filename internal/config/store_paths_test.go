package config

import (
	"strings"
	"testing"
)

func TestLoadFrontOptionalStorePaths(t *testing.T) {
	base := `{"realms":[{"name":"r","socket":"/tmp/b.sock","broker_uid":0}],"alias_store_path":"/tmp/aliases.json"`
	c, err := LoadFront(write(t, base+`}`))
	if err != nil || c.PreferencesStorePath != "" || c.WorkspaceStorePath != "" || c.KeyboardPreferencesStorePath != "" {
		t.Fatalf("absent store paths: %+v %v", c, err)
	}
	c, err = LoadFront(write(t, base+`,"preferences_store_path":"/var/lib/persea-terminal/preferences.json"}`))
	if err != nil || c.PreferencesStorePath != "/var/lib/persea-terminal/preferences.json" {
		t.Fatalf("valid preferences path: %+v %v", c, err)
	}
	c, err = LoadFront(write(t, base+`,"preferences_store_path":"/var/lib/persea-terminal/preferences.json","snippet_store_path":"/var/lib/persea-terminal/snippets.json"}`))
	if err != nil || c.SnippetStorePath != "/var/lib/persea-terminal/snippets.json" {
		t.Fatalf("valid snippet path: %+v %v", c, err)
	}
	c, err = LoadFront(write(t, base+`,"preferences_store_path":"/var/lib/persea-terminal/preferences.json","snippet_store_path":"/var/lib/persea-terminal/snippets.json","workspace_store_path":"/var/lib/persea-terminal/workspaces.json"}`))
	if err != nil || c.WorkspaceStorePath != "/var/lib/persea-terminal/workspaces.json" {
		t.Fatalf("valid workspace path: %+v %v", c, err)
	}
	for name, extra := range map[string]string{
		"keyboard_relative":         `"keyboard_preferences_store_path":"keyboard-v1.json"`,
		"keyboard_unclean":          `"keyboard_preferences_store_path":"/tmp/./keyboard-v1.json"`,
		"keyboard_not_sibling":      `"keyboard_preferences_store_path":"/var/lib/keyboard-v1.json"`,
		"keyboard_reuses_alias":     `"keyboard_preferences_store_path":"/tmp/aliases.json"`,
		"keyboard_reuses_prefs":     `"preferences_store_path":"/tmp/p.json","keyboard_preferences_store_path":"/tmp/p.json"`,
		"keyboard_reuses_snippet":   `"snippet_store_path":"/tmp/s.json","keyboard_preferences_store_path":"/tmp/s.json"`,
		"keyboard_reuses_workspace": `"workspace_store_path":"/tmp/w.json","keyboard_preferences_store_path":"/tmp/w.json"`,
		"workspace_relative":        `"workspace_store_path":"workspaces.json"`,
		"workspace_unclean":         `"workspace_store_path":"/var/lib/persea-terminal/./workspaces.json"`,
		"workspace_reuses_alias":    `"workspace_store_path":"/tmp/aliases.json"`,
		"workspace_reuses_prefs":    `"preferences_store_path":"/tmp/p.json","workspace_store_path":"/tmp/p.json"`,
		"workspace_reuses_snippet":  `"snippet_store_path":"/tmp/s.json","workspace_store_path":"/tmp/s.json"`,
		"snippet_relative":          `"snippet_store_path":"snippets.json"`,
		"snippet_unclean":           `"snippet_store_path":"/var/lib/persea-terminal/./snippets.json"`,
		"snippet_reuses_alias":      `"snippet_store_path":"/tmp/aliases.json"`,
		"snippet_reuses_prefs":      `"preferences_store_path":"/tmp/p.json","snippet_store_path":"/tmp/p.json"`,
		"relative":                  `"preferences_store_path":"preferences.json"`,
		"unclean":                   `"preferences_store_path":"/var/lib/persea-terminal/../preferences.json"`,
		"trailing":                  `"preferences_store_path":"/var/lib/persea-terminal/"`,
		"reuses_alias":              `"preferences_store_path":"/tmp/aliases.json"`,
		"not_a_string":              `"preferences_store_path":1`,
	} {
		if _, err := LoadFront(write(t, base+`,`+extra+`}`)); err == nil || (name != "not_a_string" && !strings.Contains(err.Error(), "_store_path")) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	c, err = LoadFront(write(t, base+`,"keyboard_preferences_store_path":"/tmp/keyboard-v1.json"}`))
	if err != nil || c.KeyboardPreferencesStorePath != "/tmp/keyboard-v1.json" {
		t.Fatalf("valid keyboard path: %+v %v", c, err)
	}
}
