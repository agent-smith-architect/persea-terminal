package broker

import (
	"testing"

	"persea-terminal/internal/unifiedjournal"
)

// Existing ordering/readiness fixtures finish using their snapshot when they
// cancel the read. Production has distinct PREPARE/backlog and tail lifetimes;
// this helper states the fixtures' simpler final-consumer boundary explicitly.
// Resource ownership tests call openSnapshotTail directly to keep them separate.
func openSnapshotTailForTest(t *testing.T, effects interface {
	openSnapshotTail(string) ([]unifiedjournal.Event, unifiedjournal.Geometry, *unifiedDevSubscriber, func(), error)
}, session string) ([]unifiedjournal.Event, unifiedjournal.Geometry, *unifiedDevSubscriber, func(), error) {
	t.Helper()
	events, geometry, tail, cancel, err := effects.openSnapshotTail(session)
	if err != nil {
		return events, geometry, tail, cancel, err
	}
	finish := func() {
		cancel()
		tail.releaseSnapshot()
	}
	t.Cleanup(finish)
	return events, geometry, tail, finish, nil
}
