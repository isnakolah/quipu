package main

// The required `policy` check. It runs on every PR event and again from the
// Writer's watchdog whenever Issue or lease state changes, and lists every
// violation with its fix so an agent can repair the PR in one pass.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	policycheck "github.com/isnakolah/quipu/internal/policy"
	"github.com/isnakolah/quipu/internal/writer"
)

// ownerApprovedLabel lets a PR change Quipu's own enforcement files. Only the
// owner adds it; the agent guard denies label changes.
const ownerApprovedLabel = "quipu:owner-approved"

// protectedPaths hold the rules agents run under: config, workflows, hooks.
var protectedPaths = []string{rootConfigPath, ".github/quipu/", ".github/workflows/", ".claude/settings.json", ".codex/",
	"plugins/claude/quipu/hooks/", "plugins/codex/quipu/hooks/",
	"plugins/claude/quipu/scripts/pre-write-guard", "plugins/codex/quipu/scripts/pre-write-guard"}

// implementableKinds are the Issue kinds a PR may implement.
var implementableKinds = map[string]bool{"Story": true, "Task": true, "Defect": true, "Contract": true}

type pullFacts struct {
	Number  int
	URL     string
	Title   string
	Body    string
	Author  string
	IsBot   bool
	Base    string
	Head    string
	HeadSHA string
	State   string
	Commits []string
	Files   []string
	Labels  []string
}

