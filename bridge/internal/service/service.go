// Package service installs claude-dial as a macOS launchd LaunchAgent so the
// bridge daemon starts at login and is respawned if it dies — the persistent
// counterpart to running `serve` by hand.
//
// It shells out to launchctl (a system tool, not a Go dependency) and is
// non-destructive: it writes a single plist under ~/Library/LaunchAgents and
// removes exactly that file on uninstall. The golden rule is unaffected — if the
// agent isn't installed, or launchd never starts it, Claude Code's hooks just
// hit a closed port and proceed as usual.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Label is the launchd job label; it also names the plist file.
const Label = "com.github.bruno00o.claude-dial"

// Options describes the serve invocation the agent should run.
type Options struct {
	Exec string // absolute path to the claude-dial binary
	Port int    // serve --port (omitted from args when default 8787)
	BLE  bool   // serve --ble
}

// PlistPath returns the LaunchAgent plist path for the current user.
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// LogPath returns where the agent's stdout/stderr are written.
func LogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "claude-dial.log"), nil
}

// ServeArgs builds the `serve` arguments (after the binary path) that the agent
// runs. Exported so the CLI can show the same command in its dry-run preview.
func ServeArgs(o Options) []string {
	args := []string{"serve"}
	if o.Port != 0 && o.Port != 8787 {
		args = append(args, "--port", strconv.Itoa(o.Port))
	}
	if o.BLE {
		args = append(args, "--ble")
	}
	return args
}

// Plist renders the LaunchAgent property list. KeepAlive+RunAtLoad make launchd
// start the daemon at login and respawn it if it exits (crash or clean), which
// is exactly what a persistent bridge wants.
func Plist(o Options, logPath string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	b.WriteString("  <key>Label</key>\n  <string>" + Label + "</string>\n")
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	b.WriteString("    <string>" + xmlEscape(o.Exec) + "</string>\n")
	for _, a := range ServeArgs(o) {
		b.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	b.WriteString("  <key>StandardOutPath</key>\n  <string>" + xmlEscape(logPath) + "</string>\n")
	b.WriteString("  <key>StandardErrorPath</key>\n  <string>" + xmlEscape(logPath) + "</string>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// domain is the per-user launchd GUI domain, e.g. "gui/501".
func domain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// Install writes the plist and (re)loads it into launchd. It is idempotent: an
// already-loaded job is booted out first so the new definition takes effect.
func Install(o Options) (path string, err error) {
	path, err = PlistPath()
	if err != nil {
		return "", err
	}
	logPath, err := LogPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(Plist(o, logPath)), 0o644); err != nil {
		return "", err
	}
	// Replace any previous definition, then load the new one. bootout fails when
	// nothing is loaded — that's fine, so its error is ignored.
	_ = exec.Command("launchctl", "bootout", domain()+"/"+Label).Run()
	if out, err := exec.Command("launchctl", "bootstrap", domain(), path).CombinedOutput(); err != nil {
		return "", fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return path, nil
}

// Uninstall boots the job out of launchd and removes the plist.
func Uninstall() (path string, err error) {
	path, err = PlistPath()
	if err != nil {
		return "", err
	}
	_ = exec.Command("launchctl", "bootout", domain()+"/"+Label).Run()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return path, nil
}

// Loaded reports whether the job is currently loaded in launchd.
func Loaded() bool {
	// `launchctl list <label>` exits non-zero when the job isn't loaded.
	return exec.Command("launchctl", "list", Label).Run() == nil
}
