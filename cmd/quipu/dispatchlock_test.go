package main

import (
	"errors"
	"testing"
)

func TestOneDispatcherClaimsAnIssueAtATime(t *testing.T) {
	w, err := openWorktree(gitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	unlock, ok := w.dispatchLock(3)
	if !ok {
		t.Fatal("the first dispatcher takes the lock")
	}
	if _, ok := w.dispatchLock(3); ok {
		t.Fatal("a second dispatcher for the same Issue waits its turn")
	}
	other, ok := w.dispatchLock(4)
	if !ok {
		t.Fatal("another Issue is not held up")
	}
	other()
	unlock()
	again, ok := w.dispatchLock(3)
	if !ok {
		t.Fatal("the lock goes when the claim is done, so a rerun takes it again")
	}
	again()
	if err := claimedHere(3); !errors.Is(err, errRejected) || flowCode(err) != "held-by" {
		t.Fatalf("a lost race inside the clone moves start --next on: %v", err)
	}
}

func TestNextNeverResumesAnotherDispatchersWorktree(t *testing.T) {
	w := leasedWorktree(t, 3)
	if err := reuseRefusal(w, false, true, 3); !errors.Is(err, errRejected) {
		t.Fatalf("--next skips a worktree whose lease is for this Issue: %v", err)
	}
	if err := reuseRefusal(w, false, false, 3); err != nil {
		t.Fatalf("start N --worktree resumes it: %v", err)
	}
	if err := reuseRefusal(w, false, true, 4); err != nil {
		t.Fatalf("a worktree with no lease for this Issue is a no-receipt rerun's, and is reused: %v", err)
	}
	if err := reuseRefusal(w, true, true, 3); err != nil {
		t.Fatalf("a worktree this dispatch created is its own: %v", err)
	}
}
