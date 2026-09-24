package broker

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

// Resize failure regressions against a real tmux.
//
// A failure AFTER the guarded resize command was issued — the witness recheck,
// the attachment PTY resize, or the durable commit — ends the attachment
// fatally, because tmux may hold the new geometry while the journal does not;
// the generation is faulted so nothing further is published under the old
// grid. The shared fixture here is also used by the stale-target regression tests in
// unified_resize_stale_target_e2e_test.go.

// unifiedResizeFixture is one adopted session with a live Control attachment
// at its birth geometry.
type unifiedResizeFixture struct {
	disposable *disposable
	server     config.TmuxServer
	effects    *UnifiedDevPaneEffects
	socket     string
	sessionID  string
	authority  proto.Authority
	birth      terminal.SourceWitness
	conn       net.Conn
	prepared   terminal.Frame
	stream     *unifiedE2E1FitStream
}

func newUnifiedResizeFixture(t *testing.T, shape func(*UnifiedDevPaneEffects)) *unifiedResizeFixture {
	t.Helper()
	disposable := newDisposable(t)
	disposable.run("new-session", "-d", "-s", "anchor", "-x", "80", "-y", "24", "sh")
	disposable.run("new-session", "-d", "-s", "0", "-x", "80", "-y", "24", "sh")
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	cfg := unifiedAdoptionDevConfig(t, tmuxServer, t.TempDir(), 4)
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if shape != nil {
		shape(effects)
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
	detail, err := details(tmuxServer, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(tmuxServer)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = "e2e1", "main", detail.ID, detail.Created
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	birth, err := buildSourceWitness(ctx, tmuxServer, "", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &unifiedResizeFixture{
		disposable: disposable, server: tmuxServer, effects: effects, socket: listener.Addr().String(),
		sessionID: sessionID, authority: authority, birth: birth,
	}
	fixture.conn, fixture.prepared, fixture.stream = unifiedE2E1OpenStream(t, fixture.socket, authority)
	t.Cleanup(func() { _ = fixture.conn.Close() })
	unifiedE2E1InputUntilEchoed(t, fixture.conn, fixture.prepared, fixture.stream, "E2E1-RESIZE-FIXTURE-LIVE")
	return fixture
}

// fit sends one vertical Fit and reads until the attachment answers: a
// committed geometry, an error control, or the connection ending. The
// returned code is the error control's code, "exit" for an exit control, or
// "closed" when the broker ended the connection without one.
func (fixture *unifiedResizeFixture) fit(t *testing.T, rows int) (geometry bool, code string) {
	t.Helper()
	request, err := attachmentwire.Encode(terminal.Frame{
		Version: terminal.ProtocolVersion, Type: terminal.FrameResize,
		Source: fixture.prepared.Source, Epoch: fixture.prepared.Epoch, Columns: fixture.birth.Columns, Rows: rows,
	}, attachmentwire.BrowserToServer)
	if err != nil {
		t.Fatal(err)
	}
	// The write may race a close the broker already decided on; the read
	// below reports the verdict either way.
	_ = proto.WriteFrame(fixture.conn, proto.FrameAttachment, request)
	if err := fixture.conn.SetReadDeadline(time.Now().Add(40 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		raw, err := proto.ReadFrame(fixture.conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "closed") {
				return false, "closed"
			}
			t.Fatalf("waiting for fit verdict: %v (order=%v)", err, fixture.stream.order)
		}
		if raw.Type == proto.FrameControl {
			decoded, err := proto.DecodeControl(raw.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Type == "error" {
				return false, decoded.Code
			}
			if decoded.Type == "exit" {
				return false, "exit"
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
			fixture.stream.text = append(fixture.stream.text, frame.Data...)
		case frame.Type == terminal.FrameEnd:
			// The epoch ended; the broker's error control, if any, follows or
			// preceded it. Keep reading for the code until the socket closes.
		case frame.Type == terminal.FramePrepare && frame.Kind == terminal.CutResize:
			if frame.Columns == fixture.birth.Columns && frame.Rows == rows {
				return true, ""
			}
		}
	}
}

// closedByBroker proves the broker ended the attachment: the next read
// reports EOF within a bounded wait.
func (fixture *unifiedResizeFixture) closedByBroker(t *testing.T) bool {
	t.Helper()
	if err := fixture.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := proto.ReadFrame(fixture.conn); err != nil {
			return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "closed")
		}
	}
}

func (fixture *unifiedResizeFixture) witness(t *testing.T) (terminal.SourceWitness, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return buildSourceWitness(ctx, fixture.server, "", fixture.sessionID)
}

func isOperationalResizeCode(code string) bool {
	return code == "resize_failed" || code == "resize_rejected"
}

// Failures after the guarded command was issued are fatal. Each
// edge is reached through the transaction's own edge seam, and the failure
// inflicted there is real: tmux's pane set changes, or the journal generation
// is invalidated by an ordinary over-cap append. In every case tmux holds the
// requested geometry afterwards — the mutation happened — the attachment ends
// with attachment_failed, never resize_failed, and the generation is faulted
// so no byte is published under the old grid.
func TestUnifiedResizePostIssueFailureIsFatalAndFaultsGeneration(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux post-issue resize regression tests")
	}
	for _, variant := range []struct {
		name string
		edge string
		// inflict runs synchronously inside the transaction at the edge.
		inflict func(t *testing.T, fixture *unifiedResizeFixture)
		// faultEvent is the retention observation that proves the fault.
		faultEvent string
	}{
		{
			name: "pane changes before the witness recheck",
			edge: "issued",
			inflict: func(t *testing.T, fixture *unifiedResizeFixture) {
				fixture.disposable.run("split-window", "-d", "-t", "="+fixture.sessionID+":", "sh")
			},
			faultEvent: "geometry_fault",
		},
		{
			name: "journal generation dies before the durable commit",
			edge: "sized",
			inflict: func(t *testing.T, fixture *unifiedResizeFixture) {
				key, ok := fixture.effects.paneKey(fixture.sessionID)
				if !ok {
					t.Fatal("pane key missing")
				}
				fixture.effects.journalMu.Lock()
				defer fixture.effects.journalMu.Unlock()
				// An over-cap append is the journal's own fail-closed path.
				if _, err := fixture.effects.realm.Append(key, make([]byte, unifiedJournalCaps.pane+1)); err == nil {
					t.Fatal("over-cap append was admitted")
				}
			},
			faultEvent: "geometry_fault",
		},
	} {
		t.Run(variant.name, func(t *testing.T) {
			var mu sync.Mutex
			observed := map[string]int{}
			var fixture *unifiedResizeFixture
			edges := make(chan string, 8)
			fixture = newUnifiedResizeFixture(t, func(effects *UnifiedDevPaneEffects) {
				effects.retentionObserve = func(event string, _ map[string]int64) {
					mu.Lock()
					observed[event]++
					mu.Unlock()
				}
				effects.resizeEdge = func(session, edge string) {
					edges <- edge
					if edge == variant.edge {
						variant.inflict(t, fixture)
					}
				}
			})
			requested := fixture.birth.Rows + 8
			geometry, code := fixture.fit(t, requested)
			if geometry {
				t.Fatalf("%s: a post-issue failure committed a geometry", variant.name)
			}
			if isOperationalResizeCode(code) {
				t.Fatalf("%s: post-issue failure reported as operational %q", variant.name, code)
			}
			if code != "attachment_failed" {
				t.Fatalf("%s: verdict=%q want attachment_failed", variant.name, code)
			}
			if !fixture.closedByBroker(t) {
				t.Fatalf("%s: the broker kept the attachment open", variant.name)
			}
			reached := map[string]bool{}
			for len(edges) != 0 {
				reached[<-edges] = true
			}
			if !reached["issued"] || !reached[variant.edge] {
				t.Fatalf("%s: edges reached=%v, the command was never issued", variant.name, reached)
			}
			// The WINDOW carries the mutation: a split afterwards divides the
			// window's rows between panes, the window height stays requested.
			height := strings.TrimSpace(fixture.disposable.run("display-message", "-p", "-t", "="+fixture.sessionID+":", "#{window_height}"))
			if height != strconv.Itoa(requested) {
				t.Fatalf("%s: tmux window height=%s want %d: the fixture did not reach the post-mutation state", variant.name, height, requested)
			}
			after := terminal.SourceWitness{Columns: fixture.birth.Columns, Rows: requested}
			deadline := time.Now().Add(10 * time.Second)
			for {
				mu.Lock()
				faults := observed[variant.faultEvent]
				mu.Unlock()
				if faults != 0 {
					break
				}
				if time.Now().After(deadline) {
					mu.Lock()
					t.Fatalf("%s: generation was not faulted after the post-issue failure: observations=%v", variant.name, observed)
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Logf("RECEIPT %s: tmux=%dx%d (mutated), verdict=%q, generation faulted via %s", variant.name, after.Columns, after.Rows, code, variant.faultEvent)
		})
	}
}

// The exec path (no issuer) classifies the same way at the transaction layer:
// a guarded command that ran and then cannot be followed through — here the
// pane set changes between the command and the witness recheck — is untyped,
// while a request the transaction rejects before running anything is a typed
// refusal.
func TestTmuxPinnedTransactionResizeTypesOnlyProvenPreIssueRefusals(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux transaction-layer resize classification")
	}
	disposable := newDisposable(t)
	disposable.run("new-session", "-d", "-s", "0", "-x", "80", "-y", "24", "sh")
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	sessionID := strings.TrimSpace(disposable.run("display-message", "-p", "-t", "=0:", "#{session_id}"))
	detail, err := details(tmuxServer, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(tmuxServer)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = "e2e1", "main", detail.ID, detail.Created
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	witness, err := buildSourceWitness(ctx, tmuxServer, "", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	txn := &tmuxPinnedTransaction{server: tmuxServer, authority: authority, pty: newDelayedPTY()}
	txn.pty.setConsumer(func([]byte) error { return nil }, nil)
	bound, err := txn.RunPinned(ctx, terminal.TransactionRequest{Action: terminal.ActionBind, Witness: witness})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	t.Cleanup(func() {
		_, _ = txn.RunPinned(context.Background(), terminal.TransactionRequest{Action: terminal.ActionCleanup, Witness: witness, Attachment: bound.Attachment})
	})

	// Malformed: rejected before anything runs.
	_, err = txn.RunPinned(ctx, terminal.TransactionRequest{Action: terminal.ActionResize, Witness: witness, Attachment: bound.Attachment, Columns: 80, Rows: 0})
	if !terminal.IsResizeRefusal(err) {
		t.Fatalf("malformed request err=%v want a typed refusal", err)
	}

	// Post-issue: the guard passes and tmux resizes; a pane is then added
	// behind the bound one before the witness recheck, which can no longer
	// confirm the requested geometry on the bound pane.
	edges := make(chan string, 8)
	txn.resizeEdge = func(edge string) {
		edges <- edge
		if edge == "issued" {
			disposable.run("split-window", "-d", "-t", "="+sessionID+":", "sh")
		}
	}
	_, err = txn.RunPinned(ctx, terminal.TransactionRequest{Action: terminal.ActionResize, Witness: witness, Attachment: bound.Attachment, Columns: 80, Rows: 32})
	if err == nil || terminal.IsResizeRefusal(err) {
		t.Fatalf("post-issue witness failure err=%v must be untyped", err)
	}
	if !strings.Contains(err.Error(), "post-issue witness recheck") {
		t.Fatalf("post-issue failure does not name its edge: %v", err)
	}
	if len(edges) == 0 || <-edges != "issued" {
		t.Fatal("the command was never issued")
	}
	height := strings.TrimSpace(disposable.run("display-message", "-p", "-t", "="+sessionID+":", "#{window_height}"))
	if height != "32" {
		t.Fatalf("window height=%s: the fixture did not mutate before the witness edge", height)
	}
}
