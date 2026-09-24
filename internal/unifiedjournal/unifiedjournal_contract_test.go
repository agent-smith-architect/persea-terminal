package unifiedjournal

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

const (
	journalRed     = "ISSUE25/PHASE6A1/CANDIDATE_RED/EPHEMERAL_JOURNAL"
	sequencerRed   = "ISSUE25/PHASE6A1/CANDIDATE_RED/WRITE_AHEAD_SEQUENCER"
	eligibilityRed = "ISSUE25/PHASE6A1/CANDIDATE_RED/CONTINUOUS_ONLY_ELIGIBILITY"
)

func journalOptions(t *testing.T) OpenOptions {
	t.Helper()
	runtimeDir := filepath.Join(t.TempDir(), "realm-runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatalf("%s create runtime: %v", journalRed, err)
	}
	return OpenOptions{
		RuntimeDir:        runtimeDir,
		Realm:             "realm-a",
		BrokerIncarnation: "broker-incarnation-a",
		UID:               os.Geteuid(), GID: os.Getegid(),
		DirectoryMode: 0o700, FileMode: 0o600,
		PaneCapBytes:  DefaultPaneCapBytes,
		RealmCapBytes: DefaultRealmCapBytes,
		// The logical-cap contracts below shrink PaneCapBytes to a handful of
		// bytes; the physical pane cap is pinned independently so the ledger
		// under test is the one the test names. The physical ledger has its own
		// contracts in physical_ledger_test.go.
		PanePhysicalCapBytes: DefaultPaneCapBytes * DefaultPanePhysicalMultiplier,
	}
}

func journalKey(pane, incarnation string) PaneKey {
	return PaneKey{
		Server: "server-a", Session: "$1", ControlGeneration: 7,
		Window: "@1", Pane: pane, Incarnation: incarnation,
	}
}

func TestDefaultCapsAreExplicitAndBounded(t *testing.T) {
	if DefaultPaneCapBytes != 8<<20 {
		t.Fatalf("%s pane cap=%d want=%d", journalRed, DefaultPaneCapBytes, 8<<20)
	}
	if DefaultRealmCapBytes != 64<<20 {
		t.Fatalf("%s realm cap=%d want=%d", journalRed, DefaultRealmCapBytes, 64<<20)
	}
}

func TestRealmOpenRequiresOwnedNonSymlinkRuntimeAndExactModes(t *testing.T) {
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s safe runtime rejected: %v", journalRed, err)
	}
	key := journalKey("%1", "pane-inc-a")
	if _, err := realm.Append(key, []byte("first")); err != nil {
		t.Fatalf("%s append: %v", journalRed, err)
	}
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", journalRed, err)
	}

	err = filepath.WalkDir(options.RuntimeDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s journal created symlink %s", journalRed, path)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("%s no unix stat for %s", journalRed, path)
		}
		if int(stat.Uid) != options.UID || int(stat.Gid) != options.GID {
			t.Fatalf("%s owner %s=%d:%d want=%d:%d", journalRed, path, stat.Uid, stat.Gid, options.UID, options.GID)
		}
		wantMode := options.FileMode
		if info.IsDir() {
			wantMode = options.DirectoryMode
		}
		if info.Mode().Perm() != wantMode {
			t.Fatalf("%s mode %s=%#o want=%#o", journalRed, path, info.Mode().Perm(), wantMode)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s inspect runtime: %v", journalRed, err)
	}

	outside := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "runtime-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("%s create symlink: %v", journalRed, err)
	}
	unsafe := options
	unsafe.RuntimeDir = link
	if _, err := OpenRealm(unsafe); !errors.Is(err, ErrUnsafeRuntime) {
		t.Fatalf("%s symlink runtime err=%v want=%v", journalRed, err, ErrUnsafeRuntime)
	}

	wrongMode := journalOptions(t)
	if err := os.Chmod(wrongMode.RuntimeDir, 0o755); err != nil {
		t.Fatalf("%s chmod: %v", journalRed, err)
	}
	if _, err := OpenRealm(wrongMode); !errors.Is(err, ErrUnsafeRuntime) {
		t.Fatalf("%s permissive runtime err=%v want=%v", journalRed, err, ErrUnsafeRuntime)
	}
	info, err := os.Stat(wrongMode.RuntimeDir)
	if err != nil {
		t.Fatalf("%s stat rejected runtime: %v", journalRed, err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("%s rejected runtime was silently repaired mode=%v", journalRed, info.Mode().Perm())
	}

	for _, item := range []struct {
		name   string
		mutate func(*OpenOptions)
	}{
		{"wrong expected uid", func(options *OpenOptions) { options.UID++ }},
		{"wrong expected gid", func(options *OpenOptions) { options.GID++ }},
	} {
		t.Run(item.name, func(t *testing.T) {
			mismatch := journalOptions(t)
			before, err := os.Stat(mismatch.RuntimeDir)
			if err != nil {
				t.Fatalf("%s pre-stat: %v", journalRed, err)
			}
			beforeStat := before.Sys().(*syscall.Stat_t)
			item.mutate(&mismatch)
			if _, err := OpenRealm(mismatch); !errors.Is(err, ErrUnsafeRuntime) {
				t.Fatalf("%s ownership mismatch err=%v want=%v", journalRed, err, ErrUnsafeRuntime)
			}
			after, err := os.Stat(mismatch.RuntimeDir)
			if err != nil {
				t.Fatalf("%s post-stat: %v", journalRed, err)
			}
			afterStat := after.Sys().(*syscall.Stat_t)
			if beforeStat.Uid != afterStat.Uid || beforeStat.Gid != afterStat.Gid || before.Mode().Perm() != after.Mode().Perm() {
				t.Fatalf("%s rejected ownership was repaired before=%d:%d/%#o after=%d:%d/%#o", journalRed, beforeStat.Uid, beforeStat.Gid, before.Mode().Perm(), afterStat.Uid, afterStat.Gid, after.Mode().Perm())
			}
		})
	}
}

func TestOpenRealmRemainsAnchoredAcrossRuntimePathRedirect(t *testing.T) {
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", journalRed, err)
	}
	defer realm.Close()
	moved := options.RuntimeDir + "-moved"
	outside := t.TempDir()
	if err := os.Rename(options.RuntimeDir, moved); err != nil {
		t.Fatalf("%s rename runtime: %v", journalRed, err)
	}
	if err := os.Symlink(outside, options.RuntimeDir); err != nil {
		t.Fatalf("%s redirect runtime path: %v", journalRed, err)
	}
	_, appendErr := realm.Append(journalKey("%1", "pane-inc-a"), []byte("must-not-escape"))
	if appendErr != nil && !errors.Is(appendErr, ErrUnsafeRuntime) && !errors.Is(appendErr, ErrInvalidated) {
		t.Fatalf("%s redirected append unexpected err=%v", journalRed, appendErr)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("%s read outside: %v", journalRed, err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s redirected append escaped runtime: %v", journalRed, entries)
	}
}

type observedStorage struct {
	Directories []string
	Files       []string
	Journal     string
}

func normalizeStoragePath(t *testing.T, runtimeDir, path string) string {
	t.Helper()
	relative, err := filepath.Rel(filepath.Clean(runtimeDir), filepath.Clean(path))
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		t.Fatalf("%s storage path is not a descendant root=%s path=%s relative=%s err=%v", journalRed, runtimeDir, path, relative, err)
	}
	return filepath.Clean(relative)
}

func normalizeStorageSet(t *testing.T, runtimeDir string, paths []string) []string {
	t.Helper()
	normalized := make([]string, len(paths))
	for index, path := range paths {
		normalized[index] = normalizeStoragePath(t, runtimeDir, path)
	}
	sort.Strings(normalized)
	for index := 1; index < len(normalized); index++ {
		if normalized[index] == normalized[index-1] {
			t.Fatalf("%s storage path seam contains duplicate %s", journalRed, normalized[index])
		}
	}
	return normalized
}

func requireObservedStorageMatchesHelper(t *testing.T, options OpenOptions, key PaneKey) observedStorage {
	t.Helper()
	var observedDirectories, observedFiles []string
	err := filepath.WalkDir(options.RuntimeDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, relErr := filepath.Rel(options.RuntimeDir, path)
		if relErr != nil {
			return relErr
		}
		if relative == "." {
			return nil
		}
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		if info.IsDir() {
			observedDirectories = append(observedDirectories, filepath.Clean(relative))
			return nil
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("%s successful control I/O created non-regular storage %s mode=%v", journalRed, path, info.Mode())
		}
		observedFiles = append(observedFiles, filepath.Clean(relative))
		return nil
	})
	if err != nil {
		t.Fatalf("%s observe storage tree: %v", journalRed, err)
	}
	sort.Strings(observedDirectories)
	sort.Strings(observedFiles)
	paths := storagePathsForTest(options, key)
	reportedDirectories := normalizeStorageSet(t, options.RuntimeDir, paths.Directories)
	reportedFiles := normalizeStorageSet(t, options.RuntimeDir, paths.Files)
	journal := normalizeStoragePath(t, options.RuntimeDir, paths.Journal)
	if !equalStrings(observedDirectories, reportedDirectories) || !equalStrings(observedFiles, reportedFiles) {
		t.Fatalf("%s storage path seam differs from successful I/O observed dirs=%v files=%v reported dirs=%v files=%v", journalRed, observedDirectories, observedFiles, reportedDirectories, reportedFiles)
	}
	foundJournal := false
	for _, path := range observedFiles {
		foundJournal = foundJournal || path == journal
	}
	if !foundJournal {
		t.Fatalf("%s reported journal %s is not an observed regular file %v", journalRed, journal, observedFiles)
	}
	return observedStorage{Directories: observedDirectories, Files: observedFiles, Journal: journal}
}

func observeSafeStorage(t *testing.T, key PaneKey) observedStorage {
	t.Helper()
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open safe control: %v", journalRed, err)
	}
	payload := []byte("observed-storage-control-c92f74")
	record, err := realm.Append(key, payload)
	if err != nil {
		t.Fatalf("%s append safe control: %v", journalRed, err)
	}
	if err := realm.Sync(key); err != nil {
		t.Fatalf("%s sync safe control: %v", journalRed, err)
	}
	if err := realm.AdvanceCommitted(key, record); err != nil {
		t.Fatalf("%s commit safe control: %v", journalRed, err)
	}
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close safe control: %v", journalRed, err)
	}
	return requireObservedStorageMatchesHelper(t, options, key)
}

