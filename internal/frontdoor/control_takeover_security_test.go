package frontdoor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const f3BaselineEndpointPrefix = "F3-W1 secure-post-matrix: secured POST /api/control-takeovers endpoint absent"
const f3BadRequestEndpointPrefix = "F3-W2 closed-request-decoder: secured POST /api/control-takeovers invalid response signature"

var f3Mutants = map[string]string{
	"W1":  "route-outside-secure-or-optional-csrf",
	"W2":  "permissive-decoder-prefer-source",
	"W3":  "takeover-purpose-collapses-to-control",
	"W4":  "record-every-failed-handle-as-offer",
	"W5":  "mint-on-retry-or-request-id-only-key",
	"W6":  "transfer-before-broker-validation",
	"W7":  "forward-broker-prepare-before-commit",
	"W8":  "broadcast-displacement-by-authority",
	"W9":  "stale-byte-crosses-input-fence",
	"W10": "first-wins-or-post-arrival-order",
	"W11": "stale-generation-renewal",
	"W12": "takeover-ttl-removed-or-server-renewed",
	"W13": "reserve-in-post-or-omit-post-transfer-release",
	"W14": "single-owner-across-modes",
	"W20": "log-request-json-subprotocol-or-raw-id",
}

type f3HTTPResult struct {
	status int
	body   []byte
	header http.Header
}

type f3EndpointTesting interface {
	Helper()
	Fatalf(string, ...any)
}

type f3EndpointFailureRecorder struct {
	message string
}

func (*f3EndpointFailureRecorder) Helper() {}

func (r *f3EndpointFailureRecorder) Fatalf(format string, args ...any) {
	r.message = fmt.Sprintf(format, args...)
}

// f3ContextualFatalf preserves the row that supplied a shared helper's context:
// shared setup must not turn a row failure into anonymous test plumbing.
func f3ContextualFatalf(t f3EndpointTesting, prefix, format string, args ...any) {
	t.Helper()
	if !strings.HasPrefix(prefix, "F3-W") {
		t.Fatalf("F3-W0 takeover-oracle-infrastructure: missing row prefix %q", prefix)
	}
	t.Fatalf(prefix+": "+format, args...)
}

func f3Authority(name string) proto.Authority {
	return proto.Authority{
		Realm: "r", Server: "s", UID: uint32(os.Getuid()),
		SelectorKind: "socket_name", SelectorValue: name,
		BootID: "f3", ServerPID: 7, ServerStart: 11,
		SessionID: "$" + name, SessionCreated: 13,
	}
}

func f3SecureServer(t *testing.T) (*Server, http.Handler, config.Front) {
	t.Helper()
	cfg := config.Front{
		Ingress: config.Ingress{
			SocketPath: "/tmp/f3.sock", PeerUID: uint32(os.Geteuid()),
			PeerUIDConfigured: true, CanonicalHost: "localhost:43210",
			OperatorLogin: "operator@example.com", MaxConnections: 64,
		},
		HandleTTLSeconds: 120, HandleCapacity: 64,
	}
	s := newServer(cfg, t.TempDir(), "localhost:43210")
	return s, s.handler(), cfg
}

func f3SecurePOST(t *testing.T, h http.Handler, cfg config.Front, body string, mutate func(*http.Request)) f3HTTPResult {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://localhost/api/control-takeovers", strings.NewReader(body))
	r.Host = "localhost"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-Host", cfg.Ingress.CanonicalHost)
	r.Header.Set("X-Forwarded-Proto", strings.TrimSuffix(externalOrigin(cfg.Ingress), "://"+cfg.Ingress.CanonicalHost))
	r.Header.Set("Tailscale-User-Login", cfg.Ingress.OperatorLogin)
	r.Header.Set("Origin", externalOrigin(cfg.Ingress))
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Cookie", csrfCookie+"="+testCSRF)
	r.Header.Set("X-Persea-CSRF", testCSRF)
	r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: cfg.Ingress.PeerUID}))
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return f3HTTPResult{status: w.Code, body: w.Body.Bytes(), header: w.Header()}
}

func f3RequireCodeEndpoint(t f3EndpointTesting, got f3HTTPResult, wantStatus int, prefix, mutant string) {
	t.Helper()
	if reason := f3CodeEndpointSignatureFailure(got, wantStatus); reason != "" {
		f3ContextualFatalf(t, prefix,
			"status=%d content_type=%q cache_control=%q body_bytes=%d reason=%s mutant=%s",
			got.status,
			got.header.Get("Content-Type"),
			got.header.Get("Cache-Control"),
			len(got.body),
			reason,
			mutant,
		)
	}
}

