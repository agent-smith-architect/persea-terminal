package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
	"persea-terminal/internal/unixcred"
)

const HistoryLineLimit = 1000
const HistoryByteLimit = proto.MaxData
const (
	SnapshotDefaultDepth = 1000
	SnapshotLineLimit    = 5000
)
const SnapshotByteLimit = 2 * 1024 * 1024
const InventorySessionLimit = 128
const SetupTimeout = 2 * time.Second
const AttachSetupTimeout = SetupTimeout
const AttachedReadTimeout = 100 * time.Second

type Server struct {
	socketPath string
	config     config.Broker
	images     *imageStager
	panes      *paneRegistry
	birth      sessionBirthEffect
	unified    *UnifiedDevPaneEffects
}

type unifiedAttachmentFrameWriter struct {
	mu         sync.Mutex
	downstream *attachmentFrameWriter
	provider   *UnifiedDevPaneEffects
	session    string
	epoch      *terminal.Epoch
	prepared   bool
	live       bool
	closing    bool
	source     string
	epochID    uint64
	cut        uint64
	backlog    []unifiedjournal.Event
	tail       *unifiedDevSubscriber
	cancel     func()
	// end unblocks the attachment loop after a typed subscriber close has
	// been written, so the broker ends the attachment itself rather than
	// waiting for the peer to hang up. Closing the connection is also what
	// aborts a downstream write parked on a peer that stopped reading, which
	// is why the bounded verdict path (watchVerdict) invokes it too. Nil when
	// no loop owns this writer.
	end func()
	// ended is closed exactly once, by finish, when this writer has triggered
	// the attachment's end — in-band through terminate, or by the bounded
	// verdict path. It is created with the tail at PREPARE; watchVerdict
	// waits on it.
	ended   chan struct{}
	endOnce sync.Once
	lease   *recordingReaderLease
	resize  *unifiedGeometryReadiness
	// handleReady is the narrow handler seam for deterministic lifecycle tests.
	// Production uses epoch.HandleFrame.
	handleReady     func(terminal.Frame) error
	geometryWaiting func()
	geometryReady   func()
}

// A committed journal geometry is not yet proof that this attachment's resize
// cut admits input. Its live tail waits for the exact server-side READY owner;
// the journal and the attachment input reader remain independent.
type unifiedGeometryReadiness struct {
	done    chan struct{}
	settled bool
	err     error
}

func (writer *unifiedAttachmentFrameWriter) beginResize() (*unifiedGeometryReadiness, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.closing {
		return nil, terminal.ErrClosed
	}
	if writer.resize != nil && !writer.resize.settled {
		return nil, terminal.ErrOutOfState
	}
	fence := &unifiedGeometryReadiness{done: make(chan struct{})}
	writer.resize = fence
	return fence, nil
}

func (writer *unifiedAttachmentFrameWriter) settleResizeLocked(fence *unifiedGeometryReadiness, err error) {
	if fence != nil && !fence.settled {
		fence.err, fence.settled = err, true
		close(fence.done)
	}
}

func (writer *unifiedAttachmentFrameWriter) settleResize(fence *unifiedGeometryReadiness, err error) {
	writer.mu.Lock()
	writer.settleResizeLocked(fence, err)
	writer.mu.Unlock()
}

func (writer *unifiedAttachmentFrameWriter) awaitGeometryReadiness(tail *unifiedDevSubscriber) error {
	for {
		writer.mu.Lock()
		fence, closing, waiting := writer.resize, writer.closing, writer.geometryWaiting
		writer.mu.Unlock()
		if closing {
			return terminal.ErrClosed
		}
		if fence == nil {
			return nil
		}
		if waiting != nil {
			waiting()
		}
		select {
		case <-fence.done:
		case <-tail.done:
			return terminal.ErrClosed
		case <-tail.verdictSignal():
			return terminal.ErrClosed
		}
		writer.mu.Lock()
		err, closing, current := fence.err, writer.closing, writer.resize == fence
		writer.mu.Unlock()
		if closing {
			return terminal.ErrClosed
		}
		if err != nil || current {
			return err
		}
	}
}

func (writer *unifiedAttachmentFrameWriter) writeTailEvent(tail *unifiedDevSubscriber, event unifiedjournal.Event) error {
	for {
		if event.Kind == unifiedjournal.RecordGeometry {
			if err := writer.awaitGeometryReadiness(tail); err != nil {
				return err
			}
			writer.mu.Lock()
			ready := writer.geometryReady
			writer.mu.Unlock()
			if ready != nil {
				ready()
			}
		}
		select {
		case <-tail.verdictSignal():
			return terminal.ErrClosed
		default:
		}
		writer.mu.Lock()
		if writer.closing {
			writer.mu.Unlock()
			return terminal.ErrClosed
		}
		// The input reader may have installed another resize after the wait
		// returned, even when it originally saw no fence. Admission and this
		// publication share the lock; retry without holding it if the new
		// owner is not ready yet.
		if event.Kind == unifiedjournal.RecordGeometry && writer.resize != nil {
			if !writer.resize.settled {
				writer.mu.Unlock()
				continue
			}
			if err := writer.resize.err; err != nil {
				writer.mu.Unlock()
				return err
			}
		}
		err := writer.writeEventLocked(event)
		writer.mu.Unlock()
		return err
	}
}

// The journal snapshot and tail are this attachment's only output source.
// Epoch retains all input/control and exact marker/geometry authority.
func (*unifiedAttachmentFrameWriter) OwnsTerminalOutput() {}

// unifiedSubscriberCloseGrace bounds how long a typed subscriber close may
// queue behind this attachment's own downstream write. A verdict is delivered
// in-band when it can be: the error control is the only way the front door
// learns the reconnectable reason, and a peer that is merely slow drains the
// one in-flight frame and receives it. A peer that has not drained within
// the grace is not going to be told anything: the connection is closed from
// outside the writer's lock, which aborts the parked write, ends the
// attachment through the same teardown every verdict takes, and reaches the
// front door as a broker close it already classifies as a transient
// reconnect. Without this bound the verdict — and the attachment's end — sat
// behind the wedged write until the peer chose to read again.
// The front door's own WebSocket write timeout is an order of magnitude
// longer, so this bound is the one that acts.
const unifiedSubscriberCloseGrace = 1 * time.Second

// unifiedAdmissionReplayBytes bounds the replay a unified PREPARE carries.
// PREPARE must reach the browser, be written into its terminal and be
// answered with READY inside the epoch's 5 s cut timer and the page's 5 s
// attempt deadline, and on a slow link every replay byte spends that
// deadline: 16 KiB is about 22 KiB of base64, 0.7 s at 32 KiB/s, which
// leaves the rest for connection setup and the READY round trip. Everything
// past it streams after COMMIT as ordinary backlog, so the cost of admission
// no longer grows with the size of the history.
const unifiedAdmissionReplayBytes = 16 << 10

// unifiedLiveFrameBytes is the most output one LIVE frame carries.
const unifiedLiveFrameBytes = 64 << 10

func (writer *unifiedAttachmentFrameWriter) WriteFrame(ctx context.Context, raw []byte) (resultErr error) {
	frame, err := terminal.DecodeFrame(raw)
	if err != nil {
		return err
	}
	writer.mu.Lock()
	verdictClosed := false
	defer func() {
		writer.mu.Unlock()
		if verdictClosed {
			writer.finish()
		}
	}()
	checkVerdict := func() bool {
		if writer.tail != nil {
			select {
			case <-writer.tail.verdictSignal():
				writer.terminateLocked(writer.tail.closeReason(), writer.tail.closeLimit())
				verdictClosed = true
				return true
			default:
			}
		}
		return false
	}
	defer func() {
		if resultErr != nil && writer.tail != nil {
			writer.releaseSnapshotLocked()
			if writer.cancel != nil {
				writer.cancel()
				writer.cancel = nil
			}
		}
	}()
	switch frame.Type {
	case terminal.FrameEnd:
		if writer.closing {
			return nil
		}
		writer.settleResizeLocked(writer.resize, terminal.ErrClosed)
		return writer.downstream.WriteFrame(ctx, raw)
	case terminal.FramePrepare:
		if writer.prepared {
			// The cut lifecycle is completed server-side. A mid-session CutResize
			// PREPARE must not reach the browser from here: the browser learns the
			// new geometry from the committed event on the ordered tail, which is
			// the only place its position relative to output is guaranteed.
			ready := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameReady, Source: frame.Source, Epoch: frame.Epoch, Cut: frame.Cut}
			var fence *unifiedGeometryReadiness
			if frame.Kind == terminal.CutResize {
				fence = writer.resize
			}
			handleReady := writer.handleReady
			if handleReady == nil {
				handleReady = writer.epoch.HandleFrame
			}
			go func() {
				err := handleReady(ready)
				writer.settleResize(fence, err)
				if err != nil && fence != nil {
					writer.finish()
				}
			}()
			return nil
		}
		events, initial, tail, cancel, err := writer.provider.openSnapshotTailWithLease(writer.session, writer.lease)
		if err != nil {
			return err
		}
		writer.prepared, writer.tail, writer.cancel = true, tail, cancel
		writer.ended = make(chan struct{})
		writer.source, writer.epochID, writer.cut = frame.Source, frame.Epoch, frame.Cut
		// Registration starts the verdict lifetime, including a blocked PREPARE.
		go writer.watchVerdict(tail, writer.ended)
		// Replay starts at the generation's birth geometry, never at the current
		// one: the committed events that follow re-derive the current geometry in
		// the same order the live session produced it.
		frame.Columns, frame.Rows = initial.Columns, initial.Rows
		replay := make([]byte, 0, unifiedAdmissionReplayBytes)
		rest := events
		for index, event := range events {
			if event.Kind != unifiedjournal.RecordOutput {
				rest = events[index:]
				break
			}
			if len(replay)+len(event.Payload) > unifiedAdmissionReplayBytes {
				rest = events[index:]
				break
			}
			replay = append(replay, event.Payload...)
			rest = events[index+1:]
		}
		// The snapshot already owns its event index. Keeping its suffix avoids
		// another index allocation while preserving the copied payload lifetime.
		writer.backlog = rest
		frame.History = []string{}
		frame.Replay = replay
		frame.Truncated = false
		encoded, err := attachmentwire.Encode(frame, attachmentwire.ServerToBrowser)
		if err != nil {
			return err
		}
		return writer.downstream.wire.frame(proto.FrameAttachment, encoded)
	case terminal.FrameLive:
		return nil
	case terminal.FrameCommit:
		if writer.live {
			return nil
		}
		if checkVerdict() {
			return terminal.ErrClosed
		}
		encoded, err := attachmentwire.Encode(frame, attachmentwire.ServerToBrowser)
		if err != nil {
			return err
		}
		if err := writer.downstream.wire.frame(proto.FrameAttachment, encoded); err != nil {
			return err
		}
		writer.live = true
		writer.cut = frame.Cut
		if err := writer.writeBacklogLocked(checkVerdict); err != nil {
			return err
		}
		writer.releaseSnapshotLocked()
		go writer.streamTail()
		return nil
	default:
		return writer.downstream.WriteFrame(ctx, raw)
	}
}

