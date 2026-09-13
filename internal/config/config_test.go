package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	if strings.Contains(body, `"realms"`) && !strings.Contains(body, `"ingress"`) {
		body = strings.Replace(body, "{", `{"ingress":{"socket_path":"/run/persea-terminal/front.sock","peer_uid":0,"canonical_host":"terminal.example.ts.net","operator_login":"operator@example.com","max_connections":64},`, 1)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigByteLimitBeforeDecode(t *testing.T) {
	front := `{"ingress":{"socket_path":"/run/persea-terminal/front.sock","peer_uid":0,"canonical_host":"terminal.example.ts.net","operator_login":"operator@example.com","max_connections":64},"realms":[{"name":"r","socket":"/tmp/b.sock","broker_uid":0}],"alias_store_path":"/tmp/aliases.json"}`
	broker := `{"realm":"r","front_uid":0,"servers":[{"label":"s","socket_path":"/tmp/tmux.sock"}]}`
	for name, tc := range map[string]struct {
		body string
		load func(string) error
	}{
		"front":  {front, func(path string) error { _, err := LoadFront(path); return err }},
		"broker": {broker, func(path string) error { _, err := LoadBroker(path); return err }},
	} {
		t.Run(name+"_over_limit", func(t *testing.T) {
			body := tc.body + strings.Repeat(" ", MaxConfigBytes-len(tc.body)+1)
			if err := tc.load(write(t, body)); err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("over-limit config error = %v", err)
			}
		})
	}
	t.Run("valid_at_limit", func(t *testing.T) {
		body := front + strings.Repeat(" ", MaxConfigBytes-len(front))
		if _, err := LoadFront(write(t, body)); err != nil {
			t.Fatalf("at-limit config rejected: %v", err)
		}
	})
}
func TestBrokerConfigStrictAndSelectorExclusive(t *testing.T) {
	if _, e := LoadBroker(write(t, `{"realm":"r","front_uid":0,"servers":[{"label":"s","socket_path":"/tmp/x"}]}`)); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{`{"realm":"r","servers":[{"label":"s"}]}`, `{"realm":"r","servers":[{"label":"s","socket_name":"x","socket_path":"/tmp/x"}]}`, `{"realm":"r","servers":[],"unknown":1}`} {
		if _, e := LoadBroker(write(t, bad)); e == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
func TestFrontConfigDefaultsBoundsAndUnknownFields(t *testing.T) {
	c, e := LoadFront(write(t, `{"realms":[{"name":"r","socket":"/tmp/b.sock","broker_uid":0}],"alias_store_path":"/tmp/aliases.json"}`))
	if e != nil || c.HandleTTLSeconds != 120 || c.HandleCapacity != 4096 {
		t.Fatalf("%+v %v", c, e)
	}
	if c.Realms[0].DisplayLabel() != "r" {
		t.Fatalf("missing display-name fallback: %+v", c.Realms[0])
	}
	if _, e = LoadFront(write(t, `{"realms":[{"name":"r","socket":"/tmp/b","broker_uid":0}],"alias_store_path":"/tmp/aliases.json","extra":true}`)); e == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestDiagnosticTraceDirectoryIsConfinedToEvidenceRoot(t *testing.T) {
	valid := `{"realms":[{"name":"r","socket":"/tmp/b.sock","broker_uid":0}],"alias_store_path":"/tmp/aliases.json","diagnostic_trace_dir":"/var/lib/persea-terminal-diagnostics/diagnostic-run"}`
	loaded, err := LoadFront(write(t, valid))
	if err != nil || loaded.DiagnosticTraceDir != "/var/lib/persea-terminal-diagnostics/diagnostic-run" {
		t.Fatalf("valid diagnostic trace directory = %q, %v", loaded.DiagnosticTraceDir, err)
	}
	for _, path := range []string{
		"/tmp/diagnostic-run",
		"/var/lib/persea-terminal-diagnostics",
		"/var/lib/persea-terminal-diagnostics/../escape",
		"data4/agent/artifacts/evidence/persea-terminal/run",
	} {
		body := strings.Replace(valid, "/var/lib/persea-terminal-diagnostics/diagnostic-run", path, 1)
		if _, err := LoadFront(write(t, body)); err == nil {
			t.Fatalf("unsafe diagnostic trace directory %q accepted", path)
		}
	}
}

func TestRealmDisplayNameIsDisplayOnlyAndBounded(t *testing.T) {
	c, err := LoadFront(write(t, `{"realms":[{"name":"stable","display_name":"Fixture Operator K7M2","socket":"/tmp/b.sock","broker_uid":1000}],"alias_store_path":"/tmp/aliases.json"}`))
	if err != nil || c.Realms[0].Name != "stable" || c.Realms[0].DisplayLabel() != "Fixture Operator K7M2" {
		t.Fatalf("display name changed stable identity: %+v %v", c.Realms[0], err)
	}
	for _, display := range []string{" leading", "trailing ", "line\nbreak", "zero\u200bwidth", strings.Repeat("x", 129)} {
		body, marshalErr := json.Marshal(map[string]any{
			"realms":           []any{map[string]any{"name": "stable", "display_name": display, "socket": "/tmp/b.sock", "broker_uid": 1000}},
			"alias_store_path": "/tmp/aliases.json",
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, loadErr := LoadFront(write(t, string(body))); loadErr == nil {
			t.Fatalf("accepted unsafe display name %q", display)
		}
	}
}

func TestIngressConfigStrictConnectionBound(t *testing.T) {
	for _, maximum := range []int{0, 257} {
		body := strings.Replace(`{"ingress":{"socket_path":"/run/persea-terminal/front.sock","peer_uid":0,"canonical_host":"terminal.example.ts.net","operator_login":"operator@example.com","max_connections":64},"realms":[{"name":"r","socket":"/tmp/b.sock","broker_uid":0}],"alias_store_path":"/tmp/aliases.json"}`, `"max_connections":64`, `"max_connections":`+fmt.Sprint(maximum), 1)
		if _, err := LoadFront(write(t, body)); err == nil {
			t.Fatalf("accepted max_connections=%d", maximum)
		}
	}
}

func TestIngressProductionHermeticSplit(t *testing.T) {
	tests := []struct {
		name         string
		effectiveUID uint32
		in           Ingress
		ok           bool
	}{
		{"ordinary hermetic", 1000, Ingress{SocketPath: "/tmp/front.sock", PeerUID: 1000, PeerUIDConfigured: true, CanonicalHost: "127.0.0.1:43210", OperatorLogin: "operator@example.com", MaxConnections: 1}, true},
		{"root loopback private", 0, Ingress{SocketPath: "/tmp/front.sock", PeerUID: 0, PeerUIDConfigured: true, CanonicalHost: "127.0.0.1:43210", OperatorLogin: "operator@example.com", MaxConnections: 1}, false},
		{"root dns private", 0, Ingress{SocketPath: "/tmp/front.sock", PeerUID: 0, PeerUIDConfigured: true, CanonicalHost: "terminal.example.ts.net", OperatorLogin: "operator@example.com", MaxConnections: 1}, false},
		{"root dns production", 0, Ingress{SocketPath: "/run/persea-terminal/front.sock", PeerUID: 0, PeerUIDConfigured: true, CanonicalHost: "terminal.example.ts.net", OperatorLogin: "operator@example.com", MaxConnections: 1}, true},
		{"hermetic authority production", 1000, Ingress{SocketPath: "/run/persea-terminal/front.sock", PeerUID: 1000, PeerUIDConfigured: true, CanonicalHost: "localhost:43210", OperatorLogin: "operator@example.com", MaxConnections: 1}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateIngressForEUID(tc.in, tc.effectiveUID); (err == nil) != tc.ok {
				t.Fatalf("validateIngress() error = %v, want success=%v", err, tc.ok)
			}
		})
	}
}

func TestIngressCanonicalHostsAndPorts(t *testing.T) {
	production := Ingress{SocketPath: "/run/persea-terminal/front.sock", PeerUID: 0, PeerUIDConfigured: true, OperatorLogin: "operator@example.com", MaxConnections: 1}
	for _, host := range []string{"a..b", "-a.example", "a-.example", strings.Repeat("a", 64) + ".example", "under_score.example", "caf\u00e9.example", "example.com.", "https://example.com", "example.com:443"} {
		t.Run("dns_"+host, func(t *testing.T) {
			in := production
			in.CanonicalHost = host
			if err := validateIngress(in); err == nil {
				t.Fatalf("invalid production host %q accepted", host)
			}
		})
	}
	for _, authority := range []string{"localhost:0", "localhost:080", "localhost:+80", "localhost:65536", "localhost:http", "192.0.2.1:80", "[0:0:0:0:0:0:0:1]:80"} {
		t.Run("authority_"+authority, func(t *testing.T) {
			if isLoopbackAuthority(authority) {
				t.Fatalf("noncanonical authority %q accepted", authority)
			}
		})
	}
	for _, authority := range []string{"localhost:80", "127.0.0.1:43210", "[::1]:65535"} {
		if !isLoopbackAuthority(authority) {
			t.Fatalf("canonical loopback authority %q rejected", authority)
		}
	}
}
func TestFrontConfigRejectsDuplicateAliasNames(t *testing.T) {
	if _, e := LoadFront(write(t, `{"realms":[{"name":"r","socket":"/tmp/b.sock","broker_uid":0}],"alias_store_path":"/tmp/aliases.json","aliases":[{"alias":"primary","realm":"r","server":"s","session":"one"},{"alias":"primary","realm":"r","server":"s","session":"two"}]}`)); e == nil {
		t.Fatal("duplicate alias name accepted")
	}
}

func TestRequiredUIDsRejectMissingNegativeOverflowNullAndAmbiguous(t *testing.T) {
	brokers := []string{
		`{"realm":"r","servers":[{"label":"s","socket_path":"/tmp/x"}]}`,
		`{"realm":"r","front_uid":-1,"servers":[{"label":"s","socket_path":"/tmp/x"}]}`,
		`{"realm":"r","front_uid":4294967296,"servers":[{"label":"s","socket_path":"/tmp/x"}]}`,
		`{"realm":"r","front_uid":null,"servers":[{"label":"s","socket_path":"/tmp/x"}]}`,
		`{"realm":"r","front_uid":1.0,"servers":[{"label":"s","socket_path":"/tmp/x"}]}`,
		`{"realm":"r","front_uid":0,"front_uid":0,"servers":[{"label":"s","socket_path":"/tmp/x"}]}`,
		`{"realm":"r","front_uid":0,"front_uid":1,"servers":[{"label":"s","socket_path":"/tmp/x"}]}`,
		`{"realm":"r","front_uid":0,"FRONT_UID":0,"servers":[{"label":"s","socket_path":"/tmp/x"}]}`,
		`{"realm":"r","front_uid":0,"FRONT_UID":1,"servers":[{"label":"s","socket_path":"/tmp/x"}]}`,
		`{"realm":"r","front_uid":0,"servers":[{"label":"s","label":"s","socket_path":"/tmp/x"}]}`,
	}
	fronts := []string{
		`{"realms":[{"name":"r","socket":"/tmp/x"}]}`,
		`{"realms":[{"name":"r","socket":"/tmp/x","broker_uid":-1}]}`,
		`{"realms":[{"name":"r","socket":"/tmp/x","broker_uid":4294967296}]}`,
		`{"realms":[{"name":"r","socket":"/tmp/x","broker_uid":null}]}`,
		`{"realms":[{"name":"r","socket":"/tmp/x","broker_uid":"0"}]}`,
		`{"realms":[{"name":"r","socket":"/tmp/x","broker_uid":0,"broker_uid":0}]}`,
		`{"realms":[{"name":"r","socket":"/tmp/x","broker_uid":0,"broker_uid":1}]}`,
		`{"realms":[{"name":"r","socket":"/tmp/x","broker_uid":0,"BROKER_UID":0}]}`,
		`{"realms":[{"name":"r","socket":"/tmp/x","broker_uid":0,"BROKER_UID":1}]}`,
		`{"realms":[{"name":"r","socket":"/tmp/x","broker_uid":0,"bro\u212Aer_uid":1}]}`,
		`{"realms":[{"name":"r","socket":"/tmp/x","broker_uid":0}],"aliases":[{"alias":"a","realm":"r","server":"s","session":"one","session":"two"}]}`,
	}
	for _, body := range brokers {
		if _, err := LoadBroker(write(t, body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	for _, body := range fronts {
		if _, err := LoadFront(write(t, body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	if c, err := LoadBroker(write(t, `{"realm":"r","front_uid":0,"servers":[{"label":"s","socket_path":"/tmp/x"}]}`)); err != nil || c.FrontUID != 0 {
		t.Fatalf("uid zero: %+v %v", c, err)
	}
	if c, err := LoadFront(write(t, `{"realms":[{"name":"r","socket":"/tmp/x","broker_uid":0}],"alias_store_path":"/tmp/aliases.json"}`)); err != nil || c.Realms[0].BrokerUID != 0 {
		t.Fatalf("uid zero: %+v %v", c, err)
	}
	if _, err := LoadBroker(write(t, `{"realm":"r","front_uid":1,"servers":[{"label":"s","socket_path":"/tmp/x"}]}`)); err != nil {
		t.Fatalf("distinct legitimate broker keys rejected: %v", err)
	}
	if _, err := LoadFront(write(t, `{"realms":[{"name":"r","socket":"/tmp/x","broker_uid":1}],"alias_store_path":"/tmp/aliases.json","aliases":[{"alias":"a","realm":"r","server":"s","session":"one"}]}`)); err != nil {
		t.Fatalf("distinct legitimate front keys rejected: %v", err)
	}
	programmaticBroker := Broker{FrontUID: 0, FrontUIDConfigured: true}
	programmaticRealm := Realm{BrokerUID: 0, BrokerUIDConfigured: true}
	if !programmaticBroker.HasFrontUID() || !programmaticRealm.HasBrokerUID() {
		t.Fatal("programmatic root UID presence was lost")
	}
}

func brokerWithCreate(create string) string {
	body := `{"realm":"r","front_uid":0,"servers":[{"label":"s","socket_path":"/tmp/tmux.sock"},{"label":"t","socket_name":"alt"}]`
	if create != "" {
		body += `,"session_create":` + create
	}
	return body + `}`
}

func TestSessionCreateAbsentKeepsAttachOnly(t *testing.T) {
	c, err := LoadBroker(write(t, brokerWithCreate("")))
	if err != nil {
		t.Fatalf("broker without session_create rejected: %v", err)
	}
	if c.SessionCreate != nil {
		t.Fatal("absent session_create must stay nil so creation is disabled")
	}
	// The nil receiver must answer closed rather than panic: every call site
	// reaches these through a possibly-absent block.
	if c.SessionCreate.AllowsServer("s") || c.SessionCreate.AllowsName("ok") {
		t.Fatal("absent session_create permitted creation")
	}
}

func TestSessionCreateDisabledBlockPermitsNothing(t *testing.T) {
	c, err := LoadBroker(write(t, brokerWithCreate(`{"enabled":false}`)))
	if err != nil {
		t.Fatalf("disabled session_create rejected: %v", err)
	}
	if c.SessionCreate.AllowsServer("s") || c.SessionCreate.AllowsName("ok") {
		t.Fatal("disabled session_create permitted creation")
	}
}

func TestSessionCreateDefaultsAndServerScope(t *testing.T) {
	c, err := LoadBroker(write(t, brokerWithCreate(`{"enabled":true,"servers":["s"]}`)))
	if err != nil {
		t.Fatalf("valid session_create rejected: %v", err)
	}
	if c.SessionCreate.MaxSessions != 20 || c.SessionCreate.NamePattern != DefaultSessionNamePattern {
		t.Fatalf("defaults not applied: %+v", c.SessionCreate)
	}
	if !c.SessionCreate.AllowsServer("s") {
		t.Fatal("listed server was refused")
	}
	if c.SessionCreate.AllowsServer("t") {
		t.Fatal("unlisted server was permitted")
	}
	// An empty list means every configured server, not none.
	all, err := LoadBroker(write(t, brokerWithCreate(`{"enabled":true}`)))
	if err != nil {
		t.Fatalf("session_create without servers rejected: %v", err)
	}
	if !all.SessionCreate.AllowsServer("s") || !all.SessionCreate.AllowsServer("t") {
		t.Fatal("empty server list did not mean all configured servers")
	}
	if all.SessionCreate.AllowsServer("absent") {
		t.Fatal("empty server list permitted an unconfigured server")
	}
}

// The operator's name_pattern may only narrow the baseline grammar. A permissive
// manifest must not be able to admit a tmux target-syntax separator, because the
// name reaches tmux as a command argument where ':' and '.' change addressing.
func TestSessionCreateOperatorPatternCannotWidenBaseline(t *testing.T) {
	c, err := LoadBroker(write(t, brokerWithCreate(`{"enabled":true,"name_pattern":"^.*$"}`)))
	if err != nil {
		t.Fatalf("permissive pattern rejected at load: %v", err)
	}
	for _, name := range []string{
		"has:colon", "has.dot", "has space", "has/slash", "has$dollar", "has;semi",
		"has'quote", "-leading-dash", "", "has\nnewline", strings.Repeat("x", 65),
	} {
		if c.SessionCreate.AllowsName(name) {
			t.Fatalf("baseline grammar admitted %q under a permissive operator pattern", name)
		}
	}
	for _, name := range []string{"a", "shell1", "Work_2", "a-b-c", strings.Repeat("x", 64)} {
		if !c.SessionCreate.AllowsName(name) {
			t.Fatalf("baseline grammar refused acceptable name %q", name)
		}
	}
}

func TestSessionCreateNarrowingPatternIsApplied(t *testing.T) {
	c, err := LoadBroker(write(t, brokerWithCreate(`{"enabled":true,"name_pattern":"^shell[0-9]{1,2}$"}`)))
	if err != nil {
		t.Fatalf("narrowing pattern rejected: %v", err)
	}
	if !c.SessionCreate.AllowsName("shell12") {
		t.Fatal("narrowing pattern refused a name it should admit")
	}
	if c.SessionCreate.AllowsName("work") || c.SessionCreate.AllowsName("shell123") {
		t.Fatal("narrowing pattern admitted a name outside itself")
	}
}

func TestSessionCreateRejectsInvalidBlocks(t *testing.T) {
	for name, body := range map[string]string{
		"unknown_server":       `{"enabled":true,"servers":["nope"]}`,
		"duplicate_server":     `{"enabled":true,"servers":["s","s"]}`,
		"unanchored_pattern":   `{"enabled":true,"name_pattern":"[a-z]+"}`,
		"head_anchor_only":     `{"enabled":true,"name_pattern":"^[a-z]+"}`,
		"uncompilable_pattern": `{"enabled":true,"name_pattern":"^([a-z$"}`,
		"max_sessions_zero_ok": "",
		"max_sessions_high":    `{"enabled":true,"max_sessions":257}`,
		"max_sessions_neg":     `{"enabled":true,"max_sessions":-1}`,
		"relative_start_dir":   `{"enabled":true,"start_directory":"relative/path"}`,
		"unclean_start_dir":    `{"enabled":true,"start_directory":"/tmp/../tmp"}`,
		"columns_without_rows": `{"enabled":true,"columns":200}`,
		"rows_without_columns": `{"enabled":true,"rows":50}`,
		"oversize_geometry":    `{"enabled":true,"columns":1001,"rows":50}`,
		"unknown_field":        `{"enabled":true,"nope":1}`,
	} {
		if body == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			if _, err := LoadBroker(write(t, brokerWithCreate(body))); err == nil {
				t.Fatalf("invalid session_create %s was accepted", body)
			}
		})
	}
}

func TestSessionCreateAcceptsFullBlock(t *testing.T) {
	body := `{"enabled":true,"servers":["s"],"name_pattern":"^[a-z][a-z0-9-]{0,20}$","max_sessions":8,"start_directory":"/opt/example/project","columns":200,"rows":50}`
	c, err := LoadBroker(write(t, brokerWithCreate(body)))
	if err != nil {
		t.Fatalf("full session_create block rejected: %v", err)
	}
	got := c.SessionCreate
	if got.MaxSessions != 8 || got.StartDirectory != "/opt/example/project" || got.Columns != 200 || got.Rows != 50 {
		t.Fatalf("full block not carried through: %+v", got)
	}
}

func TestUnifiedTerminalDevIsExactAndDefaultOff(t *testing.T) {
	base := `{"realm":"r","front_uid":0,"servers":[{"label":"s","socket_path":"/tmp/tmux.sock"}],"session_create":{"enabled":true,"servers":["s"],"name_pattern":"^unified_target$"}`
	loaded, err := LoadBroker(write(t, base+`,"unified_terminal_dev":{"enabled":true,"server":"s","session":"unified_target","observer_session":"anchor","runtime_dir":"/tmp/persea-unified"}}`))
	if err != nil || loaded.UnifiedTerminalDev == nil || !loaded.UnifiedTerminalDev.Enabled {
		t.Fatalf("valid unified development target = %+v, %v", loaded.UnifiedTerminalDev, err)
	}
	if loaded.UnifiedTerminalDev.AdoptionSlots != 0 {
		t.Fatalf("adoption slots default = %d, want 0 (journal default)", loaded.UnifiedTerminalDev.AdoptionSlots)
	}
	sized, err := LoadBroker(write(t, base+`,"unified_terminal_dev":{"enabled":true,"server":"s","session":"unified_target","observer_session":"anchor","runtime_dir":"/tmp/persea-unified","adoption_slots":64}}`))
	if err != nil || sized.UnifiedTerminalDev == nil || sized.UnifiedTerminalDev.AdoptionSlots != 64 {
		t.Fatalf("adoption slots not carried through = %+v, %v", sized.UnifiedTerminalDev, err)
	}
	without, err := LoadBroker(write(t, base+`}`))
	if err != nil || without.UnifiedTerminalDev != nil {
		t.Fatalf("absent unified development target = %+v, %v", without.UnifiedTerminalDev, err)
	}
	for name, block := range map[string]string{
		"foreign_server":     `{"enabled":true,"server":"other","session":"unified_target","observer_session":"anchor","runtime_dir":"/tmp/persea-unified"}`,
		"outside_create":     `{"enabled":true,"server":"s","session":"other","observer_session":"anchor","runtime_dir":"/tmp/persea-unified"}`,
		"same_observer":      `{"enabled":true,"server":"s","session":"unified_target","observer_session":"unified_target","runtime_dir":"/tmp/persea-unified"}`,
		"relative_runtime":   `{"enabled":true,"server":"s","session":"unified_target","observer_session":"anchor","runtime_dir":"relative"}`,
		"disabled_authority": `{"enabled":false,"server":"s"}`,
		"disabled_slots":     `{"enabled":false,"adoption_slots":8}`,
		"negative_slots":     `{"enabled":true,"server":"s","session":"unified_target","observer_session":"anchor","runtime_dir":"/tmp/persea-unified","adoption_slots":-1}`,
		"oversized_slots":    `{"enabled":true,"server":"s","session":"unified_target","observer_session":"anchor","runtime_dir":"/tmp/persea-unified","adoption_slots":4097}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadBroker(write(t, base+`,"unified_terminal_dev":`+block+`}`)); err == nil {
				t.Fatalf("invalid unified block accepted: %s", block)
			}
		})
	}
}

// TestIngressHermeticTLSIsHermeticOnly pins the fail-closed rule: hermetic_tls
// is accepted only on an ingress that Hermetic() already recognises, so no
// production ingress — and no near-miss of one — can ever carry it.
func TestIngressHermeticTLSIsHermeticOnly(t *testing.T) {
	tests := []struct {
		name         string
		effectiveUID uint32
		in           Ingress
		ok           bool
	}{
		{"hermetic loopback", 1000, Ingress{SocketPath: "/tmp/front.sock", PeerUID: 1000, PeerUIDConfigured: true, CanonicalHost: "127.0.0.1:43210", OperatorLogin: "operator@example.com", MaxConnections: 1, HermeticTLS: true}, true},
		{"hermetic localhost", 1000, Ingress{SocketPath: "/tmp/front.sock", PeerUID: 1000, PeerUIDConfigured: true, CanonicalHost: "localhost:43210", OperatorLogin: "operator@example.com", MaxConnections: 1, HermeticTLS: true}, true},
		{"production", 0, Ingress{SocketPath: ProductionSocketPath, PeerUID: 0, PeerUIDConfigured: true, CanonicalHost: "terminal.example.ts.net", OperatorLogin: "operator@example.com", MaxConnections: 1, HermeticTLS: true}, false},
		{"production socket with hermetic authority", 1000, Ingress{SocketPath: ProductionSocketPath, PeerUID: 1000, PeerUIDConfigured: true, CanonicalHost: "127.0.0.1:43210", OperatorLogin: "operator@example.com", MaxConnections: 1, HermeticTLS: true}, false},
		{"root loopback", 0, Ingress{SocketPath: "/tmp/front.sock", PeerUID: 0, PeerUIDConfigured: true, CanonicalHost: "127.0.0.1:43210", OperatorLogin: "operator@example.com", MaxConnections: 1, HermeticTLS: true}, false},
		{"foreign peer uid", 1000, Ingress{SocketPath: "/tmp/front.sock", PeerUID: 1001, PeerUIDConfigured: true, CanonicalHost: "127.0.0.1:43210", OperatorLogin: "operator@example.com", MaxConnections: 1, HermeticTLS: true}, false},
		{"dns authority", 1000, Ingress{SocketPath: "/tmp/front.sock", PeerUID: 1000, PeerUIDConfigured: true, CanonicalHost: "terminal.example.ts.net", OperatorLogin: "operator@example.com", MaxConnections: 1, HermeticTLS: true}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIngressForEUID(tc.in, tc.effectiveUID)
			if (err == nil) != tc.ok {
				t.Fatalf("validateIngressForEUID() error = %v, want success=%v", err, tc.ok)
			}
			if tc.ok && !Hermetic(tc.in, tc.effectiveUID) {
				t.Fatal("accepted hermetic_tls on an ingress Hermetic() does not recognise")
			}
		})
	}
	// The same ingress without the option must still be accepted, so the rule
	// rejects the option and not the shape.
	for _, tc := range tests {
		if tc.ok {
			continue
		}
		plain := tc.in
		plain.HermeticTLS = false
		if validateIngressForEUID(plain, tc.effectiveUID) != nil {
			continue
		}
		err := validateIngressForEUID(tc.in, tc.effectiveUID)
		if err == nil || !strings.Contains(err.Error(), "hermetic_tls") {
			t.Fatalf("%s: otherwise-valid ingress rejected for the wrong reason: %v", tc.name, err)
		}
	}
}

func TestIngressHermeticTLSDefaultsOffAndParses(t *testing.T) {
	body := `{"ingress":{"socket_path":"/run/persea-terminal/front.sock","peer_uid":0,"canonical_host":"terminal.example.ts.net","operator_login":"operator@example.com","max_connections":64},"realms":[{"name":"r","socket":"/tmp/b.sock","broker_uid":0}],"alias_store_path":"/tmp/aliases.json"}`
	if os.Geteuid() == 0 {
		loaded, err := LoadFront(write(t, body))
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Ingress.HermeticTLS {
			t.Fatal("hermetic_tls defaulted on")
		}
		withTLS := strings.Replace(body, `"max_connections":64`, `"max_connections":64,"hermetic_tls":true`, 1)
		if _, err := LoadFront(write(t, withTLS)); err == nil {
			t.Fatal("production config accepted hermetic_tls")
		}
		return
	}
	hermetic := fmt.Sprintf(`{"ingress":{"socket_path":"/tmp/front.sock","peer_uid":%d,"canonical_host":"127.0.0.1:43210","operator_login":"operator@example.com","max_connections":64,"hermetic_tls":true},"realms":[{"name":"r","socket":"/tmp/b.sock","broker_uid":%d}],"alias_store_path":"/tmp/aliases.json"}`, os.Geteuid(), os.Geteuid())
	loaded, err := LoadFront(write(t, hermetic))
	if err != nil || !loaded.Ingress.HermeticTLS {
		t.Fatalf("hermetic config with hermetic_tls: %+v %v", loaded.Ingress, err)
	}
	plain := strings.Replace(hermetic, `,"hermetic_tls":true`, "", 1)
	unset, err := LoadFront(write(t, plain))
	if err != nil || unset.Ingress.HermeticTLS {
		t.Fatalf("omitted hermetic_tls: %+v %v", unset.Ingress, err)
	}
	production := strings.Replace(hermetic, `"canonical_host":"127.0.0.1:43210"`, `"canonical_host":"terminal.example.ts.net"`, 1)
	if _, err := LoadFront(write(t, production)); err == nil {
		t.Fatal("non-loopback authority accepted hermetic_tls")
	}
}
