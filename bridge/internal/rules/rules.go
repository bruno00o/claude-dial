// Package rules persists the "always allow" decisions a user makes on the dial.
// When a permission is answered "always allow", the exact (session, tool,
// command) is remembered so the same call auto-approves next time without
// lighting the dial again. Grants are scoped per session and survive a daemon
// restart via a small JSON file.
//
// This is a convenience layer, never a gate: every failure mode (unreadable
// file, unwritable dir) degrades to "not remembered", so a matching call simply
// prompts again. It can never block Claude Code (golden rule).
package rules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// toolBash names the one tool whose grants match by command prefix rather than
// exactly (see Allowed). Everything else (Read, Edit, WebFetch, …) carries a
// path/URL where a prefix match would be meaningless or unsafe.
const toolBash = "Bash"

// grant is one always-allow entry, matched exactly.
type grant struct {
	Session string `json:"session"`
	Tool    string `json:"tool"`
	Command string `json:"command"`
}

// Store is a concurrency-safe set of always-allow grants, optionally backed by a
// JSON file at path (path == "" keeps it in memory only).
type Store struct {
	mu   sync.Mutex
	set  map[grant]bool
	path string
}

// Load reads the store from path. A missing or corrupt file yields an empty
// store rather than an error — always-allow must never block startup.
func Load(path string) *Store {
	s := &Store{set: make(map[grant]bool), path: path}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s // absent or unreadable: start empty
	}
	var gs []grant
	if json.Unmarshal(data, &gs) == nil {
		for _, g := range gs {
			s.set[g] = true
		}
	}
	return s
}

// Allowed reports whether this session already always-allowed this call. For
// Bash, a grant matches by command prefix on word boundaries — always-allowing
// "npm test" covers "npm test" and "npm test --coverage" but not "npm testfoo"
// or "npm publish" — so trailing args don't force a re-prompt, mirroring Claude
// Code's own Bash(npm test:*) rule. Other tools match the command exactly.
func (s *Store) Allowed(session, tool, command string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tool != toolBash {
		return s.set[grant{session, tool, command}]
	}
	cand := strings.Fields(command)
	for g := range s.set {
		if g.Session == session && g.Tool == toolBash && tokenPrefix(strings.Fields(g.Command), cand) {
			return true
		}
	}
	return false
}

// tokenPrefix reports whether cand starts with every token of prefix. An empty
// prefix never matches (it would allow everything).
func tokenPrefix(prefix, cand []string) bool {
	if len(prefix) == 0 || len(cand) < len(prefix) {
		return false
	}
	for i, tok := range prefix {
		if cand[i] != tok {
			return false
		}
	}
	return true
}

// Allow records an always-allow grant and persists the store.
func (s *Store) Allow(session, tool, command string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := grant{session, tool, command}
	if s.set[g] {
		return
	}
	s.set[g] = true
	s.save()
}

// Forget drops every grant for a session (its terminal closed) and persists, so
// the file doesn't grow without bound as sessions come and go.
func (s *Store) Forget(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for g := range s.set {
		if g.Session == session {
			delete(s.set, g)
			changed = true
		}
	}
	if changed {
		s.save()
	}
}

// save writes the store to disk atomically. Best-effort: a failure just means a
// grant won't outlive this run — never fatal. The caller holds the lock.
func (s *Store) save() {
	if s.path == "" {
		return
	}
	gs := make([]grant, 0, len(s.set))
	for g := range s.set {
		gs = append(gs, g)
	}
	data, err := json.MarshalIndent(gs, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(s.path), 0o755) != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, s.path) // atomic replace
	}
}
