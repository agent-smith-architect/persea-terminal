package broker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
)

type procProbe struct{}

func (procProbe) Witness(ctx context.Context, pid int) (terminal.ProcessWitness, error) {
	if pid <= 0 || ctx == nil {
		return terminal.ProcessWitness{}, terminal.ErrMalformed
	}
	select {
	case <-ctx.Done():
		return terminal.ProcessWitness{}, ctx.Err()
	default:
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return terminal.ProcessWitness{}, err
	}
	i := strings.LastIndexByte(string(raw), ')')
	if i < 0 {
		return terminal.ProcessWitness{}, errors.New("invalid /proc stat")
	}
	fields := strings.Fields(string(raw)[i+1:])
	if len(fields) <= 19 {
		return terminal.ProcessWitness{}, errors.New("short /proc stat")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return terminal.ProcessWitness{}, errors.New("invalid process start time")
	}
	return terminal.ProcessWitness{PID: pid, StartTime: start}, nil
}

// delayedPTY breaks the intentional construction cycle: Epoch owns the
// writer before BIND, while BIND installs the sole PTY and starts its sole
// reader.  Bytes are never spooled outside Epoch.
type delayedPTY struct {
	mu      sync.Mutex
	file    *os.File
	consume func([]byte) error
	fault   func(error)
	done    chan struct{}
	started bool
	closing bool
	writer  *terminal.NonblockingFDWriter
	lease   *recordingReaderLease
}

func newDelayedPTY() *delayedPTY { return &delayedPTY{done: make(chan struct{})} }
func (d *delayedPTY) setConsumer(consume func([]byte) error, fault func(error)) {
	d.mu.Lock()
	d.consume, d.fault = consume, fault
	d.mu.Unlock()
}
func (d *delayedPTY) install(f *os.File) error {
	if f == nil {
		return terminal.ErrMalformed
	}
	d.mu.Lock()
	if d.started || d.consume == nil {
		d.mu.Unlock()
		return terminal.ErrOutOfState
	}
	w, err := terminal.NewNonblockingFDWriter(f)
	if err != nil {
		d.mu.Unlock()
		return err
	}
	d.file, d.writer, d.started = f, w, true
	d.lease.hold()
	d.mu.Unlock()
	go d.drain(f)
	return nil
}
func (d *delayedPTY) drain(f *os.File) {
	defer d.lease.done()
	defer close(d.done)
	buf := make([]byte, 64*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			d.mu.Lock()
			consume := d.consume
			fault := d.fault
			d.mu.Unlock()
			if e := consume(append([]byte(nil), buf[:n]...)); e != nil {
				if fault != nil {
					fault(e)
				}
				return
			}
		}
		if err != nil {
			d.mu.Lock()
			fault := d.fault
			closing := d.closing
			d.mu.Unlock()
			if fault != nil && !closing {
				fault(err)
			}
			return
		}
	}
}
func (d *delayedPTY) WriteContext(ctx context.Context, p []byte) (int, error) {
	d.mu.Lock()
	f := d.file
	w := d.writer
	d.mu.Unlock()
	if f == nil || w == nil {
		return 0, terminal.ErrOutOfState
	}
	return w.WriteContext(ctx, p)
}
func (d *delayedPTY) Close() error { return nil }
func (d *delayedPTY) seal() {
	d.mu.Lock()
	d.closing = true
	w, f := d.writer, d.file
	d.mu.Unlock()
	if w != nil {
		_ = w.Close()
	}
	if f != nil {
		_ = f.Close()
	}
}

type attachmentFrameWriter struct{ wire *lockedWriter }

func (w *attachmentFrameWriter) WriteFrame(ctx context.Context, raw []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	payload, err := attachmentwire.EncodeCoreJSON(raw, attachmentwire.ServerToBrowser)
	if err != nil {
		return err
	}
	return w.wire.frame(proto.FrameAttachment, payload)
}
func (w *attachmentFrameWriter) Close(context.Context) error { return nil }

