package frontdoor

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
	"strings"
	"testing"
	"time"
)

var testCSRF = base64.RawURLEncoding.EncodeToString(make([]byte, 32))

func securedMethod(t *testing.T, method string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	cfg := config.Front{Ingress: config.Ingress{SocketPath: "/tmp/front.sock", PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true, CanonicalHost: "localhost:43210", OperatorLogin: "operator@example.com", MaxConnections: 64}}
	s := &Server{cfg: cfg}
	h := s.secure(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	r := httptest.NewRequest(method, "http://localhost/api/inventory", nil)
	r.Host = "localhost"
	r.Header.Set("X-Forwarded-Host", cfg.Ingress.CanonicalHost)
	r.Header.Set("X-Forwarded-Proto", strings.TrimSuffix(externalOrigin(cfg.Ingress), "://"+cfg.Ingress.CanonicalHost))
	r.Header.Set("Tailscale-User-Login", cfg.Ingress.OperatorLogin)
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set("Origin", externalOrigin(cfg.Ingress))
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Cookie", csrfCookie+"="+testCSRF)
		r.Header.Set("X-Persea-CSRF", testCSRF)
	}
	r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: cfg.Ingress.PeerUID}))
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func securedRequest(t *testing.T, mutate func(*http.Request)) *httptest.ResponseRecorder {
	return securedMethod(t, http.MethodGet, mutate)
}
func TestSecurityAuthorityOrderingAndOrigin(t *testing.T) {
	if w := securedRequest(t, nil); w.Code != 204 {
		t.Fatalf("positive=%d", w.Code)
	}
	if w := securedRequest(t, func(r *http.Request) { *r = *r.WithContext(context.Background()) }); w.Code == 204 {
		t.Fatal("untrusted connection accepted")
	}
	if w := securedRequest(t, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); w.Code == 204 {
		t.Fatal("cross origin accepted")
	}
}

func TestUnifiedDevDocumentCSPCapabilityRequiresExactTerminalSelector(t *testing.T) {
	staticDir := writeTestStaticBundle(t, `<html><head><meta name="persea-style-nonce" content="`+styleNoncePlaceholder+`"></head><body><script src="/app.js"></script></body></html>`)
	s := newServer(config.Front{HandleTTLSeconds: 1, HandleCapacity: 1}, staticDir, "127.0.0.1:8080")
	const attributeCapability = "style-src-attr 'unsafe-inline'"
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{name: "exact", path: "/terminal?engine=unified-dev", want: true},
		{name: "missing", path: "/terminal"},
		{name: "wrong value", path: "/terminal?engine=legacy"},
		{name: "duplicate", path: "/terminal?engine=unified-dev&engine=unified-dev"},
		{name: "additional", path: "/terminal?engine=unified-dev&extra=1"},
		{name: "encoded", path: "/terminal?engine=unified%2Ddev"},
		{name: "root document", path: "/?engine=unified-dev"},
		{name: "index document", path: "/index.html?engine=unified-dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+tc.path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
			}
			nonce := documentNonce(t, rr)
			wantCSP := strings.Replace(baseContentSecurityPolicy, "style-src 'self'", "style-src 'self' 'nonce-"+nonce+"'", 1)
			if tc.want {
				wantCSP += "; " + attributeCapability
			}
			if got := rr.Header().Get("Content-Security-Policy"); got != wantCSP {
				t.Fatalf("CSP=%q want=%q", got, wantCSP)
			}
			body := rr.Body.String()
			if strings.Count(body, nonce) != 1 || strings.Contains(body, styleNoncePlaceholder) {
				t.Fatalf("nonce binding failed: %q", body)
			}
			wantCount := 0
			if tc.want {
				wantCount = 1
			}
			if csp := rr.Header().Get("Content-Security-Policy"); strings.Count(csp, attributeCapability) != wantCount || !strings.Contains(csp, "script-src 'self';") || strings.Contains(csp, "unsafe-eval") {
				t.Fatalf("capability/script policy mismatch: %q", csp)
			}
		})
	}
}

