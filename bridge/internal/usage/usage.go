// Package usage estimates how much of your Claude Code quota the current
// 5-hour window has burned, by reading Claude Code's own local transcripts
// (~/.claude/projects/**/*.jsonl) — no API key, reflecting real usage. It's the
// data behind the Dial's rim gauge.
//
// The transcripts log one JSON object per line; assistant turns carry
// message.usage.{input,output,cache_creation,cache_read}_tokens and a top-level
// RFC3339 timestamp. We sum "work" tokens (input+output+cache_creation — cache
// reads are the cheap, discounted, and huge part, so they'd swamp the gauge) in
// the trailing 5h window, and divide by a budget. The budget is either set
// explicitly, or self-calibrated to the heaviest 5h seen over the last week —
// so the gauge is meaningful without knowing the (unpublished) plan limit.
package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// Window is Claude Code's usage block length.
	Window = 5 * time.Hour
	// historyDays bounds how far back we look to self-calibrate the budget.
	historyDays = 7
	// floorTokens keeps a light week from pegging the gauge: the self-calibrated
	// budget never drops below this, so a small block reads as a small fraction.
	floorTokens = 1_000_000
)

// Stats is a snapshot of current usage.
type Stats struct {
	Tokens   int64   // work tokens in the trailing 5h window
	Budget   int64   // denominator (explicit override, or self-calibrated peak)
	Fraction float64 // Tokens/Budget, clamped 0..1
}

// Pct returns the gauge fill as a 0..100 integer.
func (s Stats) Pct() int { return int(s.Fraction*100 + 0.5) }

// SessionUsage is per-conversation usage derived from a single transcript file.
type SessionUsage struct {
	Total     int64   // cumulative "work" tokens (input+output+cache_creation) over all turns
	Context   int64   // tokens resident in the context window at the last main-thread assistant turn
	SubAgents int     // Task sub-agents spawned (count of Task tool_use blocks in the transcript)
	Cost      float64 // cumulative USD cost for this conversation (ccusage-style, all turns)
}

// modelPriceUSD returns (inputPerM, outputPerM) in USD per 1M tokens for a model,
// matched by family so it survives version bumps. Cache writes bill at 1.25x the
// input rate, cache reads at 0.1x (the standard Anthropic multipliers, applied in
// lineUsage.costUSD). Unknown models fall back to Sonnet-tier pricing.
func modelPriceUSD(model string) (in, out float64) {
	switch {
	case strings.Contains(model, "opus"):
		return 5, 25
	case strings.Contains(model, "haiku"):
		return 1, 5
	case strings.Contains(model, "fable"), strings.Contains(model, "mythos"):
		return 10, 50
	default: // sonnet and anything unrecognized
		return 3, 15
	}
}

// Reader scans the transcript dir and keeps the latest Stats.
type Reader struct {
	dir    string
	budget int64 // explicit override; <=0 self-calibrates

	mu         sync.RWMutex
	latest     Stats
	perSession map[string]SessionUsage
}

// NewReader reads transcripts from dir (empty → ~/.claude/projects). budgetTokens
// <= 0 self-calibrates the denominator to the heaviest 5h in the last week.
func NewReader(dir string, budgetTokens int64) *Reader {
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".claude", "projects")
		}
	}
	return &Reader{dir: dir, budget: budgetTokens, perSession: map[string]SessionUsage{}}
}

// Latest returns the most recent computed stats (zero value until first Refresh).
func (r *Reader) Latest() Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.latest
}

// PerSession returns per-conversation usage keyed by session id (the transcript
// filename, minus .jsonl — the exact key the hooks report). Empty until the
// first Refresh. The map is replaced whole on each Refresh and never mutated in
// place, so callers may read it without copying.
func (r *Reader) PerSession() map[string]SessionUsage {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.perSession
}

// Run refreshes now and then every interval until ctx is cancelled.
func (r *Reader) Run(ctx context.Context, interval time.Duration) {
	_ = r.Refresh(time.Now())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = r.Refresh(time.Now())
		}
	}
}

type event struct {
	t   time.Time
	tok int64
}

// Refresh rescans the transcripts and recomputes Stats as of now.
func (r *Reader) Refresh(now time.Time) error {
	events, per, err := r.collect(now.Add(-historyDays * 24 * time.Hour))
	if err != nil {
		return err
	}
	sort.Slice(events, func(i, j int) bool { return events[i].t.Before(events[j].t) })

	cutoff := now.Add(-Window)
	var current int64
	for _, e := range events {
		if !e.t.Before(cutoff) {
			current += e.tok
		}
	}

	budget := r.budget
	if budget <= 0 {
		budget = peak5h(events)
		if budget < floorTokens {
			budget = floorTokens
		}
	}
	frac := 0.0
	if budget > 0 {
		frac = float64(current) / float64(budget)
		if frac > 1 {
			frac = 1
		}
	}

	r.mu.Lock()
	r.latest = Stats{Tokens: current, Budget: budget, Fraction: frac}
	r.perSession = per
	r.mu.Unlock()
	return nil
}

// peak5h is the largest sum of tokens over any trailing 5h window across events
// (assumed sorted by time) — a two-pointer sliding window.
func peak5h(events []event) int64 {
	var max, sum int64
	left := 0
	for right := range events {
		sum += events[right].tok
		for events[right].t.Sub(events[left].t) > Window {
			sum -= events[left].tok
			left++
		}
		if sum > max {
			max = sum
		}
	}
	return max
}

