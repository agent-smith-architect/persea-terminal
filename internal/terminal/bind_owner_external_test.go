package terminal_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	terminal "persea-terminal/internal/terminal"
)

var exactBindIDs = terminal.AttachmentIDs{ShadowSessionID: "$bind-owner-shadow", ClientID: "$bind-owner-client"}
var errScriptedProbe = errors.New("scripted process probe failure")

type scriptedProbe struct {
	mu                     sync.Mutex
	witness                map[int]terminal.ProcessWitness
	calls                  int
	blockAt                int
	blocked                chan struct{}
	release                chan struct{}
	blockOnce              sync.Once
	errorAt                int
	mismatchAt             int
	persistentErrorFrom    int
	persistentErrorPID     int
	persistentMismatchFrom int
	persistentMismatchPID  int
}

func (p *scriptedProbe) Witness(_ context.Context, pid int) (terminal.ProcessWitness, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	if p.calls == p.errorAt || (p.persistentErrorFrom > 0 && p.calls >= p.persistentErrorFrom && pid == p.persistentErrorPID) {
		p.mu.Unlock()
		return terminal.ProcessWitness{}, errScriptedProbe
	}
	w, ok := p.witness[pid]
	if !ok {
		p.mu.Unlock()
		return terminal.ProcessWitness{}, errors.New("missing process")
	}
	if p.calls == p.mismatchAt || (p.persistentMismatchFrom > 0 && p.calls >= p.persistentMismatchFrom && pid == p.persistentMismatchPID) {
		w.StartTime++
	}
	blocked, release := p.blocked, p.release
	shouldBlock := p.blockAt > 0 && call == p.blockAt
	p.mu.Unlock()
	if shouldBlock {
		if blocked != nil {
			p.blockOnce.Do(func() { close(blocked) })
		}
		if release != nil {
			<-release
		}
	}
	return w, nil
}

type observedContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *observedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func newScriptedProbe() *scriptedProbe {
	w := witness()
	return &scriptedProbe{witness: map[int]terminal.ProcessWitness{w.Server.PID: w.Server, w.Pane.PID: w.Pane}}
}

