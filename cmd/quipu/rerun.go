package main

// `quipu rerun N`: the agent holding #N reruns the failed GitHub Actions runs on
// its own PR's head, so a flaky failure, or one that ran before a fix such as an
// owner's label, never waits for the owner to run `gh run rerun`. Only failed
// jobs rerun, and a run is never rerun past maxRerunAttempts, so a real failure
// does not loop.

import (
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/model"
)

// maxRerunAttempts is the attempt a run may reach through quipu rerun: the
// first run and two reruns.
const maxRerunAttempts = 3

// actionsRun is one GitHub Actions workflow run on a commit.
type actionsRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Attempt    int    `json:"run_attempt"`
	URL        string `json:"html_url"`
}

// failedConclusions are the run results a rerun can change.
var failedConclusions = setOf("failure", "timed_out", "cancelled", "startup_failure")

// rerunPlan picks the finished, failed runs to rerun and says why each other
// failed run is left alone.
func rerunPlan(runs []actionsRun) (rerun []actionsRun, skipped []string) {
	for _, run := range runs {
		switch {
		case run.Status != "completed" || !failedConclusions[run.Conclusion]:
		case run.Attempt >= maxRerunAttempts:
			skipped = append(skipped, fmt.Sprintf("%s (%s) already ran %d times; fix the cause instead", run.Name, run.URL, run.Attempt))
		default:
			rerun = append(rerun, run)
		}
	}
	return rerun, skipped
}

// rerunResult is what `quipu rerun --json` prints.
type rerunResult struct {
	Issue    int      `json:"issue"`
	Status   string   `json:"status"`
	PR       string   `json:"pr,omitempty"`
	Rerun    []string `json:"rerun"`
	Warnings []string `json:"warnings,omitempty"`
	Next     string   `json:"next"`
}

// rerunFailedRuns reruns the failed runs on the head of the open PR from
// branch. gh is ghOutput outside tests.
func rerunFailedRuns(gh func(...string) ([]byte, error), c model.Config, number int, branch string) (rerunResult, error) {
	repo := c.Owner + "/" + c.Repository
	result := rerunResult{Issue: number, Rerun: []string{}}
	output, err := gh("pr", "list", "--repo", repo, "--head", branch, "--state", "open", "--json", "url,headRefOid", "--limit", "1")
	if err != nil {
		return result, err
	}
	var pulls []struct {
		URL  string `json:"url"`
		Head string `json:"headRefOid"`
	}
	if err := json.Unmarshal(output, &pulls); err != nil {
		return result, fmt.Errorf("read PRs from %s: %w", branch, err)
	}
	if len(pulls) == 0 {
		return result, flowErr("pr-missing", "no open PR from %s", branch)
	}
	result.PR = pulls[0].URL
	output, err = gh("api", fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=100", repo, pulls[0].Head))
	if err != nil {
		return result, err
	}
	var page struct {
		Runs []actionsRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(output, &page); err != nil {
		return result, fmt.Errorf("read workflow runs: %w", err)
	}
	runs, skipped := rerunPlan(page.Runs)
	result.Warnings = skipped
	for _, run := range runs {
		if _, err := gh("run", "rerun", fmt.Sprint(run.ID), "--failed", "--repo", repo); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s (%s) did not rerun: %v", run.Name, run.URL, err))
			continue
		}
		result.Rerun = append(result.Rerun, fmt.Sprintf("%s (attempt %d)", run.Name, run.Attempt+1))
	}
	switch {
	case len(result.Rerun) > 0:
		result.Status = "Rerunning"
		result.Next = fmt.Sprintf("Take other work and look once with quipu status --json; when the checks finish, rerun quipu done %d.", number)
	case len(skipped) > 0:
		result.Status = "Nothing rerun"
		result.Next = fmt.Sprintf("Every failed run is at its attempt limit: fix the cause, push to %s, then rerun quipu done %d.", branch, number)
	default:
		result.Status = "Nothing rerun"
		result.Next = fmt.Sprintf("No failed run on the PR's head. Check quipu status --json for what quipu done %d is waiting on.", number)
	}
	return result, nil
}

func rerunCommand(args []string) error {
	fs := flag.NewFlagSet("rerun", flag.ContinueOnError)
	asJSON := jsonFlag(fs)
	number, err := parseIssueArg(fs, args, false)
	if err != nil {
		return keepHelp(err, "usage: quipu rerun N [--json]")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	lease, err := rerunLease(here.holding(number), number, time.Now().UTC())
	if err != nil {
		return err
	}
	result, err := rerunFailedRuns(ghOutput, c, number, lease.Branch)
	if err != nil {
		return err
	}
	if *asJSON {
		return printValue(result, true)
	}
	fmt.Printf("#%d %s: %s\n", number, result.Status, strings.Join(result.Rerun, ", "))
	for _, warning := range result.Warnings {
		fmt.Println("  ! " + warning)
	}
	fmt.Println(result.Next)
	return nil
}

// rerunLease is this machine's live lease on #number: only the agent holding
// the work reruns its checks.
func rerunLease(w worktree, number int, now time.Time) (leaseContext, error) {
	lease, ok := w.leaseFor(number)
	if !ok || lease.expired(now) {
		return leaseContext{}, flowErr("rejected-holder", "no worktree here holds a live lease on #%d; `quipu start %d --json` renews it", number, number)
	}
	return lease, nil
}
