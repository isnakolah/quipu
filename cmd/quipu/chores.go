package main

// Chores agents may do: assign an Issue and fix its body under issues_by: agents,
// close a PR nothing needs any more, and move work between Backlog and Ready under
// ready_by: agents. Each change is one Writer request, answered by one receipt.
// None merges, approves, closes an Issue, or decides a Decision or Gate.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	policycheck "github.com/isnakolah/quipu/internal/policy"
	"github.com/isnakolah/quipu/internal/writer"
)

// editKinds are the kinds whose body an agent may replace: those it may create.
var editKinds = setOf("story", "task", "defect")

// maxEditBody keeps the request comment that carries a body under GitHub's limit.
const maxEditBody = 50000

// choreOps are the reads and writes a chore makes, so tests can stand in for GitHub.
type choreOps struct {
	state     func(number int) (liveWork, error)
	assign    func(number int, login string) error
	edit      func(number int, body string) error
	viewPR    func(pr string) (prView, error)
	mergedFor func(number int) ([]string, error)
	comment   func(pr int, body string) error
	closePR   func(url string) error
}

func liveChoreOps(c model.Config) choreOps {
	client, repo := github.NewClient(), c.Owner+"/"+c.Repository
	return choreOps{
		state: func(number int) (liveWork, error) { return loadLiveWork(c, number) },
		assign: func(number int, login string) error {
			return client.AddAssignee(context.Background(), c.Owner, c.Repository, number, login)
		},
		edit: func(number int, body string) error {
			return client.SetIssueBody(context.Background(), c.Owner, c.Repository, number, body)
		},
		viewPR:    func(pr string) (prView, error) { return viewPR(c, pr) },
		mergedFor: func(number int) ([]string, error) { return mergedPRsFor(c, number) },
		comment: func(pr int, body string) error {
			_, err := receiptClient().CreateComment(context.Background(), c.Owner, c.Repository, pr, body)
			return err
		},
		closePR: func(url string) error {
			_, err := ghOutput("pr", "close", url, "--repo", repo)
			return err
		},
	}
}

// choreProblem refuses a chore outside its setting, or of the wrong Issue.
func choreProblem(c model.Config, number int, state liveWork, r writer.Request) error {
	if err := writer.CheckChore(workStateOf(state), r); err != nil {
		return err
	}
	if r.Action == "pr.close" {
		return nil
	}
	if !c.AgentsCreateIssues() {
		return fmt.Errorf("%s needs issues_by: agents on the default branch", r.Action)
	}
	if state.IssueState != "OPEN" {
		return fmt.Errorf("#%d is closed; chores change only open Issues", number)
	}
	if r.Action == "edit" {
		if kind := labelKind(state.Labels); !editKinds[strings.ToLower(kind)] {
			return fmt.Errorf("agents edit only Stories, Tasks and Defects, and #%d is %s", number, kindName(kind))
		}
		if err := model.ValidateWorkContract(r.Body); err != nil {
			return fmt.Errorf("the new body is not a valid work contract: %w", err)
		}
	}
	return nil
}

func kindName(kind string) string {
	if kind == "" {
		return "an Issue with no kind"
	}
	return "a " + kind
}

// applyChore is the Writer's answer to a chore request. Its receipt carries no
// state: the lifecycle stays where it was.
func applyChore(c model.Config, number int, state liveWork, r writer.Request, fingerprint string, ops choreOps) writer.Receipt {
	if err := choreProblem(c, number, state, r); err != nil {
		return rejectedReceipt(r, err)
	}
	var detail string
	switch r.Action {
	case "assign":
		if err := ops.assign(number, r.Assignee); err != nil {
			return rejectedReceipt(r, err)
		}
		detail = "assigned " + r.Assignee
	case "edit":
		if err := ops.edit(number, r.Body); err != nil {
			return rejectedReceipt(r, err)
		}
		detail = "Issue body replaced with a valid work contract"
	case "pr.close":
		view, err := ops.viewPR(r.PR)
		if err != nil {
			return rejectedReceipt(r, err)
		}
		if view.URL != r.PR {
			return rejectedReceipt(r, fmt.Errorf("%q is not a canonical pull request URL", r.PR))
		}
		reason, problem := prCloseProblem(number, state, view, ops)
		if problem != "" {
			return rejectedReceipt(r, errors.New(problem))
		}
		if err := ops.comment(view.Number, prCloseComment(r.Actor, reason)); err != nil {
			return rejectedReceipt(r, err)
		}
		if err := ops.closePR(view.URL); err != nil {
			return rejectedReceipt(r, err)
		}
		detail = fmt.Sprintf("closed PR #%d: %s", view.Number, reason)
	}
	return writer.Receipt{RequestID: r.ID, Fingerprint: fingerprint, Result: "accepted", Detail: detail, At: time.Now().UTC()}
}

