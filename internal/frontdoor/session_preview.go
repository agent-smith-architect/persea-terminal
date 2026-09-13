package frontdoor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

// Dashboard session previews.
//
// GET /api/session-previews?realm=&server=&session_id= asks one realm's broker
// for an on-demand plain-text glance at a session's active pane. The front
// door forwards identity and relays rows; it never interprets the content —
// the broker strips control bytes and the dashboard renders through
// textContent only. Nothing is persisted anywhere: the only state here is a
// short in-memory TTL cache plus a token budget, so an expanded row that
// refreshes eagerly cannot hammer tmux.

// PreviewCacheTTL is how long one session's preview outcome is replayed
// before the broker is consulted again. Long enough to absorb bursts from a
// re-rendering dashboard, short enough that a manual refresh feels live.
const PreviewCacheTTL = 3 * time.Second

// PreviewCacheCapacity bounds the cache map. When it is full of fresh
// entries the response is simply served uncached — correctness never depends
// on the cache.
const PreviewCacheCapacity = 256

// PreviewFetchBurst / PreviewFetchPerSecond form the cache-miss token budget
// across all sessions. Cache hits are free; only broker round-trips spend.
const PreviewFetchBurst = 10
const PreviewFetchPerSecond = 2

type previewEntry struct {
	status int
	json   bool
	body   []byte
}

type previewCacheRecord struct {
	at    time.Time
	entry previewEntry
}

type previewStore struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]previewCacheRecord
	tokens  float64
	at      time.Time
}

func newPreviewStore(now func() time.Time) *previewStore {
	return &previewStore{now: now, entries: map[string]previewCacheRecord{}}
}

func (p *previewStore) lookup(key string) (previewEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	record, ok := p.entries[key]
	if !ok || p.now().Sub(record.at) > PreviewCacheTTL {
		delete(p.entries, key)
		return previewEntry{}, false
	}
	return record.entry, true
}

// allowFetch spends one token from the cache-miss budget. Exhausting it is a
// pacing signal, not an authorization failure, and maps to 429 accordingly.
func (p *previewStore) allowFetch() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.at.IsZero() {
		p.tokens = PreviewFetchBurst
	} else {
		p.tokens += now.Sub(p.at).Seconds() * PreviewFetchPerSecond
		if p.tokens > PreviewFetchBurst {
			p.tokens = PreviewFetchBurst
		}
	}
	p.at = now
	if p.tokens < 1 {
		return false
	}
	p.tokens--
	return true
}

func (p *previewStore) store(key string, entry previewEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if len(p.entries) >= PreviewCacheCapacity {
		for existing, record := range p.entries {
			if now.Sub(record.at) > PreviewCacheTTL {
				delete(p.entries, existing)
			}
		}
	}
	if len(p.entries) >= PreviewCacheCapacity {
		return
	}
	p.entries[key] = previewCacheRecord{at: now, entry: entry}
}