type attachmentClient struct {
	ids             terminal.AttachmentIDs
	shadowName, tty string
	file            *os.File
	cmd             *exec.Cmd
	reaped          chan struct{}
}

const attachmentShadowPrefix = "persea-attach-"
const attachmentClientPrefix = "client-"

func removeOrphanShadow(server config.TmuxServer, shadow orphanShadow) error {
	guard := "#{&&:#{==:#{session_id}," + shadow.sessionID + "}," +
		"#{&&:#{==:#{session_name}," + shadow.name + "}," +
		"#{&&:#{==:#{session_attached},0}," +
		"#{&&:#{==:#{session_windows},1}," +
		"#{&&:#{==:#{@persea_client_id}," + shadow.clientID + "}," +
		"#{&&:#{==:#{" + attachmentOwnerPIDOption + "}," + strconv.Itoa(shadow.ownerPID) + "}," +
		"#{==:#{" + attachmentOwnerStartOption + "}," + strconv.FormatUint(shadow.ownerStart, 10) + "}}}}}}}"
	success := "kill-session -t " + shellQuote("="+shadow.sessionID)
	if _, guardErr := tmuxOutput(server, "if-shell", "-F", "-t", "="+shadow.sessionID+":", guard, success, "run-shell 'exit 78'"); guardErr != nil {
		present, verifyErr := tmuxSessionPresent(server, shadow.sessionID)
		if verifyErr == nil && !present {
			return nil
		}
		if verifyErr != nil {
			return fmt.Errorf("guarded orphan shadow cleanup: %v; verify target: %w", guardErr, verifyErr)
		}
		return fmt.Errorf("guarded orphan shadow cleanup: %w", guardErr)
	}
	present, err := tmuxSessionPresent(server, shadow.sessionID)
	if err != nil {
		return fmt.Errorf("verify orphan shadow cleanup: %w", err)
	}
	if present {
		return fmt.Errorf("attachment shadow survived guarded cleanup")
	}
	return nil
}

func tmuxSessionPresent(server config.TmuxServer, sessionID string) (bool, error) {
	out, err := tmuxOutput(server, "list-sessions", "-F", "#{session_id}")
	if errors.Is(err, errNoServer) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, id := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if id == sessionID {
			return true, nil
		}
	}
	return false, nil
}

// guardedResizeIssuer relocates only *where* the already-guarded resize command
// is issued. Every check in tmuxPinnedTransaction.resize — the in-transaction
// shell guard, the exact post-command witness, and the attachment PTY Setsize —
// stays exactly where it is and applies unchanged, so the seam cannot weaken the
// guard set by construction rather than by re-implementation.
//
// The unified engine issues the command on its observer's own control
// connection, because only there is the command's completion a sequence point in
// the same stream the pane's output arrives on.
//
// BeginGeometry types its own failures under the RESIZE guarantee: a rejection
// that leaves the attachment's authority intact (no command slot, no journal
// room) is returned as terminal.RefuseResize; a verdict on the attachment (its
// generation is dead, its target is no longer journal-active) is returned
// untyped. The transaction passes both through unchanged, so the issuer is
// the single place that decides, and an untyped error fails closed as fatal.
type guardedResizeIssuer interface {
	BeginGeometry(ctx context.Context) (geometryTicket, error)
}

// geometryTicket brackets one deliberate geometry mutation: publication is held
// from before the command is issued until the committed geometry is durable, and
// released exactly once afterwards — including when the mutation is refused or
// fails, which is the path a stalled pane would otherwise be reachable from.
//
// BeginGeometry is the reservation point: a ticket exists only once every
// capacity the mutation will need — command slots, the publication hold, and
// the journal's logical AND physical budget for the geometry record plus its
// commit — has been taken, so an expected quota rejection happens here, before
// tmux is touched, and Commit cannot fail for want of capacity.
//
// Issue's error carries mutation certainty: an error wrapping
// errGeometryNotIssued certifies that no command reached tmux; any other
// error means the command was submitted or its transport outcome is unknown.
type geometryTicket interface {
	Issue(ctx context.Context, args []string) error
	Commit(ctx context.Context, columns, rows int) error
	Release()
}

