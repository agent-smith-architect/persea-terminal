package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

func rotationSnapshotBytes(events []unifiedjournal.Event) []byte {
	var output []byte
	for _, event := range events {
		if event.Kind == unifiedjournal.RecordOutput {
			output = append(output, event.Payload...)
		}
	}
	return output
}

func TestUnifiedExplicitWidthRefitCreatesCaptureAuthoritativeSuccessor(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "explicit-refit", `sh -c 'stty -echo; printf "REFIT-BEFORE-UNIQUE\nabcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrstuvwxyz\nwide-界界-combining-é-end\n"; while :; do sleep 1; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 10*time.Second, "pre-refit marker", func() bool {
		return bytes.Contains(fixture.journalBytes(t, adoption.Key), []byte("REFIT-BEFORE-UNIQUE"))
	})
	_, _, predecessor, cancelTail, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelTail()
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
	wantColumns := source.Columns + 17
	if err := fixture.effects.refitSession(ctx, authority, source, wantColumns, strings.Repeat("r", 43)); err != nil {
		t.Fatalf("explicit refit: %v", err)
	}
	if reason := waitRotationSubscriberClose(t, predecessor); reason != proto.SubscriberClosedGenerationRefit {
		t.Fatalf("predecessor close reason=%q want %q", reason, proto.SubscriberClosedGenerationRefit)
	}
	cancelTail()
	after, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Columns != wantColumns || after.Rows != source.Rows {
		t.Fatalf("tmux geometry=%dx%d want %dx%d", after.Columns, after.Rows, wantColumns, source.Rows)
	}
	newKey, ok := fixture.effects.paneKey(sessionID)
	if !ok || newKey == adoption.Key {
		t.Fatalf("active key=%+v old=%+v", newKey, adoption.Key)
	}
	fixture.effects.journalMu.Lock()
	initial, initialErr := fixture.effects.realm.InitialGeometry(newKey)
	events, replayErr := fixture.effects.realm.ReadCommittedEvents(newKey)
	fixture.effects.journalMu.Unlock()
	if initialErr != nil || replayErr != nil {
		t.Fatalf("successor read initial=%v replay=%v", initialErr, replayErr)
	}
	wantGeometry := (unifiedjournal.Geometry{Columns: wantColumns, Rows: source.Rows})
	if initial != wantGeometry {
		t.Fatalf("successor initial geometry=%+v want=%+v", initial, wantGeometry)
	}
	for _, event := range events {
		if event.Kind == unifiedjournal.RecordGeometry {
			t.Fatalf("new successor unexpectedly carried a relative geometry record: %+v", event)
		}
	}
	bootstrap := rotationSnapshotBytes(events)
	if !bytes.Contains(bootstrap, []byte("REFIT-BEFORE-UNIQUE")) {
		t.Fatalf("successor capture omitted predecessor screen: %q", bootstrap)
	}
	rows, _ := adoptionBootstrapParts(t, bootstrap)
	raw, err := tmuxCombinedOutput(fixture.disposable.path,
		"capture-pane", "-e", "-p", "-N", "-S", "-2000", "-E", "-", "-t", fixture.paneID(t, "explicit-refit"))
	if err != nil {
		t.Fatalf("post-refit ground truth capture: %v %s", err, raw)
	}
	truth := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(rows) != len(truth) {
		t.Fatalf("post-refit bootstrap rows=%d ground truth rows=%d", len(rows), len(truth))
	}
	for index := range rows {
		if rows[index] != truth[index] {
			t.Fatalf("post-refit row %d diverges:\n journal: %q\n tmux:    %q", index, rows[index], truth[index])
		}
	}
	fixture.effects.subscriberMu.Lock()
	_, retainedSubscriberBucket := fixture.effects.subscribers[newKey]
	fixture.effects.subscriberMu.Unlock()
	if retainedSubscriberBucket {
		t.Fatal("successor authority retained an empty subscriber bucket")
	}
	freshSnapshot, freshGeometry, freshSubscriber, cancelFresh, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatalf("fresh successor attach without an existing subscriber: %v", err)
	}
	cancelFresh()
	if freshGeometry != wantGeometry || !bytes.Equal(rotationSnapshotBytes(freshSnapshot), bootstrap) || freshSubscriber == nil {
		t.Fatalf("fresh successor attach geometry=%+v events=%d subscriber=%p", freshGeometry, len(freshSnapshot), freshSubscriber)
	}
}

// A committed rows-only Fit changes tmux's current geometry-bearing source
// incarnation but deliberately does not rotate the journal generation. Width
// refit must bind that fresh physical owner back to the generation's durable
// birth geometry, then repeat from the landed successor without accepting a
// stale pane/process replacement.
func TestUnifiedExplicitWidthRefitAfterRowsChangeRepeats(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "explicit-refit-after-rows", `sh -c 'stty -echo; printf "REFIT-ROWS-BEFORE\n"; while :; do sleep 1; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	filesBefore := fixture.journalFileCount(t)
	fixture.effects.journalMu.Lock()
	slotsBefore := fixture.effects.realm.AvailableCompletePaneSlots()
	_, logicalHeldBefore, _ := fixture.effects.realm.LogicalBudget()
	_, physicalHeldBefore, _ := fixture.effects.realm.PhysicalBudget()
	fixture.effects.journalMu.Unlock()
	fixture.registry.retention.mu.Lock()
	generationsBefore := len(fixture.registry.retention.generations)
	pBefore, qBefore, eBefore, bBefore, oBefore := fixture.registry.retention.pUsed, fixture.registry.retention.qUsed, fixture.registry.retention.eUsed, fixture.registry.retention.bUsed, fixture.registry.retention.oUsed
	fixture.registry.retention.mu.Unlock()
	detail, err := details(fixture.server, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(fixture.server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = fixture.cfg.Realm, fixture.server.Label, detail.ID, detail.Created
	birth, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.disposable.run("resize-window", "-x", fmt.Sprint(birth.Columns), "-y", fmt.Sprint(birth.Rows+1), "-t", sessionID+":")
	rowFitted, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if rowFitted.Incarnation == adoption.Key.Incarnation || rowFitted.Rows != birth.Rows+1 {
		t.Fatalf("rows-only change did not create the causal geometry-bearing witness: birth=%+v current=%+v active=%+v", birth, rowFitted, adoption.Key)
	}
	var edges []string
	fixture.effects.rotationEdge = func(_ string, edge string) { edges = append(edges, edge) }
	firstColumns := birth.Columns + 17
	if err := fixture.effects.refitSession(ctx, authority, rowFitted, firstColumns, strings.Repeat("w", 43)); err != nil {
		t.Fatalf("refit after rows-only change: %v edges=%v", err, edges)
	}
	first, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Columns != firstColumns || first.Rows != rowFitted.Rows {
		t.Fatalf("first landed geometry=%dx%d want %dx%d", first.Columns, first.Rows, firstColumns, rowFitted.Rows)
	}
	guard := guardedResizeArgs(authority, first, birth.Columns, first.Rows)
	guard[4] = "display-message -p REFIT-GUARD-OK"
	guard[5] = "display-message -p REFIT-GUARD-STALE"
	guardResult, guardErr := tmuxCombinedOutput(fixture.disposable.path, guard...)
	if guardErr != nil || strings.TrimSpace(string(guardResult)) != "REFIT-GUARD-OK" {
		t.Fatalf("repeated-refit current-source guard=%q err=%v source=%+v", guardResult, guardErr, first)
	}
	fixture.effects.mu.Lock()
	activeKey := fixture.effects.active[sessionID]
	unit := fixture.effects.units[sessionID]
	fixture.effects.mu.Unlock()
	activeWitness, activeFound := unit.rotationWitness(activeKey)
	unit.holder.mu.Lock()
	holderWitness, holderCommitted, holderRotation, holderAborted := unit.holder.witness, unit.holder.committed, unit.holder.rotation, unit.holder.aborted
	unit.holder.mu.Unlock()
	if !activeFound || holderAborted || !holderCommitted || holderRotation != nil || holderWitness != activeWitness {
		t.Fatalf("first refit did not settle reusable holder: active=%+v found=%t witness=%+v holder=%+v committed=%t pending=%t aborted=%t", activeKey, activeFound, activeWitness, holderWitness, holderCommitted, holderRotation != nil, holderAborted)
	}
	edges = nil
	if err := fixture.effects.refitSession(ctx, authority, first, birth.Columns, strings.Repeat("x", 43)); err != nil {
		t.Fatalf("repeated refit: %v edges=%v", err, edges)
	}
	second, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Columns != birth.Columns || second.Rows != rowFitted.Rows {
		t.Fatalf("second landed geometry=%dx%d want %dx%d", second.Columns, second.Rows, birth.Columns, rowFitted.Rows)
	}
	pollUntil(t, 10*time.Second, "repeated refit resource settlement", func() bool {
		fixture.effects.journalMu.Lock()
		slots := fixture.effects.realm.AvailableCompletePaneSlots()
		_, logicalHeld, _ := fixture.effects.realm.LogicalBudget()
		_, physicalHeld, _ := fixture.effects.realm.PhysicalBudget()
		fixture.effects.journalMu.Unlock()
		fixture.registry.retention.mu.Lock()
		generations := len(fixture.registry.retention.generations)
		p, q, e, b, o := fixture.registry.retention.pUsed, fixture.registry.retention.qUsed, fixture.registry.retention.eUsed, fixture.registry.retention.bUsed, fixture.registry.retention.oUsed
		fixture.registry.retention.mu.Unlock()
		return fixture.journalFileCount(t) == filesBefore && slots == slotsBefore && logicalHeld == logicalHeldBefore && physicalHeld == physicalHeldBefore &&
			generations == generationsBefore && p == pBefore && q <= qBefore && e <= eBefore && b <= bBefore && o <= oBefore
	})
}

func TestUnifiedExplicitWidthRefitAfterRowsChangeRejectsProcessReplacement(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "explicit-refit-replacement", `sh -c 'stty -echo; while :; do sleep 1; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
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
	birth, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.disposable.run("resize-window", "-x", fmt.Sprint(birth.Columns), "-y", fmt.Sprint(birth.Rows+1), "-t", sessionID+":")
	fixture.disposable.run("respawn-pane", "-k", "-t", sessionID+":", `sh -c 'stty -echo; while :; do sleep 1; done'`)
	replacement, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.PaneID != birth.PaneID || replacement.Pane == birth.Pane {
		t.Fatalf("fixture did not preserve pane id while replacing its process: birth=%+v replacement=%+v", birth, replacement)
	}
	if err := fixture.effects.refitSession(ctx, authority, replacement, birth.Columns+13, strings.Repeat("z", 43)); !errors.Is(err, ErrUnifiedRefitStale) {
		t.Fatalf("replacement refit error=%v want ErrUnifiedRefitStale", err)
	}
	after, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Columns != replacement.Columns || after.Rows != replacement.Rows {
		t.Fatalf("stale replacement refusal mutated geometry: before=%dx%d after=%dx%d", replacement.Columns, replacement.Rows, after.Columns, after.Rows)
	}
	active, ok := fixture.effects.paneKey(sessionID)
	if !ok || active != adoption.Key {
		t.Fatalf("stale replacement refusal changed active generation: active=%+v ok=%t want=%+v", active, ok, adoption.Key)
	}
}

func TestUnifiedExplicitWidthRefitAcceptsAlternateScreen(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "refit-alt", `sh -c 'stty -echo; while IFS= read -r line; do eval "$line"; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.disposable.run("send-keys", "-l", "-t", sessionID+":", `printf '\033[?1049hALT-SCREEN\n'`)
	fixture.disposable.run("send-keys", "-t", sessionID+":", "Enter")
	pollUntil(t, 10*time.Second, "alternate screen", func() bool {
		return strings.TrimSpace(fixture.disposable.run("display-message", "-p", "-t", sessionID+":", "#{alternate_on}")) == "1"
	})
	_, _, predecessor, cancelTail, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelTail()
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
	err = fixture.effects.refitSession(ctx, authority, source, source.Columns+11, strings.Repeat("a", 43))
	if err != nil {
		t.Fatalf("refit alternate screen: %v", err)
	}
	after, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Columns != source.Columns+11 || after.Rows != source.Rows {
		t.Fatalf("refit geometry=%dx%d want %dx%d", after.Columns, after.Rows, source.Columns+11, source.Rows)
	}
	if key, ok := fixture.effects.paneKey(sessionID); !ok || key == adoption.Key {
		t.Fatalf("refit did not replace active key=%+v present=%t", key, ok)
	}
	if reason := waitRotationSubscriberClose(t, predecessor); reason != proto.SubscriberClosedGenerationRefit {
		t.Fatalf("refit close reason=%q", reason)
	}
}

func TestUnifiedExplicitWidthRefitPostPONRFaultRetainsUncertainOwner(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "refit-fatal", `sh -c 'stty -echo; printf "REFIT-FATAL-BEFORE\n"; while :; do sleep 1; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, predecessor, cancelTail, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelTail()
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
	fixture.effects.rotationFault = func(_ string, edge string) error {
		if edge == "boundary_wait" {
			return errors.New("injected post-PONR storage fault")
		}
		return nil
	}
	err = fixture.effects.refitSession(ctx, authority, source, source.Columns+9, strings.Repeat("f", 43))
	if !errors.Is(err, ErrUnifiedRefitFatal) {
		t.Fatalf("refit error=%v want typed fatal", err)
	}
	if reason := waitRotationSubscriberClose(t, predecessor); reason != proto.SubscriberClosedRefitFaulted {
		t.Fatalf("predecessor close reason=%q want %q", reason, proto.SubscriberClosedRefitFaulted)
	}
	cancelTail()
	if key, ok := fixture.effects.paneKey(sessionID); ok {
		t.Fatalf("fatal refit retained active key=%+v old=%+v", key, adoption.Key)
	}
	fixture.registry.retention.mu.Lock()
	_, retained := fixture.registry.retention.generations[adoption.Key]
	fixture.registry.retention.mu.Unlock()
	if !retained {
		t.Fatal("exact-owner transport loss retired an uncertain predecessor")
	}
}

func TestUnifiedExplicitWidthRefitHasOneExactOperationOwner(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "refit-owner", `sh -c 'stty -echo; printf "REFIT-OWNER-BEFORE\n"; while :; do sleep 1; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := fixture.effects.AdoptSession(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	_, _, predecessor, cancelTail, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelTail()
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

	start := make(chan struct{})
	results := make(chan error, 2)
	for index, columns := range []int{source.Columns + 7, source.Columns + 13} {
		go func(index, columns int) {
			<-start
			results <- fixture.effects.refitSession(ctx, authority, source, columns, strings.Repeat(string(rune('g'+index)), 43))
		}(index, columns)
	}
	close(start)
	successes := 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, ErrUnifiedRotateInProgress) && !errors.Is(err, ErrUnifiedRefitStale) {
			t.Fatalf("losing refit error=%v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful width owners=%d want exactly one", successes)
	}
	if reason := waitRotationSubscriberClose(t, predecessor); reason != proto.SubscriberClosedGenerationRefit {
		t.Fatalf("predecessor close reason=%q want %q", reason, proto.SubscriberClosedGenerationRefit)
	}
	after, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Rows != source.Rows || (after.Columns != source.Columns+7 && after.Columns != source.Columns+13) {
		t.Fatalf("winning refit geometry=%dx%d from %dx%d", after.Columns, after.Rows, source.Columns, source.Rows)
	}
}

func TestUnifiedExplicitWidthRefitRefusesStaleOwnerBeforeIssue(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "refit-stale", `sh -c 'stty -echo; printf "REFIT-STALE-BEFORE\n"; while :; do sleep 1; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
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
	stale := source
	stale.PaneID += "-stale"
	if err := fixture.effects.refitSession(ctx, authority, stale, source.Columns+5, strings.Repeat("s", 43)); !errors.Is(err, ErrUnifiedRefitStale) {
		t.Fatalf("stale refit error=%v want typed stale", err)
	}
	after, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Columns != source.Columns || after.Rows != source.Rows {
		t.Fatalf("stale refusal mutated tmux geometry=%dx%d", after.Columns, after.Rows)
	}
	if key, ok := fixture.effects.paneKey(sessionID); !ok || key != adoption.Key {
		t.Fatalf("stale refusal changed active key=%+v present=%t", key, ok)
	}
}

func TestUnifiedExplicitWidthRefitReservesCapacityBeforeIssue(t *testing.T) {
	fixture := newAdoptionFixture(t, 3)
	sessionID := fixture.startPaneCommand(t, "refit-capacity", `sh -c 'stty -echo; printf "REFIT-CAPACITY-BEFORE\n"; while :; do sleep 1; done'`)
	blockerID := fixture.startPaneCommand(t, "refit-capacity-blocker", `sh -c 'stty -echo; printf "REFIT-CAPACITY-BLOCKER\n"; while :; do sleep 1; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	blockerAdoption, err := fixture.effects.AdoptSession(ctx, blockerID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.effects.journalMu.Lock()
	blocker, err := fixture.effects.realm.BeginRotationCapacity(blockerAdoption.Key)
	fixture.effects.journalMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		fixture.effects.journalMu.Lock()
		blocker.Release()
		fixture.effects.journalMu.Unlock()
	}()
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
	if err := fixture.effects.refitSession(ctx, authority, source, source.Columns+5, strings.Repeat("q", 43)); !errors.Is(err, ErrUnifiedRotateSlotsExhausted) {
		t.Fatalf("reserved-capacity refit error=%v want typed capacity refusal", err)
	}
	after, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Columns != source.Columns || after.Rows != source.Rows {
		t.Fatalf("capacity refusal mutated tmux geometry=%dx%d", after.Columns, after.Rows)
	}
	if key, ok := fixture.effects.paneKey(sessionID); !ok || key != adoption.Key {
		t.Fatalf("capacity refusal changed active key=%+v present=%t", key, ok)
	}
}

func waitRotationSubscriberClose(t *testing.T, subscriber *unifiedDevSubscriber) proto.SubscriberCloseReason {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case deliveredForTest, ok := <-subscriber.events():
			if ok {
				subscriber.releaseEvent(deliveredForTest)
			}
			if !ok {
				return subscriber.closeReason()
			}
		case <-timer.C:
			t.Fatal("timed out waiting for rotated predecessor subscriber close")
		}
	}
}

