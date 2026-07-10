// Package protocol defines the message contract shared by the bridge daemon,
// the web simulator, and the M5Stack Dial firmware.
//
// The firmware (firmware/claude-dial) speaks these same JSON shapes over BLE:
//
//	Host -> device : {"session_id":"…","state":"permission_request",
//	                  "tool_name":"Bash","command":"rm -rf /tmp/x"}
//	Device -> host : {"session_id":"…","decision":"allow_once"}
//
// The web simulator speaks the same logical messages over SSE + HTTP, so the
// daemon's Device abstraction is transport-agnostic: swapping the simulator for
// the real Dial changes nothing above the transport layer.
package protocol

// Session states, as rendered on the device. Kept identical to the firmware.
// Vocabulary follows herdr: working = processing a task (between UserPromptSubmit
// and Stop), idle = ready for a prompt, blocked = waiting on the user (e.g. a
// permission prompt answered in the terminal). permission_request is our
// addition: blocked AND actively awaiting a dial decision.
const (
	StateWorking    = "working"
	StateIdle       = "idle"
	StateBlocked    = "blocked"
	StatePermission = "permission_request"
	StateClosed     = "closed"
)

// Device decisions, as emitted by the dial. Mapped to Claude Code permission
// decisions by the daemon (see daemon.mapDecision).
const (
	DecisionAllowOnce   = "allow_once"
	DecisionAlwaysAllow = "always_allow"
	DecisionReject      = "reject"
	DecisionAsk         = "ask"
)

// SessionView is one session as shown on the round screen.
type SessionView struct {
	SessionID string `json:"session_id"`
	Project   string `json:"project"`
	State     string `json:"state"`
	ToolName  string `json:"tool_name,omitempty"`
	Command   string `json:"command,omitempty"`

	// Per-conversation usage, filled in by the daemon from this session's own
	// transcript (whose filename is the session id). Both are raw token counts,
	// never a percentage — context has no reliable denominator (a model's max is
	// not carried in the transcript, and 1M-context variants share the base
	// model id), so we surface honest counts and let the reader judge. Omitted
	// (zero) when the daemon has no usage for the session yet.
	TotalTokens   int64   `json:"total_tokens,omitempty"`   // cumulative "work" tokens spent (input+output+cache_creation)
	ContextTokens int64   `json:"context_tokens,omitempty"` // tokens resident in the context window right now
	ContextPct    int     `json:"context_pct,omitempty"`    // ContextTokens as a % of the model's max context (for the rim)
	SubAgents     int     `json:"sub_agents,omitempty"`     // Task sub-agents this conversation has spawned
	CostUSD       float64 `json:"cost_usd,omitempty"`       // cumulative USD cost for this conversation (ccusage-style)
	Model         string  `json:"model,omitempty"`          // short model name, e.g. "sonnet-4-6"
	Errored       bool    `json:"errored,omitempty"`        // the conversation's most recent tool call failed
	ElapsedSecs   int     `json:"elapsed_secs,omitempty"`   // seconds since this session first appeared
	CachePct      int     `json:"cache_pct,omitempty"`      // % of input tokens served from cache (the cache saver)
	Stuck         bool    `json:"stuck,omitempty"`          // working the same command too long (hung / loop)
	ColorIdx      int     `json:"color_idx,omitempty"`      // per-project palette index, for a colour dot
}