// collect reads every transcript touched since `since` and returns both the
// global usage events (for the 5h gauge) and per-session aggregates keyed by
// session id (the file's basename). Files not modified within the window can't
// hold recent events, so we skip them by mtime — the only thing that keeps a
// full rescan cheap. Both outputs come from one pass over each file.
func (r *Reader) collect(since time.Time) ([]event, map[string]SessionUsage, error) {
	var events []event
	per := make(map[string]SessionUsage)
	err := filepath.WalkDir(r.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if info, e := d.Info(); e == nil && info.ModTime().Before(since) {
			return nil // untouched in the window → no recent events
		}
		var su SessionUsage
		events, su = scanFile(events, path, since)
		per[strings.TrimSuffix(filepath.Base(path), ".jsonl")] = su
		return nil
	})
	if os.IsNotExist(err) {
		return events, per, nil // no transcripts yet → zero usage, not an error
	}
	return events, per, err
}

// lineUsage is the minimal shape we parse out of each transcript line.
type lineUsage struct {
	Timestamp   time.Time `json:"timestamp"`
	Type        string    `json:"type"`        // "assistant", "user", …
	IsSidechain bool      `json:"isSidechain"` // true for Task sub-agent turns
	Message     struct {
		Model string `json:"model"` // e.g. "claude-sonnet-4-6" — for cost pricing
		Usage struct {
			Input       int64 `json:"input_tokens"`
			Output      int64 `json:"output_tokens"`
			CacheCreate int64 `json:"cache_creation_input_tokens"`
			CacheRead   int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
		// Content is a string (user turns) or an array of blocks (assistant turns);
		// RawMessage captures either without failing the token parse.
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// countTaskBlocks counts Task tool_use blocks in an assistant turn's content — one
// per sub-agent this turn spawned. Content that isn't a JSON array (user string
// turns) yields 0, so it never disturbs token parsing.
func countTaskBlocks(content json.RawMessage) int {
	if len(content) == 0 || content[0] != '[' {
		return 0
	}
	var blocks []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return 0
	}
	n := 0
	for _, b := range blocks {
		// Sub-agents are spawned by the Task tool in Claude Code; the Agent tool
		// (and its aliases in some harnesses) does the same. Count either.
		if b.Type == "tool_use" && (b.Name == "Task" || b.Name == "Agent") {
			n++
		}
	}
	return n
}

// workTokens is what the 5h QUOTA gauge (and cumulative per-conversation spend)
// counts: input + output + cache_creation. cache_read is deliberately excluded —
// it is huge, cheap and discounted, and would swamp the number.
func (l lineUsage) workTokens() int64 {
	return l.Message.Usage.Input + l.Message.Usage.Output + l.Message.Usage.CacheCreate
}

// residentContextTokens is what fills the CONTEXT window on a turn: the input
// actually handed to the model = input + cache_read + cache_creation. It is the
// OPPOSITE cache_read rule to workTokens — it INCLUDES cache_read (the cached
// prefix IS the in-context history) and EXCLUDES output (generated text is not
// resident input). Keep the two helpers separate so a future "let's unify token
// counting" refactor can't silently fold cache_read into the quota gauge.
func (l lineUsage) residentContextTokens() int64 {
	return l.Message.Usage.Input + l.Message.Usage.CacheRead + l.Message.Usage.CacheCreate
}

// costUSD prices one turn the ccusage way: input + cache_creation×1.25 +
// cache_read×0.1 at the model's input rate, plus output at its output rate.
func (l lineUsage) costUSD() float64 {
	inRate, outRate := modelPriceUSD(l.Message.Model)
	u := l.Message.Usage
	billedInput := float64(u.Input) + float64(u.CacheCreate)*1.25 + float64(u.CacheRead)*0.1
	return (billedInput*inRate + float64(u.Output)*outRate) / 1_000_000
}

// scanFile reads one transcript once and returns (a) the in-window quota events
// appended to events — identical to the old behaviour — and (b) the whole file's
// per-conversation SessionUsage.
func scanFile(events []event, path string, since time.Time) ([]event, SessionUsage) {
	f, err := os.Open(path)
	if err != nil {
		return events, SessionUsage{}
	}
	defer f.Close()

	var su SessionUsage
	// Transcript lines can be very large (images, long tool output), so read with
	// an unbounded ReadBytes rather than a fixed-buffer Scanner.
	br := bufio.NewReader(f)
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			var l lineUsage
			if json.Unmarshal(raw, &l) == nil {
				work := l.workTokens()
				// Global 5h gauge — unchanged: work tokens, in-window only.
				if work > 0 && !l.Timestamp.IsZero() && !l.Timestamp.Before(since) {
					events = append(events, event{t: l.Timestamp, tok: work})
				}
				// Cumulative per conversation: every turn's work tokens over the
				// whole file (window-independent). Sub-agent (sidechain) turns are
				// real spend, so they count here.
				su.Total += work
				su.Cost += l.costUSD()
				// Context fill: the LAST main-thread assistant turn's resident input.
				// Skip sidechain turns — a sub-agent's context is not this
				// conversation's — and overwrite so the final value wins.
				if l.Type == "assistant" && !l.IsSidechain {
					if c := l.residentContextTokens(); c > 0 {
						su.Context = c
					}
					// Sub-agents are launched from the main thread via the Task tool.
					su.SubAgents += countTaskBlocks(l.Message.Content)
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
	}
	return events, su
}