func TestFaviconIsClosedNoContentResource(t *testing.T) {
	staticDir := writeTestStaticBundle(t, `<html><head><meta name="persea-style-nonce" content="`+styleNoncePlaceholder+`"></head></html>`)
	s := newServer(config.Front{HandleTTLSeconds: 1, HandleCapacity: 1}, staticDir, "127.0.0.1:8080")

	get := httptest.NewRecorder()
	s.handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/favicon.ico", nil))
	if get.Code != http.StatusNoContent || get.Body.Len() != 0 {
		t.Fatalf("GET favicon status=%d body=%q", get.Code, get.Body.String())
	}
	if got := get.Header().Get("Content-Security-Policy"); got != baseContentSecurityPolicy {
		t.Fatalf("GET favicon CSP=%q want=%q", got, baseContentSecurityPolicy)
	}
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
	} {
		if got := get.Header().Get(name); got != want {
			t.Fatalf("GET favicon %s=%q want=%q", name, got, want)
		}
	}

	post := httptest.NewRecorder()
	s.handler().ServeHTTP(post, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/favicon.ico", nil))
	if post.Code != http.StatusMethodNotAllowed || post.Body.Len() == 0 {
		t.Fatalf("POST favicon status=%d body=%q", post.Code, post.Body.String())
	}
}

func TestProductionHTTPRedirectIsExactAndFailClosed(t *testing.T) {
	cfg := config.Front{Ingress: config.Ingress{
		SocketPath: "/run/persea-terminal/front.sock", PeerUID: 0, PeerUIDConfigured: true,
		CanonicalHost: "terminal.example.test", OperatorLogin: "operator@example.test", MaxConnections: 64,
	}}
	s := &Server{cfg: cfg}
	called := false
	h := s.secure(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	request := func(method, target string, mutate func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://localhost"+target, nil)
		r.Host = "localhost"
		r.Header.Set("X-Forwarded-Host", cfg.Ingress.CanonicalHost)
		r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: 0}))
		if mutate != nil {
			mutate(r)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		called = false
		w := request(method, "/terminal?history=5000", nil)
		if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != "https://terminal.example.test/terminal?history=5000" || called {
			t.Fatalf("%s redirect = status %d location %q downstream=%v", method, w.Code, w.Header().Get("Location"), called)
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Frame-Options") != "DENY" {
			t.Fatalf("%s redirect security headers missing: %v", method, w.Header())
		}
	}
	called = false
	https := request(http.MethodGet, "/", func(r *http.Request) {
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("Tailscale-User-Login", cfg.Ingress.OperatorLogin)
	})
	if https.Code != http.StatusNoContent || !called {
		t.Fatalf("authenticated HTTPS = status %d downstream=%v", https.Code, called)
	}
	for name, tc := range map[string]struct {
		method, target string
		mutate         func(*http.Request)
	}{
		"post":              {http.MethodPost, "/", nil},
		"websocket_path":    {http.MethodGet, "/ws", nil},
		"upgrade_header":    {http.MethodGet, "/", func(r *http.Request) { r.Header.Set("Upgrade", "websocket") }},
		"explicit_http":     {http.MethodGet, "/", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") }},
		"duplicate_proto":   {http.MethodGet, "/", func(r *http.Request) { r.Header["X-Forwarded-Proto"] = []string{"https", "https"} }},
		"comma_proto":       {http.MethodGet, "/", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https,https") }},
		"untrusted_context": {http.MethodGet, "/", func(r *http.Request) { *r = *r.WithContext(context.Background()) }},
	} {
		t.Run(name, func(t *testing.T) {
			called = false
			w := request(tc.method, tc.target, tc.mutate)
			if w.Code != http.StatusForbidden || called || w.Header().Get("Location") != "" {
				t.Fatalf("unsafe redirect = status %d location %q downstream=%v", w.Code, w.Header().Get("Location"), called)
			}
		})
	}
}
func TestSecurityRejectsVacuousCSRF(t *testing.T) {
	if w := securedMethod(t, http.MethodPost, nil); w.Code != 204 {
		t.Fatalf("positive=%d", w.Code)
	}
	for name, mutate := range map[string]func(*http.Request){
		"missing_header": func(r *http.Request) { r.Header.Del("X-Persea-CSRF") },
		"mismatch":       func(r *http.Request) { r.Header.Set("X-Persea-CSRF", "B"+strings.Repeat("A", 42)) },
		"missing_origin": func(r *http.Request) { r.Header.Del("Origin") },
		"missing_fetch":  func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") },
		"cross_site":     func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
	} {
		t.Run(name, func(t *testing.T) {
			if w := securedMethod(t, http.MethodPost, mutate); w.Code == 204 {
				t.Fatal("mutation accepted")
			}
		})
	}
}

func TestSecurityHeaderIdentityAndFunnelMatrix(t *testing.T) {
	for name, mutate := range map[string]func(*http.Request){
		"host":            func(r *http.Request) { r.Host = "evil" },
		"forwarded_host":  func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "evil") },
		"forwarded_proto": func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") },
		"operator":        func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "other@example.test") },
		"duplicate_login": func(r *http.Request) {
			r.Header["Tailscale-User-Login"] = []string{"operator@example.com", "operator@example.com"}
		},
		"comma_login": func(r *http.Request) {
			r.Header.Set("Tailscale-User-Login", "operator@example.com,operator@example.com")
		},
		"optional_duplicate": func(r *http.Request) {
			r.Header["Tailscale-User-Name"] = []string{"one", "two"}
		},
		"funnel":            func(r *http.Request) { r.Header.Set("Tailscale-Funnel-Request", "1") },
		"unexpected_header": func(r *http.Request) { r.Header.Set("Tailscale-App-Capabilities", "marker") },
	} {
		t.Run(name, func(t *testing.T) {
			if w := securedRequest(t, mutate); w.Code == 204 {
				t.Fatal("authority mutation accepted")
			}
		})
	}
}

