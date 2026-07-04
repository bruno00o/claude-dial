// Command claude-dial is the bridge between Claude Code and the physical dial.
//
// Usage:
//
//	claude-dial serve [--port N] [--timeout DUR]   run the bridge daemon
//	claude-dial hooks print [--port N]             show the settings.json snippet
//	claude-dial hooks install [--port N] [--write] install the hooks (needs --write)
//	claude-dial hooks uninstall [--write]          remove the hooks (needs --write)
//	claude-dial firmware flash [--serial DEV] [--image BIN]  first USB flash of a blank Dial
//	claude-dial status [--port N]                  query a running daemon
//	claude-dial version
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bruno00o/claude-dial/bridge/internal/ble"
	"github.com/bruno00o/claude-dial/bridge/internal/daemon"
	"github.com/bruno00o/claude-dial/bridge/internal/firmware"
	"github.com/bruno00o/claude-dial/bridge/internal/flash"
	"github.com/bruno00o/claude-dial/bridge/internal/hooks"
	"github.com/bruno00o/claude-dial/bridge/internal/service"
	"github.com/bruno00o/claude-dial/bridge/internal/session"
	"github.com/bruno00o/claude-dial/bridge/internal/web"
)

const version = "1.1.0" // x-release-please-version

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
  firmware update   flash the latest firmware to the Dial over BLE (already-flashed Dial)
  firmware flash    first flash of a blank Dial over USB (needs esptool)
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
	usageBudget := fs.Int64("usage-budget", 0, "token budget for the 5h usage gauge (0 = auto-calibrate to your heaviest recent 5h)")
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

	d := daemon.New(store, dev, daemon.Config{Timeout: *timeout, IdleAfter: *idleAfter, RulesPath: rulesPath(), BridgeVersion: version, UsageBudgetTokens: *usageBudget, Debug: *debug})

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
		fmt.Fprintln(os.Stderr, "firmware: expected status|update|flash")
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("firmware "+sub, flag.ExitOnError)
	port := fs.Int("port", defaultPort(), "daemon port")
	force := fs.Bool("force", false, "flash even if the Dial is already up to date")
	// flash-only flags
	_ = fs.Bool("usb", true, "flash over USB (the only transport for a blank Dial)")
	serial := fs.String("serial", "", "USB serial device to flash (default: pick interactively)")
	image := fs.String("image", "", "local factory .bin to flash (default: download from the latest release)")
	baud := fs.Int("baud", 0, "esptool baud rate (0 = default 460800)")
	assumeYes := fs.Bool("yes", false, "skip the confirmation prompt (non-interactive)")
	_ = fs.Parse(args[1:])

	switch sub {
	case "status":
		firmwareStatus(*port)
	case "update":
		firmwareUpdate(*port, *force)
	case "flash":
		firmwareFlash(*serial, *image, *baud, *assumeYes)
	default:
		fmt.Fprintf(os.Stderr, "firmware: unknown subcommand %q\n", sub)
		os.Exit(2)
	}
}

// firmwareFlash lands claude-dial on a blank Dial over USB via esptool. Unlike
// `firmware update` (BLE OTA of an already-flashed Dial), this needs no running
// daemon: it picks the target USB device, fetches the full factory image (or a
// local --image), confirms, then delegates the flash to esptool.
func firmwareFlash(serial, image string, baud int, assumeYes bool) {
	tool, err := flash.FindESPTool()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// 1. Choose which USB device to flash — interactively unless --serial pins it.
	if serial == "" {
		serial, err = pickSerialPort()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	// 2. Resolve the image: a local --image, or the release's factory image.
	imgPath, imgDesc, cleanup := resolveFactoryImage(image)
	defer cleanup()

	// 3. Confirm before overwriting the board's flash.
	fmt.Printf("\nReady to flash:\n  device : %s\n  image  : %s\n  esptool: %s\n", serial, imgDesc, tool)
	if !assumeYes && !confirm("Flash this device now? It overwrites its flash") {
		fmt.Println("aborted — nothing was written.")
		return
	}

	// 4. Flash.
	fmt.Println()
	if err := flash.Run(context.Background(), tool, serial, imgPath, baud, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "\nflash failed:", err)
		fmt.Fprintln(os.Stderr, "  if it can't connect, hold the Dial's BOOT button while plugging it in, then re-run.")
		os.Exit(1)
	}
	fmt.Println("\ndone — the Dial rebooted into claude-dial firmware.")
	fmt.Println("next: grant Bluetooth once in the foreground →  claude-dial serve --ble")
}

// pickSerialPort lists the USB serial devices and lets the user choose one. With
// a single device it still shows it (the final confirm covers the mistake case);
// with none it explains how to make the Dial appear.
func pickSerialPort() (string, error) {
	ports := flash.Ports()
	switch len(ports) {
	case 0:
		return "", fmt.Errorf("no USB serial device found — plug the Dial into the Mac with a data USB-C cable (not charge-only), then re-run")
	case 1:
		fmt.Printf("USB serial device: %s\n", ports[0])
		return ports[0], nil
	}
	fmt.Println("USB serial devices:")
	for i, p := range ports {
		fmt.Printf("  [%d] %s\n", i+1, p)
	}
	for {
		fmt.Printf("Which one is the Dial? [1-%d]: ", len(ports))
		line, ok := readLine()
		if !ok {
			return "", fmt.Errorf("no selection (not a terminal?) — pass --serial <device>")
		}
		line = strings.TrimSpace(line)
		if line == "" {
			line = "1"
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(ports) {
			return ports[n-1], nil
		}
		fmt.Println("  please enter a number from the list.")
	}
}

// resolveFactoryImage returns a path to the factory .bin to flash, a
// human-readable description, and a cleanup func (removes a downloaded temp
// file; a no-op for a local --image).
func resolveFactoryImage(local string) (path, desc string, cleanup func()) {
	if local != "" {
		return local, local + " (local)", func() {}
	}
	ck := firmware.NewChecker("")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := ck.Refresh(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "cannot fetch the firmware manifest:", err)
		fmt.Fprintln(os.Stderr, "  offline? build locally and pass --image factory.bin — see firmware/README.md")
		os.Exit(1)
	}
	data, m, err := ck.DownloadFactory(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot download the factory image:", err)
		os.Exit(1)
	}
	f, err := os.CreateTemp("", "claude-dial-factory-*.bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "temp file:", err)
		os.Exit(1)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		fmt.Fprintln(os.Stderr, "write image:", err)
		os.Exit(1)
	}
	f.Close()
	return f.Name(), fmt.Sprintf("claude-dial %s from the latest release (%d bytes)", m.Version, len(data)), func() { os.Remove(f.Name()) }
}

// confirm asks a yes/no question on stdin, defaulting to no.
func confirm(prompt string) bool {
	fmt.Printf("%s [y/N]: ", prompt)
	line, ok := readLine()
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// stdinReader is shared so successive prompts don't drop input buffered by a
// fresh reader (bufio reads ahead past the newline).
var stdinReader = bufio.NewReader(os.Stdin)

// readLine reads one line from stdin; ok is false at EOF (e.g. stdin not a tty).
func readLine() (string, bool) {
	line, err := stdinReader.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return line, true
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
