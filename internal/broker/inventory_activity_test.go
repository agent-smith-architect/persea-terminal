package broker

import (
	"testing"
	"time"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func TestInventoryOutputActivityTracksOutputWithoutClientInteraction(t *testing.T) {
	d := newDisposable(t)
	d.run("new-session", "-d", "-s", "background-output", "-x", "80", "-y", "24", "sleep 2; printf 'background output\\n'; sleep 600")
	broker := &Server{config: config.Broker{Realm: "local"}}
	read := func() proto.Session {
		t.Helper()
		inventory := broker.inventoryServer(d.tmux, 10)
		if inventory.Status != "ok" || inventory.Error != "" {
			t.Fatalf("inventory failed: %s %s", inventory.Status, inventory.Error)
		}
		for _, session := range inventory.Sessions {
			if session.Name == "background-output" {
				return session
			}
		}
		t.Fatal("private output session missing")
		return proto.Session{}
	}
	before := read()
	if before.OutputActivity <= 0 {
		t.Fatal("window creation timestamp missing")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		after := read()
		if after.OutputActivity > before.OutputActivity {
			if after.Activity != before.Activity {
				t.Fatal("background output changed the separate client-interaction timestamp")
			}
			if after.Width != before.Width || after.Height != before.Height || after.Attached != before.Attached {
				t.Fatal("inventory changed geometry or clients")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("inventory output timestamp did not advance with background output")
}
