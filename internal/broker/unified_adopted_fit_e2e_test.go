package broker

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

const unifiedE2E1AdoptedFitRed = "E2E1/ADOPTED_EXPLICIT_VERTICAL_FIT"

// unifiedE2E1FitOutcome drains the attachment until either a committed RESIZE
// geometry event for the requested rows or an error control arrives. Unlike
// readUntil it records error controls instead of skipping them, because the
// outcome under test IS the control frame.
func unifiedE2E1FitOutcome(t *testing.T, conn net.Conn, stream *unifiedE2E1FitStream, columns, rows int) (geometry bool, control *proto.Control) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(40 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		raw, err := proto.ReadFrame(conn)
		if err != nil {
			t.Fatalf("%s: waiting for fit outcome: %v (order=%v)", unifiedE2E1AdoptedFitRed, err, stream.order)
		}
		if raw.Type == proto.FrameControl {
			decoded, err := proto.DecodeControl(raw.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Type == "error" || decoded.Type == "exit" {
				return false, &decoded
			}
			continue
		}
		if raw.Type != proto.FrameAttachment {
			continue
		}
		frame, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case frame.Type == terminal.FrameLive:
			stream.text = append(stream.text, frame.Data...)
			stream.order = append(stream.order, "OUTPUT")
		case frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutResize:
			stream.order = append(stream.order, "GEOMETRY")
			if frame.Columns == columns && frame.Rows == rows {
				return true, nil
			}
		}
	}
}

// seedFramingHeavyJournal grows an adopted session's journal FILE past the pane
// cap on framing overhead alone — hundreds of one-byte output records, each
// costing 128 bytes of framing the live charge never counts — the shape a
// long-lived, chatty agent shell reaches in a day. Records go through the realm
// under the provider's journal lock, exactly as the retention runtime's own
// appends do, so the realm stays the one sequence authority.
func seedFramingHeavyJournal(t *testing.T, effects *UnifiedDevPaneEffects, sessionID, runtimeDir string, cap int64) int {
	t.Helper()
	key, ok := effects.paneKey(sessionID)
	if !ok {
		t.Fatalf("%s: adopted session %s has no journal pane", unifiedE2E1AdoptedFitRed, sessionID)
	}
	journalSize := func() int64 {
		journals := unifiedE2E1JournalFiles(t, runtimeDir)
		if len(journals) != 1 {
			t.Fatalf("%s: %d pane journals, want 1", unifiedE2E1AdoptedFitRed, len(journals))
		}
		info, err := os.Stat(journals[0])
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	records := 0
	effects.journalMu.Lock()
	defer effects.journalMu.Unlock()
	for journalSize() <= cap {
		record, err := effects.realm.Append(key, []byte{'x'})
		if err == nil {
			err = effects.realm.Sync(key)
		}
		if err == nil {
			err = effects.realm.AdvanceCommitted(key, record)
		}
		if err != nil {
			t.Fatalf("%s: seed record %d: %v", unifiedE2E1AdoptedFitRed, records, err)
		}
		records++
	}
	return records
}

// TestUnifiedTerminalE2E1AdoptedExplicitVerticalFit is the operator's iPhone
// report replayed against a real tmux: a session born outside the unified
// engine, adopted with one click, then fitted vertically from its Control
// attachment. The second case is the measured production shape — a journal
// whose file outgrew the pane cap on framing while its charge had headroom —
// where the first Fit used to come back as resize_failed and take the
// attachment down with it.
// unifiedE2E1InputUntilEchoed proves the attachment still accepts input after
// a geometry event. The committed GEOMETRY event reaches the browser before
// the epoch's own RESIZE cut has been acknowledged server-side, so one frame
// typed immediately after it can land inside the cut window and be refused
// (a typed refusal that drops the frame, never queues it). That window is a
// few milliseconds; a bounded retry with a unique token per attempt shows the
// attachment is live without depending on where the window fell.
func unifiedE2E1InputUntilEchoed(t *testing.T, conn net.Conn, prepared terminal.Frame, stream *unifiedE2E1FitStream, token string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		unifiedE2E1FitInput(t, conn, prepared, "printf '"+token+"\\n'\n")
		if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		for !bytes.Contains(stream.text, []byte(token)) {
			raw, err := proto.ReadFrame(conn)
			if err != nil {
				break
			}
			if raw.Type != proto.FrameAttachment {
				continue
			}
			frame, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
			if err != nil {
				t.Fatal(err)
			}
			if frame.Type == terminal.FrameLive {
				stream.text = append(stream.text, frame.Data...)
				stream.order = append(stream.order, "OUTPUT")
			}
		}
		if bytes.Contains(stream.text, []byte(token)) {
			return
		}
	}
	t.Fatalf("%s: input was never echoed after the fit: order=%v", unifiedE2E1AdoptedFitRed, stream.order)
}