func assertPostPONRFatalSettled(t *testing.T, fixture *adoptionFixture, sessionID string, rotation *unifiedDevRotation, slotsBefore int64) {
	t.Helper()
	pollUntil(t, 10*time.Second, "fatal rotation unit reap", func() bool {
		fixture.effects.mu.Lock()
		defer fixture.effects.mu.Unlock()
		return fixture.effects.units[sessionID] == nil
	})
	if rotation == nil || rotation.registryT != nil || rotation.journal != nil {
		t.Fatalf("fatal rotation retained transaction owners: rotation=%v registry=%v journal=%v", rotation != nil, rotation != nil && rotation.registryT != nil, rotation != nil && rotation.journal != nil)
	}
	rotation.holder.mu.Lock()
	pending := rotation.holder.rotation
	rotation.holder.mu.Unlock()
	if pending != nil {
		t.Fatal("fatal rotation retained holder pending output")
	}
	fixture.registry.mu.Lock()
	_, routeHeld := fixture.registry.rotations[routeCoordinateKey(rotation.old)]
	_, generationHeld := fixture.registry.rotationGenerations[rotation.newKey]
	fixture.registry.mu.Unlock()
	if routeHeld || generationHeld {
		t.Fatalf("fatal rotation retained provisional registry state: route=%v generation=%v", routeHeld, generationHeld)
	}
	charged, reserved, cap := fixture.effects.realm.PhysicalBudget()
	if reserved != 0 || charged > cap {
		t.Fatalf("fatal rotation physical budget charged=%d reserved=%d cap=%d", charged, reserved, cap)
	}
	expectedSlots := slotsBefore + 1
	if rotation.registryCommitted {
		pollUntil(t, 10*time.Second, "failed successor runtime cleanup", func() bool {
			fixture.registry.retention.mu.Lock()
			defer fixture.registry.retention.mu.Unlock()
			return fixture.registry.retention.generations[rotation.newKey] == nil && fixture.registry.retention.panes[rotation.newKey] == nil
		})
		fixture.registry.mu.Lock()
		session := fixture.registry.sessions[sessionKey(rotation.next.Session)]
		eligible := session != nil && session.router.UnifiedEligible(rotation.next)
		fixture.registry.mu.Unlock()
		if eligible {
			t.Fatal("fatal rotation left committed successor router-eligible")
		}
		fixture.effects.subscriberMu.Lock()
		active := fixture.effects.active[sessionID]
		_, subscriberBucket := fixture.effects.subscribers[rotation.newKey]
		fixture.effects.subscriberMu.Unlock()
		if active == rotation.newKey || subscriberBucket {
			t.Fatalf("fatal rotation retained successor authority active=%v subscriber_bucket=%v", active == rotation.newKey, subscriberBucket)
		}
	}
	if got := fixture.effects.realm.AvailableCompletePaneSlots(); got != expectedSlots {
		t.Fatalf("fatal rotation available slots=%d want %d", got, expectedSlots)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := fixture.effects.AdoptSession(ctx, sessionID); err != nil {
		t.Fatalf("fatal rotation did not leave session adoptable: %v", err)
	}
	if got := fixture.effects.realm.AvailableCompletePaneSlots(); got != slotsBefore {
		t.Fatalf("fresh adoption slots=%d want pre-rotation %d", got, slotsBefore)
	}
}

func TestUnifiedRotationRealTmuxMidFloodClosesPredecessorAndReplaysSuccessor(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	const bootstrapMarker = "ROTATION-BOOTSTRAP-UNIQUE"
	sessionID := fixture.startPaneCommand(t, "rotation-flow", `sh -c 'stty -echo; printf "ROTATION-BOOTSTRAP-UNIQUE\\n"; i=0; while :; do printf "ROT-%06d\\n" "$i"; i=$((i+1)); sleep 0.005; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 10*time.Second, "predecessor flood", func() bool {
		return bytes.Contains(fixture.journalBytes(t, adoption.Key), []byte("ROT-"))
	})
	slotsBefore := fixture.effects.realm.AvailableCompletePaneSlots()
	_, _, predecessor, cancelTail, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelTail()

	if err := fixture.effects.rotateSession(ctx, sessionID); err != nil {
		t.Fatalf("rotate real tmux session: %v", err)
	}
	if reason := waitRotationSubscriberClose(t, predecessor); reason != proto.SubscriberClosedGenerationRotated {
		t.Fatalf("predecessor close reason=%q want %q", reason, proto.SubscriberClosedGenerationRotated)
	}
	newKey, ok := fixture.effects.paneKey(sessionID)
	if !ok || newKey == adoption.Key {
		t.Fatalf("active key=%+v old=%+v", newKey, adoption.Key)
	}
	fixture.effects.journalMu.Lock()
	origin, originErr := fixture.effects.realm.Origin(newKey)
	fixture.effects.journalMu.Unlock()
	if originErr != nil || origin != unifiedjournal.OriginRotated {
		t.Fatalf("successor origin=%q err=%v", origin, originErr)
	}
	events, _, successor, cancelSuccessor, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelSuccessor()
	if !bytes.Contains(rotationSnapshotBytes(events), []byte("ROT-")) {
		t.Fatalf("successor replay has no flood rows: %q", rotationSnapshotBytes(events))
	}
	if count := bytes.Count(rotationSnapshotBytes(events), []byte(bootstrapMarker)); count != 1 {
		t.Fatalf("successor bootstrap marker count=%d want 1", count)
	}
	if got := fixture.effects.realm.AvailableCompletePaneSlots(); got != slotsBefore {
		t.Fatalf("available slots=%d want %d", got, slotsBefore)
	}
	committedBefore := len(fixture.journalBytes(t, newKey))
	pollUntil(t, 10*time.Second, "post-rotation journal output", func() bool {
		return len(fixture.journalBytes(t, newKey)) > committedBefore
	})
	pollUntil(t, 10*time.Second, "post-rotation live output", func() bool {
		select {
		case event, ok := <-successor.events():
			if ok {
				successor.releaseEvent(event)
			}
			return ok && bytes.Contains(event.Payload, []byte("ROT-"))
		default:
			return false
		}
	})
}

func TestUnifiedRotationExactSuccessEdgeOrder(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "rotation-edge-order", `sh -c 'stty -echo; printf "ROTATION-EDGE-ORDER\\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := fixture.effects.AdoptSession(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	wanted := []string{
		"capacity_reserved", "before_composite_write", "holder_flipped",
		"successor_materialized", "bootstrap_durable", "registry_validated",
		"predecessor_sealed", "registry_committed", "active_swapped",
		"journal_committed", "pending_durable",
	}
	wantedSet := make(map[string]struct{}, len(wanted))
	for _, edge := range wanted {
		wantedSet[edge] = struct{}{}
	}
	var mu sync.Mutex
	var got []string
	fixture.effects.rotationEdge = func(_ string, edge string) {
		if _, keep := wantedSet[edge]; !keep {
			return
		}
		mu.Lock()
		got = append(got, edge)
		mu.Unlock()
	}
	if err := fixture.effects.rotateSession(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotText := strings.Join(got, " -> ")
	mu.Unlock()
	if wantText := strings.Join(wanted, " -> "); gotText != wantText {
		t.Fatalf("rotation edge order:\n got %s\nwant %s", gotText, wantText)
	}
	for index := 0; index+1 < len(wanted); index++ {
		left, right := wanted[index], wanted[index+1]
		leftAt, rightAt := -1, -1
		for at, edge := range got {
			if edge == left {
				leftAt = at
			}
			if edge == right {
				rightAt = at
			}
		}
		if leftAt < 0 || rightAt != leftAt+1 {
			t.Fatalf("adjacent rotation edge %q -> %q positions=(%d,%d)", left, right, leftAt, rightAt)
		}
	}
}

func TestUnifiedRotationUnavailableIsTyped(t *testing.T) {
	effects := &UnifiedDevPaneEffects{}
	if err := effects.rotateSession(context.Background(), "missing"); !errors.Is(err, ErrUnifiedRotateUnavailable) {
		t.Fatalf("rotate missing error=%v", err)
	}
}

func TestUnifiedRotationCapacityRefusesBeforeObserverCommand(t *testing.T) {
	fixture := newAdoptionFixture(t, 3)
	sessionID := fixture.startPaneCommand(t, "rotation-capacity-refusal", `sh -c 'stty -echo; printf "ROTATION-CAPACITY-REFUSAL\\n"; sleep 60'`)
	blockerID := fixture.startPaneCommand(t, "rotation-capacity-blocker", `sh -c 'stty -echo; printf "ROTATION-CAPACITY-BLOCKER\\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	blockerAdoption, err := fixture.effects.AdoptSession(ctx, blockerID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.effects.journalMu.Lock()
	blocker, err := fixture.effects.realm.BeginRotationCapacity(blockerAdoption.Key)
	fixture.effects.journalMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		fixture.effects.journalMu.Lock()
		blocker.Release()
		fixture.effects.journalMu.Unlock()
	}()
	unit := fixture.effects.units[sessionID]
	unit.holder.mu.Lock()
	beforeWitness, beforeCommitted, beforeRotation := unit.holder.witness, unit.holder.committed, unit.holder.rotation
	unit.holder.mu.Unlock()
	fixture.effects.journalMu.Lock()
	chargedBefore, reservedBefore, capBefore := fixture.effects.realm.PhysicalBudget()
	slotsBefore := fixture.effects.realm.AvailableCompletePaneSlots()
	fixture.effects.journalMu.Unlock()
	var commandWrites atomic.Int32
	fixture.effects.rotationEdge = func(_ string, edge string) {
		if edge == "before_composite_write" {
			commandWrites.Add(1)
		}
	}
	if err := fixture.effects.rotateSession(ctx, sessionID); !errors.Is(err, ErrUnifiedRotateSlotsExhausted) {
		t.Fatalf("capacity refusal error=%v", err)
	}
	if got := commandWrites.Load(); got != 0 {
		t.Fatalf("capacity refusal issued %d observer composite writes", got)
	}
	unit.holder.mu.Lock()
	afterWitness, afterCommitted, afterRotation := unit.holder.witness, unit.holder.committed, unit.holder.rotation
	unit.holder.mu.Unlock()
	if beforeWitness != afterWitness || beforeCommitted != afterCommitted || beforeRotation != afterRotation || afterRotation != nil {
		t.Fatal("capacity refusal flipped the holder")
	}
	fixture.effects.mu.Lock()
	rotation := fixture.effects.rotation
	fixture.effects.mu.Unlock()
	fixture.registry.mu.Lock()
	rotations, generations := len(fixture.registry.rotations), len(fixture.registry.rotationGenerations)
	fixture.registry.mu.Unlock()
	fixture.effects.journalMu.Lock()
	chargedAfter, reservedAfter, capAfter := fixture.effects.realm.PhysicalBudget()
	slotsAfter := fixture.effects.realm.AvailableCompletePaneSlots()
	fixture.effects.journalMu.Unlock()
	if rotation != nil || rotations != 0 || generations != 0 {
		t.Fatalf("capacity refusal retained authority: rotation=%v routes=%d generations=%d", rotation != nil, rotations, generations)
	}
	if chargedAfter != chargedBefore || reservedAfter != reservedBefore || capAfter != capBefore || slotsAfter != slotsBefore {
		t.Fatalf("capacity refusal changed ledger: before=(%d,%d,%d,%d) after=(%d,%d,%d,%d)", chargedBefore, reservedBefore, capBefore, slotsBefore, chargedAfter, reservedAfter, capAfter, slotsAfter)
	}
	owner, err := fixture.registry.retention.acquireGeometryOwner(adoption.Key)
	if err != nil {
		t.Fatalf("capacity refusal retained predecessor owner: %v", err)
	}
	fixture.registry.retention.releaseGeometryOwner(adoption.Key, owner)
}

func TestUnifiedRotationConsumesRefusedPostSealAbort(t *testing.T) {
	fixture := newRotationFixture(t)
	next := fixture.next(2)
	reservation := fixture.materialize(next)
	txn, err := fixture.registry.BeginPaneRotation(fixture.previous, next)
	if err != nil {
		t.Fatal(err)
	}
	fixture.writeBootstrap(txn)
	if err := txn.Validate(); err != nil {
		t.Fatal(err)
	}
	fixture.sealPredecessor(t)
	txn.markSealed()
	rotation := &unifiedDevRotation{registryT: txn}
	if rotation.abortRegistry() {
		t.Fatal("rotation flow treated a refused post-seal Abort as rollback success")
	}
	if rotation.registryT != txn || txn.settled {
		t.Fatalf("refused Abort discarded registry ownership: txn=%v settled=%v", rotation.registryT == txn, txn.settled)
	}
	if disposition := txn.Commit(); disposition != paneRotationCommitted {
		t.Fatalf("Commit after refused Abort disposition=%d", disposition)
	}
	fixture.effects.journalMu.Lock()
	reservation.Commit()
	fixture.effects.journalMu.Unlock()
}

func TestUnifiedRotationN4AttachRaceNeverStrandsPredecessorTail(t *testing.T) {
	fixture := newAdoptionFixture(t, 8)
	const marker = "ROTATION-ATTACH-RACE-MARKER"
	sessionID := fixture.startPaneCommand(t, "rotation-attach-race", `sh -c 'stty -echo; printf "ROTATION-ATTACH-RACE-MARKER\\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 10*time.Second, "attach-race marker", func() bool {
		return bytes.Contains(fixture.journalBytes(t, adoption.Key), []byte(marker))
	})

	commitEdge := make(chan struct{})
	releaseCommit := make(chan struct{})
	fixture.effects.rotationEdge = func(_ string, edge string) {
		if edge == "before_seal_start" {
			close(commitEdge)
			<-releaseCommit
		}
	}
	rotateDone := make(chan error, 1)
	go func() { rotateDone <- fixture.effects.rotateSession(ctx, sessionID) }()
	<-commitEdge

	type opened struct {
		events     []unifiedjournal.Event
		subscriber *unifiedDevSubscriber
		cancel     func()
		err        error
	}
	openOne := func() opened {
		events, _, subscriber, stop, openErr := openSnapshotTailForTest(t, fixture.effects, sessionID)
		return opened{events: events, subscriber: subscriber, cancel: stop, err: openErr}
	}
	var results []opened
	for index := 0; index < 4; index++ {
		results = append(results, openOne())
	}
	const racers = 32
	resultCh := make(chan opened, racers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for index := 0; index < racers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			resultCh <- openOne()
		}()
	}
	close(start)
	close(releaseCommit)
	group.Wait()
	close(resultCh)
	for result := range resultCh {
		results = append(results, result)
	}
	if err := <-rotateDone; err != nil {
		t.Fatal(err)
	}
	results = append(results, openOne())

	oldCount, newCount := 0, 0
	for _, result := range results {
		if result.err != nil {
			t.Fatalf("racing snapshot open: %v", result.err)
		}
		if count := bytes.Count(rotationSnapshotBytes(result.events), []byte(marker)); count != 1 {
			t.Fatalf("racing snapshot marker count=%d want 1", count)
		}
		select {
		case deliveredForTest, open := <-result.subscriber.events():
			if open {
				result.subscriber.releaseEvent(deliveredForTest)
			}
			if open {
				t.Fatal("static racing tail unexpectedly carried output")
			}
			if reason := result.subscriber.closeReason(); reason != proto.SubscriberClosedGenerationRotated {
				t.Fatalf("old racing tail reason=%q", reason)
			}
			oldCount++
		default:
			newCount++
		}
		result.cancel()
	}
	if oldCount == 0 || newCount == 0 {
		t.Fatalf("attach race did not cover both generations: old=%d new=%d", oldCount, newCount)
	}
	fixture.effects.subscriberMu.Lock()
	stranded := len(fixture.effects.subscribers[adoption.Key])
	fixture.effects.subscriberMu.Unlock()
	if stranded != 0 {
		t.Fatalf("predecessor retains %d racing subscribers", stranded)
	}
}

