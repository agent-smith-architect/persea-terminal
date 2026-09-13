package frontdoor

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"persea-terminal/internal/config"
)

// Shared fixtures for the ergonomics routes (preferences, snippets). Requests
// travel through the full handler chain including the secure wrapper, so a
// red case that the wrapper refuses (identity, CSRF) is measured at the same
// place production measures it.

var ergoCSRF = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x3c}, 32))

func ergoFrontConfig(t *testing.T) config.Front {
	t.Helper()
	dir := shortTestDir(t)
	return config.Front{
		Ingress: config.Ingress{
			SocketPath: "/tmp/persea-front-ergo.sock", PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true,
			CanonicalHost: "localhost:43210", OperatorLogin: "operator@example.com", MaxConnections: 64,
		},
		Realms:               []config.Realm{{Name: "r", Socket: dir + "/absent.sock", BrokerUID: uint32(os.Geteuid()), BrokerUIDConfigured: true}},
		PreferencesStorePath: dir + "/preferences.json",
		SnippetStorePath:     dir + "/snippets.json",
		HandleTTLSeconds:     120,
		HandleCapacity:       64,
	}
}

// ergoRequest refills the shared operator limiter first so a contract test
// with many red cases measures the route, not the budget; the limiter test
// uses ergoRequestMetered to measure the budget itself.
func ergoRequest(t *testing.T, server *Server, method, target, mediaType string, body io.Reader, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	server.limiter.mu.Lock()
	server.limiter.at = time.Time{}
	server.limiter.mu.Unlock()
	return ergoRequestMetered(t, server, method, target, mediaType, body, mutate)
}

func ergoRequestMetered(t *testing.T, server *Server, method, target, mediaType string, body io.Reader, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, body)
	r.Host = "localhost"
	r.Header.Set("X-Forwarded-Host", server.cfg.Ingress.CanonicalHost)
	r.Header.Set("X-Forwarded-Proto", "http")
	r.Header.Set("Tailscale-User-Login", server.cfg.Ingress.OperatorLogin)
	r.Header.Set("Origin", externalOrigin(server.cfg.Ingress))
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Cookie", csrfCookie+"="+ergoCSRF)
	r.Header.Set("X-Persea-CSRF", ergoCSRF)
	if mediaType != "" {
		r.Header.Set("Content-Type", mediaType)
	}
	r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: server.cfg.Ingress.PeerUID}))
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	server.handler().ServeHTTP(w, r)
	return w
}

// ergoRedCases are the M4 clauses every ergonomics route must refuse the same
// way, expressed as request mutations and the status they must produce.
type ergoRedCase struct {
	name   string
	status int
	mutate func(*http.Request)
}

func ergoIdentityRedCases(cfg config.Front) []ergoRedCase {
	return []ergoRedCase{
		{"missing_identity", http.StatusForbidden, func(r *http.Request) { *r = *r.WithContext(context.Background()) }},
		{"wrong_uid", http.StatusForbidden, func(r *http.Request) {
			*r = *r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: cfg.Ingress.PeerUID + 1}))
		}},
		{"wrong_operator", http.StatusForbidden, func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "other@example.com") }},
	}
}

func ergoMutationRedCases(cfg config.Front) []ergoRedCase {
	return append(ergoIdentityRedCases(cfg),
		ergoRedCase{"missing_csrf", http.StatusForbidden, func(r *http.Request) { r.Header.Del("X-Persea-CSRF") }},
		ergoRedCase{"wrong_csrf", http.StatusForbidden, func(r *http.Request) { r.Header.Set("X-Persea-CSRF", ergoCSRF[1:]+"A") }},
		ergoRedCase{"cross_origin", http.StatusForbidden, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }},
		ergoRedCase{"wrong_content_type", http.StatusBadRequest, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
		ergoRedCase{"content_type_parameter", http.StatusBadRequest, func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") }},
		ergoRedCase{"query_string", http.StatusBadRequest, func(r *http.Request) { r.URL.RawQuery = "x=1" }},
	)
}

// assertBoundedRefusal checks the response half of M4: the fixed status, the
// no-store header, and an error body that carries none of the request bytes.
func assertBoundedRefusal(t *testing.T, name string, w *httptest.ResponseRecorder, status int, secrets ...string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("%s: status=%d want %d body=%q", name, w.Code, status, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%s: Cache-Control=%q", name, w.Header().Get("Cache-Control"))
	}
	if w.Body.Len() > 256 {
		t.Fatalf("%s: unbounded error body (%d bytes)", name, w.Body.Len())
	}
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(w.Body.Bytes(), []byte(secret)) {
			t.Fatalf("%s: error body echoes request bytes: %q", name, w.Body.String())
		}
	}
}