// errGeometryNotIssued marks an Issue failure that provably preceded any
// submission to tmux. It is the only Issue error the resize transaction may
// type as a pre-issue refusal.
var errGeometryNotIssued = errors.New("guarded geometry command was not issued")

type tmuxPinnedTransaction struct {
	server    config.TmuxServer
	bootID    string
	authority proto.Authority
	pty       *delayedPTY
	issuer    guardedResizeIssuer
	// Only the unified writer supplies its own authoritative snapshot and tail.
	// Its cut still writes the exact marker, but has no legacy history consumer.
	writerOwnsOutput bool
	lease            *recordingReaderLease
	// resizeEdge is a test seam invoked at each post-issue edge of the resize
	// transaction ("issued", "witnessed", "sized") so a suite can inflict a
	// REAL failure — kill the session, close the attachment PTY, poison the
	// journal — exactly there and prove the outcome is fatal, not operational.
	// Nil in production.
	resizeEdge func(edge string)
	mu         sync.Mutex
	client     *attachmentClient
}

func pinnedTmuxTarget(w terminal.SourceWitness) string {
	return "=" + w.SessionID + ":" + w.WindowID + "." + w.PaneID
}

func (t *tmuxPinnedTransaction) RunPinned(ctx context.Context, req terminal.TransactionRequest) (terminal.TransactionResult, error) {
	switch req.Action {
	case terminal.ActionBind:
		return t.bind(ctx, req)
	case terminal.ActionCut:
		return t.cut(ctx, req)
	case terminal.ActionResize:
		return t.resize(ctx, req)
	case terminal.ActionCleanup:
		return t.cleanup(ctx, req)
	default:
		return terminal.TransactionResult{}, terminal.ErrMalformed
	}
}

func (t *tmuxPinnedTransaction) exactWitness(ctx context.Context, want terminal.SourceWitness) error {
	got, err := buildSourceWitness(ctx, t.server, t.bootID, want.SessionID)
	if err != nil || got != want {
		return terminal.ErrReopenRequired
	}
	inc, err := incarnation(t.server)
	if err != nil || !sameIncarnation(inc, t.authority) {
		return terminal.ErrReopenRequired
	}
	d, err := details(t.server, want.SessionID)
	if err != nil || d.Created != t.authority.SessionCreated {
		return terminal.ErrReopenRequired
	}
	return nil
}

