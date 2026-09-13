package frontdoor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

func f3AuthorizeTakeover(t *testing.T, addr, offer, requestID, prefix, mutant string) string {
	t.Helper()
	got, err := f3Post(addr, map[string]string{"request_id": requestID, "offer": offer})
	if err != nil {
		t.Fatal(err)
	}
	f3RequireHandleEndpoint(t, got, http.StatusCreated, prefix, mutant)
	if got.status != http.StatusCreated {
		f3ContextualFatalf(t, prefix, "status=%d body=%q mutant=%s", got.status, got.body, mutant)
	}
	var response struct {
		Handle string `json:"handle"`
	}
	if err := json.Unmarshal(got.body, &response); err != nil || len(response.Handle) != 43 {
		f3ContextualFatalf(t, prefix, "response=%q err=%v mutant=%s", got.body, err, mutant)
	}
	return response.Handle
}

func f3DialTakeover(addr, handle string) (*websocket.Conn, *http.Response, error) {
	dialer := websocket.Dialer{Subprotocols: []string{
		"persea-engine.unified-dev",
		"persea-terminal.v1",
		"persea-handle." + handle,
		"persea-mode.control",
		"persea-csrf." + testCSRF,
		"persea-history.5000",
		"persea-takeover.v1",
	}}
	header := currentFixtureHeaders(addr)
	return dialer.Dial("ws://"+addr+"/ws", header)
}

// f3DialTerminalWS uses the current capability protocol and keeps a F3 row's
// handshake failure attributable to that row instead of to shared test plumbing.
func f3DialTerminalWS(t *testing.T, addr, handle, mode, prefix, mutant string) *websocket.Conn {
	t.Helper()
	ws, response, err := currentFixtureDial(addr, handle, mode)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		f3ContextualFatalf(t, prefix, "websocket dial=%v status=%d mutant=%s", err, status, mutant)
	}
	return ws
}

// f3DialTakeoverRequired preserves the real takeover dial while giving callers
// an exact row-local failure if the handshake cannot enter their assertion path.
func f3DialTakeoverRequired(t *testing.T, addr, handle, prefix, mutant string) *websocket.Conn {
	t.Helper()
	ws, response, err := f3DialTakeover(addr, handle)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		f3ContextualFatalf(t, prefix, "takeover dial=%v status=%d mutant=%s", err, status, mutant)
	}
	return ws
}

func f3BrowserOracleTrustedHop(next http.Handler, ingress config.Ingress) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "localhost"
		r.Header.Set("X-Forwarded-Host", ingress.CanonicalHost)
		r.Header.Set("X-Forwarded-Proto", "http")
		r.Header.Set("Tailscale-User-Login", ingress.OperatorLogin)
		ctx := context.WithValue(r.Context(), ingressKey{}, ingressIdentity{uid: uint32(os.Geteuid())})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// TestF3FailurePrefixesStatic keeps the dedicated F3 assertions attributable.
// websocket_integration_test.go has broader integration coverage; these
// contracts reach it through contextual wrappers that supply the row prefix.
func TestF3FailurePrefixesStatic(t *testing.T) {
	files := []string{
		"control_takeover_security_test.go",
		"control_takeover_contract_test.go",
		"control_takeover_race_test.go",
	}
	for _, name := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("F3-W0 takeover-oracle-infrastructure: parse %s: %v", name, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (selector.Sel.Name != "Fatalf" && selector.Sel.Name != "Errorf") {
					return true
				}
				ident, ok := selector.X.(*ast.Ident)
				if !ok || ident.Name != "t" || len(call.Args) == 0 {
					return true
				}
				// The sole contextual wrapper preserves a caller-supplied F3-W
				// prefix; all other direct assertions must contain one literally.
				if function.Name.Name == "f3ContextualFatalf" {
					return true
				}
				literal, ok := call.Args[0].(*ast.BasicLit)
				if !ok || !strings.Contains(literal.Value, "F3-W") {
					t.Fatalf("F3-W0 takeover-oracle-infrastructure: %s:%d has unattributed t.%s", name, fset.Position(call.Pos()).Line, selector.Sel.Name)
				}
				return true
			})
		}
	}
}

