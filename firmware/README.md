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

## Flashing a Dial

**A blank Dial** (or one running other firmware) needs a first flash over USB —
BLE OTA (`claude-dial firmware update`) only works once claude-dial is already on
the device. The bridge drives this via esptool, with a device picker + confirm:

```sh
brew install esptool                 # one-time (or: pip install esptool)
claude-dial firmware flash           # lists USB devices → pick → confirm → flash
```

It downloads the release's **factory image** (bootloader + partition table +
app, merged at 0x0). Flags: `--serial <dev>` to skip the picker, `--yes` to skip
the prompt, `--image <bin>` to flash a local build instead of downloading.

Once flashed, later updates go over BLE: `claude-dial firmware update`.

## Build (from source)

```sh
cd firmware/claude-dial
pio run                # build
pio run -t upload      # flash the Dial over USB-C directly
pio device monitor     # serial logs
```

To flash a **locally built** image with the bridge (e.g. offline, before a
release ships the factory image), merge it the way CI does and pass `--image`:

```sh
B=.pio/build/m5stack-dial
BOOT_APP0=$(find ~/.platformio/packages -name boot_app0.bin | head -1)
esptool --chip esp32s3 merge_bin -o factory.bin \
  --flash_mode dio --flash_freq 80m --flash_size 8MB \
  0x0 $B/bootloader.bin 0x8000 $B/partitions.bin \
  0xe000 "$BOOT_APP0" 0x10000 $B/firmware.bin
claude-dial firmware flash --image factory.bin
```

`platformio.ini` is a starting point; lib versions may need pinning.

## Next

- Confirm the BLE UUIDs / message field names stay in lockstep with `internal/protocol`.
- Implement the matching BLE `Device` on the daemon side (`internal/ble`).
