package terminal

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestBoundedOffscreenRowsRetainsNewestCompleteSuffix(t *testing.T) {
	physical := make([]string, 300)
	for i := range physical {
		physical[i] = fmt.Sprintf("%03d", i) + strings.Repeat("x", HistoryRowByteCap-3)
	}
	rows, truncated, err := boundedOffscreenRows([]byte(strings.Join(physical, "\n")+"\n"), 500)
	if err != nil || !truncated || len(rows) != 255 || rows[0] != physical[45] || rows[len(rows)-1] != physical[len(physical)-1] {
		t.Fatalf("rows=%d truncated=%v err=%v", len(rows), truncated, err)
	}
	if _, _, err := boundedOffscreenRows([]byte("ok\n\xff\n"), 500); err == nil {
		t.Fatal("invalid UTF-8 must fail closed")
	}
	oversizedWitness := append(bytes.Repeat([]byte{'x'}, HistoryRowByteCap+1), []byte("\nnewest\n")...)
	if _, _, err := boundedOffscreenRows(oversizedWitness, 0); err == nil {
		t.Fatal("an oversized row must fail even when line truncation would discard it")
	}
}

func TestHistoryChoiceMatrixAndZeroDepth(t *testing.T) {
	for _, limit := range []int{0, 500, 1_000, 2_000, 5_000, 7_500, 10_000} {
		if !ValidHistoryRows(limit) {
			t.Fatalf("valid history choice rejected: %d", limit)
		}
	}
	for _, limit := range []int{-1, 1, 499, 501, 10_001} {
		if ValidHistoryRows(limit) {
			t.Fatalf("invalid history choice accepted: %d", limit)
		}
	}
	rows, truncated, err := boundedOffscreenRows([]byte("older\n"), 0)
	if err != nil || len(rows) != 0 || !truncated {
		t.Fatalf("zero history rows=%q truncated=%v err=%v", rows, truncated, err)
	}
}

func TestCutBoundsRejectWithoutMutation(t *testing.T) {
	payload := append([]byte(markerNamespace), bytes.Repeat([]byte{'A'}, markerNonceSize)...)
	c, err := newCut(1, CutInitial, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.acceptMarker(payload); err != nil {
		t.Fatal(err)
	}
	c.held = bytes.Repeat([]byte{'x'}, HeldByteCap)
	if _, err := c.feedOrdinary([]byte("y")); err != ErrSaturated || len(c.held) != HeldByteCap {
		t.Fatalf("err=%v held=%d", err, len(c.held))
	}
}

func TestIngressFramerArbitraryChunksAndReservedFailures(t *testing.T) {
	nonce := bytes.Repeat([]byte{'A'}, markerNonceSize)
	payload := append([]byte(markerNamespace), nonce...)
	marker := append(append([]byte("\x1b]52;;"), payload...), '\a')
	stream := append(append([]byte("pre"), marker...), []byte("post")...)
	for split := 0; split <= len(stream); split++ {
		var f ingressFramer
		var ordinary []byte
		var markers [][]byte
		for _, chunk := range [][]byte{stream[:split], stream[split:]} {
			events, err := f.feed(chunk)
			if err != nil {
				t.Fatalf("split=%d: %v", split, err)
			}
			for _, event := range events {
				if event.kind == ingressOrdinary {
					ordinary = append(ordinary, event.payload...)
				} else {
					markers = append(markers, event.payload)
				}
			}
		}
		if string(ordinary) != "prepost" || len(markers) != 1 || !bytes.Equal(markers[0], payload) {
			t.Fatalf("split=%d ordinary=%q markers=%q", split, ordinary, markers)
		}
	}
	var malformed ingressFramer
	bad := append([]byte(markerWirePrefix), bytes.Repeat([]byte{'A'}, markerNonceSize)...)
	bad = append(bad, 'x')
	if _, err := malformed.feed(bad); err == nil {
		t.Fatal("over-limit reserved marker must fail closed")
	}
}
