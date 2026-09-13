package frontdoor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func shortTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ptf-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestOrderlyWebSocketCloseClassification(t *testing.T) {
	for _, code := range []int{websocket.CloseNormalClosure, websocket.CloseGoingAway} {
		if !orderlyWebSocketClose(&websocket.CloseError{Code: code, Text: "orderly"}) {
			t.Fatalf("close code %d was not orderly", code)
		}
	}
	for _, err := range []error{
		errors.New("read failed"),
		&websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: "lost"},
		&websocket.CloseError{Code: websocket.CloseProtocolError, Text: "OUT_OF_STATE"},
		&websocket.CloseError{Code: websocket.CloseInternalServerErr, Text: "RESOURCE_FAILURE"},
	} {
		if orderlyWebSocketClose(err) {
			t.Fatalf("abnormal error was classified orderly: %v", err)
		}
	}
}

type errorListener struct{ err error }

func (l errorListener) Accept() (net.Conn, error) { return nil, l.err }
func (errorListener) Close() error                { return nil }
func (errorListener) Addr() net.Addr              { return &net.UnixAddr{Name: "bounded-test", Net: "unix"} }

func TestValidateListenAddressLoopbackOnly(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8080", "127.9.8.7:8080", "[::1]:8080"} {
		t.Run("accept_"+strings.NewReplacer(":", "_", "[", "", "]", "").Replace(address), func(t *testing.T) {
			if err := validateListenAddress(address); err != nil {
				t.Fatalf("safe loopback address rejected: %v", err)
			}
		})
	}
	for _, address := range []string{"0.0.0.0:8080", "192.0.2.1:8080", "[::]:8080", ":8080", "localhost:8080", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:08080", "[0:0:0:0:0:0:0:1]:8080"} {
		t.Run("reject_"+strings.NewReplacer(":", "_", "[", "", "]", "").Replace(address), func(t *testing.T) {
			if err := validateListenAddress(address); err == nil {
				t.Fatalf("unsafe or ambiguous address %q accepted", address)
			}
		})
	}
}