func TestPreexistingJournalDescendantOrLeafSymlinkFailsClosed(t *testing.T) {
	key := journalKey("%1", "pane-inc-a")
	observed := observeSafeStorage(t, key)

	t.Run("descendant directory", func(t *testing.T) {
		if len(observed.Directories) == 0 || len(observed.Files) == 0 {
			t.Fatalf("%s successful I/O produced incomplete tree: %+v", journalRed, observed)
		}
		options := journalOptions(t)
		directory := filepath.Join(options.RuntimeDir, observed.Directories[0])
		if err := os.MkdirAll(filepath.Dir(directory), 0o700); err != nil {
			t.Fatalf("%s create descendant parent: %v", journalRed, err)
		}
		outside := t.TempDir()
		if err := os.Symlink(outside, directory); err != nil {
			t.Fatalf("%s create descendant symlink: %v", journalRed, err)
		}
		realm, attackErr := OpenRealm(options)
		if attackErr == nil {
			defer realm.Close()
			_, attackErr = realm.Append(key, []byte("must-not-follow-descendant"))
		}
		if attackErr == nil {
			t.Fatalf("%s preexisting descendant symlink accepted", journalRed)
		}
		entries, err := os.ReadDir(outside)
		if err != nil || len(entries) != 0 {
			t.Fatalf("%s descendant symlink escaped entries=%v err=%v", journalRed, entries, err)
		}
	})

	t.Run("leaf files", func(t *testing.T) {
		options := journalOptions(t)
		outside := t.TempDir()
		for index, relative := range observed.Files {
			path := filepath.Join(options.RuntimeDir, relative)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("%s create storage parent: %v", journalRed, err)
			}
			target := filepath.Join(outside, fmt.Sprintf("target-%d", index))
			if err := os.WriteFile(target, []byte("UNCHANGED"), 0o600); err != nil {
				t.Fatalf("%s create outside target: %v", journalRed, err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatalf("%s create leaf symlink: %v", journalRed, err)
			}
		}
		realm, attackErr := OpenRealm(options)
		if attackErr == nil {
			defer realm.Close()
			_, attackErr = realm.Append(key, []byte("must-not-follow-leaf"))
		}
		if attackErr == nil {
			t.Fatalf("%s preexisting leaf symlink accepted", journalRed)
		}
		for index := range observed.Files {
			target := filepath.Join(outside, fmt.Sprintf("target-%d", index))
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, []byte("UNCHANGED")) {
				t.Fatalf("%s symlink target %d changed bytes=%q err=%v", journalRed, index, got, err)
			}
		}
	})
}

func TestJournalOffsetsHashesIncarnationAndCommitBoundary(t *testing.T) {
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", journalRed, err)
	}
	defer realm.Close()
	key := journalKey("%1", "pane-inc-a")
	admitJournalPane(t, realm, key)
	firstData := []byte("abc")
	first, err := realm.Append(key, firstData)
	if err != nil {
		t.Fatalf("%s first append: %v", journalRed, err)
	}
	if first.Key != key || first.Start != 0 || first.End != 3 || first.Hash != sha256.Sum256(firstData) {
		t.Fatalf("%s first record=%+v", journalRed, first)
	}
	secondData := []byte{0, 0xff, '\n', '\\'}
	second, err := realm.Append(key, secondData)
	if err != nil {
		t.Fatalf("%s second append: %v", journalRed, err)
	}
	if second.Key != key || second.Start != first.End || second.End != first.End+int64(len(secondData)) || second.Hash != sha256.Sum256(secondData) {
		t.Fatalf("%s second record=%+v", journalRed, second)
	}
	if got := realm.CommittedOffset(key); got != 0 {
		t.Fatalf("%s append advanced committed offset=%d", journalRed, got)
	}
	if err := realm.Sync(key); err != nil {
		t.Fatalf("%s sync: %v", journalRed, err)
	}
	if got := realm.CommittedOffset(key); got != 0 {
		t.Fatalf("%s sync alone advanced committed offset=%d", journalRed, got)
	}
	if err := realm.AdvanceCommitted(key, second); err != nil {
		t.Fatalf("%s advance committed: %v", journalRed, err)
	}
	if got := realm.CommittedOffset(key); got != second.End {
		t.Fatalf("%s committed offset=%d want=%d", journalRed, got, second.End)
	}
	committed, err := realm.ReadCommitted(key)
	if err != nil {
		t.Fatalf("%s read committed: %v", journalRed, err)
	}
	want := append(append([]byte(nil), firstData...), secondData...)
	if !bytes.Equal(committed, want) {
		t.Fatalf("%s committed bytes=%x want=%x", journalRed, committed, want)
	}
}

func TestPaneQuotaFailsClosedWithoutTruncateOrContinue(t *testing.T) {
	options := journalOptions(t)
	options.PaneCapBytes = 8
	options.RealmCapBytes = 32
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", journalRed, err)
	}
	defer realm.Close()
	key := journalKey("%1", "pane-inc-a")
	record, err := realm.Append(key, []byte("12345678"))
	if err != nil || record.End != 8 {
		t.Fatalf("%s exact-cap append record=%+v err=%v", journalRed, record, err)
	}
	if _, err := realm.Append(key, []byte("9")); !errors.Is(err, ErrQuota) {
		t.Fatalf("%s over-cap append err=%v want=%v", journalRed, err, ErrQuota)
	}
	if realm.Eligibility(key) != EligibilityUntrusted {
		t.Fatalf("%s quota did not invalidate pane state=%v", journalRed, realm.Eligibility(key))
	}
	if _, err := realm.Append(key, []byte("later")); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("%s invalidated pane continued err=%v", journalRed, err)
	}
	if got := realm.EndOffset(key); got != 8 {
		t.Fatalf("%s quota path truncated or extended end=%d", journalRed, got)
	}
}

func TestRealmQuotaCountsAllPanesAndInvalidatesBeforeAppend(t *testing.T) {
	options := journalOptions(t)
	options.PaneCapBytes = 16
	options.RealmCapBytes = 12
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", journalRed, err)
	}
	defer realm.Close()
	a := journalKey("%1", "pane-inc-a")
	b := journalKey("%2", "pane-inc-b")
	if _, err := realm.Append(a, []byte("12345678")); err != nil {
		t.Fatalf("%s append a: %v", journalRed, err)
	}
	if _, err := realm.Append(b, []byte("1234")); err != nil {
		t.Fatalf("%s append b: %v", journalRed, err)
	}
	beforeA, beforeB := realm.EndOffset(a), realm.EndOffset(b)
	if _, err := realm.Append(a, []byte("x")); !errors.Is(err, ErrQuota) {
		t.Fatalf("%s realm over-cap err=%v want=%v", journalRed, err, ErrQuota)
	}
	if realm.EndOffset(a) != beforeA || realm.EndOffset(b) != beforeB {
		t.Fatalf("%s realm quota changed offsets a=%d/%d b=%d/%d", journalRed, beforeA, realm.EndOffset(a), beforeB, realm.EndOffset(b))
	}
	if realm.Eligibility(a) != EligibilityUntrusted || realm.Reason(a) != ReasonQuota {
		t.Fatalf("%s realm quota state=%v reason=%v", journalRed, realm.Eligibility(a), realm.Reason(a))
	}
	if _, err := realm.Append(a, []byte("later")); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("%s realm quota accepted continuation err=%v", journalRed, err)
	}
	if realm.Reason(a) != ReasonQuota {
		t.Fatalf("%s realm quota first cause changed=%v", journalRed, realm.Reason(a))
	}
}

func TestAdvanceCommittedRejectsEveryAuthorityMutation(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(Record) Record
	}{
		{"key pane", func(record Record) Record { record.Key.Pane = "%foreign"; return record }},
		{"key incarnation", func(record Record) Record { record.Key.Incarnation = "foreign-incarnation"; return record }},
		{"start", func(record Record) Record { record.Start++; return record }},
		{"end", func(record Record) Record { record.End++; return record }},
		{"hash", func(record Record) Record { record.Hash[0] ^= 0xff; return record }},
	}
	for _, item := range mutations {
		t.Run(item.name, func(t *testing.T) {
			options := journalOptions(t)
			realm, err := OpenRealm(options)
			if err != nil {
				t.Fatalf("%s open: %v", journalRed, err)
			}
			defer realm.Close()
			key := journalKey("%1", "pane-inc-a")
			record, err := realm.Append(key, []byte("authority"))
			if err != nil {
				t.Fatalf("%s append: %v", journalRed, err)
			}
			if err := realm.Sync(key); err != nil {
				t.Fatalf("%s sync: %v", journalRed, err)
			}
			if err := realm.AdvanceCommitted(key, item.mutate(record)); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("%s mutation=%s err=%v want=%v", journalRed, item.name, err, ErrInvalidRecord)
			}
			if realm.CommittedOffset(key) != 0 || realm.Eligibility(key) != EligibilityUntrusted || realm.Reason(key) != ReasonCorruptJournal {
				t.Fatalf("%s mutation=%s offset=%d state=%v reason=%v", journalRed, item.name, realm.CommittedOffset(key), realm.Eligibility(key), realm.Reason(key))
			}
		})
	}
}

func TestRestartIgnoresUncommittedTailAndExposesCommittedPrefixOnly(t *testing.T) {
	options := journalOptions(t)
	first, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s first open: %v", journalRed, err)
	}
	key := journalKey("%1", "pane-inc-a")
	admitJournalPane(t, first, key)
	prefix := []byte("committed-prefix")
	committed, err := first.Append(key, prefix)
	if err != nil {
		t.Fatalf("%s append prefix: %v", journalRed, err)
	}
	if err := first.Sync(key); err != nil {
		t.Fatalf("%s sync prefix: %v", journalRed, err)
	}
	if err := first.AdvanceCommitted(key, committed); err != nil {
		t.Fatalf("%s commit prefix: %v", journalRed, err)
	}
	if _, err := first.Append(key, []byte("UNCOMMITTED-HIGH-ENTROPY-TAIL-8f7b2a91")); err != nil {
		t.Fatalf("%s append tail: %v", journalRed, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("%s close: %v", journalRed, err)
	}

	restartedOptions := options
	restartedOptions.BrokerIncarnation = "broker-incarnation-b"
	restarted, err := OpenRealm(restartedOptions)
	if err != nil {
		t.Fatalf("%s reopen: %v", journalRed, err)
	}
	defer restarted.Close()
	got, err := restarted.ReadCommitted(key)
	if err != nil || !bytes.Equal(got, prefix) {
		t.Fatalf("%s recovered committed=%q err=%v want=%q", journalRed, got, err, prefix)
	}
	recovered := restarted.Recovered()
	if len(recovered) != 1 || recovered[0].Committed != committed.End || recovered[0].Eligibility != EligibilityLegacy {
		t.Fatalf("%s uncommitted-tail recovery=%+v", journalRed, recovered)
	}
}

