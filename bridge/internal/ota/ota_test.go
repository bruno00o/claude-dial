package ota

import (
	"encoding/json"
	"testing"
	"time"
)

// fakeTransport records writes and replies with status the way the firmware
// would: "ready" after ota_begin, endState after ota_end, and an optional
// "error" once a given number of data chunks have landed.
type fakeTransport struct {
	mtu           int
	status        chan Status
	control       [][]byte
	data          [][]byte
	suppressReady bool
	endState      string // default "done"
	errorAfter    int    // 0 = never; else emit error after this many data writes
}

func newFake(mtu int) *fakeTransport {
	return &fakeTransport{mtu: mtu, status: make(chan Status, 16), endState: "done"}
}

func (f *fakeTransport) WriteControl(b []byte) error {
	f.control = append(f.control, append([]byte(nil), b...))
	var m struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(b, &m)
	switch m.Type {
	case "ota_begin":
		if !f.suppressReady {
			f.status <- Status{State: "ready"}
		}
	case "ota_end":
		f.status <- Status{State: f.endState}
	}
	return nil
}

func (f *fakeTransport) WriteData(b []byte) error {
	f.data = append(f.data, append([]byte(nil), b...))
	if f.errorAfter > 0 && len(f.data) == f.errorAfter {
		f.status <- Status{State: "error", Msg: "write failed"}
	}
	return nil
}
func (f *fakeTransport) Status() <-chan Status { return f.status }
func (f *fakeTransport) MTU() int              { return f.mtu }

func TestFlashHappyPath(t *testing.T) {
	readyTimeout, doneTimeout = time.Second, time.Second
	f := newFake(103) // chunk = 100

	image := make([]byte, 250) // -> 3 chunks: 100,100,50
	for i := range image {
		image[i] = byte(i)
	}
	var last int
	if err := Flash(f, image, func(p int) { last = p }); err != nil {
		t.Fatalf("Flash: %v", err)
	}

	if len(f.data) != 3 {
		t.Fatalf("expected 3 data chunks, got %d", len(f.data))
	}
	if len(f.data[0]) != 100 || len(f.data[2]) != 50 {
		t.Errorf("chunk sizes wrong: %d,%d,%d", len(f.data[0]), len(f.data[1]), len(f.data[2]))
	}
	if last != 100 {
		t.Errorf("final progress = %d, want 100", last)
	}
	var begin struct {
		Type string `json:"type"`
		Size int    `json:"size"`
	}
	_ = json.Unmarshal(f.control[0], &begin)
	if begin.Type != "ota_begin" || begin.Size != 250 {
		t.Errorf("begin = %+v, want ota_begin size 250", begin)
	}
	var end struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(f.control[len(f.control)-1], &end)
	if end.Type != "ota_end" {
		t.Errorf("last control = %q, want ota_end", end.Type)
	}
}

func TestFlashDeviceErrorDuringStream(t *testing.T) {
	readyTimeout, doneTimeout = time.Second, time.Second
	f := newFake(103)
	f.errorAfter = 1 // fail right after the first chunk

	if err := Flash(f, make([]byte, 500), nil); err == nil {
		t.Fatal("expected an error when the device reports a failure")
	}
}

func TestFlashTimesOutWithoutReady(t *testing.T) {
	readyTimeout, doneTimeout = 50*time.Millisecond, time.Second
	f := newFake(103)
	f.suppressReady = true
	if err := Flash(f, make([]byte, 10), nil); err == nil {
		t.Fatal("expected a timeout waiting for ready")
	}
}

func TestFlashRejectsEmptyImage(t *testing.T) {
	if err := Flash(newFake(103), nil, nil); err == nil {
		t.Fatal("expected an error for an empty image")
	}
}
