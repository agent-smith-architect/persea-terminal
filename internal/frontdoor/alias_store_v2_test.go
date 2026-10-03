package frontdoor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func aliasTarget(pid int, id, name string) aliasSession {
	return aliasSession{Authority: auth(pid, id), Name: name}
}

func memoryAliases(t *testing.T) *aliasStore {
	t.Helper()
	s, err := newAliasStore("")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustAlias(t *testing.T, s *aliasStore, display string, target aliasSession) AliasRecord {
	t.Helper()
	r, err := s.create(display, target)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAliasRestartAndRenameFollowSession(t *testing.T) {
	s := memoryAliases(t)
	first := aliasTarget(1, "$0", "tm2")
	r := mustAlias(t, s, "Research", first)
	complete := map[string]bool{"r\x00s": true}
	renamed := first
	renamed.Name = "tm3"
	if err := s.reconcile([]aliasSession{renamed}, complete); err != nil {
		t.Fatal(err)
	}
	r = s.list()[0]
	if r.SessionName != "tm3" || r.State != "active" || r.Revision != 2 {
		t.Fatalf("rename did not follow: %+v", r)
	}
	replacement := aliasTarget(2, "$0", "tm3")
	replacement.Authority.BootID = "next-boot"
	if err := s.reconcile([]aliasSession{replacement}, complete); err != nil {
		t.Fatal(err)
	}
	r = s.list()[0]
	if r.State != "active" || !sameAuthority(r.Incarnation, replacement.Authority) || r.Revision != 3 {
		t.Fatalf("restart did not rebind: %+v", r)
	}
	if err := s.reconcile([]aliasSession{replacement}, complete); err != nil || s.list()[0] != r {
		t.Fatalf("unchanged inventory rewrote record: %v", err)
	}
}

func TestAliasDetachedClaimsAndExactWitnessPriority(t *testing.T) {
	for _, tied := range []bool{false, true} {
		t.Run(fmt.Sprint(tied), func(t *testing.T) {
			s := memoryAliases(t)
			older := mustAlias(t, s, "Older", aliasTarget(1, "$0", "tm2"))
			newer := mustAlias(t, s, "Newer", aliasTarget(1, "$1", "tm3"))
			older.AliasID, newer.AliasID = "a", "b"
			older.State, newer.State = "detached", "detached"
			newer.SessionName = "tm2"
			older.UpdatedAt = time.Unix(100, 0)
			newer.UpdatedAt = time.Unix(101, 0)
			want := newer.AliasID
			if tied {
				newer.UpdatedAt = older.UpdatedAt
				want = older.AliasID
			}
			s.records = map[string]AliasRecord{older.AliasID: older, newer.AliasID: newer}
			if err := s.reconcile([]aliasSession{aliasTarget(2, "$7", "tm2")}, map[string]bool{"r\x00s": true}); err != nil {
				t.Fatal(err)
			}
			for id, got := range s.records {
				if (got.State == "active") != (id == want) {
					t.Fatalf("claim winner=%s record=%+v", want, got)
				}
			}
			// A live incarnation follows a rename even when a newer record claims its name.
			s.records[older.AliasID] = older
			s.records[newer.AliasID] = newer
			if err := s.reconcile([]aliasSession{{Authority: older.Incarnation, Name: "tm4"}}, map[string]bool{"r\x00s": true}); err != nil {
				t.Fatal(err)
			}
			if s.records[older.AliasID].State != "active" || s.records[older.AliasID].SessionName != "tm4" || s.records[newer.AliasID].State != "detached" {
				t.Fatal("exact witness lost to name claim")
			}
		})
	}
}

func TestAliasActiveUniquenessSupersedesDetachedAndOnePerSession(t *testing.T) {
	s := memoryAliases(t)
	first := aliasTarget(1, "$0", "tm2")
	second := aliasTarget(1, "$1", "tm3")
	a := mustAlias(t, s, "Work", first)
	if current, err := s.create("Other", first); !errors.Is(err, errAliasExists) || current != a {
		t.Fatalf("second alias accepted: %+v %v", current, err)
	}
	if _, err := s.create("work", second); !errors.Is(err, errAliasInUse) {
		t.Fatalf("active duplicate accepted: %v", err)
	}
	b := mustAlias(t, s, "Play", second)
	if _, err := s.update(b.AliasID, "WORK", b.Revision); !errors.Is(err, errAliasInUse) {
		t.Fatalf("active text conflict accepted: %v", err)
	}
	if err := s.reconcile([]aliasSession{second}, map[string]bool{"r\x00s": true}); err != nil {
		t.Fatal(err)
	}
	b, err := s.update(b.AliasID, "Work", b.Revision)
	if err != nil || len(s.list()) != 1 || s.records[a.AliasID].AliasID != "" {
		t.Fatalf("detached name blocked update or survived: %v %+v", err, s.list())
	}
	if err := s.reconcile(nil, map[string]bool{"r\x00s": true}); err != nil {
		t.Fatal(err)
	}
	c := mustAlias(t, s, "work", aliasTarget(2, "$5", "tm5"))
	if len(s.list()) != 1 || s.records[b.AliasID].AliasID != "" || c.State != "active" {
		t.Fatalf("detached name blocked create or survived: %+v", s.list())
	}
}

func TestAliasIncompleteInventoryNeverChangesRecords(t *testing.T) {
	s := memoryAliases(t)
	target := aliasTarget(1, "$0", "tm2")
	r := mustAlias(t, s, "Work", target)
	target.Name = "renamed"
	if err := s.reconcile([]aliasSession{target}, map[string]bool{}); err != nil || s.list()[0] != r {
		t.Fatalf("partial inventory changed alias: %v %+v", err, s.list())
	}
	if err := s.reconcile(nil, map[string]bool{}); err != nil || s.list()[0] != r {
		t.Fatal("missing partial inventory detached alias")
	}
}

func TestAliasOldStoreResets(t *testing.T) {
	var logs []string
	priorLog := frontLogf
	frontLogf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { frontLogf = priorLog })
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "aliases.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"aliases":[{"alias_id":"old","display_alias":"Work","state":"tombstone"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := newAliasStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk aliasStoreFile
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &disk); err != nil || disk.Version != 2 || len(disk.Aliases) != 0 || len(s.list()) != 0 {
		t.Fatalf("old store not replaced: %s %v", data, err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "event=alias_store_reset") {
		t.Fatalf("reset log=%v", logs)
	}
}

func TestAliasRebindStaysWithinRealmAndServer(t *testing.T) {
	s := memoryAliases(t)
	r := mustAlias(t, s, "Work", aliasTarget(1, "$0", "tm2"))
	for _, otherRealm := range []bool{false, true} {
		target := aliasTarget(2, "$0", "tm2")
		if otherRealm {
			target.Authority.Realm = "other"
		} else {
			target.Authority.Server = "other"
		}
		if err := s.reconcile([]aliasSession{target}, map[string]bool{"r\x00s": true, "r\x00other": true, "other\x00s": true}); err != nil {
			t.Fatal(err)
		}
		got := s.list()[0]
		if got.State != "detached" || !sameAuthority(got.Incarnation, r.Incarnation) {
			t.Fatalf("alias crossed its name scope: %+v", got)
		}
	}
}

func TestAliasSupersessionRollsBackAndUnchangedInventoryDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := newAliasStore(filepath.Join(dir, "aliases.json"))
	if err != nil {
		t.Fatal(err)
	}
	target := aliasTarget(1, "$0", "tm2")
	r := mustAlias(t, s, "Work", target)
	s.fs = faultAliasFS{aliasFS: s.fs, rename: true}
	if err := s.reconcile([]aliasSession{target}, map[string]bool{"r\x00s": true}); err != nil || s.list()[0] != r {
		t.Fatal("unchanged inventory attempted to persist")
	}
	s.records[r.AliasID] = func() AliasRecord { r.State = "detached"; return r }()
	if _, err := s.create("work", aliasTarget(2, "$4", "tm3")); err == nil {
		t.Fatal("failed durable supersession reported success")
	}
	if len(s.list()) != 1 || s.list()[0] != r {
		t.Fatalf("supersession failure lost detached record: %+v", s.list())
	}
}

