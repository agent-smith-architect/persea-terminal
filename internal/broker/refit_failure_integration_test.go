package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

type refit_failureRefitResponse struct {
	Operation       string `json:"operation"`
	Columns         int    `json:"columns"`
	Rows            int    `json:"rows"`
	SuccessorSource string `json:"successor_source"`
}

func refit_failurePostRefit(t *testing.T, client *http.Client, source string, columns, rows int, operation string) refit_failureRefitResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"source": source, "columns": columns, "rows": rows, "operation": operation,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://localhost/api/session-refits", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = frontdoorRotationSecureHeaders()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Persea-CSRF", frontdoorRotationCSRF)
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("refit request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("refit status=%d body=%q", response.StatusCode, payload)
	}
	var result refit_failureRefitResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	rowsMatch := result.Rows == rows
	if rows == 0 {
		rowsMatch = result.Rows > 0
	}
	if result.Operation != operation || result.Columns != columns || !rowsMatch || len(result.SuccessorSource) != 43 {
		t.Fatalf("refit response=%+v", result)
	}
	return result
}

// TestRefitPostPONRCaptureFailureCarriesClosedMetadata is the independent
// A real tmux refit
// composite conservatively crosses PONR before the capture is judged, so even
// a capture drift must retain a closed stage/class through fatal settlement.
func TestRefitPostPONRCaptureFailureCarriesClosedMetadata(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "refit_failure-post-ponr-capture", `sh -c 'stty -echo; while :; do sleep 1; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	wantColumns := source.Columns + 11
	fixture.effects.adoptionPostTamper = func(_ int, post string) string { return post + " REFIT_FAILURE_CAPTURE_DRIFT" }
	operation := strings.Repeat("v", 43)
	err = fixture.effects.refitSession(ctx, authority, source, wantColumns, operation)
	if !errors.Is(err, ErrUnifiedRefitFatal) {
		t.Fatalf("capture failure=%v want ErrUnifiedRefitFatal", err)
	}
	landed, witnessErr := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if witnessErr != nil || landed.Columns != wantColumns {
		t.Fatalf("capture failure did not cross width PONR: landed=%+v err=%v", landed, witnessErr)
	}
	stage, class, ok := refitFailureMetadata(err)
	if !ok || stage != refitFailureCapture || class != proto.RefitFailureInternal {
		t.Fatalf("post-PONR capture metadata=(%q,%q,%t) want=(%q,%q,true): err=%v", stage, class, ok, refitFailureCapture, proto.RefitFailureInternal, err)
	}
	fixture.registry.retention.mu.Lock()
	settlementErr := fixture.registry.retention.closeErr
	fixture.registry.retention.mu.Unlock()
	settledStage, settledClass, settled := refitFailureMetadata(settlementErr)
	if !settled || settledStage != refitFailureCapture || settledClass != proto.RefitFailureInternal {
		t.Fatalf("capture settlement metadata=(%q,%q,%t) err=%v", settledStage, settledClass, settled, settlementErr)
	}
	var wire bytes.Buffer
	(&Server{config: fixture.cfg, unified: fixture.effects}).refit(&lockedWriter{w: &wire}, proto.Control{
		Type: "refit", Authority: &authority, Cols: wantColumns, ID: operation,
	})
	frame, frameErr := proto.ReadFrame(&wire)
	if frameErr != nil {
		t.Fatal(frameErr)
	}
	response, decodeErr := proto.DecodeControl(frame.Payload)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if response.Code != "refit_faulted" || response.RefitStage != refitFailureCapture || response.RefitClass != proto.RefitFailureInternal {
		t.Fatalf("capture broker response=%+v", response)
	}
}

func refit_failureWriteAndObserve(t *testing.T, ws *frontdoorRotationClient, prepared terminal.Frame, marker string) {
	t.Helper()
	input, err := attachmentwire.Encode(terminal.Frame{
		Version: terminal.ProtocolVersion, Type: terminal.FrameInput,
		Source: prepared.Source, Epoch: prepared.Epoch, Data: []byte("printf '" + marker + "\\n'\n"),
	}, attachmentwire.BrowserToServer)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.TextMessage, input); err != nil {
		t.Fatal(err)
	}
	if err := ws.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var visible []byte
	for !bytes.Contains(visible, []byte(marker)) {
		kind, payload, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("input %q was not live: visible=%q err=%v", marker, visible, err)
		}
		if kind != websocket.TextMessage {
			continue
		}
		frame, err := attachmentwire.Decode(payload, attachmentwire.ServerToBrowser)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == terminal.FrameLive {
			visible = append(visible, frame.Data...)
		}
	}
	if err := ws.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func refit_failurePreparedCount(prepared terminal.Frame, marker string) int {
	return bytes.Count(prepared.Replay, []byte(marker)) +
		bytes.Count([]byte(strings.Join(prepared.History, "\n")), []byte(marker))
}

// This is the production shape that a direct effects.refitSession call cannot
// model: each successful refit closes one attached controller, the front door
// mints a distinct one-time handle, and the successor COMMIT publishes a fresh
// source binding before the next explicit refit.
func TestRefitFrontdoorRepeatedRefitAfterFreshController(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux/frontdoor repeated-refit regression test")
	}
	disposable := newDisposable(t)
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	runtimeDir := t.TempDir()
	cfg := unifiedAdoptionDevConfig(t, tmuxServer, runtimeDir, 4)
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var edgeMu sync.Mutex
	var refitEdges []string
	effects.refitOperationEdge = func(operation, edge string) {
		edgeMu.Lock()
		refitEdges = append(refitEdges, operation+":"+edge)
		edgeMu.Unlock()
	}
	t.Cleanup(func() {
		edgeMu.Lock()
		defer edgeMu.Unlock()
		t.Logf("refit edges=%v", refitEdges)
	})
	socketDir, err := os.MkdirTemp("/tmp", "u17-broker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	listener, err := net.Listen("unix", filepath.Join(socketDir, "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	brokerDone := make(chan error, 1)
	go func() { brokerDone <- ServeWithPaneEffects(listener, cfg, effects) }()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-brokerDone:
		case <-time.After(3 * time.Second):
			t.Error("broker did not stop")
		}
	})
	frontSocket, client := frontdoorRotationStartFrontdoor(t, listener.Addr().String())

	disposable.run("new-session", "-d", "-s", "refit_failure_refit", "-x", "80", "-y", "24", "sh", "-c", "stty -echo; exec sh")
	sessionID := strings.TrimSpace(disposable.run("display-message", "-p", "-t", "refit_failure_refit:", "#{session_id}"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adoption, err := effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &adoptionFixture{effects: effects, runtimeDir: runtimeDir}
	filesBefore := fixture.journalFileCount(t)
	effects.journalMu.Lock()
	slotsBefore := effects.realm.AvailableCompletePaneSlots()
	_, logicalHeldBefore, _ := effects.realm.LogicalBudget()
	_, physicalHeldBefore, _ := effects.realm.PhysicalBudget()
	effects.journalMu.Unlock()
	registry := effects.observer.(*paneRegistry)
	registry.retention.mu.Lock()
	generationsBefore := len(registry.retention.generations)
	pBefore, qBefore, eBefore, bBefore, oBefore := registry.retention.pUsed, registry.retention.qUsed, registry.retention.eUsed, registry.retention.bUsed, registry.retention.oUsed
	registry.retention.mu.Unlock()

	firstHandle := frontdoorRotationInventoryHandle(t, client, sessionID)
	firstWS := frontdoorRotationDialController(t, frontSocket, firstHandle)
	firstPrepared := frontdoorRotationCommitController(t, firstWS)
	refit_failureWriteAndObserve(t, firstWS, firstPrepared, "REFIT_FAILURE-BEFORE-FIRST")
	firstClose := frontdoorRotationDrainUntilClosed(firstWS)
	firstRows := firstPrepared.Rows + 1
	firstResult := refit_failurePostRefit(t, client, firstPrepared.Source, 97, firstRows, strings.Repeat("a", 43))
	if reason := <-firstClose; reason != string(proto.SubscriberClosedGenerationRefit) {
		t.Fatalf("first close=%q want=%q", reason, proto.SubscriberClosedGenerationRefit)
	}
	_ = firstWS.Close()

	secondHandle := frontdoorRotationInventoryHandle(t, client, sessionID)
	if secondHandle == firstHandle {
		t.Fatal("first successor reused consumed handle")
	}
	secondWS := frontdoorRotationDialController(t, frontSocket, secondHandle)
	secondPrepared := frontdoorRotationCommitController(t, secondWS)
	if secondPrepared.Source != firstResult.SuccessorSource || secondPrepared.Columns != 97 || secondPrepared.Rows != firstRows {
		t.Fatalf("first successor PREPARE=%+v response=%+v", secondPrepared, firstResult)
	}
	if count := refit_failurePreparedCount(secondPrepared, "REFIT_FAILURE-BEFORE-FIRST"); count != 1 {
		t.Fatalf("first successor predecessor transcript count=%d want=1", count)
	}
	refit_failureWriteAndObserve(t, secondWS, secondPrepared, "REFIT_FAILURE-BETWEEN-REFITS")
	secondClose := frontdoorRotationDrainUntilClosed(secondWS)
	secondResult := refit_failurePostRefit(t, client, secondPrepared.Source, firstPrepared.Columns, firstPrepared.Rows, strings.Repeat("b", 43))
	if reason := <-secondClose; reason != string(proto.SubscriberClosedGenerationRefit) {
		t.Fatalf("second close=%q want=%q", reason, proto.SubscriberClosedGenerationRefit)
	}
	_ = secondWS.Close()

	thirdHandle := frontdoorRotationInventoryHandle(t, client, sessionID)
	if thirdHandle == firstHandle || thirdHandle == secondHandle {
		t.Fatal("second successor reused a consumed handle")
	}
	thirdWS := frontdoorRotationDialController(t, frontSocket, thirdHandle)
	defer thirdWS.Close()
	thirdPrepared := frontdoorRotationCommitController(t, thirdWS)
	if thirdPrepared.Source != secondResult.SuccessorSource || thirdPrepared.Columns != firstPrepared.Columns || thirdPrepared.Rows != firstPrepared.Rows {
		t.Fatalf("second successor PREPARE=%+v response=%+v", thirdPrepared, secondResult)
	}
	if count := refit_failurePreparedCount(thirdPrepared, "REFIT_FAILURE-BEFORE-FIRST"); count != 1 {
		t.Fatalf("second successor predecessor transcript count=%d want=1", count)
	}
	if count := refit_failurePreparedCount(thirdPrepared, "REFIT_FAILURE-BETWEEN-REFITS"); count != 1 {
		t.Fatalf("second successor inter-refit transcript count=%d want=1", count)
	}
	refit_failureWriteAndObserve(t, thirdWS, thirdPrepared, "REFIT_FAILURE-AFTER-SECOND")

	pollUntil(t, 10*time.Second, "repeated frontdoor refit resource settlement", func() bool {
		effects.journalMu.Lock()
		slots := effects.realm.AvailableCompletePaneSlots()
		_, logicalHeld, _ := effects.realm.LogicalBudget()
		_, physicalHeld, _ := effects.realm.PhysicalBudget()
		effects.journalMu.Unlock()
		registry.retention.mu.Lock()
		generations := len(registry.retention.generations)
		p, q, e, b, o := registry.retention.pUsed, registry.retention.qUsed, registry.retention.eUsed, registry.retention.bUsed, registry.retention.oUsed
		registry.retention.mu.Unlock()
		return fixture.journalFileCount(t) == filesBefore && slots == slotsBefore && logicalHeld == logicalHeldBefore && physicalHeld == physicalHeldBefore &&
			generations == generationsBefore && p == pBefore && q <= qBefore && e <= eBefore && b <= bBefore && o <= oBefore
	})
	if active, ok := effects.paneKey(sessionID); !ok || active == adoption.Key {
		t.Fatalf("second successor is not active: key=%+v ok=%t", active, ok)
	}
}

func TestRefitPostMutationRefitStageFailuresNeverRevivePredecessor(t *testing.T) {
	stages := []struct {
		stage unifiedRefitFailureStage
		cause error
		class proto.RefitFailureClass
	}{
		{refitFailureBindSuccessor, io.EOF, proto.RefitFailureObserver},
		{refitFailureMaterialize, unifiedjournal.ErrQuota, proto.RefitFailureCapacity},
		{refitFailureBeginRegistry, unifiedjournal.ErrInvalidated, proto.RefitFailureInvalidated},
		{refitFailureQueueBootstrap, unifiedjournal.ErrStorage, proto.RefitFailureStorage},
		{refitFailureSubmitBoundary, unifiedjournal.ErrPhysicalQuota, proto.RefitFailureCapacity},
		{refitFailureAwaitBoundary, io.EOF, proto.RefitFailureObserver},
		{refitFailureValidateRegistry, unifiedjournal.ErrInvalidated, proto.RefitFailureInvalidated},
		{refitFailureSubmitSeal, unifiedjournal.ErrStorage, proto.RefitFailureStorage},
		{refitFailureAwaitSeal, io.EOF, proto.RefitFailureObserver},
		{refitFailureCommitRegistry, errors.New("commit disposition fatal"), proto.RefitFailureInternal},
	}
	for _, item := range stages {
		t.Run(string(item.stage), func(t *testing.T) {
			stage := item.stage
			fixture := newAdoptionFixture(t, 4)
			sessionID := fixture.startPaneCommand(t, "refit_failure-stage-"+string(stage), `sh -c 'stty -echo; printf "REFIT_FAILURE-STAGE-BEFORE\n"; while :; do sleep 1; done'`)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
			wantColumns := source.Columns + 11
			injected := fmt.Errorf("%w: REFIT_FAILURE_PRIVATE_STAGE_ERROR_MUST_NOT_ESCAPE", item.cause)
			fixture.effects.refitStageFault = func(gotSession string, gotStage unifiedRefitFailureStage) error {
				if gotSession == sessionID && gotStage == stage {
					return injected
				}
				return nil
			}
			operation := strings.Repeat(string(stage[0]), 43)
			err = fixture.effects.refitSession(ctx, authority, source, wantColumns, operation)
			if !errors.Is(err, ErrUnifiedRefitFatal) {
				t.Fatalf("stage %s error=%v want ErrUnifiedRefitFatal", stage, err)
			}
			if strings.Contains(err.Error(), injected.Error()) {
				t.Fatalf("stage %s exposed arbitrary cause: %v", stage, err)
			}
			gotStage, gotClass, ok := refitFailureMetadata(err)
			if !ok || gotStage != stage || gotClass != item.class {
				t.Fatalf("stage metadata=(%q,%q,%t) want=(%q,%q,true)", gotStage, gotClass, ok, stage, item.class)
			}
			if reason := waitRotationSubscriberClose(t, predecessor); reason != proto.SubscriberClosedRefitFaulted {
				t.Fatalf("stage %s predecessor close=%q want %q", stage, reason, proto.SubscriberClosedRefitFaulted)
			}
			landed, witnessErr := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
			if witnessErr != nil || landed.Columns != wantColumns {
				t.Fatalf("stage %s did not cross width PONR: landed=%+v err=%v", stage, landed, witnessErr)
			}
			fixture.effects.mu.Lock()
			_, active := fixture.effects.active[sessionID]
			fixture.effects.mu.Unlock()
			if active {
				t.Fatalf("stage %s revived predecessor authority %+v", stage, adoption.Key)
			}
			if _, _, _, reopenedCancel, openErr := openSnapshotTailForTest(t, fixture.effects, sessionID); openErr == nil {
				reopenedCancel()
				t.Fatalf("stage %s reopened a terminal provider after fatal settlement", stage)
			}
			fixture.registry.retention.mu.Lock()
			settlementErr := fixture.registry.retention.closeErr
			fixture.registry.retention.mu.Unlock()
			settledStage, settledClass, settled := refitFailureMetadata(settlementErr)
			if !settled || settledStage != stage || settledClass != item.class {
				t.Fatalf("settlement lost safe metadata: (%q,%q,%t) err=%v", settledStage, settledClass, settled, settlementErr)
			}

			// The immutable-operation replay is the broker wire seam used by the
			// front door after a lost response. It must carry only the closed
			// stage/class, never the injected cause text.
			var wire bytes.Buffer
			var logs strings.Builder
			oldLogf := brokerLogf
			brokerLogf = func(format string, args ...any) { _, _ = fmt.Fprintf(&logs, format, args...) }
			(&Server{config: fixture.cfg, unified: fixture.effects}).refit(&lockedWriter{w: &wire}, proto.Control{
				Type: "refit", Authority: &authority, Cols: wantColumns, ID: operation,
			})
			brokerLogf = oldLogf
			frame, frameErr := proto.ReadFrame(&wire)
			if frameErr != nil {
				t.Fatal(frameErr)
			}
			response, decodeErr := proto.DecodeControl(frame.Payload)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if response.Type != "refit_refused" || response.Code != "refit_faulted" || response.RefitStage != stage || response.RefitClass != item.class {
				t.Fatalf("stage %s broker response=%+v", stage, response)
			}
			if bytes.Contains(frame.Payload, []byte(injected.Error())) || strings.Contains(logs.String(), injected.Error()) {
				t.Fatalf("stage %s leaked arbitrary cause: wire=%q log=%q", stage, frame.Payload, logs.String())
			}
			if !strings.Contains(logs.String(), `stage="`+string(stage)+`"`) || !strings.Contains(logs.String(), `class="`+string(item.class)+`"`) {
				t.Fatalf("stage %s broker log omitted closed metadata: %q", stage, logs.String())
			}
		})
	}
}

func TestRefitPostCommitRefitStageFailuresCarryClosedMetadata(t *testing.T) {
	stages := []struct {
		stage unifiedRefitFailureStage
		cause error
		class proto.RefitFailureClass
	}{
		{refitFailureCommitJournal, unifiedjournal.ErrStorage, proto.RefitFailureStorage},
		{refitFailureQueuePending, unifiedjournal.ErrQuota, proto.RefitFailureCapacity},
		{refitFailureSubmitPending, unifiedjournal.ErrStorage, proto.RefitFailureStorage},
		{refitFailureAwaitPending, io.EOF, proto.RefitFailureObserver},
	}
	for _, item := range stages {
		t.Run(string(item.stage), func(t *testing.T) {
			stage := item.stage
			fixture := newAdoptionFixture(t, 4)
			sessionID := fixture.startPaneCommand(t, "refit_failure-commit-"+string(stage), `sh -c 'stty -echo; while :; do sleep 1; done'`)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
			wantColumns := source.Columns + 11
			injected := fmt.Errorf("%w: REFIT_FAILURE_PRIVATE_POST_COMMIT_ERROR", item.cause)
			fixture.effects.refitStageFault = func(gotSession string, gotStage unifiedRefitFailureStage) error {
				if gotSession == sessionID && gotStage == stage {
					return injected
				}
				return nil
			}
			operation := strings.Repeat(string(stage[0]), 43)
			err = fixture.effects.refitSession(ctx, authority, source, wantColumns, operation)
			if !errors.Is(err, ErrUnifiedRefitFatal) {
				t.Fatalf("stage %s error=%v want ErrUnifiedRefitFatal", stage, err)
			}
			gotStage, gotClass, ok := refitFailureMetadata(err)
			if !ok || gotStage != stage || gotClass != item.class {
				t.Fatalf("stage metadata=(%q,%q,%t) want=(%q,%q,true)", gotStage, gotClass, ok, stage, item.class)
			}
			if strings.Contains(err.Error(), injected.Error()) {
				t.Fatalf("stage %s exposed arbitrary cause: %v", stage, err)
			}
			if reason := waitRotationSubscriberClose(t, predecessor); reason != proto.SubscriberClosedGenerationRefit {
				t.Fatalf("stage %s predecessor close=%q want %q", stage, reason, proto.SubscriberClosedGenerationRefit)
			}
			landed, witnessErr := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
			if witnessErr != nil || landed.Columns != wantColumns {
				t.Fatalf("stage %s did not retain the mutated width: landed=%+v err=%v", stage, landed, witnessErr)
			}
			fixture.effects.mu.Lock()
			_, active := fixture.effects.active[sessionID]
			fixture.effects.mu.Unlock()
			if active {
				t.Fatalf("stage %s left successor attachment authority active", stage)
			}
			fixture.registry.retention.mu.Lock()
			settlementErr := fixture.registry.retention.closeErr
			fixture.registry.retention.mu.Unlock()
			settledStage, settledClass, settled := refitFailureMetadata(settlementErr)
			if !settled || settledStage != stage || settledClass != item.class {
				t.Fatalf("settlement lost safe metadata: (%q,%q,%t) err=%v", settledStage, settledClass, settled, settlementErr)
			}
		})
	}
}

func TestRefitBrokerNeverEmitsUntypedRefitFault(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "refit_failure-broker-metadata-guard", `sh -c 'stty -echo; while :; do sleep 1; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	operation := strings.Repeat("m", 43)
	recorded := &unifiedRefitOperation{
		request: unifiedRefitRequest{columns: 97},
		done:    make(chan struct{}),
		result:  unifiedRefitResult{err: ErrUnifiedRefitFatal},
	}
	close(recorded.done)
	fixture.effects.mu.Lock()
	fixture.effects.refitOperations[unifiedRefitOperationKey{authority: authority, operation: operation}] = recorded
	fixture.effects.mu.Unlock()

	var wire bytes.Buffer
	var logs strings.Builder
	oldLogf := brokerLogf
	defer func() { brokerLogf = oldLogf }()
	brokerLogf = func(format string, args ...any) { _, _ = fmt.Fprintf(&logs, format, args...) }
	(&Server{config: fixture.cfg, unified: fixture.effects}).refit(&lockedWriter{w: &wire}, proto.Control{
		Type: "refit", Authority: &authority, Cols: 97, ID: operation,
	})
	frame, err := proto.ReadFrame(&wire)
	if err != nil {
		t.Fatal(err)
	}
	response, err := proto.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if response.Type != "refit_refused" || response.Code != "refit_failed" || response.RefitStage != "" || response.RefitClass != "" {
		t.Fatalf("untyped fatal escaped broker guard: %+v", response)
	}
	if strings.Contains(logs.String(), `code="refit_faulted"`) || !strings.Contains(logs.String(), "event=refit_failure_metadata_invalid") {
		t.Fatalf("broker metadata guard log=%q", logs.String())
	}
}

func TestRefitFailureMetadataRejectsConflictingPairs(t *testing.T) {
	first := &unifiedRefitStageError{stage: refitFailureCapture, class: proto.RefitFailureInternal, cause: io.EOF}
	duplicate := &unifiedRefitStageError{stage: refitFailureCapture, class: proto.RefitFailureInternal, cause: unifiedjournal.ErrStorage}
	if stage, class, ok := refitFailureMetadata(errors.Join(first, duplicate)); !ok || stage != refitFailureCapture || class != proto.RefitFailureInternal {
		t.Fatalf("one duplicated closed pair rejected: (%q,%q,%t)", stage, class, ok)
	}
	conflict := &unifiedRefitStageError{stage: refitFailureCommitRegistry, class: proto.RefitFailureStorage, cause: unifiedjournal.ErrStorage}
	if stage, class, ok := refitFailureMetadata(errors.Join(first, conflict)); ok || stage != "" || class != "" {
		t.Fatalf("conflicting closed pairs accepted: (%q,%q,%t)", stage, class, ok)
	}
}
