package ingresstest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"persea-terminal/internal/attachmentwire"
)

// Run serves a disposable loopback model of Tailscale 1.98.9's Unix proxy.
//
// serveTLS terminates TLS at the adapter with a certificate minted for this
// process only, which is what real Tailscale Serve does. It exists because
// WebKit refuses to store a Secure cookie on any http origin, loopback
// included, and the front door's CSRF cookie is unconditionally Secure — so a
// WebKit browser gate cannot boot the page over a plaintext hermetic origin.
// The front door must be configured with ingress.hermetic_tls to match; that
// option is rejected on anything but a hermetic ingress.
func Run(listen, socket, operator string, serveTLS bool) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	authority := ln.Addr().String()
	scheme := "http"
	if serveTLS {
		scheme = "https"
	}
	target := &url.URL{Scheme: "http", Host: "localhost"}
	p := httputil.NewSingleHostReverseProxy(target)
	p.Transport = &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return net.Dial("unix", socket) }}
	original := p.Director
	p.Director = func(r *http.Request) {
		original(r)
		r.Host = "localhost"
		r.Header.Set("X-Forwarded-Host", authority)
		r.Header.Set("X-Forwarded-Proto", scheme)
		r.Header.Set("Tailscale-User-Login", operator)
		r.Header.Set("Tailscale-User-Name", "Hermetic Operator")
		r.Header.Set("Tailscale-User-Profile-Pic", "https://invalid.example.test/profile")
		r.Header.Set("Tailscale-Headers-Info", "https://tailscale.com/s/serve-headers")
	}
	s := &http.Server{Handler: p}
	fmt.Printf("LISTEN=%s\nSCHEME=%s\n", authority, scheme)
	if !serveTLS {
		return s.Serve(ln)
	}
	certificate, err := EphemeralLoopbackCertificate()
	if err != nil {
		return err
	}
	s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	// Both paths are empty: the key pair is held in memory and never reaches a
	// file, so a run leaves no private key behind to be reused or committed.
	return s.ServeTLS(ln, "", "")
}

// EphemeralLoopbackCertificate mints a short-lived self-signed certificate for
// loopback names. The key is generated per call and returned in memory only.
func EphemeralLoopbackCertificate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "persea-terminal hermetic adapter"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

type inventory struct {
	Realms []struct {
		Name    string `json:"name"`
		Servers []struct {
			Label    string `json:"label"`
			Sessions []struct {
				Name    string `json:"name"`
				ID      string `json:"session_id"`
				Handles struct {
					Alias   string `json:"alias"`
					Observe string `json:"observe"`
					Control string `json:"control"`
				} `json:"handles"`
			} `json:"sessions"`
		} `json:"servers"`
	} `json:"realms"`
}

