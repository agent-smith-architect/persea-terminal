package broker

import (
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/unifiedjournal"
)

// Birth output is one contiguous initial state, bounded by the same 2 MiB
// contract as reconstructed initial state. Tiny reads do not create an
// unbounded Observation index while a birth caller is delayed.
func (birth *unifiedDevBirth) appendPendingLocked(data []byte) error {
	var payload []byte
	if len(birth.pending) != 0 {
		payload = birth.pending[0].Data
	}
	if len(data) > adoptionBootstrapCapBytes-len(payload) {
		return errRecordingTransients
	}
	want := len(payload) + len(data)
	if want > cap(payload) {
		capacity := max(64<<10, cap(payload)*2)
		for capacity < want {
			capacity *= 2
		}
		cost := unifiedjournal.AllocationCharge(int64(capacity)) + 256
		if !birth.memory.Reserve(cost) {
			return errRecordingTransients
		}
		next := make([]byte, len(payload), capacity)
		copy(next, payload)
		payload = next
		birth.memory.Release(birth.pendingCharge)
		birth.pendingCharge = cost
	}
	payload = append(payload, data...)
	if len(birth.pending) == 0 {
		birth.pending = []controlmode.Observation{{Kind: controlmode.ObservationOutput, Witness: birth.witness}}
	}
	birth.pending[0].Data = payload
	return nil
}

func (birth *unifiedDevBirth) clearPendingLocked() {
	birth.pending = nil
	birth.memory.Release(birth.pendingCharge)
	birth.pendingCharge = 0
}

func (pending *unifiedRotationPending) release() {
	if pending == nil {
		return
	}
	pending.data = nil
	pending.memory.Release(pending.charge)
	pending.charge = 0
}
