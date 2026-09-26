package frontdoor

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/terminal"
)

const csrfCookie = "__Host-persea-terminal-csrf"

type ingressIdentity struct {
	uid              uint32
	operator, origin string
}
type ingressKey struct{}

func identity(r *http.Request) (ingressIdentity, bool) {
	v, ok := r.Context().Value(ingressKey{}).(ingressIdentity)
	return v, ok
}
func logIngress(reason string, uid uint32) {
	log.Printf("component=frontdoor event=ingress_denial reason=%q peer_uid=%d", reason, uid)
}
func logIngressReason(reason string) {
	log.Printf("component=frontdoor event=ingress_denial reason=%q", reason)
}
func logRequestIngress(r *http.Request, reason string) {
	if id, ok := identity(r); ok {
		logIngress(reason, id.uid)
		return
	}
	logIngressReason(reason)
}
func one(h http.Header, k string) (string, bool) {
	v, ok := h[http.CanonicalHeaderKey(k)]
	return firstExact(v, ok)
}
func firstExact(v []string, ok bool) (string, bool) {
	returnValue := ""
	if !ok || len(v) != 1 || v[0] == "" || strings.Contains(v[0], ",") {
		return returnValue, false
	}
	return v[0], true
}
func externalOrigin(i config.Ingress) string {
	if isHermetic(i) && !i.HermeticTLS {
		return "http://" + i.CanonicalHost
	}
	return "https://" + i.CanonicalHost
}

// hermeticScheme is the exact forwarded proto a hermetic adapter must declare.
// It is derived from the same origin the request is checked against, so the two
// can never disagree.
func hermeticScheme(i config.Ingress) string {
	return strings.TrimSuffix(externalOrigin(i), "://"+i.CanonicalHost)
}

// isHermetic defers to the config package so this predicate has one definition.
// The earlier local copy accepted authorities the validator would have rejected,
// because it did not check the port.
func isHermetic(i config.Ingress) bool {
	return config.Hermetic(i, uint32(os.Geteuid()))
}
func trustedContext(ctx context.Context, c net.Conn) context.Context {
	if tc, ok := c.(*trustedConn); ok {
		return context.WithValue(ctx, ingressKey{}, ingressIdentity{uid: tc.uid})
	}
	return ctx
}

// The operator request budget: a token bucket charged once per secured
// request. The burst is sized for the coldest single operator action the
// product performs: a cold-cache six-pane workspace load is the document,
// three static assets, an optional favicon, the workspace-record slot, one
// shared inventory snapshot, and six times (adoption, initial upgrade refused
// lease_held, the takeover helper's CSRF-refresh GET, takeover, replacement
// upgrade) — 36 requests, 37 with the favicon — with no refill
// between requests.
const (
	operatorBurst  = 40.0
	operatorRefill = 10.0
)

type operatorLimiter struct {
	mu     sync.Mutex
	tokens float64
	at     time.Time
	// The clock; nil means time.Now. A test freezes it to model a load with
	// no refill.
	now func() time.Time
}