func TestRecoveryScannerRejectsTruncatedOrHashCorruptJournal(t *testing.T) {
	for _, mutation := range []string{"truncate", "flip"} {
		t.Run(mutation, func(t *testing.T) {
			options := journalOptions(t)
			realm, err := OpenRealm(options)
			if err != nil {
				t.Fatalf("%s open: %v", journalRed, err)
			}
			key := journalKey("%1", "pane-inc-a")
			admitJournalPane(t, realm, key)
			payload := []byte("CORRUPTION-PROBE-56f2d908a713")
			record, err := realm.Append(key, payload)
			if err != nil {
				t.Fatalf("%s append: %v", journalRed, err)
			}
			if err := realm.Sync(key); err != nil {
				t.Fatalf("%s sync: %v", journalRed, err)
			}
			if err := realm.AdvanceCommitted(key, record); err != nil {
				t.Fatalf("%s commit: %v", journalRed, err)
			}
			if err := realm.Close(); err != nil {
				t.Fatalf("%s close: %v", journalRed, err)
			}
			observed := requireObservedStorageMatchesHelper(t, options, key)
			journalPath := filepath.Join(options.RuntimeDir, observed.Journal)
			contents, err := os.ReadFile(journalPath)
			if err != nil || len(contents) < 2 {
				t.Fatalf("%s read persisted journal path=%s size=%d err=%v", journalRed, journalPath, len(contents), err)
			}
			switch mutation {
			case "truncate":
				err = os.Truncate(journalPath, int64(len(contents)-1))
			case "flip":
				contents[len(contents)-1] ^= 0xff
				err = os.WriteFile(journalPath, contents, 0o600)
			}
			if err != nil {
				t.Fatalf("%s mutate journal: %v", journalRed, err)
			}
			restartedOptions := options
			restartedOptions.BrokerIncarnation = "broker-incarnation-b"
			restarted, err := OpenRealm(restartedOptions)
			if err != nil {
				t.Fatalf("%s scanner did not return classified realm: %v", journalRed, err)
			}
			defer restarted.Close()
			recovered := restarted.Recovered()
			if len(recovered) != 1 || recovered[0].Key != key || recovered[0].Eligibility != EligibilityUntrusted || recovered[0].Reason != ReasonCorruptJournal {
				t.Fatalf("%s mutation=%s recovered=%+v", journalRed, mutation, recovered)
			}
			got, readErr := restarted.ReadCommitted(key)
			if len(got) != 0 || !errors.Is(readErr, ErrCorruptJournal) || restarted.UnifiedEligible(key) {
				t.Fatalf("%s mutation=%s corrupt payload=%x err=%v eligible=%v", journalRed, mutation, got, readErr, restarted.UnifiedEligible(key))
			}
		})
	}
}

func TestVolatileBrokerRestartRecoversOnlyLegacyEvidence(t *testing.T) {
	options := journalOptions(t)
	first, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s first open: %v", journalRed, err)
	}
	key := journalKey("%1", "pane-inc-a")
	admitJournalPane(t, first, key)
	record, err := first.Append(key, []byte("before restart"))
	if err != nil {
		t.Fatalf("%s append: %v", journalRed, err)
	}
	if err := first.Sync(key); err != nil {
		t.Fatalf("%s sync: %v", journalRed, err)
	}
	if err := first.AdvanceCommitted(key, record); err != nil {
		t.Fatalf("%s commit: %v", journalRed, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("%s first close: %v", journalRed, err)
	}

	restartedOptions := options
	restartedOptions.BrokerIncarnation = "broker-incarnation-b"
	restarted, err := OpenRealm(restartedOptions)
	if err != nil {
		t.Fatalf("%s restart scan: %v", journalRed, err)
	}
	defer restarted.Close()
	recovered := restarted.Recovered()
	if len(recovered) != 1 || recovered[0].Key != key || recovered[0].Committed != record.End || recovered[0].Eligibility != EligibilityLegacy || recovered[0].Reason != ReasonVolatileRestart {
		t.Fatalf("%s recovered=%+v", journalRed, recovered)
	}
	if restarted.UnifiedEligible(key) {
		t.Fatalf("%s volatile restart resumed unified eligibility", journalRed)
	}
}

type authorityCall struct {
	Name    string
	Key     PaneKey
	Record  Record
	Payload []byte
}

type fakeWriteAhead struct {
	calls  []authorityCall
	end    int64
	failAt string
	last   Record
}

func (fake *fakeWriteAhead) Append(key PaneKey, payload []byte) (Record, error) {
	fake.calls = append(fake.calls, authorityCall{Name: "append", Key: key, Payload: append([]byte(nil), payload...)})
	if fake.failAt == "append" {
		return Record{}, syscall.ENOSPC
	}
	fake.last = Record{Key: key, Start: fake.end, End: fake.end + int64(len(payload)), Hash: sha256.Sum256(payload)}
	fake.end = fake.last.End
	return fake.last, nil
}

func (fake *fakeWriteAhead) Sync(key PaneKey) error {
	fake.calls = append(fake.calls, authorityCall{Name: "sync", Key: key})
	if fake.failAt == "sync" {
		return errors.New("injected sync failure")
	}
	return nil
}

func (fake *fakeWriteAhead) AdvanceCommitted(key PaneKey, record Record) error {
	fake.calls = append(fake.calls, authorityCall{Name: "commit", Key: key, Record: record})
	if fake.failAt == "commit" {
		return errors.New("injected commit failure")
	}
	return nil
}

func callNames(calls []authorityCall) []string {
	names := make([]string, len(calls))
	for index := range calls {
		names[index] = calls[index].Name
	}
	return names
}

func TestWriteAheadSequencerOrdersAppendSyncCommitFeed(t *testing.T) {
	sink := &fakeWriteAhead{}
	sequencer := NewSequencer(sink, func(key PaneKey, record Record, payload []byte) error {
		sink.calls = append(sink.calls, authorityCall{Name: "feed", Key: key, Record: record, Payload: append([]byte(nil), payload...)})
		return nil
	})
	key := journalKey("%1", "pane-inc-a")
	payload := []byte("lineage-payload-7e4c92")
	if err := sequencer.Write(key, payload); err != nil {
		t.Fatalf("%s write: %v", sequencerRed, err)
	}
	wantOrder := []string{"append", "sync", "commit", "feed"}
	if !equalStrings(callNames(sink.calls), wantOrder) || len(sink.calls) != 4 {
		t.Fatalf("%s calls=%+v", sequencerRed, sink.calls)
	}
	if sink.calls[0].Key != key || !bytes.Equal(sink.calls[0].Payload, payload) {
		t.Fatalf("%s append lineage=%+v", sequencerRed, sink.calls[0])
	}
	if sink.calls[1].Key != key || sink.calls[2].Key != key || sink.calls[2].Record != sink.last {
		t.Fatalf("%s sync/commit lineage sync=%+v commit=%+v appended=%+v", sequencerRed, sink.calls[1], sink.calls[2], sink.last)
	}
	if sink.calls[3].Key != key || sink.calls[3].Record != sink.last || !bytes.Equal(sink.calls[3].Payload, payload) {
		t.Fatalf("%s feed lineage=%+v appended=%+v", sequencerRed, sink.calls[3], sink.last)
	}
}

func TestWriteAheadSequencerNeverFeedsUncommittedBytes(t *testing.T) {
	for _, failAt := range []string{"append", "sync", "commit"} {
		t.Run(failAt, func(t *testing.T) {
			sink := &fakeWriteAhead{failAt: failAt}
			feedCalls := 0
			sequencer := NewSequencer(sink, func(PaneKey, Record, []byte) error {
				feedCalls++
				return nil
			})
			key := journalKey("%1", "pane-inc-a")
			if err := sequencer.Write(key, []byte("must not feed")); err == nil {
				t.Fatalf("%s injected %s returned nil", sequencerRed, failAt)
			}
			if feedCalls != 0 {
				t.Fatalf("%s %s failure fed %d uncommitted payloads calls=%+v", sequencerRed, failAt, feedCalls, sink.calls)
			}
			wantCalls := map[string][]string{
				"append": {"append"},
				"sync":   {"append", "sync"},
				"commit": {"append", "sync", "commit"},
			}[failAt]
			if !equalStrings(callNames(sink.calls), wantCalls) {
				t.Fatalf("%s %s failure calls=%v want=%v", sequencerRed, failAt, callNames(sink.calls), wantCalls)
			}
			if !sequencer.Failed() {
				t.Fatalf("%s %s failure did not make sequencer sticky", sequencerRed, failAt)
			}
			callsBeforeRetry := len(sink.calls)
			if err := sequencer.Write(key, []byte("later")); !errors.Is(err, ErrInvalidated) {
				t.Fatalf("%s %s failure accepted continuation err=%v", sequencerRed, failAt, err)
			}
			if len(sink.calls) != callsBeforeRetry {
				t.Fatalf("%s %s sticky retry made later authority calls: %+v", sequencerRed, failAt, sink.calls[callsBeforeRetry:])
			}
		})
	}
}

