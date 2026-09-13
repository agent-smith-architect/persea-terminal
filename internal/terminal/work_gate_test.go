package terminal

import "testing"

func TestWorkGateSealsWaitsAndRejectsDoubleRelease(t *testing.T) {
	g := newWorkGate()
	a, _ := g.claim("a")
	b, _ := g.claim("b")
	q := g.seal()
	a.release()
	select {
	case <-q:
		t.Fatal("quiesced before final claim")
	default:
	}
	b.release()
	select {
	case <-q:
	default:
		t.Fatal("did not quiesce")
	}
	if _, err := g.claim("late"); err == nil {
		t.Fatal("sealed gate admitted work")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("double release did not panic")
		}
	}()
	b.release()
}
