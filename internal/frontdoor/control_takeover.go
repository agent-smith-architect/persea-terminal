package frontdoor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"persea-terminal/internal/proto"
)

var errTakeoverUnavailable = errors.New("control takeover unavailable")
var errTakeoverConflict = errors.New("control takeover request conflicts with an earlier request")
var errTakeoverUsed = errors.New("control takeover request was already used")
var errTakeoverCapacity = errors.New("control takeover capacity exhausted")
var errTakeoverService = errors.New("control takeover service unavailable")

type controlTakeoverCheckpointKind uint8

const (
	controlTakeoverBeforeInputGate controlTakeoverCheckpointKind = iota + 1
	controlTakeoverAfterInputGate
	controlTakeoverTransferCommit
	controlTakeoverExactTupleSnapshot
	controlTakeoverStaleInjectionScheduling
	controlTakeoverStaleObservation
)

type controlTakeoverOwnerRef struct{ opaque uint64 }

func (r controlTakeoverOwnerRef) IsZero() bool { return r.opaque == 0 }

type controlTakeoverStaleOperation uint8

const (
	controlTakeoverStaleNone controlTakeoverStaleOperation = iota
	controlTakeoverStalePong
	controlTakeoverStaleRelease
	controlTakeoverStaleClose
	controlTakeoverStaleCleanup
)

type controlTakeoverTupleSnapshot struct {
	Authority  proto.Authority
	Generation uint64
	Holder     [32]byte
	Owner      controlTakeoverOwnerRef
	Expires    time.Time
}

type controlTakeoverCheckpoint struct {
	Kind      controlTakeoverCheckpointKind
	Owner     controlTakeoverOwnerRef
	Tuple     controlTakeoverTupleSnapshot
	Operation controlTakeoverStaleOperation
}

type controlTakeoverTestAdapter interface {
	Checkpoint(controlTakeoverCheckpoint)
}

var controlTakeoverTestAdapterHook controlTakeoverTestAdapter

type controlLeaseOwner struct {
	ref        controlTakeoverOwnerRef
	authority  proto.Authority
	holder     string
	generation uint64
	inputGate  sync.Mutex
	fenced     bool
	takeover   bool
	displaced  chan string
}

func controlDisplacement(owner *controlLeaseOwner) (string, bool) {
	if owner == nil {
		return "", false
	}
	select {
	case reason := <-owner.displaced:
		return reason, true
	default:
		return "", false
	}
}

func validTakeoverToken(value string) bool {
	if len(value) != 43 {
		return false
	}
	// Tokens are opaque string keys. Their wire shape is fixed;
	// the mint source guarantees entropy, so the validator checks only that shape.
	for index := 0; index < len(value); index++ {
		character := value[index]
		if !((character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func takeoverProtocolRequest(r *http.Request) (*http.Request, bool, bool) {
	var protocols []string
	takeover := false
	for _, raw := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, value := range strings.Split(raw, ",") {
			value = strings.TrimSpace(value)
			if value == "persea-takeover.v1" {
				if takeover {
					return r, false, false
				}
				takeover = true
				continue
			}
			protocols = append(protocols, value)
		}
	}
	if !takeover {
		return r, false, true
	}
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	clone.Header.Del("Sec-WebSocket-Protocol")
	clone.Header.Set("Sec-WebSocket-Protocol", strings.Join(protocols, ", "))
	return clone, true, true
}

func (s *leaseStore) newOwner(a proto.Authority, holder string, takeover bool) *controlLeaseOwner {
	s.mu.Lock()
	s.serial++
	ref := controlTakeoverOwnerRef{opaque: s.serial}
	s.mu.Unlock()
	return &controlLeaseOwner{ref: ref, authority: a, holder: holder, takeover: takeover, displaced: make(chan string, 1)}
}

func (s *leaseStore) acquireOwner(owner *controlLeaseOwner) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	key := authorityKey(owner.authority)
	if current, ok := s.entries[key]; ok && now.Before(current.expires) {
		return current.since.UnixMilli(), false
	}
	s.serial++
	owner.generation = s.serial
	s.entries[key] = leaseEntry{
		generation: owner.generation, holder: owner.holder, since: now,
		expires: now.Add(s.ttl), owner: owner,
	}
	return now.UnixMilli(), true
}

func (s *leaseStore) exactLocked(owner *controlLeaseOwner, current leaseEntry) bool {
	return current.generation == owner.generation && current.holder == owner.holder && current.owner == owner
}

func (s *leaseStore) renewOwner(owner *controlLeaseOwner) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := authorityKey(owner.authority)
	current, ok := s.entries[key]
	if !ok || !s.exactLocked(owner, current) || owner.fenced || !s.now().Before(current.expires) {
		return false
	}
	current.expires = s.now().Add(s.ttl)
	s.entries[key] = current
	return true
}

func (s *leaseStore) releaseOwner(owner *controlLeaseOwner) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := authorityKey(owner.authority)
	current, ok := s.entries[key]
	if !ok || !s.exactLocked(owner, current) {
		return false
	}
	delete(s.entries, key)
	return true
}