func TestWriteAheadSequencerFeedFailureOccursOnlyAfterCommitAndFailsSticky(t *testing.T) {
	sink := &fakeWriteAhead{}
	sequencer := NewSequencer(sink, func(key PaneKey, record Record, payload []byte) error {
		sink.calls = append(sink.calls, authorityCall{Name: "feed", Key: key, Record: record, Payload: append([]byte(nil), payload...)})
		return errors.New("injected helper failure")
	})
	key := journalKey("%1", "pane-inc-a")
	if err := sequencer.Write(key, []byte("committed first")); err == nil {
		t.Fatalf("%s feed failure returned nil", sequencerRed)
	}
	if !equalStrings(callNames(sink.calls), []string{"append", "sync", "commit", "feed"}) || !sequencer.Failed() {
		t.Fatalf("%s feed failure calls=%+v failed=%v", sequencerRed, sink.calls, sequencer.Failed())
	}
	feed := sink.calls[len(sink.calls)-1]
	if feed.Key != key || feed.Record != sink.last || !bytes.Equal(feed.Payload, []byte("committed first")) {
		t.Fatalf("%s feed failure lost lineage feed=%+v appended=%+v", sequencerRed, feed, sink.last)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func payloadTextEncodings(payload []byte) []string {
	decimal := make([]string, len(payload))
	for index, value := range payload {
		decimal[index] = strconv.Itoa(int(value))
	}
	hexadecimal := hex.EncodeToString(payload)
	commaDecimal := strings.Join(decimal, ", ")
	hexBytes := make([]string, len(payload))
	for index, value := range payload {
		hexBytes[index] = fmt.Sprintf("0x%02x", value)
	}
	return []string{
		string(payload),
		hexadecimal,
		strings.ToUpper(hexadecimal),
		base64.StdEncoding.EncodeToString(payload),
		base64.RawStdEncoding.EncodeToString(payload),
		base64.URLEncoding.EncodeToString(payload),
		base64.RawURLEncoding.EncodeToString(payload),
		strings.Join(decimal, " "),
		strings.Join(decimal, ","),
		commaDecimal,
		"[" + strings.Join(decimal, " ") + "]",
		"[" + commaDecimal + "]",
		"[]byte{" + commaDecimal + "}",
		"[]uint8{" + commaDecimal + "}",
		strings.Join(hexBytes, ", "),
		fmt.Sprintf("%q", payload),
	}
}

func requirePayloadFreeText(t *testing.T, label string, payload []byte, text string) {
	t.Helper()
	for _, encoded := range payloadTextEncodings(payload) {
		if encoded != "" && strings.Contains(text, encoded) {
			t.Fatalf("%s %s leaked payload encoding=%q text=%q", journalRed, label, encoded, text)
		}
	}
}

func requirePayloadFreeValue(t *testing.T, label string, payload []byte, value any) {
	t.Helper()
	seen := make(map[uintptr]bool)
	var inspect func(reflect.Value, string)
	inspect = func(current reflect.Value, path string) {
		if !current.IsValid() {
			return
		}
		for current.Kind() == reflect.Interface {
			if current.IsNil() {
				return
			}
			current = current.Elem()
		}
		switch current.Kind() {
		case reflect.Pointer:
			if current.IsNil() {
				return
			}
			pointer := current.Pointer()
			if seen[pointer] {
				return
			}
			seen[pointer] = true
			inspect(current.Elem(), path+"*")
		case reflect.String:
			if strings.Contains(current.String(), string(payload)) {
				t.Fatalf("%s %s contains raw payload at %s", journalRed, label, path)
			}
		case reflect.Slice, reflect.Array:
			if current.Type().Elem().Kind() == reflect.Uint8 {
				data := make([]byte, current.Len())
				for index := 0; index < current.Len(); index++ {
					data[index] = byte(current.Index(index).Uint())
				}
				if bytes.Contains(data, payload) {
					t.Fatalf("%s %s contains raw payload bytes at %s", journalRed, label, path)
				}
				return
			}
			for index := 0; index < current.Len(); index++ {
				inspect(current.Index(index), fmt.Sprintf("%s[%d]", path, index))
			}
		case reflect.Map:
			iterator := current.MapRange()
			for iterator.Next() {
				inspect(iterator.Key(), path+".key")
				inspect(iterator.Value(), path+".value")
			}
		case reflect.Struct:
			for index := 0; index < current.NumField(); index++ {
				field := current.Type().Field(index)
				if field.PkgPath == "" {
					inspect(current.Field(index), path+"."+field.Name)
				}
			}
		}
	}
	inspect(reflect.ValueOf(value), label)
	for _, rendered := range []string{fmt.Sprint(value), fmt.Sprintf("%+v", value), fmt.Sprintf("%#v", value)} {
		requirePayloadFreeText(t, label, payload, rendered)
	}
}

func TestJournalErrorsRecoveryAndStatusNeverPublishPayload(t *testing.T) {
	const sentinel = "PERSEA-JOURNAL-SECRET-c4b17e96f032a8d5"
	payload := []byte(sentinel)
	options := journalOptions(t)
	options.PaneCapBytes = int64(len(sentinel))
	options.RealmCapBytes = int64(len(sentinel) * 3)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", journalRed, err)
	}
	key := journalKey("%1", "pane-secret")
	record, appendErr := realm.Append(key, payload)
	if appendErr != nil {
		t.Fatalf("%s append sentinel: %v", journalRed, appendErr)
	}
	syncErr := realm.Sync(key)
	if syncErr != nil {
		t.Fatalf("%s sync sentinel: %v", journalRed, syncErr)
	}
	commitErr := realm.AdvanceCommitted(key, record)
	if commitErr != nil {
		t.Fatalf("%s commit sentinel: %v", journalRed, commitErr)
	}
	tooLargeKey := journalKey("%2", "pane-overflow")
	quotaRecord, quotaErr := realm.Append(tooLargeKey, []byte(sentinel+sentinel))
	if !errors.Is(quotaErr, ErrQuota) {
		t.Fatalf("%s privacy quota err=%v want=%v", journalRed, quotaErr, ErrQuota)
	}
	invalidatedRecord, invalidatedErr := realm.Append(tooLargeKey, payload)
	if !errors.Is(invalidatedErr, ErrInvalidated) {
		t.Fatalf("%s privacy continuation err=%v want=%v", journalRed, invalidatedErr, ErrInvalidated)
	}
	beforeRestart := map[string]any{
		"record":             record,
		"append_error":       appendErr,
		"sync_error":         syncErr,
		"commit_error":       commitErr,
		"quota_record":       quotaRecord,
		"quota_error":        quotaErr,
		"invalidated_record": invalidatedRecord,
		"invalidated_error":  invalidatedErr,
		"eligibility":        realm.Eligibility(tooLargeKey),
		"reason":             realm.Reason(tooLargeKey),
		"end_offset":         realm.EndOffset(tooLargeKey),
		"committed_offset":   realm.CommittedOffset(tooLargeKey),
		"unified_eligible":   realm.UnifiedEligible(tooLargeKey),
	}
	for label, value := range beforeRestart {
		requirePayloadFreeValue(t, "before_restart."+label, payload, value)
	}
	closeErr := realm.Close()
	if closeErr != nil {
		t.Fatalf("%s close: %v", journalRed, closeErr)
	}
	requirePayloadFreeValue(t, "close_error", payload, closeErr)

	restartedOptions := options
	restartedOptions.BrokerIncarnation = "broker-incarnation-b"
	restarted, restartErr := OpenRealm(restartedOptions)
	if restartErr != nil {
		t.Fatalf("%s restart: %v", journalRed, restartErr)
	}
	afterRestart := map[string]any{
		"restart_error":    restartErr,
		"recovery":         restarted.Recovered(),
		"eligibility":      restarted.Eligibility(key),
		"reason":           restarted.Reason(key),
		"end_offset":       restarted.EndOffset(key),
		"committed_offset": restarted.CommittedOffset(key),
		"unified_eligible": restarted.UnifiedEligible(key),
	}
	for label, value := range afterRestart {
		requirePayloadFreeValue(t, "after_restart."+label, payload, value)
	}
	restartCloseErr := restarted.Close()
	if restartCloseErr != nil {
		t.Fatalf("%s restart close: %v", journalRed, restartCloseErr)
	}
	requirePayloadFreeValue(t, "restart_close_error", payload, restartCloseErr)
}

func TestEligibilityStateMachineAdmitsContinuousOnlyAndFaultsSticky(t *testing.T) {
	key := journalKey("%1", "pane-inc-a")
	continuous := NewEligibility(Admission{
		Kind:         AdmissionObservedBeforeFirstByte,
		Key:          key,
		JournalStart: 0,
	})
	if continuous.State() != EligibilityContinuous || !continuous.UnifiedEligible() {
		t.Fatalf("%s birth-observed state=%v eligible=%v", eligibilityRed, continuous.State(), continuous.UnifiedEligible())
	}
	if err := continuous.ObserveCommittedRange(0, 4); err != nil {
		t.Fatalf("%s first range: %v", eligibilityRed, err)
	}
	if err := continuous.ObserveCommittedRange(4, 9); err != nil {
		t.Fatalf("%s contiguous range: %v", eligibilityRed, err)
	}
	if continuous.CommittedOffset() != 9 || !continuous.UnifiedEligible() {
		t.Fatalf("%s contiguous state=%v offset=%d", eligibilityRed, continuous.State(), continuous.CommittedOffset())
	}
	if err := continuous.ObserveCommittedRange(10, 12); !errors.Is(err, ErrJournalGap) {
		t.Fatalf("%s gap err=%v want=%v", eligibilityRed, err, ErrJournalGap)
	}
	if continuous.State() != EligibilityUntrusted || continuous.UnifiedEligible() {
		t.Fatalf("%s gap state=%v eligible=%v", eligibilityRed, continuous.State(), continuous.UnifiedEligible())
	}
	if err := continuous.ObserveCommittedRange(9, 10); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("%s gap was healed err=%v", eligibilityRed, err)
	}

	for _, kind := range []AdmissionKind{AdmissionExistingPane, AdmissionReconstructedPane, AdmissionAfterBrokerRestart} {
		legacy := NewEligibility(Admission{Kind: kind, Key: key})
		if legacy.State() != EligibilityLegacy || legacy.UnifiedEligible() {
			t.Fatalf("%s admission=%v state=%v eligible=%v", eligibilityRed, kind, legacy.State(), legacy.UnifiedEligible())
		}
	}

	for _, reason := range []InvalidationReason{
		ReasonControlPaused,
		ReasonControlDisconnected,
		ReasonDecoderAmbiguous,
		ReasonSourceReplacement,
		ReasonQuota,
		ReasonENOSPC,
		ReasonCorruptJournal,
	} {
		tracker := NewEligibility(Admission{Kind: AdmissionObservedBeforeFirstByte, Key: key, JournalStart: 0})
		tracker.Invalidate(reason)
		if tracker.State() != EligibilityUntrusted || tracker.Reason() != reason || tracker.UnifiedEligible() {
			t.Fatalf("%s reason=%v state=%v reported=%v eligible=%v", eligibilityRed, reason, tracker.State(), tracker.Reason(), tracker.UnifiedEligible())
		}
		tracker.Invalidate(ReasonControlDisconnected)
		if tracker.Reason() != reason {
			t.Fatalf("%s first fault reason overwritten first=%v got=%v", eligibilityRed, reason, tracker.Reason())
		}
	}
}

// ---------------------------------------------------------------------------
// ISSUE25 — PUJ2 typed event log.
//
// The unified terminal's replay truth becomes an ordered event stream, not a
// byte stream: an initial geometry, then committed OUTPUT and GEOMETRY events
// in one order. These tests fix that format's fail-closed edges. Pane output is
// attacker-influenceable, so the single most important property here is that no
// sequence of payload bytes can produce a geometry event.
// ---------------------------------------------------------------------------

const puj2Red = "ISSUE25/PUJ2/CANDIDATE_RED/TYPED_EVENT_JOURNAL"

func admitJournalPane(t *testing.T, realm *Realm, key PaneKey) Geometry {
	t.Helper()
	initial := Geometry{Columns: 80, Rows: 24}
	if err := realm.AdmitPane(key, initial); err != nil {
		t.Fatalf("%s admit pane: %v", puj2Red, err)
	}
	return initial
}

func journalPathFor(t *testing.T, options OpenOptions, key PaneKey) string {
	t.Helper()
	observed := requireObservedStorageMatchesHelper(t, options, key)
	return filepath.Join(options.RuntimeDir, observed.Journal)
}

type puj2Frame struct {
	offset int
	size   int
	commit bool
}

// puj2Frames walks the on-disk layout independently of the production writer,
// so a silent layout change is a red rather than a coincidence.
func puj2Frames(t *testing.T, contents []byte) []puj2Frame {
	t.Helper()
	if len(contents) < 8 {
		t.Fatalf("%s journal shorter than a header: %d bytes", puj2Red, len(contents))
	}
	position := 8 + int(binary.BigEndian.Uint32(contents[4:8]))
	var frames []puj2Frame
	for position < len(contents) {
		switch contents[position] {
		case 0xa1:
			if len(contents)-position < 70 {
				t.Fatalf("%s truncated append frame at %d", puj2Red, position)
			}
			length := int(binary.BigEndian.Uint32(contents[position+26 : position+30]))
			frames = append(frames, puj2Frame{offset: position, size: 70 + length})
			position += 70 + length
		case 0xc1:
			if len(contents)-position < 58 {
				t.Fatalf("%s truncated commit frame at %d", puj2Red, position)
			}
			frames = append(frames, puj2Frame{offset: position, size: 58, commit: true})
			position += 58
		default:
			t.Fatalf("%s unknown record marker %#x at %d", puj2Red, contents[position], position)
		}
	}
	return frames
}

func commitLast(t *testing.T, realm *Realm, key PaneKey, record Record) {
	t.Helper()
	if err := realm.Sync(key); err != nil {
		t.Fatalf("%s sync: %v", puj2Red, err)
	}
	if err := realm.AdvanceCommitted(key, record); err != nil {
		t.Fatalf("%s advance committed: %v", puj2Red, err)
	}
}

