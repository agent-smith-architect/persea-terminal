package terminal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	terminal "persea-terminal/internal/terminal"
)

const reservedWirePrefix = "\x1b]52;;persea:v1:"

func newMarkerIngressEpoch(t *testing.T, kind terminal.CutKind, onCut func(*terminal.Epoch, terminal.TransactionRequest) error) (*terminal.Epoch, *fakeTransaction, *memoryTransport, *manualDeadlineClock) {
	t.Helper()
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire := newMemoryTransport()
	clock := newManualDeadlineClock()
	var epoch *terminal.Epoch
	tx.onCut = func(req terminal.TransactionRequest) error { return onCut(epoch, req) }
	var err error
	epoch, err = terminal.NewEpoch(context.Background(), source, 41, wire, newRecordingPTY(), terminal.Config{
		Clock: clock, QuietInterval: 5 * time.Millisecond, MaximumInterval: 10 * time.Millisecond, CutTimeout: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := epoch.Start(context.Background(), kind); err != nil {
		t.Fatal(err)
	}
	return epoch, tx, wire, clock
}

func preparedFrame(t *testing.T, wire *memoryTransport) terminal.Frame {
	t.Helper()
	frames := wire.wait(t, func(fs []terminal.Frame) bool {
		for _, frame := range fs {
			if frame.Type == terminal.FramePrepare {
				return true
			}
		}
		return false
	})
	for _, frame := range frames {
		if frame.Type == terminal.FramePrepare {
			return frame
		}
	}
	t.Fatal("missing PREPARE")
	return terminal.Frame{}
}

func markerBrowserFrame(kind terminal.FrameType, prepare terminal.Frame) terminal.Frame {
	return terminal.Frame{Version: terminal.ProtocolVersion, Type: kind, Source: prepare.Source, Epoch: prepare.Epoch, Cut: prepare.Cut}
}

func lastCutRequest(t *testing.T, tx *fakeTransaction) terminal.TransactionRequest {
	t.Helper()
	_, requests, _, _ := tx.snapshot()
	for i := len(requests) - 1; i >= 0; i-- {
		if requests[i].Action == terminal.ActionCut {
			return requests[i]
		}
	}
	t.Fatal("missing cut request")
	return terminal.TransactionRequest{}
}

func assertNoPrivateOrSuffix(t *testing.T, frames []terminal.Frame, private, suffix []byte) {
	t.Helper()
	for _, frame := range frames {
		for _, data := range [][]byte{frame.Data, frame.Replay} {
			if bytes.Contains(data, private) || bytes.Contains(data, suffix) {
				t.Fatalf("private marker or following suffix leaked in %#v", frame)
			}
		}
	}
}

func TestMarkerIngressRejectedSplitDuplicateAcrossCommit(t *testing.T) {
	epoch, tx, wire, _ := newMarkerIngressEpoch(t, terminal.CutInitial, func(epoch *terminal.Epoch, req terminal.TransactionRequest) error {
		return epoch.PTYBytes(append([]byte("redraw"), marker(req)...))
	})
	prepare := preparedFrame(t, wire)
	duplicate := marker(lastCutRequest(t, tx))
	split := 7
	if err := epoch.PTYBytes(duplicate[:split]); err != nil {
		t.Fatal(err)
	}
	if err := epoch.HandleFrame(markerBrowserFrame(terminal.FrameReady, prepare)); err != nil {
		t.Fatal(err)
	}
	suffix := []byte("ordinary-suffix")
	if err := epoch.PTYBytes(append(append([]byte(nil), duplicate[split:]...), suffix...)); err == nil {
		t.Fatal("split duplicate crossing COMMIT must fault")
	}
	<-epoch.Done()
	_ = epoch.Finalize(context.Background())
	assertNoPrivateOrSuffix(t, wire.snapshot(), duplicate, suffix)
}

func TestMarkerIngressDuplicateWhollyAfterCommit(t *testing.T) {
	epoch, tx, wire, _ := newMarkerIngressEpoch(t, terminal.CutInitial, func(epoch *terminal.Epoch, req terminal.TransactionRequest) error {
		return epoch.PTYBytes(marker(req))
	})
	prepare := preparedFrame(t, wire)
	if err := epoch.HandleFrame(markerBrowserFrame(terminal.FrameReady, prepare)); err != nil {
		t.Fatal(err)
	}
	duplicate := marker(lastCutRequest(t, tx))
	suffix := []byte("after-private")
	if err := epoch.PTYBytes(append(append([]byte(nil), duplicate...), suffix...)); err == nil {
		t.Fatal("out-of-cut private marker must fault")
	}
	<-epoch.Done()
	_ = epoch.Finalize(context.Background())
	assertNoPrivateOrSuffix(t, wire.snapshot(), duplicate, suffix)
}

func TestMarkerIngressWrongNonceAndMalformedSplitAcrossCommit(t *testing.T) {
	cases := map[string]func([]byte) []byte{
		"wrong-nonce": func(exact []byte) []byte {
			wrong := append([]byte(nil), exact...)
			wrong[len(reservedWirePrefix)] = map[bool]byte{true: 'B', false: 'A'}[wrong[len(reservedWirePrefix)] == 'A']
			return wrong
		},
		"malformed": func(exact []byte) []byte {
			bad := append([]byte(nil), exact...)
			bad[len(reservedWirePrefix)+3] = '_'
			return bad
		},
	}
	for name, mutate := range cases {
		for _, phase := range []string{"before-commit", "across-commit", "after-commit"} {
			t.Run(name+"/"+phase, func(t *testing.T) {
				epoch, tx, wire, _ := newMarkerIngressEpoch(t, terminal.CutInitial, func(epoch *terminal.Epoch, req terminal.TransactionRequest) error {
					return epoch.PTYBytes(marker(req))
				})
				prepare := preparedFrame(t, wire)
				private := mutate(marker(lastCutRequest(t, tx)))
				split := len(reservedWirePrefix) - 1
				if phase == "after-commit" {
					if err := epoch.HandleFrame(markerBrowserFrame(terminal.FrameReady, prepare)); err != nil {
						t.Fatal(err)
					}
				}
				if err := epoch.PTYBytes(private[:split]); err != nil {
					t.Fatal(err)
				}
				if phase == "across-commit" {
					if err := epoch.HandleFrame(markerBrowserFrame(terminal.FrameReady, prepare)); err != nil {
						t.Fatal(err)
					}
				}
				suffix := []byte("must-not-follow")
				if err := epoch.PTYBytes(append(append([]byte(nil), private[split:]...), suffix...)); err == nil {
					t.Fatal("reserved wrong/malformed marker must fault")
				}
				<-epoch.Done()
				_ = epoch.Finalize(context.Background())
				assertNoPrivateOrSuffix(t, wire.snapshot(), private, suffix)
			})
		}
	}
}

func TestMarkerIngressPartialReservedPrefixAcrossCommitReleasesOnce(t *testing.T) {
	for split := 1; split < len(reservedWirePrefix); split++ {
		t.Run(strings.ReplaceAll(reservedWirePrefix[:split], "\x1b", "ESC"), func(t *testing.T) {
			epoch, _, wire, _ := newMarkerIngressEpoch(t, terminal.CutInitial, func(epoch *terminal.Epoch, req terminal.TransactionRequest) error {
				return epoch.PTYBytes(marker(req))
			})
			prepare := preparedFrame(t, wire)
			prefix := []byte(reservedWirePrefix[:split])
			if err := epoch.PTYBytes(prefix); err != nil {
				t.Fatal(err)
			}
			if err := epoch.HandleFrame(markerBrowserFrame(terminal.FrameReady, prepare)); err != nil {
				t.Fatal(err)
			}
			remainder := []byte("X-ordinary")
			if err := epoch.PTYBytes(remainder); err != nil {
				t.Fatal(err)
			}
			want := append(append([]byte(nil), prefix...), remainder...)
			frames := wire.wait(t, func(fs []terminal.Frame) bool {
				for _, frame := range fs {
					if frame.Type == terminal.FrameLive && bytes.Equal(frame.Data, want) {
						return true
					}
				}
				return false
			})
			count := 0
			for _, frame := range frames {
				if frame.Type == terminal.FrameLive && bytes.Equal(frame.Data, want) {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("ordinary prefix release count=%d want=%q", count, want)
			}
			_ = epoch.Finalize(context.Background())
		})
	}
}

func TestTerminalBoundaryGracefulEOFProperPrefixesBeforeEND(t *testing.T) {
	for split := 1; split < len(reservedWirePrefix); split++ {
		t.Run(fmt.Sprintf("prefix-%02d", split), func(t *testing.T) {
			epoch, _, wire, _ := newMarkerIngressEpoch(t, terminal.CutInitial, func(epoch *terminal.Epoch, req terminal.TransactionRequest) error {
				return epoch.PTYBytes(marker(req))
			})
			prepare := preparedFrame(t, wire)
			if err := epoch.HandleFrame(markerBrowserFrame(terminal.FrameReady, prepare)); err != nil {
				t.Fatal(err)
			}
			prefix := []byte(reservedWirePrefix[:split])
			if err := epoch.PTYBytes(prefix); err != nil {
				t.Fatal(err)
			}
			if err := epoch.Finalize(context.Background()); err != nil {
				t.Fatal(err)
			}
			frames := wire.snapshot()
			liveAt, endAt, liveCount, endCount := -1, -1, 0, 0
			for i, frame := range frames {
				if frame.Type == terminal.FrameLive && bytes.Equal(frame.Data, prefix) {
					liveAt, liveCount = i, liveCount+1
				}
				if frame.Type == terminal.FrameEnd {
					endAt, endCount = i, endCount+1
				}
			}
			if liveCount != 1 || endCount != 1 || liveAt < 0 || liveAt >= endAt {
				t.Fatalf("graceful EOF order/count live=%d@%d end=%d@%d frames=%#v", liveCount, liveAt, endCount, endAt, frames)
			}
		})
	}
}

func TestTerminalBoundaryGracefulEOFReservedPartialsFault(t *testing.T) {
	nonce := strings.Repeat("A", 43)
	var cases [][]byte
	for n := 0; n < len(nonce); n++ {
		cases = append(cases, []byte(reservedWirePrefix+nonce[:n]))
	}
	cases = append(cases, []byte(reservedWirePrefix+nonce), []byte(reservedWirePrefix+nonce+"\x1b"))
	for i, pending := range cases {
		t.Run(fmt.Sprintf("partial-%02d", i), func(t *testing.T) {
			epoch, _, wire, _ := newMarkerIngressEpoch(t, terminal.CutInitial, func(epoch *terminal.Epoch, req terminal.TransactionRequest) error {
				return epoch.PTYBytes(marker(req))
			})
			prepare := preparedFrame(t, wire)
			if err := epoch.HandleFrame(markerBrowserFrame(terminal.FrameReady, prepare)); err != nil {
				t.Fatal(err)
			}
			if err := epoch.PTYBytes(pending); err != nil {
				t.Fatal(err)
			}
			if err := epoch.Finalize(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := epoch.Err(); err == nil || !strings.Contains(err.Error(), "unterminated reserved Persea marker") {
				t.Fatalf("immutable terminal error=%v", err)
			}
			frames := wire.snapshot()
			for _, frame := range frames {
				if frame.Type == terminal.FrameLive && bytes.Contains(frame.Data, pending) {
					t.Fatalf("private partial leaked: %#v", frame)
				}
			}
			if len(frames) == 0 || frames[len(frames)-1].Type != terminal.FrameEnd || frames[len(frames)-1].Reason != "fault" {
				t.Fatalf("missing fault END: %#v", frames)
			}
		})
	}
}

func TestTerminalBoundaryExplicitFaultSuppressesPendingAndFinalizeIsSingle(t *testing.T) {
	for name, pending := range map[string][]byte{
		"ordinary-prefix": []byte(reservedWirePrefix[:7]),
		"private-partial": []byte(reservedWirePrefix + "AAAA"),
	} {
		t.Run(name, func(t *testing.T) {
			epoch, _, wire, _ := newMarkerIngressEpoch(t, terminal.CutInitial, func(epoch *terminal.Epoch, req terminal.TransactionRequest) error {
				return epoch.PTYBytes(marker(req))
			})
			prepare := preparedFrame(t, wire)
			if err := epoch.HandleFrame(markerBrowserFrame(terminal.FrameReady, prepare)); err != nil {
				t.Fatal(err)
			}
			if err := epoch.PTYBytes(pending); err != nil {
				t.Fatal(err)
			}
			first := errors.New("explicit terminal fault")
			if err := epoch.Fault(first); err != nil {
				t.Fatal(err)
			}
			const callers = 16
			errs := make(chan error, callers)
			var wg sync.WaitGroup
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); errs <- epoch.Finalize(context.Background()) }()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := epoch.Err(); !errors.Is(err, first) {
				t.Fatalf("first fault replaced: %v", err)
			}
			frames := wire.snapshot()
			endCount := 0
			for _, frame := range frames {
				if frame.Type == terminal.FrameLive && bytes.Contains(frame.Data, pending) {
					t.Fatalf("fault flushed pending ingress: %#v", frame)
				}
				if frame.Type == terminal.FrameEnd {
					endCount++
				}
			}
			wire.mu.Lock()
			closeCount := wire.closeCount
			wire.mu.Unlock()
			if endCount != 1 || closeCount != 1 {
				t.Fatalf("END=%d close=%d frames=%#v", endCount, closeCount, frames)
			}
		})
	}
}

func TestTerminalBoundaryConcurrentGracefulFinalizeConsumesEOFOnce(t *testing.T) {
	epoch, _, wire, _ := newMarkerIngressEpoch(t, terminal.CutInitial, func(epoch *terminal.Epoch, req terminal.TransactionRequest) error {
		return epoch.PTYBytes(marker(req))
	})
	prepare := preparedFrame(t, wire)
	if err := epoch.HandleFrame(markerBrowserFrame(terminal.FrameReady, prepare)); err != nil {
		t.Fatal(err)
	}
	pending := []byte(reservedWirePrefix[:11])
	if err := epoch.PTYBytes(pending); err != nil {
		t.Fatal(err)
	}
	const callers = 32
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- epoch.Finalize(context.Background()) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	frames := wire.snapshot()
	liveCount, endCount := 0, 0
	for _, frame := range frames {
		if frame.Type == terminal.FrameLive && bytes.Equal(frame.Data, pending) {
			liveCount++
		}
		if frame.Type == terminal.FrameEnd {
			endCount++
		}
	}
	wire.mu.Lock()
	closeCount := wire.closeCount
	wire.mu.Unlock()
	if liveCount != 1 || endCount != 1 || closeCount != 1 {
		t.Fatalf("LIVE=%d END=%d close=%d frames=%#v", liveCount, endCount, closeCount, frames)
	}
}

func TestMarkerIngressOrdinaryOSC52AndLookalikesAllSplits(t *testing.T) {
	streams := [][]byte{
		[]byte("left\x1b]52;;clipboard-data\aright"),
		[]byte("left\x1b]52;;persea:v2:AAAAAAAA\aright"),
		[]byte("left\x1b]52;;persea:v1x:AAAAAAAA\aright"),
	}
	for caseID, stream := range streams {
		for split := 0; split <= len(stream); split++ {
			epoch, _, wire, _ := newMarkerIngressEpoch(t, terminal.CutInitial, func(epoch *terminal.Epoch, req terminal.TransactionRequest) error {
				return epoch.PTYBytes(marker(req))
			})
			prepare := preparedFrame(t, wire)
			if err := epoch.HandleFrame(markerBrowserFrame(terminal.FrameReady, prepare)); err != nil {
				t.Fatal(err)
			}
			for _, part := range [][]byte{stream[:split], stream[split:]} {
				if len(part) > 0 {
					if err := epoch.PTYBytes(part); err != nil {
						t.Fatalf("case=%d split=%d: %v", caseID, split, err)
					}
				}
			}
			frames := wire.wait(t, func(fs []terminal.Frame) bool {
				var got []byte
				for _, frame := range fs {
					if frame.Type == terminal.FrameLive {
						got = append(got, frame.Data...)
					}
				}
				return bytes.Equal(got, stream)
			})
			var got []byte
			for _, frame := range frames {
				if frame.Type == terminal.FrameLive {
					got = append(got, frame.Data...)
				}
			}
			if !bytes.Equal(got, stream) {
				t.Fatalf("case=%d split=%d got=%q want=%q", caseID, split, got, stream)
			}
			_ = epoch.Finalize(context.Background())
		}
	}
}

func TestMarkerIngressExpectedMarkerEverySplitInitialAndReconnect(t *testing.T) {
	for _, kind := range []terminal.CutKind{terminal.CutInitial, terminal.CutReconnect} {
		for split := 0; split <= len(reservedWirePrefix)+43+1; split++ {
			epoch, _, wire, _ := newMarkerIngressEpoch(t, kind, func(epoch *terminal.Epoch, req terminal.TransactionRequest) error {
				private := marker(req)
				for _, part := range [][]byte{private[:split], private[split:]} {
					if len(part) > 0 {
						if err := epoch.PTYBytes(part); err != nil {
							return err
						}
					}
				}
				return nil
			})
			prepare := preparedFrame(t, wire)
			if prepare.Kind != kind {
				t.Fatalf("kind=%s split=%d prepare=%s", kind, split, prepare.Kind)
			}
			if err := epoch.HandleFrame(markerBrowserFrame(terminal.FrameReady, prepare)); err != nil {
				t.Fatal(err)
			}
			_ = epoch.Finalize(context.Background())
		}
	}
}

func TestMarkerIngressOngoingExpectedMarkerAndActivity(t *testing.T) {
	var epoch *terminal.Epoch
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	source, _ := sourceFor(t, tx)
	wire := newMemoryTransport()
	clock := newManualDeadlineClock()
	tx.onCut = func(req terminal.TransactionRequest) error {
		private := marker(req)
		if req.Kind == terminal.CutOngoing {
			mid := len(private) / 2
			if err := epoch.PTYBytes([]byte("delta")); err != nil {
				return err
			}
			if err := epoch.PTYBytes(private[:mid]); err != nil {
				return err
			}
			return epoch.PTYBytes(append(private[mid:], []byte("held")...))
		}
		return epoch.PTYBytes(private)
	}
	epoch, _ = terminal.NewEpoch(context.Background(), source, 42, wire, newRecordingPTY(), terminal.Config{
		Clock: clock, QuietInterval: 5 * time.Millisecond, MaximumInterval: 10 * time.Millisecond, CutTimeout: 25 * time.Millisecond,
	})
	if err := epoch.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatal(err)
	}
	<-clock.created
	initial := preparedFrame(t, wire)
	if err := epoch.HandleFrame(markerBrowserFrame(terminal.FrameReady, initial)); err != nil {
		t.Fatal(err)
	}
	if err := epoch.PTYBytes([]byte("trigger")); err != nil {
		t.Fatal(err)
	}
	schedulerTimer := <-clock.created
	schedulerTimer.fire(clock.Now())
	<-clock.created
	ongoing := waitLatestPrepare(t, wire, initial.Cut)
	if ongoing.Kind != terminal.CutOngoing {
		t.Fatalf("kind=%s", ongoing.Kind)
	}
	if err := epoch.HandleFrame(markerBrowserFrame(terminal.FrameReady, ongoing)); err != nil {
		t.Fatal(err)
	}
	frames := wire.wait(t, func(fs []terminal.Frame) bool {
		seenDelta, seenHeld := false, false
		for _, frame := range fs {
			seenDelta = seenDelta || frame.Type == terminal.FrameLive && bytes.Equal(frame.Data, []byte("delta"))
			seenHeld = seenHeld || frame.Type == terminal.FrameLive && bytes.Equal(frame.Data, []byte("held"))
		}
		return seenDelta && seenHeld
	})
	if len(frames) == 0 || errors.Is(epoch.Err(), terminal.ErrClosed) {
		t.Fatal("ongoing marker lifecycle faulted")
	}
	_ = epoch.Finalize(context.Background())
}
