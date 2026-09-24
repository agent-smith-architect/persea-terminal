package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/attachmentwire"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
)

// Subscriber lifetime and transport-close regressions.
//
// The subscriber channel a unified attachment tails is the only thing that
// carries committed output to the browser. Evicting a lagged/full subscriber
// used to close only that Go channel: the attachment writer drained what was
// buffered, returned, and left the attachment open on a healthy socket that
// would never carry another byte — a silently dead pane. The invariant pinned
// here is that EVERY provider-side subscriber removal is a typed close that
// ends the attachment with a reconnectable reason, and that re-attaching on
// the same session replays the journal INCLUDING the event that evicted it.
//
// The harness is the real writer seam (unifiedAttachmentFrameWriter over a
// lockedWriter) on a real journal realm; the downstream is a net.Pipe, whose
// unbuffered writes wedge the writer exactly the way a front door that has
// stopped reading does. The test is the browser: it reads the wire, and it
// re-mints once by opening a second attachment on the same session.

// b1Attachment is one Control attachment as the wire sees it.
type b1Attachment struct {
	session string
	writer  *unifiedAttachmentFrameWriter
	server  net.Conn
	client  net.Conn

	mu       sync.Mutex
	paused   bool
	resume   chan struct{}
	live     []byte
	controls []proto.Control
	replay   []byte
	closed   bool
	readErr  error
	readDone chan struct{}
	// ended is closed when the broker ends the attachment from its side (the
	// writer's end hook), which is the only observation a peer that never
	// reads can be held to.
	ended chan struct{}
}

