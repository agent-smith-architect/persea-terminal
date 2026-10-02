package broker

import (
	"strings"
	"testing"
	"time"

	"persea-terminal/internal/config"
)

type shadowTestClock struct {
	wall    int64
	elapsed time.Duration
}

func TestRestoredShadowGraceBounds(t *testing.T) {
	if restoredShadowGrace < 10*time.Second || restoredShadowGrace < 4*shadowBirthTimeout {
		t.Fatalf("restored-copy grace must cover at least ten seconds and four birth deadlines: grace=%v birth=%v", restoredShadowGrace, shadowBirthTimeout)
	}
}

func newShadowTestClock(t *testing.T) *shadowTestClock {
	t.Helper()
	c := &shadowTestClock{wall: time.Now().Unix()}
	restoredShadowObservations.Lock()
	previous := restoredShadowObservations.now
	restoredShadowObservations.now = func() (int64, time.Duration) { return c.wall, c.elapsed }
	restoredShadowObservations.Unlock()
	t.Cleanup(func() {
		restoredShadowObservations.Lock()
		restoredShadowObservations.now = previous
		restoredShadowObservations.Unlock()
	})
	return c
}

func (c *shadowTestClock) advance(wall, elapsed time.Duration) {
	restoredShadowObservations.Lock()
	defer restoredShadowObservations.Unlock()
	c.wall += int64(wall / time.Second)
	c.elapsed += elapsed
}

func ageRestoredShadow(t *testing.T, server config.TmuxServer, id string) {
	t.Helper()
	c := newShadowTestClock(t)
	inc, err := readIncarnation(server)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := inspectRestoredShadow(server, id, inc); ok {
		t.Fatal("new observation was already eligible for removal")
	}
	c.advance(restoredShadowGrace+time.Second, restoredShadowGrace)
}

func TestRestoredShadowAgeAndClockSteps(t *testing.T) {
	for _, step := range []string{"normal", "forward", "backward"} {
		t.Run(step, func(t *testing.T) {
			d := newDisposable(t)
			s := &Server{config: bootCreationConfig(t, d.tmux)}
			if _, err := incarnation(d.tmux); err != nil {
				t.Fatal(err)
			}
			name := attachmentShadowPrefix + strings.Repeat("9", 32)
			d.run("new-session", "-d", "-s", name, "sleep 600")
			id := d.run("display-message", "-p", "-t", name, "#{session_id}")
			c := newShadowTestClock(t)
			check := func(present bool) {
				t.Helper()
				if got := s.inventoryServer(d.tmux, 10); got.Status != "ok" {
					t.Fatalf("inventory: %+v", got)
				}
				if got, err := tmuxSessionPresent(d.tmux, id); err != nil || got != present {
					t.Fatalf("candidate age protection: present=%t want=%t err=%v", got, present, err)
				}
			}
			check(true)
			switch step {
			case "forward":
				c.advance(time.Hour, restoredShadowGrace-time.Nanosecond)
				check(true)
				c.advance(0, time.Nanosecond)
			case "backward":
				c.advance(-time.Hour, restoredShadowGrace)
				check(true)
				c.advance(time.Hour+restoredShadowGrace, 0)
			default:
				c.advance(restoredShadowGrace-time.Second, restoredShadowGrace-time.Second)
				check(true)
				c.advance(time.Second, time.Second)
			}
			check(false)
		})
	}
}