func TestCapabilityConsumeIsSingleUse(t *testing.T) {
	store := newHandleStore(time.Minute, 4)
	handle, err := store.mint(auth(11, "$0"), "operator@example.com", "control")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.consume(handle, "operator@example.com", "control"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.consume(handle, "operator@example.com", "control"); err == nil {
		t.Fatal("consumed capability replayed")
	}
}

func TestSnapshotRejectsQueryCapability(t *testing.T) {
	var logs bytes.Buffer
	prior := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prior)
	s := &Server{cfg: config.Front{Ingress: config.Ingress{PeerUIDConfigured: true}}}
	marker := "SECRET_QUERY_HANDLE_MARKER"
	r := httptest.NewRequest(http.MethodGet, "http://localhost/api/snapshot?handle="+marker, nil)
	w := httptest.NewRecorder()
	s.snapshot(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("query capability status=%d", w.Code)
	}
	if got := logs.String(); !strings.Contains(got, `reason="capability"`) || strings.Contains(got, marker) {
		t.Fatalf("capability denial log = %q", got)
	}
}

func TestCapabilityReplayDenialIsBounded(t *testing.T) {
	var logs bytes.Buffer
	prior := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prior)
	marker := "SECRET_REPLAY_HANDLE_MARKER"
	cfg := config.Front{Ingress: config.Ingress{SocketPath: "/tmp/front.sock", PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true, CanonicalHost: "localhost:43210", OperatorLogin: "operator@example.com"}}
	s := &Server{cfg: cfg, handles: newHandleStore(time.Minute, 4)}
	r := httptest.NewRequest(http.MethodGet, "http://localhost/ws", nil)
	r.Header.Set("Sec-WebSocket-Protocol", "persea-engine.unified-dev, persea-terminal.v1, persea-handle."+marker+", persea-mode.observe, persea-csrf."+testCSRF+", persea-history.5000")
	r.Header.Set("Cookie", csrfCookie+"="+testCSRF)
	r.Header.Set("Origin", "http://localhost:43210")
	r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: cfg.Ingress.PeerUID, operator: cfg.Ingress.OperatorLogin, origin: "http://localhost:43210"}))
	w := httptest.NewRecorder()
	s.terminal(w, r)
	if w.Code != http.StatusGone {
		t.Fatalf("replay status=%d", w.Code)
	}
	if got := logs.String(); !strings.Contains(got, `reason="capability_replay"`) || strings.Contains(got, marker) {
		t.Fatalf("replay denial log = %q", got)
	}
}