func f3RequireForbidden(t f3EndpointTesting, got f3HTTPResult) {
	t.Helper()
	contentType := got.header.Get("Content-Type")
	if got.status != http.StatusForbidden || !strings.HasPrefix(contentType, "text/plain") || !bytes.Equal(got.body, []byte("forbidden\n")) {
		t.Fatalf(
			"F3-W1 secure-post-matrix: status=%d content_type=%q body=%q want=403/text-plain/forbidden mutant=%s",
			got.status,
			contentType,
			got.body,
			f3Mutants["W1"],
		)
	}
}

func f3RequireHandleEndpoint(t f3EndpointTesting, got f3HTTPResult, wantStatus int, prefix, mutant string) {
	t.Helper()
	if reason := f3HandleEndpointSignatureFailure(got, wantStatus); reason != "" {
		f3ContextualFatalf(t, prefix,
			"status=%d content_type=%q cache_control=%q body_bytes=%d reason=%s mutant=%s",
			got.status,
			got.header.Get("Content-Type"),
			got.header.Get("Cache-Control"),
			len(got.body),
			reason,
			mutant,
		)
	}
}

func f3EndpointFieldValue(got f3HTTPResult, wantStatus int, wantField string) (string, string) {
	if got.status != wantStatus {
		return "", "wrong-status"
	}
	if got.header.Get("Content-Type") != "application/json" {
		return "", "wrong-content-type"
	}
	if got.header.Get("Cache-Control") != "no-store" {
		return "", "wrong-cache-control"
	}
	decoder := json.NewDecoder(bytes.NewReader(got.body))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return "", "non-json-object"
	}
	if !decoder.More() {
		return "", "missing-field"
	}
	key, err := decoder.Token()
	if err != nil || key != wantField {
		return "", "wrong-field"
	}
	var value string
	if err := decoder.Decode(&value); err != nil {
		return "", "non-string-field"
	}
	if decoder.More() {
		return "", "extra-field"
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return "", "malformed-object"
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", "trailing-json"
	}
	return value, ""
}

func f3CodeEndpointSignatureFailure(got f3HTTPResult, wantStatus int) string {
	code, reason := f3EndpointFieldValue(got, wantStatus, "code")
	if reason != "" {
		return reason
	}
	if code == "" {
		return "empty-code"
	}
	return ""
}

func f3HandleEndpointSignatureFailure(got f3HTTPResult, wantStatus int) string {
	handle, reason := f3EndpointFieldValue(got, wantStatus, "handle")
	if reason != "" {
		return reason
	}
	if len(handle) != 43 {
		return "wrong-handle-length"
	}
	raw, err := base64.RawURLEncoding.DecodeString(handle)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != handle {
		return "invalid-handle-base64url"
	}
	return ""
}

func f3EndpointSignatureFailure(got f3HTTPResult) string {
	return f3CodeEndpointSignatureFailure(got, http.StatusGone)
}

func f3EndpointSignatureFalsifierControls(t *testing.T, cfg config.Front, claim string) {
	t.Helper()
	html := http.NewServeMux()
	html.HandleFunc("/api/control-takeovers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<!doctype html><title>SPA fallback</title>")
	})
	if reason := f3EndpointSignatureFailure(f3SecurePOST(t, html, cfg, claim, nil)); reason == "" {
		t.Fatal("F3-W1 falsifier control: registered 200 HTML endpoint was admitted")
	}

	gone := http.NewServeMux()
	gone.HandleFunc("/api/control-takeovers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, `{"code":"nonempty"}`)
	})
	if reason := f3EndpointSignatureFailure(f3SecurePOST(t, gone, cfg, claim, nil)); reason != "" {
		t.Fatalf("F3-W1 falsifier control: valid 410 JSON endpoint was rejected: %s", reason)
	}
}

func f3EndpointHelperMappingFalsifierControl(t *testing.T, cfg config.Front, claim string) {
	t.Helper()
	handle := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	created := http.NewServeMux()
	created.HandleFunc("/api/control-takeovers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"handle":"%s"}`, handle))
	})
	got := f3SecurePOST(t, created, cfg, claim, nil)
	red := &f3EndpointFailureRecorder{}
	f3RequireCodeEndpoint(red, got, http.StatusGone, f3BaselineEndpointPrefix, f3Mutants["W1"])
	if !strings.HasPrefix(red.message, f3BaselineEndpointPrefix+":") {
		t.Fatalf("F3-W1 helper-mapping contradiction control: contextual signature=%q", red.message)
	}
	f3RequireHandleEndpoint(t, got, http.StatusCreated, "F3-W1 helper-mapping contradiction control", f3Mutants["W1"])
}

