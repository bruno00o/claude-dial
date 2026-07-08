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
	TotalTokens   int64 `json:"total_tokens,omitempty"`   // cumulative "work" tokens spent (input+output+cache_creation)
	ContextTokens int64 `json:"context_tokens,omitempty"` // tokens resident in the context window right now
}

// Snapshot is the full state pushed to a Device on every change.
type Snapshot struct {
	Sessions []SessionView `json:"sessions"`
	// UsagePct is how full the 5h usage window is (0..100), for the rim gauge.
	UsagePct int `json:"usage_pct,omitempty"`
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
	TotalTokens   int64 `json:"total_tokens,omitempty"`
	ContextTokens int64 `json:"context_tokens,omitempty"`

	// control messages: {"type":"set_time","epoch":…,"tz_offset":…,"host":"…"},
	// {"type":"ota_available","version":"0.6.0"} (empty version clears the prompt),
	// and {"type":"usage","pct":42} (the 5h usage gauge).
	Type     string `json:"type,omitempty"`
	Epoch    int64  `json:"epoch,omitempty"`
	TZOffset int    `json:"tz_offset,omitempty"`
	Version  string `json:"version,omitempty"`
	Host     string `json:"host,omitempty"` // the bridge's machine name, shown on the Dial
	Pct      int    `json:"pct,omitempty"`  // usage gauge fill (0..100)
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