func TestUnifiedTerminalE2E1AdoptedExplicitVerticalFit(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux adopted explicit vertical fit")
	}
	for _, variant := range []struct {
		name         string
		framingHeavy bool
	}{
		{"fresh adoption", false},
		{"journal file past the pane cap on framing", true},
	} {
		t.Run(variant.name, func(t *testing.T) {
			runAdoptedExplicitVerticalFit(t, variant.framingHeavy)
		})
	}
}

func runAdoptedExplicitVerticalFit(t *testing.T, framingHeavy bool) {
	t.Helper()
	if framingHeavy {
		saved := unifiedJournalCaps
		unifiedJournalCaps.pane = 64 << 10
		t.Cleanup(func() { unifiedJournalCaps = saved })
	}
	disposable := newDisposable(t)
	disposable.run("new-session", "-d", "-s", "anchor", "-x", "80", "-y", "24", "sh")
	// The operator's session: born outside the unified engine, the ordinary way.
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
			t.Error("unified broker did not stop")
		}
	})

	sessionID := strings.TrimSpace(disposable.run("display-message", "-p", "-t", "=0:", "#{session_id}"))
	adoptConn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	unifiedE2E1Hello(t, adoptConn)
	if adopted := unifiedE2E1Control(t, adoptConn, proto.Control{Type: "adopt", ServerLabel: "main", SessionID: sessionID}); adopted.Type != "adopt_ok" {
		t.Fatalf("%s: adopt=%+v", unifiedE2E1AdoptedFitRed, adopted)
	}
	_ = adoptConn.Close()
	seeded := 0
	if framingHeavy {
		seeded = seedFramingHeavyJournal(t, effects, sessionID, runtimeDir, unifiedJournalCaps.pane)
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
	if prepared.Columns != birth.Columns || prepared.Rows != birth.Rows {
		t.Fatalf("%s: initial PREPARE geometry=%dx%d want adoption=%dx%d",
			unifiedE2E1AdoptedFitRed, prepared.Columns, prepared.Rows, birth.Columns, birth.Rows)
	}
	unifiedE2E1FitInput(t, conn, prepared, "printf 'E2E1-BEFORE-FIT\\n'\n")
	stream.readUntil(func() bool { return bytes.Contains(stream.text, []byte("E2E1-BEFORE-FIT")) }, "pre-fit output")

	requestedRows := birth.Rows + 24
	unifiedE2E1FitRequest(t, conn, prepared, birth.Columns, requestedRows)
	geometry, control := unifiedE2E1FitOutcome(t, conn, stream, birth.Columns, requestedRows)
	after, witnessErr := buildSourceWitness(ctx, tmuxServer, "", sessionID)
	if !geometry {
		t.Fatalf("%s: adopted fit did not commit (seeded_records=%d tmux_after=%dx%d witness_err=%v): control=%+v",
			unifiedE2E1AdoptedFitRed, seeded, after.Columns, after.Rows, witnessErr, control)
	}
	if witnessErr != nil || after.Columns != birth.Columns || after.Rows != requestedRows {
		t.Fatalf("%s: tmux geometry=%dx%d want %dx%d: %v", unifiedE2E1AdoptedFitRed, after.Columns, after.Rows, birth.Columns, requestedRows, witnessErr)
	}
	// The attachment is still live after the fit, and the journal recorded it.
	unifiedE2E1InputUntilEchoed(t, conn, prepared, stream, "E2E1-AFTER-FIT")
	journals := unifiedE2E1JournalFiles(t, runtimeDir)
	if len(journals) != 1 {
		t.Fatalf("%s: %d pane journals, want 1", unifiedE2E1AdoptedFitRed, len(journals))
	}
	if _, geometryRecords := unifiedE2E1JournalGeometry(t, journals[0]); geometryRecords != 1 {
		t.Fatalf("%s: durable geometry records=%d want 1", unifiedE2E1AdoptedFitRed, geometryRecords)
	}
	t.Logf("%s RECEIPT: framing_heavy=%t seeded_records=%d adoption=%dx%d fitted=%dx%d order=%v",
		unifiedE2E1AdoptedFitRed, framingHeavy, seeded, birth.Columns, birth.Rows, after.Columns, after.Rows, stream.order)
	unifiedE2E1Detach(t, conn)
}

