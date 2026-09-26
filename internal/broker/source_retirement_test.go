package broker

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/unifiedjournal"
)

func TestSourceBrowserSocketClosePreservesGeneration(t *testing.T) {
	effects, key := unifiedE2E1JournalProvider(t, "rotation-f2-browser-close", "$browser")
	defer effects.realm.Close()
	unifiedE2E1Commit(t, effects.realm, key, []byte("F2-BROWSER-CLOSE"))
	beforeLogical, beforeReserved, _ := effects.realm.LogicalBudget()
	beforePhysical, beforePhysicalReserved, _ := effects.realm.PhysicalBudget()
	beforeSlots := effects.realm.AvailableCompletePaneSlots()
	_, _, subscriber, cancel, err := openSnapshotTailForTest(t, effects, "$browser")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	for deliveredForTest, open := subscriber.receive(); open; deliveredForTest, open = subscriber.receive() {
		subscriber.releaseEvent(deliveredForTest)
	}
	if reason := subscriber.closeReason(); reason != "" {
		t.Fatalf("browser cancellation gained provider verdict %q", reason)
	}
	if active, ok := effects.paneKey("$browser"); !ok || active != key {
		t.Fatalf("browser cancellation removed active generation: %+v %v", active, ok)
	}
	if _, _, _, stop, err := openSnapshotTailForTest(t, effects, "$browser"); err != nil {
		t.Fatalf("fresh replay after browser close: %v", err)
	} else {
		stop()
	}
	afterLogical, afterReserved, _ := effects.realm.LogicalBudget()
	afterPhysical, afterPhysicalReserved, _ := effects.realm.PhysicalBudget()
	if beforeLogical != afterLogical || beforeReserved != afterReserved || beforePhysical != afterPhysical ||
		beforePhysicalReserved != afterPhysicalReserved || beforeSlots != effects.realm.AvailableCompletePaneSlots() {
		t.Fatal("browser cancellation mutated durable capacity")
	}
}

func TestSourceSourceClassificationMatrix(t *testing.T) {
	want := controlmode.PaneWitness{Session: controlmode.SessionWitness{Server: "main", Session: "$1", ControlGeneration: 9}, Window: "@1", Pane: "%1", Incarnation: "exact"}
	different := want
	different.Incarnation = "replacement"
	tests := []struct {
		name          string
		current       *controlmode.PaneWitness
		present       bool
		authoritative bool
		want          observerSourceDisposition
	}{
		{name: "exact live observer transport loss", current: &want, present: true, authoritative: true, want: observerSourceTransportLost},
		{name: "authoritative absent owner", present: false, authoritative: true, want: observerSourceOwnerGone},
		{name: "different incarnation replacement", current: &different, present: true, authoritative: true, want: observerSourceReplacement},
		{name: "unavailable inventory", present: false, authoritative: false, want: observerSourceAmbiguous},
		{name: "ambiguous live without full witness", present: true, authoritative: true, want: observerSourceAmbiguous},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := classifyObserverRecheck(want, test.current, test.present, test.authoritative)
			if decision.disposition != test.want {
				t.Fatalf("disposition=%d want=%d", decision.disposition, test.want)
			}
		})
	}
}

