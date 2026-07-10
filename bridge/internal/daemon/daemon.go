// Package daemon is the bridge core: it receives Claude Code hook calls over
// HTTP, keeps per-session state, and drives a Device (the web simulator today,
// the BLE Dial tomorrow) for the approve/deny loop.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
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

	// dailyBudget is an optional daily spend target (USD). >0 drives the budget
	// gauge/alert; 0 disables it.
	dailyBudget float64

	// notifyURL, if set, receives a JSON POST whenever the fleet mood changes
	// (idle / working / needs_you) — wire it to Home Assistant, a smart light, …
	notifyURL string
	lastMood  atomic.Value // string, the last mood POSTed (dedup)

	// milestone (achievement) tracking — fired at most once per local day.
	msMu    sync.Mutex
	msDay   int
	msFired map[string]bool
}

// milestones are the daily achievements celebrated with a glance-flash on the Dial.
var milestones = []struct {
	key, label, metric string
	threshold          float64
}{
	{"diff10k", "10k lines today", "diff", 10000},
	{"diff5k", "5000 lines today", "diff", 5000},
	{"diff1k", "1000 lines today", "diff", 1000},
	{"tok10m", "10M tokens today", "tokens", 10_000_000},
	{"tok1m", "1M tokens today", "tokens", 1_000_000},
	{"cost250", "$250 today", "cost", 250},
	{"cost100", "$100 today", "cost", 100},
	{"cost50", "$50 today", "cost", 50},
}

// checkMilestone returns the label of a milestone freshly crossed today (empty if
// none). On a fresh day / startup it seeds the already-crossed ones without
// celebrating, so only NEW crossings flash. Cheap; called on each broadcast.
func (d *Daemon) checkMilestone() string {
	diff := float64(d.usage.DiffToday().Added)
	cost, _ := d.todaySpend()
	tokens := float64(d.usage.TodayTokens())
	val := func(metric string) float64 {
		switch metric {
		case "diff":
			return diff
		case "cost":
			return cost
		case "tokens":
			return tokens
		}
		return 0
	}
	day := time.Now().YearDay()
	d.msMu.Lock()
	defer d.msMu.Unlock()
	if day != d.msDay {
		d.msDay = day
		d.msFired = map[string]bool{}
		for _, m := range milestones {
			if val(m.metric) >= m.threshold {
				d.msFired[m.key] = true // seed history — no celebration for it
			}
		}
		return ""
	}
	for _, m := range milestones {
		if !d.msFired[m.key] && val(m.metric) >= m.threshold {
			d.msFired[m.key] = true
			return m.label
		}
	}
	return ""
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
	// AliasesPath is an optional JSON map of project-name → display-name overrides.
	// Empty disables aliasing (projects show their resolved name).
	AliasesPath string
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
	// DailyBudget is an optional daily spend target in USD. >0 shows a budget
	// gauge and alerts when today's spend crosses it.
	DailyBudget float64
	// NotifyURL, if set, receives a JSON POST on every fleet-mood change — wire it
	// to Home Assistant / a webhook / a smart light. Empty disables it.
	NotifyURL string
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
		dailyBudget:   cfg.DailyBudget,
		notifyURL:     cfg.NotifyURL,
	}
	projAliases = loadAliases(cfg.AliasesPath) // project-name → display-name overrides
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
	raw := d.store.Snapshot()
	for i := range raw {
		raw[i].ColorIdx = colorIdx(raw[i].Project) // hash the true project, before disambiguation
	}
	sessions := prioritize(disambiguate(raw))
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
		sessions[i].Model = strings.TrimPrefix(u.Model, "claude-") // "claude-sonnet-4-6" → "sonnet-4-6"
		sessions[i].Errored = u.LastError
		sessions[i].CachePct = u.CachePct
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

// todaySpend sums today's dollar spend across conversations and expresses it as a
// percentage of the daily budget (0 when no budget is configured).
func (d *Daemon) todaySpend() (cost float64, pct int) {
	for _, u := range d.usage.PerSession() {
		cost += u.TodayCost
	}
	if d.dailyBudget > 0 {
		pct = int(cost * 100 / d.dailyBudget)
	}
	return cost, pct
}

// colorIdx maps a project name to one of 12 palette slots (FNV-1a), so each repo
// wears a stable colour dot on the roster and you recognise it at a glance.
func colorIdx(s string) int {
	if s == "" {
		return 0
	}
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return int(h % 12)
}