// Probe exercises the authenticated adapter-to-AF_UNIX path without printing
// cookies or capability values. The browser-specific proof remains separate.
func Probe(base, realm, session, mode, sentinel string, mutateAlias bool) error {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid probe base")
	}
	// The adapter's certificate is minted per run and is not anchored anywhere,
	// so verification is skipped — but only for a loopback authority, which is
	// the only shape a hermetic ingress can have.
	var probeTLS *tls.Config
	if u.Scheme == "https" {
		if !loopbackAuthority(u.Host) {
			return fmt.Errorf("https probe base must be loopback")
		}
		probeTLS = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: probeTLS}}
	response, err := client.Get(base + "/api/inventory")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("inventory status %d", response.StatusCode)
	}
	var csrf string
	for _, cookie := range response.Cookies() {
		if cookie.Name == "__Host-persea-terminal-csrf" && cookie.Secure && cookie.Path == "/" && cookie.Domain == "" && cookie.SameSite == http.SameSiteStrictMode {
			if csrf != "" {
				return fmt.Errorf("duplicate CSRF cookie")
			}
			csrf = cookie.Value
		}
	}
	if raw, err := base64.RawURLEncoding.DecodeString(csrf); err != nil || len(raw) != 32 {
		return fmt.Errorf("invalid CSRF cookie")
	}
	var view inventory
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&view); err != nil {
		return err
	}
	var aliasHandle, wsHandle, serverLabel, sessionID string
	for _, r := range view.Realms {
		if r.Name != realm {
			continue
		}
		for _, server := range r.Servers {
			for _, candidate := range server.Sessions {
				if candidate.Name == session {
					aliasHandle = candidate.Handles.Alias
					serverLabel, sessionID = server.Label, candidate.ID
					if mode == "observe" {
						wsHandle = candidate.Handles.Observe
					} else if mode == "control" {
						wsHandle = candidate.Handles.Control
					}
				}
			}
		}
	}
	if aliasHandle == "" || wsHandle == "" {
		return fmt.Errorf("bound capabilities unavailable")
	}
	cookieHeader := "__Host-persea-terminal-csrf=" + csrf
	if mutateAlias {
		body, _ := json.Marshal(map[string]string{"display_alias": "Probe-" + fmt.Sprint(time.Now().UnixNano()), "handle": aliasHandle})
		req, _ := http.NewRequest(http.MethodPost, base+"/api/aliases", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", cookieHeader)
		req.Header.Set("Origin", base)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("X-Persea-CSRF", csrf)
		aliasResponse, e := client.Do(req)
		if e != nil {
			return e
		}
		_, _ = io.Copy(io.Discard, aliasResponse.Body)
		_ = aliasResponse.Body.Close()
		if aliasResponse.StatusCode != http.StatusCreated {
			return fmt.Errorf("alias mutation status %d", aliasResponse.StatusCode)
		}
	}
	// Adopt the exact selected source through the current dashboard endpoint.
	// Adoption does not consume the already minted attachment capability.
	adoptBody, _ := json.Marshal(map[string]any{"realm": realm, "server": serverLabel, "session_id": sessionID, "history_rows": 5000})
	adoptRequest, _ := http.NewRequest(http.MethodPost, base+"/api/session-adoptions", bytes.NewReader(adoptBody))
	adoptRequest.Header.Set("Content-Type", "application/json")
	adoptRequest.Header.Set("Cookie", cookieHeader)
	adoptRequest.Header.Set("Origin", base)
	adoptRequest.Header.Set("Sec-Fetch-Site", "same-origin")
	adoptRequest.Header.Set("X-Persea-CSRF", csrf)
	adoptResponse, err := client.Do(adoptRequest)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, adoptResponse.Body)
	_ = adoptResponse.Body.Close()
	if adoptResponse.StatusCode != http.StatusOK {
		return fmt.Errorf("session adoption status %d", adoptResponse.StatusCode)
	}
	wsURL := *u
	wsURL.Scheme = "ws"
	if u.Scheme == "https" {
		wsURL.Scheme = "wss"
	}
	wsURL.Path = "/ws"
	if wsURL.RawQuery != "" {
		return fmt.Errorf("capability leaked into WebSocket query")
	}
	protocols := []string{"persea-engine.unified-dev", "persea-history.5000", "persea-terminal.v2", "persea-handle." + wsHandle, "persea-mode." + mode, "persea-csrf." + csrf}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Subprotocols: protocols, TLSClientConfig: probeTLS}
	headers := http.Header{"Cookie": []string{cookieHeader}, "Origin": []string{base}}
	ws, wsResponse, err := dialer.Dial(wsURL.String(), headers)
	if err != nil {
		if wsResponse != nil {
			_ = wsResponse.Body.Close()
		}
		return fmt.Errorf("WebSocket upgrade: %w", err)
	}
	if ws.Subprotocol() != "persea-terminal.v2" {
		_ = ws.Close()
		return fmt.Errorf("unexpected echoed subprotocol")
	}
	_ = ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	committed, controlled, sent := false, false, false
	live := ""
	var consumed uint64
	for {
		_, payload, e := ws.ReadMessage()
		if e != nil {
			_ = ws.Close()
			return e
		}
		var frame map[string]any
		if e := json.Unmarshal(payload, &frame); e != nil {
			_ = ws.Close()
			return e
		}
		// Acknowledge each attachment frame as it is handled, as a page does:
		// the front door stops sending output a client has not acknowledged.
		consumed++
		ack, e := attachmentwire.EncodeTransportFlowAck(consumed, attachmentwire.BrowserToServer)
		if e == nil {
			e = ws.WriteMessage(websocket.TextMessage, ack)
		}
		if e != nil {
			_ = ws.Close()
			return e
		}
		typeName, _ := frame["type"].(string)
		source, _ := frame["source"].(string)
		epoch, _ := frame["epoch"].(string)
		switch typeName {
		case "PREPARE":
			ready := map[string]any{"type": "READY", "version": 1, "source": source, "epoch": epoch, "cut": frame["cut"]}
			if e := ws.WriteJSON(ready); e != nil {
				return e
			}
		case "COMMIT":
			if !committed {
				committed = true
				if mode == "observe" {
					_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "probe_complete"), time.Now().Add(time.Second))
					_ = ws.Close()
					return proveReplayRejected(&dialer, wsURL.String(), headers)
				}
				if e := ws.WriteJSON(map[string]any{"type": "MODE_REQUEST", "version": 1, "source": source, "epoch": epoch, "mode": "CONTROL"}); e != nil {
					return e
				}
			}
		case "MODE":
			controlled = frame["mode"] == "CONTROL"
			if controlled && !sent {
				sent = true
				command := "printf '" + strings.ReplaceAll(sentinel, "'", "") + "\\n'\n"
				if e := ws.WriteJSON(map[string]any{"type": "INPUT", "version": 1, "source": source, "epoch": epoch, "data": base64.StdEncoding.EncodeToString([]byte(command))}); e != nil {
					return e
				}
			}
		case "LIVE":
			encoded, _ := frame["data"].(string)
			decoded, _ := base64.StdEncoding.DecodeString(encoded)
			live += string(decoded)
			if controlled && strings.Contains(live, sentinel) {
				_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "probe_complete"), time.Now().Add(time.Second))
				_ = ws.Close()
				return proveReplayRejected(&dialer, wsURL.String(), headers)
			}
		case "END":
			return fmt.Errorf("attachment ended early")
		}
	}
}

func proveReplayRejected(dialer *websocket.Dialer, wsURL string, headers http.Header) error {
	replay, response, err := dialer.Dial(wsURL, headers)
	if replay != nil {
		_ = replay.Close()
		return fmt.Errorf("consumed capability replay opened")
	}
	if response != nil {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusGone {
		return fmt.Errorf("consumed capability replay was not rejected as stale")
	}
	return nil
}

func loopbackAuthority(authority string) bool {
	host, port, err := net.SplitHostPort(authority)
	if err != nil || port == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