func TestUnifiedRotationN4ActiveSwapWindowBindsSuccessorSnapshotAndTail(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	const bootstrap = "ROTATION-N4-BOOTSTRAP"
	const pending = "ROTATION-N4-PENDING"
	const after = "ROTATION-N4-AFTER"
	sessionID := fixture.startPaneCommand(t, "rotation-n4-window", `sh -c 'stty -echo; printf "ROTATION-N4-BOOTSTRAP\\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 10*time.Second, "N4 bootstrap", func() bool {
		return bytes.Contains(fixture.journalBytes(t, adoption.Key), []byte(bootstrap))
	})
	_, _, predecessor, cancelPredecessor, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelPredecessor()

	preCommit := make(chan struct{})
	releasePreCommit := make(chan struct{})
	window := make(chan struct{})
	release := make(chan struct{})
	fixture.effects.rotationEdge = func(_ string, edge string) {
		switch edge {
		case "before_registry_commit":
			close(preCommit)
			<-releasePreCommit
		case "active_swapped":
			close(window)
			<-release
		}
	}
	rotateDone := make(chan error, 1)
	go func() { rotateDone <- fixture.effects.rotateSession(ctx, sessionID) }()
	<-preCommit
	preEvents, _, preCommitSubscriber, cancelPreCommit, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		close(releasePreCommit)
		t.Fatalf("open snapshot before registry commit: %v", err)
	}
	defer cancelPreCommit()
	if count := bytes.Count(rotationSnapshotBytes(preEvents), []byte(bootstrap)); count != 1 {
		close(releasePreCommit)
		t.Fatalf("pre-commit predecessor bootstrap count=%d want 1", count)
	}
	fixture.effects.subscriberMu.Lock()
	_, preCommitOnOld := fixture.effects.subscribers[adoption.Key][preCommitSubscriber]
	fixture.effects.subscriberMu.Unlock()
	if !preCommitOnOld {
		close(releasePreCommit)
		t.Fatal("pre-commit snapshot did not bind the predecessor")
	}
	close(releasePreCommit)
	<-window

	events, _, successor, cancelSuccessor, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		close(release)
		t.Fatalf("open snapshot in active-swap window: %v", err)
	}
	defer cancelSuccessor()
	if count := bytes.Count(rotationSnapshotBytes(events), []byte(bootstrap)); count != 1 {
		close(release)
		t.Fatalf("successor bootstrap count=%d want 1", count)
	}
	fixture.effects.mu.Lock()
	rotation := fixture.effects.rotation
	fixture.effects.mu.Unlock()
	if rotation == nil || rotation.journal == nil || rotation.registryT == nil {
		close(release)
		t.Fatalf("active-swap window owners: rotation=%v journal=%v registry=%v", rotation != nil, rotation != nil && rotation.journal != nil, rotation != nil && rotation.registryT != nil)
	}
	rotation.holder.mu.Lock()
	appendErr := rotation.holder.rotation.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: []byte(pending)})
	rotation.holder.mu.Unlock()
	if appendErr != nil {
		close(release)
		t.Fatal(appendErr)
	}
	if reason := waitRotationSubscriberClose(t, predecessor); reason != proto.SubscriberClosedGenerationRotated {
		close(release)
		t.Fatalf("predecessor reason=%q", reason)
	}
	if reason := waitRotationSubscriberClose(t, preCommitSubscriber); reason != proto.SubscriberClosedGenerationRotated {
		close(release)
		t.Fatalf("pre-commit subscriber reason=%q", reason)
	}
	fixture.effects.subscriberMu.Lock()
	_, successorOnNew := fixture.effects.subscribers[rotation.newKey][successor]
	_, successorOnOld := fixture.effects.subscribers[rotation.oldKey][successor]
	fixture.effects.subscriberMu.Unlock()
	if !successorOnNew || successorOnOld {
		close(release)
		t.Fatalf("window subscriber registration: new=%v old=%v", successorOnNew, successorOnOld)
	}
	close(release)
	if err := <-rotateDone; err != nil {
		t.Fatal(err)
	}
	postEvents, _, postCommitSubscriber, cancelPostCommit, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatalf("open snapshot after journal commit: %v", err)
	}
	defer cancelPostCommit()
	if count := bytes.Count(rotationSnapshotBytes(postEvents), []byte(bootstrap)); count != 1 {
		t.Fatalf("post-commit successor bootstrap count=%d want 1", count)
	}
	fixture.effects.subscriberMu.Lock()
	_, postCommitOnNew := fixture.effects.subscribers[rotation.newKey][postCommitSubscriber]
	fixture.effects.subscriberMu.Unlock()
	if !postCommitOnNew {
		t.Fatal("post-commit snapshot did not bind the successor")
	}

	if err := fixture.registry.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: rotation.next, Data: []byte(after)}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.registry.retentionBoundary(rotation.next, "n4_post_rotation"); err != nil {
		t.Fatal(err)
	}
	var live []byte
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for bytes.Count(live, []byte(pending)) != 1 || bytes.Count(live, []byte(after)) != 1 {
		select {
		case event, open := <-successor.events():
			if open {
				successor.releaseEvent(event)
			}
			if !open {
				t.Fatalf("successor closed with reason %q", successor.closeReason())
			}
			live = append(live, event.Payload...)
		case <-deadline.C:
			t.Fatalf("successor live bytes=%q", live)
		}
	}
	if bytes.Count(live, []byte(pending)) != 1 || bytes.Count(live, []byte(after)) != 1 {
		t.Fatalf("successor tail duplication: %q", live)
	}
}

func TestUnifiedRotationN5GeometryOwnerExcludesFitAcrossBothKeys(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "rotation-owner", `sh -c 'stty -echo; printf "ROTATION-OWNER\\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := fixture.registry.retention.acquireGeometryOwner(adoption.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.effects.rotateSession(ctx, sessionID); !errors.Is(err, ErrUnifiedRotateInProgress) {
		t.Fatalf("Fit-owned rotation error=%v", err)
	}
	fixture.registry.retention.releaseGeometryOwner(adoption.Key, owner)

	oldHeld := make(chan struct{})
	releaseOld := make(chan struct{})
	newHeld := make(chan struct{})
	releaseNew := make(chan struct{})
	fixture.effects.rotationEdge = func(_ string, edge string) {
		switch edge {
		case "successor_owner_held":
			close(oldHeld)
			<-releaseOld
		case "active_swapped":
			close(newHeld)
			<-releaseNew
		}
	}
	rotateDone := make(chan error, 1)
	go func() { rotateDone <- fixture.effects.rotateSession(ctx, sessionID) }()
	issuer := &unifiedGeometryIssuer{provider: fixture.effects, registry: fixture.registry, session: sessionID}
	<-oldHeld
	if ticket, err := issuer.BeginGeometry(ctx); ticket != nil || !terminal.IsResizeRefusal(err) || !errors.Is(err, errGeometryPauseOwned) {
		t.Fatalf("old-key owner Fit result ticket=%T err=%v", ticket, err)
	}
	close(releaseOld)
	<-newHeld
	if ticket, err := issuer.BeginGeometry(ctx); ticket != nil || !terminal.IsResizeRefusal(err) || !errors.Is(err, errGeometryPauseOwned) {
		t.Fatalf("new-key owner Fit result ticket=%T err=%v", ticket, err)
	}
	close(releaseNew)
	if err := <-rotateDone; err != nil {
		t.Fatal(err)
	}
	ticket, err := issuer.BeginGeometry(ctx)
	if err != nil {
		t.Fatalf("post-rotation Fit retry: %v", err)
	}
	ticket.Release()
}

