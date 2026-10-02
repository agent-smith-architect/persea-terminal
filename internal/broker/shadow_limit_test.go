package broker

import (
	"context"
	"fmt"
	"os"
	"testing"

	"persea-terminal/internal/config"
)

func TestInternalShadowsDoNotConsumeInventoryLimit(t *testing.T) {
	for _, liveOwner := range []bool{false, true} {
		t.Run(fmt.Sprintf("live-owner=%t", liveOwner), func(t *testing.T) {
			d := newDisposable(t)
			d.run("rename-session", "-t", "alpha", "zz-live")
			inc, err := readIncarnation(d.tmux)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := (procProbe{}).Witness(context.Background(), os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < InventorySessionLimit; i++ {
				args := []string{"new-session", "-d", "-s", fmt.Sprintf("%s%032x", attachmentShadowPrefix, i)}
				if liveOwner {
					args = append(args, "-e", fmt.Sprintf("%s=%s:%d:%d", attachmentOwnerEnvironment, inc.BootID, owner.PID, owner.StartTime))
				}
				d.run(append(args, "sleep 600")...)
			}
			s := &Server{config: bootCreationConfig(t, d.tmux)}
			got := s.inventoryServer(d.tmux, InventorySessionLimit)
			hidden, found := 0, false
			for _, row := range got.Sessions {
				if attachmentShadowName(row.Name) {
					hidden++
				}
				found = found || row.Name == "zz-live"
			}
			wantHidden := 0
			if liveOwner {
				wantHidden = InventorySessionLimit
			}
			if got.Status != "ok" || got.Error != "" || !found || hidden != wantHidden {
				t.Fatalf("internal rows consumed visible budget: status=%q error=%q hidden=%d found=%t", got.Status, got.Error, hidden, found)
			}
			zero := s.inventoryServer(d.tmux, 0)
			if zero.Status != "ok" || zero.Error != "inventory session limit reached" || len(zero.Sessions) != wantHidden {
				t.Fatalf("zero visible budget changed internal rows: %+v", zero)
			}
			other := newDisposable(t)
			d.tmux.Label, other.tmux.Label = "first", "second"
			s.config.Servers = []config.TmuxServer{d.tmux, other.tmux}
			all := controlResult(t, s.inventory)
			if len(all.Servers) != 2 || all.Servers[1].Status != "ok" || all.Servers[1].Error != "" || len(all.Servers[1].Sessions) != 1 || all.Servers[1].Sessions[0].Name != "alpha" {
				t.Fatalf("internal rows consumed another server's visible budget: %+v", all)
			}
		})
	}
}