func b1Open(t *testing.T, effects *UnifiedDevPaneEffects, session, source string) *b1Attachment {
	t.Helper()
	server, client := net.Pipe()
	attachment := &b1Attachment{session: session, server: server, client: client, resume: make(chan struct{}), readDone: make(chan struct{}), ended: make(chan struct{})}
	close(attachment.resume)
	var endOnce sync.Once
	attachment.writer = &unifiedAttachmentFrameWriter{
		downstream: &attachmentFrameWriter{wire: &lockedWriter{w: server}},
		provider:   effects, session: session,
		end: func() {
			_ = server.Close()
			endOnce.Do(func() { close(attachment.ended) })
		},
	}
	go attachment.read()
	t.Cleanup(func() {
		_ = attachment.writer.Close(context.Background())
		_ = server.Close()
		_ = client.Close()
	})
	// PREPARE then COMMIT, the way the epoch drives a real attachment: the
	// snapshot arrives as PREPARE's Replay, COMMIT starts the live tail.
	prepared := make(chan error, 1)
	go func() {
		prepare := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FramePrepare, Source: source, Epoch: 1, Cut: 1, Kind: terminal.CutInitial, Columns: 80, Rows: 24}
		prepared <- attachment.writer.WriteFrame(context.Background(), unifiedE2E1RawFrame(t, prepare))
	}()
	select {
	case err := <-prepared:
		if err != nil {
			t.Fatalf("%s PREPARE: %v", session, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s PREPARE did not complete", session)
	}
	committed := make(chan error, 1)
	go func() {
		commit := terminal.Frame{Version: terminal.ProtocolVersion, Type: terminal.FrameCommit, Source: source, Epoch: 1, Cut: 1}
		committed <- attachment.writer.WriteFrame(context.Background(), unifiedE2E1RawFrame(t, commit))
	}()
	select {
	case err := <-committed:
		if err != nil {
			t.Fatalf("%s COMMIT: %v", session, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s COMMIT did not complete", session)
	}
	return attachment
}

// Read is the peer's byte-level intake: every read of the wire passes the
// wedge gate, so a wedged peer stops consuming bytes exactly where it is —
// mid-frame if that is where the gate closed — and the writer's next write
// parks on the unbuffered pipe deterministically, the way a kernel socket
// buffer that has filled parks a writer whatever frame boundary it is at.
func (attachment *b1Attachment) Read(p []byte) (int, error) {
	attachment.mu.Lock()
	resume := attachment.resume
	attachment.mu.Unlock()
	<-resume
	return attachment.client.Read(p)
}

func (attachment *b1Attachment) read() {
	defer close(attachment.readDone)
	for {
		wire, err := proto.ReadFrame(attachment)
		if err != nil {
			attachment.mu.Lock()
			attachment.closed, attachment.readErr = true, err
			attachment.mu.Unlock()
			return
		}
		switch wire.Type {
		case proto.FrameControl:
			control, decodeErr := proto.DecodeControl(wire.Payload)
			attachment.mu.Lock()
			if decodeErr == nil {
				attachment.controls = append(attachment.controls, control)
			}
			attachment.mu.Unlock()
		case proto.FrameAttachment:
			frame, decodeErr := attachmentwire.Decode(wire.Payload, attachmentwire.ServerToBrowser)
			attachment.mu.Lock()
			if decodeErr == nil {
				switch frame.Type {
				case terminal.FramePrepare:
					attachment.replay = append([]byte(nil), frame.Replay...)
				case terminal.FrameLive:
					attachment.live = append(attachment.live, frame.Data...)
				}
			}
			attachment.mu.Unlock()
		}
	}
}

// wedge stops the downstream reader at its next byte: the next write on the
// pipe blocks, which is what a front door that stopped draining looks like.
func (attachment *b1Attachment) wedge() {
	attachment.mu.Lock()
	defer attachment.mu.Unlock()
	if attachment.paused {
		return
	}
	attachment.paused = true
	attachment.resume = make(chan struct{})
}

func (attachment *b1Attachment) unwedge() {
	attachment.mu.Lock()
	defer attachment.mu.Unlock()
	if !attachment.paused {
		return
	}
	attachment.paused = false
	close(attachment.resume)
}

func (attachment *b1Attachment) snapshot() (live []byte, controls []proto.Control, closed bool, readErr error) {
	attachment.mu.Lock()
	defer attachment.mu.Unlock()
	return append([]byte(nil), attachment.live...), append([]proto.Control(nil), attachment.controls...), attachment.closed, attachment.readErr
}

func (attachment *b1Attachment) replayBytes() []byte {
	attachment.mu.Lock()
	defer attachment.mu.Unlock()
	return append([]byte(nil), attachment.replay...)
}

// b1Commit commits one payload to the journal and publishes it the way the
// retention runtime does after a durable commit, returning how long the
// publication itself took: the subscriber-close latency bound is on publication, never on
// fsync.
func b1Commit(t *testing.T, effects *UnifiedDevPaneEffects, key unifiedjournal.PaneKey, payload []byte) time.Duration {
	t.Helper()
	record, err := effects.realm.Append(key, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := effects.realm.Sync(key); err != nil {
		t.Fatal(err)
	}
	if err := effects.realm.AdvanceCommitted(key, record); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := effects.WritePaneRange(key, payload, record.Start, record.End, record.Sequence); err != nil {
		t.Fatal(err)
	}
	return time.Since(started)
}

func b1SubscriberCount(effects *UnifiedDevPaneEffects, key unifiedjournal.PaneKey) int {
	effects.subscriberMu.Lock()
	defer effects.subscriberMu.Unlock()
	return len(effects.subscribers[key])
}

func b1SubscriberOf(effects *UnifiedDevPaneEffects, key unifiedjournal.PaneKey) *unifiedDevSubscriber {
	effects.subscriberMu.Lock()
	defer effects.subscriberMu.Unlock()
	for subscriber := range effects.subscribers[key] {
		return subscriber
	}
	return nil
}

// TestUnifiedSubscriberLagClosesAttachmentTypedAndReconnectable is regression test
// A lagged subscriber closes within the publication-latency bound while
// sibling subscriber counts remain unchanged. A wedged attachment must
// receive a typed close without receiving the triggering event.
func TestUnifiedSubscriberLagClosesAttachmentTypedAndReconnectable(t *testing.T) {
	const panes = 6
	const wedgedIndex = panes - 1
	const publishLatencyBound = 500 * time.Millisecond

	realm := openRetentionRealm(t, "b1-subscriber-close")
	effects := &UnifiedDevPaneEffects{
		realm: realm, active: map[string]unifiedjournal.PaneKey{},
		subscribers: make(map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}),
	}
	sessions := make([]string, panes)
	keys := make([]unifiedjournal.PaneKey, panes)
	for index := range keys {
		sessions[index] = fmt.Sprintf("$%d", index+1)
		keys[index] = unifiedjournal.PaneKey{
			Server: "test", Session: sessions[index], ControlGeneration: 1,
			Window: fmt.Sprintf("@%d", index+1), Pane: fmt.Sprintf("%%%d", index+1), Incarnation: fmt.Sprintf("b1-incarnation-%d", index+1),
		}
		if err := realm.AdmitPane(keys[index], unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
			t.Fatal(err)
		}
		effects.active[sessions[index]] = keys[index]
	}
	prepareRecordingProviderForTest(t, effects, keys...)

	// 1. Six Control attachments on six distinct pane keys on one provider.
	attachments := make([]*b1Attachment, panes)
	expected := make([][]byte, panes)
	for index := range attachments {
		attachments[index] = b1Open(t, effects, sessions[index], fmt.Sprintf("b1-source-%d", index+1))
		if got := b1SubscriberCount(effects, keys[index]); got != 1 {
			t.Fatalf("pane %d subscribers=%d after open, want 1", index+1, got)
		}
	}
	siblingSubscribers := make([]*unifiedDevSubscriber, panes)
	for index := range siblingSubscribers {
		siblingSubscribers[index] = b1SubscriberOf(effects, keys[index])
	}
	var maxPublish time.Duration
	publish := func(index int, payload []byte) {
		expected[index] = append(expected[index], payload...)
		if took := b1Commit(t, effects, keys[index], payload); took > maxPublish {
			maxPublish = took
		}
	}
	waitLive := func(index int, what string) {
		pollUntil(t, 5*time.Second, what, func() bool {
			live, _, _, _ := attachments[index].snapshot()
			return bytes.Equal(live, expected[index])
		})
	}
	// Precondition: every pane is live on its own tail.
	for index := range attachments {
		publish(index, []byte(fmt.Sprintf("[warm-%d]", index+1)))
		waitLive(index, fmt.Sprintf("pane %d warm-up sentinel", index+1))
	}

	// 2. Wedge one attachment's downstream writer and overflow its subscriber
	// channel while unique output keeps flowing on all six panes. The
	// triggering event is the publication that evicted the subscriber; the
	// wedged writer can hold at most one in-flight event plus the channel's
	// capacity, so eviction cannot come before the 65th publication.
	attachments[wedgedIndex].wedge()
	var trigger []byte
	round := 0
	for trigger == nil {
		round++
		if round > 512 {
			t.Fatalf("the wedged subscriber was never evicted after %d publications", round)
		}
		for index := 0; index < panes; index++ {
			payload := []byte(fmt.Sprintf("[r%03d-p%d]", round, index+1))
			publish(index, payload)
			if index == wedgedIndex && b1SubscriberCount(effects, keys[wedgedIndex]) == 0 {
				trigger = payload
			}
		}
	}
	if round < 65 {
		t.Fatalf("eviction after %d publications: the 64-record channel was not overflowed", round)
	}
	if maxPublish > publishLatencyBound {
		t.Fatalf("S1: publication latency %v exceeded %v while one subscriber was wedged", maxPublish, publishLatencyBound)
	}
	// the five sibling keys keep exactly the subscriber they had.
	for index := 0; index < wedgedIndex; index++ {
		if got := b1SubscriberCount(effects, keys[index]); got != 1 {
			t.Fatalf("S1: sibling pane %d subscribers=%d after eviction, want 1", index+1, got)
		}
		if b1SubscriberOf(effects, keys[index]) != siblingSubscribers[index] {
			t.Fatalf("S1: sibling pane %d subscriber was replaced", index+1)
		}
	}
	if evicted := siblingSubscribers[wedgedIndex]; evicted.closeReason() != proto.SubscriberClosedLagged {
		t.Fatalf("evicted subscriber closeReason=%q want %q", evicted.closeReason(), proto.SubscriberClosedLagged)
	}

	// 3. The five healthy panes receive every sentinel in order and never
	// reconnect: no control on their wire, socket still open, tail intact.
	for index := 0; index < wedgedIndex; index++ {
		waitLive(index, fmt.Sprintf("pane %d full ordered output", index+1))
		_, controls, closed, readErr := attachments[index].snapshot()
		if len(controls) != 0 || closed {
			t.Fatalf("healthy pane %d saw controls=%v closed=%v err=%v", index+1, controls, closed, readErr)
		}
	}

	// 4. The wedged writer stays wedged: the peer never drains. Eviction closes
	// the subscriber's tail, but the causal writer holds writer.mu inside
	// writeEventLocked on a downstream write the peer will never complete.
	// The broker must end the attachment
	// itself within a bound that does not depend on the peer, and must never
	// have delivered the triggering event on the old attachment.
	select {
	case <-attachments[wedgedIndex].ended:
	case <-time.After(unifiedSubscriberCloseGrace + 5*time.Second):
		t.Fatalf("F1: the broker did not end the wedged attachment within %v of the eviction", unifiedSubscriberCloseGrace+5*time.Second)
	}
	if proto.ClassifyAttachmentError(string(proto.SubscriberClosedLagged)) != proto.AttachmentErrorFatal {
		t.Fatalf("close code %q is not a fatal attachment code: the front door would not close the socket with it", proto.SubscriberClosedLagged)
	}
	// Only now does the peer read again, and only to observe what the broker
	// left it: the connection is closed, nothing after the last frame it
	// took, no typed control — an unbuffered pipe to a peer that was not
	// reading cannot carry one; the verdict is recorded on the subscriber
	// and in the broker journal, and the front door turns the close into a
	// reconnect.
	attachments[wedgedIndex].unwedge()
	pollUntil(t, 5*time.Second, "closure observed on the wedged attachment", func() bool {
		_, _, closed, _ := attachments[wedgedIndex].snapshot()
		return closed
	})
	live, controls, closed, readErr := attachments[wedgedIndex].snapshot()
	if len(controls) != 0 {
		t.Fatalf("a peer that never read received controls=%+v", controls)
	}
	if !closed || !(errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrClosedPipe) || errors.Is(readErr, io.ErrUnexpectedEOF)) {
		t.Fatalf("wedged attachment was not ended by the broker: closed=%v err=%v", closed, readErr)
	}
	if bytes.Contains(live, trigger) {
		t.Fatalf("the old attachment delivered the triggering event %q after eviction", trigger)
	}
	if !bytes.HasPrefix(expected[wedgedIndex], live) {
		t.Fatalf("the old attachment's output is not a prefix of the committed stream")
	}
	if b1SubscriberCount(effects, keys[wedgedIndex]) != 0 {
		t.Fatal("the evicted subscriber is still registered")
	}

	// Re-mint once: the browser re-attaches on the same session. The
	// snapshot must carry everything committed, including the triggering
	// event, and the new tail must be the only subscriber of that key.
	reminted := b1Open(t, effects, sessions[wedgedIndex], "b1-source-6-reminted")
	if got := b1SubscriberCount(effects, keys[wedgedIndex]); got != 1 {
		t.Fatalf("re-minted attachment subscribers=%d want 1", got)
	}
	replay := reminted.replayBytes()
	if !bytes.Equal(replay, expected[wedgedIndex]) {
		t.Fatalf("re-minted replay does not equal the committed stream: replay=%d committed=%d", len(replay), len(expected[wedgedIndex]))
	}
	if !bytes.Contains(replay, trigger) {
		t.Fatalf("re-minted replay is missing the triggering event %q", trigger)
	}

	// A post-reconnect sentinel arrives on the new attachment exactly once,
	// and on every healthy sibling exactly once, in order.
	post := []byte("[post-reconnect-6]")
	before := len(expected[wedgedIndex])
	publish(wedgedIndex, post)
	pollUntil(t, 5*time.Second, "post-reconnect sentinel on the re-minted tail", func() bool {
		live, _, _, _ := reminted.snapshot()
		return bytes.Equal(live, expected[wedgedIndex][before:])
	})
	newLive, newControls, newClosed, _ := reminted.snapshot()
	if bytes.Count(newLive, post) != 1 || len(newControls) != 0 || newClosed {
		t.Fatalf("re-minted attachment: post sentinel count=%d controls=%v closed=%v", bytes.Count(newLive, post), newControls, newClosed)
	}
	if oldLive, _, _, _ := attachments[wedgedIndex].snapshot(); bytes.Contains(oldLive, post) {
		t.Fatal("the closed attachment received the post-reconnect sentinel")
	}
	for index := 0; index < wedgedIndex; index++ {
		publish(index, []byte(fmt.Sprintf("[post-%d]", index+1)))
		waitLive(index, fmt.Sprintf("pane %d post-reconnect sentinel", index+1))
		_, controls, closed, _ := attachments[index].snapshot()
		if len(controls) != 0 || closed {
			t.Fatalf("healthy pane %d reconnected: controls=%v closed=%v", index+1, controls, closed)
		}
	}
	if maxPublish > publishLatencyBound {
		t.Fatalf("S1: publication latency %v exceeded %v", maxPublish, publishLatencyBound)
	}
}

// TestUnifiedSubscriberSelfCancelIsNotATypedClose pins the other half of the
// contract: an attachment that ends on its own terms (Close/stopOutput) cancels
// its subscriber with NO reason, so streamTail writes nothing — the socket is
// already the attachment's own to close. A typed close must never be
// fabricated for a cancel.
func TestUnifiedSubscriberSelfCancelIsNotATypedClose(t *testing.T) {
	realm := openRetentionRealm(t, "b1-self-cancel")
	key := unifiedjournal.PaneKey{Server: "test", Session: "$1", ControlGeneration: 1, Window: "@1", Pane: "%1", Incarnation: "self-cancel"}
	if err := realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	effects := &UnifiedDevPaneEffects{
		realm: realm, active: map[string]unifiedjournal.PaneKey{"$1": key},
		subscribers: make(map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}),
	}
	prepareRecordingProviderForTest(t, effects, key)
	attachment := b1Open(t, effects, "$1", "self-cancel-source")
	subscriber := b1SubscriberOf(effects, key)
	if subscriber == nil {
		t.Fatal("no subscriber after open")
	}
	attachment.writer.stopOutput()
	select {
	case deliveredForTest, open := <-subscriber.events():
		if open {
			subscriber.releaseEvent(deliveredForTest)
		}
		if open {
			t.Fatal("cancel did not close the tail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not close the tail")
	}
	if subscriber.closeReason() != "" {
		t.Fatalf("self-cancel carried a typed reason %q", subscriber.closeReason())
	}
	effects.subscriberMu.Lock()
	_, subscriberBucket := effects.subscribers[key]
	effects.subscriberMu.Unlock()
	if subscriberBucket {
		t.Fatal("self-cancel retained the last subscriber's outer bucket")
	}
	// Nothing is written on the wire for a self-cancel: the pipe stays open
	// with no control on it.
	time.Sleep(50 * time.Millisecond)
	_, controls, closed, _ := attachment.snapshot()
	if len(controls) != 0 || closed {
		t.Fatalf("self-cancel wrote to the wire: controls=%v closed=%v", controls, closed)
	}
}

func TestUnifiedSubscriberAlreadyDonePublishRemovesLastOuterBucket(t *testing.T) {
	key := unifiedjournal.PaneKey{Server: "test", Session: "$1", ControlGeneration: 1, Window: "@1", Pane: "%1", Incarnation: "already-done"}
	done := make(chan struct{})
	close(done)
	subscriber := &unifiedDevSubscriber{
		data: make(chan unifiedjournal.Event), done: done,
	}
	effects := &UnifiedDevPaneEffects{
		subscribers: map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}{
			key: {subscriber: {}},
		},
	}
	if err := effects.publishEvent(key, unifiedjournal.Event{Kind: unifiedjournal.RecordOutput, Sequence: 1, Payload: []byte("already done")}); err != nil {
		t.Fatal(err)
	}
	effects.subscriberMu.Lock()
	_, subscriberBucket := effects.subscribers[key]
	effects.subscriberMu.Unlock()
	if subscriberBucket {
		t.Fatal("publisher already-done branch retained the last subscriber's outer bucket")
	}
}

// TestUnifiedCloseSubscribersIsTheSharedTypedPrimitive pins the shape a
// deliberate provider-side close takes — the primitive journal rotation calls
// with generation_rotated: every subscriber of the key is closed with the one
// typed reason, siblings on other keys are untouched, an already-cancelled
// subscriber is never double-closed, and a reason outside the closed set is
// never invented at the call site.
func TestUnifiedCloseSubscribersIsTheSharedTypedPrimitive(t *testing.T) {
	keyA := unifiedjournal.PaneKey{Server: "main", Session: "$1", ControlGeneration: 1, Window: "@1", Pane: "%1", Incarnation: "one"}
	keyB := unifiedjournal.PaneKey{Server: "main", Session: "$2", ControlGeneration: 1, Window: "@2", Pane: "%2", Incarnation: "two"}
	effects := &UnifiedDevPaneEffects{subscribers: map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}{}}
	first := &unifiedDevSubscriber{data: make(chan unifiedjournal.Event, 1), done: make(chan struct{})}
	second := &unifiedDevSubscriber{data: make(chan unifiedjournal.Event, 1), done: make(chan struct{})}
	sibling := &unifiedDevSubscriber{data: make(chan unifiedjournal.Event, 1), done: make(chan struct{})}
	effects.subscribers[keyA] = map[*unifiedDevSubscriber]struct{}{first: {}, second: {}}
	effects.subscribers[keyB] = map[*unifiedDevSubscriber]struct{}{sibling: {}}

	if closed := effects.closeSubscribers(keyA, proto.SubscriberClosedLagged); closed != 2 {
		t.Fatalf("closed=%d want 2", closed)
	}
	for _, subscriber := range []*unifiedDevSubscriber{first, second} {
		if _, open := <-subscriber.events(); open {
			t.Fatal("subscriber tail not closed")
		}
		if subscriber.closeReason() != proto.SubscriberClosedLagged {
			t.Fatalf("closeReason=%q", subscriber.closeReason())
		}
	}
	if b1SubscriberCount(effects, keyA) != 0 || b1SubscriberCount(effects, keyB) != 1 {
		t.Fatalf("subscriber counts A=%d B=%d", b1SubscriberCount(effects, keyA), b1SubscriberCount(effects, keyB))
	}
	select {
	case <-sibling.events():
		t.Fatal("a sibling key's subscriber was touched")
	default:
	}
	// Closing again is a no-op: the removed subscribers are not double-closed.
	if closed := effects.closeSubscribers(keyA, proto.SubscriberClosedLagged); closed != 0 {
		t.Fatalf("second close closed=%d want 0", closed)
	}
	// A subscriber may have removed itself immediately before provider
	// settlement. Bulk close must still retire the already-empty outer bucket.
	effects.subscriberMu.Lock()
	effects.subscribers[keyA] = map[*unifiedDevSubscriber]struct{}{}
	effects.subscriberMu.Unlock()
	if closed := effects.closeSubscribers(keyA, proto.SubscriberClosedLagged); closed != 0 {
		t.Fatalf("empty-bucket close closed=%d want 0", closed)
	}
	effects.subscriberMu.Lock()
	_, emptyBucket := effects.subscribers[keyA]
	effects.subscriberMu.Unlock()
	if emptyBucket {
		t.Fatal("provider fatal close retained an already-empty subscriber bucket")
	}
	// A reason outside the closed set never reaches the wire as-is.
	effects.subscriberMu.Lock()
	effects.closeSubscriberLocked(keyB, sibling, proto.SubscriberCloseReason("made_up_reason"))
	effects.subscriberMu.Unlock()
	if _, open := <-sibling.events(); open {
		t.Fatal("sibling tail not closed")
	}
	if !proto.IsSubscriberCloseReason(sibling.closeReason()) {
		t.Fatalf("an invented reason %q reached the subscriber", sibling.closeReason())
	}
}

