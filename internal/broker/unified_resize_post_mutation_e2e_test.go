package broker

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/unifiedjournal"
)

// The advisor's post-mutation falsifier (2026-08-23 resize review, BLOCKER).
//
// Before this ruling a logical-cap refusal happened inside geometry Commit,
// AFTER the guarded resize command and witness recheck: tmux had already
// changed while the durable journal geometry had not, and the broker reported
// that as an operational non-mutation (resize_failed) and kept the attachment
// live at the old geometry. The journal's capacity for the geometry record and
// its commit is now reserved BEFORE the command is issued, so the same cap
// refuses the Fit with tmux untouched, the attachment live, and the generation
// still eligible — which is the only state resize_failed may ever describe.
//
// One deviation from the advisor's verbatim fixture: the logical cap is filled
// AFTER the control attachment opens, not before. Attaching the shadow client
// makes tmux redraw the pane (about 220 bytes of output), and a pane filled to
// zero headroom before that redraw fails closed on it — an ordinary output-cap
// fault, unrelated to the Fit — so the verbatim ordering could never observe an
// eligible generation after ANY correct fix. Filled after the attach, the pane
// is idle until the Fit, and the Fit's own reservation is what meets the cap.
// At the baseline commit this ordering is RED for the same root cause the
// advisor reported: tmux at 80x36 behind a resize_failed.
func TestAdvisorResizeFailureAfterTmuxMutationCannotKeepEpochLive(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux post-mutation resize falsifier")
	}
	saved := unifiedJournalCaps
	unifiedJournalCaps.pane = 64 << 10
	t.Cleanup(func() { unifiedJournalCaps = saved })

	disposable := newDisposable(t)
	disposable.run("new-session", "-d", "-s", "anchor", "-x", "80", "-y", "24", "sh")
	disposable.run("new-session", "-d", "-s", "0", "-x", "80", "-y", "24", "sh")
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	runtimeDir := t.TempDir()
	cfg := unifiedAdoptionDevConfig(t, tmuxServer, runtimeDir, 4)
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(shortSocketDir(t), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ServeWithPaneEffects(listener, cfg, effects) }()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("broker did not stop")
		}
	})

	sessionID := strings.TrimSpace(disposable.run("display-message", "-p", "-t", "=0:", "#{session_id}"))
	adoptConn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	unifiedE2E1Hello(t, adoptConn)
	if adopted := unifiedE2E1Control(t, adoptConn, proto.Control{Type: "adopt", ServerLabel: "main", SessionID: sessionID}); adopted.Type != "adopt_ok" {
		t.Fatalf("adopt=%+v", adopted)
	}
	_ = adoptConn.Close()

	key, ok := effects.paneKey(sessionID)
	if !ok {
		t.Fatal("adopted pane key missing")
	}
	detail, err := details(tmuxServer, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(tmuxServer)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = "e2e1", "main", detail.ID, detail.Created
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	birth, err := buildSourceWitness(ctx, tmuxServer, "", sessionID)
	if err != nil {
		t.Fatal(err)
	}

	conn, prepared, stream := unifiedE2E1OpenStream(t, listener.Addr().String(), authority)
	defer conn.Close()
	// Let the attach-time redraw reach the journal before measuring the charge;
	// the pane is idle from here until the Fit.
	unifiedE2E1InputUntilEchoed(t, conn, prepared, stream, "E2E1-BEFORE-CAP")
	time.Sleep(200 * time.Millisecond)

	effects.journalMu.Lock()
	events, err := effects.realm.ReadCommittedEvents(key)
	if err != nil {
		effects.journalMu.Unlock()
		t.Fatal(err)
	}
	var charge int64
	for _, event := range events {
		switch event.Kind {
		case unifiedjournal.RecordOutput:
			charge += int64(len(event.Payload))
		case unifiedjournal.RecordGeometry:
			charge += 128
		}
	}
	headroom := unifiedJournalCaps.pane - charge
	if headroom <= 0 {
		effects.journalMu.Unlock()
		t.Fatalf("initial charge=%d leaves no fixture headroom", charge)
	}
	record, err := effects.realm.Append(key, make([]byte, headroom))
	if err == nil {
		err = effects.realm.Sync(key)
	}
	if err == nil {
		err = effects.realm.AdvanceCommitted(key, record)
	}
	effects.journalMu.Unlock()
	if err != nil {
		t.Fatalf("fill logical pane cap: %v", err)
	}

	requestedRows := birth.Rows + 12
	unifiedE2E1FitRequest(t, conn, prepared, birth.Columns, requestedRows)
	geometry, control := unifiedE2E1FitOutcome(t, conn, stream, birth.Columns, requestedRows)
	if geometry || control == nil || control.Code != "resize_failed" {
		t.Fatalf("fit outcome geometry=%v control=%+v, want resize_failed", geometry, control)
	}
	after, err := buildSourceWitness(ctx, tmuxServer, "", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Rows != birth.Rows || after.Columns != birth.Columns {
		t.Fatalf("resize_failed kept attachment live after tmux mutated: before=%dx%d after=%dx%d", birth.Columns, birth.Rows, after.Columns, after.Rows)
	}
	if !effects.realm.UnifiedEligible(key) {
		t.Fatalf("resize_failed kept attachment live after its journal generation became untrusted (reason=%d)", effects.realm.Reason(key))
	}
	t.Logf("RECEIPT: logical cap met by the Fit's pre-issue reservation; tmux held at %dx%d; generation eligible", after.Columns, after.Rows)
}

// The physical ledger refuses a Fit exactly as the logical cap does: before
// any command is issued, as one request's outcome, with the generation still
// eligible. The logical budget here has megabytes of room; only the on-disk
// budget is exhausted.
func TestUnifiedResizePhysicalBudgetRefusesBeforeIssue(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux physical-budget resize refusal")
	}
	saved := unifiedJournalCaps
	unifiedJournalCaps.panePhysical = 256 << 10
	t.Cleanup(func() { unifiedJournalCaps = saved })
	fixture := newUnifiedResizeFixture(t, nil)
	key, ok := fixture.effects.paneKey(fixture.sessionID)
	if !ok {
		t.Fatal("pane key missing")
	}
	time.Sleep(200 * time.Millisecond)
	fixture.effects.journalMu.Lock()
	charged, cap := fixture.effects.realm.PanePhysical(key)
	// Leave less physical room than one geometry record (append + commit
	// frames) while the logical pane cap keeps megabytes of headroom.
	room := cap - charged
	fill := int(room) - 128 - 64
	if fill <= 0 {
		fixture.effects.journalMu.Unlock()
		t.Fatalf("fixture physical charge=%d cap=%d leaves no room to shape", charged, cap)
	}
	record, err := fixture.effects.realm.Append(key, make([]byte, fill))
	if err == nil {
		err = fixture.effects.realm.Sync(key)
	}
	if err == nil {
		err = fixture.effects.realm.AdvanceCommitted(key, record)
	}
	after, _ := fixture.effects.realm.PanePhysical(key)
	fixture.effects.journalMu.Unlock()
	if err != nil {
		t.Fatalf("shape physical budget: %v", err)
	}
	if cap-after >= 128 {
		t.Fatalf("fixture left %d bytes of physical room, want fewer than a geometry record", cap-after)
	}
	geometry, code := fixture.fit(t, fixture.birth.Rows+8)
	if geometry || code != "resize_failed" {
		t.Fatalf("physical refusal: geometry=%t code=%q want resize_failed", geometry, code)
	}
	height := strings.TrimSpace(fixture.disposable.run("display-message", "-p", "-t", "="+fixture.sessionID+":", "#{window_height}"))
	if height != "24" {
		t.Fatalf("a physically refused Fit mutated tmux: window height=%s", height)
	}
	if !fixture.effects.realm.UnifiedEligible(key) {
		t.Fatalf("a physical refusal invalidated the generation: reason=%d", fixture.effects.realm.Reason(key))
	}
	// The attachment is still open. (Echoing input would itself need journal
	// room the fixture deliberately left none of: output at a physical cap fails
	// closed exactly as output at the logical cap does.) A ping on the live
	// attachment loop answers; a closed one reads EOF.
	payload, err := proto.MarshalControl(proto.Control{Type: "ping"})
	if err != nil || proto.WriteFrame(fixture.conn, proto.FrameControl, payload) != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := fixture.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		raw, err := proto.ReadFrame(fixture.conn)
		if err != nil {
			t.Fatalf("attachment did not survive the physical refusal: %v", err)
		}
		if raw.Type != proto.FrameControl {
			continue
		}
		decoded, err := proto.DecodeControl(raw.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Type == "pong" {
			break
		}
		if decoded.Type == "error" || decoded.Type == "exit" {
			t.Fatalf("attachment ended after the physical refusal: %+v", decoded)
		}
	}
	t.Logf("RECEIPT: physical room=%d B, Fit refused pre-issue, tmux held at 80x24, attachment live", cap-after)
}

