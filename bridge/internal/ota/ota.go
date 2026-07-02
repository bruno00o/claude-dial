// Package ota drives a firmware update to the Dial over an abstract transport.
//
// The sequence mirrors the firmware (internal firmware/claude-dial): write an
// ota_begin control message with the image size, wait for "ready", stream the
// image in MTU-sized chunks over the data channel, write ota_end, then wait for
// "done" (the Dial verifies, switches the boot slot and reboots) or "error".
//
// The transport is an interface so this logic is unit-testable without BLE; the
// BLE device implements it. An interrupted flash is safe by construction: the
// firmware only commits a verified image, so a failure here never bricks the
// Dial (golden rule extends to OTA).
package ota

import (
	"encoding/json"
	"fmt"
	"time"
)

// Status is one parsed ota_status notification from the Dial.
type Status struct {
	State string `json:"ota"` // ready | progress | done | error | aborted
	Pct   int    `json:"pct"`
	Msg   string `json:"msg"`
}

// Transport is the BLE plumbing Flash needs: two write channels (control + raw
// data) and the stream of status notifications, plus the negotiated MTU.
type Transport interface {
	WriteControl([]byte) error // ota_ctrl characteristic (JSON)
	WriteData([]byte) error    // ota_data characteristic (raw image bytes)
	Status() <-chan Status     // parsed ota_status notifications
	MTU() int                  // negotiated ATT MTU (payload cap is MTU-3)
}

// Tunables (var, not const, so tests can shrink the waits).
var (
	readyTimeout = 30 * time.Second // Update.begin erases flash — allow time
	doneTimeout  = 60 * time.Second // verify + set boot slot
	minChunk     = 20               // floor when the MTU is unknown/tiny
)

// Flash streams image to the Dial and returns once it reports "done" (or fails).
// version is echoed to the Dial so its progress screen can show what's being
// installed. onProgress, if non-nil, is called with 0..100 as bytes are acked.
func Flash(t Transport, image []byte, version string, onProgress func(pct int)) error {
	if len(image) == 0 {
		return fmt.Errorf("ota: empty image")
	}

	// Drain any stale status from a previous attempt so waitFor sees only ours.
	drain(t.Status())

	begin, _ := json.Marshal(map[string]any{"type": "ota_begin", "size": len(image), "version": version})
	if err := t.WriteControl(begin); err != nil {
		return fmt.Errorf("ota: begin: %w", err)
	}
	if err := waitFor(t.Status(), "ready", readyTimeout); err != nil {
		return err
	}

	chunk := t.MTU() - 3
	if chunk < minChunk {
		chunk = minChunk
	}
	for off := 0; off < len(image); off += chunk {
		end := off + chunk
		if end > len(image) {
			end = len(image)
		}
		if err := t.WriteData(image[off:end]); err != nil {
			return fmt.Errorf("ota: data at %d: %w", off, err)
		}
		if onProgress != nil {
			onProgress(end * 100 / len(image))
		}
		// A device-side failure (e.g. flash write error) shouldn't wait for the
		// whole stream to finish — surface it now.
		if s, ok := poll(t.Status()); ok && s.State == "error" {
			return fmt.Errorf("ota: device error: %s", s.Msg)
		}
	}

	endMsg, _ := json.Marshal(map[string]any{"type": "ota_end"})
	if err := t.WriteControl(endMsg); err != nil {
		return fmt.Errorf("ota: end: %w", err)
	}
	return waitFor(t.Status(), "done", doneTimeout)
}

// waitFor blocks until a status with the wanted state arrives, returning an
// error if an "error"/"aborted" status or the timeout comes first.
func waitFor(ch <-chan Status, want string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case s := <-ch:
			switch s.State {
			case want:
				return nil
			case "error":
				return fmt.Errorf("ota: device error: %s", s.Msg)
			case "aborted":
				return fmt.Errorf("ota: aborted by device")
			}
			// progress / other: keep waiting
		case <-deadline.C:
			return fmt.Errorf("ota: timed out waiting for %q", want)
		}
	}
}

// poll returns a status if one is immediately available, without blocking.
func poll(ch <-chan Status) (Status, bool) {
	select {
	case s := <-ch:
		return s, true
	default:
		return Status{}, false
	}
}

func drain(ch <-chan Status) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
