package unifiedjournal

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

type retentionFileFact struct {
	size   int64
	blocks int64
	inode  uint64
	digest [sha256.Size]byte
}

func retentionPaneKey(index int) PaneKey {
	return journalKey(fmt.Sprintf("retention-pane-%02d", index), fmt.Sprintf("retention-incarnation-%02d", index))
}

func inspectRetentionFile(t *testing.T, path string) retentionFileFact {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open journal fact %q: %v", path, err)
	}
	defer file.Close()

	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		t.Fatalf("hash journal fact %q: %v", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat journal fact %q: %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("journal fact %q has no unix stat", path)
	}
	var sum [sha256.Size]byte
	copy(sum[:], digest.Sum(nil))
	return retentionFileFact{size: info.Size(), blocks: stat.Blocks, inode: stat.Ino, digest: sum}
}

func requireRetentionPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied pane journal path exists or cannot be classified: path=%q err=%v", path, err)
	}
}

func TestRetentionDefaultCompleteSlotAdmission(t *testing.T) {
	options := journalOptions(t)
	realm, err := OpenRealm(options)
	if err != nil {
		t.Fatalf("open realm: %v", err)
	}
	defer realm.Close()

	// The ninth configured slot is held for a transactional rotation swap;
	// ordinary implicit admissions may consume the other eight.
	keys := make([]PaneKey, 8)
	paths := make([]string, len(keys))
	for index := range keys {
		keys[index] = retentionPaneKey(index)
		paths[index] = storagePathsForTest(options, keys[index]).Journal
		if _, err := realm.Append(keys[index], []byte{byte(index + 1)}); err != nil {
			t.Fatalf("default complete-slot pane %d was not admitted: %v", index, err)
		}
	}

	before := make([]retentionFileFact, len(paths))
	for index, path := range paths {
		before[index] = inspectRetentionFile(t, path)
	}

	deniedKey := retentionPaneKey(len(keys))
	deniedPath := storagePathsForTest(options, deniedKey).Journal
	if _, err := realm.Append(deniedKey, []byte("ninth-default-pane")); err == nil {
		t.Fatalf("ninth default pane append consumed the rotation reserve")
	} else if !errors.Is(err, ErrQuota) {
		t.Fatalf("ninth default pane returned %v, want ordinary quota error", err)
	}
	requireRetentionPathAbsent(t, deniedPath)

	for index, path := range paths {
		if after := inspectRetentionFile(t, path); after != before[index] {
			t.Fatalf("denied pane mutated prior journal %d: before=%+v after=%+v", index, before[index], after)
		}
	}

	fill := make([]byte, options.PaneCapBytes-1)
	for index, key := range keys {
		fill[0] = byte(index + 17)
		if _, err := realm.Append(key, fill); err != nil {
			t.Fatalf("promised default slot %d was not complete: %v", index, err)
		}
	}
}

func TestRetentionOrderedConcurrentAdmission(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)

	options := journalOptions(t)
	options.PaneCapBytes = 256 << 10
	options.RealmCapBytes = 2 * options.PaneCapBytes
	options.retention = &retentionPolicy{
		maximumCompletePanes: 8,
		completePaneUnits:    1,
		completeRealmUnits:   3,
	}

	firstKey := retentionPaneKey(20)
	writeReached := make(chan struct{})
	releaseWrite := make(chan struct{})
	armed := false
	var blockOnce sync.Once
	ops := realJournalOps()
	realWrite := ops.write
	ops.write = func(file *os.File, data []byte) (int, error) {
		if armed && file.Name() == paneFileName(firstKey) {
			blockOnce.Do(func() {
				close(writeReached)
				<-releaseWrite
			})
		}
		return realWrite(file, data)
	}

	realm, err := openRealm(options, ops)
	if err != nil {
		t.Fatalf("open coordinated realm: %v", err)
	}
	defer realm.Close()
	if _, err := realm.Append(firstKey, []byte("first-slot")); err != nil {
		t.Fatalf("admit first slot: %v", err)
	}
	armed = true

	blockedDone := make(chan error, 1)
	go func() {
		_, appendErr := realm.Append(firstKey, []byte("hold-admission-turn"))
		blockedDone <- appendErr
	}()
	select {
	case <-writeReached:
	case <-time.After(5 * time.Second):
		t.Fatal("existing-pane append did not reach the coordinated write")
	}

	const contenders = 4
	keys := make([]PaneKey, contenders)
	results := make([]chan error, contenders)
	for index := range keys {
		keys[index] = retentionPaneKey(21 + index)
		results[index] = make(chan error, 1)
		started := make(chan struct{})
		go func(index int, started chan<- struct{}) {
			close(started)
			_, appendErr := realm.Append(keys[index], []byte{byte(index + 1)})
			results[index] <- appendErr
		}(index, started)
		<-started
		runtime.Gosched()
	}

	close(releaseWrite)
	if err := <-blockedDone; err != nil {
		t.Fatalf("coordinating existing-pane append failed: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for index, result := range results {
		var appendErr error
		select {
		case appendErr = <-result:
		case <-deadline:
			t.Fatalf("ordered contender %d made no bounded progress", index)
		}
		path := storagePathsForTest(options, keys[index]).Journal
		if index == 0 {
			if appendErr != nil {
				t.Fatalf("first promised contender lost FIFO admission: %v", appendErr)
			}
			fact := inspectRetentionFile(t, path)
			if fact.size <= 0 || fact.blocks <= 0 || fact.inode == 0 {
				t.Fatalf("first promised contender has incomplete filesystem facts: %+v", fact)
			}
			continue
		}
		if !errors.Is(appendErr, ErrQuota) {
			t.Fatalf("ordered contender %d returned %v, want ordinary quota error", index, appendErr)
		}
		requireRetentionPathAbsent(t, path)
	}

	remaining := make([]byte, options.PaneCapBytes-1)
	if _, err := realm.Append(keys[0], remaining); err != nil {
		t.Fatalf("first promised contender lost its complete slot after denials: %v", err)
	}
}

