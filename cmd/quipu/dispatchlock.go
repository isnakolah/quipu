package main

import (
	"fmt"
	"os"
)

// dispatchLock holds Issue number's dispatch lock for this clone, without
// waiting. ok is false when another process holds it: that process is creating
// the Issue's worktree and claiming it right now. The lock goes when unlock runs
// or the process ends, so a rerun after a no-receipt takes it again.
func (w worktree) dispatchLock(number int) (unlock func(), ok bool) {
	path, err := w.commonPath(fmt.Sprintf("quipu-dispatch-%d.lock", number))
	if err != nil {
		// No common directory to lock in: carry on as before.
		return func() {}, true
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return func() {}, true
	}
	free, err := tryLockFile(f)
	if err != nil {
		// No file locks here (Windows): carry on as before.
		f.Close()
		return func() {}, true
	}
	if !free {
		f.Close()
		return nil, false
	}
	return func() { f.Close() }, true
}

// claimedHere is a lost race inside one clone. It counts as a rejected claim,
// so start --next moves on to the next candidate.
func claimedHere(number int) error {
	e := flowErr("held-by", "#%d is being claimed, or is held, from another worktree of this clone", number)
	e.cause = errRejected
	return e
}

// reuseRefusal decides whether a dispatch may use a worktree that was already
// there. start N --worktree resumes it, as a rerun must. start --next refuses
// one that holds this Issue's lease: another dispatcher's claim went through.
// With no lease, the worktree is a rerun's after a no-receipt, and is reused.
func reuseRefusal(w worktree, created, next bool, number int) error {
	if created || !next {
		return nil
	}
	if _, held := w.leaseFor(number); held {
		return claimedHere(number)
	}
	return nil
}