type f3BrowserOracleProvisioner struct {
	front              *Server
	next               http.Handler
	authority          proto.Authority
	operator           string
	historyRows        string
	redirectHits       atomic.Uint64
	websocketAttaches  atomic.Uint64
	leaseHeldEmissions atomic.Uint64
	gate               f3TakeoverGate
	commits            *f3BrowserTransferCommits
}

type f3BrowserTransferCommit struct {
	Generation uint64 `json:"generation"`
	Owner      string `json:"owner"`
}

type f3BrowserTransferCommits struct {
	mu      sync.Mutex
	commits []f3BrowserTransferCommit
	events  chan f3BrowserTransferCommit
	entered chan f3BrowserTransferCommit
	release chan struct{}
}

type f3RawAttachmentMessage struct {
	kind    int
	payload []byte
}

type f3CandidateAttachmentObservation struct {
	semantic          terminal.Frame
	semanticAvailable bool
	displacement      string
	err               error
}

// f3ReadRawAttachmentMessage intentionally exposes the next wire message unchanged.
// W7 needs to prove that no server output crosses the held transfer-commit boundary.
func f3ReadRawAttachmentMessage(ws *websocket.Conn) (f3RawAttachmentMessage, error) {
	kind, payload, err := ws.ReadMessage()
	if err != nil {
		return f3RawAttachmentMessage{}, err
	}
	return f3RawAttachmentMessage{kind: kind, payload: payload}, nil
}

func f3ReadSemanticAttachment(ws *websocket.Conn, first f3RawAttachmentMessage) (terminal.Frame, error) {
	next := first
	for {
		if next.kind != websocket.TextMessage {
			return terminal.Frame{}, fmt.Errorf("attachment websocket kind=%d", next.kind)
		}
		typed, err := attachmentwire.Decode(next.payload, attachmentwire.ServerToBrowser)
		if err != nil {
			return terminal.Frame{}, err
		}
		if typed.Type == terminal.FramePrepare {
			ready, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameReady, Source: typed.Source, Epoch: typed.Epoch, Cut: typed.Cut}, attachmentwire.BrowserToServer)
			if err != nil {
				return terminal.Frame{}, err
			}
			if err := ws.WriteMessage(websocket.TextMessage, ready); err != nil {
				return terminal.Frame{}, err
			}
		} else if typed.Type == terminal.FrameCommit {
			// Commit is transport sequencing; the first semantic attachment frame is Live.
		} else {
			return typed, nil
		}
		next, err = f3ReadRawAttachmentMessage(ws)
		if err != nil {
			return terminal.Frame{}, err
		}
	}
}

// f3ObserveCandidateAttachment classifies the candidate's wire outcome before
// W7/W8 evaluate it. That keeps a broadcast displacement from being mislabeled
// as a missing Live frame merely because the Live assertion happened first.
func f3ObserveCandidateAttachment(ws *websocket.Conn, first f3RawAttachmentMessage, initialErr error) f3CandidateAttachmentObservation {
	if initialErr != nil {
		var closeErr *websocket.CloseError
		if errors.As(initialErr, &closeErr) && (closeErr.Text == "control_displaced" || closeErr.Text == "takeover_superseded") {
			return f3CandidateAttachmentObservation{displacement: closeErr.Text}
		}
		return f3CandidateAttachmentObservation{err: initialErr}
	}
	next := first
	for {
		if next.kind != websocket.TextMessage {
			return f3CandidateAttachmentObservation{err: fmt.Errorf("attachment websocket kind=%d", next.kind)}
		}
		typed, err := attachmentwire.Decode(next.payload, attachmentwire.ServerToBrowser)
		if err != nil {
			return f3CandidateAttachmentObservation{err: err}
		}
		if typed.Type == terminal.FramePrepare {
			ready, err := attachmentwire.Encode(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameReady, Source: typed.Source, Epoch: typed.Epoch, Cut: typed.Cut}, attachmentwire.BrowserToServer)
			if err != nil {
				return f3CandidateAttachmentObservation{err: err}
			}
			if err := ws.WriteMessage(websocket.TextMessage, ready); err != nil {
				return f3CandidateAttachmentObservation{err: err}
			}
		} else if typed.Type != terminal.FrameCommit {
			return f3CandidateAttachmentObservation{semantic: typed, semanticAvailable: true}
		}
		next, err = f3ReadRawAttachmentMessage(ws)
		if err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) && (closeErr.Text == "control_displaced" || closeErr.Text == "takeover_superseded") {
				return f3CandidateAttachmentObservation{displacement: closeErr.Text}
			}
			return f3CandidateAttachmentObservation{err: err}
		}
	}
}

