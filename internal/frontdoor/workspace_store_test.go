package frontdoor

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type workspaceNameLawFixture struct {
	Version        int `json:"version"`
	MaximumScalars int `json:"maximum_scalars"`
	ControlRanges  []struct {
		First rune `json:"first"`
		Last  rune `json:"last"`
	} `json:"control_ranges"`
	Cases []struct {
		ID             string  `json:"id"`
		Value          *string `json:"value"`
		Repeat         string  `json:"repeat"`
		Count          int     `json:"count"`
		UTF8Hex        string  `json:"utf8_hex"`
		Valid          bool    `json:"valid"`
		NormalizedName *string `json:"normalized_name"`
	} `json:"cases"`
}

func loadWorkspaceNameLaw(t *testing.T) workspaceNameLawFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "workspace_name_law.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture workspaceNameLawFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestWorkspaceNameLawCorpus(t *testing.T) {
	fixture := loadWorkspaceNameLaw(t)
	if fixture.Version != 1 || fixture.MaximumScalars != workspaceNameMaxRunes {
		t.Fatalf("fixture version=%d maximum=%d", fixture.Version, fixture.MaximumScalars)
	}
	for _, test := range fixture.Cases {
		t.Run(test.ID, func(t *testing.T) {
			var value string
			switch {
			case test.Value != nil:
				value = *test.Value
			case test.Repeat != "":
				value = strings.Repeat(test.Repeat, test.Count)
			case test.UTF8Hex != "":
				data, err := hex.DecodeString(test.UTF8Hex)
				if err != nil {
					t.Fatal(err)
				}
				value = string(data)
			default:
				t.Fatal("fixture has no Go value")
			}
			normalized, valid := workspaceLabel(value)
			if valid != test.Valid {
				t.Fatalf("valid=%t want=%t", valid, test.Valid)
			}
			if test.NormalizedName != nil && normalized != *test.NormalizedName {
				t.Fatalf("normalized=%q want=%q", normalized, *test.NormalizedName)
			}
		})
	}
	for _, controlRange := range fixture.ControlRanges {
		for scalar := controlRange.First; scalar <= controlRange.Last; scalar++ {
			if _, valid := workspaceLabel("x" + string(scalar) + "y"); valid {
				t.Fatalf("control U+%04X accepted", scalar)
			}
		}
	}
}

func workspaceLeaf(name string) WorkspaceNode {
	return WorkspaceNode{Kind: "leaf", Session: &WorkspaceSession{Realm: "local", Server: "private", Name: name}, OnMissing: "offer"}
}

func workspaceSixTree() WorkspaceNode {
	return WorkspaceNode{Kind: "split", Direction: "row", Weights: []int{55, 45}, Children: []WorkspaceNode{
		{Kind: "split", Direction: "column", Weights: []int{34, 33, 33}, Children: []WorkspaceNode{workspaceLeaf("one"), workspaceLeaf("two"), workspaceLeaf("three")}},
		{Kind: "split", Direction: "column", Weights: []int{25, 35, 40}, Children: []WorkspaceNode{workspaceLeaf("four"), workspaceLeaf("five"), workspaceLeaf("six")}},
	}}
}

func openWorkspaceTestStore(t *testing.T) (*workspaceStore, string) {
	t.Helper()
	dir := shortTestDir(t)
	path := filepath.Join(dir, "workspaces.json")
	store, err := newWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	next := 0
	store.newID = func() (string, error) {
		next++
		return fmt.Sprintf("%032x", next), nil
	}
	store.now = func() time.Time { return time.Unix(1700000000+int64(next), 0).UTC() }
	return store, path
}