func TestUnifiedDevWebSocketSelectorIsClosedAndUnique(t *testing.T) {
	base := "persea-terminal.v1, persea-handle.h, persea-mode.control, persea-csrf.c, persea-history.5000"
	request := httptest.NewRequest(http.MethodGet, "http://localhost/ws", nil)
	request.Header.Set("Sec-WebSocket-Protocol", base+", persea-engine.unified-dev")
	authority, ok := parseWSAuthority(request)
	if !ok || authority.engine != "unified-dev" {
		t.Fatalf("unified selector = %+v, %v", authority, ok)
	}
	for _, suffix := range []string{", persea-engine.", ", persea-engine.other", ", persea-engine.unified-dev, persea-engine.unified-dev"} {
		invalid := httptest.NewRequest(http.MethodGet, "http://localhost/ws", nil)
		invalid.Header.Set("Sec-WebSocket-Protocol", base+suffix)
		if _, ok := parseWSAuthority(invalid); ok {
			t.Fatalf("invalid unified selector accepted: %s", suffix)
		}
	}
	legacy := httptest.NewRequest(http.MethodGet, "http://localhost/ws", nil)
	legacy.Header.Set("Sec-WebSocket-Protocol", base)
	legacyAuthority, ok := parseWSAuthority(legacy)
	if ok {
		t.Fatalf("missing engine accepted: %+v, %v", legacyAuthority, ok)
	}
}

func TestAuthenticatedAPIResponseHeaders(t *testing.T) {
	cfg := config.Front{Ingress: config.Ingress{SocketPath: "/tmp/front.sock", PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true, CanonicalHost: "localhost:43210", OperatorLogin: "operator@example.com", MaxConnections: 64}}
	aliases, err := newAliasStore(filepath.Join(shortTestDir(t), "aliases.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: cfg, handles: newHandleStore(time.Minute, 4), aliases: aliases}
	r := httptest.NewRequest(http.MethodGet, "http://localhost/api/inventory", nil)
	r.Host = "localhost"
	r.Header.Set("X-Forwarded-Host", cfg.Ingress.CanonicalHost)
	r.Header.Set("X-Forwarded-Proto", strings.TrimSuffix(externalOrigin(cfg.Ingress), "://"+cfg.Ingress.CanonicalHost))
	r.Header.Set("Tailscale-User-Login", cfg.Ingress.OperatorLogin)
	r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: cfg.Ingress.PeerUID}))
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated API status=%d", w.Code)
	}
	wants := map[string]string{
		"X-Frame-Options":           "DENY",
		"X-Content-Type-Options":    "nosniff",
		"Referrer-Policy":           "no-referrer",
		"Strict-Transport-Security": "max-age=31536000",
		"Cache-Control":             "no-store",
	}
	for key, want := range wants {
		if got := w.Header().Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if policy := w.Header().Get("Permissions-Policy"); !strings.Contains(policy, "camera=()") || !strings.Contains(policy, "microphone=()") || !strings.Contains(policy, "geolocation=()") {
		t.Fatalf("Permissions-Policy = %q", policy)
	}
	for key := range w.Header() {
		if strings.HasPrefix(http.CanonicalHeaderKey(key), "Access-Control-Allow-") {
			t.Fatalf("CORS response header emitted: %s", key)
		}
	}
}

func TestRateLimitReasonIsExercised(t *testing.T) {
	var logs bytes.Buffer
	prior := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prior)
	cfg := config.Front{Ingress: config.Ingress{PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true, CanonicalHost: "localhost:43210", OperatorLogin: "operator@example.com"}}
	s := &Server{cfg: cfg, limiter: operatorLimiter{tokens: 0, at: time.Now()}}
	h := s.secure(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	r := httptest.NewRequest(http.MethodGet, "http://localhost/api/inventory", nil)
	r.Host = "localhost"
	r.Header.Set("X-Forwarded-Host", cfg.Ingress.CanonicalHost)
	r.Header.Set("X-Forwarded-Proto", strings.TrimSuffix(externalOrigin(cfg.Ingress), "://"+cfg.Ingress.CanonicalHost))
	r.Header.Set("Tailscale-User-Login", cfg.Ingress.OperatorLogin)
	r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: cfg.Ingress.PeerUID}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	// 429, not 403: budget exhaustion is not an authorization failure, and a shared
	// status makes the two indistinguishable in logs during an incident.
	if w.Code != http.StatusTooManyRequests || !strings.Contains(logs.String(), `reason="rate_limit"`) {
		t.Fatalf("rate-limit status=%d log=%q", w.Code, logs.String())
	}
}

