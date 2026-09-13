package broker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

var errNoServer = errors.New("tmux server is not running")
var errStaleTarget = errors.New("stale target")

func selector(s config.TmuxServer) []string {
	if s.SocketName != "" {
		return []string{"-L", s.SocketName}
	}
	return []string{"-S", s.SocketPath}
}

func selectorIdentity(s config.TmuxServer) (string, string, error) {
	if s.SocketName != "" && s.SocketPath == "" {
		return "socket_name", s.SocketName, nil
	}
	if s.SocketPath != "" && s.SocketName == "" && filepath.IsAbs(s.SocketPath) && filepath.Clean(s.SocketPath) == s.SocketPath {
		return "socket_path", s.SocketPath, nil
	}
	return "", "", errStaleTarget
}

func tmuxArgv(s config.TmuxServer, args ...string) []string {
	// Structured responses use tabs and UTF-8 text regardless of the service
	// locale. Without -u, tmux replaces tab delimiters with underscores in C.
	return append([]string{"tmux", "-u"}, append(selector(s), args...)...)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func incarnationCondition(want proto.Authority) string {
	return "pid=" + shellQuote("#{pid}") +
		"; case $pid in ''|*[!0-9]*) exit 1;; esac" +
		"; [ \"$pid\" = " + shellQuote(strconv.Itoa(want.ServerPID)) + " ] || exit 1" +
		"; [ \"$(cat /proc/sys/kernel/random/boot_id)\" = " + shellQuote(want.BootID) + " ] || exit 1" +
		"; stat=$(cat \"/proc/$pid/stat\") || exit 1" +
		"; suffix=${stat##*) }; set -- $suffix" +
		"; [ \"$#\" -ge 20 ] && [ \"${20}\" = " + shellQuote(strconv.FormatUint(want.ServerStart, 10)) + " ] || exit 1" +
		"; uid=$(awk '/^Uid:/{print $2}' \"/proc/$pid/status\") || exit 1" +
		"; [ \"$uid\" = " + shellQuote(strconv.FormatUint(uint64(want.UID), 10)) + " ] || exit 1" +
		"; [ " + shellQuote("#{session_created}") + " = " + shellQuote(strconv.FormatInt(want.SessionCreated, 10)) + " ] || exit 1"
}

func invocationSentinel(label string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return "PERSEA_" + hex.EncodeToString(nonce[:]) + "_" + label, nil
}

func guardedSnapshotArgv(s config.TmuxServer, want proto.Authority, paneID string, depth int, metadataSentinel, staleSentinel string) ([]string, error) {
	if !validSessionID(want.SessionID) {
		return nil, errStaleTarget
	}
	if !validPaneID(paneID) {
		return nil, errStaleTarget
	}
	if depth < 1 || depth > SnapshotLineLimit {
		return nil, fmt.Errorf("invalid snapshot depth")
	}
	metadata := metadataSentinel + "|#{pane_id}|#{pane_width}|#{pane_height}|#{alternate_on}|#{history_size}|#{session_id}"
	success := fmt.Sprintf("capture-pane -p -e -S -%d -t %s ; display-message -p -t %s -F %s", depth, paneID, paneID, shellQuote(metadata))
	staleCommand := "display-message -p " + staleSentinel
	return tmuxArgv(s, "if-shell", "-t", paneID, incarnationCondition(want), success, staleCommand), nil
}

func guardedAttachArgv(s config.TmuxServer, want proto.Authority, mode, staleSentinel string) ([]string, error) {
	if !validSessionID(want.SessionID) {
		return nil, errStaleTarget
	}
	if mode != "observe" && mode != "control" {
		return nil, fmt.Errorf("invalid attach mode")
	}
	readonly := ""
	if mode == "observe" {
		readonly = " -r"
	}
	success := "attach-session" + readonly + " -f ignore-size -t =" + want.SessionID
	staleCommand := "display-message -p " + staleSentinel
	return tmuxArgv(s, "if-shell", "-t", "="+want.SessionID+":", incarnationCondition(want), success, staleCommand), nil
}

func validSessionID(id string) bool {
	if len(id) < 2 || len(id) > 32 || id[0] != '$' {
		return false
	}
	_, err := strconv.ParseUint(id[1:], 10, 31)
	return err == nil
}

func validPaneID(id string) bool {
	if len(id) < 2 || len(id) > 32 || id[0] != '%' {
		return false
	}
	_, err := strconv.ParseUint(id[1:], 10, 31)
	return err == nil
}

func tmuxOutput(s config.TmuxServer, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	argv := tmuxArgv(s, args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out, stderr limitedBuffer
	out.max = 1024 * 1024
	stderr.max = 64 * 1024
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "no server running") || strings.Contains(msg, "failed to connect") || strings.Contains(msg, "No such file") {
			return "", fmt.Errorf("%w: %s", errNoServer, msg)
		}
		return "", fmt.Errorf("tmux %s failed: %s", args[0], msg)
	}
	if out.overflow {
		return "", fmt.Errorf("tmux output exceeds limit")
	}
	return out.String(), nil
}

