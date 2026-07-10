// Package session holds the live state of every Claude Code session the bridge
// knows about. It is the single source of truth the daemon renders to devices.
package session

import (
	"sync"
	"time"

	"github.com/bruno00o/claude-dial/bridge/internal/protocol"
)

// Session is one Claude Code session.
type Session struct {
	ID       string
	Project  string
	State    string
	Tool     string
	Command  string
	Updated  time.Time
	Started  time.Time // first seen — for the "elapsed" readout
	CmdSince time.Time // when the current tool/command started — for the "stuck" clock
}

// Store is a concurrency-safe set of sessions, kept in insertion order so the
// dial's list is stable.
type Store struct {
	mu    sync.RWMutex
	byID  map[string]*Session
	order []string
	now   func() time.Time
}

// New returns an empty store.
func New() *Store {
	return &Store{byID: make(map[string]*Session), now: time.Now}
}

func (s *Store) ensure(id, project string) *Session {
	ss := s.byID[id]
	if ss == nil {
		ss = &Session{ID: id, State: protocol.StateIdle, Started: s.now()}
		s.byID[id] = ss
		s.order = append(s.order, id)
	}
	if project != "" {
		ss.Project = project
	}
	ss.Updated = s.now()
	return ss
}

// Upsert records a full session update, including the tool/command awaiting a
// decision. Used for permission requests.
func (s *Store) Upsert(id, project, state, tool, command string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.ensure(id, project)
	if ss.Tool != tool || ss.Command != command {
		ss.CmdSince = s.now() // command changed → restart the "stuck" clock
	}
	ss.State = state
	ss.Tool = tool
	ss.Command = command
}

// Touch creates the session if needed and sets its state, without disturbing
// the tool/command fields. Used for lifecycle events (start, stop, …).
func (s *Store) Touch(id, project, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensure(id, project).State = state
}

// blockedRenderGrace separates two message-render cases on a waiting session: a
// message within this window of the prompt appearing is part of *drawing* it
// (e.g. AskUserQuestion renders text in the same instant PermissionRequest
// fires) and must not clear the cue; a message arriving later means the
// permission was answered and work resumed, so it should clear to working.
const blockedRenderGrace = 3 * time.Second

// TouchLiveness refreshes a session from a weak liveness signal — an assistant
// message rendering (MessageDisplay), or a tool about to run (the monitor-mode
// PreToolUse notifier). It asserts "working" for idle/working/new sessions, and
// for a waiting session only once past blockedRenderGrace — so the "needs you"
// cue isn't clobbered the instant it appears, whether by a coincident render or
// by the PreToolUse that races the PermissionRequest for the same tool call
// (AskUserQuestion fires both at once), yet a genuinely resumed session (you
// answered, Claude is working again) doesn't stay stuck on "waiting". It never
// refreshes a still-fresh waiting session's timestamp, so its decay window stays
// anchored to when the request appeared.
func (s *Store) TouchLiveness(id, project string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.byID[id]; ss != nil &&
		(ss.State == protocol.StateBlocked || ss.State == protocol.StatePermission) &&
		s.now().Sub(ss.Updated) < blockedRenderGrace {
		return
	}
	s.ensure(id, project).State = protocol.StateWorking
}

// SetState updates only the state of an existing session.
func (s *Store) SetState(id, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.byID[id]; ss != nil {
		ss.State = state
		ss.Updated = s.now()
	}
}

// Remove drops a session entirely.
func (s *Store) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return
	}
	delete(s.byID, id)
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Sweep heals stale state, herdr-style: hooks are point-in-time events with no
// guaranteed "it's over" signal, so we can't trust a state to clear itself.
// herdr re-scans the terminal and notices a prompt is gone; lacking terminal
// access, we let transient states time out instead:
//
//   - "blocked" comes from a permission dialog. Answering "yes" resumes tool
//     activity within a second or two (which clears it); answering "no" fires
//     NO clearing hook at all, and the turn may end with no Stop. So a blocked
//     session silent longer than blockedSilence (a few seconds — longer than a
//     normal answer round-trip) is assumed answered and demoted to idle.
//   - "working" is demoted after the longer workingSilence to catch a missed
//     Stop or a daemon restart mid-turn.
//   - anything silent past maxAge is dropped (terminal closed, machine slept).
//
// permission_request (an active dial approval) is left alone — that path holds
// its own request open with its own timeout. Reports whether anything changed.
func (s *Store) Sweep(workingSilence, blockedSilence, maxAge time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	changed := false
	for i := 0; i < len(s.order); i++ {
		id := s.order[i]
		ss := s.byID[id]
		if ss == nil {
			continue
		}
		silence := now.Sub(ss.Updated)
		if maxAge > 0 && silence > maxAge {
			delete(s.byID, id)
			s.order = append(s.order[:i], s.order[i+1:]...)
			i--
			changed = true
			continue
		}
		// Deliberately leave Updated untouched on demotion: the maxAge clock
		// keeps running from the last real event.
		switch {
		case blockedSilence > 0 && ss.State == protocol.StateBlocked && silence > blockedSilence:
			ss.State = protocol.StateIdle
			changed = true
		case workingSilence > 0 && ss.State == protocol.StateWorking && silence > workingSilence:
			ss.State = protocol.StateIdle
			changed = true
		}
	}
	return changed
}

// Snapshot returns the current sessions in stable order.
func (s *Store) Snapshot() []protocol.SessionView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]protocol.SessionView, 0, len(s.order))
	for _, id := range s.order {
		ss := s.byID[id]
		if ss == nil {
			continue
		}
		elapsed := 0
		if !ss.Started.IsZero() {
			elapsed = int(s.now().Sub(ss.Started) / time.Second)
		}
		// Stuck: working on the same command for a long stretch (a hung tool or a
		// loop). Advisory only, so a generous threshold avoids flagging long builds.
		stuck := ss.State == protocol.StateWorking && ss.Command != "" &&
			!ss.CmdSince.IsZero() && s.now().Sub(ss.CmdSince) > 4*time.Minute
		out = append(out, protocol.SessionView{
			SessionID:   ss.ID,
			Project:     ss.Project,
			State:       ss.State,
			ToolName:    ss.Tool,
			Command:     ss.Command,
			ElapsedSecs: elapsed,
			Stuck:       stuck,
		})
	}
	return out
}
