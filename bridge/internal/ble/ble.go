// Package ble drives the physical M5Stack Dial over Bluetooth Low Energy.
//
// It implements daemon.Device: it scans for and connects to the Dial (a BLE
// peripheral advertising our service UUID), writes per-session state to the RX
// characteristic, and receives the user's dial answers from TX notifications.
// It reconnects automatically if the Dial drops. When no Dial is connected,
// Connected() is false and the daemon falls back to the terminal (golden rule).
//
// Requires cgo: CoreBluetooth on macOS, BlueZ on Linux, WinRT on Windows, via
// tinygo.org/x/bluetooth.
package ble

import (
	"encoding/json"
	"errors"
	"log"
	"maps"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"

	"github.com/bruno00o/claude-dial/bridge/internal/protocol"
)

var (
	errNoService = errors.New("dial service not found")
	errNoRX      = errors.New("dial RX characteristic not found")
)

// GATT UUIDs — must stay in lockstep with firmware/claude-dial (SVC/RX/TX).
const (
	svcUUID = "12345678-1234-1234-1234-123456789abc"
	rxUUID  = "12345678-1234-1234-1234-123456789abd" // host -> device (write)
	txUUID  = "12345678-1234-1234-1234-123456789abe" // device -> host (notify)

	// Keep a single session message inside a typical negotiated BLE MTU so it
	// arrives in one write. Commands are already truncated upstream; this is a
	// harder ceiling for the wire.
	bleCommandMax = 140

	retryDelay = 2 * time.Second
)

// Device is the BLE transport to the Dial.
type Device struct {
	service, rx, tx bluetooth.UUID
	debug           bool

	mu        sync.Mutex
	rxChar    bluetooth.DeviceCharacteristic
	hasRX     bool
	connected bool
	last      map[string]protocol.SessionView

	pending   chan protocol.Snapshot // coalescing hand-off to the writer goroutine
	decisions chan protocol.Decision
}

// New starts the BLE device. It returns immediately: enabling the adapter
// (which on macOS can block on a Bluetooth-permission prompt) and all scanning
// happen in the background, so BLE never delays or blocks the bridge starting.
// Until a Dial is found, Connected() is false and the daemon uses the terminal.
func New(debug bool) (*Device, error) {
	svc, err := bluetooth.ParseUUID(svcUUID)
	if err != nil {
		return nil, err
	}
	rx, err := bluetooth.ParseUUID(rxUUID)
	if err != nil {
		return nil, err
	}
	tx, err := bluetooth.ParseUUID(txUUID)
	if err != nil {
		return nil, err
	}
	d := &Device{
		service:   svc,
		rx:        rx,
		tx:        tx,
		debug:     debug,
		last:      map[string]protocol.SessionView{},
		pending:   make(chan protocol.Snapshot, 1),
		decisions: make(chan protocol.Decision, 32),
	}
	go d.run()
	go d.writer()
	return d, nil
}

// run enables the adapter, then keeps a connection to the Dial alive,
// reconnecting on drop. Enabling happens here (not in New) so a permission
// prompt can never block the bridge from starting.
func (d *Device) run() {
	if err := bluetooth.DefaultAdapter.Enable(); err != nil {
		d.logf("enable adapter: %v (BLE disabled for this run)", err)
		return
	}
	adapter := bluetooth.DefaultAdapter
	for {
		addr, ok := d.scan(adapter)
		if !ok {
			time.Sleep(retryDelay)
			continue
		}
		device, err := adapter.Connect(addr, bluetooth.ConnectionParams{})
		if err != nil {
			d.logf("connect: %v", err)
			time.Sleep(retryDelay)
			continue
		}

		disconnected := make(chan struct{})
		var once sync.Once
		adapter.SetConnectHandler(func(_ bluetooth.Device, connected bool) {
			if !connected {
				once.Do(func() { close(disconnected) })
			}
		})

		if err := d.setup(device); err != nil {
			d.logf("setup: %v", err)
			_ = device.Disconnect()
			time.Sleep(retryDelay)
			continue
		}

		d.setConnected(true)
		d.sendTime()
		d.logf("connected to dial")

		<-disconnected
		d.setConnected(false)
		d.logf("dial disconnected; rescanning")
	}
}

// scan returns the address of the first peripheral advertising our service.
func (d *Device) scan(adapter *bluetooth.Adapter) (bluetooth.Address, bool) {
	var addr bluetooth.Address
	found := false
	err := adapter.Scan(func(a *bluetooth.Adapter, res bluetooth.ScanResult) {
		if res.HasServiceUUID(d.service) {
			addr = res.Address
			found = true
			_ = a.StopScan()
		}
	})
	if err != nil {
		d.logf("scan: %v", err)
	}
	return addr, found
}

