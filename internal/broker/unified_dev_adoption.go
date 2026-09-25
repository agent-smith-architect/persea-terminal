// Unified adoption capture, validation, and bootstrap synthesis.
package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/proto"
	"persea-terminal/internal/unifiedjournal"
	"strconv"
	"strings"
	"time"
)

// Adoption refusals are typed so the projection layer can map each one to a
// distinct dashboard state. Every refusal fails closed: when any of these is
// returned, no journal generation, registry admission, or provider registration
// exists for the attempt.
var (
	ErrUnifiedAdoptSessionMissing = errors.New("unified adoption target session is unavailable")
	ErrUnifiedAdoptMultiWindow    = errors.New("unified adoption target has more than one window")
	ErrUnifiedAdoptMultiPane      = errors.New("unified adoption target has more than one pane")
	ErrUnifiedAdoptSlotsExhausted = errors.New("unified adoption slots are exhausted")
	ErrUnifiedAdoptInProgress     = errors.New("unified adoption is already in progress")
	ErrUnifiedAdoptUnstable       = errors.New("unified adoption capture did not stabilize")
	ErrUnifiedAdoptHistory        = errors.New("unified adoption history is outside its bounds")
	ErrUnifiedObserverFlowControl = errors.New("unified observer flow control violated byte authority")
)

// errUnifiedAdoptDrift is the internal PRE!=POST signal: the pane mutated (or a
// future tmux stopped draining a client's command line atomically) during the
// capture composite, so the attempt's capture is discarded and retried. It is
// never returned to a caller; exhausted retries surface ErrUnifiedAdoptUnstable.
var errUnifiedAdoptDrift = errors.New("unified adoption capture drifted")

const (
	// adoptionHistoryCapRows bounds how much scrollback the capture composite
	// requests. Deeper history stays in tmux and is not reconstructed.
	adoptionHistoryCapRows = proto.AdoptionHistoryMaxRows
	// adoptionBootstrapCapBytes bounds the synthesized bootstrap; oldest
	// history rows are trimmed first, then hidden normal rows if needed.
	// The trim is surfaced to the caller.
	// The pane generation's whole lifetime is 8 MiB, so a full bootstrap
	// spends a quarter of it.
	adoptionBootstrapCapBytes = int(unifiedjournal.AdoptionBootstrapCapBytes)
	// adoptionCaptureAttempts is the initial capture plus the PRE!=POST
	// version-drift retries. Measured on tmux 3.4: the composite is atomic by
	// construction (zero %output inside the span under flood), so a retry
	// fires only if a future tmux breaks that property.
	adoptionCaptureAttempts = 4
	// adoptionCompositeBlocks is the composite's sub-command count. Measured:
	// each semicolon sub-command produces its own %begin/%end block, so the
	// block count consumed must equal the sub-command count exactly.
	adoptionCompositeBlocks = 8
)

// unifiedDevAdoption carries the identity of the session an adoption unit
// attaches to. Attachment is by session ID, never name: readiness
// exact-matches the $id field of %session-changed, and name targets are both
// ambiguous ("main" prefixing "main2") and subject to the measured
// `=name` targeting gotcha.
type unifiedDevAdoption struct {
	sessionID   string
	historyRows int
	recovery    *unifiedDevRotation
	holder      *unifiedDevBirth
	witnesses   []controlmode.PaneWitness
}

// UnifiedAdoption is the provider-level outcome of adopting an existing
// session into the unified journal.
type UnifiedAdoption struct {
	SessionID string
	Key       unifiedjournal.PaneKey
	// Trimmed reports that the reconstructed bootstrap dropped its oldest
	// history rows to fit the bootstrap byte cap; the projection layer can
	// surface a shallow reconstruction honestly.
	Trimmed bool
	// Existing marks the idempotent path: the session was already
	// journal-active and this call created nothing.
	Existing bool
}

