package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"persea-terminal/internal/proto"
)

var labelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var imageStagingPathRE = regexp.MustCompile(`^[A-Za-z0-9/_.-]+$`)

// MaxConfigBytes bounds front-door and broker configuration reads before decode.
const MaxConfigBytes = 1 << 20

type TmuxServer struct {
	Label      string `json:"label"`
	SocketName string `json:"socket_name,omitempty"`
	SocketPath string `json:"socket_path,omitempty"`
}

// SessionNameRE is the baseline session-name grammar. An operator's
// name_pattern may only narrow this, never widen it: the name reaches tmux as a
// command argument, and `.` and `:` are tmux target-syntax separators, so they
// are excluded here regardless of what a manifest asks for.
var SessionNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// DefaultSessionNamePattern is applied when a realm enables creation without
// stating its own grammar.
const DefaultSessionNamePattern = `^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`

// SessionCreate opts a realm in to creating new tmux sessions. Absent means
// disabled, so an existing deployment keeps the attach-only invariant.
//
// The browser supplies a name and nothing else. StartDirectory and the geometry
// come from this file only: accepting a command or working directory over HTTP
// would turn the front door into a remote execution service.
type SessionCreate struct {
	Enabled        bool     `json:"enabled"`
	Servers        []string `json:"servers,omitempty"`
	NamePattern    string   `json:"name_pattern,omitempty"`
	MaxSessions    int      `json:"max_sessions,omitempty"`
	StartDirectory string   `json:"start_directory,omitempty"`
	Columns        int      `json:"columns,omitempty"`
	Rows           int      `json:"rows,omitempty"`

	pattern *regexp.Regexp
	// configured is the realm's full set of server labels, captured at load time.
	// Without it an empty Servers list ("every configured server") would make
	// AllowsServer answer true for a label that does not exist — a method that
	// does not mean what its name says is a trap for the next caller.
	configured map[string]bool
}

// UnifiedTerminalDev enables one exact, development-only journal-backed target.
// It is intentionally a closed single-target allowlist rather than a general
// engine policy.
type UnifiedTerminalDev struct {
	Enabled bool   `json:"enabled"`
	Server  string `json:"server"`
	Session string `json:"session"`
	// ObserverSession is deprecated: it was the anchor session the single
	// global control observer attached to. Per-session observer units observe
	// exactly the session they create, so nothing attaches to this anchor
	// anymore. The field stays accepted and validated so deployed configs keep
	// loading unchanged.
	ObserverSession string `json:"observer_session"`
	RuntimeDir      string `json:"runtime_dir"`
	// AdoptionSlots overrides the journal's complete-pane slot cap for this
	// realm. Zero keeps the nine-slot journal default: eight ordinary sources
	// and one transactional successor. Births, adoptions and retained journals
	// share the ledger; retirement releases a slot only after actual cleanup.
	AdoptionSlots int `json:"adoption_slots,omitempty"`
}

// AllowsServer reports whether this realm may create sessions on a server
// label. An empty Servers list means every configured server, never an
// unconfigured one.
func (s *SessionCreate) AllowsServer(label string) bool {
	if s == nil || !s.Enabled || !s.configured[label] {
		return false
	}
	if len(s.Servers) == 0 {
		return true
	}
	for _, allowed := range s.Servers {
		if allowed == label {
			return true
		}
	}
	return false
}

// AllowsName applies the baseline grammar first, then the realm's own pattern.
func (s *SessionCreate) AllowsName(name string) bool {
	if s == nil || !s.Enabled || !SessionNameRE.MatchString(name) {
		return false
	}
	return s.pattern != nil && s.pattern.MatchString(name)
}