func (r *f3BrowserTransferCommits) Checkpoint(checkpoint controlTakeoverCheckpoint) {
	if checkpoint.Kind != controlTakeoverTransferCommit {
		return
	}
	r.mu.Lock()
	event := f3BrowserTransferCommit{Generation: checkpoint.Tuple.Generation, Owner: fmt.Sprintf("%x", checkpoint.Owner.opaque)}
	r.commits = append(r.commits, event)
	if r.events != nil {
		select {
		case r.events <- event:
		default:
		}
	}
	entered, release := r.entered, r.release
	r.mu.Unlock()
	if entered != nil {
		entered <- event
	}
	if release != nil {
		<-release
	}
}

func (r *f3BrowserTransferCommits) snapshot() []f3BrowserTransferCommit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]f3BrowserTransferCommit(nil), r.commits...)
}

type f3TakeoverGate struct {
	mu             sync.Mutex
	armed          bool
	holdAll        bool
	holdOrdinals   map[int]bool
	seen           int
	parked         int
	waiters        map[int]chan struct{}
	maxParkTimeout time.Duration
	timeouts       int
}

type f3TakeoverGateArm struct {
	HoldAll       bool  `json:"hold_all"`
	HoldOrdinals  []int `json:"hold_ordinals"`
	MaxParkMillis int   `json:"max_park_ms"`
}

type f3TakeoverGateRelease struct {
	Ordinals []int `json:"ordinals"`
	All      bool  `json:"all"`
}

func (o *f3BrowserOracleProvisioner) armTakeoverGate(arm f3TakeoverGateArm) error {
	if arm.MaxParkMillis <= 0 {
		return errors.New("F3 oracle gate max_park_ms must be positive")
	}
	holds := make(map[int]bool, len(arm.HoldOrdinals))
	for _, ordinal := range arm.HoldOrdinals {
		if ordinal <= 0 {
			return errors.New("F3 oracle gate ordinal must be positive")
		}
		holds[ordinal] = true
	}
	o.gate.mu.Lock()
	defer o.gate.mu.Unlock()
	if o.gate.parked != 0 {
		return errors.New("F3 oracle gate cannot arm while requests are parked")
	}
	o.gate.armed = true
	o.gate.holdAll = arm.HoldAll
	o.gate.holdOrdinals = holds
	o.gate.seen = 0
	o.gate.parked = 0
	o.gate.waiters = make(map[int]chan struct{})
	o.gate.maxParkTimeout = time.Duration(arm.MaxParkMillis) * time.Millisecond
	o.gate.timeouts = 0
	return nil
}

