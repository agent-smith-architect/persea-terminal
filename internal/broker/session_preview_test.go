package broker

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func previewControl(t *testing.T, server *Server, serverLabel, sessionID string) proto.Control {
	t.Helper()
	var output bytes.Buffer
	server.preview(&lockedWriter{w: &output}, proto.Control{Type: "preview", ServerLabel: serverLabel, SessionID: sessionID})
	frame, err := proto.ReadFrame(&output)
	if err != nil {
		t.Fatal(err)
	}
	control, err := proto.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return control
}

// Refusals are typed before any tmux process is consulted: an unknown server
// label and a malformed session identity each fail closed with their own code,
// and a configured-but-absent server reports server_unavailable rather than a
// free-text transport error.
func TestPreviewRefusalsAreTypedWithoutFreeText(t *testing.T) {
	dir := shortTempDir(t)
	server := &Server{config: config.Broker{Realm: "local", Servers: []config.TmuxServer{{Label: "main", SocketPath: filepath.Join(dir, "absent.sock")}}}}
	for name, tc := range map[string]struct {
		label, session, code string
	}{
		"unknown_server":  {"other", "$1", "server_unavailable"},
		"name_not_id":     {"main", "alpha", "session_gone"},
		"empty_session":   {"main", "", "session_gone"},
		"overlong_id":     {"main", "$123456789012345678901234567890123", "session_gone"},
		"absent_server":   {"main", "$1", "server_unavailable"},
		"injection_shape": {"main", "$1; kill-server", "session_gone"},
	} {
		t.Run(name, func(t *testing.T) {
			got := previewControl(t, server, tc.label, tc.session)
			if got.Type != "preview_refused" || got.Code != tc.code {
				t.Fatalf("preview(%q, %q) = %+v, want refusal %q", tc.label, tc.session, got, tc.code)
			}
			if got.Msg != "" || len(got.Lines) != 0 {
				t.Fatalf("refusal leaked content: %+v", got)
			}
		})
	}
}

func TestStripPreviewControlBytes(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"escape_sequences": {"a\x1b[31mb\x1b[0mc", "a[31mb[0mc"},
		"crlf":             {"one\r\ntwo\r", "one\ntwo"},
		"kept_layout":      {"col\tumn\nrow", "col\tumn\nrow"},
		"low_controls":     {"\x00\x01\x07\x08x\x0b\x0c\x1f", "x"},
		"utf8_preserved":   {"héllo … 世界", "héllo … 世界"},
		"empty":            {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := stripPreviewControlBytes(tc.in); got != tc.want {
				t.Fatalf("strip(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The preview op against a real disposable tmux server: marker rows printed
// into a session's active pane come back as plain rows with the pane's exact
// geometry, tabs preserved, and no escape byte surviving the strip — while the
// pane state is left untouched (read-only: no mode, no size, no focus change).
func TestPreviewControlOpCapturesActivePane(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux preview lifecycle")
	}
	d := newDisposable(t)
	server := &Server{config: config.Broker{Realm: "local", Servers: []config.TmuxServer{d.tmux}}}
	d.run("new-session", "-d", "-s", "previewed", "-x", "80", "-y", "24",
		"sh -c 'printf \"PREVIEW_ALPHA\\nPREVIEW\\tTAB\\n\\033[31mPREVIEW_RED\\033[0m\\n\"; sleep 600'")
	id := d.run("display-message", "-p", "-t", "=previewed:", "-F", "#{session_id}")

	var got proto.Control
	waitFor(t, func() bool {
		got = previewControl(t, server, "test", id)
		return got.Type == "preview_ok" && strings.Contains(strings.Join(got.Lines, "\n"), "PREVIEW_RED")
	})
	if got.Width != 80 || got.Height != 24 {
		t.Fatalf("preview geometry = %dx%d, want 80x24: %+v", got.Width, got.Height, got)
	}
	if !validPaneID(got.Pane) {
		t.Fatalf("preview pane %q is not a pane ID", got.Pane)
	}
	if got.FrozenAt <= 0 {
		t.Fatalf("preview carries no captured-at time: %+v", got)
	}
	if got.Truncated {
		t.Fatalf("small capture reported truncation: %+v", got)
	}
	joined := strings.Join(got.Lines, "\n")
	// Measured: the pane grid holds RENDERED cells, so a printed \t arrives as
	// the spaces the terminal advanced over, never as a tab byte.
	if !strings.Contains(joined, "PREVIEW_ALPHA") || !strings.Contains(joined, "PREVIEW TAB") {
		t.Fatalf("marker rows did not come back: %q", joined)
	}
	if strings.ContainsAny(joined, "\x1b\x07\x00\r") {
		t.Fatalf("control bytes escaped the strip: %q", joined)
	}
	styled := strings.Join(got.ANSILines, "\n")
	if !strings.Contains(styled, "\x1b[31m") || previewSGR.ReplaceAllString(styled, "") != joined {
		t.Fatalf("colored and plain snapshots disagree: %q", styled)
	}
	if len(got.Lines) > proto.PreviewRowLimit+1 {
		t.Fatalf("preview exceeded its row bound: %d rows", len(got.Lines))
	}

	// Read-only: the previewed pane is still on its primary screen with its
	// original geometry, and no client got attached by the capture.
	after := d.run("display-message", "-p", "-t", "=previewed:", "-F", "#{alternate_on}\t#{pane_width}\t#{pane_height}\t#{session_attached}")
	if after != "0\t80\t24\t0" {
		t.Fatalf("preview disturbed pane state: %q", after)
	}

	if refused := previewControl(t, server, "test", "$424242"); refused.Type != "preview_refused" || refused.Code != "session_gone" {
		t.Fatalf("missing-session preview = %+v", refused)
	}
}

// A pane with deep history is bounded to the newest PreviewRowLimit rows: the
// tail survives, the oldest rows fall off, and the bound is surfaced as
// truncated rather than silently.
func TestPreviewBoundsRowsToNewestLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux preview lifecycle")
	}
	d := newDisposable(t)
	server := &Server{config: config.Broker{Realm: "local", Servers: []config.TmuxServer{d.tmux}}}
	d.run("new-session", "-d", "-s", "deep", "-x", "80", "-y", "24",
		"sh -c 'i=1; while [ $i -le 200 ]; do echo \"ROW_$i\"; i=$((i+1)); done; echo DEEP_DONE; sleep 600'")
	id := d.run("display-message", "-p", "-t", "=deep:", "-F", "#{session_id}")

	var got proto.Control
	waitFor(t, func() bool {
		got = previewControl(t, server, "test", id)
		return got.Type == "preview_ok" && strings.Contains(strings.Join(got.Lines, "\n"), "DEEP_DONE")
	})
	if len(got.Lines) > proto.PreviewRowLimit+1 {
		t.Fatalf("deep preview exceeded its row bound: %d rows", len(got.Lines))
	}
	joined := strings.Join(got.Lines, "\n")
	if !strings.Contains(joined, "ROW_200") {
		t.Fatalf("newest rows were dropped: %q", joined)
	}
	if strings.Contains(joined, "ROW_100") {
		t.Fatalf("oldest rows survived the bound: %q", joined)
	}
	if !got.Truncated {
		t.Fatalf("bounded deep preview did not surface truncation: %+v", got)
	}
}
