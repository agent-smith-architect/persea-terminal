package ingresstest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"

	"persea-terminal/internal/attachmentwire"
)

func TestProbeAdoptsExactSourceBeforeUnifiedAttachAndRejectsReplay(t *testing.T) {
	for _, mode := range []string{"observe", "control"} {
		for _, refuse := range []bool{false, true} {
			t.Run(mode+"/refuse="+map[bool]string{false: "false", true: "true"}[refuse], func(t *testing.T) {
				csrf := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
				var adopted atomic.Bool
				var connections atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/inventory":
						http.SetCookie(w, &http.Cookie{Name: "__Host-persea-terminal-csrf", Value: csrf, Secure: true, Path: "/", SameSite: http.SameSiteStrictMode})
						_, _ = w.Write([]byte(`{"realms":[{"name":"realm","servers":[{"label":"private","sessions":[{"name":"source","session_id":"$7","handles":{"alias":"alias","observe":"observe","control":"control"}}]}]}]}`))
					case "/api/session-adoptions":
						var body struct {
							Realm   string `json:"realm"`
							Server  string `json:"server"`
							Session string `json:"session_id"`
							History int    `json:"history_rows"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if body.Realm != "realm" || body.Server != "private" || body.Session != "$7" || body.History != 5000 || r.Method != http.MethodPost || r.Header.Get("X-Persea-CSRF") != csrf || r.Header.Get("Sec-Fetch-Site") != "same-origin" || r.Header.Get("Origin") != "http://"+r.Host {
							t.Errorf("invalid adoption authority/metadata: %+v", body)
						}
						if cookie, err := r.Cookie("__Host-persea-terminal-csrf"); err != nil || cookie.Value != csrf {
							t.Error("adoption cookie missing")
						}
						adopted.Store(true)
						if refuse {
							w.WriteHeader(http.StatusConflict)
							return
						}
						w.WriteHeader(http.StatusOK)
					case "/ws":
						if !adopted.Load() || refuse {
							t.Error("attach preceded successful adoption")
						}
						if r.URL.RawQuery != "" {
							t.Error("query authority")
						}
						protocols := websocket.Subprotocols(r)
						for _, want := range []string{"persea-engine.unified-dev", "persea-history.5000", "persea-terminal.v2", "persea-handle." + mode, "persea-mode." + mode, "persea-csrf." + csrf} {
							n := 0
							for _, got := range protocols {
								if got == want {
									n++
								}
							}
							if n != 1 {
								t.Errorf("protocol %s count=%d", want, n)
							}
						}
						if connections.Add(1) > 1 {
							w.WriteHeader(http.StatusGone)
							return
						}
						upgrader := websocket.Upgrader{Subprotocols: []string{"persea-terminal.v2"}}
						conn, err := upgrader.Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer conn.Close()
						// Flow acknowledgements may arrive between any two frames;
						// each must advance the cumulative count.
						var acknowledged uint64
						readBrowser := func(message *map[string]any) error {
							for {
								_, payload, err := conn.ReadMessage()
								if err != nil {
									return err
								}
								count, recognized, err := attachmentwire.DecodeTransportFlowAck(payload, attachmentwire.BrowserToServer)
								if !recognized {
									return json.Unmarshal(payload, message)
								}
								if err != nil || count <= acknowledged {
									return fmt.Errorf("bad acknowledgement %q after %d", payload, acknowledged)
								}
								acknowledged = count
							}
						}
						_ = conn.WriteJSON(map[string]any{"type": "PREPARE", "source": "source-id", "epoch": "1", "cut": "1"})
						var message map[string]any
						if err := readBrowser(&message); err != nil || message["type"] != "READY" {
							t.Errorf("READY: %v %v", message, err)
							return
						}
						_ = conn.WriteJSON(map[string]any{"type": "COMMIT", "source": "source-id", "epoch": "1"})
						if mode == "control" {
							if err := readBrowser(&message); err != nil || message["type"] != "MODE_REQUEST" {
								t.Error("missing control request")
								return
							}
							_ = conn.WriteJSON(map[string]any{"type": "MODE", "source": "source-id", "epoch": "1", "mode": "CONTROL"})
							if err := readBrowser(&message); err != nil || message["type"] != "INPUT" {
								t.Error("missing input")
								return
							}
							// A front door sends no more output than the client has
							// acknowledged; by now PREPARE and COMMIT were consumed.
							if acknowledged < 2 {
								t.Errorf("probe acknowledged %d frames before input, want at least PREPARE and COMMIT", acknowledged)
								return
							}
							data, _ := base64.StdEncoding.DecodeString(message["data"].(string))
							if !strings.Contains(string(data), "sentinel") {
								t.Error("input changed")
							}
							_ = conn.WriteJSON(map[string]any{"type": "LIVE", "data": base64.StdEncoding.EncodeToString([]byte("sentinel"))})
						}
						_, _, _ = conn.ReadMessage()
					default:
						t.Errorf("unexpected path %s", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer server.Close()
				err := Probe(server.URL, "realm", "source", mode, "sentinel", false)
				if refuse {
					if err == nil || !strings.Contains(err.Error(), "session adoption status 409") || connections.Load() != 0 {
						t.Fatalf("refusal=%v connections=%d", err, connections.Load())
					}
				} else if err != nil || connections.Load() != 2 {
					t.Fatalf("probe=%v connections=%d", err, connections.Load())
				}
			})
		}
	}
}
