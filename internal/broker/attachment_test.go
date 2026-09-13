package broker

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"persea-terminal/internal/terminal"
)

func TestDelayedPTYUnexpectedEOFIsFaultAndCleanupEOFIsNot(t *testing.T) {
	unexpected := newDelayedPTY()
	faults := make(chan error, 1)
	unexpected.setConsumer(func([]byte) error { return nil }, func(err error) { faults <- err })
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = unexpected.install(r); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	select {
	case err := <-faults:
		if err != io.EOF {
			t.Fatalf("unexpected EOF fault=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unexpected PTY EOF was suppressed")
	}

	clean := newDelayedPTY()
	cleanFault := make(chan error, 1)
	clean.setConsumer(func([]byte) error { return nil }, func(err error) { cleanFault <- err })
	r2, w2, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = clean.install(r2); err != nil {
		t.Fatal(err)
	}
	clean.seal()
	_ = w2.Close()
	select {
	case err := <-cleanFault:
		t.Fatalf("cleanup EOF faulted: %v", err)
	case <-clean.done:
	}
}

func TestCleanupWrongIDsPreservesExactOwner(t *testing.T) {
	witness, err := (procProbe{}).Witness(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	real := terminal.AttachmentIDs{ShadowSessionID: "$91", ClientID: "client-real"}
	txn := &tmuxPinnedTransaction{pty: newDelayedPTY(), client: &attachmentClient{ids: real}}
	_, err = txn.cleanup(context.Background(), terminal.TransactionRequest{Witness: terminal.SourceWitness{Server: witness}, Attachment: terminal.AttachmentIDs{ShadowSessionID: "$92", ClientID: "client-wrong"}})
	if err == nil {
		t.Fatal("mismatched cleanup accepted")
	}
	txn.mu.Lock()
	kept := txn.client
	txn.mu.Unlock()
	if kept == nil || kept.ids != real {
		t.Fatal("mismatched cleanup discarded the real owner")
	}
}

func TestDistinctSessionsOnOneBootHaveDistinctOpaqueSourceIDs(t *testing.T) {
	d := newDisposable(t)
	d.run("new-session", "-d", "-s", "beta", "sleep", "600")
	inc, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	a, err := buildSourceWitness(context.Background(), d.tmux, inc.BootID, "$0")
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildSourceWitness(context.Background(), d.tmux, inc.BootID, "$1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Incarnation == inc.BootID || a.Incarnation == b.Incarnation {
		t.Fatalf("source incarnations are not opaque and unique: %q %q", a.Incarnation, b.Incarnation)
	}
}

func TestExplicitResizeIsExactSinglePaneAndSplitPaneFailsWithoutMutation(t *testing.T) {
	d := newDisposable(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	authority, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := details(d.tmux, "$0")
	if err != nil {
		t.Fatal(err)
	}
	authority.SessionID, authority.SessionCreated = detail.ID, detail.Created
	witness, err := buildSourceWitness(ctx, d.tmux, authority.BootID, authority.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	delayed := newDelayedPTY()
	delayed.setConsumer(func([]byte) error { return nil }, func(error) {})
	txn := &tmuxPinnedTransaction{server: d.tmux, bootID: authority.BootID, authority: authority, pty: delayed}
	bound, err := txn.RunPinned(ctx, terminal.TransactionRequest{Action: terminal.ActionBind, Witness: witness})
	if err != nil {
		t.Fatal(err)
	}
	currentWitness, attachment := bound.Witness, bound.Attachment
	afterBind, err := buildSourceWitness(ctx, d.tmux, authority.BootID, authority.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if afterBind != currentWitness {
		t.Fatalf("attachment bind mutated the pinned source: before=%+v after=%+v", currentWitness, afterBind)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = txn.RunPinned(cleanupCtx, terminal.TransactionRequest{
			Action: terminal.ActionCleanup, Witness: currentWitness, Attachment: attachment,
		})
	}()

	resized, err := txn.RunPinned(ctx, terminal.TransactionRequest{
		Action: terminal.ActionResize, Witness: currentWitness, Attachment: attachment, Columns: 97, Rows: 31,
	})
	if err != nil {
		t.Fatalf("guarded resize: %v; tmux=%s", err, d.run(
			"display-message", "-p", "-t", currentWitness.PaneID,
			"#{session_id}|#{window_id}|#{pane_id}|#{pane_pid}|#{pane_width}|#{pane_height}|#{window_panes}|#{session_created}|#{pid}",
		))
	}
	currentWitness = resized.Witness
	if currentWitness.Columns != 97 || currentWitness.Rows != 31 ||
		d.run("display-message", "-p", "-t", "="+authority.SessionID+":", "#{window_width}x#{window_height}") != "97x31" {
		t.Fatalf("resize did not publish exact geometry: %+v", currentWitness)
	}

	staleWitness := currentWitness
	staleWitness.Pane.StartTime++
	beforeStale := d.run("display-message", "-p", "-t", "="+authority.SessionID+":", "#{window_width}x#{window_height}")
	if _, err := txn.RunPinned(ctx, terminal.TransactionRequest{
		Action: terminal.ActionResize, Witness: staleWitness, Attachment: attachment, Columns: 103, Rows: 33,
	}); err == nil {
		t.Fatal("stale process witness resize was accepted")
	}
	afterStale := d.run("display-message", "-p", "-t", "="+authority.SessionID+":", "#{window_width}x#{window_height}")
	if afterStale != beforeStale {
		t.Fatalf("rejected stale-witness resize mutated geometry: before=%s after=%s", beforeStale, afterStale)
	}

	d.run("split-window", "-d", "-t", currentWitness.PaneID, "sleep 600")
	splitWitness, err := buildSourceWitness(ctx, d.tmux, authority.BootID, authority.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	before := d.run("display-message", "-p", "-t", "="+authority.SessionID+":", "#{window_width}x#{window_height}")
	if _, err := txn.RunPinned(ctx, terminal.TransactionRequest{
		Action: terminal.ActionResize, Witness: splitWitness, Attachment: attachment, Columns: 111, Rows: 37,
	}); err == nil {
		t.Fatal("multi-pane resize was accepted")
	}
	after := d.run("display-message", "-p", "-t", "="+authority.SessionID+":", "#{window_width}x#{window_height}")
	if after != before {
		t.Fatalf("rejected multi-pane resize mutated geometry: before=%s after=%s", before, after)
	}
	currentWitness = splitWitness
}
