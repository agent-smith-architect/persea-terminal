package broker

import (
	"sync"
	"time"

	"persea-terminal/internal/unifiedjournal"
)

// Input freshness keeps a Control page from typing against a screen that is
// far behind the session.
//
// A view that falls behind is caught up from the journal rather than ended
// (see catchUp), so under output faster than its link a page can keep Control
// while what it shows is a minute or more old. Input is therefore accepted only
// from a page that has consumed every event committed at least
// inputFreshnessWindow ago. The broker learns what the page consumed from
// receipts: the front door forwards the page's cumulative flow acknowledgement
// (the count of attachment frames written into its terminal), and this writer
// maps frame counts to the journal sequence those frames completed. What was
// committed when comes from a per-generation frontier clock kept at
// publication. A quiet session whose output the page has consumed is always
// fresh; a page that has not yet consumed an old snapshot is not.
//
// Refused input is dropped, never queued, and the page is told in-band
// (input_paused). Once refused, input stays refused until the page is fresh
// again and has sent no input for inputResumeQuiet, so a command typed while
// the page was behind can never arrive with its beginning missing. The Control
// lease is not touched: output keeps flowing and the page keeps catching up.
const (
	inputFreshnessWindow = 10 * time.Second
	inputResumeQuiet     = 2 * time.Second
	// frontierClockMarks bounds a generation's frontier clock. Marks are
	// window/frontierClockResolution apart at most, so the newest mark at least
	// one window old always survives pruning; the bound is only a backstop.
	frontierClockResolution = 64
	frontierClockMarks      = frontierClockResolution + 8
	// deliveredMarks bounds the frame-to-sequence marks one writer keeps for
	// frames not yet receipted. When it is full the oldest mark is dropped: a
	// receipt covering only that mark then maps to the mark before it, which
	// under-reports consumption and can only pause input, never admit it.
	deliveredMarks = 128
)

type frontierMark struct {
	at       time.Time
	sequence int64
}

// frontierClock records which sequence of one generation had been published
// by when, at window/frontierClockResolution resolution. It is guarded by the
// provider's subscriberMu.
type frontierClock struct {
	marks []frontierMark
}

func (clock *frontierClock) observe(now time.Time, sequence int64, window time.Duration) {
	resolution := window / frontierClockResolution
	if n := len(clock.marks); n > 0 && now.Sub(clock.marks[n-1].at) < resolution {
		if sequence > clock.marks[n-1].sequence {
			clock.marks[n-1].sequence = sequence
		}
	} else {
		clock.marks = append(clock.marks, frontierMark{at: now, sequence: sequence})
	}
	// Only the newest mark at least one window old can still answer a query;
	// every older one is dropped.
	cutoff := now.Add(-window)
	drop := 0
	for drop+1 < len(clock.marks) && !clock.marks[drop+1].at.After(cutoff) {
		drop++
	}
	if excess := len(clock.marks) - drop - frontierClockMarks; excess > 0 {
		drop += excess
	}
	if drop > 0 {
		clock.marks = append(clock.marks[:0], clock.marks[drop:]...)
	}
}

// at returns the last sequence published by instant, rounded up to the clock's
// resolution (which can only demand more consumption), or 0 when nothing had
// been published by then.
func (clock *frontierClock) at(instant time.Time) int64 {
	for index := len(clock.marks) - 1; index >= 0; index-- {
		if !clock.marks[index].at.After(instant) {
			return clock.marks[index].sequence
		}
	}
	return 0
}

type deliveredMark struct {
	frames   uint64
	sequence int64
}

// inputFreshness is one Control attachment's consumption and pause state. It
// has its own lock: the input path must never wait behind writer.mu, which an
// output write parked on a slow peer can hold for a long time.
type inputFreshness struct {
	mu        sync.Mutex
	live      bool
	key       unifiedjournal.PaneKey
	marks     [deliveredMarks]deliveredMark
	head      int
	count     int
	receipted uint64
	consumed  int64
	paused    bool
	refusedAt time.Time
}

// start arms the gate once the initial backlog has been written; before that
// the epoch itself refuses input.
func (fresh *inputFreshness) start(key unifiedjournal.PaneKey) {
	fresh.mu.Lock()
	defer fresh.mu.Unlock()
	fresh.live, fresh.key = true, key
}

// delivered records that the first frames attachment frames complete every
// event up to sequence.
func (fresh *inputFreshness) delivered(frames uint64, sequence int64) {
	fresh.mu.Lock()
	defer fresh.mu.Unlock()
	if fresh.count > 0 {
		last := fresh.marks[(fresh.head+fresh.count-1)%deliveredMarks]
		if sequence <= last.sequence {
			return
		}
	}
	if sequence <= fresh.consumed {
		return
	}
	if fresh.count == deliveredMarks {
		fresh.head = (fresh.head + 1) % deliveredMarks
		fresh.count--
	}
	fresh.marks[(fresh.head+fresh.count)%deliveredMarks] = deliveredMark{frames: frames, sequence: sequence}
	fresh.count++
}

