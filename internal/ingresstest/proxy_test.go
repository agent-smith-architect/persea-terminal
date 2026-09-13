package ingresstest

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// startAdapter runs the disposable adapter on a loopback port and returns its
// authority once it accepts connections.
func startAdapter(t *testing.T, socket, operator string, serveTLS bool) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	authority := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	failed := make(chan error, 1)
	go func() { failed <- Run(authority, socket, operator, serveTLS) }()
	for i := 0; i < 200; i++ {
		select {
		case err := <-failed:
			t.Fatalf("adapter exited: %v", err)
		default:
		}
		var c net.Conn
		var err error
		if serveTLS {
			c, err = tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", authority, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
		} else {
			c, err = net.DialTimeout("tcp", authority, time.Second)
		}
		if err == nil {
			_ = c.Close()
			return authority
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("adapter did not accept connections")
	return ""
}

// TestAdapterDeclaresItsOwnScheme pins the adapter contract both ways: the
// forwarded proto it stamps is the scheme it actually serves, so the front
// door's hermetic proto check can be exact.
func TestAdapterDeclaresItsOwnScheme(t *testing.T) {
	for _, tc := range []struct {
		name     string
		serveTLS bool
		scheme   string
	}{
		{"plaintext", false, "http"},
		{"tls", true, "https"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// AF_UNIX paths are capped at 107 bytes and need a local filesystem,
			// so this does not follow TMPDIR — the same reason the product's own
			// local harness puts its runtime tree under /tmp.
			dir, err := os.MkdirTemp("/tmp", "persea-ingresstest")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			socket := filepath.Join(dir, "front.sock")
			seen := make(chan http.Header, 4)
			hosts := make(chan string, 4)
			backend, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Clone()
				hosts <- r.Host
				http.SetCookie(w, &http.Cookie{Name: "__Host-persea-terminal-csrf", Value: "token", Path: "/", Secure: true, SameSite: http.SameSiteStrictMode})
				w.WriteHeader(http.StatusNoContent)
			})}
			go func() { _ = server.Serve(backend) }()
			t.Cleanup(func() { _ = server.Close() })

			authority := startAdapter(t, socket, "operator@example.test", tc.serveTLS)
			client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
			}}
			response, err := client.Get(tc.scheme + "://" + authority + "/api/inventory")
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d", response.StatusCode)
			}
			header := <-seen
			if got := header.Get("X-Forwarded-Proto"); got != tc.scheme {
				t.Fatalf("X-Forwarded-Proto = %q, want %q", got, tc.scheme)
			}
			if got := header.Get("X-Forwarded-Host"); got != authority {
				t.Fatalf("X-Forwarded-Host = %q, want %q", got, authority)
			}
			if got := header.Get("Tailscale-User-Login"); got != "operator@example.test" {
				t.Fatalf("modeled login = %q", got)
			}
			if host := <-hosts; host != "localhost" {
				t.Fatalf("backend Host = %q", host)
			}
			// The TLS material stays in memory: the run leaves nothing beside the
			// socket it was pointed at.
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "front.sock" {
				t.Fatalf("adapter wrote files: %v", entries)
			}
		})
	}
}

func TestEphemeralCertificateIsPerRunAndLoopbackOnly(t *testing.T) {
	first, err := EphemeralLoopbackCertificate()
	if err != nil {
		t.Fatal(err)
	}
	second, err := EphemeralLoopbackCertificate()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(first.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	other, err := x509.ParseCertificate(second.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if parsed.SerialNumber.Cmp(other.SerialNumber) == 0 {
		t.Fatal("serial reused across runs")
	}
	if _, ok := first.PrivateKey.(*ecdsa.PrivateKey); !ok {
		t.Fatalf("unexpected key type %T", first.PrivateKey)
	}
	if parsed.PublicKey.(*ecdsa.PublicKey).Equal(other.PublicKey) {
		t.Fatal("key reused across runs")
	}
	for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
		if err := parsed.VerifyHostname(host); err != nil {
			t.Fatalf("certificate does not cover %s: %v", host, err)
		}
	}
	if err := parsed.VerifyHostname("terminal.example.ts.net"); err == nil {
		t.Fatal("certificate covers a non-loopback name")
	}
	if life := parsed.NotAfter.Sub(parsed.NotBefore); life > 25*time.Hour {
		t.Fatalf("certificate lifetime = %s", life)
	}
}
