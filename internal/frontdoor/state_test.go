package frontdoor

import (
	"bytes"
	"os"
	"testing"
	"time"

	"persea-terminal/internal/proto"
)

func auth(pid int, id string) proto.Authority {
	return proto.Authority{Realm: "r", Server: "s", UID: uint32(os.Getuid()), SelectorKind: "socket_name", SelectorValue: "test", BootID: "boot", ServerPID: pid, ServerStart: uint64(pid), SessionID: id, SessionCreated: int64(pid)}
}
func TestHandleStoreRandomTTLAndCapacity(t *testing.T) {
	now := time.Unix(100, 0)
	s := newHandleStore(time.Second, 2)
	s.now = func() time.Time { return now }
	h1, e := s.mint(auth(1, "$0"))
	if e != nil {
		t.Fatal(e)
	}
	h2, _ := s.mint(auth(1, "$1"))
	if h1 == h2 || len(h1) < 22 {
		t.Fatal("handles not random opaque")
	}
	h3, _ := s.mint(auth(1, "$2"))
	if _, e = s.resolve(h1); e == nil {
		t.Fatal("capacity did not evict oldest")
	}
	if _, e = s.resolve(h3); e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Second)
	if _, e = s.resolve(h2); e == nil {
		t.Fatal("expired handle resolved")
	}
}

func TestAuthorityKeyBindsCompleteIncarnation(t *testing.T) {
	base := auth(7, "$3")
	mutations := []func(*proto.Authority){
		func(a *proto.Authority) { a.UID++ },
		func(a *proto.Authority) { a.SelectorKind = "socket_path" },
		func(a *proto.Authority) { a.SelectorValue += "-other" },
		func(a *proto.Authority) { a.SessionCreated++ },
	}
	for i, mutate := range mutations {
		changed := base
		mutate(&changed)
		if authorityKey(base) == authorityKey(changed) {
			t.Fatalf("mutation %d did not change authority key", i)
		}
	}
}

func TestSourceBindingPinsCompleteAuthorityOperatorTTLAndCapacity(t *testing.T) {
	now := time.Unix(1_000, 0)
	store := newSourceBindingStore(time.Second, 2)
	store.now = func() time.Time { return now }
	first := auth(7, "$3")
	releaseFirst, err := store.bind("operator-a", "source-a", first)
	if err != nil {
		t.Fatal(err)
	}
	if got, resolveErr := store.resolve("operator-a", "source-a"); resolveErr != nil || authorityKey(got) != authorityKey(first) {
		t.Fatalf("active source did not resolve exact authority: %+v %v", got, resolveErr)
	}
	if _, resolveErr := store.resolve("operator-b", "source-a"); resolveErr == nil {
		t.Fatal("source crossed operator binding")
	}
	changed := first
	changed.SessionCreated++
	if _, bindErr := store.bind("operator-a", "source-a", changed); bindErr == nil {
		t.Fatal("same source accepted a different incarnation")
	}
	now = now.Add(2 * time.Second)
	if _, resolveErr := store.resolve("operator-a", "source-a"); resolveErr != nil {
		t.Fatal("active binding expired")
	}
	releaseFirst()
	releaseFirst()
	if _, resolveErr := store.resolve("operator-a", "source-a"); resolveErr != nil {
		t.Fatal("released binding lost its bounded reconnect grace")
	}
	now = now.Add(time.Second)
	if _, resolveErr := store.resolve("operator-a", "source-a"); resolveErr == nil {
		t.Fatal("released binding survived its reconnect grace")
	}

	releaseSecond, err := store.bind("operator-a", "source-b", auth(8, "$4"))
	if err != nil {
		t.Fatal(err)
	}
	releaseThird, err := store.bind("operator-a", "source-c", auth(9, "$5"))
	if err != nil {
		t.Fatal(err)
	}
	if _, bindErr := store.bind("operator-a", "source-d", auth(10, "$6")); bindErr == nil {
		t.Fatal("capacity evicted an active binding")
	}
	releaseSecond()
	releaseFourth, err := store.bind("operator-a", "source-d", auth(10, "$6"))
	if err != nil {
		t.Fatal(err)
	}
	if _, resolveErr := store.resolve("operator-a", "source-b"); resolveErr == nil {
		t.Fatal("capacity did not evict the oldest inactive binding")
	}
	releaseThird()
	releaseFourth()
}

func TestFrontSnapshotBounds(t *testing.T) {
	p := []byte{}
	for i := 0; i < 10000; i++ {
		p = append(p, []byte("line\n")...)
	}
	out, tr := boundFrontSnapshot(p, false, SnapshotLineLimit+24)
	if !tr || len(out) > SnapshotByteLimit || bytes.Count(out, []byte{'\n'}) > SnapshotLineLimit+24 {
		t.Fatalf("front bounds failed %d %v", len(out), tr)
	}
}
