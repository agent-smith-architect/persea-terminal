// Unified observer units, control commands, and event handling.
package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/unifiedjournal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

type unifiedDevCommand struct {
	spawnDeadline time.Time // only queued founding admission; never decoder cancellation
	// A founding caller transfers this existing owner with its spawn request.
	// It covers preflight command children before the observer process exists.
	memory *recordingUnitMemory
	// Runs on the decoder owner before its response storage is released.
	consumeResponse func(string) error
	context         context.Context
	birthCapture    *unifiedDevBirth
	server          config.TmuxServer
	args            []string
	birth           *unifiedDevBirth
	// adoption marks a founding request that attaches to an EXISTING session
	// instead of creating one. It mirrors the birth field: exactly one of the
	// two is set on a founding request, and the unit goroutine owns the whole
	// adoption sequence just as it owns birth.
	adoption *unifiedDevAdoption
	// rotation is a maintenance request executed on this session's existing
	// observer unit. It owns no second decoder or control client: the unit
	// goroutine consumes the capture boundary and every surrounding pane byte.
	rotation *unifiedDevRotation
	// blocks is how many %begin/%end command blocks this command occupies on the
	// control connection. Session birth occupies one. A guarded if-shell occupies
	// two: the submission, and the deferred success command tmux queues once the
	// guard passes — proven against real tmux 3.4 in the Phase-1 boundary test.
	// The completion of the LAST block is the sequence point; the first is not,
	// because pane output classified after it can still be pre-mutation output.
	blocks int
	done   chan unifiedDevCommandResult
}

type unifiedDevCommandResult struct {
	session string
	key     unifiedjournal.PaneKey
	trimmed bool
	// existing marks the idempotent adoption outcome: the session was already
	// journal-active and no new generation was created.
	existing bool
	err      error
}

// unifiedDevUnit is one per-session control-mode observer. Measured on tmux
// 3.4 (evidence: 2026-08-19_controlmode_output_reach): a control client
// receives %output ONLY for the session it is attached to, so observing a
// session requires a client of its own. Each unit owns its control-client
// process, PTY, decoder feed, read loop, and command channel; the session it
// observes is the session its startup command created, which is why it owns
// byte zero by construction.
type unifiedDevUnit struct {
	supervisorFault atomic.Bool
	supervisorStage atomic.Uint32
	supervisorKey   atomic.Pointer[unifiedjournal.PaneKey]
	sessionIdentity string // effects.mu; available before completed-loop fields
	memory          *recordingUnitMemory
	progress        observerProgress
	owner           *UnifiedDevPaneEffects
	// generation is the ControlGeneration this unit stamps on every witness it
	// builds. Birth units keep the historical generation 1; adoption units must
	// mint fresh (see mintControlGeneration).
	generation uint64
	ptmx       *os.File
	process    *exec.Cmd
	birth      unifiedDevCommand
	commands   chan unifiedDevCommand
	// done closes when the unit's loop has exited; command submitters select on
	// it so a dead unit refuses instead of swallowing requests.
	done       chan struct{}
	readFailed atomic.Bool
	reapOnce   sync.Once
	exitErr    error

	// Unit-goroutine state. The supervisor reads these only after receiving the
	// unit on the exits channel, which orders the memory.
	sessionID string
	holder    *unifiedDevBirth
	witnesses []controlmode.PaneWitness
	delivered bool
	ready     bool
}

// run drives one unit to completion and hands the carcass to the supervisor.
// The founding request is always answered exactly once, even when the unit
// dies before its startup block completes.
func (unit *unifiedDevUnit) run(ctx context.Context) {
	defer unit.memory.done()
	err := unit.serve(ctx)
	if unit.supervisorFault.Load() && err == nil {
		err = errRecordingStalled
	}
	unit.progress.recordExit(err)
	unit.exitErr = err
	if !unit.delivered {
		if err == nil {
			err = errors.New("unified session birth observer unavailable")
		}
		unit.birth.done <- unifiedDevCommandResult{err: err}
	}
	close(unit.done)
	unit.owner.recordUnitExit(unit)
	// The unit is over, so its control client dies with it. Closing the PTY
	// master is not enough: a client that is still healthily attached — an
	// adoption refused after a successful attach is the common case — survives
	// the close, and waiting on it would strand this carcass forever and with
	// it the reap that releases the session for the next attempt.
	if unit.process.Process != nil {
		_ = unit.process.Process.Kill()
	}
	_ = unit.process.Wait()
	unit.owner.mu.Lock()
	delete(unit.owner.supervised, unit)
	unit.owner.mu.Unlock()
	select {
	case unit.owner.exits <- unit:
	case <-ctx.Done():
		unit.reapOnce.Do(func() { unit.memory.done() })
	}
}

