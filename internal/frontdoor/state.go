package frontdoor

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"persea-terminal/internal/proto"
)

var errInvalidHandle = errors.New("invalid or expired snapshot handle")
var errInvalidSourceBinding = errors.New("invalid or expired attachment source binding")

type leaseEntry struct {
	generation uint64
	holder     string
	since      time.Time
	expires    time.Time
	owner      *controlLeaseOwner
}

type leaseStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	serial  uint64
	entries map[string]leaseEntry
}

func newLeaseStore(ttl time.Duration) *leaseStore {
	return &leaseStore{ttl: ttl, now: time.Now, entries: map[string]leaseEntry{}}
}

func authorityKey(a proto.Authority) string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%d\x00%d\x00%s\x00%d", a.Realm, a.Server, a.UID, a.SelectorKind, a.SelectorValue, a.BootID, a.ServerPID, a.ServerStart, a.SessionID, a.SessionCreated)
}

func (s *leaseStore) acquire(a proto.Authority, holder string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	key := authorityKey(a)
	if current, ok := s.entries[key]; ok && now.Before(current.expires) && current.holder != holder {
		return current.since.UnixMilli(), false
	}
	if current, ok := s.entries[key]; ok && current.holder == holder && now.Before(current.expires) {
		current.expires = now.Add(s.ttl)
		s.entries[key] = current
		return current.since.UnixMilli(), true
	}
	s.serial++
	s.entries[key] = leaseEntry{generation: s.serial, holder: holder, since: now, expires: now.Add(s.ttl)}
	return now.UnixMilli(), true
}

func (s *leaseStore) renew(a proto.Authority, holder string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := authorityKey(a)
	current, ok := s.entries[key]
	if !ok || current.holder != holder {
		return false
	}
	if !s.now().Before(current.expires) {
		delete(s.entries, key)
		return false
	}
	current.expires = s.now().Add(s.ttl)
	s.entries[key] = current
	return true
}

func (s *leaseStore) release(a proto.Authority, holder string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := authorityKey(a)
	if current, ok := s.entries[key]; ok && current.holder == holder {
		delete(s.entries, key)
	}
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type handleEntry struct {
	authority proto.Authority
	operator  string
	purpose   string
	expires   time.Time
	created   uint64
}
type handleStore struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	now      func() time.Time
	serial   uint64
	entries  map[string]handleEntry
}

func newHandleStore(ttl time.Duration, capacity int) *handleStore {
	return &handleStore{ttl: ttl, capacity: capacity, now: time.Now, entries: map[string]handleEntry{}}
}
func (s *handleStore) mint(a proto.Authority, binding ...string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	for len(s.entries) >= s.capacity {
		s.evictOldest()
	}
	h, err := randomToken()
	if err != nil {
		return "", err
	}
	s.serial++
	operator, purpose := "", "legacy"
	if len(binding) > 0 {
		operator = binding[0]
	}
	if len(binding) > 1 {
		purpose = binding[1]
	}
	s.entries[h] = handleEntry{authority: a, operator: operator, purpose: purpose, expires: s.now().Add(s.ttl), created: s.serial}
	return h, nil
}
func (s *handleStore) resolve(h string, binding ...string) (proto.Authority, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	e, ok := s.entries[h]
	// Legacy local APIs historically accept inventory handles. Capability purposes are
	// separated by construction, so a new purpose must be deliberately added to this allow-list rather than inherited by default.
	if !ok || (len(binding) == 0 && e.purpose != "legacy" && e.purpose != "alias" && e.purpose != "observe" && e.purpose != "control") || (len(binding) > 0 && e.operator != binding[0]) || (len(binding) > 1 && e.purpose != binding[1]) {
		return proto.Authority{}, errInvalidHandle
	}
	return e.authority, nil
}
func (s *handleStore) consume(h, operator, purpose string) (proto.Authority, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	e, ok := s.entries[h]
	if !ok || e.operator != operator || e.purpose != purpose {
		return proto.Authority{}, errInvalidHandle
	}
	delete(s.entries, h)
	return e.authority, nil
}
func (s *handleStore) consumeLegacy(h string) (proto.Authority, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	e, ok := s.entries[h]
	if !ok || e.purpose != "legacy" {
		return proto.Authority{}, errInvalidHandle
	}
	delete(s.entries, h)
	return e.authority, nil
}
func (s *handleStore) prune() {
	now := s.now()
	for h, e := range s.entries {
		if !now.Before(e.expires) {
			delete(s.entries, h)
		}
	}
}
func (s *handleStore) evictOldest() {
	var key string
	var serial uint64 = ^uint64(0)
	for h, e := range s.entries {
		if e.created < serial {
			key, serial = h, e.created
		}
	}
	delete(s.entries, key)
}

type sourceBindingEntry struct {
	authority proto.Authority
	expires   time.Time
	serial    uint64
	active    uint32
}

type sourceBindingStore struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	now      func() time.Time
	serial   uint64
	entries  map[string]sourceBindingEntry
}

func newSourceBindingStore(ttl time.Duration, capacity int) *sourceBindingStore {
	return &sourceBindingStore{ttl: ttl, capacity: capacity, now: time.Now, entries: map[string]sourceBindingEntry{}}
}

func sourceBindingKey(operator, source string) string { return operator + "\x00" + source }

func (s *sourceBindingStore) bind(operator, source string, authority proto.Authority) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	key := sourceBindingKey(operator, source)
	now := s.now()
	entry, exists := s.entries[key]
	if exists {
		if authorityKey(entry.authority) != authorityKey(authority) {
			return nil, errInvalidSourceBinding
		}
		entry.active++
		entry.expires = now.Add(s.ttl)
		s.serial++
		entry.serial = s.serial
		s.entries[key] = entry
	} else {
		for len(s.entries) >= s.capacity && s.evictOldestInactiveLocked() {
		}
		if len(s.entries) >= s.capacity {
			return nil, errInvalidSourceBinding
		}
		s.serial++
		s.entries[key] = sourceBindingEntry{authority: authority, expires: now.Add(s.ttl), serial: s.serial, active: 1}
	}
	var once sync.Once
	return func() { once.Do(func() { s.release(key) }) }, nil
}

func (s *sourceBindingStore) resolve(operator, source string) (proto.Authority, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	entry, ok := s.entries[sourceBindingKey(operator, source)]
	if !ok {
		return proto.Authority{}, errInvalidSourceBinding
	}
	return entry.authority, nil
}

func (s *sourceBindingStore) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok || entry.active == 0 {
		return
	}
	entry.active--
	entry.expires = s.now().Add(s.ttl)
	s.serial++
	entry.serial = s.serial
	s.entries[key] = entry
}

func (s *sourceBindingStore) pruneLocked() {
	now := s.now()
	for key, entry := range s.entries {
		if entry.active == 0 && !now.Before(entry.expires) {
			delete(s.entries, key)
		}
	}
}

func (s *sourceBindingStore) evictOldestInactiveLocked() bool {
	var key string
	oldest := ^uint64(0)
	for candidate, entry := range s.entries {
		if entry.active == 0 && entry.serial < oldest {
			key, oldest = candidate, entry.serial
		}
	}
	if key == "" {
		return false
	}
	delete(s.entries, key)
	return true
}
