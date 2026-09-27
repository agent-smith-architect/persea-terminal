package broker

import (
	"testing"
	"time"

	"persea-terminal/internal/unifiedjournal"
)

func TestFrontierClockAnswersTheLastSequencePublishedByAnInstant(t *testing.T) {
	const window = 640 * time.Millisecond // 10 ms resolution
	base := time.Unix(1_000_000, 0)
	var clock frontierClock
	clock.observe(base, 5, window)
	clock.observe(base.Add(3*time.Millisecond), 6, window)
	clock.observe(base.Add(20*time.Millisecond), 9, window)
	for _, check := range []struct {
		at   time.Duration
		want int64
	}{
		{-time.Millisecond, 0},
		// A mark answers for its whole resolution bucket: rounding up can only
		// demand more consumption.
		{0, 6},
		{19 * time.Millisecond, 6},
		{20 * time.Millisecond, 9},
		{time.Hour, 9},
	} {
		if got := clock.at(base.Add(check.at)); got != check.want {
			t.Fatalf("at(%v)=%d, want %d", check.at, got, check.want)
		}
	}

	// Pruning keeps the newest mark at least one window old, so every query a
	// gate can make at or after the latest publication is still answered.
	later := base.Add(10 * time.Second)
	clock.observe(later, 20, window)
	if got := clock.at(later.Add(-window)); got != 9 {
		t.Fatalf("after pruning at(latest-window)=%d, want 9", got)
	}
	if len(clock.marks) != 2 {
		t.Fatalf("pruned clock holds %d marks, want 2", len(clock.marks))
	}
	// An instant older than every retained mark is answered with the oldest
	// retained one, never with "nothing was published".
	if got := clock.at(base.Add(10 * time.Millisecond)); got != 9 {
		t.Fatalf("after pruning at(before the oldest mark)=%d, want 9", got)
	}

	// A long, dense run stays bounded and keeps answering one window back.
	start := later.Add(time.Second)
	var last time.Time
	for i := 0; i < 20_000; i++ {
		last = start.Add(time.Duration(i) * time.Millisecond)
		clock.observe(last, int64(21+i), window)
		if len(clock.marks) > frontierClockMarks {
			t.Fatalf("clock grew to %d marks", len(clock.marks))
		}
	}
	// Exactly one window back the published sequence is 21+(n-1-640); the
	// answer may round up by at most one bucket, never down.
	want := int64(21 + 20_000 - 1 - 640)
	if got := clock.at(last.Add(-window)); got < want || got > want+10 {
		t.Fatalf("at(last-window)=%d, want within [%d, %d]", got, want, want+10)
	}
}

func TestInputFreshnessMapsReceiptsToCompletedSequences(t *testing.T) {
	var fresh inputFreshness
	fresh.start(unifiedjournal.PaneKey{})
	fresh.delivered(3, 10)
	fresh.delivered(5, 12)
	fresh.delivered(9, 20)
	fresh.delivered(9, 20) // repeated: ignored
	fresh.delivered(8, 15) // not beyond the last mark: ignored
	for _, bad := range []uint64{0, 10} {
		if fresh.receipt(bad, 9) {
			t.Fatalf("receipt %d of 9 written frames was accepted", bad)
		}
	}
	steps := []struct {
		frames uint64
		want   int64
	}{{2, 0}, {4, 10}, {5, 12}, {9, 20}}
	for _, step := range steps {
		if !fresh.receipt(step.frames, 9) {
			t.Fatalf("receipt %d refused", step.frames)
		}
		if fresh.consumed != step.want {
			t.Fatalf("after receipt %d consumed=%d, want %d", step.frames, fresh.consumed, step.want)
		}
	}
	for _, stale := range []uint64{9, 8} {
		if fresh.receipt(stale, 9) {
			t.Fatalf("a receipt that does not advance (%d) was accepted", stale)
		}
	}
}

// A page's receipt can arrive between a frame's write and the mark that frame
// completes; the mark must still advance consumption when it lands, or a quiet
// caught-up page stays paused with nothing left to acknowledge.
func TestInputFreshnessAppliesAReceiptThatOvertakesItsMark(t *testing.T) {
	var fresh inputFreshness
	fresh.start(unifiedjournal.PaneKey{})
	fresh.delivered(1, 3)
	if !fresh.receipt(2, 2) || fresh.consumed != 3 {
		t.Fatalf("consumed=%d after receipting past the only mark, want 3", fresh.consumed)
	}
	fresh.delivered(2, 7)
	if fresh.consumed != 7 || fresh.count != 0 {
		t.Fatalf("mark landing after its receipt: consumed=%d pending=%d, want 7 and 0", fresh.consumed, fresh.count)
	}
	// A mark beyond the receipt still waits for its own receipt.
	fresh.delivered(3, 9)
	if fresh.consumed != 7 || fresh.count != 1 {
		t.Fatalf("unreceipted mark: consumed=%d pending=%d, want 7 and 1", fresh.consumed, fresh.count)
	}
}

