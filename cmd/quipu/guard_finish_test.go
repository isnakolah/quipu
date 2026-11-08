package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// finishedFixture is the guard fixture after `quipu done` freed worktree a,
// whose repository configures a cleanup and whose build left output behind.
func finishedFixture(t *testing.T, grace time.Duration) (guardFixture, worktree) {
	t.Helper()
	f := newGuardFixture(t)
	config := filepath.Join(f.a, configPath)
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, append(raw, []byte("cleanup: cargo xtask cleanup --agent\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.a, "target", "debug"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.a, "target", "debug", "app"), []byte("binary"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.a, "README.md"), []byte("work\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.a, "add", "README.md")
	gitIn(t, f.a, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "work")
	a, err := openWorktree(f.a)
	if err != nil {
		t.Fatal(err)
	}
	if warnings := a.release(1, "", grace); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	return f, a
}

func bashCode(t *testing.T, cwd, command string) string {
	t.Helper()
	return guardCode(t, cwd, "Bash", map[string]any{"command": command})
}

// Eight Calla agents were denied the cleanup their AGENTS.md requires after
// `quipu done`. Its documented order now passes: check, done, cleanup, remove.
func TestCallaFinishOrderPassesTheGuard(t *testing.T) {
	f, _ := finishedFixture(t, time.Hour)
	for _, command := range []string{
		"cargo xtask cleanup --agent",
		"cd " + f.a + " && cargo xtask cleanup --agent",
		"cargo clean",
		"make clean",
		"rm -rf target",
		"rm -rf " + filepath.Join(f.a, "target", "debug"),
		"git clean -fdx",
	} {
		if got := bashCode(t, f.a, command); got != "" {
			t.Errorf("%q in a finished worktree: %s", command, got)
		}
	}
	if got := bashCode(t, f.top, "git worktree remove --force "+f.a); got != "" {
		t.Fatalf("removing the finished worktree: %s", got)
	}
	if got := bashCode(t, f.top, "git branch -d 001/a"); got != "branch-change" {
		t.Fatalf("a branch still checked out in a worktree stays: %q", got)
	}
	gitIn(t, f.top, "worktree", "remove", "--force", f.a)
	if got := bashCode(t, f.top, "git branch -d 001/a"); got != "" {
		t.Fatalf("deleting the merged work branch: %s", got)
	}
	for _, command := range []string{"git branch -D 001/a", "git branch -d main", "git branch -d 002/b"} {
		if got := bashCode(t, f.top, command); got != "branch-change" {
			t.Errorf("%q: %q", command, got)
		}
	}
}

// The window is for tidying up, not for more work.
func TestFinishedWorktreeTakesNoMoreWork(t *testing.T) {
	f, _ := finishedFixture(t, time.Hour)
	for _, command := range []string{
		"git commit -qm more",
		"git add -A",
		"rm README.md",
		"rm -rf .",
		"npm install",
		"cargo xtask cleanup --agent && git commit -qm more",
	} {
		if got := bashCode(t, f.a, command); got != "no-lease" {
			t.Errorf("%q in a finished worktree: %q", command, got)
		}
	}
	if got := bashCode(t, f.a, "git push origin 001/a"); got == "" {
		t.Error("a push from a finished worktree must be denied")
	}
	if got := guardCode(t, f.a, "Edit", map[string]any{"file_path": filepath.Join(f.a, "x.go")}); got != "no-lease" {
		t.Errorf("an edit in a finished worktree: %q", got)
	}
}

func TestTidyUpWindowCloses(t *testing.T) {
	f, a := finishedFixture(t, time.Hour)
	raw, _ := json.Marshal(finishedRecord{Issue: 1, Branch: "001/a", Until: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)})
	if err := os.WriteFile(filepath.Join(a.GitDir, finishedFileName), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if got := bashCode(t, f.a, "cargo xtask cleanup --agent"); got != "no-lease" {
		t.Fatalf("cleanup after the window: %q", got)
	}
	if got := bashCode(t, f.top, "git worktree remove --force "+f.a); got != "" {
		t.Fatalf("a Done worktree can still be removed: %s", got)
	}

	t.Run("done_grace_minutes: 0", func(t *testing.T) {
		off, _ := finishedFixture(t, 0)
		if got := bashCode(t, off.a, "cargo clean"); got != "no-lease" {
			t.Fatalf("a zero window is off: %q", got)
		}
	})
}

// A new lease ends the window.
func TestNewLeaseEndsTheWindow(t *testing.T) {
	_, a := finishedFixture(t, time.Hour)
	if _, ok := a.readFinished(time.Now()); !ok {
		t.Fatal("release must open the window")
	}
	now := time.Now().UTC()
	if err := a.writeLease(leaseContext{Issue: 3, Holder: "claude/octo/aaaa1111", Branch: "003/c",
		Expires: now.Add(time.Hour).Format(time.RFC3339), VerifiedAt: now.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.readFinished(time.Now()); ok {
		t.Fatal("a new lease must close the window")
	}
}
