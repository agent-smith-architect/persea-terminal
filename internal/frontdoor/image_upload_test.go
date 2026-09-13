package frontdoor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

const oracleImageRealm = "desk-a7"

var oracleCSRF = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32))

type oracleBrokerResult struct {
	control proto.Control
	payload []byte
	err     error
}

type oracleBrokerReply struct {
	control           proto.Control
	wrongType         bool
	malformed         []byte
	closeWithoutReply bool
	awaitClose        bool
	reached           chan<- struct{}
}

func startOracleImageBroker(t *testing.T, reply oracleBrokerReply) (string, <-chan oracleBrokerResult) {
	t.Helper()
	dir := shortTestDir(t)
	socket := filepath.Join(dir, "image.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan oracleBrokerResult, 1)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- oracleBrokerResult{err: err}
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		frame, err := proto.ReadFrame(conn)
		if err != nil || frame.Type != proto.FrameControl {
			result <- oracleBrokerResult{err: fmt.Errorf("hello frame: %w", err)}
			return
		}
		hello, err := proto.DecodeControl(frame.Payload)
		if err != nil || hello.Type != "hello" || hello.V != 1 {
			result <- oracleBrokerResult{err: fmt.Errorf("hello control: %w", err)}
			return
		}
		if err := writeControl(conn, proto.Control{Type: "hello_ok", V: 1}); err != nil {
			result <- oracleBrokerResult{err: err}
			return
		}
		frame, err = proto.ReadFrame(conn)
		if err != nil || frame.Type != proto.FrameControl {
			result <- oracleBrokerResult{err: fmt.Errorf("stage frame: %w", err)}
			return
		}
		stage, err := proto.DecodeControl(frame.Payload)
		if err != nil || stage.Type != proto.ControlImageStage {
			result <- oracleBrokerResult{err: fmt.Errorf("stage control: %w", err)}
			return
		}
		frame, err = proto.ReadFrame(conn)
		if err != nil || frame.Type != proto.FrameImage {
			result <- oracleBrokerResult{err: fmt.Errorf("image frame: %w", err)}
			return
		}
		observed := oracleBrokerResult{control: stage, payload: append([]byte(nil), frame.Payload...)}
		if stage.Bytes != len(frame.Payload) {
			observed.err = fmt.Errorf("declared bytes %d != payload %d", stage.Bytes, len(frame.Payload))
			result <- observed
			return
		}
		if reply.awaitClose {
			if reply.reached != nil {
				close(reply.reached)
			}
			one := make([]byte, 1)
			_, observed.err = conn.Read(one)
			result <- observed
			return
		}
		if reply.closeWithoutReply {
			result <- observed
			return
		}
		if reply.wrongType {
			err = proto.WriteFrame(conn, proto.FrameImage, []byte("not-control"))
		} else if reply.malformed != nil {
			err = proto.WriteFrame(conn, proto.FrameControl, reply.malformed)
		} else {
			err = writeControl(conn, reply.control)
		}
		observed.err = err
		result <- observed
	}()
	return socket, result
}

func oracleFrontConfig(socket string, max int) config.Front {
	return config.Front{
		Ingress: config.Ingress{
			SocketPath: "/tmp/persea-front-oracle.sock", PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true,
			CanonicalHost: "localhost:43210", OperatorLogin: "operator@example.com", MaxConnections: 64,
		},
		Realms: []config.Realm{{
			Name: oracleImageRealm, Socket: socket, BrokerUID: uint32(os.Geteuid()), BrokerUIDConfigured: true,
		}},
		AliasStorePath:      filepath.Join(os.TempDir(), "persea-image-oracle-aliases.json"),
		ImageUploadMaxBytes: max,
		HandleTTLSeconds:    120,
		HandleCapacity:      64,
	}
}

func oracleImageRequest(t *testing.T, server *Server, method, target, mediaType string, body io.Reader, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, body)
	r.Host = "localhost"
	r.Header.Set("X-Forwarded-Host", server.cfg.Ingress.CanonicalHost)
	r.Header.Set("X-Forwarded-Proto", "http")
	r.Header.Set("Tailscale-User-Login", server.cfg.Ingress.OperatorLogin)
	r.Header.Set("Origin", externalOrigin(server.cfg.Ingress))
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Cookie", csrfCookie+"="+oracleCSRF)
	r.Header.Set("X-Persea-CSRF", oracleCSRF)
	if mediaType != "" {
		r.Header.Set("Content-Type", mediaType)
	}
	r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: server.cfg.Ingress.PeerUID}))
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	server.handler().ServeHTTP(w, r)
	return w
}

func oraclePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func oracleJPEG(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := jpeg.Encode(&out, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func oracleGIF(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := gif.Encode(&out, image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{color.Black}), nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func oracleWebP() []byte {
	return []byte{'R', 'I', 'F', 'F', 4, 0, 0, 0, 'W', 'E', 'B', 'P', 'V', 'P', '8', ' '}
}

func oracleStagedControl(mediaType string, size int) proto.Control {
	ext := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp"}[mediaType]
	id := "0123456789abcdef0123456789abcdef"
	return proto.Control{
		Type: proto.ControlImageStaged, ID: id,
		Path:      filepath.Join(config.DefaultImageStagingRoot, oracleImageRealm, "img-"+id+"."+ext),
		ExpiresAt: "2026-07-31T18:04:11.2Z",
	}
}

func assertOracleNoStoreJSON(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d want=%d body=%q", response.Code, status, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q", got)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q", got)
	}
}

func assertOracleError(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d want=%d body=%q", response.Code, status, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q", got)
	}
}

func TestImageUploadOracleHarnessPositiveControl(t *testing.T) {
	payload := oraclePNG(t, 1, 1)
	want := oracleStagedControl("image/png", len(payload))
	socket, observed := startOracleImageBroker(t, oracleBrokerReply{control: want})
	conn, err := dial(socket, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := hello(conn); err != nil {
		t.Fatal(err)
	}
	if err := writeControl(conn, proto.Control{Type: proto.ControlImageStage, MediaType: "image/png", Bytes: len(payload)}); err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteFrame(conn, proto.FrameImage, payload); err != nil {
		t.Fatal(err)
	}
	frame, err := proto.ReadFrame(conn)
	if err != nil || frame.Type != proto.FrameControl {
		t.Fatalf("response frame=%v err=%v", frame.Type, err)
	}
	got, err := proto.DecodeControl(frame.Payload)
	if err != nil || got.Type != proto.ControlImageStaged || got.Path != want.Path || got.ID != want.ID || got.ExpiresAt != want.ExpiresAt {
		t.Fatalf("response=%+v err=%v", got, err)
	}
	if seen := <-observed; seen.err != nil || seen.control.MediaType != "image/png" || !bytes.Equal(seen.payload, payload) {
		t.Fatalf("broker witness=%+v", seen)
	}
}

func TestImageUploadRouteGateAndAuthority(t *testing.T) {
	payload := oraclePNG(t, 1, 1)
	base := oracleFrontConfig("/does/not/matter.sock", 1<<20)
	disabled := base
	disabled.ImageUploadMaxBytes = 0
	server := newServer(disabled, t.TempDir(), "localhost")
	response := oracleImageRequest(t, server, http.MethodPost, "http://localhost/api/session-images?realm="+oracleImageRealm, "image/png", bytes.NewReader(payload), nil)
	if response.Code != http.StatusMethodNotAllowed || strings.Contains(response.Header().Get("Allow"), http.MethodPost) {
		t.Fatalf("disabled route exists: status=%d allow=%q", response.Code, response.Header().Get("Allow"))
	}

	for name, mutate := range map[string]func(*http.Request){
		"missing_identity": func(r *http.Request) { *r = *r.WithContext(context.Background()) },
		"wrong_uid": func(r *http.Request) {
			*r = *r.WithContext(context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: base.Ingress.PeerUID + 1}))
		},
		"wrong_operator":              func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "other@example.com") },
		"wrong_origin":                func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"missing_csrf":                func(r *http.Request) { r.Header.Del("X-Persea-CSRF") },
		"unexpected_tailscale_header": func(r *http.Request) { r.Header.Set("Tailscale-Evil", "1") },
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(base, t.TempDir(), "localhost")
			got := oracleImageRequest(t, s, http.MethodPost, "http://localhost/api/session-images?realm="+oracleImageRealm, "image/png", bytes.NewReader(payload), mutate)
			if got.Code != http.StatusForbidden {
				t.Fatalf("status=%d want=403 body=%q", got.Code, got.Body.String())
			}
		})
	}
}

type oracleImageStageHandler interface {
	stageSessionImage(http.ResponseWriter, *http.Request)
}