func TestRetentionConfiguredCompleteSlotVectors(t *testing.T) {
	for _, vector := range []struct {
		name       string
		maximum    int64
		paneUnits  int64
		realmUnits int64
		requests   int
	}{
		{name: "realm-smaller-than-complete-slot", maximum: 8, paneUnits: 4, realmUnits: 2, requests: 2},
		{name: "one-complete-slot", maximum: 8, paneUnits: 4, realmUnits: 4, requests: 3},
		{name: "three-complete-slots", maximum: 8, paneUnits: 4, realmUnits: 12, requests: 5},
		{name: "configured-ceiling", maximum: 6, paneUnits: 4, realmUnits: 48, requests: 8},
		{name: "default-ceiling", maximum: 8, paneUnits: 4, realmUnits: 48, requests: 10},
	} {
		t.Run(vector.name, func(t *testing.T) {
			options := journalOptions(t)
			options.Realm = "retention-vector-" + vector.name
			options.PaneCapBytes = 4096

			wantAdmitted := int(vector.realmUnits / vector.paneUnits)
			if int64(wantAdmitted) > vector.maximum {
				wantAdmitted = int(vector.maximum)
			}
			if wantAdmitted > 0 {
				wantAdmitted-- // preserve one total slot for rotation
			}
			options.RealmCapBytes = int64(wantAdmitted) * options.PaneCapBytes
			if options.RealmCapBytes == 0 {
				options.RealmCapBytes = options.PaneCapBytes
			}
			options.retention = &retentionPolicy{
				maximumCompletePanes: vector.maximum,
				completePaneUnits:    vector.paneUnits,
				completeRealmUnits:   vector.realmUnits,
			}

			realm, err := OpenRealm(options)
			if err != nil {
				t.Fatalf("open configured realm: %v", err)
			}
			defer realm.Close()

			var admittedKeys []PaneKey
			var admittedPaths []string
			for index := 0; index < vector.requests; index++ {
				key := journalKey(fmt.Sprintf("vector-pane-%02d", index), fmt.Sprintf("vector-incarnation-%02d", index))
				path := storagePathsForTest(options, key).Journal
				_, appendErr := realm.Append(key, []byte{byte(index + 1)})
				if index < wantAdmitted {
					if appendErr != nil {
						t.Fatalf("configured request %d was not admitted: %v", index, appendErr)
					}
					fact := inspectRetentionFile(t, path)
					if fact.size <= 0 || fact.blocks <= 0 || fact.inode == 0 {
						t.Fatalf("configured request %d has incomplete filesystem facts: %+v", index, fact)
					}
					admittedKeys = append(admittedKeys, key)
					admittedPaths = append(admittedPaths, path)
					continue
				}

				before := make([]retentionFileFact, len(admittedPaths))
				for admittedIndex, admittedPath := range admittedPaths {
					before[admittedIndex] = inspectRetentionFile(t, admittedPath)
				}
				if !errors.Is(appendErr, ErrQuota) {
					t.Fatalf("configured request %d returned %v, want ordinary quota error", index, appendErr)
				}
				requireRetentionPathAbsent(t, path)
				for admittedIndex, admittedPath := range admittedPaths {
					if after := inspectRetentionFile(t, admittedPath); after != before[admittedIndex] {
						t.Fatalf("configured rejection %d mutated admitted journal %d: before=%+v after=%+v", index, admittedIndex, before[admittedIndex], after)
					}
				}
			}

			remaining := make([]byte, options.PaneCapBytes-1)
			for index, key := range admittedKeys {
				remaining[0] = byte(index + 33)
				if _, err := realm.Append(key, remaining); err != nil {
					t.Fatalf("configured promised slot %d was not complete: %v", index, err)
				}
			}
		})
	}
}