func (unit *unifiedDevUnit) serve(ctx context.Context) error {
	defer unit.ptmx.Close()
	read := make(chan []byte, 16)
	readErr := make(chan error, 1)
	unit.memory.hold()
	go func() {
		defer unit.memory.done()
		unifiedDevReadLoop(unit.ptmx, read, readErr, unit.done, &unit.readFailed)
	}()
	decoder := controlmode.NewBudgetedDecoder(unit.memory)
	defer decoder.Release()
	if unit.birth.adoption != nil {
		unit.progress.setStage(observerStageAttach)
		if err := unit.finishAdoptionAttach(ctx, decoder, read, readErr); err != nil {
			return err
		}
		unit.progress.setStage(observerStageReady)
		if err := unit.awaitReady(ctx, decoder, read, readErr); err != nil {
			return err
		}
		if edge := unit.owner.observerReadinessEdge; edge != nil {
			if err := edge(unit.sessionID); err != nil {
				return err
			}
		}
		unit.progress.setStage(observerStageFlags)
		if err := unit.verifyObserverClientFlags(ctx, decoder, read, readErr); err != nil {
			return err
		}
		if recovery := unit.birth.adoption.recovery; recovery != nil {
			unit.progress.setStage(observerStageRotation)
			if err := unit.runRotation(ctx, decoder, read, readErr, recovery); err != nil {
				return err
			}
			unit.delivered = true
			unit.owner.mu.Lock()
			delete(unit.owner.adopting, unit.sessionID)
			unit.owner.mu.Unlock()
		} else {
			unit.progress.setStage(observerStageAdoption)
			if err := unit.runAdoption(ctx, decoder, read, readErr); err != nil {
				return err
			}
		}
	} else {
		unit.progress.setStage(observerStageBirth)
		if err := unit.finishBirth(ctx, decoder, read, readErr); err != nil {
			return err
		}
		unit.progress.setStage(observerStageReady)
		if err := unit.awaitReady(ctx, decoder, read, readErr); err != nil {
			return err
		}
		if edge := unit.owner.observerReadinessEdge; edge != nil {
			if err := edge(unit.sessionID); err != nil {
				return err
			}
		}
		unit.progress.setStage(observerStageFlags)
		if err := unit.verifyObserverClientFlags(ctx, decoder, read, readErr); err != nil {
			return err
		}
	}
	if unit.birth.birth != nil {
		birth := unit.birth.birth
		birth.mu.Lock()
		birth.observerVerified = true
		birth.readyOnce.Do(func() { close(birth.ready) })
		birth.mu.Unlock()
	}
	for {
		unit.progress.setStage(observerStageStream)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			unit.progress.setStage(observerStageRead)
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				return ctx.Err()
			}
			return fmt.Errorf("unified control observer: %w", err)
		case request := <-unit.commands:
			unit.progress.setStage(observerStageCommand)
			if request.server.Label != unit.owner.server.Label {
				request.done <- unifiedDevCommandResult{err: fmt.Errorf("%w: foreign unified server", errGeometryNotIssued)}
				continue
			}
			if request.birthCapture != nil {
				err := unit.captureBirthInitial(ctx, decoder, read, readErr, request)
				request.done <- unifiedDevCommandResult{err: err}
				if err != nil {
					return err
				}
				continue
			}
			if request.rotation != nil {
				unit.progress.setStage(observerStageRotation)
				err := unit.runRotation(ctx, decoder, read, readErr, request.rotation)
				request.done <- unifiedDevCommandResult{err: err}
				if errors.Is(err, ErrUnifiedRotateFatal) || errors.Is(err, ErrUnifiedObserverFlowControl) {
					return err
				}
				continue
			}
			line := make([]string, len(request.args))
			for index, argument := range request.args {
				line[index] = shellQuote(argument)
			}
			if _, err := io.WriteString(unit.ptmx, strings.Join(line, " ")+"\n"); err != nil {
				request.done <- unifiedDevCommandResult{err: err}
				continue
			}
			if err := unit.finishCommand(ctx, decoder, read, readErr, request); err != nil {
				request.done <- unifiedDevCommandResult{err: err}
			}
		case chunk := <-read:
			unit.progress.setStage(observerStageDecode)
			if err := unit.owner.consumeObserverEvents(decoder, chunk); err != nil {
				return err
			}
		}
	}
}