func TestSourceSevenDeadSessionsRetireAndEighthAdmits(t *testing.T) {
	if testing.Short() {
		t.Skip("real private-tmux terminal-retirement regression test")
	}
	fixture := newAdoptionFixture(t, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	const sessionCount = 7
	sessionIDs := make([]string, 0, sessionCount)
	subscribers := make([]*unifiedDevSubscriber, 0, sessionCount)
	stops := make([]func(), 0, sessionCount)
	for index := 0; index < sessionCount; index++ {
		name := "rotation_f2_dead_" + string(rune('a'+index))
		sessionID := fixture.startPaneCommand(t, name, "sh -c 'stty -echo; printf F2-READY\\n; sleep 120'")
		if _, err := fixture.effects.AdoptSession(ctx, sessionID); err != nil {
			t.Fatalf("adopt %s: %v", name, err)
		}
		if index%2 == 0 {
			if err := fixture.effects.rotateSession(ctx, sessionID); err != nil {
				t.Fatalf("rotate %s: %v", name, err)
			}
		}
		_, _, subscriber, stop, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
		if err != nil {
			t.Fatalf("subscribe %s: %v", name, err)
		}
		sessionIDs = append(sessionIDs, sessionID)
		subscribers = append(subscribers, subscriber)
		stops = append(stops, stop)
	}
	for _, sessionID := range sessionIDs {
		fixture.disposable.run("kill-session", "-t", "="+sessionID)
	}
	pollUntil(t, 20*time.Second, "seven terminal generations to settle", func() bool {
		fixture.effects.mu.Lock()
		providerEmpty := len(fixture.effects.active) == 0 && len(fixture.effects.units) == 0
		fixture.effects.mu.Unlock()
		fixture.effects.subscriberMu.Lock()
		subscribersEmpty := len(fixture.effects.subscribers) == 0
		fixture.effects.subscriberMu.Unlock()
		fixture.registry.mu.Lock()
		admissionsEmpty := len(fixture.registry.admitted) == 0
		fixture.registry.mu.Unlock()
		fixture.registry.retention.mu.Lock()
		runtimeEmpty := len(fixture.registry.retention.generations) == 0 &&
			len(fixture.registry.retention.panes) == 0 &&
			fixture.registry.retention.pUsed == 0 && fixture.registry.retention.qUsed == 0 &&
			fixture.registry.retention.eUsed == 0 && fixture.registry.retention.bUsed == 0 &&
			fixture.registry.retention.oUsed == 0
		fixture.registry.retention.mu.Unlock()
		fixture.effects.journalMu.Lock()
		logical, logicalReserved, _ := fixture.effects.realm.LogicalBudget()
		physical, physicalReserved, _ := fixture.effects.realm.PhysicalBudget()
		slots := fixture.effects.realm.AvailableCompletePaneSlots()
		fixture.effects.journalMu.Unlock()
		return providerEmpty && subscribersEmpty && admissionsEmpty && runtimeEmpty &&
			logical == 0 && logicalReserved == 0 && physical == 0 && physicalReserved == 0 &&
			slots == 7 && fixture.journalFileCount(t) == 0
	})
	for index, subscriber := range subscribers {
		for deliveredForTest, open := subscriber.receive(); open; deliveredForTest, open = subscriber.receive() {
			subscriber.releaseEvent(deliveredForTest)
		}
		if reason := subscriber.closeReason(); reason != proto.SubscriberClosedGenerationFailed {
			t.Fatalf("subscriber %d close reason=%q", index, reason)
		}
		stops[index]()
	}

	eighth := fixture.startPaneCommand(t, "rotation_f2_eighth", "sh -c 'stty -echo; printf F2-EIGHTH\\n; sleep 120'")
	adoption, err := fixture.effects.AdoptSession(ctx, eighth)
	if err != nil {
		t.Fatalf("differently named eighth admission: %v", err)
	}
	if active, ok := fixture.effects.paneKey(eighth); !ok || active != adoption.Key {
		t.Fatalf("eighth active=%+v ok=%v want=%+v", active, ok, adoption.Key)
	}
}

func TestSourceObserverTransportRecoveryCapturesGapExactlyOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux observer transport regression test")
	}
	disposable := newDisposable(t)
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	cfg := unifiedAdoptionDevConfig(t, tmuxServer, t.TempDir(), 4)
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- ServeWithPaneEffects(listener, cfg, effects) }()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Error("broker did not stop")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	disposable.run("new-session", "-d", "-s", "rotation_f2_transport", "-x", "80", "-y", "24",
		"sh", "-c", "stty -echo; printf 'F2-TRANSPORT\\n'; exec sh")
	sessionID := strings.TrimSpace(disposable.run("display-message", "-p", "-t", "rotation_f2_transport:", "#{session_id}"))
	adoption, err := effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 5*time.Second, "transport marker", func() bool {
		return bytes.Contains((&adoptionFixture{effects: effects}).journalBytes(t, adoption.Key), []byte("F2-TRANSPORT"))
	})
	inventoryConn, inventory := request(t, listener.Addr().String(), proto.Control{Type: "inventory"})
	_ = inventoryConn.Close()
	var authority proto.Authority
	for _, server := range inventory.Servers {
		for _, session := range server.Sessions {
			if session.Authority.SessionID == sessionID {
				authority = session.Authority
			}
		}
	}
	if !authority.Valid() {
		t.Fatal("live session is absent from broker inventory")
	}
	_, _, subscriber, stop, err := openSnapshotTailForTest(t, effects, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	effects.journalMu.Lock()
	logicalBefore, logicalReservedBefore, _ := effects.realm.LogicalBudget()
	physicalBefore, physicalReservedBefore, _ := effects.realm.PhysicalBudget()
	slotsBefore := effects.realm.AvailableCompletePaneSlots()
	effects.journalMu.Unlock()
	effects.mu.Lock()
	unit := effects.units[sessionID]
	effects.mu.Unlock()
	if unit == nil || unit.process.Process == nil {
		t.Fatal("observer control process absent")
	}
	recoveryReady := make(chan struct{})
	recoveryContinue := make(chan struct{})
	var recoveryEdge sync.Once
	effects.observerReadinessEdge = func(got string) error {
		if got == sessionID {
			recoveryEdge.Do(func() { close(recoveryReady) })
			<-recoveryContinue
		}
		return nil
	}
	if err := unit.process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-recoveryReady:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement observer did not reach the capture edge")
	}
	unifiedE2E1SendPaneLine(t, disposable, sessionID, "printf 'F2-GAP-ONCE\\n'")
	unifiedE2E1AwaitPaneText(t, disposable, sessionID, "F2-GAP-ONCE")
	effects.journalMu.Lock()
	logicalGap, logicalReservedGap, _ := effects.realm.LogicalBudget()
	physicalGap, physicalReservedGap, _ := effects.realm.PhysicalBudget()
	slotsGap := effects.realm.AvailableCompletePaneSlots()
	effects.journalMu.Unlock()
	if logicalGap != logicalBefore || physicalGap != physicalBefore || slotsGap != slotsBefore-1 ||
		logicalReservedGap <= logicalReservedBefore || physicalReservedGap <= physicalReservedBefore {
		t.Fatalf("pre-capture recovery did not preserve predecessor/capacity logical=%d/%d reserved=%d/%d physical=%d/%d held=%d/%d slots=%d/%d",
			logicalGap, logicalBefore, logicalReservedGap, logicalReservedBefore, physicalGap, physicalBefore,
			physicalReservedGap, physicalReservedBefore, slotsGap, slotsBefore)
	}
	close(recoveryContinue)
	var recoveredKey unifiedjournal.PaneKey
	pollUntil(t, 10*time.Second, "observer capture-authoritative recovery", func() bool {
		effects.mu.Lock()
		recoveredKey = effects.active[sessionID]
		recoveredUnit := effects.units[sessionID]
		effects.mu.Unlock()
		return recoveredUnit != nil && recoveredKey != (unifiedjournal.PaneKey{}) && recoveredKey != adoption.Key
	})
	for deliveredForTest, open := subscriber.receive(); open; deliveredForTest, open = subscriber.receive() {
		subscriber.releaseEvent(deliveredForTest)
	}
	if reason := subscriber.closeReason(); reason != proto.SubscriberClosedGenerationFailed {
		t.Fatalf("transport subscriber reason=%q", reason)
	}

	inventoryConn, inventory = request(t, listener.Addr().String(), proto.Control{Type: "inventory"})
	_ = inventoryConn.Close()
	for _, server := range inventory.Servers {
		for _, session := range server.Sessions {
			if session.Authority.SessionID == sessionID {
				authority = session.Authority
			}
		}
	}
	reattached, prepared := unifiedE2E1OpenAttachment(t, listener.Addr().String(), authority)
	if prepared.Columns != 80 || prepared.Rows != 24 ||
		(!bytes.Contains(prepared.Replay, []byte("F2-TRANSPORT")) && !bytes.Contains([]byte(strings.Join(prepared.History, "\n")), []byte("F2-TRANSPORT"))) {
		t.Fatalf("retained attachment geometry=%dx%d replay=%q history=%q", prepared.Columns, prepared.Rows, prepared.Replay, prepared.History)
	}
	replayed := append(append([]byte(nil), prepared.Replay...), []byte(strings.Join(prepared.History, "\n"))...)
	if count := bytes.Count(replayed, []byte("F2-GAP-ONCE")); count != 1 {
		t.Fatalf("gap output replay count=%d want 1 replay=%q history=%q", count, prepared.Replay, prepared.History)
	}
	unifiedE2E1FitInput(t, reattached, prepared, "printf 'F2-POST-RECOVERY\\n'\n")
	unifiedE2E1AwaitPaneText(t, disposable, sessionID, "F2-POST-RECOVERY")
	pollUntil(t, 5*time.Second, "post-recovery input", func() bool {
		key, ok := effects.paneKey(sessionID)
		return ok && bytes.Contains((&adoptionFixture{effects: effects}).journalBytes(t, key), []byte("F2-POST-RECOVERY"))
	})
	unifiedE2E1Detach(t, reattached)
	_ = reattached.Close()
	registry, ok := effects.observer.(*paneRegistry)
	if !ok {
		t.Fatal("production registry absent")
	}
	registry.mu.Lock()
	state, admitted := registry.admitted[routeCoordinateJournalKey(recoveredKey)]
	session := registry.sessions[sessionKey(state.witness.Session)]
	routerEligible := admitted && session != nil && session.router.UnifiedEligible(state.witness)
	registry.mu.Unlock()
	if !routerEligible {
		t.Fatal("transport failure removed registry/router authority")
	}
	effects.journalMu.Lock()
	logical, logicalReserved, _ := effects.realm.LogicalBudget()
	physical, physicalReserved, _ := effects.realm.PhysicalBudget()
	slots := effects.realm.AvailableCompletePaneSlots()
	effects.journalMu.Unlock()
	if logical == 0 || logicalReserved != logicalReservedBefore || physical == 0 || physicalReserved != physicalReservedBefore || slots != slotsBefore {
		t.Fatalf("recovery lost durable authority logical=%d reserved=%d/%d physical=%d held=%d/%d slots=%d/%d",
			logical, logicalReserved, logicalReservedBefore, physical, physicalReserved, physicalReservedBefore, slots, slotsBefore)
	}
	effects.mu.Lock()
	observerCount := len(effects.units)
	effects.mu.Unlock()
	if observerCount != 1 {
		t.Fatalf("observer count=%d want exactly 1", observerCount)
	}
}

