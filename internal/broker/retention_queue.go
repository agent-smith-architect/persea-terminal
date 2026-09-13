package broker

// retentionCommandQueue is owned by retentionTrialRuntime.mu. Its backing
// array never grows. Ordinary queued commands consume Q reservations; each
// live generation funds one cleanup command, and Close owns one marker. A
// control run (keyless diagnostics and a shared fence) can occur only between
// those commands and at the ends. Thus 2*(Q+P+1)+1 cells cover every producer,
// including generation reuse while earlier envelopes remain in the outbox.
type retentionCommandQueue struct {
	cells       []*retentionCommand
	head, count int
	cellWrites  uint64
}

func newRetentionCommandQueue(commands, generations int) retentionCommandQueue {
	return retentionCommandQueue{cells: make([]*retentionCommand, 2*(commands+generations+1)+1)}
}

func (command *retentionCommand) controlRun() bool {
	return command != nil && (command.kind == retentionCommandRejectedOutput || command.kind == retentionCommandDispatchFence)
}

func (queue *retentionCommandQueue) len() int { return queue.count }

func (queue *retentionCommandQueue) front() *retentionCommand {
	if queue.count == 0 {
		return nil
	}
	return queue.cells[queue.head]
}

func (queue *retentionCommandQueue) back() *retentionCommand {
	if queue.count == 0 {
		return nil
	}
	return queue.cells[(queue.head+queue.count-1)%len(queue.cells)]
}

func (queue *retentionCommandQueue) push(command *retentionCommand) bool {
	if queue.count == len(queue.cells) {
		return false
	}
	queue.cells[(queue.head+queue.count)%len(queue.cells)] = command
	queue.cellWrites++
	queue.count++
	return true
}

func (queue *retentionCommandQueue) pop() *retentionCommand {
	command := queue.front()
	if command == nil {
		return nil
	}
	queue.cells[queue.head] = nil
	queue.cellWrites++
	queue.head = (queue.head + 1) % len(queue.cells)
	queue.count--
	return command
}

func (runtime *retentionTrialRuntime) pushCommandLocked(command *retentionCommand) bool {
	if len(runtime.queue.cells) == 0 {
		runtime.queue = newRetentionCommandQueue(runtime.options.maxCommands, runtime.options.maxPanes)
	}
	if !runtime.queue.push(command) {
		return false
	}
	runtime.nextTicket++
	command.ticket = runtime.nextTicket
	runtime.recordAcceptedLocked(command)
	return true
}
