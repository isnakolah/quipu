package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Calla #315: the PR that quipu done merged added `cleanup`. done had read the
// config before the merge, so it ran nothing, and the guard, reading the same
// stale copy, then denied the cleanup in the finished worktree.
func TestMergedCleanupReadsTheConfigAsMerged(t *testing.T) {
	top, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	gitIn(t, top, "remote", "set-head", "origin", "main") // as a clone records it
	if got := mergedCleanup(agent, ""); got != "" {
		t.Fatalf("no cleanup on the default branch yet: %q", got)
	}
	stale, err := agent.git("rev-parse", "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	stale = strings.TrimSpace(stale)
	writeRepoFile(t, top, rootConfigPath, append(repoConfig(t), "cleanup: cargo xtask cleanup --agent\n"...))
	commitAll(t, top)
	gitIn(t, top, "push", "-q", "origin", "main")
	// The merge happened on GitHub: this clone's copy of main is the old one.
	gitIn(t, top, "update-ref", "refs/remotes/origin/main", stale)

	if got := mergedCleanup(agent, ""); got != "cargo xtask cleanup --agent" {
		t.Fatalf("cleanup after the merge: %q, want the merged command", got)
	}
	if c, ok := agent.publishedConfig(); !ok || c.Cleanup != "cargo xtask cleanup --agent" {
		t.Fatalf("the guard's copy of main is still stale: %q", c.Cleanup)
	}
}

func TestReleaseRecordsAndReportsTheCleanup(t *testing.T) {
	_, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	if err := agent.writeLease(leaseContext{Issue: 1, Holder: "h", Branch: "001/a", Expires: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	ran, warnings := agent.releaseReport(1, "true", time.Hour)
	if !ran || len(warnings) != 0 {
		t.Fatalf("release ran %v, warnings %q", ran, warnings)
	}
	f, ok := agent.readFinished(time.Now())
	if !ok || f.Cleanup != "true" {
		t.Fatalf("finished record %+v, want the cleanup kept", f)
	}
	result := withCleanup(doneResult(agent, 1, "", nil), "true", ran)
	if result.Cleanup != "true" {
		t.Fatalf("done's result does not name the cleanup: %+v", result)
	}
	if withCleanup(doneResult(agent, 1, "", nil), "true", false).Cleanup != "" {
		t.Fatal("a cleanup that did not run is reported")
	}
}

func TestGuardAllowsTheRecordedCleanupDuringTheWindow(t *testing.T) {
	f := newGuardFixture(t)
	a, err := openWorktree(f.a)
	if err != nil {
		t.Fatal(err)
	}
	a.release(1, "", time.Hour)
	record := func(until time.Time) {
		raw, _ := json.Marshal(finishedRecord{Issue: 1, Branch: "001/a", Until: until.UTC().Format(time.RFC3339), Cleanup: "cargo xtask cleanup --agent"})
		if err := os.WriteFile(filepath.Join(a.GitDir, finishedFileName), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The repository's config here has no cleanup: only the record names it.
	record(time.Now().Add(time.Hour))
	if got := bashCode(t, f.a, "cargo xtask cleanup --agent"); got != "" {
		t.Fatalf("the recorded cleanup in the window: %q", got)
	}
	if got := bashCode(t, f.a, "cargo xtask deploy"); got == "" {
		t.Fatal("a command that is not the cleanup passed")
	}
	record(time.Now().Add(-time.Minute))
	if got := bashCode(t, f.a, "cargo xtask cleanup --agent"); got == "" {
		t.Fatal("the recorded cleanup passed after the window closed")
	}
}
