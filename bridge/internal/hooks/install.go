// Package hooks reads and writes the Claude Code settings.json so the bridge's
// HTTP endpoint is invoked on the relevant hook events.
//
// It is deliberately non-destructive: our hook groups are appended alongside any
// hooks the user already has, and identified by their localhost /hook URL so
// uninstall removes only ours.
package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// managedEvent is one hook event the bridge listens to.
type managedEvent struct {
	Event   string
	Timeout int    // seconds
	Matcher string // event-type filter ("" = all)
	Query   string // query string appended to the hook URL ("" = none)
	Mode    int    // which install modes include this event
}

// Install modes for a managed event.
const (
	modeBoth    = iota // installed in both modes
	modeApprove        // only with tactile approval
	modeMonitor        // only in monitor-only mode
)

// managedEvents. Liveness philosophy (borrowed from herdr): hooks are events,
// not a live feed, so we subscribe to every cheap signal that proves the
// session is doing something — PreToolUse fires the instant a tool starts
// (right after you answer "yes" in the terminal), MessageDisplay fires on every
// assistant message (covers "no" answers and long text-only turns), and
// PermissionRequest marks the session blocked the moment the dialog shows,
// with the tool + command so the dial can display what's being asked.
//
// In approval mode PreToolUse is the blocking approve/deny hook; in
// monitor-only mode it is reinstalled as a non-blocking notifier
// (?mode=monitor) so nothing ever gates a command.
var managedEvents = []managedEvent{
	{"PreToolUse", 90, "", "", modeApprove},
	{"PreToolUse", 5, "", "mode=monitor", modeMonitor},
	{"PermissionRequest", 5, "", "", modeBoth},
	{"SessionStart", 5, "", "", modeBoth},
	{"UserPromptSubmit", 5, "", "", modeBoth},
	{"PostToolUse", 5, "", "", modeBoth},
	{"MessageDisplay", 5, "", "", modeBoth},
	{"Stop", 5, "", "", modeBoth},
	{"Notification", 5, "idle_prompt", "", modeBoth},
	{"SessionEnd", 5, "", "", modeBoth},
}

// selected returns the events to install for the chosen mode.
func selected(monitorOnly bool) []managedEvent {
	var out []managedEvent
	for _, e := range managedEvents {
		if e.Mode == modeApprove && monitorOnly {
			continue
		}
		if e.Mode == modeMonitor && !monitorOnly {
			continue
		}
		out = append(out, e)
	}
	return out
}

// SettingsPath returns the user-level settings.json path.
func SettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func hookURL(port int, query string) string {
	u := fmt.Sprintf("http://localhost:%d/hook", port)
	if query != "" {
		u += "?" + query
	}
	return u
}

// hookGroup is one matcher-group holding our HTTP hook.
func hookGroup(port int, e managedEvent) map[string]any {
	return map[string]any{
		"matcher": e.Matcher,
		"hooks": []any{map[string]any{
			"type":    "http",
			"url":     hookURL(port, e.Query),
			"timeout": e.Timeout,
		}},
	}
}

// isOursGroup reports whether a matcher-group is a claude-dial group (contains an
// HTTP hook pointing at a localhost /hook endpoint).
func isOursGroup(g any) bool {
	gm, ok := g.(map[string]any)
	if !ok {
		return false
	}
	hs, ok := gm["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range hs {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if hm["type"] != "http" {
			continue
		}
		if u, ok := hm["url"].(string); ok &&
			strings.Contains(u, "/hook") &&
			(strings.Contains(u, "localhost") || strings.Contains(u, "127.0.0.1")) {
			return true
		}
	}
	return false
}

// stripOurs removes our groups from an event's group list, leaving the user's
// own hooks untouched.
func stripOurs(groups []any) []any {
	out := make([]any, 0, len(groups))
	for _, g := range groups {
		if isOursGroup(g) {
			continue
		}
		out = append(out, g)
	}
	return out
}

// Snippet returns the hooks fragment as pretty JSON, for the user to inspect or
// paste manually.
func Snippet(port int, monitorOnly bool) string {
	hooks := map[string]any{}
	for _, e := range selected(monitorOnly) {
		hooks[e.Event] = []any{hookGroup(port, e)}
	}
	b, _ := json.MarshalIndent(map[string]any{"hooks": hooks}, "", "  ")
	return string(b)
}

// Install appends the bridge hooks into settings.json without disturbing the
// user's existing hooks. A backup (.bak) is written first. It is idempotent:
// re-running (or switching modes) replaces only our own groups.
func Install(port int, monitorOnly bool) (path string, err error) {
	path, err = SettingsPath()
	if err != nil {
		return "", err
	}
	settings, err := load(path)
	if err != nil {
		return "", err
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	// Remove any of our previous groups from every managed event first.
	for _, e := range managedEvents {
		if groups, ok := hooks[e.Event].([]any); ok {
			hooks[e.Event] = stripOurs(groups)
		}
	}
	// Append our group to each selected event.
	for _, e := range selected(monitorOnly) {
		groups, _ := hooks[e.Event].([]any)
		hooks[e.Event] = append(groups, hookGroup(port, e))
	}
	// Drop any event arrays we've emptied out.
	for _, e := range managedEvents {
		if groups, ok := hooks[e.Event].([]any); ok && len(groups) == 0 {
			delete(hooks, e.Event)
		}
	}

	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}
	if err := save(path, settings); err != nil {
		return "", err
	}
	return path, nil
}

// Uninstall removes only the claude-dial groups from settings.json.
func Uninstall() (path string, err error) {
	path, err = SettingsPath()
	if err != nil {
		return "", err
	}
	settings, err := load(path)
	if err != nil {
		return "", err
	}
	if hooks, ok := settings["hooks"].(map[string]any); ok {
		for _, e := range managedEvents {
			if groups, ok := hooks[e.Event].([]any); ok {
				kept := stripOurs(groups)
				if len(kept) == 0 {
					delete(hooks, e.Event)
				} else {
					hooks[e.Event] = kept
				}
			}
		}
		if len(hooks) == 0 {
			delete(settings, "hooks")
		} else {
			settings["hooks"] = hooks
		}
	}
	if err := save(path, settings); err != nil {
		return "", err
	}
	return path, nil
}

func load(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func save(path string, settings map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil && len(existing) > 0 {
		if err := os.WriteFile(path+".bak", existing, 0o644); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}
