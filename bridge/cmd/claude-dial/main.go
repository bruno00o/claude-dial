// Command claude-dial is the bridge between Claude Code and the physical dial.
//
// Usage:
//
//	claude-dial serve [--port N] [--timeout DUR]   run the bridge daemon
//	claude-dial hooks print [--port N]             show the settings.json snippet
//	claude-dial hooks install [--port N] [--write] install the hooks (needs --write)
//	claude-dial hooks uninstall [--write]          remove the hooks (needs --write)
//	claude-dial status [--port N]                  query a running daemon
//	claude-dial version
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bruno00o/claude-dial/bridge/internal/ble"
	"github.com/bruno00o/claude-dial/bridge/internal/daemon"
	"github.com/bruno00o/claude-dial/bridge/internal/hooks"
	"github.com/bruno00o/claude-dial/bridge/internal/service"
	"github.com/bruno00o/claude-dial/bridge/internal/session"
	"github.com/bruno00o/claude-dial/bridge/internal/web"
)

const version = "0.10.0" // x-release-please-version

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "hooks":
		cmdHooks(os.Args[2:])
	case "service":
		cmdService(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "firmware":
		cmdFirmware(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("claude-dial", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `claude-dial — physical dial for Claude Code

  serve             run the bridge daemon (open http://localhost:PORT for the simulator)
  hooks print       show the settings.json snippet
  hooks install     write the hooks into ~/.claude/settings.json (use --write)
  hooks uninstall   remove them (use --write)
  service install   keep the daemon running at login via launchd (use --write)
  service uninstall  remove the launchd agent (use --write)
  service status    show whether the agent is loaded
  firmware status   show the Dial's firmware version and whether an update exists
  firmware update   flash the latest firmware to the Dial over BLE
  status            query a running daemon
  version
`)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", defaultPort(), "port to listen on")
	timeout := fs.Duration("timeout", 90*time.Second, "how long to wait for the dial before falling back to the terminal")
	idleAfter := fs.Duration("idle-after", 90*time.Second, "demote a silent working session to idle after this long")
	useBLE := fs.Bool("ble", false, "also drive a physical M5Stack Dial over BLE")
	debug := fs.Bool("debug", false, "log every hook event received")
	_ = fs.Parse(args)

	store := session.New()
	hub := web.NewHub()

	// The web simulator is always available; the BLE Dial joins it if asked for
	// and reachable. If BLE can't start, we keep going with just the simulator —
	// the gadget must never take the bridge down (golden rule).
	var dev daemon.Device = hub
	bleStatus := "off (--ble to enable)"
	if *useBLE {
		bleStatus = "scanning…"
		if bd, err := ble.New(*debug); err != nil {
			fmt.Fprintln(os.Stderr, "ble: disabled:", err)
			bleStatus = "unavailable: " + err.Error()
		} else {
			dev = daemon.NewFanout(hub, bd)
		}
	}

	d := daemon.New(store, dev, daemon.Config{Timeout: *timeout, IdleAfter: *idleAfter, RulesPath: rulesPath(), BridgeVersion: version, Debug: *debug})

	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	d.RegisterRoutes(mux)

	addr := fmt.Sprintf("localhost:%d", *port)
	fmt.Printf("claude-dial %s\n", version)
	fmt.Printf("  bridge    http://%s/hook\n", addr)
	fmt.Printf("  simulator http://%s/\n", addr)
	fmt.Printf("  dial      %s\n", bleStatus)
	fmt.Printf("  fallback  terminal prompt after %s with no dial\n", *timeout)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

func cmdHooks(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "hooks: expected print|install|uninstall")
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("hooks "+sub, flag.ExitOnError)
	port := fs.Int("port", defaultPort(), "daemon port the hooks point to")
	write := fs.Bool("write", false, "actually modify ~/.claude/settings.json")
	monitorOnly := fs.Bool("monitor-only", false, "install only the non-blocking session-view hooks (no tactile approval)")
	_ = fs.Parse(rest)

	switch sub {
	case "print":
		fmt.Println(hooks.Snippet(*port, *monitorOnly))
	case "install":
		mode := "with tactile approval (PreToolUse blocks until you answer on the dial)"
		if *monitorOnly {
			mode = "monitor-only (sessions appear on the dial; nothing blocks)"
		}
		if !*write {
			path, _ := hooks.SettingsPath()
			fmt.Printf("Would merge these hooks — %s — into %s:\n\n%s\n\nRe-run with --write to apply (a .bak backup is kept).\n", mode, path, hooks.Snippet(*port, *monitorOnly))
			return
		}
		path, err := hooks.Install(*port, *monitorOnly)
		check(err)
		fmt.Printf("Installed hooks (%s) into %s (backup at %s.bak)\n", mode, path, path)
	case "uninstall":
		if !*write {
			fmt.Println("Would remove claude-dial hooks from settings.json. Re-run with --write to apply.")
			return
		}
		path, err := hooks.Uninstall()
		check(err)
		fmt.Printf("Removed hooks from %s\n", path)
	default:
		fmt.Fprintf(os.Stderr, "hooks: unknown subcommand %q\n", sub)
		os.Exit(2)
	}
}

func cmdService(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "service: expected install|uninstall|status")
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("service "+sub, flag.ExitOnError)
	port := fs.Int("port", defaultPort(), "daemon port the agent serves on")
	useBLE := fs.Bool("ble", false, "run the daemon with --ble (drive the physical Dial)")
	write := fs.Bool("write", false, "actually load/unload the launchd agent")
	_ = fs.Parse(rest)

	switch sub {
	case "install":
		exe, err := os.Executable()
		check(err)
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		opts := service.Options{Exec: exe, Port: *port, BLE: *useBLE}
		plistPath, _ := service.PlistPath()
		if !*write {
			logPath, _ := service.LogPath()
			fmt.Printf("Would install a launchd agent at %s running:\n\n  %s %s\n\nLogs go to %s. Re-run with --write to load it.\n\n%s\n",
				plistPath, exe, strings.Join(service.ServeArgs(opts), " "), logPath, service.Plist(opts, logPath))
			return
		}
		path, err := service.Install(opts)
		check(err)
		fmt.Printf("Installed and loaded launchd agent %s\n  plist: %s\n  the daemon now starts at login and respawns if it exits.\n", service.Label, path)
	case "uninstall":
		if !*write {
			fmt.Println("Would unload the launchd agent and remove its plist. Re-run with --write to apply.")
			return
		}
		path, err := service.Uninstall()
		check(err)
		fmt.Printf("Removed launchd agent %s (plist %s)\n", service.Label, path)
	case "status":
		if service.Loaded() {
			path, _ := service.PlistPath()
			fmt.Printf("loaded (%s)\n", path)
		} else {
			fmt.Println("not loaded")
		}
	default:
		fmt.Fprintf(os.Stderr, "service: unknown subcommand %q\n", sub)
		os.Exit(2)
	}
}

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	port := fs.Int("port", defaultPort(), "daemon port")
	_ = fs.Parse(args)

	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/status", *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon not reachable:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body))
}