func TestImageUploadHandlerRechecksIdentityWithoutSecureWrapper(t *testing.T) {
	payload := oraclePNG(t, 1, 1)
	cfg := oracleFrontConfig(filepath.Join(t.TempDir(), "missing.sock"), 1<<20)
	server := newServer(cfg, t.TempDir(), "localhost")
	handler, ok := any(server).(oracleImageStageHandler)
	if !ok {
		t.Fatal("Server has no stageSessionImage handler for the defence-in-depth identity oracle")
	}
	for name, identityValue := range map[string]any{
		"missing":        nil,
		"wrong_uid":      ingressIdentity{uid: cfg.Ingress.PeerUID + 1, operator: cfg.Ingress.OperatorLogin},
		"wrong_operator": ingressIdentity{uid: cfg.Ingress.PeerUID, operator: "other@example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://localhost/api/session-images?realm="+oracleImageRealm, bytes.NewReader(payload))
			r.Header.Set("Content-Type", "image/png")
			if identityValue != nil {
				r = r.WithContext(context.WithValue(r.Context(), ingressKey{}, identityValue))
			}
			w := httptest.NewRecorder()
			handler.stageSessionImage(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status=%d want=403 body=%q", w.Code, w.Body.String())
			}
		})
	}
}

func TestImageUploadEndpointStagesAllowedTypes(t *testing.T) {
	for mediaType, payload := range map[string][]byte{
		"image/png": oraclePNG(t, 1, 1), "image/jpeg": oracleJPEG(t), "image/gif": oracleGIF(t), "image/webp": oracleWebP(),
	} {
		t.Run(mediaType, func(t *testing.T) {
			want := oracleStagedControl(mediaType, len(payload))
			socket, observed := startOracleImageBroker(t, oracleBrokerReply{control: want})
			server := newServer(oracleFrontConfig(socket, 1<<20), t.TempDir(), "localhost")
			response := oracleImageRequest(t, server, http.MethodPost, "http://localhost/api/session-images?realm="+oracleImageRealm, mediaType, bytes.NewReader(payload), func(r *http.Request) {
				r.URL.Fragment = "realm=wrong&filename=private.png"
			})
			assertOracleNoStoreJSON(t, response, http.StatusCreated)
			var body struct {
				ID        string `json:"id"`
				Path      string `json:"path"`
				Bytes     int    `json:"bytes"`
				MediaType string `json:"media_type"`
				ExpiresAt string `json:"expires_at"`
			}
			decoder := json.NewDecoder(response.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
				t.Fatalf("response JSON: %v body=%q", err, response.Body.String())
			}
			if body.ID != want.ID || body.Path != want.Path || body.Bytes != len(payload) || body.MediaType != mediaType || body.ExpiresAt != want.ExpiresAt {
				t.Fatalf("response=%+v want control=%+v bytes=%d media=%q", body, want, len(payload), mediaType)
			}
			seen := <-observed
			if seen.err != nil || seen.control.MediaType != mediaType || seen.control.Bytes != len(payload) || !bytes.Equal(seen.payload, payload) {
				t.Fatalf("broker witness=%+v", seen)
			}
		})
	}
}

func TestImageUploadRejectsRoutingAndContentErrorsBeforeBroker(t *testing.T) {
	valid := oraclePNG(t, 1, 1)
	server := newServer(oracleFrontConfig(filepath.Join(t.TempDir(), "missing.sock"), 1<<20), t.TempDir(), "localhost")
	for name, tc := range map[string]struct {
		target, media string
		body          []byte
		status        int
	}{
		"missing_realm":          {"http://localhost/api/session-images", "image/png", valid, 400},
		"unknown_realm":          {"http://localhost/api/session-images?realm=other", "image/png", valid, 400},
		"duplicate_realm":        {"http://localhost/api/session-images?realm=" + oracleImageRealm + "&realm=" + oracleImageRealm, "image/png", valid, 400},
		"unexpected_query":       {"http://localhost/api/session-images?realm=" + oracleImageRealm + "&filename=x.png", "image/png", valid, 400},
		"missing_content_type":   {"http://localhost/api/session-images?realm=" + oracleImageRealm, "", valid, 415},
		"octet_stream":           {"http://localhost/api/session-images?realm=" + oracleImageRealm, "application/octet-stream", valid, 415},
		"heic":                   {"http://localhost/api/session-images?realm=" + oracleImageRealm, "image/heic", valid, 415},
		"malformed_content_type": {"http://localhost/api/session-images?realm=" + oracleImageRealm, "image/png; bad", valid, 415},
		"declared_mismatch":      {"http://localhost/api/session-images?realm=" + oracleImageRealm, "image/jpeg", valid, 415},
		"empty":                  {"http://localhost/api/session-images?realm=" + oracleImageRealm, "image/png", nil, 415},
		"invalid_png":            {"http://localhost/api/session-images?realm=" + oracleImageRealm, "image/png", []byte("not a png"), 415},
		"dimension_bomb":         {"http://localhost/api/session-images?realm=" + oracleImageRealm, "image/png", oraclePNG(t, 16385, 1), 415},
	} {
		t.Run(name, func(t *testing.T) {
			got := oracleImageRequest(t, server, http.MethodPost, tc.target, tc.media, bytes.NewReader(tc.body), nil)
			if got.Code != tc.status || got.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d want=%d cache=%q body=%q", got.Code, tc.status, got.Header().Get("Cache-Control"), got.Body.String())
			}
		})
	}
}

