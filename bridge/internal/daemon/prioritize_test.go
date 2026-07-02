package daemon

import (
	"testing"

	"github.com/bruno00o/claude-dial/bridge/internal/protocol"
)

// prioritize must surface needs-you sessions first and sink idle ones, while
// keeping the incoming (insertion) order within each tier so rows are stable.
func TestPrioritize(t *testing.T) {
	in := []protocol.SessionView{
		{SessionID: "a", State: protocol.StateIdle},
		{SessionID: "b", State: protocol.StateWorking},
		{SessionID: "c", State: protocol.StatePermission},
		{SessionID: "d", State: protocol.StateIdle},
		{SessionID: "e", State: protocol.StateBlocked},
		{SessionID: "f", State: protocol.StateWorking},
	}
	got := prioritize(in)

	// needs-you (c permission, e blocked) → working (b, f) → idle (a, d),
	// insertion order preserved inside each tier.
	want := []string{"c", "e", "b", "f", "a", "d"}
	for i, id := range want {
		if got[i].SessionID != id {
			t.Fatalf("position %d = %q, want %q (full order: %v)", i, got[i].SessionID, id, ids(got))
		}
	}
}

// A roster with a single tier must come back untouched (stable, no churn).
func TestPrioritizeStableWithinTier(t *testing.T) {
	in := []protocol.SessionView{
		{SessionID: "x", State: protocol.StateWorking},
		{SessionID: "y", State: protocol.StateWorking},
		{SessionID: "z", State: protocol.StateWorking},
	}
	got := prioritize(in)
	if want := []string{"x", "y", "z"}; !equal(ids(got), want) {
		t.Fatalf("order = %v, want %v", ids(got), want)
	}
}

func ids(vs []protocol.SessionView) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.SessionID
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