func TestUnifiedRotationN7NoSubscriberReopensFromSuccessorBootstrap(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	const marker = "ROTATION-NO-SUBSCRIBER"
	sessionID := fixture.startPaneCommand(t, "rotation-no-subscriber", `sh -c 'stty -echo; printf "ROTATION-NO-SUBSCRIBER\\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 10*time.Second, "no-subscriber marker", func() bool {
		return bytes.Contains(fixture.journalBytes(t, adoption.Key), []byte(marker))
	})
	if err := fixture.effects.rotateSession(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	events, _, subscriber, stop, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if count := bytes.Count(rotationSnapshotBytes(events), []byte(marker)); count != 1 {
		t.Fatalf("no-subscriber reopen marker count=%d want 1", count)
	}
	select {
	case deliveredForTest, open := <-subscriber.events():
		if open {
			subscriber.releaseEvent(deliveredForTest)
		}
		if !open {
			t.Fatal("successor subscriber closed immediately")
		}
	default:
	}
}

func TestUnifiedRotationClosesSixPredecessorsOnceAndReopensSuccessorOnce(t *testing.T) {
	fixture := newAdoptionFixture(t, 8)
	const marker = "ROTATION-SIX-SUBSCRIBERS"
	sessionID := fixture.startPaneCommand(t, "rotation-six-subscribers", `sh -c 'stty -echo; printf "ROTATION-SIX-SUBSCRIBERS\\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 10*time.Second, "six-subscriber marker", func() bool {
		return bytes.Contains(fixture.journalBytes(t, adoption.Key), []byte(marker))
	})

	predecessors := make([]*unifiedDevSubscriber, 0, 6)
	cancels := make([]func(), 0, 6)
	for index := 0; index < 6; index++ {
		_, _, subscriber, stop, openErr := openSnapshotTailForTest(t, fixture.effects, sessionID)
		if openErr != nil {
			t.Fatal(openErr)
		}
		predecessors = append(predecessors, subscriber)
		cancels = append(cancels, stop)
	}
	defer func() {
		for _, stop := range cancels {
			stop()
		}
	}()
	// One subscriber deliberately does not drain and has a full event queue.
	// Provider rotation must still close it in the same bounded lock-held pass;
	// the attachment writer's transport-cut behavior is pinned by the B1 suite.
	for {
		event := unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Payload: []byte("wedged")}
		if !predecessors[5].lease.reserveEvent(recordingEventBytes(event)) {
			t.Fatal("fixture could not fund its queued event")
		}
		select {
		case predecessors[5].data <- event:
		default:
			predecessors[5].releaseEvent(event)
			goto wedged
		}
	}

