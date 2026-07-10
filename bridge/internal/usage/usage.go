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
	EtaMins  int     // at the recent burn rate, minutes until the 5h budget is hit (0 = n/a)
}

// DiffToday is the edit volume since local midnight, from Edit/Write tool inputs.
type DiffToday struct {
	Added, Removed, Files int
}

// Event is a notable moment worth a glance-flash on the Dial (a commit landing,
// a test run passing or failing). Zero value = nothing recent.
type Event struct {
	Kind  string    // "commit" | "test_pass" | "test_fail"
	Label string    // short caption, e.g. "committed"
	Time  time.Time // when it happened (the resolving tool_result's timestamp)
}

// isTestCmd reports whether a shell command looks like a test run.
func isTestCmd(cmd string) bool {
	for _, m := range []string{"pytest", "npm test", "npm run test", "yarn test", "jest",
		"vitest", "go test", "cargo test", "rspec", "phpunit", "mvn test", "gradle test",
		"ctest", "rake test", "bun test", "deno test"} {
		if strings.Contains(cmd, m) {
			return true
		}
	}
	return false
}

// scanEventBlocks folds a turn's tool_use / tool_result blocks into the detectors:
// git-commit and test-run Bash calls are remembered by tool_use id, then resolved
// into ev (keeping the newest) when their result arrives — commits on success,
// tests as pass/fail from is_error.
func scanEventBlocks(content json.RawMessage, ts time.Time, pendCommit, pendTest map[string]bool, ev *Event) {
	if len(content) == 0 || content[0] != '[' {
		return
	}
	var blocks []struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		ID        string `json:"id"`
		ToolUseID string `json:"tool_use_id"`
		IsError   bool   `json:"is_error"`
		Input     struct {
			Command string `json:"command"`
		} `json:"input"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return
	}
	set := func(kind, label string) {
		if ts.After(ev.Time) {
			*ev = Event{Kind: kind, Label: label, Time: ts}
		}
	}
	for _, b := range blocks {
		switch {
		case b.Type == "tool_use" && b.Name == "Bash":
			if strings.Contains(b.Input.Command, "git commit") {
				pendCommit[b.ID] = true
			}
			if isTestCmd(b.Input.Command) {
				pendTest[b.ID] = true
			}
		case b.Type == "tool_result":
			if pendCommit[b.ToolUseID] {
				delete(pendCommit, b.ToolUseID)
				if !b.IsError {
					set("commit", "committed")
				}
			}
			if pendTest[b.ToolUseID] {
				delete(pendTest, b.ToolUseID)
				if b.IsError {
					set("test_fail", "tests fail")
				} else {
					set("test_pass", "tests pass")
				}
			}
		}
	}
}

// countLines returns the line count of s (0 for empty).
func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// addEdits folds an assistant turn's Edit/Write/MultiEdit tool_use blocks into the
// running day diff: added/removed line counts and the set of touched files.
func addEdits(content json.RawMessage, d *DiffToday, files map[string]bool) {
	if len(content) == 0 || content[0] != '[' {
		return
	}
	var blocks []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Input struct {
			FilePath  string `json:"file_path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
			Content   string `json:"content"`
			Edits     []struct {
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			} `json:"edits"`
		} `json:"input"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		switch b.Name {
		case "Edit":
			d.Added += countLines(b.Input.NewString)
			d.Removed += countLines(b.Input.OldString)
		case "Write":
			d.Added += countLines(b.Input.Content)
		case "MultiEdit":
			for _, e := range b.Input.Edits {
				d.Added += countLines(e.NewString)
				d.Removed += countLines(e.OldString)
			}
		default:
			continue
		}
		if b.Input.FilePath != "" {
			files[b.Input.FilePath] = true
		}
	}
}

// Pct returns the gauge fill as a 0..100 integer.
func (s Stats) Pct() int { return int(s.Fraction*100 + 0.5) }

// SessionUsage is per-conversation usage derived from a single transcript file.
type SessionUsage struct {
	Total      int64     // cumulative "work" tokens (input+output+cache_creation) over all turns
	Context    int64     // tokens resident in the context window at the last main-thread assistant turn
	SubAgents  int       // sub-agents spawned (count of this conversation's agent-*.jsonl transcripts)
	Cost       float64   // cumulative USD cost for this conversation (ccusage-style, all turns)
	Model      string    // the last main-thread assistant model (e.g. "claude-sonnet-4-6")
	LastError  bool      // the most recent tool result in the transcript was an error
	CachePct   int       // % of input tokens served from cache (the cache saver)
	TodayCost  float64   // USD spent by this conversation since local midnight
	Cwd        string    // working dir (from the transcript) — for resolving the project
	LastActive time.Time // most recent turn timestamp — for recency sorting
}

// lastToolResultError scans a turn's content for tool_result blocks and reports
// whether the last one was an error. Non-array content (plain text turns) yields
// (false, false), so it never trips on ordinary messages.
func lastToolResultError(content json.RawMessage) (found, isErr bool) {
	if len(content) == 0 || content[0] != '[' {
		return false, false
	}
	var blocks []struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return false, false
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			found, isErr = true, b.IsError
		}
	}
	return found, isErr
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
	diffToday  DiffToday
	lastEvent  Event
	activity   string // 24-char today heatmap, one char/hour ('0'..'9')
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

// DiffToday returns today's edit volume (added/removed lines, files touched).
func (r *Reader) DiffToday() DiffToday {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.diffToday
}

// LastEvent returns the most recent notable event (commit / test pass or fail).
func (r *Reader) LastEvent() Event {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastEvent
}

// Activity returns today's 24-hour token heatmap, one char per hour ('0'..'9').
func (r *Reader) Activity() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.activity
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
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	eventCut := now.Add(-5 * time.Minute)
	events, per, diff, ev, err := r.collect(now.Add(-historyDays*24*time.Hour), dayStart, eventCut)
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

	// Burn forecast: at the last 20 minutes' rate, minutes until the budget is hit.
	etaMins := 0
	recentCut := now.Add(-20 * time.Minute)
	var recentTok int64
	for _, e := range events {
		if !e.t.Before(recentCut) {
			recentTok += e.tok
		}
	}
	if recentTok > 0 && current < budget {
		if ratePerMin := float64(recentTok) / 20.0; ratePerMin > 0 {
			etaMins = int(float64(budget-current) / ratePerMin)
		}
	}

	// Today activity heatmap: 24 hourly token buckets since local midnight, scaled
	// to '0'..'9' by the busiest hour.
	var buckets [24]int64
	for _, e := range events {
		if !e.t.Before(dayStart) {
			buckets[e.t.In(now.Location()).Hour()] += e.tok
		}
	}
	var maxb int64
	for _, b := range buckets {
		if b > maxb {
			maxb = b
		}
	}
	act := make([]byte, 24)
	for i, b := range buckets {
		if maxb == 0 || b == 0 {
			act[i] = '0'
			continue
		}
		v := int(9 * b / maxb)
		if v < 1 {
			v = 1
		}
		act[i] = byte('0' + v)
	}

	r.mu.Lock()
	r.latest = Stats{Tokens: current, Budget: budget, Fraction: frac, EtaMins: etaMins}
	r.perSession = per
	r.diffToday = diff
	r.lastEvent = ev
	r.activity = string(act)
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
func (r *Reader) collect(since, dayStart, eventCut time.Time) ([]event, map[string]SessionUsage, DiffToday, Event, error) {
	var events []event
	per := make(map[string]SessionUsage)
	diff := DiffToday{}
	files := map[string]bool{} // distinct files touched today, across all transcripts
	ev := &Event{}             // most recent notable event (commit / test) in the window
	// Sub-agents run in their own agent-*.jsonl transcripts, each tagged with the
	// parent conversation's sessionId. Roll them onto the parent: count them, and
	// fold their token + dollar spend into the conversation's totals. This is
	// spawn-mechanism-agnostic — Task, Agent, and Workflow all produce these files
	// — where counting tool_use blocks missed Workflow entirely.
	type rollup struct {
		count int
		total int64
		cost  float64
	}
	agents := make(map[string]rollup)
	err := filepath.WalkDir(r.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if info, e := d.Info(); e == nil && info.ModTime().Before(since) {
			return nil // untouched in the window → no recent events
		}
		var su SessionUsage
		events, su = scanFile(events, path, since, dayStart, eventCut, &diff, files, ev)
		base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if strings.HasPrefix(base, "agent-") {
			if parent := parentSessionID(path); parent != "" {
				a := agents[parent]
				a.count++
				a.total += su.Total
				a.cost += su.Cost
				agents[parent] = a
			}
			return nil // a sub-agent, not a conversation of its own
		}
		per[base] = su
		return nil
	})
	for parent, a := range agents { // merge sub-agent rollups onto their parents
		su := per[parent]
		su.SubAgents += a.count
		su.Total += a.total
		su.Cost += a.cost
		per[parent] = su
	}
	diff.Files = len(files)
	if os.IsNotExist(err) {
		return events, per, diff, *ev, nil // no transcripts yet → zero usage, not an error
	}
	return events, per, diff, *ev, err
}

// parentSessionID reads a sub-agent transcript's parent conversation id from the
// "sessionId" every agent-*.jsonl line carries. Only the first line is needed.
func parentSessionID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if sc.Scan() {
		var h struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(sc.Bytes(), &h) == nil {
			return h.SessionID
		}
	}
	return ""
}

// lineUsage is the minimal shape we parse out of each transcript line.
type lineUsage struct {
	Timestamp   time.Time `json:"timestamp"`
	Type        string    `json:"type"`        // "assistant", "user", …
	IsSidechain bool      `json:"isSidechain"` // true for Task sub-agent turns
	Cwd         string    `json:"cwd"`         // working dir — for resolving the project name
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
func scanFile(events []event, path string, since, dayStart, eventCut time.Time, diff *DiffToday, files map[string]bool, ev *Event) ([]event, SessionUsage) {
	f, err := os.Open(path)
	if err != nil {
		return events, SessionUsage{}
	}
	defer f.Close()

	var su SessionUsage
	var cacheReadSum, inputAllSum int64                          // for the cache-saver percentage
	pendCommit, pendTest := map[string]bool{}, map[string]bool{} // commit/test detectors
	// Transcript lines can be very large (images, long tool output), so read with
	// an unbounded ReadBytes rather than a fixed-buffer Scanner.
	br := bufio.NewReader(f)
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			var l lineUsage
			if json.Unmarshal(raw, &l) == nil {
				if l.Cwd != "" {
					su.Cwd = l.Cwd // last non-empty wins — for the recent-history project name
				}
				if l.Timestamp.After(su.LastActive) {
					su.LastActive = l.Timestamp
				}
				// Error sniffer: the most recent tool result's error state (tool
				// results live in user turns). The last one seen wins, so a later
				// success clears it — the row reflects "the last tool call failed".
				if found, isErr := lastToolResultError(l.Message.Content); found {
					su.LastError = isErr
				}
				work := l.workTokens()
				// Global 5h gauge — unchanged: work tokens, in-window only.
				if work > 0 && !l.Timestamp.IsZero() && !l.Timestamp.Before(since) {
					events = append(events, event{t: l.Timestamp, tok: work})
				}
				// Cumulative per conversation: every turn's work tokens over the
				// whole file (window-independent). Sub-agent (sidechain) turns are
				// real spend, so they count here.
				su.Total += work
				c := l.costUSD()
				su.Cost += c
				if !l.Timestamp.IsZero() && !l.Timestamp.Before(dayStart) {
					su.TodayCost += c // spend since local midnight, for the daily budget
					if l.Type == "assistant" {
						addEdits(l.Message.Content, diff, files) // today's edit volume
					}
				}
				// Commit/test detectors: only very recent turns (eventCut), across
				// both the tool_use (assistant) and its tool_result (user) turns.
				if !l.Timestamp.IsZero() && !l.Timestamp.Before(eventCut) {
					scanEventBlocks(l.Message.Content, l.Timestamp, pendCommit, pendTest, ev)
				}
				u := l.Message.Usage
				cacheReadSum += u.CacheRead
				inputAllSum += u.Input + u.CacheCreate + u.CacheRead
				// Context fill: the LAST main-thread assistant turn's resident input.
				// Skip sidechain turns — a sub-agent's context is not this
				// conversation's — and overwrite so the final value wins.
				if l.Type == "assistant" && !l.IsSidechain {
					if c := l.residentContextTokens(); c > 0 {
						su.Context = c
					}
					if l.Message.Model != "" && l.Message.Model != "<synthetic>" {
						su.Model = l.Message.Model // last real model wins
					}
					// Sub-agents are counted in collect() from their own agent-*.jsonl
					// transcripts (which capture Task, Agent, and Workflow spawns alike),
					// not from tool_use blocks here.
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
	if inputAllSum > 0 {
		su.CachePct = int(cacheReadSum * 100 / inputAllSum) // share of input served from cache
	}
	return events, su
}