func (t *tmuxPinnedTransaction) bind(ctx context.Context, req terminal.TransactionRequest) (terminal.TransactionResult, error) {
	if err := t.exactWitness(ctx, req.Witness); err != nil {
		return terminal.TransactionResult{}, err
	}
	owner, err := (procProbe{}).Witness(ctx, os.Getpid())
	if err != nil {
		return terminal.TransactionResult{}, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return terminal.TransactionResult{}, err
	}
	name := fmt.Sprintf("%s%x", attachmentShadowPrefix, nonce[:])
	clientID := fmt.Sprintf("%s%x", attachmentClientPrefix, nonce[:])
	lookupShadowID := func() string {
		out, err := tmuxOutput(t.server, "list-sessions", "-f", "#{==:#{session_name},"+name+"}", "-F", "#{session_id}")
		if err != nil {
			return ""
		}
		fields := strings.Fields(out)
		if len(fields) == 1 && validSessionID(fields[0]) {
			return fields[0]
		}
		return ""
	}
	if lookupShadowID() != "" {
		return terminal.TransactionResult{}, errors.New("attachment shadow nonce collision")
	}
	condition := incarnationCondition(t.authority) +
		"; [ " + shellQuote("#{session_id}") + " = " + shellQuote(req.Witness.SessionID) + " ] || exit 1" +
		"; [ " + shellQuote("#{window_id}") + " = " + shellQuote(req.Witness.WindowID) + " ] || exit 1" +
		"; [ " + shellQuote("#{pane_id}") + " = " + shellQuote(req.Witness.PaneID) + " ] || exit 1" +
		"; [ " + shellQuote("#{pane_pid}") + " = " + shellQuote(strconv.Itoa(req.Witness.Pane.PID)) + " ] || exit 1" +
		"; [ " + shellQuote("#{pane_width}") + " = " + shellQuote(strconv.Itoa(req.Witness.Columns)) + " ] || exit 1" +
		"; [ " + shellQuote("#{pane_height}") + " = " + shellQuote(strconv.Itoa(req.Witness.Rows)) + " ] || exit 1" +
		"; stat=$(cat /proc/" + strconv.Itoa(req.Witness.Pane.PID) + "/stat) || exit 1; suffix=${stat##*) }; set -- $suffix; [ \"$#\" -ge 20 ] && [ \"${20}\" = " + shellQuote(strconv.FormatUint(req.Witness.Pane.StartTime, 10)) + " ]"
	success := fmt.Sprintf("new-session -d -P -F '#{session_id}' -x %d -y %d -s %s ; link-window -s %s -t %s:1 ; kill-window -t %s:0 ; move-window -s %s:1 -t %s:0 ; set-option -t %s status off ; set-option -t %s prefix None ; set-option -t %s prefix2 None ; set-option -t %s @persea_client_id %s ; set-option -t %s %s %d ; set-option -t %s %s %d", req.Witness.Columns, req.Witness.Rows, shellQuote(name), req.Witness.WindowID, name, name, name, name, name, name, name, name, shellQuote(clientID), name, attachmentOwnerPIDOption, owner.PID, name, attachmentOwnerStartOption, owner.StartTime)
	argv := tmuxArgv(t.server, "if-shell", "-t", pinnedTmuxTarget(req.Witness), condition, success, "run-shell 'exit 77'")
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var bindStderr strings.Builder
	cmd.Stderr = &bindStderr
	out, bindErr := cmd.Output()
	ids := terminal.AttachmentIDs{ClientID: clientID}
	for _, line := range strings.Fields(string(out)) {
		if validSessionID(line) {
			ids.ShadowSessionID = line
			break
		}
	}
	if !validSessionID(ids.ShadowSessionID) {
		ids.ShadowSessionID = lookupShadowID()
	}
	var client *attachmentClient
	if validSessionID(ids.ShadowSessionID) {
		client = &attachmentClient{ids: ids, shadowName: name, reaped: make(chan struct{})}
		t.mu.Lock()
		t.client = client
		t.mu.Unlock()
	}
	if bindErr != nil {
		return terminal.TransactionResult{Witness: req.Witness, Attachment: ids}, fmt.Errorf("guarded BIND: %w: %s", bindErr, strings.TrimSpace(bindStderr.String()))
	}
	if !validSessionID(ids.ShadowSessionID) {
		return terminal.TransactionResult{}, errors.New("guarded BIND did not publish shadow owner")
	}
	attach := tmuxArgv(t.server, "-f", "/dev/null", "attach-session", "-f", "ignore-size,active-pane", "-t", "="+ids.ShadowSessionID)
	pcmd := exec.CommandContext(context.Background(), attach[0], attach[1:]...)
	pcmd.Env = append(os.Environ(), "TERM=xterm-256color")
	f, startErr := pty.StartWithSize(pcmd, &pty.Winsize{Rows: uint16(req.Witness.Rows), Cols: uint16(req.Witness.Columns)})
	if startErr != nil {
		return terminal.TransactionResult{Witness: req.Witness, Attachment: ids}, startErr
	}
	client.file, client.cmd = f, pcmd
	t.lease.hold()
	go func() { defer t.lease.done(); _ = pcmd.Wait(); close(client.reaped) }()
	if err := t.pty.install(f); err != nil {
		return terminal.TransactionResult{Witness: req.Witness, Attachment: ids}, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := tmuxOutput(t.server, "list-clients", "-F", "#{client_tty}\t#{session_id}")
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			p := strings.Split(line, "\t")
			if len(p) == 2 && p[1] == ids.ShadowSessionID {
				client.tty = p[0]
				break
			}
		}
		if client.tty != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if client.tty == "" {
		return terminal.TransactionResult{Witness: req.Witness, Attachment: ids}, errors.New("attachment client tty discovery failed")
	}
	return terminal.TransactionResult{Witness: req.Witness, Attachment: ids}, nil
}

