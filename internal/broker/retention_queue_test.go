package broker

import (
	"sync"
	"testing"
	"time"
)

func queuedRetentionCommands(queue *retentionCommandQueue) []*retentionCommand {
	commands := make([]*retentionCommand, queue.count)
	for i := range commands {
		commands[i] = queue.cells[(queue.head+i)%len(queue.cells)]
	}
	return commands
}

func TestRetentionControlRunsBoundAlternatingRejectionsAndFundedSeparators(t *testing.T) {
	runtime := sourceReservationRuntime()
	runtime.options.sourceRealm = ""
	runtime.options.maxCommands = 16
	runtime.options.maxPanes = 2
	runtime.queue = newRetentionCommandQueue(16, 2)
	witness := retentionWitness("%bounded", "bounded")
	key := journalKey(witness)
	var expected int
	for {
		first, err := runtime.startDispatchFence()
		if err != nil {
			t.Fatal(err)
		}
		for failedSource := 0; failedSource < 1000; failedSource++ {
			bytes := 1 + failedSource%2
			runtime.publishRejectedOutput(bytes)
			expected += bytes
			next, err := runtime.startDispatchFence()
			if err != nil || next != first {
				t.Fatal("adjacent control run created another fence owner")
			}
		}
		reservation, err := runtime.reserve(key, 0)
		if err != nil {
			break
		}
		if err := runtime.enqueue(&retentionCommand{kind: retentionCommandBoundary, key: key, reservation: reservation}); err != nil {
			t.Fatal(err)
		}
	}
	// All ordinary Q is spent; funded cleanup and the final lifecycle fence
	// still fit. The runtime's P/Q credits own cleanup until it is processed.
	runtime.appendCleanup(&retentionCommand{kind: retentionCommandCleanup, key: key})
	if _, err := runtime.startDispatchFence(); err != nil {
		t.Fatal(err)
	}
	if !runtime.pushCommandLocked(&retentionCommand{kind: retentionCommandClose}) {
		t.Fatal("close reserve unavailable at saturation")
	}
	var discarded int
	var previousTicket uint64
	for _, command := range queuedRetentionCommands(&runtime.queue) {
		if command.ticket <= previousTicket || command.acceptedUnixNano == 0 {
			t.Fatal("FIFO ticket/age lost")
		}
		previousTicket = command.ticket
		discarded += command.bytes
	}
	if discarded != expected || runtime.queue.len() > 2*(runtime.options.maxCommands+runtime.options.maxPanes+1)+1 {
		t.Fatalf("diagnostic total/bound: got=%d want=%d depth=%d", discarded, expected, runtime.queue.len())
	}
	t.Logf("alternating rejected bytes=%d queue=%d fixed capacity=%d", discarded, runtime.queue.len(), len(runtime.queue.cells))
}

func TestRetentionHeldFenceChurnHasBoundedOutboxOwnership(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	effects := &retentionShadowEffects{realm: openRetentionRealm(t, "fence-churn"), maxBytes: 64 << 10, maxDelay: 16 * time.Millisecond, clock: newRetentionManualClock(), maxPanes: 2, maxCommands: 16, maxEnvelopes: 64, maxIngressBytes: 64 << 10, maxOutboxBytes: 64 << 10,
		beforeObserve: func(event string) {
			if event == "discarded_after_fault" {
				once.Do(func() { close(entered); <-release })
			}
		},
	}
	runtime := newRetentionTrialRuntime(effects)
	if runtime == nil {
		t.Fatal("invalid fixture limits")
	}
	defer func() { unblock(); _ = runtime.Close() }()
	runtime.publishRejectedOutput(1)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher was not held")
	}
	fences := make(map[<-chan struct{}]struct{})
	for i := 0; i < 10000; i++ {
		key := journalKey(retentionWitness("%churn", "churn"))
		key.ControlGeneration = uint64(i + 1)
		if _, _, err := runtime.ensureGeneration(key, false); err != nil {
			t.Fatal(err)
		}
		runtime.requestRetire(key)
		runtime.publishRejectedOutput(1 + i%2)
		fence, err := runtime.startDispatchFence()
		if err != nil {
			t.Fatal(err)
		}
		fences[fence] = struct{}{}
	}
	runtime.mu.Lock()
	depth, generations := runtime.queue.len(), runtime.pUsed
	runtime.mu.Unlock()
	// Each dequeued control run can own at most two envelopes. With the
	// dispatcher held, the fixed outbox plus one manager run and one queued
	// run bound every distinct completion channel despite generation reuse.
	if generations != 0 || depth > 1 || len(fences) > cap(runtime.outbox)+2 {
		t.Fatalf("churn retained unbounded ownership: P=%d queue=%d fences=%d outbox=%d", generations, depth, len(fences), cap(runtime.outbox))
	}
	unblock()
	final, err := runtime.startDispatchFence()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-final:
	case <-time.After(2 * time.Second):
		t.Fatal("fence did not settle")
	}
	effects.mu.Lock()
	var total int64
	for i, event := range effects.observations {
		if event == "discarded_after_fault" {
			total += effects.fields[i]["discarded_bytes"]
		}
	}
	effects.mu.Unlock()
	if total != 15001 {
		t.Fatalf("coalescing lost rejected bytes: %d", total)
	}
	t.Logf("generation churn=10000 distinct pending fences=%d outbox capacity=%d queued runs=%d", len(fences), cap(runtime.outbox), depth)
}

func TestRetentionQueueConstantWorkAndClearedOwnership(t *testing.T) {
	for _, count := range []int{1, 1024, 131072} {
		queue := newRetentionCommandQueue(count, 1)
		commands := make([]retentionCommand, count)
		for i := range commands {
			commands[i].ticket = uint64(i + 1)
			if !queue.push(&commands[i]) {
				t.Fatal("premature full queue")
			}
		}
		before := queue.cellWrites
		for i := range commands {
			if command := queue.pop(); command != &commands[i] {
				t.Fatalf("ticket %d: %#v", i+1, command)
			}
		}
		if queue.cellWrites-before != uint64(count) {
			t.Fatalf("dequeue cell writes=%d for %d commands", queue.cellWrites-before, count)
		}
		for _, command := range queue.cells {
			if command != nil {
				t.Fatal("consumed command retained")
			}
		}
		allocs := testing.AllocsPerRun(100, func() { queue.push(&commands[0]); queue.pop() })
		if allocs != 0 {
			t.Fatalf("steady queue allocations=%v", allocs)
		}
		t.Logf("depth=%d dequeue cell writes=%d allocations/op=%g", count, count, allocs)
	}
}
