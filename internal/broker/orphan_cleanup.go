package broker

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"persea-terminal/internal/config"
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
}

func cleanupOrphanedShadows(cfg config.Broker) error {
	for _, server := range cfg.Servers {
		if err := cleanupServerOrphanedShadows(server); err != nil {
			return fmt.Errorf("tmux server %q: %w", server.Label, err)
		}
	}
	return nil
}

func cleanupServerOrphanedShadows(server config.TmuxServer) error {
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
			continue
		}
		alive, err := orphanOwnerAlive(shadow)
		if err != nil || alive {
			continue
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
	if !ok {
		return orphanShadow{}, false
	}
	nonce := strings.TrimPrefix(name, attachmentShadowPrefix)
	if nonce == name || len(nonce) != attachmentNonceHexLength {
		return orphanShadow{}, false
	}
	decoded, err := hex.DecodeString(nonce)
	if err != nil || len(decoded) != attachmentNonceHexLength/2 {
		return orphanShadow{}, false
	}
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
