package broker

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUnifiedAdoptionSelectedHistoryRows(t *testing.T) {
	for _, requested := range []int{-1, 0, 500, 5000, 10000} {
		t.Run(strconv.Itoa(requested), func(t *testing.T) {
			depth := requested
			var history []int
			if requested == -1 {
				depth = 10000 // The shared import stays independent of browser retention.
			} else {
				history = []int{requested}
			}
			fixture := newAdoptionFixture(t, 0)
			fixture.disposable.run("set-option", "-g", "history-limit", "12000")
			sessionID := fixture.startPaneCommand(t, "history",
				`i=1; while [ "$i" -le 6000 ]; do printf 'ROW-%05d\n' "$i"; i=$((i+1)); done; exec sleep 600`)
			pollUntil(t, 5*time.Second, "numbered history", func() bool { return strings.Contains(fixture.capture(t, "history"), "ROW-06000") })
			witness := func() string {
				return fixture.disposable.run("display-message", "-p", "-t", "history:", "#{window_width}x#{window_height}|#{history_limit}|#{history_size}|#{session_windows}|#{window_panes}")
			}
			before := witness()
			adopted, err := fixture.effects.AdoptSession(context.Background(), sessionID, history...)
			if err != nil {
				t.Fatal(err)
			}
			var data []byte
			pollUntil(t, 5*time.Second, "recorded history", func() bool {
				data = fixture.journalBytes(t, adopted.Key)
				return strings.Contains(string(data), "ROW-06000")
			})
			rows, _ := adoptionBootstrapParts(t, data)
			text := strings.Join(rows, "\n")
			if len(rows) > depth+24 {
				t.Fatalf("depth %d retained %d rows", depth, len(rows))
			}
			for marker, want := range map[string]bool{"ROW-00001": depth == 10000, "ROW-02000": depth >= 5000, "ROW-05900": depth >= 500, "ROW-06000": true} {
				if strings.Contains(text, marker) != want {
					t.Fatalf("depth %d marker %s present=%t want=%t", depth, marker, strings.Contains(text, marker), want)
				}
			}
			if after := witness(); after != before {
				t.Fatalf("import changed tmux state: before %s after %s", before, after)
			}
		})
	}
}

func TestUnifiedAdoptionHistoryBoundsBeforeEffects(t *testing.T) {
	fixture := newAdoptionFixture(t, 0)
	before := fixture.journalFileCount(t)
	for _, depth := range []int{-1, 10001} {
		if _, err := fixture.effects.AdoptSession(context.Background(), "$999", depth); !errors.Is(err, ErrUnifiedAdoptHistory) {
			t.Fatalf("depth %d: %v", depth, err)
		}
	}
	if after := fixture.journalFileCount(t); after != before {
		t.Fatalf("invalid import created journal artifacts")
	}
}
