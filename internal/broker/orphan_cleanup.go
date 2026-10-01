package broker

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

const attachmentNonceHexLength = 32
const attachmentOwnerPIDOption = "@persea_broker_pid"
const attachmentOwnerStartOption = "@persea_broker_start"

type orphanShadow struct {
	sessionID  string
	name       string
	clientID   string
	ownerPID   int
	ownerStart uint64
	restored   bool
	authority  proto.Authority
}

// Incarnations are published only after their first sweep. Keep every admitted
// identity for the process lifetime: revisiting a socket must never sweep an
// incarnation on which this broker could already have created a shadow.
var shadowAdmission = struct {
	sync.Mutex
	ready map[tmuxProcessIdentity]bool
}{ready: make(map[tmuxProcessIdentity]bool)}

type tmuxProcessIdentity struct {
	uid   uint32
	boot  string
	pid   int
	start uint64
}

func incarnation(server config.TmuxServer) (proto.Authority, error) {
	shadowAdmission.Lock()
	defer shadowAdmission.Unlock()
	inc, err := readIncarnation(server)
	// Different configured selectors can reach the same running server.
	identity := tmuxProcessIdentity{inc.UID, inc.BootID, inc.ServerPID, inc.ServerStart}
	if err != nil || shadowAdmission.ready[identity] {
		return inc, err
	}
	if err := cleanupServerOrphanedShadows(server, inc); err != nil {
		return proto.Authority{}, err
	}
	after, err := readIncarnation(server)
	if err != nil {
		return proto.Authority{}, err
	}
	if !sameIncarnation(inc, after) {
		return proto.Authority{}, errStaleTarget
	}
	shadowAdmission.ready[identity] = true
	return inc, nil
}

func cleanupOrphanedShadows(cfg config.Broker) error {
	for _, server := range cfg.Servers {
		if _, err := incarnation(server); err != nil && !errors.Is(err, errNoServer) {
			return fmt.Errorf("tmux server %q: %w", server.Label, err)
		}
	}
	return nil
}

func cleanupServerOrphanedShadows(server config.TmuxServer, inc proto.Authority) error {
	out, err := tmuxOutput(server, "list-sessions", "-F", "#{session_id}")
	if errors.Is(err, errNoServer) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inventory attachment shadow ids: %w", err)
	}
	for _, sessionID := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if sessionID == "" {
			continue
		}
		if !validSessionID(sessionID) {
			return fmt.Errorf("tmux returned invalid session id")
		}
		shadow, owned := inspectOrphanShadow(server, sessionID)
		if !owned {
			shadow, owned = inspectRestoredShadow(server, sessionID, inc)
			if !owned {
				continue
			}
		} else {
			alive, err := orphanOwnerAlive(shadow)
			if err != nil || alive {
				continue
			}
		}
		if err := removeOrphanShadow(server, shadow); err != nil {
			return err
		}
		brokerLogf("component=broker event=orphan_shadow_removed server=%q session=%q", server.Label, shadow.sessionID)
	}
	return nil
}

func inspectOrphanShadow(server config.TmuxServer, sessionID string) (orphanShadow, bool) {
	name, ok := sessionField(server, sessionID, "#{session_name}")
	if !ok || !attachmentShadowName(name) {
		return orphanShadow{}, false
	}
	nonce := strings.TrimPrefix(name, attachmentShadowPrefix)
	clientID, ok := sessionField(server, sessionID, "#{@persea_client_id}")
	if !ok || clientID != attachmentClientPrefix+nonce {
		return orphanShadow{}, false
	}
	ownerPIDText, ok := sessionField(server, sessionID, "#{"+attachmentOwnerPIDOption+"}")
	if !ok {
		return orphanShadow{}, false
	}
	ownerStartText, ok := sessionField(server, sessionID, "#{"+attachmentOwnerStartOption+"}")
	if !ok {
		return orphanShadow{}, false
	}
	attachedText, ok := sessionField(server, sessionID, "#{session_attached}")
	if !ok {
		return orphanShadow{}, false
	}
	windowsText, ok := sessionField(server, sessionID, "#{session_windows}")
	if !ok {
		return orphanShadow{}, false
	}
	ownerPID, err := strconv.Atoi(ownerPIDText)
	if err != nil || ownerPID <= 0 {
		return orphanShadow{}, false
	}
	ownerStart, err := strconv.ParseUint(ownerStartText, 10, 64)
	if err != nil || ownerStart == 0 {
		return orphanShadow{}, false
	}
	attached, err := strconv.Atoi(attachedText)
	if err != nil || attached != 0 {
		return orphanShadow{}, false
	}
	windows, err := strconv.Atoi(windowsText)
	if err != nil || windows != 1 {
		return orphanShadow{}, false
	}
	return orphanShadow{sessionID: sessionID, name: name, clientID: clientID, ownerPID: ownerPID, ownerStart: ownerStart}, true
}

func attachmentShadowName(name string) bool {
	nonce := strings.TrimPrefix(name, attachmentShadowPrefix)
	if nonce == name || len(nonce) != attachmentNonceHexLength {
		return false
	}
	_, err := hex.DecodeString(nonce)
	return err == nil
}

func inspectRestoredShadow(server config.TmuxServer, sessionID string, inc proto.Authority) (orphanShadow, bool) {
	row, ok := sessionField(server, sessionID, "#{session_name}\t#{session_attached}\t#{session_windows}\t#{session_created}")
	fields := strings.Split(row, "\t")
	if !ok || len(fields) != 4 || !attachmentShadowName(fields[0]) || fields[1] != "0" || fields[2] != "1" {
		return orphanShadow{}, false
	}
	created, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || created <= 0 {
		return orphanShadow{}, false
	}
	for _, option := range []string{"@persea_client_id", attachmentOwnerPIDOption, attachmentOwnerStartOption} {
		// show-options distinguishes an absent option from an explicitly empty
		// one; either a local or inherited marker prevents restored-copy cleanup.
		out, err := tmuxOutput(server, "show-options", "-Aq", "-t", "="+sessionID+":", option)
		if err != nil || out != "" {
			return orphanShadow{}, false
		}
	}
	inc.SessionCreated = created
	return orphanShadow{sessionID: sessionID, name: fields[0], restored: true, authority: inc}, true
}

func sessionField(server config.TmuxServer, sessionID, format string) (string, bool) {
	out, err := tmuxOutput(server, "display-message", "-p", "-t", "="+sessionID+":", "-F", format)
	if err != nil || !strings.HasSuffix(out, "\n") {
		return "", false
	}
	return strings.TrimSuffix(out, "\n"), true
}

func orphanOwnerAlive(shadow orphanShadow) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	witness, err := (procProbe{}).Witness(ctx, shadow.ownerPID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return witness.StartTime == shadow.ownerStart, nil
}
