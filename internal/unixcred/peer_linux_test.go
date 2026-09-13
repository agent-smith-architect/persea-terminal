package unixcred

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func unixPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "peer.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); accepted <- c }()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}

func TestPeerUIDBothDirectionsAndMismatch(t *testing.T) {
	client, server := unixPair(t)
	want := uint32(os.Getuid())
	for _, c := range []net.Conn{client, server} {
		if got, err := PeerUID(c); err != nil || got != want {
			t.Fatalf("uid=%d err=%v", got, err)
		}
		if err := Verify(c, want); err != nil {
			t.Fatal(err)
		}
		if err := Verify(c, want+1); err == nil {
			t.Fatal("wrong peer accepted")
		}
	}
}

func TestPeerUIDRejectsNonUnix(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, err := PeerUID(a); err == nil {
		t.Fatal("non-Unix connection accepted")
	}
}
