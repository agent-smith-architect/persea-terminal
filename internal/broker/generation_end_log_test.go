package broker

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/proto"
	"persea-terminal/internal/unifiedjournal"
)

// generationEnds collects the generation-end log lines of effects. Install it
// before the observer starts.
func generationEnds(effects *UnifiedDevPaneEffects) func() []string {
	var mu sync.Mutex
	var lines []string
	effects.generationEndEdge = func(line string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, line)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(lines)
	}
}

// awaitGenerationEnd waits for the first line and requires it to be the only
// one and to equal want.
func awaitGenerationEnd(t *testing.T, ends func() []string, want string) {
	t.Helper()
	pollUntil(t, 5*time.Second, "generation-end log", func() bool { return len(ends()) > 0 })
	if got := ends(); len(got) != 1 || got[0] != want {
		t.Fatalf("generation-end log=%q want exactly %q", got, want)
	}
}

func generationEndLine(session string, reason proto.SubscriberCloseReason, cause string, recovering bool) string {
	return fmt.Sprintf("component=broker event=generation_end server=%q session=%q close=%q cause=%q recovering=%t", "main", session, reason, cause, recovering)
}

// A flow-control violation ends the generation through the faulted reap, which
// must state its cause like an ordinary observer exit.
func TestFlowControlFailureLogsItsCause(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux observer flow-control test")
	}
	fixture := newAdoptionFixture(t, 4)
	ends := generationEnds(fixture.effects)
	session := fixture.startPaneCommand(t, "flow-cause", "sh")
	if _, err := fixture.effects.AdoptSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	_, _, subscriber, cancel, err := openSnapshotTailForTest(t, fixture.effects, session)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	var client string
	for _, row := range strings.Split(fixture.disposable.run("list-clients", "-F", "#{client_name}|#{client_control_mode}|#{session_id}"), "\n") {
		if fields := strings.Split(row, "|"); len(fields) == 3 && fields[1] == "1" && fields[2] == session {
			client = fields[0]
		}
	}
	if client == "" {
		t.Fatal("observer client absent")
	}
	fixture.disposable.run("refresh-client", "-t", client, "-f", "pause-after=1")
	fixture.disposable.run("refresh-client", "-t", client, "-A", fixture.paneID(t, "flow-cause")+":pause")
	if got := waitRotationSubscriberClose(t, subscriber); got != proto.SubscriberClosedGenerationFailed {
		t.Fatalf("close=%q", got)
	}
	awaitGenerationEnd(t, ends, generationEndLine(session, proto.SubscriberClosedGenerationFailed, "flow_control", false))
}

// Fatal settlement ends a generation before its unit has exited, so each
// settlement owner names the cause. An owner that removes the active key logs
// the end itself; the unit's later reap must add no second line.
func TestFatalSettlementLogsItsCauseOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux observer rotation test")
	}
	for _, site := range []string{"rotation", "uncertain_refit", "refit_predecessor", "committed_successor"} {
		t.Run(site, func(t *testing.T) {
			fixture := newAdoptionFixture(t, 4)
			ends := generationEnds(fixture.effects)
			session := fixture.startPaneCommand(t, "settlement-cause", "sh")
			adoption, err := fixture.effects.AdoptSession(context.Background(), session)
			if err != nil {
				t.Fatal(err)
			}
			_, _, subscriber, cancel, err := openSnapshotTailForTest(t, fixture.effects, session)
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			fixture.effects.mu.Lock()
			unit := fixture.effects.units[session]
			fixture.effects.mu.Unlock()
			old, ok := unit.rotationWitness(adoption.Key)
			if !ok {
				t.Fatal("missing predecessor witness")
			}
			rotation := &unifiedDevRotation{unit: unit, session: session, registry: fixture.registry, old: old, oldKey: adoption.Key, ponr: true}
			reason := proto.SubscriberClosedGenerationFailed
			switch site {
			case "rotation":
				_ = rotation.settleFatal(unifiedjournal.ErrStorage)
			case "uncertain_refit":
				// The tmux session still exists, so the refit retains its
				// uncertain state and ends the tails itself.
				rotation.refit, reason = true, proto.SubscriberClosedRefitFaulted
				_ = rotation.settleFatal(unifiedjournal.ErrStorage)
			case "refit_predecessor":
				rotation.refit, reason = true, proto.SubscriberClosedRefitFaulted
				rotation.failRefitPredecessor(unifiedjournal.ErrStorage, observerSourceDecision{disposition: observerSourceOwnerGone})
				fixture.effects.reapFaultedUnitWithReason(unit, reason, observerCauseStorage)
			case "committed_successor":
				// The adopted generation stands in for a committed successor.
				rotation.newKey, rotation.next = adoption.Key, old
				rotation.failCommittedSuccessor(unifiedjournal.ErrStorage)
				fixture.effects.reapFaultedUnitWithReason(unit, reason, observerCauseStorage)
			}
			if got := waitRotationSubscriberClose(t, subscriber); got != reason {
				t.Fatalf("close=%q want %q", got, reason)
			}
			select {
			case <-unit.done:
			case <-time.After(5 * time.Second):
				t.Fatal("reaped unit did not exit")
			}
			awaitGenerationEnd(t, ends, generationEndLine(session, reason, "storage", false))
		})
	}
}