func (o *f3BrowserOracleProvisioner) releaseTakeoverGate(release f3TakeoverGateRelease) {
	o.gate.mu.Lock()
	waiters := make([]chan struct{}, 0, len(o.gate.waiters))
	armNextTransferCommit := false
	if release.All {
		for ordinal, waiter := range o.gate.waiters {
			delete(o.gate.waiters, ordinal)
			waiters = append(waiters, waiter)
		}
	} else {
		for _, ordinal := range release.Ordinals {
			if waiter, ok := o.gate.waiters[ordinal]; ok {
				delete(o.gate.waiters, ordinal)
				waiters = append(waiters, waiter)
			}
		}
		// W18's ordinal gate holds A while B completes.  Arm the already-frozen
		// transfer-commit blocker before A is released so its commit can be
		// observed before it presents success.
		armNextTransferCommit = !o.gate.holdAll && len(waiters) != 0
	}
	o.gate.mu.Unlock()

	if armNextTransferCommit && o.commits != nil {
		o.commits.mu.Lock()
		if o.commits.entered == nil && o.commits.release == nil {
			o.commits.entered = make(chan f3BrowserTransferCommit, 1)
			o.commits.release = make(chan struct{})
		}
		o.commits.mu.Unlock()
	}
	for _, waiter := range waiters {
		close(waiter)
	}
	if release.All || len(waiters) != 0 || o.commits == nil {
		return
	}
	// A second existing release request carries no parked POST.  It releases
	// only the W18 transfer-commit blocker after the browser has observed it.
	o.commits.mu.Lock()
	commitRelease := o.commits.release
	o.commits.release = nil
	o.commits.mu.Unlock()
	if commitRelease != nil {
		close(commitRelease)
	}
}

func (o *f3BrowserOracleProvisioner) parkTakeover(r *http.Request) {
	o.gate.mu.Lock()
	if !o.gate.armed {
		o.gate.mu.Unlock()
		return
	}
	o.gate.seen++
	ordinal := o.gate.seen
	hold := o.gate.holdAll || o.gate.holdOrdinals[ordinal]
	if !hold {
		o.gate.mu.Unlock()
		return
	}
	waiter := make(chan struct{})
	o.gate.waiters[ordinal] = waiter
	o.gate.parked++
	timeout := o.gate.maxParkTimeout
	o.gate.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	timedOut := false
	select {
	case <-waiter:
	case <-r.Context().Done():
	case <-timer.C:
		timedOut = true
	}
	o.gate.mu.Lock()
	if current, ok := o.gate.waiters[ordinal]; ok && current == waiter {
		delete(o.gate.waiters, ordinal)
	}
	o.gate.parked--
	if timedOut {
		o.gate.timeouts++
	}
	o.gate.mu.Unlock()
}