// RecentConv is one recently-active conversation for the history screen. Built
// from the transcripts the daemon already scans, so it survives restarts and
// includes conversations that ended before the device connected.
type RecentConv struct {
	SessionID string  `json:"session_id"`
	Project   string  `json:"project"`
	Total     int64   `json:"total,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
	Model     string  `json:"model,omitempty"`
	Errored   bool    `json:"errored,omitempty"`
	AgeSecs   int     `json:"age_secs,omitempty"`
}

// Snapshot is the full state pushed to a Device on every change.
type Snapshot struct {
	Sessions []SessionView `json:"sessions"`
	// Recent is the recent-history list (newest first), for the history screen.
	Recent []RecentConv `json:"recent,omitempty"`
	// UsagePct is how full the 5h usage window is (0..100), for the rim gauge.
	UsagePct int `json:"usage_pct,omitempty"`
	// TodayCost is total spend since local midnight; BudgetPct is that as a % of
	// the configured daily budget (0 when no budget is set).
	TodayCost float64 `json:"today_cost,omitempty"`
	BudgetPct int     `json:"budget_pct,omitempty"`
	// EtaMins is the burn forecast: minutes until the 5h budget is hit (0 = n/a).
	EtaMins int `json:"eta_mins,omitempty"`
	// Today's edit volume, from Edit/Write tool inputs since local midnight.
	DiffAdded   int `json:"diff_added,omitempty"`
	DiffRemoved int `json:"diff_removed,omitempty"`
	DiffFiles   int `json:"diff_files,omitempty"`
	// Event is a one-shot glance-flash (commit / test pass·fail); EventEpoch is its
	// unix time, so the device flashes each event exactly once.
	Event      string `json:"event,omitempty"`
	EventLabel string `json:"event_label,omitempty"`
	EventEpoch int64  `json:"event_epoch,omitempty"`
	// Activity is a 24-char today heatmap, one char per hour ('0'..'9' intensity).
	Activity string `json:"activity,omitempty"`
	// ModelSpend is today's spend split by model family, e.g. "opus $200 · sonnet $80".
	ModelSpend string `json:"model_spend,omitempty"`
}

// Outbound is a single host -> device message (an RX write on the firmware).
// The BLE device sends one per changed session (State StateClosed removes it),
// plus a set_time control message on connect.
type Outbound struct {
	SessionID string `json:"session_id,omitempty"`
	Project   string `json:"project,omitempty"`
	State     string `json:"state,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	Command   string `json:"command,omitempty"`

	// Per-conversation usage (see SessionView). Raw token counts, not a gauge.
	TotalTokens   int64   `json:"total_tokens,omitempty"`
	ContextTokens int64   `json:"context_tokens,omitempty"`
	ContextPct    int     `json:"context_pct,omitempty"`
	SubAgents     int     `json:"sub_agents,omitempty"`
	CostUSD       float64 `json:"cost_usd,omitempty"`
	Model         string  `json:"model,omitempty"`
	Errored       bool    `json:"errored,omitempty"`
	ElapsedSecs   int     `json:"elapsed_secs,omitempty"`
	CachePct      int     `json:"cache_pct,omitempty"`
	Stuck         bool    `json:"stuck,omitempty"`
	ColorIdx      int     `json:"color_idx,omitempty"`

	// control messages: {"type":"set_time","epoch":…,"tz_offset":…,"host":"…"},
	// {"type":"ota_available","version":"0.6.0"} (empty version clears the prompt),
	// and {"type":"usage","pct":42} (the 5h usage gauge).
	Type     string `json:"type,omitempty"`
	Epoch    int64  `json:"epoch,omitempty"`
	TZOffset int    `json:"tz_offset,omitempty"`
	Version  string `json:"version,omitempty"`
	Host     string `json:"host,omitempty"` // the bridge's machine name, shown on the Dial
	Pct      int    `json:"pct,omitempty"`  // usage gauge fill (0..100)
	// carried on the {"type":"usage"} message: today's spend and its % of budget,
	// the burn-forecast ETA, and today's edit volume.
	TodayCost   float64 `json:"today_cost,omitempty"`
	BudgetPct   int     `json:"budget_pct,omitempty"`
	EtaMins     int     `json:"eta_mins,omitempty"`
	DiffAdded   int     `json:"diff_added,omitempty"`
	DiffRemoved int     `json:"diff_removed,omitempty"`
	DiffFiles   int     `json:"diff_files,omitempty"`
	Event       string  `json:"event,omitempty"`
	EventLabel  string  `json:"event_label,omitempty"`
	EventEpoch  int64   `json:"event_epoch,omitempty"`
	Activity    string  `json:"activity,omitempty"`
	ModelSpend  string  `json:"model_spend,omitempty"`
}

// Decision is a device -> host message: the user's answer on the dial.
type Decision struct {
	SessionID string `json:"session_id"`
	Decision  string `json:"decision"`
}

// DeviceHello is a device -> host announcement sent on connect ({"type":"hello",
// "fw":"0.4.0"}). It carries the running firmware version so the daemon can flag
// an available OTA update.
type DeviceHello struct {
	Type     string `json:"type"`
	Firmware string `json:"fw"`
	OTA      bool   `json:"ota"` // firmware accepts BLE OTA updates
}
