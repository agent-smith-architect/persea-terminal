package frontdoor

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"persea-terminal/internal/proto"
)

func TestReadSnapshotReassemblesChunksAndCaps(t *testing.T) {
	historyRows := 5000
	meta := proto.Control{Type: "snapshot_ok", Width: 80, Height: 24, Pane: "%1", FrozenAt: time.Now().UnixMilli(), Depth: 5000, HistoryRows: &historyRows}
	var wire bytes.Buffer
	if err := writeControl(&wire, meta); err != nil {
		t.Fatal(err)
	}
	payload := []byte(strings.Repeat("line\n", 6000))
	for len(payload) > 0 {
		n := len(payload)
		if n > proto.MaxData {
			n = proto.MaxData
		}
		if err := proto.WriteFrame(&wire, proto.FrameData, payload[:n]); err != nil {
			t.Fatal(err)
		}
		payload = payload[n:]
	}
	if err := writeControl(&wire, proto.Control{Type: "snapshot_end"}); err != nil {
		t.Fatal(err)
	}
	gotMeta, got, bounded, err := readSnapshot(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if gotMeta.Pane != "%1" || gotMeta.HistoryRows == nil || *gotMeta.HistoryRows != 5000 || !bounded || bytes.Count(got, []byte{'\n'}) > SnapshotLineLimit+gotMeta.Height || len(got) > SnapshotByteLimit {
		t.Fatalf("front snapshot bounds failed: meta=%+v bytes=%d lines=%d bounded=%v", gotMeta, len(got), bytes.Count(got, []byte{'\n'}), bounded)
	}
}

func TestReadSnapshotRejectsMissingTerminatorAndBadMetadata(t *testing.T) {
	for _, meta := range []proto.Control{
		{Type: "snapshot_ok", Width: 0, Height: 24, Pane: "%1", FrozenAt: 1, Depth: 1, HistoryRows: new(int)},
		{Type: "snapshot_ok", Width: 80, Height: 24, Pane: "%1", FrozenAt: 1, Depth: SnapshotLineLimit + 1, HistoryRows: new(int)},
		{Type: "snapshot_ok", Width: 80, Height: 24, Pane: "%1", FrozenAt: 1, Depth: 1},
	} {
		var wire bytes.Buffer
		_ = writeControl(&wire, meta)
		_ = writeControl(&wire, proto.Control{Type: "snapshot_end"})
		if _, _, _, err := readSnapshot(&wire); err == nil {
			t.Fatalf("bad metadata accepted: %+v", meta)
		}
	}
	var missing bytes.Buffer
	_ = writeControl(&missing, proto.Control{Type: "snapshot_ok", Width: 80, Height: 24, Pane: "%1", FrozenAt: 1, Depth: 1, HistoryRows: new(int)})
	if _, _, _, err := readSnapshot(&missing); err == nil {
		t.Fatal("snapshot without terminator accepted")
	}
}

func TestLeaseAcquireConflictRenewExpiryAndRelease(t *testing.T) {
	store := newLeaseStore(time.Minute)
	now := time.Unix(1000, 0)
	store.now = func() time.Time { return now }
	a := auth(1, "$9")
	since, ok := store.acquire(a, "holder-a")
	if !ok || since != now.UnixMilli() {
		t.Fatal("first lease acquire failed")
	}
	if heldSince, ok := store.acquire(a, "holder-b"); ok || heldSince != since {
		t.Fatal("second lease did not conflict cleanly")
	}
	if !store.renew(a, "holder-a") {
		t.Fatal("holder renew failed")
	}
	store.release(a, "holder-a")
	if _, ok := store.acquire(a, "holder-b"); !ok {
		t.Fatal("release did not free lease")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := store.acquire(a, "holder-c"); !ok {
		t.Fatal("expired lease was not acquirable")
	}
}
