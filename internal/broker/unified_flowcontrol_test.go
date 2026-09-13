package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
)

// TestUnifiedAdoptionRejectsObserverFlowControl is the advisor's A6 adoption
// falsifier. A test-only scheduling edge holds the production observer after
// readiness while a second real same-server client enables tmux flow control
// and pauses the target pane. The pane then floods unique rows before the exact
// production adoption composite runs.
func TestUnifiedAdoptionRejectsObserverFlowControl(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux flow-control adoption falsifier")
	}
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "flow-control", "sh")
	paneID := fixture.paneID(t, "flow-control")
	var observerClient string
	const marker = "FLOW_LATE_0099"
	command := `i=0; pad=$(printf '%0200d' 0); while [ $i -lt 100 ]; do printf 'FLOW_LATE_%04d:%s\n' "$i" "$pad"; i=$((i+1)); done`
	fixture.effects.observerReadinessEdge = func(readySession string) error {
		if readySession != sessionID {
			return fmt.Errorf("readiness session=%s want=%s", readySession, sessionID)
		}
		out, err := tmuxCombinedOutput(fixture.disposable.path, "list-clients", "-F", "#{client_name}|#{client_control_mode}|#{session_id}")
		if err != nil {
			return fmt.Errorf("list observer clients: %w: %s", err, out)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Split(line, "|")
			if len(fields) == 3 && fields[1] == "1" && fields[2] == sessionID {
				observerClient = fields[0]
				break
			}
		}
		if observerClient == "" {
			return errors.New("production observer control client was not found")
		}
		for _, args := range [][]string{
			{"refresh-client", "-t", observerClient, "-f", "pause-after=1"},
			{"refresh-client", "-t", observerClient, "-A", paneID + ":pause"},
			{"send-keys", "-l", "-t", "=" + sessionID + ":", command},
			{"send-keys", "-t", "=" + sessionID + ":", "Enter"},
		} {
			out, err := tmuxCombinedOutput(fixture.disposable.path, args...)
			if err != nil {
				return fmt.Errorf("tmux %v: %w: %s", args, err, out)
			}
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			out, err := tmuxCombinedOutput(fixture.disposable.path, "capture-pane", "-p", "-t", "="+sessionID+":")
			if err == nil && strings.Contains(string(out), marker) {
				return nil
			}
			time.Sleep(20 * time.Millisecond)
		}
		return errors.New("withheld marker did not reach tmux capture state")
	}
	adoption, err := fixture.effects.AdoptSession(context.Background(), sessionID)

	// Exercise the advisor's final "then continue" step if the vulnerable
	// observer survived. A corrected unit has already died, so tmux rejects the
	// stale client target and there is nothing to resume.
	_, _ = tmuxCombinedOutput(fixture.disposable.path, "refresh-client", "-t", observerClient, "-A", paneID+":continue")
	if !errors.Is(err, ErrUnifiedObserverFlowControl) {
		if err != nil {
			t.Fatalf("flow-control adoption error=%v, want typed fatal %v", err, ErrUnifiedObserverFlowControl)
		}
		time.Sleep(250 * time.Millisecond)
		if err := fixture.registry.retention.Boundary(adoption.Key, "flow_control_falsifier"); err != nil {
			t.Fatalf("seal vulnerable journal: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && bytes.Count(fixture.journalBytes(t, adoption.Key), []byte(marker)) < 2 {
			time.Sleep(20 * time.Millisecond)
		}
		journal := fixture.journalBytes(t, adoption.Key)
		t.Fatalf("flow-controlled observer admitted session %s; marker %q appears %d times across capture bootstrap plus late delivery (journal=%d bytes)",
			sessionID, marker, bytes.Count(journal, []byte(marker)), len(journal))
	}

	fixture.effects.mu.Lock()
	_, active := fixture.effects.active[sessionID]
	fixture.effects.mu.Unlock()
	if active || fixture.journalFileCount(t) != 0 {
		t.Fatalf("flow-control refusal exposed active=%v journal_files=%d", active, fixture.journalFileCount(t))
	}
	pollUntil(t, 5*time.Second, "flow-control unit death and adoption reap", func() bool {
		fixture.effects.mu.Lock()
		defer fixture.effects.mu.Unlock()
		_, adopting := fixture.effects.adopting[sessionID]
		_, unit := fixture.effects.units[sessionID]
		return !adopting && !unit
	})
	if out, err := tmuxCombinedOutput(fixture.disposable.path, "has-session", "-t", sessionID); err != nil {
		t.Fatalf("old session was not preserved: %v %s", err, out)
	}
	if state := fixture.effects.projectSession("main", sessionID, unifiedSessionFacts{Windows: 1, Panes: 1, Valid: true}); state == nil || state.State != "adoptable" {
		t.Fatalf("session did not return adoptable after flow-control fault: %+v", state)
	}
}

func TestUnifiedObserverFlowControlTripwireClosedSet(t *testing.T) {
	effects := &UnifiedDevPaneEffects{}
	for _, event := range []controlmode.Event{
		{Kind: controlmode.EventPause, Name: "pause", Args: "%1"},
		{Kind: controlmode.EventContinue, Name: "continue", Args: "%1"},
		{Kind: controlmode.EventExtendedOutput, PaneID: "%1", Data: []byte("late")},
	} {
		if err := effects.consumeObserverEvent(event); !errors.Is(err, ErrUnifiedObserverFlowControl) {
			t.Fatalf("tripwire event %+v error=%v", event, err)
		}
	}
	if err := effects.consumeObserverEvent(controlmode.Event{Kind: controlmode.EventNotification, Name: "layout-change"}); err != nil {
		t.Fatalf("ordinary notification tripped flow-control fault: %v", err)
	}
}

