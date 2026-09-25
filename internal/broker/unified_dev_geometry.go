// Unified guarded geometry issuance and ticket settlement.
package broker

import (
	"context"
	"errors"
	"fmt"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
	"time"
)

// unifiedGeometryIssuer is the seam attachment.go's canonical resize transaction
// issues through for the configured unified-dev target.
type unifiedGeometryIssuer struct {
	provider *UnifiedDevPaneEffects
	registry *paneRegistry
	session  string
}

// guardedResizeBlocks is how many command blocks the guarded if-shell occupies on
// the control connection: the submission, and the deferred success command tmux
// queues once the guard passes. Established against real tmux by the Phase-1
// boundary proof; the completion of the last block is sequence N.
const guardedResizeBlocks = 2

// BeginGeometry reserves every capacity one geometry mutation needs, before
// any command reaches tmux, and types each failure under the RESIZE
// guarantee. The default is fatal: only a rejection that leaves this
// attachment's authority intact — a command budget with no slot to spare, a
// journal cap with no room for the record — is typed terminal.RefuseResize,
// because the operator can simply try again. A target that is no longer
// journal-active, a runtime that is gone, a pause that faulted the
// generation, or a journal that has invalidated this generation are verdicts
// on the attachment: nothing was mutated, but nothing later could succeed, so
// the attachment closes and a reopen re-mints.
func (issuer *unifiedGeometryIssuer) BeginGeometry(ctx context.Context) (geometryTicket, error) {
	if refuse := issuer.provider.geometryRefusal; refuse != nil {
		if err := refuse(issuer.session); err != nil {
			return nil, terminal.RefuseResize(err)
		}
	}
	key, ok := issuer.provider.paneKey(issuer.session)
	if !ok {
		return nil, errors.New("unified target is not journal-active")
	}
	runtime := issuer.registry.retention
	if runtime == nil {
		return nil, errors.New("unified retention runtime is unavailable")
	}
	owner, err := runtime.acquireGeometryOwner(key)
	if err != nil {
		if errors.Is(err, errGeometryPauseOwned) {
			return nil, terminal.RefuseResize(err)
		}
		return nil, terminal.RefuseResize(fmt.Errorf("geometry command budget: %w", err))
	}
	ownerHeld := true
	releaseOwner := func() {
		if ownerHeld {
			runtime.releaseGeometryOwner(key, owner)
			ownerHeld = false
		}
	}
	commitSlot, err := runtime.reserveSlot(key)
	if err != nil {
		releaseOwner()
		return nil, terminal.RefuseResize(fmt.Errorf("geometry command budget: %w", err))
	}
	releaseSlot, err := runtime.reserveSlot(key)
	if err != nil {
		runtime.releaseUnusedSlot(commitSlot)
		releaseOwner()
		return nil, terminal.RefuseResize(fmt.Errorf("geometry command budget: %w", err))
	}
	// faultSlot is the third reservation: if the command is issued and never
	// reaches a durable commit, the generation must be faulted (tmux may hold a
	// geometry the journal does not), and that fault must never be refused for
	// want of a command slot.
	faultSlot, err := runtime.reserveSlot(key)
	if err != nil {
		runtime.releaseUnusedSlot(commitSlot)
		runtime.releaseUnusedSlot(releaseSlot)
		releaseOwner()
		return nil, terminal.RefuseResize(fmt.Errorf("geometry command budget: %w", err))
	}
	releaseAll := func() {
		runtime.releaseUnusedSlot(commitSlot)
		runtime.releaseUnusedSlot(releaseSlot)
		runtime.releaseUnusedSlot(faultSlot)
		releaseOwner()
	}
	// startBoundary, never the blocking Boundary: the observer loop must keep
	// reading the control stream throughout, and a hold that stops ingestion
	// would deadlock the very command it is waiting for.
	started, err := runtime.startBoundary(key, "pause_start", false)
	if err != nil {
		releaseAll()
		return nil, terminal.RefuseResize(fmt.Errorf("geometry command budget: %w", err))
	}
	select {
	case err := <-started:
		if err != nil {
			// The pause flushed this pane's pending output and the journal
			// refused it: the generation has just failed closed. Not a refusal
			// of this request — the attachment's authority is gone.
			releaseAll()
			return nil, fmt.Errorf("geometry pause faulted the generation: %w", err)
		}
	case <-ctx.Done():
		// The pause_start is already queued and cannot be recalled: from the
		// moment startBoundary accepted it, something must own the matching
		// pause_end, or the late pause lands with no ticket to release it and
		// the pane holds output forever behind a "refused" Fit. Ownership here
		// is positional, not temporal: the manager queue is FIFO, so the
		// pause_end enqueued below — before this request returns — lands
		// strictly after its own pause_start AND strictly before any later
		// Fit's pause_start. Waiting for the pause to settle first and only
		// then submitting the end (from a detached completion) would open the
		// opposite hazard: a subsequent Fit's pause_start could interleave
		// (start1, start2, end1), and the late end would resume publication in
		// the middle of the new barrier. The caller still gets the typed
		// operational refusal — nothing reached tmux.
		// The existing runtime hook is test-only and lets the settled-race
		// regression test hold this exact edge until pause_start publishes its result.
		runtime.callHook("geometry_cancel_observed", key)
		select {
		case err := <-started:
			// The pause settled while cancellation was being observed. Honor
			// the settled verdict deterministically instead of racing on it:
			// a failed pause is the same fatal generation verdict as the
			// uncanceled path, with every slot returned — no pause applied,
			// so no pause_end is owed.
			if err != nil {
				releaseAll()
				return nil, fmt.Errorf("geometry pause faulted the generation: %w", err)
			}
		default:
			// Not yet settled. If the pause later applies, the queued
			// pause_end resumes it in order; if its flush fails first, the
			// pause never applies (pane.paused is never set, the fault is
			// classified on the manager loop) and the pause_end drains as a
			// harmless boundary on an unpaused pane. Either way nothing is
			// stranded, and a fatal verdict, when there is one, reaches the
			// caller on its next attempt.
		}
		// The release slot was reserved before the pause was queued for
		// exactly this submission, so ending the pause can never be denied
		// for want of capacity. submitBoundary fails only when the runtime is
		// closing: enqueue has then canceled the release reservation itself,
		// the FIFO queue still settles the already-accepted pause_start ahead
		// of the close command, and close tears down every pane — ownership
		// never dangles. No result is awaited: the buffered done channel
		// stalls nothing, and the strand window is precisely a manager
		// blocked mid-flush, which a canceled request must not wait behind.
		_, _ = runtime.submitBoundary(releaseSlot, key, "pause_end", false)
		// A future owner may enqueue only after this matching end is already in
		// the manager FIFO. If submission failed, runtime close owns teardown and
		// rejects every future owner.
		releaseOwner()
		runtime.releaseUnusedSlot(commitSlot)
		runtime.releaseUnusedSlot(faultSlot)
		return nil, terminal.RefuseResize(ctx.Err())
	}
	ticket := &unifiedGeometryTicketState{
		provider: issuer.provider, runtime: runtime, key: key,
		commitSlot: commitSlot, releaseSlot: releaseSlot, faultSlot: faultSlot,
		geometryOwner: owner,
	}
	ownerHeld = false
	// The journal's own capacity — logical record cost AND physical
	// append+commit cost — is reserved here, after the pause boundary has
	// flushed this pane's pending output (so the charge it is measured against
	// is current) and before any command reaches tmux. A journal that would
	// refuse the geometry refuses NOW, with tmux untouched, as one request's
	// outcome; once the command is issued, Commit cannot meet a cap. A journal
	// that has already invalidated this generation is not refusing a request:
	// it has no authority left to record one.
	var reservation *unifiedjournal.GeometryReservation
	var reserveErr error
	runtime.withJournalLock(func() {
		reservation, reserveErr = issuer.provider.realm.ReserveGeometry(key)
	})
	if reserveErr != nil {
		ticket.Release()
		if errors.Is(reserveErr, unifiedjournal.ErrQuota) {
			return nil, terminal.RefuseResize(reserveErr)
		}
		return nil, fmt.Errorf("geometry journal reservation: %w", reserveErr)
	}
	ticket.reservation = reservation
	return ticket, nil
}

