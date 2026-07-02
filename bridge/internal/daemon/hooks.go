package daemon

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bruno00o/claude-dial/bridge/internal/protocol"
)

// hookInput is the JSON Claude Code sends to a hook (the subset we use).
type hookInput struct {
	HookEventName string          `json:"hook_event_name"`
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
}

// hookOutput is the JSON a PreToolUse hook returns to steer the permission.
type hookOutput struct {
	HookSpecificOutput hookSpecific `json:"hookSpecificOutput"`
}

type hookSpecific struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
}

// RegisterRoutes wires the hook + status endpoints onto mux.
func (d *Daemon) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/hook", d.handleHook)
	mux.HandleFunc("/status", d.handleStatus)
}

func (d *Daemon) handleHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in hookInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.SessionID == "" {
		// Malformed input: stay out of the way, let Claude Code proceed.
		writeEmpty(w)
		return
	}

	if d.debug {
		short := in.SessionID
		if len(short) > 8 {
			short = short[:8]
		}
		log.Printf("hook %-18s session=%s tool=%s", in.HookEventName, short, in.ToolName)
	}

	// In monitor-only installs, PreToolUse points at /hook?mode=monitor: it is a
	// pure liveness notifier (a tool just started — e.g. right after the user
	// answered "yes" in the terminal), never a gate.
	if in.HookEventName == "PreToolUse" && r.URL.Query().Get("mode") != "monitor" {
		d.handlePreToolUse(w, r, in)
		return
	}
	d.handleEvent(w, in)
}

// handleEvent maps lifecycle hooks to session states. It never blocks and never
// returns a decision.
//
// State model (herdr vocabulary): "working" strictly between UserPromptSubmit
// and Stop, re-asserted by every liveness signal (a tool starting, a tool
// finishing, an assistant message rendering) so the dial tracks reality with
// sub-second lag instead of waiting for the next coarse event. "blocked" = the
// permission dialog is on screen (PermissionRequest, which also carries the
// tool + command for display). Answering it un-blocks fast: "yes" → the tool
// starts → PreToolUse notifier; "no" → Claude replies to the denial →
// MessageDisplay. The idle_prompt Notification and the Stop hook both settle
// the session back to "idle".
func (d *Daemon) handleEvent(w http.ResponseWriter, in hookInput) {
	project := projectName(in.Cwd)
	switch in.HookEventName {
	case "SessionStart", "Stop", "Notification":
		// Notification is installed with the idle_prompt matcher only.
		d.store.Touch(in.SessionID, project, protocol.StateIdle)
	case "UserPromptSubmit", "PostToolUse", "PostToolUseFailure":
		// A tool finishing / a new turn resolves any pending permission.
		d.store.Touch(in.SessionID, project, protocol.StateWorking)
	case "MessageDisplay":
		// Weak liveness: must not un-block a session waiting on a permission.
		d.store.TouchLiveness(in.SessionID, project)
	case "PreToolUse":
		// Monitor-mode notifier: a tool is about to run. This fires *before* any
		// PermissionRequest for the same call (not "after yes" — the post-yes
		// clear is PostToolUse), and the two hooks land in separate goroutines
		// that race. So this must be weak liveness that can't clobber a fresh
		// permission cue: a tool that also asks (AskUserQuestion fires both
		// PreToolUse and PermissionRequest at once) must land on "needs you",
		// not "working". Auto-approved tools have no PermissionRequest, so they
		// still assert working normally.
		d.store.TouchLiveness(in.SessionID, project)
	case "PermissionRequest": // the permission dialog just appeared
		d.store.Upsert(in.SessionID, project, protocol.StateBlocked,
			in.ToolName, extractCommand(in.ToolInput))
	case "SessionEnd":
		d.store.Remove(in.SessionID)
	default:
		writeEmpty(w)
		return
	}
	d.broadcast()
	writeEmpty(w)
}

