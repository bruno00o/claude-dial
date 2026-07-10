package daemon

import "testing"

// autoApproves gates the tactile dial approval: only "default" (Claude asks the
// user) drives the device; every auto-approving mode — and an empty mode from an
// older Claude Code — must NOT.
func TestAutoApproves(t *testing.T) {
	prompts := []string{"", "default"}       // dial takes over
	auto := []string{"acceptEdits", "auto", "plan", "dontAsk", "bypassPermissions"}
	for _, m := range prompts {
		if autoApproves(m) {
			t.Errorf("autoApproves(%q) = true, want false (dial should take over)", m)
		}
	}
	for _, m := range auto {
		if !autoApproves(m) {
			t.Errorf("autoApproves(%q) = false, want true (dial should stay out)", m)
		}
	}
}
