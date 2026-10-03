package frontdoor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"persea-terminal/internal/broker"
	"persea-terminal/internal/config"
)

func TestAliasRestartOverRealBroker(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatal("tmux is required")
	}
	label := fmt.Sprintf("pt-al-%d-%d", os.Getpid(), time.Now().UnixNano())
	tmux := func(args ...string) {
		t.Helper()
		cmd := exec.Command("tmux", append([]string{"-L", label, "-f", "/dev/null"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("private tmux: %v: %s", err, out)
		}
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", label, "kill-server").Run() })
	tmux("new-session", "-d", "-s", "he2", "sleep 300")
	socket := filepath.Join(shortTestDir(t), "broker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- broker.Serve(listener, config.Broker{Realm: "r", FrontUID: uint32(os.Getuid()), FrontUIDConfigured: true, Servers: []config.TmuxServer{{Label: "s", SocketName: label}}})
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("broker did not stop")
		}
	})
	s := newServer(config.Front{Realms: []config.Realm{{Name: "r", Socket: socket, BrokerUID: uint32(os.Getuid()), BrokerUIDConfigured: true}}, HandleTTLSeconds: 60, HandleCapacity: 32}, ".", "127.0.0.1:8080")
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, bytes.NewBufferString(body))
		s.handler().ServeHTTP(w, r)
		return w
	}
	inventory := func() sessionView {
		w := request("GET", "/api/inventory", "")
		if w.Code != 200 {
			t.Fatalf("inventory=%d %s", w.Code, w.Body.String())
		}
		var parsed struct {
			Realms []realmView `json:"realms"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
			t.Fatal(err)
		}
		if len(parsed.Realms) != 1 || len(parsed.Realms[0].Servers) != 1 || len(parsed.Realms[0].Servers[0].Sessions) != 1 {
			t.Fatalf("missing private session: %s", w.Body.String())
		}
		return *parsed.Realms[0].Servers[0].Sessions[0]
	}
	first := inventory()
	body, _ := json.Marshal(map[string]string{"display_alias": "Research", "handle": first.Handles.Alias})
	if w := request(http.MethodPost, "/api/aliases", string(body)); w.Code != 201 {
		t.Fatalf("save=%d %s", w.Code, w.Body.String())
	}
	tmux("kill-server")
	// tmux may finish removing its socket just after the command reply.
	serverSocket := filepath.Join(os.TempDir(), fmt.Sprintf("tmux-%d", os.Getuid()), label)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(serverSocket); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("private server socket was not removed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	tmux("new-session", "-d", "-s", "he2", "sleep 300")
	second := inventory()
	if sameAuthority(first.Authority, second.Authority) {
		t.Fatal("restart did not change authority")
	}
	if second.Alias != "Research" || second.AliasState != "active" || s.aliases.list()[0].SessionName != "he2" || !sameAuthority(s.aliases.list()[0].Incarnation, second.Authority) {
		t.Fatalf("restart lost alias: %+v %+v", second, s.aliases.list())
	}
}