// previewSessionStatus maps a broker preview refusal to a status code. The
// judgement is the broker's; the front door only reports it.
func previewSessionStatus(code string) int {
	switch code {
	case "session_gone":
		return http.StatusGone
	case "server_unavailable":
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

// previewSession serves the dashboard's on-demand pane preview. It is an
// authenticated GET exactly like /api/inventory: operator identity and rate
// limiting come from the secure() chain, and safe methods need no CSRF token.
func (s *Server) previewSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	values := r.URL.Query()
	for key, list := range values {
		if (key != "realm" && key != "server" && key != "session_id") || len(list) != 1 {
			http.Error(w, "invalid preview request", http.StatusBadRequest)
			return
		}
	}
	realmName, serverLabel, sessionID := values.Get("realm"), values.Get("server"), values.Get("session_id")
	var realm config.Realm
	found := false
	for _, candidate := range s.cfg.Realms {
		if candidate.Name == realmName {
			realm, found = candidate, true
			break
		}
	}
	if !found || serverLabel == "" || len(serverLabel) > 128 || sessionID == "" || len(sessionID) > 32 {
		http.Error(w, "invalid preview request", http.StatusBadRequest)
		return
	}
	key := realmName + "\x00" + serverLabel + "\x00" + sessionID
	if entry, ok := s.previews.lookup(key); ok {
		writePreviewEntry(w, entry)
		return
	}
	if !s.previews.allowFetch() {
		logRequestIngress(r, "preview_budget")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	outcome, err := requestRealmPreview(realm, serverLabel, sessionID)
	if err != nil {
		logRequestIngress(r, "preview_unreachable")
		http.Error(w, "the realm broker is unreachable", http.StatusServiceUnavailable)
		return
	}
	var entry previewEntry
	if outcome.code != "" {
		// The code is a closed set the UI maps to its own copy; broker text is
		// never echoed (same rule as creation and adoption).
		entry = previewEntry{status: previewSessionStatus(outcome.code), body: []byte(outcome.code)}
	} else {
		body, marshalErr := json.Marshal(map[string]any{
			"realm":       realmName,
			"server":      serverLabel,
			"session_id":  sessionID,
			"rows":        outcome.rows,
			"ansi_rows":   outcome.ansiRows,
			"width":       outcome.width,
			"height":      outcome.height,
			"captured_at": outcome.capturedAt,
			"truncated":   outcome.truncated,
		})
		if marshalErr != nil {
			http.Error(w, "preview unavailable", http.StatusInternalServerError)
			return
		}
		entry = previewEntry{status: http.StatusOK, json: true, body: body}
	}
	s.previews.store(key, entry)
	writePreviewEntry(w, entry)
}

func writePreviewEntry(w http.ResponseWriter, entry previewEntry) {
	if entry.json {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(entry.status)
		_, _ = w.Write(entry.body)
		return
	}
	http.Error(w, string(entry.body), entry.status)
}

type previewOutcome struct {
	code          string
	rows          []string
	ansiRows      []string
	width, height int
	capturedAt    int64
	truncated     bool
}

// requestRealmPreview returns a refusal code in outcome.code, or the rows. A
// transport or protocol failure is an error, never a silent refusal, and a
// response that violates the wire bounds is refused rather than relayed.
func requestRealmPreview(realm config.Realm, server, sessionID string) (previewOutcome, error) {
	c, err := dial(realm.Socket, realm.BrokerUID)
	if err != nil {
		return previewOutcome{}, err
	}
	defer c.Close()
	if err = hello(c); err != nil {
		return previewOutcome{}, err
	}
	if err = writeControl(c, proto.Control{Type: "preview", ServerLabel: server, SessionID: sessionID}); err != nil {
		return previewOutcome{}, err
	}
	f, err := proto.ReadFrame(c)
	if err != nil {
		return previewOutcome{}, err
	}
	if f.Type != proto.FrameControl {
		return previewOutcome{}, fmt.Errorf("invalid preview response")
	}
	m, err := proto.DecodeControl(f.Payload)
	if err != nil {
		return previewOutcome{}, err
	}
	switch m.Type {
	case "preview_ok":
		if m.Width < 1 || m.Width > 1000 || m.Height < 1 || m.Height > 1000 || m.FrozenAt <= 0 || len(m.Lines) > proto.PreviewRowLimit+1 || m.ANSILines != nil && len(m.ANSILines) != len(m.Lines) {
			return previewOutcome{}, fmt.Errorf("preview response out of bounds")
		}
		rows := m.Lines
		if rows == nil {
			rows = []string{}
		}
		return previewOutcome{rows: rows, ansiRows: m.ANSILines, width: m.Width, height: m.Height, capturedAt: m.FrozenAt, truncated: m.Truncated}, nil
	case "preview_refused":
		if m.Code == "" {
			return previewOutcome{}, fmt.Errorf("refusal without a code")
		}
		return previewOutcome{code: m.Code}, nil
	default:
		return previewOutcome{}, fmt.Errorf("unexpected preview response %q", m.Type)
	}
}