// finishAdoptionAttach consumes the unit's startup attach block. The attach
// prints nothing; tmux reports an unavailable target as %error on this same
// block (measured: the message text arrives as the block's response line), and
// the attach is the block's only command, so %error here IS the typed
// missing-session refusal — no message parsing is needed or wanted.
func (unit *unifiedDevUnit) finishAdoptionAttach(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error) error {
	var batch controlmode.EventBatch
	defer batch.Release()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case chunk := <-read:
			events, err := batch.Feed(decoder, chunk)
			if err != nil {
				return fmt.Errorf("decode unified adoption attach: %w", err)
			}
			for index, event := range events {
				switch event.Kind {
				case controlmode.EventCommandResponse:
					// Response text here can only be an error message; the
					// %error that follows carries the judgement.
				case controlmode.EventCommandError:
					for _, remainder := range events[index+1:] {
						if err := unit.owner.consumeObserverEvent(remainder); err != nil {
							return err
						}
					}
					return ErrUnifiedAdoptSessionMissing
				case controlmode.EventCommandEnd:
					// Readiness may share the decoded chunk with the attach's
					// own %end; the remainder is consumed through the
					// readiness watch so it cannot be lost.
					for _, remainder := range events[index+1:] {
						if err := unit.consumeReadyEvent(remainder); err != nil {
							return err
						}
					}
					return nil
				default:
					if err := unit.consumeReadyEvent(event); err != nil {
						return err
					}
				}
			}
		}
	}
}

