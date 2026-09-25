// Unified session birth and initial recording lifecycle.
package broker

import (
	"context"
	"errors"
	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/terminal"
	"persea-terminal/internal/unifiedjournal"
	"sync"
	"time"
)

type unifiedDevBirth struct {
	memory        *recordingUnitMemory
	pendingCharge int64
	owner         *UnifiedDevPaneEffects
	server        string
	name          string

	mu               sync.Mutex
	witness          controlmode.PaneWitness
	source           terminal.SourceWitness
	geometry         unifiedjournal.Geometry
	initial          *recordingInitialOperation
	streaming        bool
	observerVerified bool
	pending          []controlmode.Observation
	ready            chan struct{}
	readyOnce        sync.Once
	committed        bool
	aborted          bool
	rotation         *unifiedRotationPending
}

func (birth *unifiedDevBirth) CreateSession(server config.TmuxServer, args []string) (string, error) {
	if server.Label != birth.server || len(args) < 2 {
		return "", errors.New("invalid unified session birth request")
	}
	memory, err := birth.owner.transients.acquire()
	if err != nil {
		return "", err
	}
	transferred := false
	defer func() {
		if !transferred {
			memory.done()
		}
	}()
	result := make(chan unifiedDevCommandResult, 1)
	select {
	case birth.owner.spawns <- unifiedDevCommand{spawnDeadline: time.Now().Add(5 * time.Second), memory: memory, server: server, args: append([]string(nil), args...), birth: birth, done: result}:
		transferred = true
	case <-time.After(5 * time.Second):
		return "", errors.New("unified session birth observer unavailable")
	}
	select {
	case outcome := <-result:
		return outcome.session, outcome.err
	case <-time.After(5 * time.Second):
		return "", errors.New("unified session birth timed out")
	}
}

func (birth *unifiedDevBirth) CommitSessionBirth(sessionID string) error {
	if !birth.memory.Reserve(int64(adoptionBootstrapCapBytes)) {
		return errRecordingTransients
	}
	defer birth.memory.Release(int64(adoptionBootstrapCapBytes))
	select {
	case <-birth.ready:
	case <-time.After(2 * time.Second):
		return errors.New("unified session birth observer is not ready")
	}
	if err := birth.captureInitialIfEmpty(); err != nil {
		return err
	}
	birth.mu.Lock()
	if birth.aborted || birth.committed || birth.streaming || birth.witness.Session.Session != sessionID || len(birth.pending) == 0 {
		birth.mu.Unlock()
		return errors.New("unified session birth has no byte-zero output")
	}
	observer := birth.owner.initialObserver()
	if observer == nil {
		birth.mu.Unlock()
		return errRecordingInitial
	}
	registry := observer.recordingRegistry()
	geometry := birth.geometry
	if geometry.Columns == 0 {
		birth.owner.journalMu.Lock()
		geometry, _ = birth.owner.realm.InitialGeometry(journalKey(birth.witness))
		birth.owner.journalMu.Unlock()
	}
	size := 0
	for _, observation := range birth.pending {
		size += len(observation.Data)
	}
	payload := make([]byte, 0, size)
	for _, observation := range birth.pending {
		if observation.Witness != birth.witness || observation.Kind != controlmode.ObservationOutput {
			birth.mu.Unlock()
			return errRecordingInitial
		}
		payload = append(payload, observation.Data...)
	}
	op, err := observer.beginInitial(recordingInitialBirth, birth.witness, birth.source, geometry, payload)
	if err != nil {
		birth.mu.Unlock()
		return err
	}
	birth.initial = op
	birth.clearPendingLocked()
	birth.streaming = true
	birth.mu.Unlock()
	birth.owner.mu.Lock()
	initialUnit := birth.owner.units[sessionID]
	birth.owner.mu.Unlock()
	op.bindOwner(initialUnit)
	<-op.done
	if _, err := op.result(); err != nil {
		return settleInitial(op, registry, err)
	}
	if err := birth.verifyInitialSource(context.Background()); err != nil {
		return settleInitial(op, registry, err)
	}
	err = func() error {
		birth.mu.Lock()
		defer birth.mu.Unlock()
		if birth.aborted || birth.initial != op {
			return errRecordingInitial
		}
		key := journalKey(birth.witness)
		birth.owner.mu.Lock()
		defer birth.owner.mu.Unlock()
		unit := birth.owner.units[sessionID]
		if unit == nil || unit != initialUnit || unit.holder != birth || !unit.isLive() {
			return errRecordingInitial
		}
		if err := observer.publishInitial(op); err != nil {
			return err
		}
		birth.committed = true
		birth.owner.active[sessionID] = key
		return nil
	}()
	if err != nil {
		return settleInitial(op, registry, err)
	}
	return nil
}

func (birth *unifiedDevBirth) AbortSessionBirth(error) error {
	birth.mu.Lock()
	if birth.committed {
		birth.mu.Unlock()
		return nil
	}
	birth.aborted = true
	birth.clearPendingLocked()
	pane := birth.witness.Pane
	op := birth.initial
	birth.mu.Unlock()
	if op != nil {
		if observer := birth.owner.initialObserver(); observer != nil {
			_ = settleInitial(op, observer.recordingRegistry(), errRecordingInitial)
		}
	}
	if pane != "" {
		birth.owner.mu.Lock()
		if birth.owner.panes[pane] == birth {
			delete(birth.owner.panes, pane)
		}
		birth.owner.mu.Unlock()
	}
	return nil
}

func (birth *unifiedDevBirth) observe(data []byte) error {
	birth.mu.Lock()
	defer birth.mu.Unlock()
	if birth.aborted {
		return unifiedjournal.ErrInvalidated
	}
	if !birth.committed && birth.rotation != nil {
		return birth.rotation.append(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: birth.witness, Data: data})
	}
	if !birth.streaming && !birth.committed {
		if err := birth.appendPendingLocked(data); err != nil {
			return err
		}
		if birth.observerVerified {
			birth.readyOnce.Do(func() { close(birth.ready) })
		}
		return nil
	}
	// ObservePane synchronously reserves and copies accepted data. This caller
	// keeps the decoder batch alive through that copy; no intermediate clone.
	return birth.owner.observer.ObservePane(controlmode.Observation{Kind: controlmode.ObservationOutput, Witness: birth.witness, Data: data})
}
