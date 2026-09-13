package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

// TestUX12TransportLossAfterRefitPONRRetainsUncertainPredecessor is the
// independent review's natural C1 falsifier. A dead observer transport is not
// proof that tmux's exact owner disappeared, so the already durable generation
// and every byte/slot charge must remain until an authoritative disposition
// owns cleanup.
func TestUX12TransportLossAfterRefitPONRRetainsUncertainPredecessor(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "ux12-transport-loss", `sh -c 'stty -echo; printf "UX12-TRANSPORT-LOSS\n"; while :; do sleep 1; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 10*time.Second, "predecessor output", func() bool {
		return strings.Contains(string(fixture.journalBytes(t, adoption.Key)), "UX12-TRANSPORT-LOSS")
	})
	detail, err := details(fixture.server, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(fixture.server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = fixture.cfg.Realm, fixture.server.Label, detail.ID, detail.Created
	source, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	filesBefore := fixture.journalFileCount(t)
	fixture.effects.mu.Lock()
	unit := fixture.effects.units[sessionID]
	fixture.effects.mu.Unlock()
	if unit == nil {
		t.Fatal("missing exact observer owner")
	}
	old, ok := unit.rotationWitness(adoption.Key)
	if !ok {
		t.Fatal("missing predecessor witness")
	}
	rotation := &unifiedDevRotation{
		session: sessionID, oldKey: adoption.Key, old: old,
		unit: unit, registry: fixture.registry, refit: true, ponr: true,
	}
	err = rotation.settleFatal(io.EOF)
	if !errors.Is(err, ErrUnifiedRefitFatal) {
		t.Fatalf("transport-loss refit error=%v want typed fatal", err)
	}
	after, witnessErr := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if witnessErr != nil || after.Columns != source.Columns || after.Rows != source.Rows {
		t.Fatalf("exact tmux source did not survive transport loss: witness=%+v err=%v", after, witnessErr)
	}
	fixture.registry.retention.mu.Lock()
	_, predecessorRetained := fixture.registry.retention.generations[adoption.Key]
	fixture.registry.retention.mu.Unlock()
	if !predecessorRetained || fixture.journalFileCount(t) < filesBefore {
		t.Fatalf("exact-owner transport loss unlinked/refunded uncertain predecessor: retained=%t files=%d before=%d", predecessorRetained, fixture.journalFileCount(t), filesBefore)
	}
}

func TestUX12RefitSourceDispositionUsesStableOwnerAcrossWidthChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		want observerSourceDisposition
		cut  func(*adoptionFixture, string)
	}{
		{name: "exact_owner", want: observerSourceTransportLost},
		{name: "replacement", want: observerSourceReplacement, cut: func(f *adoptionFixture, session string) {
			f.disposable.run("respawn-pane", "-k", "-t", session+":", `sh -c 'stty -echo; while :; do sleep 1; done'`)
		}},
		{name: "owner_gone", want: observerSourceOwnerGone, cut: func(f *adoptionFixture, session string) {
			f.disposable.run("kill-session", "-t", session)
		}},
		{name: "ambiguous", want: observerSourceAmbiguous, cut: func(f *adoptionFixture, _ string) {
			f.effects.server.SocketPath += ".unreachable"
			f.effects.sessionPresence = func(config.TmuxServer, string) (bool, bool) { return false, false }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newAdoptionFixture(t, 4)
			sessionID := fixture.startPaneCommand(t, "ux12-source-"+tc.name, `sh -c 'stty -echo; while :; do sleep 1; done'`)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			before, err := buildSourceWitness(ctx, fixture.server, "", sessionID)
			if err != nil {
				t.Fatal(err)
			}
			fixture.disposable.run("resize-window", "-x", "96", "-y", fmt.Sprint(before.Rows), "-t", sessionID+":")
			if tc.cut != nil {
				tc.cut(fixture, sessionID)
			}
			witness := controlmode.PaneWitness{Session: controlmode.SessionWitness{Server: adoption.Key.Server, Session: adoption.Key.Session, ControlGeneration: adoption.Key.ControlGeneration}, Window: adoption.Key.Window, Pane: adoption.Key.Pane, Incarnation: adoption.Key.Incarnation}
			decision := fixture.effects.classifyRefitSource(ctx, before, witness)
			if decision.disposition != tc.want {
				t.Fatalf("changed-width disposition=%v want %v", decision.disposition, tc.want)
			}
		})
	}
}

// TestUX12ConcurrentExactRefitSharesOneComposite pins the in-flight half of
// C2. Both callers carry the same immutable operation, but only the ledger
// owner may cross the capacity/composite boundary; the follower receives the
// owner's typed result after settlement.
func TestUX12ConcurrentExactRefitSharesOneComposite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	authority := proto.Authority{Realm: "r", Server: "s", SessionID: "$1", SessionCreated: 1}
	source := terminal.SourceWitness{Incarnation: "inc", SessionID: "$1", Columns: 80, Rows: 24}
	effects := &UnifiedDevPaneEffects{refitOperations: make(map[unifiedRefitOperationKey]*unifiedRefitOperation)}
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	var executions atomic.Int32
	effects.refitOperationEdge = func(gotOperation, edge string) {
		if gotOperation == strings.Repeat("c", 43) && edge == "recorded" {
			executions.Add(1)
			enterOnce.Do(func() {
				close(entered)
				<-release
			})
		}
	}
	operation := strings.Repeat("c", 43)
	results := make(chan error, 2)
	go func() { results <- effects.refitSession(ctx, authority, source, source.Columns+7, operation) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first refit did not reserve capacity")
	}
	go func() { results <- effects.refitSession(ctx, authority, source, source.Columns+7, operation) }()
	close(release)
	for range 2 {
		if err := <-results; !errors.Is(err, ErrUnifiedRotateUnavailable) {
			t.Fatalf("shared refit result=%v want ErrUnifiedRotateUnavailable", err)
		}
	}
	if got := executions.Load(); got != 1 {
		t.Fatalf("exact concurrent retries acquired %d execution owners, want 1", got)
	}
}

// TestUX12RefitOperationLedgerRejectsTupleReuseAndFailsClosedAtBound pins the
// token collision and bounded-cleanup contract without invoking tmux. Exact
// completed entries remain replayable at the ceiling; new ambiguity is
// refused instead of evicting an old token and reopening execution.
func TestUX12RefitOperationLedgerRejectsTupleReuseAndFailsClosedAtBound(t *testing.T) {
	authority := proto.Authority{Realm: "r", Server: "s", SessionID: "$1", SessionCreated: 1}
	source := terminal.SourceWitness{Incarnation: "inc", SessionID: "$1", Columns: 80, Rows: 24}
	effects := &UnifiedDevPaneEffects{refitOperations: make(map[unifiedRefitOperationKey]*unifiedRefitOperation)}
	for index := 0; index < unifiedRefitOperationLimit; index++ {
		operation := fmt.Sprintf("%043d", index)
		done := make(chan struct{})
		close(done)
		effects.refitOperations[unifiedRefitOperationKey{authority: authority, operation: operation}] = &unifiedRefitOperation{
			request: unifiedRefitRequest{source: source, columns: 100}, done: done,
		}
	}
	existing := fmt.Sprintf("%043d", 0)
	if err := effects.refitSession(context.Background(), authority, source, 100, existing); err != nil {
		t.Fatalf("exact completed replay at bound: %v", err)
	}
	if err := effects.refitSession(context.Background(), authority, source, 101, existing); !errors.Is(err, ErrUnifiedRefitMalformed) {
		t.Fatalf("token tuple reuse=%v want ErrUnifiedRefitMalformed", err)
	}
	if err := effects.refitSession(context.Background(), authority, source, 100, strings.Repeat("z", 43)); !errors.Is(err, ErrUnifiedRotateInProgress) {
		t.Fatalf("new token past bound=%v want fail-closed ErrUnifiedRotateInProgress", err)
	}
}

// TestUX12ExactRefitRetryIsIdempotent is the independent review's natural C2
// falsifier: replaying the exact immutable request after a lost success reply
// returns the original result and must not execute a second composite.
func TestUX12ExactRefitRetryIsIdempotent(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "ux12-idempotent", `sh -c 'stty -echo; while :; do sleep 1; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := fixture.effects.AdoptSession(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	detail, err := details(fixture.server, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(fixture.server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = fixture.cfg.Realm, fixture.server.Label, detail.ID, detail.Created
	source, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	columns := source.Columns + 9
	operation := strings.Repeat("i", 43)
	if err := fixture.effects.refitSession(ctx, authority, source, columns, operation); err != nil {
		t.Fatalf("first refit: %v", err)
	}
	keyBeforeRetry, ok := fixture.effects.paneKey(sessionID)
	if !ok {
		t.Fatal("missing committed successor")
	}
	if err := fixture.effects.refitSession(ctx, authority, source, columns, operation); err != nil {
		t.Fatalf("exact retry after a lost success response was not idempotent: %v", err)
	}
	keyAfterRetry, ok := fixture.effects.paneKey(sessionID)
	if !ok || keyAfterRetry != keyBeforeRetry {
		t.Fatalf("idempotent retry changed generation: before=%+v after=%+v present=%t", keyBeforeRetry, keyAfterRetry, ok)
	}
}