func f3RandomClaimToken(t *testing.T) string {
	t.Helper()
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("F3-W1 fixture random token: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func f3Post(addr string, claim map[string]string) (f3HTTPResult, error) {
	body, err := json.Marshal(claim)
	if err != nil {
		return f3HTTPResult{}, err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/control-takeovers", bytes.NewReader(body))
	if err != nil {
		return f3HTTPResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://"+addr)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-Persea-CSRF", testCSRF)
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: testCSRF})
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return f3HTTPResult{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return f3HTTPResult{status: response.StatusCode, body: data, header: response.Header.Clone()}, err
}

func f3GateControl(t *testing.T, origin, pathname string, body any) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, origin+pathname, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("F3-W0 takeover-oracle-infrastructure: gate %s status=%d", pathname, response.StatusCode)
	}
}

func f3StageOffer(t *testing.T, name, prefix, mutant string) (*Server, *fakeAttachBroker, string, string, *websocket.Conn, func()) {
	t.Helper()
	broker := startFakeAttachBroker(t, false)
	front, addr := startFrontHTTP(t, broker.listener.Addr().String())
	handle, err := front.handles.mint(f3Authority(name), front.cfg.Ingress.OperatorLogin, "control")
	if err != nil {
		t.Fatal(err)
	}
	control := f3DialTerminalWS(t, addr, handle, "control", prefix, mutant)
	if got := readWSAttachment(t, control); got.Type != terminal.FrameLive {
		f3ContextualFatalf(t, prefix, "incumbent frame=%+v mutant=%s", got, mutant)
	}
	handle, err = front.handles.mint(f3Authority(name), front.cfg.Ingress.OperatorLogin, "control")
	if err != nil {
		t.Fatal(err)
	}
	refused := f3DialTerminalWS(t, addr, handle, "control", prefix, mutant)
	if reason := readWSCloseReason(t, refused); reason != "lease_held" {
		f3ContextualFatalf(t, prefix, "refusal=%q mutant=%s", reason, mutant)
	}
	_ = refused.Close()
	return front, broker, addr, handle, control, func() { _ = control.Close() }
}

func f3StageOfferWithGate(t *testing.T, name, prefix, mutant string) (*Server, *fakeAttachBroker, string, string, *websocket.Conn, *f3BrowserOracleProvisioner, func()) {
	t.Helper()
	broker := startFakeAttachBroker(t, false)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	cfg := currentFrontFixtureConfig(addr, broker.listener.Addr().String())
	front := newServer(cfg, ".", addr)
	gate := &f3BrowserOracleProvisioner{front: front, next: front.handler()}
	startJoinedHTTPServer(t, listener, f3BrowserOracleTrustedHop(gate, cfg.Ingress))
	handle, err := front.handles.mint(f3Authority(name), front.cfg.Ingress.OperatorLogin, "control")
	if err != nil {
		t.Fatal(err)
	}
	control := f3DialTerminalWS(t, addr, handle, "control", prefix, mutant)
	if got := readWSAttachment(t, control); got.Type != terminal.FrameLive {
		f3ContextualFatalf(t, prefix, "incumbent frame=%+v mutant=%s", got, mutant)
	}
	handle, err = front.handles.mint(f3Authority(name), front.cfg.Ingress.OperatorLogin, "control")
	if err != nil {
		t.Fatal(err)
	}
	refused := f3DialTerminalWS(t, addr, handle, "control", prefix, mutant)
	if reason := readWSCloseReason(t, refused); reason != "lease_held" {
		f3ContextualFatalf(t, prefix, "refusal=%q mutant=%s", reason, mutant)
	}
	_ = refused.Close()
	return front, broker, addr, handle, control, gate, func() { _ = control.Close() }
}