// setup discovers the RX/TX characteristics and subscribes to notifications.
func (d *Device) setup(device bluetooth.Device) error {
	svcs, err := device.DiscoverServices([]bluetooth.UUID{d.service})
	if err != nil {
		return err
	}
	if len(svcs) == 0 {
		return errNoService
	}
	chars, err := svcs[0].DiscoverCharacteristics([]bluetooth.UUID{d.rx, d.tx})
	if err != nil {
		return err
	}

	d.mu.Lock()
	d.hasRX = false
	d.last = map[string]protocol.SessionView{} // force a full resend after (re)connect
	for _, c := range chars {
		switch c.UUID() {
		case d.rx:
			d.rxChar = c
			d.hasRX = true
		case d.tx:
			if err := c.EnableNotifications(d.onNotify); err != nil {
				d.logf("subscribe TX: %v", err)
			}
		}
	}
	ok := d.hasRX
	d.mu.Unlock()

	if !ok {
		return errNoRX
	}
	return nil
}

// onNotify handles a decision coming back from the dial.
func (d *Device) onNotify(buf []byte) {
	var dec protocol.Decision
	if err := json.Unmarshal(buf, &dec); err != nil || dec.SessionID == "" {
		return
	}
	select {
	case d.decisions <- dec:
	default:
	}
}

// Update hands the latest snapshot to the writer goroutine. Implements
// daemon.Device. It never blocks: a slow BLE link must not stall the caller,
// which is often a Claude Code hook waiting on the daemon's HTTP response
// (otherwise the hook times out). Only the newest snapshot is kept — older
// pending ones are dropped, since each snapshot is the full current state.
func (d *Device) Update(snap protocol.Snapshot) {
	for {
		select {
		case d.pending <- snap:
			return
		case <-d.pending: // buffer full: drop the stale snapshot and retry
		}
	}
}

// writer serializes all BLE writes off the caller's goroutine.
func (d *Device) writer() {
	for snap := range d.pending {
		d.flush(snap)
	}
}

// flush diffs the snapshot against what the Dial last confirmed and writes only
// the changes. It runs solely on the writer goroutine. Crucially, d.last records
// only what actually landed: a session is marked delivered *after* a successful
// write, so a dropped update (e.g. a full TX buffer) is re-attempted on the next
// broadcast instead of being silently assumed sent — which would freeze the
// Dial on a stale state until that session changed again.
func (d *Device) flush(snap protocol.Snapshot) {
	d.mu.Lock()
	if !d.connected {
		d.mu.Unlock()
		return
	}
	prev := maps.Clone(d.last)
	d.mu.Unlock()

	cur := make(map[string]protocol.SessionView, len(snap.Sessions))
	for _, s := range snap.Sessions {
		s.Command = truncate(s.Command, bleCommandMax)
		cur[s.SessionID] = s
	}

	// Sessions that vanished -> tell the Dial to drop them.
	for id := range prev {
		if _, ok := cur[id]; !ok {
			if d.write(protocol.Outbound{SessionID: id, State: protocol.StateClosed}) {
				d.mu.Lock()
				delete(d.last, id)
				d.mu.Unlock()
			}
		}
	}
	// New or changed sessions -> push them; record only on success.
	for _, s := range cur {
		if p, ok := prev[s.SessionID]; !ok || p != s {
			if d.write(protocol.Outbound{
				SessionID: s.SessionID,
				Project:   s.Project,
				State:     s.State,
				ToolName:  s.ToolName,
				Command:   s.Command,
			}) {
				d.mu.Lock()
				d.last[s.SessionID] = s
				d.mu.Unlock()
			}
		}
	}
}

// Connected reports whether a Dial is currently connected. Implements
// daemon.Device.
func (d *Device) Connected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connected
}

// Decisions streams dial answers. Implements daemon.Device.
func (d *Device) Decisions() <-chan protocol.Decision {
	return d.decisions
}

func (d *Device) sendTime() {
	now := time.Now()
	_, offset := now.Zone()
	d.write(protocol.Outbound{Type: "set_time", Epoch: now.Unix(), TZOffset: offset})
}

// write pushes one message to the Dial, returning whether it landed. The BLE TX
// buffer fills under bursts ("timed out waiting for buffer space"); that error
// is transient, so we retry briefly. This runs on the writer goroutine, never a
// hook handler, so the short sleeps are harmless.
func (d *Device) write(msg protocol.Outbound) bool {
	d.mu.Lock()
	rx, ok := d.rxChar, d.hasRX
	d.mu.Unlock()
	if !ok {
		return false
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	for attempt := 0; attempt < 6; attempt++ {
		if _, err = rx.WriteWithoutResponse(payload); err == nil {
			return true
		}
		d.logf("write (try %d): %v", attempt+1, err)
		time.Sleep(40 * time.Millisecond)
	}
	return false
}

func (d *Device) setConnected(v bool) {
	d.mu.Lock()
	d.connected = v
	if !v {
		d.hasRX = false
		d.last = map[string]protocol.SessionView{}
	}
	d.mu.Unlock()
}

func (d *Device) logf(format string, args ...any) {
	if d.debug {
		log.Printf("ble: "+format, args...)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
