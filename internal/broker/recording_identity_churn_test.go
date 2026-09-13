package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"persea-terminal/internal/unifiedjournal"
)

func TestRecordingProductionRotationIdentityChurnWithOldOwners(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	session := fixture.startPaneCommand(t, "identity-churn", "printf 'identity-proof\\n'; sleep 120")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adopted, err := fixture.effects.AdoptSession(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _, subscriber, detach, err := fixture.effects.openSnapshotTail(session)
	if err != nil {
		t.Fatal(err)
	}
	defer detach()
	defer subscriber.releaseSnapshot()
	if len(snapshot) == 0 {
		t.Fatal("missing held snapshot")
	}
	firstPayload := append([]byte(nil), snapshot[0].Payload...)
	key := adopted.Key
	for cycle := 0; cycle < unifiedjournal.MaxRealmIdentities+4; cycle++ {
		fixture.effects.journalMu.Lock()
		writer, err := fixture.effects.realm.BindWriter(key)
		fixture.effects.journalMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.effects.rotateSession(ctx, session); err != nil {
			t.Fatalf("cycle%d: %v", cycle, err)
		}
		fixture.effects.journalMu.Lock()
		identities := fixture.effects.realm.IdentityCount()
		_, staleErr := writer.Append(key, []byte("late"))
		fixture.effects.journalMu.Unlock()
		if identities != 1 || !errors.Is(staleErr, unifiedjournal.ErrInvalidated) {
			t.Fatalf("cycle%d identities=%d stale=%v", cycle, identities, staleErr)
		}
		key, _ = fixture.effects.paneKey(session)
	}
	if string(snapshot[0].Payload) != string(firstPayload) {
		t.Fatal("retired identity invalidated independent snapshot copy")
	}
	usage := fixture.effects.readers.snapshot()
	if usage.Snapshots != 1 || usage.Bytes == 0 {
		t.Fatal("held old snapshot was refunded by rotation")
	}
	snapshot = nil
	subscriber.releaseSnapshot()
	detach()
	if usage := fixture.effects.readers.snapshot(); usage.Bytes != 0 || usage.Readers != 0 {
		t.Fatal("reader settlement did not plateau", usage)
	}
}
