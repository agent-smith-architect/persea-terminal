package frontdoor

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func getPreview(h http.Handler, query string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/session-previews?"+query, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func previewTestServer(t *testing.T, socket string, now func() time.Time) *Server {
	t.Helper()
	cfg := config.Front{
		Realms:           []config.Realm{{Name: "r", Socket: socket, BrokerUID: uint32(os.Getuid()), BrokerUIDConfigured: true}},
		HandleTTLSeconds: 60,
		HandleCapacity:   8,
	}
	s := newServer(cfg, ".", "127.0.0.1:8080")
	s.previews.now = now
	return s
}

func TestPreviewSessionForwardsIdentityAndRelaysRows(t *testing.T) {
	socket, captured := stubCreateBroker(t, func(proto.Control) proto.Control {
		return proto.Control{Type: "preview_ok", Width: 80, Height: 24, Pane: "%3", FrozenAt: 1_700_000_000_123, Truncated: true, Lines: []string{"alpha", "", "tab\there"}}
	})
	w := getPreview(createServer(t, socket), "realm=r&server=s&session_id=%2417")
	if w.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body %q", w.Code, w.Body.String())
	}
	got := captured()
	if got.Type != "preview" || got.ServerLabel != "s" || got.SessionID != "$17" {
		t.Fatalf("front door forwarded %+v", got)
	}
	var body struct {
		Realm      string   `json:"realm"`
		Server     string   `json:"server"`
		SessionID  string   `json:"session_id"`
		Rows       []string `json:"rows"`
		Width      int      `json:"width"`
		Height     int      `json:"height"`
		CapturedAt int64    `json:"captured_at"`
		Truncated  bool     `json:"truncated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Realm != "r" || body.Server != "s" || body.SessionID != "$17" || body.Width != 80 || body.Height != 24 || body.CapturedAt != 1_700_000_000_123 || !body.Truncated {
		t.Fatalf("preview body lost fields: %+v", body)
	}
	if len(body.Rows) != 3 || body.Rows[0] != "alpha" || body.Rows[1] != "" || body.Rows[2] != "tab\there" {
		t.Fatalf("preview rows changed in transit: %q", body.Rows)
	}
}

func TestPreviewSessionRelaysEmptyRowsAsArray(t *testing.T) {
	socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control {
		return proto.Control{Type: "preview_ok", Width: 80, Height: 24, FrozenAt: 1}
	})
	w := getPreview(createServer(t, socket), "realm=r&server=s&session_id=%2417")
	if w.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body %q", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"rows":[]`)) {
		t.Fatalf("empty preview did not serialize rows as an array: %q", w.Body.String())
	}
}

func TestPreviewSessionMapsRefusalCodes(t *testing.T) {
	for code, want := range map[string]int{
		"session_gone":       http.StatusGone,
		"server_unavailable": http.StatusServiceUnavailable,
		"preview_failed":     http.StatusBadGateway,
		"something_new":      http.StatusBadGateway,
	} {
		t.Run(code, func(t *testing.T) {
			socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control {
				return proto.Control{Type: "preview_refused", Code: code}
			})
			if w := getPreview(createServer(t, socket), "realm=r&server=s&session_id=%2417"); w.Code != want {
				t.Fatalf("code %q mapped to %d, want %d", code, w.Code, want)
			}
		})
	}
}

func TestPreviewSessionDoesNotEchoBrokerText(t *testing.T) {
	socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control {
		return proto.Control{Type: "preview_refused", Code: "session_gone", Msg: "<img src=x onerror=alert(1)>"}
	})
	w := getPreview(createServer(t, socket), "realm=r&server=s&session_id=%2417")
	if bytes.Contains(w.Body.Bytes(), []byte("onerror")) {
		t.Fatalf("broker message reached the response: %q", w.Body.String())
	}
}

// The preview carries identity only, in exactly three query parameters. Extra
// parameters, duplicates, unknown realms, and oversized identities are all
// rejected before any broker is consulted.
func TestPreviewSessionRejectsMalformedRequests(t *testing.T) {
	socket, captured := stubCreateBroker(t, func(proto.Control) proto.Control {
		return proto.Control{Type: "preview_ok", Width: 1, Height: 1, FrozenAt: 1}
	})
	h := createServer(t, socket)
	for name, query := range map[string]string{
		"unknown_realm":     "realm=other&server=s&session_id=%2417",
		"missing_realm":     "server=s&session_id=%2417",
		"empty_server":      "realm=r&server=&session_id=%2417",
		"empty_session":     "realm=r&server=s&session_id=",
		"missing_session":   "realm=r&server=s",
		"depth_param":       "realm=r&server=s&session_id=%2417&depth=4000",
		"duplicate_session": "realm=r&server=s&session_id=%2417&session_id=%2418",
		"oversized_session": "realm=r&server=s&session_id=" + string(bytes.Repeat([]byte{'9'}, 33)),
	} {
		t.Run(name, func(t *testing.T) {
			if w := getPreview(h, query); w.Code != http.StatusBadRequest {
				t.Fatalf("malformed request accepted with %d: %s", w.Code, query)
			}
			if got := captured(); got.Type != "" {
				t.Fatalf("malformed request reached the broker: %+v", got)
			}
		})
	}
}

