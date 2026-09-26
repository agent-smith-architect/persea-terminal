package broker

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

// unifiedDevObservationRecorder is a PaneObservationEffects double that records
// admissions and typed owner-gone observations so the unit-death protocol is
// observable without the full registry.
type unifiedDevObservationRecorder struct {
	inner       *paneRegistry
	mu          sync.Mutex
	admitted    []controlmode.PaneWitness
	disconnects []controlmode.PaneWitness
}

func (recorder *unifiedDevObservationRecorder) AdmitPane(witness controlmode.PaneWitness) error {
	recorder.mu.Lock()
	recorder.admitted = append(recorder.admitted, witness)
	recorder.mu.Unlock()
	return recorder.inner.AdmitPane(witness)
}

func (recorder *unifiedDevObservationRecorder) recordingRegistry() *paneRegistry {
	return recorder.inner
}
func (recorder *unifiedDevObservationRecorder) publishInitial(op *recordingInitialOperation) error {
	return recorder.inner.publishInitial(op)
}
func (recorder *unifiedDevObservationRecorder) beginInitial(kind recordingInitialKind, witness controlmode.PaneWitness, source terminal.SourceWitness, geometry unifiedjournal.Geometry, payload []byte) (*recordingInitialOperation, error) {
	return recorder.inner.beginInitial(kind, witness, source, geometry, payload)
}

func (recorder *unifiedDevObservationRecorder) ObservePane(observation controlmode.Observation) error {
	if observation.Kind != controlmode.ObservationOwnerGone {
		return recorder.inner.ObservePane(observation)
	}
	recorder.mu.Lock()
	recorder.disconnects = append(recorder.disconnects, observation.Witness)
	recorder.mu.Unlock()
	return recorder.inner.ObservePane(observation)
}

func (recorder *unifiedDevObservationRecorder) disconnectedSessions() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	sessions := make([]string, 0, len(recorder.disconnects))
	for _, witness := range recorder.disconnects {
		sessions = append(sessions, witness.Session.Session)
	}
	return sessions
}

func unifiedDevTestBirth(t *testing.T, effects *UnifiedDevPaneEffects, server config.TmuxServer, name string) string {
	t.Helper()
	birthEffects, err := effects.BeginSessionBirth(server.Label, name)
	if err != nil || birthEffects == nil {
		t.Fatalf("begin unified birth: effects=%v err=%v", birthEffects, err)
	}
	creator, ok := birthEffects.(sessionBirthCreator)
	if !ok {
		t.Fatal("unified birth is not a session creator")
	}
	sessionID, err := creator.CreateSession(server, []string{"new-session", "-P", "-F", "#{session_id}", "-s", name, "-x", "80", "-y", "24"})
	if err != nil {
		t.Fatalf("unified unit birth: %v", err)
	}
	if err := birthEffects.CommitSessionBirth(sessionID); err != nil {
		t.Fatalf("unified birth commit for %q: %v", sessionID, err)
	}
	return sessionID
}

