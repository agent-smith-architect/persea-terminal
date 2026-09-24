package terminal_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	terminal "persea-terminal/internal/terminal"
)

type journalOwnedTransport struct{ *memoryTransport }

func (*journalOwnedTransport) OwnsTerminalOutput() {}

type countingCutClock struct{ created atomic.Int64 }

func (*countingCutClock) Now() time.Time { return time.Now() }
func (c *countingCutClock) NewTimer(d time.Duration) terminal.Timer {
	c.created.Add(1)
	return terminal.RealClock{}.NewTimer(d)
}

func TestRecordingOutputDoesNotStartPresentationCutTimers(t *testing.T) {
	tx := &fakeTransaction{}
	source, _ := sourceFor(t, tx)
	wire := newMemoryTransport()
	clock := &countingCutClock{}
	cfg := config()
	cfg.Clock = clock
	epoch, err := terminal.NewEpoch(context.Background(), source, 9, &journalOwnedTransport{wire}, newRecordingPTY(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer epoch.Finalize(context.Background())
	tx.onCut = func(req terminal.TransactionRequest) error { return epoch.PTYBytes(marker(req)) }
	startLive(t, epoch, wire)
	before := clock.created.Load()
	if err := epoch.PTYBytes([]byte("ordinary pane output")); err != nil {
		t.Fatal(err)
	}
	// PTYBytes waits for the scheduler's acknowledgement, so timer creation is
	// observable here without sleeping or asking a timer goroutine to win a race.
	if after := clock.created.Load(); after != before {
		t.Fatalf("journal-owned output armed a redundant presentation cut: timers %d -> %d", before, after)
	}
}