// handlePreToolUse is the heart of the bridge: show the request on the dial and
// wait for the user's answer, falling back to the terminal prompt if the dial
// isn't there or doesn't respond in time.
func (d *Daemon) handlePreToolUse(w http.ResponseWriter, r *http.Request, in hookInput) {
	project := projectName(in.Cwd)
	command := extractCommand(in.ToolInput)
	d.store.Upsert(in.SessionID, project, protocol.StatePermission, in.ToolName, command)
	d.broadcast()

	// Golden rule: no device to ask -> hand back to the terminal immediately.
	if !d.dev.Connected() {
		d.store.SetState(in.SessionID, protocol.StateWorking)
		d.broadcast()
		writeDecision(w, "ask", "no claude-dial device connected")
		return
	}

	ch, cancel := d.router.register(in.SessionID)
	defer cancel()

	select {
	case dec := <-ch:
		perm, reason, state := mapDecision(dec.Decision)
		d.store.SetState(in.SessionID, state)
		d.broadcast()
		writeDecision(w, perm, reason)
	case <-time.After(d.timeout):
		d.store.SetState(in.SessionID, protocol.StateWorking)
		d.broadcast()
		writeDecision(w, "ask", "timed out waiting for the dial")
	case <-r.Context().Done():
		// Claude Code gave up on us; leave state as-is.
	}
}

func (d *Daemon) handleStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"connected": d.dev.Connected(),
		"sessions":  d.store.Snapshot(),
	})
}

// mapDecision translates a dial answer into a Claude Code permission decision
// and the session state to show afterwards.
func mapDecision(dec string) (permission, reason, state string) {
	switch dec {
	case protocol.DecisionAllowOnce:
		return "allow", "Approved once via claude-dial", protocol.StateWorking
	case protocol.DecisionAlwaysAllow:
		// TODO: persist a permission rule so future matching calls auto-allow.
		return "allow", "Always-allow via claude-dial", protocol.StateWorking
	case protocol.DecisionReject:
		return "deny", "Rejected via claude-dial", protocol.StateIdle
	default: // ask / unknown
		return "ask", "", protocol.StateWorking
	}
}

// projectMarkers name a project root, checked most-specific first. Covers the
// common ecosystems so the label is stable without relying on git alone.
var projectMarkers = []string{".git", "go.mod", "package.json", "Cargo.toml", "pyproject.toml", ".hg", ".svn"}

var (
	projMu    sync.Mutex
	projCache = map[string]string{}
)

// projectName derives a stable, human label for a session from its cwd (the
// basename of the nearest ancestor that looks like a project root), memoized by
// cwd. The marker walk stats the filesystem and runs on the dense liveness
// hooks, but a cwd's root never changes within a run, so each is resolved once.
func projectName(cwd string) string {
	if cwd == "" {
		return ""
	}
	projMu.Lock()
	defer projMu.Unlock()
	if n, ok := projCache[cwd]; ok {
		return n
	}
	n := resolveProject(cwd)
	projCache[cwd] = n
	return n
}

// resolveProject returns the basename of the nearest ancestor of cwd holding a
// project marker (.git, go.mod, …), or cwd's own basename if none is found — so
// the name stays steady as a session moves between subdirectories (bridge, src).
func resolveProject(cwd string) string {
	for dir := cwd; ; {
		for _, m := range projectMarkers {
			if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
				return filepath.Base(dir)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Base(cwd)
}

// extractCommand pulls a human-readable command out of a tool's input.
func extractCommand(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"command", "file_path", "path", "url", "pattern"} {
			if v, ok := m[k].(string); ok && v != "" {
				return truncate(v, 180)
			}
		}
	}
	return truncate(string(raw), 180)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func writeDecision(w http.ResponseWriter, decision, reason string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(hookOutput{HookSpecificOutput: hookSpecific{
		HookEventName:            "PreToolUse",
		PermissionDecision:       decision,
		PermissionDecisionReason: reason,
	}})
}

func writeEmpty(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}
