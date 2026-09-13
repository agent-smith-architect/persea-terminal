package frontdoor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"persea-terminal/internal/config"
)

func diagnosticTestLimits() diagnosticTraceLimits {
	limits := productionDiagnosticTraceLimits()
	limits.files = 32
	limits.diskBytes = 32 << 20
	return limits
}

func TestDiagnosticTraceProductionCapsMatchCaptureBudget(t *testing.T) {
	limits := productionDiagnosticTraceLimits()
	if limits.writesPerCapture != 512 || limits.bytesPerCapture != 1<<30 ||
		limits.capturesPerOperator != 4 || limits.capturesGlobal != 8 ||
		limits.files != 4 || limits.diskBytes != 64<<20 ||
		limits.inactivity != 30*time.Minute || diagnosticTraceFileMode != 0640 {
		t.Fatalf("production diagnostic limits = %+v", limits)
	}
}

func newDiagnosticTestStore(t *testing.T, limits diagnosticTraceLimits) (*diagnosticTraceStore, string) {
	t.Helper()
	parent := t.TempDir()
	directory := filepath.Join(parent, "evidence")
	if err := os.Mkdir(directory, 0770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, os.ModeSetgid|0770); err != nil {
		t.Fatal(err)
	}
	root, info, err := openDiagnosticTraceRoot(directory, parent)
	if err != nil {
		t.Fatal(err)
	}
	store, err := newDiagnosticTraceStoreFromRoot(root, info, limits)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	return store, directory
}

func diagnosticTestTrace(ordinal uint64) map[string]any {
	return map[string]any{
		"schema":         diagnosticTraceSchema,
		"capacity":       diagnosticTraceEventCapacity,
		"retainedEvents": 1,
		"droppedEvents":  0,
		"experiment": map[string]any{
			"enabled": true,
			"comparisonInputTypes": map[string]any{
				"terminal":              []string{},
				"web-control-allowed":   []string{},
				"web-control-prevented": []string{},
			},
		},
		"events": []any{map[string]any{
			"ordinal": ordinal, "kind": "experiment-toggle", "source": nil,
			"monotonicMs": 12.5, "experimentEnabled": true,
		}},
	}
}

func diagnosticTestBody(t *testing.T, captureID *string, trace map[string]any) []byte {
	t.Helper()
	request := map[string]any{"trace": trace}
	if captureID != nil {
		request["capture_id"] = *captureID
	}
	body, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func diagnosticDirectRequest(store *diagnosticTraceStore, body []byte, operator string) *httptest.ResponseRecorder {
	cfg := config.Front{Ingress: config.Ingress{
		PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true,
		OperatorLogin: operator, CanonicalHost: "localhost:43210",
	}}
	server := &Server{cfg: cfg, diagnostic: store}
	request := httptest.NewRequest(http.MethodPost, "http://localhost/api/diagnostic-traces", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), ingressKey{}, ingressIdentity{
		uid: cfg.Ingress.PeerUID, operator: operator, origin: "http://localhost:43210",
	}))
	response := httptest.NewRecorder()
	server.diagnosticTrace(response, request)
	return response
}

