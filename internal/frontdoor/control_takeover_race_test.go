//go:build f3_takeover_adapter

package frontdoor

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"persea-terminal/internal/terminal"
)

// The production definitions referenced below are supplied on the baseline by
// the sealed external Go overlay. They are package-private and nil by default.
// Production may emit only the six closed checkpoint kinds. Checkpoint is
// observation/blocking coordination only: no return value can mutate lease
// state, mint/bypass authority, or short-circuit CSRF/capability validation.

type f3CheckpointRecorder struct {
	events chan controlTakeoverCheckpoint
	mu     sync.Mutex
	gates  map[controlTakeoverCheckpointKind]chan struct{}
}

func newF3CheckpointRecorder() *f3CheckpointRecorder {
	return &f3CheckpointRecorder{
		events: make(chan controlTakeoverCheckpoint, 256),
		gates:  make(map[controlTakeoverCheckpointKind]chan struct{}),
	}
}

func (r *f3CheckpointRecorder) gate(kind controlTakeoverCheckpointKind) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	gate := make(chan struct{})
	r.gates[kind] = gate
	return gate
}

func (r *f3CheckpointRecorder) Checkpoint(event controlTakeoverCheckpoint) {
	r.events <- event
	r.mu.Lock()
	gate := r.gates[event.Kind]
	r.mu.Unlock()
	if gate != nil {
		<-gate
	}
}

func (r *f3CheckpointRecorder) await(t *testing.T, kind controlTakeoverCheckpointKind) controlTakeoverCheckpoint {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-r.events:
			if event.Kind == kind {
				return event
			}
		case <-timer.C:
			t.Fatalf("F3-W0 takeover-oracle-infrastructure: adapter checkpoint absent: %v", kind)
		}
	}
}

func f3InstallRecorder(t *testing.T, recorder *f3CheckpointRecorder) {
	t.Helper()
	if controlTakeoverTestAdapterHook != nil {
		t.Fatal("F3 adapter precondition: hook was not nil")
	}
	controlTakeoverTestAdapterHook = recorder
	t.Cleanup(func() { controlTakeoverTestAdapterHook = nil })
}

func TestControlTakeoverInputFenceW9(t *testing.T) {
	_, broker, addr, offer, incumbent, cleanup := f3StageOffer(t, "input-fence", "F3-W9 post-fence-input-race", f3Mutants["W9"])
	defer cleanup()
	recorder := newF3CheckpointRecorder()
	f3InstallRecorder(t, recorder)
	beforeGate := recorder.gate(controlTakeoverBeforeInputGate)

	preFence := []byte("pre-fence")
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		writeWSAttachment(t, incumbent, terminal.Frame{
			Version: terminal.ProtocolVersion, Type: terminal.FrameInput,
			Source: integrationSource, Epoch: 1, Data: preFence,
		})
	}()
	old := recorder.await(t, controlTakeoverBeforeInputGate)
	if old.Owner.IsZero() {
		t.Fatalf("F3-W9 post-fence-input-race: zero opaque owner mutant=%s", f3Mutants["W9"])
	}

	handle := f3AuthorizeTakeover(t, addr, offer, strings.Repeat("F", 43), "F3-W9 post-fence-input-race", f3Mutants["W9"])
	type dialResult struct {
		ws  *websocket.Conn
		err error
	}
	dialed := make(chan dialResult, 1)
	go func() {
		ws, _, err := f3DialTakeover(addr, handle)
		dialed <- dialResult{ws: ws, err: err}
	}()
	select {
	case event := <-recorder.events:
		if event.Kind == controlTakeoverTransferCommit {
			t.Fatalf("F3-W9 post-fence-input-race: transfer crossed held input gate mutant=%s", f3Mutants["W9"])
		}
	case <-time.After(100 * time.Millisecond):
	}
	close(beforeGate)
	<-inputDone
	select {
	case got := <-broker.inputs:
		if !bytes.Equal(got, preFence) {
			t.Fatalf("F3-W9 post-fence-input-race: pre-fence bytes=%q mutant=%s", got, f3Mutants["W9"])
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("F3-W9 post-fence-input-race: pre-fence write did not complete mutant=%s", f3Mutants["W9"])
	}
	result := <-dialed
	if result.err != nil {
		t.Fatalf("F3-W9 post-fence-input-race: takeover=%v mutant=%s", result.err, f3Mutants["W9"])
	}
	defer result.ws.Close()
	recorder.await(t, controlTakeoverTransferCommit)

	writeWSAttachment(t, incumbent, terminal.Frame{
		Version: terminal.ProtocolVersion, Type: terminal.FrameInput,
		Source: integrationSource, Epoch: 2, Data: []byte("stale-byte"),
	})
	select {
	case got := <-broker.inputs:
		if bytes.Equal(got, []byte("stale-byte")) {
			t.Fatalf("F3-W9 post-fence-input-race: stale byte crossed input fence mutant=%s", f3Mutants["W9"])
		}
	case <-time.After(150 * time.Millisecond):
	}
}