// TestUnifiedSubscriberVerdictIsBoundedWhileDownstreamStaysWedged is the
// stalled-subscriber regression test: one attachment, its writer parked on
// a downstream write to a peer that never reads again, and a typed verdict
// delivered through closeSubscribers — the primitive journal rotation calls
// at step 7c. The broker must end the attachment within
// unifiedSubscriberCloseGrace plus scheduling slack, without the peer, and
// not before the in-band path had its full grace. The peer is never
// unwedged. Re-minting on the same session must replay the event the old
// attachment never delivered, and a post-reconnect sentinel must arrive on
// the new attachment exactly once.
func TestUnifiedSubscriberVerdictIsBoundedWhileDownstreamStaysWedged(t *testing.T) {
	realm := openRetentionRealm(t, "f1-bounded-verdict")
	key := unifiedjournal.PaneKey{Server: "test", Session: "$1", ControlGeneration: 1, Window: "@1", Pane: "%1", Incarnation: "f1-bounded"}
	if err := realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	effects := &UnifiedDevPaneEffects{
		realm: realm, active: map[string]unifiedjournal.PaneKey{"$1": key},
		subscribers: make(map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}),
	}
	prepareRecordingProviderForTest(t, effects, key)
	attachment := b1Open(t, effects, "$1", "f1-bounded-source")
	subscriber := b1SubscriberOf(effects, key)
	if subscriber == nil {
		t.Fatal("no subscriber after open")
	}
	var expected []byte
	publish := func(payload []byte) {
		expected = append(expected, payload...)
		b1Commit(t, effects, key, payload)
	}
	publish([]byte("[warm]"))
	pollUntil(t, 5*time.Second, "warm-up sentinel", func() bool {
		live, _, _, _ := attachment.snapshot()
		return bytes.Equal(live, expected)
	})

	// Park the writer: the peer stops reading, one committed event is taken
	// off the tail and its write blocks on the unbuffered pipe. The peer's
	// intake is gated per byte, so the frame never completes on its side.
	attachment.wedge()
	parked := []byte("[parked]")
	publish(parked)
	pollUntil(t, 5*time.Second, "writer parked on the wedged write", func() bool {
		return len(subscriber.events()) == 0
	})
	// A second event sits in the tail behind the parked one: the verdict must
	// outrank it, never drain it.
	buffered := []byte("[buffered]")
	publish(buffered)

	verdictAt := time.Now()
	if closed := effects.closeSubscribers(key, proto.SubscriberClosedLagged); closed != 1 {
		t.Fatalf("closeSubscribers closed=%d want 1", closed)
	}
	select {
	case <-attachment.ended:
	case <-time.After(unifiedSubscriberCloseGrace + 5*time.Second):
		t.Fatalf("F1: the broker did not end the attachment within %v of the verdict while the peer stayed wedged", unifiedSubscriberCloseGrace+5*time.Second)
	}
	if elapsed := time.Since(verdictAt); elapsed < unifiedSubscriberCloseGrace {
		t.Fatalf("the attachment was cut after %v, before the in-band close had its %v grace", elapsed, unifiedSubscriberCloseGrace)
	}
	if subscriber.closeReason() != proto.SubscriberClosedLagged {
		t.Fatalf("closeReason=%q want %q", subscriber.closeReason(), proto.SubscriberClosedLagged)
	}
	if b1SubscriberCount(effects, key) != 0 {
		t.Fatal("the closed subscriber is still registered")
	}
	// The peer is never unwedged: nothing after the warm-up ever reached it.
	if live, controls, closed, _ := attachment.snapshot(); !bytes.Equal(live, []byte("[warm]")) || len(controls) != 0 || closed {
		t.Fatalf("wedged peer observed live=%q controls=%v closed=%v", live, controls, closed)
	}

	// Re-mint once on the same session: the snapshot carries everything
	// committed, including both events the old attachment never delivered.
	reminted := b1Open(t, effects, "$1", "f1-bounded-source-reminted")
	if got := b1SubscriberCount(effects, key); got != 1 {
		t.Fatalf("re-minted attachment subscribers=%d want 1", got)
	}
	if replay := reminted.replayBytes(); !bytes.Equal(replay, expected) || !bytes.Contains(replay, parked) || !bytes.Contains(replay, buffered) {
		t.Fatalf("re-minted replay=%q want the committed stream %q", replay, expected)
	}
	post := []byte("[post-reconnect]")
	publish(post)
	pollUntil(t, 5*time.Second, "post-reconnect sentinel on the re-minted tail", func() bool {
		live, _, _, _ := reminted.snapshot()
		return bytes.Equal(live, post)
	})
	if live, controls, closed, _ := reminted.snapshot(); bytes.Count(live, post) != 1 || len(controls) != 0 || closed {
		t.Fatalf("re-minted attachment: post sentinel count=%d controls=%v closed=%v", bytes.Count(live, post), controls, closed)
	}
	if live, _, _, _ := attachment.snapshot(); bytes.Contains(live, post) {
		t.Fatal("the cut attachment received the post-reconnect sentinel")
	}
}