func workspaceStoreRecords(t *testing.T, store *workspaceStore) []WorkspaceRecord {
	t.Helper()
	records, err := store.list()
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestWorkspaceStoreRoundTripAndStrictCaps(t *testing.T) {
	store, path := openWorkspaceTestStore(t)
	record, err := store.create("Ops 🛰", workspaceSixTree())
	if err != nil {
		t.Fatal(err)
	}
	if record.Revision != 1 || record.NormalizedName != "ops 🛰" || len(workspaceStoreRecords(t, store)) != 1 {
		t.Fatalf("created=%+v list=%d", record, len(workspaceStoreRecords(t, store)))
	}
	reopened, err := newWorkspaceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := workspaceStoreRecords(t, reopened)
	if len(got) != 1 || got[0].WorkspaceID != record.WorkspaceID || got[0].Name != record.Name || got[0].Revision != 1 {
		t.Fatalf("reopened=%+v", got)
	}
	want, _ := json.Marshal(record.Tree)
	have, _ := json.Marshal(got[0].Tree)
	if string(want) != string(have) {
		t.Fatalf("tree round trip\nwant=%s\nhave=%s", want, have)
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || parent.Mode().Perm() != 0700 {
		t.Fatalf("parent=%v err=%v", parent, err)
	}
	file, err := os.Stat(path)
	if err != nil || file.Mode().Perm() != 0600 || file.Size() > workspaceStoreMaxBytes {
		t.Fatalf("file=%v err=%v", file, err)
	}

	for i := 1; i < workspaceStoreMaxRecords; i++ {
		name := fmt.Sprintf("workspace-%02d", i)
		if _, err := store.create(name, workspaceLeaf("session-"+name)); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	if _, err := store.create("sixty-five", workspaceLeaf("overflow")); !errors.Is(err, errWorkspaceStoreFull) {
		t.Fatalf("65th record err=%v", err)
	}
}

func TestWorkspaceStoreTransactionalConcurrency(t *testing.T) {
	store, _ := openWorkspaceTestStore(t)
	record, err := store.create("Alpha", workspaceLeaf("one"))
	if err != nil {
		t.Fatal(err)
	}
	if current, err := store.create("alpha", workspaceLeaf("other")); !errors.Is(err, errWorkspaceConflict) || current.WorkspaceID != record.WorkspaceID {
		t.Fatalf("case-fold collision current=%+v err=%v", current, err)
	}

	type result struct {
		record WorkspaceRecord
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, name := range []string{"Winner A", "Winner B"} {
		name := name
		go func() {
			<-start
			next, err := store.update(record.WorkspaceID, name, workspaceLeaf("one"), record.Revision)
			results <- result{next, err}
		}()
	}
	close(start)
	a, b := <-results, <-results
	if (a.err == nil) == (b.err == nil) {
		t.Fatalf("concurrent results a=%v b=%v", a.err, b.err)
	}
	loser := a
	if a.err == nil {
		loser = b
	}
	if !errors.Is(loser.err, errWorkspaceConflict) || loser.record.Revision != 2 {
		t.Fatalf("loser=%+v err=%v", loser.record, loser.err)
	}
	if _, err := store.update(record.WorkspaceID, "Replay", workspaceLeaf("one"), record.Revision); !errors.Is(err, errWorkspaceConflict) {
		t.Fatalf("stale replay err=%v", err)
	}
	current := workspaceStoreRecords(t, store)[0]
	if stale, err := store.delete(current.WorkspaceID, current.Revision-1); !errors.Is(err, errWorkspaceConflict) || stale.Revision != current.Revision {
		t.Fatalf("stale delete=%+v err=%v", stale, err)
	}
	if _, err := store.delete(current.WorkspaceID, current.Revision); err != nil || len(workspaceStoreRecords(t, store)) != 0 {
		t.Fatalf("delete err=%v list=%d", err, len(workspaceStoreRecords(t, store)))
	}
}

func TestWorkspaceStoreValidationAndWholeStoreFailure(t *testing.T) {
	badTrees := []WorkspaceNode{
		{},
		{Kind: "mystery"},
		{Kind: "leaf", Session: &WorkspaceSession{Realm: "r", Server: "s", Name: "x"}, OnMissing: "guess"},
		{Kind: "split", Direction: "row", Weights: []int{1}, Children: []WorkspaceNode{workspaceLeaf("one"), workspaceLeaf("two")}},
		{Kind: "split", Direction: "row", Weights: []int{1, 1}, Children: []WorkspaceNode{workspaceLeaf("same"), workspaceLeaf("same")}},
	}
	store, _ := openWorkspaceTestStore(t)
	for i, tree := range badTrees {
		if _, err := store.create("bad-"+string(rune('a'+i)), tree); !errors.Is(err, errWorkspaceValidation) {
			t.Fatalf("bad tree %d err=%v", i, err)
		}
	}
	for _, name := range []string{"", " padded", "padded ", "bad\u0000name", strings.Repeat("🛰", workspaceNameMaxRunes+1)} {
		if _, err := store.create(name, workspaceLeaf("ok")); !errors.Is(err, errWorkspaceValidation) {
			t.Fatalf("bad name %q err=%v", name, err)
		}
	}

	cases := map[string][]byte{
		"unknown_version": []byte(`{"version":2,"workspaces":[]}`),
		"unknown_field":   []byte(`{"version":1,"workspaces":[],"handle":"secret"}`),
		"duplicate_key":   []byte(`{"version":1,"version":1,"workspaces":[]}`),
		"oversize":        make([]byte, workspaceStoreMaxBytes+1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			dir := shortTestDir(t)
			path := filepath.Join(dir, "workspaces.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := newWorkspaceStore(path); err == nil {
				t.Fatal("invalid whole store opened")
			}
		})
	}
}

func TestWorkspaceStoreAtomicWriteFailuresPreserveWholeFile(t *testing.T) {
	for _, step := range []string{"write", "sync", "close", "dirsync"} {
		t.Run(step, func(t *testing.T) {
			store, path := openWorkspaceTestStore(t)
			base, err := store.create("Before", workspaceLeaf("one"))
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch step {
			case "write":
				store.file.writeFile = func(*os.File, []byte) (int, error) { return 0, errors.New("write fault") }
			case "sync":
				store.file.fileSync = func(*os.File) error { return errors.New("sync fault") }
			case "close":
				store.file.closeFile = func(*os.File) error { return errors.New("close fault") }
			case "dirsync":
				store.file.dirSync = func(*os.File) error { return errors.New("dir sync fault") }
			}
			_, err = store.update(base.WorkspaceID, "After", workspaceLeaf("one"), base.Revision)
			if err == nil {
				t.Fatal("injected write succeeded")
			}
			reopened, reopenErr := newWorkspaceStore(path)
			if reopenErr != nil {
				t.Fatal(reopenErr)
			}
			got := workspaceStoreRecords(t, reopened)
			if len(got) != 1 {
				t.Fatalf("reopened=%+v", got)
			}
			if step == "dirsync" {
				if got[0].Name != "After" {
					t.Fatalf("post-rename file=%+v", got[0])
				}
			} else {
				if got[0].Name != "Before" {
					t.Fatalf("pre-rename file=%+v", got[0])
				}
				now, _ := os.ReadFile(path)
				if string(now) != string(original) {
					t.Fatal("failed pre-rename write changed durable bytes")
				}
			}
		})
	}
}