func (l *operatorLimiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func (l *operatorLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if l.at.IsZero() {
		l.tokens = operatorBurst
		l.at = now
	}
	l.tokens += now.Sub(l.at).Seconds() * operatorRefill
	if l.tokens > operatorBurst {
		l.tokens = operatorBurst
	}
	l.at = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := identity(r)
		if !ok || id.uid != s.cfg.Ingress.PeerUID {
			deny(w, "untrusted_conn", id.uid)
			return
		}
		if r.Host != "localhost" {
			deny(w, "host", id.uid)
			return
		}
		xfh, ok := one(r.Header, "X-Forwarded-Host")
		if !ok || xfh != s.cfg.Ingress.CanonicalHost {
			deny(w, "host", id.uid)
			return
		}
		hermetic := isHermetic(s.cfg.Ingress)
		redirectHTTP := false
		xfpValues, xfpPresent := r.Header[http.CanonicalHeaderKey("X-Forwarded-Proto")]
		xfp, xfpExact := firstExact(xfpValues, xfpPresent)
		if hermetic {
			// A hermetic adapter always declares its proto exactly; there is no
			// redirect branch here, so a TLS hermetic adapter must say https and a
			// plaintext one must say http. Neither may say the other.
			if !xfpExact || xfp != hermeticScheme(s.cfg.Ingress) {
				deny(w, "proto", id.uid)
				return
			}
		} else if !xfpPresent {
			// Tailscale Serve sets X-Forwarded-Proto only when the inbound
			// connection has TLS. An absent header is therefore the strictly
			// redirect-only plaintext branch after peer and authority checks.
			redirectHTTP = true
		} else if !xfpExact || xfp != "https" {
			deny(w, "proto", id.uid)
			return
		}
		if redirectHTTP {
			if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.URL.Path == "/ws" {
				deny(w, "proto", id.uid)
				return
			}
			if _, present := r.Header[http.CanonicalHeaderKey("Upgrade")]; present {
				deny(w, "proto", id.uid)
				return
			}
			destination := (&url.URL{
				Scheme:     "https",
				Host:       s.cfg.Ingress.CanonicalHost,
				Path:       r.URL.Path,
				RawPath:    r.URL.RawPath,
				ForceQuery: r.URL.ForceQuery,
				RawQuery:   r.URL.RawQuery,
			}).String()
			securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, destination, http.StatusPermanentRedirect)
			})).ServeHTTP(w, r)
			return
		}
		login, ok := one(r.Header, "Tailscale-User-Login")
		if !ok || login != s.cfg.Ingress.OperatorLogin {
			deny(w, "operator", id.uid)
			return
		}
		for k, v := range r.Header {
			if strings.HasPrefix(http.CanonicalHeaderKey(k), "Tailscale-") {
				switch http.CanonicalHeaderKey(k) {
				case "Tailscale-User-Login":
				case "Tailscale-User-Name", "Tailscale-User-Profile-Pic", "Tailscale-Headers-Info":
					if _, ok := firstExact(v, true); !ok {
						deny(w, "tailscale_header", id.uid)
						return
					}
				default:
					deny(w, "funnel", id.uid)
					return
				}
			}
		}
		origin := externalOrigin(s.cfg.Ingress)
		if v, present := r.Header["Origin"]; present {
			got, valid := firstExact(v, true)
			if !valid || got != origin {
				deny(w, "origin", id.uid)
				return
			}
		}
		if site, present := r.Header["Sec-Fetch-Site"]; present {
			got, valid := firstExact(site, true)
			if !valid || (got != "same-origin" && got != "none") {
				deny(w, "fetch_site", id.uid)
				return
			}
		}
		if !s.limiter.allow() {
			// Not 403: exhausting a budget is not an authorization failure, and
			// conflating them makes the logs unreadable during an incident.
			logIngress("rate_limit", id.uid)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		id.operator, id.origin = login, origin
		r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, id))
		token, valid := csrfFromRequest(r)
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead
		if safe && !valid {
			var err error
			token, err = randomToken()
			if err != nil {
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "unavailable", 503)
				return
			}
			// Secure is unconditional and must stay that way. The __Host- prefix is
			// only honoured when the cookie carries Secure, so making it conditional
			// does not relax the cookie — the browser rejects it outright and every
			// unsafe method then fails CSRF. The hermetic adapter is not a reason to
			// vary it either: http://localhost is a trustworthy origin, so browsers
			// accept Secure cookies there. This was briefly made conditional and had
			// to be reverted; do not try it again.
			http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: token, Path: "/", MaxAge: 600, Secure: true, SameSite: http.SameSiteStrictMode})
		}
		if !safe && r.URL.Path != "/ws" {
			requestOrigin, originOK := one(r.Header, "Origin")
			fetchSite, fetchOK := one(r.Header, "Sec-Fetch-Site")
			if !originOK || requestOrigin != origin || !fetchOK || fetchSite != "same-origin" {
				deny(w, "origin", id.uid)
				return
			}
			header, hok := one(r.Header, "X-Persea-CSRF")
			if !valid || !hok || subtle.ConstantTimeCompare([]byte(token), []byte(header)) != 1 {
				deny(w, "csrf", id.uid)
				return
			}
		}
		securityHeaders(next).ServeHTTP(w, r)
	})
}
func csrfFromRequest(r *http.Request) (string, bool) {
	found := ""
	n := 0
	for _, c := range r.Cookies() {
		if c.Name == csrfCookie {
			n++
			found = c.Value
		}
	}
	b, e := base64.RawURLEncoding.DecodeString(found)
	return found, n == 1 && e == nil && len(b) == 32
}

