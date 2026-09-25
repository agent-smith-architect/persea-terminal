package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/config"
	"persea-terminal/internal/frontdoor"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

const (
	frontdoorRotationFrontHelperEnv = "PERSEA_ROTATION_FRONT_HELPER"
	frontdoorRotationFrontConfigEnv = "PERSEA_ROTATION_FRONT_CONFIG"
	frontdoorRotationStaticDirEnv   = "PERSEA_ROTATION_STATIC_DIR"
	frontdoorRotationCSRF           = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

func TestSourceFrontdoorHelperProcess(t *testing.T) {
	if os.Getenv(frontdoorRotationFrontHelperEnv) != "1" {
		t.Skip("frontdoor subprocess helper")
	}
	var cfg config.Front
	if err := json.Unmarshal([]byte(os.Getenv(frontdoorRotationFrontConfigEnv)), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := frontdoor.Run(cfg, os.Getenv(frontdoorRotationStaticDirEnv)); err != nil {
		t.Fatal(err)
	}
}

type frontdoorRotationInventory struct {
	Realms []struct {
		Servers []struct {
			Sessions []struct {
				SessionID string `json:"session_id"`
				Handles   struct {
					Control string `json:"control"`
				} `json:"handles"`
				Unified *struct {
					State string `json:"state"`
				} `json:"unified"`
			} `json:"sessions"`
		} `json:"servers"`
	} `json:"realms"`
}

func frontdoorRotationStaticBundle(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".rotation-static-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	for _, name := range []string{"index.html", "app.js", "app.css", "xterm.css", "manifest.webmanifest", "icon-192.png", "icon-512.png", "apple-touch-icon.png"} {
		body := []byte("test")
		if name == "index.html" {
			body = []byte("<!doctype html><title>rotation</title>")
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	relative, err := filepath.Rel(".", dir)
	if err != nil {
		t.Fatal(err)
	}
	return relative
}

func frontdoorRotationStartFrontdoor(t *testing.T, brokerSocket string) (string, *http.Client) {
	t.Helper()
	frontDir, err := os.MkdirTemp("", "rotationf-front-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(frontDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(frontDir) })
	frontSocket := filepath.Join(frontDir, "front.sock")
	cfg := config.Front{
		Ingress: config.Ingress{
			SocketPath: frontSocket, PeerUID: uint32(os.Geteuid()), PeerUIDConfigured: true,
			CanonicalHost: "127.0.0.1:43210", OperatorLogin: "operator@example.com", MaxConnections: 64,
		},
		Realms: []config.Realm{{
			Name: "e2e1", Socket: brokerSocket, BrokerUID: uint32(os.Geteuid()), BrokerUIDConfigured: true,
		}},
		HandleTTLSeconds: 120,
		HandleCapacity:   256,
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSourceFrontdoorHelperProcess$")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(),
		frontdoorRotationFrontHelperEnv+"=1",
		frontdoorRotationFrontConfigEnv+"="+string(raw),
		frontdoorRotationStaticDirEnv+"="+frontdoorRotationStaticBundle(t),
	)
	var childOutput bytes.Buffer
	cmd.Stdout, cmd.Stderr = &childOutput, &childOutput
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(frontSocket); err == nil {
			transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", frontSocket)
			}}
			return frontSocket, &http.Client{Transport: transport, Timeout: 5 * time.Second}
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatalf("frontdoor exited: %s", childOutput.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("frontdoor socket absent: %s", childOutput.String())
	return "", nil
}

func frontdoorRotationSecureHeaders() http.Header {
	return http.Header{
		"X-Forwarded-Host":     []string{"127.0.0.1:43210"},
		"X-Forwarded-Proto":    []string{"http"},
		"Tailscale-User-Login": []string{"operator@example.com"},
		"Origin":               []string{"http://127.0.0.1:43210"},
		"Sec-Fetch-Site":       []string{"same-origin"},
		"Cookie":               []string{"__Host-persea-terminal-csrf=" + frontdoorRotationCSRF},
	}
}

func frontdoorRotationInventoryHandle(t *testing.T, client *http.Client, sessionID string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://localhost/api/inventory", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = frontdoorRotationSecureHeaders()
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("inventory status=%d body=%q", response.StatusCode, body)
	}
	var inventory frontdoorRotationInventory
	if err := json.NewDecoder(response.Body).Decode(&inventory); err != nil {
		t.Fatal(err)
	}
	for _, realm := range inventory.Realms {
		for _, server := range realm.Servers {
			for _, session := range server.Sessions {
				if session.SessionID == sessionID && session.Unified != nil && session.Unified.State == proto.UnifiedSessionOpen {
					if len(session.Handles.Control) != 43 {
						t.Fatalf("control handle length=%d", len(session.Handles.Control))
					}
					return session.Handles.Control
				}
			}
		}
	}
	t.Fatalf("session %q is not projected open: %+v", sessionID, inventory)
	return ""
}

func frontdoorRotationDialController(t *testing.T, frontSocket, handle string) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{
		NetDial: func(_, _ string) (net.Conn, error) { return net.Dial("unix", frontSocket) },
		Subprotocols: []string{
			"persea-terminal.v1", "persea-handle." + handle, "persea-mode.control",
			"persea-csrf." + frontdoorRotationCSRF, "persea-history.5000", "persea-engine.unified-dev",
		},
	}
	header := frontdoorRotationSecureHeaders()
	ws, response, err := dialer.Dial("ws://localhost/ws", header)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("controller dial: %v status=%d", err, status)
	}
	return ws
}

func frontdoorRotationCommitController(t *testing.T, ws *websocket.Conn) terminal.Frame {
	t.Helper()
	return frontdoorRotationCommitControllerWithMarker(t, ws, "")
}

func frontdoorRotationCommitControllerWithMarker(t *testing.T, ws *websocket.Conn, marker string) terminal.Frame {
	t.Helper()
	if err := ws.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var prepared terminal.Frame
	markerSeen := marker == ""
	var markerTail []byte
	observeReplay := func(data []byte) {
		if markerSeen {
			return
		}
		joined := append(markerTail, data...)
		markerSeen = bytes.Contains(joined, []byte(marker))
		keep := len(marker) - 1
		if len(joined) > keep {
			joined = joined[len(joined)-keep:]
		}
		markerTail = append([]byte(nil), joined...)
	}
	for {
		kind, payload, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("controller before COMMIT: %v", err)
		}
		if kind != websocket.TextMessage {
			t.Fatalf("controller message kind=%d", kind)
		}
		frame, err := attachmentwire.Decode(payload, attachmentwire.ServerToBrowser)
		if err != nil {
			t.Fatal(err)
		}
		switch frame.Type {
		case terminal.FramePrepare:
			prepared = frame
			observeReplay(frame.Replay)
			ready, err := attachmentwire.Encode(terminal.Frame{
				Version: terminal.ProtocolVersion, Type: terminal.FrameReady,
				Source: frame.Source, Epoch: frame.Epoch, Cut: frame.Cut,
			}, attachmentwire.BrowserToServer)
			if err != nil || ws.WriteMessage(websocket.TextMessage, ready) != nil {
				t.Fatalf("READY: %v", err)
			}
		case terminal.FrameCommit:
			if prepared.Type != terminal.FramePrepare {
				t.Fatal("COMMIT preceded PREPARE")
			}
			request, err := attachmentwire.Encode(terminal.Frame{
				Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest,
				Source: prepared.Source, Epoch: prepared.Epoch, Mode: terminal.ModeControl,
			}, attachmentwire.BrowserToServer)
			if err != nil {
				t.Fatal(err)
			}
			if err := ws.WriteMessage(websocket.TextMessage, request); err != nil {
				t.Fatal(err)
			}
			controlGranted := false
			for {
				kind, payload, err := ws.ReadMessage()
				if err != nil {
					t.Fatalf("controller before CONTROL and recorded marker (marker seen=%v): %v", markerSeen, err)
				}
				if kind != websocket.TextMessage {
					continue
				}
				mode, err := attachmentwire.Decode(payload, attachmentwire.ServerToBrowser)
				if err != nil {
					t.Fatal(err)
				}
				// A reconstructed snapshot can exceed the first PREPARE's byte
				// cap. Its remaining recorded output arrives on the ordered tail.
				if mode.Type == terminal.FrameLive {
					if mode.Source != prepared.Source || mode.Epoch != prepared.Epoch || mode.Cut != prepared.Cut {
						t.Fatal("recorded replay changed its admitted source")
					}
					observeReplay(mode.Data)
				}
				if mode.Type == terminal.FrameMode && mode.Mode == terminal.ModeControl {
					controlGranted = true
				}
				if controlGranted && markerSeen {
					if err := ws.SetReadDeadline(time.Time{}); err != nil {
						t.Fatal(err)
					}
					return prepared
				}
			}
		}
	}
}

