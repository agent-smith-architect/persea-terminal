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
	"syscall"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

const attachmentNonceHexLength = 32
const attachmentOwnerPIDOption = "@persea_broker_pid"
const attachmentOwnerStartOption = "@persea_broker_start"
const attachmentOwnerEnvironment = "PERSEA_SHADOW_OWNER"

// Bound the client's normal wait; ownership does not depend on cancellation.
const shadowBirthTimeout = 3 * time.Second

const shadowMarkerFlags = "#{!=:#{@persea_client_id},}\t#{!=:#{@persea_broker_pid},}\t#{!=:#{@persea_broker_start},}"
const shadowSessionFormat = "#{session_id}\t#{session_name}\t#{session_attached}\t#{session_windows}\t#{session_created}\t" + shadowMarkerFlags

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
var shadowAdmission sync.Map // tmuxProcessIdentity -> *tmuxShadowAdmission

type tmuxShadowAdmission struct {
	sync.Mutex
	ready bool
}

type tmuxProcessIdentity struct {
	uid   uint32
	boot  string
	pid   int
	start uint64
}

func incarnation(server config.TmuxServer) (proto.Authority, error) {
	inc, err := readIncarnation(server)
	if err != nil {
		return inc, err
	}
	// Different configured selectors can reach the same running server.
	identity := tmuxProcessIdentity{inc.UID, inc.BootID, inc.ServerPID, inc.ServerStart}
	value, _ := shadowAdmission.LoadOrStore(identity, &tmuxShadowAdmission{})
	admission := value.(*tmuxShadowAdmission)
	admission.Lock()
	defer admission.Unlock()
	if admission.ready {
		return inc, nil
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
	admission.ready = true
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
	out, err := tmuxOutput(server, "list-sessions", "-F", shadowSessionFormat)
	if errors.Is(err, errNoServer) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inventory attachment shadow ids: %w", err)
	}
	for _, row := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if row == "" {
			continue
		}
		fields := strings.Split(row, "\t")
		if len(fields) < 2 || !attachmentShadowName(fields[1]) {
			continue
		}
		shadow, restored, err := restoredShadowCandidate(server, fields, inc)
		if err != nil {
			return err
		}
		if !restored {
			if len(fields) != 8 || fields[5] != "1" || fields[6] != "1" || fields[7] != "1" {
				continue
			}
			var owned bool
			shadow, owned = inspectOrphanShadow(server, fields[0])
			if !owned {
				continue
			}
			alive, err := orphanOwnerAlive(shadow)
			if err != nil {
				logShadowOwnerSkip(server, shadow.sessionID, err)
				continue
			}
			if alive {
				continue
			}
		}
		if _, err := reapOrphanShadow(server, shadow); err != nil {
			return err
		}
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
	row, ok := sessionField(server, sessionID, shadowSessionFormat)
	if !ok {
		return orphanShadow{}, false
	}
	shadow, candidate, err := restoredShadowCandidate(server, strings.Split(row, "\t"), inc)
	return shadow, candidate && err == nil
}

func restoredShadowCandidate(server config.TmuxServer, fields []string, inc proto.Authority) (orphanShadow, bool, error) {
	if len(fields) != 8 || !validSessionID(fields[0]) || !attachmentShadowName(fields[1]) || fields[2] != "0" || fields[3] != "1" || fields[5] != "0" || fields[6] != "0" || fields[7] != "0" {
		return orphanShadow{}, false, nil
	}
	created, err := strconv.ParseInt(fields[4], 10, 64)
	if err != nil || created <= 0 {
		return orphanShadow{}, false, nil
	}
	alive, err := restoredShadowOwnerAlive(server, fields[0], inc.BootID)
	if errors.Is(err, errShadowOwnerUncertain) {
		logShadowOwnerSkip(server, fields[0], err)
		return orphanShadow{}, false, nil
	}
	if err != nil || alive {
		return orphanShadow{}, false, err
	}
	inc.SessionCreated = created
	return orphanShadow{sessionID: fields[0], name: fields[1], restored: true, authority: inc}, true, nil
}