type Broker struct {
	Realm              string              `json:"realm"`
	FrontUID           uint32              `json:"front_uid"`
	Servers            []TmuxServer        `json:"servers"`
	SessionCreate      *SessionCreate      `json:"session_create,omitempty"`
	UnifiedTerminalDev *UnifiedTerminalDev `json:"unified_terminal_dev,omitempty"`
	// ImageStagingRoot overrides DefaultImageStagingRoot for this host; empty
	// means the default. Loading normalizes it, so validated configs always
	// carry the effective root.
	ImageStagingRoot   string `json:"image_staging_root,omitempty"`
	ImageStagingDir    string `json:"image_staging_dir,omitempty"`
	FrontUIDConfigured bool   `json:"-"`
}
type Realm struct {
	Name                string `json:"name"`
	DisplayName         string `json:"display_name,omitempty"`
	Socket              string `json:"socket"`
	BrokerUID           uint32 `json:"broker_uid"`
	BrokerUIDConfigured bool   `json:"-"`
}
type Alias struct {
	Alias   string `json:"alias"`
	Realm   string `json:"realm"`
	Server  string `json:"server"`
	Session string `json:"session"`
}
type Ingress struct {
	SocketPath    string `json:"socket_path"`
	PeerUID       uint32 `json:"peer_uid"`
	CanonicalHost string `json:"canonical_host"`
	OperatorLogin string `json:"operator_login"`
	// HermeticTLS declares that the disposable loopback adapter in front of a
	// hermetic ingress terminates TLS, so the external origin is https and the
	// forwarded proto must be https. It exists because WebKit refuses Secure
	// cookies on any http origin, loopback included, and the CSRF cookie is
	// unconditionally Secure. Validation rejects it on anything that is not a
	// hermetic ingress, so production can never carry it.
	HermeticTLS       bool `json:"hermetic_tls,omitempty"`
	MaxConnections    int  `json:"max_connections"`
	PeerUIDConfigured bool `json:"-"`
}

func (i *Ingress) UnmarshalJSON(data []byte) error {
	type wire struct {
		SocketPath     string  `json:"socket_path"`
		PeerUID        *uint32 `json:"peer_uid"`
		CanonicalHost  string  `json:"canonical_host"`
		OperatorLogin  string  `json:"operator_login"`
		HermeticTLS    bool    `json:"hermetic_tls"`
		MaxConnections int     `json:"max_connections"`
	}
	var v wire
	if err := strictObject(data, &v); err != nil {
		return err
	}
	i.SocketPath, i.CanonicalHost, i.OperatorLogin, i.MaxConnections = v.SocketPath, v.CanonicalHost, v.OperatorLogin, v.MaxConnections
	i.HermeticTLS = v.HermeticTLS
	if v.PeerUID != nil {
		i.PeerUID, i.PeerUIDConfigured = *v.PeerUID, true
	}
	return nil
}

type Front struct {
	Ingress        Ingress `json:"ingress"`
	Realms         []Realm `json:"realms"`
	Aliases        []Alias `json:"aliases,omitempty"`
	AliasStorePath string  `json:"alias_store_path"`
	// PreferencesStorePath holds per-operator preferences (theme, font size,
	// default session). Optional: an absent path leaves the store unconfigured
	// and the routes answer defaults / 503 honestly. When present it is
	// validated like alias_store_path; the deployment generator always emits
	// it beside the alias store inside the front's 0700 state directory.
	PreferencesStorePath string `json:"preferences_store_path,omitempty"`
	// SnippetStorePath holds the global snippets/clips store (secret-bearing
	// terminal text). Same optionality and validation as the preferences
	// store; same generated home beside the alias store.
	SnippetStorePath string `json:"snippet_store_path,omitempty"`
	// WorkspaceStorePath holds authority-free named workspace records. It is
	// optional for old/dev configs and, when configured, must be a distinct
	// clean absolute path beside the other front-owned stores.
	WorkspaceStorePath string `json:"workspace_store_path,omitempty"`
	// KeyboardPreferencesStorePath is separate so rollback leaves older closed
	// preference schemas readable. Optional for existing/dev configurations.
	KeyboardPreferencesStorePath string `json:"keyboard_preferences_store_path,omitempty"`

	DiagnosticTraceDir string `json:"diagnostic_trace_dir,omitempty"`
	// ImageStagingRoot overrides DefaultImageStagingRoot for this host; empty
	// means the default. Loading normalizes it. The front never opens the
	// root — it only confines broker-returned staged paths against it.
	ImageStagingRoot    string `json:"image_staging_root,omitempty"`
	ImageUploadMaxBytes int    `json:"image_upload_max_bytes,omitempty"`
	HandleTTLSeconds    int    `json:"handle_ttl_seconds,omitempty"`
	HandleCapacity      int    `json:"handle_capacity,omitempty"`
}

func (c Broker) HasFrontUID() bool { return c.FrontUIDConfigured }
func (r Realm) HasBrokerUID() bool { return r.BrokerUIDConfigured }
func (r Realm) DisplayLabel() string {
	if r.DisplayName != "" {
		return r.DisplayName
	}
	return r.Name
}

func validDisplayName(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 128 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	return true
}

