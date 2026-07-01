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
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/bruno00o/claude-dial/bridge/internal/daemon"
	"github.com/bruno00o/claude-dial/bridge/internal/hooks"
	"github.com/bruno00o/claude-dial/bridge/internal/session"
	"github.com/bruno00o/claude-dial/bridge/internal/web"
)

const version = "0.1.0-dev"

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
	case "status":
		cmdStatus(os.Args[2:])
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
  status            query a running daemon
  version
`)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", defaultPort(), "port to listen on")
	timeout := fs.Duration("timeout", 90*time.Second, "how long to wait for the dial before falling back to the terminal")
	idleAfter := fs.Duration("idle-after", 90*time.Second, "demote a silent working session to idle after this long")
	debug := fs.Bool("debug", false, "log every hook event received")
	_ = fs.Parse(args)

	store := session.New()
	hub := web.NewHub()
	d := daemon.New(store, hub, daemon.Config{Timeout: *timeout, IdleAfter: *idleAfter, Debug: *debug})

	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	d.RegisterRoutes(mux)

	addr := fmt.Sprintf("localhost:%d", *port)
	fmt.Printf("claude-dial %s\n", version)
	fmt.Printf("  bridge   http://%s/hook\n", addr)
	fmt.Printf("  simulator http://%s/\n", addr)
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

func defaultPort() int {
	if v := os.Getenv("CLAUDE_DIAL_PORT"); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil && p > 0 {
			return p
		}
	}
	return 8787
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
