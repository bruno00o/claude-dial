package daemon

import (
	"errors"

	"github.com/bruno00o/claude-dial/bridge/internal/protocol"
)

// fanout drives several Devices as one: state goes to all of them, decisions
// are merged. Used to run the web simulator and the physical BLE Dial together.
type fanout struct {
	devices   []Device
	decisions chan protocol.Decision
}

// NewFanout returns a Device that mirrors updates to every device and merges
// their decisions. Connected() is true if any device is connected.
func NewFanout(devices ...Device) Device {
	f := &fanout{
		devices:   devices,
		decisions: make(chan protocol.Decision, 32),
	}
	for _, dev := range devices {
		go func(dev Device) {
			for dec := range dev.Decisions() {
				select {
				case f.decisions <- dec:
				default:
				}
			}
		}(dev)
	}
	return f
}

func (f *fanout) Update(s protocol.Snapshot) {
	for _, d := range f.devices {
		d.Update(s)
	}
}

func (f *fanout) Connected() bool {
	for _, d := range f.devices {
		if d.Connected() {
			return true
		}
	}
	return false
}

func (f *fanout) Decisions() <-chan protocol.Decision {
	return f.decisions
}

// FirmwareVersion returns the first non-empty version among the devices — in
// practice the BLE Dial's, since the simulator has none.
func (f *fanout) FirmwareVersion() string {
	for _, d := range f.devices {
		if v := d.FirmwareVersion(); v != "" {
			return v
		}
	}
	return ""
}

// OTACapable / Flash delegate to the first OTA-capable device (the BLE Dial).
func (f *fanout) OTACapable() bool {
	for _, d := range f.devices {
		if fl, ok := d.(Flasher); ok && fl.OTACapable() {
			return true
		}
	}
	return false
}

func (f *fanout) Flash(image []byte, version string, onProgress func(pct int)) error {
	for _, d := range f.devices {
		if fl, ok := d.(Flasher); ok && fl.OTACapable() {
			return fl.Flash(image, version, onProgress)
		}
	}
	return errors.New("no OTA-capable device connected")
}

// SetUpdateAvailable forwards to every Flasher (there's only the Dial in
// practice); each dedups its own writes.
func (f *fanout) SetUpdateAvailable(version string) {
	for _, d := range f.devices {
		if fl, ok := d.(Flasher); ok {
			fl.SetUpdateAvailable(version)
		}
	}
}

// OTARequests returns the first Flasher's request stream (the Dial's). If none
// implements it, a nil channel that never fires.
func (f *fanout) OTARequests() <-chan struct{} {
	for _, d := range f.devices {
		if fl, ok := d.(Flasher); ok {
			return fl.OTARequests()
		}
	}
	return nil
}