func cmdFirmware(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "firmware: expected status|update")
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("firmware "+sub, flag.ExitOnError)
	port := fs.Int("port", defaultPort(), "daemon port")
	force := fs.Bool("force", false, "flash even if the Dial is already up to date")
	_ = fs.Parse(args[1:])

	switch sub {
	case "status":
		firmwareStatus(*port)
	case "update":
		firmwareUpdate(*port, *force)
	default:
		fmt.Fprintf(os.Stderr, "firmware: unknown subcommand %q\n", sub)
		os.Exit(2)
	}
}

func firmwareStatus(port int) {
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/status", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon not reachable:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	var s struct {
		Firmware struct {
			Running               string `json:"running"`
			Latest                string `json:"latest"`
			Bridge                string `json:"bridge"`
			UpdateAvailable       bool   `json:"update_available"`
			UpdateBlockedByBridge bool   `json:"update_blocked_by_bridge"`
		} `json:"firmware"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		fmt.Fprintln(os.Stderr, "unexpected daemon response:", err)
		os.Exit(1)
	}

	fw := s.Firmware
	switch {
	case fw.Running == "":
		fmt.Println("dial: not connected (no firmware version reported)")
	case fw.UpdateAvailable:
		fmt.Printf("dial %s — update available: %s (run: claude-dial firmware update)\n", fw.Running, fw.Latest)
	case fw.UpdateBlockedByBridge:
		fmt.Printf("dial %s — firmware %s is available but needs a newer bridge (this bridge is %s).\n  run: brew upgrade claude-dial\n", fw.Running, fw.Latest, fw.Bridge)
	case fw.Latest == "":
		fmt.Printf("dial %s — latest version unknown (no manifest published yet)\n", fw.Running)
	default:
		fmt.Printf("dial %s — up to date\n", fw.Running)
	}
}

func firmwareUpdate(port int, force bool) {
	url := fmt.Sprintf("http://localhost:%d/firmware/update", port)
	if force {
		url += "?force=1"
	}
	resp, err := http.Post(url, "text/plain", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon not reachable:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintln(os.Stderr, "cannot update:", strings.TrimSpace(string(body)))
		os.Exit(1)
	}
	// Stream progress lines as the daemon flashes; fail if any reports an error.
	failed := false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		text := sc.Text()
		fmt.Println(text)
		if strings.HasPrefix(text, "error:") {
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

func defaultPort() int {
	if v := os.Getenv("CLAUDE_DIAL_PORT"); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil && p > 0 {
			return p
		}
	}
	return 8787
}

// rulesPath is where per-session "always allow" grants are persisted. An error
// resolving the user config dir yields "" — the daemon then keeps grants in
// memory only, which is fine (always-allow is a convenience, never a gate).
func rulesPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "claude-dial", "always-allow.json")
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
