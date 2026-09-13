package broker

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

// createMu serialises the count-then-create sequence.
//
// Each connection is handled on its own goroutine, so without this two
// authenticated requests can both read a count one below the cap and both
// proceed, putting the realm over max_sessions. tmux gives us atomicity for
// *name* uniqueness but has no notion of our cap, so the cap has to be made
// atomic here. The critical section is one tmux list plus one tmux create, which
// is short and uncontended in normal use; creation is an operator action, not a
// hot path.
//
// Package-scoped rather than per-Server because a broker process serves exactly
// one realm, and the bound being defended is per realm.
var createMu sync.Mutex

// sessionBirthCreator is an optional extension used only by the development
// unified-terminal birth observer. The caller still constructs and validates
// the complete tmux argument vector; the observer only executes that structured
// request while it owns the byte-zero control stream.
type sessionBirthCreator interface {
	CreateSession(config.TmuxServer, []string) (string, error)
}

// Operator session creation.
//
// This file is the only place outside the shadow-attachment transaction that may
// create a tmux session, and it may not destroy one: session teardown commands
// are forbidden here by the callsite guard in broker_test.go, which scans for the
// literal tmux verbs. (That guard is a token scan, so do not spell a forbidden
// verb in these comments either.) The two lifecycles are kept apart
// on purpose. A shadow (`persea-attach-<nonce>`) is an internal wrapper with
// witness and cleanup invariants; a session created here is an ordinary,
// operator-visible session that Persea Terminal does not own and never removes.

// create makes one new detached tmux session on a configured server.
//
// Deliberately absent: any client control over the command, the working
// directory, or the environment. tmux is asked to start the realm user's default
// shell in the configured directory and nothing else. A browser-supplied command
// would make this a remote execution service rather than a terminal attachment.
//
// Uniqueness is left to tmux. The creation command below omits -A, so tmux fails
// when the name is taken; there is deliberately no existence pre-check, which
// would be a time-of-check to time-of-use race and strictly weaker than the
// atomic failure tmux already gives us.
func (s *Server) create(writer *lockedWriter, ctrl proto.Control) {
	refuse := func(code, msg string) {
		log.Printf("component=broker event=create_refused realm=%q server=%q reason=%q", s.config.Realm, ctrl.ServerLabel, code)
		_ = writer.control(proto.Control{Type: "create_refused", Code: code, Msg: msg})
	}
	createMu.Lock()
	defer createMu.Unlock()
	policy := s.config.SessionCreate
	if !policy.AllowsServer(ctrl.ServerLabel) {
		refuse("not_permitted", "session creation is not enabled for this server")
		return
	}
	if !policy.AllowsName(ctrl.Name) {
		refuse("invalid_name", "the name does not match the permitted pattern for this realm")
		return
	}
	var server config.TmuxServer
	found := false
	for _, candidate := range s.config.Servers {
		if candidate.Label == ctrl.ServerLabel {
			server, found = candidate, true
			break
		}
	}
	if !found {
		refuse("not_permitted", "unknown server")
		return
	}
	// The cap is enforced here rather than in the UI so a looping or hostile client
	// cannot fork an unbounded number of shells.
	if existing, err := sessionCount(server); err != nil {
		if !errors.Is(err, errNoServer) {
			refuse("server_unavailable", "the tmux server could not be inspected")
			return
		}
		// No server yet: tmux will start one for this session, which is the normal
		// first-session path and is within the cap by definition.
	} else if existing >= policy.MaxSessions {
		refuse("at_capacity", fmt.Sprintf("this realm already holds its maximum of %d sessions", policy.MaxSessions))
		return
	}
	args := []string{"new-session", "-d", "-P", "-F", "#{session_id}", "-s", ctrl.Name}
	if policy.StartDirectory != "" {
		args = append(args, "-c", policy.StartDirectory)
	}
	if policy.Columns > 0 && policy.Rows > 0 {
		args = append(args, "-x", strconv.Itoa(policy.Columns), "-y", strconv.Itoa(policy.Rows))
	}
	var birth SessionBirthEffects
	if s.birth != nil {
		var err error
		birth, err = s.birth.beginSessionBirth(server.Label, ctrl.Name)
		if err != nil {
			refuse("create_failed", "tmux refused to create the session")
			return
		}
	}
	var out string
	var err error
	if creator, ok := birth.(sessionBirthCreator); ok {
		// A control client must follow the new session to observe its first byte.
		// The legacy path remains detached and byte-for-byte unchanged. The
		// observer client carries ignore-size so the observed session keeps its
		// configured geometry instead of shrinking to the observer's own PTY.
		observerArgs := append([]string(nil), args[:1]...)
		observerArgs = append(observerArgs, "-f", "ignore-size")
		observerArgs = append(observerArgs, args[2:]...)
		out, err = creator.CreateSession(server, observerArgs)
	} else {
		out, err = tmuxOutput(server, args...)
	}
	if err != nil {
		log.Printf("component=broker event=create_observer_failed realm=%q server=%q error=%q", s.config.Realm, server.Label, err)
		if birth != nil {
			if abortErr := birth.AbortSessionBirth(err); abortErr != nil {
				refuse("create_failed", "tmux refused to create the session")
				return
			}
		}
		if strings.Contains(err.Error(), "duplicate session") {
			refuse("name_taken", "a session with that name already exists")
			return
		}
		refuse("create_failed", "tmux refused to create the session")
		return
	}
	sessionID := strings.TrimSpace(out)
	if !validSessionID(sessionID) {
		if birth != nil {
			_ = birth.AbortSessionBirth(fmt.Errorf("tmux returned an unusable session id"))
		}
		refuse("create_failed", "tmux returned an unusable session id")
		return
	}
	if birth != nil {
		if err := birth.CommitSessionBirth(sessionID); err != nil {
			log.Printf("component=broker event=create_birth_commit_failed realm=%q server=%q error=%q", s.config.Realm, server.Label, err)
			_ = birth.AbortSessionBirth(err)
			refuse("create_failed", "tmux refused to create the session")
			return
		}
	}
	log.Printf("component=broker event=create_ok realm=%q server=%q session=%q", s.config.Realm, server.Label, sessionID)
	_ = writer.control(proto.Control{Type: "create_ok", ServerLabel: server.Label, Name: ctrl.Name, SessionID: sessionID})
}

func sessionCount(server config.TmuxServer) (int, error) {
	out, err := tmuxOutput(server, "list-sessions", "-F", "#{session_id}")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count, nil
}
