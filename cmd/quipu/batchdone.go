package main

// `quipu done A B C` (or `quipu done --batch`) finishes leaf Issues worked
// together on their Epic's rolling branch, with batches: epic. The PR merges
// once; each Issue whose commits landed then records evidence and closes, and
// an Issue no commit references stays In progress on the branch. The branch
// then moves to the new default-branch head, so the next leaves continue on it.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	policycheck "github.com/isnakolah/quipu/internal/policy"
)

// doneValueFlags take a value, so the word after them is not an Issue.
var doneValueFlags = setOf("actor", "pr", "wait", "criteria", "commands", "documentation", "risks", "boundary",
	"environments", "artifacts", "ci-url", "evidence-file")

// doneIssues separates `quipu done` arguments into Issue numbers and flags.
func doneIssues(args []string) (numbers []int, flags []string, batch bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name := strings.TrimLeft(arg, "-")
		switch {
		case name == "batch" && strings.HasPrefix(arg, "-"):
			batch = true
		case strings.HasPrefix(arg, "-"):
			flags = append(flags, arg)
			if doneValueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		default:
			if n, err := strconv.Atoi(strings.TrimPrefix(arg, "#")); err == nil && n > 0 {
				numbers = append(numbers, n)
			} else {
				flags = append(flags, arg)
			}
		}
	}
	return numbers, flags, batch
}

// referencesIssue reports whether a commit message names Issue number, as
// `Refs #N`, a `Refs: #N` footer, or an issue-prefix subject `#NNN ...`.
func referencesIssue(message string, number int) bool {
	refs := regexp.MustCompile(fmt.Sprintf(`(?mi)\bRefs:?\s+#0*%d\b|^#0*%d\s`, number, number))
	return refs.MatchString(message)
}

// landedIssues splits numbers by whether any commit references them.
func landedIssues(numbers []int, commits []string) (landed, left []int) {
	for _, n := range numbers {
		found := false
		for _, message := range commits {
			found = found || referencesIssue(message, n)
		}
		if found {
			landed = append(landed, n)
		} else {
			left = append(left, n)
		}
	}
	return landed, left
}

func doneBatch(numbers []int, flags []string, all bool) error {
	asJSON := false
	for _, flag := range flags {
		asJSON = asJSON || flag == "--json" || flag == "-json"
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	c, _, err := loadEnforcedConfig(here, true)
	if err != nil {
		return err
	}
	if !c.BatchesByEpic() {
		return flowErr("batch-off", "finishing several Issues at once needs batches: epic in quipu.yml")
	}
	if all {
		leases, err := here.readLeases()
		if err != nil {
			return flowErr("no-record", "--batch finishes the Issues leased in this worktree, and it holds none")
		}
		numbers = nil
		for _, lease := range leases {
			numbers = append(numbers, lease.Issue)
		}
	}
	if len(numbers) == 0 {
		return flowErr("batch-off", "name the Issues to finish: quipu done A B ... --json")
	}
	states := map[int]liveWork{}
	branch := ""
	for _, n := range numbers {
		state, err := loadLiveWork(c, n)
		if err != nil {
			return err
		}
		if _, ok := policycheck.BranchEpic(state.Branch); !ok || (branch != "" && state.Branch != branch) {
			return flowErr("batch-mixed", "#%d is on %q; a batch finishes Issues of one rolling Epic branch (%s)", n, state.Branch, orUnknown(branch))
		}
		branch = state.Branch
		states[n] = state
	}
	w := here.holding(numbers[0])
	pull, err := findPullRequest(c, "", branch)
	if err != nil {
		return err
	}
	view, err := loadPullFacts(c, pull.URL)
	if err != nil {
		return err
	}
	landed, left := landedIssues(numbers, view.Commits)
	if len(landed) == 0 {
		return flowErr("batch-empty", "no commit in %s references any of %v; commit messages say `Refs #N`", pull.URL, numbers)
	}
	// Every Issue that landed moves to review before the merge, so none is
	// left In progress beside a merged PR.
	if pull.State == "OPEN" {
		for _, n := range landed[1:] {
			state := states[n]
			if state.Status != "Claimed" && state.Status != "In progress" {
				continue
			}
			actor, err := resolveActor(here.holding(n), "", n)
			if err != nil {
				return err
			}
			if err := linkForReview(c, n, actor, state, pull.URL, 3*time.Minute); err != nil {
				return err
			}
		}
	}
	var results []flowResult
	saved := doneEmit
	doneEmit = func(result flowResult, _ bool) error { results = append(results, result); return nil }
	defer func() { doneEmit = saved }()
	for i, n := range landed {
		if err := doneCommand(append([]string{strconv.Itoa(n)}, flags...)); err != nil {
			if i == 0 {
				// The merge itself did not happen; nothing else can close.
				doneEmit = saved
				return err
			}
			results = append(results, flowResult{Issue: n, Status: states[n].Status, PR: pull.URL, Warnings: []string{err.Error()},
				Next: fmt.Sprintf("rerun quipu done %d --json with the same evidence", n)})
		}
	}
	for _, n := range left {
		results = append(results, flowResult{Issue: n, Status: states[n].Status, Branch: branch,
			Next: fmt.Sprintf("No commit in %s references #%d, so it stays %s on %s and lands with a later merge.", pull.URL, n, states[n].Status, branch)})
	}
	doneEmit = saved
	if after, err := findPullRequest(c, pull.URL, branch); err == nil && after.State == "MERGED" {
		if warning := continueEpicBranch(w, branch, c.DefaultBranch); warning != "" {
			results[0].Warnings = append(results[0].Warnings, warning)
		}
	}
	if asJSON {
		return printValue(results, true)
	}
	for _, result := range results {
		if err := printFlowResult(result, false); err != nil {
			return err
		}
	}
	return nil
}

// continueEpicBranch moves the rolling branch to the default branch's new
// head after its PR merged, so the Epic's next leaves start from what landed.
func continueEpicBranch(w worktree, branch, base string) string {
	if w.branch() != branch {
		return ""
	}
	if _, err := w.git("fetch", "--quiet", "origin", base); err != nil {
		return "could not fetch " + base + " to continue " + branch + ": " + err.Error()
	}
	if _, err := w.git("merge", "--ff-only", "--quiet", "origin/"+base); err != nil {
		return "could not move " + branch + " to " + base + ": " + err.Error()
	}
	if _, err := w.git("push", "--quiet", "-u", "origin", branch); err != nil {
		return "moved " + branch + " to " + base + " but could not push it: " + err.Error()
	}
	return ""
}
