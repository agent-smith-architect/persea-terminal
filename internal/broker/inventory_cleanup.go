package broker

import (
	"sort"
	"strconv"
	"strings"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

// In 100 samples, a live-owner read took at most 9.01 ms and a read with
// guarded removal took at most 31.39 ms. Round the latter to 32 ms and allow
// 2x headroom: floor(300 ms / 64 ms) = four candidates across the whole reply.
// The 300 ms allowance leaves 85% of the 2 s deadline for normal inventory.
const inventoryOwnerReadBudget = 4

// Collect configured lists with a fixed process fan-out. Projection and shadow
// probes remain ordered, so the reply's session limit and probe budget are shared.
const inventoryServerReadConcurrency = 8

type inventoryServerSnapshot struct {
	result      proto.ServerInventory
	inc         proto.Authority
	lines       []string
	ownerBudget int
}

type shadowInventoryRow struct {
	id     uint64
	fields []string
}

// A fixed frontier lets a round finish even while newer shadows keep arriving.
// This adds only scalar state to the retained admission identity, not a row cache.
type shadowInventoryCursor struct {
	next    uint64
	through uint64
	active  bool
}

func (cursor *shadowInventoryCursor) take(rows []shadowInventoryRow, budget int) []shadowInventoryRow {
	if len(rows) == 0 || budget <= 0 {
		return nil
	}
	if !cursor.active {
		cursor.next, cursor.through, cursor.active = 0, rows[len(rows)-1].id, true
	}
	start := sort.Search(len(rows), func(i int) bool { return rows[i].id >= cursor.next })
	if start == len(rows) || rows[start].id > cursor.through {
		// The rest of the old round disappeared. Start a fresh finite round.
		cursor.next, cursor.through = 0, rows[len(rows)-1].id
		start = 0
	}
	end := start
	for end < len(rows) && end-start < budget && rows[end].id <= cursor.through {
		end++
	}
	last := rows[end-1].id
	if last == cursor.through {
		cursor.active = false
	} else {
		cursor.next = last + 1
	}
	return rows[start:end]
}

func inspectInventoryShadows(server config.TmuxServer, inc proto.Authority, lines []string, budget int) error {
	if budget <= 0 {
		return nil
	}
	var rows []shadowInventoryRow
	for _, line := range lines {
		p := strings.Split(line, "\t")
		// Reject protected shapes before scheduling a read. The existing
		// candidate predicate still validates every field before inspecting ownership.
		if len(p) != 14 || !validSessionID(p[0]) || !attachmentShadowName(p[1]) || p[4] != "0" || p[9] != "1" || p[11] != "0" || p[12] != "0" || p[13] != "0" {
			continue
		}
		id, err := strconv.ParseUint(p[0][1:], 10, 64)
		if err != nil {
			continue
		}
		rows = append(rows, shadowInventoryRow{id: id, fields: []string{p[0], p[1], p[4], p[9], p[6], p[11], p[12], p[13]}})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	identity := tmuxProcessIdentity{inc.UID, inc.BootID, inc.ServerPID, inc.ServerStart}
	value, _ := shadowAdmission.Load(identity)
	admission := value.(*tmuxShadowAdmission)
	admission.Lock()
	selected := admission.inventoryCursor.take(rows, budget)
	admission.Unlock()
	// No owner read or tmux command holds the cursor lock. Concurrent replies
	// advance the same cursor without serializing their foreground I/O.
	for _, row := range selected {
		shadow, ok, err := restoredShadowCandidate(server, row.fields, inc)
		if err != nil {
			return err
		}
		if ok {
			if _, err := reapOrphanShadow(server, shadow); err != nil {
				return err
			}
		}
	}
	return nil
}