func TestIngressDenialLogDoesNotContainAuthorityMaterial(t *testing.T) {
	var output bytes.Buffer
	prior := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(prior)
	markers := []string{"DO_NOT_LOG_LOGIN_MARKER", "DO_NOT_LOG_COOKIE_MARKER", "DO_NOT_LOG_CSRF_MARKER", "DO_NOT_LOG_INPUT_MARKER"}
	_ = securedRequest(t, func(r *http.Request) { r.Header.Set("Tailscale-User-Login", markers[0]) })
	_ = securedMethod(t, http.MethodPost, func(r *http.Request) {
		r.Header.Set("Cookie", csrfCookie+"="+markers[1])
		r.Header.Set("X-Persea-CSRF", markers[2])
	})
	_ = securedRequest(t, func(r *http.Request) {
		r.Header.Set("X-Terminal-Input", markers[3])
		r.Header.Set("Origin", "https://denied.example")
	})
	for _, marker := range markers {
		if strings.Contains(output.String(), marker) {
			t.Fatalf("denial log leaked marker %q", marker)
		}
	}
}
func TestWSAuthorityStrict(t *testing.T) {
	r := httptest.NewRequest("GET", "http://localhost/ws", nil)
	r.Header.Set("Sec-WebSocket-Protocol", "persea-engine.unified-dev, persea-terminal.v1, persea-handle.h, persea-mode.observe, persea-csrf.c, persea-history.5000")
	a, ok := parseWSAuthority(r)
	if !ok || a.handle != "h" {
		t.Fatal("valid protocols rejected")
	}
	r.Header.Set("Sec-WebSocket-Protocol", "persea-engine.unified-dev, persea-terminal.v1, persea-handle.h, persea-mode.observe, persea-csrf.c, persea-history.5000, evil")
	if _, ok := parseWSAuthority(r); ok {
		t.Fatal("unknown protocol accepted")
	}
	r.Header.Set("Sec-WebSocket-Protocol", "persea-engine.unified-dev, persea-terminal.v1, persea-handle.h, persea-handle.other, persea-mode.observe, persea-csrf.c, persea-history.5000")
	if _, ok := parseWSAuthority(r); ok {
		t.Fatal("duplicate handle protocol accepted")
	}
}

func TestWSAuthorityCategoriesArePresentExactlyOnce(t *testing.T) {
	type category struct {
		name      string
		valid     string
		alternate string
		empty     string
	}
	categories := []category{
		{name: "engine", valid: "persea-engine.unified-dev", alternate: "persea-engine.other", empty: "persea-engine."},
		{name: "version", valid: "persea-terminal.v1", alternate: "persea-terminal.v2", empty: "persea-terminal."},
		{name: "handle", valid: "persea-handle.h", alternate: "persea-handle.other", empty: "persea-handle."},
		{name: "mode", valid: "persea-mode.observe", alternate: "persea-mode.control", empty: "persea-mode."},
		{name: "csrf", valid: "persea-csrf.c", alternate: "persea-csrf.other", empty: "persea-csrf."},
		{name: "history", valid: "persea-history.5000", alternate: "persea-history.7500", empty: "persea-history."},
	}
	baseline := make([]string, len(categories))
	for i, item := range categories {
		baseline[i] = item.valid
	}
	request := func(protocols []string) bool {
		r := httptest.NewRequest(http.MethodGet, "http://localhost/ws", nil)
		r.Header.Set("Sec-WebSocket-Protocol", strings.Join(protocols, ", "))
		_, ok := parseWSAuthority(r)
		return ok
	}
	if !request(baseline) {
		t.Fatal("valid exact-one category baseline rejected")
	}
	for index, item := range categories {
		t.Run(item.name+"/missing", func(t *testing.T) {
			protocols := append([]string(nil), baseline[:index]...)
			protocols = append(protocols, baseline[index+1:]...)
			if request(protocols) {
				t.Fatal("missing category accepted")
			}
		})
		forms := [][]string{
			{item.valid, item.valid},
			{item.valid, item.alternate},
			{item.alternate, item.valid},
			{item.empty, item.valid},
			{item.valid, item.empty},
			{item.empty, item.empty},
		}
		for formIndex, form := range forms {
			t.Run(fmt.Sprintf("%s/duplicate-%d", item.name, formIndex), func(t *testing.T) {
				protocols := append([]string(nil), baseline[:index]...)
				protocols = append(protocols, form...)
				protocols = append(protocols, baseline[index+1:]...)
				if request(protocols) {
					t.Fatalf("duplicate category accepted: %q", protocols)
				}
			})
		}
	}
}

