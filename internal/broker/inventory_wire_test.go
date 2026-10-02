package broker

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func inventoryWireResult(t *testing.T, s *Server) proto.Control {
	t.Helper()
	var wire bytes.Buffer
	s.inventory(&lockedWriter{w: &wire})
	frame, err := proto.ReadFrame(&wire)
	if err != nil {
		t.Fatalf("inventory did not emit a valid control frame: %v", err)
	}
	if frame.Type != proto.FrameControl || len(frame.Payload) > proto.MaxControl || wire.Len() != 0 {
		t.Fatalf("invalid inventory wire reply: type=%v bytes=%d trailing=%d", frame.Type, len(frame.Payload), wire.Len())
	}
	control, err := proto.DecodeControl(frame.Payload)
	if err != nil || control.Type != "inventory_ok" {
		t.Fatalf("invalid inventory control: %+v %v", control, err)
	}
	t.Logf("inventory control payload: %d bytes", len(frame.Payload))
	return control
}

func TestInventoryOmitsInternalRowsOnWire(t *testing.T) {
	d := newDisposable(t)
	d.run("rename-session", "-t", "alpha", "zz-live")
	if _, err := incarnation(d.tmux); err != nil {
		t.Fatal(err)
	}
	d.run("set-option", "-g", "@persea_client_id", "protected-internal")
	for i := 0; i < 200; i++ {
		d.run("new-session", "-d", "-t", "zz-live", "-s", fmt.Sprintf("%s%032x", attachmentShadowPrefix, i))
	}
	got := inventoryWireResult(t, &Server{config: bootCreationConfig(t, d.tmux)})
	if len(got.Servers) != 1 || got.Servers[0].Status != "ok" || got.Servers[0].Error != "" || len(got.Servers[0].Sessions) != 1 || got.Servers[0].Sessions[0].Name != "zz-live" {
		t.Fatalf("internal rows leaked or displaced operator: %+v", got)
	}
	if ids := strings.Fields(d.run("list-sessions", "-F", "#{session_id}")); len(ids) != 201 {
		t.Fatalf("protected rows were removed: %d", len(ids))
	}
}

func TestInventoryWireBudgetTruncatesAffectedServers(t *testing.T) {
	ordinary, first, second := newDisposable(t), newDisposable(t), newDisposable(t)
	ordinary.tmux.Label, first.tmux.Label, second.tmux.Label = "ordinary", "first", "second"
	for _, d := range []*disposable{first, second} {
		for i := 0; i < 60; i++ {
			d.run("new-session", "-d", "-t", "alpha", "-s", fmt.Sprintf("row-%02d-%s", i, strings.Repeat("<&>", 40)))
		}
	}
	s := &Server{config: bootCreationConfig(t, ordinary.tmux)}
	s.config.Servers = []config.TmuxServer{ordinary.tmux, first.tmux, second.tmux}
	got := inventoryWireResult(t, s)
	if len(got.Servers) != 3 {
		t.Fatal(got)
	}
	if r := got.Servers[0]; r.Error != "" || r.Status != "ok" || len(r.Sessions) != 1 {
		t.Fatalf("unaffected server was truncated: %+v", r)
	}
	for _, r := range got.Servers[1:] {
		if r.Status != "ok" || r.Error != "inventory session limit reached" || len(r.Sessions) >= 61 {
			t.Fatalf("lost rows were not marked incomplete: label=%q status=%q error=%q rows=%d", r.Label, r.Status, r.Error, len(r.Sessions))
		}
	}
}

func TestInventoryWireBudgetIncludesMetadata(t *testing.T) {
	servers := make([]proto.ServerInventory, 64)
	for i := range servers {
		servers[i] = proto.ServerInventory{Label: fmt.Sprintf("server-%02d-%s", i, strings.Repeat("s", 54)), Status: "error", Error: strings.Repeat("<&>", 1000)}
	}
	payload, err := inventoryPayload(servers)
	if err != nil || len(payload) > proto.MaxControl {
		t.Fatalf("metadata exceeded control frame: bytes=%d err=%v", len(payload), err)
	}
	got, err := proto.DecodeControl(payload)
	if err != nil || len(got.Servers) != 64 {
		t.Fatalf("metadata lost servers: %v", err)
	}
	for i, r := range got.Servers {
		if r.Label != fmt.Sprintf("server-%02d-%s", i, strings.Repeat("s", 54)) || r.Status != "error" || r.Error == "" {
			t.Fatalf("metadata lost availability: %+v", r)
		}
	}
}

type failedInventoryWriter struct{}

func (failedInventoryWriter) Write([]byte) (int, error) {
	return 0, errors.New("inventory transport sentinel")
}

func TestInventoryLogsFailedWrite(t *testing.T) {
	logs := captureBrokerLogs(t)
	(&Server{}).inventory(&lockedWriter{w: failedInventoryWriter{}})
	if !strings.Contains(logs.String(), "event=inventory_write_failed") || !strings.Contains(logs.String(), "inventory transport sentinel") {
		t.Fatalf("failed inventory write was silent: %q", logs.String())
	}
}
