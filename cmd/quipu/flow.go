package main

// Team-flow commands: `status`, `start`, and `done` wrap the lower-level
// request commands so a person or agent never handles Issue node IDs,
// fingerprints, or lease environment variables directly. Every state change
// still goes through a durable Writer request and waits for its receipt.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

// ---- quipu status ----

type statusItem struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	Kind           string `json:"kind"`
	Status         string `json:"status"`
	Priority       string `json:"priority,omitempty"`
	LeaseHolder    string `json:"lease_holder,omitempty"`
	LeaseExpires   string `json:"lease_expires,omitempty"`
	Branch         string `json:"branch,omitempty"`
	ParentNumber   int    `json:"parent,omitempty"`
	OpenBlockers   int    `json:"open_blockers,omitempty"`
	ChildTotal     int    `json:"child_total,omitempty"`
	ChildCompleted int    `json:"child_completed,omitempty"`
}

type statusReport struct {
	Repository string       `json:"repository"`
	Goals      []statusItem `json:"goals"`
	InProgress []statusItem `json:"in_progress"`
	InReview   []statusItem `json:"in_review"`
	Verifying  []statusItem `json:"verifying"`
	Blocked    []statusItem `json:"blocked"`
	Ready      []statusItem `json:"ready"`
	Backlog    int          `json:"backlog"`
	Attention  []string     `json:"attention"`
}

func statusCommand(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	items, err := loadStatusItems(c)
	if err != nil {
		return err
	}
	report := buildStatusReport(c.Owner+"/"+c.Repository, items, time.Now())
	if *asJSON {
		return printValue(report, true)
	}
	fmt.Print(renderStatus(report, time.Now()))
	return nil
}