func TestWSAuthorityDuplicateRejectsBeforeHandleConsumptionAndUpgrade(t *testing.T) {
	store := newHandleStore(time.Minute, 4)
	handle, err := store.mint(proto.Authority{}, "operator@example.com", "observe")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: config.Front{Ingress: config.Ingress{PeerUIDConfigured: true}}, handles: store}
	r := httptest.NewRequest(http.MethodGet, "http://localhost/ws", nil)
	r.Header.Set("Sec-WebSocket-Protocol", "persea-engine.unified-dev, persea-terminal.v1, persea-handle., persea-handle."+handle+", persea-mode.observe, persea-csrf.c, persea-history.5000")
	w := httptest.NewRecorder()
	s.terminal(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate category reached upgrade path: status=%d", w.Code)
	}
	if _, err := store.consume(handle, "operator@example.com", "observe"); err != nil {
		t.Fatalf("duplicate category consumed handle before denial: %v", err)
	}
}

func TestWSHistoryProtocolExactChoiceAndNegativeMatrix(t *testing.T) {
	base := "persea-engine.unified-dev, persea-terminal.v1, persea-handle.h, persea-mode.observe, persea-csrf.c"
	for _, want := range []int{0, 500, 1_000, 2_000, 5_000, 7_500, 10_000} {
		r := httptest.NewRequest("GET", "http://localhost/ws", nil)
		r.Header.Set("Sec-WebSocket-Protocol", fmt.Sprintf("%s, persea-history.%d", base, want))
		got, ok := parseWSAuthority(r)
		if !ok || got.history != want {
			t.Fatalf("history %d parsed as %+v ok=%v", want, got, ok)
		}
	}
	invalid := []string{
		base,
		base + ", persea-history.500, persea-history.7500",
		base + ", persea-history.unknown",
		base + ", persea-history.-1",
		base + ", persea-history.1",
		base + ", persea-history.0500",
		base + ", persea-history.500.0",
		base + ", persea-history. 500",
		base + ", persea-history.+500",
		base + ", persea-history.10001",
	}
	for _, protocols := range invalid {
		r := httptest.NewRequest("GET", "http://localhost/ws", nil)
		r.Header.Set("Sec-WebSocket-Protocol", protocols)
		if _, ok := parseWSAuthority(r); ok {
			t.Fatalf("invalid history protocols accepted: %q", protocols)
		}
	}
}

// The __Host- prefix is only honoured on a cookie that carries Secure. Making
// Secure conditional does not relax the cookie, it makes the browser discard it,
// and every unsafe method then fails CSRF. This was shipped conditional once.
func TestCSRFCookieIsAlwaysSecureIncludingHermetic(t *testing.T) {
	for name, ingress := range map[string]config.Ingress{
		"hermetic":   {PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true, CanonicalHost: "localhost:43210", OperatorLogin: "operator@example.com", SocketPath: "/tmp/hermetic.sock"},
		"production": {PeerUID: 0, PeerUIDConfigured: true, CanonicalHost: "terminal.example.ts.net", OperatorLogin: "operator@example.com", SocketPath: config.ProductionSocketPath},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{cfg: config.Front{Ingress: ingress}}
			h := s.secure(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			r := httptest.NewRequest(http.MethodGet, "http://localhost/api/inventory", nil)
			r.Host = "localhost"
			r.Header.Set("X-Forwarded-Host", ingress.CanonicalHost)
			r.Header.Set("X-Forwarded-Proto", strings.TrimPrefix(strings.TrimSuffix(externalOrigin(ingress), "://"+ingress.CanonicalHost), ""))
			r.Header.Set("Tailscale-User-Login", ingress.OperatorLogin)
			r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: ingress.PeerUID}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var csrf *http.Cookie
			for _, c := range w.Result().Cookies() {
				if c.Name == csrfCookie {
					csrf = c
				}
			}
			if csrf == nil {
				t.Fatalf("no CSRF cookie issued (status %d)", w.Code)
			}
			if !csrf.Secure {
				t.Fatal("__Host- CSRF cookie lacks Secure; browsers will reject it entirely")
			}
			if csrf.Path != "/" || csrf.Domain != "" {
				t.Fatalf("__Host- prefix requires Path=/ and no Domain: path=%q domain=%q", csrf.Path, csrf.Domain)
			}
		})
	}
}