func TestPreviewSessionUnreachableBrokerIsNotARefusal(t *testing.T) {
	dir := shortTestDir(t)
	s := previewTestServer(t, filepath.Join(dir, "absent.sock"), time.Now)
	if w := getPreview(s.handler(), "realm=r&server=s&session_id=%2417"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable broker status = %d", w.Code)
	}
}

func TestPreviewSessionRejectsUnexpectedBrokerReplies(t *testing.T) {
	tooManyRows := make([]string, proto.PreviewRowLimit+2)
	for name, reply := range map[string]proto.Control{
		"wrong_type":      {Type: "inventory_ok"},
		"refusal_no_code": {Type: "preview_refused"},
		"adopt_response":  {Type: "adopt_ok"},
		"zero_geometry":   {Type: "preview_ok", Width: 0, Height: 24, FrozenAt: 1},
		"no_captured_at":  {Type: "preview_ok", Width: 80, Height: 24},
		"row_overflow":    {Type: "preview_ok", Width: 80, Height: 24, FrozenAt: 1, Lines: tooManyRows},
	} {
		t.Run(name, func(t *testing.T) {
			socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control { return reply })
			if w := getPreview(createServer(t, socket), "realm=r&server=s&session_id=%2417"); w.Code != http.StatusServiceUnavailable {
				t.Fatalf("unexpected broker reply produced %d", w.Code)
			}
		})
	}
}

// One session's outcome is replayed from the TTL cache: the second request
// answers identically without a broker round-trip, and after the TTL the
// broker is consulted again.
func TestPreviewSessionCachesWithinTTL(t *testing.T) {
	brokered := 0
	socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control {
		brokered++
		return proto.Control{Type: "preview_ok", Width: 80, Height: 24, FrozenAt: int64(brokered)}
	})
	current := time.Unix(1_700_000_000, 0)
	s := previewTestServer(t, socket, func() time.Time { return current })
	h := s.handler()

	first := getPreview(h, "realm=r&server=s&session_id=%2417")
	second := getPreview(h, "realm=r&server=s&session_id=%2417")
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("cached preview statuses = %d, %d", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("cache replay changed the body: %q vs %q", first.Body.String(), second.Body.String())
	}
	if brokered != 1 {
		t.Fatalf("fresh cache entry still reached the broker %d times", brokered)
	}
	current = current.Add(PreviewCacheTTL + time.Second)
	third := getPreview(h, "realm=r&server=s&session_id=%2417")
	if third.Code != http.StatusOK || brokered != 2 {
		t.Fatalf("expired cache entry was not refreshed: status=%d brokered=%d", third.Code, brokered)
	}
	if third.Body.String() == first.Body.String() {
		t.Fatalf("post-TTL response did not come from a fresh capture: %q", third.Body.String())
	}
}

// Typed refusals are cached exactly like successes, so a dashboard retrying a
// dead row cannot hammer tmux either.
func TestPreviewSessionCachesRefusals(t *testing.T) {
	brokered := 0
	socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control {
		brokered++
		return proto.Control{Type: "preview_refused", Code: "session_gone"}
	})
	current := time.Unix(1_700_000_000, 0)
	s := previewTestServer(t, socket, func() time.Time { return current })
	h := s.handler()
	for i := 0; i < 3; i++ {
		if w := getPreview(h, "realm=r&server=s&session_id=%2417"); w.Code != http.StatusGone {
			t.Fatalf("refusal replay %d = %d", i, w.Code)
		}
	}
	if brokered != 1 {
		t.Fatalf("cached refusal still reached the broker %d times", brokered)
	}
}

// Cache misses spend from a token budget: with a frozen clock the burst is
// finite, exhaustion answers 429 without consulting the broker, and refill
// restores service.
func TestPreviewSessionBudgetLimitsCacheMisses(t *testing.T) {
	brokered := 0
	socket, captured := stubCreateBroker(t, func(proto.Control) proto.Control {
		brokered++
		return proto.Control{Type: "preview_ok", Width: 80, Height: 24, FrozenAt: 1}
	})
	current := time.Unix(1_700_000_000, 0)
	s := previewTestServer(t, socket, func() time.Time { return current })
	h := s.handler()
	for i := 0; i < PreviewFetchBurst; i++ {
		query := "realm=r&server=s&session_id=%24" + string(rune('1'+i))
		if w := getPreview(h, query); w.Code != http.StatusOK {
			t.Fatalf("in-budget miss %d = %d body %q", i, w.Code, w.Body.String())
		}
		// The stub's capture channel is small; keep it drained.
		_ = captured()
	}
	if w := getPreview(h, "realm=r&server=s&session_id=%24999"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("exhausted budget answered %d", w.Code)
	}
	if brokered != PreviewFetchBurst {
		t.Fatalf("exhausted budget still reached the broker: %d", brokered)
	}
	current = current.Add(time.Second)
	if w := getPreview(h, "realm=r&server=s&session_id=%24999"); w.Code != http.StatusOK {
		t.Fatalf("refilled budget answered %d", w.Code)
	}
}
