package frontdoor

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

// stubCreateBroker answers one create request with the supplied control message.
// The captured request is returned so a test can assert what the front door
// actually forwarded — the point of several cases below is that it forwards a
// name unchanged rather than pre-validating it.
func stubCreateBroker(t *testing.T, reply func(proto.Control) proto.Control) (string, func() proto.Control) {
	t.Helper()
	dir := shortTestDir(t)
	socket := filepath.Join(dir, "create.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	seen := make(chan proto.Control, 4)
	go func() {
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			go func() {
				defer c.Close()
				if _, e := proto.ReadFrame(c); e != nil {
					return
				}
				if writeControl(c, proto.Control{Type: "hello_ok", V: 1}) != nil {
					return
				}
				f, e := proto.ReadFrame(c)
				if e != nil {
					return
				}
				ctrl, e := proto.DecodeControl(f.Payload)
				if e != nil {
					return
				}
				seen <- ctrl
				_ = writeControl(c, reply(ctrl))
			}()
		}
	}()
	return socket, func() proto.Control {
		select {
		case c := <-seen:
			return c
		default:
			return proto.Control{}
		}
	}
}

func createServer(t *testing.T, socket string) http.Handler {
	t.Helper()
	cfg := config.Front{
		Realms:           []config.Realm{{Name: "r", Socket: socket, BrokerUID: uint32(os.Getuid()), BrokerUIDConfigured: true}},
		HandleTTLSeconds: 60,
		HandleCapacity:   8,
	}
	return newServer(cfg, ".", "127.0.0.1:8080").handler()
}

func postSession(h http.Handler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/sessions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCreateSessionForwardsAndReportsSuccess(t *testing.T) {
	socket, captured := stubCreateBroker(t, func(proto.Control) proto.Control {
		return proto.Control{Type: "create_ok", ServerLabel: "s", Name: "work"}
	})
	h := createServer(t, socket)
	w := postSession(h, `{"realm":"r","server":"s","name":"work"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %q", w.Code, w.Body.String())
	}
	got := captured()
	if got.Type != "create" || got.ServerLabel != "s" || got.Name != "work" {
		t.Fatalf("front door forwarded %+v", got)
	}
}

func TestUnifiedDevLaunchBindsOnlyTheCreateArm(t *testing.T) {
	create := bindUnifiedDevLaunch(&proto.UnifiedDevLaunch{State: proto.UnifiedDevLaunchCreate, Name: "unified-target"}, nil)
	if create == nil || create.State != proto.UnifiedDevLaunchCreate || create.Name != "unified-target" || create.SessionID != "" || create.Control != "" {
		t.Fatalf("create launch binding = %+v", create)
	}
	for name, launch := range map[string]*proto.UnifiedDevLaunch{
		"create with session": {State: proto.UnifiedDevLaunchCreate, Name: "unified-target", SessionID: "$17"},
		"retired open state":  {State: "open", Name: "unified-target", SessionID: "$17"},
		"retired blocked":     {State: "blocked_existing_unobserved", Name: "unified-target", SessionID: "$17"},
		"unknown state":       {State: "late_attach", Name: "unified-target"},
		"nameless":            {State: proto.UnifiedDevLaunchCreate},
	} {
		t.Run(name, func(t *testing.T) {
			if got := bindUnifiedDevLaunch(launch, nil); got != nil {
				t.Fatalf("unsafe launch projection escaped: %+v", got)
			}
		})
	}
}

// The per-session unified projection passes through the front door only inside
// its closed vocabulary; anything else hides the affordance for that row.
func TestUnifiedSessionBindingFailsClosed(t *testing.T) {
	for name, test := range map[string]struct {
		state *proto.UnifiedSessionState
		want  *unifiedSessionView
	}{
		"absent":                {nil, nil},
		"open birth":            {&proto.UnifiedSessionState{State: "open", Origin: "birth"}, &unifiedSessionView{State: "open", Origin: "birth"}},
		"open reconstructed":    {&proto.UnifiedSessionState{State: "open", Origin: "reconstructed"}, &unifiedSessionView{State: "open", Origin: "reconstructed"}},
		"open without origin":   {&proto.UnifiedSessionState{State: "open"}, nil},
		"open bad origin":       {&proto.UnifiedSessionState{State: "open", Origin: "cloned"}, nil},
		"adoptable":             {&proto.UnifiedSessionState{State: "adoptable"}, &unifiedSessionView{State: "adoptable"}},
		"adoptable with origin": {&proto.UnifiedSessionState{State: "adoptable", Origin: "birth"}, nil},
		"blocked alt":           {&proto.UnifiedSessionState{State: "blocked_alt_screen"}, &unifiedSessionView{State: "blocked_alt_screen"}},
		"blocked multi pane":    {&proto.UnifiedSessionState{State: "blocked_multi_pane"}, &unifiedSessionView{State: "blocked_multi_pane"}},
		"blocked multi window":  {&proto.UnifiedSessionState{State: "blocked_multi_window"}, &unifiedSessionView{State: "blocked_multi_window"}},
		"blocked foreign":       {&proto.UnifiedSessionState{State: "blocked_foreign_server"}, &unifiedSessionView{State: "blocked_foreign_server"}},
		"slots exhausted":       {&proto.UnifiedSessionState{State: "slots_exhausted"}, &unifiedSessionView{State: "slots_exhausted"}},
		"unavailable":           {&proto.UnifiedSessionState{State: "unavailable"}, &unifiedSessionView{State: "unavailable"}},
		"unknown state":         {&proto.UnifiedSessionState{State: "late_attach"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := bindUnifiedSession(test.state)
			if (got == nil) != (test.want == nil) {
				t.Fatalf("binding = %+v, want %+v", got, test.want)
			}
			if got != nil && *got != *test.want {
				t.Fatalf("binding = %+v, want %+v", got, test.want)
			}
		})
	}
}

// The grammar lives in exactly one place: broker configuration. The front door
// must forward a name it would consider odd rather than second-guessing policy,
// otherwise two validators drift apart and the refusal messages stop matching
// what the realm actually allows.
func TestCreateSessionDoesNotPreValidateTheName(t *testing.T) {
	socket, captured := stubCreateBroker(t, func(proto.Control) proto.Control {
		return proto.Control{Type: "create_refused", Code: "invalid_name", Msg: "no"}
	})
	h := createServer(t, socket)
	w := postSession(h, `{"realm":"r","server":"s","name":"Has_Odd-Name"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("refusal status = %d", w.Code)
	}
	if got := captured(); got.Name != "Has_Odd-Name" {
		t.Fatalf("front door altered or withheld the name: %+v", got)
	}
}

func TestCreateSessionMapsRefusalCodes(t *testing.T) {
	for code, want := range map[string]int{
		"invalid_name":       http.StatusBadRequest,
		"not_permitted":      http.StatusForbidden,
		"name_taken":         http.StatusConflict,
		"at_capacity":        http.StatusConflict,
		"server_unavailable": http.StatusServiceUnavailable,
		"create_failed":      http.StatusBadGateway,
		"something_new":      http.StatusBadGateway,
	} {
		t.Run(code, func(t *testing.T) {
			socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control {
				return proto.Control{Type: "create_refused", Code: code, Msg: "refused"}
			})
			w := postSession(createServer(t, socket), `{"realm":"r","server":"s","name":"work"}`)
			if w.Code != want {
				t.Fatalf("code %q mapped to %d, want %d", code, w.Code, want)
			}
		})
	}
}

// A broker must not be able to place arbitrary text on the operator's page. The
// response carries the closed-set code, never the broker's free-text message.
func TestCreateSessionDoesNotEchoBrokerText(t *testing.T) {
	socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control {
		return proto.Control{Type: "create_refused", Code: "name_taken", Msg: "<img src=x onerror=alert(1)>"}
	})
	w := postSession(createServer(t, socket), `{"realm":"r","server":"s","name":"work"}`)
	if bytes.Contains(w.Body.Bytes(), []byte("onerror")) {
		t.Fatalf("broker message reached the response: %q", w.Body.String())
	}
}

