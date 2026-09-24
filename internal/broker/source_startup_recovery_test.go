package broker

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

func rotationStartupWitness() startupPaneWitness {
	return startupPaneWitness{
		server: "main", session: "$1", window: "@2", pane: "%3",
		socket:        terminal.SocketIdentity{Path: "/run/tmux/main", Device: 11, Inode: 12},
		serverProcess: terminal.ProcessWitness{PID: 101, StartTime: 102},
		paneProcess:   terminal.ProcessWitness{PID: 201, StartTime: 202},
		geometry:      unifiedjournal.Geometry{Columns: 80, Rows: 34},
	}
}

func TestSourceStartupIdentityUsesOriginalAndChecksFinalGeometry(t *testing.T) {
	live := rotationStartupWitness()
	initial := unifiedjournal.Geometry{Columns: 80, Rows: 24}
	final := live.geometry
	recovery := unifiedjournal.Recovery{
		Key: unifiedjournal.PaneKey{
			Server: live.server, Session: live.session, Window: live.window, Pane: live.pane,
			Incarnation: live.incarnation(initial), ControlGeneration: 9,
		},
		Initial: initial, Final: final,
	}
	cases := []struct {
		name          string
		inventory     []startupPaneWitness
		authoritative bool
		server        string
		want          unifiedjournal.RecoveryDisposition
	}{
		{name: "fit_restart_exact", inventory: []startupPaneWitness{live}, authoritative: true, server: "main", want: unifiedjournal.RecoveryKeepExact},
		{name: "unexplained_final_geometry_drift", inventory: []startupPaneWitness{func() startupPaneWitness { changed := live; changed.geometry.Rows = 35; return changed }()}, authoritative: true, server: "main", want: unifiedjournal.RecoveryRetainAmbiguous},
		{name: "changed_process_identity", inventory: []startupPaneWitness{func() startupPaneWitness { changed := live; changed.paneProcess.StartTime++; return changed }()}, authoritative: true, server: "main", want: unifiedjournal.RecoveryRetireReplacement},
		{name: "absent", authoritative: true, server: "main", want: unifiedjournal.RecoveryRetireAbsent},
		{name: "duplicate_session_rows", inventory: []startupPaneWitness{live, live}, authoritative: true, server: "main", want: unifiedjournal.RecoveryRetainAmbiguous},
		{name: "inventory_error", inventory: []startupPaneWitness{live}, server: "main", want: unifiedjournal.RecoveryRetainAmbiguous},
		{name: "foreign_server", inventory: []startupPaneWitness{live}, authoritative: true, server: "other", want: unifiedjournal.RecoveryRetainAmbiguous},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := classifyStartupRecovery(recovery, item.inventory, item.authoritative, item.server); got != item.want {
				t.Fatalf("disposition=%s want=%s", got, item.want)
			}
		})
	}

	currentGeometryKey := recovery
	currentGeometryKey.Key.Incarnation = live.incarnation(final)
	if got := classifyStartupRecovery(currentGeometryKey, []startupPaneWitness{live}, true, "main"); got != unifiedjournal.RecoveryRetireReplacement {
		t.Fatalf("current-geometry incarnation was accepted: %s", got)
	}
}

