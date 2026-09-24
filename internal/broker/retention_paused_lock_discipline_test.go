package broker

import (
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/unifiedjournal"
)

// A geometry owner's mu-guarded read of pane.paused races a bare manager
// write in processBoundary's pause_end case. The pane runtime is
// manager-owned, but paused is observed from outside the manager under
// runtime.mu, so the manager's writes must be published under that mutex.
// The race is a scheduling window — between the write and the manager's
// next runtime.mu acquisition — that a loaded host widens enough for the
// settle poll to land in; on a quiet host the stress test passes hundreds
// of times. This pin removes the timing from the question: the manager is
// parked inside that window through the runtime's hook seam, and the
// mu-guarded read is performed there. With the write under runtime.mu the
// read is ordered after it; with a bare write the race detector reports it
// every time. The verdict is the race detector's, so the pin runs only in a
// race-instrumented binary.

// pausedDisciplineEffects is the geometry barrier harness's effects with a
// hook installed, so the manager can be parked at the two pause writes.
type pausedDisciplineEffects struct {
	geometryBarrierEffects
	hook func(string, unifiedjournal.PaneKey)
}

func (effects pausedDisciplineEffects) retentionTrial() retentionTrialOptions {
	options := effects.geometryBarrierEffects.retentionTrial()
	options.hook = effects.hook
	return options
}

func TestRetentionPausedIsPublishedUnderRuntimeMu(t *testing.T) {
	if !raceDetectorEnabled {
		t.Skip("verdict is the race detector's; run with -race")
	}
	realm := openRetentionRealm(t, "paused-lock-discipline")
	key := journalKey(retentionWitness("%0", "paused-discipline-incarnation"))
	if err := realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("admit pane: %v", err)
	}
	// Two hook points bracket each paused write on the manager. The one
	// before the write announces it; the one after parks the manager until
	// the observer has read. Neither channel orders the write relative to
	// the observer's reads — the announcement precedes the write, and the
	// release follows the reads — so only runtime.mu can, which is the
	// discipline under test: an unpublished write is unordered with reads
	// on either side of it, and the race detector reports it as such.
	release := map[string]chan struct{}{
		"pause_start": make(chan struct{}),
		"pause_end":   make(chan struct{}),
	}
	entered := make(chan string, 2)
	var journalMu sync.Mutex
	down := &geometryBarrierDownstream{delivered: make(chan struct{}, 64)}
	effects := pausedDisciplineEffects{
		geometryBarrierEffects: geometryBarrierEffects{realm: realm, journalMu: &journalMu, downstream: down},
		hook: func(point string, _ unifiedjournal.PaneKey) {
			switch point {
			case "before_pause_start_pause":
				entered <- "pause_start"
			case "before_pause_end_unpause":
				entered <- "pause_end"
			case "after_pause_start_pause":
				<-release["pause_start"]
			case "after_pause_end_unpause":
				<-release["pause_end"]
			}
		},
	}
	runtime := newRetentionTrialRuntime(effects)
	if runtime == nil {
		t.Fatal("retention runtime unavailable")
	}
	t.Cleanup(func() { _ = runtime.Close() })

	// The pane runtime must exist before the observer looks it up: the
	// first command creates it on the manager, and the completed boundary
	// orders that creation before every read below.
	if err := runtime.WritePane(key, []byte("pre;")); err != nil {
		t.Fatalf("pre write: %v", err)
	}
	if err := runtime.Boundary(key, "settle"); err != nil {
		t.Fatalf("settle boundary: %v", err)
	}
	observe := func() bool {
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		pane := runtime.panes[key]
		return pane != nil && pane.paused
	}

	for _, reason := range []string{"pause_start", "pause_end"} {
		reason := reason
		result := make(chan error, 1)
		go func() { result <- runtime.Boundary(key, reason) }()
		select {
		case point := <-entered:
			if point != reason {
				t.Fatalf("%s: the manager announced %s", reason, point)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the manager never reached the pause write", reason)
		}
		// The observer's reads, exactly where the settle poll's read lands
		// on a loaded host: around the manager's write and before it has
		// published anything else under runtime.mu. Whether a read lands
		// before or after the write, only a write made under runtime.mu is
		// ordered with it.
		for i := 0; i < 3; i++ {
			observe()
			time.Sleep(time.Millisecond)
		}
		close(release[reason])
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("%s boundary: %v", reason, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s boundary did not complete", reason)
		}
		want := reason == "pause_start"
		if got := observe(); got != want {
			t.Fatalf("after %s paused=%v want %v", reason, got, want)
		}
	}
	down.awaitOutput(t, "released output", "pre;")
}
