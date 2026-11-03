package main

// `quipu park N --on D`: an agent that needs the owner's decision hands the work back
// to wait for it, and takes other work. The Writer checks D and does it all at once.

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

// parkProblem says why work can't wait on this Issue, or "".
func parkProblem(on liveWork, onNumber int) string {
	switch {
	case labelKind(on.Labels) != "Decision":
		return fmt.Sprintf("#%d is not a Decision; work parks only on a Decision the owner has to make", onNumber)
	case on.IssueState != "OPEN":
		return fmt.Sprintf("Decision #%d is already closed", onNumber)
	}
	return ""
}

func hasBlocker(state liveWork, id string) bool {
	for _, blocker := range state.Blockers {
		if blocker.ID == id {
			return true
		}
	}
	return false
}

func parkCommand(args []string) error {
	fs := flag.NewFlagSet("park", flag.ContinueOnError)
	on := fs.Int("on", 0, "the open Decision this work waits for")
	wait := fs.Duration("wait", 3*time.Minute, "how long to wait for the Writer receipt")
	asJSON := jsonFlag(fs)
	number, err := parseIssueArg(fs, args, false)
	if err != nil || *on < 1 {
		return errors.New("usage: quipu park N --on DECISION [--json]")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	w := here.holding(number)
	actor, err := resolveActor(w, "", number)
	if err != nil {
		return err
	}
	if err := sendAndWait(c, number, actor, writer.Request{Action: "park", On: *on}, *wait); err != nil {
		return err
	}
	releaseParked(c, w, number)
	result := flowResult{Issue: number, Status: "Blocked", Next: fmt.Sprintf("#%d waits on Decision #%d. %s Take other work: quipu start --next --json.", number, *on, parkedReturn(c))}
	if changes, err := w.git("status", "--porcelain"); err == nil && changes != "" {
		result.Warnings = append(result.Warnings, "this worktree has uncommitted changes, which only it holds: commit and push them so the work can resume anywhere")
	}
	return printFlowResult(result, *asJSON)
}

// releaseParked frees what the work held here. The worktree keeps its files and
// skips cleanup, since the work comes back.
func releaseParked(c model.Config, w worktree, number int) {
	w.removeLease(number)
	freeWorkSlot(c, w)
}

func parkedReturn(c model.Config) string {
	if c.AutoReady {
		return "It returns to Ready when that closes."
	}
	return "The owner makes it Ready once that closes."
}

// autoReadyProblem says why work stays where it is, or "" when its last blocker just
// closed. Only work that waited on something moves: the owner decides everything else.
func autoReadyProblem(state liveWork, inProject bool) string {
	switch {
	case !inProject:
		return "not on the board"
	case state.IssueState != "OPEN":
		return "closed"
	case state.Status != "Backlog" && state.Status != "Blocked":
		return "not waiting"
	case len(state.Blockers) == 0:
		return "never had a blocker"
	case ownerDecidedKinds[strings.ToLower(labelKind(state.Labels))] || labelKind(state.Labels) == "":
		return "the owner decides this kind"
	case state.ParentID == "" || state.Children > 0:
		return "not startable"
	case model.ValidateWorkContract(state.Body) != nil:
		return "no valid work contract"
	}
	for _, label := range state.Labels {
		if label == needsOwnerLabel {
			return "labelled for the owner"
		}
	}
	for _, blocker := range state.Blockers {
		if blocker.State != "CLOSED" {
			return "still blocked"
		}
	}
	return ""
}

// needsOwnerLabel keeps work from becoming Ready by itself.
const needsOwnerLabel = "quipu:needs-owner"

// autoReady moves work whose last blocker closed. It posts a receipt, so drift treats
// Ready as where the work should be.
func (w *watch) autoReady(state *liveWork, inProject bool) (bool, error) {
	if !w.c.AutoReady || autoReadyProblem(*state, inProject) != "" {
		return false, nil
	}
	w.report.AutoReady++
	if !w.apply {
		return false, nil
	}
	next := writer.WorkState{Status: "Ready", Lease: writer.Lease{Branch: state.Branch}}
	if err := updateLiveWork(w.c, state.ItemID, next); err != nil {
		return false, err
	}
	requestID := fmt.Sprintf("auto-ready-%s-%d", state.IssueID, w.now.Unix())
	state.Status = "Ready"
	return true, w.post(writer.Receipt{RequestID: requestID, Result: "accepted", Detail: "its last blocker closed; Issue returned to Ready", At: w.now.UTC(), State: writer.StateOf(next)})
}

// dueSlowChecks names the nightly workflows with no run in a day.
func dueSlowChecks(checks []model.SlowCheck, last func(workflow string) (time.Time, error), now time.Time) ([]string, error) {
	var due []string
	var failure error
	for _, check := range checks {
		if check.When != "nightly" {
			continue
		}
		ran, err := last(check.Workflow)
		if err != nil {
			failure = errors.Join(failure, fmt.Errorf("%s: %w", check.Workflow, err))
			continue
		}
		if now.Sub(ran) >= 24*time.Hour {
			due = append(due, check.Workflow)
		}
	}
	return due, failure
}

// dispatchSlowChecks starts workflows on the default branch. A failure reaches the
// Writer's workflow_run trigger, which files a P0 Defect, because validation requires
// each one to be in defects_from.
func dispatchSlowChecks(c model.Config, workflows []string) error {
	var failure error
	for _, workflow := range workflows {
		if _, err := ghOutput("workflow", "run", workflow, "--repo", c.Owner+"/"+c.Repository, "--ref", c.DefaultBranch); err != nil {
			failure = errors.Join(failure, fmt.Errorf("%s: %w", workflow, err))
		}
	}
	return failure
}

func runNightlySlowChecks(c model.Config, now time.Time) error {
	due, err := dueSlowChecks(c.SlowChecks, func(workflow string) (time.Time, error) {
		output, err := ghOutput("run", "list", "--repo", c.Owner+"/"+c.Repository, "--workflow", workflow, "--branch", c.DefaultBranch, "--limit", "1", "--json", "createdAt", "--jq", ".[0].createdAt // \"\"")
		if err != nil {
			return time.Time{}, err
		}
		text := strings.TrimSpace(string(output))
		if text == "" {
			return time.Time{}, nil
		}
		return time.Parse(time.RFC3339, text)
	}, now)
	return errors.Join(err, dispatchSlowChecks(c, due))
}

// batchSlowChecks names what runs after a batch lands.
func batchSlowChecks(c model.Config) []string {
	var workflows []string
	for _, check := range c.SlowChecks {
		if check.When == "batch" {
			workflows = append(workflows, check.Workflow)
		}
	}
	return workflows
}
