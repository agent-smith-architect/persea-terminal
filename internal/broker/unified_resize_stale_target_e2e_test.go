package broker

import (
	"strings"
	"testing"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

// Resize review (2026-08-23) falsifiers against a real tmux — finding 2.
//
// A Fit on a source that can no longer be witnessed as the one this attachment
// bound — server gone, session replaced, pane changed, columns moved — is a
// verdict on the attachment (fatal, closes), never an operational refusal that
// leaves a dead attachment open behind a passing notice. The fixture lives in
// unified_resize_post_issue_e2e_test.go.

// Finding 2: identity drift underneath a live attachment is fatal at the
// resize gate, and the gate says so with stale_target before anything is
// issued; tmux geometry is untouched by the refused request.
func TestUnifiedResizeStaleTargetIsFatalNotOperational(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux stale-target resize falsifiers")
	}
	for _, variant := range []struct {
		name  string
		drift func(t *testing.T, fixture *unifiedResizeFixture)
		want  map[string]bool
	}{
		{
			name: "pane change",
			drift: func(t *testing.T, fixture *unifiedResizeFixture) {
				// The new pane takes focus: the session's active pane is no
				// longer the one this attachment bound.
				fixture.disposable.run("split-window", "-t", "="+fixture.sessionID+":", "sh")
			},
			want: map[string]bool{"stale_target": true},
		},
		{
			name: "pane added behind the bound pane",
			drift: func(t *testing.T, fixture *unifiedResizeFixture) {
				// The bound pane stays active, so the gate's witness still
				// matches; the transaction's own window_panes guard refuses
				// inside tmux, which the post-issue recheck cannot tell from a
				// mutation. Fatal either way; never a refusal.
				fixture.disposable.run("split-window", "-d", "-t", "="+fixture.sessionID+":", "sh")
			},
			want: map[string]bool{"stale_target": true, "attachment_failed": true},
		},
		{
			name: "unexpected column change",
			drift: func(t *testing.T, fixture *unifiedResizeFixture) {
				fixture.disposable.run("resize-window", "-t", "="+fixture.sessionID+":", "-x", "100", "-y", "24")
			},
			want: map[string]bool{"stale_target": true},
		},
		{
			name: "session replacement",
			drift: func(t *testing.T, fixture *unifiedResizeFixture) {
				fixture.disposable.run("kill-session", "-t", "="+fixture.sessionID)
				fixture.disposable.run("new-session", "-d", "-s", "0", "-x", "80", "-y", "24", "sh")
			},
			// The replaced session also tears the attachment's own shadow
			// client down, so the epoch may fault on its PTY before the Fit is
			// judged; either verdict is fatal, and neither is a refusal.
			want: map[string]bool{"stale_target": true, "attachment_failed": true, "generation_failed": true, "closed": true, "exit": true},
		},
		{
			name: "server disappearance",
			drift: func(t *testing.T, fixture *unifiedResizeFixture) {
				fixture.disposable.run("kill-server")
			},
			want: map[string]bool{"stale_target": true, "attachment_failed": true, "closed": true, "exit": true},
		},
	} {
		t.Run(variant.name, func(t *testing.T) {
			fixture := newUnifiedResizeFixture(t, nil)
			variant.drift(t, fixture)
			geometry, code := fixture.fit(t, fixture.birth.Rows+8)
			if geometry {
				t.Fatalf("%s: a Fit on a drifted source committed a geometry", variant.name)
			}
			if isOperationalResizeCode(code) {
				t.Fatalf("%s: drifted source answered with operational %q; the attachment is dead behind a notice", variant.name, code)
			}
			if !variant.want[code] {
				t.Fatalf("%s: verdict=%q want one of %v", variant.name, code, variant.want)
			}
			if !fixture.closedByBroker(t) {
				t.Fatalf("%s: the broker kept the attachment open after %q", variant.name, code)
			}
			if variant.name == "pane change" || variant.name == "unexpected column change" || variant.name == "pane added behind the bound pane" {
				height := strings.TrimSpace(fixture.disposable.run("display-message", "-p", "-t", "="+fixture.sessionID+":", "#{window_height}"))
				if height != "24" {
					t.Fatalf("%s: the refused Fit changed the window height to %s", variant.name, height)
				}
			}
			t.Logf("RECEIPT %s: verdict=%q, attachment closed", variant.name, code)
		})
	}
}

// The request-policy refusal stays operational: the browser disagreeing with
// an UNCHANGED pane about its columns is one request's outcome, and the
// attachment carries on to a correct Fit.
func TestUnifiedResizeBrowserColumnMismatchStaysOperational(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux request-policy resize falsifier")
	}
	fixture := newUnifiedResizeFixture(t, nil)
	wrong, err := attachmentwire.Encode(terminal.Frame{
		Version: terminal.ProtocolVersion, Type: terminal.FrameResize,
		Source: fixture.prepared.Source, Epoch: fixture.prepared.Epoch, Columns: fixture.birth.Columns + 1, Rows: fixture.birth.Rows + 8,
	}, attachmentwire.BrowserToServer)
	if err != nil || proto.WriteFrame(fixture.conn, proto.FrameAttachment, wrong) != nil {
		t.Fatal(err)
	}
	geometry, control := unifiedE2E1FitOutcome(t, fixture.conn, fixture.stream, fixture.birth.Columns+1, fixture.birth.Rows+8)
	if geometry || control == nil || control.Code != "resize_rejected" {
		t.Fatalf("browser column mismatch: geometry=%t control=%+v want resize_rejected", geometry, control)
	}
	unifiedE2E1InputUntilEchoed(t, fixture.conn, fixture.prepared, fixture.stream, "E2E1-AFTER-REJECTED")
	if geometry, code := fixture.fit(t, fixture.birth.Rows+8); !geometry {
		t.Fatalf("a correct Fit after a rejected one did not commit: %q", code)
	}
}