func (o *f3BrowserOracleProvisioner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/__f3_oracle/gate/arm" {
		var arm f3TakeoverGateArm
		if err := json.NewDecoder(r.Body).Decode(&arm); err != nil || o.armTakeoverGate(arm) != nil {
			http.Error(w, "F3 oracle gate arm invalid", http.StatusBadRequest)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/__f3_oracle/gate/release" {
		var release f3TakeoverGateRelease
		if err := json.NewDecoder(r.Body).Decode(&release); err != nil {
			http.Error(w, "F3 oracle gate release invalid", http.StatusBadRequest)
			return
		}
		o.releaseTakeoverGate(release)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/api/control-takeovers" {
		o.parkTakeover(r)
	}
	if r.Method == http.MethodGet && r.URL.Path == "/__f3_oracle/observations" {
		o.front.takeovers.mu.Lock()
		offerCreations := o.front.takeovers.serial
		o.front.takeovers.mu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		o.gate.mu.Lock()
		gate := struct {
			Armed    bool `json:"armed"`
			Seen     int  `json:"seen"`
			Parked   int  `json:"parked"`
			Timeouts int  `json:"timeouts"`
		}{o.gate.armed, o.gate.seen, o.gate.parked, o.gate.timeouts}
		o.gate.mu.Unlock()
		commits := []f3BrowserTransferCommit(nil)
		if o.commits != nil {
			commits = o.commits.snapshot()
		}
		if err := json.NewEncoder(w).Encode(struct {
			RedirectHits       uint64                    `json:"redirect_hits"`
			WebsocketAttaches  uint64                    `json:"ws_attaches"`
			LeaseHeldEmissions uint64                    `json:"lease_held_emissions"`
			OfferCreations     uint64                    `json:"offer_creations"`
			Gate               any                       `json:"gate"`
			TransferCommits    []f3BrowserTransferCommit `json:"transfer_commits"`
		}{
			RedirectHits:       o.redirectHits.Load(),
			WebsocketAttaches:  o.websocketAttaches.Load(),
			LeaseHeldEmissions: o.leaseHeldEmissions.Load(),
			OfferCreations:     offerCreations,
			Gate:               gate,
			TransferCommits:    commits,
		}); err != nil {
			http.Error(w, "F3 oracle observations unavailable", http.StatusInternalServerError)
		}
		return
	}
	if r.URL.Path == "/ws" && strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		o.websocketAttaches.Add(1)
	}
	if r.Method == http.MethodGet && (r.URL.Path == "/__f3_oracle/incumbent" || r.URL.Path == "/__f3_oracle/refused") {
		o.redirectHits.Add(1)
		handle, err := o.front.handles.mint(o.authority, o.operator, "control")
		if err != nil {
			http.Error(w, "F3 oracle handle unavailable", http.StatusServiceUnavailable)
			return
		}
		role := strings.TrimPrefix(r.URL.Path, "/__f3_oracle/")
		target := url.URL{Path: "/terminal"}
		target.Fragment = url.Values{
			"handle":  []string{handle},
			"history": []string{o.historyRows},
			"mode":    []string{"control"},
			"name":    []string{"F3 " + role},
		}.Encode()
		http.Redirect(w, r, target.String(), http.StatusFound)
		return
	}
	o.next.ServeHTTP(w, r)
}