func (s *leaseStore) transfer(owner *controlLeaseOwner) controlTakeoverTupleSnapshot {
	key := authorityKey(owner.authority)
	for {
		s.mu.Lock()
		incumbent := s.entries[key]
		now := s.now()
		if incumbent.owner == nil || !now.Before(incumbent.expires) {
			s.serial++
			owner.generation = s.serial
			entry := leaseEntry{generation: owner.generation, holder: owner.holder, since: now, expires: now.Add(s.ttl), owner: owner}
			s.entries[key] = entry
			snapshot := takeoverTupleSnapshot(owner.authority, entry)
			checkpointControlTransfer(owner, snapshot)
			s.mu.Unlock()
			return snapshot
		}
		oldOwner := incumbent.owner
		s.mu.Unlock()

		oldOwner.inputGate.Lock()
		s.mu.Lock()
		now = s.now()
		current, ok := s.entries[key]
		if !ok || !s.exactLocked(oldOwner, current) {
			s.mu.Unlock()
			oldOwner.inputGate.Unlock()
			continue
		}
		oldOwner.fenced = true
		s.serial++
		owner.generation = s.serial
		entry := leaseEntry{generation: owner.generation, holder: owner.holder, since: now, expires: now.Add(s.ttl), owner: owner}
		s.entries[key] = entry
		reason := "control_displaced"
		if oldOwner.takeover {
			reason = "takeover_superseded"
		}
		snapshot := takeoverTupleSnapshot(owner.authority, entry)
		oldOwner.displaced <- reason
		checkpointControlTransfer(owner, snapshot)
		s.mu.Unlock()
		oldOwner.inputGate.Unlock()
		return snapshot
	}
}

func checkpointControlTransfer(owner *controlLeaseOwner, snapshot controlTakeoverTupleSnapshot) {
	if adapter := controlTakeoverTestAdapterHook; adapter != nil {
		adapter.Checkpoint(controlTakeoverCheckpoint{Kind: controlTakeoverTransferCommit, Owner: owner.ref, Tuple: snapshot})
	}
	if adapter := controlTakeoverTestAdapterHook; adapter != nil {
		adapter.Checkpoint(controlTakeoverCheckpoint{Kind: controlTakeoverStaleInjectionScheduling, Owner: owner.ref, Tuple: snapshot})
	}
}

func (s *leaseStore) ownerFenced(owner *controlLeaseOwner) bool {
	owner.inputGate.Lock()
	defer owner.inputGate.Unlock()
	return owner.fenced
}

func (s *leaseStore) useOwnerInput(owner *controlLeaseOwner, action func() bool) (bool, bool) {
	owner.inputGate.Lock()
	defer owner.inputGate.Unlock()
	if adapter := controlTakeoverTestAdapterHook; adapter != nil {
		adapter.Checkpoint(controlTakeoverCheckpoint{Kind: controlTakeoverBeforeInputGate, Owner: owner.ref})
	}
	if owner.fenced || !s.renewOwner(owner) {
		return false, false
	}
	ok := action()
	if adapter := controlTakeoverTestAdapterHook; adapter != nil {
		adapter.Checkpoint(controlTakeoverCheckpoint{Kind: controlTakeoverAfterInputGate, Owner: owner.ref})
	}
	return true, ok
}