// runAdoption owns the whole adoption sequence on the unit goroutine, exactly
// as the unit goroutine owns birth: witness resolution, the buffered holder,
// the atomic capture composite with its drift retries, journal admission, the
// synthesized bootstrap, and registration. It answers the founding request
// itself only on success; every failure propagates so the unit's death
// protocol delivers it exactly once.
func (unit *unifiedDevUnit) runAdoption(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error) error {
	effects := unit.owner
	sessionID := unit.birth.adoption.sessionID
	source, err := buildSourceWitness(ctx, effects.server, "", sessionID)
	if err != nil {
		return errors.Join(ErrUnifiedAdoptSessionMissing, err)
	}
	witness := controlmode.PaneWitness{
		Session: controlmode.SessionWitness{Server: effects.server.Label, Session: sessionID, ControlGeneration: unit.generation},
		Window:  source.WindowID, Pane: source.PaneID, Incarnation: source.Incarnation,
	}
	holder := &unifiedDevBirth{owner: effects, memory: unit.memory, server: effects.server.Label, ready: make(chan struct{}), witness: witness, source: source}
	for attempt := 1; attempt <= adoptionCaptureAttempts; attempt++ {
		if attempt > 1 {
			effects.adoptionRetries.Add(1)
			select {
			case <-time.After(time.Duration(attempt) * 25 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		reservation, key, trimmed, err := unit.submitAdoptionComposite(ctx, decoder, read, readErr, holder, &witness, attempt)
		if errors.Is(err, errUnifiedAdoptDrift) {
			continue
		}
		if err != nil {
			return err
		}
		return unit.finishAdoption(ctx, decoder, read, readErr, holder, reservation, key, trimmed)
	}
	return ErrUnifiedAdoptUnstable
}

// The accepted adoption has one failure owner across receipt, source recheck,
// authority validation and publication. Every unsuccessful exit drains its
// stream through settlement before the provisional journal can be removed.
func (unit *unifiedDevUnit) finishAdoption(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error, holder *unifiedDevBirth, reservation *unifiedjournal.AdoptionReservation, key unifiedjournal.PaneKey, trimmed bool) (err error) {
	effects := unit.owner
	stream := &recordingSettlementStream{unit: unit, decoder: decoder, read: read, readErr: readErr}
	published := false
	defer func() {
		if !published {
			err = stream.abortAdoption(holder, reservation, err)
		}
	}()
	if err := stream.awaitInitial(ctx, holder.initial); err != nil {
		return err
	}
	effects.initialObserver().recordingRegistry().retention.callHook("adoption_initial_complete", key)
	if err := holder.verifyInitialSource(ctx); err != nil {
		return err
	}
	witness := holder.witness
	sessionID := witness.Session.Session
	unit.holder = holder
	unit.witnesses = append(unit.witnesses, witness)
	effects.mu.Lock()
	if effects.panes[witness.Pane] != holder || !unit.isLive() || (unit.birth.context != nil && unit.birth.context.Err() != nil) {
		effects.mu.Unlock()
		return errRecordingInitial
	}
	if err := effects.initialObserver().publishInitial(holder.initial); err != nil {
		effects.mu.Unlock()
		return err
	}
	holder.mu.Lock()
	holder.committed = true
	holder.mu.Unlock()
	effects.units[sessionID] = unit
	unit.sessionIdentity = sessionID
	effects.active[sessionID] = key
	delete(effects.adopting, sessionID)
	published = true
	effects.mu.Unlock()
	// Active registration is the activation boundary; the provisional
	// complete-pane reservation now becomes a durable charge.
	effects.journalMu.Lock()
	reservation.Commit()
	effects.journalMu.Unlock()
	unit.delivered = true
	unit.birth.done <- unifiedDevCommandResult{session: sessionID, key: key, trimmed: trimmed}
	return nil
}

func (stream *recordingSettlementStream) abortAdoption(holder *unifiedDevBirth, reservation *unifiedjournal.AdoptionReservation, cause error) error {
	holder.mu.Lock()
	holder.aborted = true
	holder.mu.Unlock()
	if holder.initial != nil {
		cause = stream.settleInitial(holder.initial, stream.unit.owner.initialObserver().recordingRegistry(), cause)
	}
	stream.unit.owner.journalMu.Lock()
	reservation.Abort()
	stream.unit.owner.realm.ReclaimRetired(journalKey(holder.witness))
	stream.unit.owner.journalMu.Unlock()
	// Receipt failure can precede registration on the unit, so its reaper
	// does not yet own this provisional holder. Release it with the abort.
	stream.unit.owner.mu.Lock()
	if stream.unit.owner.panes[holder.witness.Pane] == holder {
		delete(stream.unit.owner.panes, holder.witness.Pane)
	}
	stream.unit.owner.mu.Unlock()
	return cause
}

// submitAdoptionComposite runs one capture attempt: it registers the buffering
// holder, submits the composite as ONE line, consumes its seven blocks, and at
// the final %end — the boundary — admits the initial operation ON THIS GOROUTINE
// before events[index+1:] are consumed, so output decoded behind the boundary
// finds the holder streaming and ordered after its bootstrap. Ready publication
// separately waits for the exact operation receipt. On any failure
// the holder is released and its buffer discarded before the decoded remainder
// is routed onward.
func (unit *unifiedDevUnit) submitAdoptionComposite(ctx context.Context, decoder *controlmode.Decoder, read <-chan []byte, readErr <-chan error, holder *unifiedDevBirth, witness *controlmode.PaneWitness, attempt int) (*unifiedjournal.AdoptionReservation, unifiedjournal.PaneKey, bool, error) {
	var batch controlmode.EventBatch
	defer batch.Release()
	effects := unit.owner
	// The holder is registered BEFORE the composite is submitted: every
	// %output this connection delivers before the boundary is buffered and
	// discarded there, because its effect is already inside the capture; the
	// gap where a byte could fall between capture and live flow does not
	// exist.
	effects.mu.Lock()
	effects.panes[witness.Pane] = holder
	effects.mu.Unlock()
	release := func() {
		effects.mu.Lock()
		if effects.panes[witness.Pane] == holder {
			delete(effects.panes, witness.Pane)
		}
		effects.mu.Unlock()
		holder.mu.Lock()
		holder.clearPendingLocked()
		holder.mu.Unlock()
	}
	if _, err := io.WriteString(unit.ptmx, adoptionCompositeLine(witness.Pane, unit.birth.adoption.historyRows)); err != nil {
		release()
		return nil, unifiedjournal.PaneKey{}, false, err
	}
	remaining := adoptionCompositeBlocks
	responses := make([]string, 0, adoptionCompositeBlocks)
	response := recordingResponse{owner: unit.memory}
	defer response.release()
	// The submission span opens at the composite's first %begin, not at the
	// PTY write: flood output emitted before tmux processes the line is still
	// in flight on the stream and precedes the span, and its effect is inside
	// the capture exactly like every other pre-boundary byte.
	began := false
	for {
		select {
		case <-ctx.Done():
			release()
			return nil, unifiedjournal.PaneKey{}, false, ctx.Err()
		case err := <-readErr:
			release()
			return nil, unifiedjournal.PaneKey{}, false, err
		case chunk := <-read:
			events, err := batch.Feed(decoder, chunk)
			if err != nil {
				release()
				return nil, unifiedjournal.PaneKey{}, false, fmt.Errorf("decode unified adoption composite: %w", err)
			}
			// Judge the entire decoded chunk before its final command boundary can
			// open a generation. A tripwire record sharing that read with %end
			// must fault the unit before BeginReconstructedPane or AdmitPane.
			for _, event := range events {
				if err := observerFlowControlFault(event); err != nil {
					release()
					return nil, unifiedjournal.PaneKey{}, false, err
				}
			}
			for index, event := range events {
				switch event.Kind {
				case controlmode.EventCommandResponse:
					if err := response.Write(event.Data); err != nil {
						release()
						return nil, unifiedjournal.PaneKey{}, false, err
					}
				case controlmode.EventCommandError:
					// The target vanished mid-composite: the only rejection
					// these sub-commands can draw. Route the decoded
					// remainder onward before failing, as every consumer on
					// this connection must.
					release()
					for _, rest := range events[index+1:] {
						if err := effects.consumeObserverEvent(rest); err != nil {
							return nil, unifiedjournal.PaneKey{}, false, err
						}
					}
					return nil, unifiedjournal.PaneKey{}, false, ErrUnifiedAdoptSessionMissing
				case controlmode.EventCommandEnd:
					responses = append(responses, response.String())
					response.Reset()
					remaining--
					if remaining != 0 {
						continue
					}
					reservation, key, trimmed, err := unit.commitAdoption(responses, holder, witness, attempt)
					if err != nil {
						release()
					}
					for _, rest := range events[index+1:] {
						if consumeErr := effects.consumeObserverEvent(rest); consumeErr != nil {
							// The attempt cannot activate: even after a
							// successful boundary flip, a dead consumer means
							// this unit dies before registration, so the
							// provisional charge is released with it.
							if reservation != nil {
								stream := &recordingSettlementStream{unit: unit, decoder: decoder, read: read, readErr: readErr}
								consumeErr = stream.abortAdoption(holder, reservation, consumeErr)
								release()
							}
							return nil, unifiedjournal.PaneKey{}, false, consumeErr
						}
					}
					return reservation, key, trimmed, err
				case controlmode.EventCommandBegin:
					began = true
				default:
					if began && (event.Kind == controlmode.EventOutput || event.Kind == controlmode.EventExtendedOutput) {
						// Measured zero on tmux 3.4: the composite is atomic
						// by construction. The counter turns the measurement
						// into a regression gate.
						effects.adoptionSpanOutputs.Add(1)
					}
					if err := effects.consumeObserverEvent(event); err != nil {
						release()
						return nil, unifiedjournal.PaneKey{}, false, err
					}
				}
			}
		}
	}
}

// commitAdoption judges one attempt's seven block responses at the boundary and,
// when they hold, opens the reconstructed generation: a provisional journal
// reservation first, then registry admission, then the synthesized bootstrap
// as ordinary output, and only then the boundary flip that discards the
// holder's pre-boundary buffer and commits it to live flow. The returned
// reservation still holds the complete-pane charge provisionally: the caller
// commits it at activation, and every failure between here and activation
// aborts it — a failed adoption never consumes a slot.
func (unit *unifiedDevUnit) commitAdoption(responses []string, holder *unifiedDevBirth, witness *controlmode.PaneWitness, attempt int) (*unifiedjournal.AdoptionReservation, unifiedjournal.PaneKey, bool, error) {
	effects := unit.owner
	var tamper func(string) string
	if effects.adoptionPostTamper != nil {
		tamper = func(post string) string { return effects.adoptionPostTamper(attempt, post) }
	}
	bootstrap, geometry, trimmed, err := parseInitialCapture(responses, unit.process.Process.Pid, tamper)
	if err != nil {
		return nil, unifiedjournal.PaneKey{}, false, err
	}
	var key unifiedjournal.PaneKey
	var reservation *unifiedjournal.AdoptionReservation
	for tries := 0; tries < 16 && reservation == nil; tries++ {
		key = journalKey(*witness)
		source, err := effects.journalSourceIdentity(key, holder.source)
		if err != nil {
			return nil, unifiedjournal.PaneKey{}, false, err
		}
		effects.journalMu.Lock()
		provisional, admitErr := effects.realm.BeginReconstructedPaneForSource(key, geometry, source)
		effects.journalMu.Unlock()
		switch {
		case admitErr == nil:
			reservation = provisional
		case errors.Is(admitErr, unifiedjournal.ErrQuota):
			return nil, unifiedjournal.PaneKey{}, false, ErrUnifiedAdoptSlotsExhausted
		case errors.Is(admitErr, unifiedjournal.ErrInvalidated):
			// A surviving journal file from an earlier broker run occupies
			// this generation's key: the realm outlives active state whenever
			// its runtime directory does. A stale generation is never
			// resumed; the next generation is minted and admission retried so
			// the session stays adoptable across restarts. The stale
			// generation itself is slot-retired at scan and superseded when
			// this admission commits, so it never consumes capacity for good.
			unit.generation = effects.mintControlGeneration(true)
			witness.Session.ControlGeneration = unit.generation
		default:
			return nil, unifiedjournal.PaneKey{}, false, admitErr
		}
	}
	if reservation == nil {
		return nil, unifiedjournal.PaneKey{}, false, errors.New("unified adoption found no unused generation")
	}
	abort := func() {
		effects.journalMu.Lock()
		reservation.Abort()
		effects.realm.ReclaimRetired(key)
		effects.journalMu.Unlock()
	}
	holder.mu.Lock()
	holder.witness = *witness
	holder.mu.Unlock()
	if err := effects.observer.AdmitPane(*witness); err != nil {
		abort()
		return nil, unifiedjournal.PaneKey{}, false, err
	}
	observer := effects.initialObserver()
	if observer == nil {
		abort()
		return nil, unifiedjournal.PaneKey{}, false, errRecordingInitial
	}
	op, err := observer.beginInitial(recordingInitialAdoption, *witness, holder.source, geometry, bootstrap)
	if err != nil {
		abort()
		return nil, unifiedjournal.PaneKey{}, false, err
	}
	op.bindContext(unit.birth.context)
	op.bindOwner(unit)
	// The boundary flip. Pre-boundary buffered output is discarded because its
	// effect is inside the capture; everything after it flows to the journal
	// on the same ordered path, behind the bootstrap.
	holder.mu.Lock()
	holder.clearPendingLocked()
	holder.initial = op
	holder.geometry = geometry
	holder.streaming = true
	holder.mu.Unlock()
	return reservation, key, trimmed, nil
}

// adoptionCompositeLine is the atomic capture composite: eight semicolon
// sub-commands submitted as ONE control-mode line, which tmux 3.4 drains in a
// single command-queue run without processing pane reads (measured by the
// adoption regression test). The first command rechecks this observer's
// flags and the second normalizes its target subscription to pane:on before
// PRE/capture. This is intentionally honest rather than complete tripwire
// coverage: pane:off and no-output are signal-less in tmux, so the readiness
// flag interrogation plus this boundary normalization cover those states.
// The PRE and POST probes carry every field
// whose mutation could split the capture — history_limit included so at-cap
// saturation stays visible. The capture keeps hard row boundaries (-N, and
// deliberately no -J): the wrap bit is unrecoverable either way and columns
// never change after attach. The -P sub-command recovers the pending parser
// prefix — an escape sequence the pane terminal has consumed but not
// completed — without which a capture taken mid-sequence journals the
// continuation without its prefix and corrupts the parser at the seam.
func adoptionCompositeLine(pane string, requestedHistory ...int) string {
	historyRows := adoptionHistoryCapRows
	if len(requestedHistory) == 1 {
		historyRows = requestedHistory[0]
	}
	const probe = "#{history_size} #{history_limit} #{cursor_x} #{cursor_y} #{alternate_on} #{window_panes} #{session_windows} #{?alternate_on,#{alternate_saved_x},0} #{?alternate_on,#{alternate_saved_y},0}"
	// pane_tabs is last because it is the only field that can be empty.
	const modes = "#{cursor_flag} #{insert_flag} #{keypad_cursor_flag} #{keypad_flag} #{origin_flag} #{wrap_flag} #{mouse_standard_flag} #{mouse_button_flag} #{mouse_all_flag} #{mouse_utf8_flag} #{mouse_sgr_flag} #{scroll_region_upper} #{scroll_region_lower} #{pane_width} #{pane_height} #{pane_tabs}"
	target := shellQuote(pane)
	return strings.Join([]string{
		"list-clients -F " + shellQuote("#{client_pid}|#{client_flags}"),
		"refresh-client -A " + shellQuote(pane+":on"),
		"display-message -p -t " + target + " " + shellQuote(probe),
		"capture-pane -e -p -N -S -" + strconv.Itoa(historyRows) + " -E - -t " + target,
		// -q makes the absent saved screen an empty successful block. Both
		// captures stay in this command-queue run, inside the drift probes.
		"capture-pane -a -q -e -p -N -t " + target,
		"capture-pane -p -P -t " + target,
		"display-message -p -t " + target + " " + shellQuote(modes),
		"display-message -p -t " + target + " " + shellQuote(probe),
	}, " ; ") + "\n"
}

type adoptionProbe struct {
	historySize, historyLimit, cursorX, cursorY, alternate, panes, windows int
	savedX, savedY                                                         int
}

func parseAdoptionProbe(text string) (adoptionProbe, error) {
	invalid := errors.New("unified adoption probe returned an invalid shape")
	fields := strings.Fields(text)
	if len(fields) != 9 {
		return adoptionProbe{}, invalid
	}
	values := make([]int, len(fields))
	for index, field := range fields {
		value, err := strconv.Atoi(field)
		if err != nil || value < 0 {
			return adoptionProbe{}, invalid
		}
		values[index] = value
	}
	return adoptionProbe{
		historySize: values[0], historyLimit: values[1],
		cursorX: values[2], cursorY: values[3],
		alternate: values[4], panes: values[5], windows: values[6],
		savedX: values[7], savedY: values[8],
	}, nil
}

type adoptionModes struct {
	cursorVisible, insert, keypadCursor, keypad, origin, wrap bool
	mouseStandard, mouseButton, mouseAll, mouseUTF8, mouseSGR bool
	scrollTop, scrollBottom, columns, rows                    int
	tabs                                                      []int
}

func parseAdoptionModes(text string) (adoptionModes, error) {
	invalid := errors.New("unified adoption mode probe returned an invalid shape")
	fields := strings.Fields(text)
	// 15 fields when the pane has no tab stops at all; 16 otherwise.
	if len(fields) != 15 && len(fields) != 16 {
		return adoptionModes{}, invalid
	}
	flags := make([]bool, 11)
	for index := range flags {
		switch fields[index] {
		case "0":
		case "1":
			flags[index] = true
		default:
			return adoptionModes{}, invalid
		}
	}
	numbers := make([]int, 4)
	for index := range numbers {
		value, err := strconv.Atoi(fields[11+index])
		if err != nil || value < 0 {
			return adoptionModes{}, invalid
		}
		numbers[index] = value
	}
	modes := adoptionModes{
		cursorVisible: flags[0], insert: flags[1], keypadCursor: flags[2], keypad: flags[3],
		origin: flags[4], wrap: flags[5],
		mouseStandard: flags[6], mouseButton: flags[7], mouseAll: flags[8], mouseUTF8: flags[9], mouseSGR: flags[10],
		scrollTop: numbers[0], scrollBottom: numbers[1], columns: numbers[2], rows: numbers[3],
	}
	if modes.columns < 1 || modes.rows < 1 || modes.scrollBottom < modes.scrollTop || modes.scrollBottom >= modes.rows {
		return adoptionModes{}, invalid
	}
	if len(fields) == 16 {
		for _, column := range strings.Split(fields[15], ",") {
			value, err := strconv.Atoi(column)
			if err != nil || value < 0 {
				return adoptionModes{}, invalid
			}
			if value >= modes.columns {
				// A stop beyond the current width can survive a shrink; it is
				// unreachable and dropped rather than refused.
				continue
			}
			modes.tabs = append(modes.tabs, value)
		}
	}
	return modes, nil
}

// parseAdoptionCapture splits the capture block into rows. Every row arrives
// as one response line; the final line's terminator belongs to the block
// framing, not to the last row.
func parseAdoptionCapture(response string) []string {
	if response == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(response, "\n"), "\n")
}

type adoptionAlternate struct {
	rows             []string
	cursorX, cursorY int
}

// tmux keeps the saved grid at its original height until alternate exit.
// Shrink discards bottom rows below the saved cursor first, then scrolls the
// remaining overflow into history. Growth pulls history into view, then pads.
// hscrolled (history eligible for growth), old width and wrap/allocation flags
// are not exposed. Use available history and hard captured rows; cells clipped
// by capture cannot be recovered. These hidden-screen limits must not prevent
// opening the exact visible alternate display.
func fitAdoptionAlternate(history []string, saved adoptionAlternate, modes adoptionModes) ([]string, int, int) {
	x := min(max(saved.cursorX, 0), modes.columns-1)
	y := min(max(saved.cursorY, 0), max(0, len(saved.rows)-1))
	display := saved.rows
	padding := 0
	if len(display) > modes.rows {
		shrink := len(display) - modes.rows
		drop := min(shrink, len(display)-1-y)
		display = display[:len(display)-drop]
		y -= shrink - drop
	} else {
		growth := modes.rows - len(display)
		pull := min(growth, len(history))
		y += pull
		padding = growth - pull
	}
	rows := make([]string, 0, len(history)+len(display)+padding)
	rows = append(rows, history...)
	rows = append(rows, display...)
	rows = append(rows, make([]string, padding)...)
	return rows, x, min(max(y, 0), modes.rows-1)
}

// synthesizeAdoptionBootstrap renders the captured pane state as one byte
// sequence a fresh terminal of the captured geometry replays into the
// capture-equivalent visible screen, with a fitted hidden normal screen when
// alternate mode is active. The emit order is pinned: attribute reset, rows
// in order (history scrolls through naturally, CRLF between rows and none
// after the last), saved normal cursor and alternate display when active,
// then DECSTBM — which homes the cursor — then DECOM per the
// captured origin flag, then CUP with region-relative coordinates iff origin
// mode is on, then the remaining modes, then tab stops (whose HTS placement
// moves the cursor by column, so the captured position is restored again
// after them), and the captured pending parser prefix LAST, immediately
// before live bytes. G0/G1 designation, arbitrary saved DECSC state, cursor style, and
// the SGR live at the seam are unreadable on tmux 3.4 and reset to defaults:
// the hidden normal display also has the resize limits described above.
func synthesizeAdoptionBootstrap(rows []string, pending string, cursorX, cursorY int, modes adoptionModes, alternate ...adoptionAlternate) ([]byte, bool, error) {
	var switchScreen strings.Builder
	if len(alternate) > 0 {
		saved := alternate[0]
		if len(alternate) != 1 || len(rows) < modes.rows {
			return nil, false, errors.New("unified adoption saved screen has an invalid shape")
		}
		// Ordinary capture is normal history followed by the alternate view.
		// Seed the saved normal display before entering 1049, so a later exit
		// restores it and its cursor. CUP paints the alternate rows without
		// scrolling either buffer, including a full-width bottom row.
		visible := rows[len(rows)-modes.rows:]
		var savedX, savedY int
		rows, savedX, savedY = fitAdoptionAlternate(rows[:len(rows)-modes.rows], saved, modes)
		fmt.Fprintf(&switchScreen, "\x1b[%d;%dH\x1b[?1049h\x1b[0m", savedY+1, savedX+1)
		for index, line := range visible {
			fmt.Fprintf(&switchScreen, "\x1b[%d;1H%s", index+1, line)
		}
	}
	set := func(builder *strings.Builder, on bool, enable, disable string) {
		if on {
			builder.WriteString(enable)
		} else {
			builder.WriteString(disable)
		}
	}
	row := cursorY + 1
	if modes.origin {
		row = cursorY - modes.scrollTop + 1
		if row < 1 {
			row = 1
		}
	}
	cursor := fmt.Sprintf("\x1b[%d;%dH", row, cursorX+1)
	var tail strings.Builder
	fmt.Fprintf(&tail, "\x1b[%d;%dr", modes.scrollTop+1, modes.scrollBottom+1)
	set(&tail, modes.origin, "\x1b[?6h", "\x1b[?6l")
	tail.WriteString(cursor)
	set(&tail, modes.wrap, "\x1b[?7h", "\x1b[?7l")
	set(&tail, modes.cursorVisible, "\x1b[?25h", "\x1b[?25l")
	set(&tail, modes.insert, "\x1b[4h", "\x1b[4l")
	set(&tail, modes.keypadCursor, "\x1b[?1h", "\x1b[?1l")
	set(&tail, modes.keypad, "\x1b=", "\x1b>")
	set(&tail, modes.mouseStandard, "\x1b[?1000h", "\x1b[?1000l")
	set(&tail, modes.mouseButton, "\x1b[?1002h", "\x1b[?1002l")
	set(&tail, modes.mouseAll, "\x1b[?1003h", "\x1b[?1003l")
	set(&tail, modes.mouseUTF8, "\x1b[?1005h", "\x1b[?1005l")
	set(&tail, modes.mouseSGR, "\x1b[?1006h", "\x1b[?1006l")
	tail.WriteString("\x1b[3g")
	for _, column := range modes.tabs {
		fmt.Fprintf(&tail, "\x1b[%dG\x1bH", column+1)
	}
	tail.WriteString(cursor)
	tail.WriteString(pending)

	const head = "\x1b[0m"
	size := len(head) + switchScreen.Len() + tail.Len()
	for _, line := range rows {
		size += len(line) + 2
	}
	if len(rows) != 0 {
		size -= 2
	}
	trimmed := false
	// Preserve the visible screen. Under alternate mode the normal display is
	// hidden: after exhausting history, replace its oldest rows with blanks as
	// needed. Keep row positions and the saved cursor, never trim alternate rows.
	for size > adoptionBootstrapCapBytes && len(rows) > modes.rows {
		size -= len(rows[0]) + 2
		rows = rows[1:]
		trimmed = true
	}
	if len(alternate) != 0 {
		for index := 0; size > adoptionBootstrapCapBytes && index < len(rows); index++ {
			size -= len(rows[index])
			rows[index] = ""
			trimmed = true
		}
	}
	if size > adoptionBootstrapCapBytes {
		return nil, false, errors.New("unified adoption screen exceeds the bootstrap cap")
	}
	bootstrap := make([]byte, 0, size)
	bootstrap = append(bootstrap, head...)
	for index, line := range rows {
		if index != 0 {
			bootstrap = append(bootstrap, '\r', '\n')
		}
		bootstrap = append(bootstrap, line...)
	}
	bootstrap = append(bootstrap, switchScreen.String()...)
	bootstrap = append(bootstrap, tail.String()...)
	return bootstrap, trimmed, nil
}
