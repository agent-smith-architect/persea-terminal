package frontdoor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unixcred"
)

const BrokerSetupTimeout = 2 * time.Second
const SnapshotLineLimit = 5000
const SnapshotByteLimit = 2 * 1024 * 1024
const LeaseTTL = 60 * time.Second
const BrowserProofTimeout = 20 * time.Second
const browserProofArmReservation = 10 * time.Millisecond
const WSPingInterval = 20 * time.Second
const WSWriteTimeout = 10 * time.Second
const WSReadTimeout = 40 * time.Second
const BrokerPingInterval = 30 * time.Second

// The static bundle a release must contain. session memory added the installable
// shell: a manifest and three icons, all REGULAR files in the release, all
// hash-covered by the release MANIFEST. A release is judged by its own
// binary, so an older release keeps its own (shorter) list.
var requiredBundleFiles = []string{"index.html", "app.js", "app.css", "xterm.css", "manifest.webmanifest", "icon-192.png", "icon-512.png", "apple-touch-icon.png"}

// Go has no built-in media type for .webmanifest, and an unmapped extension is
// content-sniffed — a JSON manifest would be served as text/plain. Registering
// it at package init keeps the production server and every test that serves the
// bundle in agreement. The argument is a constant, so a failure here is a
// programming error rather than a runtime condition.
func init() {
	if err := mime.AddExtensionType(".webmanifest", "application/manifest+json"); err != nil {
		panic("static manifest media type: " + err.Error())
	}
}

const styleNoncePlaceholder = "__PERSEA_STYLE_NONCE__"
const baseContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"

type Server struct {
	cfg                     config.Front
	listen                  string
	staticDir               string
	handles                 *handleStore
	bindings                *sourceBindingStore
	leases                  *leaseStore
	takeovers               *controlTakeoverStore
	aliases                 *aliasStore
	aliasErr                error
	preferences             *preferencesStore
	preferencesErr          error
	keyboardPreferences     *keyboardPreferencesStore
	keyboardPreferencesErr  error
	dashboardPreferences    *dashboardPreferencesStore
	dashboardPreferencesErr error
	snippets                *snippetStore
	snippetErr              error
	clipboardImages         *clipboardImageStore
	clipboardImageErr       error
	clipboardPreferences    *clipboardPreferencesStore
	clipboardPreferencesErr error
	workspaces              *workspaceStore
	workspaceErr            error
	diagnostic              *diagnosticTraceStore
	diagnosticErr           error
	browserProofTimeout     time.Duration
	browserProofNow         func() time.Time
	upgrader                websocket.Upgrader
	limiter                 operatorLimiter
	previews                *previewStore
}

var frontLogf = log.Printf
var boundedFailureCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var sourceBindingRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
var listenIngressForRun = listenIngress

func canonicalFailureCode(code string) string {
	if boundedFailureCode.MatchString(code) {
		return code
	}
	return "attachment_failed"
}

func (s *Server) logTerminalFailure(code string, authority *proto.Authority) string {
	code = canonicalFailureCode(code)
	realm, server, session := "", "", ""
	if authority != nil {
		realm, server, session = authority.Realm, authority.Server, authority.SessionID
	}
	frontLogf("component=frontdoor event=terminal_failure code=%q realm=%q server=%q session=%q", code, realm, server, session)
	return code
}

// logOperationalRefusal records a broker refusal that did NOT end the
// attachment. It is deliberately a different event from terminal_failure so a
// journal grep for closes never counts a Fit that merely did not apply.
func (s *Server) logOperationalRefusal(code string, authority *proto.Authority) {
	realm, server, session := "", "", ""
	if authority != nil {
		realm, server, session = authority.Realm, authority.Server, authority.SessionID
	}
	frontLogf("component=frontdoor event=operational_refusal code=%q realm=%q server=%q session=%q", canonicalFailureCode(code), realm, server, session)
}

func writeWSCloseReason(writes chan<- wsWrite, writerDone <-chan struct{}, code string) error {
	return writeWS(writes, writerDone, websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, canonicalFailureCode(code)))
}