// Output that meets the physical cap fails closed exactly as the logical cap
// does — the generation faults with the physical reason — so a framing-heavy
// pane can never grow its file past the budget.
func TestUnifiedOutputPhysicalExhaustionFailsClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux physical-budget output fault")
	}
	saved := unifiedJournalCaps
	unifiedJournalCaps.panePhysical = 48 << 10
	t.Cleanup(func() { unifiedJournalCaps = saved })
	var mu sync.Mutex
	var faults []map[string]int64
	fixture := newUnifiedResizeFixture(t, func(effects *UnifiedDevPaneEffects) {
		effects.retentionObserve = func(event string, fields map[string]int64) {
			if event == "storage_fault" {
				mu.Lock()
				faults = append(faults, fields)
				mu.Unlock()
			}
		}
	})
	key, ok := fixture.effects.paneKey(fixture.sessionID)
	if !ok {
		t.Fatal("pane key missing")
	}
	// Enough output to cross the physical pane cap while the logical cap (the
	// journal default, megabytes) keeps ample room.
	unifiedE2E1FitInput(t, fixture.conn, fixture.prepared, "seq 1 30000\n")
	deadline := time.Now().Add(30 * time.Second)
	for {
		mu.Lock()
		count := len(faults)
		mu.Unlock()
		if count != 0 {
			break
		}
		if time.Now().After(deadline) {
			fixture.effects.journalMu.Lock()
			charged, cap := fixture.effects.realm.PanePhysical(key)
			fixture.effects.journalMu.Unlock()
			t.Fatalf("output never met the physical cap: charged=%d cap=%d", charged, cap)
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	first := faults[0]
	mu.Unlock()
	if first["storage_reason"] != int64(unifiedjournal.ReasonPhysicalQuota) || first["cap_winner"] != 1 {
		t.Fatalf("storage fault fields=%v want physical quota as cap winner", first)
	}
	fixture.effects.journalMu.Lock()
	charged, cap := fixture.effects.realm.PanePhysical(key)
	eligible := fixture.effects.realm.UnifiedEligible(key)
	reason := fixture.effects.realm.Reason(key)
	fixture.effects.journalMu.Unlock()
	if eligible || reason != unifiedjournal.ReasonPhysicalQuota || charged > cap {
		t.Fatalf("physical exhaustion did not fail closed: eligible=%t reason=%d charged=%d cap=%d", eligible, reason, charged, cap)
	}
	journals := unifiedE2E1JournalFiles(t, fixture.effects.dev.RuntimeDir)
	for _, journal := range journals {
		info, err := os.Stat(journal)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > cap {
			t.Fatalf("journal file %s size=%d exceeds the physical pane cap %d", journal, info.Size(), cap)
		}
	}
	t.Logf("RECEIPT: physical cap %d met by output; generation faulted closed (reason=%d); files within cap", cap, reason)
}