func (writer *unifiedAttachmentFrameWriter) streamTail() {
	defer writer.stopOutput()
	tail := writer.tail
	for {
		// A typed verdict outranks every event still buffered behind it. The
		// journal has moved on without this attachment; whatever the tail
		// still holds is replayed from the snapshot by the re-attach, and
		// writing it first would only queue the verdict behind bytes a peer
		// that has stopped reading will never drain. Checked before every
		// receive so the verdict is seen as soon as the in-flight write
		// returns, however many events the provider buffered before evicting.
		select {
		case <-tail.verdictSignal():
			writer.terminate(tail.closeReason(), tail.closeLimit())
			return
		default:
		}
		event, open := tail.receive()
		if !open {
			break
		}
		err := writer.writeTailEvent(tail, event)
		// Receiving transfers ownership to this writer. Cancellation and
		// eviction cannot refund the event while the actual write is parked.
		bytes := recordingEventBytes(event)
		event = unifiedjournal.Event{}
		tail.lease.releaseEvent(bytes)
		if err != nil {
			select {
			case <-tail.verdictSignal():
				writer.terminate(tail.closeReason(), tail.closeLimit())
			default:
			}
			return
		}
	}
	// The tail is closed. Either this attachment cancelled it (Close or
	// stopOutput: nothing to say, the attachment is already ending) or the
	// provider removed the subscriber with a typed reason. A typed removal
	// is a verdict on THIS attachment: the journal has moved on without it,
	// so it must end now with that reason rather than stay open on a healthy
	// socket that will never carry another byte.
	if reason := tail.closeReason(); reason != "" {
		writer.terminate(reason, tail.closeLimit())
	}
}

// watchVerdict is the bounded close path a typed subscriber verdict takes
// when the in-band one cannot run: it never takes writer.mu and never waits
// on the peer beyond unifiedSubscriberCloseGrace. streamTail is the only
// consumer of the tail and may be parked inside writeEventLocked, holding
// writer.mu, on a downstream write to a peer that has stopped reading; the
// provider's eviction closes the tail but cannot interrupt that write, so
// terminate — the typed control, then end — would wait for the peer. This
// goroutine observes the verdict through the subscriber's separate signal,
// gives the in-band path the grace to finish on a peer that is merely slow,
// and otherwise ends the attachment from outside the lock: end closes the
// connection, which aborts the parked write and returns the attachment loop
// into its ordinary teardown. It stands down when the attachment ends on its
// own terms (the tail's done, closed by Close/stopOutput) or when terminate
// has already ended it (ended).
func (writer *unifiedAttachmentFrameWriter) watchVerdict(tail *unifiedDevSubscriber, ended <-chan struct{}) {
	select {
	case <-tail.verdictSignal():
	case <-tail.done:
		return
	case <-ended:
		return
	}
	grace := time.NewTimer(unifiedSubscriberCloseGrace)
	defer grace.Stop()
	select {
	case <-ended:
		return
	case <-tail.done:
		return
	case <-grace.C:
	}
	brokerLogf("component=broker event=subscriber_close_cut reason=%q limit=%q session=%q epoch=%d grace=%s", string(tail.closeReason()), string(tail.closeLimit()), writer.session, writer.epochID, unifiedSubscriberCloseGrace)
	writer.finish()
}

// finish triggers the attachment's end exactly once, whichever path gets
// there first, and tells the other path so through ended.
func (writer *unifiedAttachmentFrameWriter) finish() {
	writer.endOnce.Do(func() {
		if writer.end != nil {
			writer.end()
		}
		if writer.ended != nil {
			close(writer.ended)
		}
	})
}

// terminate ends the attachment with a typed subscriber close reason: the
// reason is written as the FATAL error control the front door turns into the
// WebSocket close reason, then the attachment loop is unblocked so the
// broker's own teardown runs. The browser classifies every subscriber close
// reason reconnectable and re-attaches on the same session identity, which
// rebuilds it from snapshot+tail. Nothing is written on a writer that is
// already closing: its attachment ended first and owns its own reason. The
// control write here is itself bounded by watchVerdict: a peer that does not
// drain it within the grace is cut from outside this lock.
func (writer *unifiedAttachmentFrameWriter) terminate(reason proto.SubscriberCloseReason, limit recordingTailLimit) {
	writer.mu.Lock()
	writer.terminateLocked(reason, limit)
	writer.mu.Unlock()
	writer.finish()
}

func (writer *unifiedAttachmentFrameWriter) terminateLocked(reason proto.SubscriberCloseReason, limit recordingTailLimit) {
	if writer.closing {
		return
	}
	writer.closing = true
	detail := ""
	if limit != "" {
		detail = fmt.Sprintf(" limit=%q", string(limit))
	}
	brokerLogf("component=broker event=subscriber_closed reason=%q%s session=%q epoch=%d", string(reason), detail, writer.session, writer.epochID)
	_ = writer.downstream.wire.control(proto.Control{Type: "error", Code: string(reason), Msg: "unified subscriber closed"})
}

// writeEventLocked projects one committed journal event onto the browser wire.
// Output becomes LIVE frames; a committed geometry becomes a typed CutResize
// PREPARE carrying only the new dimensions. Both travel the one stream, so the
// browser sees geometry exactly where the journal committed it: after every
// earlier byte and before every later one.
func (writer *unifiedAttachmentFrameWriter) writeEventLocked(event unifiedjournal.Event) error {
	if event.Kind == unifiedjournal.RecordGeometry {
		frame := terminal.Frame{
			Version: terminal.ProtocolVersion, Type: terminal.FramePrepare, Source: writer.source,
			Epoch: writer.epochID, Cut: writer.cut, Kind: terminal.CutResize,
			Columns: event.Geometry.Columns, Rows: event.Geometry.Rows, History: []string{},
		}
		encoded, err := attachmentwire.Encode(frame, attachmentwire.ServerToBrowser)
		if err != nil {
			return err
		}
		return writer.downstream.wire.frame(proto.FrameAttachment, encoded)
	}
	for payload := event.Payload; len(payload) != 0; {
		chunkBytes := min(len(payload), unifiedLiveFrameBytes)
		if err := writer.writeLiveLocked(payload[:chunkBytes]); err != nil {
			return err
		}
		payload = payload[chunkBytes:]
	}
	return nil
}

func (writer *unifiedAttachmentFrameWriter) writeLiveLocked(data []byte) error {
	frame := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameLive, Source: writer.source, Epoch: writer.epochID, Cut: writer.cut, Data: data}
	encoded, err := attachmentwire.Encode(frame, attachmentwire.ServerToBrowser)
	if err != nil {
		return err
	}
	return writer.downstream.wire.frame(proto.FrameAttachment, encoded)
}