wedged:
	closeCalls := make(map[*unifiedDevSubscriber]int)
	fixture.effects.subscriberCloseEdge = func(key unifiedjournal.PaneKey, subscriber *unifiedDevSubscriber, reason proto.SubscriberCloseReason) {
		if key == adoption.Key && reason == proto.SubscriberClosedGenerationRotated {
			closeCalls[subscriber]++
		}
	}
	if err := fixture.effects.rotateSession(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	for index, subscriber := range predecessors {
		for deliveredForTest := range subscriber.events() {
			subscriber.releaseEvent(deliveredForTest)
		}
		if reason := subscriber.closeReason(); reason != proto.SubscriberClosedGenerationRotated {
			t.Fatalf("predecessor %d close reason=%q", index, reason)
		}
		if closeCalls[subscriber] != 1 {
			t.Fatalf("predecessor %d close calls=%d want 1", index, closeCalls[subscriber])
		}
	}
	fixture.effects.subscriberMu.Lock()
	oldCount := len(fixture.effects.subscribers[adoption.Key])
	fixture.effects.subscriberMu.Unlock()
	if oldCount != 0 {
		t.Fatalf("predecessor subscriber set has %d entries", oldCount)
	}
	newKey, ok := fixture.effects.paneKey(sessionID)
	if !ok || newKey == adoption.Key {
		t.Fatalf("successor key=%#v predecessor=%#v", newKey, adoption.Key)
	}
	for index := 0; index < 6; index++ {
		events, _, subscriber, stop, openErr := openSnapshotTailForTest(t, fixture.effects, sessionID)
		if openErr != nil {
			t.Fatalf("reopen %d: %v", index, openErr)
		}
		if count := bytes.Count(rotationSnapshotBytes(events), []byte(marker)); count != 1 {
			stop()
			t.Fatalf("reopen %d marker count=%d want 1", index, count)
		}
		select {
		case deliveredForTest, open := <-subscriber.events():
			if open {
				subscriber.releaseEvent(deliveredForTest)
			}
			if !open {
				stop()
				t.Fatalf("successor reopen %d closed with %q", index, subscriber.closeReason())
			}
		default:
		}
		stop()
	}
}