func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var consume func() error
	consume = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make([]string, 0)
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("object key is not a string")
				}
				for _, prior := range seen {
					if strings.EqualFold(prior, key) {
						return fmt.Errorf("duplicate object key %q", key)
					}
				}
				seen = append(seen, key)
				if err := consume(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := consume(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delim)
		}
		_, err = dec.Token()
		return err
	}
	if err := consume(); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing object data")
		}
		return err
	}
	return nil
}

func strictObject(data []byte, dst any) error {
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing object data")
	}
	return nil
}

func (c *Broker) UnmarshalJSON(data []byte) error {
	type wire struct {
		Realm              string              `json:"realm"`
		FrontUID           *uint32             `json:"front_uid"`
		Servers            []TmuxServer        `json:"servers"`
		SessionCreate      *SessionCreate      `json:"session_create"`
		UnifiedTerminalDev *UnifiedTerminalDev `json:"unified_terminal_dev"`
		ImageStagingRoot   string              `json:"image_staging_root"`
		ImageStagingDir    string              `json:"image_staging_dir"`
	}
	var v wire
	if err := strictObject(data, &v); err != nil {
		return err
	}
	c.Realm, c.Servers, c.SessionCreate, c.UnifiedTerminalDev, c.ImageStagingRoot, c.ImageStagingDir = v.Realm, v.Servers, v.SessionCreate, v.UnifiedTerminalDev, v.ImageStagingRoot, v.ImageStagingDir
	if v.FrontUID != nil {
		c.FrontUID, c.FrontUIDConfigured = *v.FrontUID, true
	}
	return nil
}

func (r *Realm) UnmarshalJSON(data []byte) error {
	type wire struct {
		Name        string  `json:"name"`
		DisplayName string  `json:"display_name,omitempty"`
		Socket      string  `json:"socket"`
		BrokerUID   *uint32 `json:"broker_uid"`
	}
	var v wire
	if err := strictObject(data, &v); err != nil {
		return err
	}
	r.Name, r.DisplayName, r.Socket = v.Name, v.DisplayName, v.Socket
	if v.BrokerUID != nil {
		r.BrokerUID, r.BrokerUIDConfigured = *v.BrokerUID, true
	}
	return nil
}

func load(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil {
		return err
	}
	if len(b) > MaxConfigBytes {
		return fmt.Errorf("config exceeds %d bytes", MaxConfigBytes)
	}
	if err := rejectDuplicateKeys(b); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("invalid trailing config data")
		}
		return fmt.Errorf("invalid trailing config data: %w", err)
	}
	return nil
}

func LoadBroker(path string) (Broker, error) {
	var c Broker
	if err := load(path, &c); err != nil {
		return c, err
	}
	if !labelRE.MatchString(c.Realm) || len(c.Servers) == 0 || len(c.Servers) > 64 {
		return c, fmt.Errorf("invalid realm or server count")
	}
	if !c.FrontUIDConfigured {
		return c, fmt.Errorf("front_uid is required")
	}
	if c.ImageStagingRoot == "" {
		c.ImageStagingRoot = DefaultImageStagingRoot
	} else if !validImageStagingRoot(c.ImageStagingRoot) {
		return c, fmt.Errorf("image_staging_root must be a clean absolute path outside %s", strings.Join(forbiddenImageStagingParents, ", "))
	}
	if c.ImageStagingDir != "" && !imageStagingPath(c.Realm, c.ImageStagingRoot, c.ImageStagingDir) {
		return c, fmt.Errorf("image_staging_dir must be a clean absolute path within the realm staging root")
	}
	seen := map[string]bool{}
	for _, s := range c.Servers {
		if !labelRE.MatchString(s.Label) || seen[s.Label] || ((s.SocketName == "") == (s.SocketPath == "")) {
			return c, fmt.Errorf("invalid or duplicate server %q", s.Label)
		}
		if s.SocketName != "" && !labelRE.MatchString(s.SocketName) {
			return c, fmt.Errorf("invalid socket name")
		}
		if s.SocketPath != "" && (!filepath.IsAbs(s.SocketPath) || filepath.Clean(s.SocketPath) != s.SocketPath) {
			return c, fmt.Errorf("socket_path must be clean and absolute")
		}
		seen[s.Label] = true
	}
	if err := validateSessionCreate(c.SessionCreate, seen); err != nil {
		return c, err
	}
	if err := validateUnifiedTerminalDev(c.UnifiedTerminalDev, c.SessionCreate, seen); err != nil {
		return c, err
	}
	return c, nil
}