type sessionView struct {
	Handles        capabilityHandles   `json:"handles"`
	Handle         string              `json:"handle,omitempty"`
	Realm          string              `json:"realm"`
	Server         string              `json:"server"`
	ServerStatus   string              `json:"server_status"`
	SessionID      string              `json:"session_id"`
	Name           string              `json:"name"`
	Width          int                 `json:"width"`
	Height         int                 `json:"height"`
	Attached       int                 `json:"attached"`
	Activity       int64               `json:"activity"`
	OutputActivity int64               `json:"output_activity,omitempty"`
	Authority      proto.Authority     `json:"authority"`
	Alias          string              `json:"alias,omitempty"`
	AliasState     string              `json:"alias_state,omitempty"`
	ExpiresAt      int64               `json:"expires_at"`
	Unified        *unifiedSessionView `json:"unified,omitempty"`
	authority      proto.Authority
}
type unifiedSessionView struct {
	State  string `json:"state"`
	Origin string `json:"origin,omitempty"`
	Detail string `json:"detail,omitempty"`
}
type capabilityHandles struct {
	Alias   string `json:"alias"`
	Observe string `json:"observe"`
	Control string `json:"control"`
}
type serverView struct {
	Label     string `json:"label"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	CanCreate bool   `json:"can_create"`
	// CanStageImages is the broker's own advertisement, relayed verbatim.
	// The UI must also see the top-level image_upload gate before offering
	// the affordance: both processes consent independently, and the broker
	// re-decides on every image_stage exchange regardless.
	CanStageImages bool                  `json:"can_stage_images,omitempty"`
	Sessions       []*sessionView        `json:"sessions"`
	UnifiedDev     *unifiedDevLaunchView `json:"unified_dev,omitempty"`
}
type unifiedDevLaunchView struct {
	State     string `json:"state"`
	Name      string `json:"name"`
	SessionID string `json:"session_id,omitempty"`
	Control   string `json:"control,omitempty"`
}

// bindUnifiedDevLaunch carries only the configured target's CREATE affordance:
// an existing target is served per session (bindUnifiedSession), where the
// browser navigates with the row's own minted control handle instead of a
// projection-matched one.
func bindUnifiedDevLaunch(launch *proto.UnifiedDevLaunch, _ []*sessionView) *unifiedDevLaunchView {
	if launch == nil || launch.Name == "" {
		return nil
	}
	if launch.State == proto.UnifiedDevLaunchCreate && launch.SessionID == "" {
		return &unifiedDevLaunchView{State: launch.State, Name: launch.Name}
	}
	return nil
}

// bindUnifiedSession passes one broker per-session unified state through the
// front door, failing closed on anything outside the closed vocabulary: an
// unknown state or misplaced origin/detail hides the affordance rather than
// letting a broker put an unreviewed shape in front of the strict parser.
func bindUnifiedSession(state *proto.UnifiedSessionState) *unifiedSessionView {
	if state == nil {
		return nil
	}
	switch state.State {
	case proto.UnifiedSessionOpen:
		originOK := state.Origin == proto.UnifiedOriginBirth || state.Origin == proto.UnifiedOriginReconstructed
		detailOK := state.Detail == "" || state.Detail == proto.UnifiedSessionDetailRotationDeferredAltScreen
		if originOK && detailOK {
			return &unifiedSessionView{State: state.State, Origin: state.Origin, Detail: state.Detail}
		}
	case proto.UnifiedSessionAdoptable,
		proto.UnifiedSessionBlockedAltScreen,
		proto.UnifiedSessionBlockedMultiPane,
		proto.UnifiedSessionBlockedMultiWindow,
		proto.UnifiedSessionBlockedForeignServer,
		proto.UnifiedSessionSlotsExhausted,
		proto.UnifiedSessionUnavailable:
		if state.Origin == "" && state.Detail == "" {
			return &unifiedSessionView{State: state.State}
		}
	}
	return nil
}

type realmView struct {
	Name        string       `json:"name"`
	DisplayName string       `json:"display_name"`
	Error       string       `json:"error,omitempty"`
	Servers     []serverView `json:"servers"`
}

func Run(cfg config.Front, staticDir string) error {
	if err := validateStaticDir(staticDir); err != nil {
		return err
	}
	s := newServer(cfg, staticDir, cfg.Ingress.CanonicalHost)
	if s.aliasErr != nil {
		return fmt.Errorf("alias store: %w", s.aliasErr)
	}
	if s.diagnosticErr != nil {
		return fmt.Errorf("diagnostic trace store: %w", s.diagnosticErr)
	}
	defer s.diagnostic.close()
	defer s.clipboardImages.close()
	s.logUnavailableStores()
	ln, cleanup, err := listenIngressForRun(cfg.Ingress)
	if err != nil {
		logIngressReason("socket_state")
		return err
	}
	httpServer := &http.Server{Handler: s.handler(), ConnContext: trustedContext, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32768}
	shutdownContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	clipboardTicks := time.NewTicker(clipboardReapInterval)
	defer clipboardTicks.Stop()
	clipboardDone := make(chan struct{})
	go func() {
		defer close(clipboardDone)
		s.maintainClipboard(shutdownContext, clipboardTicks.C)
	}()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-shutdownContext.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()
	err = httpServer.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	stopSignals()
	<-clipboardDone
	<-shutdownDone
	cleanupErr := cleanup()
	if cleanupErr != nil {
		logIngressReason("socket_state")
	}
	return errors.Join(err, cleanupErr)
}

// openPreferencesStore keeps an unconfigured or unopenable preferences store
// as a held error rather than a boot failure: the routes then answer with the
// defaults (GET) or 503 (PUT); the reason is logged once at start.
func openPreferencesStore(path string) (*preferencesStore, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: preferences_store_path is not configured", errPreferencesStoreUnavailable)
	}
	return newPreferencesStore(path)
}

func openKeyboardPreferencesStore(path string) (*keyboardPreferencesStore, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: keyboard_preferences_store_path is not configured", errKeyboardPreferencesStoreUnavailable)
	}
	return newKeyboardPreferencesStore(path)
}

// openSnippetStore mirrors openPreferencesStore: an unconfigured or
// unopenable snippet store is held, logged once, and answered with 503 on
// every route rather than a partial list or a boot failure.
func openSnippetStore(path string) (*snippetStore, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: snippet_store_path is not configured", errSnippetStoreUnavailable)
	}
	return newSnippetStore(path)
}

func openWorkspaceStore(path string) (*workspaceStore, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: workspace_store_path is not configured", errWorkspaceStoreUnavailable)
	}
	return newWorkspaceStore(path)
}

func (s *Server) logUnavailableStores() {
	if s.cfg.ImageUploadMaxBytes > 0 && s.clipboardImageErr != nil {
		frontLogf("component=frontdoor event=store_unavailable store=clipboard_images")
	}
	if s.keyboardPreferencesErr != nil {
		frontLogf("component=frontdoor event=store_unavailable store=%q reason=%q", "keyboard_preferences", s.keyboardPreferencesErr.Error())
	}
	if s.preferencesErr != nil {
		frontLogf("component=frontdoor event=store_unavailable store=%q reason=%q", "preferences", s.preferencesErr.Error())
	}
	if s.dashboardPreferencesErr != nil {
		frontLogf("component=frontdoor event=store_unavailable store=%q reason=%q", "dashboard_preferences", s.dashboardPreferencesErr.Error())
	}
	if s.snippetErr != nil {
		frontLogf("component=frontdoor event=store_unavailable store=%q reason=%q", "snippets", s.snippetErr.Error())
	}
	if s.workspaceErr != nil {
		frontLogf("component=frontdoor event=store_unavailable store=%q reason=%q", "workspaces", s.workspaceErr.Error())
	}
}

func validateStaticDir(staticDir string) error {
	if staticDir == "" || filepath.IsAbs(staticDir) || filepath.Clean(staticDir) != staticDir {
		return fmt.Errorf("static directory must be a clean relative path")
	}
	info, err := os.Lstat(staticDir)
	if err != nil {
		return fmt.Errorf("static directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("static directory must be a real directory")
	}
	for _, name := range requiredBundleFiles {
		path := filepath.Join(staticDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("static bundle file %s: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("static bundle file %s must be a regular file", name)
		}
	}
	return nil
}
func newServer(cfg config.Front, staticDir, listen string) *Server {
	for i := range cfg.Realms {
		if !cfg.Realms[i].HasBrokerUID() && cfg.Realms[i].BrokerUID == 0 {
			cfg.Realms[i].BrokerUID = uint32(os.Getuid())
			cfg.Realms[i].BrokerUIDConfigured = true
		}
	}
	aliases, aliasErr := newAliasStore(cfg.AliasStorePath)
	preferences, preferencesErr := openPreferencesStore(cfg.PreferencesStorePath)
	keyboardPreferences, keyboardPreferencesErr := openKeyboardPreferencesStore(cfg.KeyboardPreferencesStorePath)
	snippets, snippetErr := openSnippetStore(cfg.SnippetStorePath)
	workspaces, workspaceErr := openWorkspaceStore(cfg.WorkspaceStorePath)
	var diagnostic *diagnosticTraceStore
	var diagnosticErr error
	if cfg.DiagnosticTraceDir != "" && cfg.Ingress.PeerUIDConfigured {
		diagnostic, diagnosticErr = newDiagnosticTraceStore(cfg.DiagnosticTraceDir)
	}
	bindingTTL := time.Duration(cfg.HandleTTLSeconds) * time.Second
	s := &Server{cfg: cfg, listen: listen, staticDir: staticDir, handles: newHandleStore(bindingTTL, cfg.HandleCapacity), bindings: newSourceBindingStore(bindingTTL, cfg.HandleCapacity), leases: newLeaseStore(LeaseTTL), takeovers: newControlTakeoverStore(LeaseTTL, cfg.HandleCapacity), aliases: aliases, aliasErr: aliasErr, preferences: preferences, preferencesErr: preferencesErr, snippets: snippets, snippetErr: snippetErr, workspaces: workspaces, workspaceErr: workspaceErr, diagnostic: diagnostic, diagnosticErr: diagnosticErr, browserProofTimeout: BrowserProofTimeout, browserProofNow: time.Now, previews: newPreviewStore(time.Now)}
	s.upgrader = websocket.Upgrader{ReadBufferSize: proto.MaxAttachment, WriteBufferSize: proto.MaxAttachment, Subprotocols: []string{"persea-terminal.v1"}, CheckOrigin: func(*http.Request) bool { return true }}
	s.keyboardPreferences, s.keyboardPreferencesErr = keyboardPreferences, keyboardPreferencesErr
	s.dashboardPreferences, s.dashboardPreferencesErr = openDashboardPreferencesStore(cfg.PreferencesStorePath)
	s.clipboardImages, s.clipboardImageErr = openClipboardImageStore(cfg.SnippetStorePath, cfg.ImageUploadMaxBytes)
	s.clipboardPreferences, s.clipboardPreferencesErr = openClipboardPreferencesStore(cfg.SnippetStorePath)
	return s
}
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/inventory", s.inventory)
	mux.HandleFunc("POST /api/attachment-handles", s.attachmentHandle)
	mux.HandleFunc("POST /api/control-takeovers", s.controlTakeover)
	mux.HandleFunc("POST /api/sessions", s.createSession)
	mux.HandleFunc("POST /api/session-adoptions", s.adoptSession)
	mux.HandleFunc("POST /api/session-refits", s.refitSession)
	mux.HandleFunc("GET /api/session-previews", s.previewSession)
	mux.HandleFunc("POST /api/aliases", s.createAlias)
	mux.HandleFunc("PATCH /api/aliases/{id}", s.updateAlias)
	mux.HandleFunc("DELETE /api/aliases/{id}", s.deleteAlias)
	mux.HandleFunc("GET /api/preferences", s.getPreferences)
	mux.HandleFunc("PUT /api/preferences", s.putPreferences)
	mux.HandleFunc("GET /api/keyboard-preferences", s.getKeyboardPreferences)
	mux.HandleFunc("PUT /api/keyboard-preferences", s.putKeyboardPreferences)
	mux.HandleFunc("GET /api/dashboard-preferences", s.dashboardPreferencesAPI)
	mux.HandleFunc("PUT /api/dashboard-preferences", s.dashboardPreferencesAPI)
	mux.HandleFunc("GET /api/snippets", s.listSnippets)
	mux.HandleFunc("POST /api/snippets", s.createSnippet)
	mux.HandleFunc("PUT /api/snippets/osc52", s.upsertOSCSnippet)
	mux.HandleFunc("PATCH /api/snippets/{id}", s.updateSnippet)
	mux.HandleFunc("DELETE /api/snippets/{id}", s.deleteSnippet)
	mux.HandleFunc("GET /api/clipboard/images", s.listClipboardImages)
	mux.HandleFunc("POST /api/clipboard/images", s.createClipboardImage)
	mux.HandleFunc("GET /api/clipboard/images/{id}", s.getClipboardImage)
	mux.HandleFunc("DELETE /api/clipboard/images/{id}", s.deleteClipboardImage)
	mux.HandleFunc("PATCH /api/clipboard/images/{id}", s.updateClipboardImage)
	mux.HandleFunc("GET /api/clipboard/preferences", s.getClipboardPreferences)
	mux.HandleFunc("PUT /api/clipboard/preferences", s.putClipboardPreferences)
	mux.HandleFunc("GET /api/workspaces", s.listWorkspaces)
	mux.HandleFunc("POST /api/workspaces", s.createWorkspace)
	mux.HandleFunc("PUT /api/workspaces/{id}", s.updateWorkspace)
	mux.HandleFunc("DELETE /api/workspaces/{id}", s.deleteWorkspace)
	if s.cfg.DiagnosticTraceDir != "" && s.cfg.Ingress.PeerUIDConfigured && s.diagnostic != nil {
		mux.HandleFunc("POST /api/diagnostic-traces", s.diagnosticTrace)
	}
	if s.cfg.ImageUploadMaxBytes > 0 {
		mux.HandleFunc("POST /api/session-images", s.stageSessionImage)
	}
	mux.HandleFunc("GET /api/snapshot", s.snapshot)
	mux.HandleFunc("GET /ws", s.terminal)
	mux.HandleFunc("GET /terminal", s.index)
	mux.HandleFunc("GET /workspace", s.index)
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /index.html", s.index)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("GET /", staticAssets(s.staticDir))
	if s.cfg.Ingress.PeerUIDConfigured {
		return s.secure(mux)
	}
	return securityHeaders(requireCanonicalHost(s.listen, mux))
}
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	template, err := os.ReadFile(filepath.Join(s.staticDir, "index.html"))
	if err != nil || bytes.Count(template, []byte(styleNoncePlaceholder)) != 1 {
		http.Error(w, "terminal document unavailable", http.StatusInternalServerError)
		return
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		http.Error(w, "terminal document unavailable", http.StatusInternalServerError)
		return
	}
	nonce := base64.RawStdEncoding.EncodeToString(nonceBytes)
	body := bytes.Replace(template, []byte(styleNoncePlaceholder), []byte(nonce), 1)
	csp := strings.Replace(baseContentSecurityPolicy, "style-src 'self'", "style-src 'self' 'nonce-"+nonce+"'", 1)
	// The query string is the CSP capability key and must stay byte-exact. A
	// workspace document hosts N unified xterm instances and needs exactly
	// what one needs: the same relaxation on the same key.
	if (r.URL.Path == "/terminal" || r.URL.Path == "/workspace") && r.URL.RawQuery == "engine=unified-dev" {
		csp += "; style-src-attr 'unsafe-inline'"
	}
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}
func checkOriginForListen(listen string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		if r.Host != listen {
			return false
		}
		o := r.Header.Get("Origin")
		return o == "" || o == "http://"+listen || o == "https://"+listen
	}
}
func requireCanonicalHost(listen string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != listen {
			http.Error(w, "invalid host", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func validateListenAddress(address string) error {
	host, p, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid front-door listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("front-door listen host must be loopback")
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port")
	}
	if address != net.JoinHostPort(ip.String(), strconv.Itoa(port)) {
		return fmt.Errorf("listen address must be canonical")
	}
	return nil
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", baseContentSecurityPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=()")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
func writeControl(w io.Writer, c proto.Control) error {
	p, e := proto.MarshalControl(c)
	if e != nil {
		return e
	}
	return proto.WriteFrame(w, proto.FrameControl, p)
}
func hello(c net.Conn) error {
	if e := writeControl(c, proto.Control{Type: "hello", V: 1}); e != nil {
		return e
	}
	f, e := proto.ReadFrame(c)
	if e != nil || f.Type != proto.FrameControl {
		return fmt.Errorf("invalid broker hello")
	}
	m, e := proto.DecodeControl(f.Payload)
	if e != nil || m.Type != "hello_ok" || m.V != 1 {
		return fmt.Errorf("invalid broker hello")
	}
	return nil
}
func dial(path string, brokerUID uint32) (net.Conn, error) {
	deadline := time.Now().Add(BrokerSetupTimeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err := c.SetDeadline(deadline); err != nil {
		c.Close()
		return nil, err
	}
	if err := unixcred.Verify(c, brokerUID); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

type realmInventoryResult struct {
	servers []proto.ServerInventory
	err     error
}

func fetchRealmInventory(realm config.Realm) realmInventoryResult {
	c, err := dial(realm.Socket, realm.BrokerUID)
	if err != nil {
		return realmInventoryResult{err: err}
	}
	defer c.Close()
	if err = hello(c); err == nil {
		err = writeControl(c, proto.Control{Type: "inventory"})
	}
	if err != nil {
		return realmInventoryResult{err: err}
	}
	f, err := proto.ReadFrame(c)
	if err != nil || f.Type != proto.FrameControl {
		if err == nil {
			err = fmt.Errorf("invalid inventory response")
		}
		return realmInventoryResult{err: err}
	}
	m, err := proto.DecodeControl(f.Payload)
	if err != nil || m.Type != "inventory_ok" {
		if err == nil {
			err = fmt.Errorf("invalid inventory response")
		}
		return realmInventoryResult{err: err}
	}
	return realmInventoryResult{servers: m.Servers}
}

func (s *Server) inventory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	results := make([]realmInventoryResult, len(s.cfg.Realms))
	var wg sync.WaitGroup
	wg.Add(len(s.cfg.Realms))
	for i, realm := range s.cfg.Realms {
		go func() {
			defer wg.Done()
			results[i] = fetchRealmInventory(realm)
		}()
	}
	wg.Wait()

	views := make([]realmView, 0, len(s.cfg.Realms))
	all := []*sessionView{}
	frontTruncated := map[string]bool{}
	remaining := s.cfg.HandleCapacity / 3
	operator := ""
	if id, ok := identity(r); ok {
		operator = id.operator
	}
	expires := time.Now().Add(time.Duration(s.cfg.HandleTTLSeconds) * time.Second).UnixMilli()
	for i, realm := range s.cfg.Realms {
		rv := realmView{Name: realm.Name, DisplayName: realm.DisplayLabel(), Servers: []serverView{}}
		result := results[i]
		if result.err != nil {
			rv.Error = result.err.Error()
			views = append(views, rv)
			continue
		}
		valid := true
		for _, bs := range result.servers {
			sv := serverView{Label: bs.Label, Status: bs.Status, Error: bs.Error, CanCreate: bs.CanCreate, CanStageImages: bs.CanStageImages, Sessions: []*sessionView{}}
			for _, ss := range bs.Sessions {
				if remaining <= 0 {
					frontTruncated[realm.Name+"\x00"+bs.Label] = true
					break
				}
				if !ss.Authority.Valid() || ss.Authority.Realm != realm.Name || ss.Authority.Server != bs.Label || ss.Authority.UID != realm.BrokerUID {
					frontTruncated[realm.Name+"\x00"+bs.Label] = true
					continue
				}
				ha, x := s.handles.mint(ss.Authority, operator, "alias")
				if x != nil {
					rv.Error = x.Error()
					valid = false
					break
				}
				ho, x := s.handles.mint(ss.Authority, operator, "observe")
				if x != nil {
					rv.Error = x.Error()
					valid = false
					break
				}
				hc, x := s.handles.mint(ss.Authority, operator, "control")
				if x != nil {
					rv.Error = x.Error()
					valid = false
					break
				}
				v := &sessionView{Handles: capabilityHandles{Alias: ha, Observe: ho, Control: hc}, Realm: realm.Name, Server: bs.Label, ServerStatus: bs.Status, SessionID: ss.Authority.SessionID, Name: ss.Name, Width: ss.Width, Height: ss.Height, Attached: ss.Attached, Activity: ss.Activity, OutputActivity: ss.OutputActivity, Authority: ss.Authority, ExpiresAt: expires, Unified: bindUnifiedSession(ss.Unified), authority: ss.Authority}
				if !s.cfg.Ingress.PeerUIDConfigured {
					v.Handle = ha
				}
				sv.Sessions = append(sv.Sessions, v)
				all = append(all, v)
				remaining--
			}
			sv.UnifiedDev = bindUnifiedDevLaunch(bs.UnifiedDev, sv.Sessions)
			if !valid {
				break
			}
			rv.Servers = append(rv.Servers, sv)
		}
		if !valid {
			rv.Servers = []serverView{}
		}
		views = append(views, rv)
	}
	if s.aliasErr != nil {
		http.Error(w, "alias store unavailable", http.StatusServiceUnavailable)
		return
	}
	live := make([]proto.Authority, 0, len(all))
	complete := map[string]bool{}
	for i, realm := range s.cfg.Realms {
		if results[i].err != nil {
			continue
		}
		for _, bs := range results[i].servers {
			if bs.Status == "ok" && bs.Error == "" && !frontTruncated[realm.Name+"\x00"+bs.Label] {
				complete[realm.Name+"\x00"+bs.Label] = true
			}
		}
	}
	for _, v := range all {
		live = append(live, v.authority)
	}
	for _, seed := range s.cfg.Aliases {
		for _, v := range all {
			if v.Realm == seed.Realm && v.Server == seed.Server && v.Name == seed.Session {
				_, err := s.aliases.create(seed.Alias, v.authority)
				if err != nil && !errors.Is(err, errAliasConflict) {
					http.Error(w, "alias store unavailable", http.StatusServiceUnavailable)
					return
				}
				break
			}
		}
	}
	if err := s.aliases.reconcile(live, complete); err != nil {
		http.Error(w, "alias store unavailable", http.StatusServiceUnavailable)
		return
	}
	records := s.aliases.list()
	for _, record := range records {
		for _, v := range all {
			if sameAuthority(record.Incarnation, v.authority) {
				v.Alias = record.DisplayAlias
				v.AliasState = record.State
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"realms": views, "aliases": records, "snapshot_expires_at": expires, "image_upload": s.cfg.ImageUploadMaxBytes > 0})
}

const aliasRequestMaxBytes = 4096

func decodeAliasRequest(r *http.Request, dst any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, aliasRequestMaxBytes+1))
	if err != nil {
		return err
	}
	if len(b) > aliasRequestMaxBytes {
		return errors.New("request too large")
	}
	if !utf8.Valid(b) {
		return errors.New("invalid UTF-8")
	}
	if err := rejectAliasDuplicateKeys(b); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing request data")
	}
	return nil
}

func (s *Server) attachmentHandle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" {
		http.Error(w, "invalid attachment-handle request", http.StatusBadRequest)
		return
	}
	var req struct {
		Source  string `json:"source"`
		Purpose string `json:"purpose"`
	}
	if decodeAliasRequest(r, &req) != nil || !sourceBindingRE.MatchString(req.Source) || (req.Purpose != "observe" && req.Purpose != "control") {
		http.Error(w, "invalid attachment-handle request", http.StatusBadRequest)
		return
	}
	operator := ""
	if id, ok := identity(r); ok {
		operator = id.operator
	}
	authority, err := s.bindings.resolve(operator, req.Source)
	if err != nil {
		http.Error(w, "attachment source is unavailable", http.StatusGone)
		return
	}
	handle, err := s.handles.mint(authority, operator, req.Purpose)
	if err != nil {
		http.Error(w, "attachment handle is unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"handle": handle})
}

func parseIfMatch(r *http.Request) (uint64, error) {
	v := r.Header.Get("If-Match")
	if len(v) < 3 || v[0] != '"' || v[len(v)-1] != '"' {
		return 0, errors.New("If-Match required")
	}
	n, err := strconv.ParseUint(v[1:len(v)-1], 10, 64)
	if err != nil || n == 0 {
		return 0, errors.New("invalid If-Match")
	}
	return n, nil
}

func writeAliasRecord(w http.ResponseWriter, status int, r AliasRecord) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", fmt.Sprintf("\"%d\"", r.Revision))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(r)
}

func (s *Server) resolveCurrentHandle(r *http.Request, handle string) (proto.Authority, error) {
	if !s.cfg.Ingress.PeerUIDConfigured {
		return s.handles.resolve(handle)
	}
	operator := ""
	if id, ok := identity(r); ok {
		operator = id.operator
	}
	a, err := s.handles.resolve(handle, operator, "alias")
	if err != nil {
		logRequestIngress(r, "capability")
		return proto.Authority{}, err
	}
	realm, err := s.realmFor(a)
	if err != nil {
		return proto.Authority{}, err
	}
	result := fetchRealmInventory(realm)
	if result.err != nil {
		return proto.Authority{}, errInvalidHandle
	}
	for _, server := range result.servers {
		if server.Status != "ok" || server.Error != "" {
			continue
		}
		for _, session := range server.Sessions {
			if sameAuthority(session.Authority, a) {
				return a, nil
			}
		}
	}
	return proto.Authority{}, errInvalidHandle
}

// createSessionStatus maps a broker refusal to a status code. Refusals are the
// broker's decision, never the front door's: the front door forwards a realm, a
// server label, and a name, and reports back what the realm's policy said. It
// deliberately does not pre-validate the name, so there is exactly one place
// where the grammar lives.
func createSessionStatus(code string) int {
	switch code {
	case "invalid_name":
		return http.StatusBadRequest
	case "not_permitted":
		return http.StatusForbidden
	case "name_taken", "at_capacity":
		return http.StatusConflict
	case "server_unavailable":
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

// createSession asks one realm's broker to create a tmux session.
//
// The request carries a realm, a server label, and a name — never a command, a
// working directory, or geometry. Those live in broker configuration precisely so
// that no browser can choose what a new shell runs. Operator identity, CSRF, and
// rate limiting are already enforced by the secure() chain before this runs.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm  string `json:"realm"`
		Server string `json:"server"`
		Name   string `json:"name"`
	}
	if decodeAliasRequest(r, &req) != nil {
		http.Error(w, "invalid session request", http.StatusBadRequest)
		return
	}
	var realm config.Realm
	found := false
	for _, candidate := range s.cfg.Realms {
		if candidate.Name == req.Realm {
			realm, found = candidate, true
			break
		}
	}
	if !found || req.Server == "" || req.Name == "" {
		http.Error(w, "invalid session request", http.StatusBadRequest)
		return
	}
	code, createdID, err := requestRealmSession(realm, req.Server, req.Name)
	if err != nil {
		logRequestIngress(r, "create_unreachable")
		http.Error(w, "the realm broker is unreachable", http.StatusServiceUnavailable)
		return
	}
	if code != "" {
		// The broker's message is written for the operator, but it is not echoed:
		// the code is a closed set the UI maps to its own copy, so a broker cannot
		// place arbitrary text on the page.
		http.Error(w, code, createSessionStatus(code))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"realm": realm.Name, "server": req.Server, "name": req.Name, "session_id": createdID})
}

// requestRealmSession returns an empty code on success, or the broker's refusal
// code. A transport or protocol failure is an error, never a silent refusal.
func requestRealmSession(realm config.Realm, server, name string) (string, string, error) {
	c, err := dial(realm.Socket, realm.BrokerUID)
	if err != nil {
		return "", "", err
	}
	defer c.Close()
	if err = hello(c); err != nil {
		return "", "", err
	}
	if err = writeControl(c, proto.Control{Type: "create", ServerLabel: server, Name: name}); err != nil {
		return "", "", err
	}
	f, err := proto.ReadFrame(c)
	if err != nil {
		return "", "", err
	}
	if f.Type != proto.FrameControl {
		return "", "", fmt.Errorf("invalid create response")
	}
	m, err := proto.DecodeControl(f.Payload)
	if err != nil {
		return "", "", err
	}
	switch m.Type {
	case "create_ok":
		return "", m.SessionID, nil
	case "create_refused":
		if m.Code == "" {
			return "", "", fmt.Errorf("refusal without a code")
		}
		return m.Code, "", nil
	default:
		return "", "", fmt.Errorf("unexpected create response %q", m.Type)
	}
}

// adoptSessionStatus maps a broker adoption refusal to a status code. Like
// creation, the judgement is the broker's; the front door only reports it.
func adoptSessionStatus(code string) int {
	switch code {
	case "not_permitted":
		return http.StatusForbidden
	case "blocked_alt_screen", "blocked_multi_pane", "blocked_multi_window", "adoption_in_progress":
		return http.StatusConflict
	case "session_gone":
		return http.StatusGone
	case "slots_exhausted", "unified_unavailable", "adoption_contended":
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

// adoptSession asks one realm's broker to adopt an existing tmux session into
// the unified journal. The request carries identity and an optional bounded
// history row count, never a command, directory, or geometry. It is
// idempotent: adopting an already-open session succeeds without recapturing.
// Operator identity, CSRF, and rate limiting are already enforced by the
// secure() chain before this runs.
func (s *Server) adoptSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm       string `json:"realm"`
		Server      string `json:"server"`
		SessionID   string `json:"session_id"`
		HistoryRows *int   `json:"history_rows"`
	}
	if decodeAliasRequest(r, &req) != nil {
		http.Error(w, "invalid adoption request", http.StatusBadRequest)
		return
	}
	var realm config.Realm
	found := false
	for _, candidate := range s.cfg.Realms {
		if candidate.Name == req.Realm {
			realm, found = candidate, true
			break
		}
	}
	if !found || req.Server == "" || req.SessionID == "" || req.HistoryRows != nil && (*req.HistoryRows < 0 || *req.HistoryRows > proto.AdoptionHistoryMaxRows) {
		http.Error(w, "invalid adoption request", http.StatusBadRequest)
		return
	}
	code, err := requestRealmAdoption(realm, req.Server, req.SessionID, req.HistoryRows)
	if err != nil {
		logRequestIngress(r, "adopt_unreachable")
		http.Error(w, "the realm broker is unreachable", http.StatusServiceUnavailable)
		return
	}
	if code != "" {
		// The code is a closed set the UI maps to its own copy; broker text is
		// never echoed (same rule as creation).
		http.Error(w, code, adoptSessionStatus(code))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"realm": realm.Name, "server": req.Server, "session_id": req.SessionID})
}

// requestRealmAdoption returns an empty code on success, or the broker's
// refusal code. A transport or protocol failure is an error, never a silent
// refusal.
func requestRealmAdoption(realm config.Realm, server, sessionID string, historyRows *int) (string, error) {
	c, err := dial(realm.Socket, realm.BrokerUID)
	if err != nil {
		return "", err
	}
	defer c.Close()
	if err = hello(c); err != nil {
		return "", err
	}
	if err = writeControl(c, proto.Control{Type: "adopt", ServerLabel: server, SessionID: sessionID, HistoryRows: historyRows}); err != nil {
		return "", err
	}
	f, err := proto.ReadFrame(c)
	if err != nil {
		return "", err
	}
	if f.Type != proto.FrameControl {
		return "", fmt.Errorf("invalid adopt response")
	}
	m, err := proto.DecodeControl(f.Payload)
	if err != nil {
		return "", err
	}
	switch m.Type {
	case "adopt_ok":
		return "", nil
	case "adopt_refused":
		if m.Code == "" {
			return "", fmt.Errorf("refusal without a code")
		}
		return m.Code, nil
	default:
		return "", fmt.Errorf("unexpected adopt response %q", m.Type)
	}
}

func refitSessionStatus(code string) int {
	switch code {
	case "not_permitted":
		return http.StatusForbidden
	case "bad_refit":
		return http.StatusBadRequest
	case "stale_target", "session_gone":
		return http.StatusGone
	case "blocked_alt_screen", "refit_in_progress":
		return http.StatusConflict
	case "slots_exhausted", "unified_unavailable":
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

// refitSession submits one explicit out-of-band generation refit. Source is a
// short-lived exact binding, not a name; columns, an optional row count and
// the controller operation token are the only browser inputs. No attachment
// frame carries width. Rows 0 (or absent) keeps the predecessor's rows; a
// given row count obeys the closed row policy and is rejected, never clamped.
func (s *Server) refitSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" {
		http.Error(w, "invalid refit request", http.StatusBadRequest)
		return
	}
	var req struct {
		Source    string `json:"source"`
		Columns   int    `json:"columns"`
		Rows      int    `json:"rows"`
		Operation string `json:"operation"`
	}
	if decodeAliasRequest(r, &req) != nil || !sourceBindingRE.MatchString(req.Source) || req.Columns < 20 || req.Columns > 300 ||
		(req.Rows != 0 && !terminal.ValidVerticalFit(req.Columns, req.Rows)) || !sourceBindingRE.MatchString(req.Operation) {
		http.Error(w, "invalid refit request", http.StatusBadRequest)
		return
	}
	operator := ""
	if id, ok := identity(r); ok {
		operator = id.operator
	}
	authority, err := s.bindings.resolve(operator, req.Source)
	if err != nil {
		http.Error(w, "stale_target", http.StatusGone)
		return
	}
	var realm config.Realm
	found := false
	for _, candidate := range s.cfg.Realms {
		if candidate.Name == authority.Realm {
			realm, found = candidate, true
			break
		}
	}
	if !found {
		http.Error(w, "not_permitted", http.StatusForbidden)
		return
	}
	successor, rows, code, stage, class, err := requestRealmRefit(realm, authority, req.Columns, req.Rows, req.Operation)
	if err != nil {
		logRequestIngress(r, "refit_unreachable")
		http.Error(w, "the realm broker is unreachable", http.StatusServiceUnavailable)
		return
	}
	if code != "" {
		if code == "refit_faulted" && stage != "" {
			frontLogf("component=frontdoor event=refit_failure code=%q stage=%q class=%q realm=%q server=%q session=%q", code, stage, class, authority.Realm, authority.Server, authority.SessionID)
		}
		http.Error(w, code, refitSessionStatus(code))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// rows is always present: the row count the successor was actually built
	// with (the request's, or the predecessor's echoed by the broker).
	_ = json.NewEncoder(w).Encode(map[string]any{"operation": req.Operation, "columns": req.Columns, "rows": rows, "successor_source": successor})
}

// requestRealmRefit forwards one refit to the realm broker. rows 0 means "keep
// the predecessor's rows"; the broker echoes the rows it applied, which must be
// the requested count when one was given and a positive count otherwise.
func requestRealmRefit(realm config.Realm, authority proto.Authority, columns, rows int, operation string) (string, int, string, proto.RefitFailureStage, proto.RefitFailureClass, error) {
	c, err := dial(realm.Socket, realm.BrokerUID)
	if err != nil {
		return "", 0, "", "", "", err
	}
	defer c.Close()
	if err = hello(c); err != nil {
		return "", 0, "", "", "", err
	}
	if err = writeControl(c, proto.Control{Type: "refit", Authority: &authority, Cols: columns, Rows: rows, ID: operation}); err != nil {
		return "", 0, "", "", "", err
	}
	f, err := proto.ReadFrame(c)
	if err != nil {
		return "", 0, "", "", "", err
	}
	if f.Type != proto.FrameControl {
		return "", 0, "", "", "", fmt.Errorf("invalid refit response")
	}
	m, err := proto.DecodeControl(f.Payload)
	if err != nil {
		return "", 0, "", "", "", err
	}
	switch m.Type {
	case "refit_ok":
		rowsMatch := m.Rows == rows
		if rows == 0 {
			rowsMatch = m.Rows > 0 && m.Rows <= 1000
		}
		if m.ID != operation || m.Cols != columns || !rowsMatch || !sourceBindingRE.MatchString(m.Successor) || m.RefitStage != "" || m.RefitClass != "" {
			return "", 0, "", "", "", fmt.Errorf("mismatched refit response")
		}
		return m.Successor, m.Rows, "", "", "", nil
	case "refit_refused":
		if m.Code == "" || m.ID != operation {
			return "", 0, "", "", "", fmt.Errorf("invalid refit refusal")
		}
		hasStage, hasClass := m.RefitStage != "", m.RefitClass != ""
		validFatalMetadata := hasStage && hasClass && proto.IsRefitFailureStage(m.RefitStage) && proto.IsRefitFailureClass(m.RefitClass)
		if (m.Code == "refit_faulted" && !validFatalMetadata) || (m.Code != "refit_faulted" && (hasStage || hasClass)) {
			return "", 0, "", "", "", fmt.Errorf("invalid refit failure metadata")
		}
		return "", 0, m.Code, m.RefitStage, m.RefitClass, nil
	default:
		return "", 0, "", "", "", fmt.Errorf("unexpected refit response %q", m.Type)
	}
}

func (s *Server) createAlias(w http.ResponseWriter, r *http.Request) {
	if s.aliasErr != nil {
		http.Error(w, "alias store unavailable", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		DisplayAlias string `json:"display_alias"`
		Handle       string `json:"handle"`
	}
	if decodeAliasRequest(r, &req) != nil {
		http.Error(w, "invalid alias request", http.StatusBadRequest)
		return
	}
	a, err := s.resolveCurrentHandle(r, req.Handle)
	if err != nil {
		http.Error(w, "stale handle", http.StatusGone)
		return
	}
	record, err := s.aliases.create(req.DisplayAlias, a)
	if errors.Is(err, errAliasConflict) {
		http.Error(w, "alias conflict", http.StatusConflict)
		return
	}
	if errors.Is(err, errAliasValidation) {
		http.Error(w, "invalid alias request", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "alias store unavailable", http.StatusServiceUnavailable)
		return
	}
	writeAliasRecord(w, http.StatusCreated, record)
}

func (s *Server) updateAlias(w http.ResponseWriter, r *http.Request) {
	if s.aliasErr != nil {
		http.Error(w, "alias store unavailable", http.StatusServiceUnavailable)
		return
	}
	revision, err := parseIfMatch(r)
	if err != nil {
		http.Error(w, "If-Match required", http.StatusPreconditionFailed)
		return
	}
	var req struct {
		DisplayAlias string `json:"display_alias"`
		Handle       string `json:"handle"`
	}
	if decodeAliasRequest(r, &req) != nil {
		http.Error(w, "invalid alias request", http.StatusBadRequest)
		return
	}
	current, err := s.aliases.precondition(r.PathValue("id"), revision)
	if errors.Is(err, errAliasConflict) {
		writeAliasRecord(w, http.StatusConflict, current)
		return
	}
	if errors.Is(err, errAliasNotFound) {
		http.Error(w, "alias not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "alias store unavailable", http.StatusServiceUnavailable)
		return
	}
	var authority *proto.Authority
	if s.cfg.Ingress.PeerUIDConfigured {
		a, e := s.resolveCurrentHandle(r, req.Handle)
		if e != nil {
			http.Error(w, "stale handle", http.StatusGone)
			return
		}
		if !sameAuthority(current.Incarnation, a) {
			authority = &a
		}
	}
	record, err := s.aliases.update(r.PathValue("id"), req.DisplayAlias, authority, revision)
	if errors.Is(err, errAliasConflict) {
		if record.AliasID != "" {
			writeAliasRecord(w, http.StatusConflict, record)
		} else {
			http.Error(w, "alias conflict", http.StatusConflict)
		}
		return
	}
	if errors.Is(err, errAliasNotFound) {
		http.Error(w, "alias not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, errAliasValidation) {
		http.Error(w, "invalid alias request", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "alias store unavailable", http.StatusServiceUnavailable)
		return
	}
	writeAliasRecord(w, http.StatusOK, record)
}

func (s *Server) deleteAlias(w http.ResponseWriter, r *http.Request) {
	if s.aliasErr != nil {
		http.Error(w, "alias store unavailable", http.StatusServiceUnavailable)
		return
	}
	revision, err := parseIfMatch(r)
	if err != nil {
		http.Error(w, "If-Match required", http.StatusPreconditionFailed)
		return
	}
	if s.cfg.Ingress.PeerUIDConfigured {
		var req struct {
			Handle string `json:"handle"`
		}
		if decodeAliasRequest(r, &req) != nil {
			http.Error(w, "invalid alias request", http.StatusBadRequest)
			return
		}
		a, e := s.resolveCurrentHandle(r, req.Handle)
		if e != nil {
			http.Error(w, "stale handle", http.StatusGone)
			return
		}
		current, e := s.aliases.precondition(r.PathValue("id"), revision)
		if e != nil {
			if errors.Is(e, errAliasConflict) {
				writeAliasRecord(w, http.StatusConflict, current)
				return
			}
			if errors.Is(e, errAliasNotFound) {
				http.Error(w, "alias not found", http.StatusNotFound)
				return
			}
			http.Error(w, "alias store unavailable", http.StatusServiceUnavailable)
			return
		}
		if !sameAuthority(current.Incarnation, a) {
			http.Error(w, "stale handle", http.StatusGone)
			return
		}
	}
	record, err := s.aliases.delete(r.PathValue("id"), revision)
	if errors.Is(err, errAliasConflict) {
		writeAliasRecord(w, http.StatusConflict, record)
		return
	}
	if errors.Is(err, errAliasNotFound) {
		http.Error(w, "alias not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "alias store unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) realmFor(a proto.Authority) (config.Realm, error) {
	for _, r := range s.cfg.Realms {
		if r.Name == a.Realm {
			return r, nil
		}
	}
	return config.Realm{}, errInvalidHandle
}
func (s *Server) brokerRequest(a proto.Authority, request proto.Control) (net.Conn, error) {
	r, e := s.realmFor(a)
	if e != nil {
		return nil, e
	}
	c, e := dial(r.Socket, r.BrokerUID)
	if e != nil {
		return nil, e
	}
	if e = hello(c); e != nil {
		c.Close()
		return nil, e
	}
	if e = writeControl(c, request); e != nil {
		c.Close()
		return nil, e
	}
	return c, nil
}
func parseSnapshotDepth(r *http.Request) (int, error) {
	values, present := r.URL.Query()["depth"]
	if !present {
		return 0, nil
	}
	if len(values) != 1 || values[0] == "" {
		return 0, fmt.Errorf("invalid depth")
	}
	depth, err := strconv.Atoi(values[0])
	if err != nil || depth <= 0 {
		return 0, fmt.Errorf("invalid depth")
	}
	return depth, nil
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	depth, err := parseSnapshotDepth(r)
	if err != nil {
		http.Error(w, "invalid depth", http.StatusBadRequest)
		return
	}
	operator := ""
	if id, ok := identity(r); ok {
		operator = id.operator
	}
	var a proto.Authority
	if s.cfg.Ingress.PeerUIDConfigured {
		if r.URL.RawQuery != "" {
			deny(w, "capability", 0)
			return
		}
		handle, ok := one(r.Header, "X-Persea-Handle")
		if !ok {
			logRequestIngress(r, "capability")
			http.Error(w, "stale snapshot", http.StatusGone)
			return
		}
		a, err = s.handles.resolve(handle, operator, "observe")
	} else {
		a, err = s.handles.resolve(r.URL.Query().Get("handle"))
	}
	if err != nil {
		logRequestIngress(r, "capability")
		http.Error(w, "stale snapshot", http.StatusGone)
		return
	}
	c, err := s.brokerRequest(a, proto.Control{Type: "snapshot", Authority: &a, Depth: depth})
	if err != nil {
		http.Error(w, "broker unavailable", http.StatusBadGateway)
		return
	}
	defer c.Close()
	meta, data, bounded, err := readSnapshot(c)
	if err != nil {
		if meta.Code == "stale_target" {
			http.Error(w, meta.Code, http.StatusGone)
			return
		}
		http.Error(w, "invalid snapshot", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"text": string(data), "truncated": meta.Truncated || bounded,
		"alternate_on": meta.Alternate, "width": meta.Width, "height": meta.Height,
		"pane": meta.Pane, "frozen_at": meta.FrozenAt, "depth": meta.Depth, "history_rows": *meta.HistoryRows,
	})
}

func readSnapshot(r io.Reader) (proto.Control, []byte, bool, error) {
	first, err := proto.ReadFrame(r)
	if err != nil || first.Type != proto.FrameControl {
		return proto.Control{}, nil, false, fmt.Errorf("invalid snapshot metadata")
	}
	meta, err := proto.DecodeControl(first.Payload)
	if err != nil {
		return proto.Control{}, nil, false, err
	}
	if meta.Type == "error" {
		return meta, nil, false, fmt.Errorf("snapshot error: %s", meta.Code)
	}
	if meta.Type != "snapshot_ok" || meta.Width < 1 || meta.Height < 1 || meta.Pane == "" || meta.FrozenAt <= 0 || meta.Depth < 1 || meta.Depth > SnapshotLineLimit || meta.HistoryRows == nil || *meta.HistoryRows < 0 || *meta.HistoryRows > meta.Depth {
		return meta, nil, false, fmt.Errorf("invalid snapshot metadata")
	}
	data := []byte{}
	bounded := false
	for {
		frame, err := proto.ReadFrame(r)
		if err != nil {
			return meta, nil, bounded, err
		}
		switch frame.Type {
		case proto.FrameData:
			data = append(data, frame.Payload...)
			data, bounded = boundFrontSnapshot(data, bounded, meta.Depth+meta.Height)
		case proto.FrameControl:
			end, err := proto.DecodeControl(frame.Payload)
			if err != nil || end.Type != "snapshot_end" {
				return meta, nil, bounded, fmt.Errorf("invalid snapshot terminator")
			}
			historyRows := returnedSnapshotHistoryRows(data, meta.Height, meta.Depth)
			meta.HistoryRows = &historyRows
			return meta, data, bounded, nil
		default:
			return meta, nil, bounded, fmt.Errorf("invalid snapshot frame")
		}
	}
}

func boundFrontSnapshot(p []byte, already bool, lineCap int) ([]byte, bool) {
	truncated := already
	if len(p) > SnapshotByteLimit {
		p = p[len(p)-SnapshotByteLimit:]
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			p = p[i+1:]
		}
		truncated = true
	}
	lines := bytes.Split(p, []byte{'\n'})
	if len(lines) > lineCap+1 {
		lines = lines[len(lines)-(lineCap+1):]
		p = bytes.Join(lines, []byte{'\n'})
		truncated = true
	}
	return p, truncated
}

func returnedSnapshotHistoryRows(data []byte, height, depth int) int {
	rows := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		rows++
	}
	rows -= height
	if rows < 0 {
		return 0
	}
	if rows > depth {
		return depth
	}
	return rows
}

type wsWrite struct {
	kind    int
	payload []byte
	result  chan error
}

func wsWritePump(ws *websocket.Conn, writes <-chan wsWrite, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(WSPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case request := <-writes:
			_ = ws.SetWriteDeadline(time.Now().Add(WSWriteTimeout))
			err := ws.WriteMessage(request.kind, request.payload)
			request.result <- err
			if err != nil {
				return
			}
		case <-ticker.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(WSWriteTimeout)); err != nil {
				return
			}
		}
	}
}

func writeWS(writes chan<- wsWrite, writerDone <-chan struct{}, kind int, payload []byte) error {
	request := wsWrite{kind: kind, payload: append([]byte(nil), payload...), result: make(chan error, 1)}
	select {
	case writes <- request:
	case <-writerDone:
		return errors.New("websocket writer closed")
	}
	select {
	case err := <-request.result:
		return err
	case <-writerDone:
		return errors.New("websocket writer closed")
	}
}

func writeWSJSON(writes chan<- wsWrite, writerDone <-chan struct{}, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeWS(writes, writerDone, websocket.TextMessage, payload)
}

type wsRead struct {
	kind    int
	payload []byte
	err     error
}

func orderlyWebSocketClose(err error) bool {
	var closeErr *websocket.CloseError
	return errors.As(err, &closeErr) && (closeErr.Code == websocket.CloseNormalClosure || closeErr.Code == websocket.CloseGoingAway)
}

func wsReadPump(ws *websocket.Conn, reads chan<- wsRead, done <-chan struct{}) {
	for {
		kind, payload, err := ws.ReadMessage()
		select {
		case reads <- wsRead{kind: kind, payload: payload, err: err}:
		case <-done:
			return
		}
		if err != nil {
			return
		}
	}
}

func brokerReadPump(c io.Reader, frames chan<- readBrokerFrame, done <-chan struct{}) {
	for {
		frame, err := proto.ReadFrame(c)
		select {
		case frames <- readBrokerFrame{frame: frame, err: err}:
		case <-done:
			return
		}
		if err != nil {
			return
		}
	}
}

type readBrokerFrame struct {
	frame proto.Frame
	err   error
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

// If the actual arm occurs A after the before sample B, the after sample proves
// A-B <= reservation. Arming for expiry-B-reservation therefore establishes
// actual deadline <= A+expiry-B-reservation <= expiry.
func browserProofArmDuration(expiry, before time.Time) (time.Duration, bool) {
	remaining := expiry.Sub(before)
	if remaining <= browserProofArmReservation {
		return 0, false
	}
	return remaining - browserProofArmReservation, true
}

func browserProofArmProven(before, after, expiry time.Time) bool {
	if after.Before(before) || !after.Before(expiry) {
		return false
	}
	return after.Sub(before) <= browserProofArmReservation
}

func newBrowserProofTimer(expiry time.Time, now func() time.Time) (*time.Timer, bool) {
	before := now()
	duration, ok := browserProofArmDuration(expiry, before)
	if !ok {
		return nil, false
	}
	timer := time.NewTimer(duration)
	if !browserProofArmProven(before, now(), expiry) {
		stopTimer(timer)
		return timer, false
	}
	return timer, true
}

func advanceBrowserProof(timer *time.Timer, expiry *time.Time, receipt time.Time, timeout time.Duration, now func() time.Time) bool {
	if timeout <= 0 || !receipt.Before(*expiry) {
		return false
	}
	newExpiry := receipt.Add(timeout)
	stopTimer(timer)
	*expiry = newExpiry
	before := now()
	duration, ok := browserProofArmDuration(newExpiry, before)
	if !ok {
		return false
	}
	timer.Reset(duration)
	if !browserProofArmProven(before, now(), newExpiry) {
		stopTimer(timer)
		return false
	}
	return true
}

func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	protocolRequest, takeover, validTakeoverProtocol := takeoverProtocolRequest(r)
	if !validTakeoverProtocol {
		http.Error(w, "invalid mode", http.StatusBadRequest)
		return
	}
	r = protocolRequest
	var mode string
	engine := ""
	historyRows := terminal.DefaultHistoryRows
	var a proto.Authority
	var err error
	operator := ""
	offeredHandle := ""
	if !s.cfg.Ingress.PeerUIDConfigured {
		deny(w, "capability", 0)
		return
	} else {
		if r.URL.RawQuery != "" {
			deny(w, "capability", 0)
			return
		}
		wa, valid := parseWSAuthorityPolicy(r, !isHermetic(s.cfg.Ingress))
		mode = wa.mode
		engine = wa.engine
		historyRows = wa.history
		if !valid {
			logRequestIngress(r, "capability")
			http.Error(w, "invalid mode", 400)
			return
		}
		csrf, ok := csrfFromRequest(r)
		if !ok || subtle.ConstantTimeCompare([]byte(csrf), []byte(wa.csrf)) != 1 {
			deny(w, "csrf", 0)
			return
		}
		id, ok := identity(r)
		if !ok || r.Header.Get("Origin") != id.origin {
			deny(w, "origin", 0)
			return
		}
		operator = id.operator
		offeredHandle = wa.handle
		if takeover {
			if mode != "control" {
				http.Error(w, "invalid mode", http.StatusBadRequest)
				return
			}
			a, err = s.consumeTakeoverHandle(wa.handle, operator)
		} else {
			a, err = s.handles.consume(wa.handle, operator, mode)
		}
	}
	if err != nil {
		if s.cfg.Ingress.PeerUIDConfigured {
			logRequestIngress(r, "capability_replay")
		}
		s.logTerminalFailure("stale_snapshot", nil)
		http.Error(w, "stale snapshot", http.StatusGone)
		return
	}
	var boundSource string
	var retireSourceBinding func()
	defer func() {
		if retireSourceBinding != nil {
			retireSourceBinding()
		}
	}()
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logTerminalFailure("upgrade_failed", &a)
		return
	}
	writes := make(chan wsWrite, 64)
	writerStop := make(chan struct{})
	writerDone := make(chan struct{})
	go wsWritePump(ws, writes, writerStop, writerDone)
	defer func() {
		close(writerStop)
		_ = ws.Close()
		<-writerDone
	}()

	holder := ""
	var owner *controlLeaseOwner
	leaseInstalled := false
	if mode == "control" {
		holder, err = randomToken()
		if err != nil {
			code := s.logTerminalFailure("lease_unavailable", &a)
			_ = writeWSCloseReason(writes, writerDone, code)
			return
		}
		owner = s.leases.newOwner(a, holder, takeover)
		if !takeover {
			since, acquired := s.leases.acquireOwner(owner)
			if !acquired {
				_ = since
				if s.cfg.Ingress.PeerUIDConfigured {
					_ = s.takeovers.recordOffer(offeredHandle, operator, a)
				} else if _, consumeErr := s.handles.consumeLegacy(offeredHandle); consumeErr == nil {
					// A legacy takeover offer derives only from the consumed losing Control handle.
					_ = s.takeovers.recordOffer(offeredHandle, operator, a)
				}
				code := s.logTerminalFailure("lease_held", &a)
				_ = writeWSCloseReason(writes, writerDone, code)
				return
			}
			leaseInstalled = true
		}
		defer func() {
			if leaseInstalled {
				s.leases.releaseOwnerObserved(owner)
			}
		}()
	}

	c, err := s.brokerRequest(a, proto.Control{Type: "attach", Authority: &a, Mode: mode, Engine: engine, HistoryLimit: &historyRows})
	if err != nil {
		code := s.logTerminalFailure("broker_unavailable", &a)
		_ = writeWSCloseReason(writes, writerDone, code)
		return
	}
	defer c.Close()
	first, err := proto.ReadFrame(c)
	if err != nil || first.Type != proto.FrameControl {
		code := s.logTerminalFailure("broker_unavailable", &a)
		_ = writeWSCloseReason(writes, writerDone, code)
		return
	}
	attached, err := proto.DecodeControl(first.Payload)
	if err != nil || (attached.Type != "attach_ok" && attached.Type != "error") {
		code := s.logTerminalFailure("broker_protocol", &a)
		_ = writeWSCloseReason(writes, writerDone, code)
		return
	}
	if attached.Type == "attach_ok" && (attached.InputMax < 1 || attached.InputMax > terminal.MaxInputBytes) {
		code := s.logTerminalFailure("broker_protocol", &a)
		_ = writeWSCloseReason(writes, writerDone, code)
		return
	}
	if attached.Type == "error" {
		code := s.logTerminalFailure(attached.Code, &a)
		_ = writeWSCloseReason(writes, writerDone, code)
		return
	}
	if err = c.SetDeadline(time.Time{}); err != nil {
		code := s.logTerminalFailure("broker_deadline", &a)
		_ = writeWSCloseReason(writes, writerDone, code)
		return
	}
	if takeover {
		s.logControlTakeover("control_takeover_prepared", "takeover_prepared", &a, "")
		_ = s.leases.transfer(owner)
		leaseInstalled = true
		s.logControlTakeover("control_takeover_committed", "takeover_committed", &a, "")
	}

	ws.SetReadLimit(proto.MaxAttachment)
	if err = ws.SetReadDeadline(time.Now().Add(WSReadTimeout)); err != nil {
		s.logTerminalFailure("websocket_deadline", &a)
		return
	}
	if s.browserProofTimeout <= 0 || s.browserProofNow == nil {
		code := s.logTerminalFailure("browser_liveness", &a)
		_ = writeWSCloseReason(writes, writerDone, code)
		return
	}
	browserProofExpiry := s.browserProofNow().Add(s.browserProofTimeout)
	browserProofTimer, armed := newBrowserProofTimer(browserProofExpiry, s.browserProofNow)
	if !armed {
		code := s.logTerminalFailure("browser_liveness", &a)
		_ = writeWSCloseReason(writes, writerDone, code)
		return
	}
	defer stopTimer(browserProofTimer)
	closeBrowserLiveness := func() {
		code := s.logTerminalFailure("browser_liveness", &a)
		_ = writeWSCloseReason(writes, writerDone, code)
	}
	requireBrowserProof := func() (time.Time, bool) {
		now := s.browserProofNow()
		return now, now.Before(browserProofExpiry)
	}
	ws.SetPongHandler(func(string) error {
		if mode == "control" && !s.leases.renewOwnerGated(owner) {
			if reason, displaced := controlDisplacement(owner); displaced {
				s.logControlDisplacement(reason, &a)
				_ = writeWS(writes, writerDone, websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason))
				return errors.New(reason)
			}
			code := s.logTerminalFailure("lease_lost", &a)
			_ = writeWSCloseReason(writes, writerDone, code)
			return errors.New("control lease lost")
		}
		return ws.SetReadDeadline(time.Now().Add(WSReadTimeout))
	})
	reads := make(chan wsRead, 1)
	done := make(chan struct{})
	defer close(done)
	go wsReadPump(ws, reads, done)
	frames := make(chan readBrokerFrame, 1)
	go brokerReadPump(c, frames, done)
	brokerPing := time.NewTicker(BrokerPingInterval)
	defer brokerPing.Stop()

	for {
		if owner != nil {
			if reason, displaced := controlDisplacement(owner); displaced {
				s.logControlDisplacement(reason, &a)
				_ = writeWS(writes, writerDone, websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason))
				return
			}
		}
		select {
		case reason := <-func() <-chan string {
			if owner == nil {
				return nil
			}
			return owner.displaced
		}():
			s.logControlDisplacement(reason, &a)
			_ = writeWS(writes, writerDone, websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason))
			return
		case <-browserProofTimer.C:
			closeBrowserLiveness()
			return
		case read := <-reads:
			if read.err != nil {
				if orderlyWebSocketClose(read.err) {
					_ = writeControl(c, proto.Control{Type: "detach"})
					return
				}
				s.logTerminalFailure("websocket_read", &a)
				return
			}
			eventNow, validProof := requireBrowserProof()
			if !validProof {
				closeBrowserLiveness()
				return
			}
			switch read.kind {
			case websocket.TextMessage:
				liveness, recognized, livenessErr := attachmentwire.DecodeTransportLiveness(read.payload, attachmentwire.BrowserToServer)
				if recognized {
					if livenessErr != nil {
						code := s.logTerminalFailure("bad_liveness", &a)
						_ = writeWSCloseReason(writes, writerDone, code)
						return
					}
					if !advanceBrowserProof(browserProofTimer, &browserProofExpiry, eventNow, s.browserProofTimeout, s.browserProofNow) {
						closeBrowserLiveness()
						return
					}
					pong, encodeErr := attachmentwire.EncodeTransportLiveness(attachmentwire.TransportLivenessFrame{Kind: attachmentwire.TransportLivenessPong, Nonce: liveness.Nonce}, attachmentwire.ServerToBrowser)
					if encodeErr != nil {
						s.logTerminalFailure("websocket_write", &a)
						return
					}
					if _, validProof = requireBrowserProof(); !validProof {
						closeBrowserLiveness()
						return
					}
					if writeWS(writes, writerDone, websocket.TextMessage, pong) != nil {
						s.logTerminalFailure("websocket_write", &a)
						return
					}
					continue
				}
				typed, err := attachmentwire.Decode(read.payload, attachmentwire.BrowserToServer)
				if err != nil {
					code := s.logTerminalFailure("bad_attachment", &a)
					_ = writeWSCloseReason(writes, writerDone, code)
					return
				}
				if mode == "observe" && (typed.Type == terminal.FrameInput || typed.Type == terminal.FrameResize || (typed.Type == terminal.FrameModeRequest && typed.Mode == terminal.ModeControl)) {
					code := s.logTerminalFailure("observe_mode", &a)
					_ = writeWSCloseReason(writes, writerDone, code)
					return
				}
				current, wrote := true, true
				if mode == "control" {
					current, wrote = s.leases.useOwnerInput(owner, func() bool {
						return proto.WriteFrame(c, proto.FrameAttachment, read.payload) == nil
					})
				} else {
					wrote = proto.WriteFrame(c, proto.FrameAttachment, read.payload) == nil
				}
				if !current {
					if reason, displaced := controlDisplacement(owner); displaced {
						s.logControlDisplacement(reason, &a)
						_ = writeWS(writes, writerDone, websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason))
						return
					}
					code := s.logTerminalFailure("lease_lost", &a)
					_ = writeWSCloseReason(writes, writerDone, code)
					return
				}
				if !wrote {
					code := s.logTerminalFailure("broker_write", &a)
					_ = writeWSCloseReason(writes, writerDone, code)
					return
				}
			default:
				code := s.logTerminalFailure("websocket_message_type", &a)
				_ = writeWSCloseReason(writes, writerDone, code)
				return
			}
		case result := <-frames:
			if owner != nil && s.leases.ownerFenced(owner) {
				if reason, displaced := controlDisplacement(owner); displaced {
					s.logControlDisplacement(reason, &a)
					_ = writeWS(writes, writerDone, websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason))
				}
				return
			}
			if result.err != nil {
				code := s.logTerminalFailure("broker_read", &a)
				_ = writeWSCloseReason(writes, writerDone, code)
				return
			}
			if _, validProof := requireBrowserProof(); !validProof {
				closeBrowserLiveness()
				return
			}
			switch result.frame.Type {
			case proto.FrameControl:
				control, err := proto.DecodeControl(result.frame.Payload)
				if err != nil {
					code := s.logTerminalFailure("broker_protocol", &a)
					_ = writeWSCloseReason(writes, writerDone, code)
					return
				}
				if control.Type == "error" && proto.ClassifyAttachmentError(control.Code) == proto.AttachmentErrorOperational {
					// One request's outcome, not a verdict on the attachment:
					// the broker kept the epoch, so the front keeps the socket
					// and relays the code in-band for the page to render. Only
					// the closed operational set takes this path; anything else
					// the broker writes on the attachment is the attachment's
					// verdict and closes below, exactly as before.
					refusal, encodeErr := attachmentwire.EncodeTransportRefusal(control.Code, attachmentwire.ServerToBrowser)
					if encodeErr != nil {
						code := s.logTerminalFailure("broker_protocol", &a)
						_ = writeWSCloseReason(writes, writerDone, code)
						return
					}
					s.logOperationalRefusal(control.Code, &a)
					if writeWS(writes, writerDone, websocket.TextMessage, refusal) != nil {
						s.logTerminalFailure("websocket_write", &a)
						return
					}
					continue
				}
				if control.Type != "pong" {
					code := s.logTerminalFailure(control.Code, &a)
					_ = writeWSCloseReason(writes, writerDone, code)
					return
				}
			case proto.FrameAttachment:
				if len(result.frame.Payload) > proto.MaxAttachment {
					s.logTerminalFailure("websocket_write", &a)
					return
				}
				typed, decodeErr := attachmentwire.Decode(result.frame.Payload, attachmentwire.ServerToBrowser)
				if decodeErr != nil {
					code := s.logTerminalFailure("broker_protocol", &a)
					_ = writeWSCloseReason(writes, writerDone, code)
					return
				}
				if retireSourceBinding == nil {
					if typed.Type != terminal.FramePrepare || !sourceBindingRE.MatchString(typed.Source) {
						code := s.logTerminalFailure("broker_protocol", &a)
						_ = writeWSCloseReason(writes, writerDone, code)
						return
					}
					retireSourceBinding, err = s.bindings.bind(operator, typed.Source, a)
					if err != nil {
						code := s.logTerminalFailure("source_binding", &a)
						_ = writeWSCloseReason(writes, writerDone, code)
						return
					}
					boundSource = typed.Source
				} else if typed.Source != boundSource {
					code := s.logTerminalFailure("broker_protocol", &a)
					_ = writeWSCloseReason(writes, writerDone, code)
					return
				}
				if writeWS(writes, writerDone, websocket.TextMessage, result.frame.Payload) != nil {
					s.logTerminalFailure("websocket_write", &a)
					return
				}
			default:
				code := s.logTerminalFailure("broker_protocol", &a)
				_ = writeWSCloseReason(writes, writerDone, code)
				return
			}
		case <-brokerPing.C:
			if _, validProof := requireBrowserProof(); !validProof {
				closeBrowserLiveness()
				return
			}
			if writeControl(c, proto.Control{Type: "ping"}) != nil {
				code := s.logTerminalFailure("broker_write", &a)
				_ = writeWSCloseReason(writes, writerDone, code)
				return
			}
		case <-writerDone:
			s.logTerminalFailure("websocket_write", &a)
			return
		}
	}
}
