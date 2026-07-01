# claude-dial firmware (M5Stack Dial)

Firmware for the physical dial. It advertises a BLE GATT service, renders the
session dashboard / permission prompt on the round screen, and sends the user's
dial answer back to the bridge daemon.

> This is the **cleaned Schematik draft** — a working starting point, not yet
> polished. It already implements the important correctness bits: BLE writes are
> copied into a FreeRTOS queue and drained from `loop()` (no data races), a FIFO
> of parallel permission requests, per-detent encoder stepping, clean short-press
> vs long-hold, and **permission timeout → `ask`** (falls back to the terminal,
> honoring the golden rule).

## BLE contract

| | UUID |
|---|---|
| Service | `12345678-1234-1234-1234-123456789ABC` |
| RX (write, host → device) | `…ABD` |
| TX (notify, device → host) | `…ABE` |

Messages match `internal/protocol` on the daemon side:

```jsonc
// host → device
{"session_id":"…","state":"permission_request","tool_name":"Bash","command":"rm -rf /tmp/x"}
{"type":"set_time","epoch":1782765000,"tz_offset":7200}
// device → host
{"session_id":"…","decision":"allow_once"}   // allow_once | always_allow | reject | ask
```

## Build

```sh
cd firmware/claude-dial
pio run                # build
pio run -t upload      # flash the Dial over USB-C
pio device monitor     # serial logs
```

`platformio.ini` is a starting point; lib versions may need pinning.

## Next

- Confirm the BLE UUIDs / message field names stay in lockstep with `internal/protocol`.
- Implement the matching BLE `Device` on the daemon side (`internal/ble`).