type oracleCountingReader struct {
	remaining int
	read      int
}

type oracleErrorReader struct{ delivered bool }

func (r *oracleErrorReader) Read(p []byte) (int, error) {
	if !r.delivered {
		r.delivered = true
		copy(p, []byte("partial"))
		return len("partial"), nil
	}
	return 0, errors.New("forced body read failure")
}

func (r *oracleCountingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = 0
	}
	r.remaining -= n
	r.read += n
	return n, nil
}

func TestImageUploadMaxBytesReaderStopsAtCap(t *testing.T) {
	const capBytes = 1 << 20
	reader := &oracleCountingReader{remaining: capBytes + 65536}
	server := newServer(oracleFrontConfig(filepath.Join(t.TempDir(), "missing.sock"), capBytes), t.TempDir(), "localhost")
	response := oracleImageRequest(t, server, http.MethodPost, "http://localhost/api/session-images?realm="+oracleImageRealm, "image/png", reader, nil)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want=413 body=%q", response.Code, response.Body.String())
	}
	if reader.read > capBytes+1 {
		t.Fatalf("body buffered past MaxBytesReader proof byte: read=%d cap=%d", reader.read, capBytes)
	}
}

func TestImageUploadBodyReadFailureIsBadRequest(t *testing.T) {
	server := newServer(oracleFrontConfig(filepath.Join(t.TempDir(), "missing.sock"), 1<<20), t.TempDir(), "localhost")
	response := oracleImageRequest(t, server, http.MethodPost, "http://localhost/api/session-images?realm="+oracleImageRealm, "image/png", &oracleErrorReader{}, nil)
	if response.Code != http.StatusBadRequest || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d want=400 cache=%q body=%q", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
}

func TestImageUploadBrokerRefusalMapping(t *testing.T) {
	payload := oraclePNG(t, 1, 1)
	for code, status := range map[string]int{
		proto.ImageRefusalImagesDisabled:  503,
		proto.ImageRefusalTooLarge:        413,
		proto.ImageRefusalUnsupportedType: 415,
		proto.ImageRefusalCapacity:        507,
		proto.ImageRefusalIO:              503,
	} {
		t.Run(code, func(t *testing.T) {
			socket, observed := startOracleImageBroker(t, oracleBrokerReply{control: proto.Control{Type: proto.ControlImageRefused, Code: code}})
			server := newServer(oracleFrontConfig(socket, 1<<20), t.TempDir(), "localhost")
			response := oracleImageRequest(t, server, http.MethodPost, "http://localhost/api/session-images?realm="+oracleImageRealm, "image/png", bytes.NewReader(payload), nil)
			assertOracleError(t, response, status)
			if seen := <-observed; seen.err != nil {
				t.Fatalf("broker witness: %v", seen.err)
			}
		})
	}
}

