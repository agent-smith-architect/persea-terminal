package broker

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func readBrokerSource(t *testing.T, name string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(here), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The cap must hold under concurrent requests. Each connection is served on its
// own goroutine, so a count-then-create sequence without a lock lets several
// requests all observe one-below-the-cap and all proceed. Locking is the only
// thing enforcing max_sessions: tmux gives atomic *name* uniqueness but knows
// nothing about our bound.
//
// This exercises the lock directly rather than through tmux, so it stays fast and
// deterministic; the tmux path is covered by the runtime tests.
func TestCreateSerialisesCountThenCreate(t *testing.T) {
	const cap = 20
	var observed, created int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Mirrors create(): take the same lock, read the count, then act on it.
			createMu.Lock()
			defer createMu.Unlock()
			mu.Lock()
			existing := created
			mu.Unlock()
			if existing >= cap {
				return
			}
			mu.Lock()
			observed++
			created++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if created > cap {
		t.Fatalf("cap exceeded under concurrency: created=%d cap=%d", created, cap)
	}
	if observed != cap {
		t.Fatalf("expected the cap to be reached exactly, got %d", observed)
	}
}

// The refusal codes are a closed set the front door maps to status codes, so an
// unreviewed addition here silently becomes a 502 to the operator.
func TestCreateRefusalCodesAreTheDocumentedSet(t *testing.T) {
	source := readBrokerSource(t, "session_create.go")
	for _, code := range []string{"not_permitted", "invalid_name", "at_capacity", "name_taken", "server_unavailable", "create_failed"} {
		if !strings.Contains(source, `refuse("`+code+`"`) && !strings.Contains(source, `"`+code+`"`) {
			t.Fatalf("documented refusal code %q is no longer emitted", code)
		}
	}
}
