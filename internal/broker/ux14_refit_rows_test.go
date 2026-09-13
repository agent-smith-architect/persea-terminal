package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

// TestUX14RefitGeometryPolicy pins the refit request policy: columns inside
// the explicit width band, rows either omitted or inside the closed row policy
// the live rows-only Fit obeys. Reject, never clamp.
func TestUX14RefitGeometryPolicy(t *testing.T) {
	for name, tc := range map[string]struct {
		columns, rows int
		want          bool
	}{
		"rows_omitted":       {120, 0, true},
		"rows_at_min":        {120, terminal.MinFitRows, true},
		"rows_at_max":        {120, terminal.MaxFitRows, true},
		"rows_below_min":     {120, terminal.MinFitRows - 1, false},
		"rows_above_max":     {120, terminal.MaxFitRows + 1, false},
		"rows_negative":      {120, -1, false},
		"columns_below_band": {19, 40, false},
		"columns_above_band": {301, 40, false},
		"cells_at_cap":       {300, terminal.MaxFitCells / 300, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := validRefitGeometry(tc.columns, tc.rows); got != tc.want {
				t.Fatalf("validRefitGeometry(%d, %d)=%t want %t", tc.columns, tc.rows, got, tc.want)
			}
		})
	}
	source := terminal.SourceWitness{Columns: 80, Rows: 24}
	if got := effectiveRefitRows(source, 0); got != 24 {
		t.Fatalf("omitted rows resolve to %d want the predecessor's 24", got)
	}
	if got := effectiveRefitRows(source, 45); got != 45 {
		t.Fatalf("requested rows resolve to %d want 45", got)
	}
}

// TestUX14RefitCompositeLineCarriesRows pins the tmux line: the guarded
// resize targets the requested columns×rows, and a rows-less request resolves
// to the predecessor's rows before the line is built.
func TestUX14RefitCompositeLineCarriesRows(t *testing.T) {
	authority := proto.Authority{Realm: "r", Server: "s", SessionID: "$1", SessionCreated: 1}
	source := terminal.SourceWitness{Incarnation: "inc", SessionID: "$1", WindowID: "@1", PaneID: "%1", Columns: 80, Rows: 24}
	withRows := refitCompositeLine(authority, source, 120, 45)
	if !strings.Contains(withRows, "resize-window -t") || !strings.Contains(withRows, " -x 120 -y 45") {
		t.Fatalf("composite line does not resize to 120x45: %q", withRows)
	}
	kept := refitCompositeLine(authority, source, 120, effectiveRefitRows(source, 0))
	if !strings.Contains(kept, " -x 120 -y 24") {
		t.Fatalf("rows-less composite line does not keep the predecessor's 24 rows: %q", kept)
	}
	if !strings.Contains(withRows, "'#{pane_height}' = '24'") && !strings.Contains(withRows, "#{pane_height}") {
		t.Fatalf("composite line lost the current-geometry guard: %q", withRows)
	}
}

// TestUX14RefitLedgerTupleIncludesRows pins idempotency over the full tuple:
// a recorded token replayed with the same columns and rows returns the recorded
// result; the same token with different rows is a different request and is
// refused as malformed. No tmux is involved.
func TestUX14RefitLedgerTupleIncludesRows(t *testing.T) {
	authority := proto.Authority{Realm: "r", Server: "s", SessionID: "$1", SessionCreated: 1}
	source := terminal.SourceWitness{Incarnation: "inc", SessionID: "$1", Columns: 80, Rows: 24}
	effects := &UnifiedDevPaneEffects{refitOperations: make(map[unifiedRefitOperationKey]*unifiedRefitOperation)}
	operation := fmt.Sprintf("%043d", 7)
	done := make(chan struct{})
	close(done)
	successor := strings.Repeat("N", 43)
	effects.refitOperations[unifiedRefitOperationKey{authority: authority, operation: operation}] = &unifiedRefitOperation{
		request: unifiedRefitRequest{source: source, columns: 100, rows: 40}, done: done,
		result: unifiedRefitResult{successorSource: successor, rows: 40},
	}
	if got, rows, err := effects.refitSessionResultGeometry(context.Background(), authority, source, 100, 40, operation); err != nil || got != successor || rows != 40 {
		t.Fatalf("exact tuple replay=(%q, %d, %v) want (%q, 40, nil)", got, rows, err, successor)
	}
	if _, _, err := effects.refitSessionResultGeometry(context.Background(), authority, source, 100, 41, operation); !errors.Is(err, ErrUnifiedRefitMalformed) {
		t.Fatalf("token reuse with different rows=%v want ErrUnifiedRefitMalformed", err)
	}
	if _, _, err := effects.refitSessionResultGeometry(context.Background(), authority, source, 100, 0, operation); !errors.Is(err, ErrUnifiedRefitMalformed) {
		t.Fatalf("token reuse with omitted rows against a rows-bearing record=%v want ErrUnifiedRefitMalformed", err)
	}
	if result, found := effects.settledRefitOperationGeometry(authority, 100, 40, operation); !found || result.err != nil || result.rows != 40 || result.successorSource != successor {
		t.Fatalf("settled lookup=(%+v, %t) want the recorded rows-bearing result", result, found)
	}
	if result, found := effects.settledRefitOperationGeometry(authority, 100, 39, operation); !found || !errors.Is(result.err, ErrUnifiedRefitMalformed) {
		t.Fatalf("settled lookup with different rows=(%+v, %t) want malformed", result, found)
	}
	if result, found := effects.settledRefitOperationGeometry(authority, 100, 7, operation); !found || !errors.Is(result.err, ErrUnifiedRefitMalformed) {
		t.Fatalf("settled lookup with out-of-policy rows=(%+v, %t) want malformed", result, found)
	}
	// The legacy three-argument lookup is the rows-omitted tuple.
	if result, found := effects.settledRefitOperation(authority, 100, operation); !found || !errors.Is(result.err, ErrUnifiedRefitMalformed) {
		t.Fatalf("legacy lookup against a rows-bearing record=(%+v, %t) want malformed", result, found)
	}
}

