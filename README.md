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
| **The object** (`firmware/`) | An [M5Stack Dial](https://docs.m5stack.com/en/core/M5Dial) (round screen + encoder) | C++ / PlatformIO / M5Unified (M5GFX), NimBLE |

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

### With the physical Dial (BLE)

Flash `firmware/` to an M5Stack Dial, then add `--ble`:

```sh
./bin/claude-dial serve --ble --debug
#   dial      scanning…   → connects to the Dial advertising the service UUID
```

The simulator and the Dial run together (state mirrors to both, decisions merge
from either). BLE starts fully in the background: if Bluetooth is off, denied,
or no Dial is around, the bridge runs normally on the simulator alone — the
gadget never blocks startup.

macOS Bluetooth permission is **per-executable**, and a background service can't
show the permission prompt. So the first time — and again after switching
binaries (e.g. a dev build → the Homebrew one) — run the daemon once in the
**foreground** to grant it, then hand it back to the service:

```sh
claude-dial serve --ble   # click Allow on the Bluetooth prompt; "dial" goes scanning… → connected; Ctrl-C
```

Without this, a `brew services` daemon scans forever and never connects.

## Install (Homebrew)

```sh
brew install bruno00o/tap/claude-dial
claude-dial serve --ble             # once, in the foreground, to grant Bluetooth (see above)
brew services start claude-dial     # keep it running at login — drives the Dial over BLE
claude-dial hooks install --write   # point Claude Code at the bridge
```

The Dial's firmware updates itself over BLE: when the bridge is newer than the
Dial it offers a tactile install right on the device (`claude-dial firmware
status` shows the state). `brew upgrade claude-dial` first — the bridge never
flashes firmware newer than itself.

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

Working end-to-end, released on Homebrew, firmware validated on-device.

- [x] Bridge daemon: HTTP hook endpoint, per-session state, approve/deny loop
- [x] Golden-rule fallback (daemon down → terminal; no dial → `ask`; timeout → `ask`)
- [x] Web simulator (round screen, live sessions, tactile approve/deny)
- [x] `hooks install/uninstall` for `~/.claude/settings.json`
- [x] BLE `Device` (`--ble`) driving the real M5Stack Dial — validated on hardware
- [x] Firmware: amber "terminal" UI (agent roster, full-screen permission takeover,
      clock, settings), sound cues, a first-run pairing screen, and factory reset
- [x] `always_allow` → persistent permission rule (per session, survives a restart)
- [x] Usage view: an ambient rim gauge of the trailing 5-hour token window (see below)
- [x] Homebrew formula + `brew services` (a launchd agent that starts at login)
- [x] Firmware self-update over BLE (OTA), gated so the Dial never runs ahead of the bridge

### Usage view

The rim of the screen doubles as an ambient usage gauge: it fills with how much of
the trailing **5-hour token window** you've spent, shading amber → hot → red as you
near the cap. The numbers come from **Claude Code's local transcripts** (the
[ccusage](https://github.com/ryoppippi/ccusage) approach) — no API key and no extra
polling of Anthropic — so it reflects real usage and keeps working offline. (An
earlier plan to read API rate-limit headers, like the sibling project
[Claudial](https://github.com/Moge800/Claudial), was dropped: the daemon can't see
a session's API calls, but it *can* read what those calls wrote to disk.)

## License

MIT. See [LICENSE](LICENSE).