// TestUnifiedDevUnitDeathRunsDisconnectProtocol pins the unit-death protocol:
// when an observed session dies, its unit's exit synthesizes an
// ObservationOwnerGone for every pane the unit registered, removes the
// session from the active projection, cleans the pane routing map — and the
// supervisor keeps running, proven by a successful rebirth on the same name.
func TestUnifiedDevUnitDeathRunsDisconnectProtocol(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux unit lifecycle")
	}
	disposable := newDisposable(t)
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	cfg := unifiedE2E1DevConfig(t, tmuxServer, t.TempDir())
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	registry := newPaneRegistry(effects)
	defer registry.Close()
	recorder := &unifiedDevObservationRecorder{inner: registry}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisorDone := make(chan error, 1)
	go func() { supervisorDone <- effects.RunObserver(ctx, recorder) }()

	sessionID := unifiedDevTestBirth(t, effects, tmuxServer, "unified_target")
	if _, ok := effects.paneKey(sessionID); !ok {
		t.Fatalf("born session %q is not journal-active", sessionID)
	}

	if out, err := tmuxCombinedOutput(disposable.path, "kill-session", "-t", sessionID); err != nil {
		t.Fatalf("session teardown: %v %s", err, out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, active := effects.paneKey(sessionID)
		effects.mu.Lock()
		_, unitPresent := effects.units[sessionID]
		remainingPanes := len(effects.panes)
		effects.mu.Unlock()
		disconnected := false
		for _, session := range recorder.disconnectedSessions() {
			if session == sessionID {
				disconnected = true
			}
		}
		if !active && !unitPresent && remainingPanes == 0 && disconnected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unit death protocol incomplete: active=%t unit=%t panes=%d disconnects=%v",
				active, unitPresent, remainingPanes, recorder.disconnectedSessions())
		}
		time.Sleep(20 * time.Millisecond)
	}
	recorder.mu.Lock()
	admitted := append([]controlmode.PaneWitness(nil), recorder.admitted...)
	disconnects := append([]controlmode.PaneWitness(nil), recorder.disconnects...)
	recorder.mu.Unlock()
	if len(admitted) != 1 || len(disconnects) != 1 || admitted[0] != disconnects[0] {
		t.Fatalf("disconnect witness mismatch: admitted=%+v disconnects=%+v", admitted, disconnects)
	}

	// The supervisor must survive the unit's death: the same configured name
	// must be born again through a fresh unit.
	select {
	case err := <-supervisorDone:
		t.Fatalf("supervisor exited on unit death: %v", err)
	default:
	}
	rebornID := unifiedDevTestBirth(t, effects, tmuxServer, "unified_target")
	if rebornID == sessionID {
		t.Fatalf("rebirth reused session identity %q", sessionID)
	}
	if _, ok := effects.paneKey(rebornID); !ok {
		t.Fatalf("reborn session %q is not journal-active", rebornID)
	}
}

// TestUnifiedDevCommandErrorConsumesDecodedRemainder pins the %error remainder
// discipline: a rejected command must route the rest of its already-decoded
// chunk onward before returning the error, because that remainder carries the
// same connection's live pane bytes.
func TestUnifiedDevCommandErrorConsumesDecodedRemainder(t *testing.T) {
	chunk := []byte("%begin 100 1 0\n%error 100 1 0\n%output %7 live-after-error\n")
	newFixture := func() (*UnifiedDevPaneEffects, *unifiedDevBirth) {
		effects := &UnifiedDevPaneEffects{panes: map[string]*unifiedDevBirth{}}
		holder := &unifiedDevBirth{
			owner: effects, server: "main", name: "unified_target", ready: make(chan struct{}),
			witness: controlmode.PaneWitness{
				Session: controlmode.SessionWitness{Server: "main", Session: "$7", ControlGeneration: 1},
				Window:  "@7", Pane: "%7", Incarnation: "incarnation-7",
			},
		}
		effects.panes["%7"] = holder
		return effects, holder
	}
	assertRemainderKept := func(t *testing.T, holder *unifiedDevBirth) {
		t.Helper()
		holder.mu.Lock()
		defer holder.mu.Unlock()
		if len(holder.pending) != 1 || string(holder.pending[0].Data) != "live-after-error" {
			t.Fatalf("decoded remainder was dropped after %%error: pending=%+v", holder.pending)
		}
	}

	t.Run("command", func(t *testing.T) {
		effects, holder := newFixture()
		unit := &unifiedDevUnit{owner: effects, done: make(chan struct{})}
		read := make(chan []byte, 1)
		read <- chunk
		err := unit.finishCommand(context.Background(), controlmode.NewDecoder(), read, make(chan error, 1),
			unifiedDevCommand{blocks: 1, done: make(chan unifiedDevCommandResult, 1)})
		if err == nil {
			t.Fatal("rejected command was accepted")
		}
		assertRemainderKept(t, holder)
	})

	t.Run("birth", func(t *testing.T) {
		effects, holder := newFixture()
		born := &unifiedDevBirth{owner: effects, server: "main", name: "unified_target", ready: make(chan struct{})}
		unit := &unifiedDevUnit{
			owner: effects, done: make(chan struct{}),
			birth: unifiedDevCommand{birth: born, done: make(chan unifiedDevCommandResult, 1)},
		}
		read := make(chan []byte, 1)
		read <- chunk
		err := unit.finishBirth(context.Background(), controlmode.NewDecoder(), read, make(chan error, 1))
		if err == nil {
			t.Fatal("rejected birth was accepted")
		}
		assertRemainderKept(t, holder)
	})
}