func loadPullFacts(c model.Config, pr string) (pullFacts, error) {
	output, err := ghOutput("pr", "view", pr, "--repo", c.Owner+"/"+c.Repository, "--json",
		"number,url,title,body,author,baseRefName,headRefName,headRefOid,state,commits,files,labels")
	if err != nil {
		return pullFacts{}, err
	}
	var raw struct {
		Number      int    `json:"number"`
		URL         string `json:"url"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		BaseRefName string `json:"baseRefName"`
		HeadRefName string `json:"headRefName"`
		HeadRefOid  string `json:"headRefOid"`
		State       string `json:"state"`
		Author      struct {
			Login string `json:"login"`
			IsBot bool   `json:"is_bot"`
		} `json:"author"`
		Commits []struct {
			Headline string `json:"messageHeadline"`
			Body     string `json:"messageBody"`
		} `json:"commits"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(output, &raw); err != nil {
		return pullFacts{}, err
	}
	pull := pullFacts{Number: raw.Number, URL: raw.URL, Title: raw.Title, Body: raw.Body, Author: raw.Author.Login, IsBot: raw.Author.IsBot,
		Base: raw.BaseRefName, Head: raw.HeadRefName, HeadSHA: raw.HeadRefOid, State: raw.State}
	for _, commit := range raw.Commits {
		pull.Commits = append(pull.Commits, commit.Headline+"\n"+commit.Body)
	}
	for _, file := range raw.Files {
		pull.Files = append(pull.Files, file.Path)
	}
	for _, label := range raw.Labels {
		pull.Labels = append(pull.Labels, label.Name)
	}
	return pull, nil
}

// prViolations lists every rule the PR breaks. The Issue is the one the
// body references; loadWork loads it.
func prViolations(c model.Config, pull pullFacts, loadWork func(int) (liveWork, error), now time.Time) []string {
	var violations []string
	add := func(format string, args ...any) { violations = append(violations, fmt.Sprintf(format, args...)) }
	if exemptBot(c, pull) {
		if file := unapprovedProtectedChange(c, pull); file != "" {
			add("PR changes %s, which governs how agents work. Fix: the owner reviews it and adds the %s label", file, ownerApprovedLabel)
		}
		return violations
	}
	epic, onEpic := policycheck.BranchEpic(pull.Head)
	batch := onEpic && c.BatchesByEpic()
	referenced := policycheck.ReferencedIssues
	if batch {
		referenced = policycheck.ReferencedIssueSet
	}
	issues, err := referenced(pull.Body)
	if err != nil {
		add("%v. Fix: edit the PR body to say exactly `Refs #N` for the Issue this branch implements", err)
	}
	if err := policycheck.NoClosingKeywords(pull.Title, "PR title"); err != nil {
		add("%v. Fix: retitle the PR", err)
	}
	for _, message := range pull.Commits {
		if err := policycheck.NoClosingKeywords(message, "a commit message"); err != nil {
			add("%v. Fix: reword the commit (it would close the Issue on merge)", err)
			break
		}
	}
	if pull.Base != c.DefaultBranch {
		add("PR targets %q, not %q. Fix: change the base branch", pull.Base, c.DefaultBranch)
	}
	number, ok := policycheck.BranchIssue(pull.Head)
	switch {
	case ok || batch:
	case onEpic:
		add("branch %q is a rolling Epic branch, but this repository does not set batches: epic. Fix: work comes from `quipu start N`, which names the branch", pull.Head)
	default:
		add("branch %q is not NNN/short-description. Fix: work comes from `quipu start N`, which names the branch", pull.Head)
	}
	if file := unapprovedProtectedChange(c, pull); file != "" {
		add("PR changes %s, which governs how agents work. Fix: the owner reviews it and adds the %s label", file, ownerApprovedLabel)
	}
	if len(issues) == 0 || (!batch && len(issues) != 1) {
		return violations
	}
	if c.CommitFormat == policycheck.IssuePrefix {
		if _, _, err := policycheck.IssuePrefixMessage(issues[0], pull.Title); err != nil {
			add("%v; the merge commit is built from it. Fix: retitle the PR", err)
		}
		for _, message := range pull.Commits {
			if err := prefixedForAny(message, issues); err != nil {
				add("%v. Fix: reword the commit (git rebase -i, then push --force-with-lease)", err)
				break
			}
		}
	}
	if ok && number != issues[0] {
		add("branch %q maps to #%d but the body references #%d. Fix: reference the branch's Issue", pull.Head, number, issues[0])
	}
	epics := map[int][]int{}
	for _, issue := range issues {
		state, err := loadWork(issue)
		if err != nil {
			add("cannot load #%d: %v", issue, err)
			continue
		}
		if batch {
			epics[state.ParentNumber] = append(epics[state.ParentNumber], issue)
		}
		violations = append(violations, leaseViolations(issue, state, pull, now)...)
	}
	if batch {
		violations = append(violations, epicViolations(epic, epics)...)
	}
	return violations
}

// prefixedForAny accepts a commit prefixed with any of the referenced Issues.
func prefixedForAny(message string, issues []int) error {
	var first error
	for _, issue := range issues {
		_, err := policycheck.CheckIssuePrefix(message, issue)
		if err == nil {
			return nil
		}
		if first == nil {
			first = err
		}
	}
	return first
}

// leaseViolations lists why the PR may not implement Issue number: the Issue
// itself, its status, and its lease on the PR's branch for the PR's author.
func leaseViolations(number int, state liveWork, pull pullFacts, now time.Time) []string {
	violations := workProblems(number, state)
	add := func(format string, args ...any) { violations = append(violations, fmt.Sprintf(format, args...)) }
	switch state.Status {
	case "In progress", "In review":
	default:
		add("#%d is %q; a PR needs work In progress or In review. Fix: `quipu start %d`", number, state.Status, number)
	}
	if state.Holder == "" || state.Branch != pull.Head {
		add("#%d is not leased on branch %s. Fix: `quipu start %d` in the worktree doing the work", number, pull.Head, number)
	} else if !writer.ActorMatchesLogin(state.Holder, pull.Author) {
		add("#%d is held by %q, but the PR author is %q. Fix: the lease holder opens the PR", number, state.Holder, pull.Author)
	}
	if state.Status == "In progress" {
		if expires, err := time.Parse(time.RFC3339, state.Expiry); err != nil || !expires.After(now) {
			add("#%d lease expired. Fix: `quipu start %d` renews it", number, number)
		}
	}
	return violations
}

// epicViolations checks that a rolling branch's PR carries leaves of its own
// Epic only. byParent maps each parent Epic to the Issues under it.
func epicViolations(epic int, byParent map[int][]int) []string {
	var violations []string
	if len(byParent) > 1 {
		var parents []string
		for parent := range byParent {
			parents = append(parents, fmt.Sprintf("#%d", parent))
		}
		sort.Strings(parents)
		violations = append(violations, fmt.Sprintf("PR references leaf Issues of Epics %s; a rolling branch carries one Epic's leaves. Fix: open one PR per Epic", strings.Join(parents, " and ")))
	}
	for parent, issues := range byParent {
		if parent != epic {
			for _, issue := range issues {
				violations = append(violations, fmt.Sprintf("#%d is not a leaf of Epic #%d, whose rolling branch this is. Fix: take it out of the PR body", issue, epic))
			}
		}
	}
	return violations
}

// workProblems lists why Issue number cannot be implemented by a PR at all,
// whoever holds it. `policy` reports them on the PR; `quipu start` refuses the
// claim with the same words, before any work is done.
func workProblems(number int, state liveWork) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if state.IssueState != "OPEN" {
		add("#%d is closed. Fix: work only on open Issues", number)
	}
	if !implementableKinds[state.Kind] {
		add("#%d is a %s; only Stories, Tasks, Defects, and Contracts are implemented by PRs", number, orUnknown(state.Kind))
	}
	if state.Children > 0 {
		add("#%d has sub-issues; implement a leaf Issue instead", number)
	}
	if state.ParentID == "" {
		add("#%d has no native parent. Fix: the owner links it under its Epic or Story", number)
	}
	if err := model.ValidateWorkContract(state.Body); err != nil {
		add("#%d work contract invalid: %v", number, err)
	}
	for _, blocker := range state.Blockers {
		if blocker.State != "CLOSED" {
			add("#%d is blocked by an open Issue", number)
			break
		}
	}
	return problems
}