func frontdoorRotationDrainUntilClosed(ws *websocket.Conn) <-chan string {
	done := make(chan string, 1)
	stopPing := make(chan struct{})
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ticker.C:
				ping, err := attachmentwire.EncodeTransportLiveness(attachmentwire.TransportLivenessFrame{
					Kind: attachmentwire.TransportLivenessPing, Nonce: "0123456789abcdef0123456789abcdef",
				}, attachmentwire.BrowserToServer)
				if err != nil || ws.WriteMessage(websocket.TextMessage, ping) != nil {
					return
				}
			}
		}
	}()
	go func() {
		defer close(stopPing)
		defer close(done)
		for {
			_, _, err := ws.ReadMessage()
			if err == nil {
				continue
			}
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				done <- closeErr.Text
			} else {
				done <- err.Error()
			}
			return
		}
	}()
	return done
}

func TestSourceAutomaticRotationFreshFrontdoorController(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux/frontdoor automatic-rotation regression test")
	}
	saved := unifiedJournalCaps
	// Cross 75% pressure while retaining the mandatory 1 MiB rollback reserve.
	// 57,000 rows use 4,047,000 bytes with PTY CRLF: above 3,932,160 and below
	// 5 MiB minus that reserve. Keep the real journal and scheduler path.
	unifiedJournalCaps.pane = 5 << 20
	unifiedJournalCaps.realm = 64 << 20
	unifiedJournalCaps.panePhysical = 16 << 20
	unifiedJournalCaps.realmPhysical = 96 << 20
	t.Cleanup(func() { unifiedJournalCaps = saved })

	disposable := newDisposable(t)
	tmuxServer := config.TmuxServer{Label: "main", SocketPath: disposable.path}
	cfg := unifiedAdoptionDevConfig(t, tmuxServer, t.TempDir(), 4)
	effects, err := NewUnifiedDevPaneEffects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var predecessorCloseMu sync.Mutex
	var predecessorKey unifiedjournal.PaneKey
	predecessorCloseCount := 0
	predecessorCloseReason := proto.SubscriberCloseReason("")
	effects.subscriberCloseEdge = func(key unifiedjournal.PaneKey, _ *unifiedDevSubscriber, reason proto.SubscriberCloseReason) {
		predecessorCloseMu.Lock()
		defer predecessorCloseMu.Unlock()
		if key != predecessorKey {
			return
		}
		predecessorCloseCount++
		predecessorCloseReason = reason
	}
	var rotationEnabled atomic.Bool
	effects.rotationAttempt = func(ctx context.Context, session string) error {
		if !rotationEnabled.Load() {
			return ErrUnifiedRotateUnavailable
		}
		return effects.rotateSession(ctx, session)
	}
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	brokerDone := make(chan error, 1)
	go func() { brokerDone <- ServeWithPaneEffects(listener, cfg, effects) }()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-brokerDone:
		case <-time.After(3 * time.Second):
			t.Error("broker did not stop")
		}
	})
	frontSocket, client := frontdoorRotationStartFrontdoor(t, listener.Addr().String())

	const pressureComplete = "ROTATION-PRESSURE-COMPLETE"
	// Each input line authorizes exactly 1,000 rows. The producer then waits
	// for the test to observe their commit before it can emit the next burst.
	disposable.run("new-session", "-d", "-s", "rotation_live", "-x", "80", "-y", "24",
		"sh", "-c", `stty -echo; awk 'BEGIN { for (b=0; b<57; b++) { getline; for (i=b*1000; i<(b+1)*1000; i++) printf "rotation-AUTO-%06d-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n", i; fflush() } print "`+pressureComplete+`"; fflush() }'; exec sh`)
	sessionID := strings.TrimSpace(disposable.run("display-message", "-p", "-t", "rotation_live:", "#{session_id}"))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	adoption, err := effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	predecessorCloseMu.Lock()
	predecessorKey = adoption.Key
	predecessorCloseMu.Unlock()

	// Journal pressure is cumulative retained output, not an ingress overload.
	// Wait for each bounded burst to commit and release its source credits
	// before asking the producer for more, even when the recorder is delayed.
	pressureRegistry := effects.observer.(*paneRegistry)
	effects.journalMu.Lock()
	pressureBase := effects.realm.CommittedOffset(adoption.Key)
	effects.journalMu.Unlock()
	burst := 0
	pollUntil(t, 15*time.Second, "automatic journal pressure", func() bool {
		effects.journalMu.Lock()
		logical, cap := effects.realm.PaneLogical(adoption.Key)
		committed := effects.realm.CommittedOffset(adoption.Key)
		effects.journalMu.Unlock()
		pressureRegistry.retention.mu.Lock()
		generation := pressureRegistry.retention.generations[adoption.Key]
		failed := generation != nil && generation.failed
		idle := generation != nil && generation.source != nil && generation.source.usage.envelopes == 0
		failure := pressureRegistry.retention.closeErr
		pressureRegistry.retention.mu.Unlock()
		if failed {
			t.Fatalf("pressure recording failed: %v", failure)
		}
		if committed < pressureBase+int64(burst)*71000 || !idle {
			return false
		}
		if burst == 57 {
			return logical*4 >= cap*3
		}
		burst++
		disposable.run("send-keys", "-t", "rotation_live:", "Enter")
		return false
	})
	// Finish the pressure producer before attaching. Otherwise its remaining
	// burst can evict the subscriber for lag while this test is specifically
	// asserting the rotation close reason.
	pollUntil(t, 15*time.Second, "pressure producer recorded", func() bool {
		return bytes.Contains((&adoptionFixture{effects: effects}).journalBytes(t, adoption.Key), []byte(pressureComplete))
	})

	predecessorHandle := frontdoorRotationInventoryHandle(t, client, sessionID)
	predecessorWS := frontdoorRotationDialController(t, frontSocket, predecessorHandle)
	_ = frontdoorRotationCommitController(t, predecessorWS)
	_ = frontdoorRotationDrainUntilClosed(predecessorWS)
	rotationEnabled.Store(true)
	effects.mu.Lock()
	if state := effects.rotationStates[sessionID]; state != nil {
		state.backoff = 0
		state.nextAttempt = time.Time{}
	}
	effects.mu.Unlock()
	effects.wakeRotationScheduler()

	var successorKey unifiedjournal.PaneKey
	pollUntil(t, 25*time.Second, "automatic successor COMMIT", func() bool {
		effects.mu.Lock()
		defer effects.mu.Unlock()
		successorKey = effects.active[sessionID]
		return successorKey != (unifiedjournal.PaneKey{}) && successorKey != adoption.Key
	})
	// The active-key swap and predecessor close share the provider lock, so
	// observing the successor above also observes the completed close.
	predecessorCloseMu.Lock()
	closeCount, closeReason := predecessorCloseCount, predecessorCloseReason
	predecessorCloseMu.Unlock()
	if closeCount != 1 || closeReason != proto.SubscriberClosedGenerationRotated {
		t.Fatalf("predecessor subscriber closes=%d reason=%q, want one %q", closeCount, closeReason, proto.SubscriberClosedGenerationRotated)
	}
	_ = predecessorWS.Close()
	pollUntil(t, 10*time.Second, "automatic rotation settlement", func() bool {
		effects.mu.Lock()
		defer effects.mu.Unlock()
		return effects.rotation == nil && effects.active[sessionID] == successorKey
	})

	const replayMarker = "ROTATION-SUCCESSOR-REPLAY"
	disposable.run("send-keys", "-t", "rotation_live:", "printf '"+replayMarker+"\\n'", "Enter")
	pollUntil(t, 10*time.Second, "successor marker commit", func() bool {
		return bytes.Contains((&adoptionFixture{effects: effects}).journalBytes(t, successorKey), []byte(replayMarker))
	})

	// This first successor controller models the automatic reattach. Its COMMIT
	// is complete before it closes, and the proof below waits for the whole
	// successor subscriber bucket to disappear.
	successorHandle := frontdoorRotationInventoryHandle(t, client, sessionID)
	if successorHandle == predecessorHandle {
		t.Fatal("successor reused consumed predecessor handle")
	}
	successorWS := frontdoorRotationDialController(t, frontSocket, successorHandle)
	_ = frontdoorRotationCommitController(t, successorWS)
	_ = successorWS.Close()
	pollUntil(t, 5*time.Second, "successor subscriber bucket removal", func() bool {
		effects.subscriberMu.Lock()
		defer effects.subscriberMu.Unlock()
		_, exists := effects.subscribers[successorKey]
		return !exists
	})

	registry, ok := effects.observer.(*paneRegistry)
	if !ok {
		t.Fatal("production registry is absent")
	}
	registry.mu.Lock()
	state, admitted := registry.admitted[routeCoordinateJournalKey(successorKey)]
	session := registry.sessions[sessionKey(state.witness.Session)]
	routerEligible := admitted && session != nil && session.router.UnifiedEligible(state.witness)
	registry.mu.Unlock()
	effects.mu.Lock()
	activeKey, active := effects.active[sessionID]
	effects.mu.Unlock()
	effects.journalMu.Lock()
	journalEligible := effects.realm.UnifiedEligible(successorKey)
	effects.journalMu.Unlock()
	if !active || activeKey != successorKey || !admitted || !routerEligible || !journalEligible {
		t.Fatalf("successor route active=%v key=%+v admitted=%v router=%v journal=%v", active, activeKey, admitted, routerEligible, journalEligible)
	}

	// Inventory is still live, but this is a newly minted, one-time frontdoor
	// handle and a brand-new controller, not the automatic reattach socket and
	// not a pre-rotation broker authority.
	freshHandle := frontdoorRotationInventoryHandle(t, client, sessionID)
	if freshHandle == predecessorHandle || freshHandle == successorHandle {
		t.Fatal("fresh controller did not receive a new one-time handle")
	}
	freshWS := frontdoorRotationDialController(t, frontSocket, freshHandle)
	defer freshWS.Close()
	prepared := frontdoorRotationCommitControllerWithMarker(t, freshWS, replayMarker)
	if prepared.Columns != 80 || prepared.Rows != 24 {
		t.Fatalf("fresh geometry=%dx%d want 80x24", prepared.Columns, prepared.Rows)
	}
	const inputMarker = "ROTATION-FRESH-INPUT"
	input, err := attachmentwire.Encode(terminal.Frame{
		Version: terminal.ProtocolVersion, Type: terminal.FrameInput,
		Source: prepared.Source, Epoch: prepared.Epoch, Data: []byte("printf '" + inputMarker + "\\n'\n"),
	}, attachmentwire.BrowserToServer)
	if err != nil {
		t.Fatal(err)
	}
	if err := freshWS.WriteMessage(websocket.TextMessage, input); err != nil {
		t.Fatal(err)
	}
	if err := freshWS.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var visible []byte
	for !bytes.Contains(visible, []byte(inputMarker)) {
		kind, payload, err := freshWS.ReadMessage()
		if err != nil {
			t.Fatalf("fresh input was not live: visible=%q err=%v", visible, err)
		}
		if kind != websocket.TextMessage {
			continue
		}
		frame, err := attachmentwire.Decode(payload, attachmentwire.ServerToBrowser)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == terminal.FrameLive {
			visible = append(visible, frame.Data...)
		}
	}
}
