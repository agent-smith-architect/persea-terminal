package unifiedjournal

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func TestVerificationProgressRemainsReadableDuringReadback(t *testing.T) {
	realm, err := OpenRealm(journalOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer realm.Close()
	key := journalKey("%progress", "synthetic-progress")
	admitJournalPane(t, realm, key)
	record, err := realm.Append(key, []byte("synthetic output"))
	if err != nil {
		t.Fatal(err)
	}
	if err := realm.Sync(key); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	realRead := realm.ops.readAt
	var first sync.Once
	realm.ops.readAt = func(file *os.File, data []byte, offset int64) (int, error) {
		first.Do(func() { close(entered); <-release })
		return realRead(file, data, offset)
	}
	done := make(chan error, 1)
	go func() { done <- realm.AdvanceCommitted(key, record) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("readback did not start")
	}
	// A deadline here tests the public read boundary, not scheduler performance.
	sampled := make(chan VerificationProgress, 1)
	go func() { sampled <- realm.VerificationProgress() }()
	var sample VerificationProgress
	select {
	case sample = <-sampled:
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("progress read waited for storage")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if sample.InFlight != 1 || sample.Attempts != 1 || sample.FinishedUnixNano != 0 {
		t.Fatalf("blocked readback not visible: %+v", sample)
	}
	finished := realm.VerificationProgress()
	if finished.InFlight != 0 || finished.DecodedRecords != 1 || finished.ReadBytes == 0 || finished.FinishedUnixNano == 0 {
		t.Fatalf("verified readback not visible: %+v", finished)
	}
	record, err = realm.Append(key, []byte("more synthetic output"))
	if err != nil {
		t.Fatal(err)
	}
	if err := realm.Sync(key); err != nil {
		t.Fatal(err)
	}
	realm.ops.readAt = func(*os.File, []byte, int64) (int, error) { return 0, errors.New("synthetic read failure") }
	if err := realm.AdvanceCommitted(key, record); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("failure = %v", err)
	}
	failed := realm.VerificationProgress()
	if failed.Failures != 1 || failed.Attempts != 2 || failed.InFlight != 0 {
		t.Fatalf("failure not recorded: %+v", failed)
	}
}