func prCloseComment(actor, reason string) string {
	return fmt.Sprintf("Quipu closed this PR, as %s asked: %s. Nothing it references needs it any more.", actor, reason)
}

// prRefs are the Issues a PR body references as `Refs #N`, in order, once each.
func prRefs(body string) []int {
	var refs []int
	for _, match := range policycheck.IssueRef.FindAllStringSubmatch(body, -1) {
		if number, err := strconv.Atoi(match[1]); err == nil && number > 0 && !slices.Contains(refs, number) {
			refs = append(refs, number)
		}
	}
	return refs
}

// prCloseProblem says why a PR stays open, or else why nothing needs it: every
// Issue it references is Done or Cancelled, or has a different merged PR. state is
// #number's, the Issue the request was posted on.
func prCloseProblem(number int, state liveWork, view prView, ops choreOps) (reason, problem string) {
	if view.State != "OPEN" {
		return "", fmt.Sprintf("PR #%d is %s; only an open PR is closed", view.Number, strings.ToLower(view.State))
	}
	refs := prRefs(view.Body)
	if len(refs) == 0 {
		return "", fmt.Sprintf("PR #%d references no Issue as `Refs #N`, so only the owner can tell whether anything needs it", view.Number)
	}
	if !slices.Contains(refs, number) {
		return "", fmt.Sprintf("PR #%d does not reference #%d", view.Number, number)
	}
	var reasons []string
	for _, ref := range refs {
		work := state
		if ref != number {
			var err error
			if work, err = ops.state(ref); err != nil {
				return "", fmt.Sprintf("could not read #%d: %v", ref, err)
			}
		}
		switch {
		case work.Status == "Done" || work.Status == "Cancelled":
			reasons = append(reasons, fmt.Sprintf("#%d is %s", ref, work.Status))
			continue
		case work.Status == "Claimed" || work.Status == "In progress":
			return "", fmt.Sprintf("#%d is still %s, so PR #%d may be its work", ref, work.Status, view.Number)
		case work.Branch != "" && work.Branch == view.HeadRefName:
			return "", fmt.Sprintf("PR #%d is from %s, the branch of #%d, which is still %s", view.Number, work.Branch, ref, statusOr(work.Status))
		}
		merged, err := ops.mergedFor(ref)
		if err != nil {
			return "", fmt.Sprintf("could not list merged PRs for #%d: %v", ref, err)
		}
		other := ""
		for _, url := range merged {
			if url != view.URL {
				other = url
				break
			}
		}
		if other == "" {
			return "", fmt.Sprintf("#%d is %s and no other merged PR references it", ref, statusOr(work.Status))
		}
		reasons = append(reasons, fmt.Sprintf("#%d has merged PR %s", ref, other))
	}
	return strings.Join(reasons, "; "), ""
}

func statusOr(status string) string {
	if status == "" {
		return "not on the board"
	}
	return status
}

