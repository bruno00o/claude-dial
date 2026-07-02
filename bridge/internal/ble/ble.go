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

	"github.com/bruno00o/claude-dial/bridge/internal/ota"
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

	// OTA characteristics (see firmware): control (JSON), data (raw), status (notify).
	otaCtrlUUID   = "12345678-1234-1234-1234-123456789ac0"
	otaDataUUID   = "12345678-1234-1234-1234-123456789ac1"
	otaStatusUUID = "12345678-1234-1234-1234-123456789ac2"

	// Keep a single session message inside a typical negotiated BLE MTU so it
	// arrives in one write. Commands are already truncated upstream; this is a
	// harder ceiling for the wire.
	bleCommandMax = 140

	retryDelay = 2 * time.Second
)

// Device is the BLE transport to the Dial.
type Device struct {
	service, rx, tx              bluetooth.UUID
	otaCtrlU, otaDataU, otaStatU bluetooth.UUID
	debug                        bool

	mu         sync.Mutex
	rxChar     bluetooth.DeviceCharacteristic
	hasRX      bool
	connected  bool
	conn       bluetooth.Device // current connection, for forcing a reconnect
	hasConn    bool
	firmware   string // version the Dial announced on connect ("" until it does)
	otaCapable bool   // firmware advertised OTA support in its hello
	otaCtrl    bluetooth.DeviceCharacteristic
	otaData    bluetooth.DeviceCharacteristic
	hasOTA     bool // all OTA characteristics were discovered
	last       map[string]protocol.SessionView

	pending   chan protocol.Snapshot // coalescing hand-off to the writer goroutine
	decisions chan protocol.Decision
	otaStatus chan ota.Status // parsed ota_status notifications
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
	otaCtrl, err := bluetooth.ParseUUID(otaCtrlUUID)
	if err != nil {
		return nil, err
	}
	otaData, err := bluetooth.ParseUUID(otaDataUUID)
	if err != nil {
		return nil, err
	}
	otaStat, err := bluetooth.ParseUUID(otaStatusUUID)
	if err != nil {
		return nil, err
	}
	d := &Device{
		service:   svc,
		rx:        rx,
		tx:        tx,
		otaCtrlU:  otaCtrl,
		otaDataU:  otaData,
		otaStatU:  otaStat,
		debug:     debug,
		last:      map[string]protocol.SessionView{},
		pending:   make(chan protocol.Snapshot, 1),
		decisions: make(chan protocol.Decision, 32),
		otaStatus: make(chan ota.Status, 16),
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

		d.mu.Lock()
		d.conn, d.hasConn = device, true
		d.mu.Unlock()

		d.setConnected(true)
		d.sendTime()
		d.logf("connected to dial")

		<-disconnected
		d.setConnected(false)
		d.mu.Lock()
		d.hasConn = false
		d.mu.Unlock()
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
	chars, err := svcs[0].DiscoverCharacteristics(
		[]bluetooth.UUID{d.rx, d.tx, d.otaCtrlU, d.otaDataU, d.otaStatU})
	if err != nil {
		return err
	}

	d.mu.Lock()
	d.hasRX = false
	var gotCtrl, gotData, gotStat bool
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
		case d.otaCtrlU:
			d.otaCtrl, gotCtrl = c, true
		case d.otaDataU:
			d.otaData, gotData = c, true
		case d.otaStatU:
			gotStat = true
			if err := c.EnableNotifications(d.onOtaStatus); err != nil {
				d.logf("subscribe OTA status: %v", err)
			}
		}
	}
	d.hasOTA = gotCtrl && gotData && gotStat
	ok := d.hasRX
	d.mu.Unlock()

	if !ok {
		return errNoRX
	}
	return nil
}

