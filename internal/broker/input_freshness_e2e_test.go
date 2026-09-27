package broker

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

type freshnessTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *freshnessTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *freshnessTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// freshnessClient drives a Control attachment the way the front door and page
// do together: it counts the attachment frames it has read, which is the scale
// a consumption receipt uses.
type freshnessClient struct {
	t        *testing.T
	conn     net.Conn
	prepared terminal.Frame
	frames   uint64
	data     strings.Builder
}

func (c *freshnessClient) read() (proto.Frame, terminal.Frame) {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	raw, err := proto.ReadFrame(c.conn)
	if err != nil {
		c.t.Fatal(err)
	}
	if raw.Type != proto.FrameAttachment {
		return raw, terminal.Frame{}
	}
	c.frames++
	frame, err := attachmentwire.Decode(raw.Payload, attachmentwire.ServerToBrowser)
	if err != nil {
		c.t.Fatal(err)
	}
	if frame.Type == terminal.FrameLive {
		c.data.Write(frame.Data)
	}
	return raw, frame
}

func (c *freshnessClient) attachment(frame terminal.Frame) {
	c.t.Helper()
	raw, err := attachmentwire.Encode(frame, attachmentwire.BrowserToServer)
	if err != nil || proto.WriteFrame(c.conn, proto.FrameAttachment, raw) != nil {
		c.t.Fatalf("write %s: %v", frame.Type, err)
	}
}

func (c *freshnessClient) input(text string) {
	c.t.Helper()
	c.attachment(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameInput, Source: c.prepared.Source, Epoch: c.prepared.Epoch, Data: []byte(text)})
}

func (c *freshnessClient) receipt(frames uint64) {
	c.t.Helper()
	payload, err := proto.MarshalControl(proto.Control{Type: "consumed", Frames: frames})
	if err != nil || proto.WriteFrame(c.conn, proto.FrameControl, payload) != nil {
		c.t.Fatalf("receipt: %v", err)
	}
}

// controlError reads until the broker's next error control.
func (c *freshnessClient) controlError() string {
	c.t.Helper()
	for {
		raw, _ := c.read()
		if raw.Type != proto.FrameControl {
			continue
		}
		control, err := proto.DecodeControl(raw.Payload)
		if err != nil {
			c.t.Fatal(err)
		}
		if control.Type == "error" {
			return control.Code
		}
	}
}

// readUntil reads until the terminal output contains want.
func (c *freshnessClient) readUntil(want string) {
	c.t.Helper()
	for !strings.Contains(c.data.String(), want) {
		c.read()
	}
}

func openFreshnessClient(t *testing.T, f *adoptionFixture, session string) *freshnessClient {
	t.Helper()
	if _, err := f.effects.AdoptSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(shortTempDir(t), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server := &Server{config: f.cfg, panes: f.registry, unified: f.effects}
	go func() { _ = server.accept(listener) }()
	d, err := details(f.server, session)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(f.server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = f.cfg.Realm, "main", d.ID, d.Created
	conn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	unifiedE2E1Hello(t, conn)
	history := 5000
	if attached := unifiedE2E1Control(t, conn, proto.Control{Type: "attach", Mode: "control", Engine: "unified-dev", Authority: &authority, HistoryLimit: &history}); attached.Type != "attach_ok" {
		t.Fatalf("attach: %+v", attached)
	}
	c := &freshnessClient{t: t, conn: conn}
	if _, c.prepared = c.read(); c.prepared.Type != terminal.FramePrepare {
		t.Fatalf("first frame %s, want PREPARE", c.prepared.Type)
	}
	c.attachment(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameReady, Source: c.prepared.Source, Epoch: c.prepared.Epoch, Cut: c.prepared.Cut})
	c.attachment(terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameModeRequest, Source: c.prepared.Source, Epoch: c.prepared.Epoch, Mode: terminal.ModeControl})
	for {
		if _, frame := c.read(); frame.Type == terminal.FrameMode && frame.Mode == terminal.ModeControl {
			return c
		}
	}
}

// A Control page that has not consumed output older than the freshness window
// has its input dropped with input_paused; after it catches up, input stays
// paused until typing has been quiet, then flows again; a page that keeps
// consuming types freely however much time passes.
func TestUnifiedInputIsPausedUntilThePageHasConsumedRecentOutput(t *testing.T) {
	const window, quiet = time.Second, 500 * time.Millisecond
	clock := &freshnessTestClock{now: time.Now()}
	f := newRecordingSettlementFixture(t, func(effects *UnifiedDevPaneEffects) {
		effects.inputFreshnessWindow, effects.inputResumeQuiet, effects.freshnessNow = window, quiet, clock.Now
	})
	session := f.startPaneCommand(t, "fresh-gate", "PS1='fresh> ' exec /bin/sh")
	client := openFreshnessClient(t, f, session)
	shown := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(f.capture(t, "fresh-gate"), want) {
			if time.Now().After(deadline) {
				t.Fatalf("%q never reached the pane", want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Nothing has been receipted, and the snapshot is now a window old.
	clock.advance(window + time.Millisecond)
	client.input("printf 'EARLY-%s\\n' INPUT\n")
	if code := client.controlError(); code != "input_paused" {
		t.Fatalf("unconsumed snapshot: error %q, want input_paused", code)
	}
	// Receipting what was read covers the snapshot, but typing has not been
	// quiet since the refusal.
	client.receipt(client.frames)
	client.input("printf 'QUIET-%s\\n' BREACH\n")
	if code := client.controlError(); code != "input_paused" {
		t.Fatalf("input inside the quiet period: error %q, want input_paused", code)
	}
	clock.advance(quiet)
	client.input("printf 'AFTER-%s\\n' CATCHUP\n")
	shown("AFTER-CATCHUP")

	// A page that consumes what it is sent keeps typing freely.
	client.readUntil("AFTER-CATCHUP\r\nfresh> ")
	client.receipt(client.frames)
	clock.advance(3 * window)
	client.input("printf 'STILL-%s\\n' FRESH\n")
	shown("STILL-FRESH")

	pane := f.capture(t, "fresh-gate")
	for _, dropped := range []string{"EARLY-INPUT", "QUIET-BREACH", "EARLY-%s", "QUIET-%s"} {
		if strings.Contains(pane, dropped) {
			t.Fatalf("refused input %q reached the pane:\n%s", dropped, pane)
		}
	}
}

// A receipt for frames the broker never wrote, or one that does not advance,
// ends the attachment as a protocol violation.
func TestUnifiedConsumptionReceiptOutOfRangeEndsTheAttachment(t *testing.T) {
	for name, frames := range map[string]func(*freshnessClient) []uint64{
		"beyond written":   func(c *freshnessClient) []uint64 { return []uint64{c.frames + 1_000_000} },
		"does not advance": func(c *freshnessClient) []uint64 { return []uint64{c.frames, c.frames} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newRecordingSettlementFixture(t, func(*UnifiedDevPaneEffects) {})
			session := f.startPaneCommand(t, "receipt-range", "exec /bin/sh")
			client := openFreshnessClient(t, f, session)
			for _, count := range frames(client) {
				client.receipt(count)
			}
			if code := client.controlError(); code != "protocol" {
				t.Fatalf("error %q, want protocol", code)
			}
		})
	}
}
