package daemon

import "testing"

// The bridge must never flash firmware newer than itself: equal or older
// firmware is fine, newer is blocked. An empty bridge version disables the gate,
// and an empty firmware version (no manifest) is permissive.
func TestBridgeCanFlash(t *testing.T) {
	cases := []struct {
		bridge, fw string
		want       bool
	}{
		{"0.15.0", "0.15.0", true},  // equal → ok
		{"0.20.0", "0.15.0", true},  // bridge newer → ok
		{"0.15.0", "0.20.0", false}, // firmware minor ahead → blocked
		{"0.15.0", "0.15.1", false}, // firmware patch ahead → blocked
		{"", "0.20.0", true},        // gate disabled
		{"0.15.0", "", true},        // no manifest yet → permissive
	}
	for _, c := range cases {
		d := &Daemon{bridgeVersion: c.bridge}
		if got := d.bridgeCanFlash(c.fw); got != c.want {
			t.Errorf("bridgeCanFlash(bridge=%q, fw=%q) = %v, want %v", c.bridge, c.fw, got, c.want)
		}
	}
}