func TestWrongBrokerUIDDeniedBeforeHello(t *testing.T) {
	dir := shortTestDir(t)
	path := filepath.Join(dir, "wrong-peer.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	traffic := make(chan int, 1)
	go func() {
		c, e := l.Accept()
		if e != nil {
			traffic <- -1
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		b := make([]byte, 1)
		n, _ := c.Read(b)
		traffic <- n
	}()
	if c, err := dial(path, uint32(os.Getuid())+1); err == nil {
		c.Close()
		t.Fatal("wrong broker uid accepted")
	}
	if n := <-traffic; n != 0 {
		t.Fatalf("front door sent %d bytes before peer authentication", n)
	}
}

func TestInventoryAggregatesRealmFailure(t *testing.T) {
	dir := shortTestDir(t)
	live := filepath.Join(dir, "live.sock")
	listener, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		c, e := listener.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		_, _ = proto.ReadFrame(c)
		_ = writeControl(c, proto.Control{Type: "hello_ok", V: 1})
		_, _ = proto.ReadFrame(c)
		a := proto.Authority{Realm: "live", Server: "main", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "test", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$0", SessionCreated: 3}
		_ = writeControl(c, proto.Control{Type: "inventory_ok", Servers: []proto.ServerInventory{{Label: "main", Status: "ok", Sessions: []proto.Session{{Authority: a, Name: "alpha", Width: 80, Height: 24}}}}})
	}()
	cfg := config.Front{Realms: []config.Realm{{Name: "live", DisplayName: "Primary user", Socket: live}, {Name: "dead", Socket: filepath.Join(dir, "dead.sock")}}, HandleTTLSeconds: 120, HandleCapacity: 10}
	s := newServer(cfg, ".", "127.0.0.1:8080")
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/inventory", nil))
	if rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("inventory Cache-Control = %q, want no-store", got)
	}
	var body struct {
		Realms []realmView `json:"realms"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Realms) != 2 || body.Realms[0].Error != "" || body.Realms[1].Error == "" || len(body.Realms[0].Servers[0].Sessions) != 1 {
		t.Fatalf("aggregation failed: %+v", body)
	}
	if body.Realms[0].DisplayName != "Primary user" || body.Realms[1].DisplayName != "dead" {
		t.Fatalf("display-name projection/fallback failed: %+v", body.Realms)
	}
}

func TestAttachmentHandleMintsOnlyFromExactServerBoundSource(t *testing.T) {
	source := strings.Repeat("A", 43)
	authority := auth(77, "$9")
	s := &Server{handles: newHandleStore(time.Minute, 8), bindings: newSourceBindingStore(time.Minute, 8)}
	release, err := s.bindings.bind("", source, authority)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	req := httptest.NewRequest(http.MethodPost, "/api/attachment-handles", strings.NewReader(`{"source":"`+source+`","purpose":"control"}`))
	rr := httptest.NewRecorder()
	s.attachmentHandle(rr, req)
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("source-bound mint status=%d headers=%v body=%q", rr.Code, rr.Header(), rr.Body.String())
	}
	var response struct {
		Handle string `json:"handle"`
	}
	if json.Unmarshal(rr.Body.Bytes(), &response) != nil || !sourceBindingRE.MatchString(response.Handle) {
		t.Fatalf("invalid handle response %q", rr.Body.String())
	}
	got, err := s.handles.consume(response.Handle, "", "control")
	if err != nil || authorityKey(got) != authorityKey(authority) {
		t.Fatalf("handle lost complete authority: %+v %v", got, err)
	}

	for _, body := range []string{
		`{"source":"` + strings.Repeat("B", 43) + `","purpose":"control"}`,
		`{"source":"` + source + `","purpose":"alias"}`,
		`{"source":"` + source + `","source":"` + source + `","purpose":"control"}`,
	} {
		req = httptest.NewRequest(http.MethodPost, "/api/attachment-handles", strings.NewReader(body))
		rr = httptest.NewRecorder()
		s.attachmentHandle(rr, req)
		if rr.Code == http.StatusOK {
			t.Fatalf("invalid source request minted a handle: %s", body)
		}
	}
}

func TestInventorySilentRealmIsBoundedAndOrderIndependent(t *testing.T) {
	for _, silentFirst := range []bool{false, true} {
		name := "healthy_first"
		if silentFirst {
			name = "silent_first"
		}
		t.Run(name, func(t *testing.T) {
			dir := shortTestDir(t)
			healthyPath := filepath.Join(dir, "healthy.sock")
			silentPath := filepath.Join(dir, "silent.sock")
			healthy, err := net.Listen("unix", healthyPath)
			if err != nil {
				t.Fatal(err)
			}
			defer healthy.Close()
			silent, err := net.Listen("unix", silentPath)
			if err != nil {
				t.Fatal(err)
			}
			defer silent.Close()
			go func() {
				c, err := healthy.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				_, _ = proto.ReadFrame(c)
				_ = writeControl(c, proto.Control{Type: "hello_ok", V: 1})
				_, _ = proto.ReadFrame(c)
				a := proto.Authority{Realm: "healthy", Server: "main", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "test", BootID: "boot", ServerPID: 10, ServerStart: 20, SessionID: "$0", SessionCreated: 30}
				_ = writeControl(c, proto.Control{Type: "inventory_ok", Servers: []proto.ServerInventory{{Label: "main", Status: "ok", Sessions: []proto.Session{{Authority: a, Name: "kept", Width: 80, Height: 24}}}}})
			}()
			go func() {
				c, err := silent.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				var one [1]byte
				_, _ = c.Read(one[:])
				time.Sleep(BrokerSetupTimeout + time.Second)
			}()
			realms := []config.Realm{{Name: "healthy", Socket: healthyPath}, {Name: "silent", Socket: silentPath}}
			if silentFirst {
				realms[0], realms[1] = realms[1], realms[0]
			}
			s := newServer(config.Front{Realms: realms, HandleTTLSeconds: 120, HandleCapacity: 10}, ".", "127.0.0.1:8080")
			rr := httptest.NewRecorder()
			started := time.Now()
			s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/inventory", nil))
			elapsed := time.Since(started)
			if elapsed > BrokerSetupTimeout+750*time.Millisecond {
				t.Fatalf("inventory exceeded setup bound: %s", elapsed)
			}
			var body struct {
				Realms []realmView `json:"realms"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Realms) != 2 || body.Realms[0].Name != realms[0].Name || body.Realms[1].Name != realms[1].Name {
				t.Fatalf("config order not preserved: %+v", body.Realms)
			}
			for _, realm := range body.Realms {
				if realm.Name == "healthy" && (realm.Error != "" || len(realm.Servers) != 1 || len(realm.Servers[0].Sessions) != 1 || realm.Servers[0].Sessions[0].Name != "kept") {
					t.Fatalf("healthy realm lost: %+v", realm)
				}
				if realm.Name == "silent" && (realm.Error == "" || len(realm.Servers) != 0) {
					t.Fatalf("silent realm not isolated: %+v", realm)
				}
			}
		})
	}
}