func decodedDiagnosticResponse(t *testing.T, response *httptest.ResponseRecorder) diagnosticTraceResponse {
	t.Helper()
	var decoded diagnosticTraceResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func assertRawDiagnosticResponseContract(
	t *testing.T,
	response *httptest.ResponseRecorder,
	status int,
	expectedCaptureID string,
	eventCount int,
	writeOrdinal uint64,
	expectedAcceptedAt string,
) string {
	t.Helper()
	if response.Code != status {
		t.Fatalf("raw response status = %d, want %d; body=%q", response.Code, status, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("raw response Content-Type = %q, want application/json", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("raw response Cache-Control = %q, want no-store", got)
	}
	expectedBody := fmt.Sprintf(
		"{\"capture_id\":%q,\"event_count\":%d,\"write_ordinal\":%d,\"accepted_at\":%q}\n",
		expectedCaptureID, eventCount, writeOrdinal, expectedAcceptedAt,
	)
	if got := response.Body.String(); got != expectedBody {
		t.Fatalf("raw response body = %q, want exact %q", got, expectedBody)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatalf("raw response is not JSON: %v; body=%q", err, response.Body.String())
	}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	const expectedKeys = "accepted_at,capture_id,event_count,write_ordinal"
	if got := strings.Join(keys, ","); got != expectedKeys {
		t.Fatalf("raw response keys = %q, want %q", got, expectedKeys)
	}
	var captureID, actualAcceptedAt string
	if err := json.Unmarshal(raw["capture_id"], &captureID); err != nil {
		t.Fatalf("raw capture_id is not a string: %v", err)
	}
	if captureID != expectedCaptureID {
		t.Fatalf("raw capture_id = %q, want %q", captureID, expectedCaptureID)
	}
	if got := string(raw["event_count"]); got != fmt.Sprint(eventCount) {
		t.Fatalf("raw event_count = %s, want %d", got, eventCount)
	}
	if got := string(raw["write_ordinal"]); got != fmt.Sprint(writeOrdinal) {
		t.Fatalf("raw write_ordinal = %s, want %d", got, writeOrdinal)
	}
	if err := json.Unmarshal(raw["accepted_at"], &actualAcceptedAt); err != nil {
		t.Fatalf("raw accepted_at is not a string: %v", err)
	}
	if actualAcceptedAt != expectedAcceptedAt {
		t.Fatalf("raw accepted_at = %q, want %q", actualAcceptedAt, expectedAcceptedAt)
	}
	return captureID
}

func TestDiagnosticTraceRawHTTPResponseContract(t *testing.T) {
	store, _ := newDiagnosticTestStore(t, diagnosticTestLimits())
	fixedTime := time.Date(2026, 7, 28, 0, 30, 0, 123456789, time.UTC)
	store.now = func() time.Time { return fixedTime }
	store.randomRead = func(value []byte) (int, error) {
		for index := range value {
			value[index] = byte(index + 1)
		}
		return len(value), nil
	}
	captureID := "0102030405060708090a0b0c0d0e0f10"
	const acceptedAt = "2026-07-28T00:30:00.123456789Z"
	create := diagnosticDirectRequest(
		store,
		diagnosticTestBody(t, nil, diagnosticTestTrace(1)),
		"operator@example.com",
	)
	assertRawDiagnosticResponseContract(t, create, http.StatusCreated, captureID, 1, 1, acceptedAt)
	replace := diagnosticDirectRequest(
		store,
		diagnosticTestBody(t, &captureID, diagnosticTestTrace(1)),
		"operator@example.com",
	)
	assertRawDiagnosticResponseContract(t, replace, http.StatusOK, captureID, 1, 2, acceptedAt)
}

func TestDiagnosticTraceSchemaIsStrictAndCanonical(t *testing.T) {
	valid := diagnosticTestBody(t, nil, diagnosticTestTrace(1))
	if _, _, err := decodeDiagnosticTraceRequest(valid); err != nil {
		t.Fatalf("valid trace rejected: %v", err)
	}
	mutations := map[string][]byte{}
	var wrapper map[string]any
	if err := json.Unmarshal(valid, &wrapper); err != nil {
		t.Fatal(err)
	}
	withUnknown := cloneDiagnosticMap(t, wrapper)
	withUnknown["raw_body_marker"] = "PRINTABLE_CONTENT_MUST_NOT_REACH_DISK"
	mutations["unknown_top_level"] = marshalDiagnosticMutation(t, withUnknown)
	unknownEvent := cloneDiagnosticMap(t, wrapper)
	unknownEvent["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["message"] = "PRINTABLE_CONTENT_MUST_NOT_REACH_DISK"
	mutations["unknown_event_field"] = marshalDiagnosticMutation(t, unknownEvent)
	mutations["trailing"] = append(append([]byte{}, valid...), []byte(`{}`)...)
	mutations["invalid_utf8"] = append(append([]byte{}, valid...), 0xff)
	duplicate := strings.Replace(string(valid), `"schema": "persea-sustained-backspace-v1"`, `"schema": "persea-sustained-backspace-v1", "schema": "persea-sustained-backspace-v1"`, 1)
	mutations["duplicate"] = []byte(duplicate)
	noncontiguous := cloneDiagnosticMap(t, wrapper)
	trace := noncontiguous["trace"].(map[string]any)
	first := trace["events"].([]any)[0].(map[string]any)
	second := cloneDiagnosticMap(t, first)
	second["ordinal"] = float64(3)
	trace["events"] = append(trace["events"].([]any), second)
	trace["retainedEvents"] = float64(2)
	mutations["noncontiguous_ordinals"] = marshalDiagnosticMutation(t, noncontiguous)
	badCapture := "../../analyst/file"
	mutations["client_path_component"] = diagnosticTestBody(t, &badCapture, diagnosticTestTrace(1))
	nullCapture := cloneDiagnosticMap(t, wrapper)
	nullCapture["capture_id"] = nil
	mutations["null_capture_id"] = marshalDiagnosticMutation(t, nullCapture)
	for name, body := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, _, err := decodeDiagnosticTraceRequest(body); err == nil {
				t.Fatal("invalid diagnostic trace accepted")
			}
		})
	}

	store, directory := newDiagnosticTestStore(t, diagnosticTestLimits())
	response := diagnosticDirectRequest(store, valid, "operator@example.com")
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%q", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response headers = %v", response.Header())
	}
	decoded := decodedDiagnosticResponse(t, response)
	if decoded.EventCount != 1 || decoded.WriteOrdinal != 1 || decoded.CaptureID == "" || decoded.AcceptedAt == "" {
		t.Fatalf("response = %+v", decoded)
	}
	if _, err := time.Parse(time.RFC3339Nano, decoded.AcceptedAt); err != nil {
		t.Fatalf("accepted_at = %q: %v", decoded.AcceptedAt, err)
	}
	var responseShape map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &responseShape); err != nil || len(responseShape) != 4 {
		t.Fatalf("response shape = %v, %v", responseShape, err)
	}
	if strings.Contains(response.Body.String(), directory) || strings.Contains(response.Body.String(), "trace-") {
		t.Fatalf("response leaked filesystem material: %q", response.Body.String())
	}
	capture := store.captures[decoded.CaptureID]
	saved, err := os.ReadFile(filepath.Join(directory, capture.filename))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(saved, valid) || !bytes.HasSuffix(saved, []byte("\n")) ||
		bytes.Contains(saved, []byte("capture_id")) || bytes.Contains(saved, []byte("raw_body_marker")) {
		t.Fatalf("saved bytes were not canonical typed JSON: %q", saved)
	}
	var persisted validatedDiagnosticTrace
	if err := json.Unmarshal(saved, &persisted); err != nil || persisted.Schema != diagnosticTraceSchema {
		t.Fatalf("persisted trace = %+v, %v", persisted, err)
	}
	info, err := os.Stat(filepath.Join(directory, capture.filename))
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("file mode = %v, %v", info.Mode(), err)
	}
	replacementBody := diagnosticTestBody(t, &decoded.CaptureID, diagnosticTestTrace(1))
	replacement := diagnosticDirectRequest(store, replacementBody, "operator@example.com")
	if replacement.Code != http.StatusOK {
		t.Fatalf("replacement status=%d body=%q", replacement.Code, replacement.Body.String())
	}
	replaced := decodedDiagnosticResponse(t, replacement)
	if replaced.CaptureID != decoded.CaptureID || replaced.WriteOrdinal != 2 || len(store.captures) != 1 {
		t.Fatalf("replacement response/state = %+v captures=%d", replaced, len(store.captures))
	}
	files, _, err := store.scanDiskLocked()
	if err != nil || files != 1 {
		t.Fatalf("replacement evidence files=%d err=%v", files, err)
	}
}

