package broker

import (
	"bytes"
	"testing"

	"persea-terminal/internal/controlmode"
)

func TestRecordingFullReadersLeaveMaximumRecordAndCaptureHeadroom(t *testing.T) {
	shared := &recordingCopyBudget{}
	readers := recordingReaderBudget{copies: shared}
	transients := recordingTransientBudget{copies: shared}
	// Saturate the reader account with actual independently held snapshot
	// copies. The reserved PREPARE/writer floor stays part of the same lease.
	var leases []*recordingReaderLease
	var snapshots [][]byte
	for i := 0; i < 8; i++ {
		fixed := int64(recordingWriterBytes + recordingReaderFloor + recordingPrepareBytes)
		size := int64(recordingReaderBytes/8) - fixed
		lease, err := readers.acquire(size)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
		snapshots = append(snapshots, bytes.Repeat([]byte{'s'}, int(size)))
	}
	if readers.snapshot().Bytes != recordingReaderBytes {
		t.Fatal("reader saturation did not reach declared capacity", readers.snapshot())
	}
	var units []*recordingUnitMemory
	for i := 0; i < recordingObserverUnitLimit; i++ {
		owner, err := transients.acquire()
		if err != nil {
			t.Fatal(err)
		}
		units = append(units, owner)
	}
	decoder := controlmode.NewBudgetedDecoder(units[0])
	var batch controlmode.EventBatch
	chunk := bytes.Repeat([]byte{'x'}, 64<<10)
	prefix := []byte("%output %1 ")
	if _, err := batch.Feed(decoder, prefix); err != nil {
		t.Fatal(err)
	}
	remaining := (16 << 20) - len(prefix)
	for remaining > 0 {
		n := min(remaining, len(chunk))
		if _, err := batch.Feed(decoder, chunk[:n]); err != nil {
			t.Fatal(err)
		}
		remaining -= n
	}
	if _, err := batch.Feed(decoder, []byte{'\n'}); err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 1 || len(batch.Events[0].Data) != (16<<20)-len(prefix) {
		t.Fatal("valid maximum record lost")
	}
	// Keep that batch while a second unit owns the full bootstrap and a
	// rotation continuation; these are existing consumer lifetimes.
	birth := &unifiedDevBirth{memory: units[1]}
	if err := birth.appendPendingLocked(bytes.Repeat([]byte{'b'}, 2<<20)); err != nil {
		t.Fatal(err)
	}
	rotation := &unifiedRotationPending{memory: units[1]}
	if err := rotation.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Data: bytes.Repeat([]byte{'r'}, 1<<20)}); err != nil {
		t.Fatal(err)
	}
	response := recordingResponse{owner: units[1]}
	if err := response.Write(bytes.Repeat([]byte{'c'}, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if _, err := readers.acquire(0); err != errRecordingReaders {
		t.Fatal("reader overflow was not explicit", err)
	}
	t.Logf("reader_bytes=%d transient_peak=%d transient_held=%d shared_peak=%d recording_margin=%d", readers.bytes, transients.peak, transients.bytes, shared.peak, recordingCopyBytes-recordingReaderBytes)
	response.release()
	rotation.release()
	birth.clearPendingLocked()
	batch.Release()
	decoder.Release()
	for _, owner := range units {
		owner.done()
	}
	for i, lease := range leases {
		snapshots[i] = nil
		lease.releaseSnapshot()
		lease.detach()
	}
	if shared.bytes != 0 || readers.bytes != 0 || transients.bytes != 0 {
		t.Fatal("shared owner refund did not settle", shared.bytes)
	}
}