func validateUnifiedTerminalDev(dev *UnifiedTerminalDev, create *SessionCreate, servers map[string]bool) error {
	if dev == nil {
		return nil
	}
	if !dev.Enabled {
		if dev.Server != "" || dev.Session != "" || dev.ObserverSession != "" || dev.RuntimeDir != "" || dev.AdoptionSlots != 0 {
			return fmt.Errorf("disabled unified_terminal_dev must not carry authority")
		}
		return nil
	}
	if dev.AdoptionSlots < 0 || dev.AdoptionSlots > 4096 {
		return fmt.Errorf("unified_terminal_dev.adoption_slots is out of bounds")
	}
	if !servers[dev.Server] || create == nil || !create.AllowsServer(dev.Server) || !create.AllowsName(dev.Session) {
		return fmt.Errorf("unified_terminal_dev target is outside session_create policy")
	}
	if !SessionNameRE.MatchString(dev.ObserverSession) || dev.ObserverSession == dev.Session {
		return fmt.Errorf("unified_terminal_dev observer_session is invalid")
	}
	if !filepath.IsAbs(dev.RuntimeDir) || filepath.Clean(dev.RuntimeDir) != dev.RuntimeDir {
		return fmt.Errorf("unified_terminal_dev.runtime_dir must be clean and absolute")
	}
	return nil
}

func validateSessionCreate(s *SessionCreate, servers map[string]bool) error {
	if s == nil {
		return nil
	}
	if !s.Enabled {
		// A disabled block must still be well formed, but nothing is derived from
		// it; leaving the compiled pattern nil keeps AllowsName closed.
		return nil
	}
	s.configured = servers
	labels := map[string]bool{}
	for _, label := range s.Servers {
		if !servers[label] || labels[label] {
			return fmt.Errorf("session_create.servers references unknown or duplicate server %q", label)
		}
		labels[label] = true
	}
	if s.NamePattern == "" {
		s.NamePattern = DefaultSessionNamePattern
	}
	if !strings.HasPrefix(s.NamePattern, "^") || !strings.HasSuffix(s.NamePattern, "$") {
		return fmt.Errorf("session_create.name_pattern must be anchored with ^ and $")
	}
	pattern, err := regexp.Compile(s.NamePattern)
	if err != nil {
		return fmt.Errorf("invalid session_create.name_pattern: %w", err)
	}
	s.pattern = pattern
	if s.MaxSessions == 0 {
		s.MaxSessions = 20
	}
	if s.MaxSessions < 1 || s.MaxSessions > 256 {
		return fmt.Errorf("invalid session_create.max_sessions")
	}
	if s.StartDirectory != "" && (!filepath.IsAbs(s.StartDirectory) || filepath.Clean(s.StartDirectory) != s.StartDirectory) {
		return fmt.Errorf("session_create.start_directory must be a clean absolute path")
	}
	if s.Columns < 0 || s.Columns > 1000 || s.Rows < 0 || s.Rows > 1000 {
		return fmt.Errorf("invalid session_create geometry")
	}
	if (s.Columns == 0) != (s.Rows == 0) {
		return fmt.Errorf("session_create columns and rows must be set together")
	}
	return nil
}

// validateOptionalStorePath accepts an absent store path and otherwise
// requires a clean absolute path distinct from every other store file, so two
// stores can never share (and clobber) one file.
func validateOptionalStorePath(field, value string, others ...string) error {
	if value == "" {
		return nil
	}
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return fmt.Errorf("%s must be a clean absolute path", field)
	}
	for _, other := range others {
		if other != "" && other == value {
			return fmt.Errorf("%s must not reuse another store path", field)
		}
	}
	return nil
}