func TestSourceStartupGeometryAmbiguityKeepsReconstructionAdmission(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux source inventory and recovery gate")
	}
	disposable := newDisposable(t)
	server := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	cfg := unifiedAdoptionDevConfig(t, server, t.TempDir(), 2)
	disposable.run("new-session", "-d", "-s", "source_recovery_geometry", "-x", "80", "-y", "24", "sh")
	sessionID := strings.TrimSpace(disposable.run("display-message", "-p", "-t", "=source_recovery_geometry:", "#{session_id}"))
	first, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.realm.Close()
	inventory, releaseInventory, err := first.startupPaneInventory(context.Background())
	defer releaseInventory()
	if err != nil {
		t.Fatalf("inventory=%v err=%v", inventory, err)
	}
	var live startupPaneWitness
	for _, candidate := range inventory {
		if candidate.session == sessionID {
			live = candidate
		}
	}
	if live.session == "" {
		t.Fatal("target missing from authoritative source inventory")
	}
	identity := recordingSourceKey{cfg.Realm, server.Label, live.socket, live.serverProcess, live.session, live.window, live.pane, live.paneProcess}.journalID()
	key := unifiedjournal.PaneKey{Server: live.server, Session: live.session, Window: live.window, Pane: live.pane, Incarnation: live.incarnation(live.geometry), ControlGeneration: 1}
	appendOutput := func(key unifiedjournal.PaneKey) {
		t.Helper()
		record, err := first.realm.Append(key, []byte("retained source bytes"))
		if err != nil {
			t.Fatal(err)
		}
		if err := first.realm.Sync(key); err != nil {
			t.Fatal(err)
		}
		if err := first.realm.AdvanceCommitted(key, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.realm.ReserveSource(key, identity); err != nil {
		t.Fatal(err)
	}
	if err := first.realm.AdmitPane(key, live.geometry); err != nil {
		t.Fatal(err)
	}
	appendOutput(key)
	capacity, err := first.realm.BeginRotationCapacity(key)
	if err != nil {
		t.Fatal(err)
	}
	successor := key
	successor.ControlGeneration++
	if err := first.realm.ReserveSource(successor, identity); err != nil {
		t.Fatal(err)
	}
	if _, err := first.realm.BeginRotatedPane(successor, live.geometry, capacity); err != nil {
		t.Fatal(err)
	}
	appendOutput(successor)
	// Both valid files survive loss before predecessor retirement. A resize
	// while no observer runs makes replay ambiguous without changing source.
	if err := first.realm.Close(); err != nil {
		t.Fatal(err)
	}
	disposable.run("resize-window", "-t", "="+live.session+":", "-x", "120", "-y", "40")
	second, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.realm.Close()
	if len(second.startupRecovery) != 2 {
		t.Fatalf("recovery outcomes=%v", second.startupRecovery)
	}
	for _, recovery := range second.startupRecovery {
		if recovery.Outcome != unifiedjournal.RecoveryRetainedAmbiguous {
			t.Fatalf("geometry drift changed replay policy: %v", recovery.Outcome)
		}
	}
	if used, unknown, _, leases := second.realm.SourceUsage(identity); used != 2 || unknown != 0 || leases != 2 {
		t.Fatalf("geometry drift lost proven source association: used=%d unknown=%d leases=%d", used, unknown, leases)
	}
	if slots := second.realm.AvailableCompletePaneSlots(); slots != 0 {
		t.Fatalf("fixture did not exhaust ordinary slots: %d", slots)
	}
	adoption := &unifiedDevAdoption{sessionID: live.session}
	if err := second.admitAdoptionSpawn(unifiedDevCommand{adoption: adoption}); err != nil {
		t.Fatalf("proven source cannot reach reconstruction at slot bound: %v", err)
	}
	second.abandonAdoption(adoption)
}

func TestSourceStartupRecoveryRecordIsTypedBoundedAndPathRedacted(t *testing.T) {
	record := startupRecoveryRecord(unifiedjournal.Recovery{
		Key:      unifiedjournal.PaneKey{Server: "main", Session: "$1", Window: "@2", Pane: "%3", ControlGeneration: 7},
		Decision: unifiedjournal.RecoveryRetainAmbiguous, Outcome: unifiedjournal.RecoveryRetainedAmbiguous,
		RetainedLogical: 916, RetainedPhysical: 1294, RetainedSlot: true,
	})
	for _, want := range []string{
		`event=startup_recovery`, `decision="retain_ambiguous"`, `result="retained_ambiguous"`,
		`retained_logical_bytes=916`, `retained_physical_bytes=1294`, `retained_slot=true`, `retry_needed=false`,
	} {
		if !strings.Contains(record, want) {
			t.Fatalf("record %q missing %q", record, want)
		}
	}
	for _, forbidden := range []string{".journal", "/runtime/", "terminal-content"} {
		if strings.Contains(record, forbidden) {
			t.Fatalf("record exposes forbidden value %q: %q", forbidden, record)
		}
	}
}

func rotationStartRecoveryBroker(t *testing.T, cfg config.Broker, effects *UnifiedDevPaneEffects) (net.Listener, <-chan error) {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join(shortSocketDir(t), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ServeWithPaneEffects(listener, cfg, effects) }()
	return listener, done
}

func rotationStopRecoveryBroker(t *testing.T, listener net.Listener, done <-chan error, effects *UnifiedDevPaneEffects) {
	t.Helper()
	_ = listener.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery broker did not stop")
	}
	effects.journalMu.Lock()
	if err := effects.realm.Close(); err != nil {
		effects.journalMu.Unlock()
		t.Fatal(err)
	}
	effects.journalMu.Unlock()
}