func TestSourceTerminalRetirementRetriesAfterRetentionSaturation(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux terminal-retirement saturation regression test")
	}
	fixture := newAdoptionFixture(t, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sessionID := fixture.startPaneCommand(t, "rotation_f2_saturated", "sh -c 'printf F2-SATURATED\\n; sleep 120'")
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, subscriber, stop, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	fixture.registry.retention.mu.Lock()
	originalLimit := fixture.registry.retention.options.maxCommands
	fixture.registry.retention.options.maxCommands = 0
	fixture.registry.retention.mu.Unlock()
	attempts := make(chan chan struct{}, 4)
	fixture.registry.retention.setHook(func(point string, _ unifiedjournal.PaneKey) {
		if point != "before_disconnect_reserve" {
			return
		}
		gate := make(chan struct{})
		attempts <- gate
		<-gate
	})
	defer fixture.registry.retention.setHook(nil)
	fixture.disposable.run("kill-session", "-t", "="+sessionID)
	var firstGate chan struct{}
	select {
	case firstGate = <-attempts:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal retirement never reached the owned boundary")
	}
	fixture.effects.mu.Lock()
	_, pending := fixture.effects.terminalRetires[adoption.Key]
	fixture.effects.mu.Unlock()
	if !pending {
		t.Fatal("terminal boundary had no retry owner")
	}
	close(firstGate)
	var retryGate chan struct{}
	select {
	case retryGate = <-attempts:
	case <-time.After(5 * time.Second):
		t.Fatal("saturated terminal boundary was dropped instead of retried")
	}
	fixture.registry.mu.Lock()
	_, admittedWhileSaturated := fixture.registry.admitted[routeCoordinateJournalKey(adoption.Key)]
	fixture.registry.mu.Unlock()
	if !admittedWhileSaturated {
		t.Fatal("saturated first attempt partially removed registry admission")
	}
	fixture.registry.retention.mu.Lock()
	fixture.registry.retention.options.maxCommands = originalLimit
	fixture.registry.retention.mu.Unlock()
	close(retryGate)
	pollUntil(t, 10*time.Second, "saturated terminal retry settlement", func() bool {
		fixture.effects.mu.Lock()
		_, pending := fixture.effects.terminalRetires[adoption.Key]
		fixture.effects.mu.Unlock()
		fixture.registry.mu.Lock()
		_, admitted := fixture.registry.admitted[routeCoordinateJournalKey(adoption.Key)]
		fixture.registry.mu.Unlock()
		fixture.effects.journalMu.Lock()
		logical, logicalReserved, _ := fixture.effects.realm.LogicalBudget()
		physical, physicalReserved, _ := fixture.effects.realm.PhysicalBudget()
		slots := fixture.effects.realm.AvailableCompletePaneSlots()
		fixture.effects.journalMu.Unlock()
		return !pending && !admitted && logical == 0 && logicalReserved == 0 &&
			physical == 0 && physicalReserved == 0 && slots == 3 && fixture.journalFileCount(t) == 0
	})
	for deliveredForTest, open := subscriber.receive(); open; deliveredForTest, open = subscriber.receive() {
		subscriber.releaseEvent(deliveredForTest)
	}
	if reason := subscriber.closeReason(); reason != proto.SubscriberClosedGenerationFailed {
		t.Fatalf("saturated subscriber reason=%q", reason)
	}
}

