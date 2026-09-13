package broker

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"persea-terminal/internal/config"
	"persea-terminal/internal/unifiedjournal"
)

const (
	recordingTransientBytes = 128 << 20
	// Eight supported sources plus the spare slot may each retain a previous
	// process/read owner while a replacement is admitted. Further founding
	// requests wait for actual settlement by receiving an explicit refusal.
	recordingObserverUnitLimit = 18
	// Read buffer, sixteen queued normalized chunks, producer and consumer
	// chunks (72 KiB each after rounding), plus unit/channel/stack allowance.
	recordingUnitBytes = 1536 << 10
)

var errRecordingTransients = errors.New("recording transient capacity exhausted")

// This is the account for existing observer and capture owners. It performs
// no I/O, callbacks or lifecycle transitions while locked.
type recordingTransientBudget struct {
	copies             *recordingCopyBudget
	mu                 sync.Mutex
	bytes, peak, limit int64
	units              int
}

type recordingUnitMemory struct {
	budget *recordingTransientBudget
	refs   int
	bytes  int64
}

func (budget *recordingTransientBudget) capacity() int64 {
	if budget.limit > 0 {
		return budget.limit
	}
	return recordingTransientBytes
}

func (budget *recordingTransientBudget) acquire() (*recordingUnitMemory, error) {
	budget.mutex().Lock()
	defer budget.mutex().Unlock()
	if budget.units >= recordingObserverUnitLimit || recordingUnitBytes > budget.capacity()-budget.bytes || !budget.copies.available(recordingUnitBytes) {
		return nil, errRecordingTransients
	}
	budget.charge(recordingUnitBytes)
	budget.units++
	return &recordingUnitMemory{budget: budget, refs: 1}, nil
}

func (owner *recordingUnitMemory) hold() {
	if owner == nil {
		return
	}
	owner.budget.mutex().Lock()
	defer owner.budget.mutex().Unlock()
	if owner.refs <= 0 {
		panic("recording: holding a settled unit")
	}
	owner.refs++
}

func (owner *recordingUnitMemory) done() {
	if owner == nil {
		return
	}
	owner.budget.mutex().Lock()
	defer owner.budget.mutex().Unlock()
	if owner.refs <= 0 {
		panic("recording: unit settled twice")
	}
	owner.refs--
	owner.releaseLocked()
}

func (owner *recordingUnitMemory) Reserve(bytes int64) bool {
	if owner == nil {
		return true
	} // synthetic protocol fixtures have no unit
	budget := owner.budget
	budget.mutex().Lock()
	defer budget.mutex().Unlock()
	if owner.refs <= 0 || bytes < 0 || bytes > budget.capacity()-budget.bytes || !budget.copies.available(bytes) {
		return false
	}
	owner.bytes += bytes
	budget.charge(bytes)
	return true
}

func (owner *recordingUnitMemory) Release(bytes int64) {
	if owner == nil {
		return
	}
	owner.budget.mutex().Lock()
	defer owner.budget.mutex().Unlock()
	if bytes < 0 || bytes > owner.bytes {
		panic("recording: transient release without owner")
	}
	owner.bytes -= bytes
	owner.budget.charge(-bytes)
	owner.releaseLocked()
}

func (owner *recordingUnitMemory) releaseLocked() {
	if owner.refs == 0 && owner.bytes == 0 {
		owner.refs = -1
		owner.budget.charge(-recordingUnitBytes)
		owner.budget.units--
	}
}

// A command owns all response blocks until parsing and the last dependent
// consumer finish. Reserving before Grow includes old/new builder overlap and
// a conservative parsing allowance (field/row indices, copied strings and
// the synthesized initial payload). Reset starts a block, not a refund.
type recordingResponse struct {
	owner   *recordingUnitMemory
	builder strings.Builder
	bytes   int64
}

func (response *recordingResponse) Write(data []byte) error {
	// Charging every appended byte also covers obsolete growth allocations
	// until the command finishes; it does not rely on a GC between appends.
	cost := 4*unifiedjournal.AllocationCharge(int64(len(data))) + 256
	// Splitting metadata/capture rows can create an index per whitespace
	// byte. Charge those indices before parsing as well.
	for remaining := data; len(remaining) > 0; {
		value, width := utf8.DecodeRune(remaining)
		remaining = remaining[width:]
		if unicode.IsSpace(value) {
			cost += 32
		}
	}
	if response.bytes == 0 {
		cost += 2 << 20
	}
	if !response.owner.Reserve(cost) {
		return errRecordingTransients
	}
	response.bytes += cost
	_, _ = response.builder.Write(data)
	return nil
}

func (response *recordingResponse) String() string { return response.builder.String() }
func (response *recordingResponse) Reset()         { response.builder.Reset() }
func (response *recordingResponse) release() {
	response.builder.Reset()
	response.owner.Release(response.bytes)
	response.bytes = 0
}

type recordingResponseSink struct{ response *recordingResponse }

func (sink *recordingResponseSink) Write(data []byte) (int, error) {
	if err := sink.response.Write(data); err != nil {
		return 0, err
	}
	return len(data), nil
}

// Run returns only after the process and output copier settle. Sharing the
// identical sink for stdout/stderr makes os/exec serialize those writes.
func recordingCommandOutput(ctx context.Context, server config.TmuxServer, response *recordingResponse, args ...string) error {
	argv := tmuxArgv(server, args...)
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	sink := &recordingResponseSink{response: response}
	command.Stdout, command.Stderr = sink, sink
	return command.Run()
}
