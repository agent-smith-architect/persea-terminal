package broker

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"testing"

	"persea-terminal/internal/config"
	"persea-terminal/internal/proto"
)

func TestGuardedSnapshotArgvUsesEscapesDepthAndExactID(t *testing.T) {
	server := config.TmuxServer{Label: "main", SocketName: "disposable"}
	authority := proto.Authority{BootID: "boot", ServerPID: 123, ServerStart: 456, SessionID: "$7"}
	argv, err := guardedSnapshotArgv(server, authority, "%9", 5000, "PERSEA_nonce_META", "PERSEA_nonce_STALE")
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 11 || argv[1] != "-u" || argv[2] != "-N" || argv[3] != "-L" || argv[4] != "disposable" || argv[7] != "%9" || !strings.Contains(argv[9], "capture-pane -p -e -S -5000 -t %9") || strings.Contains(argv[9], "=$7:") {
		t.Fatalf("unexpected guarded snapshot argv: %q", argv)
	}
	for _, depth := range []int{0, SnapshotLineLimit + 1} {
		if _, err := guardedSnapshotArgv(server, authority, "%9", depth, "PERSEA_nonce_META", "PERSEA_nonce_STALE"); err == nil {
			t.Fatalf("invalid depth %d accepted", depth)
		}
	}
	for _, pane := range []string{"", "9", "%", "%9x", "%999999999999999999999999999999999"} {
		if _, err := guardedSnapshotArgv(server, authority, pane, 5000, "PERSEA_nonce_META", "PERSEA_nonce_STALE"); err == nil {
			t.Fatalf("invalid pane id %q accepted", pane)
		}
	}
}

func TestIncarnationConditionServerStartMismatchIsTerminal(t *testing.T) {
	authority := proto.Authority{
		BootID:      "boot",
		ServerPID:   123,
		ServerStart: 456,
		SessionID:   "$7",
	}
	condition := incarnationCondition(authority)
	const terminalStartGuard = `[ "$#" -ge 20 ] && [ "${20}" = '456' ] || exit 1`
	if !strings.Contains(condition, terminalStartGuard) {
		t.Fatalf("server-start mismatch is not terminal: %s", condition)
	}
}

func TestSnapshotChunksReassembleAndRespectFrameCap(t *testing.T) {
	payload := []byte(strings.Repeat("line-0123456789\n", 9000))
	var wire bytes.Buffer
	writer := &lockedWriter{w: &wire}
	if err := writeDataChunks(writer, payload); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for wire.Len() > 0 {
		frame, err := proto.ReadFrame(&wire)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type != proto.FrameData || len(frame.Payload) > proto.MaxData {
			t.Fatalf("invalid data chunk: type=%x bytes=%d", frame.Type, len(frame.Payload))
		}
		got = append(got, frame.Payload...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("snapshot chunks did not reassemble byte-for-byte")
	}
}

func TestSnapshotTailBufferAndBoundsPreserveTailLineBoundary(t *testing.T) {
	var tail tailLimitedBuffer
	tail.max = 64
	input := []byte(strings.Repeat("prefix-line\n", 20) + "FINAL\n")
	if n, err := tail.Write(input); err != nil || n != len(input) || !tail.truncated {
		t.Fatalf("tail write failed: n=%d err=%v truncated=%v", n, err, tail.truncated)
	}
	out, truncated := boundHistory([]byte(tail.String()), SnapshotLineLimit, 40)
	if !truncated || !bytes.HasSuffix(out, []byte("FINAL\n")) || (len(out) > 0 && out[0] == 'x') {
		t.Fatalf("snapshot tail bound failed: %q truncated=%v", out, truncated)
	}
}

func TestControlInputIsByteTransparent(t *testing.T) {
	payload := []byte{'x', '\r', 0x03, 0x1b, '[', 'A', '\t', 0x02, 'd'}
	var out bytes.Buffer
	if err := writeInput("control", &out, payload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("control input changed: got=%x want=%x", out.Bytes(), payload)
	}
}

func TestEffectiveSnapshotDepthDefaultsAndClamps(t *testing.T) {
	if SnapshotDefaultDepth == SnapshotLineLimit {
		t.Fatal("snapshot default regressed to the line cap")
	}
	for _, tc := range []struct{ in, want int }{{0, SnapshotDefaultDepth}, {-1, SnapshotDefaultDepth}, {1, 1}, {4999, 4999}, {5000, SnapshotLineLimit}, {5001, SnapshotLineLimit}} {
		if got := effectiveSnapshotDepth(tc.in); got != tc.want {
			t.Fatalf("depth %d => %d, want %d", tc.in, got, tc.want)
		}
	}
}

// exec copies a non-file Stdout with io.Copy, which prefers a ReaderFrom over
// Write. The caps must hold on that path, not only for direct Write calls.
func TestTmuxOutputBuffersKeepTheirCapsBehindExec(t *testing.T) {
	const capBytes = 1 << 20
	// Patterned output larger than the cap, arriving in many pipe writes, so
	// the kept bytes show which end of the output was kept.
	full, err := exec.Command("seq", "1", "400000").Output()
	if err != nil || len(full) <= 2*capBytes {
		t.Fatalf("seq produced %d bytes: %v", len(full), err)
	}
	run := func(stdout io.Writer) {
		t.Helper()
		cmd := exec.Command("seq", "1", "400000")
		cmd.Stdout = stdout
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
	}
	limited := limitedBuffer{max: capBytes}
	run(&limited)
	if !limited.overflow || limited.String() != string(full[:capBytes]) {
		t.Fatalf("limited buffer kept %d of %d bytes (overflow=%v), want the first %d", len(limited.String()), len(full), limited.overflow, capBytes)
	}
	tail := tailLimitedBuffer{max: capBytes}
	run(&tail)
	if !tail.truncated || tail.String() != string(full[len(full)-capBytes:]) {
		t.Fatalf("tail buffer kept %d of %d bytes (truncated=%v), want the last %d", len(tail.String()), len(full), tail.truncated, capBytes)
	}
	// Exact boundaries: output that fills the cap is whole; one byte more is not.
	for _, writes := range [][]int{{capBytes}, {capBytes - 1, 1}, {capBytes, 1}, {capBytes + 1}, {3, capBytes - 3, 2}, {capBytes - 2, 5, capBytes}} {
		limited, tail := limitedBuffer{max: capBytes}, tailLimitedBuffer{max: capBytes}
		total := 0
		for _, n := range writes {
			_, _ = limited.Write(full[total : total+n])
			_, _ = tail.Write(full[total : total+n])
			total += n
		}
		over := total > capBytes
		if limited.overflow != over || limited.String() != string(full[:min(total, capBytes)]) {
			t.Fatalf("writes %v: limited kept %d bytes overflow=%v", writes, len(limited.String()), limited.overflow)
		}
		if tail.truncated != over || tail.String() != string(full[max(0, total-capBytes):total]) {
			t.Fatalf("writes %v: tail kept %d bytes truncated=%v", writes, len(tail.String()), tail.truncated)
		}
	}
}
