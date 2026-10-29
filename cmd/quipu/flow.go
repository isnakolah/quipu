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
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

const leaseFileName = "quipu-lease"

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

func startCommand(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	actorFlag := fs.String("actor", "", "lease holder; defaults to QUIPU_ACTOR, then the GitHub login plus QUIPU_AGENT")
	branchFlag := fs.String("branch", "", "NNN/short-description branch; derived from the title when omitted")
	next := fs.Bool("next", false, "pick the highest-priority Ready, unblocked, unleased leaf")
	wait := fs.Duration("wait", 3*time.Minute, "how long to wait for each Writer receipt")
	asJSON := jsonFlag(fs)
	number, err := parseIssueArg(fs, args, true)
	if err != nil || (number == 0) != *next {
		return errors.New("usage: quipu start (N | --next) [--branch NNN/slug] [--actor ID] [--json]")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if !*next {
		result, err := startIssue(c, number, *actorFlag, *branchFlag, *wait)
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
	// Several agents may race for the same top item; the Writer accepts one
	// claim and rejects the rest, so a rejected claim moves on to the next.
	for i, item := range ready {
		if i == 3 {
			break
		}
		result, err := startIssue(c, item.Number, *actorFlag, "", *wait)
		if err == nil {
			return printFlowResult(result, *asJSON)
		}
		if !errors.Is(err, errRejected) {
			return err
		}
		fmt.Fprintf(os.Stderr, "#%d taken (%v); trying next\n", item.Number, err)
	}
	return printFlowResult(flowResult{Status: "Idle", Next: "No Ready, unblocked work. Ask the owner to make work Ready, or run quipu status."}, *asJSON)
}

// nextWork orders pick-up candidates by priority, then Issue number.
func nextWork(candidates []workCandidate, now time.Time) []readyWork {
	ready := selectReadyWork(candidates, now)
	sort.SliceStable(ready, func(i, j int) bool { return priorityRank(ready[i].Priority) < priorityRank(ready[j].Priority) })
	return ready
}

func startIssue(c model.Config, number int, actorFlag, branch string, wait time.Duration) (flowResult, error) {
	actor, err := resolveActor(actorFlag, number)
	if err != nil {
		return flowResult{}, err
	}
	state, err := loadLiveWork(c, number)
	if err != nil {
		return flowResult{}, err
	}
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
		return flowResult{}, fmt.Errorf("#%d is %q; only Ready work can be started", number, state.Status)
	}
	if state.Holder != actor {
		return flowResult{}, fmt.Errorf("#%d is held by %q, not %q", number, state.Holder, actor)
	}
	if expiry, err := time.Parse(time.RFC3339, state.Expiry); err == nil && time.Until(expiry) < renewWithin {
		if err := sendAndWait(c, number, actor, writer.Request{Action: "renew"}, wait); err != nil {
			return flowResult{}, err
		}
		if state, err = loadLiveWork(c, number); err != nil {
			return flowResult{}, err
		}
	}
	if err := switchBranch(state.Branch, c.DefaultBranch); err != nil {
		return flowResult{}, err
	}
	if err := writeLeaseFile(leaseContext{Issue: number, Holder: state.Holder, Branch: state.Branch}); err != nil {
		return flowResult{}, err
	}
	result := flowResult{Issue: number, Status: state.Status, Branch: state.Branch, Holder: state.Holder, LeaseExpires: state.Expiry}
	result.Next = fmt.Sprintf("Implement #%d on %s. Push and open a PR whose body says \"Refs #%d\", then run: quipu done %d. Rerun quipu start %d to renew the lease on long work.", number, state.Branch, number, number, number)
	if state.Status == "In review" {
		result.Next = fmt.Sprintf("Address review on %s, then run: quipu done %d", state.Branch, number)
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

func switchBranch(branch, base string) error {
	if exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil {
		return runGit("switch", branch)
	}
	_ = exec.Command("git", "fetch", "--quiet", "origin", base).Run()
	start := "origin/" + base
	if exec.Command("git", "rev-parse", "--verify", "--quiet", start).Run() != nil {
		start = base
	}
	return runGit("switch", "-c", branch, start)
}

func runGit(args ...string) error {
	output, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return nil
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
	c, err := loadConfig()
	if err != nil {
		return err
	}
	actor, err := resolveActor(*actorFlag, number)
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
			return fmt.Errorf("PR %s merged before review was recorded; link an open PR or ask an owner to recover #%d", pull.URL, number)
		}
		if pull.State != "OPEN" {
			return fmt.Errorf("PR %s is %s; reopen it or open a new one", pull.URL, pull.State)
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
		if pull.State != "MERGED" {
			return printFlowResult(flowResult{Issue: number, Status: "In review", Branch: state.Branch, PR: pull.URL, Next: fmt.Sprintf("Wait for review and merge of %s; address review comments on %s. After merge, run: quipu done %d --criteria ... --commands ... --documentation ... --risks ... --boundary ...", pull.URL, state.Branch, number)}, *asJSON)
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
		return fmt.Errorf("#%d is %q; done applies to work In progress, In review, or Verifying", number, state.Status)
	}
	if err := sendAndWait(c, number, actor, writer.Request{Action: "status", Status: "Done"}, *wait); err != nil {
		return err
	}
	removeLeaseFile(number)
	return printFlowResult(flowResult{Issue: number, Status: "Done", PR: pull.URL, Next: "Closed. Pick the next task: quipu start --next"}, *asJSON)
}

type pullRequest struct {
	URL, State, MergeCommit, CIURL string
}

func findPullRequest(c model.Config, explicit, branch string) (pullRequest, error) {
	target := explicit
	if target == "" {
		if branch == "" {
			return pullRequest{}, errors.New("work has no branch; pass --pr URL")
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
			return pullRequest{}, fmt.Errorf("no PR found from branch %s; push it and open a PR first", branch)
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

// passingCIURL returns a successful check run URL only when no check failed.
func passingCIURL(checks []prCheck) string {
	url := ""
	for _, check := range checks {
		switch check.Conclusion {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			if url == "" && check.Conclusion == "SUCCESS" {
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
		return fmt.Errorf("evidence incomplete: %s", strings.Join(missing, "; "))
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

// resolveActor picks who holds the lease. Several agents can share one GitHub
// login, so each session gets its own identity: the lease already recorded
// in this worktree, then QUIPU_ACTOR, then provider/login/session built from
// QUIPU_AGENT (set by the agent's session hook), then the bare login.
func resolveActor(explicit string, number int) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if lease, err := readLeaseFile(); err == nil && lease.Issue == number {
		return lease.Holder, nil
	}
	if actor := strings.TrimSpace(os.Getenv("QUIPU_ACTOR")); actor != "" {
		return actor, nil
	}
	output, err := ghOutput("api", "user", "--jq", ".login")
	if err != nil {
		return "", fmt.Errorf("resolve GitHub login (pass --actor): %w", err)
	}
	return agentActor(strings.TrimSpace(string(output)), os.Getenv("QUIPU_AGENT")), nil
}

// agentActor builds an identity the Writer matches to the comment author:
// "claude/session" becomes "claude/login/session".
func agentActor(login, agent string) string {
	agent = strings.Trim(strings.TrimSpace(agent), "/")
	if agent == "" {
		return login
	}
	provider, session, found := strings.Cut(agent, "/")
	if !found || session == "" {
		return provider + "/" + login
	}
	return provider + "/" + login + "/" + strings.ReplaceAll(session, "/", "-")
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
		if receipt, ok := receiptFor(comments, r.ID); ok {
			if receipt.Result != "accepted" {
				return fmt.Errorf("#%d %s %w: %s", number, r.Action, errRejected, receipt.Detail)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("#%d %s request %s has no Writer receipt after %s; check the Quipu Writer workflow, then rerun", number, r.Action, r.ID, wait)
		}
		time.Sleep(5 * time.Second)
	}
}

func receiptFor(comments []github.Comment, requestID string) (writer.Receipt, bool) {
	for _, comment := range comments {
		receipt, err := writer.ParseReceipt(comment.Body)
		if err == nil && receipt.RequestID == requestID {
			return receipt, true
		}
	}
	return writer.Receipt{}, false
}

// ---- local lease context ----

// leaseContext is the session's claimed work. Hooks read it from
// QUIPU_LEASE_* variables or, when those are unset, from .git/quipu-lease written
// by `quipu start`. Either way the guard re-verifies it against live GitHub.
type leaseContext struct {
	Issue  int    `json:"issue"`
	Holder string `json:"holder"`
	Branch string `json:"branch"`
}

func currentLeaseContext() (leaseContext, error) {
	issue, holder, branch := os.Getenv("QUIPU_LEASE_ISSUE"), os.Getenv("QUIPU_LEASE_HOLDER"), os.Getenv("QUIPU_LEASE_BRANCH")
	if issue == "" && holder == "" && branch == "" {
		return readLeaseFile()
	}
	if issue == "" || holder == "" || branch == "" {
		return leaseContext{}, errors.New("quipu active lease issue, holder, or branch context missing; operation denied")
	}
	number, err := strconv.Atoi(issue)
	if err != nil || number < 1 {
		return leaseContext{}, errors.New("QUIPU_LEASE_ISSUE must be a positive Issue number")
	}
	return leaseContext{Issue: number, Holder: holder, Branch: branch}, nil
}

func leaseFilePath() (string, error) {
	output, err := exec.Command("git", "rev-parse", "--git-dir").Output()
	if err != nil {
		return "", errors.New("not inside a git repository")
	}
	return filepath.Join(strings.TrimSpace(string(output)), leaseFileName), nil
}

func readLeaseFile() (leaseContext, error) {
	missing := errors.New("quipu no active lease; run `quipu start N` first; operation denied")
	path, err := leaseFilePath()
	if err != nil {
		return leaseContext{}, missing
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return leaseContext{}, missing
	}
	var lease leaseContext
	if err := json.Unmarshal(raw, &lease); err != nil || lease.Issue < 1 || lease.Holder == "" || lease.Branch == "" {
		return leaseContext{}, errors.New("quipu lease file is malformed; rerun `quipu start N`; operation denied")
	}
	return lease, nil
}

func writeLeaseFile(lease leaseContext) error {
	path, err := leaseFilePath()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(lease)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}

func removeLeaseFile(number int) {
	if lease, err := readLeaseFile(); err == nil && lease.Issue == number {
		if path, err := leaseFilePath(); err == nil {
			_ = os.Remove(path)
		}
	}
}