func newBindOwnerEpoch(t *testing.T, tx *fakeTransaction, probe terminal.ProcessProbe, parent context.Context) *terminal.Epoch {
	t.Helper()
	if parent == nil {
		parent = context.Background()
	}
	source, err := terminal.NewPinnedSource(witness(), tx, probe)
	if err != nil {
		t.Fatal(err)
	}
	e, err := terminal.NewEpoch(parent, source, 71, newMemoryTransport(), newRecordingPTY(), config())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func assertOneExactCleanup(t *testing.T, tx *fakeTransaction) {
	t.Helper()
	actions, requests, _, _ := tx.snapshot()
	cleanups := 0
	for i, action := range actions {
		if action != terminal.ActionCleanup {
			continue
		}
		cleanups++
		if requests[i].Attachment != exactBindIDs {
			t.Fatalf("cleanup IDs=%+v want %+v", requests[i].Attachment, exactBindIDs)
		}
	}
	if cleanups != 1 {
		t.Fatalf("cleanup count=%d actions=%v", cleanups, actions)
	}
}

func TestBindOwnerReturnedAndPostWitnessFailuresRetainExactOwnership(t *testing.T) {
	cases := []struct {
		name       string
		returned   bool
		errorAt    int
		mismatchAt int
	}{
		{name: "returned-witness-mismatch", returned: true},
		{name: "post-server-probe-error", errorAt: 3},
		{name: "post-server-probe-mismatch", mismatchAt: 3},
		{name: "post-pane-probe-error", errorAt: 4},
		{name: "post-pane-probe-mismatch", mismatchAt: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := newScriptedProbe()
			probe.errorAt, probe.mismatchAt = tc.errorAt, tc.mismatchAt
			resultWitness := witness()
			if tc.returned {
				resultWitness.Incarnation = "returned-replacement"
			}
			tx := &fakeTransaction{bindResult: &terminal.TransactionResult{Witness: resultWitness, Attachment: exactBindIDs}}
			e := newBindOwnerEpoch(t, tx, probe, nil)
			if err := e.Start(context.Background(), terminal.CutInitial); !errors.Is(err, terminal.ErrReopenRequired) {
				t.Fatalf("Start err=%v", err)
			}
			if err := e.Finalize(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertOneExactCleanup(t, tx)
		})
	}
}

func TestBindOwnerResultPlusErrorTransfersBeforeFailureAndRetriesCleanup(t *testing.T) {
	bindErr := errors.New("BIND transport reported failure after mutation")
	cleanupErr := errors.New("transient exact-ID cleanup failure")
	tx := &fakeTransaction{
		bindResult:      &terminal.TransactionResult{Witness: witness(), Attachment: exactBindIDs},
		bindErr:         bindErr,
		cleanupFailures: 1,
		cleanupErr:      cleanupErr,
	}
	e := newBindOwnerEpoch(t, tx, newScriptedProbe(), nil)
	if err := e.Start(context.Background(), terminal.CutInitial); !errors.Is(err, bindErr) {
		t.Fatalf("Start err=%v", err)
	}
	if err := e.Finalize(context.Background()); !errors.Is(err, cleanupErr) {
		t.Fatalf("first Finalize err=%v", err)
	}
	if err := e.Finalize(context.Background()); err != nil {
		t.Fatalf("retry Finalize err=%v", err)
	}
	actions, requests, _, _ := tx.snapshot()
	cleanups := 0
	for i, action := range actions {
		if action == terminal.ActionCleanup {
			cleanups++
			if requests[i].Attachment != exactBindIDs {
				t.Fatalf("cleanup IDs=%+v", requests[i].Attachment)
			}
		}
	}
	if cleanups != 2 {
		t.Fatalf("cleanup attempts=%d actions=%v", cleanups, actions)
	}
}

func TestBindOwnerNoExactIDsMeansNoAttachmentOrCleanup(t *testing.T) {
	cases := []struct {
		name string
		ids  terminal.AttachmentIDs
		err  error
		want error
	}{
		{name: "empty-success", want: terminal.ErrReopenRequired},
		{name: "partial-error", ids: terminal.AttachmentIDs{ShadowSessionID: "$partial"}, err: errors.New("partial identity is non-mutating"), want: terminal.ErrInvariant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := &fakeTransaction{
				bindResult: &terminal.TransactionResult{Witness: witness(), Attachment: tc.ids},
				bindErr:    tc.err,
			}
			e := newBindOwnerEpoch(t, tx, newScriptedProbe(), nil)
			if err := e.Start(context.Background(), terminal.CutInitial); !errors.Is(err, tc.want) {
				t.Fatalf("Start err=%v", err)
			}
			if err := e.Finalize(context.Background()); err != nil {
				t.Fatal(err)
			}
			actions, _, _, _ := tx.snapshot()
			if len(actions) != 1 || actions[0] != terminal.ActionBind {
				t.Fatalf("non-exact BIND triggered another transaction: %v", actions)
			}
		})
	}
}

func TestBindOwnerParentCancelDuringBlockedBindWaitsForOwnershipDecision(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	tx := &fakeTransaction{
		bindResult:  &terminal.TransactionResult{Witness: witness(), Attachment: exactBindIDs},
		bindErr:     context.Canceled,
		bindStarted: make(chan struct{}),
		bindRelease: make(chan struct{}),
	}
	e := newBindOwnerEpoch(t, tx, newScriptedProbe(), parent)
	startDone := make(chan error, 1)
	go func() { startDone <- e.Start(context.Background(), terminal.CutInitial) }()
	<-tx.bindStarted
	waiterBase, cancelWaiter := context.WithCancel(context.Background())
	waiterCtx := &observedContext{Context: waiterBase, entered: make(chan struct{})}
	waiterDone := make(chan error, 1)
	go func() { waiterDone <- e.Start(waiterCtx, terminal.CutInitial) }()
	<-waiterCtx.entered
	cancelParent()
	select {
	case <-e.Done():
		t.Fatal("Done closed before BIND ownership decision")
	default:
	}
	if err := e.Err(); err != nil {
		t.Fatalf("Err published before BIND ownership decision: %v", err)
	}
	cancelWaiter()
	if err := <-waiterDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("compatible waiter cancellation=%v", err)
	}
	waitCtx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if err := e.Finalize(waitCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Finalize completed before BIND ownership decision: %v", err)
	}
	actions, _, _, _ := tx.snapshot()
	if len(actions) != 1 || actions[0] != terminal.ActionBind {
		t.Fatalf("premature teardown actions=%v", actions)
	}
	close(tx.bindRelease)
	leaderErr := <-startDone
	if err := leaderErr; err == nil {
		t.Fatal("canceled blocked BIND unexpectedly succeeded")
	}
	<-e.Done()
	terminalErr := e.Err()
	if leaderErr != terminalErr {
		t.Fatalf("leader=%v want immutable terminal result %v", leaderErr, terminalErr)
	}
	if err := e.Start(context.Background(), terminal.CutInitial); err != terminalErr {
		t.Fatalf("post-terminal compatible Start=%v want identical %v", err, terminalErr)
	}
	if err := e.Start(context.Background(), terminal.CutReconnect); err != terminalErr {
		t.Fatalf("post-terminal conflicting Start=%v want identical %v", err, terminalErr)
	}
	if err := e.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertOneExactCleanup(t, tx)
}