func TestFrontRejectsInvalidSubprotocolModeBeforeUpgrade(t *testing.T) {
	cfg := config.Front{Realms: []config.Realm{{Name: "r", Socket: "/tmp/missing"}}, HandleTTLSeconds: 1, HandleCapacity: 1}
	cfg.Ingress.PeerUIDConfigured = true
	s := newServer(cfg, ".", "127.0.0.1:8080")
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://localhost/ws", nil)
	r.Header.Set("Sec-WebSocket-Protocol", "persea-engine.unified-dev, persea-terminal.v1, persea-handle.x, persea-mode.write, persea-csrf.c, persea-history.5000")
	// Exercise the current wire grammar; query-string authority is rejected by
	// a separate gate before mode parsing and has its own refusal tests.
	s.terminal(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode status %d", rr.Code)
	}
}

func TestWebSocketReadLimitIsSetBeforeReadMessage(t *testing.T) {
	_, path, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(path), "server.go"))
	if err != nil {
		t.Fatal(err)
	}
	terminal := strings.Index(string(source), "func (s *Server) terminal")
	setLimit := strings.Index(string(source)[terminal:], "ws.SetReadLimit(proto.MaxAttachment)")
	startPump := strings.Index(string(source)[terminal:], "go wsReadPump(ws, reads, done)")
	if terminal < 0 || setLimit < 0 || startPump < 0 || setLimit > startPump {
		t.Fatalf("SetReadLimit must appear before the read pump starts (terminal=%d limit=%d pump=%d)", terminal, setLimit, startPump)
	}
}

func TestWebSocketOriginRequiresConfiguredLoopbackAuthority(t *testing.T) {
	check := checkOriginForListen("127.0.0.1:8080")
	for _, tc := range []struct {
		name   string
		host   string
		origin string
		want   bool
	}{
		{name: "configured authority", host: "127.0.0.1:8080", origin: "http://127.0.0.1:8080", want: true},
		{name: "non-browser configured authority", host: "127.0.0.1:8080", want: true},
		{name: "dns rebinding", host: "attacker.example:8080", origin: "http://attacker.example:8080"},
		{name: "mismatched origin", host: "127.0.0.1:8080", origin: "http://attacker.example:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/ws", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if got := check(r); got != tc.want {
				t.Fatalf("origin decision = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rr.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Fatalf("CSP missing %q: %q", directive, csp)
		}
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
	if got := rr.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", got)
	}
	if got := rr.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options = %q", got)
	}
	if got := rr.Header().Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Fatalf("Strict-Transport-Security = %q", got)
	}
	policy := rr.Header().Get("Permissions-Policy")
	for _, denial := range []string{"camera=()", "microphone=()", "geolocation=()", "accelerometer=()", "gyroscope=()", "magnetometer=()"} {
		if !strings.Contains(policy, denial) {
			t.Fatalf("Permissions-Policy missing %q: %q", denial, policy)
		}
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	for key := range rr.Header() {
		if strings.HasPrefix(http.CanonicalHeaderKey(key), "Access-Control-Allow-") {
			t.Fatalf("permissive CORS response header emitted: %s", key)
		}
	}
}

func writeTestStaticBundle(t *testing.T, index string) string {
	t.Helper()
	dir := shortTestDir(t)
	for _, name := range requiredBundleFiles {
		body := []byte("test")
		if name == "index.html" {
			body = []byte(index)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func documentNonce(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	csp := response.Header().Get("Content-Security-Policy")
	prefix := "style-src 'self' 'nonce-"
	start := strings.Index(csp, prefix)
	if start < 0 {
		t.Fatalf("document CSP has no style nonce: %q", csp)
	}
	start += len(prefix)
	end := strings.Index(csp[start:], "'")
	if end < 0 {
		t.Fatalf("document CSP nonce is unterminated: %q", csp)
	}
	nonce := csp[start : start+end]
	decoded, err := base64.RawStdEncoding.DecodeString(nonce)
	if err != nil || len(decoded) != 16 {
		t.Fatalf("document CSP nonce is not 128 bits: %q (%v)", nonce, err)
	}
	return nonce
}

func TestIndexBindsFreshStyleNonceToHeaderAndBody(t *testing.T) {
	staticDir := writeTestStaticBundle(t, `<html><head><meta name="persea-style-nonce" content="`+styleNoncePlaceholder+`"></head><body><script src="/app.js"></script></body></html>`)
	s := newServer(config.Front{HandleTTLSeconds: 1, HandleCapacity: 1}, staticDir, "127.0.0.1:8080")
	responses := make([]*httptest.ResponseRecorder, 0, 2)
	for _, path := range []string{"/", "/terminal"} {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %q", path, rr.Code, rr.Body.String())
		}
		responses = append(responses, rr)
	}
	first := documentNonce(t, responses[0])
	second := documentNonce(t, responses[1])
	if first == second {
		t.Fatal("independent HTML responses reused a style nonce")
	}
	for index, rr := range responses {
		nonce := []string{first, second}[index]
		body := rr.Body.String()
		if strings.Count(body, nonce) != 1 || strings.Contains(body, styleNoncePlaceholder) {
			t.Fatalf("response %d header/body nonce binding failed: %q", index, body)
		}
		if !strings.Contains(body, `name="persea-style-nonce" content="`+nonce+`"`) {
			t.Fatalf("response %d did not expose its nonce once to the app bundle", index)
		}
		csp := rr.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self';") || strings.Contains(csp, "script-src 'self' ") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Fatalf("response %d weakened script/style policy: %q", index, csp)
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response %d cache policy = %q", index, rr.Header().Get("Cache-Control"))
		}
	}
}

func TestIndexFailsClosedWhenStyleNoncePlaceholderIsMissingOrDuplicated(t *testing.T) {
	for _, index := range []string{
		`<html><body>missing</body></html>`,
		`<html>` + styleNoncePlaceholder + styleNoncePlaceholder + `</html>`,
	} {
		staticDir := writeTestStaticBundle(t, index)
		s := newServer(config.Front{HandleTTLSeconds: 1, HandleCapacity: 1}, staticDir, "127.0.0.1:8080")
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/", nil))
		if rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), styleNoncePlaceholder) {
			t.Fatalf("invalid index template did not fail closed: status=%d body=%q", rr.Code, rr.Body.String())
		}
	}
}