func (t *tmuxPinnedTransaction) cut(ctx context.Context, req terminal.TransactionRequest) (terminal.TransactionResult, error) {
	if err := t.exactWitness(ctx, req.Witness); err != nil {
		return terminal.TransactionResult{}, err
	}
	t.mu.Lock()
	c := t.client
	t.mu.Unlock()
	if c == nil || c.ids != req.Attachment || c.tty == "" {
		return terminal.TransactionResult{}, terminal.ErrReopenRequired
	}
	marker := append([]byte("\x1b]52;;"), req.Marker...)
	marker = append(marker, '\a')
	buffer := "persea_cut_" + strings.TrimPrefix(c.ids.ClientID, "client-")
	encodedMarker := base64.StdEncoding.EncodeToString(marker)
	writeMarker := "printf %s " + shellQuote(encodedMarker) + " | base64 -d > " + shellQuote(c.tty)
	guard := incarnationCondition(t.authority) +
		"; [ " + shellQuote("#{session_id}") + " = " + shellQuote(req.Witness.SessionID) + " ] || exit 1; [ " + shellQuote("#{window_id}") + " = " + shellQuote(req.Witness.WindowID) + " ] || exit 1; [ " + shellQuote("#{pane_id}") + " = " + shellQuote(req.Witness.PaneID) + " ] || exit 1; [ " + shellQuote("#{pane_pid}") + " = " + shellQuote(strconv.Itoa(req.Witness.Pane.PID)) + " ] || exit 1; [ " + shellQuote("#{pane_width}") + " = " + shellQuote(strconv.Itoa(req.Witness.Columns)) + " ] || exit 1; [ " + shellQuote("#{pane_height}") + " = " + shellQuote(strconv.Itoa(req.Witness.Rows)) + " ] || exit 1; stat=$(cat /proc/" + strconv.Itoa(req.Witness.Pane.PID) + "/stat) || exit 1; suffix=${stat##*) }; set -- $suffix; [ \"${20}\" = " + shellQuote(strconv.FormatUint(req.Witness.Pane.StartTime, 10)) + " ]"
	clientGuard := "[ " + shellQuote("#{session_id}") + " = " + shellQuote(c.ids.ShadowSessionID) + " ] && [ " + shellQuote("#{@persea_client_id}") + " = " + shellQuote(c.ids.ClientID) + " ]"
	if !terminal.ValidHistoryRows(req.HistoryRows) {
		return terminal.TransactionResult{}, terminal.ErrMalformed
	}
	captureRows := req.HistoryRows + 1
	inner := "run-shell " + shellQuote(writeMarker)
	if !t.writerOwnsOutput {
		inner = fmt.Sprintf("capture-pane -e -N -S -%d -E -1 -t %s -b %s ; run-shell %s ; show-buffer -b %s ; delete-buffer -b %s", captureRows, req.Witness.PaneID, buffer, shellQuote(writeMarker), buffer, buffer)
	}
	success := fmt.Sprintf("if-shell -t %s %s %s %s", shellQuote(c.tty), shellQuote(clientGuard), shellQuote(inner), shellQuote("run-shell 'exit 78'"))
	argv := tmuxArgv(t.server, "if-shell", "-t", pinnedTmuxTarget(req.Witness), guard, success, "run-shell 'exit 77'")
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var cutStderr strings.Builder
	cmd.Stderr = &cutStderr
	raw, err := cmd.Output()
	if err != nil {
		_, _ = tmuxOutput(t.server, "delete-buffer", "-b", buffer)
		return terminal.TransactionResult{}, fmt.Errorf("guarded CUT: %w: %s", err, strings.TrimSpace(cutStderr.String()))
	}
	return terminal.TransactionResult{Witness: req.Witness, Attachment: req.Attachment, Capture: terminal.CutCapture{Capture: raw}}, nil
}

