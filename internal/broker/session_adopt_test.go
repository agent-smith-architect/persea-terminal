package broker

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"persea-terminal/internal/proto"
)

func adoptControl(t *testing.T, server *Server, serverLabel, sessionID string) proto.Control {
	t.Helper()
	var output bytes.Buffer
	server.adopt(&lockedWriter{w: &output}, proto.Control{Type: "adopt", ServerLabel: serverLabel, SessionID: sessionID})
	frame, err := proto.ReadFrame(&output)
	if err != nil {
		t.Fatal(err)
	}
	control, err := proto.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return control
}

// The refusal-code map is the wire contract the projection layer keys on:
// every typed provider error must reach the browser as its own closed code,
// and an untyped error must degrade to adopt_failed rather than leak text.
func TestAdoptRefusalCodeMapsEveryTypedError(t *testing.T) {
	for want, err := range map[string]error{
		"session_gone":         ErrUnifiedAdoptSessionMissing,
		"blocked_alt_screen":   ErrUnifiedAdoptAlternateScreen,
		"blocked_multi_window": ErrUnifiedAdoptMultiWindow,
		"blocked_multi_pane":   ErrUnifiedAdoptMultiPane,
		"slots_exhausted":      ErrUnifiedAdoptSlotsExhausted,
		"adoption_in_progress": ErrUnifiedAdoptInProgress,
		"adoption_contended":   ErrUnifiedAdoptUnstable,
		"adopt_failed":         errors.New("something untyped"),
	} {
		if got := adoptRefusalCode(err); got != want {
			t.Fatalf("adoptRefusalCode(%v) = %q, want %q", err, got, want)
		}
	}
	if got := adoptRefusalCode(errors.Join(ErrUnifiedAdoptSessionMissing, errors.New("wrapped detail"))); got != "session_gone" {
		t.Fatalf("wrapped typed error lost its code: %q", got)
	}
}

func TestAdoptControlOpRefusesWithoutProviderOrForeignServer(t *testing.T) {
	bare := &Server{}
	if got := adoptControl(t, bare, "main", "$1"); got.Type != "adopt_refused" || got.Code != "unified_unavailable" {
		t.Fatalf("providerless adopt = %+v", got)
	}
	withProvider := &Server{unified: newProjectionEffects(t, 0)}
	if got := adoptControl(t, withProvider, "other", "$1"); got.Type != "adopt_refused" || got.Code != "not_permitted" {
		t.Fatalf("foreign-server adopt = %+v", got)
	}
}

// The inventory stamps a per-session unified state from ONE list-sessions
// call whose trailing eligibility fields expand against each session's active
// window and pane — pinned here against real tmux, because the projection is
// only as honest as that measured expansion.
func TestInventoryStampsPerSessionUnifiedStates(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux inventory lifecycle")
	}
	fixture := newAdoptionFixture(t, 0)
	server := &Server{config: fixture.cfg, unified: fixture.effects}
	eligibleID := fixture.startPaneCommand(t, "eligible", "sleep 600")
	multiID := fixture.startPaneCommand(t, "twowin", "sleep 600")
	fixture.disposable.run("new-window", "-t", "twowin:", "sleep 600")

	stateOf := func(t *testing.T, sessionID string) *proto.UnifiedSessionState {
		t.Helper()
		r := server.inventoryServer(fixture.server, 10)
		if r.Status != "ok" {
			t.Fatalf("inventory status = %q error = %q", r.Status, r.Error)
		}
		for _, row := range r.Sessions {
			if row.Authority.SessionID == sessionID {
				return row.Unified
			}
		}
		t.Fatalf("session %s absent from inventory", sessionID)
		return nil
	}
	if got := stateOf(t, eligibleID); got == nil || got.State != proto.UnifiedSessionAdoptable {
		t.Fatalf("eligible session projection = %+v", got)
	}
	if got := stateOf(t, multiID); got == nil || got.State != proto.UnifiedSessionBlockedMultiWindow {
		t.Fatalf("multi-window session projection = %+v", got)
	}
	if _, err := fixture.effects.AdoptSession(context.Background(), eligibleID); err != nil {
		t.Fatalf("adopt eligible session: %v", err)
	}
	if got := stateOf(t, eligibleID); got == nil || got.State != proto.UnifiedSessionOpen || got.Origin != proto.UnifiedOriginReconstructed {
		t.Fatalf("adopted session projection = %+v", got)
	}
}

func TestAdoptControlOpAdoptsAndIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux adoption lifecycle")
	}
	fixture := newAdoptionFixture(t, 0)
	server := &Server{config: fixture.cfg, unified: fixture.effects}

	if got := adoptControl(t, server, "main", "$4242"); got.Type != "adopt_refused" || got.Code != "session_gone" {
		t.Fatalf("missing-session adopt = %+v", got)
	}

	sessionID := fixture.startPaneCommand(t, "adoptee", "sleep 600")
	first := adoptControl(t, server, "main", sessionID)
	if first.Type != "adopt_ok" || first.ServerLabel != "main" || first.SessionID != sessionID {
		t.Fatalf("adopt = %+v", first)
	}
	again := adoptControl(t, server, "main", sessionID)
	if again.Type != "adopt_ok" || again.SessionID != sessionID {
		t.Fatalf("idempotent re-adopt = %+v", again)
	}
	projection := fixture.effects.projectSession("main", sessionID, eligibleFacts())
	if projection == nil || projection.State != proto.UnifiedSessionOpen || projection.Origin != proto.UnifiedOriginReconstructed {
		t.Fatalf("post-adopt projection = %+v", projection)
	}
	if !fixture.effects.allows("main", sessionID, "adoptee") {
		t.Fatal("adopted session is not attachable through the unified gate")
	}
}