type snapshotCapture struct {
	Output      string
	Truncated   bool
	PaneID      string
	Width       int
	Height      int
	Alternate   bool
	HistorySize int
}

func guardedSnapshot(s config.TmuxServer, want proto.Authority, paneID string, depth int) (snapshotCapture, error) {
	staleSentinel, err := invocationSentinel("STALE")
	if err != nil {
		return snapshotCapture{}, err
	}
	metadataSentinel, err := invocationSentinel("META")
	if err != nil {
		return snapshotCapture{}, err
	}
	argv, err := guardedSnapshotArgv(s, want, paneID, depth, metadataSentinel, staleSentinel)
	if err != nil {
		return snapshotCapture{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out tailLimitedBuffer
	out.max = SnapshotByteLimit + proto.MaxData + 1024
	var stderr limitedBuffer
	stderr.max = 64 * 1024
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if strings.TrimSpace(out.String()) == staleSentinel {
		return snapshotCapture{}, errStaleTarget
	}
	if runErr != nil {
		return snapshotCapture{}, classifySnapshotFailure(s, want, paneID, runErr)
	}
	capture, err := parseSnapshotCapture(out.String(), out.truncated, want, metadataSentinel)
	if err != nil {
		return snapshotCapture{}, classifySnapshotFailure(s, want, paneID, err)
	}
	return capture, nil
}

func classifySnapshotFailure(s config.TmuxServer, want proto.Authority, paneID string, cause error) error {
	if err := revalidateSnapshotTarget(s, want, paneID); err != nil {
		if errors.Is(err, errStaleTarget) {
			return errStaleTarget
		}
		return fmt.Errorf("guarded snapshot failed: %w (target revalidation failed: %v)", cause, err)
	}
	return fmt.Errorf("guarded snapshot failed: %w", cause)
}

func revalidateSnapshotTarget(s config.TmuxServer, want proto.Authority, paneID string) error {
	if want.Server != s.Label || !validSessionID(want.SessionID) || !validPaneID(paneID) {
		return errStaleTarget
	}
	got, err := incarnation(s)
	if err != nil {
		if errors.Is(err, errNoServer) {
			return errStaleTarget
		}
		return fmt.Errorf("revalidate snapshot incarnation: %w", err)
	}
	if !sameIncarnation(got, want) {
		return errStaleTarget
	}
	out, err := tmuxOutput(s, "list-panes", "-a", "-F", "#{session_id}\t#{pane_id}")
	if err != nil {
		if errors.Is(err, errNoServer) {
			return errStaleTarget
		}
		after, incarnationErr := incarnation(s)
		if errors.Is(incarnationErr, errNoServer) || (incarnationErr == nil && !sameIncarnation(after, want)) {
			return errStaleTarget
		}
		return fmt.Errorf("revalidate snapshot panes: %w", err)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 2 || !validSessionID(fields[0]) || !validPaneID(fields[1]) {
			return fmt.Errorf("invalid snapshot pane revalidation row")
		}
		if fields[0] == want.SessionID && fields[1] == paneID {
			found = true
		}
	}
	if !found {
		return errStaleTarget
	}
	return nil
}

func parseSnapshotCapture(out string, truncated bool, want proto.Authority, sentinel string) (snapshotCapture, error) {
	marker := sentinel + "|"
	index := strings.LastIndex(out, "\n"+marker)
	dataEnd := index + 1
	if index < 0 && strings.HasPrefix(out, marker) {
		dataEnd = 0
		index = 0
	}
	if index < 0 {
		return snapshotCapture{}, errStaleTarget
	}
	line := strings.TrimSuffix(out[dataEnd:], "\n")
	if strings.Contains(line, "\n") {
		return snapshotCapture{}, errStaleTarget
	}
	fields := strings.Split(line, "|")
	if len(fields) != 7 || fields[0] != sentinel || !validPaneID(fields[1]) || fields[1] == "" || fields[6] != want.SessionID {
		return snapshotCapture{}, errStaleTarget
	}
	width, widthErr := strconv.Atoi(fields[2])
	height, heightErr := strconv.Atoi(fields[3])
	historySize, historyErr := strconv.Atoi(fields[5])
	if widthErr != nil || heightErr != nil || historyErr != nil || width < 1 || width > 1000 || height < 1 || height > 1000 || historySize < 0 || (fields[4] != "0" && fields[4] != "1") {
		return snapshotCapture{}, errStaleTarget
	}
	return snapshotCapture{Output: out[:dataEnd], Truncated: truncated, PaneID: fields[1], Width: width, Height: height, Alternate: fields[4] == "1", HistorySize: historySize}, nil
}

type tailLimitedBuffer struct {
	bytes.Buffer
	max       int
	truncated bool
}

func (b *tailLimitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= b.max {
		b.Buffer.Reset()
		_, _ = b.Buffer.Write(p[n-b.max:])
		b.truncated = true
		return n, nil
	}
	if b.Len()+n > b.max {
		drop := b.Len() + n - b.max
		kept := append([]byte(nil), b.Bytes()[drop:]...)
		b.Buffer.Reset()
		_, _ = b.Buffer.Write(kept)
		b.truncated = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

type limitedBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.max - b.Len()
	if remaining > 0 {
		if remaining > n {
			remaining = n
		}
		_, _ = b.Buffer.Write(p[:remaining])
	}
	if n > remaining {
		b.overflow = true
	}
	return n, nil
}

func incarnation(s config.TmuxServer) (proto.Authority, error) {
	out, err := tmuxOutput(s, "display-message", "-p", "#{pid}")
	if err != nil {
		return proto.Authority{}, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || pid < 1 {
		return proto.Authority{}, fmt.Errorf("invalid tmux pid")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return proto.Authority{}, err
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return proto.Authority{}, err
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	if closeParen < 0 {
		return proto.Authority{}, fmt.Errorf("invalid proc stat")
	}
	fields := strings.Fields(string(stat)[closeParen+1:])
	if len(fields) <= 19 {
		return proto.Authority{}, fmt.Errorf("short proc stat")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return proto.Authority{}, err
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return proto.Authority{}, err
	}
	var uid uint64
	foundUID := false
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return proto.Authority{}, fmt.Errorf("invalid proc uid")
			}
			uid, err = strconv.ParseUint(parts[1], 10, 32)
			foundUID = err == nil
			break
		}
	}
	if !foundUID || uint32(uid) != uint32(os.Getuid()) {
		return proto.Authority{}, fmt.Errorf("tmux uid does not match broker uid")
	}
	kind, value, err := selectorIdentity(s)
	if err != nil {
		return proto.Authority{}, err
	}
	return proto.Authority{UID: uint32(uid), SelectorKind: kind, SelectorValue: value, BootID: strings.TrimSpace(string(boot)), ServerPID: pid, ServerStart: start}, nil
}

func sameIncarnation(a, b proto.Authority) bool {
	return a.UID == b.UID && a.SelectorKind == b.SelectorKind && a.SelectorValue == b.SelectorValue && a.BootID == b.BootID && a.ServerPID == b.ServerPID && a.ServerStart == b.ServerStart
}

func revalidate(realm string, server config.TmuxServer, want proto.Authority) (sessionDetails, error) {
	kind, value, selectorErr := selectorIdentity(server)
	if selectorErr != nil || !want.Valid() || want.Realm != realm || want.Server != server.Label || want.UID != uint32(os.Getuid()) || want.SelectorKind != kind || want.SelectorValue != value || !validSessionID(want.SessionID) {
		return sessionDetails{}, errStaleTarget
	}
	got, err := incarnation(server)
	if err != nil || !sameIncarnation(got, want) {
		return sessionDetails{}, errStaleTarget
	}
	d, err := details(server, want.SessionID)
	if err != nil || d.ID != want.SessionID || d.Created != want.SessionCreated {
		return sessionDetails{}, errStaleTarget
	}
	return d, nil
}

type sessionDetails struct {
	ID, Name                string
	Width, Height, Attached int
	Activity                int64
	Created                 int64
	Status                  string
	Alternate               bool
	PaneID                  string
	PaneWidth, PaneHeight   int
	HistorySize             int
}

func details(s config.TmuxServer, id string) (sessionDetails, error) {
	if !validSessionID(id) {
		return sessionDetails{}, errStaleTarget
	}
	format := "#{session_id}\t#{session_name}\t#{window_width}\t#{window_height}\t#{session_attached}\t#{session_activity}\t#{session_created}\t#{status}\t#{alternate_on}\t#{pane_id}\t#{pane_width}\t#{pane_height}\t#{history_size}"
	out, err := tmuxOutput(s, "display-message", "-p", "-t", "="+id+":", "-F", format)
	if err != nil {
		return sessionDetails{}, err
	}
	return parseSession(strings.TrimSpace(out))
}

func parseSession(line string) (sessionDetails, error) {
	p := strings.Split(line, "\t")
	if len(p) != 13 || !validSessionID(p[0]) || len(p[1]) > 128 {
		return sessionDetails{}, fmt.Errorf("invalid session row")
	}
	ints := make([]int64, 5)
	for i, v := range p[2:7] {
		n, e := strconv.ParseInt(v, 10, 64)
		if e != nil {
			return sessionDetails{}, e
		}
		ints[i] = n
	}
	if ints[0] < 1 || ints[0] > 1000 || ints[1] < 1 || ints[1] > 1000 || ints[2] < 0 || ints[4] <= 0 {
		return sessionDetails{}, fmt.Errorf("session values out of bounds")
	}
	if !validPaneID(p[9]) {
		return sessionDetails{}, fmt.Errorf("invalid pane id")
	}
	paneWidth, err := strconv.Atoi(p[10])
	if err != nil || paneWidth < 1 || paneWidth > 1000 {
		return sessionDetails{}, fmt.Errorf("invalid pane width")
	}
	paneHeight, err := strconv.Atoi(p[11])
	if err != nil || paneHeight < 1 || paneHeight > 1000 {
		return sessionDetails{}, fmt.Errorf("invalid pane height")
	}
	historySize, err := strconv.Atoi(p[12])
	if err != nil || historySize < 0 {
		return sessionDetails{}, fmt.Errorf("invalid history size")
	}
	return sessionDetails{ID: p[0], Name: p[1], Width: int(ints[0]), Height: int(ints[1]), Attached: int(ints[2]), Activity: ints[3], Created: ints[4], Status: p[7], Alternate: p[8] == "1", PaneID: p[9], PaneWidth: paneWidth, PaneHeight: paneHeight, HistorySize: historySize}, nil
}
