package frontdoor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"persea-terminal/internal/config"
	"strings"
	"testing"
	"time"
)

// M9 W1b: the workspace document is served by the same index handler as the
// terminal document, and its CSP capability is keyed on the same byte-exact
// query string (packet §5, falsifier W1B-F11).
func TestWorkspaceDocumentCSPCapabilityRequiresExactSelector(t *testing.T) {
	staticDir := writeTestStaticBundle(t, `<html><head><meta name="persea-style-nonce" content="`+styleNoncePlaceholder+`"></head><body><script src="/app.js"></script></body></html>`)
	s := newServer(config.Front{HandleTTLSeconds: 1, HandleCapacity: 1}, staticDir, "127.0.0.1:8080")
	const attributeCapability = "style-src-attr 'unsafe-inline'"
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{name: "exact", path: "/workspace?engine=unified-dev", want: true},
		{name: "terminal exact unchanged", path: "/terminal?engine=unified-dev", want: true},
		{name: "missing", path: "/workspace"},
		{name: "wrong value", path: "/workspace?engine=legacy"},
		{name: "duplicate", path: "/workspace?engine=unified-dev&engine=unified-dev"},
		{name: "additional", path: "/workspace?engine=unified-dev&extra=1"},
		{name: "reordered", path: "/workspace?extra=1&engine=unified-dev"},
		{name: "encoded", path: "/workspace?engine=unified%2Ddev"},
		{name: "fragment engine is not a query", path: "/workspace?engine=unified-dev&name=ops"},
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
			if body := rr.Body.String(); strings.Count(body, nonce) != 1 || strings.Contains(body, styleNoncePlaceholder) {
				t.Fatalf("nonce binding failed: %q", body)
			}
		})
	}
	// A workspace sub-path is not a document: the static file server answers
	// it, never the index handler.
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/workspace/extra?engine=unified-dev", nil))
	if rr.Code == http.StatusOK && strings.Contains(rr.Header().Get("Content-Security-Policy"), attributeCapability) {
		t.Fatalf("workspace sub-path served with the document capability: status=%d csp=%q", rr.Code, rr.Header().Get("Content-Security-Policy"))
	}
}

// One secured request through the real `secure` middleware against a server
// whose limiter clock is frozen (no refill). Returns the status the request
// produced: 204 from the recording handler, or the middleware's own refusal.
func chargeSecured(t *testing.T, s *Server, method, target string) int {
	t.Helper()
	h := s.secure(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	r := httptest.NewRequest(method, "http://localhost"+target, nil)
	r.Host = "localhost"
	r.Header.Set("X-Forwarded-Host", s.cfg.Ingress.CanonicalHost)
	r.Header.Set("X-Forwarded-Proto", strings.TrimSuffix(externalOrigin(s.cfg.Ingress), "://"+s.cfg.Ingress.CanonicalHost))
	r.Header.Set("Tailscale-User-Login", s.cfg.Ingress.OperatorLogin)
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set("Origin", externalOrigin(s.cfg.Ingress))
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Cookie", csrfCookie+"="+testCSRF)
		r.Header.Set("X-Persea-CSRF", testCSRF)
	}
	r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: s.cfg.Ingress.PeerUID}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func frozenClockServer() *Server {
	cfg := config.Front{Ingress: config.Ingress{SocketPath: "/tmp/front.sock", PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true, CanonicalHost: "localhost:43210", OperatorLogin: "operator@example.com", MaxConnections: 64}}
	frozen := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	return &Server{cfg: cfg, limiter: operatorLimiter{now: func() time.Time { return frozen }}}
}

func remainingTokens(s *Server) float64 {
	s.limiter.mu.Lock()
	defer s.limiter.mu.Unlock()
	return s.limiter.tokens
}