// mergedPRsFor lists merged PRs whose body references #number as `Refs #N`.
// Search only narrows the list; each body is checked here.
func mergedPRsFor(c model.Config, number int) ([]string, error) {
	output, err := ghOutput("pr", "list", "--repo", c.Owner+"/"+c.Repository, "--state", "merged", "--search", fmt.Sprintf(`"Refs #%d" in:body`, number), "--json", "url,body", "--limit", "100")
	if err != nil {
		return nil, err
	}
	var pulls []struct{ URL, Body string }
	if err := json.Unmarshal(output, &pulls); err != nil {
		return nil, err
	}
	var urls []string
	for _, pull := range pulls {
		if slices.Contains(prRefs(pull.Body), number) {
			urls = append(urls, pull.URL)
		}
	}
	return urls, nil
}

// choreSetup is the worktree and the config in force: the default branch's, so a
// worktree's copy cannot turn a chore on.
func choreSetup() (worktree, model.Config, error) {
	here, err := currentWorktree()
	if err != nil {
		return worktree{}, model.Config{}, err
	}
	c, _, err := loadEnforcedConfig(here, true)
	return here, c, err
}

func issueAssignCommand(args []string) error {
	fs := flag.NewFlagSet("issue assign", flag.ContinueOnError)
	wait := fs.Duration("wait", 3*time.Minute, "how long to wait for the Writer receipt")
	asJSON := jsonFlag(fs)
	usage := "usage: quipu issue assign N LOGIN [--json]"
	positional, err := parsePositional(fs, args)
	if err != nil || len(positional) != 2 {
		return keepHelp(err, usage)
	}
	number, err := strconv.Atoi(strings.TrimPrefix(positional[0], "#"))
	if err != nil || number < 1 {
		return fmt.Errorf("%s: invalid Issue number %q", usage, positional[0])
	}
	login := strings.TrimPrefix(positional[1], "@")
	if err := writer.CheckChore(writer.WorkState{}, writer.Request{Action: "assign", Assignee: login}); err != nil {
		return err
	}
	here, c, err := choreSetup()
	if err != nil {
		return err
	}
	if !c.AgentsCreateIssues() {
		return flowErr("chore-off", "assigning Issues needs issues_by: agents on the default branch")
	}
	actor, err := resolveActor(here, "", number)
	if err != nil {
		return err
	}
	if err := sendAndWait(c, number, actor, writer.Request{Action: "assign", Assignee: login}, *wait); err != nil {
		return err
	}
	return printFlowResult(flowResult{Issue: number, Status: "Assigned", Next: fmt.Sprintf("#%d is assigned to %s. Carry on with your own work.", number, login)}, *asJSON)
}

func issueEditCommand(args []string) error {
	fs := flag.NewFlagSet("issue edit", flag.ContinueOnError)
	bodyFile := fs.String("body-file", "", "the new Issue body, a valid work contract")
	wait := fs.Duration("wait", 3*time.Minute, "how long to wait for the Writer receipt")
	asJSON := jsonFlag(fs)
	number, err := parseIssueArg(fs, args, false)
	if err != nil || *bodyFile == "" {
		return keepHelp(err, "usage: quipu issue edit N --body-file FILE [--json]")
	}
	raw, err := os.ReadFile(*bodyFile)
	if err != nil {
		return err
	}
	body := string(raw)
	if len(body) > maxEditBody {
		return fmt.Errorf("the new body is %d bytes; a request carries at most %d", len(body), maxEditBody)
	}
	if err := model.ValidateWorkContract(body); err != nil {
		return fmt.Errorf("the new body is not a valid work contract: %w", err)
	}
	if err := writer.CheckChore(writer.WorkState{}, writer.Request{Action: "edit", Body: body}); err != nil {
		return err
	}
	here, c, err := choreSetup()
	if err != nil {
		return err
	}
	if !c.AgentsCreateIssues() {
		return flowErr("chore-off", "editing Issues needs issues_by: agents on the default branch")
	}
	actor, err := resolveActor(here, "", number)
	if err != nil {
		return err
	}
	if err := sendAndWait(c, number, actor, writer.Request{Action: "edit", Body: body}, *wait); err != nil {
		return err
	}
	return printFlowResult(flowResult{Issue: number, Status: "Edited", Next: fmt.Sprintf("#%d has its new body. Carry on with your own work.", number)}, *asJSON)
}

