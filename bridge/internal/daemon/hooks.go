package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bruno00o/claude-dial/bridge/internal/firmware"
	"github.com/bruno00o/claude-dial/bridge/internal/protocol"
)

// hookInput is the JSON Claude Code sends to a hook (the subset we use).
type hookInput struct {
	HookEventName string          `json:"hook_event_name"`
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	// PermissionMode is Claude Code's current mode: "default" (asks the user),
	// or an auto-approving mode — "acceptEdits", "auto", "plan", "dontAsk",
	// "bypassPermissions". The dial only takes over a decision in "default".
	PermissionMode string `json:"permission_mode"`
}

// autoApproves reports whether Claude Code's permission mode decides on its own
// (so the dial should stay out of the way). Only "default" — the mode that asks
// the user — warrants a tactile approval; every other mode auto-resolves. An
// empty mode (older Claude Code that doesn't send it) is treated as "default"
// so behaviour is unchanged there.
func autoApproves(mode string) bool {
	return mode != "" && mode != "default"
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
	mux.HandleFunc("/firmware/update", d.handleFirmwareUpdate)
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
		d.rules.Forget(in.SessionID) // per-session grants die with the session
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

	// Claude Code is in an auto-approving mode (acceptEdits/auto/bypass/plan/…):
	// it decides on its own, so the dial must not take over the screen for a
	// decision. Show the session as working and let Claude proceed — the tactile
	// approval only fires in "default" (ask) mode.
	if autoApproves(in.PermissionMode) {
		d.store.Upsert(in.SessionID, project, protocol.StateWorking, in.ToolName, command)
		d.broadcast()
		writeEmpty(w)
		return
	}

	// Already always-allowed for this session? Approve silently — no dial, no
	// wait — and show the tool as running.
	if d.rules.Allowed(in.SessionID, in.ToolName, command) {
		d.store.Upsert(in.SessionID, project, protocol.StateWorking, in.ToolName, command)
		d.broadcast()
		writeDecision(w, "allow", "always-allowed via claude-dial")
		return
	}

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
		if dec.Decision == protocol.DecisionAlwaysAllow {
			// Remember this exact call so it won't prompt again this session.
			d.rules.Allow(in.SessionID, in.ToolName, command)
		}
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

// bridgeCanFlash reports whether the bridge is new enough to drive the given
// firmware version. The bridge is the OTA driver and must never be older than
// the image it flashes (that firmware could expect messages this bridge doesn't
// send yet), so we only offer/flash firmware up to the bridge's own version. An
// empty bridge version (gate disabled) or empty firmware version is permissive.
func (d *Daemon) bridgeCanFlash(fwVersion string) bool {
	if d.bridgeVersion == "" || fwVersion == "" {
		return true
	}
	return !firmware.Newer(d.bridgeVersion, fwVersion) // fwVersion <= bridgeVersion
}

func (d *Daemon) handleStatus(w http.ResponseWriter, _ *http.Request) {
	running := d.dev.FirmwareVersion()
	latest := d.fw.Latest().Version
	newer := firmware.Newer(running, latest)
	u := d.usage.Latest()
	todayCost, budgetPct := d.todaySpend()
	diff := d.usage.DiffToday()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"connected": d.dev.Connected(),
		"sessions":  d.enrichedSessions(),
		"usage": map[string]any{
			"pct":          u.Pct(),
			"tokens":       u.Tokens,
			"budget":       u.Budget,
			"today_cost":   todayCost,
			"budget_pct":   budgetPct,
			"eta_mins":     u.EtaMins,
			"diff_added":   diff.Added,
			"diff_removed": diff.Removed,
			"diff_files":   diff.Files,
		},
		"firmware": map[string]any{
			"running":                  running,
			"latest":                   latest,
			"bridge":                   d.bridgeVersion,
			"update_available":         newer && d.bridgeCanFlash(latest),
			"update_blocked_by_bridge": newer && !d.bridgeCanFlash(latest),
		},
	})
}