// hermeticIngress builds the loopback ingress shape this process would be
// validated with, optionally declaring the TLS-terminating adapter.
func hermeticIngress(tls bool) config.Ingress {
	return config.Ingress{SocketPath: "/tmp/front.sock", PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true, CanonicalHost: "localhost:43210", OperatorLogin: "operator@example.com", MaxConnections: 64, HermeticTLS: tls}
}

func TestHermeticTLSChangesOnlyTheHermeticOrigin(t *testing.T) {
	if externalOrigin(hermeticIngress(false)) != "http://localhost:43210" {
		t.Fatalf("plaintext hermetic origin changed: %q", externalOrigin(hermeticIngress(false)))
	}
	if got := externalOrigin(hermeticIngress(true)); got != "https://localhost:43210" {
		t.Fatalf("hermetic TLS origin = %q", got)
	}
	// A production ingress is unaffected in both directions: it is already
	// https, and it can never be configured with the option at all.
	production := config.Ingress{SocketPath: config.ProductionSocketPath, PeerUID: 0, PeerUIDConfigured: true, CanonicalHost: "terminal.example.ts.net", OperatorLogin: "operator@example.com", MaxConnections: 64}
	if got := externalOrigin(production); got != "https://terminal.example.ts.net" {
		t.Fatalf("production origin = %q", got)
	}
	withOption := production
	withOption.HermeticTLS = true
	if got := externalOrigin(withOption); got != "https://terminal.example.ts.net" {
		t.Fatalf("production origin with the option set = %q", got)
	}
}

func TestHermeticTLSRequiresMatchingForwardedProto(t *testing.T) {
	request := func(ingress config.Ingress, method, proto string) *httptest.ResponseRecorder {
		s := &Server{cfg: config.Front{Ingress: ingress}}
		called := false
		h := s.secure(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) }))
		r := httptest.NewRequest(method, "http://localhost/api/inventory", nil)
		r.Host = "localhost"
		r.Header.Set("X-Forwarded-Host", ingress.CanonicalHost)
		r.Header.Set("X-Forwarded-Proto", proto)
		r.Header.Set("Tailscale-User-Login", ingress.OperatorLogin)
		if method != http.MethodGet && method != http.MethodHead {
			r.Header.Set("Origin", externalOrigin(ingress))
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			r.Header.Set("Cookie", csrfCookie+"="+testCSRF)
			r.Header.Set("X-Persea-CSRF", testCSRF)
		}
		r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: ingress.PeerUID}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == 204 && !called {
			t.Fatal("success without reaching the handler")
		}
		return w
	}
	for _, tc := range []struct {
		name  string
		tls   bool
		proto string
		want  int
	}{
		{"plaintext accepts http", false, "http", 204},
		{"plaintext refuses https", false, "https", http.StatusForbidden},
		{"tls accepts https", true, "https", 204},
		{"tls refuses http", true, "http", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ingress := hermeticIngress(tc.tls)
			if w := request(ingress, http.MethodGet, tc.proto); w.Code != tc.want {
				t.Fatalf("GET %s = %d, want %d", tc.proto, w.Code, tc.want)
			}
			if tc.want != 204 {
				return
			}
			// The Secure CSRF cookie is issued unchanged; only the origin the
			// browser sees it on differs, which is the whole point of the option.
			cookies := (&http.Response{Header: request(ingress, http.MethodGet, tc.proto).Result().Header}).Cookies()
			issued := 0
			for _, c := range cookies {
				if c.Name == csrfCookie {
					issued++
					if !c.Secure || c.Path != "/" || c.SameSite != http.SameSiteStrictMode {
						t.Fatalf("CSRF cookie attributes changed: %+v", c)
					}
				}
			}
			if issued != 1 {
				t.Fatalf("CSRF cookies issued = %d", issued)
			}
			if w := request(ingress, http.MethodPost, tc.proto); w.Code != 204 {
				t.Fatalf("POST over the matching proto = %d", w.Code)
			}
		})
	}
}