// A publication that prunes the clock between the gate's sampling of now and
// its lookup must not turn an old debt into none. The provider samples under
// the publication lock; the clock itself answers a pruned instant
// conservatively.
func TestInputFreshnessPruningCannotEraseAnOldDebt(t *testing.T) {
	const window = 10 * time.Second
	base := time.Unix(3_000_000, 0)
	var clock frontierClock
	clock.observe(base, 1, window)
	clock.observe(base.Add(200*time.Millisecond), 2, window)
	var fresh inputFreshness
	fresh.start(unifiedjournal.PaneKey{})
	cutoff := base.Add(100 * time.Millisecond)
	accepted, _ := fresh.admit(base.Add(window+100*time.Millisecond), 2*time.Second, func(unifiedjournal.PaneKey) (int64, bool) {
		clock.observe(base.Add(window+200*time.Millisecond), 3, window)
		return clock.at(cutoff), true
	})
	if accepted {
		t.Fatal("input admitted with nothing consumed after pruning dropped the mark its cutoff needed")
	}
}

// Only a generation a view subscribed to, and that has not retired since,
// has freshness authority: a subscribed generation with no publications owes
// nothing, and a key with no clock is refused however little it owes.
func TestInputFreshnessRequiresGenerationAuthority(t *testing.T) {
	var effects UnifiedDevPaneEffects
	key := unifiedjournal.PaneKey{ControlGeneration: 1}
	writer := &unifiedAttachmentFrameWriter{provider: &effects}
	writer.fresh.start(key)
	if accepted, paused := writer.admitInput(); accepted || !paused {
		t.Fatalf("unknown generation: accepted=%v paused=%v, want a pause", accepted, paused)
	}
	effects.subscriberMu.Lock()
	effects.trackFrontierLocked(key)
	effects.subscriberMu.Unlock()
	effects.inputResumeQuiet = time.Nanosecond
	current := time.Now().Add(time.Second)
	effects.freshnessNow = func() time.Time { return current }
	if accepted, _ := writer.admitInput(); !accepted {
		t.Fatal("a subscribed generation with no publications paused input")
	}
	effects.subscriberMu.Lock()
	effects.forgetPublishedLocked(key)
	effects.subscriberMu.Unlock()
	if accepted, _ := writer.admitInput(); accepted {
		t.Fatal("a retired generation admitted input")
	}
}

// With the mark ring full, the oldest mark is dropped: a receipt covering only
// it under-reports consumption rather than over-reporting it.
func TestInputFreshnessDropsTheOldestMarkConservatively(t *testing.T) {
	var fresh inputFreshness
	for i := uint64(1); i <= deliveredMarks+1; i++ {
		fresh.delivered(i, int64(i*10))
	}
	written := uint64(deliveredMarks + 1)
	if !fresh.receipt(1, written) || fresh.consumed != 0 {
		t.Fatalf("a receipt of the dropped mark reported consumed=%d, want 0", fresh.consumed)
	}
	if !fresh.receipt(2, written) || fresh.consumed != 20 {
		t.Fatalf("consumed=%d after the next receipt, want 20", fresh.consumed)
	}
	if !fresh.receipt(written, written) || fresh.consumed != int64(written*10) {
		t.Fatalf("consumed=%d after receipting everything, want %d", fresh.consumed, written*10)
	}
}

func TestInputFreshnessPausesUntilCaughtUpAndQuiet(t *testing.T) {
	const window, quiet = 10 * time.Second, 2 * time.Second
	base := time.Unix(2_000_000, 0)
	var published int64
	publishedAt := base
	frontier := func(_ unifiedjournal.PaneKey, instant time.Time) int64 {
		if instant.Before(publishedAt) {
			return 0
		}
		return published
	}
	var fresh inputFreshness
	admit := func(at time.Duration) (bool, bool) {
		now := base.Add(at)
		return fresh.admit(now, quiet, func(key unifiedjournal.PaneKey) (int64, bool) { return frontier(key, now.Add(-window)), true })
	}
	if accepted, _ := admit(time.Hour); !accepted {
		t.Fatal("input before the backlog completed was gated; the epoch owns that")
	}
	fresh.start(unifiedjournal.PaneKey{})
	published = 5
	if accepted, paused := admit(window - time.Millisecond); !accepted || paused {
		t.Fatal("output younger than the window paused input")
	}
	if accepted, paused := admit(window); accepted || !paused {
		t.Fatalf("unconsumed output one window old: accepted=%v paused=%v, want a new pause", accepted, paused)
	}
	if accepted, paused := admit(window + time.Second); accepted || paused {
		t.Fatalf("while paused: accepted=%v paused=%v, want refused without a new pause", accepted, paused)
	}
	fresh.delivered(3, 5)
	if !fresh.receipt(3, 3) {
		t.Fatal("receipt refused")
	}
	// Caught up, but typing has not been quiet: still refused, and the quiet
	// period restarts.
	if accepted, _ := admit(window + time.Second + quiet - time.Millisecond); accepted {
		t.Fatal("input inside the quiet period after a refusal was accepted")
	}
	resumed := window + time.Second + 2*quiet - time.Millisecond
	if accepted, _ := admit(resumed); !accepted {
		t.Fatal("a caught-up page stayed paused after a quiet period")
	}
	if accepted, _ := admit(resumed + time.Millisecond); !accepted {
		t.Fatal("input right after resuming was refused")
	}
}