// unifiedDevReadLoop normalizes the control PTY's CR-LF line endings into the
// byte stream the decoder consumes. It stops feeding once the unit is done, so
// a dead unit cannot strand this goroutine on a full channel.
func unifiedDevReadLoop(ptmx *os.File, read chan<- []byte, readErr chan<- error, done <-chan struct{}, failed *atomic.Bool) {
	buffer := make([]byte, 64<<10)
	pendingCR := false
	for {
		n, err := ptmx.Read(buffer)
		if err != nil {
			// Transport health is independent of consuming the error channel.
			// A ready manager result cannot outrun a known observer read failure.
			failed.Store(true)
		}
		if n > 0 {
			chunk := make([]byte, 0, n+1)
			for _, value := range buffer[:n] {
				if pendingCR {
					if value == '\n' {
						chunk = append(chunk, '\n')
						pendingCR = false
						continue
					}
					chunk = append(chunk, '\r')
					pendingCR = false
				}
				if value == '\r' {
					pendingCR = true
				} else {
					chunk = append(chunk, value)
				}
			}
			if len(chunk) != 0 {
				select {
				case read <- chunk:
				case <-done:
					return
				}
			}
		}
		if err != nil {
			readErr <- err
			return
		}
	}
}

func unifiedDevDisableEcho(file *os.File) error {
	var state syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&state)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	state.Lflag &^= syscall.ECHO
	_, _, errno = syscall.Syscall6(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&state)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// finishBirth consumes the unit's startup command block. Because the startup
// command IS the session creation, tmux wraps the printed session identity in
// the connection's first %begin/%end block (measured: n3_born.txt), and any
// pane output can only follow that block on this same stream.
func (unit *unifiedDevUnit) finishBirth(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error) error {
	var batch controlmode.EventBatch
	defer batch.Release()
	effects := unit.owner
	request := unit.birth
	response := recordingResponse{owner: unit.memory}
	defer response.release()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case chunk := <-read:
			events, err := batch.Feed(decoder, chunk)
			if err != nil {
				return fmt.Errorf("decode unified create response: %w", err)
			}
			for index, event := range events {
				switch event.Kind {
				case controlmode.EventCommandResponse:
					if err := response.Write(event.Data); err != nil {
						return err
					}
				case controlmode.EventCommandError:
					// The rest of the decoded chunk still carries this
					// connection's live events; dropping it would silently
					// lose pane bytes, so route it onward before failing.
					for _, remainder := range events[index+1:] {
						if err := effects.consumeObserverEvent(remainder); err != nil {
							return err
						}
					}
					return errors.New("tmux rejected unified session birth")
				case controlmode.EventCommandEnd:
					sessionID := strings.Clone(strings.TrimSpace(response.String()))
					if !validSessionID(sessionID) {
						return errors.New("unified session birth returned invalid identity")
					}
					source, err := buildSourceWitness(ctx, effects.server, "", sessionID)
					if err != nil {
						return err
					}
					witness := controlmode.PaneWitness{
						Session: controlmode.SessionWitness{Server: effects.server.Label, Session: sessionID, ControlGeneration: unit.generation},
						Window:  source.WindowID, Pane: source.PaneID, Incarnation: source.Incarnation,
					}
					// The generation's birth geometry is durable truth from byte
					// zero. Without it the journal is byte-only and can never serve
					// unified replay, so admission happens before the pane is
					// observable, not after its first output.
					if err := effects.reserveJournalSource(journalKey(witness), source); err != nil {
						return err
					}
					effects.journalMu.Lock()
					admitErr := effects.realm.AdmitPane(journalKey(witness), unifiedjournal.Geometry{Columns: source.Columns, Rows: source.Rows})
					effects.journalMu.Unlock()
					if admitErr != nil {
						effects.realm.CancelSourceReservation(journalKey(witness))
						return admitErr
					}
					if err := effects.observer.AdmitPane(witness); err != nil {
						return err
					}
					request.birth.mu.Lock()
					request.birth.witness = witness
					request.birth.source = source
					request.birth.geometry = unifiedjournal.Geometry{Columns: source.Columns, Rows: source.Rows}
					request.birth.mu.Unlock()
					unit.sessionID = sessionID
					unit.holder = request.birth
					unit.witnesses = append(unit.witnesses, witness)
					effects.mu.Lock()
					effects.panes[witness.Pane] = request.birth
					effects.units[sessionID] = unit
					unit.sessionIdentity = sessionID
					effects.mu.Unlock()
					unit.delivered = true
					request.done <- unifiedDevCommandResult{session: sessionID}
					// The readiness notification can share the decoded chunk
					// with the creation's own %end, so the remainder must be
					// consumed through the readiness watch or it is lost.
					for _, remainder := range events[index+1:] {
						if err := unit.consumeReadyEvent(remainder); err != nil {
							return err
						}
					}
					return nil
				default:
					if err := effects.consumeObserverEvent(event); err != nil {
						return err
					}
				}
			}
		}
	}
}