func loadStatusItems(c model.Config) ([]statusItem, error) {
	query := fmt.Sprintf(`query { repository(owner:%q,name:%q) { issues(first:100,states:OPEN) { nodes {
number title parent { number } blockedBy(first:100) { nodes { state } }
subIssuesSummary { total completed }
labels(first:20) { nodes { name } }
projectItems(first:20) { nodes { project { id } fieldValues(first:30) { nodes {
... on ProjectV2ItemFieldSingleSelectValue { name field { ... on ProjectV2SingleSelectField { name } } }
... on ProjectV2ItemFieldTextValue { text field { ... on ProjectV2Field { name } } }
} } } }
} } } }`, c.Owner, c.Repository)
	output, err := ghOutput("api", "graphql", "-f", "query="+query)
	if err != nil {
		return nil, err
	}
	var response struct {
		Data struct {
			Repository struct {
				Issues struct {
					Nodes []struct {
						Number int    `json:"number"`
						Title  string `json:"title"`
						Parent *struct {
							Number int `json:"number"`
						} `json:"parent"`
						BlockedBy struct {
							Nodes []struct {
								State string `json:"state"`
							} `json:"nodes"`
						} `json:"blockedBy"`
						SubIssuesSummary struct {
							Total     int `json:"total"`
							Completed int `json:"completed"`
						} `json:"subIssuesSummary"`
						Labels struct {
							Nodes []struct {
								Name string `json:"name"`
							} `json:"nodes"`
						} `json:"labels"`
						ProjectItems struct {
							Nodes []struct {
								Project struct {
									ID string `json:"id"`
								} `json:"project"`
								FieldValues struct {
									Nodes []struct {
										Name  string `json:"name"`
										Text  string `json:"text"`
										Field struct {
											Name string `json:"name"`
										} `json:"field"`
									} `json:"nodes"`
								} `json:"fieldValues"`
							} `json:"nodes"`
						} `json:"projectItems"`
					} `json:"nodes"`
				} `json:"issues"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, err
	}
	items := make([]statusItem, 0, len(response.Data.Repository.Issues.Nodes))
	for _, node := range response.Data.Repository.Issues.Nodes {
		item := statusItem{Number: node.Number, Title: node.Title, ChildTotal: node.SubIssuesSummary.Total, ChildCompleted: node.SubIssuesSummary.Completed}
		if node.Parent != nil {
			item.ParentNumber = node.Parent.Number
		}
		for _, blocker := range node.BlockedBy.Nodes {
			if blocker.State != "CLOSED" {
				item.OpenBlockers++
			}
		}
		for _, label := range node.Labels.Nodes {
			if strings.HasPrefix(label.Name, "kind:") {
				item.Kind = titleCase(strings.TrimPrefix(label.Name, "kind:"))
			}
		}
		for _, projectItem := range node.ProjectItems.Nodes {
			if projectItem.Project.ID != c.Project.ID {
				continue
			}
			for _, value := range projectItem.FieldValues.Nodes {
				switch value.Field.Name {
				case "Status":
					item.Status = value.Name
				case "Kind":
					item.Kind = value.Name
				case "Priority":
					item.Priority = value.Name
				case "Lease holder":
					item.LeaseHolder = value.Text
				case "Lease expires":
					item.LeaseExpires = value.Text
				case "Branch":
					item.Branch = value.Text
				}
			}
		}
		items = append(items, item)
	}
	return items, nil
}

// buildStatusReport groups open Issues into the columns a team reads at
// standup and lists anything that needs a human decision.
func buildStatusReport(repository string, items []statusItem, now time.Time) statusReport {
	report := statusReport{Repository: repository, Goals: []statusItem{}, InProgress: []statusItem{}, InReview: []statusItem{}, Verifying: []statusItem{}, Blocked: []statusItem{}, Ready: []statusItem{}, Attention: []string{}}
	sort.Slice(items, func(i, j int) bool { return items[i].Number < items[j].Number })
	for _, item := range items {
		if strings.EqualFold(item.Kind, "Epic") {
			report.Goals = append(report.Goals, item)
			if item.ChildTotal > 0 && item.ChildCompleted == item.ChildTotal {
				report.Attention = append(report.Attention, fmt.Sprintf("#%d all %d children are done; close the goal or add remaining work", item.Number, item.ChildTotal))
			}
			continue
		}
		switch item.Status {
		case "Claimed", "In progress":
			report.InProgress = append(report.InProgress, item)
			if !hasActiveLease(item.LeaseHolder, item.LeaseExpires, now) {
				report.Attention = append(report.Attention, fmt.Sprintf("#%d lease expired; Writer will return it to Ready", item.Number))
			}
		case "In review":
			report.InReview = append(report.InReview, item)
		case "Evidence pending":
			report.Verifying = append(report.Verifying, item)
		case "Blocked":
			report.Blocked = append(report.Blocked, item)
		case "Ready":
			report.Ready = append(report.Ready, item)
		case "Backlog":
			report.Backlog++
		case "":
			report.Attention = append(report.Attention, fmt.Sprintf("#%d has no Project Status", item.Number))
		}
	}
	sort.SliceStable(report.Ready, func(i, j int) bool {
		return priorityRank(report.Ready[i].Priority) < priorityRank(report.Ready[j].Priority)
	})
	return report
}

func priorityRank(priority string) int {
	if len(priority) == 2 && priority[0] == 'P' && priority[1] >= '0' && priority[1] <= '9' {
		return int(priority[1] - '0')
	}
	return 9
}

func renderStatus(report statusReport, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", report.Repository)
	section := func(title string, items []statusItem, detail func(statusItem) string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s (%d)\n", title, len(items))
		for _, item := range items {
			line := fmt.Sprintf("  #%-4d %s", item.Number, item.Title)
			if extra := detail(item); extra != "" {
				line += "  — " + extra
			}
			fmt.Fprintln(&b, line)
		}
	}
	section("Goals", report.Goals, func(item statusItem) string {
		if item.ChildTotal == 0 {
			return "no tasks yet"
		}
		return fmt.Sprintf("%d/%d done", item.ChildCompleted, item.ChildTotal)
	})
	section("In progress", report.InProgress, func(item statusItem) string {
		return strings.TrimSpace(item.LeaseHolder + " " + leaseRemaining(item.LeaseExpires, now))
	})
	section("In review", report.InReview, func(item statusItem) string { return item.Branch })
	section("Verifying", report.Verifying, func(statusItem) string { return "run quipu done to finish" })
	section("Blocked", report.Blocked, func(statusItem) string { return "" })
	section("Ready to pick up", report.Ready, func(item statusItem) string {
		if item.OpenBlockers > 0 {
			return fmt.Sprintf("%s, waiting on %d blocker(s)", item.Priority, item.OpenBlockers)
		}
		return item.Priority
	})
	if report.Backlog > 0 {
		fmt.Fprintf(&b, "\nBacklog: %d not yet Ready\n", report.Backlog)
	}
	if len(report.Attention) > 0 {
		fmt.Fprintf(&b, "\nNeeds attention\n")
		for _, line := range report.Attention {
			fmt.Fprintf(&b, "  ! %s\n", line)
		}
	}
	return b.String()
}

func leaseRemaining(expires string, now time.Time) string {
	expiry, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		return ""
	}
	left := expiry.Sub(now.UTC()).Round(time.Minute)
	if left <= 0 {
		return "(lease expired)"
	}
	return "(" + left.String() + " left)"
}

// ---- quipu start ----

// renewWithin is how close to expiry a rerun of `start` renews the lease, so
// long-running agents keep their work by calling `start N` again.
const renewWithin = 30 * time.Minute

// startOptions are the `quipu start` flags shared by every attempt in a --next run.
type startOptions struct {
	Actor, Branch string
	Wait          time.Duration
	// Worktree dispatches the work into its own new worktree under WorktreeRoot
	// instead of switching the current one.
	Worktree     bool
	WorktreeRoot string
}

func startCommand(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	o := startOptions{}
	fs.StringVar(&o.Actor, "actor", "", "lease holder; defaults to QUIPU_ACTOR, then provider/login/worktree-id")
	fs.StringVar(&o.Branch, "branch", "", "NNN/short-description branch; derived from the title when omitted")
	next := fs.Bool("next", false, "pick the highest-priority Ready, unblocked, unleased leaf")
	fs.DurationVar(&o.Wait, "wait", 3*time.Minute, "how long to wait for each Writer receipt")
	fs.BoolVar(&o.Worktree, "worktree", false, "create a new git worktree for the work instead of switching this one")
	fs.StringVar(&o.WorktreeRoot, "worktree-root", "", "where --worktree creates worktrees; defaults to ../<checkout>-worktrees")
	asJSON := jsonFlag(fs)
	number, err := parseIssueArg(fs, args, true)
	if err != nil || (number == 0) != *next {
		return errors.New("usage: quipu start (N | --next) [--worktree [--worktree-root DIR]] [--branch NNN/slug] [--actor ID] [--json]")
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if !*next {
		result, err := startIssue(c, here, number, o)
		if err != nil {
			return err
		}
		return printFlowResult(result, *asJSON)
	}
	candidates, err := loadWorkCandidates(c)
	if err != nil {
		return err
	}
	ready := nextWork(candidates, time.Now())
	o.Branch = ""
	// Several agents may race for the same top item; the Writer accepts one
	// claim and rejects the rest, so a rejected claim moves on to the next.
	lost := 0
	for i, item := range ready {
		if i == 3 {
			break
		}
		result, err := startIssue(c, here, item.Number, o)
		if err == nil {
			return printFlowResult(result, *asJSON)
		}
		if !errors.Is(err, errRejected) {
			return err
		}
		lost++
		fmt.Fprintf(os.Stderr, "#%d taken (%v); trying next\n", item.Number, err)
	}
	if lost > 0 {
		return printFlowResult(flowResult{Status: "Busy", Next: fmt.Sprintf("Other agents took the top %d Ready items first. Rerun quipu start --next --json.", lost)}, *asJSON)
	}
	return printFlowResult(flowResult{Status: "Idle", Next: "No Ready, unblocked work. Report that and stop; the owner makes work Ready. quipu status --json shows the board."}, *asJSON)
}

// nextWork orders pick-up candidates by priority, then Issue number.
func nextWork(candidates []workCandidate, now time.Time) []readyWork {
	ready := selectReadyWork(candidates, now)
	sort.SliceStable(ready, func(i, j int) bool { return priorityRank(ready[i].Priority) < priorityRank(ready[j].Priority) })
	return ready
}

// startIssue claims and starts one Issue for the worktree that will do it:
// here, or with o.Worktree a new worktree created before the claim so the
// lease belongs to that worktree's identity. A lost claim removes what it made.
func startIssue(c model.Config, here worktree, number int, o startOptions) (result flowResult, err error) {
	state, err := loadLiveWork(c, number)
	if err != nil {
		return flowResult{}, err
	}
	branch := o.Branch
	switch {
	case branch != "":
	case state.Branch != "":
		branch = state.Branch
	default:
		title, err := issueTitle(c, number)
		if err != nil {
			return flowResult{}, err
		}
		branch = branchName(number, title)
	}
	w := here
	if o.Worktree {
		path := dispatchPath(here.Top, o.WorktreeRoot, c.Repository, number)
		created, createdTree, createdBranch, err := here.addWorktree(path, branch, c.DefaultBranch)
		if err != nil {
			return flowResult{}, err
		}
		if createdTree {
			// Undo on failure, except when a request may still be accepted:
			// a rerun then reuses this worktree and so its lease identity.
			defer func() {
				if err != nil && !errors.Is(err, errNoReceipt) {
					here.removeWorktree(path, branch, createdBranch)
				}
			}()
		}
		w = created
	} else if w.branch() != branch {
		// Fail before claiming when another worktree already has the branch.
		if err := w.requireBranchFree(branch); err != nil {
			return flowResult{}, err
		}
	}
	actor, err := resolveActor(w, o.Actor, number)
	if err != nil {
		return flowResult{}, err
	}
	wait := o.Wait
	if state.Status == "Ready" {
		if err := sendAndWait(c, number, actor, writer.Request{Action: "claim", Branch: branch}, wait); err != nil {
			return flowResult{}, err
		}
		if state, err = loadLiveWork(c, number); err != nil {
			return flowResult{}, err
		}
	}
	if state.Status == "Claimed" {
		if err := sendAndWait(c, number, actor, writer.Request{Action: "start"}, wait); err != nil {
			return flowResult{}, err
		}
		if state, err = loadLiveWork(c, number); err != nil {
			return flowResult{}, err
		}
	}
	if state.Status != "In progress" && state.Status != "In review" {
		return flowResult{}, flowErr("not-ready", "#%d is %q; only Ready work can be started", number, state.Status)
	}
	if state.Holder != actor {
		return flowResult{}, flowErr("held-by", "#%d is held by %q on %s, not by this worktree (%q)", number, state.Holder, state.Branch, actor)
	}
	if expiry, err := time.Parse(time.RFC3339, state.Expiry); err == nil && time.Until(expiry) < renewWithin {
		if err := sendAndWait(c, number, actor, writer.Request{Action: "renew"}, wait); err != nil {
			return flowResult{}, err
		}
		if state, err = loadLiveWork(c, number); err != nil {
			return flowResult{}, err
		}
	}
	if err := w.switchBranch(state.Branch, c.DefaultBranch); err != nil {
		return flowResult{}, err
	}
	lease := leaseContext{Issue: number, Holder: state.Holder, Branch: state.Branch, Expires: state.Expiry, VerifiedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := w.writeLease(lease); err != nil {
		return flowResult{}, err
	}
	result = flowResult{Issue: number, Status: state.Status, Branch: state.Branch, Holder: state.Holder, LeaseExpires: state.Expiry}
	result.Next = fmt.Sprintf("Implement #%d on %s. Push and open a PR whose body says \"Refs #%d\", then run: quipu done %d. Rerun quipu start %d to renew the lease on long work.", number, state.Branch, number, number, number)
	if state.Status == "In review" {
		result.Next = fmt.Sprintf("Address review on %s, then run: quipu done %d", state.Branch, number)
	}
	if o.Worktree {
		result.Worktree = w.Top
		result.Next = fmt.Sprintf("Work in %s (cd there or launch the agent there). %s", w.Top, result.Next)
	}
	return result, nil
}

// flowResult is the outcome of `start` or `done`. Next tells a person or an
// agent exactly what to do now, so agents can follow it without parsing prose.
type flowResult struct {
	Issue        int    `json:"issue,omitempty"`
	Status       string `json:"status"`
	Branch       string `json:"branch,omitempty"`
	Holder       string `json:"holder,omitempty"`
	LeaseExpires string `json:"lease_expires,omitempty"`
	PR           string `json:"pr,omitempty"`
	Worktree     string `json:"worktree,omitempty"`
	Next         string `json:"next"`
}

func printFlowResult(result flowResult, asJSON bool) error {
	if asJSON {
		return printValue(result, true)
	}
	if result.Issue > 0 {
		line := fmt.Sprintf("#%d %s", result.Issue, result.Status)
		if result.Branch != "" {
			line += " on " + result.Branch
		}
		if result.Holder != "" {
			line += fmt.Sprintf(" (held by %s %s)", result.Holder, leaseRemaining(result.LeaseExpires, time.Now()))
		}
		if result.PR != "" {
			line += " — " + result.PR
		}
		if result.Worktree != "" {
			line += " in " + result.Worktree
		}
		fmt.Println(line)
	}
	fmt.Println("Next: " + result.Next)
	return nil
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// branchName derives NNN/short-description from an Issue title, dropping the
// "US-016 —" style prefix so the slug describes the work.
func branchName(number int, title string) string {
	if i := strings.Index(title, "—"); i >= 0 && i < 16 {
		title = title[i+len("—"):]
	}
	slug := strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(title), "-"), "-")
	words := strings.Split(slug, "-")
	if len(words) > 5 {
		words = words[:5]
	}
	slug = strings.Join(words, "-")
	if slug == "" {
		slug = "work"
	}
	return fmt.Sprintf("%03d/%s", number, slug)
}

// ---- quipu done ----

func doneCommand(args []string) error {
	fs := flag.NewFlagSet("done", flag.ContinueOnError)
	actorFlag := fs.String("actor", "", "request actor; defaults to the lease holder in .git/quipu-lease")
	asJSON := jsonFlag(fs)
	prFlag := fs.String("pr", "", "pull request URL; found from the work branch when omitted")
	wait := fs.Duration("wait", 3*time.Minute, "how long to wait for each Writer receipt")
	e := writer.Evidence{}
	fs.StringVar(&e.Criteria, "criteria", "", "acceptance-criteria result")
	fs.StringVar(&e.Commands, "commands", "", "exact verification commands")
	fs.StringVar(&e.Documentation, "documentation", "", "documentation changes; None: reason")
	fs.StringVar(&e.Risks, "risks", "", "known residual risks; None")
	fs.StringVar(&e.Boundary, "boundary", "", "local|CI|target host|provider|staging|production|release")
	fs.StringVar(&e.Environments, "environments", "GitHub Actions CI", "verified environments")
	fs.StringVar(&e.Artifacts, "artifacts", "None: CI run linked", "artifact, screenshot, or log URLs; None: reason")
	fs.StringVar(&e.CIURL, "ci-url", "", "CI run URL; read from the PR checks when omitted")
	number, err := parseIssueArg(fs, args, false)
	if err != nil {
		return errors.New("usage: quipu done N [--pr URL] [--criteria TEXT --commands TEXT --documentation TEXT --risks TEXT --boundary TEXT] [--json]")
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	// A dispatcher may finish work from another checkout, so act as the
	// worktree that holds this Issue's lease when there is one.
	w := here.holding(number)
	actor, err := resolveActor(w, *actorFlag, number)
	if err != nil {
		return err
	}
	state, err := loadLiveWork(c, number)
	if err != nil {
		return err
	}
	if state.Status == "Done" {
		return printFlowResult(flowResult{Issue: number, Status: "Done", Next: "Pick the next task: quipu start --next"}, *asJSON)
	}
	var pull pullRequest
	if state.Status == "Claimed" || state.Status == "In progress" || state.Status == "In review" {
		if pull, err = findPullRequest(c, *prFlag, state.Branch); err != nil {
			return err
		}
	}
	if state.Status == "Claimed" || state.Status == "In progress" {
		if pull.State == "MERGED" {
			return flowErr("pr-merged-early", "PR %s merged before #%d was moved to review", pull.URL, number)
		}
		if pull.State != "OPEN" {
			return flowErr("pr-closed", "PR %s is %s", pull.URL, pull.State)
		}
		if state.Status == "Claimed" {
			if err := sendAndWait(c, number, actor, writer.Request{Action: "start"}, *wait); err != nil {
				return err
			}
		}
		if err := sendAndWait(c, number, actor, writer.Request{Action: "pr.link", PR: pull.URL}, *wait); err != nil {
			return err
		}
		if state, err = loadLiveWork(c, number); err != nil {
			return err
		}
	}
	if state.Status == "In review" {
		if pull.State == "CLOSED" {
			return flowErr("pr-closed", "PR %s was closed without merging", pull.URL)
		}
		if pull.State != "MERGED" {
			return printFlowResult(flowResult{Issue: number, Status: "In review", Branch: state.Branch, PR: pull.URL, Next: fmt.Sprintf("The owner reviews and merges %s; never merge it yourself. Address review comments by pushing to %s (rerun quipu start %d first if the guard reports the lease). After the merge, run: quipu done %d --json --criteria ... --commands ... --documentation ... --risks ... --boundary ...", pull.URL, state.Branch, number, number)}, *asJSON)
		}
		e.FinalSHA = pull.MergeCommit
		if e.CIURL == "" {
			e.CIURL = pull.CIURL
		}
		if err := promptMissingEvidence(&e, os.Stdin, os.Stderr); err != nil {
			return err
		}
		if err := sendAndWait(c, number, actor, writer.Request{Action: "evidence.submit", PR: pull.URL, Evidence: &e}, *wait); err != nil {
			return err
		}
		if state, err = loadLiveWork(c, number); err != nil {
			return err
		}
	}
	if state.Status != "Evidence pending" {
		return flowErr("not-active", "#%d is %q; done applies to work In progress, In review, or Evidence pending", number, state.Status)
	}
	if err := sendAndWait(c, number, actor, writer.Request{Action: "status", Status: "Done"}, *wait); err != nil {
		return err
	}
	w.removeLease(number)
	result := flowResult{Issue: number, Status: "Done", PR: pull.URL, Next: "Closed. Pick the next task: quipu start --next"}
	if w.linked() {
		// Never delete the worktree an agent may still be running in.
		result.Worktree = w.Top
		result.Next = fmt.Sprintf("Closed. When nothing runs in it, remove the worktree: git worktree remove %s. Pick the next task: quipu start --next", w.Top)
	}
	return printFlowResult(result, *asJSON)
}

// ---- quipu propose / quipu ready ----

// proposeCommand is how an agent suggests new work: a marked comment on the
// parent Issue for the owner, never an Issue of its own.
func proposeCommand(args []string) error {
	fs := flag.NewFlagSet("propose", flag.ContinueOnError)
	parent := fs.Int("parent", 0, "Issue the proposal belongs under (an Epic or Story)")
	title := fs.String("title", "", "proposed work title")
	body := fs.String("body", "", "why, scope, and acceptance criteria")
	bodyFile := fs.String("body-file", "", "file with the proposal body")
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bodyFile != "" {
		raw, err := os.ReadFile(*bodyFile)
		if err != nil {
			return err
		}
		*body = string(raw)
	}
	if *parent < 1 || strings.TrimSpace(*title) == "" || strings.TrimSpace(*body) == "" {
		return errors.New("usage: quipu propose --parent N --title TEXT (--body TEXT | --body-file FILE) [--json]")
	}
	if strings.Contains(*body, "quipu-request") || strings.Contains(*body, "quipu-receipt") {
		return errors.New("a proposal cannot contain Quipu request or receipt markers")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	actor, err := resolveActor(here, "", 0)
	if err != nil {
		return err
	}
	comment := fmt.Sprintf("<!-- quipu-proposal:v1 -->\n**Proposed work:** %s\n\n_Proposed by %s. The owner decides whether it becomes an Issue._\n\n%s\n", strings.TrimSpace(*title), actor, strings.TrimSpace(*body))
	if _, err := github.NewClient().CreateComment(context.Background(), c.Owner, c.Repository, *parent, comment); err != nil {
		return err
	}
	return printFlowResult(flowResult{Issue: *parent, Status: "Proposed", Next: "The owner decides. Continue your current work, or run quipu start --next --json."}, *asJSON)
}

// readyCommand is the owner's way to make Backlog work Ready through the
// Writer, so the board never needs hand edits.
func readyCommand(args []string) error {
	fs := flag.NewFlagSet("ready", flag.ContinueOnError)
	wait := fs.Duration("wait", 3*time.Minute, "how long to wait for the Writer receipt")
	asJSON := jsonFlag(fs)
	number, err := parseIssueArg(fs, args, false)
	if err != nil {
		return errors.New("usage: quipu ready N [--json]  (owner only)")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	actor, err := resolveActor(here, "", number)
	if err != nil {
		return err
	}
	if err := sendAndWait(c, number, actor, writer.Request{Action: "status", Status: "Ready"}, *wait); err != nil {
		return err
	}
	return printFlowResult(flowResult{Issue: number, Status: "Ready", Next: fmt.Sprintf("Agents can now take #%d with quipu start --next --json.", number)}, *asJSON)
}

type pullRequest struct {
	URL, State, MergeCommit, CIURL string
}

func findPullRequest(c model.Config, explicit, branch string) (pullRequest, error) {
	target := explicit
	if target == "" {
		if branch == "" {
			return pullRequest{}, flowErr("pr-missing", "the work has no branch; pass --pr URL")
		}
		output, err := ghOutput("pr", "list", "--repo", c.Owner+"/"+c.Repository, "--head", branch, "--state", "all", "--json", "url,state", "--limit", "5")
		if err != nil {
			return pullRequest{}, err
		}
		var pulls []struct{ URL, State string }
		if err := json.Unmarshal(output, &pulls); err != nil {
			return pullRequest{}, err
		}
		target = pickPullRequest(pulls)
		if target == "" {
			return pullRequest{}, flowErr("pr-missing", "no PR found from branch %s", branch)
		}
	}
	output, err := ghOutput("pr", "view", target, "--repo", c.Owner+"/"+c.Repository, "--json", "url,state,mergeCommit,statusCheckRollup")
	if err != nil {
		return pullRequest{}, err
	}
	var view struct {
		URL         string `json:"url"`
		State       string `json:"state"`
		MergeCommit *struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
		Checks []prCheck `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(output, &view); err != nil {
		return pullRequest{}, err
	}
	pull := pullRequest{URL: view.URL, State: view.State, CIURL: passingCIURL(view.Checks)}
	if view.MergeCommit != nil {
		pull.MergeCommit = view.MergeCommit.OID
	}
	return pull, nil
}

// pickPullRequest prefers an open or merged PR over closed attempts.
func pickPullRequest(pulls []struct{ URL, State string }) string {
	for _, want := range []string{"OPEN", "MERGED"} {
		for _, pull := range pulls {
			if pull.State == want {
				return pull.URL
			}
		}
	}
	return ""
}

type prCheck struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"detailsUrl"`
}

// passingCIURL returns a successful check run URL only when no check failed,
// preferring the test check: the Writer accepts only a run on this PR.
func passingCIURL(checks []prCheck) string {
	url := ""
	for _, check := range checks {
		switch check.Conclusion {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			if check.Conclusion == "SUCCESS" && (url == "" || check.Name == "test") {
				url = check.DetailsURL
			}
		default:
			return ""
		}
	}
	return url
}

// promptMissingEvidence asks only for what a person must judge. Values that
// GitHub already knows (PR, merge SHA, CI run) are filled before this runs.
func promptMissingEvidence(e *writer.Evidence, in io.Reader, out io.Writer) error {
	fields := []struct {
		value  *string
		prompt string
	}{
		{&e.Criteria, "Acceptance criteria result"},
		{&e.Commands, "Commands you ran to verify"},
		{&e.Documentation, "Documentation changed (or None: reason)"},
		{&e.Risks, "Remaining risks (or None)"},
		{&e.Boundary, "Proven where (local|CI|target host|provider|staging|production|release)"},
		{&e.CIURL, "CI run URL"},
	}
	interactive := isTerminal(in)
	reader := bufio.NewReader(in)
	var missing []string
	for _, field := range fields {
		if strings.TrimSpace(*field.value) != "" {
			continue
		}
		if !interactive {
			missing = append(missing, field.prompt)
			continue
		}
		fmt.Fprintf(out, "%s: ", field.prompt)
		line, _ := reader.ReadString('\n')
		*field.value = strings.TrimSpace(line)
		if *field.value == "" {
			missing = append(missing, field.prompt)
		}
	}
	if len(missing) > 0 {
		return flowErr("evidence-missing", "evidence incomplete: %s", strings.Join(missing, "; "))
	}
	return e.Validate()
}

func isTerminal(in io.Reader) bool {
	file, ok := in.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// ---- shared helpers ----

func parseIssueArg(fs *flag.FlagSet, args []string, optional bool) (int, error) {
	// Accept the Issue number before or after flags: `quipu start 12 --wait 1m`.
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return 0, err
		}
		args = fs.Args()
		if len(args) > 0 {
			positional = append(positional, args[0])
			args = args[1:]
		}
	}
	if optional && len(positional) == 0 {
		return 0, nil
	}
	if len(positional) != 1 {
		return 0, errors.New("exactly one Issue number required")
	}
	number, err := strconv.Atoi(strings.TrimPrefix(positional[0], "#"))
	if err != nil || number < 1 {
		return 0, fmt.Errorf("invalid Issue number %q", positional[0])
	}
	return number, nil
}

// resolveActor picks who holds the lease for work done in worktree w. Many
// agents can share one GitHub login, so the identity belongs to the worktree:
// the lease already recorded there, then QUIPU_ACTOR, then
// provider/login/worktree-id. A new session in the same worktree therefore
// resumes its work, and two worktrees never share a lease.
func resolveActor(w worktree, explicit string, number int) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if lease, err := w.readLease(); err == nil && lease.Issue == number {
		return lease.Holder, nil
	}
	if actor := strings.TrimSpace(os.Getenv("QUIPU_ACTOR")); actor != "" {
		return actor, nil
	}
	output, err := ghOutput("api", "user", "--jq", ".login")
	if err != nil {
		return "", fmt.Errorf("resolve GitHub login (pass --actor): %w", err)
	}
	id, err := w.agentID()
	if err != nil {
		return "", err
	}
	return agentActor(strings.TrimSpace(string(output)), os.Getenv("QUIPU_AGENT"), id), nil
}

// agentActor builds an identity the Writer matches to the comment author:
// provider/login/worktree-id, where the provider is QUIPU_AGENT's first
// segment (set by the agent's session hook) and defaults to "agent".
func agentActor(login, agent, id string) string {
	provider, _, _ := strings.Cut(strings.Trim(strings.TrimSpace(agent), "/"), "/")
	if provider == "" {
		provider = "agent"
	}
	return provider + "/" + login + "/" + id
}

func issueTitle(c model.Config, number int) (string, error) {
	output, err := ghOutput("issue", "view", strconv.Itoa(number), "--repo", c.Owner+"/"+c.Repository, "--json", "title", "--jq", ".title")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// errRejected marks a request the Writer refused, such as a lost claim race.
var errRejected = errors.New("rejected by Writer")

// errNoReceipt marks a request still unanswered when the wait ran out; the
// Writer may yet accept it.
var errNoReceipt = errors.New("has no Writer receipt")

// sendAndWait posts one Writer request built from fresh live state and waits
// for the Writer receipt, so callers can chain lifecycle steps safely.
func sendAndWait(c model.Config, number int, actor string, r writer.Request, wait time.Duration) error {
	state, err := loadLiveWork(c, number)
	if err != nil {
		return err
	}
	fingerprint, err := fingerprintLiveWork(state)
	if err != nil {
		return err
	}
	r.ID, r.IssueID, r.Actor, r.ExpectedFingerprint = fmt.Sprintf("%d", time.Now().UnixNano()), state.IssueID, actor, fingerprint
	body, err := writer.RenderRequest(r)
	if err != nil {
		return err
	}
	client := github.NewClient()
	if _, err := client.CreateComment(context.Background(), c.Owner, c.Repository, number, body); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "#%d %s requested; waiting for Writer...\n", number, r.Action)
	deadline := time.Now().Add(wait)
	for {
		comments, err := client.ListComments(context.Background(), c.Owner, c.Repository, number)
		if err != nil {
			return err
		}
		if receipt, ok := receiptFor(receiptTrust(c).Receipts(comments), r.ID); ok {
			if receipt.Result != "accepted" {
				e := flowErr(rejectionCode(receipt.Detail), "#%d %s rejected by the Writer: %s", number, r.Action, receipt.Detail)
				e.cause = errRejected
				return e
			}
			return nil
		}
		if time.Now().After(deadline) {
			e := flowErr("no-receipt", "#%d %s request %s has no Writer receipt after %s", number, r.Action, r.ID, wait)
			e.cause = errNoReceipt
			return e
		}
		time.Sleep(5 * time.Second)
	}
}

// receiptFor finds the receipt for requestID among trusted receipts.
func receiptFor(receipts []writer.Receipt, requestID string) (writer.Receipt, bool) {
	for _, receipt := range receipts {
		if receipt.RequestID == requestID {
			return receipt, true
		}
	}
	return writer.Receipt{}, false
}
