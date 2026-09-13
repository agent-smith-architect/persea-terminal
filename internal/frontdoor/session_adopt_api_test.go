package frontdoor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func postAdoption(h http.Handler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session-adoptions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAdoptSessionForwardsIdentityAndReportsSuccess(t *testing.T) {
	socket, captured := stubCreateBroker(t, func(proto.Control) proto.Control {
		return proto.Control{Type: "adopt_ok", ServerLabel: "s", SessionID: "$17"}
	})
	h := createServer(t, socket)
	w := postAdoption(h, `{"realm":"r","server":"s","session_id":"$17"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("adopt status = %d, body %q", w.Code, w.Body.String())
	}
	got := captured()
	if got.Type != "adopt" || got.ServerLabel != "s" || got.SessionID != "$17" {
		t.Fatalf("front door forwarded %+v", got)
	}
}

func TestAdoptSessionMapsRefusalCodes(t *testing.T) {
	for code, want := range map[string]int{
		"not_permitted":        http.StatusForbidden,
		"blocked_alt_screen":   http.StatusConflict,
		"blocked_multi_pane":   http.StatusConflict,
		"blocked_multi_window": http.StatusConflict,
		"adoption_in_progress": http.StatusConflict,
		"session_gone":         http.StatusGone,
		"slots_exhausted":      http.StatusServiceUnavailable,
		"unified_unavailable":  http.StatusServiceUnavailable,
		"adoption_contended":   http.StatusServiceUnavailable,
		"adopt_failed":         http.StatusBadGateway,
		"something_new":        http.StatusBadGateway,
	} {
		t.Run(code, func(t *testing.T) {
			socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control {
				return proto.Control{Type: "adopt_refused", Code: code}
			})
			w := postAdoption(createServer(t, socket), `{"realm":"r","server":"s","session_id":"$17"}`)
			if w.Code != want {
				t.Fatalf("code %q mapped to %d, want %d", code, w.Code, want)
			}
		})
	}
}

func TestAdoptSessionForwardsSelectedHistoryRows(t *testing.T) {
	for _, depth := range []int{0, 500, 5000, 10000} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			socket, captured := stubCreateBroker(t, func(proto.Control) proto.Control {
				return proto.Control{Type: "adopt_ok", ServerLabel: "s", SessionID: "$17"}
			})
			w := postAdoption(createServer(t, socket), fmt.Sprintf(`{"realm":"r","server":"s","session_id":"$17","history_rows":%d}`, depth))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			got := captured()
			if got.HistoryRows == nil || *got.HistoryRows != depth {
				t.Fatalf("history selection was not forwarded: %+v", got)
			}
		})
	}
}

func TestAdoptSessionDoesNotEchoBrokerText(t *testing.T) {
	socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control {
		return proto.Control{Type: "adopt_refused", Code: "session_gone", Msg: "<img src=x onerror=alert(1)>"}
	})
	w := postAdoption(createServer(t, socket), `{"realm":"r","server":"s","session_id":"$17"}`)
	if bytes.Contains(w.Body.Bytes(), []byte("onerror")) {
		t.Fatalf("broker message reached the response: %q", w.Body.String())
	}
}

// Adoption carries identity only. Anything that could shape execution —
// command, directory, geometry, or even a name — is rejected outright.
func TestAdoptSessionRejectsMalformedRequests(t *testing.T) {
	socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control {
		return proto.Control{Type: "adopt_ok"}
	})
	h := createServer(t, socket)
	for name, body := range map[string]string{
		"unknown_realm":      `{"realm":"other","server":"s","session_id":"$17"}`,
		"missing_realm":      `{"server":"s","session_id":"$17"}`,
		"empty_server":       `{"realm":"r","server":"","session_id":"$17"}`,
		"empty_session":      `{"realm":"r","server":"s","session_id":""}`,
		"name_field":         `{"realm":"r","server":"s","session_id":"$17","name":"work"}`,
		"command_field":      `{"realm":"r","server":"s","session_id":"$17","command":"sh"}`,
		"directory_field":    `{"realm":"r","server":"s","session_id":"$17","cwd":"/etc"}`,
		"geometry_field":     `{"realm":"r","server":"s","session_id":"$17","columns":200}`,
		"negative_history":   `{"realm":"r","server":"s","session_id":"$17","history_rows":-1}`,
		"excess_history":     `{"realm":"r","server":"s","session_id":"$17","history_rows":10001}`,
		"fractional_history": `{"realm":"r","server":"s","session_id":"$17","history_rows":0.5}`,
		"string_history":     `{"realm":"r","server":"s","session_id":"$17","history_rows":"500"}`,
		"duplicate_key":      `{"realm":"r","realm":"r","server":"s","session_id":"$17"}`,
		"not_json":           `not json`,
		"trailing_data":      `{"realm":"r","server":"s","session_id":"$17"} {}`,
		"array_body":         `[{"realm":"r","server":"s","session_id":"$17"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if w := postAdoption(h, body); w.Code != http.StatusBadRequest {
				t.Fatalf("malformed request accepted with %d: %s", w.Code, body)
			}
		})
	}
}

func TestAdoptSessionUnreachableBrokerIsNotARefusal(t *testing.T) {
	dir := shortTestDir(t)
	cfg := config.Front{
		Realms:           []config.Realm{{Name: "r", Socket: filepath.Join(dir, "absent.sock"), BrokerUID: uint32(os.Getuid()), BrokerUIDConfigured: true}},
		HandleTTLSeconds: 60,
		HandleCapacity:   8,
	}
	h := newServer(cfg, ".", "127.0.0.1:8080").handler()
	if w := postAdoption(h, `{"realm":"r","server":"s","session_id":"$17"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable broker status = %d", w.Code)
	}
}

func TestAdoptSessionRejectsUnexpectedBrokerReplies(t *testing.T) {
	for name, reply := range map[string]proto.Control{
		"wrong_type":      {Type: "inventory_ok"},
		"refusal_no_code": {Type: "adopt_refused"},
		"create_response": {Type: "create_ok"},
	} {
		t.Run(name, func(t *testing.T) {
			socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control { return reply })
			if w := postAdoption(createServer(t, socket), `{"realm":"r","server":"s","session_id":"$17"}`); w.Code != http.StatusServiceUnavailable {
				t.Fatalf("unexpected broker reply produced %d", w.Code)
			}
		})
	}
}

func TestRefitSessionForwardsExactBoundAuthorityAndOperation(t *testing.T) {
	operation := strings.Repeat("O", 43)
	source := strings.Repeat("S", 43)
	successor := strings.Repeat("N", 43)
	authority := auth(77, "$17")
	// The broker echoes the rows it applied: the predecessor's (24 here) when
	// none were requested.
	socket, captured := stubCreateBroker(t, func(request proto.Control) proto.Control {
		return proto.Control{Type: "refit_ok", ID: request.ID, Cols: request.Cols, Rows: 24, Successor: successor}
	})
	cfg := config.Front{
		Realms:           []config.Realm{{Name: "r", Socket: socket, BrokerUID: uint32(os.Getuid()), BrokerUIDConfigured: true}},
		HandleTTLSeconds: 60, HandleCapacity: 8,
	}
	server := newServer(cfg, ".", "127.0.0.1:8080")
	release, err := server.bindings.bind("", source, authority)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session-refits", bytes.NewBufferString(`{"source":"`+source+`","columns":117,"operation":"`+operation+`"}`))
	rr := httptest.NewRecorder()
	server.handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("refit status=%d body=%q", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, `"successor_source":"`+successor+`"`) || !strings.Contains(body, `"rows":24`) {
		t.Fatalf("refit response does not bind successor and echoed rows: %q", body)
	}
	got := captured()
	if got.Type != "refit" || got.Authority == nil || authorityKey(*got.Authority) != authorityKey(authority) || got.Cols != 117 || got.ID != operation || got.Rows != 0 {
		t.Fatalf("front door forwarded %+v", got)
	}
}

// TestRefitSessionCarriesOptionalRows pins UX-14 14.1: a request may name the
// successor's rows; the front door forwards them verbatim, requires the broker
// to echo exactly that count, and reports it in the response. Omitted rows
// forward as 0 and the response carries the broker-echoed count instead.
func TestRefitSessionCarriesOptionalRows(t *testing.T) {
	operation := strings.Repeat("O", 43)
	source := strings.Repeat("S", 43)
	successor := strings.Repeat("N", 43)
	authority := auth(77, "$17")
	for name, tc := range map[string]struct {
		body         string
		echoRows     int
		wantForward  int
		wantResponse int
		wantStatus   int
	}{
		"rows_given":        {body: `{"source":"` + source + `","columns":117,"rows":40,"operation":"` + operation + `"}`, echoRows: 40, wantForward: 40, wantResponse: 40, wantStatus: http.StatusOK},
		"rows_omitted":      {body: `{"source":"` + source + `","columns":117,"operation":"` + operation + `"}`, echoRows: 31, wantForward: 0, wantResponse: 31, wantStatus: http.StatusOK},
		"rows_zero":         {body: `{"source":"` + source + `","columns":117,"rows":0,"operation":"` + operation + `"}`, echoRows: 31, wantForward: 0, wantResponse: 31, wantStatus: http.StatusOK},
		"echo_mismatch":     {body: `{"source":"` + source + `","columns":117,"rows":40,"operation":"` + operation + `"}`, echoRows: 41, wantForward: 40, wantStatus: http.StatusServiceUnavailable},
		"echo_missing_rows": {body: `{"source":"` + source + `","columns":117,"operation":"` + operation + `"}`, echoRows: 0, wantForward: 0, wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			socket, captured := stubCreateBroker(t, func(request proto.Control) proto.Control {
				return proto.Control{Type: "refit_ok", ID: request.ID, Cols: request.Cols, Rows: tc.echoRows, Successor: successor}
			})
			cfg := config.Front{
				Realms:           []config.Realm{{Name: "r", Socket: socket, BrokerUID: uint32(os.Getuid()), BrokerUIDConfigured: true}},
				HandleTTLSeconds: 60, HandleCapacity: 8,
			}
			server := newServer(cfg, ".", "127.0.0.1:8080")
			release, err := server.bindings.bind("", source, authority)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session-refits", bytes.NewBufferString(tc.body))
			rr := httptest.NewRecorder()
			server.handler().ServeHTTP(rr, req)
			if rr.Code != tc.wantStatus {
				t.Fatalf("refit status=%d want %d body=%q", rr.Code, tc.wantStatus, rr.Body.String())
			}
			got := captured()
			if got.Type != "refit" || got.Cols != 117 || got.Rows != tc.wantForward || got.ID != operation {
				t.Fatalf("front door forwarded %+v want rows %d", got, tc.wantForward)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			body := rr.Body.String()
			if !strings.Contains(body, `"rows":`+strconvItoa(tc.wantResponse)) || !strings.Contains(body, `"columns":117`) || !strings.Contains(body, `"successor_source":"`+successor+`"`) {
				t.Fatalf("refit response %q does not carry rows %d", body, tc.wantResponse)
			}
			var fields map[string]any
			if err := json.Unmarshal([]byte(body), &fields); err != nil || len(fields) != 4 {
				t.Fatalf("refit response shape %q (%v): want exactly operation, columns, rows, successor_source", body, err)
			}
		})
	}
}

func strconvItoa(value int) string { return fmt.Sprintf("%d", value) }

func TestRefitSessionRejectsUnboundMalformedAndWidthBearingShapes(t *testing.T) {
	socket, _ := stubCreateBroker(t, func(request proto.Control) proto.Control {
		return proto.Control{Type: "refit_ok", ID: request.ID, Cols: request.Cols, Successor: strings.Repeat("N", 43)}
	})
	cfg := config.Front{
		Realms:           []config.Realm{{Name: "r", Socket: socket, BrokerUID: uint32(os.Getuid()), BrokerUIDConfigured: true}},
		HandleTTLSeconds: 60, HandleCapacity: 8,
	}
	server := newServer(cfg, ".", "127.0.0.1:8080")
	source := strings.Repeat("S", 43)
	release, err := server.bindings.bind("", source, auth(77, "$17"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	operation := strings.Repeat("O", 43)
	for name, body := range map[string]string{
		"unbound":       `{"source":"` + strings.Repeat("X", 43) + `","columns":90,"operation":"` + operation + `"}`,
		"zero":          `{"source":"` + source + `","columns":0,"operation":"` + operation + `"}`,
		"below_policy":  `{"source":"` + source + `","columns":19,"operation":"` + operation + `"}`,
		"above_policy":  `{"source":"` + source + `","columns":301,"operation":"` + operation + `"}`,
		"bad_operation": `{"source":"` + source + `","columns":90,"operation":"short"}`,
		// Rows are optional since UX-14, but a given count obeys the closed row
		// policy (MinFitRows..MaxFitRows, cell cap): reject, never clamp.
		"rows_below_policy": `{"source":"` + source + `","columns":90,"rows":7,"operation":"` + operation + `"}`,
		"rows_above_policy": `{"source":"` + source + `","columns":90,"rows":121,"operation":"` + operation + `"}`,
		"rows_negative":     `{"source":"` + source + `","columns":90,"rows":-24,"operation":"` + operation + `"}`,
		"rows_fraction":     `{"source":"` + source + `","columns":90,"rows":40.5,"operation":"` + operation + `"}`,
		"duplicate":         `{"source":"` + source + `","columns":90,"columns":91,"operation":"` + operation + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session-refits", bytes.NewBufferString(body))
			rr := httptest.NewRecorder()
			server.handler().ServeHTTP(rr, req)
			if rr.Code == http.StatusOK {
				t.Fatalf("invalid refit accepted: %s", body)
			}
		})
	}
}