func TestTerminalDurableRetirementRetriesAfterUnlinkFailure(t *testing.T) {
	var attempts int
	var retry func()
	var timers int
	runtime := &retentionTrialRuntime{options: retentionTrialOptions{
		retire: func(unifiedjournal.PaneKey) bool {
			attempts++
			return attempts == 2
		},
		after: func(delay time.Duration, fn func()) func() bool {
			if delay != durableRetirementRetryDelay {
				t.Fatalf("retry delay=%v want=%v", delay, durableRetirementRetryDelay)
			}
			retry = fn
			timers++
			return func() bool { return true }
		},
	}}
	key := unifiedjournal.PaneKey{Server: "main", Session: "rotation-f2-retry", Pane: "%1", Incarnation: "1"}

	runtime.retireDurable(key)
	if attempts != 1 || retry == nil {
		t.Fatalf("initial attempts=%d retry=%v", attempts, retry != nil)
	}
	runtime.retireDurable(key)
	if attempts != 1 || timers != 1 {
		t.Fatalf("duplicate call multiplied work attempts=%d timers=%d", attempts, timers)
	}
	retry()
	if attempts != 1 {
		t.Fatal("retry timer performed retirement I/O")
	}
	runtime.processDurableRetries()
	if attempts != 2 {
		t.Fatalf("retry attempts=%d want=2", attempts)
	}
}