// exemptBot reports whether a bot listed in policy_exempt_authors opened
// the PR. gh names app bots "app/dependabot"; config uses "dependabot[bot]".
func exemptBot(c model.Config, pull pullFacts) bool {
	if !pull.IsBot {
		return false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(pull.Author, "app/"), "[bot]")
	for _, author := range c.PolicyExemptAuthors {
		if strings.EqualFold(strings.TrimSuffix(author, "[bot]"), name) {
			return true
		}
	}
	return false
}

// unapprovedProtectedChange is the first of Quipu's own files the PR changes
// without the owner's approval label, or "". With workflow_changes: flagged,
// workflows other than Quipu's own don't count; the digest lists them instead.
func unapprovedProtectedChange(c model.Config, pull pullFacts) string {
	if hasLabel(pull.Labels, ownerApprovedLabel) {
		return ""
	}
	for _, file := range pull.Files {
		if ownerOnlyFile(c, file) {
			return file
		}
	}
	return ""
}

func protectedFile(path string) bool {
	for _, protected := range protectedPaths {
		if path == protected || strings.HasPrefix(path, protected) {
			return true
		}
	}
	return false
}

func hasLabel(labels []string, name string) bool {
	for _, label := range labels {
		if label == name {
			return true
		}
	}
	return false
}

// printFlagged notes the workflow changes the owner will see in the digest.
func printFlagged(files []string) {
	prefix := ""
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		prefix = "::notice::"
	}
	for _, file := range files {
		fmt.Printf("%sflagged for the owner (workflow_changes: flagged): %s is listed in the digest\n", prefix, file)
	}
}

func orUnknown(kind string) string {
	if kind == "" {
		return "Issue without a Kind"
	}
	return kind
}

func policyCommand(args []string) error {
	if len(args) == 0 || args[0] != "pr" {
		return errors.New("usage: quipu policy pr --pr URL [--json]")
	}
	fs := flag.NewFlagSet("policy pr", flag.ContinueOnError)
	pr := fs.String("pr", "", "pull request URL or number")
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *pr == "" {
		return errors.New("policy pr requires --pr")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	pull, err := loadPullFacts(c, *pr)
	if err != nil {
		return err
	}
	violations := prViolations(c, pull, func(n int) (liveWork, error) { return loadLiveWork(c, n) }, time.Now())
	flagged := append([]string{}, flaggedWorkflowFiles(c, pull.Files)...)
	report := map[string]any{"valid": len(violations) == 0, "pr": pull.URL, "branch": pull.Head, "violations": append([]string{}, violations...), "flagged": flagged}
	if *asJSON {
		if err := printValue(report, true); err != nil {
			return err
		}
	} else if len(violations) == 0 {
		fmt.Printf("%s follows the Quipu flow\n", pull.URL)
		printFlagged(flagged)
	} else {
		prefix := ""
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			prefix = "::error::"
		}
		fmt.Printf("%s breaks %d Quipu rule(s):\n", pull.URL, len(violations))
		for _, violation := range violations {
			fmt.Println(prefix + violation)
		}
	}
	if len(violations) > 0 {
		return errReported
	}
	return nil
}