// maybeNotify POSTs the fleet mood (idle / working / needs_you) to notifyURL when
// it changes, so an external target (Home Assistant, a smart light) can react.
// Fire-and-forget with a short timeout so it never blocks the broadcast path.
func (d *Daemon) maybeNotify(sessions []protocol.SessionView) {
	if d.notifyURL == "" {
		return
	}
	needs, working := 0, 0
	for _, s := range sessions {
		switch s.State {
		case protocol.StatePermission, protocol.StateBlocked:
			needs++
		case protocol.StateWorking:
			working++
		}
	}
	mood := "idle"
	if needs > 0 {
		mood = "needs_you"
	} else if working > 0 {
		mood = "working"
	}
	if prev, _ := d.lastMood.Load().(string); prev == mood {
		return
	}
	d.lastMood.Store(mood)
	body, _ := json.Marshal(map[string]any{
		"mood": mood, "needs_you": needs, "working": working, "sessions": len(sessions),
	})
	go func() {
		req, err := http.NewRequest(http.MethodPost, d.notifyURL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 4 * time.Second}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
}

// recentConversations builds the history list from the transcripts the usage
// reader already scans: every recent conversation with a real cwd, newest first,
// capped. Survives restarts and includes ones that ended before the device connected.
func (d *Daemon) recentConversations(limit int) []protocol.RecentConv {
	per := d.usage.PerSession()
	list := make([]protocol.RecentConv, 0, len(per))
	for sid, u := range per {
		if u.Cwd == "" {
			continue // journals / non-conversation files have no cwd
		}
		list = append(list, protocol.RecentConv{
			SessionID: sid,
			Project:   projectName(u.Cwd),
			Total:     u.Total,
			CostUSD:   u.Cost,
			Model:     strings.TrimPrefix(u.Model, "claude-"),
			Errored:   u.LastError,
			AgeSecs:   int(time.Since(u.LastActive).Seconds()),
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].AgeSecs < list[j].AgeSecs })
	if len(list) > limit {
		list = list[:limit]
	}
	return list
}

// modelFamily buckets a model id into a short family name for the spend breakdown.
func modelFamily(m string) string {
	switch {
	case strings.Contains(m, "opus"):
		return "opus"
	case strings.Contains(m, "sonnet"):
		return "sonnet"
	case strings.Contains(m, "haiku"):
		return "haiku"
	case strings.Contains(m, "fable"), strings.Contains(m, "mythos"):
		return "fable"
	case m == "":
		return "?"
	default:
		return m
	}
}

// modelSpend formats today's spend split by model family, biggest first, top 3 —
// e.g. "opus $200 · sonnet $80". Empty when nothing spent today.
func (d *Daemon) modelSpend() string {
	by := map[string]float64{}
	for _, u := range d.usage.PerSession() {
		if u.TodayCost > 0 {
			by[modelFamily(u.Model)] += u.TodayCost
		}
	}
	type ms struct {
		m string
		c float64
	}
	list := make([]ms, 0, len(by))
	for m, c := range by {
		list = append(list, ms{m, c})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].c > list[j].c })
	parts := make([]string, 0, 3)
	for i, x := range list {
		if i >= 3 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s $%.0f", x.m, x.c))
	}
	return strings.Join(parts, " · ")
}

// broadcast renders the current (enriched) store to the device.
func (d *Daemon) broadcast() {
	today, budgetPct := d.todaySpend()
	st := d.usage.Latest()
	diff := d.usage.DiffToday()
	// A notable event (commit / test) flashes on the device — but only while it's
	// fresh, so a daemon restart never re-flashes an old one; the device dedups by
	// EventEpoch so it fires exactly once.
	ev := d.usage.LastEvent()
	evKind, evLabel, evEpoch := "", "", int64(0)
	if ev.Kind != "" && time.Since(ev.Time) < 3*time.Minute {
		evKind, evLabel, evEpoch = ev.Kind, ev.Label, ev.Time.Unix()
	}
	if ms := d.checkMilestone(); ms != "" { // a fresh achievement trumps the transcript event
		evKind, evLabel, evEpoch = "milestone", ms, time.Now().Unix()
	}
	sessions := d.enrichedSessions()
	d.maybeNotify(sessions)
	d.dev.Update(protocol.Snapshot{
		Sessions:    sessions,
		Recent:      d.recentConversations(10),
		UsagePct:    st.Pct(),
		TodayCost:   today,
		BudgetPct:   budgetPct,
		EtaMins:     st.EtaMins,
		DiffAdded:   diff.Added,
		DiffRemoved: diff.Removed,
		DiffFiles:   diff.Files,
		Event:       evKind,
		EventLabel:  evLabel,
		EventEpoch:  evEpoch,
		Activity:    d.usage.Activity(),
		ModelSpend:  d.modelSpend(),
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
