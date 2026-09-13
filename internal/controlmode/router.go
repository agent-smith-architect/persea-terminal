package controlmode

import "errors"

var (
	ErrForeignSession  = errors.New("foreign control session")
	ErrMissedFirstByte = errors.New("pane observation began too late")
)

type SessionWitness struct {
	Server            string
	Session           string
	ControlGeneration uint64
}

type PaneWitness struct {
	Session     SessionWitness
	Window      string
	Pane        string
	Incarnation string
}

type ObservationKind uint8

const (
	ObservationOutput ObservationKind = iota + 1
	ObservationRename
	ObservationReplacement
	ObservationDisconnect
	ObservationPause
	ObservationDecoderAmbiguity
	ObservationServerRestart
	ObservationForeignPipe
	// ObservationOwnerGone is emitted only after an authoritative exact-witness
	// recheck proves the tmux session/pane owner absent. It is deliberately
	// distinct from a control transport disconnect.
	ObservationOwnerGone
)

type Observation struct {
	Kind        ObservationKind
	Witness     PaneWitness
	Replacement PaneWitness
	Data        []byte
	Label       string
}

type RouteDisposition uint8

const (
	RouteDeliver RouteDisposition = iota + 1
	RouteRetain
	RouteIgnore
	RouteInvalidate
)

type RouteReason uint8

const (
	RouteExactWitness RouteReason = iota + 1
	RouteIdentityUnchanged
	RouteOtherSession
	RouteMissedFirstByte
	RouteAlreadyInvalid
	RouteSourceReplacement
	RoutePaneIncarnationChanged
	RouteControlDisconnected
	RouteControlPaused
	RouteDecoderAmbiguous
	RouteServerRestarted
	RouteForeignObserverCoexists
	RouteControlGenerationChanged
	RouteOwnerGone
)

type Delivery struct {
	Witness PaneWitness
	Data    []byte
}

type RouteResult struct {
	Disposition RouteDisposition
	Reason      RouteReason
	Delivery    *Delivery
}

type paneRouteKey struct {
	window string
	pane   string
}

type SessionRouter struct {
	session SessionWitness
	panes   map[paneRouteKey]PaneWitness
	invalid map[paneRouteKey]RouteReason
	failed  bool
}

func NewSessionRouter(session SessionWitness) *SessionRouter {
	return &SessionRouter{session: session, panes: make(map[paneRouteKey]PaneWitness), invalid: make(map[paneRouteKey]RouteReason)}
}

func routeKey(witness PaneWitness) paneRouteKey {
	return paneRouteKey{window: witness.Window, pane: witness.Pane}
}

func (router *SessionRouter) Admit(witness PaneWitness) error {
	if witness.Session != router.session {
		return ErrForeignSession
	}
	key := routeKey(witness)
	if _, invalid := router.invalid[key]; invalid || router.failed {
		return ErrMissedFirstByte
	}
	if existing, exists := router.panes[key]; exists && existing != witness {
		router.invalid[key] = RoutePaneIncarnationChanged
		delete(router.panes, key)
		return ErrMissedFirstByte
	}
	router.panes[key] = witness
	return nil
}

func (router *SessionRouter) Observe(observation Observation) RouteResult {
	witness := observation.Witness
	if witness.Session.Session != router.session.Session {
		return RouteResult{Disposition: RouteIgnore, Reason: RouteOtherSession}
	}
	if witness.Session.Server != router.session.Server {
		router.invalidateAll(RouteServerRestarted)
		return RouteResult{Disposition: RouteInvalidate, Reason: RouteServerRestarted}
	}
	if witness.Session.ControlGeneration != router.session.ControlGeneration {
		router.invalidateAll(RouteControlGenerationChanged)
		return RouteResult{Disposition: RouteInvalidate, Reason: RouteControlGenerationChanged}
	}
	key := routeKey(witness)
	if _, invalid := router.invalid[key]; invalid || router.failed {
		return RouteResult{Disposition: RouteIgnore, Reason: RouteAlreadyInvalid}
	}
	existing, admitted := router.panes[key]
	if observation.Kind == ObservationOutput && !admitted {
		router.invalid[key] = RouteMissedFirstByte
		return RouteResult{Disposition: RouteIgnore, Reason: RouteMissedFirstByte}
	}
	if admitted && existing.Incarnation != witness.Incarnation {
		router.invalid[key] = RoutePaneIncarnationChanged
		delete(router.panes, key)
		return RouteResult{Disposition: RouteInvalidate, Reason: RoutePaneIncarnationChanged}
	}
	switch observation.Kind {
	case ObservationOutput:
		data := append([]byte(nil), observation.Data...)
		return RouteResult{Disposition: RouteDeliver, Reason: RouteExactWitness, Delivery: &Delivery{Witness: existing, Data: data}}
	case ObservationRename:
		return RouteResult{Disposition: RouteRetain, Reason: RouteIdentityUnchanged}
	case ObservationReplacement:
		router.invalid[key] = RouteSourceReplacement
		delete(router.panes, key)
		replacementKey := routeKey(observation.Replacement)
		router.invalid[replacementKey] = RouteSourceReplacement
		delete(router.panes, replacementKey)
		return RouteResult{Disposition: RouteInvalidate, Reason: RouteSourceReplacement}
	case ObservationDisconnect:
		router.invalidateAll(RouteControlDisconnected)
		return RouteResult{Disposition: RouteInvalidate, Reason: RouteControlDisconnected}
	case ObservationOwnerGone:
		router.invalidateAll(RouteOwnerGone)
		return RouteResult{Disposition: RouteInvalidate, Reason: RouteOwnerGone}
	case ObservationPause:
		router.invalidateAll(RouteControlPaused)
		return RouteResult{Disposition: RouteInvalidate, Reason: RouteControlPaused}
	case ObservationDecoderAmbiguity:
		router.invalidateAll(RouteDecoderAmbiguous)
		return RouteResult{Disposition: RouteInvalidate, Reason: RouteDecoderAmbiguous}
	case ObservationServerRestart:
		router.invalidateAll(RouteServerRestarted)
		return RouteResult{Disposition: RouteInvalidate, Reason: RouteServerRestarted}
	case ObservationForeignPipe:
		return RouteResult{Disposition: RouteRetain, Reason: RouteForeignObserverCoexists}
	default:
		router.invalidateAll(RouteDecoderAmbiguous)
		return RouteResult{Disposition: RouteInvalidate, Reason: RouteDecoderAmbiguous}
	}
}

func (router *SessionRouter) invalidateAll(reason RouteReason) {
	router.failed = true
	for key := range router.panes {
		router.invalid[key] = reason
	}
	clear(router.panes)
}

func (router *SessionRouter) UnifiedEligible(witness PaneWitness) bool {
	if router.failed || witness.Session != router.session {
		return false
	}
	stored, ok := router.panes[routeKey(witness)]
	return ok && stored == witness
}

func (router *SessionRouter) CanInjectFocus(witness PaneWitness) bool {
	return router.UnifiedEligible(witness)
}