// onNotify handles a message coming back from the dial: either a hello (the
// firmware version, sent on connect) or a decision (the user's dial answer).
func (d *Device) onNotify(buf []byte) {
	var hello protocol.DeviceHello
	if json.Unmarshal(buf, &hello) == nil && hello.Type == "hello" {
		d.mu.Lock()
		d.firmware = hello.Firmware
		d.otaCapable = hello.OTA
		d.mu.Unlock()
		d.logf("dial firmware %s (ota=%v)", hello.Firmware, hello.OTA)
		return
	}
	var dec protocol.Decision
	if err := json.Unmarshal(buf, &dec); err != nil || dec.SessionID == "" {
		return
	}
	select {
	case d.decisions <- dec:
	default:
	}
}

// FirmwareVersion returns the version the connected Dial announced, or "" if no
// Dial is connected or it hasn't announced one yet. Implements daemon.Device.
func (d *Device) FirmwareVersion() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.firmware
}

// onOtaStatus parses an ota_status notification and forwards it to the flasher.
func (d *Device) onOtaStatus(buf []byte) {
	var s ota.Status
	if json.Unmarshal(buf, &s) != nil || s.State == "" {
		return
	}
	select {
	case d.otaStatus <- s:
	default: // flasher is slow / not running: drop (it re-reads terminal states)
	}
}

// OTACapable reports whether a connected Dial can take a BLE firmware update.
func (d *Device) OTACapable() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connected && d.hasOTA
}

// Flash streams a firmware image to the Dial (see internal/ota). It blocks until
// the Dial verifies and reboots, or an error/timeout occurs.
func (d *Device) Flash(image []byte, onProgress func(pct int)) error {
	if !d.OTACapable() {
		return errors.New("dial not connected or not OTA-capable")
	}
	return ota.Flash(d, image, onProgress)
}

// WriteControl, WriteData, Status and MTU implement ota.Transport. Writes use
// Write (with response) for the same reason as the session writer: macOS
// WriteWithoutResponse is unreliable, and the ATT ack gives natural back-pressure.
func (d *Device) WriteControl(b []byte) error {
	d.mu.Lock()
	c, ok := d.otaCtrl, d.hasOTA
	d.mu.Unlock()
	if !ok {
		return errors.New("ota control characteristic unavailable")
	}
	_, err := c.Write(b)
	return err
}

func (d *Device) WriteData(b []byte) error {
	d.mu.Lock()
	c, ok := d.otaData, d.hasOTA
	d.mu.Unlock()
	if !ok {
		return errors.New("ota data characteristic unavailable")
	}
	_, err := c.Write(b)
	return err
}

func (d *Device) Status() <-chan ota.Status { return d.otaStatus }

