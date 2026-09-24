package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/controlmode"
	"persea-terminal/internal/unifiedjournal"
)

// A transport cut after registry/journal commit but before rotation_pending is
// still an exact-owner post-PONR failure. The correction contract requires the
// possible successor generation and its charge to remain until authoritative
// source reconciliation; transport loss alone cannot queue successor cleanup.
func TestRefitSettlementPostRegistryTransportLossRetainsUncertainSuccessor(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "refit-post-registry-loss", `sh -c 'stty -echo; printf "REFIT-POST-REGISTRY\n"; while :; do sleep 1; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 10*time.Second, "predecessor output", func() bool {
		return strings.Contains(string(fixture.journalBytes(t, adoption.Key)), "REFIT-POST-REGISTRY")
	})
	detail, err := details(fixture.server, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(fixture.server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = fixture.cfg.Realm, fixture.server.Label, detail.ID, detail.Created
	source, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	targetColumns := source.Columns + 9
	slotsBefore := fixture.effects.realm.AvailableCompletePaneSlots()
	var atPending atomic.Bool
	var successorKey unifiedjournal.PaneKey
	var successorWitness controlmode.PaneWitness
	fixture.effects.rotationEdge = func(_ string, edge string) {
		if edge != "before_pending_wait" {
			return
		}
		fixture.effects.mu.Lock()
		rotation := fixture.effects.rotation
		fixture.effects.mu.Unlock()
		if rotation == nil || !rotation.refit || !rotation.registryCommitted || !rotation.journalCommitted {
			t.Errorf("pending edge did not carry a committed refit successor: %+v", rotation)
			return
		}
		successorKey = rotation.newKey
		successorWitness = rotation.next
		atPending.Store(true)
	}
	fixture.effects.rotationFault = func(_ string, edge string) error {
		if edge == "boundary_wait" && atPending.Load() {
			return io.EOF
		}
		return nil
	}
	err = fixture.effects.refitSession(ctx, authority, source, targetColumns, strings.Repeat("r", 43))
	if !errors.Is(err, ErrUnifiedRefitFatal) {
		t.Fatalf("post-registry transport cut=%v want typed refit fatal", err)
	}
	if successorKey == (unifiedjournal.PaneKey{}) {
		t.Fatal("post-registry transport cut did not capture successor")
	}
	after, witnessErr := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if witnessErr != nil || after.Socket != source.Socket || after.Server != source.Server || after.SessionID != source.SessionID ||
		after.WindowID != source.WindowID || after.PaneID != successorWitness.Pane || after.Pane != source.Pane || after.Columns != targetColumns {
		t.Fatalf("exact changed-width tmux owner did not survive: before=%+v successor=%+v after=%+v err=%v", source, successorWitness, after, witnessErr)
	}
	fixture.registry.retention.mu.Lock()
	_, retained := fixture.registry.retention.generations[successorKey]
	fixture.registry.retention.mu.Unlock()
	if !retained || fixture.effects.realm.AvailableCompletePaneSlots() != slotsBefore {
		t.Fatalf("exact-owner post-registry loss cleaned uncertain successor: retained=%t slots=%d before=%d", retained, fixture.effects.realm.AvailableCompletePaneSlots(), slotsBefore)
	}
}

// The broker control handler rebuilds the current source witness on every
// request and does not receive the browser's original witness. A lost-success
// retry must therefore locate the settled token before geometry/current-source
// validation, even though the recomputed incarnation now describes the
// changed-width successor.
func TestRefitSettlementRevalidatedExactRetryFindsSettledOperation(t *testing.T) {
	fixture := newAdoptionFixture(t, 4)
	sessionID := fixture.startPaneCommand(t, "refit-revalidated-retry", `sh -c 'stty -echo; while :; do sleep 1; done'`)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := fixture.effects.AdoptSession(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	detail, err := details(fixture.server, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := incarnation(fixture.server)
	if err != nil {
		t.Fatal(err)
	}
	authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = fixture.cfg.Realm, fixture.server.Label, detail.ID, detail.Created
	source, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	columns := source.Columns + 9
	operation := strings.Repeat("q", 43)
	if err := fixture.effects.refitSession(ctx, authority, source, columns, operation); err != nil {
		t.Fatalf("first refit: %v", err)
	}
	revalidated, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if revalidated.Incarnation == source.Incarnation || revalidated.Columns != columns {
		t.Fatalf("refit did not produce the broker's changed current witness: before=%+v after=%+v", source, revalidated)
	}
	if err := fixture.effects.refitSession(ctx, authority, revalidated, columns, operation); err != nil {
		t.Fatalf("same broker operation after current-source revalidation was not idempotent: %v", err)
	}
}

// Exercise the real changed-width refit on both sides of registry commit. The
// exact/ambiguous cases must not elect durable cleanup; proved gone/replacement
// cases must converge through success, ENOENT and a retryable unlink failure.
func TestRefitSettlementRealDispositionAndUnlinkMatrix(t *testing.T) {
	type dispositionCase struct {
		name       string
		uncertain  bool
		mutateTmux func(*adoptionFixture, string)
	}
	dispositions := []dispositionCase{
		{name: "exact", uncertain: true},
		{name: "ambiguous", uncertain: true, mutateTmux: func(f *adoptionFixture, _ string) {
			f.effects.server.SocketPath += ".unreachable"
			f.effects.sessionPresence = func(config.TmuxServer, string) (bool, bool) { return false, false }
		}},
		{name: "owner_gone", mutateTmux: func(f *adoptionFixture, session string) {
			f.disposable.run("kill-session", "-t", session)
		}},
		{name: "replacement", mutateTmux: func(f *adoptionFixture, session string) {
			f.disposable.run("respawn-pane", "-k", "-t", session+":", `sh -c 'stty -echo; while :; do sleep 1; done'`)
		}},
	}
	uncertainFingerprints := make(map[string]string)
	matrixCells := 0
	for _, cut := range []struct{ name, edge string }{{"pre_registry", "before_bootstrap_wait"}, {"post_registry", "before_pending_wait"}} {
		for _, disposition := range dispositions {
			// Keep the literal 2 x 4 x 3 matrix in-tree. Exact and ambiguous
			// owners must not attempt cleanup, so all three injected unlink
			// labels must preserve byte-identical accounting authority.
			modes := []string{"success", "enoent", "eio"}
			for _, unlinkMode := range modes {
				t.Run(fmt.Sprintf("%s/%s/%s", cut.name, disposition.name, unlinkMode), func(t *testing.T) {
					matrixCells++
					fixture := newAdoptionFixture(t, 4)
					initialSlots := fixture.effects.realm.AvailableCompletePaneSlots()
					initialLogical, initialLogicalReserved, _ := fixture.effects.realm.LogicalBudget()
					initialPhysical, initialPhysicalReserved, _ := fixture.effects.realm.PhysicalBudget()
					sessionID := fixture.startPaneCommand(t, "refit-matrix", `sh -c 'stty -echo; while :; do sleep 1; done'`)
					ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
					defer cancel()
					if _, err := fixture.effects.AdoptSession(ctx, sessionID); err != nil {
						t.Fatal(err)
					}
					detail, err := details(fixture.server, sessionID)
					if err != nil {
						t.Fatal(err)
					}
					authority, err := incarnation(fixture.server)
					if err != nil {
						t.Fatal(err)
					}
					authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = fixture.cfg.Realm, fixture.server.Label, detail.ID, detail.Created
					source, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
					if err != nil {
						t.Fatal(err)
					}
					slotsBefore := fixture.effects.realm.AvailableCompletePaneSlots()
					filesBefore := fixture.journalFileCount(t)
					var armed atomic.Bool
					var capturedOld, capturedNew unifiedjournal.PaneKey
					var capturedPredecessor *retentionGeneration
					changedDirs := map[string]struct{}{}
					t.Cleanup(func() {
						for dir := range changedDirs {
							_ = os.Chmod(dir, 0o700)
						}
					})
					fixture.effects.rotationEdge = func(_ string, edge string) {
						if edge != cut.edge || armed.Swap(true) {
							return
						}
						fixture.effects.mu.Lock()
						rotation := fixture.effects.rotation
						fixture.effects.mu.Unlock()
						if rotation == nil || !rotation.refit {
							t.Errorf("missing real refit at %s", edge)
							return
						}
						capturedOld, capturedNew = rotation.oldKey, rotation.newKey
						if rotation.registryT != nil {
							capturedPredecessor = rotation.registryT.predecessorGeneration
						}
						if disposition.mutateTmux != nil {
							disposition.mutateTmux(fixture, sessionID)
						}
						if disposition.uncertain {
							return
						}
						files, listErr := rotationJournalFiles(fixture.runtimeDir)
						if listErr != nil {
							t.Errorf("list journals: %v", listErr)
							return
						}
						for _, path := range files {
							switch unlinkMode {
							case "enoent":
								if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
									t.Errorf("remove %s: %v", path, removeErr)
								}
							case "eio":
								dir := filepath.Dir(path)
								if _, done := changedDirs[dir]; !done {
									if chmodErr := os.Chmod(dir, 0o500); chmodErr != nil {
										t.Errorf("chmod %s: %v", dir, chmodErr)
									}
									changedDirs[dir] = struct{}{}
								}
							}
						}
					}
					fixture.effects.rotationFault = func(_ string, edge string) error {
						if edge == "boundary_wait" && armed.Load() {
							return io.EOF
						}
						return nil
					}
					err = fixture.effects.refitSession(ctx, authority, source, source.Columns+9, strings.Repeat("m", 43))
					if unlinkMode == "eio" && !disposition.uncertain {
						deadline := time.Now().Add(3 * time.Second)
						for {
							fixture.registry.retention.mu.Lock()
							generation := fixture.registry.retention.generations[capturedOld]
							retirement := fixture.registry.retention.durableRetires[capturedOld]
							pUsed, qUsed := fixture.registry.retention.pUsed, fixture.registry.retention.qUsed
							durable := generation != nil && generation.durableRequested
							lifetimeReleased := generation != nil && generation.lifetimeReleased
							runtimeOwner := generation != nil && generation == capturedPredecessor && durable && !lifetimeReleased && pUsed > 0 && qUsed >= 2
							// The retiring seal may already have settled this exact predecessor.
							// Its journal retry must then survive without a recreated P/Q lifetime.
							settledOwner := generation == nil && capturedPredecessor != nil && capturedPredecessor.key == capturedOld && capturedPredecessor.lifetimeReleased && pUsed == 0 && qUsed == 0
							fixture.registry.retention.mu.Unlock()
							if cut.name != "pre_registry" || (retirement != nil && (runtimeOwner || settledOwner)) {
								logical, logicalReserved, _ := fixture.effects.realm.LogicalBudget()
								physical, physicalReserved, _ := fixture.effects.realm.PhysicalBudget()
								if fixture.effects.realm.AvailableCompletePaneSlots() != initialSlots || fixture.journalFileCount(t) == 0 || logical <= initialLogical || physical <= initialPhysical || logicalReserved != initialLogicalReserved || physicalReserved != initialPhysicalReserved {
									t.Fatalf("EIO did not retain proof-bearing byte/file charge: slots=%d/%d files=%d logical=%d/%d physical=%d/%d reserved=%d/%d", fixture.effects.realm.AvailableCompletePaneSlots(), initialSlots, fixture.journalFileCount(t), logical, initialLogical, physical, initialPhysical, logicalReserved, physicalReserved)
								}
								break
							}
							if time.Now().After(deadline) {
								t.Fatalf("EIO lost durable runtime owner before unlink proof: generation=%v durable=%t lifetime_released=%t retry=%t p=%d q=%d", generation != nil, durable, lifetimeReleased, retirement != nil, pUsed, qUsed)
							}
							time.Sleep(10 * time.Millisecond)
						}
					}
					for dir := range changedDirs {
						if chmodErr := os.Chmod(dir, 0o700); chmodErr != nil {
							t.Fatal(chmodErr)
						}
					}
					if unlinkMode == "eio" && !disposition.uncertain {
						// Let runtime-owned predecessor/successor retries consume the
						// restored unlink proof. Only remaining journal-only tombstones
						// use the explicit journal retry API.
						for _, key := range []unifiedjournal.PaneKey{capturedOld, capturedNew} {
							runtime := fixture.registry.retention
							runtime.mu.Lock()
							generation := runtime.generations[key]
							runtimeOwned := runtime.durableRetires[key] != nil || (generation != nil && generation.durableRequested)
							runtime.mu.Unlock()
							if runtimeOwned {
								// Observe the existing retry owner; a direct journal call would bypass its
								// completion and leave a live timer after the bytes happened to reach zero.
								pollUntil(t, 3*time.Second, "restored unlink proof settles runtime retry owner", func() bool {
									runtime.mu.Lock()
									defer runtime.mu.Unlock()
									return runtime.generations[key] == nil && runtime.durableRetires[key] == nil
								})
								continue
							}
							// A journal-only tombstone has no runtime retry owner to drive its unlink.
							if !fixture.effects.retirePane(key) {
								t.Fatalf("retry journal-only tombstone %+v did not prove unlink", key)
							}
						}

					}
					if !errors.Is(err, ErrUnifiedRefitFatal) {
						t.Fatalf("refit fatal=%v", err)
					}
					if !armed.Load() || capturedOld == (unifiedjournal.PaneKey{}) || capturedNew == (unifiedjournal.PaneKey{}) {
						t.Fatalf("cut not reached old=%+v new=%+v", capturedOld, capturedNew)
					}
					if disposition.uncertain {
						fixture.registry.retention.mu.Lock()
						oldGeneration := fixture.registry.retention.generations[capturedOld]
						newGeneration := fixture.registry.retention.generations[capturedNew]
						_, oldRetirement := fixture.registry.retention.durableRetires[capturedOld]
						_, newRetirement := fixture.registry.retention.durableRetires[capturedNew]
						pUsed, qUsed := fixture.registry.retention.pUsed, fixture.registry.retention.qUsed
						oldRetained, newRetained := oldGeneration != nil, newGeneration != nil
						oldDurable := oldGeneration != nil && oldGeneration.durableRequested
						newDurable := newGeneration != nil && newGeneration.durableRequested
						fixture.registry.retention.mu.Unlock()
						fixture.effects.journalMu.Lock()
						logical, logicalReserved, _ := fixture.effects.realm.LogicalBudget()
						physical, physicalReserved, _ := fixture.effects.realm.PhysicalBudget()
						slots := fixture.effects.realm.AvailableCompletePaneSlots()
						fixture.effects.journalMu.Unlock()
						files := fixture.journalFileCount(t)
						if !newRetained || (cut.name == "pre_registry" && !oldRetained) || slots > slotsBefore || files < filesBefore {
							t.Fatalf("uncertain state not retained: old=%t new=%t slots=%d before=%d files=%d beforeFiles=%d", oldRetained, newRetained, slots, slotsBefore, files, filesBefore)
						}
						if oldRetirement || newRetirement || oldDurable || newDurable {
							t.Fatalf("uncertain owner elected durable cleanup: retirements=%t/%t durable=%t/%t", oldRetirement, newRetirement, oldDurable, newDurable)
						}
						fingerprint := fmt.Sprintf("old=%t new=%t old_durable=%t new_durable=%t old_retire=%t new_retire=%t slots=%d files=%d logical=%d/%d physical=%d/%d p=%d q=%d", oldRetained, newRetained, oldDurable, newDurable, oldRetirement, newRetirement, slots, files, logical, logicalReserved, physical, physicalReserved, pUsed, qUsed)
						fingerprintKey := cut.name + "/" + disposition.name
						if unlinkMode == "success" {
							uncertainFingerprints[fingerprintKey] = fingerprint
						} else if want := uncertainFingerprints[fingerprintKey]; fingerprint != want {
							t.Fatalf("uncertain %s label mutated proof-bearing state: got %s want %s", unlinkMode, fingerprint, want)
						}
						return
					}
					deadline := time.Now().Add(10 * time.Second)
					for {
						// Cleanup runs on the retention dispatcher. Sample its journal
						// ledgers under the same lock used by the production writer.
						fixture.effects.journalMu.Lock()
						logical, logicalReserved, logicalCap := fixture.effects.realm.LogicalBudget()
						physical, physicalReserved, physicalCap := fixture.effects.realm.PhysicalBudget()
						slots := fixture.effects.realm.AvailableCompletePaneSlots()
						fixture.effects.journalMu.Unlock()
						fileCount := fixture.journalFileCount(t)
						if slots == initialSlots && fileCount == 0 && logical == initialLogical && logicalReserved == initialLogicalReserved && physical == initialPhysical && physicalReserved == initialPhysicalReserved {
							break
						}
						if time.Now().After(deadline) {
							fixture.registry.retention.mu.Lock()
							generations, durable, pUsed, qUsed := len(fixture.registry.retention.generations), len(fixture.registry.retention.durableRetires), fixture.registry.retention.pUsed, fixture.registry.retention.qUsed
							fixture.registry.retention.mu.Unlock()
							fixture.effects.journalMu.Lock()
							files, _ := rotationJournalFiles(fixture.runtimeDir)
							oldLogical, _ := fixture.effects.realm.PaneLogical(capturedOld)
							newLogical, _ := fixture.effects.realm.PaneLogical(capturedNew)
							oldPhysical, _ := fixture.effects.realm.PanePhysical(capturedOld)
							newPhysical, _ := fixture.effects.realm.PanePhysical(capturedNew)
							fixture.effects.journalMu.Unlock()
							t.Fatalf("cleanup did not converge: slots=%d initial=%d files=%v logical=%d/%d/%d physical=%d/%d/%d generations=%d durable=%d pUsed=%d qUsed=%d old=%d/%d new=%d/%d", slots, initialSlots, files, logical, logicalReserved, logicalCap, physical, physicalReserved, physicalCap, generations, durable, pUsed, qUsed, oldLogical, oldPhysical, newLogical, newPhysical)
						}
						time.Sleep(25 * time.Millisecond)
					}
					fixture.registry.retention.mu.Lock()
					_, oldGeneration := fixture.registry.retention.generations[capturedOld]
					_, newGeneration := fixture.registry.retention.generations[capturedNew]
					_, oldRetirement := fixture.registry.retention.durableRetires[capturedOld]
					_, newRetirement := fixture.registry.retention.durableRetires[capturedNew]
					pUsed, qUsed := fixture.registry.retention.pUsed, fixture.registry.retention.qUsed
					fixture.registry.retention.mu.Unlock()
					if oldGeneration || newGeneration || oldRetirement || newRetirement || pUsed != 0 || qUsed != 0 {
						t.Fatalf("proved unlink retained runtime authority: generations=%t/%t retirements=%t/%t p=%d q=%d", oldGeneration, newGeneration, oldRetirement, newRetirement, pUsed, qUsed)
					}
				})
			}
		}
	}
	if matrixCells != 24 {
		t.Fatalf("literal disposition/unlink matrix executed %d cells, want 24", matrixCells)
	}
}

// A predecessor's retiring seal can settle before the refit driver observes
// owner disappearance. The late terminal callback must not recreate that
// already-refunded runtime lifetime merely to report the same source failure.
func TestRefitSettlementLatePredecessorFaultDoesNotRecreateRetiredLifetime(t *testing.T) {
	for _, unlinkMode := range []string{"success", "eio"} {
		t.Run(unlinkMode, func(t *testing.T) {

			fixture := newAdoptionFixture(t, 4)
			initialSlots := fixture.effects.realm.AvailableCompletePaneSlots()
			sessionID := fixture.startPaneCommand(t, "refit-late-predecessor", `sh -c 'stty -echo; while :; do sleep 1; done'`)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			adoption, err := fixture.effects.AdoptSession(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			fixture.effects.mu.Lock()
			unit := fixture.effects.units[sessionID]
			old := unit.witnesses[0]
			fixture.effects.mu.Unlock()
			runtime := fixture.registry.retention
			done, err := runtime.startBoundary(adoption.Key, "terminal_handoff", true)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			pollUntil(t, 3*time.Second, "predecessor runtime lifetime settlement", func() bool {
				runtime.mu.Lock()
				defer runtime.mu.Unlock()
				return runtime.generations[adoption.Key] == nil && runtime.pUsed == 0 && runtime.qUsed == 0
			})
			changedDirs := map[string]struct{}{}
			t.Cleanup(func() {
				for dir := range changedDirs {
					_ = os.Chmod(dir, 0o700)
				}
			})
			if unlinkMode == "eio" {
				files, err := rotationJournalFiles(fixture.runtimeDir)
				if err != nil || len(files) == 0 {
					t.Fatalf("missing retained journal before fault: files=%v err=%v", files, err)
				}
				for _, path := range files {
					dir := filepath.Dir(path)
					if err := os.Chmod(dir, 0o500); err != nil {
						t.Fatal(err)
					}
					changedDirs[dir] = struct{}{}
				}
			}
			fixture.effects.journalMu.Lock()
			logicalBefore, logicalReservedBefore, _ := fixture.effects.realm.LogicalBudget()
			physicalBefore, physicalReservedBefore, _ := fixture.effects.realm.PhysicalBudget()
			fixture.effects.journalMu.Unlock()
			fixture.disposable.run("kill-session", "-t", sessionID)
			rotation := &unifiedDevRotation{session: sessionID, oldKey: adoption.Key, old: old, unit: unit, registry: fixture.registry, refit: true}
			rotation.failRefitPredecessor(io.EOF, observerSourceDecision{disposition: observerSourceOwnerGone})
			runtime.mu.Lock()
			generation, pUsed, qUsed := runtime.generations[adoption.Key], runtime.pUsed, runtime.qUsed
			retirement := runtime.durableRetires[adoption.Key]
			runtime.mu.Unlock()
			if generation != nil || pUsed != 0 || qUsed != 0 {
				t.Fatalf("late refit fault recreated retired runtime authority: generation=%t p=%d q=%d", generation != nil, pUsed, qUsed)
			}
			if unlinkMode == "eio" {
				fixture.effects.journalMu.Lock()
				logical, logicalReserved, _ := fixture.effects.realm.LogicalBudget()
				physical, physicalReserved, _ := fixture.effects.realm.PhysicalBudget()
				slots := fixture.effects.realm.AvailableCompletePaneSlots()
				fixture.effects.journalMu.Unlock()
				if retirement == nil || fixture.journalFileCount(t) == 0 || logicalBefore <= 0 || physicalBefore <= 0 || logical != logicalBefore || physical != physicalBefore || logicalReserved != logicalReservedBefore || physicalReserved != physicalReservedBefore || slots != initialSlots {
					t.Fatalf("late fault lost durable retry/charge: retry=%t files=%d logical=%d/%d physical=%d/%d slots=%d/%d", retirement != nil, fixture.journalFileCount(t), logical, logicalBefore, physical, physicalBefore, slots, initialSlots)
				}
				// Restoring unlink authority lets the existing timer retry prove cleanup.
				// Do not invoke a second retirement API or delete the journal in the test.
				for dir := range changedDirs {
					if err := os.Chmod(dir, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			}
			pollUntil(t, 3*time.Second, "late predecessor durable retirement", func() bool {
				runtime.mu.Lock()
				settled := runtime.generations[adoption.Key] == nil && runtime.durableRetires[adoption.Key] == nil && runtime.pUsed == 0 && runtime.qUsed == 0
				runtime.mu.Unlock()
				if !settled {
					return false
				}
				fixture.effects.journalMu.Lock()
				defer fixture.effects.journalMu.Unlock()
				logical, logicalReserved, _ := fixture.effects.realm.LogicalBudget()
				physical, physicalReserved, _ := fixture.effects.realm.PhysicalBudget()
				return settled && fixture.effects.realm.AvailableCompletePaneSlots() == initialSlots && fixture.journalFileCount(t) == 0 && logical == 0 && logicalReserved == 0 && physical == 0 && physicalReserved == 0
			})

		})
	}
}

// Initial cancellation and the refit driver share the provisional successor.
// Complete the real initial cleanup before allowing the late driver Fatal to
// run, rather than relying on a fast hosted runner to win that ordering.
func TestRefitSettlementSettledSuccessorCannotBeFaultedAgain(t *testing.T) {
	for _, site := range []string{"fatal", "commit", "committed"} {
		for _, unlinkMode := range []string{"success", "eio"} {
			t.Run(site+"/"+unlinkMode, func(t *testing.T) {
				fixture := newAdoptionFixture(t, 4)
				sessionID := fixture.startPaneCommand(t, "refit-settled-successor", `sh -c 'stty -echo; while :; do sleep 1; done'`)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if _, err := fixture.effects.AdoptSession(ctx, sessionID); err != nil {
					t.Fatal(err)
				}
				detail, err := details(fixture.server, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				authority, err := incarnation(fixture.server)
				if err != nil {
					t.Fatal(err)
				}
				authority.Realm, authority.Server, authority.SessionID, authority.SessionCreated = fixture.cfg.Realm, fixture.server.Label, detail.ID, detail.Created
				source, err := buildSourceWitness(ctx, fixture.server, authority.BootID, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				changedDirs := map[string]struct{}{}
				t.Cleanup(func() {
					for dir := range changedDirs {
						_ = os.Chmod(dir, 0o700)
					}
				})
				var successorKey unifiedjournal.PaneKey
				var armed atomic.Bool
				fixture.effects.rotationEdge = func(_ string, edge string) {
					if edge != "before_bootstrap_wait" || armed.Swap(true) {
						return
					}
					fixture.effects.mu.Lock()
					rotation := fixture.effects.rotation
					fixture.effects.mu.Unlock()
					if rotation == nil || rotation.registryT == nil || rotation.registryT.initial == nil {
						t.Error("missing provisional successor")
						return
					}
					op := rotation.registryT.initial
					successorKey = rotation.newKey
					select {
					case <-op.done:
					case <-ctx.Done():
						t.Error("initial operation did not complete")
						return
					}
					fixture.disposable.run("kill-session", "-t", sessionID)
					settled := startInitialSettlement(op, fixture.registry, io.EOF)
					select {
					case <-settled:
					case <-ctx.Done():
						t.Error("initial settlement did not complete")
						return
					}
					fixture.registry.retention.mu.Lock()
					gone := fixture.registry.retention.generations[rotation.newKey] == nil && op.generation.lifetimeReleased
					fixture.registry.retention.mu.Unlock()
					if !gone {
						t.Error("initial settlement did not retire the exact successor")
					}
					if unlinkMode == "eio" {
						files, err := rotationJournalFiles(fixture.runtimeDir)
						if err != nil || len(files) == 0 {
							t.Errorf("retained journal: %v", err)
							return
						}
						for _, path := range files {
							dir := filepath.Dir(path)
							if err := os.Chmod(dir, 0o500); err != nil {
								t.Error(err)
								return
							}
							changedDirs[dir] = struct{}{}
						}
					}
					switch site {
					case "commit":
						fixture.effects.mu.Lock()
						disposition, cleanup := rotation.registryT.commitProviderLocked(false, func() { t.Error("invalid commit installed witness") })
						fixture.effects.mu.Unlock()
						rotation.registryT.finishFatal(cleanup, io.EOF)
						if disposition != paneRotationCommitFatal || cleanup != nil {
							t.Error("late invalid commit recreated cleanup ownership")
						}
					case "committed":
						rotation.failCommittedSuccessor(io.EOF)
					}
				}
				fixture.effects.rotationFault = func(_ string, edge string) error {
					if edge == "boundary_wait" && armed.Load() {
						return io.EOF
					}
					return nil
				}
				err = fixture.effects.refitSession(ctx, authority, source, source.Columns+9, strings.Repeat("s", 43))
				if !armed.Load() || !errors.Is(err, ErrUnifiedRefitFatal) {
					t.Fatalf("late fatal=%v armed=%t", err, armed.Load())
				}
				dispatched, err := fixture.registry.retention.startDispatchFence()
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-dispatched:
				case <-ctx.Done():
					t.Fatal("fatal settlement dispatcher did not finish")
				}
				runtime := fixture.registry.retention
				runtime.mu.Lock()
				p, q := runtime.pUsed, runtime.qUsed
				retryCount := len(runtime.durableRetires)
				successorRetry := runtime.durableRetires[successorKey] != nil
				successorGone := runtime.generations[successorKey] == nil
				runtime.mu.Unlock()
				if p < 0 || q != 2*p {
					t.Fatalf("late successor fault corrupted funded lifetime accounting: p=%d q=%d", p, q)
				}
				if unlinkMode == "eio" {
					fixture.effects.journalMu.Lock()
					logical, _, _ := fixture.effects.realm.LogicalBudget()
					physical, _, _ := fixture.effects.realm.PhysicalBudget()
					fixture.effects.journalMu.Unlock()
					if !successorRetry || !successorGone || retryCount == 0 || logical <= 0 || physical <= 0 || fixture.journalFileCount(t) == 0 {
						t.Fatalf("EIO lost durable owner: retries=%d successor_retry=%t successor_gone=%t logical=%d physical=%d", retryCount, successorRetry, successorGone, logical, physical)
					}
					for dir := range changedDirs {
						if err := os.Chmod(dir, 0o700); err != nil {
							t.Fatal(err)
						}
					}
				}
				defer func() {
					if t.Failed() {
						runtime.mu.Lock()
						t.Logf("settlement runtime P=%d Q=%d generations=%d retries=%d", runtime.pUsed, runtime.qUsed, len(runtime.generations), len(runtime.durableRetires))
						runtime.mu.Unlock()
						fixture.effects.journalMu.Lock()
						l, lr, _ := fixture.effects.realm.LogicalBudget()
						p, pr, _ := fixture.effects.realm.PhysicalBudget()
						t.Logf("settlement journal logical=%d/%d physical=%d/%d files=%d", l, lr, p, pr, fixture.journalFileCount(t))
						fixture.effects.journalMu.Unlock()
					}
				}()
				pollUntil(t, 3*time.Second, "successor fatal durable settlement", func() bool {
					runtime.mu.Lock()
					settled := len(runtime.generations) == 0 && len(runtime.durableRetires) == 0 && runtime.pUsed == 0 && runtime.qUsed == 0
					runtime.mu.Unlock()
					if !settled {
						return false
					}
					fixture.effects.journalMu.Lock()
					defer fixture.effects.journalMu.Unlock()
					logical, lr, _ := fixture.effects.realm.LogicalBudget()
					physical, pr, _ := fixture.effects.realm.PhysicalBudget()
					return logical == 0 && lr == 0 && physical == 0 && pr == 0 && fixture.journalFileCount(t) == 0
				})
			})
		}
	}
}
