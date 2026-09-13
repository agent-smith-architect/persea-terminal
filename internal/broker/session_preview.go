package broker

import (
	"errors"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

// Dashboard session preview.
//
// A preview is an on-demand, read-only glance at what a session's ACTIVE pane
// is showing right now, so the operator can peek from the dashboard without
// opening the session. It is deliberately NOT a snapshot pipeline: nothing is
// captured periodically, nothing is stored anywhere, and the pane's state is
// never touched — one bounded capture per request, produced only when the
// dashboard asks. Previews work for ANY session, multi-window and multi-pane
// included, because "the active pane" is always well defined; they carry no
// unified-eligibility coupling.
//
// Refusals are a closed code set the UI maps to its own copy, exactly like
// creation and adoption: free text never crosses this boundary.

// PreviewByteLimit bounds the combined plain and styled preview text.
// Together with proto.PreviewRowLimit it keeps the preview_ok control frame
// far below proto.MaxControl even under worst-case JSON escaping.
const PreviewByteLimit = 8 * 1024

func (s *Server) preview(writer *lockedWriter, ctrl proto.Control) {
	refuse := func(code string) {
		log.Printf("component=broker event=preview_refused realm=%q server=%q session=%q reason=%q", s.config.Realm, ctrl.ServerLabel, ctrl.SessionID, code)
		_ = writer.control(proto.Control{Type: "preview_refused", Code: code})
	}
	var server config.TmuxServer
	found := false
	for _, candidate := range s.config.Servers {
		if candidate.Label == ctrl.ServerLabel {
			server, found = candidate, true
			break
		}
	}
	if !found {
		refuse("server_unavailable")
		return
	}
	if !validSessionID(ctrl.SessionID) {
		refuse("session_gone")
		return
	}
	// details() resolves the session's ACTIVE pane and its geometry in one
	// display-message call. Measured on tmux 3.4: pane formats under a session
	// target expand against the session's active window and pane.
	d, err := details(server, ctrl.SessionID)
	if err != nil {
		if errors.Is(err, errNoServer) {
			refuse("server_unavailable")
			return
		}
		refuse("session_gone")
		return
	}
	// ONE colored capture of the resolved active pane, targeted by pane ID
	// (measured: name-based targets are unreliable, IDs are not): the last
	// proto.PreviewRowLimit history rows plus the visible screen. The color
	// flag changes capture formatting only; it does not attach or resize.
	out, err := tmuxOutput(server, "capture-pane", "-p", "-e", "-t", d.PaneID, "-S", strconv.Itoa(-proto.PreviewRowLimit), "-E", "-")
	if err != nil {
		if errors.Is(err, errNoServer) {
			refuse("server_unavailable")
			return
		}
		refuse("preview_failed")
		return
	}
	// Keep the text representation for older dashboards. The separate styled
	// representation permits only bounded SGR, never terminal commands or OSC.
	data, truncated := boundHistory([]byte(previewRowsWithStyle(sanitizePreviewANSI(out))), proto.PreviewRowLimit, PreviewByteLimit/2)
	styled := sanitizePreviewANSI(string(data))
	lines := strings.Split(strings.TrimSuffix(previewSGR.ReplaceAllString(styled, ""), "\n"), "\n")
	ansiLines := strings.Split(strings.TrimSuffix(styled, "\n"), "\n")
	_ = writer.control(proto.Control{
		Type:      "preview_ok",
		Width:     d.PaneWidth,
		Height:    d.PaneHeight,
		Pane:      d.PaneID,
		FrozenAt:  time.Now().UnixMilli(),
		Truncated: truncated,
		Lines:     lines,
		ANSILines: ansiLines,
	})
}

var previewSGR = regexp.MustCompile("\x1b\\[[0-9;:]{0,96}m")

// Every row starts with its own style. Dropping old rows to meet the response
// bound must not also drop the color that was set earlier in the capture.
func previewRowsWithStyle(in string) string {
	state := map[string]string{}
	order := []string{"bold", "dim", "italic", "underline", "inverse", "conceal", "strike", "foreground", "background"}
	lines := strings.Split(in, "\n")
	for index, line := range lines {
		if index == len(lines)-1 && line == "" {
			break
		}
		var active []string
		for _, key := range order {
			if value, ok := state[key]; ok {
				active = append(active, value)
			}
		}
		prefix := "\x1b[0m"
		if len(active) > 0 {
			prefix += "\x1b[" + strings.Join(active, ";") + "m"
		}
		lines[index] = prefix + line
		for _, sequence := range previewSGR.FindAllString(line, -1) {
			values := strings.Split(sequence[2:len(sequence)-1], ";")
			for i := 0; i < len(values); i++ {
				value := values[i]
				code, _ := strconv.Atoi(strings.Split(value, ":")[0])
				switch code {
				case 0:
					clear(state)
				case 1:
					state["bold"] = value
				case 2:
					state["dim"] = value
				case 3:
					state["italic"] = value
				case 4:
					state["underline"] = value
				case 7:
					state["inverse"] = value
				case 8:
					state["conceal"] = value
				case 9:
					state["strike"] = value
				case 22:
					delete(state, "bold")
					delete(state, "dim")
				case 23:
					delete(state, "italic")
				case 24:
					delete(state, "underline")
				case 27:
					delete(state, "inverse")
				case 28:
					delete(state, "conceal")
				case 29:
					delete(state, "strike")
				case 39:
					delete(state, "foreground")
				case 49:
					delete(state, "background")
				case 38, 48:
					key := "foreground"
					if code == 48 {
						key = "background"
					}
					if !strings.Contains(value, ":") && i+1 < len(values) {
						count := 0
						if values[i+1] == "5" {
							count = 2
						}
						if values[i+1] == "2" {
							count = 4
						}
						if count > 0 && i+count < len(values) {
							value = strings.Join(values[i:i+count+1], ";")
							i += count
						}
					}
					state[key] = value
				default:
					if code >= 30 && code <= 37 || code >= 90 && code <= 97 {
						state["foreground"] = value
					}
					if code >= 40 && code <= 47 || code >= 100 && code <= 107 {
						state["background"] = value
					}
				}
			}
		}
	}
	return strings.Join(lines, "\n")
}

// A capture normally contains only text and SGR generated by tmux. Drop every
// other escape sequence in full, including string payloads, before transport.
func sanitizePreviewANSI(in string) string {
	var out strings.Builder
	for i := 0; i < len(in); {
		if in[i] != 0x1b {
			c := in[i]
			if c >= 0x20 && c != 0x7f || c == '\n' || c == '\t' {
				out.WriteByte(c)
			}
			i++
			continue
		}
		start := i
		i++
		if i == len(in) {
			break
		}
		switch in[i] {
		case '[':
			i++
			for i < len(in) && (in[i] < 0x40 || in[i] > 0x7e) {
				i++
			}
			if i < len(in) {
				i++
				sequence := in[start:i]
				if match := previewSGR.FindString(sequence); match == sequence {
					out.WriteString(sequence)
				}
			}
		case ']', 'P', 'X', '^', '_':
			i++
			for i < len(in) {
				if in[i] == 0x07 {
					i++
					break
				}
				if in[i] == 0x1b && i+1 < len(in) && in[i+1] == '\\' {
					i += 2
					break
				}
				i++
			}
		default:
			for i < len(in) && in[i] >= 0x20 && in[i] <= 0x2f {
				i++
			}
			if i < len(in) {
				i++
			}
		}
	}
	return strings.Map(func(char rune) rune {
		if char >= 0x80 && char <= 0x9f {
			return -1
		}
		return char
	}, strings.ToValidUTF8(out.String(), "�"))
}

// stripPreviewControlBytes removes the control range 0x00-0x1f except \n
// (row boundary) and \t (layout the preview keeps). Plain capture output is
// normally free of these already; this is defence in depth for anything a
// terminal manages to park in its visible cells.
func stripPreviewControlBytes(in string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, in)
}
