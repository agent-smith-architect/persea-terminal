package frontdoor

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"persea-terminal/internal/proto"
)

type faultAliasFS struct {
	aliasFS
	tempOpen, rename, dirOpen bool
}

func (f faultAliasFS) Open(name string) (*os.File, error) {
	if f.dirOpen && name == "." {
		return nil, errors.New("injected directory open")
	}
	return f.aliasFS.Open(name)
}
func (f faultAliasFS) OpenFile(name string, flag int, mode os.FileMode) (*os.File, error) {
	if f.tempOpen && flag&os.O_CREATE != 0 {
		return nil, errors.New("injected temp open")
	}
	return f.aliasFS.OpenFile(name, flag, mode)
}
func (f faultAliasFS) Rename(oldname, newname string) error {
	if f.rename {
		return errors.New("injected rename")
	}
	return f.aliasFS.Rename(oldname, newname)
}

func TestAliasStoreDurableLifecycleAndCAS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aliases.json")
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := newAliasStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first := auth(1, "$0")
	r, err := s.create(" Work ", first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.create("work", auth(2, "$1")); !errors.Is(err, errAliasConflict) {
		t.Fatalf("normalization duplicate = %v", err)
	}
	r, err = s.update(r.AliasID, "Renamed", nil, r.Revision)
	if err != nil || !sameAuthority(r.Incarnation, first) {
		t.Fatalf("rename changed incarnation: %+v %v", r, err)
	}
	reopened, err := newAliasStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.list(); len(got) != 1 || got[0].DisplayAlias != "Renamed" || !sameAuthority(got[0].Incarnation, first) {
		t.Fatalf("reopen lost binding: %+v", got)
	}
	if err = reopened.reconcile(nil, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if reopened.list()[0].State == "tombstone" {
		t.Fatal("unknown inventory tombstoned alias")
	}
	if err = reopened.reconcile(nil, map[string]bool{"r\x00s": true}); err != nil {
		t.Fatal(err)
	}
	tomb := reopened.list()[0]
	if tomb.State != "tombstone" {
		t.Fatalf("complete disappearance state=%s", tomb.State)
	}
	if err = reopened.reconcile([]proto.Authority{first}, map[string]bool{"r\x00s": true}); err != nil {
		t.Fatal(err)
	}
	if reopened.list()[0].State != "tombstone" {
		t.Fatal("tombstone resurrected without reassignment")
	}
	replacement := auth(2, "$0")
	rebound, err := reopened.update(tomb.AliasID, "Renamed", &replacement, tomb.Revision)
	if err != nil || rebound.State != "rebound" {
		t.Fatalf("reassign=%+v %v", rebound, err)
	}
	if _, err = reopened.update(rebound.AliasID, "loser", nil, tomb.Revision); !errors.Is(err, errAliasConflict) {
		t.Fatalf("stale writer=%v", err)
	}
}

func TestAliasStoreSimpleFoldAndDuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "aliases.json")
	s, err := newAliasStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.create("s", auth(1, "$0")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.create("ſ", auth(2, "$1")); !errors.Is(err, errAliasConflict) {
		t.Fatalf("SimpleFold-equivalent alias accepted: %v", err)
	}
	r := s.list()[0]
	body, err := json.Marshal(aliasStoreFile{Version: aliasStoreVersion, Aliases: []AliasRecord{r, r}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = newAliasStore(path); err == nil {
		t.Fatal("duplicate alias IDs accepted")
	}
	if err = os.WriteFile(path, []byte(`{"version":1,"aliases":[],"aliaſes":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = newAliasStore(path); err == nil {
		t.Fatal("SimpleFold-equivalent JSON keys accepted")
	}
}

func TestAliasStorePersistenceFailureAlgebra(t *testing.T) {
	for _, phase := range []string{"temp-open", "write", "short-write", "file-sync", "rename", "directory-open"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "aliases.json")
			s, err := newAliasStore(path)
			if err != nil {
				t.Fatal(err)
			}
			r, err := s.create("before", auth(1, "$0"))
			if err != nil {
				t.Fatal(err)
			}
			baseFS := s.fs
			switch phase {
			case "temp-open":
				s.fs = faultAliasFS{aliasFS: baseFS, tempOpen: true}
			case "write":
				s.writeFile = func(*os.File, []byte) (int, error) { return 0, errors.New("injected write") }
			case "short-write":
				s.writeFile = func(f *os.File, b []byte) (int, error) { return f.Write(b[:len(b)/2]) }
			case "file-sync":
				s.fileSync = func(*os.File) error { return errors.New("injected file sync") }
			case "rename":
				s.fs = faultAliasFS{aliasFS: baseFS, rename: true}
			case "directory-open":
				s.fs = faultAliasFS{aliasFS: baseFS, dirOpen: true}
			}
			_, err = s.update(r.AliasID, "after", nil, r.Revision)
			if err == nil {
				t.Fatal("injected failure reported success")
			}
			got := s.list()[0]
			if phase == "directory-open" {
				if got.DisplayAlias != "after" || s.fault == nil {
					t.Fatalf("post-publication state=%+v fault=%v", got, s.fault)
				}
				if _, e := s.create("blocked", auth(2, "$1")); !errors.Is(e, errAliasStoreUnavailable) {
					t.Fatalf("faulted store mutation=%v", e)
				}
				reopened, e := newAliasStore(path)
				if e != nil || reopened.list()[0].DisplayAlias != "after" {
					t.Fatalf("published disk mismatch: %v %+v", e, reopened)
				}
			} else {
				if got.DisplayAlias != "before" || s.fault != nil {
					t.Fatalf("pre-publication rollback=%+v fault=%v", got, s.fault)
				}
				reopened, e := newAliasStore(path)
				if e != nil || reopened.list()[0].DisplayAlias != "before" {
					t.Fatalf("disk changed pre-publication: %v", e)
				}
			}
		})
	}
}

func TestAliasStoreDirectorySyncFaultAndReconcileRollback(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "aliases.json")
	s, err := newAliasStore(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.create("live", auth(1, "$0"))
	if err != nil {
		t.Fatal(err)
	}
	s.dirSync = func(*os.File) error { return errors.New("injected directory sync") }
	if _, err = s.update(r.AliasID, "published", nil, r.Revision); err == nil || s.fault == nil || s.list()[0].DisplayAlias != "published" {
		t.Fatalf("directory sync algebra: %v %+v", err, s.list())
	}

	dir2 := t.TempDir()
	if err := os.Chmod(dir2, 0700); err != nil {
		t.Fatal(err)
	}
	s2, err := newAliasStore(filepath.Join(dir2, "aliases.json"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s2.create("live", auth(1, "$0"))
	if err != nil {
		t.Fatal(err)
	}
	s2.fs = faultAliasFS{aliasFS: s2.fs, rename: true}
	if err = s2.reconcile(nil, map[string]bool{"r\x00s": true}); err == nil {
		t.Fatal("reconcile failure reported success")
	}
	got := s2.list()[0]
	if got.State != r2.State || got.Revision != r2.Revision {
		t.Fatalf("reconcile failed to roll back all fields: %+v", got)
	}
}

func TestAliasStoreStrictFilesystemAndData(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte(`{"version":1,"aliases":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newAliasStore(link); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := newAliasStore(target); err == nil {
		t.Fatal("unsafe mode accepted")
	}
	for name, body := range map[string]string{"unknown-version": `{"version":2,"aliases":[]}`, "unknown-field": `{"version":1,"aliases":[],"extra":1}`, "duplicate": `{"version":1,"version":1,"aliases":[]}`, "corrupt": `{`} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newAliasStore(p); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, make([]byte, aliasStoreMaxBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newAliasStore(large); err == nil {
		t.Fatal("oversize accepted")
	}

	utf8Dir := t.TempDir()
	if err := os.Chmod(utf8Dir, 0700); err != nil {
		t.Fatal(err)
	}
	utf8Path := filepath.Join(utf8Dir, "aliases.json")
	store, err := newAliasStore(utf8Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.create("valid", auth(1, "$0")); err != nil {
		t.Fatal(err)
	}
	valid, err := os.ReadFile(utf8Path)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"invalid-utf8-value": bytes.Replace(valid, []byte(`"display_alias":"valid"`), []byte{'"', 'd', 'i', 's', 'p', 'l', 'a', 'y', '_', 'a', 'l', 'i', 'a', 's', '"', ':', '"', 0xff, '"'}, 1),
		"invalid-utf8-key":   bytes.Replace(valid, []byte(`"display_alias"`), []byte{'"', 'd', 'i', 's', 'p', 'l', 'a', 'y', '_', 0xff, '"'}, 1),
	} {
		p := filepath.Join(utf8Dir, name)
		if err := os.WriteFile(p, body, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newAliasStore(p); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestAliasStoreRejectsUnsafeParent(t *testing.T) {
	base := t.TempDir()
	unsafe := filepath.Join(base, "unsafe")
	if err := os.Mkdir(unsafe, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := newAliasStore(filepath.Join(unsafe, "aliases.json")); err == nil {
		t.Fatal("unsafe parent mode accepted")
	}
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(base, "linked")
	if err := os.Symlink(realParent, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := newAliasStore(filepath.Join(linked, "aliases.json")); err == nil {
		t.Fatal("symlink parent accepted")
	}
	if os.Geteuid() == 0 {
		wrong := filepath.Join(base, "wrong-owner")
		if err := os.Mkdir(wrong, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(wrong, 1, -1); err != nil {
			t.Fatal(err)
		}
		defer os.Chown(wrong, 0, -1)
		if _, err := newAliasStore(filepath.Join(wrong, "aliases.json")); err == nil {
			t.Fatal("wrong-owner parent accepted")
		}
	}
}