// handleFirmwareUpdate downloads the latest firmware and flashes the connected
// Dial over BLE, streaming plain-text progress lines. It refuses when no
// OTA-capable Dial is connected or the Dial is already current (unless ?force).
func (d *Daemon) handleFirmwareUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if fl, ok := d.dev.(Flasher); !ok || !fl.OTACapable() {
		http.Error(w, "no OTA-capable Dial connected", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	flush, _ := w.(http.Flusher)
	line := func(s string) {
		fmt.Fprintln(w, s)
		if flush != nil {
			flush.Flush()
		}
	}
	force := r.URL.Query().Get("force") != ""
	if err := d.runFirmwareUpdate(r.Context(), force, line); err != nil {
		line("error: " + err.Error())
	}
}

// runFirmwareUpdate downloads the latest firmware and flashes the Dial, emitting
// progress via onLine. It is the shared core of the HTTP endpoint and the Dial's
// tactile trigger, and guards against concurrent flashes.
func (d *Daemon) runFirmwareUpdate(ctx context.Context, force bool, onLine func(string)) error {
	if !d.otaBusy.CompareAndSwap(false, true) {
		return errors.New("a firmware update is already in progress")
	}
	defer d.otaBusy.Store(false)

	fl, ok := d.dev.(Flasher)
	if !ok || !fl.OTACapable() {
		return errors.New("no OTA-capable Dial connected")
	}
	latest := d.fw.Latest()
	if latest.Version == "" {
		return errors.New("no firmware manifest available yet")
	}
	// Never flash newer than the bridge — not even with --force, since that's
	// exactly the skew we're preventing. Upgrade the bridge first.
	if !d.bridgeCanFlash(latest.Version) {
		return fmt.Errorf("firmware %s needs a newer bridge (this bridge is %s) — run `brew upgrade claude-dial` first",
			latest.Version, orUnknown(d.bridgeVersion))
	}
	running := d.dev.FirmwareVersion()
	if !force && !firmware.Newer(running, latest.Version) {
		onLine(fmt.Sprintf("already up to date (dial %s, latest %s)", orUnknown(running), latest.Version))
		return nil
	}

	onLine(fmt.Sprintf("downloading firmware %s…", latest.Version))
	image, _, err := d.fw.DownloadLatest(ctx)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	onLine(fmt.Sprintf("flashing %d bytes over BLE…", len(image)))
	last := -1
	if err := fl.Flash(image, latest.Version, func(pct int) {
		if pct != last && pct%10 == 0 {
			last = pct
			onLine(fmt.Sprintf("progress %d%%", pct))
		}
	}); err != nil {
		return err
	}
	onLine(fmt.Sprintf("done — the Dial is rebooting into %s", latest.Version))
	return nil
}

// advertiseUpdate keeps the Dial's tactile "update available" prompt in sync
// with whether a newer firmware exists. Called every sweep tick; the device
// dedups, so an unchanged state costs nothing.
func (d *Daemon) advertiseUpdate() {
	fl, ok := d.dev.(Flasher)
	if !ok {
		return
	}
	latest := d.fw.Latest().Version
	if firmware.Newer(d.dev.FirmwareVersion(), latest) && d.bridgeCanFlash(latest) {
		fl.SetUpdateAvailable(latest)
	} else {
		// Either up to date, or the bridge is too old to drive it — don't dangle
		// a tactile prompt the bridge would refuse. `firmware status` explains it.
		fl.SetUpdateAvailable("")
	}
}

// consumeOTARequests runs a firmware update whenever the user confirms one on
// the Dial. Progress shows on the Dial itself; here we just log.
func (d *Daemon) consumeOTARequests() {
	fl, ok := d.dev.(Flasher)
	if !ok {
		return
	}
	ch := fl.OTARequests()
	if ch == nil {
		return
	}
	for range ch {
		log.Println("ota: update requested from the Dial")
		if err := d.runFirmwareUpdate(context.Background(), false, func(s string) {
			if d.debug {
				log.Println("ota:", s)
			}
		}); err != nil {
			log.Println("ota: update failed:", err)
		}
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// mapDecision translates a dial answer into a Claude Code permission decision
// and the session state to show afterwards.
func mapDecision(dec string) (permission, reason, state string) {
	switch dec {
	case protocol.DecisionAllowOnce:
		return "allow", "Approved once via claude-dial", protocol.StateWorking
	case protocol.DecisionAlwaysAllow:
		// The grant itself is persisted by the caller (handlePreToolUse), which
		// has the session/tool/command; here we just allow this call through.
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
// projAliases maps a resolved project name to a user-chosen display name, loaded
// once at startup. nil when none are configured.
var projAliases map[string]string

// loadAliases reads an optional JSON object of project-name → display-name (e.g.
// {"claude-dial":"dial","5min-btc-polymarket":"btc"}) so a repo can wear a
// friendlier label on the small screen. A missing or malformed file yields none.
func loadAliases(path string) map[string]string {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

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
	if a, ok := projAliases[n]; ok && a != "" {
		n = a // user-chosen friendly name for this project
	}
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

// (autoApproves has a focused test in hooks_automode_test.go)