func cloneDiagnosticMap(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var cloned map[string]any
	if err := json.Unmarshal(body, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func marshalDiagnosticMutation(t *testing.T, value map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestDiagnosticTraceRejectsPrintableTerminalContent(t *testing.T) {
	common := map[string]any{
		"monotonicMs": 1, "cadenceMs": nil, "holdElapsedMs": nil, "experimentEnabled": true,
		"target": map[string]any{"targetIsTextarea": false, "valueLength": nil, "selectionStart": nil, "selectionEnd": nil},
		"focus":  map[string]any{"documentHasFocus": true, "focusWithinRoot": true, "activeIsTextarea": true, "visualViewport": nil},
	}
	event := map[string]any{
		"ordinal": 1, "kind": "terminal-on-data", "source": "terminal",
		"byteLength": 1, "controlHex": "41",
	}
	for key, value := range common {
		event[key] = value
	}
	trace := diagnosticTestTrace(1)
	trace["events"] = []any{event}
	body := diagnosticTestBody(t, nil, trace)
	if _, _, err := decodeDiagnosticTraceRequest(body); err == nil || !strings.Contains(err.Error(), "printable") {
		t.Fatalf("printable terminal byte error = %v", err)
	}
	event["controlHex"] = "7f"
	body = diagnosticTestBody(t, nil, trace)
	if _, _, err := decodeDiagnosticTraceRequest(body); err != nil {
		t.Fatalf("control-only terminal byte rejected: %v", err)
	}
}

func diagnosticCommonMap(textarea bool) map[string]any {
	var valueLength, selectionStart, selectionEnd any
	if textarea {
		valueLength, selectionStart, selectionEnd = 12, 3, 9
	}
	return map[string]any{
		"monotonicMs": 10.5, "cadenceMs": 1.25, "holdElapsedMs": 2.5, "experimentEnabled": true,
		"target": map[string]any{
			"targetIsTextarea": textarea, "valueLength": valueLength,
			"selectionStart": selectionStart, "selectionEnd": selectionEnd,
		},
		"focus": map[string]any{
			"documentHasFocus": true, "focusWithinRoot": true, "activeIsTextarea": true,
			"visualViewport": map[string]any{"width": 390, "height": 440, "offsetLeft": 0, "offsetTop": 2, "scale": 1},
		},
	}
}

func diagnosticSampleMap() map[string]any {
	return map[string]any{
		"ongoingCommittedReaderPaths": map[string]any{"reuse": 3, "rebuild": 1, "reusePending": false},
		"inputFrames":                 4, "inputBytes": 5, "inboxFrames": 6, "inboxWeight": 7,
	}
}

func focusScrollGeometryMap() map[string]any {
	return map[string]any{
		"scrollTop": 120, "scrollHeight": 400, "clientHeight": 280,
		"maxScrollTop": 120, "distanceFromLiveEdge": 0, "isFollowingLive": true,
	}
}

func focusScrollRectMap() map[string]any {
	return map[string]any{"top": 10, "right": 390, "bottom": 290, "left": 0, "width": 390, "height": 280}
}

func focusScrollSnapshotMap() map[string]any {
	return map[string]any{
		"viewport": map[string]any{
			"innerHeight": 844,
			"visualViewport": map[string]any{
				"width": 390, "height": 440, "offsetLeft": 0, "offsetTop": 12,
				"pageLeft": 0, "pageTop": 12, "scale": 1,
			},
			"visibleTop": 12, "visibleBottom": 452,
		},
		"scroller": mergeDiagnosticEvent(focusScrollGeometryMap(), map[string]any{"rect": focusScrollRectMap()}),
		"overlay": map[string]any{
			"composer": map[string]any{
				"open": true, "size": "compact", "panelRect": focusScrollRectMap(),
				"textarea": map[string]any{
					"rect": focusScrollRectMap(), "scrollTop": 0, "scrollHeight": 120, "clientHeight": 80,
					"valueLength": 0, "selectionStart": 0, "selectionEnd": 0,
				},
			},
			"keybar":   map[string]any{"hidden": false, "collapsed": false, "overflowVisible": false, "rect": focusScrollRectMap()},
			"dockRect": focusScrollRectMap(),
		},
		"inset": map[string]any{
			"measuredComposerHeight": 120, "publishedInsetPx": 120, "insetBudgetPx": 280,
			"cssPublishedInsetPx": 120, "publicationCount": 1, "publicationFramePending": false,
		},
		"ownership": map[string]any{
			"followLiveIntent": true, "pendingKeyboardReveal": false, "pendingOverlayReveal": false,
			"keyboardOpen": true, "attachmentGeneration": 1, "protocolEpoch": "2",
			"presentedAggregateId": 3, "presentedAggregateEpoch": "2", "surfaceInputEpoch": 4,
			"quiescenceEpoch": 5, "overlayInsetEpoch": 6, "viewportEventEpoch": 7,
			"journalSequence": 8, "displayPreparationGeneration": 9,
			"displayPreparationPublicationCount": 10,
		},
		"focus": map[string]any{
			"role": "composer", "documentHasFocus": true, "focusWithinRoot": true,
			"textarea": map[string]any{"valueLength": 0, "selectionStart": 0, "selectionEnd": 0},
		},
		"caret": map[string]any{
			"terminalCursorRect": nil, "terminalCaretDistanceToScrollerBottom": nil,
			"terminalCaretDistanceToVisualViewportBottom": nil, "terminalCaretWithinScroller": nil,
			"terminalCaretWithinVisualViewport": nil,
		},
	}
}

func focusScrollEventMap(ordinal int, kind string) map[string]any {
	return map[string]any{
		"ordinal": ordinal, "monotonicMs": float64(ordinal), "kind": kind,
		"focusTarget": nil, "activationSource": nil, "activationTarget": nil,
		"keyboardState": nil, "reveal": nil, "scrollWrite": nil,
		"snapshot": focusScrollSnapshotMap(),
	}
}

func focusScrollTraceMap(events []any) map[string]any {
	return map[string]any{
		"schema": focusScrollDiagnosticTraceSchema, "capacity": diagnosticTraceEventCapacity,
		"retainedEvents": len(events), "droppedEvents": 0, "enabled": true, "events": events,
	}
}

func focusScrollTestBody(t *testing.T, captureID *string, events []any) []byte {
	t.Helper()
	return diagnosticTestBody(t, captureID, focusScrollTraceMap(events))
}

func TestFocusScrollDiagnosticTraceExactSchemaAndVariants(t *testing.T) {
	events := []any{
		focusScrollEventMap(1, "enable-baseline"),
		mergeDiagnosticEvent(focusScrollEventMap(2, "activation"), map[string]any{
			"activationSource": "pointerdown", "activationTarget": "composer-open",
		}),
		mergeDiagnosticEvent(focusScrollEventMap(3, "focus-in"), map[string]any{"focusTarget": "composer"}),
		mergeDiagnosticEvent(focusScrollEventMap(4, "keyboard-state-edge"), map[string]any{
			"keyboardState": map[string]any{"previousOpen": false, "currentOpen": true},
		}),
		focusScrollEventMap(5, "composer-inset-publication"),
		mergeDiagnosticEvent(focusScrollEventMap(6, "reveal-request"), map[string]any{
			"reveal": map[string]any{
				"requestId": 1, "channel": "overlay", "trigger": "composer-inset-publish",
				"outcome": nil, "reasons": []any{}, "before": focusScrollGeometryMap(), "after": focusScrollGeometryMap(),
			},
		}),
		mergeDiagnosticEvent(focusScrollEventMap(7, "reveal-decision"), map[string]any{
			"reveal": map[string]any{
				"requestId": 1, "channel": "overlay", "trigger": "composer-inset-publish",
				"outcome": "already-visible", "reasons": []any{"already-at-live-edge"},
				"before": focusScrollGeometryMap(), "after": focusScrollGeometryMap(),
			},
		}),
		mergeDiagnosticEvent(focusScrollEventMap(8, "internal-scroll-write"), map[string]any{
			"scrollWrite": map[string]any{
				"reason": "live-edge-overlay", "requestedScrollTop": 400,
				"before": focusScrollGeometryMap(), "after": focusScrollGeometryMap(),
			},
		}),
		focusScrollEventMap(9, "scroller-scroll"),
		focusScrollEventMap(10, "visual-viewport-resize"),
		focusScrollEventMap(11, "settle-microtask"),
		focusScrollEventMap(12, "settle-raf-1"),
		focusScrollEventMap(13, "settle-raf-2"),
		focusScrollEventMap(14, "settle-250ms"),
	}
	body := focusScrollTestBody(t, nil, events)
	_, decoded, err := decodeDiagnosticTraceRequest(body)
	if err != nil {
		t.Fatalf("valid focus-scroll trace rejected: %v", err)
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	var persisted validatedFocusScrollDiagnosticTrace
	if err := json.Unmarshal(canonical, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Schema != focusScrollDiagnosticTraceSchema || persisted.RetainedEvents != len(events) ||
		len(persisted.Events) != len(events) || persisted.Events[2].FocusTarget == nil ||
		*persisted.Events[2].FocusTarget != "composer" {
		t.Fatalf("decoded focus-scroll trace = %+v", persisted)
	}
}

func TestFocusScrollDiagnosticTraceRejectsStrictnessMutations(t *testing.T) {
	event := focusScrollEventMap(1, "enable-baseline")
	valid := focusScrollTestBody(t, nil, []any{event})
	var baseline map[string]any
	if err := json.Unmarshal(valid, &baseline); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(map[string]any){
		"wrapper_extra": func(root map[string]any) { root["path"] = "/private/evidence" },
		"trace_missing": func(root map[string]any) { delete(root["trace"].(map[string]any), "enabled") },
		"event_extra_content": func(root map[string]any) {
			root["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["text"] = "PRINTABLE_SENTINEL"
		},
		"snapshot_missing": func(root map[string]any) {
			delete(root["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["snapshot"].(map[string]any), "caret")
		},
		"viewport_extra": func(root map[string]any) {
			root["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["snapshot"].(map[string]any)["viewport"].(map[string]any)["url"] = "https://sentinel.invalid/"
		},
		"rect_missing": func(root map[string]any) {
			delete(root["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["snapshot"].(map[string]any)["scroller"].(map[string]any)["rect"].(map[string]any), "width")
		},
		"geometry_extra": func(root map[string]any) {
			root["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["snapshot"].(map[string]any)["scroller"].(map[string]any)["session"] = "secret"
		},
		"ownership_missing": func(root map[string]any) {
			delete(root["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["snapshot"].(map[string]any)["ownership"].(map[string]any), "overlayInsetEpoch")
		},
		"inset_missing": func(root map[string]any) {
			delete(root["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["snapshot"].(map[string]any)["inset"].(map[string]any), "cssPublishedInsetPx")
		},
		"unknown_kind": func(root map[string]any) {
			root["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["kind"] = "future-kind"
		},
		"unknown_role": func(root map[string]any) {
			root["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["snapshot"].(map[string]any)["focus"].(map[string]any)["role"] = "future-role"
		},
		"unsafe_integer": func(root map[string]any) {
			root["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["snapshot"].(map[string]any)["ownership"].(map[string]any)["journalSequence"] = float64(diagnosticMaxSafeInteger) + 1
		},
		"epoch_overflow": func(root map[string]any) {
			root["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["snapshot"].(map[string]any)["ownership"].(map[string]any)["protocolEpoch"] = "18446744073709551616"
		},
		"negative_height": func(root map[string]any) {
			root["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["snapshot"].(map[string]any)["viewport"].(map[string]any)["innerHeight"] = -1
		},
		"wrong_scalar": func(root map[string]any) {
			root["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["monotonicMs"] = "one"
		},
		"ordinal_zero": func(root map[string]any) {
			root["trace"].(map[string]any)["events"].([]any)[0].(map[string]any)["ordinal"] = 0
		},
		"retained_mismatch": func(root map[string]any) { root["trace"].(map[string]any)["retainedEvents"] = 2 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			root := cloneDiagnosticMap(t, baseline)
			mutate(root)
			if _, _, err := decodeDiagnosticTraceRequest(marshalDiagnosticMutation(t, root)); err == nil {
				t.Fatal("strictness mutation accepted")
			}
		})
	}

	duplicate := bytes.Replace(valid, []byte(`"enabled": true`), []byte(`"enabled": true, "enabled": true`), 1)
	if _, _, err := decodeDiagnosticTraceRequest(duplicate); err == nil {
		t.Fatal("duplicate key accepted")
	}

	gap := []any{focusScrollEventMap(1, "enable-baseline"), focusScrollEventMap(3, "settle-250ms")}
	if _, _, err := decodeDiagnosticTraceRequest(focusScrollTestBody(t, nil, gap)); err == nil {
		t.Fatal("ordinal gap accepted")
	}
}

func TestFocusScrollDiagnosticTraceAutosavePersistence(t *testing.T) {
	store, directory := newDiagnosticTestStore(t, diagnosticTestLimits())
	first := diagnosticDirectRequest(store, focusScrollTestBody(t, nil, []any{focusScrollEventMap(1, "enable-baseline")}), "operator@example.com")
	if first.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%q", first.Code, first.Body.String())
	}
	decoded := decodedDiagnosticResponse(t, first)
	events := []any{focusScrollEventMap(1, "enable-baseline"), focusScrollEventMap(2, "settle-250ms")}
	update := diagnosticDirectRequest(store, focusScrollTestBody(t, &decoded.CaptureID, events), "operator@example.com")
	if update.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%q", update.Code, update.Body.String())
	}
	capture := store.captures[decoded.CaptureID]
	saved, err := os.ReadFile(filepath.Join(directory, capture.filename))
	if err != nil {
		t.Fatal(err)
	}
	var persisted validatedFocusScrollDiagnosticTrace
	if err := json.Unmarshal(saved, &persisted); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(directory, capture.filename))
	if err != nil || info.Mode().Perm() != diagnosticTraceFileMode ||
		persisted.RetainedEvents != 2 || len(persisted.Events) != 2 ||
		persisted.Events[1].Ordinal != 2 {
		t.Fatalf("persisted focus-scroll evidence = %+v mode=%v err=%v", persisted, info.Mode(), err)
	}
	if !bytes.Contains(saved, []byte(`"overlayInsetEpoch"`)) || !bytes.Contains(saved, []byte(`"settle-250ms"`)) {
		t.Fatalf("focus-scroll evidence is not analyst-readable: %q", saved)
	}
	before := sha256.Sum256(saved)
	originalSync := store.fileSync
	store.fileSync = func(*os.File) error { return errors.New("injected focus-scroll sync failure") }
	failedEvents := append(events, focusScrollEventMap(3, "settle-250ms"))
	failed := diagnosticDirectRequest(store, focusScrollTestBody(t, &decoded.CaptureID, failedEvents), "operator@example.com")
	store.fileSync = originalSync
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("faulted update status=%d body=%q", failed.Code, failed.Body.String())
	}
	afterBody, err := os.ReadFile(filepath.Join(directory, capture.filename))
	if err != nil {
		t.Fatal(err)
	}
	if after := sha256.Sum256(afterBody); after != before {
		t.Fatal("faulted focus-scroll autosave changed the prior complete snapshot")
	}
}

func mergeDiagnosticEvent(base map[string]any, values map[string]any) map[string]any {
	for key, value := range values {
		base[key] = value
	}
	return base
}

func TestDiagnosticTraceAcceptsEveryCurrentEventVariant(t *testing.T) {
	events := []any{
		map[string]any{"ordinal": 1, "kind": "experiment-toggle", "source": nil, "monotonicMs": 1, "experimentEnabled": true},
		mergeDiagnosticEvent(map[string]any{"ordinal": 2, "kind": "focusin", "source": "terminal"}, diagnosticCommonMap(true)),
		mergeDiagnosticEvent(map[string]any{
			"ordinal": 3, "kind": "keydown", "source": "terminal", "key": "Backspace", "code": "Backspace",
			"repeat": true, "isComposing": false, "cancelable": true, "defaultPreventedAtCapture": false,
			"diagnosticAction": "observe-only", "backspaceKeydownCount": 1,
		}, diagnosticCommonMap(true)),
		mergeDiagnosticEvent(map[string]any{
			"ordinal": 4, "kind": "terminal-on-data", "source": "terminal", "byteLength": 1, "controlHex": "7f",
		}, diagnosticCommonMap(false)),
		mergeDiagnosticEvent(map[string]any{
			"ordinal": 5, "kind": "beforeinput", "source": "web-control-allowed",
			"inputType": "deleteWordBackward", "isComposing": false, "dataLength": nil,
			"targetRanges": []any{map[string]any{"startOffset": 3, "endOffset": 9}},
			"cancelable":   true, "defaultPreventedAtCapture": false, "diagnosticAction": "observe-only",
			"sample": diagnosticSampleMap(),
		}, diagnosticCommonMap(true)),
		map[string]any{
			"ordinal": 6, "kind": "post-input-snapshot", "monotonicMs": 16,
			"eventOrdinal": 5, "sample": diagnosticSampleMap(),
		},
		mergeDiagnosticEvent(map[string]any{
			"ordinal": 7, "kind": "input", "source": "web-control-allowed",
			"inputType": "deleteWordBackward", "isComposing": false, "dataLength": nil,
			"targetRanges": []any{}, "cancelable": false, "defaultPreventedAtCapture": false,
			"diagnosticAction": "observe-only", "sample": nil,
		}, diagnosticCommonMap(true)),
		mergeDiagnosticEvent(map[string]any{"ordinal": 8, "kind": "compositionstart", "source": "terminal", "dataLength": 0}, diagnosticCommonMap(true)),
		mergeDiagnosticEvent(map[string]any{"ordinal": 9, "kind": "compositionupdate", "source": "terminal", "dataLength": 2}, diagnosticCommonMap(true)),
		mergeDiagnosticEvent(map[string]any{"ordinal": 10, "kind": "compositionend", "source": "terminal", "dataLength": 2}, diagnosticCommonMap(true)),
		mergeDiagnosticEvent(map[string]any{"ordinal": 11, "kind": "visual-viewport-resize"}, diagnosticCommonMap(false)),
		mergeDiagnosticEvent(map[string]any{"ordinal": 12, "kind": "visual-viewport-scroll"}, diagnosticCommonMap(false)),
		mergeDiagnosticEvent(map[string]any{"ordinal": 13, "kind": "focusout", "source": "terminal"}, diagnosticCommonMap(true)),
		mergeDiagnosticEvent(map[string]any{
			"ordinal": 14, "kind": "keyup", "source": "terminal", "key": "Backspace", "code": "",
			"repeat": false, "isComposing": false, "cancelable": true, "defaultPreventedAtCapture": false,
			"diagnosticAction": "observe-only", "backspaceKeydownCount": 1,
		}, diagnosticCommonMap(true)),
	}
	trace := diagnosticTestTrace(1)
	trace["retainedEvents"] = len(events)
	trace["events"] = events
	trace["experiment"].(map[string]any)["comparisonInputTypes"].(map[string]any)["web-control-allowed"] = []string{"deleteWordBackward"}
	body := diagnosticTestBody(t, nil, trace)
	if _, _, err := decodeDiagnosticTraceRequest(body); err != nil {
		t.Fatalf("current event variants rejected: %v", err)
	}

	for name, mutate := range map[string]func(map[string]any){
		"missing_nested_focus_field": func(root map[string]any) {
			root["trace"].(map[string]any)["events"].([]any)[1].(map[string]any)["focus"].(map[string]any)["activeIsTextarea"] = nil
			delete(root["trace"].(map[string]any)["events"].([]any)[1].(map[string]any)["focus"].(map[string]any), "activeIsTextarea")
		},
		"missing_sample_counter": func(root map[string]any) {
			delete(root["trace"].(map[string]any)["events"].([]any)[4].(map[string]any)["sample"].(map[string]any), "inboxWeight")
		},
	} {
		t.Run(name, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal(body, &root); err != nil {
				t.Fatal(err)
			}
			mutate(root)
			if _, _, err := decodeDiagnosticTraceRequest(marshalDiagnosticMutation(t, root)); err == nil {
				t.Fatal("inexact nested event shape accepted")
			}
		})
	}
}

func TestDiagnosticTraceBodyLimitIsNotTruncation(t *testing.T) {
	store, _ := newDiagnosticTestStore(t, diagnosticTestLimits())
	atLimit := bytes.Repeat([]byte(" "), diagnosticTraceBodyLimit)
	response := diagnosticDirectRequest(store, atLimit, "operator@example.com")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("at-limit invalid JSON status=%d", response.Code)
	}
	overLimit := append(atLimit, ' ')
	response = diagnosticDirectRequest(store, overLimit, "operator@example.com")
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit status=%d", response.Code)
	}
}

func TestDiagnosticTraceRouteAndSecurityAuthority(t *testing.T) {
	store, _ := newDiagnosticTestStore(t, diagnosticTestLimits())
	cfg := config.Front{
		Ingress: config.Ingress{
			SocketPath: "/tmp/front.sock", PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true,
			CanonicalHost: "localhost:43210", OperatorLogin: "operator@example.com", MaxConnections: 64,
		},
		DiagnosticTraceDir: "configured",
	}
	server := &Server{cfg: cfg, diagnostic: store}
	request := func(mutate func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://localhost/api/diagnostic-traces", bytes.NewReader(diagnosticTestBody(t, nil, diagnosticTestTrace(1))))
		r.Host = "localhost"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Forwarded-Host", cfg.Ingress.CanonicalHost)
		r.Header.Set("X-Forwarded-Proto", "http")
		r.Header.Set("Tailscale-User-Login", cfg.Ingress.OperatorLogin)
		r.Header.Set("Origin", externalOrigin(cfg.Ingress))
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Cookie", csrfCookie+"="+testCSRF)
		r.Header.Set("X-Persea-CSRF", testCSRF)
		r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: cfg.Ingress.PeerUID}))
		if mutate != nil {
			mutate(r)
		}
		w := httptest.NewRecorder()
		server.handler().ServeHTTP(w, r)
		return w
	}
	if response := request(nil); response.Code != http.StatusCreated {
		t.Fatalf("authenticated create status=%d body=%q", response.Code, response.Body.String())
	}
	for name, mutate := range map[string]func(*http.Request){
		"untrusted_peer":      func(r *http.Request) { *r = *r.WithContext(context.Background()) },
		"forged_operator":     func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "other@example.com") },
		"missing_origin":      func(r *http.Request) { r.Header.Del("Origin") },
		"wrong_origin":        func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"missing_fetch_site":  func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") },
		"wrong_fetch_site":    func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"missing_csrf_cookie": func(r *http.Request) { r.Header.Del("Cookie") },
		"missing_csrf_header": func(r *http.Request) { r.Header.Del("X-Persea-CSRF") },
	} {
		t.Run(name, func(t *testing.T) {
			if response := request(mutate); response.Code != http.StatusForbidden {
				t.Fatalf("denial status=%d", response.Code)
			}
		})
	}
	for name, mutated := range map[string]config.Front{
		"no_config": func() config.Front { value := cfg; value.DiagnosticTraceDir = ""; return value }(),
		"no_peer_uid": func() config.Front {
			value := cfg
			value.Ingress.PeerUIDConfigured = false
			return value
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			withoutRoute := &Server{cfg: mutated, diagnostic: store, listen: "localhost"}
			r := httptest.NewRequest(http.MethodPost, "http://localhost/api/diagnostic-traces", bytes.NewReader(diagnosticTestBody(t, nil, diagnosticTestTrace(1))))
			r.Host = "localhost"
			if mutated.Ingress.PeerUIDConfigured {
				r.Header.Set("X-Forwarded-Host", mutated.Ingress.CanonicalHost)
				r.Header.Set("X-Forwarded-Proto", "http")
				r.Header.Set("Tailscale-User-Login", mutated.Ingress.OperatorLogin)
				r.Header.Set("Origin", externalOrigin(mutated.Ingress))
				r.Header.Set("Sec-Fetch-Site", "same-origin")
				r.Header.Set("Cookie", csrfCookie+"="+testCSRF)
				r.Header.Set("X-Persea-CSRF", testCSRF)
				r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: mutated.Ingress.PeerUID}))
			}
			w := httptest.NewRecorder()
			withoutRoute.handler().ServeHTTP(w, r)
			if w.Code != http.StatusMethodNotAllowed || strings.Contains(w.Header().Get("Allow"), http.MethodPost) {
				t.Fatalf("diagnostic route exists without gate: status=%d allow=%q", w.Code, w.Header().Get("Allow"))
			}
		})
	}
}

func TestDiagnosticTraceCaptureBindingExpiryAndCaps(t *testing.T) {
	canonical := []byte(`{"schema":"test"}` + "\n")
	t.Run("binding_and_expiry", func(t *testing.T) {
		store, _ := newDiagnosticTestStore(t, diagnosticTestLimits())
		now := time.Unix(1000, 0)
		store.now = func() time.Time { return now }
		result, err := store.save("operator-a", nil, 10, 1, canonical)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.save("operator-b", &result.captureID, 10, 1, canonical); !errors.Is(err, errDiagnosticCaptureNotFound) {
			t.Fatalf("other operator error=%v", err)
		}
		guess := strings.Repeat("a", 32)
		if _, err := store.save("operator-a", &guess, 10, 1, canonical); !errors.Is(err, errDiagnosticCaptureNotFound) {
			t.Fatalf("guessed ID error=%v", err)
		}
		now = now.Add(diagnosticTraceInactivity)
		if _, err := store.save("operator-a", &result.captureID, 10, 1, canonical); !errors.Is(err, errDiagnosticCaptureNotFound) {
			t.Fatalf("expired ID error=%v", err)
		}
	})
	t.Run("write_off_by_one", func(t *testing.T) {
		limits := diagnosticTestLimits()
		limits.writesPerCapture = 2
		limits.bytesPerCapture = 100
		store, directory := newDiagnosticTestStore(t, limits)
		result, err := store.save("operator", nil, 5, 1, canonical)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.save("operator", &result.captureID, 5, 1, canonical); err != nil {
			t.Fatalf("exact write cap rejected: %v", err)
		}
		before := diagnosticCaptureHash(t, store, directory, result.captureID)
		if _, err := store.save("operator", &result.captureID, 1, 1, canonical); !errors.Is(err, errDiagnosticCaptureLimit) {
			t.Fatalf("over-cap write error=%v", err)
		}
		after := diagnosticCaptureHash(t, store, directory, result.captureID)
		if before != after {
			t.Fatal("over-cap write changed prior file")
		}
	})
	t.Run("cumulative_bytes_off_by_one", func(t *testing.T) {
		limits := diagnosticTestLimits()
		limits.writesPerCapture = 10
		limits.bytesPerCapture = 10
		store, directory := newDiagnosticTestStore(t, limits)
		result, err := store.save("operator", nil, 5, 1, canonical)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.save("operator", &result.captureID, 5, 1, canonical); err != nil {
			t.Fatalf("exact cumulative byte cap rejected: %v", err)
		}
		before := diagnosticCaptureHash(t, store, directory, result.captureID)
		if _, err := store.save("operator", &result.captureID, 1, 1, canonical); !errors.Is(err, errDiagnosticCaptureLimit) {
			t.Fatalf("over cumulative byte cap error=%v", err)
		}
		if after := diagnosticCaptureHash(t, store, directory, result.captureID); before != after {
			t.Fatal("cumulative-cap rejection changed prior file")
		}
	})
	t.Run("active_operator_and_global", func(t *testing.T) {
		limits := diagnosticTestLimits()
		limits.capturesPerOperator = 2
		limits.capturesGlobal = 3
		store, _ := newDiagnosticTestStore(t, limits)
		for index := 0; index < 2; index++ {
			if _, err := store.save("operator-a", nil, 1, 1, canonical); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.save("operator-a", nil, 1, 1, canonical); !errors.Is(err, errDiagnosticCaptureLimit) {
			t.Fatalf("operator active cap error=%v", err)
		}
		if _, err := store.save("operator-b", nil, 1, 1, canonical); err != nil {
			t.Fatal(err)
		}
		if _, err := store.save("operator-c", nil, 1, 1, canonical); !errors.Is(err, errDiagnosticCaptureLimit) {
			t.Fatalf("global active cap error=%v", err)
		}
	})
	t.Run("file_count_off_by_one", func(t *testing.T) {
		limits := diagnosticTestLimits()
		limits.files = 1
		limits.diskBytes = 1 << 20
		store, directory := newDiagnosticTestStore(t, limits)
		result, err := store.save("operator-a", nil, 1, 1, canonical)
		if err != nil {
			t.Fatal(err)
		}
		before := diagnosticCaptureHash(t, store, directory, result.captureID)
		if _, err := store.save("operator-b", nil, 1, 1, canonical); !errors.Is(err, errDiagnosticDiskLimit) {
			t.Fatalf("file cap error=%v", err)
		}
		if after := diagnosticCaptureHash(t, store, directory, result.captureID); before != after {
			t.Fatal("file-cap rejection changed prior file")
		}
	})
	t.Run("disk_bytes_off_by_one", func(t *testing.T) {
		limits := diagnosticTestLimits()
		limits.files = 4
		limits.diskBytes = int64(len(canonical))
		store, directory := newDiagnosticTestStore(t, limits)
		result, err := store.save("operator-a", nil, 1, 1, canonical)
		if err != nil {
			t.Fatal(err)
		}
		before := diagnosticCaptureHash(t, store, directory, result.captureID)
		larger := append(append([]byte{}, canonical...), 'x')
		if _, err := store.save("operator-a", &result.captureID, 1, 1, larger); !errors.Is(err, errDiagnosticDiskLimit) {
			t.Fatalf("disk byte cap error=%v", err)
		}
		if after := diagnosticCaptureHash(t, store, directory, result.captureID); before != after {
			t.Fatal("disk-cap rejection changed prior file")
		}
	})
}

func diagnosticCaptureHash(t *testing.T, store *diagnosticTraceStore, directory, captureID string) [32]byte {
	t.Helper()
	capture := store.captures[captureID]
	body, err := os.ReadFile(filepath.Join(directory, capture.filename))
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(body)
}

func TestDiagnosticTraceAtomicFailuresPreservePriorSnapshot(t *testing.T) {
	store, directory := newDiagnosticTestStore(t, diagnosticTestLimits())
	baseline := []byte("{\"revision\":1}\n")
	result, err := store.save("operator", nil, 10, 1, baseline)
	if err != nil {
		t.Fatal(err)
	}
	before := diagnosticCaptureHash(t, store, directory, result.captureID)
	replacement := []byte("{\"revision\":2}\n")
	originalWrite, originalSync, originalClose, originalRename := store.writeFile, store.fileSync, store.closeFile, store.renameFile
	faults := map[string]func(){
		"short_write": func() {
			store.writeFile = func(file *os.File, body []byte) (int, error) {
				n, err := file.Write(body[:len(body)-1])
				return n, err
			}
		},
		"file_sync": func() {
			store.fileSync = func(*os.File) error { return errors.New("injected sync failure") }
		},
		"file_close": func() {
			store.closeFile = func(*os.File) error { return errors.New("injected close failure") }
		},
		"rename": func() {
			store.renameFile = func(string, string) error { return errors.New("injected rename failure") }
		},
	}
	for name, inject := range faults {
		t.Run(name, func(t *testing.T) {
			store.writeFile, store.fileSync, store.closeFile, store.renameFile = originalWrite, originalSync, originalClose, originalRename
			inject()
			if _, err := store.save("operator", &result.captureID, 10, 1, replacement); !errors.Is(err, errDiagnosticStoreUnavailable) {
				t.Fatalf("fault error=%v", err)
			}
			if after := diagnosticCaptureHash(t, store, directory, result.captureID); after != before {
				t.Fatal("failed replacement changed prior snapshot")
			}
			capture := store.captures[result.captureID]
			if capture.writes != 1 || capture.requestBytes != 10 || capture.canonicalBytes != int64(len(baseline)) {
				t.Fatalf("pre-rename failure consumed successful budget: %+v", capture)
			}
		})
	}
	store.writeFile, store.fileSync, store.closeFile, store.renameFile = originalWrite, originalSync, originalClose, originalRename
}

type diagnosticFaultFileInfo struct {
	os.FileInfo
	sizeDelta int64
	hideSys   bool
}

func (i diagnosticFaultFileInfo) Size() int64 {
	return i.FileInfo.Size() + i.sizeDelta
}

func (i diagnosticFaultFileInfo) Sys() any {
	if i.hideSys {
		return nil
	}
	return i.FileInfo.Sys()
}

func TestDiagnosticTracePostRenameFaultsReportCommittedTruth(t *testing.T) {
	stages := []string{
		"published_validation",
		"published_ownership",
		"published_directory_open",
		"published_directory_sync",
		"published_directory_close",
		"published_backup_remove",
		"published_cleanup_directory_open",
		"published_cleanup_directory_sync",
		"published_cleanup_directory_close",
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			store, directory := newDiagnosticTestStore(t, diagnosticTestLimits())
			created, err := store.save("operator", nil, 10, 1, []byte("{\"revision\":1}\n"))
			if err != nil {
				t.Fatal(err)
			}
			originalLstat := store.publishedLstat
			originalOpen := store.openPublishedDirectory
			originalClose := store.closePublishedDirectory
			originalSync := store.dirSync
			originalRemove := store.removePublishedBackup
			switch stage {
			case "published_validation":
				store.publishedLstat = func(name string) (os.FileInfo, error) {
					info, err := originalLstat(name)
					return diagnosticFaultFileInfo{FileInfo: info, sizeDelta: 1}, err
				}
			case "published_ownership":
				store.publishedLstat = func(name string) (os.FileInfo, error) {
					info, err := originalLstat(name)
					return diagnosticFaultFileInfo{FileInfo: info, hideSys: true}, err
				}
			case "published_directory_open":
				store.openPublishedDirectory = func() (*os.File, error) {
					return nil, errors.New("injected directory open failure")
				}
			case "published_directory_sync":
				store.dirSync = func(*os.File) error { return errors.New("injected directory sync failure") }
			case "published_directory_close":
				calls := 0
				store.closePublishedDirectory = func(file *os.File) error {
					calls++
					if calls == 1 {
						return errors.New("injected directory close failure")
					}
					return originalClose(file)
				}
			case "published_backup_remove":
				store.removePublishedBackup = func(string) error { return errors.New("injected backup removal failure") }
			case "published_cleanup_directory_open":
				calls := 0
				store.openPublishedDirectory = func() (*os.File, error) {
					calls++
					if calls == 2 {
						return nil, errors.New("injected cleanup directory open failure")
					}
					return originalOpen()
				}
			case "published_cleanup_directory_sync":
				calls := 0
				store.dirSync = func(file *os.File) error {
					calls++
					if calls == 2 {
						return errors.New("injected cleanup directory sync failure")
					}
					return originalSync(file)
				}
			case "published_cleanup_directory_close":
				calls := 0
				store.closePublishedDirectory = func(file *os.File) error {
					calls++
					if calls == 2 {
						return errors.New("injected cleanup directory close failure")
					}
					return originalClose(file)
				}
			}
			replacement := []byte("{\"revision\":2}\n")
			replaced, err := store.save("operator", &created.captureID, 10, 1, replacement)
			if err != nil {
				t.Fatalf("post-rename housekeeping fault reported failure: %v", err)
			}
			if replaced.writeOrdinal != 2 || replaced.outcome != "replaced" {
				t.Fatalf("committed result = %+v", replaced)
			}
			if got := strings.Join(replaced.housekeepingFailures, ","); got != stage {
				t.Fatalf("housekeeping stages = %q, want %q", got, stage)
			}
			if after := diagnosticCaptureHash(t, store, directory, created.captureID); after != sha256.Sum256(replacement) {
				t.Fatal("post-rename housekeeping fault did not preserve visible new bytes")
			}
			store.publishedLstat = originalLstat
			store.openPublishedDirectory = originalOpen
			store.closePublishedDirectory = originalClose
			store.dirSync = originalSync
			store.removePublishedBackup = originalRemove
			files, diskBytes, err := store.scanDiskLocked()
			if err != nil {
				t.Fatalf("housekeeping recovery failed: %v", err)
			}
			if files != 1 || diskBytes != int64(len(replacement)) {
				t.Fatalf("recovered disk accounting = files %d bytes %d", files, diskBytes)
			}
			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if diagnosticBackupFilenameRE.MatchString(entry.Name()) {
					t.Fatalf("housekeeping recovery left backup %q", entry.Name())
				}
			}
		})
	}
}

func TestDiagnosticTracePostRenameCreateFaultStillAccountsCommittedTruth(t *testing.T) {
	store, directory := newDiagnosticTestStore(t, diagnosticTestLimits())
	store.openPublishedDirectory = func() (*os.File, error) {
		return nil, errors.New("injected directory open failure")
	}
	canonical := []byte("{\"revision\":1}\n")
	created, err := store.save("operator", nil, 10, 1, canonical)
	if err != nil {
		t.Fatalf("post-rename create fault reported failure: %v", err)
	}
	if created.outcome != "created" || created.writeOrdinal != 1 ||
		strings.Join(created.housekeepingFailures, ",") != "published_directory_open" {
		t.Fatalf("created result = %+v", created)
	}
	capture, ok := store.captures[created.captureID]
	if !ok || capture.writes != 1 || capture.requestBytes != 10 ||
		capture.canonicalBytes != int64(len(canonical)) {
		t.Fatalf("committed create was not accounted: %+v", capture)
	}
	if after := diagnosticCaptureHash(t, store, directory, created.captureID); after != sha256.Sum256(canonical) {
		t.Fatal("post-rename create fault did not preserve visible new bytes")
	}
}

func TestDiagnosticTraceBackupRemovalResidueIsBoundedAndRecoveredAtStartup(t *testing.T) {
	store, directory := newDiagnosticTestStore(t, diagnosticTestLimits())
	created, err := store.save("operator", nil, 10, 1, []byte("{\"revision\":1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	store.removePublishedBackup = func(string) error { return errors.New("injected backup removal failure") }
	for revision := 2; revision <= 8; revision++ {
		replacement := []byte(fmt.Sprintf("{\"revision\":%d}\n", revision))
		result, err := store.save("operator", &created.captureID, 10, 1, replacement)
		if err != nil {
			t.Fatalf("revision %d: %v", revision, err)
		}
		if got := strings.Join(result.housekeepingFailures, ","); got != "published_backup_remove" {
			t.Fatalf("revision %d housekeeping = %q", revision, got)
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		backups := 0
		for _, entry := range entries {
			if diagnosticBackupFilenameRE.MatchString(entry.Name()) {
				backups++
			}
		}
		if backups != 1 {
			t.Fatalf("revision %d backup residue count = %d, want 1", revision, backups)
		}
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	root, info, err := openDiagnosticTraceRoot(directory, filepath.Dir(directory))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := newDiagnosticTraceStoreFromRoot(root, info, diagnosticTestLimits())
	if err != nil {
		_ = root.Close()
		t.Fatalf("startup did not recover bounded backup residue: %v", err)
	}
	defer reopened.close()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if diagnosticBackupFilenameRE.MatchString(entry.Name()) {
			t.Fatalf("startup left backup residue %q", entry.Name())
		}
	}
}

func TestDiagnosticTraceDirectoryIsPinnedAndRejectsSymlinks(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.Mkdir(real, 0770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(real, os.ModeSetgid|0770); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if root, _, err := openDiagnosticTraceRoot(link, parent); err == nil {
		_ = root.Close()
		t.Fatal("symlink diagnostic directory accepted")
	}
	if root, _, err := openDiagnosticTraceRoot(real, filepath.Join(parent, "other")); err == nil {
		_ = root.Close()
		t.Fatal("directory outside configured evidence root accepted")
	}
	root, info, err := openDiagnosticTraceRoot(real, parent)
	if err != nil {
		t.Fatal(err)
	}
	store, err := newDiagnosticTraceStoreFromRoot(root, info, diagnosticTestLimits())
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	defer store.close()
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(real, moved); err != nil {
		t.Fatal(err)
	}
	result, err := store.save("operator", nil, 1, 1, []byte("{}\n"))
	if err != nil {
		t.Fatalf("pinned directory lost after rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, store.captures[result.captureID].filename)); err != nil {
		t.Fatalf("pinned write missing from moved directory: %v", err)
	}
}

func TestDiagnosticTraceConfiguredStartupFailsClosed(t *testing.T) {
	cfg := config.Front{
		Ingress: config.Ingress{
			PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true,
			CanonicalHost: "localhost:43210", OperatorLogin: "operator@example.com",
		},
		DiagnosticTraceDir: filepath.Join(t.TempDir(), "outside-evidence-root"),
	}
	server := newServer(cfg, ".", "localhost:43210")
	if server.diagnosticErr == nil || server.diagnostic != nil {
		t.Fatalf("unsafe configured diagnostic store = store %v error %v", server.diagnostic, server.diagnosticErr)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://localhost/api/diagnostic-traces", nil)
	server.handler().ServeHTTP(response, request)
	if response.Code == http.StatusCreated || response.Code == http.StatusOK {
		t.Fatalf("failed diagnostic store left route active: %d", response.Code)
	}
}

func TestDiagnosticTraceAuditIsMetadataOnly(t *testing.T) {
	store, directory := newDiagnosticTestStore(t, diagnosticTestLimits())
	var logs bytes.Buffer
	prior := frontLogf
	frontLogf = func(format string, args ...any) { _, _ = fmt.Fprintf(&logs, format, args...) }
	t.Cleanup(func() { frontLogf = prior })
	body := diagnosticTestBody(t, nil, diagnosticTestTrace(1))
	response := diagnosticDirectRequest(store, body, "operator@example.com")
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d", response.Code)
	}
	result := decodedDiagnosticResponse(t, response)
	logged := logs.String()
	for _, required := range []string{"event=diagnostic_trace_write", `operator="operator@example.com"`, "capture_hash=", "request_bytes=", "canonical_bytes=", "event_count=1", "write_ordinal=1", `outcome="created"`} {
		if !strings.Contains(logged, required) {
			t.Fatalf("audit missing %q: %q", required, logged)
		}
	}
	for _, forbidden := range []string{result.CaptureID, directory, "trace-", `"schema"`, testCSRF} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("audit leaked %q: %q", forbidden, logged)
		}
	}
	logs.Reset()
	store.openPublishedDirectory = func() (*os.File, error) {
		return nil, errors.New("injected post-rename directory open failure")
	}
	replacement := diagnosticDirectRequest(
		store,
		diagnosticTestBody(t, &result.CaptureID, diagnosticTestTrace(1)),
		"operator@example.com",
	)
	if replacement.Code != http.StatusOK {
		t.Fatalf("housekeeping fault status=%d body=%q", replacement.Code, replacement.Body.String())
	}
	logged = logs.String()
	for _, required := range []string{
		"event=diagnostic_trace_housekeeping_failure",
		`stages="published_directory_open"`,
		"event=diagnostic_trace_write",
		`outcome="replaced"`,
	} {
		if !strings.Contains(logged, required) {
			t.Fatalf("housekeeping audit missing %q: %q", required, logged)
		}
	}
	for _, forbidden := range []string{result.CaptureID, directory, "trace-", `"schema"`, testCSRF} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("housekeeping audit leaked %q: %q", forbidden, logged)
		}
	}
}

func TestDiagnosticTraceEndpointRejectsWrongMediaTypeAndMissingIdentity(t *testing.T) {
	store, _ := newDiagnosticTestStore(t, diagnosticTestLimits())
	cfg := config.Front{Ingress: config.Ingress{PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true, OperatorLogin: "operator@example.com"}}
	server := &Server{cfg: cfg, diagnostic: store}
	for name, mutate := range map[string]func(*http.Request){
		"wrong_media_type": func(request *http.Request) {
			request.Header.Set("Content-Type", "text/plain")
			request = request.WithContext(context.WithValue(request.Context(), ingressKey{}, ingressIdentity{
				uid: cfg.Ingress.PeerUID, operator: cfg.Ingress.OperatorLogin,
			}))
		},
		"missing_identity": func(request *http.Request) {
			request.Header.Set("Content-Type", "application/json")
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://localhost/api/diagnostic-traces", bytes.NewReader(diagnosticTestBody(t, nil, diagnosticTestTrace(1))))
			mutate(request)
			if name == "wrong_media_type" {
				request = request.WithContext(context.WithValue(request.Context(), ingressKey{}, ingressIdentity{
					uid: cfg.Ingress.PeerUID, operator: cfg.Ingress.OperatorLogin,
				}))
			}
			response := httptest.NewRecorder()
			server.diagnosticTrace(response, request)
			if name == "wrong_media_type" && response.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("wrong media type status=%d", response.Code)
			}
			if name == "missing_identity" && response.Code != http.StatusForbidden {
				t.Fatalf("missing identity status=%d", response.Code)
			}
		})
	}
}

func TestDiagnosticTraceStartupCapsExistingEvidence(t *testing.T) {
	limits := diagnosticTestLimits()
	limits.files = 1
	parent := t.TempDir()
	directory := filepath.Join(parent, "evidence")
	if err := os.Mkdir(directory, 0770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, os.ModeSetgid|0770); err != nil {
		t.Fatal(err)
	}
	name := "trace-" + strings.Repeat("a", 32) + ".json"
	if err := os.WriteFile(filepath.Join(directory, name), []byte("{}\n"), diagnosticTraceFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(directory, name), diagnosticTraceFileMode); err != nil {
		t.Fatal(err)
	}
	root, info, err := openDiagnosticTraceRoot(directory, parent)
	if err != nil {
		t.Fatal(err)
	}
	store, err := newDiagnosticTraceStoreFromRoot(root, info, limits)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	defer store.close()
	if _, err := store.save("operator", nil, 1, 1, []byte("{}\n")); !errors.Is(err, errDiagnosticDiskLimit) {
		t.Fatalf("pre-existing evidence file cap error=%v", err)
	}
}

func TestDiagnosticTraceShortRandomReadFailsClosed(t *testing.T) {
	store, _ := newDiagnosticTestStore(t, diagnosticTestLimits())
	store.randomRead = func(value []byte) (int, error) {
		for index := range value[:len(value)-1] {
			value[index] = 1
		}
		return len(value) - 1, nil
	}
	if _, err := store.save("operator", nil, 1, 1, []byte("{}\n")); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short entropy read error=%v", err)
	}
}