// TestUX12BrokerRefitRetryPrecedesWitnessRevalidation pins the production
// request boundary: a lost success reply remains replayable from immutable
// authority+token even after tmux can no longer reconstruct the old request's
// current witness. The retry returns the original successor and issues no
// second observer composite.
func TestUX12BrokerRefitRetryPrecedesWitnessRevalidation(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "ux12-broker-idempotent", `sh -c 'stty -echo; while :; do sleep 1; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := fixture.effects.AdoptSession(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	detail, err := details(fixture.server, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(fixture.server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = fixture.cfg.Realm, fixture.server.Label, detail.ID, detail.Created
	source, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	columns := source.Columns + 9
	operation := strings.Repeat("b", 43)
	successor, err := fixture.effects.refitSessionResult(ctx, authority, source, columns, operation)
	if err != nil {
		t.Fatalf("first refit: %v", err)
	}
	fixture.disposable.run("kill-session", "-t", sessionID)
	var wire bytes.Buffer
	server := &Server{config: fixture.cfg, unified: fixture.effects}
	server.refit(&lockedWriter{w: &wire}, proto.Control{Type: "refit", Authority: &authority, Cols: columns, ID: operation})
	frame, err := proto.ReadFrame(&wire)
	if err != nil {
		t.Fatal(err)
	}
	response, err := proto.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if response.Type != "refit_ok" || response.ID != operation || response.Cols != columns || response.Successor != successor {
		t.Fatalf("broker lost-success replay=%+v want original successor %q", response, successor)
	}
}