func restoredShadowOwnerAlive(server config.TmuxServer, sessionID, bootID string) (bool, error) {
	out, err := tmuxOutput(server, "show-environment", "-t", "="+sessionID+":", attachmentOwnerEnvironment)
	if err != nil {
		var failure *tmuxCommandError
		if errors.As(err, &failure) && (failure.message == "unknown variable: "+attachmentOwnerEnvironment || failure.message == "no such session: ="+sessionID+":") {
			return false, nil
		}
		if failure != nil && strings.HasPrefix(failure.message, "unknown variable:") {
			return false, fmt.Errorf("%w: unexpected environment diagnostic", errShadowOwnerUncertain)
		}
		return false, err
	}
	if out == "-"+attachmentOwnerEnvironment+"\n" {
		return false, nil
	}
	value, ok := strings.CutPrefix(out, attachmentOwnerEnvironment+"=")
	if !ok || !strings.HasSuffix(value, "\n") {
		return false, fmt.Errorf("%w: invalid environment output", errShadowOwnerUncertain)
	}
	parts := strings.Split(strings.TrimSuffix(value, "\n"), ":")
	if len(parts) != 3 || !canonicalBootID(parts[0]) {
		return false, fmt.Errorf("%w: invalid witness", errShadowOwnerUncertain)
	}
	pid, pidErr := strconv.Atoi(parts[1])
	start, startErr := strconv.ParseUint(parts[2], 10, 64)
	if pidErr != nil || pid <= 0 || strconv.Itoa(pid) != parts[1] || startErr != nil || start == 0 || strconv.FormatUint(start, 10) != parts[2] {
		return false, fmt.Errorf("%w: invalid witness", errShadowOwnerUncertain)
	}
	if parts[0] != bootID {
		return false, nil
	}
	// The witness is unchanged until all markers are set. Inspection reads
	// markers first, and the destruction guard rechecks them before removal.
	return orphanOwnerAlive(orphanShadow{ownerPID: pid, ownerStart: start})
}

func canonicalBootID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

var errShadowOwnerUncertain = errors.New("attachment owner is uncertain")

func logShadowOwnerSkip(server config.TmuxServer, sessionID string, err error) {
	brokerLogf("component=broker event=orphan_shadow_skipped server=%q session=%q reason=%q", server.Label, sessionID, err)
}

var errShadowProtected = errors.New("attachment shadow became protected")
var errShadowMissing = errors.New("attachment shadow disappeared")

func shadowCleanupRefusal(server config.TmuxServer, shadow orphanShadow, refusal error) error {
	if shadow.restored {
		inc, err := readIncarnation(server)
		if err != nil {
			return err
		}
		if !sameIncarnation(inc, shadow.authority) {
			return errStaleTarget
		}
	}
	out, err := tmuxOutput(server, "list-sessions", "-F", "#{session_id}")
	if err != nil {
		return err
	}
	for _, id := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if id == shadow.sessionID {
			return refusal
		}
	}
	return errShadowMissing
}

func reapOrphanShadow(server config.TmuxServer, shadow orphanShadow) (bool, error) {
	err := removeOrphanShadow(server, shadow)
	if errors.Is(err, errShadowProtected) || errors.Is(err, errShadowMissing) {
		brokerLogf("component=broker event=orphan_shadow_skipped server=%q session=%q reason=%q", server.Label, shadow.sessionID, err)
		return errors.Is(err, errShadowMissing), nil
	}
	if err != nil {
		return false, err
	}
	brokerLogf("component=broker event=orphan_shadow_removed server=%q session=%q", server.Label, shadow.sessionID)
	return true, nil
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
	witness, err := readOwnerProcess(ctx, shadow.ownerPID)
	if errors.Is(err, os.ErrNotExist) {
		// Proc visibility can hide a living, non-dumpable process. Only the
		// kernel's absence result proves death when its identity is unreadable.
		err = signalOwnerProcess(shadow.ownerPID, 0)
		switch {
		case errors.Is(err, syscall.ESRCH):
			return false, nil
		case err == nil || errors.Is(err, syscall.EPERM):
			return true, nil
		default:
			return false, fmt.Errorf("%w: probe process: %v", errShadowOwnerUncertain, err)
		}
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspect process: %v", errShadowOwnerUncertain, err)
	}
	return witness.StartTime == shadow.ownerStart, nil
}

var readOwnerProcess = (procProbe{}).Witness
var signalOwnerProcess = syscall.Kill