func TestPUJ2HeaderCarriesMandatoryInitialGeometryAndPUJ1FailsClosed(t *testing.T) {
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", puj2Red, err)
	}
	key := journalKey("%1", "pane-inc-a")
	initial := Geometry{Columns: 132, Rows: 43}
	if err := realm.AdmitPane(key, initial); err != nil {
		t.Fatalf("%s admit: %v", puj2Red, err)
	}
	if got, err := realm.InitialGeometry(key); err != nil || got != initial {
		t.Fatalf("%s initial geometry=%+v err=%v want=%+v", puj2Red, got, err, initial)
	}
	record, err := realm.Append(key, []byte("after-birth"))
	if err != nil {
		t.Fatalf("%s append: %v", puj2Red, err)
	}
	commitLast(t, realm, key, record)
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", puj2Red, err)
	}

	path := journalPathFor(t, options, key)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s read journal: %v", puj2Red, err)
	}
	if string(contents[:4]) != "PUJ2" {
		t.Fatalf("%s journal magic=%q want PUJ2", puj2Red, contents[:4])
	}

	// The durable header must carry the birth geometry; a generation that does
	// not is not a generation this engine may replay.
	restarted := reopenRealm(t, options)
	if got, err := restarted.InitialGeometry(key); err != nil || got != initial {
		t.Fatalf("%s reopened initial geometry=%+v err=%v want=%+v", puj2Red, got, err, initial)
	}
	_ = restarted.Close()

	// A journal whose framing itself is unreadable cannot even be attributed to a
	// pane, so the realm refuses to open. That is the heaviest fail-closed edge
	// and it is the correct one: nothing is guessed and nothing is migrated.
	for name, mutate := range map[string]func([]byte) []byte{
		"PUJ1 magic": func(raw []byte) []byte {
			out := append([]byte(nil), raw...)
			out[3] = '1'
			return out
		},
		"unknown future version": func(raw []byte) []byte {
			return replaceHeaderJSON(t, raw, `"Version":2`, `"Version":9`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			fresh := freshRuntime(t, options)
			writeJournalCopy(t, fresh, key, mutate(contents))
			reopened, err := OpenRealm(fresh)
			if err == nil {
				defer reopened.Close()
				if reopened.UnifiedEligible(key) {
					t.Fatalf("%s %s produced a unified-eligible generation", puj2Red, name)
				}
				if _, err := reopened.InitialGeometry(key); err == nil {
					t.Fatalf("%s %s exposed an initial geometry", puj2Red, name)
				}
				if _, err := reopened.ReadCommitted(key); err == nil {
					t.Fatalf("%s %s exposed committed bytes", puj2Red, name)
				}
			}
		})
	}

	// A readable header that carries no usable birth geometry is attributable, so
	// it is classified per pane and the rest of the realm survives.
	for name, mutate := range map[string]func([]byte) []byte{
		"absent initial geometry": func(raw []byte) []byte {
			return replaceHeaderJSON(t, raw, `"GeometryInitial":{"columns":132,"rows":43}`, `"GeometryInitial":null`)
		},
		"zero initial geometry": func(raw []byte) []byte {
			return replaceHeaderJSON(t, raw, `"columns":132,"rows":43`, `"columns":0,"rows":0`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			fresh := freshRuntime(t, options)
			writeJournalCopy(t, fresh, key, mutate(contents))
			reopened, err := OpenRealm(fresh)
			if err != nil {
				t.Fatalf("%s scanner did not classify realm: %v", puj2Red, err)
			}
			defer reopened.Close()
			if reopened.UnifiedEligible(key) {
				t.Fatalf("%s %s remained unified-eligible", puj2Red, name)
			}
			if _, err := reopened.InitialGeometry(key); err == nil {
				t.Fatalf("%s %s exposed an initial geometry", puj2Red, name)
			}
			if _, err := reopened.ReadCommittedEvents(key); err == nil {
				t.Fatalf("%s %s exposed committed events", puj2Red, name)
			}
			if _, err := reopened.ReadCommitted(key); !errors.Is(err, ErrCorruptJournal) {
				t.Fatalf("%s %s committed read err=%v want=%v", puj2Red, name, err, ErrCorruptJournal)
			}
		})
	}
}

func TestUnadmittedPaneIsByteOnlyAndNeverGainsUnifiedAuthority(t *testing.T) {
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", puj2Red, err)
	}
	key := journalKey("%1", "pane-inc-a")
	// No AdmitPane: this pane never received a durable birth geometry.
	record, err := realm.Append(key, []byte("byte-only"))
	if err != nil {
		t.Fatalf("%s append: %v", puj2Red, err)
	}
	if realm.UnifiedEligible(key) {
		t.Fatalf("%s a pane with no durable birth geometry claimed unified authority", puj2Red)
	}
	if _, err := realm.InitialGeometry(key); err == nil {
		t.Fatalf("%s unadmitted pane produced an initial geometry", puj2Red)
	}
	if _, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: 30}); err == nil {
		t.Fatalf("%s unadmitted pane accepted a geometry event", puj2Red)
	}
	if _, err := realm.ReadCommittedEvents(key); err == nil {
		t.Fatalf("%s unadmitted pane produced committed events", puj2Red)
	}
	// Admission after the fact must not retrofit authority onto existing bytes.
	if err := realm.AdmitPane(key, Geometry{Columns: 80, Rows: 24}); err == nil {
		t.Fatalf("%s admission retrofitted authority onto an existing byte-only pane", puj2Red)
	}
	commitLast(t, realm, key, record)
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", puj2Red, err)
	}
	restarted := reopenRealm(t, options)
	defer restarted.Close()
	if restarted.UnifiedEligible(key) {
		t.Fatalf("%s byte-only generation became unified-eligible after restart", puj2Red)
	}
	if _, err := restarted.ReadCommitted(key); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("%s byte-only generation did not fail closed on reopen: %v", puj2Red, err)
	}
}

func TestTypedOutputGeometryOutputProjectionSurvivesReopenExactly(t *testing.T) {
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", puj2Red, err)
	}
	key := journalKey("%1", "pane-inc-a")
	initial := admitJournalPane(t, realm, key)

	before := []byte("before-geometry")
	record, err := realm.Append(key, before)
	if err != nil {
		t.Fatalf("%s append before: %v", puj2Red, err)
	}
	commitLast(t, realm, key, record)
	geometry := Geometry{Columns: 80, Rows: 43}
	geometryRecord, err := realm.AppendGeometry(key, geometry)
	if err != nil {
		t.Fatalf("%s append geometry: %v", puj2Red, err)
	}
	if geometryRecord.Kind != RecordGeometry || geometryRecord.Geometry != geometry ||
		geometryRecord.Start != record.End || geometryRecord.End != record.End {
		t.Fatalf("%s geometry record=%+v", puj2Red, geometryRecord)
	}
	if geometryRecord.Sequence != record.Sequence+1 {
		t.Fatalf("%s geometry sequence=%d after output sequence=%d", puj2Red, geometryRecord.Sequence, record.Sequence)
	}
	commitLast(t, realm, key, geometryRecord)
	after := []byte("after-geometry")
	last, err := realm.Append(key, after)
	if err != nil {
		t.Fatalf("%s append after: %v", puj2Red, err)
	}
	commitLast(t, realm, key, last)

	want := []Event{
		{Kind: RecordOutput, Sequence: 1, Start: 0, End: int64(len(before)), Payload: before},
		{Kind: RecordGeometry, Sequence: 2, Start: int64(len(before)), End: int64(len(before)), Geometry: geometry},
		{Kind: RecordOutput, Sequence: 3, Start: int64(len(before)), End: int64(len(before) + len(after)), Payload: after},
	}
	requireEvents(t, "live", realm, key, initial, want)
	if got, err := realm.ReadCommitted(key); err != nil || !bytes.Equal(got, append(append([]byte(nil), before...), after...)) {
		t.Fatalf("%s committed bytes=%q err=%v", puj2Red, got, err)
	}
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", puj2Red, err)
	}

	// Reconnect equivalence: a restart must rebuild the identical projection.
	restarted := reopenRealm(t, options)
	defer restarted.Close()
	requireEvents(t, "reopened", restarted, key, initial, want)
}

func TestConsecutiveGeometryEventsRemainDistinctlyOrdered(t *testing.T) {
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", puj2Red, err)
	}
	key := journalKey("%1", "pane-inc-a")
	initial := admitJournalPane(t, realm, key)
	geometries := []Geometry{{Columns: 80, Rows: 30}, {Columns: 80, Rows: 30}, {Columns: 80, Rows: 44}}
	var want []Event
	for index, geometry := range geometries {
		record, err := realm.AppendGeometry(key, geometry)
		if err != nil {
			t.Fatalf("%s append geometry %d: %v", puj2Red, index, err)
		}
		commitLast(t, realm, key, record)
		want = append(want, Event{Kind: RecordGeometry, Sequence: int64(index + 1), Geometry: geometry})
	}
	requireEvents(t, "live", realm, key, initial, want)
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", puj2Red, err)
	}
	// Three zero-length records share one byte offset. Only a sequence number
	// independent of byte offset can keep them distinct and ordered.
	restarted := reopenRealm(t, options)
	defer restarted.Close()
	requireEvents(t, "reopened", restarted, key, initial, want)
}

func TestGeometryAppendRejectsMalformedDimensions(t *testing.T) {
	for _, geometry := range []Geometry{
		{Columns: 0, Rows: 24}, {Columns: 80, Rows: 0}, {Columns: -1, Rows: 24},
		{Columns: 80, Rows: -1}, {Columns: 100000, Rows: 24}, {Columns: 80, Rows: 100000},
	} {
		t.Run(fmt.Sprintf("%dx%d", geometry.Columns, geometry.Rows), func(t *testing.T) {
			options := journalOptions(t)
			realm, err := OpenRealm(options)
			if err != nil {
				t.Fatalf("%s open: %v", puj2Red, err)
			}
			defer realm.Close()
			key := journalKey("%1", "pane-inc-a")
			admitJournalPane(t, realm, key)
			if _, err := realm.AppendGeometry(key, geometry); err == nil {
				t.Fatalf("%s malformed geometry %+v accepted", puj2Red, geometry)
			}
			// A refused malformed request is not a fault: the generation continues.
			if !realm.UnifiedEligible(key) {
				t.Fatalf("%s malformed geometry invalidated the generation", puj2Red)
			}
			if err := realm.AdmitPane(journalKey("%2", "pane-inc-b"), geometry); err == nil {
				t.Fatalf("%s malformed birth geometry %+v admitted", puj2Red, geometry)
			}
		})
	}
}

func TestTerminalPayloadCannotConstructGeometryEvent(t *testing.T) {
	// Real geometry frames, taken byte for byte out of a journal the production
	// writer produced, are the strongest forgery attempt a pane can make.
	_, _, corpus := puj2CorpusJournal(t)
	corpusFrames := puj2Frames(t, corpus)
	forged := append([]byte(nil), corpus[corpusFrames[2].offset:corpusFrames[3].offset+corpusFrames[3].size]...)

	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", puj2Red, err)
	}
	key := journalKey("%1", "pane-inc-a")
	initial := admitJournalPane(t, realm, key)
	payload := append([]byte(nil), forged...)
	for value := 0; value < 256; value++ {
		payload = append(payload, byte(value))
	}
	payload = append(payload, []byte("\x1b]1337;Geometry=80x99\x07\x1bPGEOMETRY 80 99\x1b\\GEOMETRY_INITIAL{\"columns\":80,\"rows\":99}")...)
	payload = append(payload, forged...)
	record, err := realm.Append(key, payload)
	if err != nil {
		t.Fatalf("%s append hostile payload: %v", puj2Red, err)
	}
	commitLast(t, realm, key, record)
	want := []Event{{Kind: RecordOutput, Sequence: 1, Start: 0, End: int64(len(payload)), Payload: payload}}
	requireEvents(t, "live", realm, key, initial, want)
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", puj2Red, err)
	}
	restarted := reopenRealm(t, options)
	defer restarted.Close()
	requireEvents(t, "reopened", restarted, key, initial, want)
	if got, err := restarted.InitialGeometry(key); err != nil || got != initial {
		t.Fatalf("%s hostile payload moved the initial geometry to %+v err=%v", puj2Red, got, err)
	}
}