// TestUnifiedSubscriberVerdictReachesADrainingPeerInBand pins the other
// half of the bound: a peer that IS reading receives the typed close as the
// error control on its wire — the only way the front door learns the
// reconnectable reason — followed by the broker's close, and is never cut
// without it. The verdict outranks the events still buffered on the tail:
// nothing committed after the peer's last frame is written ahead of it.
func TestUnifiedSubscriberVerdictReachesADrainingPeerInBand(t *testing.T) {
	realm := openRetentionRealm(t, "f1-inband-verdict")
	key := unifiedjournal.PaneKey{Server: "test", Session: "$1", ControlGeneration: 1, Window: "@1", Pane: "%1", Incarnation: "f1-inband"}
	if err := realm.AdmitPane(key, unifiedjournal.Geometry{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	effects := &UnifiedDevPaneEffects{
		realm: realm, active: map[string]unifiedjournal.PaneKey{"$1": key},
		subscribers: make(map[unifiedjournal.PaneKey]map[*unifiedDevSubscriber]struct{}),
	}
	prepareRecordingProviderForTest(t, effects, key)
	attachment := b1Open(t, effects, "$1", "f1-inband-source")
	b1Commit(t, effects, key, []byte("[warm]"))
	pollUntil(t, 5*time.Second, "warm-up sentinel", func() bool {
		live, _, _, _ := attachment.snapshot()
		return bytes.Equal(live, []byte("[warm]"))
	})
	if closed := effects.closeSubscribers(key, proto.SubscriberClosedLagged); closed != 1 {
		t.Fatalf("closeSubscribers closed=%d want 1", closed)
	}
	pollUntil(t, 5*time.Second, "typed close on the draining peer", func() bool {
		_, controls, closed, _ := attachment.snapshot()
		return len(controls) != 0 && closed
	})
	live, controls, closed, readErr := attachment.snapshot()
	if len(controls) != 1 || controls[0].Type != "error" || controls[0].Code != string(proto.SubscriberClosedLagged) {
		t.Fatalf("draining peer controls=%+v want one error %q", controls, proto.SubscriberClosedLagged)
	}
	if !closed || !(errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrClosedPipe) || errors.Is(readErr, io.ErrUnexpectedEOF)) {
		t.Fatalf("draining peer was not ended by the broker: closed=%v err=%v", closed, readErr)
	}
	if !bytes.Equal(live, []byte("[warm]")) {
		t.Fatalf("draining peer live=%q want only the warm-up sentinel", live)
	}
	select {
	case <-attachment.ended:
	default:
		t.Fatal("the broker did not end the attachment after the typed close")
	}
}