func TestUnifiedRotationN8FiftySequentialRotationsConserveCapacity(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "rotation-n8", `sh -c 'stty -echo; printf "ROTATION-N8\\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	key := adoption.Key
	available := fixture.effects.realm.AvailableCompletePaneSlots()
	for attempt := 0; attempt < 50; attempt++ {
		if err := fixture.effects.rotateSession(ctx, sessionID); err != nil {
			t.Fatalf("rotation %d: %v", attempt, err)
		}
		next, ok := fixture.effects.paneKey(sessionID)
		if !ok || next == key {
			t.Fatalf("rotation %d active key=%#v predecessor=%#v", attempt, next, key)
		}
		key = next
		if got := fixture.effects.realm.AvailableCompletePaneSlots(); got != available {
			t.Fatalf("rotation %d available slots=%d want %d", attempt, got, available)
		}
		charged, reserved, cap := fixture.effects.realm.PhysicalBudget()
		if reserved != 0 || charged > cap {
			t.Fatalf("rotation %d physical charged=%d reserved=%d cap=%d", attempt, charged, reserved, cap)
		}
	}
}

func TestUnifiedRotationN8UsesSingleSourcedComposite(t *testing.T) {
	source, err := os.ReadFile("unified_rotation.go")
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(source), "adoptionCompositeLine(rotation.old.Pane)"); count != 1 {
		t.Fatalf("rotation adoptionCompositeLine callsites=%d want 1", count)
	}
	for _, private := range []string{"capture-pane -e -p -N", "display-message -p '#{alternate_on}"} {
		if strings.Contains(string(source), private) {
			t.Fatalf("rotation duplicated adoption composite command %q", private)
		}
	}
}

func TestUnifiedRotationF14StopsBeforeSwapCloseAndReservationCommit(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "rotation-f14", `sh -c 'stty -echo; printf "ROTATION-F14\\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	slotsBefore := fixture.effects.realm.AvailableCompletePaneSlots()
	_, _, predecessor, stop, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	var activeSwapped atomic.Bool
	var captured *unifiedDevRotation
	fixture.effects.rotationEdge = func(_ string, edge string) {
		switch edge {
		case "before_registry_commit":
			fixture.effects.mu.Lock()
			captured = fixture.effects.rotation
			sibling := captured.old
			sibling.Pane = "%rotation-f14-sibling"
			sibling.Incarnation = "rotation-f14-sibling"
			captured.unit.witnesses = append(captured.unit.witnesses, sibling)
			fixture.effects.mu.Unlock()
		case "active_swapped":
			activeSwapped.Store(true)
		}
	}
	if err := fixture.effects.rotateSession(ctx, sessionID); !errors.Is(err, ErrUnifiedRotateFatal) {
		t.Fatalf("F14 rotation error=%v", err)
	}
	if activeSwapped.Load() {
		t.Fatal("F14 reached active swap")
	}
	if reason := waitRotationSubscriberClose(t, predecessor); reason != proto.SubscriberClosedGenerationFailed {
		t.Fatalf("F14 predecessor close reason=%q want %q", reason, proto.SubscriberClosedGenerationFailed)
	}
	assertPostPONRFatalSettled(t, fixture, sessionID, captured, slotsBefore)
	if _, err := fixture.effects.realm.ReadCommittedEvents(adoption.Key); !errors.Is(err, unifiedjournal.ErrInvalidated) {
		t.Fatalf("F14 left the sealed predecessor readable: %v", err)
	}
	fixture.effects.subscriberMu.Lock()
	_, predecessorBucket := fixture.effects.subscribers[adoption.Key]
	fixture.effects.subscriberMu.Unlock()
	if predecessorBucket {
		t.Fatal("F14 retained predecessor subscriber bucket")
	}
}