// The cold six-pane workspace load, request by request, as the product
// really performs it (W1B-R1): document + three static assets (+ optional
// favicon) + the workspace-record slot + ONE shared inventory snapshot + six
// × (adoption, initial upgrade answered lease_held, the takeover helper's
// CSRF-refresh GET of /api/inventory, takeover, replacement upgrade). The
// CSRF refresh is a charged secured request (ui/src/csrf_refresh.ts precedes
// every handle/takeover POST with it) and is accounted per pane, apart from
// the single workspace snapshot.
func coldWorkspaceLoad(withFavicon bool) [][2]string {
	requests := [][2]string{
		{http.MethodGet, "/workspace?engine=unified-dev"},
		{http.MethodGet, "/xterm.css"},
		{http.MethodGet, "/app.css"},
		{http.MethodGet, "/app.js"},
	}
	if withFavicon {
		requests = append(requests, [2]string{http.MethodGet, "/favicon.ico"})
	}
	// W1 keeps its arrangement in the tab (no store yet); the slot is charged
	// anyway so the arithmetic already covers W2's record GET.
	// The ONE shared workspace inventory snapshot (single-flight, W1B-F7).
	requests = append(requests, [2]string{http.MethodGet, "/api/workspaces"}, [2]string{http.MethodGet, "/api/inventory"})
	for pane := 0; pane < 6; pane++ {
		requests = append(requests,
			[2]string{http.MethodPost, "/api/session-adoptions"},
			[2]string{http.MethodGet, "/ws"},
			// refreshCSRFToken before the takeover POST: a charged GET per pane.
			[2]string{http.MethodGet, "/api/inventory"},
			[2]string{http.MethodPost, "/api/control-takeovers"},
			[2]string{http.MethodGet, "/ws"},
		)
	}
	return requests
}

// Falsifier W1B-F8 / FW-M2-cold (W1B-R1 arithmetic): with the operator burst
// at its W1 value and no refill, the coldest six-pane load produces zero 429
// and leaves EXACTLY three tokens in the favicon form (37 charged) and four
// without it (36 charged). A burst of 36 makes the favicon form refuse its
// last request — that mutant is the RED receipt for the margin.
func TestColdWorkspaceLoadFitsOperatorBurstWithoutRefill(t *testing.T) {
	for _, tc := range []struct {
		name        string
		withFavicon bool
		charged     int
		minLeft     float64
	}{
		{name: "favicon form", withFavicon: true, charged: 37, minLeft: 3},
		{name: "no favicon", withFavicon: false, charged: 36, minLeft: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := frozenClockServer()
			requests := coldWorkspaceLoad(tc.withFavicon)
			if len(requests) != tc.charged {
				t.Fatalf("modeled sequence has %d requests, want %d", len(requests), tc.charged)
			}
			for i, request := range requests {
				if code := chargeSecured(t, s, request[0], request[1]); code != http.StatusNoContent {
					t.Fatalf("request %d %s %s: status=%d (tokens left before it: %v)", i+1, request[0], request[1], code, remainingTokens(s))
				}
			}
			if left := remainingTokens(s); left != tc.minLeft {
				t.Fatalf("tokens left after the cold load: %v, want exactly %v", left, tc.minLeft)
			}
			if operatorBurst-float64(tc.charged) != remainingTokens(s) {
				t.Fatalf("every request must be charged exactly once: burst=%v charged=%d left=%v", operatorBurst, tc.charged, remainingTokens(s))
			}
		})
	}
	// Positive control: the frozen clock really does not refill, and the
	// middleware really refuses past the burst — so the assertions above
	// observe the limiter, not a permissive fake.
	t.Run("past the burst", func(t *testing.T) {
		s := frozenClockServer()
		for i := 0; i < int(operatorBurst); i++ {
			if code := chargeSecured(t, s, http.MethodGet, "/api/inventory"); code != http.StatusNoContent {
				t.Fatalf("request %d refused early: status=%d", i+1, code)
			}
		}
		if code := chargeSecured(t, s, http.MethodGet, "/api/inventory"); code != http.StatusTooManyRequests {
			t.Fatalf("request %d past the burst: status=%d, want 429", int(operatorBurst)+1, code)
		}
	})
	// The refill is unchanged by the raise: one second restores ten tokens,
	// and the bucket caps at the burst.
	t.Run("refill unchanged", func(t *testing.T) {
		s := frozenClockServer()
		at := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
		s.limiter.now = func() time.Time { return at }
		for i := 0; i < int(operatorBurst); i++ {
			chargeSecured(t, s, http.MethodGet, "/api/inventory")
		}
		at = at.Add(time.Second)
		if code := chargeSecured(t, s, http.MethodGet, "/api/inventory"); code != http.StatusNoContent {
			t.Fatalf("after one second of refill: status=%d", code)
		}
		if left := remainingTokens(s); left != operatorRefill-1 {
			t.Fatalf("refill after one second: %v tokens left, want %v", left, operatorRefill-1)
		}
		at = at.Add(time.Hour)
		chargeSecured(t, s, http.MethodGet, "/api/inventory")
		if left := remainingTokens(s); left != operatorBurst-1 {
			t.Fatalf("bucket cap: %v tokens left, want %v", left, operatorBurst-1)
		}
	})
}
