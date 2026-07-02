package service

import (
	"strings"
	"testing"
)

func TestServeArgs(t *testing.T) {
	cases := []struct {
		name string
		o    Options
		want string
	}{
		{"defaults", Options{Port: 8787}, "serve"},
		{"ble", Options{Port: 8787, BLE: true}, "serve --ble"},
		{"custom port", Options{Port: 9000}, "serve --port 9000"},
		{"port zero omitted", Options{Port: 0}, "serve"},
		{"port and ble", Options{Port: 9000, BLE: true}, "serve --port 9000 --ble"},
	}
	for _, c := range cases {
		if got := strings.Join(ServeArgs(c.o), " "); got != c.want {
			t.Errorf("%s: ServeArgs = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPlistContainsArgsAndPaths(t *testing.T) {
	o := Options{Exec: "/usr/local/bin/claude-dial", Port: 8787, BLE: true}
	p := Plist(o, "/Users/x/Library/Logs/claude-dial.log")

	for _, want := range []string{
		"<string>" + Label + "</string>",
		"<string>/usr/local/bin/claude-dial</string>",
		"<string>serve</string>",
		"<string>--ble</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<string>/Users/x/Library/Logs/claude-dial.log</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q\n---\n%s", want, p)
		}
	}
	// The default port must not leak a --port flag into the plist.
	if strings.Contains(p, "--port") {
		t.Errorf("default port should not add --port:\n%s", p)
	}
}

func TestPlistEscapesExec(t *testing.T) {
	o := Options{Exec: "/path/with & <special>", Port: 8787}
	p := Plist(o, "/log")
	if strings.Contains(p, "& <special>") {
		t.Error("exec path must be XML-escaped in the plist")
	}
	if !strings.Contains(p, "&amp; &lt;special&gt;") {
		t.Errorf("expected escaped exec path in plist:\n%s", p)
	}
}
