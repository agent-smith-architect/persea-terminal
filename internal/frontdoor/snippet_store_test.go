package frontdoor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fastSnippetStore disables the fsync seams for bulk fixtures; the durability
// algebra has its own test.
func fastSnippetStore(t *testing.T, dir string) *snippetStore {
	t.Helper()
	s, err := newSnippetStore(filepath.Join(dir, "snippets.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.file.fileSync = func(*os.File) error { return nil }
	s.file.dirSync = func(*os.File) error { return nil }
	return s
}

func snippetIDs(views []snippetView) []string {
	ids := make([]string, 0, len(views))
	for _, v := range views {
		ids = append(ids, v.ID)
	}
	return ids
}

func TestSnippetStoreLifecycleAndOrdering(t *testing.T) {
	for _, year := range []int{2026, 2027} {
		t.Run(strconv.Itoa(year), func(t *testing.T) {
			dir := shortTestDir(t)
			path := filepath.Join(dir, "snippets.json")
			clock := time.Date(year, 8, 27, 12, 0, 0, 0, time.UTC)
			now := func() time.Time { return clock }
			s, err := newSnippetStoreWithClock(path, now)
			if err != nil {
				t.Fatal(err)
			}
			tick := func() { clock = clock.Add(time.Second) }

			plain, err := s.create(snippetKindSnippet, "  Deploy  ", "make deploy\n", false, "laptop")
			if err != nil || plain.Label != "Deploy" || plain.Revision != 1 || plain.ExpiresAt != nil || plain.Origin != "laptop" || !snippetIDRE.MatchString(plain.ID) {
				t.Fatalf("snippet=%+v err=%v", plain, err)
			}
			tick()
			pinned, err := s.create(snippetKindSnippet, "Logs", "tail -f\tlog", true, "")
			if err != nil || !pinned.Pinned {
				t.Fatalf("pinned=%+v %v", pinned, err)
			}
			tick()
			clip, err := s.create(snippetKindClip, "", "line one\nline two", false, "iphone")
			if err != nil || clip.ExpiresAt == nil || !clip.ExpiresAt.Equal(clock.Add(snippetClipTTL)) || clip.Label != "" {
				t.Fatalf("clip=%+v %v", clip, err)
			}
			list := s.list()
			if got := snippetIDs(list); len(got) != 3 || got[0] != clip.ID || got[1] != pinned.ID || got[2] != plain.ID {
				t.Fatalf("order=%v want newest clip, older snippets", got)
			}
			if list[0].Preview != "line one line two" || list[1].Preview != "" || list[2].Preview != "" {
				t.Fatalf("previews=%q %q %q", list[0].Preview, list[1].Preview, list[2].Preview)
			}

			tick()
			stale, err := s.update(plain.ID, snippetUpdate{Label: strPtr("Ship")}, 7)
			if !errors.Is(err, errSnippetConflict) || stale.ID != plain.ID || stale.Revision != 1 {
				t.Fatalf("stale update=%+v %v", stale, err)
			}
			renamed, err := s.update(plain.ID, snippetUpdate{Label: strPtr("Ship"), Pinned: boolPtr(true)}, 1)
			if err != nil || renamed.Label != "Ship" || !renamed.Pinned || renamed.Revision != 2 || renamed.Body != "make deploy\n" || !renamed.UpdatedAt.Equal(clock) {
				t.Fatalf("renamed=%+v %v", renamed, err)
			}
			if got := snippetIDs(s.list()); got[0] != renamed.ID || got[1] != clip.ID {
				t.Fatalf("updated_at desc broken: %v", got)
			}
			if _, err = s.update(clip.ID, snippetUpdate{Label: strPtr("x")}, 1); !errors.Is(err, errSnippetValidation) {
				t.Fatalf("clip label update=%v", err)
			}
			if _, err = s.update(clip.ID, snippetUpdate{Pinned: boolPtr(true)}, 1); !errors.Is(err, errSnippetValidation) {
				t.Fatalf("clip pin update=%v", err)
			}
			if _, err = s.update(clip.ID, snippetUpdate{}, 1); !errors.Is(err, errSnippetValidation) {
				t.Fatalf("empty update=%v", err)
			}
			tick()
			edited, err := s.update(clip.ID, snippetUpdate{Body: strPtr("edited")}, 1)
			if err != nil || edited.Body != "edited" || edited.Revision != 2 || !edited.ExpiresAt.Equal(clock.Add(snippetClipTTL)) || !edited.ExpiresAt.After(*clip.ExpiresAt) {
				t.Fatalf("clip body update=%+v %v", edited, err)
			}
			if _, err = s.delete(pinned.ID, 3); !errors.Is(err, errSnippetConflict) {
				t.Fatalf("stale delete=%v", err)
			}
			if _, err = s.delete(pinned.ID, 1); err != nil {
				t.Fatal(err)
			}
			if _, err = s.delete(pinned.ID, 1); !errors.Is(err, errSnippetNotFound) {
				t.Fatalf("double delete=%v", err)
			}
			if _, err = s.update("0123456789abcdef0123456789abcdef", snippetUpdate{Body: strPtr("x")}, 1); !errors.Is(err, errSnippetNotFound) {
				t.Fatalf("unknown update=%v", err)
			}

			reopened, err := newSnippetStoreWithClock(path, now)
			if err != nil {
				t.Fatal(err)
			}
			if got := snippetIDs(reopened.list()); len(got) != 2 || got[0] != clip.ID || got[1] != renamed.ID {
				t.Fatalf("reopen=%v", got)
			}
			info, err := os.Lstat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("mode=%v %v", info.Mode(), err)
			}
		})
	}
}

func strPtr(v string) *string    { return &v }
func boolPtr(v bool) *bool       { return &v }
func uint64Ptr(v uint64) *uint64 { return &v }

func TestSnippetStoreLimitsAndValidation(t *testing.T) {
	s := fastSnippetStore(t, shortTestDir(t))
	maxBody := strings.Repeat("x", snippetBodyMaxBytes-1) + "\n"
	if _, err := s.create(snippetKindSnippet, strings.Repeat("l", snippetLabelMaxRunes), maxBody, false, strings.Repeat("o", snippetOriginMaxRunes)); err != nil {
		t.Fatalf("maximal record refused: %v", err)
	}
	if _, err := s.create(snippetKindSnippet, "wide", "ünïcödé — 日本語 🚀\n\ttabbed", false, "日本"); err != nil {
		t.Fatalf("unicode body refused: %v", err)
	}
	for name, tc := range map[string]struct {
		kind, label, body string
		pinned            bool
		origin            string
		want              error
	}{
		"body_too_large":     {snippetKindSnippet, "l", strings.Repeat("x", snippetBodyMaxBytes+1), false, "", errSnippetTooLarge},
		"clip_too_large":     {snippetKindClip, "", strings.Repeat("x", snippetBodyMaxBytes+1), false, "", errSnippetTooLarge},
		"empty_body":         {snippetKindSnippet, "l", "", false, "", errSnippetValidation},
		"carriage_return":    {snippetKindSnippet, "l", "a\r\nb", false, "", errSnippetValidation},
		"escape_in_body":     {snippetKindSnippet, "l", "a\x1b[31mb", false, "", errSnippetValidation},
		"nul_in_body":        {snippetKindSnippet, "l", "a\x00b", false, "", errSnippetValidation},
		"del_in_body":        {snippetKindSnippet, "l", "a\x7fb", false, "", errSnippetValidation},
		"c1_in_body":         {snippetKindSnippet, "l", "a\u0085b", false, "", errSnippetValidation},
		"invalid_utf8_body":  {snippetKindSnippet, "l", "a\xffb", false, "", errSnippetValidation},
		"label_too_long":     {snippetKindSnippet, strings.Repeat("l", snippetLabelMaxRunes+1), "b", false, "", errSnippetValidation},
		"label_control":      {snippetKindSnippet, "a\tb", "b", false, "", errSnippetValidation},
		"label_empty":        {snippetKindSnippet, "   ", "b", false, "", errSnippetValidation},
		"origin_too_long":    {snippetKindSnippet, "l", "b", false, strings.Repeat("o", snippetOriginMaxRunes+1), errSnippetValidation},
		"origin_control":     {snippetKindClip, "", "b", false, "a\nb", errSnippetValidation},
		"snippet_osc_origin": {snippetKindSnippet, "l", "b", false, snippetOSCID, errSnippetValidation},
		"clip_osc_origin":    {snippetKindClip, "", "b", false, snippetOSCID, errSnippetValidation},
		"clip_osc_padded":    {snippetKindClip, "", "b", false, " osc52 ", errSnippetValidation},
		"clip_with_label":    {snippetKindClip, "l", "b", false, "", errSnippetValidation},
		"clip_pinned":        {snippetKindClip, "", "b", true, "", errSnippetValidation},
		"unknown_kind":       {"note", "l", "b", false, "", errSnippetValidation},
		"empty_kind":         {"", "l", "b", false, "", errSnippetValidation},
	} {
		if _, err := s.create(tc.kind, tc.label, tc.body, tc.pinned, tc.origin); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err=%v want %v", name, err, tc.want)
		}
	}
	if got := s.list(); len(got) != 2 {
		t.Fatalf("refusals changed the store: %d records", len(got))
	}
	for i := len(s.list()); i < snippetStoreMaxSnippets; i++ {
		if _, err := s.create(snippetKindSnippet, "n"+strconv.Itoa(i), "b"+strconv.Itoa(i), false, ""); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	if _, err := s.create(snippetKindSnippet, "overflow", "overflow body", false, ""); !errors.Is(err, errSnippetStoreFull) {
		t.Fatalf("257th snippet=%v", err)
	}
	if _, err := s.create(snippetKindClip, "", "clips have their own ring", false, ""); err != nil {
		t.Fatalf("clip refused by the snippet cap: %v", err)
	}

	// The 1 MiB file cap binds before the 256-record cap when bodies are
	// large: the refusal is "full", the mutation rolls back, the disk is the
	// prior file.
	bigDir := shortTestDir(t)
	big := fastSnippetStore(t, bigDir)
	bigBody := strings.Repeat("y", snippetBodyMaxBytes)
	var lastErr error
	count := 0
	for ; count < snippetStoreMaxSnippets; count++ {
		unique := strconv.Itoa(count) + bigBody[len(strconv.Itoa(count)):]
		if _, lastErr = big.create(snippetKindSnippet, "big"+strconv.Itoa(count), unique, false, ""); lastErr != nil {
			break
		}
	}
	if !errors.Is(lastErr, errSnippetStoreFull) || count == 0 || count >= snippetStoreMaxSnippets {
		t.Fatalf("file cap: count=%d err=%v", count, lastErr)
	}
	if len(big.list()) != count || big.file.fault != nil {
		t.Fatalf("file-cap refusal left state: %d records fault=%v", len(big.list()), big.file.fault)
	}
	reopened, err := newSnippetStore(filepath.Join(bigDir, "snippets.json"))
	if err != nil || len(reopened.list()) != count {
		t.Fatalf("file-cap refusal changed the disk: %v", err)
	}
}

// SF4 (server half) and SF5 (store half): clock-injected TTL, the ring of 20,
// and the single distinguished OSC 52 record.
func TestSnippetStoreClipRingTTLAndOSCRecord(t *testing.T) {
	dir := shortTestDir(t)
	s := fastSnippetStore(t, dir)
	clock := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	manual := []string{}
	for i := 0; i < snippetClipRing; i++ {
		clock = clock.Add(time.Second)
		r, err := s.create(snippetKindClip, "", "manual "+strconv.Itoa(i), false, "iphone")
		if err != nil {
			t.Fatal(err)
		}
		manual = append(manual, r.ID)
	}
	clock = clock.Add(time.Second)
	extra, err := s.create(snippetKindClip, "", "manual 20", false, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, v := range s.list() {
		ids[v.ID] = true
	}
	if len(ids) != snippetClipRing || ids[manual[0]] || !ids[manual[1]] || !ids[extra.ID] {
		t.Fatalf("ring after 21 manual clips: %d live, oldest present=%v", len(ids), ids[manual[0]])
	}
	// The OSC 52 record (E2): thirty upserts from two panes occupy one fixed
	// record, updated in place, never created through create().
	if _, err := s.create(snippetKindClip, "", "masquerade", false, snippetOSCID); !errors.Is(err, errSnippetValidation) {
		t.Fatalf("create with the reserved origin=%v", err)
	}
	var osc SnippetRecord
	for i := 0; i < 30; i++ {
		clock = clock.Add(time.Second)
		origin := "pane-a"
		if i%2 == 1 {
			origin = "pane-b"
		}
		r, err := s.upsertOSC("osc value "+strconv.Itoa(i), origin, nil)
		if err != nil {
			t.Fatalf("osc write %d: %v", i, err)
		}
		if i == 0 {
			if r.ID != snippetOSCID || r.Revision != 1 || r.Kind != snippetKindClip {
				t.Fatalf("first osc write=%+v", r)
			}
			osc = r
		} else if r.ID != osc.ID || r.Revision != uint64(i+1) || r.Body != "osc value "+strconv.Itoa(i) || r.Origin != origin || !r.ExpiresAt.Equal(clock.Add(snippetClipTTL)) || !r.CreatedAt.Equal(osc.CreatedAt) {
			t.Fatalf("osc write %d appended or did not refresh: %+v", i, r)
		}
	}
	// CAS on the upsert: a stale expectation returns the current record; the
	// current one lands; the reserved token is never an origin.
	if cur, err := s.upsertOSC("late", "pane-c", uint64Ptr(3)); !errors.Is(err, errSnippetConflict) || cur.Revision != 30 {
		t.Fatalf("stale osc upsert=%+v %v", cur, err)
	}
	if r, err := s.upsertOSC("cas", "pane-c", uint64Ptr(30)); err != nil || r.Revision != 31 {
		t.Fatalf("cas osc upsert=%+v %v", r, err)
	}
	if _, err := s.upsertOSC("x", snippetOSCID, nil); !errors.Is(err, errSnippetValidation) {
		t.Fatalf("osc upsert with reserved origin=%v", err)
	}
	if _, err := s.update(snippetOSCID, snippetUpdate{Body: strPtr("patched")}, 31); !errors.Is(err, errSnippetValidation) {
		t.Fatalf("patch of the server-owned osc record=%v", err)
	}
	live := s.list()
	oscCount, manualCount := 0, 0
	for _, v := range live {
		if isOSCSnippetID(v.ID) {
			oscCount++
		} else {
			manualCount++
		}
	}
	if oscCount != 0 || manualCount != snippetClipRing+1 || len(live) != snippetClipRing+1 {
		t.Fatalf("after the osc flood: osc=%d manual=%d total=%d", oscCount, manualCount, len(live))
	}
	if !snippetIDRE.MatchString(live[0].ID) || live[0].Preview != "cas" || s.records[snippetOSCID].Revision != 31 {
		t.Fatalf("newest canonical clip/publication mismatch: %+v", live[0])
	}
	// A snippet is untouched by the ring and the TTL.
	snip, err := s.create(snippetKindSnippet, "keep", "forever", true, "")
	if err != nil {
		t.Fatal(err)
	}
	// The operator can clear the OSC record by its fixed id; the next upsert
	// recreates it at revision 1 without touching the manual ring.
	if _, err := s.delete(snippetOSCID, 3); !errors.Is(err, errSnippetConflict) {
		t.Fatalf("stale osc delete=%v", err)
	}
	if _, err := s.delete(snippetOSCID, 31); err != nil {
		t.Fatal(err)
	}
	if _, err := s.delete(snippetOSCID, 1); !errors.Is(err, errSnippetNotFound) {
		t.Fatalf("deleted osc record=%v", err)
	}
	if cur, err := s.upsertOSC("after delete", "pane-a", uint64Ptr(31)); !errors.Is(err, errSnippetConflict) || cur.ID != "" {
		t.Fatalf("stale upsert after delete=%+v %v", cur, err)
	}
	if r, err := s.upsertOSC("after delete", "pane-a", uint64Ptr(0)); err != nil || r.Revision != 1 || r.ID != snippetOSCID {
		t.Fatalf("recreate after delete=%+v %v", r, err)
	}
	manualNow := 0
	for _, v := range s.list() {
		if v.Kind == snippetKindClip && !isOSCSnippetID(v.ID) {
			manualNow++
		}
	}
	if manualNow != snippetClipRing+1 {
		t.Fatalf("delete/recreate of the osc record touched the manual ring: %d", manualNow)
	}
	// TTL: just before expiry the oldest manual clip is still listed; at
	// expiry it is gone from the list, from the next persist, and from a
	// reload at that clock.
	var oldest SnippetRecord
	for _, view := range s.list() {
		if view.Kind == snippetKindClip && (oldest.ID == "" || view.ExpiresAt.Before(*oldest.ExpiresAt)) {
			oldest = view.SnippetRecord
		}
	}
	manual[2] = oldest.ID
	clock = oldest.ExpiresAt.Add(-time.Millisecond)
	if !contains(snippetIDs(s.list()), manual[2]) {
		t.Fatal("clip expired early")
	}
	clock = *oldest.ExpiresAt
	if contains(snippetIDs(s.list()), manual[2]) {
		t.Fatal("expired clip still listed")
	}
	if _, err = s.update(manual[2], snippetUpdate{Body: strPtr("x")}, 1); !errors.Is(err, errSnippetNotFound) {
		t.Fatalf("expired clip update=%v", err)
	}
	if _, err = s.delete(manual[2], 1); !errors.Is(err, errSnippetNotFound) {
		t.Fatalf("expired clip delete=%v", err)
	}
	if _, err = s.create(snippetKindSnippet, "persist", "x", false, ""); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "snippets.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk snippetStoreFile
	if err = json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	for _, r := range onDisk.Items {
		if r.ID == manual[2] {
			t.Fatal("persist kept an expired clip")
		}
	}
	clock = clock.Add(2 * snippetClipTTL)
	reopened, err := newSnippetStore(filepath.Join(dir, "snippets.json"))
	if err != nil {
		t.Fatal(err)
	}
	reopened.now = func() time.Time { return clock }
	for _, v := range reopened.list() {
		if v.Kind == snippetKindClip {
			t.Fatalf("reload kept expired clip %+v", v)
		}
	}
	if got := snippetIDs(reopened.list()); len(got) != 2 || got[1] != snip.ID {
		t.Fatalf("snippets after clip expiry=%v", got)
	}
	// Load-time pruning is what the clock-injected reopen above measured;
	// the pruned set is what the next persist writes.
	if _, err = reopened.create(snippetKindClip, "", "fresh", false, "iphone"); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "snippets.json"))
	if err = json.Unmarshal(raw, &onDisk); err != nil || len(onDisk.Items) != 3 {
		t.Fatalf("post-expiry persist=%d items err=%v", len(onDisk.Items), err)
	}
	long := strings.Repeat("é", snippetPreviewRunes+5)
	if got := snippetPreview(long); got != strings.Repeat("é", snippetPreviewRunes) {
		t.Fatalf("preview truncation by runes broken: %d", len([]rune(got)))
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// SF1: an injected crash before and after the file sync, before and after
// the rename, and before and after the directory sync leaves either the prior
// file or the complete new file, never a partial one.
func TestSnippetStorePersistenceFailureAlgebra(t *testing.T) {
	for _, phase := range []string{"temp-open", "write", "short-write", "file-sync", "close", "rename", "directory-open", "directory-sync"} {
		t.Run(phase, func(t *testing.T) {
			dir := shortTestDir(t)
			path := filepath.Join(dir, "snippets.json")
			s, err := newSnippetStore(path)
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.create(snippetKindSnippet, "before", "one", false, "")
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
			if _, err = s.update(before.ID, snippetUpdate{Label: strPtr("after")}, before.Revision); err == nil {
				t.Fatal("injected failure reported success")
			}
			got := s.list()
			published := phase == "directory-open" || phase == "directory-sync"
			if published {
				if len(got) != 1 || got[0].Label != "after" || got[0].Revision != 2 || s.file.fault == nil {
					t.Fatalf("post-publication state=%+v fault=%v", got, s.file.fault)
				}
				if _, e := s.create(snippetKindSnippet, "blocked", "x", false, ""); !errors.Is(e, errSnippetStoreUnavailable) {
					t.Fatalf("faulted store create=%v", e)
				}
				if _, e := s.delete(before.ID, 2); !errors.Is(e, errSnippetStoreUnavailable) {
					t.Fatalf("faulted store delete=%v", e)
				}
				reopened, e := newSnippetStore(path)
				if e != nil || reopened.list()[0].Label != "after" {
					t.Fatalf("published disk mismatch: %v", e)
				}
			} else {
				if len(got) != 1 || got[0].Label != "before" || got[0].Revision != 1 || s.file.fault != nil {
					t.Fatalf("pre-publication rollback=%+v fault=%v", got, s.file.fault)
				}
				reopened, e := newSnippetStore(path)
				if e != nil || reopened.list()[0].Label != "before" {
					t.Fatalf("disk changed pre-publication: %v", e)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != "snippets.json" {
					t.Fatalf("residue after %s: %s", phase, entry.Name())
				}
			}
		})
	}
}

func snippetFixtureRecord(id, kind string, expires *time.Time) SnippetRecord {
	r := SnippetRecord{ID: id, Kind: kind, Body: "body", Revision: 1, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(), ExpiresAt: expires}
	if kind == snippetKindSnippet {
		r.Label = "label"
	}
	return r
}

func TestSnippetStoreStrictFilesystemAndData(t *testing.T) {
	dir := shortTestDir(t)
	far := time.Now().Add(365 * 24 * time.Hour).UTC()
	valid := snippetStoreFile{Version: 1, Items: []SnippetRecord{snippetFixtureRecord("0123456789abcdef0123456789abcdef", snippetKindSnippet, nil)}}
	validBytes, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, validBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := newSnippetStore(target); err != nil || len(s.list()) != 1 {
		t.Fatalf("valid file refused: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newSnippetStore(link); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := newSnippetStore(target); err == nil {
		t.Fatal("unsafe mode accepted")
	}
	marshal := func(items ...SnippetRecord) []byte {
		b, _ := json.Marshal(snippetStoreFile{Version: 1, Items: items})
		return b
	}
	snippetWithExpiry := snippetFixtureRecord("1123456789abcdef0123456789abcdef", snippetKindSnippet, &far)
	clipWithLabel := snippetFixtureRecord("2123456789abcdef0123456789abcdef", snippetKindClip, &far)
	clipWithLabel.Label = "x"
	clipWithoutExpiry := snippetFixtureRecord("3123456789abcdef0123456789abcdef", snippetKindClip, nil)
	badID := snippetFixtureRecord("not-hex", snippetKindSnippet, nil)
	crBody := snippetFixtureRecord("4123456789abcdef0123456789abcdef", snippetKindSnippet, nil)
	crBody.Body = "a\r\n"
	oscA := snippetFixtureRecord(snippetOSCID, snippetKindClip, &far)
	oscA.Origin = "pane-a"
	oscB := snippetFixtureRecord("6123456789abcdef0123456789abcdef", snippetKindClip, &far)
	oscB.Origin = snippetOSCID
	oscSnippet := snippetFixtureRecord("7123456789abcdef0123456789abcdef", snippetKindSnippet, nil)
	oscSnippet.Origin = snippetOSCID
	oscIDSnippet := snippetFixtureRecord(snippetOSCID, snippetKindSnippet, nil)
	oscLabelled := snippetFixtureRecord(snippetOSCID, snippetKindClip, &far)
	oscLabelled.Label = "x"
	tooManyClips := make([]SnippetRecord, 0, snippetClipRing+1)
	for i := 0; i <= snippetClipRing; i++ {
		id := strconv.FormatInt(int64(i), 16)
		tooManyClips = append(tooManyClips, snippetFixtureRecord(strings.Repeat("0", 32-len(id))+id, snippetKindClip, &far))
	}
	tooManySnippets := make([]SnippetRecord, 0, snippetStoreMaxSnippets+1)
	for i := 0; i <= snippetStoreMaxSnippets; i++ {
		id := strconv.FormatInt(int64(i), 16)
		tooManySnippets = append(tooManySnippets, snippetFixtureRecord(strings.Repeat("a", 32-len(id))+id, snippetKindSnippet, nil))
	}
	for name, body := range map[string][]byte{
		"unknown-version":      []byte(`{"version":3,"items":[]}`),
		"unknown-field":        []byte(`{"version":1,"items":[],"extra":1}`),
		"duplicate-key":        []byte(`{"version":1,"version":1,"items":[]}`),
		"folded-key":           []byte(`{"version":1,"items":[],"ITEMS":[]}`),
		"corrupt":              []byte(`{`),
		"trailing":             append(append([]byte{}, validBytes...), []byte(`{}`)...),
		"array":                []byte(`[]`),
		"invalid-utf8":         append(append([]byte{}, validBytes[:len(validBytes)-2]...), 0xff, '}', ']', '}'),
		"duplicate-id":         marshal(valid.Items[0], valid.Items[0]),
		"snippet-with-expiry":  marshal(snippetWithExpiry),
		"clip-with-label":      marshal(clipWithLabel),
		"clip-without-expiry":  marshal(clipWithoutExpiry),
		"bad-id":               marshal(badID),
		"carriage-return":      marshal(crBody),
		"clip-with-osc-origin": marshal(oscB),
		"osc-snippet":          marshal(oscSnippet),
		"osc-id-snippet":       marshal(oscIDSnippet),
		"osc-id-labelled":      marshal(oscLabelled),
		"osc-with-manual-dup":  marshal(oscA, oscA),
		"too-many-clips":       marshal(tooManyClips...),
		"too-many-snippets":    marshal(tooManySnippets...),
		"oversize":             make([]byte, snippetStoreMaxBytes+1),
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, body, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newSnippetStore(p); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	fullRing := append([]SnippetRecord{oscA}, tooManyClips[:snippetClipRing]...)
	full := filepath.Join(dir, "full-ring-plus-osc")
	if err := os.WriteFile(full, marshal(fullRing...), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := newSnippetStoreWithClock(full, func() time.Time { return oscA.CreatedAt }); err != nil || len(s.list()) != 1 {
		t.Fatalf("identical legacy clips and publication did not deduplicate: %v", err)
	}
	oneOSC := filepath.Join(dir, "one-osc")
	if err := os.WriteFile(oneOSC, marshal(oscA, valid.Items[0]), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := newSnippetStoreWithClock(oneOSC, func() time.Time { return oscA.CreatedAt }); err != nil || len(s.list()) != 1 {
		t.Fatalf("single osc record refused: %v", err)
	}
	unsafe := filepath.Join(dir, "unsafe")
	if err := os.Mkdir(unsafe, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := newSnippetStore(filepath.Join(unsafe, "snippets.json")); err == nil {
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
	if _, err := newSnippetStore(filepath.Join(linked, "snippets.json")); err == nil {
		t.Fatal("symlink parent accepted")
	}
	if _, err := newSnippetStore(realParent + "/../real/snippets.json"); err == nil {
		t.Fatal("unclean path accepted")
	}
}
