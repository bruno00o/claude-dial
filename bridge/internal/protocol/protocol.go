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
}

// Snapshot is the full state pushed to a Device on every change.
type Snapshot struct {
	Sessions []SessionView `json:"sessions"`
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

	// control messages: {"type":"set_time","epoch":…,"tz_offset":…} and
	// {"type":"ota_available","version":"0.6.0"} (empty version clears the prompt).
	Type     string `json:"type,omitempty"`
	Epoch    int64  `json:"epoch,omitempty"`
	TZOffset int    `json:"tz_offset,omitempty"`
	Version  string `json:"version,omitempty"`
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