func TestUnifiedRotationF14RemovesAlreadyEmptyPredecessorBucket(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "rotation-f14-empty", `sh -c 'stty -echo; printf "ROTATION-F14-EMPTY\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	slotsBefore := fixture.effects.realm.AvailableCompletePaneSlots()
	_, _, _, stop, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	// Model the exact scheduling edge where the subscriber has departed but
	// fatal settlement still observes the outer bucket allocated earlier.
	fixture.effects.subscriberMu.Lock()
	fixture.effects.subscribers[adoption.Key] = map[*unifiedDevSubscriber]struct{}{}
	fixture.effects.subscriberMu.Unlock()
	var captured *unifiedDevRotation
	fixture.effects.rotationEdge = func(_ string, edge string) {
		if edge != "before_registry_commit" {
			return
		}
		fixture.effects.mu.Lock()
		captured = fixture.effects.rotation
		sibling := captured.old
		sibling.Pane = "%rotation-f14-empty-sibling"
		sibling.Incarnation = "rotation-f14-empty-sibling"
		captured.unit.witnesses = append(captured.unit.witnesses, sibling)
		fixture.effects.mu.Unlock()
	}
	if err := fixture.effects.rotateSession(ctx, sessionID); !errors.Is(err, ErrUnifiedRotateFatal) {
		t.Fatalf("F14 empty-bucket rotation error=%v", err)
	}
	assertPostPONRFatalSettled(t, fixture, sessionID, captured, slotsBefore)
	fixture.effects.subscriberMu.Lock()
	_, predecessorBucket := fixture.effects.subscribers[adoption.Key]
	fixture.effects.subscriberMu.Unlock()
	if predecessorBucket {
		t.Fatal("post-seal fatal retained the already-empty predecessor subscriber bucket")
	}
}

func TestUnifiedRotationSealWaitFailureNeverRollsBack(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	var oldKey unifiedjournal.PaneKey
	var failSeal atomic.Bool
	sealFailure := errors.New("injected rotation seal flush failure")
	fixture.registry.retention.options.stage = func(operation string, key unifiedjournal.PaneKey) error {
		if failSeal.Load() && operation == "append" && key == oldKey {
			return sealFailure
		}
		return nil
	}
	sessionID := fixture.startPaneCommand(t, "rotation-seal-wait-failure", `sh -c 'stty -echo; printf "ROTATION-SEAL-WAIT-FAILURE\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	slotsBefore := fixture.effects.realm.AvailableCompletePaneSlots()
	oldKey = adoption.Key

	var captured *unifiedDevRotation
	var restoreWait, activeSwapped atomic.Bool
	fixture.effects.rotationEdge = func(_ string, edge string) {
		switch edge {
		case "before_seal_start":
			// FIFO puts this ordinary predecessor output immediately before the
			// retiring seal. Its failed flush makes the seal wait fail after the
			// boundary was accepted: this is past the point of no return.
			if writeErr := fixture.registry.retention.WritePane(oldKey, []byte("FORCE-SEAL-FLUSH")); writeErr != nil {
				t.Errorf("queue predecessor output: %v", writeErr)
			}
			failSeal.Store(true)
		case "before_seal_wait":
			fixture.effects.mu.Lock()
			captured = fixture.effects.rotation
			fixture.effects.mu.Unlock()
		case "before_restore_wait":
			restoreWait.Store(true)
		case "active_swapped":
			activeSwapped.Store(true)
		}
	}
	if err := fixture.effects.rotateSession(ctx, sessionID); !errors.Is(err, ErrUnifiedRotateFatal) || !errors.Is(err, unifiedjournal.ErrStorage) {
		t.Fatalf("seal-wait rotation error=%v, want fatal injected failure", err)
	}
	if captured == nil || !captured.ponr || !captured.settled {
		t.Fatalf("seal-wait fatal settlement: rotation=%v ponr=%v settled=%v", captured != nil, captured != nil && captured.ponr, captured != nil && captured.settled)
	}
	if restoreWait.Load() {
		t.Fatal("failed retiring-seal wait entered 4R restore")
	}
	if activeSwapped.Load() {
		t.Fatal("failed retiring-seal wait reached active swap")
	}
	assertPostPONRFatalSettled(t, fixture, sessionID, captured, slotsBefore)
}