func TestImageUploadBrokerFailuresAreUnavailable(t *testing.T) {
	payload := oraclePNG(t, 1, 1)
	for name, reply := range map[string]oracleBrokerReply{
		"wrong_frame":         {wrongType: true},
		"malformed_control":   {malformed: []byte(`{"type":"image_staged","path":1}`)},
		"unexpected_control":  {control: proto.Control{Type: "inventory_ok"}},
		"eof_before_response": {closeWithoutReply: true},
	} {
		t.Run(name, func(t *testing.T) {
			socket, observed := startOracleImageBroker(t, reply)
			server := newServer(oracleFrontConfig(socket, 1<<20), t.TempDir(), "localhost")
			response := oracleImageRequest(t, server, http.MethodPost, "http://localhost/api/session-images?realm="+oracleImageRealm, "image/png", bytes.NewReader(payload), nil)
			if response.Code != http.StatusServiceUnavailable || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d cache=%q body=%q", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
			}
			if seen := <-observed; seen.err != nil {
				t.Fatalf("broker witness: %v", seen.err)
			}
		})
	}

	server := newServer(oracleFrontConfig(filepath.Join(t.TempDir(), "missing.sock"), 1<<20), t.TempDir(), "localhost")
	response := oracleImageRequest(t, server, http.MethodPost, "http://localhost/api/session-images?realm="+oracleImageRealm, "image/png", bytes.NewReader(payload), nil)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestImageUploadCancellationClosesBrokerConnection(t *testing.T) {
	payload := oraclePNG(t, 1, 1)
	reached := make(chan struct{})
	socket, observed := startOracleImageBroker(t, oracleBrokerReply{awaitClose: true, reached: reached})
	server := newServer(oracleFrontConfig(socket, 1<<20), t.TempDir(), "localhost")
	var cancel context.CancelFunc
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- oracleImageRequest(t, server, http.MethodPost, "http://localhost/api/session-images?realm="+oracleImageRealm, "image/png", bytes.NewReader(payload), func(r *http.Request) {
			var ctx context.Context
			ctx, cancel = context.WithCancel(r.Context())
			*r = *r.WithContext(ctx)
		})
	}()
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("broker did not receive the staged image")
	}
	cancel()
	select {
	case response := <-done:
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("cancel status=%d", response.Code)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancelled request did not close its broker exchange promptly")
	}
	if seen := <-observed; seen.err == nil {
		t.Fatal("broker write unexpectedly survived request cancellation")
	}
}

func TestImageUploadLogsOnlyMetadata(t *testing.T) {
	payload := oraclePNG(t, 1, 1)
	want := oracleStagedControl("image/png", len(payload))
	socket, observed := startOracleImageBroker(t, oracleBrokerReply{control: want})
	server := newServer(oracleFrontConfig(socket, 1<<20), t.TempDir(), "localhost")
	var mu sync.Mutex
	var lines []string
	previous := frontLogf
	frontLogf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { frontLogf = previous })
	response := oracleImageRequest(t, server, http.MethodPost, "http://localhost/api/session-images?realm="+oracleImageRealm, "image/png", bytes.NewReader(payload), nil)
	assertOracleNoStoreJSON(t, response, http.StatusCreated)
	if seen := <-observed; seen.err != nil {
		t.Fatal(seen.err)
	}
	mu.Lock()
	logText := strings.Join(lines, "\n")
	mu.Unlock()
	hash := sha256.Sum256([]byte(want.ID))
	for _, required := range []string{"realm=\"" + oracleImageRealm + "\"", fmt.Sprintf("bytes=%d", len(payload)), "media_type=\"image/png\"", "id_hash=\"" + hex.EncodeToString(hash[:6]) + "\""} {
		if !strings.Contains(logText, required) {
			t.Fatalf("log missing %q: %s", required, logText)
		}
	}
	for _, forbidden := range []string{want.Path, want.ID, base64.StdEncoding.EncodeToString(payload), "filename"} {
		if strings.Contains(logText, forbidden) {
			t.Fatalf("log leaked %q: %s", forbidden, logText)
		}
	}
}

func TestImageUploadHasNoReadSurface(t *testing.T) {
	server := newServer(oracleFrontConfig(filepath.Join(t.TempDir(), "missing.sock"), 1<<20), t.TempDir(), "localhost")
	for _, target := range []string{
		"http://localhost/api/session-images?realm=" + oracleImageRealm,
		"http://localhost/var/lib/persea-terminal-staging/" + oracleImageRealm + "/img-0123456789abcdef0123456789abcdef.png",
		"http://localhost/api/session-images/img-0123456789abcdef0123456789abcdef.png",
	} {
		response := oracleImageRequest(t, server, http.MethodGet, target, "", nil, nil)
		if response.Code == http.StatusOK || response.Code == http.StatusCreated || bytes.Contains(response.Body.Bytes(), []byte("image")) {
			t.Fatalf("GET exposed staged content: target=%s status=%d body=%q", target, response.Code, response.Body.String())
		}
	}
}