func TestCorruptTypedRecordFieldsFailClosedOnReopen(t *testing.T) {
	options, key, contents := puj2CorpusJournal(t)
	frames := puj2Frames(t, contents)
	if len(frames) != 6 {
		t.Fatalf("%s corpus frame count=%d want 6", puj2Red, len(frames))
	}
	geometryAppend := frames[2].offset
	outputAppend := frames[0].offset
	mutations := map[string]func([]byte){
		"unknown record kind":      func(raw []byte) { raw[geometryAppend+1] = 0x7f },
		"output reinterpreted":     func(raw []byte) { raw[geometryAppend+1] = 0x01 },
		"geometry reinterpreted":   func(raw []byte) { raw[outputAppend+1] = 0x02 },
		"sequence gap":             func(raw []byte) { raw[geometryAppend+9] = 0x09 },
		"geometry columns":         func(raw []byte) { raw[geometryAppend+33] ^= 0xff },
		"geometry rows":            func(raw []byte) { raw[geometryAppend+37] ^= 0xff },
		"geometry hash":            func(raw []byte) { raw[geometryAppend+38] ^= 0xff },
		"geometry commit sequence": func(raw []byte) { raw[frames[3].offset+9] ^= 0xff },
		"geometry commit hash":     func(raw []byte) { raw[frames[3].offset+26] ^= 0xff },
		"output payload hash":      func(raw []byte) { raw[outputAppend+38] ^= 0xff },
		"output start byte offset": func(raw []byte) { raw[outputAppend+17] = 0x05 },
		// Flipping a payload byte leaves every framing field and the commit binding
		// intact, so only the payload hash can reject it.
		"output payload bytes":       func(raw []byte) { raw[outputAppend+70] ^= 0xff },
		"geometry carries a payload": func(raw []byte) { raw[geometryAppend+29] = 0x01 },
		// Corrupting append and commit consistently defeats the commit binding, so
		// only the closed record-kind set can reject this.
		"unknown record kind in append and commit": func(raw []byte) {
			raw[geometryAppend+1] = 0x7f
			raw[frames[3].offset+1] = 0x7f
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			raw := append([]byte(nil), contents...)
			mutate(raw)
			fresh := freshRuntime(t, options)
			writeJournalCopy(t, fresh, key, raw)
			reopened, err := OpenRealm(fresh)
			if err != nil {
				t.Fatalf("%s scanner did not classify realm: %v", puj2Red, err)
			}
			defer reopened.Close()
			if reopened.UnifiedEligible(key) {
				t.Fatalf("%s %s remained unified-eligible", puj2Red, name)
			}
			if _, err := reopened.ReadCommittedEvents(key); err == nil {
				t.Fatalf("%s %s exposed committed events", puj2Red, name)
			}
			if reopened.Reason(key) != ReasonCorruptJournal {
				t.Fatalf("%s %s reason=%v", puj2Red, name, reopened.Reason(key))
			}
		})
	}
}

func TestTruncationAtEveryByteNeverForgesCommittedEvents(t *testing.T) {
	options, key, contents := puj2CorpusJournal(t)
	full := puj2CorpusEvents()
	for cut := 0; cut < len(contents); cut++ {
		fresh := freshRuntime(t, options)
		writeJournalCopy(t, fresh, key, contents[:cut])
		reopened, err := OpenRealm(fresh)
		if err != nil {
			// A cut inside the framing leaves nothing attributable; refusing the
			// realm is the fail-closed outcome, not a forged one.
			continue
		}
		events, eventsErr := reopened.ReadCommittedEvents(key)
		if eventsErr == nil {
			if len(events) > len(full) {
				t.Fatalf("%s cut=%d produced %d committed events, more than the whole journal holds", puj2Red, cut, len(events))
			}
			for index := range events {
				if !sameEvent(events[index], full[index]) {
					t.Fatalf("%s cut=%d event %d=%+v want prefix element %+v", puj2Red, cut, index, events[index], full[index])
				}
			}
		}
		if reopened.UnifiedEligible(key) {
			t.Fatalf("%s cut=%d a recovered generation claimed unified authority", puj2Red, cut)
		}
		_ = reopened.Close()
	}
}

// TestRemovedGeometryRecordFailsClosed is the regression test for sequence numbering.
// Consecutive geometry records share one byte offset, so a byte-offset journal
// cannot notice that one of them is gone: the event would silently disappear
// from replay while every remaining record still validated.
func TestRemovedGeometryRecordFailsClosed(t *testing.T) {
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", puj2Red, err)
	}
	key := journalKey("%1", "pane-inc-a")
	admitJournalPane(t, realm, key)
	for _, rows := range []int{30, 37, 44} {
		record, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: rows})
		if err != nil {
			t.Fatalf("%s append geometry %d: %v", puj2Red, rows, err)
		}
		commitLast(t, realm, key, record)
	}
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", puj2Red, err)
	}
	contents, err := os.ReadFile(journalPathFor(t, options, key))
	if err != nil {
		t.Fatalf("%s read journal: %v", puj2Red, err)
	}
	frames := puj2Frames(t, contents)
	if len(frames) != 6 {
		t.Fatalf("%s geometry-only journal frames=%d want 6", puj2Red, len(frames))
	}
	spliced := append([]byte(nil), contents[:frames[2].offset]...)
	spliced = append(spliced, contents[frames[3].offset+frames[3].size:]...)

	fresh := freshRuntime(t, options)
	writeJournalCopy(t, fresh, key, spliced)
	reopened, err := OpenRealm(fresh)
	if err != nil {
		t.Fatalf("%s scanner did not classify realm: %v", puj2Red, err)
	}
	defer reopened.Close()
	if _, err := reopened.ReadCommittedEvents(key); err == nil {
		t.Fatalf("%s a removed geometry record went unnoticed", puj2Red)
	}
	if reopened.Reason(key) != ReasonCorruptJournal {
		t.Fatalf("%s removed geometry record reason=%v", puj2Red, reopened.Reason(key))
	}
}

func TestUncommittedGeometryIsInvisibleAfterRestart(t *testing.T) {
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", puj2Red, err)
	}
	key := journalKey("%1", "pane-inc-a")
	initial := admitJournalPane(t, realm, key)
	record, err := realm.Append(key, []byte("committed"))
	if err != nil {
		t.Fatalf("%s append: %v", puj2Red, err)
	}
	commitLast(t, realm, key, record)
	if _, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: 55}); err != nil {
		t.Fatalf("%s append uncommitted geometry: %v", puj2Red, err)
	}
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", puj2Red, err)
	}
	restarted := reopenRealm(t, options)
	defer restarted.Close()
	requireEvents(t, "reopened", restarted, key, initial, []Event{
		{Kind: RecordOutput, Sequence: 1, Start: 0, End: 9, Payload: []byte("committed")},
	})
}

func TestGeometryRecordsChargeStorageBudget(t *testing.T) {
	options := journalOptions(t)
	options.PaneCapBytes = 512
	options.RealmCapBytes = 4096
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", puj2Red, err)
	}
	defer realm.Close()
	key := journalKey("%1", "pane-inc-a")
	admitJournalPane(t, realm, key)
	accepted := 0
	for iteration := 0; iteration < 64; iteration++ {
		record, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: 24 + iteration%16})
		if errors.Is(err, ErrQuota) {
			break
		}
		if err != nil {
			t.Fatalf("%s geometry append %d: %v", puj2Red, iteration, err)
		}
		commitLast(t, realm, key, record)
		accepted++
	}
	if accepted == 0 || accepted >= 64 {
		t.Fatalf("%s zero-payload geometry records are not bounded by the pane cap: accepted=%d", puj2Red, accepted)
	}
	if realm.Eligibility(key) != EligibilityUntrusted || realm.Reason(key) != ReasonQuota {
		t.Fatalf("%s geometry quota state=%v reason=%v", puj2Red, realm.Eligibility(key), realm.Reason(key))
	}
	if _, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: 24}); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("%s invalidated generation accepted a further geometry event: %v", puj2Red, err)
	}
}

// --- shared helpers ---------------------------------------------------------

func sameEvent(left, right Event) bool {
	return left.Kind == right.Kind && left.Sequence == right.Sequence &&
		left.Start == right.Start && left.End == right.End &&
		left.Geometry == right.Geometry && bytes.Equal(left.Payload, right.Payload)
}

func requireEvents(t *testing.T, label string, realm *Realm, key PaneKey, initial Geometry, want []Event) {
	t.Helper()
	got, err := realm.ReadCommittedEvents(key)
	if err != nil {
		t.Fatalf("%s %s read committed events: %v", puj2Red, label, err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s %s committed events=%d want=%d got=%+v", puj2Red, label, len(got), len(want), got)
	}
	for index := range want {
		if !sameEvent(got[index], want[index]) {
			t.Fatalf("%s %s event %d=%+v want=%+v", puj2Red, label, index, got[index], want[index])
		}
	}
	if geometry, err := realm.InitialGeometry(key); err != nil || geometry != initial {
		t.Fatalf("%s %s initial geometry=%+v err=%v want=%+v", puj2Red, label, geometry, err, initial)
	}
}

func reopenRealm(t *testing.T, options OpenOptions) *Realm {
	t.Helper()
	restarted := options
	restarted.BrokerIncarnation = options.BrokerIncarnation + "-restarted"
	realm, err := OpenRealm(restarted)
	if err != nil {
		t.Fatalf("%s reopen: %v", puj2Red, err)
	}
	return realm
}

func freshRuntime(t *testing.T, options OpenOptions) OpenOptions {
	t.Helper()
	fresh := options
	fresh.RuntimeDir = filepath.Join(t.TempDir(), "realm-runtime")
	fresh.BrokerIncarnation = options.BrokerIncarnation + "-copy"
	if err := os.Mkdir(fresh.RuntimeDir, 0o700); err != nil {
		t.Fatalf("%s create runtime: %v", puj2Red, err)
	}
	return fresh
}

func writeJournalCopy(t *testing.T, options OpenOptions, key PaneKey, contents []byte) {
	t.Helper()
	paths := storagePathsForTest(options, key)
	for _, directory := range paths.Directories {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("%s create journal directory: %v", puj2Red, err)
		}
	}
	if err := os.WriteFile(paths.Journal, contents, 0o600); err != nil {
		t.Fatalf("%s write journal copy: %v", puj2Red, err)
	}
}

func replaceHeaderJSON(t *testing.T, contents []byte, from, to string) []byte {
	t.Helper()
	size := int(binary.BigEndian.Uint32(contents[4:8]))
	header := string(contents[8 : 8+size])
	if !strings.Contains(header, from) {
		t.Fatalf("%s header %q does not contain %q", puj2Red, header, from)
	}
	replaced := strings.Replace(header, from, to, 1)
	out := make([]byte, 0, 8+len(replaced)+len(contents)-8-size)
	out = append(out, contents[:4]...)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(replaced)))
	out = append(out, length[:]...)
	out = append(out, replaced...)
	out = append(out, contents[8+size:]...)
	return out
}