func TestControlTakeoverSecurePostMatrixW1(t *testing.T) {
	_, handler, cfg := f3SecureServer(t)
	claim := fmt.Sprintf(`{"request_id":"%s","offer":"%s"}`, f3RandomClaimToken(t), f3RandomClaimToken(t))
	f3EndpointSignatureFalsifierControls(t, cfg, claim)
	f3EndpointHelperMappingFalsifierControl(t, cfg, claim)
	pristine := f3SecurePOST(t, handler, cfg, claim, nil)
	f3RequireCodeEndpoint(t, pristine, http.StatusGone, f3BaselineEndpointPrefix, f3Mutants["W1"])
	mutations := []struct {
		name      string
		forbidden bool
		fn        func(*http.Request)
	}{
		{"untrusted-peer", true, func(r *http.Request) { *r = *r.WithContext(context.Background()) }},
		{"host", true, func(r *http.Request) { r.Host = "evil.example" }},
		{"forwarded-host", true, func(r *http.Request) { r.Header.Del("X-Forwarded-Host") }},
		{"forwarded-proto", true, func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }},
		{"operator", true, func(r *http.Request) { r.Header.Del("Tailscale-User-Login") }},
		{"origin", true, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }},
		{"fetch-site", true, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }},
		{"csrf-cookie", true, func(r *http.Request) { r.Header.Del("Cookie") }},
		{"csrf-header", true, func(r *http.Request) { r.Header.Del("X-Persea-CSRF") }},
		{"csrf-wrong", true, func(r *http.Request) { r.Header.Set("X-Persea-CSRF", strings.Repeat("X", 43)) }},
		{"unknown-tailscale", true, func(r *http.Request) { r.Header.Set("Tailscale-Evil", "x") }},
		{"duplicate-origin", true, func(r *http.Request) { r.Header.Add("Origin", externalOrigin(cfg.Ingress)) }},
		{"duplicate-operator", true, func(r *http.Request) { r.Header.Add("Tailscale-User-Login", cfg.Ingress.OperatorLogin) }},
		{"wrong-method-tunnel", false, func(r *http.Request) { r.Header.Set("X-HTTP-Method-Override", "GET") }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			got := f3SecurePOST(t, handler, cfg, claim, mutation.fn)
			if mutation.forbidden {
				f3RequireForbidden(t, got)
			} else {
				f3RequireCodeEndpoint(t, got, http.StatusGone, f3BaselineEndpointPrefix, f3Mutants["W1"])
			}
			after := f3SecurePOST(t, handler, cfg, claim, nil)
			if after.status != pristine.status || !bytes.Equal(after.body, pristine.body) {
				t.Fatalf("F3-W1 secure-post-matrix: denied request changed observable state mutant=%s", f3Mutants["W1"])
			}
		})
	}
}

func TestControlTakeoverClosedDecoderW2(t *testing.T) {
	_, handler, cfg := f3SecureServer(t)
	r := strings.Repeat("R", 43)
	o := strings.Repeat("O", 43)
	cases := []string{
		"{}", `{"request_id":"` + r + `"}`,
		`{"request_id":"` + r + `","offer":"` + o + `","source":"` + o + `"}`,
		`{"request_id":"` + r + `","offer":"` + o + `","unknown":1}`,
		`{"request_id":"` + r + `","request_id":"` + r + `","offer":"` + o + `"}`,
		`{"request_id":"short","offer":"` + o + `"}`,
		`{"request_id":"` + r + `","offer":"short"}`,
		`{"request_id":"` + r + `","offer":7}`,
		`{"request_id":"` + r + `","offer":"` + o + `"} trailing`,
	}
	for index, body := range cases {
		got := f3SecurePOST(t, handler, cfg, body, nil)
		f3RequireCodeEndpoint(t, got, http.StatusBadRequest, f3BadRequestEndpointPrefix, f3Mutants["W2"])
		if got.status != http.StatusBadRequest {
			t.Fatalf("F3-W2 closed-request-decoder: case=%d status=%d mutant=%s", index, got.status, f3Mutants["W2"])
		}
	}
}