func TestImageUploadResponseRejectsPartialServerValues(t *testing.T) {
	payload := oraclePNG(t, 1, 1)
	valid := oracleStagedControl("image/png", len(payload))
	for name, mutate := range map[string]func(*proto.Control){
		"missing_id": func(c *proto.Control) { c.ID = "" },
		"short_id": func(c *proto.Control) {
			c.ID = "x"
			c.Path = filepath.Join(config.DefaultImageStagingRoot, oracleImageRealm, "img-x.png")
		},
		"uppercase_id": func(c *proto.Control) {
			c.ID = strings.ToUpper(c.ID)
			c.Path = filepath.Join(config.DefaultImageStagingRoot, oracleImageRealm, "img-"+c.ID+".png")
		},
		"nonhex_id": func(c *proto.Control) {
			c.ID = strings.Repeat("g", 32)
			c.Path = filepath.Join(config.DefaultImageStagingRoot, oracleImageRealm, "img-"+c.ID+".png")
		},
		"missing_path":   func(c *proto.Control) { c.Path = "" },
		"missing_expiry": func(c *proto.Control) { c.ExpiresAt = "" },
		"invalid_expiry": func(c *proto.Control) { c.ExpiresAt = "not-a-time" },
		"wrong_extension": func(c *proto.Control) {
			c.Path = filepath.Join(config.DefaultImageStagingRoot, oracleImageRealm, "img-"+c.ID+".gif")
		},
		"noncanonical_basename": func(c *proto.Control) {
			c.Path = filepath.Join(config.DefaultImageStagingRoot, oracleImageRealm, "anything.png")
		},
		"outside_realm": func(c *proto.Control) {
			c.Path = filepath.Join(config.DefaultImageStagingRoot, "other", "img-"+c.ID+".png")
		},
	} {
		t.Run(name, func(t *testing.T) {
			control := valid
			mutate(&control)
			socket, observed := startOracleImageBroker(t, oracleBrokerReply{control: control})
			server := newServer(oracleFrontConfig(socket, 1<<20), t.TempDir(), "localhost")
			response := oracleImageRequest(t, server, http.MethodPost, "http://localhost/api/session-images?realm="+oracleImageRealm, "image/png", bytes.NewReader(payload), nil)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			if seen := <-observed; seen.err != nil && !errors.Is(seen.err, net.ErrClosed) {
				t.Fatalf("broker witness: %v", seen.err)
			}
		})
	}
}

func TestImageUploadResponseAcceptsCanonicalNestedStagingPath(t *testing.T) {
	payload := oraclePNG(t, 1, 1)
	want := oracleStagedControl("image/png", len(payload))
	want.Path = filepath.Join(config.DefaultImageStagingRoot, oracleImageRealm, "uploads", "day-1", "img-"+want.ID+".png")
	socket, observed := startOracleImageBroker(t, oracleBrokerReply{control: want})
	server := newServer(oracleFrontConfig(socket, 1<<20), t.TempDir(), "localhost")
	response := oracleImageRequest(t, server, http.MethodPost, "http://localhost/api/session-images?realm="+oracleImageRealm, "image/png", bytes.NewReader(payload), nil)
	assertOracleNoStoreJSON(t, response, http.StatusCreated)
	if !strings.Contains(response.Body.String(), want.Path) {
		t.Fatalf("nested staged path was not preserved: body=%q want path=%q", response.Body.String(), want.Path)
	}
	if seen := <-observed; seen.err != nil {
		t.Fatalf("broker witness: %v", seen.err)
	}
}

func TestImageUploadValidationUsesFrontSniffDataflow(t *testing.T) {
	if err := imageValidationDataflowError("image_upload.go"); err != nil {
		t.Fatal(err)
	}
}