func TestTerminalFailureLogAndClientCodeAreBounded(t *testing.T) {
	var logs bytes.Buffer
	prior := frontLogf
	frontLogf = func(format string, args ...any) { _, _ = fmt.Fprintf(&logs, format, args...) }
	t.Cleanup(func() { frontLogf = prior })
	authority := &proto.Authority{Realm: "realm", Server: "server", SessionID: "$1"}
	code := (&Server{}).logTerminalFailure("secret input with spaces", authority)
	if code != "attachment_failed" {
		t.Fatalf("unbounded code was not canonicalized: %q", code)
	}
	got := logs.String()
	if !strings.Contains(got, `code="attachment_failed"`) || !strings.Contains(got, `realm="realm"`) {
		t.Fatalf("bounded terminal fields missing: %q", got)
	}
	if strings.Contains(got, "secret input with spaces") {
		t.Fatalf("unbounded payload leaked into log: %q", got)
	}
}

func TestRunPropagatesCleanupFailureAndLogsSocketStateWithoutValues(t *testing.T) {
	d := shortTestDir(t)
	static := filepath.Join(d, "static")
	if err := os.Mkdir(static, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range requiredBundleFiles {
		if err := os.WriteFile(filepath.Join(static, name), []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	priorDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(d); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(priorDir) }()
	serveErr := errors.New("forced serve failure")
	cleanupErr := errors.New("forced cleanup failure")
	priorListen := listenIngressForRun
	listenIngressForRun = func(config.Ingress) (net.Listener, func() error, error) {
		return errorListener{err: serveErr}, func() error { return cleanupErr }, nil
	}
	defer func() { listenIngressForRun = priorListen }()
	var logs bytes.Buffer
	priorLog := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(priorLog)
	cfg := config.Front{Ingress: config.Ingress{CanonicalHost: "localhost:43210"}, AliasStorePath: filepath.Join(d, "aliases.json"), HandleTTLSeconds: 1, HandleCapacity: 1}
	err = Run(cfg, "static")
	if !errors.Is(err, serveErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("Run error = %v", err)
	}
	if got := logs.String(); !strings.Contains(got, `reason="socket_state"`) || strings.Contains(got, "peer_uid=") {
		t.Fatalf("socket-state log = %q", got)
	}
}

func TestBindUnifiedSessionCarriesOnlyClosedRotationDeferralDetail(t *testing.T) {
	valid := bindUnifiedSession(&proto.UnifiedSessionState{
		State: proto.UnifiedSessionOpen, Origin: proto.UnifiedOriginBirth,
		Detail: proto.UnifiedSessionDetailRotationDeferredAltScreen,
	})
	if valid == nil || valid.Detail != proto.UnifiedSessionDetailRotationDeferredAltScreen {
		t.Fatalf("valid deferred projection=%+v", valid)
	}
	for _, state := range []*proto.UnifiedSessionState{
		{State: proto.UnifiedSessionOpen, Origin: proto.UnifiedOriginBirth, Detail: "unknown"},
		{State: proto.UnifiedSessionAdoptable, Detail: proto.UnifiedSessionDetailRotationDeferredAltScreen},
	} {
		if got := bindUnifiedSession(state); got != nil {
			t.Fatalf("misplaced/unknown detail escaped closed projection: %+v", got)
		}
	}
}
