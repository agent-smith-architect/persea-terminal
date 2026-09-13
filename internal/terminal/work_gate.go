package terminal

import (
	"fmt"
	"sync"
)

type workGate struct {
	mu        sync.Mutex
	sealed    bool
	active    map[*workClaim]struct{}
	quiescent chan struct{}
	closed    bool
}

type workClaim struct {
	gate     *workGate
	phase    string
	released bool
}

func newWorkGate() *workGate {
	return &workGate{active: make(map[*workClaim]struct{}), quiescent: make(chan struct{})}
}

func (g *workGate) claim(phase string) (*workClaim, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return nil, fmt.Errorf("%w: work gate sealed", ErrClosed)
	}
	c := &workClaim{gate: g, phase: phase}
	g.active[c] = struct{}{}
	return c, nil
}

func (c *workClaim) release() {
	if c == nil || c.gate == nil {
		panic("terminal work claim has no owner")
	}
	g := c.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if c.released {
		panic("terminal work claim released twice: " + c.phase)
	}
	if _, ok := g.active[c]; !ok {
		panic("terminal work claim is not active: " + c.phase)
	}
	delete(g.active, c)
	c.released = true
	g.closeIfReady()
}

func (g *workGate) seal() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sealed = true
	g.closeIfReady()
	return g.quiescent
}

func (g *workGate) closeIfReady() {
	if g.sealed && len(g.active) == 0 && !g.closed {
		close(g.quiescent)
		g.closed = true
	}
}
