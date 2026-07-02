package rules

import (
	"path/filepath"
	"testing"
)

func TestAllowAndMatch(t *testing.T) {
	s := Load("")
	if s.Allowed("s1", "Bash", "npm test") {
		t.Fatal("empty store should allow nothing")
	}
	s.Allow("s1", "Bash", "npm test")
	if !s.Allowed("s1", "Bash", "npm test") {
		t.Fatal("granted call should be allowed")
	}
	// Match is exact and per session.
	if s.Allowed("s1", "Bash", "npm run build") {
		t.Fatal("different command must not match")
	}
	if s.Allowed("s2", "Bash", "npm test") {
		t.Fatal("different session must not match")
	}
	if s.Allowed("s1", "Read", "npm test") {
		t.Fatal("different tool must not match")
	}
}

func TestForget(t *testing.T) {
	s := Load("")
	s.Allow("s1", "Bash", "ls")
	s.Allow("s1", "Bash", "pwd")
	s.Allow("s2", "Bash", "ls")
	s.Forget("s1")
	if s.Allowed("s1", "Bash", "ls") || s.Allowed("s1", "Bash", "pwd") {
		t.Fatal("Forget must drop all grants for the session")
	}
	if !s.Allowed("s2", "Bash", "ls") {
		t.Fatal("Forget must leave other sessions untouched")
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "always-allow.json")
	s := Load(path) // dir doesn't exist yet; save must create it
	s.Allow("s1", "Bash", "git push")

	reloaded := Load(path)
	if !reloaded.Allowed("s1", "Bash", "git push") {
		t.Fatal("grant should survive a reload")
	}

	reloaded.Forget("s1")
	if Load(path).Allowed("s1", "Bash", "git push") {
		t.Fatal("Forget should persist too")
	}
}

func TestMissingFileIsEmptyNotError(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if s.Allowed("s1", "Bash", "ls") {
		t.Fatal("absent file must yield an empty store")
	}
	// And it must still be usable (writable) afterwards.
	s.Allow("s1", "Bash", "ls")
	if !s.Allowed("s1", "Bash", "ls") {
		t.Fatal("store should work after starting from an absent file")
	}
}
