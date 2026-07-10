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

// writeLines writes raw transcript lines (arbitrary JSON objects) verbatim.
func writeLines(t *testing.T, dir, name string, lines []map[string]any) {
	t.Helper()
	var buf []byte
	for _, m := range lines {
		b, _ := json.Marshal(m)
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func asstLine(ts time.Time, sidechain bool, in, out, cc, cr int64) map[string]any {
	return map[string]any{
		"type":        "assistant",
		"isSidechain": sidechain,
		"timestamp":   ts.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{
			"role": "assistant",
			"usage": map[string]any{
				"input_tokens":                in,
				"output_tokens":               out,
				"cache_creation_input_tokens": cc,
				"cache_read_input_tokens":     cr,
			},
		},
	}
}

// TestPerSessionUsage pins the two opposite token rules the debate flagged:
//   - Total (cumulative spend) = work tokens (input+output+cache_creation), across
//     ALL turns including sub-agent (sidechain) turns; cache_read excluded.
//   - Context (resident window) = the LAST MAIN-THREAD assistant turn's
//     input+cache_read+cache_creation; cache_read INCLUDED, output excluded, and
//     sidechain turns skipped so a sub-agent's context never lands on the parent.
func TestPerSessionUsage(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	writeLines(t, dir, "sess-1.jsonl", []map[string]any{
		{"type": "user", "timestamp": now.Add(-30 * time.Minute).UTC().Format(time.RFC3339Nano)}, // no usage
		asstLine(now.Add(-20*time.Minute), false, 100, 50, 10, 1000),                             // work 160, ctx 1110
		asstLine(now.Add(-10*time.Minute), true, 5, 5, 0, 7000),                                  // sidechain: work 10 → Total; ctx ignored
		asstLine(now.Add(-5*time.Minute), false, 200, 20, 30, 5000),                              // last main-thread: work 250, ctx 5230
	})
	writeLines(t, dir, "sess-2.jsonl", []map[string]any{
		asstLine(now.Add(-1*time.Minute), false, 1, 1, 1, 9), // work 3, ctx 11
	})

	r := NewReader(dir, 0)
	if err := r.Refresh(now); err != nil {
		t.Fatal(err)
	}
	per := r.PerSession()

	s1, ok := per["sess-1"] // key is the filename minus .jsonl == the session id
	if !ok {
		t.Fatal("sess-1 missing from PerSession")
	}
	if s1.Total != 420 {
		t.Errorf("sess-1 Total = %d, want 420 (160 + 10 sidechain + 250; cache_read excluded)", s1.Total)
	}
	if s1.Context != 5230 {
		t.Errorf("sess-1 Context = %d, want 5230 (last main-thread 200+5000+30; cache_read INCLUDED, output & sidechain excluded)", s1.Context)
	}
	if s2 := per["sess-2"]; s2.Total != 3 || s2.Context != 11 {
		t.Errorf("sess-2 = %+v, want {Total:3 Context:11}", s2)
	}
}

func TestPerSessionCost(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	// One Sonnet turn ($3/$15 per 1M): input 1000, output 2000, cache_create 400,
	// cache_read 10000. billedInput = 1000 + 400*1.25 + 10000*0.1 = 2500.
	// cost = (2500*3 + 2000*15)/1e6 = (7500 + 30000)/1e6 = 0.0375
	writeLines(t, dir, "sess.jsonl", []map[string]any{{
		"type": "assistant", "timestamp": now.Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		"message": map[string]any{"role": "assistant", "model": "claude-sonnet-4-6",
			"usage": map[string]any{"input_tokens": 1000, "output_tokens": 2000,
				"cache_creation_input_tokens": 400, "cache_read_input_tokens": 10000}},
	}})
	r := NewReader(dir, 0)
	if err := r.Refresh(now); err != nil {
		t.Fatal(err)
	}
	got := r.PerSession()["sess"].Cost
	if want := 0.0375; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("Cost = %v, want %v (Sonnet pricing with cache multipliers)", got, want)
	}
}

func TestPerSessionEmptyBeforeRefresh(t *testing.T) {
	if r := NewReader(t.TempDir(), 0); len(r.PerSession()) != 0 {
		t.Errorf("PerSession should be empty before the first Refresh")
	}
}

// asstWithContent is an assistant line whose message.content carries the given
// blocks (e.g. Task tool_use), so we can exercise sub-agent counting.
func asstWithContent(ts time.Time, blocks []map[string]any) map[string]any {
	return map[string]any{
		"type": "assistant", "isSidechain": false,
		"timestamp": ts.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{
			"role": "assistant", "content": blocks,
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
		},
	}
}

func TestSubAgentCount(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	// Parent conversation: one assistant turn worth 100 work tokens.
	writeLines(t, dir, "s.jsonl", []map[string]any{
		{"type": "user", "timestamp": now.Add(-9 * time.Minute).UTC().Format(time.RFC3339Nano),
			"message": map[string]any{"role": "user", "content": "go do the thing"}},
		{"type": "assistant", "timestamp": now.Add(-8 * time.Minute).UTC().Format(time.RFC3339Nano),
			"message": map[string]any{"role": "assistant", "model": "claude-sonnet-4-6",
				"usage": map[string]any{"input_tokens": 100, "output_tokens": 0}}},
	})
	// Two sub-agents, each in its own agent-*.jsonl tagged with the parent
	// sessionId — whatever spawned them (Task / Agent / Workflow) is irrelevant.
	for i, id := range []string{"agent-aaa", "agent-bbb"} {
		writeLines(t, dir, id+".jsonl", []map[string]any{
			{"type": "assistant", "isSidechain": true, "sessionId": "s",
				"timestamp": now.Add(time.Duration(-7+i) * time.Minute).UTC().Format(time.RFC3339Nano),
				"message": map[string]any{"role": "assistant", "model": "claude-sonnet-4-6",
					"usage": map[string]any{"input_tokens": 50, "output_tokens": 0}}},
		})
	}

	r := NewReader(dir, 0)
	if err := r.Refresh(now); err != nil {
		t.Fatal(err)
	}
	su := r.PerSession()["s"]
	if su.SubAgents != 2 {
		t.Errorf("SubAgents = %d, want 2 (one per agent-*.jsonl tagged to this conversation)", su.SubAgents)
	}
	if su.Total != 200 { // parent 100 + two sub-agents × 50, rolled up
		t.Errorf("Total = %d, want 200 (parent + sub-agent spend rolled up)", su.Total)
	}
}