func TestCreateSessionRejectsMalformedRequests(t *testing.T) {
	socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control {
		return proto.Control{Type: "create_ok"}
	})
	h := createServer(t, socket)
	for name, body := range map[string]string{
		"unknown_realm": `{"realm":"other","server":"s","name":"work"}`,
		"missing_realm": `{"server":"s","name":"work"}`,
		"empty_server":  `{"realm":"r","server":"","name":"work"}`,
		"empty_name":    `{"realm":"r","server":"s","name":""}`,
		"unknown_field": `{"realm":"r","server":"s","name":"work","command":"sh"}`,
		"duplicate_key": `{"realm":"r","realm":"r","server":"s","name":"work"}`,
		"not_json":      `not json`,
		"trailing_data": `{"realm":"r","server":"s","name":"work"} {}`,
		"array_body":    `[{"realm":"r","server":"s","name":"work"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if w := postSession(h, body); w.Code != http.StatusBadRequest {
				t.Fatalf("malformed request accepted with %d: %s", w.Code, body)
			}
		})
	}
}

// A browser must never be able to name the command or the working directory: the
// "unknown_field" case above proves the decoder rejects them outright, and this
// pins the reason so nobody relaxes DisallowUnknownFields for convenience.
func TestCreateSessionRejectsCommandAndDirectoryFields(t *testing.T) {
	socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control {
		return proto.Control{Type: "create_ok"}
	})
	h := createServer(t, socket)
	for _, body := range []string{
		`{"realm":"r","server":"s","name":"work","start_directory":"/etc"}`,
		`{"realm":"r","server":"s","name":"work","cwd":"/etc"}`,
		`{"realm":"r","server":"s","name":"work","columns":200,"rows":50}`,
	} {
		if w := postSession(h, body); w.Code != http.StatusBadRequest {
			t.Fatalf("client-supplied execution input accepted: %s", body)
		}
	}
}

func TestCreateSessionUnreachableBrokerIsNotARefusal(t *testing.T) {
	dir := shortTestDir(t)
	cfg := config.Front{
		Realms:           []config.Realm{{Name: "r", Socket: filepath.Join(dir, "absent.sock"), BrokerUID: uint32(os.Getuid()), BrokerUIDConfigured: true}},
		HandleTTLSeconds: 60,
		HandleCapacity:   8,
	}
	h := newServer(cfg, ".", "127.0.0.1:8080").handler()
	if w := postSession(h, `{"realm":"r","server":"s","name":"work"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable broker status = %d", w.Code)
	}
}

func TestCreateSessionRejectsUnexpectedBrokerReplies(t *testing.T) {
	for name, reply := range map[string]proto.Control{
		"wrong_type":      {Type: "inventory_ok"},
		"refusal_no_code": {Type: "create_refused"},
		"attach_response": {Type: "attach_ok"},
	} {
		t.Run(name, func(t *testing.T) {
			socket, _ := stubCreateBroker(t, func(proto.Control) proto.Control { return reply })
			if w := postSession(createServer(t, socket), `{"realm":"r","server":"s","name":"work"}`); w.Code != http.StatusServiceUnavailable {
				t.Fatalf("unexpected broker reply produced %d", w.Code)
			}
		})
	}
}
