package frontdoor

import (
	"context"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func TestTerminalRequiresUnifiedBeforeCapabilityConsumption(t *testing.T) {
	for _, engine := range []string{"", ", persea-engine.", ", persea-engine.other"} {
		for _, mode := range []string{"observe", "control"} {
			store := newHandleStore(time.Minute, 4)
			handle, err := store.mint(proto.Authority{}, "operator@example.com", mode)
			if err != nil {
				t.Fatal(err)
			}
			s := &Server{cfg: config.Front{Ingress: config.Ingress{PeerUIDConfigured: true}}, handles: store}
			r := httptest.NewRequest(http.MethodGet, "http://localhost/ws", nil)
			r.Header.Set("Sec-WebSocket-Protocol", "persea-terminal.v1, persea-handle."+handle+", persea-mode."+mode+", persea-csrf."+testCSRF+", persea-history.5000"+engine)
			r.Header.Set("Origin", "http://localhost")
			r.Header.Set("Cookie", csrfCookie+"="+testCSRF)
			r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{operator: "operator@example.com", origin: "http://localhost"}))
			w := httptest.NewRecorder()
			s.terminal(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("engine=%q mode=%s status=%d", engine, mode, w.Code)
			}
			if _, err := store.consume(handle, "operator@example.com", mode); err != nil {
				t.Fatalf("invalid engine consumed capability: %v", err)
			}
		}
	}
}

func TestTerminalRejectsQueryAuthority(t *testing.T) {
	for _, peer := range []bool{false, true} {
		store := newHandleStore(time.Minute, 4)
		handle, err := store.mint(proto.Authority{})
		if err != nil {
			t.Fatal(err)
		}
		s := &Server{cfg: config.Front{Ingress: config.Ingress{PeerUIDConfigured: peer}}, handles: store}
		r := httptest.NewRequest(http.MethodGet, "http://localhost/ws?handle="+handle+"&mode=observe&engine=unified-dev", nil)
		w := httptest.NewRecorder()
		s.terminal(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("peer=%v query authority status=%d", peer, w.Code)
		}
		if _, err := store.resolve(handle); err != nil {
			t.Fatalf("query authority consumed capability: %v", err)
		}
	}
}

// currentFrontFixtureConfig supplies the same authenticated ingress contract as
// current serving, with the existing private loopback trusted-hop adapter.
func currentFrontFixtureConfig(addr, brokerPath string) config.Front {
	return config.Front{Realms: []config.Realm{{Name: "r", Socket: brokerPath}}, HandleTTLSeconds: 120, HandleCapacity: 32, Ingress: config.Ingress{PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true, CanonicalHost: addr, OperatorLogin: "operator@example.com", MaxConnections: 64}}
}
func currentFixtureHeaders(addr string) http.Header {
	return http.Header{"Origin": []string{"http://" + addr}, "Cookie": []string{csrfCookie + "=" + testCSRF}}
}
func currentFixtureDial(addr, handle, mode string) (*websocket.Conn, *http.Response, error) {
	dialer := websocket.Dialer{Subprotocols: []string{"persea-engine.unified-dev", "persea-terminal.v1", "persea-handle." + handle, "persea-mode." + mode, "persea-csrf." + testCSRF, "persea-history.5000"}}
	return dialer.Dial("ws://"+addr+"/ws", currentFixtureHeaders(addr))
}