func puj2CorpusEvents() []Event {
	before := []byte("corpus-before")
	after := []byte("corpus-after")
	return []Event{
		{Kind: RecordOutput, Sequence: 1, Start: 0, End: int64(len(before)), Payload: before},
		{Kind: RecordGeometry, Sequence: 2, Start: int64(len(before)), End: int64(len(before)), Geometry: Geometry{Columns: 80, Rows: 43}},
		{Kind: RecordOutput, Sequence: 3, Start: int64(len(before)), End: int64(len(before) + len(after)), Payload: after},
	}
}

// puj2CorpusJournal builds one committed OUTPUT -> GEOMETRY -> OUTPUT journal and
// returns its exact bytes, so the corruption and truncation corpora operate on a
// real production-written file rather than a reconstruction.
func puj2CorpusJournal(t *testing.T) (OpenOptions, PaneKey, []byte) {
	t.Helper()
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", puj2Red, err)
	}
	key := journalKey("%1", "pane-inc-a")
	admitJournalPane(t, realm, key)
	for _, event := range puj2CorpusEvents() {
		var record Record
		var appendErr error
		if event.Kind == RecordGeometry {
			record, appendErr = realm.AppendGeometry(key, event.Geometry)
		} else {
			record, appendErr = realm.Append(key, event.Payload)
		}
		if appendErr != nil {
			t.Fatalf("%s corpus append %d: %v", puj2Red, event.Sequence, appendErr)
		}
		commitLast(t, realm, key, record)
	}
	if err := realm.Close(); err != nil {
		t.Fatalf("%s corpus close: %v", puj2Red, err)
	}
	contents, err := os.ReadFile(journalPathFor(t, options, key))
	if err != nil {
		t.Fatalf("%s read corpus journal: %v", puj2Red, err)
	}
	return options, key, contents
}

// ---------------------------------------------------------------------------
// ISSUE25 B1 — durable geometry cost survives a realm reopen.
//
// A geometry record carries no payload, so it adds nothing to the byte stream a
// pane holds. It does occupy 128 durable bytes on disk, and live accounting
// charges exactly that against the realm. Rebuilding the realm charge from the
// pane's output length alone therefore forgets every geometry record that was
// ever written, and each restart hands back capacity for durable bytes that are
// still sitting in the file. Enough restarts and the realm's files exceed the
// cap the realm believes it is enforcing.
// ---------------------------------------------------------------------------

const budgetRed = "ISSUE25/PUJ2/CANDIDATE_RED/DURABLE_GEOMETRY_BUDGET"

func TestCommittedGeometryChargeSurvivesReopen(t *testing.T) {
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", budgetRed, err)
	}
	key := journalKey("%1", "pane-inc-a")
	admitJournalPane(t, realm, key)
	record, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: 43})
	if err != nil {
		t.Fatalf("%s append geometry: %v", budgetRed, err)
	}
	commitLast(t, realm, key, record)
	live := realm.total
	if live != geometryRecordCost {
		t.Fatalf("%s live realm charge=%d want=%d", budgetRed, live, geometryRecordCost)
	}
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", budgetRed, err)
	}

	reopened := reopenRealm(t, options)
	defer reopened.Close()
	if reopened.total != live {
		t.Fatalf("%s reopen forgot durable geometry charge: total=%d cost=%d want=%d",
			budgetRed, reopened.total, geometryRecordCost, live)
	}
}

// liveAndReopenedCharge runs one write programme, records the live realm charge,
// closes, reopens, and returns both charges. Every claim below is a comparison
// between what the realm billed while it was running and what it rebuilds from
// the file, because those two numbers being equal *is* the invariant.
func liveAndReopenedCharge(t *testing.T, options OpenOptions, programme func(realm *Realm)) (int64, int64) {
	t.Helper()
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", budgetRed, err)
	}
	programme(realm)
	live := realm.total
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", budgetRed, err)
	}
	reopened := reopenRealm(t, options)
	rebuilt := reopened.total
	if err := reopened.Close(); err != nil {
		t.Fatalf("%s reopened close: %v", budgetRed, err)
	}
	return live, rebuilt
}

func TestInterleavedOutputAndGeometryChargeSurvivesReopen(t *testing.T) {
	options := journalOptions(t)
	key := journalKey("%1", "pane-inc-a")
	payloads := [][]byte{[]byte("alpha"), []byte("bravo-bravo"), []byte("c")}
	geometries := []Geometry{{Columns: 80, Rows: 30}, {Columns: 80, Rows: 44}}
	live, rebuilt := liveAndReopenedCharge(t, options, func(realm *Realm) {
		admitJournalPane(t, realm, key)
		for index := range payloads {
			record, err := realm.Append(key, payloads[index])
			if err != nil {
				t.Fatalf("%s append output %d: %v", budgetRed, index, err)
			}
			commitLast(t, realm, key, record)
			if index < len(geometries) {
				geometry, err := realm.AppendGeometry(key, geometries[index])
				if err != nil {
					t.Fatalf("%s append geometry %d: %v", budgetRed, index, err)
				}
				commitLast(t, realm, key, geometry)
			}
		}
	})
	// The charge is output payload plus one fixed reservation per geometry
	// record — not the file size, and not the payload alone.
	var payloadBytes int64
	for _, payload := range payloads {
		payloadBytes += int64(len(payload))
	}
	want := payloadBytes + geometryRecordCost*int64(len(geometries))
	if live != want {
		t.Fatalf("%s live charge=%d want=%d (payload=%d geometry=%d)", budgetRed, live, want, payloadBytes, len(geometries))
	}
	if rebuilt != live {
		t.Fatalf("%s interleaved charge changed across reopen: live=%d rebuilt=%d", budgetRed, live, rebuilt)
	}
}

func TestRepeatedReopenCannotMintCapacity(t *testing.T) {
	options := journalOptions(t)
	key := journalKey("%1", "pane-inc-a")
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", budgetRed, err)
	}
	admitJournalPane(t, realm, key)
	if _, err := realm.Append(key, []byte("seed")); err != nil {
		t.Fatalf("%s append seed: %v", budgetRed, err)
	}
	for iteration := 0; iteration < 4; iteration++ {
		record, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: 24 + iteration})
		if err != nil {
			t.Fatalf("%s append geometry %d: %v", budgetRed, iteration, err)
		}
		commitLast(t, realm, key, record)
	}
	live := realm.total
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", budgetRed, err)
	}

	// Restarting is the attack: if each reopen forgets part of the charge, a
	// long-lived realm can be walked past its cap one restart at a time.
	current := options
	for restart := 0; restart < 5; restart++ {
		reopened := reopenRealm(t, current)
		if reopened.total != live {
			t.Fatalf("%s restart %d changed the realm charge: total=%d want=%d", budgetRed, restart, reopened.total, live)
		}
		if err := reopened.Close(); err != nil {
			t.Fatalf("%s restart %d close: %v", budgetRed, restart, err)
		}
		current.BrokerIncarnation = current.BrokerIncarnation + "-again"
	}
}

func TestPaneAndRealmCapsRemainEnforcedAfterReopen(t *testing.T) {
	options := journalOptions(t)
	// Room for a handful of geometry records and nothing more.
	options.PaneCapBytes = 8 * geometryRecordCost
	options.RealmCapBytes = 3 * geometryRecordCost
	key := journalKey("%1", "pane-inc-a")

	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", budgetRed, err)
	}
	admitJournalPane(t, realm, key)
	accepted := 0
	for iteration := 0; iteration < 8; iteration++ {
		record, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: 24 + iteration})
		if errors.Is(err, ErrQuota) {
			break
		}
		if err != nil {
			t.Fatalf("%s append geometry %d: %v", budgetRed, iteration, err)
		}
		commitLast(t, realm, key, record)
		accepted++
	}
	if accepted == 0 || accepted >= 8 {
		t.Fatalf("%s the realm cap did not bound geometry records live: accepted=%d", budgetRed, accepted)
	}
	live := realm.total
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", budgetRed, err)
	}

	// The cap has to mean the same thing after a restart. A reopened realm that
	// under-counts would accept records the running realm just refused.
	reopened := reopenRealm(t, options)
	defer reopened.Close()
	if reopened.total != live {
		t.Fatalf("%s reopen changed the charge under a cap: total=%d want=%d", budgetRed, reopened.total, live)
	}
	if remaining := reopened.options.RealmCapBytes - reopened.total; remaining >= geometryRecordCost {
		t.Fatalf("%s a reopened realm at its cap still has room for a geometry record: remaining=%d", budgetRed, remaining)
	}
	if remaining := reopened.options.PaneCapBytes - reopened.CommittedOffset(key); remaining <= 0 {
		t.Fatalf("%s pane accounting was destroyed by reopen: remaining=%d", budgetRed, remaining)
	}
}

func TestUncommittedGeometryChargeMatchesLiveAccountingAfterCrash(t *testing.T) {
	options := journalOptions(t)
	key := journalKey("%1", "pane-inc-a")
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", budgetRed, err)
	}
	admitJournalPane(t, realm, key)
	committed, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: 30})
	if err != nil {
		t.Fatalf("%s append committed geometry: %v", budgetRed, err)
	}
	commitLast(t, realm, key, committed)
	// Appended and paid for, then the process dies before the commit record is
	// written. The bytes are on disk either way, so the charge must survive.
	if _, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: 55}); err != nil {
		t.Fatalf("%s append uncommitted geometry: %v", budgetRed, err)
	}
	live := realm.total
	if live != 2*geometryRecordCost {
		t.Fatalf("%s live charge=%d want=%d", budgetRed, live, 2*geometryRecordCost)
	}
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", budgetRed, err)
	}

	reopened := reopenRealm(t, options)
	defer reopened.Close()
	if reopened.total != live {
		t.Fatalf("%s a crash discounted an appended geometry record: total=%d want=%d", budgetRed, reopened.total, live)
	}
	// The uncommitted record is still invisible to replay: paying for bytes is
	// not the same as trusting them.
	events, err := reopened.ReadCommittedEvents(key)
	if err != nil {
		t.Fatalf("%s read committed events: %v", budgetRed, err)
	}
	if len(events) != 1 || events[0].Kind != RecordGeometry || events[0].Geometry != (Geometry{Columns: 80, Rows: 30}) {
		t.Fatalf("%s uncommitted geometry became visible: %+v", budgetRed, events)
	}
}

