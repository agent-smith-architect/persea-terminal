package broker

import (
	"context"
	"errors"
	"log"

	"persea-terminal/internal/proto"
)

// Operator session adoption.
//
// Adoption is the create twin for sessions that already exist: the browser
// supplies identity and a bounded history count, and the unified
// provider owns every judgement — existence, eligibility, slot budget, and the
// capture itself. Each refusal is a code from a closed set the UI maps to its
// own copy; free text never crosses this boundary.
func (s *Server) adopt(writer *lockedWriter, ctrl proto.Control) {
	refuse := func(code string) {
		log.Printf("component=broker event=adopt_refused realm=%q server=%q session=%q reason=%q", s.config.Realm, ctrl.ServerLabel, ctrl.SessionID, code)
		_ = writer.control(proto.Control{Type: "adopt_refused", Code: code})
	}
	if s.unified == nil {
		refuse("unified_unavailable")
		return
	}
	if !s.unified.forServer(ctrl.ServerLabel) {
		refuse("not_permitted")
		return
	}
	historyRows := proto.AdoptionHistoryMaxRows
	if ctrl.HistoryRows != nil {
		historyRows = *ctrl.HistoryRows
	}
	adoption, err := s.unified.AdoptSession(context.Background(), ctrl.SessionID, historyRows)
	if err != nil {
		refuse(adoptRefusalCode(err))
		return
	}
	log.Printf("component=broker event=adopt_ok realm=%q server=%q session=%q existing=%t trimmed=%t", s.config.Realm, ctrl.ServerLabel, adoption.SessionID, adoption.Existing, adoption.Trimmed)
	_ = writer.control(proto.Control{Type: "adopt_ok", ServerLabel: ctrl.ServerLabel, SessionID: adoption.SessionID})
}

// adoptRefusalCode maps each typed provider refusal onto the closed wire code
// set. Anything untyped degrades to adopt_failed rather than leaking an error
// string.
func adoptRefusalCode(err error) string {
	switch {
	case errors.Is(err, ErrUnifiedAdoptHistory):
		return "invalid_history"
	case errors.Is(err, ErrUnifiedAdoptSessionMissing):
		return "session_gone"
	case errors.Is(err, ErrUnifiedAdoptAlternateScreen):
		return "blocked_alt_screen"
	case errors.Is(err, ErrUnifiedAdoptMultiWindow):
		return "blocked_multi_window"
	case errors.Is(err, ErrUnifiedAdoptMultiPane):
		return "blocked_multi_pane"
	case errors.Is(err, ErrUnifiedAdoptSlotsExhausted):
		return "slots_exhausted"
	case errors.Is(err, ErrUnifiedAdoptInProgress):
		return "adoption_in_progress"
	case errors.Is(err, ErrUnifiedAdoptUnstable):
		return "adoption_contended"
	default:
		return "adopt_failed"
	}
}