func TestUnifiedActiveObserverFlowControlRunsDeathProtocol(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux observer death protocol")
	}
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "active-flow-control", "sh")
	paneID := fixture.paneID(t, "active-flow-control")
	if _, err := fixture.effects.AdoptSession(context.Background(), sessionID); err != nil {
		t.Fatalf("safe adoption: %v", err)
	}

	var observerClient string
	pollUntil(t, 5*time.Second, "active production observer control client", func() bool {
		out, err := tmuxCombinedOutput(fixture.disposable.path, "list-clients", "-F", "#{client_name}|#{client_control_mode}|#{session_id}")
		if err != nil {
			return false
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Split(line, "|")
			if len(fields) == 3 && fields[1] == "1" && fields[2] == sessionID {
				observerClient = fields[0]
				return observerClient != ""
			}
		}
		return false
	})
	fixture.disposable.run("refresh-client", "-t", observerClient, "-f", "pause-after=1")
	fixture.disposable.run("refresh-client", "-t", observerClient, "-A", paneID+":pause")

	pollUntil(t, 5*time.Second, "fatal flow-control unit reap", func() bool {
		fixture.effects.mu.Lock()
		defer fixture.effects.mu.Unlock()
		_, active := fixture.effects.active[sessionID]
		_, unit := fixture.effects.units[sessionID]
		return !active && !unit
	})
	if out, err := tmuxCombinedOutput(fixture.disposable.path, "has-session", "-t", sessionID); err != nil {
		t.Fatalf("death protocol destroyed old session: %v %s", err, out)
	}
	if state := fixture.effects.projectSession("main", sessionID, unifiedSessionFacts{Windows: 1, Panes: 1, Valid: true}); state == nil || state.State != "adoptable" {
		t.Fatalf("reaped active session is not adoptable: %+v", state)
	}
}

func TestUnifiedAdoptionReadinessRejectsSignalLessNoOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux readiness interrogation")
	}
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "no-output", "sh")
	fixture.effects.observerReadinessEdge = func(readySession string) error {
		out, err := tmuxCombinedOutput(fixture.disposable.path, "list-clients", "-F", "#{client_name}|#{client_control_mode}|#{session_id}")
		if err != nil {
			return fmt.Errorf("list observer clients: %w: %s", err, out)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Split(line, "|")
			if len(fields) != 3 || fields[1] != "1" || fields[2] != readySession {
				continue
			}
			setOut, setErr := tmuxCombinedOutput(fixture.disposable.path, "refresh-client", "-t", fields[0], "-f", "no-output")
			if setErr != nil {
				return fmt.Errorf("set no-output: %w: %s", setErr, setOut)
			}
			return nil
		}
		return errors.New("production observer control client was not found")
	}
	if _, err := fixture.effects.AdoptSession(context.Background(), sessionID); !errors.Is(err, ErrUnifiedObserverFlowControl) {
		t.Fatalf("signal-less no-output adoption error=%v", err)
	}
	if fixture.journalFileCount(t) != 0 {
		t.Fatalf("no-output readiness refusal created %d journal files", fixture.journalFileCount(t))
	}
	if out, err := tmuxCombinedOutput(fixture.disposable.path, "has-session", "-t", sessionID); err != nil {
		t.Fatalf("no-output refusal destroyed old session: %v %s", err, out)
	}
}

func TestUnifiedObserverReadinessRejectsDangerousClientFlags(t *testing.T) {
	for _, flags := range []string{"pause-after=1", "ignore-size,pause-after=500", "no-output", "ignore-size,no-output"} {
		if err := rejectObserverClientFlags(flags); !errors.Is(err, ErrUnifiedObserverFlowControl) {
			t.Fatalf("flags %q error=%v", flags, err)
		}
	}
	for _, flags := range []string{"", "ignore-size", "control-mode,ignore-size"} {
		if err := rejectObserverClientFlags(flags); err != nil {
			t.Fatalf("safe flags %q rejected: %v", flags, err)
		}
	}
	if err := rejectObserverClientInventory("11|attached,control-mode\n42|attached,ignore-size,UTF-8\n", 42); err != nil {
		t.Fatalf("safe exact observer inventory rejected: %v", err)
	}
	for name, test := range map[string]struct {
		inventory string
		pid       int
	}{
		"dangerous exact observer": {"11|ignore-size\n42|ignore-size,pause-after=1\n", 42},
		"missing observer":         {"11|ignore-size\n", 42},
		"duplicate observer":       {"42|ignore-size\n42|ignore-size\n", 42},
	} {
		t.Run(name, func(t *testing.T) {
			if err := rejectObserverClientInventory(test.inventory, test.pid); !errors.Is(err, ErrUnifiedObserverFlowControl) {
				t.Fatalf("inventory %q pid=%d error=%v", test.inventory, test.pid, err)
			}
		})
	}
}

func TestUnifiedAdoptionCompositeInterrogatesAndNormalizesBeforeCapture(t *testing.T) {
	line := adoptionCompositeLine("%42")
	parts := strings.Split(strings.TrimSuffix(line, "\n"), " ; ")
	if len(parts) != adoptionCompositeBlocks {
		t.Fatalf("composite blocks=%d want=%d: %q", len(parts), adoptionCompositeBlocks, line)
	}
	if !strings.Contains(parts[0], "#{client_flags}") {
		t.Fatalf("first composite block does not interrogate observer flags: %q", parts[0])
	}
	if parts[1] != `refresh-client -A '%42:on'` {
		t.Fatalf("second composite block does not normalize exact pane subscription: %q", parts[1])
	}
	if !strings.Contains(parts[2], "display-message") || !strings.Contains(parts[3], "capture-pane") {
		t.Fatalf("normalization is not before PRE/capture: %q", line)
	}
}
