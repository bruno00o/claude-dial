package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTranscript writes a .jsonl transcript whose assistant lines carry the
// given work-token totals at the given offsets before `now`. cache_read is set
// nonzero to prove it's excluded from the gauge.
func writeTranscript(t *testing.T, dir, name string, now time.Time, entries []struct {
	ago time.Duration
	tok int64
}) {
	t.Helper()
	var buf []byte
	for _, e := range entries {
		line, _ := json.Marshal(map[string]any{
			"type":      "assistant",
			"timestamp": now.Add(-e.ago).UTC().Format(time.RFC3339Nano),
			"message": map[string]any{
				"usage": map[string]any{
					"input_tokens":                e.tok, // whole total goes to input for simplicity
					"output_tokens":               0,
					"cache_creation_input_tokens": 0,
					"cache_read_input_tokens":     9_999_999, // must be ignored
				},
			},
		})
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentWindowAndExplicitBudget(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	writeTranscript(t, dir, "a.jsonl", now, []struct {
		ago time.Duration
		tok int64
	}{
		{1 * time.Hour, 160},      // in window
		{2 * time.Hour, 160},      // in window
		{6 * time.Hour, 500},      // outside the 5h window (older)
		{8 * 24 * time.Hour, 999}, // outside the 7-day history
	})

	r := NewReader(dir, 1000) // explicit budget
	if err := r.Refresh(now); err != nil {
		t.Fatal(err)
	}
	s := r.Latest()
	if s.Tokens != 320 {
		t.Errorf("Tokens = %d, want 320 (two in-window events; cache_read ignored)", s.Tokens)
	}
	if s.Budget != 1000 || s.Pct() != 32 {
		t.Errorf("Budget/Pct = %d/%d, want 1000/32", s.Budget, s.Pct())
	}
}

func TestSelfCalibratedBudget(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	writeTranscript(t, dir, "a.jsonl", now, []struct {
		ago time.Duration
		tok int64
	}{
		{1 * time.Hour, 600_000}, // current window: 1.2M total
		{2 * time.Hour, 600_000},
		{10 * time.Hour, 600_000}, // older cluster within a 5h window: 1.8M peak
		{10*time.Hour + time.Minute, 600_000},
		{10*time.Hour + 2*time.Minute, 600_000},
	})

	r := NewReader(dir, 0) // self-calibrate
	if err := r.Refresh(now); err != nil {
		t.Fatal(err)
	}
	s := r.Latest()
	if s.Tokens != 1_200_000 {
		t.Errorf("Tokens = %d, want 1_200_000", s.Tokens)
	}
	if s.Budget != 1_800_000 {
		t.Errorf("Budget = %d, want 1_800_000 (heaviest 5h)", s.Budget)
	}
	if s.Pct() != 67 {
		t.Errorf("Pct = %d, want 67", s.Pct())
	}
}

func TestNoTranscriptsIsZero(t *testing.T) {
	r := NewReader(filepath.Join(t.TempDir(), "does-not-exist"), 0)
	if err := r.Refresh(time.Now()); err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if s := r.Latest(); s.Tokens != 0 || s.Pct() != 0 {
		t.Errorf("want zero usage, got %+v", s)
	}
}