func TestBindOwnerConcurrentRepeatedStartAdmitsOneBindAndOneCut(t *testing.T) {
	for _, kind := range []terminal.CutKind{terminal.CutInitial, terminal.CutReconnect} {
		t.Run(string(kind), func(t *testing.T) {
			tx := &fakeTransaction{bindStarted: make(chan struct{}), bindRelease: make(chan struct{}), cutCapture: []byte("history\n")}
			e := newBindOwnerEpoch(t, tx, newScriptedProbe(), nil)
			tx.onCut = func(req terminal.TransactionRequest) error { return e.PTYBytes(marker(req)) }
			const callers = 32
			ready := make(chan struct{})
			results := make(chan error, callers)
			contexts := make([]*observedContext, callers)
			for i := 0; i < callers; i++ {
				contexts[i] = &observedContext{Context: context.Background(), entered: make(chan struct{})}
				go func(ctx context.Context) {
					<-ready
					results <- e.Start(ctx, kind)
				}(contexts[i])
			}
			close(ready)
			<-tx.bindStarted
			for _, ctx := range contexts {
				<-ctx.entered
			}
			close(tx.bindRelease)
			for i := 0; i < callers; i++ {
				if err := <-results; err != nil {
					t.Fatalf("concurrent Start err=%v", err)
				}
			}
			if err := e.Start(context.Background(), kind); err != nil {
				t.Fatalf("repeated Start err=%v", err)
			}
			actions, _, callbackErr, _ := tx.snapshot()
			if callbackErr != nil {
				t.Fatal(callbackErr)
			}
			binds, cuts := 0, 0
			for _, action := range actions {
				if action == terminal.ActionBind {
					binds++
				}
				if action == terminal.ActionCut {
					cuts++
				}
			}
			if binds != 1 || cuts != 1 {
				t.Fatalf("BIND=%d CUT=%d actions=%v", binds, cuts, actions)
			}
			if err := e.Finalize(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBindOwnerCompatibleStartWaitersShareEveryFailure(t *testing.T) {
	cases := []struct {
		name string
		new  func(*testing.T) (*terminal.Epoch, *fakeTransaction, <-chan struct{}, chan struct{}, error)
	}{
		{
			name: "BIND",
			new: func(t *testing.T) (*terminal.Epoch, *fakeTransaction, <-chan struct{}, chan struct{}, error) {
				cause := errors.New("BIND failed after waiter admission")
				started, release := make(chan struct{}), make(chan struct{})
				tx := &fakeTransaction{bindResult: &terminal.TransactionResult{Witness: witness(), Attachment: exactBindIDs}, bindErr: cause, bindStarted: started, bindRelease: release}
				return newBindOwnerEpoch(t, tx, newScriptedProbe(), nil), tx, started, release, cause
			},
		},
		{
			name: "post-BIND-validation",
			new: func(t *testing.T) (*terminal.Epoch, *fakeTransaction, <-chan struct{}, chan struct{}, error) {
				started, release := make(chan struct{}), make(chan struct{})
				probe := newScriptedProbe()
				probe.persistentErrorFrom, probe.persistentErrorPID = 4, witness().Pane.PID
				tx := &fakeTransaction{bindResult: &terminal.TransactionResult{Witness: witness(), Attachment: exactBindIDs}, bindStarted: started, bindRelease: release}
				return newBindOwnerEpoch(t, tx, probe, nil), tx, started, release, errScriptedProbe
			},
		},
		{
			name: "CUT",
			new: func(t *testing.T) (*terminal.Epoch, *fakeTransaction, <-chan struct{}, chan struct{}, error) {
				cause := errors.New("CUT failed after waiter admission")
				started, release := make(chan struct{}), make(chan struct{})
				tx := &fakeTransaction{bindResult: &terminal.TransactionResult{Witness: witness(), Attachment: exactBindIDs}, cutErr: cause, cutStarted: started, cutRelease: release}
				return newBindOwnerEpoch(t, tx, newScriptedProbe(), nil), tx, started, release, cause
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, tx, started, release, cause := tc.new(t)
			leader := make(chan error, 1)
			go func() { leader <- e.Start(context.Background(), terminal.CutInitial) }()
			<-started

			waiterCtx := &observedContext{Context: context.Background(), entered: make(chan struct{})}
			waiter := make(chan error, 1)
			go func() { waiter <- e.Start(waiterCtx, terminal.CutInitial) }()
			<-waiterCtx.entered
			if err := e.Start(context.Background(), terminal.CutReconnect); !errors.Is(err, terminal.ErrOutOfState) {
				t.Fatalf("conflicting Start err=%v", err)
			}
			close(release)

			leaderErr, waiterErr := <-leader, <-waiter
			if leaderErr == nil || waiterErr == nil || !errors.Is(leaderErr, cause) || !errors.Is(waiterErr, cause) {
				t.Fatalf("leader=%v waiter=%v cause=%v", leaderErr, waiterErr, cause)
			}
			postDoneErr := e.Start(context.Background(), terminal.CutInitial)
			if postDoneErr == nil || leaderErr != waiterErr || leaderErr != postDoneErr || leaderErr != e.Err() {
				t.Fatalf("immutable Start result leader=%v waiter=%v post-Done=%v", leaderErr, waiterErr, postDoneErr)
			}
			if err := e.Finalize(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertOneExactCleanup(t, tx)
		})
	}
}

func TestBindOwnerCompatibleWaiterCancellationIsLocal(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	tx := &fakeTransaction{bindResult: &terminal.TransactionResult{Witness: witness(), Attachment: exactBindIDs}, bindStarted: started, bindRelease: release, cutCapture: []byte("history\n")}
	e := newBindOwnerEpoch(t, tx, newScriptedProbe(), nil)
	tx.onCut = func(req terminal.TransactionRequest) error { return e.PTYBytes(marker(req)) }
	leader := make(chan error, 1)
	go func() { leader <- e.Start(context.Background(), terminal.CutInitial) }()
	<-started

	base, cancel := context.WithCancel(context.Background())
	waiterCtx := &observedContext{Context: base, entered: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { waiter <- e.Start(waiterCtx, terminal.CutInitial) }()
	<-waiterCtx.entered
	cancel()
	if err := <-waiter; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter cancellation=%v", err)
	}
	select {
	case err := <-leader:
		t.Fatalf("waiter canceled leader: %v", err)
	default:
	}
	close(release)
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
	if err := e.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBindOwnerPersistentPaneFailureStillCleansExactAttachment(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		name := "pane-error"
		probe := newScriptedProbe()
		if mismatch {
			name = "pane-replacement"
			probe.persistentMismatchFrom, probe.persistentMismatchPID = 4, witness().Pane.PID
		} else {
			probe.persistentErrorFrom, probe.persistentErrorPID = 4, witness().Pane.PID
		}
		t.Run(name, func(t *testing.T) {
			returned := witness()
			returned.Pane.StartTime++
			returned.Columns++
			tx := &fakeTransaction{
				bindResult:    &terminal.TransactionResult{Witness: witness(), Attachment: exactBindIDs},
				cleanupResult: &terminal.TransactionResult{Witness: returned, Attachment: exactBindIDs},
			}
			e := newBindOwnerEpoch(t, tx, probe, nil)
			if err := e.Start(context.Background(), terminal.CutInitial); !errors.Is(err, terminal.ErrReopenRequired) {
				t.Fatalf("Start err=%v", err)
			}
			if err := e.Finalize(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertOneExactCleanup(t, tx)
			if probe.witness[witness().Pane.PID] != witness().Pane {
				t.Fatal("cleanup mutated source pane")
			}
		})
	}
}

func TestBindOwnerPersistentServerReplacementFailsClosedBeforeCleanup(t *testing.T) {
	probe := newScriptedProbe()
	probe.persistentMismatchFrom, probe.persistentMismatchPID = 3, witness().Server.PID
	tx := &fakeTransaction{bindResult: &terminal.TransactionResult{Witness: witness(), Attachment: exactBindIDs}}
	e := newBindOwnerEpoch(t, tx, probe, nil)
	if err := e.Start(context.Background(), terminal.CutInitial); !errors.Is(err, terminal.ErrReopenRequired) {
		t.Fatalf("Start err=%v", err)
	}
	if err := e.Finalize(context.Background()); !errors.Is(err, terminal.ErrReopenRequired) {
		t.Fatalf("Finalize err=%v", err)
	}
	actions, _, _, _ := tx.snapshot()
	if len(actions) != 1 || actions[0] != terminal.ActionBind {
		t.Fatalf("server replacement reached cleanup: %v", actions)
	}
}

func TestBindOwnerFinalizeBeforeStartIsBoundedAndDoesNoCleanup(t *testing.T) {
	tx := &fakeTransaction{}
	e := newBindOwnerEpoch(t, tx, newScriptedProbe(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Finalize(ctx); err != nil {
		t.Fatal(err)
	}
	actions, _, _, _ := tx.snapshot()
	if len(actions) != 0 {
		t.Fatalf("pre-Start Finalize issued transactions: %v", actions)
	}
}

func TestBindOwnerPreCanceledFirstStartDoesNotPublishAttempt(t *testing.T) {
	tx := &fakeTransaction{}
	e := newBindOwnerEpoch(t, tx, newScriptedProbe(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Start(ctx, terminal.CutInitial); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled Start err=%v", err)
	}
	if err := e.Err(); err != nil {
		t.Fatalf("pre-canceled Start terminalized epoch: %v", err)
	}
	select {
	case <-e.Done():
		t.Fatal("pre-canceled Start closed epoch")
	default:
	}
	actions, _, _, _ := tx.snapshot()
	if len(actions) != 0 {
		t.Fatalf("pre-canceled Start issued pinned actions: %v", actions)
	}
	if err := e.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBindOwnerPreCanceledFirstStartLeavesLiveAdmission(t *testing.T) {
	tx := &fakeTransaction{cutCapture: []byte("history\n")}
	e := newBindOwnerEpoch(t, tx, newScriptedProbe(), nil)
	tx.onCut = func(req terminal.TransactionRequest) error { return e.PTYBytes(marker(req)) }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Start(ctx, terminal.CutInitial); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled Start err=%v", err)
	}
	if err := e.Start(context.Background(), terminal.CutInitial); err != nil {
		t.Fatalf("live Start after pre-admission cancellation err=%v", err)
	}
	actions, _, callbackErr, _ := tx.snapshot()
	if callbackErr != nil {
		t.Fatal(callbackErr)
	}
	binds, cuts := 0, 0
	for _, action := range actions {
		if action == terminal.ActionBind {
			binds++
		}
		if action == terminal.ActionCut {
			cuts++
		}
	}
	if binds != 1 || cuts != 1 {
		t.Fatalf("live admission BIND=%d CUT=%d actions=%v", binds, cuts, actions)
	}
	if err := e.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBindOwnerPreCanceledParentStartTerminalizesBeforePinnedActions(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	parent, cancelParent := context.WithCancel(context.Background())
	tx := &fakeTransaction{}
	e := newBindOwnerEpoch(t, tx, newScriptedProbe(), parent)
	cancelParent()
	err := e.Start(context.Background(), terminal.CutInitial)
	if !errors.Is(err, terminal.ErrClosed) || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled parent Start err=%v", err)
	}
	if err != e.Err() {
		t.Fatalf("Start err=%v want immutable terminal err=%v", err, e.Err())
	}
	select {
	case <-e.Done():
	default:
		t.Fatal("pre-canceled parent Start did not synchronously terminalize")
	}
	actions, _, _, _ := tx.snapshot()
	if len(actions) != 0 {
		t.Fatalf("pre-canceled parent Start issued pinned actions: %v", actions)
	}
	if err := e.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBindOwnerParentCancelBeforeStartDoesNotBind(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	tx := &fakeTransaction{}
	e := newBindOwnerEpoch(t, tx, newScriptedProbe(), parent)
	cancelParent()
	select {
	case <-e.Done():
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not terminalize")
	}
	if err := e.Start(context.Background(), terminal.CutInitial); !errors.Is(err, terminal.ErrClosed) {
		t.Fatalf("Start err=%v", err)
	}
	if err := e.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	actions, _, _, _ := tx.snapshot()
	if len(actions) != 0 {
		t.Fatalf("cancellation before Start issued transactions: %v", actions)
	}
}