type unifiedGeometryTicketState struct {
	provider      *UnifiedDevPaneEffects
	runtime       *retentionTrialRuntime
	key           unifiedjournal.PaneKey
	commitSlot    *retentionReservation
	releaseSlot   *retentionReservation
	faultSlot     *retentionReservation
	reservation   *unifiedjournal.GeometryReservation
	geometryOwner uint64
	// issued records that the guarded command reached the observer, or that
	// its outcome is unknown; committed records a durable geometry record.
	// Issued without committed is the post-mutation state: Release faults the
	// generation so no further output is published under a geometry the
	// journal cannot vouch for.
	issued    bool
	committed bool
	released  bool
}

func (ticket *unifiedGeometryTicketState) Issue(ctx context.Context, args []string) error {
	err := ticket.provider.issueGuarded(ctx, ticket.key.Session, args, guardedResizeBlocks)
	if err == nil || !errors.Is(err, errGeometryNotIssued) {
		ticket.issued = true
	}
	return err
}

func (ticket *unifiedGeometryTicketState) Commit(ctx context.Context, columns, rows int) error {
	reservation := ticket.reservation
	ticket.reservation = nil
	done, err := ticket.runtime.submitGeometry(ticket.commitSlot, reservation, ticket.key, unifiedjournal.Geometry{Columns: columns, Rows: rows})
	if err != nil {
		ticket.commitSlot = nil
		ticket.runtime.withJournalLock(reservation.Release)
		return err
	}
	ticket.commitSlot = nil
	select {
	case err := <-done:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	ticket.committed = true
	ticket.Release()
	return nil
}

// Release resumes publication exactly once, in the original order, whether the
// mutation committed, was refused, or failed. Without it an ordinary authority
// rejection would leave the pane holding output until a reservation failed and
// the generation silently died. An unconsumed journal reservation is refunded
// here too, so a refused or failed mutation leaves no capacity orphaned.
//
// A command that was issued but never durably committed faults the generation
// first: tmux may now hold a geometry the journal's last record contradicts,
// and every byte published after it would be rendered under the wrong grid.
// The fault is a queued boundary on its own pre-reserved slot, so it lands
// before the pause ends and cannot be refused for capacity. A commit failure
// has already faulted the generation on the manager loop; the second fault
// loses the CAS and is inert.
func (ticket *unifiedGeometryTicketState) Release() {
	if ticket.released {
		return
	}
	ticket.released = true
	if ticket.commitSlot != nil {
		ticket.runtime.releaseUnusedSlot(ticket.commitSlot)
		ticket.commitSlot = nil
	}
	if ticket.reservation != nil {
		reservation := ticket.reservation
		ticket.reservation = nil
		ticket.runtime.withJournalLock(reservation.Release)
	}
	if ticket.faultSlot != nil {
		faultSlot := ticket.faultSlot
		ticket.faultSlot = nil
		if ticket.issued && !ticket.committed {
			if done, err := ticket.runtime.submitBoundary(faultSlot, ticket.key, "geometry_fault", false); err == nil {
				select {
				case <-done:
				case <-time.After(10 * time.Second):
				}
			}
		} else {
			ticket.runtime.releaseUnusedSlot(faultSlot)
		}
	}
	done, err := ticket.runtime.submitBoundary(ticket.releaseSlot, ticket.key, "pause_end", false)
	ticket.releaseSlot = nil
	// The owner is released after pause_end is enqueued, never merely after
	// pause_start settles. That makes the unpaused-drain path structural: no
	// other same-pane pause owner can exist ahead of this end in the FIFO.
	ticket.runtime.releaseGeometryOwner(ticket.key, ticket.geometryOwner)
	ticket.geometryOwner = 0
	if err != nil {
		return
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}