func rotationRecoveryAuthority(t *testing.T, cfg config.Broker, server config.TmuxServer, sessionID string) proto.Authority {
	t.Helper()
	detail, err := details(server, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = cfg.Realm, server.Label, detail.ID, detail.Created
	return authority
}

func TestSourceFitRestartKeepsExactGenerationAndFreshInput(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux fit/restart regression test")
	}
	disposable := newDisposable(t)
	server := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	runtimeDir := t.TempDir()
	cfg := unifiedAdoptionDevConfig(t, server, runtimeDir, 4)
	disposable.run("new-session", "-d", "-s", "startup_fit", "-x", "80", "-y", "24", "sh", "-c", "stty -echo; printf 'D11-BEFORE\\n'; exec sh")
	sessionID := strings.TrimSpace(disposable.run("display-message", "-p", "-t", "=startup_fit:", "#{session_id}"))
	authority := rotationRecoveryAuthority(t, cfg, server, sessionID)

	first, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstListener, firstDone := rotationStartRecoveryBroker(t, cfg, first)
	adoption, err := first.AdoptSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	fitConn, prepared, stream := unifiedE2E1OpenStream(t, firstListener.Addr().String(), authority)
	unifiedE2E1FitRequest(t, fitConn, prepared, 80, 34)
	stream.readUntil(func() bool { return stream.sawGeometry(80, 34) }, "committed 80x34 geometry")
	_ = fitConn.Close()
	first.journalMu.Lock()
	logicalBefore, logicalReservedBefore, _ := first.realm.LogicalBudget()
	physicalBefore, physicalReservedBefore, _ := first.realm.PhysicalBudget()
	slotsBefore := first.realm.AvailableCompletePaneSlots()
	first.journalMu.Unlock()
	files := unifiedE2E1JournalFiles(t, runtimeDir)
	if len(files) != 1 {
		t.Fatalf("pre-restart journal files=%v", files)
	}
	journalBefore, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	rotationStopRecoveryBroker(t, firstListener, firstDone, first)

	second, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.startupRecovery) != 1 {
		t.Fatalf("startup outcome ledger=%+v", second.startupRecovery)
	}
	startupRecord := startupRecoveryRecord(second.startupRecovery[0])
	if !strings.Contains(startupRecord, `decision="keep_exact"`) ||
		!strings.Contains(startupRecord, `result="kept_exact"`) || !strings.Contains(startupRecord, `retained_slot=true`) ||
		strings.Contains(startupRecord, runtimeDir) || strings.Contains(startupRecord, "D11-BEFORE") {
		t.Fatalf("startup outcome record=%q", startupRecord)
	}
	second.journalMu.Lock()
	recovered := second.realm.Recovered()
	logicalAfter, logicalReservedAfter, _ := second.realm.LogicalBudget()
	physicalAfter, physicalReservedAfter, _ := second.realm.PhysicalBudget()
	slotsAfter := second.realm.AvailableCompletePaneSlots()
	events, eventsErr := second.realm.ReadCommittedEvents(adoption.Key)
	second.journalMu.Unlock()
	if len(recovered) != 1 || recovered[0].Outcome != unifiedjournal.RecoveryKeptExact ||
		recovered[0].Decision != unifiedjournal.RecoveryKeepExact ||
		recovered[0].Initial != (unifiedjournal.Geometry{Columns: 80, Rows: 24}) ||
		recovered[0].Final != (unifiedjournal.Geometry{Columns: 80, Rows: 34}) {
		t.Fatalf("fit restart recovery=%+v", recovered)
	}
	if eventsErr != nil || len(events) == 0 || events[len(events)-1].Kind != unifiedjournal.RecordGeometry || events[len(events)-1].Geometry.Rows != 34 {
		t.Fatalf("replayed final geometry events=%+v err=%v", events, eventsErr)
	}
	if logicalAfter != logicalBefore || logicalReservedAfter != logicalReservedBefore ||
		physicalAfter != physicalBefore || physicalReservedAfter != physicalReservedBefore || slotsAfter != slotsBefore {
		t.Fatalf("fit restart ledger drift logical=%d/%d reserved=%d/%d physical=%d/%d held=%d/%d slots=%d/%d",
			logicalAfter, logicalBefore, logicalReservedAfter, logicalReservedBefore, physicalAfter, physicalBefore,
			physicalReservedAfter, physicalReservedBefore, slotsAfter, slotsBefore)
	}
	journalAfter, err := os.ReadFile(files[0])
	if err != nil || !bytes.Equal(journalAfter, journalBefore) {
		t.Fatalf("exact journal changed err=%v", err)
	}

	secondListener, secondDone := rotationStartRecoveryBroker(t, cfg, second)
	readopted, err := second.AdoptSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if readopted.Existing || readopted.Key == adoption.Key {
		t.Fatalf("restart did not mint fresh observer generation: %+v", readopted)
	}
	fresh, freshPrepared := unifiedE2E1OpenAttachment(t, secondListener.Addr().String(), authority)
	if freshPrepared.Columns != 80 || freshPrepared.Rows != 34 {
		t.Fatalf("fresh geometry=%dx%d", freshPrepared.Columns, freshPrepared.Rows)
	}
	unifiedE2E1FitInput(t, fresh, freshPrepared, "printf 'D11-AFTER-RESTART\\n'\n")
	unifiedE2E1AwaitPaneText(t, disposable, sessionID, "D11-AFTER-RESTART")
	_ = fresh.Close()
	rotationStopRecoveryBroker(t, secondListener, secondDone, second)
}

