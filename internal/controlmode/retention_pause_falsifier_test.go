package controlmode

import (
	"bytes"
	"testing"
)

func TestRetentionPauseStartAndEndRemainTypedBoundaries(t *testing.T) {
	decoder := NewDecoder()
	events, err := decoder.Feed([]byte("%pause %0\n%continue %0\n"))
	if err != nil {
		t.Fatalf("decode pause boundaries: %v", err)
	}
	if err := decoder.Close(); err != nil {
		t.Fatalf("close decoder: %v", err)
	}
	if len(events) != 2 || events[0].Kind != EventPause || events[0].Name != "pause" ||
		events[1].Kind != EventContinue || events[1].Name != "continue" {
		t.Fatalf("pause boundary typing = %#v", events)
	}
	for _, event := range events {
		if len(event.Data) != 0 || bytes.Contains([]byte(event.Args), []byte("payload")) {
			t.Fatalf("pause boundary published data: %#v", event)
		}
	}
}