// resize is the canonical explicit geometry mutation. Its outcome is typed
// under the RESIZE guarantee of terminal.PinnedTransaction:
//
//   - Before the guarded command is submitted, a malformed request, a refused
//     BeginGeometry (journal budget, command slots, observer unavailable), or
//     an Issue that provably never reached tmux are returned as a typed
//     terminal.RefuseResize: nothing changed, the caller keeps its attachment.
//   - A dead or changed source detected before issue is ErrReopenRequired,
//     untyped: nothing changed either, but nothing later could succeed.
//   - From the moment the command may have reached tmux, every failure — an
//     ambiguous transport outcome, a witness recheck that does not show the
//     requested geometry, an attachment PTY that cannot be resized, a journal
//     commit that fails — is returned untyped. The caller must assume tmux holds
//     the new geometry while the attachment and journal do not, and retire the
//     attachment.
func guardedResizeArgs(authority proto.Authority, witness terminal.SourceWitness, columns, rows int) []string {
	guard := incarnationCondition(authority) +
		"; [ " + shellQuote("#{session_id}") + " = " + shellQuote(witness.SessionID) + " ] || exit 1" +
		"; [ " + shellQuote("#{window_id}") + " = " + shellQuote(witness.WindowID) + " ] || exit 1" +
		"; [ " + shellQuote("#{pane_id}") + " = " + shellQuote(witness.PaneID) + " ] || exit 1" +
		"; [ " + shellQuote("#{pane_pid}") + " = " + shellQuote(strconv.Itoa(witness.Pane.PID)) + " ] || exit 1" +
		"; [ " + shellQuote("#{pane_width}") + " = " + shellQuote(strconv.Itoa(witness.Columns)) + " ] || exit 1" +
		"; [ " + shellQuote("#{pane_height}") + " = " + shellQuote(strconv.Itoa(witness.Rows)) + " ] || exit 1" +
		"; [ " + shellQuote("#{window_panes}") + " = 1 ] || exit 1" +
		"; stat=$(cat /proc/" + strconv.Itoa(witness.Pane.PID) + "/stat) || exit 1; suffix=${stat##*) }; set -- $suffix; [ \"${20}\" = " + shellQuote(strconv.FormatUint(witness.Pane.StartTime, 10)) + " ]"
	success := fmt.Sprintf("resize-window -t %s -x %d -y %d", shellQuote(witness.WindowID), columns, rows)
	return []string{"if-shell", "-t", pinnedTmuxTarget(witness), guard, success, "run-shell 'exit 77'"}
}

