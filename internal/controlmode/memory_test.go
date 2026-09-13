package controlmode

import (
	"bytes"
	"errors"
	"testing"
)

type decoderMemory struct{ used, peak, limit int64 }

func (memory *decoderMemory) Reserve(n int64) bool {
	if n < 0 || n > memory.limit-memory.used {
		return false
	}
	memory.used += n
	memory.peak = max(memory.peak, memory.used)
	return true
}
func (memory *decoderMemory) Release(n int64) {
	if n < 0 || n > memory.used {
		panic("invalid release")
	}
	memory.used -= n
}

func TestRecordingDecoderMaximumRecordAndBatchTransfer(t *testing.T) {
	memory := &decoderMemory{limit: 128 << 20}
	decoder := NewBudgetedDecoder(memory)
	defer decoder.Release()
	input := append([]byte("%output %1 "), bytes.Repeat([]byte{'x'}, maxControlRecordBytes-len("%output %1 "))...)
	input = append(input, '\n')
	var batch EventBatch
	for len(input) > 0 {
		n := min(len(input), 64<<10)
		if _, err := batch.Feed(decoder, input[:n]); err != nil {
			t.Fatal(err)
		}
		input = input[n:]
	}
	if len(batch.Events) != 1 || len(batch.Events[0].Data) != maxControlRecordBytes-len("%output %1 ") {
		t.Fatal("maximum record changed")
	}
	held := batch.Take()
	before := memory.used
	if _, err := batch.Feed(decoder, []byte("%output %1 later\n")); err != nil {
		t.Fatal(err)
	}
	if memory.used <= before || held.Events[0].Data[0] != 'x' {
		t.Fatal("nested feed refunded retained batch")
	}
	batch.Release()
	if memory.used != before {
		t.Fatalf("nested batch retained %d extra", memory.used-before)
	}
	held.Release()
	if memory.used != 0 {
		t.Fatalf("settled bytes=%d", memory.used)
	}
	t.Logf("maximum-record peak=%d", memory.peak)
}

func TestRecordingDecoderRefusesBeforeUnfundedBatch(t *testing.T) {
	memory := &decoderMemory{limit: 8192}
	decoder := NewBudgetedDecoder(memory)
	var batch EventBatch
	if _, err := batch.Feed(decoder, []byte("%output %1 no-credit\n")); !errors.Is(err, ErrDecoderFailed) {
		t.Fatal(err)
	}
	if len(batch.Events) != 0 || memory.used != 0 || !decoder.Failed() {
		t.Fatalf("unfunded allocation retained: %+v", memory)
	}
	batch.Release()
}

func TestRecordingDecoderFailureKeepsAlreadyDecodedBatchOwned(t *testing.T) {
	memory := &decoderMemory{limit: 1 << 20}
	decoder := NewBudgetedDecoder(memory)
	var batch EventBatch
	if _, err := batch.Feed(decoder, []byte("%output %1 retained\ninvalid\n")); !errors.Is(err, ErrDecoderFailed) {
		t.Fatal(err)
	}
	decoder.Release()
	if memory.used == 0 || len(batch.Events) != 1 {
		t.Fatal("failed batch refunded before consumer settlement")
	}
	batch.Release()
	if memory.used != 0 {
		t.Fatal(memory.used)
	}
}