// consumeReadyEvent routes one decoded event onward while watching for the
// unit's readiness notification: an exact match on the $id field of
// %session-changed, never a name substring, which would let one session's
// readiness satisfy another's ("main" matching "main2"). Everything else keeps
// its decoded order through the routing map, so byte-zero output cannot be
// dropped while readiness is pending.
func (unit *unifiedDevUnit) consumeReadyEvent(event controlmode.Event) error {
	if !unit.ready && unit.sessionID != "" && event.Kind == controlmode.EventNotification && event.Name == "session-changed" {
		fields := strings.Fields(event.Args)
		if len(fields) != 0 && fields[0] == unit.sessionID {
			unit.ready = true
			return nil
		}
	}
	return unit.owner.consumeObserverEvent(event)
}

// awaitReady blocks until the readiness watch has seen the observed session's
// own %session-changed, which may already have arrived inside the creation
// chunk consumed by finishBirth.
func (unit *unifiedDevUnit) awaitReady(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error) error {
	var batch controlmode.EventBatch
	defer batch.Release()
	for {
		if unit.ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case chunk := <-read:
			events, err := batch.Feed(decoder, chunk)
			if err != nil {
				return fmt.Errorf("decode unified observer bootstrap: %w", err)
			}
			for _, event := range events {
				if err := unit.consumeReadyEvent(event); err != nil {
					return err
				}
			}
		}
	}
}

// verifyObserverClientFlags interrogates this unit's own control client on its
// ordered stream after readiness and before any reconstructed capture. tmux's
// pause-after and no-output flags are additive and another same-server client
// may set them at any time, so this is an early rejection gate rather than a
// lasting proof. The adoption composite repeats the check at its boundary.
func (unit *unifiedDevUnit) verifyObserverClientFlags(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error) error {
	request := unifiedDevCommand{
		args:            []string{"list-clients", "-F", "#{client_pid}|#{client_flags}"},
		consumeResponse: func(raw string) error { return rejectObserverClientInventory(raw, unit.process.Process.Pid) },
		blocks:          1,
		done:            make(chan unifiedDevCommandResult, 1),
	}
	line := make([]string, len(request.args))
	for index, argument := range request.args {
		line[index] = shellQuote(argument)
	}
	if _, err := io.WriteString(unit.ptmx, strings.Join(line, " ")+"\n"); err != nil {
		return err
	}
	if err := unit.finishCommand(ctx, decoder, read, readErr, request); err != nil {
		return err
	}
	return (<-request.done).err
}

func rejectObserverClientInventory(raw string, observerPID int) error {
	matches := 0
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "|", 2)
		if len(fields) != 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid != observerPID {
			continue
		}
		matches++
		if err := rejectObserverClientFlags(fields[1]); err != nil {
			return err
		}
	}
	if matches != 1 {
		return fmt.Errorf("%w: observer client identity count %d", ErrUnifiedObserverFlowControl, matches)
	}
	return nil
}

func rejectObserverClientFlags(raw string) error {
	flags := strings.TrimSpace(raw)
	if strings.ContainsAny(flags, " \t\r\n") {
		return fmt.Errorf("%w: malformed client flags", ErrUnifiedObserverFlowControl)
	}
	for _, flag := range strings.Split(flags, ",") {
		if flag == "no-output" || flag == "pause-after" || strings.HasPrefix(flag, "pause-after=") {
			return fmt.Errorf("%w: dangerous client flag %s", ErrUnifiedObserverFlowControl, flag)
		}
	}
	return nil
}

