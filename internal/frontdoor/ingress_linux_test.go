package frontdoor

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"persea-terminal/internal/config"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestIngressWrongPeerGetsZeroBytes(t *testing.T) {
	var logs lockedBuffer
	prior := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prior)
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "front.sock")
	cfg := config.Ingress{SocketPath: p, PeerUID: uint32(os.Geteuid() + 1), PeerUIDConfigured: true, MaxConnections: 2}
	ln, cleanup, e := listenIngress(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer cleanup()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "protected") })}
	defer server.Close()
	go server.Serve(ln)
	c, e := net.Dial("unix", p)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	// Peer rejection may close the socket before this write reaches it.
	if _, e := io.WriteString(c, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); e != nil && !errors.Is(e, syscall.EPIPE) && !errors.Is(e, syscall.ECONNRESET) {
		t.Fatal(e)
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	b, readErr := io.ReadAll(c)
	if readErr != nil && !errors.Is(readErr, syscall.ECONNRESET) {
		t.Fatalf("denied connection did not close: %v", readErr)
	}
	if len(b) != 0 {
		t.Fatalf("denied peer received %d bytes", len(b))
	}
	if !strings.Contains(logs.String(), `reason="peer_uid"`) {
		t.Fatalf("wrong-UID denial log = %q", logs.String())
	}
}

func TestIngressProductionUIDZeroRejectsSameUIDWithZeroBytes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an ordinary same-UID peer")
	}
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "front.sock")
	cfg := config.Ingress{SocketPath: p, PeerUID: 0, PeerUIDConfigured: true, MaxConnections: 2}
	ln, cleanup, err := listenIngress(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "protected") })}
	defer server.Close()
	go server.Serve(ln)
	connection, err := net.Dial("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, _ = io.WriteString(connection, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	bytes, _ := io.ReadAll(connection)
	if len(bytes) != 0 {
		t.Fatalf("same-UID production spoof received %d bytes", len(bytes))
	}
}

func TestIngressSocketStateAndCleanupFailClosed(t *testing.T) {
	t.Run("parent_mode", func(t *testing.T) {
		d := t.TempDir()
		if err := os.Chmod(d, 0755); err != nil {
			t.Fatal(err)
		}
		_, _, err := listenIngress(config.Ingress{SocketPath: filepath.Join(d, "front.sock"), PeerUID: uint32(os.Geteuid()), MaxConnections: 1})
		if err == nil {
			t.Fatal("permissive parent accepted")
		}
	})
	t.Run("occupied_regular", func(t *testing.T) {
		d := t.TempDir()
		_ = os.Chmod(d, 0700)
		p := filepath.Join(d, "front.sock")
		if err := os.WriteFile(p, []byte("foreign"), 0600); err != nil {
			t.Fatal(err)
		}
		_, _, err := listenIngress(config.Ingress{SocketPath: p, PeerUID: uint32(os.Geteuid()), MaxConnections: 1})
		if err == nil {
			t.Fatal("regular file accepted")
		}
		if b, _ := os.ReadFile(p); string(b) != "foreign" {
			t.Fatal("foreign object changed")
		}
	})
	t.Run("replacement_cleanup", func(t *testing.T) {
		d := t.TempDir()
		_ = os.Chmod(d, 0700)
		p := filepath.Join(d, "front.sock")
		ln, cleanup, err := listenIngress(config.Ingress{SocketPath: p, PeerUID: uint32(os.Geteuid()), MaxConnections: 1})
		if err != nil {
			t.Fatal(err)
		}
		_ = ln
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("replacement"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err == nil {
			t.Fatal("replacement cleanup succeeded")
		}
		if b, _ := os.ReadFile(p); string(b) != "replacement" {
			t.Fatal("replacement removed")
		}
	})
	t.Run("absent_cleanup", func(t *testing.T) {
		d := t.TempDir()
		_ = os.Chmod(d, 0700)
		p := filepath.Join(d, "front.sock")
		_, cleanup, err := listenIngress(config.Ingress{SocketPath: p, PeerUID: uint32(os.Geteuid()), MaxConnections: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err == nil {
			t.Fatal("absent bound socket cleanup succeeded")
		}
	})
	t.Run("chmod_failure_removes_exact_socket", func(t *testing.T) {
		d := t.TempDir()
		_ = os.Chmod(d, 0700)
		p := filepath.Join(d, "front.sock")
		prior := ingressChmod
		ingressChmod = func(string, os.FileMode) error { return errors.New("forced chmod failure") }
		t.Cleanup(func() { ingressChmod = prior })
		if _, _, err := listenIngress(config.Ingress{SocketPath: p, PeerUID: uint32(os.Geteuid()), MaxConnections: 1}); err == nil {
			t.Fatal("chmod failure accepted")
		}
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("chmod-failure socket residue: %v", err)
		}
	})
}

func TestIngressConnectionOverflowGetsZeroBytes(t *testing.T) {
	var logs lockedBuffer
	prior := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prior)
	d := t.TempDir()
	_ = os.Chmod(d, 0700)
	p := filepath.Join(d, "front.sock")
	ln, cleanup, err := listenIngress(config.Ingress{SocketPath: p, PeerUID: uint32(os.Geteuid()), MaxConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	firstAccepted := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); firstAccepted <- c }()
	first, err := net.Dial("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	accepted := <-firstAccepted
	defer first.Close()
	defer accepted.Close()
	second, err := net.Dial("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	go ln.Accept()
	_, _ = io.WriteString(second, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
	_ = second.SetReadDeadline(time.Now().Add(time.Second))
	b, _ := io.ReadAll(second)
	if len(b) != 0 {
		t.Fatalf("overflow peer received %d bytes", len(b))
	}
	if !strings.Contains(logs.String(), `reason="conn_limit"`) {
		t.Fatalf("overflow denial log = %q", logs.String())
	}
}
func TestIngressAcceptedConnectionCarriesKernelUID(t *testing.T) {
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "front.sock")
	cfg := config.Ingress{SocketPath: p, PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true, MaxConnections: 2}
	ln, cleanup, e := listenIngress(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer cleanup()
	done := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); done <- c }()
	c, e := net.Dial("unix", p)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	accepted := <-done
	defer accepted.Close()
	if accepted.(*trustedConn).uid != uint32(os.Geteuid()) {
		t.Fatal("wrong credential")
	}
}

func TestIngressCredentialInfrastructureFailureGetsZeroBytes(t *testing.T) {
	var logs lockedBuffer
	priorLog := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(priorLog)
	priorPeer := ingressPeerUID
	ingressPeerUID = func(net.Conn) (uint32, error) { return 0, errors.New("forced credential failure") }
	defer func() { ingressPeerUID = priorPeer }()
	d := t.TempDir()
	_ = os.Chmod(d, 0700)
	p := filepath.Join(d, "front.sock")
	ln, cleanup, err := listenIngress(config.Ingress{SocketPath: p, PeerUID: uint32(os.Geteuid()), MaxConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	go ln.Accept()
	c, err := net.Dial("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "GET /secret-query-marker HTTP/1.1\r\nHost: localhost\r\n\r\n")
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	b, _ := io.ReadAll(c)
	if len(b) != 0 {
		t.Fatalf("credential failure received %d bytes", len(b))
	}
	if got := logs.String(); !strings.Contains(got, `reason="peer_cred"`) || strings.Contains(got, "secret-query-marker") {
		t.Fatalf("credential denial log = %q", got)
	}
}