func LoadFront(path string) (Front, error) {
	var c Front
	if err := load(path, &c); err != nil {
		return c, err
	}
	if len(c.Realms) == 0 || len(c.Realms) > 64 {
		return c, fmt.Errorf("invalid realm count")
	}
	if err := validateIngress(c.Ingress); err != nil {
		return c, err
	}
	if c.AliasStorePath == "" || !filepath.IsAbs(c.AliasStorePath) || filepath.Clean(c.AliasStorePath) != c.AliasStorePath {
		return c, fmt.Errorf("alias_store_path must be a clean absolute path")
	}
	if err := validateOptionalStorePath("preferences_store_path", c.PreferencesStorePath, c.AliasStorePath); err != nil {
		return c, err
	}
	if err := validateOptionalStorePath("snippet_store_path", c.SnippetStorePath, c.AliasStorePath, c.PreferencesStorePath); err != nil {
		return c, err
	}
	if err := validateOptionalStorePath("workspace_store_path", c.WorkspaceStorePath, c.AliasStorePath, c.PreferencesStorePath, c.SnippetStorePath); err != nil {
		return c, err
	}
	if err := validateOptionalStorePath("keyboard_preferences_store_path", c.KeyboardPreferencesStorePath, c.AliasStorePath, c.PreferencesStorePath, c.SnippetStorePath, c.WorkspaceStorePath); err != nil {
		return c, err
	}
	if c.KeyboardPreferencesStorePath != "" && filepath.Dir(c.KeyboardPreferencesStorePath) != filepath.Dir(c.AliasStorePath) {
		return c, fmt.Errorf("keyboard_preferences_store_path must be a sibling of alias_store_path")
	}
	if c.DiagnosticTraceDir != "" && !diagnosticTracePath(c.DiagnosticTraceDir) {
		return c, fmt.Errorf("diagnostic_trace_dir must be a clean absolute path below %s", DiagnosticTraceRoot)
	}
	if c.ImageStagingRoot == "" {
		c.ImageStagingRoot = DefaultImageStagingRoot
	} else if !validImageStagingRoot(c.ImageStagingRoot) {
		return c, fmt.Errorf("image_staging_root must be a clean absolute path outside %s", strings.Join(forbiddenImageStagingParents, ", "))
	}
	if c.ImageUploadMaxBytes != 0 && (c.ImageUploadMaxBytes < 1<<20 || c.ImageUploadMaxBytes > proto.MaxImage) {
		return c, fmt.Errorf("image_upload_max_bytes must be zero or between %d and %d", 1<<20, proto.MaxImage)
	}
	seen := map[string]bool{}
	for _, r := range c.Realms {
		if !labelRE.MatchString(r.Name) || !validDisplayName(r.DisplayName) || seen[r.Name] || !filepath.IsAbs(r.Socket) || filepath.Clean(r.Socket) != r.Socket || !r.BrokerUIDConfigured {
			return c, fmt.Errorf("invalid or duplicate realm %q", r.Name)
		}
		seen[r.Name] = true
	}
	if c.HandleTTLSeconds == 0 {
		c.HandleTTLSeconds = 120
	}
	if c.HandleTTLSeconds < 1 || c.HandleTTLSeconds > 3600 {
		return c, fmt.Errorf("invalid handle TTL")
	}
	if c.HandleCapacity == 0 {
		c.HandleCapacity = 4096
	}
	if c.HandleCapacity < 1 || c.HandleCapacity > 65536 {
		return c, fmt.Errorf("invalid handle capacity")
	}
	aliasSeen := map[string]bool{}
	for _, a := range c.Aliases {
		if !labelRE.MatchString(a.Alias) || aliasSeen[a.Alias] || !seen[a.Realm] || !labelRE.MatchString(a.Server) || a.Session == "" || len(a.Session) > 128 {
			return c, fmt.Errorf("invalid or duplicate alias %q", a.Alias)
		}
		aliasSeen[a.Alias] = true
	}
	return c, nil
}

// DefaultImageStagingRoot is the default global subtree from which a broker
// derives its realm-owned image staging directory. The root is host-level
// configuration (image_staging_root, rendered from the deployment manifest's
// front.staging_root); this constant is only the value an absent key means.
// The default lives outside the front door's state directory on purpose: the
// alias store requires /var/lib/persea-terminal to keep mode exactly 0700,
// while realm brokers need traversal into the staging root, and the two
// demands cannot meet in one directory.
const DefaultImageStagingRoot = "/var/lib/persea-terminal-staging"

// forbiddenImageStagingParents are subtrees an image staging root may never
// live under (or be): the front's exact-0700 state directory, the
// world-writable ephemeral tree, and the read-only install root.
var forbiddenImageStagingParents = []string{"/var/lib/persea-terminal", "/tmp", "/opt/persea-terminal"}

func validImageStagingRoot(root string) bool {
	if root == "" || root == "/" || !filepath.IsAbs(root) ||
		filepath.Clean(root) != root || !imageStagingPathRE.MatchString(root) {
		return false
	}
	for _, forbidden := range forbiddenImageStagingParents {
		relative, err := filepath.Rel(forbidden, root)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return false
		}
	}
	return true
}

