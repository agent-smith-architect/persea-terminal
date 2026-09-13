package broker

import (
	"strings"
	"testing"
)

func TestTmuxOutputPreservesProtocolDelimitersInCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "C")
	d := newDisposable(t)
	out, err := tmuxOutput(d.tmux, "list-sessions", "-F", "#{session_id}\t#{session_name}\t#{window_width}")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSuffix(out, "\n"), "\t")
	if len(fields) != 3 || !validSessionID(fields[0]) || fields[1] != "alpha" || fields[2] != "80" {
		t.Fatalf("C locale corrupted structured tmux output: %q", out)
	}
}
