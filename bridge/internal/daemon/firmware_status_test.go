package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bruno00o/claude-dial/bridge/internal/protocol"
	"github.com/bruno00o/claude-dial/bridge/internal/session"
)

// stubDevice is a Device that reports a fixed firmware version and nothing else.
type stubDevice struct{ fw string }

func (s stubDevice) Update(protocol.Snapshot)            {}
func (s stubDevice) Connected() bool                     { return true }
func (s stubDevice) Decisions() <-chan protocol.Decision { return nil }
func (s stubDevice) FirmwareVersion() string             { return s.fw }

func TestStatusReportsFirmwareUpdate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"0.5.0","url":"http://x/fw.bin","sha256":"abc"}`))
	}))
	defer srv.Close()

	d := New(session.New(), stubDevice{fw: "0.4.0"}, Config{FirmwareManifestURL: srv.URL})
	if err := d.fw.Refresh(context.Background()); err != nil { // deterministic: fetch now
		t.Fatalf("refresh: %v", err)
	}

	w := httptest.NewRecorder()
	d.handleStatus(w, httptest.NewRequest(http.MethodGet, "/status", nil))

	var got struct {
		Firmware struct {
			Running         string `json:"running"`
			Latest          string `json:"latest"`
			UpdateAvailable bool   `json:"update_available"`
		} `json:"firmware"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if got.Firmware.Running != "0.4.0" || got.Firmware.Latest != "0.5.0" || !got.Firmware.UpdateAvailable {
		t.Errorf("firmware status = %+v, want running 0.4.0 / latest 0.5.0 / update true", got.Firmware)
	}
}

func TestStatusNoUpdateWhenDeviceSilent(t *testing.T) {
	// No firmware reported (old firmware or no Dial) -> never advertise an update.
	d := New(session.New(), stubDevice{fw: ""}, Config{FirmwareManifestURL: "http://127.0.0.1:0/none"})

	w := httptest.NewRecorder()
	d.handleStatus(w, httptest.NewRequest(http.MethodGet, "/status", nil))

	var got struct {
		Firmware struct {
			UpdateAvailable bool `json:"update_available"`
		} `json:"firmware"`
	}
	_ = json.NewDecoder(w.Body).Decode(&got)
	if got.Firmware.UpdateAvailable {
		t.Error("update_available should be false when the device reports no version")
	}
}