func (s *leaseStore) renewOwnerGated(owner *controlLeaseOwner) bool {
	owner.inputGate.Lock()
	defer owner.inputGate.Unlock()
	return !owner.fenced && s.renewOwner(owner)
}

func (s *leaseStore) releaseOwnerObserved(owner *controlLeaseOwner) {
	if s.releaseOwner(owner) {
		return
	}
	if adapter := controlTakeoverTestAdapterHook; adapter != nil {
		adapter.Checkpoint(controlTakeoverCheckpoint{Kind: controlTakeoverStaleObservation, Owner: owner.ref, Operation: controlTakeoverStaleCleanup})
	}
	if adapter := controlTakeoverTestAdapterHook; adapter != nil {
		adapter.Checkpoint(controlTakeoverCheckpoint{Kind: controlTakeoverExactTupleSnapshot, Owner: owner.ref, Tuple: s.snapshot(owner.authority)})
	}
}

func (s *leaseStore) snapshot(a proto.Authority) controlTakeoverTupleSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return takeoverTupleSnapshot(a, s.entries[authorityKey(a)])
}

func takeoverTupleSnapshot(a proto.Authority, entry leaseEntry) controlTakeoverTupleSnapshot {
	snapshot := controlTakeoverTupleSnapshot{Authority: a, Generation: entry.generation, Holder: sha256.Sum256([]byte(entry.holder)), Expires: entry.expires}
	if entry.owner != nil {
		snapshot.Owner = entry.owner.ref
	}
	return snapshot
}

type takeoverOffer struct {
	authority proto.Authority
	operator  string
	expires   time.Time
	serial    uint64
	requestID string
}

type takeoverRequest struct {
	claim   string
	handle  string
	used    bool
	expires time.Time
}

type controlTakeoverStore struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	now      func() time.Time
	serial   uint64
	offers   map[string]takeoverOffer
	requests map[string]takeoverRequest
	handles  map[string]string
}

func newControlTakeoverStore(ttl time.Duration, capacity int) *controlTakeoverStore {
	return &controlTakeoverStore{ttl: ttl, capacity: capacity, now: time.Now, offers: make(map[string]takeoverOffer), requests: make(map[string]takeoverRequest), handles: make(map[string]string)}
}

func takeoverRequestKey(operator, requestID string) string { return operator + "\x00" + requestID }

func (s *controlTakeoverStore) recordOffer(token, operator string, authority proto.Authority) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	if _, exists := s.offers[token]; exists {
		return true
	}
	if len(s.offers) >= s.capacity {
		return false
	}
	s.serial++
	s.offers[token] = takeoverOffer{authority: authority, operator: operator, expires: s.now().Add(s.ttl), serial: s.serial}
	return true
}

func (s *controlTakeoverStore) authorize(operator, requestID, claimKind, claim string, resolveSource func() (proto.Authority, error), mint func(proto.Authority) (string, error)) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	key := takeoverRequestKey(operator, requestID)
	claimKey := claimKind + "\x00" + claim
	if prior, ok := s.requests[key]; ok {
		if prior.claim != claimKey {
			return "", false, errTakeoverConflict
		}
		if prior.used {
			return "", false, errTakeoverUsed
		}
		return prior.handle, true, nil
	}
	if len(s.requests) >= s.capacity {
		return "", false, errTakeoverCapacity
	}
	var authority proto.Authority
	if claimKind == "offer" {
		offer, ok := s.offers[claim]
		if !ok || offer.operator != operator || offer.requestID != "" {
			return "", false, errTakeoverUnavailable
		}
		authority = offer.authority
		offer.requestID = requestID
		s.offers[claim] = offer
	} else {
		var err error
		authority, err = resolveSource()
		if err != nil {
			return "", false, errTakeoverUnavailable
		}
	}
	handle, err := mint(authority)
	if err != nil {
		if claimKind == "offer" {
			offer := s.offers[claim]
			offer.requestID = ""
			s.offers[claim] = offer
		}
		return "", false, errTakeoverService
	}
	s.requests[key] = takeoverRequest{claim: claimKey, handle: handle, expires: s.now().Add(s.ttl)}
	s.handles[handle] = key
	return handle, false, nil
}

