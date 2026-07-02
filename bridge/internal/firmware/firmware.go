// Package firmware tells the daemon what the latest published Dial firmware is,
// so it can flag when the connected Dial is out of date (phase 1 of OTA).
//
// The "latest" version comes from a small JSON manifest attached to the GitHub
// release by CI (phase 3). Every failure to read it — offline, no manifest
// published yet, malformed — degrades to "unknown"; the daemon then simply
// doesn't advertise an update. This is a convenience signal, never a gate, and
// it can't affect Claude Code (golden rule).
package firmware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultManifestURL is the stable "latest release" asset URL. GitHub redirects
// it to the newest release's firmware.json; CI publishes that asset per release.
const DefaultManifestURL = "https://github.com/bruno00o/claude-dial/releases/latest/download/firmware.json"

// Manifest describes the latest published firmware image.
type Manifest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

// Checker caches the latest manifest, refreshed periodically by Run. Reads are
// cheap and lock-free-ish (a single mutex), so /status can call Latest freely.
type Checker struct {
	url    string
	client *http.Client

	mu     sync.Mutex
	latest Manifest
}

// NewChecker returns a Checker for the given manifest URL ("" uses the default).
func NewChecker(url string) *Checker {
	if url == "" {
		url = DefaultManifestURL
	}
	return &Checker{url: url, client: &http.Client{Timeout: 10 * time.Second}}
}

// Latest returns the most recently fetched manifest (zero value until the first
// successful refresh).
func (c *Checker) Latest() Manifest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest
}

// Refresh fetches the manifest once. A failure leaves the cached value intact
// and returns the error (callers may log it at most); it never panics.
func (c *Checker) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &httpError{resp.StatusCode}
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&m); err != nil {
		return err
	}
	c.mu.Lock()
	c.latest = m
	c.mu.Unlock()
	return nil
}

// Run refreshes immediately, then every interval until ctx is cancelled. Errors
// are ignored here (the cached value simply stays stale) — the daemon logs
// transitions separately.
func (c *Checker) Run(ctx context.Context, interval time.Duration) {
	_ = c.Refresh(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.Refresh(ctx)
		}
	}
}

type httpError struct{ code int }

func (e *httpError) Error() string { return "firmware manifest: HTTP " + strconv.Itoa(e.code) }

// DownloadLatest fetches the latest firmware image and verifies its sha256
// against the manifest. Returns the image bytes and the manifest it came from.
func (c *Checker) DownloadLatest(ctx context.Context) ([]byte, Manifest, error) {
	m := c.Latest()
	if m.URL == "" {
		return nil, m, fmt.Errorf("no firmware manifest available")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return nil, m, err
	}
	client := &http.Client{Timeout: 2 * time.Minute} // images are ~1 MB over BLE-less HTTP
	resp, err := client.Do(req)
	if err != nil {
		return nil, m, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, m, &httpError{resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) // 16 MB cap
	if err != nil {
		return nil, m, err
	}
	if m.SHA256 != "" {
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != m.SHA256 {
			return nil, m, fmt.Errorf("firmware sha256 mismatch: manifest %s, got %s", m.SHA256, got)
		}
	}
	return data, m, nil
}

// Newer reports whether available is a strictly newer version than running.
// Both are dotted numeric versions ("0.4.0"), optionally "v"-prefixed. If either
// is empty or unparseable the answer is false — we never advertise an update we
// can't be sure about.
func Newer(running, available string) bool {
	if running == "" || available == "" {
		return false
	}
	return compare(available, running) > 0
}

// compare returns >0 if a>b, <0 if a<b, 0 if equal, comparing dotted numeric
// fields left to right (missing fields count as 0, so "0.4" == "0.4.0").
func compare(a, b string) int {
	as := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bs := strings.Split(strings.TrimPrefix(b, "v"), ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		if d := field(as, i) - field(bs, i); d != 0 {
			return d
		}
	}
	return 0
}

// field parses the i-th dotted component as an int (0 if absent or non-numeric).
func field(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, _ := strconv.Atoi(parts[i])
	return n
}
