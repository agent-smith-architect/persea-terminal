package frontdoor

// Lease ABA regression: a stale holder must never mutate a reacquired lease.
//
// Observed defect in the in-progress leaseStore.renew: its failure branch
// (!ok || holder mismatch || expired) executed delete(entries, key), so a
// STALE holder's renew after a reacquire deleted the NEW holder's live
// lease. Required semantics:
//   - renew on missing key      -> false, no mutation;
//   - renew by non-holder       -> false, NO mutation of the live entry;
//   - only the MATCHING holder may delete/expire its own expired entry;
//   - release (incl. deferred WS-close release) stays compare-and-release.
//
// Keep these assertions when adapting a future lease API. Broker dial/attach
// rollback remains a server-flow property covered by the adjacent tests.

import (
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/proto"
)

func abaAuthority() proto.Authority {
	return proto.Authority{Realm: "r", Server: "s", UID: 1000, SelectorKind: "socket_name", SelectorValue: "test", BootID: "b", ServerPID: 1, ServerStart: 2, SessionID: "$1", SessionCreated: 3}
}

func TestLeaseRenewByStaleHolderMustNotMutateLiveLease(t *testing.T) {
	ls := newLeaseStore(time.Minute)
	cur := time.Unix(1000, 0)
	ls.now = func() time.Time { return cur }
	a := abaAuthority()

	if _, ok := ls.acquire(a, "token-A"); !ok {
		t.Fatal("A must acquire an empty lease")
	}
	cur = cur.Add(2 * time.Minute) // A expires
	if _, ok := ls.acquire(a, "token-B"); !ok {
		t.Fatal("B must acquire after A expired")
	}
	if ls.renew(a, "token-A") {
		t.Fatal("stale A renew must return false after B reacquired")
	}
	// B's live lease must be fully intact after A's stale renew.
	if !ls.renew(a, "token-B") {
		t.Fatal("ABA: stale A renew mutated/deleted B's live lease")
	}
	if _, ok := ls.acquire(a, "token-C"); ok {
		t.Fatal("ABA: C could acquire while B holds a live lease")
	}
}

func TestLeaseRenewMissingKeyDoesNotCreate(t *testing.T) {
	ls := newLeaseStore(time.Minute)
	a := abaAuthority()
	if ls.renew(a, "token-A") {
		t.Fatal("renew of a missing lease must fail")
	}
	if _, ok := ls.acquire(a, "token-B"); !ok {
		t.Fatal("acquire after failed renew must succeed (renew must not create state)")
	}
}

func TestLeaseExpiryReacquireOldCloseDoesNotReleaseNewHolder(t *testing.T) {
	ls := newLeaseStore(time.Minute)
	cur := time.Unix(1000, 0)
	ls.now = func() time.Time { return cur }
	a := abaAuthority()

	if _, ok := ls.acquire(a, "token-A"); !ok {
		t.Fatal("A must acquire")
	}
	cur = cur.Add(2 * time.Minute)
	if _, ok := ls.acquire(a, "token-B"); !ok {
		t.Fatal("B must acquire after expiry")
	}
	ls.release(a, "token-A") // A's late deferred WS-close release
	if _, ok := ls.acquire(a, "token-C"); ok {
		t.Fatal("ABA: A's late release freed B's live lease")
	}
	if !ls.renew(a, "token-B") {
		t.Fatal("B's lease must survive A's late release")
	}
}

func TestLeaseConcurrentAcquireSingleWinner(t *testing.T) {
	ls := newLeaseStore(time.Minute)
	a := abaAuthority()
	const n = 32
	var wg sync.WaitGroup
	wins := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if _, ok := ls.acquire(a, string(rune('a'+id%26))+"-token-"+string(rune('0'+id%10))); ok {
				wins <- id
			}
		}(i)
	}
	wg.Wait()
	close(wins)
	count := 0
	for range wins {
		count++
	}
	if count != 1 {
		t.Fatalf("expected exactly one lease winner, got %d", count)
	}
}

func TestLeaseRenewAndInputGuardInterleave(t *testing.T) {
	store := newLeaseStore(time.Minute)
	a := abaAuthority()
	if _, ok := store.acquire(a, "holder"); !ok {
		t.Fatal("initial acquire failed")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 1000; n++ {
				_ = store.renew(a, "holder")
			}
		}()
	}
	wg.Wait()
	if !store.renew(a, "holder") {
		t.Fatal("concurrent renew/input guards lost the current holder")
	}
}
