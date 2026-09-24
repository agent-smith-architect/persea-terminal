package frontdoor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// font-auto-preference rollback. The tri-state is a one-way door for the
// store file: a release without configurable font sizes decodes `"font_size": null` into a
// plain `int` as a no-op zero and then fails its own 9…24 range check, which
// rejects the WHOLE file — one operator on auto costs every other operator
// their theme and default session. `deploy/preferences-store-downgrade.sh` is
// the repair, and these cases are what say it works.
//
// The old rule is not paraphrased here, it is re-declared: legacyPreferenceEntry
// carries the older `FontSize int`, and loadLegacyPreferencesStore is the
// older load path. A test that merely asserted "no nulls remain" would pass
// against a file the old binary still rejects.

type legacyPreferenceEntry struct {
	Operator       string          `json:"operator"`
	Theme          string          `json:"theme"`
	FontSize       int             `json:"font_size"`
	DefaultSession *DefaultSession `json:"default_session"`
	Revision       uint64          `json:"revision"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type legacyPreferencesStoreFile struct {
	Version   int                     `json:"version"`
	Operators []legacyPreferenceEntry `json:"operators"`
}

// loadLegacyPreferencesStore reproduces the older loader, including its
// strict decode (unknown fields are fatal, which is why the repair may only
// change values and never add a key) and its unconditional font-size range
// check on a non-pointer int.
func loadLegacyPreferencesStore(b []byte) error {
	var f legacyPreferencesStoreFile
	if err := decodeStrict(b, &f); err != nil {
		return errors.New("invalid preferences store")
	}
	if f.Version != preferencesStoreVersion || len(f.Operators) > preferencesStoreMaxRecords {
		return errors.New("unsupported or oversized preferences store")
	}
	seen := map[string]bool{}
	for _, e := range f.Operators {
		if !validOperatorLogin(e.Operator) || e.Revision == 0 || e.CreatedAt.IsZero() || e.UpdatedAt.Before(e.CreatedAt) {
			return errors.New("invalid preferences record")
		}
		if seen[e.Operator] {
			return errors.New("duplicate preferences operator")
		}
		seen[e.Operator] = true
		// The older validatePreferences, verbatim in its font clause.
		if !preferenceThemes[e.Theme] || e.FontSize < preferenceFontSizeMin || e.FontSize > preferenceFontSizeMax || !validDefaultSession(e.DefaultSession) {
			return errors.New("invalid preferences record")
		}
	}
	return nil
}

// A downgraded record has no null: every record an older release is asked to
// read must carry an integer font size inside the closed 9…24 range. The table
// states the rule directly, so it holds even where the script cannot run.
func TestLegacyPreferencesStoreRejectsAuto(t *testing.T) {
	const head = `{"version":1,"operators":[{"operator":"a@example.com","theme":"dracula",`
	const tail = `,"default_session":null,"revision":1,"created_at":"2026-08-01T10:00:00Z","updated_at":"2026-08-01T10:00:00Z"}]}`
	for _, c := range []struct {
		name  string
		font  string
		loads bool
	}{
		{"auto is rejected by the old loader", `"font_size":null`, false},
		{"an absent key decodes to zero and is rejected", ``, false},
		{"the downgrade value is accepted", `"font_size":14`, true},
		{"the floor is accepted", `"font_size":9`, true},
		{"the ceiling is accepted", `"font_size":24`, true},
		{"below the floor is rejected", `"font_size":8`, false},
		{"above the ceiling is rejected", `"font_size":25`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := head + c.font + tail
			if c.font == "" {
				body = head + strings.TrimPrefix(tail, ",")
			}
			err := loadLegacyPreferencesStore([]byte(body))
			if c.loads != (err == nil) {
				t.Fatalf("legacy load of %q -> %v, wanted loads=%v", c.font, err, c.loads)
			}
		})
	}
}

// downgradeScriptHarness resolves the shipped script, a private store path and
// a runner that invokes it under the hermetic root with the front reading as
// stopped — the state the runbook requires. Shared so every case that exercises
// the script exercises the SAME invocation, rather than a second imitation of
// it that could drift from the runbook.
func downgradeScriptHarness(t *testing.T) (func(args ...string) string, string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is unavailable: %v", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 is unavailable: %v", err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "deploy", "preferences-store-downgrade.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("downgrade script is missing: %v", err)
	}
	dir := shortTestDir(t)
	mock := filepath.Join(dir, "mock")
	if err := os.MkdirAll(mock, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mock, "systemctl"), []byte("#!/bin/bash\n[[ $1 == is-active ]] && { echo inactive; exit 3; }\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bash, append([]string{script}, args...)...)
		cmd.Env = append(os.Environ(),
			"PATH="+mock+string(os.PathListSeparator)+os.Getenv("PATH"),
			"PERSEA_DEPLOY_HERMETIC=1",
			"PERSEA_DEPLOY_ROOT="+dir,
			"PERSEA_DEPLOY_MOCK_BIN="+mock,
			"PERSEA_TEST_EUID=0",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("downgrade %v failed: %v\n%s", args, err, out)
		}
		return string(out)
	}
	return run, filepath.Join(dir, "preferences.json")
}

// End to end: a store this release wrote with an operator on auto, put through
// the shipped downgrade script, must load under the old rules — and must still
// load under the current ones, as an explicit size.
func TestPreferencesStoreDowngradeScript(t *testing.T) {
	run, path := downgradeScriptHarness(t)
	dir := filepath.Dir(path)
	s, err := newPreferencesStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// One operator on auto, one with an explicit size that must survive
	// untouched, so the case also pins that the repair is not a blanket rewrite.
	if _, err := s.put("auto@example.com", preferencesAuto("dracula"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.put("explicit@example.com", preferencesFixture("one-dark", 16), 0); err != nil {
		t.Fatal(err)
	}
	before := s.get("auto@example.com")
	if before.FontSize != nil {
		t.Fatalf("the seeded record is not on auto: %+v", before)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The blast radius itself, pinned: this is what an older binary is handed.
	if err := loadLegacyPreferencesStore(raw); err == nil {
		t.Fatal("an older release accepted a store containing auto; the downgrade would be pointless")
	}

	_ = dir
	if out := run("--dry-run", path); !strings.Contains(out, "would rewrite 1 record") {
		t.Fatalf("dry run did not report the pending rewrite: %s", out)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(raw) {
		t.Fatalf("dry run modified the store: err=%v changed=%v", err, string(after) != string(raw))
	}

	out := run(path)
	if !strings.Contains(out, "rewritten from auto to 14: 1") || !strings.Contains(out, "already explicit: 1") {
		t.Fatalf("summary did not report one rewrite and one untouched record: %s", out)
	}

	downgraded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := loadLegacyPreferencesStore(downgraded); err != nil {
		t.Fatalf("an older release still rejects the downgraded store: %v", err)
	}

	// A backup sits beside the file, and it is the pre-rewrite bytes.
	matches, err := filepath.Glob(path + ".pre-tristate-*.bak")
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one timestamped backup: %v %v", matches, err)
	}
	backup, err := os.ReadFile(matches[0])
	if err != nil || string(backup) != string(raw) {
		t.Fatalf("backup is not the pre-rewrite store: err=%v", err)
	}
	if info, err := os.Lstat(matches[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode=%v err=%v", info.Mode(), err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("downgraded store mode=%v err=%v", info.Mode(), err)
	}

	// Idempotent: a second run finds nothing to do and writes no second backup.
	if out := run(path); !strings.Contains(out, "no change") {
		t.Fatalf("a second run was not a no-op: %s", out)
	}
	if again, err := os.ReadFile(path); err != nil || string(again) != string(downgraded) {
		t.Fatalf("the second run changed the store: err=%v", err)
	}
	if matches, err := filepath.Glob(path + ".pre-tristate-*.bak"); err != nil || len(matches) != 1 {
		t.Fatalf("the second run wrote another backup: %v %v", matches, err)
	}

	// The current release still reads the repaired file, and reads the rewritten
	// operator as an explicit 14 — which is the cost the summary states.
	reopened, err := newPreferencesStore(path)
	if err != nil {
		t.Fatalf("the current release rejects the downgraded store: %v", err)
	}
	got := reopened.get("auto@example.com")
	if !fontIs(got.FontSize, 14) || got.Theme != "dracula" || got.Revision != before.Revision {
		t.Fatalf("the downgraded record lost fidelity: %+v (was revision %d)", got, before.Revision)
	}
	if got.DefaultSession == nil || got.DefaultSession.Name != "work" {
		t.Fatalf("the downgrade dropped the default session: %+v", got.DefaultSession)
	}
	if untouched := reopened.get("explicit@example.com"); !fontIs(untouched.FontSize, 16) || untouched.Theme != "one-dark" {
		t.Fatalf("the downgrade disturbed an explicit record: %+v", untouched)
	}
}
