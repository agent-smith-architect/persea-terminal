package controlmode

// Memory follows the decoder's buffer and its consumers' decoded batches.
// Reserve must reject before allocation and Release must not call the decoder.
type Memory interface {
	Reserve(int64) bool
	Release(int64)
}

// EventBatch owns one decoded read, including a suffix retained across a nested
// settlement read. Feed settles the preceding batch; Take transfers it instead.
// Consumers must finish every use before Release or the next Feed on this batch.
type EventBatch struct {
	Events []Event
	memory Memory
	bytes  int64
}

func (batch *EventBatch) Release() {
	batch.Events = nil
	if batch.memory != nil && batch.bytes != 0 {
		batch.memory.Release(batch.bytes)
	}
	batch.memory, batch.bytes = nil, 0
}

func (batch *EventBatch) Take() EventBatch {
	owned := *batch
	*batch = EventBatch{}
	return owned
}

func (batch *EventBatch) Feed(decoder *Decoder, input []byte) ([]Event, error) {
	batch.Release()
	return decoder.feed(input, batch)
}

func NewBudgetedDecoder(memory Memory) *Decoder { return &Decoder{memory: memory} }

// Release drops the partial record. It does not release batches still owned by
// consumers and does not change the parser's Close validation semantics.
func (decoder *Decoder) Release() {
	decoder.buffer = nil
	if decoder.memory != nil && decoder.bufferBytes != 0 {
		decoder.memory.Release(decoder.bufferBytes)
	}
	decoder.bufferBytes = 0
}

func roundedBytes(n int64) int64 {
	if n == 0 {
		return 0
	}
	return (n + 8191) &^ 8191
}