func TestImageUploadValidationDataflowOraclePositiveControl(t *testing.T) {
	good := `package frontdoor
func stageSessionImage() { var result, realm any; sniffed := "image/png"; validStagedImage(result, realm, sniffed, "/root") }
func validStagedImage(result, realm any, media, stagingRoot string) bool { return imageExtension(media) != "" }
func imageExtension(media string) string { return media }
`
	deriveFromReply := `package frontdoor
func stageSessionImage() { var result, realm any; sniffed := "image/png"; validStagedImage(result, realm, sniffed, "/root") }
func validStagedImage(result, realm any, media, stagingRoot string) bool { return filepath.Ext(result.Path) != "" }
`
	for name, source := range map[string]string{"good": good, "derive_from_reply": deriveFromReply} {
		path := filepath.Join(t.TempDir(), name+".go")
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		err := imageValidationDataflowError(path)
		if name == "good" && err != nil {
			t.Fatalf("valid dataflow rejected: %v", err)
		}
		if name == "derive_from_reply" && (err == nil || !strings.Contains(err.Error(), "derive only from sniffed media")) {
			t.Fatalf("derive-from-reply mutant survived: %v", err)
		}
	}
}

func imageValidationDataflowError(filename string) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		return err
	}
	var handler, validator *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		switch fn.Name.Name {
		case "stageSessionImage":
			handler = fn
		case "validStagedImage":
			validator = fn
		}
	}
	if handler == nil || validator == nil {
		return errors.New("image response validation functions are missing")
	}
	frontSniffForwarded := false
	ast.Inspect(handler.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 4 {
			return true
		}
		callee, ok := call.Fun.(*ast.Ident)
		if !ok || callee.Name != "validStagedImage" {
			return true
		}
		media, ok := call.Args[2].(*ast.Ident)
		frontSniffForwarded = ok && media.Name == "sniffed"
		return true
	})
	if !frontSniffForwarded {
		return errors.New("validStagedImage must receive the front door's own sniffed media type")
	}
	if validator.Type.Params == nil || len(validator.Type.Params.List) == 0 {
		return errors.New("validStagedImage must carry a distinct media parameter")
	}
	var parameterNames []string
	for _, field := range validator.Type.Params.List {
		for _, name := range field.Names {
			parameterNames = append(parameterNames, name.Name)
		}
	}
	if len(parameterNames) != 4 || parameterNames[2] != "media" {
		return errors.New("validStagedImage must carry a distinct media parameter")
	}
	extensionFromMedia := false
	pathExtensionRead := false
	ast.Inspect(validator.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "imageExtension" && len(call.Args) == 1 {
			arg, ok := call.Args[0].(*ast.Ident)
			extensionFromMedia = ok && arg.Name == "media"
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Ext" {
			if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "filepath" {
				pathExtensionRead = true
			}
		}
		return true
	})
	if !extensionFromMedia || pathExtensionRead {
		return fmt.Errorf("required basename extension must derive only from sniffed media: fromMedia=%t pathExtRead=%t", extensionFromMedia, pathExtensionRead)
	}
	return nil
}

func TestInventoryAdvertisesImageUploadGateAndRelaysBrokerCapability(t *testing.T) {
	for _, maxBytes := range []int{0, 5 << 20} {
		dir := shortTestDir(t)
		socket := filepath.Join(dir, "realm.sock")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			defer c.Close()
			_, _ = proto.ReadFrame(c)
			_ = writeControl(c, proto.Control{Type: "hello_ok", V: 1})
			_, _ = proto.ReadFrame(c)
			_ = writeControl(c, proto.Control{Type: "inventory_ok", Servers: []proto.ServerInventory{{Label: "main", Status: "ok", CanStageImages: true}}})
		}()
		cfg := config.Front{
			Realms:           []config.Realm{{Name: oracleImageRealm, Socket: socket}},
			HandleTTLSeconds: 120, HandleCapacity: 10, ImageUploadMaxBytes: maxBytes,
		}
		s := newServer(cfg, ".", "127.0.0.1:8080")
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/inventory", nil))
		listener.Close()
		if rr.Code != 200 {
			t.Fatal(rr.Code)
		}
		var body struct {
			ImageUpload bool        `json:"image_upload"`
			Realms      []realmView `json:"realms"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.ImageUpload != (maxBytes > 0) {
			t.Fatalf("max=%d image_upload=%v", maxBytes, body.ImageUpload)
		}
		// The broker's advice is relayed verbatim either way; the UI must gate
		// on BOTH facts, and the endpoint itself is absent when the gate is off.
		if len(body.Realms) != 1 || len(body.Realms[0].Servers) != 1 || !body.Realms[0].Servers[0].CanStageImages {
			t.Fatalf("capability relay failed: %+v", body.Realms)
		}
		if !strings.Contains(rr.Body.String(), `"can_stage_images":true`) {
			t.Fatalf("wire field missing: %s", rr.Body.String())
		}
	}
}
