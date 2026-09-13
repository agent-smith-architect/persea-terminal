package unifiedjournal

import (
	"sync/atomic"
	"time"
)

// VerificationProgress is content-free diagnostic state. It is an approximate
// sample of independent counters, never commit authority. Reading it does not
// require the caller's journal lock and cannot wait for storage to finish.
type VerificationProgress struct {
	StartedUnixNano, FinishedUnixNano int64
	InFlight, Attempts, Failures      uint64
	ReadBytes, DecodedRecords         uint64
	LastDurationNS                    int64
}

type verificationProgress struct {
	started, finished, duration  atomic.Int64
	inFlight, attempts, failures atomic.Uint64
	readBytes, decodedRecords    atomic.Uint64
}

func (p *verificationProgress) begin() time.Time {
	now := time.Now()
	p.started.Store(now.UnixNano())
	p.inFlight.Store(1)
	p.attempts.Add(1)
	return now
}

func (p *verificationProgress) finish(start time.Time, failed bool) {
	if failed {
		p.failures.Add(1)
	}
	p.duration.Store(time.Since(start).Nanoseconds())
	p.finished.Store(time.Now().UnixNano())
	p.inFlight.Store(0)
}

// VerificationProgress reports live-commit readback work, including the first
// header verification. DecodedRecords counts new append frames from successful
// verifications; a failed verification contributes no partial record count.
// Recovery parsing is deliberately excluded so comparative runs measure the
// same ordinary commit boundary before and after incremental verification.
func (realm *Realm) VerificationProgress() VerificationProgress {
	p := &realm.verification
	return VerificationProgress{
		StartedUnixNano: p.started.Load(), FinishedUnixNano: p.finished.Load(),
		InFlight: p.inFlight.Load(), Attempts: p.attempts.Load(), Failures: p.failures.Load(),
		ReadBytes: p.readBytes.Load(), DecodedRecords: p.decodedRecords.Load(),
		LastDurationNS: p.duration.Load(),
	}
}