func (t *tmuxPinnedTransaction) resize(ctx context.Context, req terminal.TransactionRequest) (terminal.TransactionResult, error) {
	if req.Columns < 1 || req.Columns > 1000 || req.Rows < 1 || req.Rows > 1000 ||
		!validSessionID(req.Attachment.ShadowSessionID) || req.Attachment.ClientID == "" {
		return terminal.TransactionResult{}, terminal.RefuseResize(terminal.ErrMalformed)
	}
	if err := t.exactWitness(ctx, req.Witness); err != nil {
		return terminal.TransactionResult{}, err
	}
	t.mu.Lock()
	client := t.client
	t.mu.Unlock()
	if client == nil || client.ids != req.Attachment || client.file == nil {
		return terminal.TransactionResult{}, terminal.ErrReopenRequired
	}
	edge := t.resizeEdge
	if edge == nil {
		edge = func(string) {}
	}
	args := guardedResizeArgs(t.authority, req.Witness, req.Columns, req.Rows)
	var ticket geometryTicket
	if t.issuer != nil {
		var err error
		ticket, err = t.issuer.BeginGeometry(ctx)
		if err != nil {
			// Before any mutation: every capacity the mutation needs is reserved
			// by BeginGeometry, so an exhausted journal or command budget is
			// rejected here, typed by the issuer, while geometry, attachment and
			// journal are all untouched. An issuer error it did NOT type is a
			// verdict on the attachment (its journal generation is dead, its
			// target is no longer journal-active) and stays untyped: nothing
			// changed, but nothing later could succeed either.
			return terminal.TransactionResult{}, err
		}
		defer ticket.Release()
		if err := ticket.Issue(ctx, args); err != nil {
			if errors.Is(err, errGeometryNotIssued) {
				return terminal.TransactionResult{}, terminal.RefuseResize(fmt.Errorf("guarded RESIZE: %w", err))
			}
			// Submitted, or unknowable: from here on tmux may hold the new
			// geometry, and only a reopen can re-establish what it holds.
			return terminal.TransactionResult{}, fmt.Errorf("guarded RESIZE issued with unknown outcome: %w", err)
		}
	} else {
		argv := tmuxArgv(t.server, args...)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			// The tmux client never started: proven non-submission.
			return terminal.TransactionResult{}, terminal.RefuseResize(fmt.Errorf("guarded RESIZE: %w", err))
		}
		if err := cmd.Wait(); err != nil {
			// The client ran. A non-zero exit is the guard's own rejection OR a
			// failed mutation; the two are not distinguishable here, and an
			// ambiguous outcome is never a refusal.
			return terminal.TransactionResult{}, fmt.Errorf("guarded RESIZE issued with unknown outcome: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
	}
	edge("issued")
	// The witness recheck is the only rejection detector on the issuer path: a
	// guarded if-shell whose guard evaluates false completes normally on a control
	// connection and carries no exit status. It is load-bearing, not a second
	// opinion. It runs after the command was submitted, so a recheck that does
	// not show the requested geometry on the same pane is never typed as a
	// refusal: the guard may have refused (nothing changed) or the pane may have
	// changed underneath (tmux mutated); both end the attachment.
	after, err := buildSourceWitness(ctx, t.server, t.bootID, req.Witness.SessionID)
	if err != nil || after.SessionID != req.Witness.SessionID || after.WindowID != req.Witness.WindowID ||
		after.PaneID != req.Witness.PaneID || after.Columns != req.Columns || after.Rows != req.Rows {
		return terminal.TransactionResult{}, fmt.Errorf("%w: post-issue witness recheck did not confirm the resize", terminal.ErrReopenRequired)
	}
	edge("witnessed")
	if err := pty.Setsize(client.file, &pty.Winsize{Cols: uint16(req.Columns), Rows: uint16(req.Rows)}); err != nil {
		return terminal.TransactionResult{}, fmt.Errorf("resize attachment PTY after tmux mutated: %w", err)
	}
	edge("sized")
	if ticket != nil {
		if err := ticket.Commit(ctx, after.Columns, after.Rows); err != nil {
			return terminal.TransactionResult{}, fmt.Errorf("durable geometry commit after tmux mutated: %w", err)
		}
	}
	return terminal.TransactionResult{Witness: after, Attachment: req.Attachment}, nil
}

func (t *tmuxPinnedTransaction) cleanup(ctx context.Context, req terminal.TransactionRequest) (terminal.TransactionResult, error) {
	server, err := (procProbe{}).Witness(ctx, req.Witness.Server.PID)
	if err != nil || server != req.Witness.Server {
		return terminal.TransactionResult{}, terminal.ErrReopenRequired
	}
	t.mu.Lock()
	c := t.client
	if c == nil || c.ids != req.Attachment {
		t.mu.Unlock()
		return terminal.TransactionResult{}, terminal.ErrReopenRequired
	}
	t.client = nil
	t.mu.Unlock()
	t.pty.seal()
	if c.tty != "" {
		_, _ = tmuxOutput(t.server, "detach-client", "-t", c.tty)
	} else {
		_, _ = tmuxOutput(t.server, "detach-client", "-s", "="+c.ids.ShadowSessionID)
	}
	if c.cmd != nil {
		select {
		case <-c.reaped:
		case <-time.After(2 * time.Second):
			_ = c.cmd.Process.Kill()
			<-c.reaped
		}
	}
	if _, err := tmuxOutput(t.server, "kill-session", "-t", "="+c.ids.ShadowSessionID); err != nil && !strings.Contains(err.Error(), "can't find session") {
		return terminal.TransactionResult{}, err
	}
	_, _ = tmuxOutput(t.server, "delete-buffer", "-b", "persea_cut_"+strings.TrimPrefix(c.ids.ClientID, "client-"))
	if out, _ := tmuxOutput(t.server, "list-sessions", "-F", "#{session_id}"); strings.Contains("\n"+out+"\n", "\n"+c.ids.ShadowSessionID+"\n") {
		return terminal.TransactionResult{}, errors.New("attachment shadow survived cleanup")
	}
	server, err = (procProbe{}).Witness(ctx, req.Witness.Server.PID)
	if err != nil || server != req.Witness.Server {
		return terminal.TransactionResult{}, terminal.ErrReopenRequired
	}
	return terminal.TransactionResult{Witness: req.Witness, Attachment: req.Attachment}, nil
}

func buildSourceWitness(ctx context.Context, server config.TmuxServer, incarnationID, sessionID string) (terminal.SourceWitness, error) {
	socketPath, err := resolveSocketPath(server)
	if err != nil {
		return terminal.SourceWitness{}, err
	}
	info, err := os.Stat(socketPath)
	if err != nil {
		return terminal.SourceWitness{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return terminal.SourceWitness{}, errors.New("socket stat unavailable")
	}
	inc, err := incarnation(server)
	if err != nil {
		return terminal.SourceWitness{}, err
	}
	if incarnationID != "" && incarnationID != inc.BootID {
		return terminal.SourceWitness{}, terminal.ErrReopenRequired
	}
	format := "#{session_id}\t#{window_id}\t#{pane_id}\t#{pane_pid}\t#{pane_width}\t#{pane_height}"
	out, err := tmuxOutput(server, "display-message", "-p", "-t", "="+sessionID+":", "-F", format)
	if err != nil {
		return terminal.SourceWitness{}, err
	}
	p := strings.Split(strings.TrimSpace(out), "\t")
	if len(p) != 6 {
		return terminal.SourceWitness{}, errors.New("invalid source witness row")
	}
	panePID, e1 := strconv.Atoi(p[3])
	cols, e2 := strconv.Atoi(p[4])
	rows, e3 := strconv.Atoi(p[5])
	if e1 != nil || e2 != nil || e3 != nil {
		return terminal.SourceWitness{}, errors.New("invalid source witness numbers")
	}
	probe := procProbe{}
	sw, err := probe.Witness(ctx, inc.ServerPID)
	if err != nil {
		return terminal.SourceWitness{}, err
	}
	pw, err := probe.Witness(ctx, panePID)
	if err != nil {
		return terminal.SourceWitness{}, err
	}
	identity := fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%d\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d", socketPath, st.Dev, st.Ino, sw.PID, sw.StartTime, p[0], p[1], p[2], pw.PID, pw.StartTime, cols, rows)
	sum := sha256.Sum256([]byte(identity))
	return terminal.SourceWitness{Incarnation: base64.RawURLEncoding.EncodeToString(sum[:]), Socket: terminal.SocketIdentity{Path: socketPath, Device: uint64(st.Dev), Inode: st.Ino}, Server: sw, SessionID: p[0], WindowID: p[1], PaneID: p[2], Pane: pw, Columns: cols, Rows: rows}, nil
}

func resolveSocketPath(server config.TmuxServer) (string, error) {
	if server.SocketPath != "" {
		return server.SocketPath, nil
	}
	out, err := tmuxOutput(server, "display-message", "-p", "-F", "#{socket_path}")
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(out)
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return "", errors.New("tmux returned invalid socket path")
	}
	return p, nil
}

func randomEpoch() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	n := binary.BigEndian.Uint64(b[:])
	if n == 0 {
		n = 1
	}
	return n, nil
}