func TestOutputOnlyAndByteOnlyAccountingIsUnchanged(t *testing.T) {
	t.Run("PUJ2 output only", func(t *testing.T) {
		options := journalOptions(t)
		key := journalKey("%1", "pane-inc-a")
		payloads := [][]byte{[]byte("first"), []byte("second-second")}
		live, rebuilt := liveAndReopenedCharge(t, options, func(realm *Realm) {
			admitJournalPane(t, realm, key)
			for index := range payloads {
				record, err := realm.Append(key, payloads[index])
				if err != nil {
					t.Fatalf("%s append %d: %v", budgetRed, index, err)
				}
				commitLast(t, realm, key, record)
			}
		})
		var want int64
		for _, payload := range payloads {
			want += int64(len(payload))
		}
		// A journal with no geometry must charge exactly the payload bytes it
		// always charged: the framing and the header are still not billed.
		if live != want || rebuilt != want {
			t.Fatalf("%s output-only charge live=%d rebuilt=%d want=%d", budgetRed, live, rebuilt, want)
		}
	})

	t.Run("byte-only generation", func(t *testing.T) {
		options := journalOptions(t)
		key := journalKey("%1", "pane-inc-a")
		payload := []byte("byte-only-generation")
		live, rebuilt := liveAndReopenedCharge(t, options, func(realm *Realm) {
			// No AdmitPane: a byte-only generation, which can never hold a
			// geometry record and must therefore be charged exactly as before.
			record, err := realm.Append(key, payload)
			if err != nil {
				t.Fatalf("%s append: %v", budgetRed, err)
			}
			commitLast(t, realm, key, record)
		})
		if live != int64(len(payload)) || rebuilt != int64(len(payload)) {
			t.Fatalf("%s byte-only charge live=%d rebuilt=%d want=%d", budgetRed, live, rebuilt, len(payload))
		}
	})

	t.Run("PUJ1 fails closed and charges nothing", func(t *testing.T) {
		options := journalOptions(t)
		key := journalKey("%1", "pane-inc-a")
		realm, err := OpenRealm(options)
		if err != nil {
			t.Fatalf("%s open: %v", budgetRed, err)
		}
		admitJournalPane(t, realm, key)
		record, err := realm.Append(key, []byte("legacy"))
		if err != nil {
			t.Fatalf("%s append: %v", budgetRed, err)
		}
		commitLast(t, realm, key, record)
		if err := realm.Close(); err != nil {
			t.Fatalf("%s close: %v", budgetRed, err)
		}
		contents, err := os.ReadFile(journalPathFor(t, options, key))
		if err != nil {
			t.Fatalf("%s read journal: %v", budgetRed, err)
		}
		downgraded := append([]byte(nil), contents...)
		downgraded[3] = '1'
		fresh := freshRuntime(t, options)
		writeJournalCopy(t, fresh, key, downgraded)
		// A PUJ1 file is unframable, so it cannot be attributed to a pane at all
		// and the realm refuses to open. Either way nothing is charged for it,
		// and this correction must not have changed that.
		reopened, err := OpenRealm(fresh)
		if err == nil {
			defer reopened.Close()
			if reopened.total != 0 {
				t.Fatalf("%s a PUJ1 generation was charged: total=%d", budgetRed, reopened.total)
			}
			if reopened.UnifiedEligible(key) {
				t.Fatalf("%s a PUJ1 generation claimed unified authority", budgetRed)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// ISSUE25 B1-F1 — a torn commit must not discard an already-validated geometry
// append's charge.
//
// The B1 correction rebuilt the realm charge from a count accumulated while
// parsing, but published that count only after the whole file parsed. A geometry
// append can be complete and fully validated — type, dimensions, sequence and
// hash all checked, and 128 bytes already billed live — and still lose its
// charge because the *next* frame, its commit record, was torn by a crash. The
// generation correctly fails replay closed; the durable bytes it left behind do
// not disappear with it, and neither may the charge.
//
// This is not the ambiguous short-append tail. Nothing about the append is in
// doubt at the moment it validates.
// ---------------------------------------------------------------------------

const tornCommitRed = "ISSUE25/PUJ2/CANDIDATE_RED/TORN_COMMIT_GEOMETRY_BUDGET"

// tornGeometryGeneration writes one admitted pane holding exactly one validated
// geometry append, then truncates its commit frame ten bytes in — the shape a
// crash between the commit write and its completion leaves on disk. It returns
// the truncated journal bytes and the live charge the realm took for them.
func tornGeometryGeneration(t *testing.T, pane, incarnation string, rows int) (PaneKey, []byte, int64) {
	t.Helper()
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", tornCommitRed, err)
	}
	key := journalKey(pane, incarnation)
	admitJournalPane(t, realm, key)
	record, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: rows})
	if err != nil {
		t.Fatalf("%s append geometry: %v", tornCommitRed, err)
	}
	live := realm.total
	if live != geometryRecordCost {
		t.Fatalf("%s live charge for one geometry append=%d want=%d", tornCommitRed, live, geometryRecordCost)
	}
	commitLast(t, realm, key, record)
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", tornCommitRed, err)
	}
	contents, err := os.ReadFile(journalPathFor(t, options, key))
	if err != nil {
		t.Fatalf("%s read journal: %v", tornCommitRed, err)
	}
	frames := puj2Frames(t, contents)
	if len(frames) != 2 || frames[0].commit || !frames[1].commit {
		t.Fatalf("%s geometry-only journal frames=%+v", tornCommitRed, frames)
	}
	// Ten bytes into the commit frame: past the marker and the kind, short of a
	// complete record. The append before it is untouched and complete.
	torn := append([]byte(nil), contents[:frames[1].offset+10]...)
	return key, torn, live
}

func TestTornGeometryCommitCannotMintCapacity(t *testing.T) {
	first, firstBytes, firstLive := tornGeometryGeneration(t, "%1", "pane-inc-a", 30)
	second, secondBytes, secondLive := tornGeometryGeneration(t, "%2", "pane-inc-b", 44)
	want := firstLive + secondLive
	if want != 2*geometryRecordCost {
		t.Fatalf("%s combined live charge=%d want=%d", tornCommitRed, want, 2*geometryRecordCost)
	}

	// Both torn generations land in one realm. Repeating this must not
	// hand back capacity for durable bytes
	// that are still on disk.
	options := journalOptions(t)
	fresh := freshRuntime(t, options)
	writeJournalCopy(t, fresh, first, firstBytes)
	writeJournalCopy(t, fresh, second, secondBytes)
	reopened, err := OpenRealm(fresh)
	if err != nil {
		t.Fatalf("%s scanner did not classify realm: %v", tornCommitRed, err)
	}
	defer reopened.Close()
	if reopened.total != want {
		t.Fatalf("%s two torn commits minted capacity: total=%d want=%d", tornCommitRed, reopened.total, want)
	}

	// Charging for the bytes is not trusting them: both generations must still
	// fail replay closed.
	for _, key := range []PaneKey{first, second} {
		if reopened.UnifiedEligible(key) {
			t.Fatalf("%s a torn generation claimed unified authority: %+v", tornCommitRed, key)
		}
		if reopened.Reason(key) != ReasonCorruptJournal {
			t.Fatalf("%s torn generation reason=%v want=%v", tornCommitRed, reopened.Reason(key), ReasonCorruptJournal)
		}
		if _, err := reopened.ReadCommittedEvents(key); err == nil {
			t.Fatalf("%s a torn generation exposed committed events", tornCommitRed)
		}
		if _, err := reopened.ReadCommitted(key); !errors.Is(err, ErrCorruptJournal) {
			t.Fatalf("%s a torn generation did not fail closed: %v", tornCommitRed, err)
		}
	}
}

func TestTornGeometryCommitChargeSurvivesRepeatedReopenAndHoldsTheCap(t *testing.T) {
	first, firstBytes, _ := tornGeometryGeneration(t, "%1", "pane-inc-a", 30)
	second, secondBytes, _ := tornGeometryGeneration(t, "%2", "pane-inc-b", 44)
	want := 2 * geometryRecordCost

	options := journalOptions(t)
	// A cap with room for exactly the two torn generations and nothing more. If a
	// restart forgot either charge, the realm would accept a third generation it
	// has no durable room for.
	options.RealmCapBytes = want
	options.PaneCapBytes = 8 * geometryRecordCost
	fresh := freshRuntime(t, options)
	fresh.RealmCapBytes = options.RealmCapBytes
	fresh.PaneCapBytes = options.PaneCapBytes
	writeJournalCopy(t, fresh, first, firstBytes)
	writeJournalCopy(t, fresh, second, secondBytes)

	current := fresh
	for restart := 0; restart < 5; restart++ {
		reopened, err := OpenRealm(current)
		if err != nil {
			t.Fatalf("%s restart %d open: %v", tornCommitRed, restart, err)
		}
		if reopened.total != want {
			t.Fatalf("%s restart %d changed the torn charge: total=%d want=%d", tornCommitRed, restart, reopened.total, want)
		}
		// The realm is full: a fresh generation must be refused, not admitted on
		// capacity the torn journals are still occupying.
		third := journalKey("%3", "pane-inc-c")
		if err := reopened.AdmitPane(third, Geometry{Columns: 80, Rows: 24}); err == nil {
			if _, err := reopened.AppendGeometry(third, Geometry{Columns: 80, Rows: 32}); !errors.Is(err, ErrQuota) {
				t.Fatalf("%s restart %d admitted a geometry record past a full realm: %v", tornCommitRed, restart, err)
			}
		}
		if err := reopened.Close(); err != nil {
			t.Fatalf("%s restart %d close: %v", tornCommitRed, restart, err)
		}
		current.BrokerIncarnation = current.BrokerIncarnation + "-again"
	}
}

func TestTornCommitAfterOutputAndGeometryChargesEveryValidatedAppend(t *testing.T) {
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("%s open: %v", tornCommitRed, err)
	}
	key := journalKey("%1", "pane-inc-a")
	admitJournalPane(t, realm, key)
	payload := []byte("output-before-geometry")
	output, err := realm.Append(key, payload)
	if err != nil {
		t.Fatalf("%s append output: %v", tornCommitRed, err)
	}
	commitLast(t, realm, key, output)
	for _, rows := range []int{30, 44} {
		record, err := realm.AppendGeometry(key, Geometry{Columns: 80, Rows: rows})
		if err != nil {
			t.Fatalf("%s append geometry %d: %v", tornCommitRed, rows, err)
		}
		commitLast(t, realm, key, record)
	}
	live := realm.total
	if live != int64(len(payload))+2*geometryRecordCost {
		t.Fatalf("%s live charge=%d want=%d", tornCommitRed, live, int64(len(payload))+2*geometryRecordCost)
	}
	if err := realm.Close(); err != nil {
		t.Fatalf("%s close: %v", tornCommitRed, err)
	}
	contents, err := os.ReadFile(journalPathFor(t, options, key))
	if err != nil {
		t.Fatalf("%s read journal: %v", tornCommitRed, err)
	}
	frames := puj2Frames(t, contents)
	if len(frames) != 6 {
		t.Fatalf("%s frames=%d want 6", tornCommitRed, len(frames))
	}
	torn := append([]byte(nil), contents[:frames[5].offset+10]...)

	fresh := freshRuntime(t, options)
	writeJournalCopy(t, fresh, key, torn)
	reopened, err := OpenRealm(fresh)
	if err != nil {
		t.Fatalf("%s scanner did not classify realm: %v", tornCommitRed, err)
	}
	defer reopened.Close()
	// Both geometry appends validated before the tear, so both are charged. The
	// output payload is not: pane.end is only meaningful for a journal that
	// parsed, and this correction deliberately does not broaden physical-file
	// accounting to cover a generation that failed.
	if reopened.total != 2*geometryRecordCost {
		t.Fatalf("%s torn interleaved charge=%d want=%d", tornCommitRed, reopened.total, 2*geometryRecordCost)
	}
	if reopened.UnifiedEligible(key) || reopened.Reason(key) != ReasonCorruptJournal {
		t.Fatalf("%s torn interleaved generation was not fail-closed: eligible=%v reason=%v",
			tornCommitRed, reopened.UnifiedEligible(key), reopened.Reason(key))
	}
}
