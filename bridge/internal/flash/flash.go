// Package flash performs the Dial's first, over-USB firmware flash by delegating
// to esptool — the canonical, chip-quirk-aware ESP flasher. The BLE OTA path
// (internal/ota) only updates an already-flashed Dial; a blank board's ROM
// bootloader speaks the esptool protocol over its USB-serial port, so this is
// how claude-dial first lands on the device.
//
// We shell out rather than reimplement the esptool protocol in Go: the M5Stack
// Dial's StampS3 uses the ESP32-S3 native USB peripheral, whose download-mode
// reset sequence has enough quirks that esptool is the reliable choice. esptool
// is a one-time, first-flash-only dependency (brew install esptool).
package flash

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
)

// Chip is the StampS3's SoC, passed to esptool.
const Chip = "esp32s3"

// DefaultBaud is esptool's flashing speed. 460800 is reliable over the S3's
// native USB; drop to 115200 with --baud if a cable/hub misbehaves.
const DefaultBaud = 460800

// FindESPTool locates the esptool executable, trying the v4+ `esptool` entry
// point then the legacy `esptool.py`. The returned error carries install
// guidance so the CLI can print it verbatim.
func FindESPTool() (string, error) {
	for _, name := range []string{"esptool", "esptool.py"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("esptool not found — install it once with `brew install esptool` (or `pip install esptool`), then re-run")
}

// serialGlobs are the macOS device patterns for the boards we ship on: the
// StampS3's native USB CDC shows up as cu.usbmodem*; USB-UART bridges (CH34x,
// CP210x) as cu.wchusbserial*/cu.usbserial*/cu.SLAB_USBtoUART*.
var serialGlobs = []string{
	"/dev/cu.usbmodem*",
	"/dev/cu.wchusbserial*",
	"/dev/cu.usbserial*",
	"/dev/cu.SLAB_USBtoUART*",
}

// Ports lists candidate USB-serial devices currently present, de-duplicated
// (the globs can overlap) and in stable order.
func Ports() []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range serialGlobs {
		m, err := filepath.Glob(g)
		if err != nil {
			continue
		}
		for _, p := range m {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// Run flashes imagePath (a merged factory image, written at 0x0) to port via
// esptool, streaming esptool's output to w. baud <= 0 uses DefaultBaud.
//
// The command uses only syntax common to esptool v4 and v5: the `write_flash`
// subcommand (v5 keeps the underscore alias) and no --before/--after, since
// default-reset-before / hard-reset-after are the default on both — and their
// *values* are the one thing v5 renamed (default_reset -> default-reset).
func Run(ctx context.Context, tool, port, imagePath string, baud int, w io.Writer) error {
	if baud <= 0 {
		baud = DefaultBaud
	}
	args := []string{
		"--chip", Chip,
		"--port", port,
		"--baud", strconv.Itoa(baud),
		"write_flash", "0x0", imagePath,
	}
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}
