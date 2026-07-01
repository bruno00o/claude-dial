# claude-dial

A physical companion for Claude Code: a desk object with a round screen and a
rotary dial that shows, at a glance, what your parallel Claude Code sessions are
doing — who's working, who's waiting, and who needs a decision. When a session
asks for permission, the object lights up and you **turn the dial to approve or
reject**, without leaving your terminal.

> **Golden rule:** if the object is off or absent, Claude Code works exactly as
> usual. The gadget never blocks you.

The golden rule isn't a feature we maintain — it falls out of the transport. The
bridge listens on `localhost` via Claude Code's HTTP hooks; when the daemon is
down, the hook hits a closed port (connection refused, <10 ms) and Claude Code
proceeds with its normal terminal prompt. Nothing to configure.

## Two halves

| Half | What it is | Stack |
|------|------------|-------|
| **The bridge** (`bridge/`) | A daemon on your Mac that speaks Claude Code's hooks and drives the object | Go, zero external deps |
| **The object** (`firmware/`) | An [M5Stack Dial](https://docs.m5stack.com/en/core/M5Dial) (round screen + encoder) | C++ / PlatformIO / LVGL, BLE |

```
 Claude Code sessions ──HTTP hooks──▶  claude-dial daemon  ──BLE──▶  M5Stack Dial
   (PreToolUse, Stop, …)                (session state +              (round screen
                                         approve/deny loop)            + rotary dial)
                                              │
                                              └──SSE/HTTP──▶ web simulator (dev)
```

The daemon talks to a `Device` through one small interface. The **web simulator**
and the **real BLE Dial** are two implementations of it, speaking the same JSON —
so everything above the transport is developed and tested without hardware.

## Quick start (bridge + simulator)

```sh
make build   # -> bin/claude-dial  (or: go -C bridge build -o ../bin/claude-dial ./cmd/claude-dial)

# 1. run the bridge (also serves the simulator)
./bin/claude-dial serve
#   → open http://localhost:8787/  ← this page IS the dial

# 2. point Claude Code's hooks at the bridge
./bin/claude-dial hooks print            # inspect the settings.json snippet
./bin/claude-dial hooks install --write  # merge it into ~/.claude/settings.json (keeps a .bak)
```

Now run Claude Code. Sessions appear on the simulator; when one asks permission,
click **Allow once / Always allow / Reject** and watch the session continue.

Remove the hooks anytime: `claude-dial hooks uninstall --write`.

## Protocol

Host → device and device → host share one JSON contract
(`internal/protocol`), spoken over BLE by the firmware and over SSE/HTTP by the
simulator:

```jsonc
// host → device: session state
{"session_id":"…","state":"permission_request","tool_name":"Bash","command":"git push"}
//   states: working | idle | blocked | permission_request | closed
//   (herdr vocabulary: working = between prompt and Stop, idle = ready,
//    blocked = waiting on the user; stale "working" decays to idle after
//    --idle-after of hook silence)

// device → host: the dial's answer
{"session_id":"…","decision":"allow_once"}
//   decisions: allow_once | always_allow | reject | ask
```

The daemon maps decisions to Claude Code permission decisions:
`allow_once`/`always_allow` → `allow`, `reject` → `deny`, `ask` (and timeouts) →
`ask` (fall back to the terminal).

## Status

Early scaffold. Working today:

- [x] Bridge daemon: HTTP hook endpoint, per-session state, approve/deny loop
- [x] Golden-rule fallback (daemon down → terminal; no dial → `ask`; timeout → `ask`)
- [x] Web simulator (round screen, live sessions, tactile approve/deny)
- [x] `hooks install/uninstall` for `~/.claude/settings.json`
- [ ] BLE `Device` implementation (talk to the real M5Stack Dial)
- [ ] Firmware polish (`firmware/`, draft included)
- [ ] `always_allow` → persistent permission rule
- [ ] Usage / limits view (see below)
- [ ] Homebrew formula + `brew services`

### On usage (a later view)

Showing usage/consumption is planned as a **secondary view**, not competing with
the session/approval face. The proven recipe (see the sibling project
[Claudial](https://github.com/Moge800/Claudial)) is to poll Claude's API
rate-limit headers (~60 s, ~cents/month), rather than OTEL or transcript parsing.

## License

MIT. See [LICENSE](LICENSE).