// MTU returns the negotiated ATT MTU so the flasher can size chunks; it falls
// back to the 23-byte BLE minimum if the platform can't report it.
func (d *Device) MTU() int {
	d.mu.Lock()
	c, ok := d.otaData, d.hasOTA
	d.mu.Unlock()
	if ok {
		if m, err := c.GetMTU(); err == nil && m >= 23 {
			return int(m)
		}
	}
	return 23
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

// writer serializes all BLE writes off the caller's goroutine. A with-response
// write that keeps timing out means the ATT ack never comes — the link stalled —
// so it forces a reconnect to clear it rather than leaving the Dial frozen on a
// stale view.
func (d *Device) writer() {
	fails := 0
	var lastReset time.Time
	for snap := range d.pending {
		if d.flush(snap) {
			fails = 0
			continue
		}
		// One reconnect can clear a link whose TX buffer wedged mid-session. But
		// if a *fresh* connection also fails to write (a born-dead link — the
		// macOS BLE stack itself is wedged), reconnecting in a tight loop only
		// churns it further. So rate-limit forced reconnects: try one, then back
		// off and keep failing quietly (the daemon still works — golden rule)
		// until the link recovers or the device is power-cycled.
		if fails++; fails >= 2 {
			if time.Since(lastReset) > 45*time.Second {
				d.logf("BLE writes wedged; forcing a reconnect")
				d.resetLink()
				lastReset = time.Now()
			}
			fails = 0
		}
	}
}

// resetLink drops the current connection so run() rescans and reconnects with a
// fresh, uncongested link. Safe to call from the writer goroutine.
func (d *Device) resetLink() {
	d.mu.Lock()
	c, ok := d.conn, d.hasConn
	d.mu.Unlock()
	if ok {
		_ = c.Disconnect()
	}
}

// flush diffs the snapshot against what the Dial last confirmed and writes only
// the changes, returning whether every write landed. It runs solely on the
// writer goroutine. Crucially, d.last records only what actually landed: a
// session is marked delivered *after* a successful write, so a dropped update
// (e.g. a full TX buffer) is re-attempted on the next broadcast instead of being
// silently assumed sent — which would freeze the Dial on a stale state.
func (d *Device) flush(snap protocol.Snapshot) bool {
	d.mu.Lock()
	if !d.connected {
		d.mu.Unlock()
		return true // run() owns reconnect while down; not a write failure
	}
	prev := maps.Clone(d.last)
	d.mu.Unlock()

	cur := make(map[string]protocol.SessionView, len(snap.Sessions))
	for _, s := range snap.Sessions {
		s.Command = truncate(s.Command, bleCommandMax)
		cur[s.SessionID] = s
	}

	ok := true
	// Sessions that vanished -> tell the Dial to drop them.
	for id := range prev {
		if _, exists := cur[id]; !exists {
			if d.write(protocol.Outbound{SessionID: id, State: protocol.StateClosed}) {
				d.mu.Lock()
				delete(d.last, id)
				d.mu.Unlock()
			} else {
				ok = false
			}
		}
	}
	// New or changed sessions -> push them; record only on success. Only writes
	// when what the Dial *shows* changes (see displayEqual), so per-tool-call
	// command churn across busy sessions doesn't flood the slow BLE link.
	for _, s := range cur {
		if p, exists := prev[s.SessionID]; !exists || !displayEqual(p, s) {
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
			} else {
				ok = false
			}
		}
	}
	return ok
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

// displayEqual reports whether two views render identically on the Dial. The
// roster shows only project + state; a tool/command is shown only in a permission
// takeover. So a "working" session whose command changes on every tool call looks
// no different on screen — and must not cost a (slow, congestion-prone) BLE write.
func displayEqual(a, b protocol.SessionView) bool {
	if a.Project != b.Project || a.State != b.State {
		return false
	}
	if a.State == protocol.StatePermission {
		return a.ToolName == b.ToolName && a.Command == b.Command
	}
	return true
}

// write pushes one message to the Dial, returning whether it landed. It uses
// Write (with response), not WriteWithoutResponse, on purpose: on macOS
// CoreBluetooth's CanSendWriteWithoutResponse flag is unreliable — the
// peripheralIsReadyToSendWriteWithoutResponse callback that clears it can get
// stuck false forever (a long-standing Apple bug), so tinygo's
// WriteWithoutResponse spins on that flag and fails with "timed out waiting for
// buffer space", wedging the link until the whole CBCentralManager is torn down.
// A burst of writes was enough to trip it. Write instead waits on the ATT write
// response — a reliable core-protocol ack — and that per-write ack gives natural
// back-pressure that paces us to the link and makes a buffer overflow impossible.
// Throughput is irrelevant here: updates are small, infrequent, and coalesced.
// A dropped update is re-sent on the next broadcast (flush records delivery only
// on success), and a persistently wedged link is reconnected by the writer.
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
	if _, err = rx.Write(payload); err != nil {
		d.logf("write: %v", err)
		return false
	}
	return true
}

func (d *Device) setConnected(v bool) {
	d.mu.Lock()
	d.connected = v
	if !v {
		d.hasRX = false
		d.hasOTA = false
		d.firmware = ""
		d.otaCapable = false
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