// TestUnifiedDevPublishEventEvictsWedgedSubscriber pins delivery-plane
// isolation: publication holds the realm-wide subscriber lock, so a reader
// that never drains must be evicted on a full buffer — never blocked on —
// and every other pane's tail keeps flowing. The evicted reader recovers by
// reconnecting through snapshot+tail, the same contract as lagging-cursor
// eviction.
func TestUnifiedDevPublishEventEvictsWedgedSubscriber(t *testing.T) {
	keyA := unifiedjournal.PaneKey{Server: "main", Session: "$1", ControlGeneration: 1, Window: "@1", Pane: "%1", Incarnation: "one"}
	keyB := unifiedjournal.PaneKey{Server: "main", Session: "$2", ControlGeneration: 1, Window: "@2", Pane: "%2", Incarnation: "two"}
	effects := &UnifiedDevPaneEffects{subscribers: map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}{}}
	var budget recordingReaderBudget
	lease, err := budget.acquire(0)
	if err != nil {
		t.Fatal(err)
	}
	lease.releaseSnapshot()
	defer lease.detach()
	wedged := &unifiedDevSubscriber{lease: lease, data: newRecordingTailQueue(), done: make(chan struct{})}
	healthy := &unifiedDevSubscriber{data: newRecordingTailQueue(), done: make(chan struct{})}
	effects.subscribers[keyA] = map[*unifiedDevSubscriber]struct{}{wedged: {}}
	effects.subscribers[keyB] = map[*unifiedDevSubscriber]struct{}{healthy: {}}
	output := func(sequence int64) unifiedjournal.Event {
		return unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Sequence: sequence, Start: sequence - 1, End: sequence, Payload: bytes.Repeat([]byte("x"), recordingTailBytes/2)}
	}

	if err := effects.publishEvent(keyA, output(1)); err != nil {
		t.Fatal(err)
	}
	published := make(chan struct{})
	go func() {
		_ = effects.publishEvent(keyA, output(2))
		close(published)
	}()
	select {
	case <-published:
	case <-time.After(2 * time.Second):
		t.Fatal("a wedged subscriber stalled publication")
	}

	if err := effects.publishEvent(keyB, output(1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-healthy.events():
		event, _ := healthy.receive()
		if event.Sequence != 1 {
			t.Fatalf("healthy subscriber sequence=%d", event.Sequence)
		}
	default:
		t.Fatal("the healthy subscriber's tail did not advance")
	}

	effects.subscriberMu.Lock()
	_, present := effects.subscribers[keyA][wedged]
	effects.subscriberMu.Unlock()
	if present {
		t.Fatal("the wedged subscriber was not evicted")
	}
	if wedged.data.len() != 0 {
		t.Fatal("eviction retained the abandoned queued payload")
	}
	if _, open := wedged.receive(); open {
		t.Fatal("the evicted subscriber's channel was not closed")
	}
	// Eviction is a typed close, never a bare channel close: the attachment
	// that tails this subscriber ends with this reason (B1).
	if wedged.closeReason() != proto.SubscriberClosedLagged {
		t.Fatalf("evicted subscriber closeReason=%q want %q", wedged.closeReason(), proto.SubscriberClosedLagged)
	}
	if healthy.closeReason() != "" {
		t.Fatalf("healthy subscriber carries a close reason %q", healthy.closeReason())
	}
}
