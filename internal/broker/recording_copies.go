package broker

import "sync"

// Readers can use at most 128 MiB of this account, leaving 96 MiB for the
// observer's existing unit/decoder/capture owners. The observer may use spare
// reader capacity up to its own 128 MiB ceiling. There is no new lease: each
// original owner updates both scopes under this one mutex at the same edge.
const recordingCopyBytes = 224 << 20

type recordingCopyBudget struct {
	mu                 sync.Mutex
	bytes, peak, limit int64
}

func (budget *recordingCopyBudget) available(bytes int64) bool {
	if budget == nil {
		return true
	}
	limit := budget.limit
	if limit == 0 {
		limit = recordingCopyBytes
	}
	return bytes >= 0 && bytes <= limit-budget.bytes
}

func (budget *recordingCopyBudget) charge(bytes int64) {
	if budget == nil {
		return
	}
	budget.bytes += bytes
	budget.peak = max(budget.peak, budget.bytes)
	if budget.bytes < 0 {
		panic("recording: copy account underflow")
	}
}

func (budget *recordingReaderBudget) mutex() *sync.Mutex {
	if budget.copies != nil {
		return &budget.copies.mu
	}
	return &budget.mu
}

func (budget *recordingReaderBudget) charge(bytes int64) {
	budget.bytes += bytes
	budget.copies.charge(bytes)
}

func (budget *recordingTransientBudget) mutex() *sync.Mutex {
	if budget.copies != nil {
		return &budget.copies.mu
	}
	return &budget.mu
}

func (budget *recordingTransientBudget) charge(bytes int64) {
	budget.bytes += bytes
	budget.peak = max(budget.peak, budget.bytes)
	budget.copies.charge(bytes)
}