func imageStagingPath(realm, root, candidate string) bool {
	if !labelRE.MatchString(realm) || candidate == "" || !filepath.IsAbs(candidate) ||
		filepath.Clean(candidate) != candidate || !imageStagingPathRE.MatchString(candidate) {
		return false
	}
	realmRoot := filepath.Join(root, realm)
	relative, err := filepath.Rel(realmRoot, candidate)
	return err == nil && (relative == "." ||
		(relative != "" && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

// DiagnosticTraceRoot is the only filesystem subtree where the temporary
// metadata-only capture endpoint may persist evidence. Keeping the boundary in
// config makes a deployment outside the analyst-owned evidence tree
// unrepresentable before any HTTP route exists.
const DiagnosticTraceRoot = "/var/lib/persea-terminal-diagnostics"

func diagnosticTracePath(path string) bool {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	relative, err := filepath.Rel(DiagnosticTraceRoot, path)
	return err == nil && relative != "." && relative != "" &&
		relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// ProductionSocketPath is the only socket path a production ingress may use.
const ProductionSocketPath = "/run/persea-terminal/front.sock"

// Hermetic reports whether an ingress is the loopback test shape for a given
// effective uid. This is the single definition of that predicate: the front door
// asks the same question when choosing an external origin and a forwarded-proto
// expectation, and two implementations of one rule drift.
func Hermetic(i Ingress, effectiveUID uint32) bool {
	return effectiveUID != 0 && i.PeerUID == effectiveUID &&
		i.SocketPath != ProductionSocketPath && isLoopbackAuthority(i.CanonicalHost)
}

func validateIngress(i Ingress) error {
	return validateIngressForEUID(i, uint32(os.Geteuid()))
}

func validateIngressForEUID(i Ingress, effectiveUID uint32) error {
	if !i.PeerUIDConfigured {
		return fmt.Errorf("ingress.peer_uid is required")
	}
	if i.SocketPath == "" || !filepath.IsAbs(i.SocketPath) || filepath.Clean(i.SocketPath) != i.SocketPath || len(i.SocketPath) > 107 {
		return fmt.Errorf("invalid ingress.socket_path")
	}
	if i.OperatorLogin == "" || strings.TrimSpace(i.OperatorLogin) != i.OperatorLogin || strings.ContainsAny(i.OperatorLogin, "\r\n") {
		return fmt.Errorf("invalid ingress.operator_login")
	}
	if i.MaxConnections < 1 || i.MaxConnections > 256 {
		return fmt.Errorf("invalid ingress.max_connections")
	}
	if i.CanonicalHost == "" || i.CanonicalHost != strings.ToLower(i.CanonicalHost) || strings.ContainsAny(i.CanonicalHost, "/\\@ \t\r\n") {
		return fmt.Errorf("invalid ingress.canonical_host")
	}
	// Fail closed: hermetic_tls is only ever legal on an ingress that Hermetic()
	// already recognises. Asking Hermetic() itself — rather than re-deriving the
	// shape here — is what keeps a production ingress from ever carrying it, and
	// keeps this rule and the front door's origin choice on one definition.
	if i.HermeticTLS && !Hermetic(i, effectiveUID) {
		return fmt.Errorf("ingress.hermetic_tls requires a hermetic loopback ingress")
	}
	hermeticAuthority := isLoopbackAuthority(i.CanonicalHost)
	if effectiveUID != 0 && i.PeerUID == effectiveUID && hermeticAuthority {
		if i.SocketPath == ProductionSocketPath {
			return fmt.Errorf("hermetic ingress forbidden on production socket")
		}
		return nil
	}
	if hermeticAuthority || i.PeerUID != 0 || i.SocketPath != ProductionSocketPath || !validProductionHost(i.CanonicalHost) {
		return fmt.Errorf("invalid production ingress")
	}
	return nil
}

func validProductionHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) < 1 || len(label) > 63 || !asciiAlphaNumeric(label[0]) || !asciiAlphaNumeric(label[len(label)-1]) {
			return false
		}
		for i := 1; i < len(label)-1; i++ {
			if !asciiAlphaNumeric(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	return true
}

func asciiAlphaNumeric(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}

func isLoopbackAuthority(v string) bool {
	h, p, err := net.SplitHostPort(v)
	if err != nil || p == "" || (len(p) > 1 && p[0] == '0') {
		return false
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != p {
		return false
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	if ip.To4() == nil {
		return h == "::1"
	}
	return h == ip.String()
}
