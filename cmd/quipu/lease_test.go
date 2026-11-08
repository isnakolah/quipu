package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
)

// guardCodeLive is guardCode with GitHub's answer about each Issue faked.
func guardCodeLive(t *testing.T, cwd, tool string, input map[string]any, live func(model.Config, int) (liveWork, error)) string {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	g := newGuard(cwd, time.Now().UTC())
	g.caller = fixtureCaller
	g.live = live
	var d denial
	if err := g.check(hookInput{ToolName: tool, Cwd: cwd, ToolInput: raw}); errors.As(err, &d) {
		return d.Code
	} else if err != nil {
		return "error: " + err.Error()
	}
	return ""
}

func noNetwork(model.Config, int) (liveWork, error) {
	return liveWork{}, errors.New("no network in tests")
}

func writeRecord(t *testing.T, dir string, lease leaseContext) worktree {
	t.Helper()
	w, err := openWorktree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.writeLease(lease); err != nil {
		t.Fatal(err)
	}
	return w
}

func hasRecord(w worktree) bool {
	_, err := os.Stat(filepath.Join(w.GitDir, leaseFileName))
	return err == nil
}

// The Calla #238 case: `quipu start 238` ran in the main checkout, the owner
// switched it back to main, and the lease expired on GitHub. The record stayed
// and every command there was wrong-branch for five days.
func TestExpiredRecordInMainCheckoutBlocksNothing(t *testing.T) {
	f := newGuardFixture(t)
	past := time.Now().UTC().Add(-2 * time.Hour)
	stale := leaseContext{Issue: 238, Holder: "agent/octo/1aef1808", Branch: "238/the-open-shell-shows-nothing",
		Expires: past.Format(time.RFC3339), VerifiedAt: past.Add(-2 * time.Hour).Format(time.RFC3339)}
	main := writeRecord(t, f.top, stale)

	// A change in the main checkout is judged as if there were no record:
	// no-lease, never wrong-branch, and the record is gone.
	if code := guardCodeLive(t, f.top, "Bash", map[string]any{"command": "touch scratch.txt"}, noNetwork); code != "no-lease" {
		t.Fatalf("change in main checkout: %q, want no-lease", code)
	}
	if hasRecord(main) {
		t.Fatal("the expired record was not cleared")
	}

	// A subagent started in the main checkout drives a leased worktree. The
	// stale record must not make its session count as leased.
	writeRecord(t, f.top, stale)
	edit := map[string]any{"file_path": filepath.Join(f.a, "a.txt")}
	if code := guardCodeLive(t, f.top, "Edit", edit, noNetwork); code != "" {
		t.Fatalf("subagent from the main checkout editing a leased worktree: %q", code)
	}
}

func TestDeadRecordOnAnotherBranchIsCleared(t *testing.T) {
	f := newGuardFixture(t)
	now := time.Now().UTC()
	record := leaseContext{Issue: 5, Holder: "agent/octo/bbbb2222", Branch: "005/x",
		Expires: now.Add(time.Hour).Format(time.RFC3339), VerifiedAt: now.Add(-time.Hour).Format(time.RFC3339)}
	main := writeRecord(t, f.top, record)
	done := func(model.Config, int) (liveWork, error) {
		return liveWork{Status: "Done", Expiry: record.Expires, Branch: record.Branch}, nil
	}
	if code := guardCodeLive(t, f.top, "Bash", map[string]any{"command": "touch x"}, done); code != "no-lease" {
		t.Fatalf("finished work's record on another branch: %q, want no-lease", code)
	}
	if hasRecord(main) {
		t.Fatal("a record GitHub says is over was kept")
	}

	// With no answer from GitHub the record might be live: it stays, and the
	// branch rule still holds.
	writeRecord(t, f.top, record)
	if code := guardCodeLive(t, f.top, "Bash", map[string]any{"command": "touch x"}, noNetwork); code != "wrong-branch" {
		t.Fatalf("unverifiable record: %q, want wrong-branch", code)
	}
	if !hasRecord(main) {
		t.Fatal("a record was cleared without an answer from GitHub")
	}
}

