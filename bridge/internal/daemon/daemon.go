// Package daemon is the bridge core: it receives Claude Code hook calls over
// HTTP, keeps per-session state, and drives a Device (the web simulator today,
// the BLE Dial tomorrow) for the approve/deny loop.
package daemon

import (
	"time"

	"github.com/bruno00o/claude-dial/bridge/internal/protocol"
	"github.com/bruno00o/claude-dial/bridge/internal/session"
)

// Device is anything that can render the sessions and answer permission
// requests. The web simulator and the (future) BLE Dial both satisfy it.
type Device interface {
	// Update pushes the full current state to the device.
	Update(protocol.Snapshot)
	// Connected reports whether a device is actually there to answer.
	Connected() bool
	// Decisions streams the user's answers back from the device.
	Decisions() <-chan protocol.Decision
}

// Daemon wires the session store to a device.
type Daemon struct {
	store   *session.Store
	dev     Device
	timeout time.Duration
	debug   bool

	router *router
}

// Config tunes the daemon.
type Config struct {
	// Timeout is how long to wait for the dial before falling back to the
	// normal terminal prompt (permissionDecision: "ask"). Golden rule.
	Timeout time.Duration
	// IdleAfter demotes a "working" session to "idle" after this much hook
	// silence — hooks are events, not a live feed, so a missed Stop (daemon
	// restart, crash) would otherwise stick forever. Long text-only turns fire
	// no hooks either, so keep this comfortably above a normal turn.
	IdleAfter time.Duration
	// BlockedIdleAfter demotes a "blocked" session to "idle" after this much
	// silence. A terminal-denied permission fires no clearing hook (confirmed:
	// Claude Code has no heartbeat while a permission pends, and no hook on a
	// terminal deny), so blocked can't be cleared positively and must self-heal
	// on a timer. This timer is only the *last resort*: an allowed permission
	// clears via PostToolUse, a deny-then-reply via MessageDisplay, and a
	// deny-then-new-prompt via UserPromptSubmit — all well before it fires. So it
	// really only governs the "you walked away, a permission is genuinely pending"
	// case, where a longer window keeps the "needs you" cue lit until you return.
	BlockedIdleAfter time.Duration
	// ForgetAfter drops a session entirely after this much silence (terminal
	// closed without a SessionEnd, machine slept, …).
	ForgetAfter time.Duration
	// Debug logs every hook event received.
	Debug bool
}

// New builds a daemon and starts routing device decisions to waiting requests.
func New(store *session.Store, dev Device, cfg Config) *Daemon {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 90 * time.Second
	}
	if cfg.IdleAfter <= 0 {
		cfg.IdleAfter = 90 * time.Second
	}
	if cfg.BlockedIdleAfter <= 0 {
		cfg.BlockedIdleAfter = 60 * time.Second
	}
	if cfg.ForgetAfter <= 0 {
		cfg.ForgetAfter = time.Hour
	}
	d := &Daemon{
		store:   store,
		dev:     dev,
		timeout: cfg.Timeout,
		debug:   cfg.Debug,
		router:  newRouter(),
	}
	go d.dispatch()
	go d.sweep(cfg.IdleAfter, cfg.BlockedIdleAfter, cfg.ForgetAfter)
	return d
}

// sweep periodically heals stale session state (see session.Store.Sweep) and
// re-broadcasts. It ticks frequently so a blocked session clears promptly after
// an answer; broadcasting every tick (not just on change) also lets a device
// that just (re)connected — e.g. the BLE Dial after a reconnect — catch up to
// the current state within one tick. Devices diff internally, so an unchanged
// broadcast is cheap.
func (d *Daemon) sweep(idleAfter, blockedIdleAfter, forgetAfter time.Duration) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for range tick.C {
		d.store.Sweep(idleAfter, blockedIdleAfter, forgetAfter)
		d.broadcast()
	}
}

// dispatch forwards each incoming device decision to the request waiting on
// that session, if any.
func (d *Daemon) dispatch() {
	for dec := range d.dev.Decisions() {
		d.router.deliver(dec)
	}
}

// broadcast renders the current store to the device.
func (d *Daemon) broadcast() {
	d.dev.Update(protocol.Snapshot{Sessions: disambiguate(d.store.Snapshot())})
}

// disambiguate makes each session's display name unique. When two or more
// sessions share a project label (several Claude sessions in the same repo —
// even on the same branch), a short session-id suffix is appended to the
// colliding ones; a session alone in its project keeps the bare name. The
// session id is the only guaranteed-unique key, and the suffix is stable (unlike
// an ordinal, which would shift when a session ends). Operates on the fresh
// slice from Snapshot, so the store is untouched, and benefits every device.
func disambiguate(sessions []protocol.SessionView) []protocol.SessionView {
	counts := make(map[string]int, len(sessions))
	for _, s := range sessions {
		counts[s.Project]++
	}
	for i := range sessions {
		if counts[sessions[i].Project] > 1 && len(sessions[i].SessionID) >= 3 {
			sessions[i].Project += " " + sessions[i].SessionID[:3]
		}
	}
	return sessions
}