func TestUnifiedRotationPostPONRPendingOverflowUsesFatalSettlement(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "rotation-post-ponr-overflow", `sh -c 'stty -echo; printf "ROTATION-POST-PONR-OVERFLOW\\n"; sleep 60'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	slotsBefore := fixture.effects.realm.AvailableCompletePaneSlots()
	var captured *unifiedDevRotation
	fixture.effects.rotationEdge = func(_ string, edge string) {
		fixture.effects.mu.Lock()
		rotation := fixture.effects.rotation
		fixture.effects.mu.Unlock()
		if edge != "active_swapped" || rotation == nil || !rotation.ponr {
			return
		}
		captured = rotation
		rotation.holder.mu.Lock()
		rotation.holder.rotation.data = make([]byte, unifiedjournal.RotationPendingCapBytes+1)
		rotation.holder.mu.Unlock()
	}
	err = fixture.effects.rotateSession(ctx, sessionID)
	if !errors.Is(err, ErrUnifiedRotateFatal) || !errors.Is(err, ErrUnifiedRotatePendingOverflow) {
		t.Fatalf("post-PONR pending overflow error=%v", err)
	}
	assertPostPONRFatalSettled(t, fixture, sessionID, captured, slotsBefore)
}

func TestUnifiedRotationPostPONRStorageFailureUsesFatalSettlement(t *testing.T) {
	for _, unlinkMode := range []string{"success", "enoent", "eio"} {
		for _, leaveBeforeFatal := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/leave_before_fatal_%v", unlinkMode, leaveBeforeFatal), func(t *testing.T) {
				fixture := newAdoptionFixture(t, 4)
				var failKey unifiedjournal.PaneKey
				var failSuccessor atomic.Bool
				failure := errors.New("injected successor pending append failure")
				fixture.registry.retention.options.stage = func(operation string, key unifiedjournal.PaneKey) error {
					if failSuccessor.Load() && operation == "append" && key == failKey {
						return failure
					}
					return nil
				}
				sessionID := fixture.startPaneCommand(t, "rotation-post-ponr-storage", `sh -c 'stty -echo; printf "ROTATION-POST-PONR-STORAGE\\n"; sleep 60'`)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_, err := fixture.effects.AdoptSession(ctx, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				beforeFiles := make(map[string]struct{})
				for _, path := range unifiedE2E1JournalFiles(t, fixture.runtimeDir) {
					beforeFiles[path] = struct{}{}
				}
				slotsBefore := fixture.effects.realm.AvailableCompletePaneSlots()
				var captured *unifiedDevRotation
				var successorSubscriber *unifiedDevSubscriber
				var successorStop func()
				var changedDirectory string
				fixture.effects.rotationEdge = func(_ string, edge string) {
					if edge != "active_swapped" {
						return
					}
					fixture.effects.mu.Lock()
					captured = fixture.effects.rotation
					fixture.effects.mu.Unlock()
					_, _, successorSubscriber, successorStop, err = openSnapshotTailForTest(t, fixture.effects, sessionID)
					if err != nil {
						t.Errorf("open committed successor tail: %v", err)
						return
					}
					if leaveBeforeFatal {
						successorStop()
						// Preserve the exact settlement ordering under test: the last
						// member has departed, but the outer bucket is still present
						// when committed-successor fatal cleanup begins.
						fixture.effects.subscriberMu.Lock()
						fixture.effects.subscribers[captured.newKey] = map[*unifiedDevSubscriber]struct{}{}
						fixture.effects.subscriberMu.Unlock()
					}
					captured.holder.mu.Lock()
					if appendErr := captured.holder.rotation.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: []byte("PENDING-STORAGE-FAILURE")}); appendErr != nil {
						t.Errorf("append pending: %v", appendErr)
					}
					captured.holder.mu.Unlock()
					if unlinkMode != "success" {
						files, listErr := rotationJournalFiles(fixture.runtimeDir)
						if listErr != nil {
							t.Errorf("list successor journals: %v", listErr)
							return
						}
						for _, path := range files {
							if _, existed := beforeFiles[path]; existed {
								continue
							}
							switch unlinkMode {
							case "enoent":
								if removeErr := os.Remove(path); removeErr != nil {
									t.Errorf("remove successor journal: %v", removeErr)
								}
							case "eio":
								changedDirectory = filepath.Dir(path)
								if chmodErr := os.Chmod(changedDirectory, 0o500); chmodErr != nil {
									t.Errorf("make successor directory read-only: %v", chmodErr)
								}
							}
						}
					}
					failKey = captured.newKey
					failSuccessor.Store(true)
				}
				err = fixture.effects.rotateSession(ctx, sessionID)
				if changedDirectory != "" {
					if chmodErr := os.Chmod(changedDirectory, 0o700); chmodErr != nil {
						t.Fatal(chmodErr)
					}
				}
				if !errors.Is(err, ErrUnifiedRotateFatal) || !errors.Is(err, unifiedjournal.ErrStorage) {
					t.Fatalf("post-PONR storage error=%v", err)
				}
				if successorSubscriber == nil {
					t.Fatal("post-commit failure did not open successor subscriber")
				}
				if leaveBeforeFatal {
					if reason := successorSubscriber.closeReason(); reason != "" {
						t.Fatalf("self-removed successor got provider reason=%q", reason)
					}
				} else if reason := waitRotationSubscriberClose(t, successorSubscriber); reason != proto.SubscriberClosedGenerationFailed {
					t.Fatalf("failed successor close reason=%q want %q", reason, proto.SubscriberClosedGenerationFailed)
				}
				fixture.effects.subscriberMu.Lock()
				_, successorBucket := fixture.effects.subscribers[captured.newKey]
				fixture.effects.subscriberMu.Unlock()
				if successorBucket {
					t.Fatal("committed-successor fatal retained an already-empty subscriber bucket")
				}
				assertPostPONRFatalSettled(t, fixture, sessionID, captured, slotsBefore)
			})
		}
	}
}

func TestUnifiedSnapshotReadNeverHoldsSubscriberMutex(t *testing.T) {
	realm := openRetentionRealm(t, "rotation-snapshot-lock")
	key := unifiedjournal.PaneKey{Server: "main", Session: "$1", ControlGeneration: 1, Window: "@1", Pane: "%1", Incarnation: "snapshot-lock"}
	if err := realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	effects := &UnifiedDevPaneEffects{
		realm: realm, active: map[string]unifiedjournal.PaneKey{"$1": key},
		subscribers: make(map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}),
	}
	called := false
	registry := newPaneRegistry(effects)
	effects.observer = registry
	witness := controlmode.PaneWitness{Session: controlmode.SessionWitness{Server: key.Server, Session: key.Session, ControlGeneration: key.ControlGeneration}, Window: key.Window, Pane: key.Pane, Incarnation: key.Incarnation}
	if err := registry.AdmitPane(witness); err != nil {
		t.Fatal(err)
	}
	commitRecordingInitialForTest(t, registry, witness, nil)
	t.Cleanup(func() { _ = registry.Close() })
	effects.snapshotReadEdge = func() {
		called = true
		if !effects.subscriberMu.TryLock() {
			t.Fatal("snapshot storage read held subscriberMu")
		}
		effects.subscriberMu.Unlock()
	}
	_, _, _, stop, err := openSnapshotTailForTest(t, effects, "$1")
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if !called {
		t.Fatal("snapshot storage-read edge was not exercised")
	}
}

func TestUnifiedRotationN14PublicCloseWrapperUnderLockTimesOut(t *testing.T) {
	const child = "PERSEA_ROTATION_N14_CHILD"
	if os.Getenv(child) == "1" {
		key := unifiedjournal.PaneKey{Server: "main", Session: "$1", ControlGeneration: 1}
		effects := &UnifiedDevPaneEffects{subscribers: map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}{}}
		keepalive := time.NewTimer(time.Hour)
		defer keepalive.Stop()
		go func() { <-keepalive.C }()
		effects.subscriberMu.Lock()
		_ = os.WriteFile(os.Getenv("PERSEA_ROTATION_N14_READY"), []byte("ready"), 0o600)
		effects.closeSubscribers(key, proto.SubscriberClosedGenerationRotated)
		os.Exit(0)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(os.Args[0], "-test.run=^TestUnifiedRotationN14PublicCloseWrapperUnderLockTimesOut$")
	command.Env = append(os.Environ(), child+"=1", "PERSEA_ROTATION_N14_READY="+ready)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-waited:
			t.Fatalf("mutant exited before reaching held-lock wrapper: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			<-waited
			t.Fatal("mutant never reached held-lock wrapper")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-waited:
		t.Fatalf("public wrapper mutant did not deadlock: %v", err)
	case <-time.After(100 * time.Millisecond):
		_ = command.Process.Kill()
		<-waited
	}
}

func TestUnifiedRotationPendingShapeCountsOneByteObservations(t *testing.T) {
	var pending unifiedRotationPending
	for index := int64(0); index < unifiedjournal.RotationPendingCapBytes; index++ {
		if err := pending.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: []byte{'x'}}); err != nil {
			t.Fatalf("one-byte append %d: %v", index, err)
		}
	}
	if err := pending.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: []byte{'x'}}); !errors.Is(err, ErrUnifiedRotatePendingOverflow) {
		t.Fatalf("one-byte overflow error=%v", err)
	}
	if int64(len(pending.data)) != unifiedjournal.RotationPendingCapBytes {
		t.Fatalf("pending bytes=%d", len(pending.data))
	}

	var recordBound unifiedRotationPending
	chunk := bytes.Repeat([]byte{'r'}, rotationReplayBatchBytes)
	for record := int64(0); record < unifiedjournal.RotationPendingCapRecords; record++ {
		if err := recordBound.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: chunk}); err != nil {
			t.Fatalf("64-KiB record %d: %v", record, err)
		}
	}
	if got := int64(len(recordBound.data)); got != unifiedjournal.RotationPendingCapBytes {
		t.Fatalf("record-bound bytes=%d want %d", got, unifiedjournal.RotationPendingCapBytes)
	}
	if err := recordBound.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: []byte{'!'}}); !errors.Is(err, ErrUnifiedRotatePendingOverflow) {
		t.Fatalf("seventeenth-record overflow error=%v", err)
	}
}

func TestUnifiedRotationN10PrePONRRestoreWaitsForDurableReplay(t *testing.T) {
	for _, materialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "pre_materialization", true: "post_materialization"}[materialized], func(t *testing.T) {
			fixture := newAdoptionFixture(t, 4)
			sessionID := fixture.startPaneCommand(t, "rotation-restore", `sh -c 'stty -echo; printf "ROTATION-RESTORE-PRE\\n"; sleep 60'`)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			unit := fixture.effects.units[sessionID]
			old, found := unit.rotationWitness(adoption.Key)
			if !found {
				t.Fatal("missing predecessor witness")
			}
			next, err := unit.unusedRotationWitness(old)
			if err != nil {
				t.Fatal(err)
			}
			oldOwner, err := fixture.registry.retention.acquireGeometryOwner(adoption.Key)
			if err != nil {
				t.Fatal(err)
			}
			defer fixture.registry.retention.releaseGeometryOwner(adoption.Key, oldOwner)
			fixture.effects.journalMu.Lock()
			capacity, err := fixture.effects.realm.BeginRotationCapacity(adoption.Key)
			fixture.effects.journalMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			rotation := &unifiedDevRotation{
				session: sessionID, oldKey: adoption.Key, unit: unit, registry: fixture.registry,
				capacity: capacity, oldOwner: oldOwner, holder: unit.holder, old: old,
				next: next, newKey: journalKey(next), flipped: true,
			}
			if materialized {
				rotation.newOwner, err = fixture.registry.retention.acquireGeometryOwner(rotation.newKey)
				if err != nil {
					t.Fatal(err)
				}
				rotation.newHeld = true
				defer fixture.registry.retention.releaseGeometryOwner(rotation.newKey, rotation.newOwner)
				if err := fixture.effects.reserveJournalSource(rotation.newKey, unit.holder.source); err != nil {
					t.Fatal(err)
				}
				fixture.effects.journalMu.Lock()
				rotation.journal, err = fixture.effects.realm.BeginRotatedPane(rotation.newKey, unifiedjournal.Geometry{Columns: 80, Rows: 24}, capacity)
				fixture.effects.journalMu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				rotation.registryT, err = fixture.registry.BeginPaneRotation(old, next)
				if err != nil {
					t.Fatal(err)
				}
			}
			pending := &unifiedRotationPending{}
			for _, value := range []byte("RESTORE-ORDER-ONE-BYTE") {
				if err := pending.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: []byte{value}}); err != nil {
					t.Fatal(err)
				}
			}
			rotation.holder.mu.Lock()
			rotation.holder.witness = next
			rotation.holder.committed = false
			rotation.holder.rotation = pending
			rotation.holder.mu.Unlock()
			_, _, subscriber, stop, err := openSnapshotTailForTest(t, fixture.effects, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			if err := rotation.restore(nil); err != nil {
				t.Fatal(err)
			}
			committed := fixture.journalBytes(t, adoption.Key)
			if count := bytes.Count(committed, []byte("RESTORE-ORDER-ONE-BYTE")); count != 1 {
				t.Fatalf("restored journal marker count=%d want 1", count)
			}
			seen := false
			for !seen {
				select {
				case event, open := <-subscriber.events():
					if open {
						subscriber.releaseEvent(event)
					}
					if !open {
						t.Fatal("restore closed predecessor subscriber")
					}
					seen = bytes.Contains(event.Payload, []byte("RESTORE-ORDER-ONE-BYTE"))
				case <-time.After(5 * time.Second):
					t.Fatal("restore boundary returned before replay publication")
				}
			}
			rotation.holder.mu.Lock()
			witness, committedHolder, pendingHolder := rotation.holder.witness, rotation.holder.committed, rotation.holder.rotation
			rotation.holder.mu.Unlock()
			if witness != old || !committedHolder || pendingHolder != nil {
				t.Fatalf("restored holder witness=%#v committed=%v pending=%v", witness, committedHolder, pendingHolder != nil)
			}
		})
	}
}
