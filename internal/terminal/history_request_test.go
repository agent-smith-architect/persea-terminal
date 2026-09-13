package terminal

import (
	"errors"
	"testing"
)

func TestHistoryRequestRecoverableSupersessionExistsOnlyBeforeCutIssue(t *testing.T) {
	queued := &historyRequest{request: 1, rows: 1_000, phase: historyRequestQueued}
	if err := queued.supersedeBeforeIssue(); err != nil || queued.phase != historyRequestSuperseded {
		t.Fatalf("queued supersession err=%v phase=%d", err, queued.phase)
	}
	for _, phase := range []historyRequestPhase{
		historyRequestIssued,
		historyRequestDeferred,
		historyRequestCommitted,
		historyRequestSuperseded,
	} {
		request := &historyRequest{request: 2, rows: 1_000, phase: phase}
		if err := request.supersedeBeforeIssue(); !errors.Is(err, ErrInvariant) {
			t.Fatalf("post-issue phase %d admitted a recoverable rejection: %v", phase, err)
		}
		if request.phase != phase {
			t.Fatalf("post-issue rejection mutated phase %d to %d", phase, request.phase)
		}
	}
}

func TestHistoryProtocolFenceReturnsLiveOnReadyOrReaderDefer(t *testing.T) {
	for _, response := range []FrameType{FrameReady, FrameDefer} {
		automaton, err := newAutomaton("source", 1)
		if err != nil {
			t.Fatal(err)
		}
		initial := Frame{Version: 1, Type: FramePrepare, Source: "source", Epoch: 1, Cut: 1, Kind: CutInitial, Columns: 80, Rows: 24}
		if err := automaton.apply(fromServer, initial); err != nil {
			t.Fatal(err)
		}
		if err := automaton.apply(fromBrowser, Frame{Version: 1, Type: FrameReady, Source: "source", Epoch: 1, Cut: 1}); err != nil {
			t.Fatal(err)
		}
		if err := automaton.apply(fromServer, Frame{Version: 1, Type: FrameCommit, Source: "source", Epoch: 1, Cut: 1}); err != nil {
			t.Fatal(err)
		}
		request := Frame{Version: 1, Type: FrameHistory, Source: "source", Epoch: 1, Request: 9, HistoryRows: 1_000}
		if err := automaton.apply(fromBrowser, request); err != nil {
			t.Fatal(err)
		}
		prepare := Frame{
			Version: 1, Type: FramePrepare, Source: "source", Epoch: 1, Cut: 2, Kind: CutHistory,
			Request: 9, EffectiveHistoryRows: 1_000, Columns: 80, Rows: 24,
		}
		if err := automaton.apply(fromServer, prepare); err != nil {
			t.Fatal(err)
		}
		switch response {
		case FrameReady:
			if err := automaton.apply(fromBrowser, Frame{Version: 1, Type: FrameReady, Source: "source", Epoch: 1, Cut: 2}); err != nil {
				t.Fatal(err)
			}
			if err := automaton.apply(fromServer, Frame{Version: 1, Type: FrameCommit, Source: "source", Epoch: 1, Cut: 2}); err != nil {
				t.Fatal(err)
			}
		case FrameDefer:
			if err := automaton.apply(fromBrowser, Frame{
				Version: 1, Type: FrameDefer, Source: "source", Epoch: 1, Cut: 2,
				Reason: "VISIBLE_ANCHOR_WOULD_BE_EVICTED",
			}); err != nil {
				t.Fatal(err)
			}
		}
		if automaton.state != stateLive || automaton.cut != 2 {
			t.Fatalf("%s left state=%d cut=%d", response, automaton.state, automaton.cut)
		}
	}
}