func TestSourceChangedPaneProcessIsReplacement(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux replacement regression test")
	}
	disposable := newDisposable(t)
	server := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	runtimeDir := t.TempDir()
	cfg := unifiedAdoptionDevConfig(t, server, runtimeDir, 4)
	disposable.run("new-session", "-d", "-s", "startup_replacement", "-x", "80", "-y", "24", "sh", "-c", "printf 'D11-OLD\\n'; exec sh")
	sessionID := strings.TrimSpace(disposable.run("display-message", "-p", "-t", "=startup_replacement:", "#{session_id}"))

	first, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstRegistry := newPaneRegistry(first)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = first.RunObserver(ctx, firstRegistry) }()
	if _, err := first.AdoptSession(context.Background(), sessionID); err != nil {
		t.Fatal(err)
	}
	before, err := buildSourceWitness(context.Background(), server, "", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := firstRegistry.Close(); err != nil {
		t.Fatal(err)
	}
	first.journalMu.Lock()
	if err := first.realm.Close(); err != nil {
		first.journalMu.Unlock()
		t.Fatal(err)
	}
	first.journalMu.Unlock()

	disposable.run("respawn-pane", "-k", "-t", "="+sessionID+":", "sh", "-c", "printf 'D11-NEW\\n'; exec sh")
	after, err := buildSourceWitness(context.Background(), server, "", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Pane == before.Pane {
		t.Fatalf("respawn did not change process witness: %+v", after.Pane)
	}

	second, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		second.journalMu.Lock()
		_ = second.realm.Close()
		second.journalMu.Unlock()
	}()
	second.journalMu.Lock()
	recovered := second.realm.Recovered()
	logical, logicalReserved, _ := second.realm.LogicalBudget()
	physical, physicalReserved, _ := second.realm.PhysicalBudget()
	second.journalMu.Unlock()
	if len(recovered) != 1 || recovered[0].Decision != unifiedjournal.RecoveryRetireReplacement ||
		recovered[0].Outcome != unifiedjournal.RecoveryRetiredStale || recovered[0].RetryNeeded ||
		recovered[0].RetainedLogical != 0 || recovered[0].RetainedPhysical != 0 || recovered[0].RetainedSlot {
		t.Fatalf("process replacement recovery=%+v", recovered)
	}
	if logical != 0 || logicalReserved != 0 || physical != 0 || physicalReserved != 0 {
		t.Fatalf("replacement retained ledgers logical=%d reserved=%d physical=%d held=%d", logical, logicalReserved, physical, physicalReserved)
	}
	if files := unifiedE2E1JournalFiles(t, runtimeDir); len(files) != 0 {
		t.Fatalf("replacement retained another process generation: %v", files)
	}
}
