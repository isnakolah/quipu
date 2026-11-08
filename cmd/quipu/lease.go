package main

// `quipu lease release`: a lease left behind is cleared without the owner. The
// holder gives its own live lease back to Ready through the Writer; anyone may
// clear a local record whose lease GitHub no longer holds. A live lease held by
// someone else is never touched.

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/writer"
)

// What lease release does with a record.
const (
	releaseClear = "clear"   // the lease is over; only the local record goes
	releaseLive  = "release" // the caller's own live lease goes back to Ready
)

// releasePlan decides what releasing record means, from GitHub's answer about
// it (state, or liveErr when there was none) and the caller's identity.
func releasePlan(record leaseContext, state liveWork, liveErr error, actor string, now time.Time) (string, error) {
	switch {
	case errors.Is(liveErr, errNotInProject):
		return releaseClear, nil
	case liveErr != nil && record.expired(now):
		// The record's own expiry is a local fact; no answer is needed for it.
		return releaseClear, nil
	case liveErr != nil:
		return "", flowErr("lease-unverified", "could not read #%d from GitHub (%v), and its record has not expired, so nothing changed", record.Issue, liveErr)
	}
	if expires, err := time.Parse(time.RFC3339, state.Expiry); err != nil || !expires.After(now) || !leaseStillHeld(record, state) {
		return releaseClear, nil
	}
	if state.Holder != actor {
		return "", flowErr("held-by", "#%d is live and held by %q on %s; only its holder can release it", record.Issue, state.Holder, state.Branch)
	}
	return releaseLive, nil
}

func leaseCommand(args []string) error {
	if len(args) == 0 || args[0] != "release" {
		return errors.New("usage: quipu lease release [N] [--json]")
	}
	fs := flag.NewFlagSet("lease release", flag.ContinueOnError)
	wait := fs.Duration("wait", 3*time.Minute, "how long to wait for the Writer's receipt")
	asJSON := jsonFlag(fs)
	number, err := parseIssueArg(fs, args[1:], true)
	if err != nil {
		return err
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	w := here
	if number > 0 {
		w = here.holding(number)
	}
	record, err := w.readLease()
	if number > 0 {
		var ok bool
		if record, ok = w.leaseFor(number); !ok {
			err = errors.New("no record")
		}
	}
	if err != nil {
		what := "this worktree"
		if number > 0 {
			what = fmt.Sprintf("#%d in any worktree of this clone", number)
		}
		return flowErr("no-record", "no lease record for %s", what)
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	state, liveErr := loadLiveWork(c, record.Issue)
	actor, err := resolveActor(w, "", record.Issue)
	if err != nil {
		return err
	}
	plan, err := releasePlan(record, state, liveErr, actor, time.Now().UTC())
	if err != nil {
		return err
	}
	result := flowResult{Issue: record.Issue, Branch: record.Branch, Worktree: w.Top}
	switch plan {
	case releaseClear:
		w.removeLease(record.Issue)
		result.Status = "Cleared"
		result.Next = fmt.Sprintf("The record for #%d was left over; nothing on GitHub changed. Carry on, or take work with quipu start --next --json.", record.Issue)
	case releaseLive:
		if err := sendAndWait(c, record.Issue, actor, writer.Request{Action: "release"}, *wait); err != nil {
			return err
		}
		w.removeLease(record.Issue)
		result.Status = "Released"
		result.Next = fmt.Sprintf("#%d is Ready again and its branch %s is kept. Take work with quipu start --next --json.", record.Issue, record.Branch)
	}
	return printFlowResult(result, *asJSON)
}

// clearExpiredRecords removes every record in this clone's worktrees whose lease
// has run out, and names each one it removed.
func clearExpiredRecords(here worktree, now time.Time) []string {
	porcelain, err := here.git("worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	var cleared []string
	for _, entry := range parseWorktreeList(porcelain) {
		w, err := openWorktree(entry.Path)
		if err != nil {
			continue
		}
		if record, err := w.readLease(); err == nil && record.expired(now) && w.removeLease(record.Issue) {
			cleared = append(cleared, fmt.Sprintf("#%d in %s (expired %s)", record.Issue, w.Top, strings.TrimSuffix(record.Expires, "Z")))
		}
	}
	return cleared
}