func TestAliasLimitEvictsOldestDetached(t *testing.T) {
	s := memoryAliases(t)
	for i := 0; i < aliasStoreMaxRecords; i++ {
		id := fmt.Sprint(i)
		a := auth(1, "$"+id)
		s.records[id] = AliasRecord{AliasID: id, DisplayAlias: id, NormalizedAlias: id, Realm: "r", Server: "s", SessionName: "tm" + id, Incarnation: a, State: "active", Revision: 1, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(int64(i+1), 0)}
	}
	if _, err := s.create("New", aliasTarget(2, "$new", "new")); !errors.Is(err, errAliasStoreUnavailable) || len(s.records) != aliasStoreMaxRecords {
		t.Fatalf("full live store=%v", err)
	}
	for _, id := range []string{"4", "8"} {
		r := s.records[id]
		r.State = "detached"
		s.records[id] = r
	}
	mustAlias(t, s, "New", aliasTarget(2, "$new", "new"))
	if _, exists := s.records["4"]; exists || s.records["8"].State != "detached" || len(s.records) != aliasStoreMaxRecords {
		t.Fatal("did not evict oldest detached")
	}
}

func TestAliasCharacterValidation(t *testing.T) {
	for _, display := range []string{"", "  ", "x\n", "\tx", strings.Repeat("界", 129), string([]byte{0xff})} {
		if _, err := normalizeAlias(display); !errors.Is(err, errAliasValidation) {
			t.Fatalf("accepted invalid %q", display)
		}
	}
	if _, err := normalizeAlias(strings.Repeat("界", 128)); err != nil {
		t.Fatal("128 characters were measured as bytes")
	}
}

