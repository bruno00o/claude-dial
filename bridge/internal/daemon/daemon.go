// Package daemon is the bridge core: it receives Claude Code hook calls over
// HTTP, keeps per-session state, and drives a Device (the web simulator today,
// the BLE Dial tomorrow) for the approve/deny loop.
package daemon

import (
	"context"
	"sort"
	"sync/atomic"
	"time"

	"github.com/bruno00o/claude-dial/bridge/internal/firmware"
	"github.com/bruno00o/claude-dial/bridge/internal/protocol"
	"github.com/bruno00o/claude-dial/bridge/internal/rules"
	"github.com/bruno00o/claude-dial/bridge/internal/session"
	"github.com/bruno00o/claude-dial/bridge/internal/usage"
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
	// FirmwareVersion is the version the device reported, or "" if it has none
	// (the web simulator) or hasn't announced one yet.
	FirmwareVersion() string
}

// Flasher is an optional Device capability: pushing a firmware image over the
// transport, plus the tactile "update available" prompt loop. Only the BLE Dial
// implements it; the daemon type-asserts for it.
type Flasher interface {
	OTACapable() bool
	Flash(image []byte, version string, onProgress func(pct int)) error
	// SetUpdateAvailable drives the Dial's "update available" prompt (empty
	// version clears it). OTARequests streams the user's install confirmations.
	SetUpdateAvailable(version string)
	OTARequests() <-chan struct{}
}

// Daemon wires the session store to a device.
type Daemon struct {
	store   *session.Store
	dev     Device
	timeout time.Duration
	debug   bool

	router  *router
	rules   *rules.Store
	fw      *firmware.Checker
	usage   *usage.Reader
	otaBusy atomic.Bool // guards against concurrent firmware flashes

	// bridgeVersion is this binary's own version. The bridge drives the OTA, so
	// it must never flash firmware newer than itself (that firmware could expect
	// messages this bridge doesn't send yet); we only offer/flash up to it.
	bridgeVersion string

	// contextMax is the model's max context window (tokens), the denominator for
	// the per-session context gauge on the rim. Defaults to 1M.
	contextMax int64
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
	// RulesPath is the JSON file backing per-session "always allow" grants.
	// Empty keeps them in memory only (lost on restart).
	RulesPath string
	// FirmwareManifestURL overrides where the latest-firmware manifest is fetched
	// from. Empty uses firmware.DefaultManifestURL (the GitHub latest release).
	FirmwareManifestURL string
	// BridgeVersion is this binary's version, used to gate firmware OTA so the
	// bridge never flashes an image newer than itself. Empty disables the gate.
	BridgeVersion string
	// UsageBudgetTokens is the denominator for the 5h usage gauge. 0 self-calibrates
	// to the heaviest 5h in the last week. UsageDir overrides the transcript dir
	// (empty → ~/.claude/projects).
	UsageBudgetTokens int64
	UsageDir          string
	// ContextMax is the model's max context window (tokens) — the denominator for
	// the per-session context gauge shown on the Dial's rim. 0 defaults to 1M.
	ContextMax int64
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
	if cfg.ContextMax <= 0 {
		cfg.ContextMax = 1_000_000 // 1M-context models (the current default)
	}
	d := &Daemon{
		store:         store,
		dev:           dev,
		timeout:       cfg.Timeout,
		debug:         cfg.Debug,
		router:        newRouter(),
		rules:         rules.Load(cfg.RulesPath),
		fw:            firmware.NewChecker(cfg.FirmwareManifestURL),
		usage:         usage.NewReader(cfg.UsageDir, cfg.UsageBudgetTokens),
		bridgeVersion: cfg.BridgeVersion,
		contextMax:    cfg.ContextMax,
	}
	go d.dispatch()
	go d.sweep(cfg.IdleAfter, cfg.BlockedIdleAfter, cfg.ForgetAfter)
	go d.fw.Run(context.Background(), 30*time.Minute)
	go d.usage.Run(context.Background(), time.Minute)
	go d.consumeOTARequests()
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
		d.advertiseUpdate()
	}
}

// dispatch forwards each incoming device decision to the request waiting on
// that session, if any.
func (d *Daemon) dispatch() {
	for dec := range d.dev.Decisions() {
		d.router.deliver(dec)
	}
}

// enrichedSessions returns the prioritized roster with each session joined to its
// per-conversation usage (tokens, context %, sub-agents). The usage reader keys
// transcripts by session id (the filename), so this is an exact join onto the
// sessions we already track. Shared by broadcast() (to the device) and /status.
func (d *Daemon) enrichedSessions() []protocol.SessionView {
	sessions := prioritize(disambiguate(d.store.Snapshot()))
	per := d.usage.PerSession()
	if len(per) == 0 {
		return sessions
	}
	for i := range sessions {
		u, ok := per[sessions[i].SessionID]
		if !ok {
			continue
		}
		sessions[i].TotalTokens = u.Total
		sessions[i].ContextTokens = u.Context
		sessions[i].SubAgents = u.SubAgents
		sessions[i].CostUSD = u.Cost
		// Context as a % of the model's max context — the rim's gauge.
		if d.contextMax > 0 {
			pct := int(u.Context * 100 / d.contextMax)
			if pct > 100 {
				pct = 100
			}
			sessions[i].ContextPct = pct
		}
	}
	return sessions
}

// broadcast renders the current (enriched) store to the device.
func (d *Daemon) broadcast() {
	d.dev.Update(protocol.Snapshot{
		Sessions: d.enrichedSessions(),
		UsagePct: d.usage.Latest().Pct(),
	})
}

// stateRank is the roster priority key: sessions that need you sort to the top,
// idle ones sink to the bottom. Lower = nearer the top.
func stateRank(state string) int {
	switch state {
	case protocol.StatePermission, protocol.StateBlocked:
		return 0 // needs you — surface first
	case protocol.StateWorking:
		return 1
	default:
		return 2 // idle (and any unknown state)
	}
}

// prioritize reorders the roster so it reads needs-you → working → idle, keeping
// the store's insertion order within each tier (a stable sort, so rows don't
// jump around from tick to tick). Done once here so every device shows the same
// order: the simulator renders this slice directly; the BLE Dial re-buckets by
// session id into its own slots, so its render mirrors this same ranking.
func prioritize(sessions []protocol.SessionView) []protocol.SessionView {
	sort.SliceStable(sessions, func(i, j int) bool {
		return stateRank(sessions[i].State) < stateRank(sessions[j].State)
	})
	return sessions
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