func TestControlTakeoverPurposeOfferIdempotencyW3W4W5(t *testing.T) {
	front, _, addr, offer, _, gate, cleanup := f3StageOfferWithGate(t, "purpose", "F3-W4 offer-provenance", f3Mutants["W4"])
	defer cleanup()
	requestID := strings.Repeat("I", 43)
	first, err := f3Post(addr, map[string]string{"request_id": requestID, "offer": offer})
	if err != nil {
		t.Fatal(err)
	}
	f3RequireHandleEndpoint(t, first, http.StatusCreated, "F3-W4 offer-provenance: secured POST /api/control-takeovers authorization response signature", f3Mutants["W4"])
	if first.status != http.StatusCreated {
		t.Fatalf("F3-W4 offer-provenance: status=%d body=%q mutant=%s", first.status, first.body, f3Mutants["W4"])
	}
	var minted struct {
		Handle string `json:"handle"`
	}
	if json.Unmarshal(first.body, &minted) != nil || len(minted.Handle) != 43 {
		t.Fatalf("F3-W3 purpose-separation: invalid takeover capability mutant=%s", f3Mutants["W3"])
	}
	if _, err := front.handles.resolve(minted.Handle); err == nil {
		t.Fatalf("F3-W3 purpose-separation: takeover capability resolved as normal control mutant=%s", f3Mutants["W3"])
	}
	const concurrent = 32
	f3GateControl(t, "http://"+addr, "/__f3_oracle/gate/arm", f3TakeoverGateArm{HoldAll: true, MaxParkMillis: 3000})
	results := make(chan f3HTTPResult, concurrent)
	ready := make(chan struct{}, concurrent)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			got, postErr := f3Post(addr, map[string]string{"request_id": requestID, "offer": offer})
			if postErr == nil {
				results <- got
			}
		}()
	}
	for range concurrent {
		<-ready
	}
	close(start)
	deadline := time.Now().Add(3 * time.Second)
	for {
		gate.gate.mu.Lock()
		parked := gate.gate.parked
		timeouts := gate.gate.timeouts
		gate.gate.mu.Unlock()
		if timeouts != 0 || parked == concurrent || time.Now().After(deadline) {
			if parked != concurrent || timeouts != 0 {
				f3GateControl(t, "http://"+addr, "/__f3_oracle/gate/release", f3TakeoverGateRelease{All: true})
				wg.Wait()
				t.Fatalf("F3-W5 post-idempotency: parked=%d want=%d timeout=%d overlap-boundary absent mutant=%s", parked, concurrent, timeouts, f3Mutants["W5"])
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Every request crossed the start barrier and parked behind the real gate
	// before release.  Keep the witnessed overlap in the verbose test record;
	// a sequential retry cannot satisfy this count.
	t.Logf("F3-W5 overlap-witness: released=%d parked=%d timeouts=0", concurrent, concurrent)
	f3GateControl(t, "http://"+addr, "/__f3_oracle/gate/release", f3TakeoverGateRelease{All: true})
	wg.Wait()
	close(results)
	identical := 0
	for got := range results {
		f3RequireHandleEndpoint(t, got, http.StatusOK, "F3-W5 post-idempotency: secured POST /api/control-takeovers replay response signature", f3Mutants["W5"])
		var replay struct {
			Handle string `json:"handle"`
		}
		if err := json.Unmarshal(got.body, &replay); err != nil || len(replay.Handle) != 43 || replay.Handle != minted.Handle {
			t.Fatalf("F3-W5 post-idempotency: status=%d body=%q mutant=%s", got.status, got.body, f3Mutants["W5"])
		}
		identical++
	}
	if identical != concurrent {
		t.Fatalf("F3-W5 post-idempotency: identical=%d want=%d mutant=%s", identical, concurrent, f3Mutants["W5"])
	}
	changed, err := f3Post(addr, map[string]string{"request_id": requestID, "offer": strings.Repeat("Z", 43)})
	if err != nil || changed.status != http.StatusConflict {
		t.Fatalf("F3-W5 post-idempotency: changed-claim status=%d err=%v mutant=%s", changed.status, err, f3Mutants["W5"])
	}
}

func TestControlTakeoverAuditPrivacyW20(t *testing.T) {
	var logs bytes.Buffer
	oldLog := frontLogf
	frontLogf = log.New(&logs, "", 0).Printf
	t.Cleanup(func() { frontLogf = oldLog })
	_, handler, cfg := f3SecureServer(t)
	requestID := strings.Repeat("Q", 43)
	offer := strings.Repeat("S", 43)
	got := f3SecurePOST(t, handler, cfg, fmt.Sprintf(`{"request_id":"%s","offer":"%s"}`, requestID, offer), nil)
	f3RequireCodeEndpoint(t, got, http.StatusGone, "F3-W20 audit-privacy: secured POST /api/control-takeovers refusal response signature", f3Mutants["W20"])
	for _, secret := range []string{requestID, offer, testCSRF, "persea-takeover.v1"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("F3-W20 audit-privacy: secret=%q mutant=%s", secret, f3Mutants["W20"])
		}
	}
}

func TestControlTakeoverSecurityFixtureDoesNotChangeTTL(t *testing.T) {
	if LeaseTTL != 60*time.Second {
		t.Fatalf("F3-W12 ttl-preservation: LeaseTTL=%s mutant=%s", LeaseTTL, f3Mutants["W12"])
	}
}
