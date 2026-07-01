package daemon

import (
	"sync"

	"github.com/bruno00o/claude-dial/bridge/internal/protocol"
)

// router matches device decisions to the HTTP requests blocked on them, keyed
// by session id.
type router struct {
	mu      sync.Mutex
	waiters map[string]chan protocol.Decision
}

func newRouter() *router {
	return &router{waiters: make(map[string]chan protocol.Decision)}
}

// register returns a channel that will receive the decision for sid, plus a
// cancel func to release it.
func (r *router) register(sid string) (<-chan protocol.Decision, func()) {
	ch := make(chan protocol.Decision, 1)
	r.mu.Lock()
	r.waiters[sid] = ch
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		if r.waiters[sid] == ch {
			delete(r.waiters, sid)
		}
		r.mu.Unlock()
	}
}

// deliver routes a decision to the waiter for its session, if one exists.
func (r *router) deliver(dec protocol.Decision) {
	r.mu.Lock()
	ch := r.waiters[dec.SessionID]
	r.mu.Unlock()
	if ch != nil {
		select {
		case ch <- dec:
		default:
		}
	}
}
