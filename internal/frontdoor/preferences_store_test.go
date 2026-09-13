package frontdoor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fontPtr(n int) *int { return &n }

func fontIs(v *int, want int) bool { return v != nil && *v == want }

func fontEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func preferencesFixture(theme string, size int) Preferences {
	return Preferences{Version: 1, Theme: theme, FontSize: fontPtr(size), ComposerFontSize: preferenceDefaultComposerFontSize, DefaultSession: &DefaultSession{Realm: "desk-a7", Server: "primary", Name: "work"}}
}

// preferencesAuto is the same fixture with the font left on "auto" (J-UX-9).
func preferencesAuto(theme string) Preferences {
	return Preferences{Version: 1, Theme: theme, ComposerFontSize: preferenceDefaultComposerFontSize, DefaultSession: &DefaultSession{Realm: "desk-a7", Server: "primary", Name: "work"}}
}

func TestPreferencesStoreLifecycleAndCAS(t *testing.T) {
	dir := shortTestDir(t)
	path := filepath.Join(dir, "preferences.json")
	s, err := newPreferencesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.get("operator@example.com"); got.Stored || got.Revision != 0 || got.Theme != preferenceDefaultTheme || got.FontSize != nil || got.ComposerFontSize != preferenceDefaultComposerFontSize || got.DefaultSession != nil {
		t.Fatalf("defaults=%+v", got)
	}
	if _, err = s.put("operator@example.com", preferencesFixture("dracula", 16), 1); !errors.Is(err, errPreferencesConflict) {
		t.Fatalf("first put with revision 1=%v", err)
	}
	first, err := s.put("operator@example.com", preferencesFixture("dracula", 16), 0)
	if err != nil || !first.Stored || first.Revision != 1 || first.Theme != "dracula" || !fontIs(first.FontSize, 16) || first.DefaultSession == nil || first.DefaultSession.Name != "work" {
		t.Fatalf("first put=%+v %v", first, err)
	}
	current, err := s.put("operator@example.com", preferencesFixture("one-dark", 12), 0)
	if !errors.Is(err, errPreferencesConflict) || current.Revision != 1 || current.Theme != "dracula" {
		t.Fatalf("stale put returned %+v %v", current, err)
	}
	second, err := s.put("operator@example.com", Preferences{Version: 1, Theme: "one-dark", FontSize: fontPtr(12), ComposerFontSize: preferenceDefaultComposerFontSize}, 1)
	if err != nil || second.Revision != 2 || second.DefaultSession != nil {
		t.Fatalf("second put=%+v %v", second, err)
	}
	if got := s.get("someone-else@example.com"); got.Stored {
		t.Fatalf("operators leaked: %+v", got)
	}
	for name, p := range map[string]Preferences{
		"unknown_theme":   preferencesFixture("solarized-light", 14),
		"empty_theme":     preferencesFixture("", 14),
		"font_too_small":  preferencesFixture("default", 8),
		"font_too_large":  preferencesFixture("default", 25),
		"wrong_version":   {Version: 2, Theme: "default", FontSize: fontPtr(14)},
		"bad_realm":       {Version: 1, Theme: "default", FontSize: fontPtr(14), DefaultSession: &DefaultSession{Realm: "bad realm", Server: "s", Name: "n"}},
		"bad_server":      {Version: 1, Theme: "default", FontSize: fontPtr(14), DefaultSession: &DefaultSession{Realm: "r", Server: "", Name: "n"}},
		"empty_name":      {Version: 1, Theme: "default", FontSize: fontPtr(14), DefaultSession: &DefaultSession{Realm: "r", Server: "s", Name: ""}},
		"control_in_name": {Version: 1, Theme: "default", FontSize: fontPtr(14), DefaultSession: &DefaultSession{Realm: "r", Server: "s", Name: "a\x00b"}},
	} {
		if _, err := s.put("operator@example.com", p, 2); !errors.Is(err, errPreferencesValidation) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	if _, err := s.put("bad operator", preferencesFixture("default", 14), 0); !errors.Is(err, errPreferencesValidation) {
		t.Fatalf("operator with whitespace accepted: %v", err)
	}
	if got := s.get("operator@example.com"); got.Revision != 2 || got.Theme != "one-dark" {
		t.Fatalf("validation failures mutated the record: %+v", got)
	}
	reopened, err := newPreferencesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.get("operator@example.com"); !got.Stored || got.Revision != 2 || got.Theme != "one-dark" || !fontIs(got.FontSize, 12) {
		t.Fatalf("reopen lost record: %+v", got)
	}
	// A record handed to a caller never aliases the store's own entry: the
	// nullable font size and the default session are both copies, so writing
	// through one cannot rewrite what the next reader sees.
	handed := reopened.get("operator@example.com")
	*handed.FontSize = 9
	if again := reopened.get("operator@example.com"); !fontIs(again.FontSize, 12) {
		t.Fatalf("the returned record aliased the store: %+v", again)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("store file mode=%v err=%v", info.Mode(), err)
	}
}

// SF1 mirror for the preferences store: an injected crash at every phase of
// the publish sequence leaves either the prior file or the complete new file.
func TestPreferencesStorePersistenceFailureAlgebra(t *testing.T) {
	for _, phase := range []string{"temp-open", "write", "short-write", "file-sync", "close", "rename", "directory-open", "directory-sync"} {
		t.Run(phase, func(t *testing.T) {
			dir := shortTestDir(t)
			path := filepath.Join(dir, "preferences.json")
			s, err := newPreferencesStore(path)
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.put("operator@example.com", preferencesFixture("dracula", 16), 0)
			if err != nil {
				t.Fatal(err)
			}
			baseFS := s.file.fs
			switch phase {
			case "temp-open":
				s.file.fs = faultAliasFS{aliasFS: baseFS, tempOpen: true}
			case "write":
				s.file.writeFile = func(*os.File, []byte) (int, error) { return 0, errors.New("injected write") }
			case "short-write":
				s.file.writeFile = func(f *os.File, b []byte) (int, error) { return f.Write(b[:len(b)/2]) }
			case "file-sync":
				s.file.fileSync = func(*os.File) error { return errors.New("injected file sync") }
			case "close":
				s.file.closeFile = func(*os.File) error { return errors.New("injected close") }
			case "rename":
				s.file.fs = faultAliasFS{aliasFS: baseFS, rename: true}
			case "directory-open":
				s.file.fs = faultAliasFS{aliasFS: baseFS, dirOpen: true}
			case "directory-sync":
				s.file.dirSync = func(*os.File) error { return errors.New("injected directory sync") }
			}
			if _, err = s.put("operator@example.com", preferencesFixture("one-dark", 12), before.Revision); err == nil {
				t.Fatal("injected failure reported success")
			}
			got := s.get("operator@example.com")
			published := phase == "directory-open" || phase == "directory-sync"
			if published {
				if got.Theme != "one-dark" || got.Revision != 2 || s.file.fault == nil {
					t.Fatalf("post-publication state=%+v fault=%v", got, s.file.fault)
				}
				if _, e := s.put("operator@example.com", preferencesFixture("default", 14), 2); !errors.Is(e, errPreferencesStoreUnavailable) {
					t.Fatalf("faulted store mutation=%v", e)
				}
				reopened, e := newPreferencesStore(path)
				if e != nil || reopened.get("operator@example.com").Theme != "one-dark" {
					t.Fatalf("published disk mismatch: %v", e)
				}
			} else {
				if got.Theme != "dracula" || got.Revision != 1 || s.file.fault != nil {
					t.Fatalf("pre-publication rollback=%+v fault=%v", got, s.file.fault)
				}
				reopened, e := newPreferencesStore(path)
				if e != nil || reopened.get("operator@example.com").Theme != "dracula" {
					t.Fatalf("disk changed pre-publication: %v", e)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != "preferences.json" {
					t.Fatalf("residue after %s: %s", phase, entry.Name())
				}
			}
		})
	}
}

func TestPreferencesStoreStrictFilesystemAndData(t *testing.T) {
	dir := shortTestDir(t)
	valid := preferencesStoreFile{Version: 1, Operators: []preferenceEntry{{Operator: "operator@example.com", Theme: "dracula", FontSize: fontPtr(16), Revision: 1, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(2, 0).UTC()}}}
	validBytes, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, validBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := newPreferencesStore(target); err != nil || !s.get("operator@example.com").Stored {
		t.Fatalf("valid file refused: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newPreferencesStore(link); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := newPreferencesStore(target); err == nil {
		t.Fatal("unsafe mode accepted")
	}
	duplicate := preferencesStoreFile{Version: 1, Operators: append(valid.Operators, valid.Operators[0])}
	duplicateBytes, _ := json.Marshal(duplicate)
	badTheme := preferencesStoreFile{Version: 1, Operators: []preferenceEntry{{Operator: "operator@example.com", Theme: "nope", FontSize: fontPtr(16), Revision: 1, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0)}}}
	badThemeBytes, _ := json.Marshal(badTheme)
	zeroRevision := preferencesStoreFile{Version: 1, Operators: []preferenceEntry{{Operator: "operator@example.com", Theme: "default", FontSize: fontPtr(16), Revision: 0, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0)}}}
	zeroRevisionBytes, _ := json.Marshal(zeroRevision)
	for name, body := range map[string][]byte{
		"unknown-version":    []byte(`{"version":2,"operators":[]}`),
		"unknown-field":      []byte(`{"version":1,"operators":[],"extra":1}`),
		"duplicate-key":      []byte(`{"version":1,"version":1,"operators":[]}`),
		"folded-key":         []byte(`{"version":1,"operators":[],"OPERATORS":[]}`),
		"corrupt":            []byte(`{`),
		"trailing":           append(append([]byte{}, validBytes...), []byte(`{}`)...),
		"array":              []byte(`[]`),
		"invalid-utf8":       append(append([]byte{}, validBytes[:len(validBytes)-2]...), 0xff, '}', ']', '}'),
		"duplicate-operator": duplicateBytes,
		"invalid-theme":      badThemeBytes,
		"zero-revision":      zeroRevisionBytes,
		"oversize":           make([]byte, preferencesStoreMaxBytes+1),
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, body, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newPreferencesStore(p); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	unsafe := filepath.Join(dir, "unsafe")
	if err := os.Mkdir(unsafe, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := newPreferencesStore(filepath.Join(unsafe, "preferences.json")); err == nil {
		t.Fatal("unsafe parent mode accepted")
	}
	realParent := filepath.Join(dir, "real")
	if err := os.Mkdir(realParent, 0700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(dir, "linked")
	if err := os.Symlink(realParent, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := newPreferencesStore(filepath.Join(linked, "preferences.json")); err == nil {
		t.Fatal("symlink parent accepted")
	}
	if _, err := newPreferencesStore("relative/preferences.json"); err == nil {
		t.Fatal("relative path accepted")
	}
	if _, err := newPreferencesStore(realParent + "/../real/preferences.json"); err == nil {
		t.Fatal("unclean path accepted")
	}
}