// writeBacklogLocked writes the part of the snapshot that PREPARE did not
// carry. It is history rather than live output, so consecutive output events
// share LIVE frames up to unifiedLiveFrameBytes: written one frame per event,
// a history of many small events would cost a frame envelope per event on
// the wire and a terminal write per event in the page, which on a slow link
// multiplies the time to catch up. A geometry event ends the run before it,
// so it keeps its exact position between output bytes. stopped reports a
// typed verdict and is checked before every frame, as before every event.
func (writer *unifiedAttachmentFrameWriter) writeBacklogLocked(stopped func() bool) error {
	var run []byte
	flush := func() error {
		if len(run) == 0 {
			return nil
		}
		if stopped() {
			return terminal.ErrClosed
		}
		err := writer.writeLiveLocked(run)
		run = run[:0]
		return err
	}
	for _, event := range writer.backlog {
		if event.Kind != unifiedjournal.RecordOutput {
			if err := flush(); err != nil {
				return err
			}
			if stopped() {
				return terminal.ErrClosed
			}
			if err := writer.writeEventLocked(event); err != nil {
				return err
			}
			continue
		}
		for payload := event.Payload; len(payload) != 0; {
			if run == nil {
				run = make([]byte, 0, unifiedLiveFrameBytes)
			}
			take := min(unifiedLiveFrameBytes-len(run), len(payload))
			run = append(run, payload[:take]...)
			payload = payload[take:]
			if len(run) == unifiedLiveFrameBytes {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	return flush()
}

func (writer *unifiedAttachmentFrameWriter) Close(context.Context) error {
	writer.mu.Lock()
	writer.closing = true
	writer.settleResizeLocked(writer.resize, terminal.ErrClosed)
	writer.releaseSnapshotLocked()
	cancel := writer.cancel
	writer.cancel = nil
	writer.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if writer.lease != nil {
		writer.lease.releaseSnapshot()
		writer.lease.detach()
	}
	return nil
}

func (writer *unifiedAttachmentFrameWriter) stopOutput() {
	writer.mu.Lock()
	writer.closing = true
	writer.settleResizeLocked(writer.resize, terminal.ErrClosed)
	writer.releaseSnapshotLocked()
	cancel := writer.cancel
	writer.cancel = nil
	writer.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (writer *unifiedAttachmentFrameWriter) releaseSnapshotLocked() {
	writer.backlog = nil
	if writer.tail != nil {
		writer.tail.releaseSnapshot()
	}
}

// PaneObservationEffects accepts observer facts without returning routing or
// lifecycle truth.
type PaneObservationEffects interface {
	AdmitPane(controlmode.PaneWitness) error
	ObservePane(controlmode.Observation) error
}

// SessionBirthEffects surrounds the existing real session factory.
type SessionBirthEffects interface {
	CommitSessionBirth(string) error
	AbortSessionBirth(error) error
}

// PaneEffects supplies the explicit production observer, journal, and birth
// operations used by the broker-private pane coordinators.
type PaneEffects interface {
	RunObserver(context.Context, PaneObservationEffects) error
	WritePane(unifiedjournal.PaneKey, []byte) error
	BeginSessionBirth(string, string) (SessionBirthEffects, error)
}

type sessionBirthEffect interface {
	beginSessionBirth(string, string) (SessionBirthEffects, error)
}

type paneJournalEffect interface {
	WritePane(unifiedjournal.PaneKey, []byte) error
}

type paneCoordinateKey struct {
	server      string
	session     string
	window      string
	pane        string
	incarnation string
}

type paneRouteCoordinateKey struct {
	server            string
	session           string
	controlGeneration uint64
	window            string
	pane              string
}

type sessionCoordinateKey struct {
	server  string
	session string
}

type paneCoordinator struct {
	failure     error
	mu          sync.Mutex
	journal     paneJournalEffect
	attachments map[*terminal.Epoch]struct{}
}

type sessionCoordinator struct {
	witness controlmode.SessionWitness
	router  *controlmode.SessionRouter
}

type paneAdmissionState struct {
	witness           controlmode.PaneWitness
	failedIncarnation string
}

type paneRegistry struct {
	mu                  sync.Mutex
	effects             PaneEffects
	retention           *retentionTrialRuntime
	sessions            map[sessionCoordinateKey]*sessionCoordinator
	panes               map[paneCoordinateKey]*paneCoordinator
	admitted            map[paneRouteCoordinateKey]paneAdmissionState
	rotations           map[paneRouteCoordinateKey]*paneRotationTxn
	rotationGenerations map[unifiedjournal.PaneKey]*paneRotationTxn
	failed              int
	broken              bool
	closing             bool
	closed              bool
	closeDone           chan struct{}
	closeErr            error
}

type paneAttachmentEffects struct {
	registry *paneRegistry
	key      paneCoordinateKey
}

func newPaneRegistry(effects PaneEffects) *paneRegistry {
	registry := &paneRegistry{
		effects: effects, sessions: make(map[sessionCoordinateKey]*sessionCoordinator),
		panes: make(map[paneCoordinateKey]*paneCoordinator), closeDone: make(chan struct{}),
		rotations:           make(map[paneRouteCoordinateKey]*paneRotationTxn),
		rotationGenerations: make(map[unifiedjournal.PaneKey]*paneRotationTxn),
	}
	registry.retention = newRetentionTrialRuntime(effects)
	if registry.retention != nil {
		registry.admitted = make(map[paneRouteCoordinateKey]paneAdmissionState)
		registry.retention.commitFault = registry.commitRetentionFault
	}
	return registry
}

func routeCoordinateJournalKey(key unifiedjournal.PaneKey) paneRouteCoordinateKey {
	return paneRouteCoordinateKey{
		server: key.Server, session: key.Session, controlGeneration: key.ControlGeneration,
		window: key.Window, pane: key.Pane,
	}
}

// markFailed synchronously transfers sticky incarnation ineligibility from
// runtime fault authority into the existing coordinate record. It is called
// only after runtime authority releases runtime.mu and before any result or
// cleanup effect becomes visible.
// commitRetentionFault is the single authoritative fault linearization point.
// The registry lock precedes the runtime lock, matching retained admission and
// observation. Runtime failure and exact-marker/breaker publication therefore
// become visible together, before any result callback or cleanup can run.
func (registry *paneRegistry) commitRetentionFault(key unifiedjournal.PaneKey, origin string, cause error) (bool, *retentionCommand) {
	registry.mu.Lock()
	registry.retention.mu.Lock()
	winner, cleanup := registry.commitRetentionFaultLocked(key, origin, cause)
	registry.retention.mu.Unlock()
	registry.mu.Unlock()
	return winner, cleanup
}

// Both state locks are held in registry-before-runtime order. Supervision
// shares this exact authority after checking its sampled generation identity.
func (registry *paneRegistry) commitRetentionFaultLocked(key unifiedjournal.PaneKey, origin string, cause error) (bool, *retentionCommand) {
	winner, cleanup := registry.retention.commitFaultLocked(key, origin, cause)
	if winner {
		registry.publishRetentionFailureLocked(key)
	}
	return winner, cleanup
}

func (registry *paneRegistry) publishRetentionFailureLocked(key unifiedjournal.PaneKey) {
	if registry.broken {
		return
	}
	// A pre-commit rotation successor has no admitted coordinate by design.
	// Its owning transaction observes the failed runtime generation at Validate
	// and rolls it back; treating that expected provisional absence as a realm
	// authority gap would destroy the healthy predecessor admission.
	if txn := registry.rotationGenerations[key]; txn != nil && !txn.settled {
		return
	}
	// Abort may outlive an in-flight successor write. Its unadmitted generation
	// remains a bounded, successor-local fault authority until the last runtime
	// reference settles; it must never be mistaken for a missing realm route.
	if generation := registry.retention.generations[key]; generation != nil &&
		generation.abandoned && !generation.admitted {
		return
	}
	coordinate := routeCoordinateJournalKey(key)
	state, ok := registry.admitted[coordinate]
	if ok && state.witness.Incarnation == key.Incarnation && state.failedIncarnation == key.Incarnation {
		return
	}
	if ok && state.witness.Incarnation == key.Incarnation && registry.failed < registry.retention.options.maxPanes {
		state.failedIncarnation = key.Incarnation
		registry.admitted[coordinate] = state
		registry.failed++
		return
	}

	// F=P is exhausted, or exact admitted authority is already unavailable.
	// A single bounded breaker replaces all marker authority. Existing runtime
	// generations still drain and retire, but no retained path can reopen.
	registry.broken = true
	registry.failed = 0
	clear(registry.admitted)
	clear(registry.rotations)
	clear(registry.rotationGenerations)
}

func (registry *paneRegistry) failedIncarnationLocked(witness controlmode.PaneWitness) bool {
	state, ok := registry.admitted[routeCoordinateKey(witness)]
	return ok && state.failedIncarnation == witness.Incarnation
}

func (registry *paneRegistry) pendingRetentionFaultLocked(witness controlmode.PaneWitness) <-chan struct{} {
	state, ok := registry.admitted[routeCoordinateKey(witness)]
	if !ok {
		return nil
	}
	return registry.retention.pendingFault(journalKey(state.witness))
}

// admitReplacementLocked atomically rebuilds the session router without the
// failed coordinate, admits a different incarnation, and only then replaces
// the sticky marker. Other admitted coordinates are replayed unchanged.
func (registry *paneRegistry) admitReplacementLocked(session *sessionCoordinator, witness controlmode.PaneWitness) bool {
	coordinate := routeCoordinateKey(witness)
	state, ok := registry.admitted[coordinate]
	if !ok || state.failedIncarnation == "" || state.failedIncarnation == witness.Incarnation {
		return false
	}
	if !session.router.UnifiedEligible(state.witness) {
		return false
	}
	candidate := controlmode.NewSessionRouter(witness.Session)
	for candidateCoordinate, candidateState := range registry.admitted {
		if candidateCoordinate == coordinate || candidateState.witness.Session != witness.Session ||
			!session.router.UnifiedEligible(candidateState.witness) {
			continue
		}
		if err := candidate.Admit(candidateState.witness); err != nil {
			return false
		}
	}
	if err := candidate.Admit(witness); err != nil {
		return false
	}
	session.router = candidate
	registry.admitted[coordinate] = paneAdmissionState{witness: witness}
	registry.failed--
	return true
}

func sessionKey(witness controlmode.SessionWitness) sessionCoordinateKey {
	return sessionCoordinateKey{server: witness.Server, session: witness.Session}
}

func coordinateKey(witness controlmode.PaneWitness) paneCoordinateKey {
	return paneCoordinateKey{
		server: witness.Session.Server, session: witness.Session.Session,
		window: witness.Window, pane: witness.Pane, incarnation: witness.Incarnation,
	}
}

func routeCoordinateKey(witness controlmode.PaneWitness) paneRouteCoordinateKey {
	return paneRouteCoordinateKey{
		server: witness.Session.Server, session: witness.Session.Session,
		controlGeneration: witness.Session.ControlGeneration,
		window:            witness.Window, pane: witness.Pane,
	}
}

func attachmentCoordinateKey(server string, witness terminal.SourceWitness) paneCoordinateKey {
	return paneCoordinateKey{
		server: server, session: witness.SessionID, window: witness.WindowID,
		pane: witness.PaneID, incarnation: witness.Incarnation,
	}
}

func journalKey(witness controlmode.PaneWitness) unifiedjournal.PaneKey {
	return unifiedjournal.PaneKey{
		Server: witness.Session.Server, Session: witness.Session.Session,
		ControlGeneration: witness.Session.ControlGeneration, Window: witness.Window,
		Pane: witness.Pane, Incarnation: witness.Incarnation,
	}
}

func (registry *paneRegistry) coordinatorLocked(key paneCoordinateKey) *paneCoordinator {
	coordinator := registry.panes[key]
	if coordinator == nil {
		journal := paneJournalEffect(registry.effects)
		if registry.retention != nil {
			journal = registry.retention
		}
		coordinator = &paneCoordinator{journal: journal, attachments: make(map[*terminal.Epoch]struct{})}
		registry.panes[key] = coordinator
	}
	return coordinator
}

func (registry *paneRegistry) AdmitPane(witness controlmode.PaneWitness) error {
	if registry.retention == nil {
		registry.mu.Lock()
		defer registry.mu.Unlock()
		key := sessionKey(witness.Session)
		session := registry.sessions[key]
		if session == nil || session.witness != witness.Session {
			session = &sessionCoordinator{witness: witness.Session, router: controlmode.NewSessionRouter(witness.Session)}
			registry.sessions[key] = session
		}
		if err := session.router.Admit(witness); err != nil {
			return err
		}
		registry.coordinatorLocked(coordinateKey(witness))
		return nil
	}

	journal := journalKey(witness)
	for {
		_, created, err := registry.retention.ensureGeneration(journal, false)
		if err != nil {
			return err
		}
		registry.mu.Lock()
		if registry.closing || registry.closed || registry.broken {
			registry.mu.Unlock()
			registry.retention.releaseUnadmitted(journal, created)
			return unifiedjournal.ErrInvalidated
		}
		if pending := registry.pendingRetentionFaultLocked(witness); pending != nil {
			registry.mu.Unlock()
			registry.retention.releaseUnadmitted(journal, created)
			<-pending
			continue
		}
		if registry.failedIncarnationLocked(witness) || registry.retention.generationFailed(journal) {
			registry.mu.Unlock()
			registry.retention.releaseUnadmitted(journal, created)
			return unifiedjournal.ErrInvalidated
		}
		admissionChanged, admitErr := registry.retention.markAdmitted(journal)
		if admitErr != nil {
			registry.mu.Unlock()
			registry.retention.releaseUnadmitted(journal, created)
			return admitErr
		}
		key := sessionKey(witness.Session)
		session := registry.sessions[key]
		if session == nil || session.witness != witness.Session {
			session = &sessionCoordinator{witness: witness.Session, router: controlmode.NewSessionRouter(witness.Session)}
			registry.sessions[key] = session
		}
		if registry.admitReplacementLocked(session, witness) {
			registry.coordinatorLocked(coordinateKey(witness))
			registry.mu.Unlock()
			return nil
		}
		if err := session.router.Admit(witness); err != nil {
			var boundary <-chan error
			var boundaryStartErr error
			if errors.Is(err, controlmode.ErrMissedFirstByte) {
				coordinate := routeCoordinateKey(witness)
				previous, admitted := registry.admitted[coordinate]
				if admitted && previous.failedIncarnation == "" && previous.witness.Incarnation != witness.Incarnation {
					boundary, boundaryStartErr = registry.retention.startBoundary(journalKey(previous.witness), "incarnation_change", true)
					delete(registry.admitted, coordinate)
				}
			}
			registry.retention.restoreUnadmitted(journal, admissionChanged)
			registry.mu.Unlock()
			registry.retention.releaseUnadmitted(journal, created)
			if boundary != nil {
				return errors.Join(err, boundaryStartErr, <-boundary)
			}
			return errors.Join(err, boundaryStartErr)
		}
		registry.admitted[routeCoordinateKey(witness)] = paneAdmissionState{witness: witness}
		registry.coordinatorLocked(coordinateKey(witness))
		registry.mu.Unlock()
		return nil
	}
}

// disconnectAuthorityLocked reports whether witness is both the exact retained
// admission and current router authority. Dead-generation admission history is
// diagnostic only and must never reserve or submit another reconnect boundary.
// Caller holds registry.mu.
func (registry *paneRegistry) disconnectAuthorityLocked(witness controlmode.PaneWitness) bool {
	admission, admitted := registry.admitted[routeCoordinateKey(witness)]
	if !admitted || admission.witness != witness {
		return false
	}
	session := registry.sessions[sessionKey(witness.Session)]
	return session != nil && session.router.UnifiedEligible(witness)
}

// beginOwnerGone reserves the ordered terminal boundary and removes the exact
// registry/router authority. Its caller keeps provider authority frozen until
// this returns, then settles subscribers before finishOwnerGone publishes the
// retiring boundary.
func (registry *paneRegistry) beginOwnerGone(witness controlmode.PaneWitness) (*retentionReservation, error) {
	key := journalKey(witness)
	registry.mu.Lock()
	var cancelled *retentionReservation
	defer func() {
		registry.mu.Unlock()
		// A final reference can synchronously retire a file. Registry authority
		// must never be held across that storage operation.
		registry.retention.cancelReservation(cancelled)
	}()
	if registry.closing || registry.closed || registry.broken {
		return nil, unifiedjournal.ErrInvalidated
	}
	if !registry.disconnectAuthorityLocked(witness) {
		return nil, nil
	}
	reservation, err := registry.retention.reserveAdmitted(key, 0)
	if err != nil {
		return nil, err
	}
	if !registry.disconnectAuthorityLocked(witness) {
		cancelled = reservation
		return nil, nil
	}
	session := registry.sessions[sessionKey(witness.Session)]
	if session == nil {
		cancelled = reservation
		return nil, nil
	}
	session.router.Observe(controlmode.Observation{Kind: controlmode.ObservationOwnerGone, Witness: witness})
	delete(registry.admitted, routeCoordinateKey(witness))
	return reservation, nil
}

func (registry *paneRegistry) finishOwnerGone(witness controlmode.PaneWitness, reservation *retentionReservation) error {
	if reservation == nil {
		return nil
	}
	done, err := registry.retention.submitBoundary(reservation, journalKey(witness), "terminal_gone", true)
	if err != nil {
		return err
	}
	return <-done
}

func (registry *paneRegistry) ObservePane(observation controlmode.Observation) error {
	if registry.retention == nil {
		registry.mu.Lock()
		key := sessionKey(observation.Witness.Session)
		session := registry.sessions[key]
		if session == nil {
			session = &sessionCoordinator{
				witness: observation.Witness.Session,
				router:  controlmode.NewSessionRouter(observation.Witness.Session),
			}
			registry.sessions[key] = session
		}
		result := session.router.Observe(observation)
		if result.Delivery == nil {
			registry.mu.Unlock()
			return nil
		}
		delivery := *result.Delivery
		coordinator := registry.coordinatorLocked(coordinateKey(delivery.Witness))
		registry.mu.Unlock()

		coordinator.mu.Lock()
		defer coordinator.mu.Unlock()
		return coordinator.journal.WritePane(journalKey(delivery.Witness), delivery.Data)
	}

	key := journalKey(observation.Witness)
	bytes := 0
	if observation.Kind == controlmode.ObservationOutput {
		bytes = len(observation.Data)
	}
	ownerBoundary := observation.Kind == controlmode.ObservationDisconnect || observation.Kind == controlmode.ObservationOwnerGone
	if ownerBoundary {
		// This hook is deliberately before the authority transaction. Tests use
		// it to prove that a rotation completed before the transaction is a
		// stale no-op, rather than recreating the predecessor generation.
		registry.retention.callHook("before_disconnect_reserve", key)
	}

	var reservation *retentionReservation
	var err error
	registry.mu.Lock()
	if registry.closing || registry.closed || registry.broken {
		registry.mu.Unlock()
		return unifiedjournal.ErrInvalidated
	}
	if ownerBoundary {
		// Unit reap may retain immutable witness history across rotations. Only
		// the exact currently admitted route owns a Disconnect. A stale or
		// absent historical witness is an idempotent no-op before reserve, so it
		// cannot recreate a retired runtime generation or consume ledger.
		if !registry.disconnectAuthorityLocked(observation.Witness) {
			registry.mu.Unlock()
			return nil
		}
		// Hold registry authority continuously through the bounded retention
		// reservation. reserveAdmitted performs its admission check and reserve
		// under one retention lock, so a retired/absent generation can never be
		// recreated in a check-to-reserve gap.
		reservation, err = registry.retention.reserveAdmitted(key, 0)
		if err != nil {
			registry.mu.Unlock()
			return err
		}
		registry.mu.Unlock()
	} else {
		registry.mu.Unlock()
		reservation, err = registry.retention.reserve(key, bytes)
	}
	if err != nil {
		if observation.Kind == controlmode.ObservationOutput && (errors.Is(err, unifiedjournal.ErrInvalidated) || errors.Is(err, unifiedjournal.ErrSourceQuota)) {
			registry.mu.Lock()
			failed := registry.failedIncarnationLocked(observation.Witness)
			// The sealed predecessor has released its runtime source owner.
			// Unexpected later output cannot acquire unbound payload credit,
			// but must still invalidate the pending rotation's route evidence.
			// Its existing commit predicate then takes the local fatal path.
			if txn := registry.rotations[routeCoordinateKey(observation.Witness)]; txn != nil && txn.oldKey == key {
				registry.retention.mu.Lock()
				sealed := txn.predecessorGeneration != nil && txn.predecessorGeneration.retireRequested
				registry.retention.mu.Unlock()
				if sealed {
					txn.session.router.Observe(controlmode.Observation{Kind: controlmode.ObservationDecoderAmbiguity, Witness: observation.Witness})
					failed = true
				}
			}
			registry.mu.Unlock()
			if failed {
				// The byte is rejected before router/session mutation and before a
				// bounded reservation is acquired, but the required content-free
				// discard observation still travels through the sole dispatcher.
				registry.retention.publishRejectedOutput(bytes)
			}
		}
		return err
	}
	registry.retention.callHook("after_ingress_reserve", key)
	registry.mu.Lock()
	if registry.closing || registry.closed || registry.broken {
		registry.mu.Unlock()
		registry.retention.rejectReservation(reservation, key, 0)
		return unifiedjournal.ErrInvalidated
	}
	if ownerBoundary {
		if !registry.disconnectAuthorityLocked(observation.Witness) {
			// Authority changed after the valid reservation but before routing.
			// The rejected authority decision is final. Releasing its reservation
			// may perform durable retirement, so it follows the registry unlock.
			registry.mu.Unlock()
			registry.retention.cancelReservation(reservation)
			return nil
		}
	}
	if pending := registry.pendingRetentionFaultLocked(observation.Witness); pending != nil {
		registry.mu.Unlock()
		registry.retention.rejectReservation(reservation, key, 0)
		<-pending
		return registry.ObservePane(observation)
	}
	// Lane-visible cutoff recheck: the reservation snapshot may predate the
	// authoritative fault CAS. A post-fault observation must become discard
	// or cancel work before any session/router/admission mutation.
	if registry.failedIncarnationLocked(observation.Witness) || reservation.failed || registry.retention.generationFailed(key) {
		registry.mu.Unlock()
		registry.retention.rejectReservation(reservation, key, bytes)
		return unifiedjournal.ErrInvalidated
	}
	// A failed admitted coordinate remains owned by its exact incarnation
	// until a different incarnation is admitted atomically. Observing a
	// prospective replacement cannot mutate the router or create an
	// eligibility gap before that admission succeeds.
	if admission, admitted := registry.admitted[routeCoordinateKey(observation.Witness)]; admitted && admission.failedIncarnation != "" {
		registry.mu.Unlock()
		registry.retention.rejectReservation(reservation, key, bytes)
		return nil
	}
	if observation.Kind == controlmode.ObservationPause {
		reason := "pause_start"
		if observation.Label == "end" || observation.Label == "continue" {
			reason = "pause_end"
		}
		done, submitErr := registry.retention.submitBoundary(reservation, key, reason, false)
		registry.mu.Unlock()
		if submitErr != nil {
			return submitErr
		}
		return <-done
	}
	sessionID := sessionKey(observation.Witness.Session)
	session := registry.sessions[sessionID]
	if session == nil {
		session = &sessionCoordinator{
			witness: observation.Witness.Session,
			router:  controlmode.NewSessionRouter(observation.Witness.Session),
		}
		registry.sessions[sessionID] = session
	}
	result := session.router.Observe(observation)
	if result.Reason == controlmode.RoutePaneIncarnationChanged {
		coordinate := routeCoordinateKey(observation.Witness)
		previous, admitted := registry.admitted[coordinate]
		if admitted {
			if previous.failedIncarnation != "" {
				registry.mu.Unlock()
				registry.retention.rejectReservation(reservation, key, bytes)
				return nil
			}
			delete(registry.admitted, coordinate)
			done, submitErr := registry.retention.submitBoundary(reservation, journalKey(previous.witness), "incarnation_change", true)
			registry.mu.Unlock()
			if submitErr != nil {
				return submitErr
			}
			boundaryErr := <-done
			if observation.Kind == controlmode.ObservationOutput && len(observation.Data) != 0 {
				return errors.Join(boundaryErr, registry.retention.Discard(key, len(observation.Data), "discarded_after_fault"))
			}
			return boundaryErr
		}
	}
	boundaryReason := ""
	retire := false
	switch observation.Kind {
	case controlmode.ObservationDisconnect:
		boundaryReason = "reconnect"
	case controlmode.ObservationOwnerGone:
		// Only the typed, authoritative owner-gone event may revoke admission
		// and enter durable terminal retirement.
		delete(registry.admitted, routeCoordinateKey(observation.Witness))
		boundaryReason = "terminal_gone"
		retire = true
	case controlmode.ObservationDecoderAmbiguity:
		boundaryReason = "decode_fault"
	case controlmode.ObservationServerRestart, controlmode.ObservationReplacement:
		boundaryReason = "terminal_handoff"
		retire = true
	}
	if boundaryReason != "" {
		done, submitErr := registry.retention.submitBoundary(reservation, key, boundaryReason, retire)
		registry.mu.Unlock()
		if submitErr != nil {
			return submitErr
		}
		return <-done
	}
	if result.Delivery == nil {
		if observation.Kind == controlmode.ObservationOutput && len(observation.Data) != 0 {
			err := registry.retention.submitDiscard(reservation, key, len(observation.Data), "decode_fault")
			registry.mu.Unlock()
			return err
		}
		registry.mu.Unlock()
		registry.retention.cancelReservation(reservation)
		return nil
	}
	delivery := *result.Delivery
	registry.coordinatorLocked(coordinateKey(delivery.Witness))
	err = registry.retention.submitOutput(reservation, journalKey(delivery.Witness), delivery.Data)
	registry.mu.Unlock()
	return err
}

func (registry *paneRegistry) Close() error {
	if registry == nil {
		return nil
	}
	registry.mu.Lock()
	if registry.closed || registry.closing {
		done := registry.closeDone
		registry.mu.Unlock()
		<-done
		return registry.closeErr
	}
	registry.closing = true
	retention := registry.retention
	registry.mu.Unlock()
	var err error
	if retention != nil {
		err = retention.Close()
	}
	registry.mu.Lock()
	registry.closeErr = err
	registry.closed = true
	registry.closing = false
	clear(registry.admitted)
	registry.failed = 0
	registry.broken = false
	close(registry.closeDone)
	registry.mu.Unlock()
	return err
}

func (registry *paneRegistry) retentionBoundary(witness controlmode.PaneWitness, reason string) error {
	if registry.retention == nil {
		return nil
	}
	// Sticky failure authority outlives the ephemeral runtime generation. A
	// content-free boundary for that exact incarnation is therefore already
	// settled even after cleanup has retired the generation; recreating it just
	// to acknowledge the boundary would spend P/Q and violate the bounded-marker
	// contract. Check again after Boundary so cleanup racing the call cannot turn
	// the same idempotent control into a scheduling-dependent refusal.
	registry.mu.Lock()
	failed := registry.failedIncarnationLocked(witness)
	registry.mu.Unlock()
	if failed {
		return nil
	}
	err := registry.retention.Boundary(journalKey(witness), reason)
	if !errors.Is(err, unifiedjournal.ErrInvalidated) {
		return err
	}
	registry.mu.Lock()
	failed = registry.failedIncarnationLocked(witness)
	registry.mu.Unlock()
	if failed {
		return nil
	}
	return err
}

func (registry *paneRegistry) beginSessionBirth(server, name string) (SessionBirthEffects, error) {
	return registry.effects.BeginSessionBirth(server, name)
}

func (registry *paneRegistry) attachmentEffects(server string, witness terminal.SourceWitness) terminal.AttachmentEffects {
	return &paneAttachmentEffects{registry: registry, key: attachmentCoordinateKey(server, witness)}
}

func (effects *paneAttachmentEffects) BindAttachment(epoch *terminal.Epoch) error {
	effects.registry.mu.Lock()
	coordinator := effects.registry.coordinatorLocked(effects.key)
	effects.registry.mu.Unlock()
	coordinator.mu.Lock()
	if coordinator.failure != nil {
		err := coordinator.failure
		coordinator.mu.Unlock()
		return err
	}
	coordinator.attachments[epoch] = struct{}{}
	coordinator.mu.Unlock()
	return nil
}

func (effects *paneAttachmentEffects) ReleaseAttachment(epoch *terminal.Epoch) error {
	effects.registry.mu.Lock()
	coordinator := effects.registry.panes[effects.key]
	effects.registry.mu.Unlock()
	if coordinator == nil {
		return nil
	}
	coordinator.mu.Lock()
	delete(coordinator.attachments, epoch)
	coordinator.mu.Unlock()
	return nil
}

var brokerLogf = log.Printf

func logAttachment(code string, authority *proto.Authority, epoch uint64) {
	realm, server, session := "", "", ""
	if authority != nil {
		realm, server, session = authority.Realm, authority.Server, authority.SessionID
	}
	brokerLogf("component=broker event=attachment_failure code=%q realm=%q server=%q session=%q epoch=%d", code, realm, server, session, epoch)
}

func Run(socketPath string, cfg config.Broker) error {
	return run(socketPath, cfg, nil)
}

// RunWithPaneEffects starts the broker on the explicit pane-effects path.
func RunWithPaneEffects(socketPath string, cfg config.Broker, effects PaneEffects) error {
	return run(socketPath, cfg, effects)
}

func run(socketPath string, cfg config.Broker, effects PaneEffects) error {
	if !cfg.HasFrontUID() && cfg.FrontUID == 0 {
		cfg.FrontUID = uint32(os.Getuid())
		cfg.FrontUIDConfigured = true
	}
	if len(socketPath) > 107 {
		return fmt.Errorf("broker socket path is %d bytes; maximum is 107", len(socketPath))
	}
	parent := filepath.Dir(socketPath)
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("socket directory: %w", err)
	}
	crossUID := cfg.FrontUID != uint32(os.Getuid())
	if err := validateSocketDirectory(info, uint32(os.Getuid()), crossUID); err != nil {
		return err
	}
	if err := cleanupOrphanedShadows(cfg); err != nil {
		return fmt.Errorf("cleanup orphaned attachment shadows: %w", err)
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socketPath)
	socketMode := os.FileMode(0600)
	if crossUID {
		socketMode = 0660
	}
	if err := os.Chmod(socketPath, socketMode); err != nil {
		return err
	}
	return serve(listener, cfg, effects)
}

func validateSocketDirectory(info os.FileInfo, brokerUID uint32, crossUID bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != brokerUID {
		return fmt.Errorf("socket directory must be broker-owned")
	}
	protectedMode := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	expectedMode := os.FileMode(0700)
	if crossUID {
		expectedMode = 0710 | os.ModeSetgid
	}
	if !info.IsDir() || protectedMode != expectedMode {
		return fmt.Errorf("socket directory must have exact protected mode 0700 or 02710")
	}
	return nil
}

func Serve(listener net.Listener, cfg config.Broker) error {
	return serve(listener, cfg, nil)
}

// ServeWithPaneEffects serves connections while the explicit production
// observer drives the broker-private pane coordinators.
func ServeWithPaneEffects(listener net.Listener, cfg config.Broker, effects PaneEffects) error {
	return serve(listener, cfg, effects)
}

func serve(listener net.Listener, cfg config.Broker, effects PaneEffects) error {
	if !cfg.HasFrontUID() && cfg.FrontUID == 0 {
		cfg.FrontUID = uint32(os.Getuid())
		cfg.FrontUIDConfigured = true
	}
	var images *imageStager
	if cfg.ImageStagingDir != "" {
		var err error
		images, err = newImageStager(cfg.ImageStagingDir)
		if err != nil {
			return fmt.Errorf("image staging: %w", err)
		}
	}
	s := &Server{config: cfg, images: images}
	if effects != nil {
		s.panes = newPaneRegistry(effects)
		s.birth = s.panes
		if unified, ok := effects.(*UnifiedDevPaneEffects); ok {
			s.unified = unified
		}
	}
	if effects == nil {
		return s.accept(listener)
	}
	ctx, cancel := context.WithCancel(context.Background())
	observerDone := make(chan error, 1)
	go func() {
		observerErr := effects.RunObserver(ctx, s.panes)
		observerDone <- errors.Join(observerErr, s.panes.Close())
		_ = listener.Close()
	}()
	acceptErr := s.accept(listener)
	select {
	case observerErr := <-observerDone:
		cancel()
		if observerErr != nil {
			return observerErr
		}
		return nil
	default:
		cancel()
		observerErr := <-observerDone
		if observerErr != nil && !errors.Is(observerErr, context.Canceled) {
			return errors.Join(acceptErr, observerErr)
		}
		return acceptErr
	}
}

func (s *Server) accept(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(conn)
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) frame(t proto.FrameType, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return proto.WriteFrame(w.w, t, payload)
}
func (w *lockedWriter) control(c proto.Control) error {
	p, e := proto.MarshalControl(c)
	if e != nil {
		return e
	}
	return w.frame(proto.FrameControl, p)
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	expectedUID := s.config.FrontUID
	if !s.config.HasFrontUID() && s.config.FrontUID == 0 {
		expectedUID = uint32(os.Getuid())
	}
	if err := unixcred.Verify(conn, expectedUID); err != nil {
		return
	}
	if err := conn.SetDeadline(time.Now().Add(SetupTimeout)); err != nil {
		return
	}
	writer := &lockedWriter{w: conn}
	helloSeen := false
	for {
		frame, err := proto.ReadFrame(conn)
		if err != nil {
			if errors.Is(err, proto.ErrInvalidFrame) {
				_ = writer.control(proto.Control{Type: "error", Code: "bad_frame", Msg: err.Error()})
			}
			return
		}
		if frame.Type != proto.FrameControl {
			_ = writer.control(proto.Control{Type: "error", Code: "protocol", Msg: "control message required"})
			return
		}
		ctrl, err := proto.DecodeControl(frame.Payload)
		if err != nil {
			_ = writer.control(proto.Control{Type: "error", Code: "bad_control", Msg: err.Error()})
			return
		}
		if ctrl.Type == proto.ControlImageStage {
			if err := validateImageStageControl(frame.Payload, ctrl); err != nil {
				_ = writer.control(proto.Control{Type: "error", Code: "bad_control", Msg: err.Error()})
				return
			}
		} else {
			ctrl, err = proto.DecodeClientControl(frame.Payload)
			if err != nil {
				_ = writer.control(proto.Control{Type: "error", Code: "bad_control", Msg: err.Error()})
				return
			}
		}
		if ctrl.Type == "hello" {
			if helloSeen {
				_ = writer.control(proto.Control{Type: "error", Code: "protocol", Msg: "duplicate hello"})
				return
			}
			helloSeen = true
			if writer.control(proto.Control{Type: "hello_ok", V: 1}) != nil {
				return
			}
			continue
		}
		if !helloSeen {
			_ = writer.control(proto.Control{Type: "error", Code: "protocol", Msg: "hello required"})
			return
		}
		switch ctrl.Type {
		case proto.ControlImageStage:
			if ctrl.Bytes > proto.MaxImage {
				_ = writer.control(proto.Control{Type: proto.ControlImageRefused, Code: proto.ImageRefusalTooLarge})
				continue
			}
			image, err := proto.ReadFrame(conn)
			if err != nil {
				if errors.Is(err, proto.ErrInvalidFrame) {
					_ = writer.control(proto.Control{Type: "error", Code: "bad_frame", Msg: err.Error()})
				}
				return
			}
			if image.Type != proto.FrameImage || len(image.Payload) != ctrl.Bytes {
				_ = writer.control(proto.Control{Type: "error", Code: "protocol", Msg: "image frame does not match declaration"})
				return
			}
			if s.images == nil {
				_ = writer.control(proto.Control{Type: proto.ControlImageRefused, Code: proto.ImageRefusalImagesDisabled})
				continue
			}
			if writer.control(s.images.stage(ctrl.MediaType, image.Payload)) != nil {
				return
			}
		case "inventory":
			s.inventory(writer)
		case "create":
			s.create(writer, ctrl)
		case "adopt":
			s.adopt(writer, ctrl)
		case "refit":
			s.refit(writer, ctrl)
		case "preview":
			s.preview(writer, ctrl)
		case "history":
			s.history(writer, ctrl)
			return
		case "snapshot":
			s.snapshot(writer, ctrl)
			return
		case "attach":
			s.attach(conn, writer, ctrl)
			return
		case "detach":
			_ = writer.control(proto.Control{Type: "exit", Reason: "detached"})
			return
		}
	}
}

func (s *Server) inventory(writer *lockedWriter) {
	result := make([]proto.ServerInventory, 0, len(s.config.Servers))
	remaining := InventorySessionLimit
	for _, server := range s.config.Servers {
		r := s.inventoryServer(server, remaining)
		remaining -= len(r.Sessions)
		result = append(result, r)
	}
	_ = writer.control(proto.Control{Type: "inventory_ok", Servers: result})
}

func (s *Server) inventoryServer(server config.TmuxServer, limit int) proto.ServerInventory {
	r := proto.ServerInventory{Label: server.Label, Status: "ok", CanCreate: s.config.SessionCreate.AllowsServer(server.Label), CanStageImages: s.images != nil}
	inc, err := incarnation(server)
	if err != nil {
		r.Error = err.Error()
		if errors.Is(err, errNoServer) {
			if server.SocketPath != "" {
				if _, statErr := os.Stat(server.SocketPath); statErr == nil {
					r.Status = "stale_socket"
				} else {
					r.Status = "no_server"
				}
			} else {
				r.Status = "no_server"
			}
		} else {
			r.Status = "error"
		}
		return r
	}
	// Fields 7–9 are the per-session unified eligibility facts.
	// Measured on tmux 3.4: under `list-sessions -F` the window/pane formats
	// expand against each session's active window and pane, which is exactly
	// sufficient for the closed eligibility class (single window, single pane,
	// primary screen).
	format := "#{session_id}\t#{session_name}\t#{window_width}\t#{window_height}\t#{session_attached}\t#{session_activity}\t#{session_created}\t#{alternate_on}\t#{window_panes}\t#{session_windows}\t#{window_activity}"
	out, err := tmuxOutput(server, "list-sessions", "-F", format)
	if err != nil {
		r.Status = "error"
		r.Error = err.Error()
		return r
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\r\n"), "\n") {
		if len(r.Sessions) >= limit {
			r.Error = "inventory session limit reached"
			break
		}
		if line == "" {
			continue
		}
		p := strings.Split(line, "\t")
		if len(p) != 11 {
			r.Status = "error"
			r.Error = "invalid inventory row"
			return r
		}
		d, e := parseSession(strings.Join(p[:7], "\t") + "\toff\t0\t%0\t1\t1\t0")
		if e != nil {
			r.Status = "error"
			r.Error = e.Error()
			return r
		}
		a := inc
		a.Realm = s.config.Realm
		a.Server = server.Label
		a.SessionID = d.ID
		a.SessionCreated = d.Created
		row := proto.Session{Authority: a, Name: d.Name, Width: d.Width, Height: d.Height, Attached: d.Attached, Activity: d.Activity}
		// An absent or malformed optional timestamp is unknown, never "active".
		if activity, err := strconv.ParseInt(p[10], 10, 64); err == nil && activity > 0 {
			row.OutputActivity = activity
		}
		if s.unified != nil {
			row.Unified = s.unified.projectSession(server.Label, d.ID, parseUnifiedSessionFacts(p[7], p[8], p[9]))
		}
		r.Sessions = append(r.Sessions, row)
	}
	if s.unified != nil {
		r.UnifiedDev = s.unified.dashboardLaunch(server.Label, r.Sessions)
	}
	return r
}

// parseUnifiedSessionFacts turns one inventory row's eligibility fields into
// facts for the unified projection. A malformed field marks the facts invalid
// so only that row's affordance fails closed, never the whole inventory.
func parseUnifiedSessionFacts(alternate, panes, windows string) unifiedSessionFacts {
	values := make([]int, 3)
	for i, field := range []string{alternate, panes, windows} {
		value, err := strconv.Atoi(field)
		if err != nil || value < 0 {
			return unifiedSessionFacts{}
		}
		values[i] = value
	}
	if values[0] > 1 || values[1] < 1 || values[2] < 1 {
		return unifiedSessionFacts{}
	}
	return unifiedSessionFacts{Alternate: values[0], Panes: values[1], Windows: values[2], Valid: true}
}

func (s *Server) configured(authority *proto.Authority) (config.TmuxServer, error) {
	if authority == nil || !authority.Valid() || authority.Realm != s.config.Realm || authority.UID != uint32(os.Getuid()) {
		return config.TmuxServer{}, errStaleTarget
	}
	for _, server := range s.config.Servers {
		if server.Label == authority.Server {
			kind, value, err := selectorIdentity(server)
			if err != nil || authority.SelectorKind != kind || authority.SelectorValue != value {
				return config.TmuxServer{}, errStaleTarget
			}
			return server, nil
		}
	}
	return config.TmuxServer{}, errStaleTarget
}

func (s *Server) history(writer *lockedWriter, ctrl proto.Control) {
	server, err := s.configured(ctrl.Authority)
	if err != nil {
		stale(writer)
		return
	}
	d, err := revalidate(s.config.Realm, server, *ctrl.Authority)
	if err != nil {
		stale(writer)
		return
	}
	s.historyValidated(writer, server, d, *ctrl.Authority)
}

func (s *Server) historyValidated(writer *lockedWriter, server config.TmuxServer, d sessionDetails, authority proto.Authority) {
	capture, err := guardedSnapshot(server, authority, d.PaneID, HistoryLineLimit)
	if err != nil {
		if errors.Is(err, errStaleTarget) {
			stale(writer)
			return
		}
		_ = writer.control(proto.Control{Type: "error", Code: "history_failed", Msg: err.Error()})
		return
	}
	text, truncated := boundHistory([]byte(capture.Output), HistoryLineLimit, HistoryByteLimit)
	truncated = truncated || capture.Truncated
	if writer.control(proto.Control{Type: "history_ok", Truncated: truncated, Alternate: capture.Alternate}) != nil {
		return
	}
	_ = writer.frame(proto.FrameData, text)
}

func effectiveSnapshotDepth(depth int) int {
	if depth <= 0 {
		return SnapshotDefaultDepth
	}
	if depth > SnapshotLineLimit {
		return SnapshotLineLimit
	}
	return depth
}

func (s *Server) snapshot(writer *lockedWriter, ctrl proto.Control) {
	server, err := s.configured(ctrl.Authority)
	if err != nil {
		stale(writer)
		return
	}
	d, err := revalidate(s.config.Realm, server, *ctrl.Authority)
	if err != nil {
		stale(writer)
		return
	}
	depth := effectiveSnapshotDepth(ctrl.Depth)
	capture, err := guardedSnapshot(server, *ctrl.Authority, d.PaneID, depth)
	if err != nil {
		if errors.Is(err, errStaleTarget) {
			stale(writer)
			return
		}
		_ = writer.control(proto.Control{Type: "error", Code: "snapshot_failed", Msg: err.Error()})
		return
	}
	data, bounded := boundHistory([]byte(capture.Output), depth+capture.Height, SnapshotByteLimit)
	historyRows := returnedHistoryRows(data, capture.Height, depth)
	truncated := bounded || capture.Truncated || capture.HistorySize > depth
	meta := proto.Control{Type: "snapshot_ok", Truncated: truncated, Alternate: capture.Alternate, Width: capture.Width, Height: capture.Height, Pane: capture.PaneID, FrozenAt: time.Now().UnixMilli(), Depth: depth, HistoryRows: &historyRows}
	if writer.control(meta) != nil || writeDataChunks(writer, data) != nil {
		return
	}
	_ = writer.control(proto.Control{Type: "snapshot_end"})
}

func returnedHistoryRows(data []byte, height, depth int) int {
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

func writeDataChunks(writer *lockedWriter, data []byte) error {
	for len(data) > 0 {
		n := len(data)
		if n > proto.MaxData {
			n = proto.MaxData
			if i := bytes.LastIndexByte(data[:n], '\n'); i >= 0 {
				n = i + 1
			}
		}
		if err := writer.frame(proto.FrameData, data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func boundHistory(in []byte, lineCap, byteCap int) ([]byte, bool) {
	truncated := false
	lines := bytes.Split(in, []byte("\n"))
	if len(lines) > lineCap+1 {
		lines = lines[len(lines)-(lineCap+1):]
		in = bytes.Join(lines, []byte("\n"))
		truncated = true
	}
	if len(in) > byteCap {
		in = in[len(in)-byteCap:]
		if i := bytes.IndexByte(in, '\n'); i >= 0 {
			in = in[i+1:]
		}
		truncated = true
	}
	return in, truncated
}

func stale(w *lockedWriter) {
	_ = w.control(proto.Control{Type: "error", Code: "stale_target", Msg: "snapshot target is stale"})
}

func refitRefusalCode(err error) string {
	switch {
	case errors.Is(err, ErrUnifiedRefitMalformed):
		return "bad_refit"
	case errors.Is(err, ErrUnifiedRefitStale):
		return "stale_target"
	case errors.Is(err, ErrUnifiedRotateInProgress):
		return "refit_in_progress"
	case errors.Is(err, ErrUnifiedRotateSlotsExhausted):
		return "slots_exhausted"
	case errors.Is(err, ErrUnifiedRefitFatal):
		return "refit_faulted"
	default:
		return "refit_failed"
	}
}

func logRefitFailure(authority *proto.Authority, stage proto.RefitFailureStage, class proto.RefitFailureClass) {
	realm, server, session := "", "", ""
	if authority != nil {
		realm, server, session = authority.Realm, authority.Server, authority.SessionID
	}
	brokerLogf("component=broker event=refit_failure code=%q stage=%q class=%q realm=%q server=%q session=%q", "refit_faulted", stage, class, realm, server, session)
}

func logRefitFailureMetadataViolation(authority *proto.Authority) {
	realm, server, session := "", "", ""
	if authority != nil {
		realm, server, session = authority.Realm, authority.Server, authority.SessionID
	}
	brokerLogf("component=broker event=refit_failure_metadata_invalid realm=%q server=%q session=%q", realm, server, session)
}

// refit revalidates the exact broker authority immediately before handing the
// operation to the observer-unit owner. Width never travels on the attachment
// protocol and no session name is accepted as authority.
func (s *Server) refit(writer *lockedWriter, ctrl proto.Control) {
	refuse := func(code string, failures ...error) {
		response := proto.Control{Type: "refit_refused", Code: code, ID: ctrl.ID}
		if code == "refit_faulted" {
			if len(failures) == 1 {
				if stage, class, ok := refitFailureMetadata(failures[0]); ok {
					response.RefitStage, response.RefitClass = stage, class
					logRefitFailure(ctrl.Authority, stage, class)
					_ = writer.control(response)
					return
				}
			}
			// A post-PONR fatal without one closed pair is an internal protocol
			// violation. Never emit an untyped refit_faulted response: downgrade
			// only the wire code to the generic fail-closed refusal and log no
			// arbitrary error text.
			response.Code = "refit_failed"
			logRefitFailureMetadataViolation(ctrl.Authority)
		}
		_ = writer.control(response)
	}
	// Rows are optional (0 keeps the predecessor's); when given they obey the
	// same closed row policy as the live rows-only Fit.
	if s.unified == nil || ctrl.Authority == nil || !validRefitGeometry(ctrl.Cols, ctrl.Rows) || !validRefitOperation(ctrl.ID) {
		refuse("bad_refit")
		return
	}
	server, err := s.configured(ctrl.Authority)
	if err != nil || !s.unified.forServer(server.Label) {
		refuse("not_permitted")
		return
	}
	if result, found := s.unified.settledRefitOperationGeometry(*ctrl.Authority, ctrl.Cols, ctrl.Rows, ctrl.ID); found {
		if result.err != nil {
			refuse(refitRefusalCode(result.err), result.err)
			return
		}
		_ = writer.control(proto.Control{Type: "refit_ok", ID: ctrl.ID, Cols: ctrl.Cols, Rows: result.rows, SessionID: ctrl.Authority.SessionID, Successor: result.successorSource})
		return
	}
	details, err := revalidate(s.config.Realm, server, *ctrl.Authority)
	if err != nil || details.ID != ctrl.Authority.SessionID || !s.unified.allows(server.Label, details.ID, details.Name) {
		refuse("stale_target")
		return
	}
	source, err := buildSourceWitness(context.Background(), server, ctrl.Authority.BootID, details.ID)
	if err != nil || source.PaneID != details.PaneID || source.Server.PID != ctrl.Authority.ServerPID || source.Server.StartTime != ctrl.Authority.ServerStart {
		refuse("stale_target")
		return
	}
	successor, rows, err := s.unified.refitSessionResultGeometry(context.Background(), *ctrl.Authority, source, ctrl.Cols, ctrl.Rows, ctrl.ID)
	if err != nil {
		refuse(refitRefusalCode(err), err)
		return
	}
	_ = writer.control(proto.Control{Type: "refit_ok", ID: ctrl.ID, Cols: ctrl.Cols, Rows: rows, SessionID: details.ID, Successor: successor})
}

func (s *Server) attach(conn net.Conn, writer *lockedWriter, ctrl proto.Control) {
	if ctrl.Engine != "unified-dev" {
		_ = writer.control(proto.Control{Type: "error", Code: "protocol"})
		return
	}
	if ctrl.Mode != "observe" && ctrl.Mode != "control" {
		logAttachment("bad_mode", ctrl.Authority, 0)
		_ = writer.control(proto.Control{Type: "error", Code: "bad_mode", Msg: "invalid attach mode"})
		return
	}
	server, err := s.configured(ctrl.Authority)
	if err != nil {
		logAttachment("stale_target", ctrl.Authority, 0)
		stale(writer)
		return
	}
	d, err := revalidate(s.config.Realm, server, *ctrl.Authority)
	if err != nil {
		logAttachment("stale_target", ctrl.Authority, 0)
		stale(writer)
		return
	}
	if ctrl.Engine == "unified-dev" {
		if s.unified == nil || !s.unified.allows(server.Label, d.ID, d.Name) {
			_ = writer.control(proto.Control{Type: "error", Code: "unified_unavailable", Msg: "unified terminal is unavailable for this target"})
			return
		}
	}
	s.attachValidated(conn, writer, server, d, ctrl)
}

func (s *Server) attachValidated(conn net.Conn, writer *lockedWriter, server config.TmuxServer, d sessionDetails, ctrl proto.Control) {
	if ctrl.Engine != "unified-dev" {
		_ = writer.control(proto.Control{Type: "error", Code: "protocol"})
		return
	}
	if s.unified == nil {
		_ = writer.control(proto.Control{Type: "error", Code: "unified_unavailable"})
		return
	}
	if ctrl.HistoryLimit == nil || !terminal.ValidHistoryRows(*ctrl.HistoryLimit) {
		_ = writer.control(proto.Control{Type: "error", Code: "bad_history", Msg: "invalid history limit"})
		return
	}
	var readerLease *recordingReaderLease
	epochOwnsReader := false
	if ctrl.Engine == "unified-dev" {
		var err error
		readerLease, err = s.unified.readers.acquireAttachment()
		if err != nil {
			_ = writer.control(proto.Control{Type: "error", Code: "unified_unavailable", Msg: err.Error()})
			return
		}
		defer func() {
			if !epochOwnsReader {
				readerLease.releaseSnapshot()
			}
			readerLease.detach()
			readerLease.done()
		}()
	}
	witness, err := buildSourceWitness(context.Background(), server, ctrl.Authority.BootID, d.ID)
	if err != nil || witness.PaneID != d.PaneID || witness.Server.PID != ctrl.Authority.ServerPID || witness.Server.StartTime != ctrl.Authority.ServerStart {
		logAttachment("stale_target", ctrl.Authority, 0)
		stale(writer)
		return
	}
	delayed := newDelayedPTY()
	delayed.lease = readerLease
	txn := &tmuxPinnedTransaction{server: server, bootID: ctrl.Authority.BootID, authority: *ctrl.Authority, pty: delayed, lease: readerLease, writerOwnsOutput: readerLease != nil}
	if ctrl.Engine == "unified-dev" && s.unified != nil && s.panes != nil {
		// The unified target issues its guarded resize on the observer's own
		// control connection. Nothing about the guard set moves with it.
		txn.issuer = &unifiedGeometryIssuer{provider: s.unified, registry: s.panes, session: d.ID}
		if hook := s.unified.resizeEdge; hook != nil {
			txn.resizeEdge = func(edge string) { hook(d.ID, edge) }
		}
	}
	source, err := terminal.NewPinnedSource(witness, txn, procProbe{})
	if err != nil {
		logAttachment("attach_failed", ctrl.Authority, 0)
		_ = writer.control(proto.Control{Type: "error", Code: "attach_failed"})
		return
	}
	epochID, err := randomEpoch()
	if err != nil {
		logAttachment("attach_failed", ctrl.Authority, 0)
		_ = writer.control(proto.Control{Type: "error", Code: "attach_failed"})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var epoch *terminal.Epoch
	frameWriter := terminal.FrameWriter(&attachmentFrameWriter{wire: writer})
	var unifiedWriter *unifiedAttachmentFrameWriter
	if ctrl.Engine == "unified-dev" {
		unifiedWriter = &unifiedAttachmentFrameWriter{
			downstream: &attachmentFrameWriter{wire: writer}, provider: s.unified, session: d.ID, lease: readerLease,
			// A typed subscriber close ends the attachment from the tail
			// goroutine: the loop below is parked in ReadFrame, and closing the
			// connection is what returns it into the same teardown every
			// in-loop verdict takes (handleConn's own Close is then a no-op).
			end: func() { _ = conn.Close() },
		}
		frameWriter = unifiedWriter
	}
	var attachmentEffects terminal.AttachmentEffects
	if s.panes != nil {
		attachmentEffects = s.panes.attachmentEffects(server.Label, witness)
	}
	if readerLease != nil {
		attachmentEffects = &recordingAttachmentEffects{delegate: attachmentEffects, lease: readerLease}
	}
	if attachmentEffects == nil {
		epoch, err = terminal.NewEpoch(ctx, source, epochID, frameWriter, delayed, terminal.Config{
			QuietInterval: 500 * time.Millisecond, MaximumInterval: 2 * time.Second,
			CutTimeout: 5 * time.Second, Clock: terminal.RealClock{}, HistoryRows: *ctrl.HistoryLimit, HistoryRowsSet: true,
		})
	} else {
		epoch, err = terminal.NewEpochWithAttachmentEffects(ctx, source, epochID, frameWriter, delayed, terminal.Config{
			QuietInterval: 500 * time.Millisecond, MaximumInterval: 2 * time.Second,
			CutTimeout: 5 * time.Second, Clock: terminal.RealClock{}, HistoryRows: *ctrl.HistoryLimit, HistoryRowsSet: true,
		}, attachmentEffects)
	}
	if err != nil {
		logAttachment("attach_failed", ctrl.Authority, epochID)
		_ = writer.control(proto.Control{Type: "error", Code: "attach_failed"})
		return
	}
	if unifiedWriter != nil {
		unifiedWriter.epoch = epoch
		epochOwnsReader = true
	}
	delayed.setConsumer(epoch.PTYBytes, func(e error) { _ = epoch.Fault(e) })
	defer func() {
		if unifiedWriter != nil {
			unifiedWriter.stopOutput()
		}
		finalizeCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := epoch.Finalize(finalizeCtx); err != nil {
			logAttachment("finalize_failed", ctrl.Authority, epochID)
		}
		cancel()
	}()
	if writer.control(proto.Control{Type: "attach_ok", Cols: witness.Columns, Rows: witness.Rows, InputMax: terminal.MaxInputBytes}) != nil {
		return
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return
	}
	go func() {
		if err := epoch.Start(ctx, terminal.CutInitial); err != nil {
			logAttachment("attach_failed", ctrl.Authority, epochID)
			_ = writer.control(proto.Control{Type: "error", Code: "attach_failed"})
			_ = epoch.Fault(err)
			return
		}
	}()
	var frame proto.Frame
	var typed terminal.Frame
	var ingressBytes int64
	releaseIngress := func() {
		frame, typed = proto.Frame{}, terminal.Frame{}
		readerLease.releaseTransport(ingressBytes)
		ingressBytes = 0
	}
	defer releaseIngress()
	var admitFrame func(proto.FrameType, uint32) error
	if readerLease != nil {
		admitFrame = func(_ proto.FrameType, bytes uint32) error {
			// Raw frame, decoder growth overlap, the bounded field index's raw
			// values, decoded strings/base64 and malformed-error formatting.
			cost := 8*unifiedjournal.AllocationCharge(int64(bytes)) + 64<<10
			if !readerLease.reserveTransport(cost) {
				return errRecordingReaders
			}
			ingressBytes = cost
			return nil
		}
	}
	for {
		releaseIngress()
		frame, err = proto.ReadFrameWithAdmission(conn, admitFrame)
		if err != nil {
			return
		}
		if frame.Type == proto.FrameControl {
			m, e := proto.DecodeClientControl(frame.Payload)
			if e == nil && m.Type == "detach" {
				_ = writer.control(proto.Control{Type: "exit", Reason: "detached"})
				return
			}
			if e == nil && m.Type == "ping" {
				if writer.control(proto.Control{Type: "pong"}) != nil {
					return
				}
				continue
			}
			_ = writer.control(proto.Control{Type: "error", Code: "protocol", Msg: "attachment frame required"})
			logAttachment("protocol", ctrl.Authority, epochID)
			return
		}
		if frame.Type != proto.FrameAttachment {
			logAttachment("protocol", ctrl.Authority, epochID)
			_ = writer.control(proto.Control{Type: "error", Code: "protocol", Msg: "attachment frame required"})
			return
		}
		typed, err = attachmentwire.Decode(frame.Payload, attachmentwire.BrowserToServer)
		if err != nil {
			logAttachment("bad_attachment", ctrl.Authority, epochID)
			_ = writer.control(proto.Control{Type: "error", Code: "bad_attachment"})
			return
		}
		if ctrl.Mode == "observe" && ((typed.Type == terminal.FrameModeRequest && typed.Mode == terminal.ModeControl) || typed.Type == terminal.FrameInput || typed.Type == terminal.FrameResize) {
			_ = writer.control(proto.Control{Type: "error", Code: "observe_mode", Msg: "control authority denied"})
			continue
		}
		if typed.Type == terminal.FrameResize {
			if ctrl.Engine == "unified-dev" {
				// The one deliberate post-birth geometry change this product allows:
				// an explicit vertical Fit, for the configured unified-dev target
				// only, from its current Control attachment. Rows come from the
				// browser; columns are derived from the exact pane witness and the
				// browser's value is only ever required to equal it, which is what
				// keeps the browser non-authoritative. Out-of-policy requests are
				// rejected, never clamped or rewritten.
				if unifiedWriter == nil || !terminal.ValidVerticalFit(typed.Columns, typed.Rows) {
					_ = writer.control(proto.Control{Type: "error", Code: "resize_rejected"})
					continue
				}
				// The exact pane witness is re-acquired for every request. Its
				// failure modes are verdicts on the attachment, never on the
				// request: a server that cannot be witnessed, a session or pane
				// that is no longer the one this attachment bound, a pane process
				// that was replaced, or a pane whose columns moved underneath us
				// (only rows ever change by this product's hand) all mean the
				// next cut's own exact-witness check would fail too. That is
				// fatal stale_target: the attachment closes and a reopen re-mints
				// against whatever tmux now holds. Reporting it as a refused Fit
				// would leave a dead attachment open behind a passing notice.
				current, witnessErr := buildSourceWitness(ctx, server, ctrl.Authority.BootID, d.ID)
				if witnessErr != nil || current.PaneID != witness.PaneID || current.SessionID != witness.SessionID ||
					current.Pane != witness.Pane || current.Columns != witness.Columns {
					logAttachment("stale_target", ctrl.Authority, epochID)
					brokerLogf("component=broker event=resize_stale_target realm=%q server=%q session=%q epoch=%d request=%dx%d witness_err=%v", ctrl.Authority.Realm, ctrl.Authority.Server, ctrl.Authority.SessionID, epochID, typed.Columns, typed.Rows, witnessErr)
					_ = writer.control(proto.Control{Type: "error", Code: "stale_target", Msg: "resize target is stale"})
					return
				}
				// Request policy: the browser's columns must equal the exact
				// pane's. A browser that disagrees with an unchanged pane is
				// asking for a geometry this product does not grant; that is one
				// request's outcome, and the attachment carries on.
				if current.Columns != typed.Columns {
					_ = writer.control(proto.Control{Type: "error", Code: "resize_rejected"})
					continue
				}
				if current.Rows == typed.Rows {
					continue
				}
			}
			var resizeFence *unifiedGeometryReadiness
			if unifiedWriter != nil {
				var fenceErr error
				resizeFence, fenceErr = unifiedWriter.beginResize()
				if fenceErr != nil {
					_ = writer.control(proto.Control{Type: "error", Code: "resize_failed"})
					continue
				}
			}
			resizeCtx, stop := context.WithTimeout(ctx, 30*time.Second)
			err := epoch.Resize(resizeCtx, typed)
			stop()
			if err != nil {
				if unifiedWriter != nil {
					// A live epoch proves a pre-issue refusal: no geometry was
					// published for this request. A fault must never release one.
					unifiedWriter.settleResize(resizeFence, epoch.Err())
				}
				if dead := epoch.Err(); dead != nil {
					// The epoch is gone. Either it was already gone and this
					// request is how we learned, or this request's transaction
					// failed AFTER its geometry command reached tmux — a witness
					// recheck, attachment PTY resize, or durable commit that did
					// not follow through — and the epoch faulted itself rather
					// than stay live under a geometry it can no longer vouch for.
					// Both end the attachment with the attachment's own verdict,
					// never with a code that names one request's outcome; the
					// reopen re-mints against whatever tmux actually holds.
					logAttachment("attachment_failed", ctrl.Authority, epochID)
					brokerLogf("component=broker event=resize_on_terminal_epoch realm=%q server=%q session=%q epoch=%d request=%dx%d err=%q", ctrl.Authority.Realm, ctrl.Authority.Server, ctrl.Authority.SessionID, epochID, typed.Columns, typed.Rows, dead.Error())
					_ = writer.control(proto.Control{Type: "error", Code: "attachment_failed"})
					return
				}
				// One request was refused before its geometry command was
				// issued — the source proved that with a typed refusal, and the
				// epoch stayed live on nothing less. tmux, the attachment and
				// the journal are exactly as they were: the outcome is reported
				// in-band and the operator may try again.
				brokerLogf("component=broker event=resize_failed realm=%q server=%q session=%q epoch=%d request=%dx%d err=%q", ctrl.Authority.Realm, ctrl.Authority.Server, ctrl.Authority.SessionID, epochID, typed.Columns, typed.Rows, err.Error())
				_ = writer.control(proto.Control{Type: "error", Code: "resize_failed"})
				continue
			}
			continue
		}
		if typed.Type == terminal.FrameHistory {
			err := epoch.RequestHistory(typed)
			if errors.Is(err, terminal.ErrStale) || errors.Is(err, terminal.ErrOutOfState) {
				continue
			}
			if err != nil {
				logAttachment("history_failed", ctrl.Authority, epochID)
				_ = writer.control(proto.Control{Type: "error", Code: "history_failed"})
				return
			}
			continue
		}
		if err := epoch.HandleFrame(typed); err != nil {
			// ErrObserveOnly means "not right now", not "this attachment is
			// broken": the frame was refused because the protocol is mid-cut or
			// observe-only, and the epoch is not faulted by it. The observe-mode
			// branch above already treats exactly that condition as a typed
			// refusal, so treating it as fatal here was an inconsistency — and a
			// costly one, because a keystroke landing inside the millisecond
			// CutResize window took the whole attachment down with it. The frame
			// is refused and dropped; it is never queued, replayed, or resent.
			if errors.Is(err, terminal.ErrObserveOnly) {
				_ = writer.control(proto.Control{Type: "error", Code: "input_refused"})
				continue
			}
			logAttachment("attachment_failed", ctrl.Authority, epochID)
			_ = writer.control(proto.Control{Type: "error", Code: "attachment_failed"})
			return
		}
	}
}

func attachHandshakeFailureCode(handshakeErr, revalidateErr error) string {
	if handshakeErr == nil {
		return ""
	}
	if revalidateErr != nil {
		return "stale_target"
	}
	return "attach_failed"
}

func readAttachHandshake(ptmx *os.File, staleSentinel string) ([]byte, bool, error) {
	if err := ptmx.SetReadDeadline(time.Now().Add(AttachSetupTimeout)); err != nil {
		return nil, false, err
	}
	defer ptmx.SetReadDeadline(time.Time{})
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 256)
	for len(buf) <= proto.MaxControl {
		n, err := ptmx.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if i := bytes.Index(buf, []byte(staleSentinel)); i >= 0 {
				end := i + len(staleSentinel)
				if end < len(buf) && buf[end] == '\r' {
					end++
				}
				if end < len(buf) && buf[end] == '\n' {
					end++
				}
				if len(bytes.TrimSpace(append(append([]byte(nil), buf[:i]...), buf[end:]...))) != 0 {
					return nil, false, fmt.Errorf("invalid stale attach handshake")
				}
				return nil, true, nil
			}
			if len(buf) > 0 && buf[0] != staleSentinel[0] {
				if buf[0] != 0x1b {
					return nil, false, fmt.Errorf("invalid tmux attach setup")
				}
				return append([]byte(nil), buf...), false, nil
			}
		}
		if err != nil {
			return nil, false, fmt.Errorf("tmux attach handshake: %w", err)
		}
	}
	return nil, false, fmt.Errorf("tmux attach handshake exceeds limit")
}

type readResult struct {
	frame proto.Frame
	err   error
}

func readClientFrames(r io.Reader, out chan<- readResult, stop <-chan struct{}) {
	for {
		f, e := proto.ReadFrame(r)
		select {
		case out <- readResult{f, e}:
		case <-stop:
			return
		}
		if e != nil {
			return
		}
	}
}

var errObserveMode = errors.New("input is disabled in observe mode")

func writeInput(mode string, w io.Writer, payload []byte) error {
	if mode == "observe" {
		return errObserveMode
	}
	if mode != "control" {
		return fmt.Errorf("invalid mode")
	}
	for len(payload) > 0 {
		n, err := w.Write(payload)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}