// receipt applies a cumulative count of attachment frames the page consumed.
// A count that does not advance, or that exceeds the frames written, is a
// protocol violation.
func (fresh *inputFreshness) receipt(frames, written uint64) bool {
	fresh.mu.Lock()
	defer fresh.mu.Unlock()
	if frames <= fresh.receipted || frames > written {
		return false
	}
	fresh.receipted = frames
	for fresh.count > 0 && fresh.marks[fresh.head].frames <= frames {
		fresh.consumed = fresh.marks[fresh.head].sequence
		fresh.head = (fresh.head + 1) % deliveredMarks
		fresh.count--
	}
	return true
}

// admit decides one input frame. frontier returns the generation's last
// sequence published by an instant. It reports whether the input is accepted
// and whether this refusal started a pause.
func (fresh *inputFreshness) admit(now time.Time, window, quiet time.Duration, frontier func(unifiedjournal.PaneKey, time.Time) int64) (accepted, paused bool) {
	fresh.mu.Lock()
	live, key, consumed := fresh.live, fresh.key, fresh.consumed
	fresh.mu.Unlock()
	if !live {
		return true, false
	}
	current := consumed >= frontier(key, now.Add(-window))
	fresh.mu.Lock()
	defer fresh.mu.Unlock()
	if fresh.paused {
		if current && now.Sub(fresh.refusedAt) >= quiet {
			fresh.paused = false
			return true, false
		}
		fresh.refusedAt = now
		return false, false
	}
	if !current {
		fresh.paused, fresh.refusedAt = true, now
		return false, true
	}
	return true, false
}

func (effects *UnifiedDevPaneEffects) freshnessClock() time.Time {
	if effects.freshnessNow != nil {
		return effects.freshnessNow()
	}
	return time.Now()
}

func (effects *UnifiedDevPaneEffects) inputWindow() time.Duration {
	if effects.inputFreshnessWindow > 0 {
		return effects.inputFreshnessWindow
	}
	return inputFreshnessWindow
}

func (effects *UnifiedDevPaneEffects) inputQuiet() time.Duration {
	if effects.inputResumeQuiet > 0 {
		return effects.inputResumeQuiet
	}
	return inputResumeQuiet
}

// observeFrontierLocked records a publication. Callers hold subscriberMu.
func (effects *UnifiedDevPaneEffects) observeFrontierLocked(key unifiedjournal.PaneKey, sequence int64) {
	if effects.frontierClocks == nil {
		effects.frontierClocks = make(map[unifiedjournal.PaneKey]*frontierClock)
	}
	clock := effects.frontierClocks[key]
	if clock == nil {
		clock = &frontierClock{}
		effects.frontierClocks[key] = clock
	}
	clock.observe(effects.freshnessClock(), sequence, effects.inputWindow())
}

// publishedBy returns the last sequence of key published by instant.
func (effects *UnifiedDevPaneEffects) publishedBy(key unifiedjournal.PaneKey, instant time.Time) int64 {
	effects.subscriberMu.Lock()
	defer effects.subscriberMu.Unlock()
	clock := effects.frontierClocks[key]
	if clock == nil {
		return 0
	}
	return clock.at(instant)
}

// forgetPublishedLocked drops a retired generation's publication record.
// Callers hold subscriberMu.
func (effects *UnifiedDevPaneEffects) forgetPublishedLocked(key unifiedjournal.PaneKey) {
	delete(effects.publishedSequence, key)
	delete(effects.frontierClocks, key)
}

// recordDelivered notes that every attachment frame written so far completes
// the events up to sequence. Frames written by other paths meanwhile (MODE,
// END) only make the mark later, never earlier than the event's own last frame.
func (writer *unifiedAttachmentFrameWriter) recordDelivered(sequence int64) {
	writer.fresh.delivered(writer.downstream.wire.attachmentFrames(), sequence)
}

// receipt applies a front-door consumption receipt.
func (writer *unifiedAttachmentFrameWriter) receipt(frames uint64) bool {
	return writer.fresh.receipt(frames, writer.downstream.wire.attachmentFrames())
}

// admitInput gates one input frame on the page's freshness.
func (writer *unifiedAttachmentFrameWriter) admitInput() (accepted, paused bool) {
	provider := writer.provider
	return writer.fresh.admit(provider.freshnessClock(), provider.inputWindow(), provider.inputQuiet(), provider.publishedBy)
}