// TestUX14ExplicitRefitCarriesRows is the tmux-backed proof: an explicit refit
// that names rows lands columns×rows on the real pane, echoes those rows, and a
// following rows-less refit keeps the landed rows.
func TestUX14ExplicitRefitCarriesRows(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "ux14-refit-rows", `sh -c 'stty -echo; while :; do sleep 1; done'`)
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
	columns, rows := source.Columns+15, source.Rows+9
	operation := strings.Repeat("y", 43)
	successor, applied, err := fixture.effects.refitSessionResultGeometry(ctx, authority, source, columns, rows, operation)
	if err != nil || applied != rows || successor == "" {
		t.Fatalf("rows-bearing refit=(%q, %d, %v) want rows %d", successor, applied, err, rows)
	}
	landed, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if landed.Columns != columns || landed.Rows != rows {
		t.Fatalf("landed geometry=%dx%d want %dx%d", landed.Columns, landed.Rows, columns, rows)
	}
	if replay, replayRows, err := fixture.effects.refitSessionResultGeometry(ctx, authority, source, columns, rows, operation); err != nil || replay != successor || replayRows != rows {
		t.Fatalf("exact replay=(%q, %d, %v) want the original (%q, %d)", replay, replayRows, err, successor, rows)
	}
	// The broker control path echoes the applied rows on the lost-success replay.
	var wire bytes.Buffer
	server := &Server{config: fixture.cfg, unified: fixture.effects}
	server.refit(&lockedWriter{w: &wire}, proto.Control{Type: "refit", Authority: &authority, Cols: columns, Rows: rows, ID: operation})
	frame, err := proto.ReadFrame(&wire)
	if err != nil {
		t.Fatal(err)
	}
	response, err := proto.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if response.Type != "refit_ok" || response.Cols != columns || response.Rows != rows || response.Successor != successor {
		t.Fatalf("broker replay=%+v want refit_ok %dx%d successor %q", response, columns, rows, successor)
	}
	// A rows-less refit from the landed geometry keeps the landed rows.
	keptColumns := columns - 6
	keptSuccessor, keptRows, err := fixture.effects.refitSessionResultGeometry(ctx, authority, landed, keptColumns, 0, strings.Repeat("k", 43))
	if err != nil || keptRows != rows || keptSuccessor == "" {
		t.Fatalf("rows-less refit=(%q, %d, %v) want rows %d kept", keptSuccessor, keptRows, err, rows)
	}
	after, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Columns != keptColumns || after.Rows != rows {
		t.Fatalf("rows-less landed geometry=%dx%d want %dx%d", after.Columns, after.Rows, keptColumns, rows)
	}
	// Out-of-policy rows never reach tmux.
	if _, _, err := fixture.effects.refitSessionResultGeometry(ctx, authority, after, keptColumns+3, terminal.MaxFitRows+1, strings.Repeat("o", 43)); !errors.Is(err, ErrUnifiedRefitMalformed) {
		t.Fatalf("out-of-policy rows=%v want ErrUnifiedRefitMalformed", err)
	}
	wire.Reset()
	server.refit(&lockedWriter{w: &wire}, proto.Control{Type: "refit", Authority: &authority, Cols: keptColumns + 3, Rows: terminal.MaxFitRows + 1, ID: strings.Repeat("p", 43)})
	frame, err = proto.ReadFrame(&wire)
	if err != nil {
		t.Fatal(err)
	}
	refused, err := proto.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if refused.Type != "refit_refused" || refused.Code != "bad_refit" {
		t.Fatalf("broker out-of-policy rows=%+v want refit_refused bad_refit", refused)
	}
	final, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Columns != keptColumns || final.Rows != rows {
		t.Fatalf("refused request changed geometry to %dx%d", final.Columns, final.Rows)
	}
}
