package broker

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"persea-terminal/internal/config"
)

const testDeadOwnerPID = 1 << 30
const testDeadOwnerStart = 1

func setShadowOwner(t *testing.T, d *disposable, name, clientID string, ownerPID int, ownerStart uint64) {
	t.Helper()
	d.run("set-option", "-t", name, "@persea_client_id", clientID)
	d.run("set-option", "-t", name, attachmentOwnerPIDOption, strconv.Itoa(ownerPID))
	d.run("set-option", "-t", name, attachmentOwnerStartOption, strconv.FormatUint(ownerStart, 10))
}

func TestCleanupOrphanedShadowsRequiresExactDeadOwner(t *testing.T) {
	d := newDisposable(t)
	ownedNonce := strings.Repeat("a", attachmentNonceHexLength)
	owned := attachmentShadowPrefix + ownedNonce
	ownedClient := attachmentClientPrefix + ownedNonce
	missingMarker := attachmentShadowPrefix + strings.Repeat("b", attachmentNonceHexLength)
	mismatchedMarker := attachmentShadowPrefix + strings.Repeat("c", attachmentNonceHexLength)
	extraWindow := attachmentShadowPrefix + strings.Repeat("d", attachmentNonceHexLength)
	legacyNoOwner := attachmentShadowPrefix + strings.Repeat("e", attachmentNonceHexLength)
	liveOwner := attachmentShadowPrefix + strings.Repeat("f", attachmentNonceHexLength)
	metadataNoise := "synthetic-metadata"

	for _, name := range []string{owned, missingMarker, mismatchedMarker, extraWindow, legacyNoOwner, liveOwner, metadataNoise, "synthetic-work"} {
		d.run("new-session", "-d", "-s", name, "sleep", "600")
	}
	setShadowOwner(t, d, owned, ownedClient, testDeadOwnerPID, testDeadOwnerStart)
	d.run("set-option", "-t", mismatchedMarker, "@persea_client_id", attachmentClientPrefix+strings.Repeat("0", attachmentNonceHexLength))
	setShadowOwner(t, d, extraWindow, attachmentClientPrefix+strings.Repeat("d", attachmentNonceHexLength), testDeadOwnerPID, testDeadOwnerStart)
	d.run("new-window", "-d", "-t", extraWindow, "sleep", "600")
	d.run("set-option", "-t", legacyNoOwner, "@persea_client_id", attachmentClientPrefix+strings.Repeat("e", attachmentNonceHexLength))
	self, err := (procProbe{}).Witness(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	setShadowOwner(t, d, liveOwner, attachmentClientPrefix+strings.Repeat("f", attachmentNonceHexLength), self.PID, self.StartTime)
	d.run("set-option", "-t", metadataNoise, "@persea_client_id", "noise\t"+attachmentShadowPrefix+ownedNonce+"\nnoise")

	if err := cleanupOrphanedShadows(config.Broker{Servers: []config.TmuxServer{d.tmux}}); err != nil {
		t.Fatal(err)
	}
	sessions := "\n" + d.run("list-sessions", "-F", "#{session_name}") + "\n"
	if strings.Contains(sessions, "\n"+owned+"\n") {
		t.Fatal("exact detached shadow with a dead owner survived cleanup")
	}
	for _, preserved := range []string{"alpha", missingMarker, mismatchedMarker, extraWindow, legacyNoOwner, liveOwner, metadataNoise, "synthetic-work"} {
		if !strings.Contains(sessions, "\n"+preserved+"\n") {
			t.Fatalf("non-orphan session %q was removed", preserved)
		}
	}
}

func TestCleanupOrphanedShadowsAllowsStoppedTmuxServer(t *testing.T) {
	dir := shortTempDir(t)
	cfg := config.Broker{Servers: []config.TmuxServer{{Label: "synthetic", SocketPath: dir + "/absent.sock"}}}
	if err := cleanupOrphanedShadows(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestOrphanOwnerLivenessUsesPIDAndStartTime(t *testing.T) {
	self, err := (procProbe{}).Witness(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	alive, err := orphanOwnerAlive(orphanShadow{ownerPID: self.PID, ownerStart: self.StartTime})
	if err != nil || !alive {
		t.Fatalf("current owner not live: alive=%v err=%v", alive, err)
	}
	alive, err = orphanOwnerAlive(orphanShadow{ownerPID: self.PID, ownerStart: self.StartTime + 1})
	if err != nil || alive {
		t.Fatalf("reused PID/start mismatch accepted: alive=%v err=%v", alive, err)
	}
	alive, err = orphanOwnerAlive(orphanShadow{ownerPID: testDeadOwnerPID, ownerStart: testDeadOwnerStart})
	if err != nil || alive {
		t.Fatalf("absent owner accepted: alive=%v err=%v", alive, err)
	}
}

func TestGuardedOrphanRemovalRejectsChangedIdentity(t *testing.T) {
	for _, mutation := range []string{"marker", "window-count", "owner-pid"} {
		t.Run(mutation, func(t *testing.T) {
			d := newDisposable(t)
			nonce := strings.Repeat("7", attachmentNonceHexLength)
			name := attachmentShadowPrefix + nonce
			clientID := attachmentClientPrefix + nonce
			d.run("new-session", "-d", "-s", name, "sleep", "600")
			setShadowOwner(t, d, name, clientID, testDeadOwnerPID, testDeadOwnerStart)
			id := d.run("display-message", "-p", "-t", name, "#{session_id}")
			shadow := orphanShadow{sessionID: id, name: name, clientID: clientID, ownerPID: testDeadOwnerPID, ownerStart: testDeadOwnerStart}
			switch mutation {
			case "marker":
				d.run("set-option", "-t", name, "@persea_client_id", attachmentClientPrefix+strings.Repeat("8", attachmentNonceHexLength))
			case "window-count":
				d.run("new-window", "-d", "-t", name, "sleep", "600")
			case "owner-pid":
				d.run("set-option", "-t", name, attachmentOwnerPIDOption, strconv.Itoa(testDeadOwnerPID-1))
			}
			if err := removeOrphanShadow(d.tmux, shadow); err == nil {
				t.Fatal("changed shadow identity was removed")
			}
			if got := d.run("display-message", "-p", "-t", name, "#{session_id}"); got != id {
				t.Fatalf("changed shadow was not preserved: got %q want %q", got, id)
			}
		})
	}
}

func TestConcurrentOrphanRemovalTreatsLostRaceAsSuccess(t *testing.T) {
	d := newDisposable(t)
	nonce := strings.Repeat("6", attachmentNonceHexLength)
	name := attachmentShadowPrefix + nonce
	clientID := attachmentClientPrefix + nonce
	d.run("new-session", "-d", "-s", name, "sleep", "600")
	setShadowOwner(t, d, name, clientID, testDeadOwnerPID, testDeadOwnerStart)
	id := d.run("display-message", "-p", "-t", name, "#{session_id}")
	shadow := orphanShadow{sessionID: id, name: name, clientID: clientID, ownerPID: testDeadOwnerPID, ownerStart: testDeadOwnerStart}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- removeOrphanShadow(d.tmux, shadow)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent cleanup did not accept an already-absent target: %v", err)
		}
	}
	if present, err := tmuxSessionPresent(d.tmux, id); err != nil || present {
		t.Fatalf("concurrent cleanup result: present=%v err=%v", present, err)
	}
}

func TestRunReapsDeadOwnerBeforeOpeningBrokerSocket(t *testing.T) {
	d := newDisposable(t)
	nonce := strings.Repeat("9", attachmentNonceHexLength)
	name := attachmentShadowPrefix + nonce
	d.run("new-session", "-d", "-s", name, "sleep", "600")
	setShadowOwner(t, d, name, attachmentClientPrefix+nonce, testDeadOwnerPID, testDeadOwnerStart)

	runtimeDir := shortTempDir(t)
	blockedSocket := filepath.Join(runtimeDir, "blocked.sock")
	if err := os.Mkdir(blockedSocket, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blockedSocket, "keep"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Broker{FrontUID: uint32(os.Getuid()), FrontUIDConfigured: true, Servers: []config.TmuxServer{d.tmux}}
	if err := Run(blockedSocket, cfg); err == nil {
		t.Fatal("expected the deliberately blocked broker socket to fail")
	}
	if sessions := "\n" + d.run("list-sessions", "-F", "#{session_name}") + "\n"; strings.Contains(sessions, "\n"+name+"\n") {
		t.Fatal("Run did not reap the exact dead-owner shadow before opening its listener")
	}
}