// A live session keeps its alias through a rename even when the inventory also
// holds a replacement with the old name and a newer alias claims the new name.
func TestAliasExactWitnessBeatsNewerNameClaims(t *testing.T) {
	s := memoryAliases(t)
	s.now = func() time.Time { return time.Unix(10, 0) }
	original := mustAlias(t, s, "Original", aliasTarget(1, "$0", "tm2"))
	competitor := mustAlias(t, s, "Competitor", aliasTarget(1, "$1", "tm3"))
	s.now = func() time.Time { return time.Unix(20, 0) }
	complete := map[string]bool{"r\x00s": true}
	if err := s.reconcile([]aliasSession{aliasTarget(1, "$0", "tm2")}, complete); err != nil {
		t.Fatal(err)
	}
	if s.records[competitor.AliasID].State != "detached" || s.records[competitor.AliasID].UpdatedAt.Before(s.records[original.AliasID].UpdatedAt) {
		t.Fatalf("competitor is not the newer detached claim: %+v", s.records[competitor.AliasID])
	}
	if err := s.reconcile([]aliasSession{aliasTarget(1, "$0", "tm3"), aliasTarget(2, "$2", "tm2")}, complete); err != nil {
		t.Fatal(err)
	}
	got, other := s.records[original.AliasID], s.records[competitor.AliasID]
	if got.State != "active" || got.SessionName != "tm3" || !sameAuthority(got.Incarnation, original.Incarnation) || other.State != "detached" {
		t.Fatalf("exact witness lost to a name claim: original=%+v competitor=%+v", got, other)
	}
}

// The byte limit is reached long before the record limit. Room is made by
// removing the oldest detached aliases, never active ones.
func TestAliasByteLimitEvictsOldestDetachedOnDisk(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := newAliasStore(filepath.Join(dir, "aliases.json"))
	if err != nil {
		t.Fatal(err)
	}
	live := mustAlias(t, s, "Live", aliasTarget(9, "$live", "live"))
	for i := 0; ; i++ {
		id := fmt.Sprintf("%032x", i)
		r := AliasRecord{AliasID: id, DisplayAlias: fmt.Sprintf("Record%05d %s", i, strings.Repeat("界", 100)), Realm: "r", Server: "s", SessionName: fmt.Sprintf("tm%d", i), Incarnation: auth(7, fmt.Sprintf("$%d", i)), State: "detached", Revision: 2, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(int64(100+i), 0)}
		r.NormalizedAlias, _ = normalizeAlias(r.DisplayAlias)
		s.records[id] = r
		if data, _ := json.Marshal(aliasStoreFile{Version: aliasStoreVersion, Aliases: s.list()}); len(data) > aliasStoreMaxBytes-600 {
			break
		}
	}
	if len(s.records) >= aliasStoreMaxRecords {
		t.Fatal("fixture reached the record limit instead of the byte limit")
	}
	if err = s.persistLocked(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err = s.create(fmt.Sprintf("New%d %s", i, strings.Repeat("界", 120)), aliasTarget(9, fmt.Sprintf("$n%d", i), fmt.Sprintf("new%d", i))); err != nil {
			t.Fatalf("create %d refused with detached history present: %v", i, err)
		}
	}
	if _, kept := s.records[fmt.Sprintf("%032x", 0)]; kept {
		t.Fatal("oldest detached alias was not removed first")
	}
	if s.records[live.AliasID].State != "active" {
		t.Fatal("an active alias was removed")
	}
	reopened, err := newAliasStore(filepath.Join(dir, "aliases.json"))
	if err != nil || len(reopened.list()) != len(s.list()) {
		t.Fatalf("store over its byte limit or unreadable: %v", err)
	}
	full := memoryAliases(t)
	for i := 0; ; i++ {
		id := fmt.Sprintf("%032x", i)
		r := AliasRecord{AliasID: id, DisplayAlias: fmt.Sprintf("Active%05d %s", i, strings.Repeat("界", 100)), Realm: "r", Server: "s", SessionName: fmt.Sprintf("tm%d", i), Incarnation: auth(7, fmt.Sprintf("$%d", i)), State: "active", Revision: 1, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0)}
		r.NormalizedAlias, _ = normalizeAlias(r.DisplayAlias)
		full.records[id] = r
		if data, _ := json.Marshal(aliasStoreFile{Version: aliasStoreVersion, Aliases: full.list()}); len(data) > aliasStoreMaxBytes-600 {
			break
		}
	}
	count := len(full.records)
	_, err = full.create(fmt.Sprintf("Over %s", strings.Repeat("界", 120)), aliasTarget(10, "$over", "over"))
	if len(full.records) != count {
		t.Fatal("a refused create changed the store")
	}
	if !errors.Is(err, errAliasStoreUnavailable) {
		t.Fatalf("an all-active full store did not refuse: %v", err)
	}
}