func TestVerifyLiveClearsAFinishedRecord(t *testing.T) {
	f := newGuardFixture(t)
	now := time.Now().UTC()
	record := leaseContext{Issue: 1, Holder: "claude/octo/aaaa1111", Branch: "001/a",
		Expires: now.Add(time.Hour).Format(time.RFC3339), VerifiedAt: now.Add(-time.Hour).Format(time.RFC3339)}
	a := writeRecord(t, f.a, record)
	edit := map[string]any{"file_path": filepath.Join(f.a, "a.txt")}

	if code := guardCodeLive(t, f.a, "Edit", edit, noNetwork); code != "lease-inactive" || !hasRecord(a) {
		t.Fatalf("no answer: %q, record kept %v; want lease-inactive and the record kept", code, hasRecord(a))
	}
	merged := func(model.Config, int) (liveWork, error) {
		return liveWork{Status: "Done", Holder: record.Holder, Branch: record.Branch, Expiry: record.Expires}, nil
	}
	if code := guardCodeLive(t, f.a, "Edit", edit, merged); code != "lease-inactive" {
		t.Fatalf("Issue Done: %q, want lease-inactive", code)
	}
	if hasRecord(a) {
		t.Fatal("the record of Done work was kept")
	}

	// Expired on its own clock, on its own branch: cleared, with renew advice.
	record.Expires = now.Add(-time.Minute).Format(time.RFC3339)
	writeRecord(t, f.a, record)
	if code := guardCodeLive(t, f.a, "Edit", edit, noNetwork); code != "lease-inactive" || hasRecord(a) {
		t.Fatalf("expired on its branch: %q, record kept %v", code, hasRecord(a))
	}
}

func TestReleasePlan(t *testing.T) {
	now := time.Now().UTC()
	live := now.Add(time.Hour).Format(time.RFC3339)
	gone := now.Add(-time.Hour).Format(time.RFC3339)
	record := leaseContext{Issue: 7, Holder: "claude/octo/me", Branch: "007/x", Expires: live}
	held := liveWork{Status: "In progress", Holder: "claude/octo/me", Branch: "007/x", Expiry: live}
	for name, tc := range map[string]struct {
		record  leaseContext
		state   liveWork
		liveErr error
		actor   string
		plan    string
		code    string
	}{
		"own live lease":       {record, held, nil, "claude/octo/me", releaseLive, ""},
		"someone else's lease": {record, held, nil, "claude/octo/other", "", "held-by"},
		"expired on GitHub":    {record, liveWork{Status: "In progress", Holder: held.Holder, Branch: held.Branch, Expiry: gone}, nil, "claude/octo/other", releaseClear, ""},
		"taken by another":     {record, liveWork{Status: "In progress", Holder: "claude/octo/other", Branch: "007/x", Expiry: live}, nil, "claude/octo/other", releaseClear, ""},
		"Issue done":           {record, liveWork{Status: "Done", Holder: held.Holder, Branch: held.Branch, Expiry: live}, nil, "anyone", releaseClear, ""},
		"not in the Project":   {record, liveWork{}, errNotInProject, "anyone", releaseClear, ""},
		"no answer, expired":   {leaseContext{Issue: 7, Holder: "x", Branch: "007/x", Expires: gone}, liveWork{}, errors.New("502"), "anyone", releaseClear, ""},
		"no answer, unexpired": {record, liveWork{}, errors.New("502"), "claude/octo/me", "", "lease-unverified"},
	} {
		plan, err := releasePlan(tc.record, tc.state, tc.liveErr, tc.actor, now)
		var coded *flowError
		switch {
		case tc.code != "" && (!errors.As(err, &coded) || coded.Code != tc.code):
			t.Errorf("%s: got %v, want %s", name, err, tc.code)
		case tc.code == "" && (err != nil || plan != tc.plan):
			t.Errorf("%s: got %q, %v; want %q", name, plan, err, tc.plan)
		}
	}
}

func TestClearExpiredRecords(t *testing.T) {
	f := newGuardFixture(t)
	now := time.Now().UTC()
	main := writeRecord(t, f.top, leaseContext{Issue: 238, Holder: "x", Branch: "238/x", Expires: now.Add(-time.Hour).Format(time.RFC3339)})
	top, err := openWorktree(f.top)
	if err != nil {
		t.Fatal(err)
	}
	cleared := clearExpiredRecords(top, now)
	if len(cleared) != 1 || hasRecord(main) {
		t.Fatalf("cleared %v, main record kept %v", cleared, hasRecord(main))
	}
	a, _ := openWorktree(f.a)
	if !hasRecord(a) {
		t.Fatal("a live record was cleared")
	}
}