func prCloseCommand(args []string) error {
	fs := flag.NewFlagSet("pr close", flag.ContinueOnError)
	wait := fs.Duration("wait", 3*time.Minute, "how long to wait for the Writer receipt")
	asJSON := jsonFlag(fs)
	positional, err := parsePositional(fs, args)
	if err != nil || len(positional) != 1 {
		return keepHelp(err, "usage: quipu pr close PR [--json]  (PR number or URL)")
	}
	here, c, err := choreSetup()
	if err != nil {
		return err
	}
	ops := liveChoreOps(c)
	view, err := ops.viewPR(positional[0])
	if err != nil {
		return err
	}
	refs := prRefs(view.Body)
	if view.State == "CLOSED" {
		result := flowResult{Status: "Closed", PR: view.URL, Next: fmt.Sprintf("PR #%d is already closed; nothing to do.", view.Number)}
		if len(refs) > 0 {
			result.Issue = refs[0]
		}
		return printFlowResult(result, *asJSON)
	}
	if len(refs) == 0 {
		return flowErr("pr-still-needed", "PR #%d references no Issue as `Refs #N`, so only the owner can tell whether anything needs it", view.Number)
	}
	target := refs[0]
	state, err := ops.state(target)
	if err != nil {
		return err
	}
	if _, problem := prCloseProblem(target, state, view, ops); problem != "" {
		return flowErr("pr-still-needed", "PR #%d stays open: %s", view.Number, problem)
	}
	actor, err := resolveActor(here, "", target)
	if err != nil {
		return err
	}
	if err := sendAndWait(c, target, actor, writer.Request{Action: "pr.close", PR: view.URL}, *wait); err != nil {
		return err
	}
	return printFlowResult(flowResult{Issue: target, Status: "Closed", PR: view.URL, Next: fmt.Sprintf("PR #%d is closed, with the reason posted on it. Carry on with your own work.", view.Number)}, *asJSON)
}

// agentMoves are the moves an agent may make, with ready_by: agents.
var agentMoves = setOf("backlog", "ready")

// guardIssueKind reads an open Issue's kind for the guard; tests stand in for GitHub.
var guardIssueKind = openIssueKind

// agentMoveProblem says why an agent may not run this `quipu ready` or `quipu
// move`, or "" when it may: ready_by: agents, Backlog or Ready only, and never an
// Epic, Decision or Gate, which the owner moves.
func agentMoveProblem(c model.Config, sub string, args []shellArg, kind func(int) (string, error)) string {
	if !c.AgentsMakeReady() {
		return "agents move work only with ready_by: agents on the default branch"
	}
	var numbers []int
	statuses := 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !arg.ok {
			return "a computed argument cannot be checked"
		}
		if strings.HasPrefix(arg.s, "-") {
			name, _, inline := strings.Cut(strings.TrimLeft(arg.s, "-"), "=")
			if name == "wait" && !inline {
				i++
			}
			continue
		}
		if _, ok := ownerMoves[strings.ToLower(arg.s)]; ok && sub == "move" {
			if !agentMoves[strings.ToLower(arg.s)] {
				return "agents move work only to backlog or ready; the owner makes every other move"
			}
			statuses++
			continue
		}
		number, err := strconv.Atoi(strings.TrimPrefix(arg.s, "#"))
		if err != nil || number < 1 {
			return "pass one Issue number"
		}
		numbers = append(numbers, number)
	}
	if len(numbers) != 1 || (sub == "move" && statuses != 1) {
		return "pass one Issue number and, for move, backlog or ready"
	}
	got, err := kind(numbers[0])
	if err != nil {
		return fmt.Sprintf("could not read #%d: %v", numbers[0], err)
	}
	switch {
	case ownerDecidedKinds[strings.ToLower(got)]:
		return fmt.Sprintf("#%d is a %s, and the owner moves Epics, Decisions and Gates", numbers[0], got)
	case got == "a closed Issue":
		return fmt.Sprintf("#%d is closed", numbers[0])
	}
	return ""
}
