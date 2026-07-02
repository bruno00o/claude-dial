package firmware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		running, available string
		want               bool
	}{
		{"0.4.0", "0.5.0", true},
		{"0.4.0", "0.4.1", true},
		{"0.4.0", "1.0.0", true},
		{"0.4.0", "0.4.0", false},  // equal
		{"0.5.0", "0.4.0", false},  // older
		{"0.4", "0.4.0", false},    // 0.4 == 0.4.0
		{"v0.4.0", "v0.5.0", true}, // v-prefixed
		{"0.4.0", "0.10.0", true},  // numeric, not lexical (10 > 4)
		{"0.10.0", "0.9.0", false}, // 10 > 9
		{"", "0.5.0", false},       // unknown running -> never advertise
		{"0.4.0", "", false},       // unknown latest -> never advertise
		{"", "", false},
	}
	for _, c := range cases {
		if got := Newer(c.running, c.available); got != c.want {
			t.Errorf("Newer(%q,%q) = %v, want %v", c.running, c.available, got, c.want)
		}
	}
}

func TestRefreshParsesManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"0.5.0","url":"http://x/fw.bin","sha256":"abc"}`))
	}))
	defer srv.Close()

	c := NewChecker(srv.URL)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := c.Latest().Version; got != "0.5.0" {
		t.Errorf("Latest().Version = %q, want 0.5.0", got)
	}
}

func TestRefreshMissingManifestKeepsCacheAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewChecker(srv.URL)
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected an error on HTTP 404")
	}
	// A failed refresh must leave a usable (empty) manifest, not panic.
	if c.Latest().Version != "" {
		t.Errorf("expected empty version after failed refresh, got %q", c.Latest().Version)
	}
}