func (s *controlTakeoverStore) consume(handle string, consume func() (proto.Authority, error)) (proto.Authority, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	key, ok := s.handles[handle]
	if !ok {
		return proto.Authority{}, errInvalidHandle
	}
	authority, err := consume()
	if err != nil {
		return proto.Authority{}, err
	}
	request := s.requests[key]
	request.used = true
	s.requests[key] = request
	delete(s.handles, handle)
	return authority, nil
}

func (s *controlTakeoverStore) pruneLocked() {
	now := s.now()
	for token, offer := range s.offers {
		if !now.Before(offer.expires) {
			delete(s.offers, token)
		}
	}
	for key, request := range s.requests {
		if !now.Before(request.expires) {
			delete(s.handles, request.handle)
			delete(s.requests, key)
		}
	}
}

func writeTakeoverCode(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}

func (s *Server) logControlTakeover(event, code string, authority *proto.Authority, requestID string) {
	realm, server, session := "", "", ""
	if authority != nil {
		realm, server, session = authority.Realm, authority.Server, authority.SessionID
	}
	digest := sha256.Sum256([]byte(requestID))
	frontLogf("component=frontdoor event=%q code=%q realm=%q server=%q session=%q request_digest=%q", event, canonicalFailureCode(code), realm, server, session, hex.EncodeToString(digest[:6]))
}

func (s *Server) logControlDisplacement(reason string, authority *proto.Authority) {
	event := "control_displaced"
	if reason == "takeover_superseded" {
		event = "control_takeover_superseded"
	}
	s.logControlTakeover(event, reason, authority, "")
}

func (s *Server) controlTakeover(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
		writeTakeoverCode(w, http.StatusBadRequest, "invalid_takeover_request")
		return
	}
	var request struct {
		RequestID string `json:"request_id"`
		Offer     string `json:"offer"`
		Source    string `json:"source"`
	}
	if decodeAliasRequest(r, &request) != nil || !validTakeoverToken(request.RequestID) || (request.Offer == "") == (request.Source == "") {
		writeTakeoverCode(w, http.StatusBadRequest, "invalid_takeover_request")
		return
	}
	claimKind, claim := "offer", request.Offer
	if request.Source != "" {
		claimKind, claim = "source", request.Source
	}
	if !validTakeoverToken(claim) {
		writeTakeoverCode(w, http.StatusBadRequest, "invalid_takeover_request")
		return
	}
	operator := ""
	if id, ok := identity(r); ok {
		operator = id.operator
	}
	var resolved proto.Authority
	handle, idempotent, err := s.takeovers.authorize(
		operator, request.RequestID, claimKind, claim,
		func() (proto.Authority, error) {
			authority, resolveErr := s.bindings.resolve(operator, claim)
			if resolveErr == nil {
				resolved = authority
			}
			return authority, resolveErr
		},
		func(authority proto.Authority) (string, error) {
			resolved = authority
			return s.handles.mint(authority, operator, "control-takeover")
		},
	)
	if err != nil {
		status, code := http.StatusGone, "takeover_unavailable"
		if errors.Is(err, errTakeoverConflict) {
			status, code = http.StatusConflict, "takeover_request_conflict"
		} else if errors.Is(err, errTakeoverUsed) {
			status, code = http.StatusConflict, "takeover_request_used"
		} else if errors.Is(err, errTakeoverCapacity) {
			status, code = http.StatusServiceUnavailable, "takeover_capacity"
		} else if errors.Is(err, errTakeoverService) {
			status, code = http.StatusServiceUnavailable, "takeover_service_unavailable"
		}
		s.logControlTakeover("control_takeover_refused", code, nil, request.RequestID)
		writeTakeoverCode(w, status, code)
		return
	}
	event := "control_takeover_requested"
	status := http.StatusCreated
	if idempotent {
		event = "control_takeover_idempotent"
		status = http.StatusOK
	}
	s.logControlTakeover(event, "takeover_authorized", &resolved, request.RequestID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"handle": handle})
}

func (s *Server) consumeTakeoverHandle(handle, operator string) (proto.Authority, error) {
	return s.takeovers.consume(handle, func() (proto.Authority, error) {
		return s.handles.consume(handle, operator, "control-takeover")
	})
}