// finishCommand consumes an already-issued command's blocks on the unit's own
// control connection, using the same discipline as finishBirth: the command
// response accumulates and is never dispatched as pane output, and every other
// event keeps its decoded order, including the remainder of the chunk that
// carried the final %end or a rejection.
//
// A refused guard is not distinguishable here. tmux reports a guarded if-shell
// whose guard evaluates false as an ordinary %end with an empty response, so
// this returns success and the caller's exact witness recheck is the only
// rejection detector. That is a property of tmux, established by the Phase-1
// proof, not an assumption.
func (unit *unifiedDevUnit) finishCommand(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error, request unifiedDevCommand) error {
	var batch controlmode.EventBatch
	defer batch.Release()
	remaining := request.blocks
	if remaining < 1 {
		return errors.New("unified observer command requires a block count")
	}
	response := recordingResponse{owner: unit.memory}
	defer response.release()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case chunk := <-read:
			events, err := batch.Feed(decoder, chunk)
			if err != nil {
				return fmt.Errorf("decode unified observer command: %w", err)
			}
			for index, event := range events {
				switch event.Kind {
				case controlmode.EventCommandResponse:
					if err := response.Write(event.Data); err != nil {
						return err
					}
				case controlmode.EventCommandError:
					// Same-connection live pane bytes can share the decoded
					// chunk with a rejection; consume the remainder before
					// returning so none of them are silently dropped.
					for _, rest := range events[index+1:] {
						if err := unit.owner.consumeObserverEvent(rest); err != nil {
							return err
						}
					}
					return errors.New("tmux rejected a unified observer command")
				case controlmode.EventCommandEnd:
					remaining--
					if remaining != 0 {
						continue
					}
					if request.consumeResponse != nil {
						if err := request.consumeResponse(response.String()); err != nil {
							return err
						}
					}
					request.done <- unifiedDevCommandResult{}
					for _, rest := range events[index+1:] {
						if err := unit.owner.consumeObserverEvent(rest); err != nil {
							return err
						}
					}
					return nil
				default:
					if err := unit.owner.consumeObserverEvent(event); err != nil {
						return err
					}
				}
			}
		}
	}
}

func (effects *UnifiedDevPaneEffects) consumeObserverEvents(decoder *controlmode.Decoder, chunk []byte) error {
	var batch controlmode.EventBatch
	defer batch.Release()
	events, err := batch.Feed(decoder, chunk)
	if err != nil {
		return fmt.Errorf("decode unified observer control: %w", err)
	}
	for _, event := range events {
		if err := effects.consumeObserverEvent(event); err != nil {
			return err
		}
	}
	return nil
}

// observerFlowControlFault is the closed signal tripwire for the delivery-mode
// assumption. Persea never requests tmux flow control on an observer unit, so
// pause, continue, or any delayed extended-output proves that byte delivery is
// no longer the ordered ordinary stream the journal is authorized to record.
// pane:off and no-output themselves emit no reliable signal; readiness flag
// interrogation and adoption's pane:on normalization are their separate,
// necessarily point-in-time coverage.
func observerFlowControlFault(event controlmode.Event) error {
	switch event.Kind {
	case controlmode.EventPause:
		return fmt.Errorf("%w: %%pause", ErrUnifiedObserverFlowControl)
	case controlmode.EventContinue:
		return fmt.Errorf("%w: %%continue", ErrUnifiedObserverFlowControl)
	case controlmode.EventExtendedOutput:
		return fmt.Errorf("%w: %%extended-output", ErrUnifiedObserverFlowControl)
	default:
		return nil
	}
}

func (effects *UnifiedDevPaneEffects) consumeObserverEvent(event controlmode.Event) error {
	if effects.observerDecoded != nil {
		effects.observerDecoded(event)
	}
	if err := observerFlowControlFault(event); err != nil {
		return err
	}
	if event.Kind != controlmode.EventOutput {
		return nil
	}
	effects.mu.Lock()
	birth := effects.panes[event.PaneID]
	effects.mu.Unlock()
	if birth == nil {
		return nil
	}
	return birth.observe(event.Data)
}
