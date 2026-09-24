package terminal

// InputAccountingSettledForTest observes the pump's completed accounting,
// rather than the writer callback that precedes it.
func (e *Epoch) InputAccountingSettledForTest() bool {
	e.input.mu.Lock()
	defer e.input.mu.Unlock()
	return e.input.bytes == 0
}