func TestControlTakeoverPrepareBeforeTransferW6(t *testing.T) {
	_, broker, addr, offer, incumbent, cleanup := f3StageOffer(t, "prepare", "F3-W6 prepare-before-transfer", f3Mutants["W6"])
	defer cleanup()
	handle := f3AuthorizeTakeover(t, addr, offer, strings.Repeat("P", 43), "F3-W6 prepare-before-transfer", f3Mutants["W6"])

	if err := broker.listener.Close(); err != nil {
		t.Fatal(err)
	}
	candidate, response, err := f3DialTakeover(addr, handle)
	if err == nil {
		reason := readWSCloseReasonSkippingText(t, candidate)
		_ = candidate.Close()
		if reason != "takeover_failed" && reason != "broker_unavailable" {
			t.Fatalf("F3-W6 prepare-before-transfer: close=%q mutant=%s", reason, f3Mutants["W6"])
		}
	} else if response != nil && response.StatusCode >= 500 {
		t.Fatalf("F3-W6 prepare-before-transfer: upgrade failed before candidate failure path status=%d mutant=%s", response.StatusCode, f3Mutants["W6"])
	}

	payload := []byte("incumbent-after-candidate-failure")
	writeWSAttachment(t, incumbent, terminal.Frame{
		Version: terminal.ProtocolVersion, Type: terminal.FrameInput,
		Source: integrationSource, Epoch: 1, Data: payload,
	})
	select {
	case got := <-broker.inputs:
		if !bytes.Equal(got, payload) {
			t.Fatalf("F3-W6 prepare-before-transfer: incumbent bytes changed mutant=%s", f3Mutants["W6"])
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("F3-W6 prepare-before-transfer: incumbent lost before validated prepare mutant=%s", f3Mutants["W6"])
	}
}

func TestControlTakeoverCommitBeforeBrowserSuccessAndSingleDisplacementW7W8(t *testing.T) {
	_, _, addr, offer, incumbent, cleanup := f3StageOffer(t, "commit", "F3-W7 browser-success-order", f3Mutants["W7"])
	defer cleanup()
	if controlTakeoverTestAdapterHook != nil {
		t.Fatal("F3-W7 browser-success-order: adapter hook was not nil")
	}
	commitRelease := make(chan struct{})
	var commitReleaseOnce sync.Once
	releaseCommit := func() { commitReleaseOnce.Do(func() { close(commitRelease) }) }
	t.Cleanup(releaseCommit)
	commits := &f3BrowserTransferCommits{events: make(chan f3BrowserTransferCommit, 1), entered: make(chan f3BrowserTransferCommit, 1), release: commitRelease}
	controlTakeoverTestAdapterHook = commits
	t.Cleanup(func() { controlTakeoverTestAdapterHook = nil })
	handle := f3AuthorizeTakeover(t, addr, offer, strings.Repeat("C", 43), "F3-W7 browser-success-order", f3Mutants["W7"])
	type dialResult struct {
		candidate *websocket.Conn
		response  *http.Response
		err       error
	}
	dialed := make(chan dialResult, 1)
	go func() {
		candidate, response, err := f3DialTakeover(addr, handle)
		dialed <- dialResult{candidate: candidate, response: response, err: err}
	}()
	select {
	case commit := <-commits.entered:
		if commit.Generation == 0 || commit.Owner == "" {
			t.Fatalf("F3-W7 browser-success-order: transfer checkpoint invalid mutant=%s", f3Mutants["W7"])
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("F3-W7 browser-success-order: transfer checkpoint absent before candidate attachment mutant=%s", f3Mutants["W7"])
	}
	result := <-dialed
	if result.err != nil {
		status := 0
		if result.response != nil {
			status = result.response.StatusCode
		}
		t.Fatalf("F3-W7 browser-success-order: dial=%v status=%d mutant=%s", result.err, status, f3Mutants["W7"])
	}
	candidate := result.candidate
	defer candidate.Close()
	type rawAttachmentResult struct {
		message f3RawAttachmentMessage
		err     error
	}
	rawAttachment := make(chan rawAttachmentResult, 1)
	go func() {
		message, err := f3ReadRawAttachmentMessage(candidate)
		rawAttachment <- rawAttachmentResult{message: message, err: err}
	}()
	select {
	case got := <-rawAttachment:
		t.Fatalf("F3-W7 browser-success-order: raw attachment arrived before transfer commit kind=%d bytes=%d err=%v mutant=%s", got.message.kind, len(got.message.payload), got.err, f3Mutants["W7"])
	case <-time.After(100 * time.Millisecond):
	}
	releaseCommit()
	// Observe once, then classify by content. W8's candidate displacement is a
	// different failure from W7's post-commit attachment ordering.
	firstRaw := <-rawAttachment
	candidateObservation := f3ObserveCandidateAttachment(candidate, firstRaw.message, firstRaw.err)
	if reason := readWSCloseReasonSkippingText(t, incumbent); reason != "control_displaced" {
		t.Fatalf("F3-W8 single-displacement: close=%q mutant=%s", reason, f3Mutants["W8"])
	}
	if candidateObservation.displacement != "" {
		t.Fatalf("F3-W8 single-displacement: candidate close=%q mutant=%s", candidateObservation.displacement, f3Mutants["W8"])
	}
	if candidateObservation.err != nil {
		t.Fatalf("F3-W7 browser-success-order: attachment read=%v mutant=%s", candidateObservation.err, f3Mutants["W7"])
	}
	if !candidateObservation.semanticAvailable || candidateObservation.semantic.Type != terminal.FrameLive {
		t.Fatalf("F3-W7 browser-success-order: first=%+v mutant=%s", candidateObservation.semantic, f3Mutants["W7"])
	}
	_ = incumbent.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, _, err := incumbent.ReadMessage(); err == nil {
		t.Fatalf("F3-W8 single-displacement: old owner admitted a second message mutant=%s", f3Mutants["W8"])
	}
}

func TestControlTakeoverTTLPreservationAndCrashBoundariesW12W13(t *testing.T) {
	if LeaseTTL != 60*time.Second {
		t.Fatalf("F3-W12 ttl-preservation: LeaseTTL=%s mutant=%s", LeaseTTL, f3Mutants["W12"])
	}
	_, broker, addr, offer, incumbent, cleanup := f3StageOffer(t, "crash", "F3-W12 ttl-preservation", f3Mutants["W12"])
	defer cleanup()
	requestID := strings.Repeat("K", 43)
	handle := f3AuthorizeTakeover(t, addr, offer, requestID, "F3-W12 ttl-preservation", f3Mutants["W12"])

	// A POST-only candidate must not reserve or displace. This is the
	// pre-prepare crash boundary; later boundaries are checkpointed in the
	// tagged race suite without permitting adapter mutation.
	payload := []byte("still-incumbent-after-post")
	writeWSAttachment(t, incumbent, terminal.Frame{
		Version: terminal.ProtocolVersion, Type: terminal.FrameInput,
		Source: integrationSource, Epoch: 1, Data: payload,
	})
	select {
	case got := <-broker.inputs:
		if !bytes.Equal(got, payload) {
			t.Fatalf("F3-W13 candidate-crash-no-orphan: changed bytes mutant=%s", f3Mutants["W13"])
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("F3-W13 candidate-crash-no-orphan: POST reserved lease mutant=%s", f3Mutants["W13"])
	}

	candidate := f3DialTakeoverRequired(t, addr, handle, "F3-W13 candidate-crash-no-orphan", f3Mutants["W13"])
	if first := readWSAttachment(t, candidate); first.Type != terminal.FrameLive {
		t.Fatalf("F3-W12 ttl-preservation: first=%+v mutant=%s", first, f3Mutants["W12"])
	}
	_ = candidate.Close()
}

func TestControlTakeoverObserveIsolationW14(t *testing.T) {
	front, _, addr, offer, _, cleanup := f3StageOffer(t, "observe-isolation", "F3-W14 observe-isolation", f3Mutants["W14"])
	defer cleanup()
	observeHandle, err := front.handles.mint(f3Authority("observe-isolation"), front.cfg.Ingress.OperatorLogin, "observe")
	if err != nil {
		t.Fatal(err)
	}
	observeA := f3DialTerminalWS(t, addr, observeHandle, "observe", "F3-W14 observe-isolation", f3Mutants["W14"])
	defer observeA.Close()
	observeHandle, err = front.handles.mint(f3Authority("observe-isolation"), front.cfg.Ingress.OperatorLogin, "observe")
	if err != nil {
		t.Fatal(err)
	}
	observeB := f3DialTerminalWS(t, addr, observeHandle, "observe", "F3-W14 observe-isolation", f3Mutants["W14"])
	defer observeB.Close()
	for index, observe := range []*websocket.Conn{observeA, observeB} {
		if got := readWSAttachment(t, observe); got.Type != terminal.FrameLive {
			t.Fatalf("F3-W14 observe-isolation: observe=%d first=%+v mutant=%s", index, got, f3Mutants["W14"])
		}
	}

	handle := f3AuthorizeTakeover(t, addr, offer, strings.Repeat("B", 43), "F3-W14 observe-isolation", f3Mutants["W14"])
	winner := f3DialTakeoverRequired(t, addr, handle, "F3-W14 observe-isolation", f3Mutants["W14"])
	defer winner.Close()
	if got := readWSAttachment(t, winner); got.Type != terminal.FrameLive {
		t.Fatalf("F3-W14 observe-isolation: winner=%+v mutant=%s", got, f3Mutants["W14"])
	}
	for index, observe := range []*websocket.Conn{observeA, observeB} {
		writeWSAttachment(t, observe, terminal.Frame{
			Version: terminal.ProtocolVersion, Type: terminal.FrameInput,
			Source: integrationSource, Epoch: 1, Data: []byte(fmt.Sprintf("denied-%d", index)),
		})
		writeWSAttachment(t, observe, terminal.Frame{
			Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest,
			Source: integrationSource, Epoch: 1, Mode: terminal.ModeControl,
		})
	}
}