// TestUnifiedTerminalE2E1ResizeFailureKeepsTheAttachment proves against real tmux that a Fit whose
// guarded transaction is refused before any mutation comes back as an in-band
// resize_failed control, and NOTHING else changes — the epoch is live, the
// attachment stays open, the tmux geometry is untouched, input still echoes,
// and a later Fit on the same attachment applies.
func TestUnifiedTerminalE2E1ResizeFailureKeepsTheAttachment(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux resize failure continuity")
	}
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
	var refuse atomic.Bool
	refusals := 0
	effects.geometryRefusal = func(string) error {
		if refuse.Load() {
			refusals++
			return errors.New("injected pre-mutation refusal")
		}
		return nil
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
			t.Error("unified broker did not stop")
		}
	})
	sessionID := strings.TrimSpace(disposable.run("display-message", "-p", "-t", "=0:", "#{session_id}"))
	adoptConn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	unifiedE2E1Hello(t, adoptConn)
	if adopted := unifiedE2E1Control(t, adoptConn, proto.Control{Type: "adopt", ServerLabel: "main", SessionID: sessionID}); adopted.Type != "adopt_ok" {
		t.Fatalf("%s: adopt=%+v", unifiedE2E1AdoptedFitRed, adopted)
	}
	_ = adoptConn.Close()
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
	unifiedE2E1FitInput(t, conn, prepared, "printf 'E2E1-BEFORE\\n'\n")
	stream.readUntil(func() bool { return bytes.Contains(stream.text, []byte("E2E1-BEFORE")) }, "pre-failure output")

	refuse.Store(true)
	requestedRows := birth.Rows + 16
	unifiedE2E1FitRequest(t, conn, prepared, birth.Columns, requestedRows)
	geometry, control := unifiedE2E1FitOutcome(t, conn, stream, birth.Columns, requestedRows)
	if geometry || control == nil || control.Type != "error" || control.Code != "resize_failed" {
		t.Fatalf("%s: refused fit outcome geometry=%t control=%+v", unifiedE2E1AdoptedFitRed, geometry, control)
	}
	if refusals != 1 {
		t.Fatalf("%s: refusals=%d want 1", unifiedE2E1AdoptedFitRed, refusals)
	}
	held, err := buildSourceWitness(ctx, tmuxServer, "", sessionID)
	if err != nil || held.Columns != birth.Columns || held.Rows != birth.Rows {
		t.Fatalf("%s: a refused fit moved the geometry to %dx%d: %v", unifiedE2E1AdoptedFitRed, held.Columns, held.Rows, err)
	}
	// The attachment is still open on the same connection, same epoch.
	unifiedE2E1InputUntilEchoed(t, conn, prepared, stream, "E2E1-AFTER-REFUSED-FIT")

	// And the same attachment can still fit once the cause is gone.
	refuse.Store(false)
	unifiedE2E1FitRequest(t, conn, prepared, birth.Columns, requestedRows)
	geometry, control = unifiedE2E1FitOutcome(t, conn, stream, birth.Columns, requestedRows)
	if !geometry {
		t.Fatalf("%s: fit after a refused fit did not commit: control=%+v", unifiedE2E1AdoptedFitRed, control)
	}
	after, err := buildSourceWitness(ctx, tmuxServer, "", sessionID)
	if err != nil || after.Columns != birth.Columns || after.Rows != requestedRows {
		t.Fatalf("%s: tmux geometry=%dx%d want %dx%d: %v", unifiedE2E1AdoptedFitRed, after.Columns, after.Rows, birth.Columns, requestedRows, err)
	}
	unifiedE2E1InputUntilEchoed(t, conn, prepared, stream, "E2E1-AFTER-SECOND-FIT")
	t.Logf("%s RECEIPT: refused fit -> in-band resize_failed, geometry held at %dx%d, input echoed, later fit -> %dx%d, order=%v",
		unifiedE2E1AdoptedFitRed, held.Columns, held.Rows, after.Columns, after.Rows, stream.order)
	unifiedE2E1Detach(t, conn)
}

// shortSocketDir returns a short temp directory for AF_UNIX sockets: t.TempDir
// embeds the full test name, and this test's subtest names push the socket path
// past the 107-byte sun_path limit under the deploy's hermetic TMPDIR prefix
// (bind: invalid argument). Mirrors frontdoor's shortTestDir; issue #10 tracks
// the package-wide hygiene.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ptb-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