func TestControlTakeoverLastCommitWinsW10(t *testing.T) {
	front, _, addr, firstOffer, _, cleanup := f3StageOffer(t, "last-wins", "F3-W10 last-wins-race", f3Mutants["W10"])
	defer cleanup()
	recorder := newF3CheckpointRecorder()
	f3InstallRecorder(t, recorder)
	const candidates = 32
	handles := make([]string, candidates)
	for index := range handles {
		offer := firstOffer
		if index > 0 {
			var err error
			offer, err = front.handles.mint(f3Authority("last-wins"), front.cfg.Ingress.OperatorLogin, "control")
			if err != nil {
				t.Fatal(err)
			}
			refused := f3DialTerminalWS(t, addr, offer, "control", "F3-W10 last-wins-race", f3Mutants["W10"])
			if reason := readWSCloseReason(t, refused); reason != "lease_held" {
				t.Fatalf("F3-W10 last-wins-race: candidate=%d refusal=%q mutant=%s", index, reason, f3Mutants["W10"])
			}
			_ = refused.Close()
		}
		requestID := fmt.Sprintf("%043d", index+1)
		handles[index] = f3AuthorizeTakeover(t, addr, offer, requestID, "F3-W10 last-wins-race", f3Mutants["W10"])
	}
	var wg sync.WaitGroup
	sockets := make([]*websocket.Conn, candidates)
	for index := range handles {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ws, _, err := f3DialTakeover(addr, handles[index])
			if err == nil {
				sockets[index] = ws
			}
		}(index)
	}
	wg.Wait()
	t.Cleanup(func() {
		for _, ws := range sockets {
			if ws != nil {
				_ = ws.Close()
			}
		}
	})
	var last controlTakeoverCheckpoint
	for range candidates {
		event := recorder.await(t, controlTakeoverTransferCommit)
		if last.Tuple.Generation >= event.Tuple.Generation {
			t.Fatalf("F3-W10 last-wins-race: generations %d then %d mutant=%s", last.Tuple.Generation, event.Tuple.Generation, f3Mutants["W10"])
		}
		last = event
	}
	if last.Tuple.Generation == 0 || last.Owner.IsZero() {
		t.Fatalf("F3-W10 last-wins-race: final tuple incomplete mutant=%s", f3Mutants["W10"])
	}
}

func TestControlTakeoverABALateCleanupW11(t *testing.T) {
	front, _, addr, offer, incumbent, cleanup := f3StageOffer(t, "aba", "F3-W11 aba-late-cleanup", f3Mutants["W11"])
	defer cleanup()
	recorder := newF3CheckpointRecorder()
	f3InstallRecorder(t, recorder)

	handleB := f3AuthorizeTakeover(t, addr, offer, strings.Repeat("A", 43), "F3-W11 aba-late-cleanup", f3Mutants["W11"])
	b, _, err := f3DialTakeover(addr, handleB)
	if err != nil {
		t.Fatalf("F3-W11 aba-late-cleanup: B dial=%v mutant=%s", err, f3Mutants["W11"])
	}
	defer b.Close()
	commitB := recorder.await(t, controlTakeoverTransferCommit)

	offerC, err := front.handles.mint(f3Authority("aba"), front.cfg.Ingress.OperatorLogin, "control")
	if err != nil {
		t.Fatal(err)
	}
	refusedC := f3DialTerminalWS(t, addr, offerC, "control", "F3-W11 aba-late-cleanup", f3Mutants["W11"])
	if reason := readWSCloseReason(t, refusedC); reason != "lease_held" {
		t.Fatalf("F3-W11 aba-late-cleanup: C offer refusal=%q mutant=%s", reason, f3Mutants["W11"])
	}
	_ = refusedC.Close()
	handleC := f3AuthorizeTakeover(t, addr, offerC, strings.Repeat("D", 43), "F3-W11 aba-late-cleanup", f3Mutants["W11"])
	c, _, err := f3DialTakeover(addr, handleC)
	if err != nil {
		t.Fatalf("F3-W11 aba-late-cleanup: C dial=%v mutant=%s", err, f3Mutants["W11"])
	}
	defer c.Close()
	commitC := recorder.await(t, controlTakeoverTransferCommit)
	if commitB.Tuple.Generation == commitC.Tuple.Generation || commitB.Owner == commitC.Owner {
		t.Fatalf("F3-W11 aba-late-cleanup: successor tuple reused mutant=%s", f3Mutants["W11"])
	}

	// Scheduling is observation-only. The actual stale operations are driven
	// through old browser WebSockets so production release/close/cleanup paths,
	// rather than adapter mutation, decide the outcome.
	recorder.await(t, controlTakeoverStaleInjectionScheduling)
	writeApplicationPing(t, incumbent, "stale-pong")
	_ = incumbent.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	_ = incumbent.Close()
	observation := recorder.await(t, controlTakeoverStaleObservation)
	if observation.Operation == controlTakeoverStaleNone {
		t.Fatalf("F3-W11 aba-late-cleanup: no real stale observation mutant=%s", f3Mutants["W11"])
	}
	snapshot := recorder.await(t, controlTakeoverExactTupleSnapshot)
	if snapshot.Tuple.Generation != commitC.Tuple.Generation || snapshot.Owner != commitC.Owner {
		t.Fatalf("F3-W11 aba-late-cleanup: stale operation changed C tuple mutant=%s", f3Mutants["W11"])
	}
}