// deny refuses before securityHeaders runs, so it stamps no-store itself: a
// refusal on a secret-bearing route must never be a cacheable document.
func deny(w http.ResponseWriter, reason string, uid uint32) {
	logIngress(reason, uid)
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "forbidden", http.StatusForbidden)
}

type wsAuthority struct {
	handle, mode, csrf string
	engine             string
	history            int
	historySet         bool
}

func historyProtocolValue(protocol string) (int, bool) {
	switch protocol {
	case "persea-history.0":
		return 0, true
	case "persea-history.500":
		return 500, true
	case "persea-history.1000":
		return 1000, true
	case "persea-history.2000":
		return 2_000, true
	case "persea-history.5000":
		return 5_000, true
	case "persea-history.7500":
		return 7_500, true
	case "persea-history.10000":
		return 10_000, true
	default:
		return 0, false
	}
}

func optionalWSHistory(r *http.Request) (int, bool) {
	history, found := terminal.DefaultHistoryRows, false
	for _, raw := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, item := range strings.Split(raw, ",") {
			item = strings.TrimSpace(item)
			if !strings.HasPrefix(item, "persea-history.") {
				continue
			}
			value, valid := historyProtocolValue(item)
			if found || !valid {
				return 0, false
			}
			history, found = value, true
		}
	}
	return history, true
}

func parseWSAuthority(r *http.Request) (wsAuthority, bool) {
	return parseWSAuthorityPolicy(r, true)
}

func parseWSAuthorityPolicy(r *http.Request, requireHistory bool) (wsAuthority, bool) {
	var a wsAuthority
	var versionSet, handleSet, modeSet, csrfSet, engineSet bool
	for _, raw := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				return a, false
			}
			switch {
			case strings.HasPrefix(p, "persea-terminal."):
				if versionSet {
					return a, false
				}
				versionSet = true
				if p != AttachmentProtocol {
					return a, false
				}
			case strings.HasPrefix(p, "persea-handle."):
				if handleSet {
					return a, false
				}
				handleSet = true
				a.handle = strings.TrimPrefix(p, "persea-handle.")
				if a.handle == "" {
					return a, false
				}
			case strings.HasPrefix(p, "persea-mode."):
				if modeSet {
					return a, false
				}
				modeSet = true
				a.mode = strings.TrimPrefix(p, "persea-mode.")
				if a.mode == "" {
					return a, false
				}
			case strings.HasPrefix(p, "persea-csrf."):
				if csrfSet {
					return a, false
				}
				csrfSet = true
				a.csrf = strings.TrimPrefix(p, "persea-csrf.")
				if a.csrf == "" {
					return a, false
				}
			case strings.HasPrefix(p, "persea-history."):
				if a.historySet {
					return a, false
				}
				var valid bool
				a.history, valid = historyProtocolValue(p)
				if !valid {
					return a, false
				}
				a.historySet = true
			case strings.HasPrefix(p, "persea-engine."):
				if engineSet || p != "persea-engine.unified-dev" {
					return a, false
				}
				engineSet = true
				a.engine = "unified-dev"
			default:
				return a, false
			}
		}
	}
	if !a.historySet && !requireHistory {
		a.history, a.historySet = terminal.DefaultHistoryRows, true
	}
	return a, versionSet && handleSet && modeSet && csrfSet && engineSet && (a.mode == "observe" || a.mode == "control") && a.historySet
}
