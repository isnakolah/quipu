package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/github-first-delivery/internal/github"
	"github.com/isnakolah/github-first-delivery/internal/writer"
)

func TestBuildStatusReportGroupsColumnsAndAttention(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	items := []statusItem{
		{Number: 1, Title: "EPIC — Goal", Kind: "Epic", ChildTotal: 3, ChildCompleted: 3},
		{Number: 5, Title: "EPIC — Next", Kind: "Epic", ChildTotal: 2, ChildCompleted: 1},
		{Number: 7, Title: "Active", Kind: "Story", Status: "In progress", LeaseHolder: "alice", LeaseExpires: "2026-10-28T13:00:00Z"},
		{Number: 8, Title: "Stale", Kind: "Story", Status: "Claimed", LeaseHolder: "bob", LeaseExpires: "2026-10-28T11:00:00Z"},
		{Number: 9, Title: "Review", Kind: "Story", Status: "In review"},
		{Number: 10, Title: "Verify", Kind: "Story", Status: "Evidence pending"},
		{Number: 11, Title: "Low", Kind: "Task", Status: "Ready", Priority: "P3"},
		{Number: 12, Title: "High", Kind: "Task", Status: "Ready", Priority: "P1"},
		{Number: 13, Title: "Later", Kind: "Task", Status: "Backlog"},
		{Number: 14, Title: "Orphan", Kind: "Task"},
	}
	report := buildStatusReport("o/r", items, now)
	if len(report.Goals) != 2 || len(report.InProgress) != 2 || len(report.InReview) != 1 || len(report.Verifying) != 1 || report.Backlog != 1 {
		t.Fatalf("unexpected grouping: %+v", report)
	}
	if report.Ready[0].Number != 12 {
		t.Fatalf("Ready not ordered by priority: %+v", report.Ready)
	}
	attention := strings.Join(report.Attention, "\n")
	for _, want := range []string{"#1 all 3 children are done", "#8 lease expired", "#14 has no Project Status"} {
		if !strings.Contains(attention, want) {
			t.Fatalf("attention missing %q:\n%s", want, attention)
		}
	}
	if strings.Contains(attention, "#5") || strings.Contains(attention, "#7") {
		t.Fatalf("unexpected attention:\n%s", attention)
	}
	output := renderStatus(report, now)
	for _, want := range []string{"Goals (2)", "3/3 done", "alice (1h0m0s left)", "Ready to pick up (2)", "Backlog: 1", "! #8 lease expired"} {
		if !strings.Contains(output, want) {
			t.Fatalf("render missing %q:\n%s", want, output)
		}
	}
}

func TestBranchName(t *testing.T) {
	for title, want := range map[string]string{
		"US-016 — Test Writer correctness with disposable live GitHub repository": "016/test-writer-correctness-with-disposable",
		"Fix: login page!": "016/fix-login-page",
		"—":                "016/work",
	} {
		if got := branchName(16, title); got != want {
			t.Fatalf("branchName(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestPassingCIURL(t *testing.T) {
	ok := []prCheck{{Conclusion: "SKIPPED", DetailsURL: "skip"}, {Conclusion: "SUCCESS", DetailsURL: "https://ci/1"}, {Conclusion: "SUCCESS", DetailsURL: "https://ci/2"}}
	if got := passingCIURL(ok); got != "https://ci/1" {
		t.Fatalf("got %q", got)
	}
	if got := passingCIURL(append(ok, prCheck{Conclusion: "FAILURE"})); got != "" {
		t.Fatalf("failing checks returned %q", got)
	}
	if got := passingCIURL(append(ok, prCheck{Conclusion: ""})); got != "" {
		t.Fatalf("pending checks returned %q", got)
	}
}

func TestPickPullRequestPrefersOpenThenMerged(t *testing.T) {
	pulls := []struct{ URL, State string }{{"closed", "CLOSED"}, {"merged", "MERGED"}, {"open", "OPEN"}}
	if got := pickPullRequest(pulls); got != "open" {
		t.Fatalf("got %q", got)
	}
	if got := pickPullRequest(pulls[:2]); got != "merged" {
		t.Fatalf("got %q", got)
	}
	if got := pickPullRequest(pulls[:1]); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestPromptMissingEvidenceNonInteractiveListsGaps(t *testing.T) {
	e := writer.Evidence{FinalSHA: "abc", CIURL: "https://ci/1", Environments: "CI", Artifacts: "None: CI", Criteria: "all met"}
	err := promptMissingEvidence(&e, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "Commands you ran") || strings.Contains(err.Error(), "Acceptance") {
		t.Fatalf("unexpected error: %v", err)
	}
	e.Commands, e.Documentation, e.Risks, e.Boundary = "go test ./...", "None: internal", "None", "CI"
	if err := promptMissingEvidence(&e, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestParseIssueArgAcceptsFlagsEitherSide(t *testing.T) {
	for _, args := range [][]string{{"12", "--branch", "012/x"}, {"--branch", "012/x", "#12"}} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		branch := fs.String("branch", "", "")
		number, err := parseIssueArg(fs, args, false)
		if err != nil || number != 12 || *branch != "012/x" {
			t.Fatalf("args %v: number=%d branch=%q err=%v", args, number, *branch, err)
		}
	}
	if _, err := parseIssueArg(flag.NewFlagSet("t", flag.ContinueOnError), []string{"1", "2"}, false); err == nil {
		t.Fatal("expected error for two numbers")
	}
}

func TestReceiptFor(t *testing.T) {
	body, err := writer.RenderReceipt(writer.Receipt{RequestID: "r1", Result: "rejected", Detail: "stale", At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	receipt, ok := receiptFor([]github.Comment{{Body: "chatter"}, {Body: body}}, "r1")
	if !ok || receipt.Result != "rejected" {
		t.Fatalf("got %+v %v", receipt, ok)
	}
	if _, ok := receiptFor([]github.Comment{{Body: body}}, "r2"); ok {
		t.Fatal("matched wrong request")
	}
}

func TestLeaseContextFromEnvOrFile(t *testing.T) {
	dir := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s", output)
	}
	t.Chdir(dir)
	t.Setenv("GFD_LEASE_ISSUE", "")
	t.Setenv("GFD_LEASE_HOLDER", "")
	t.Setenv("GFD_LEASE_BRANCH", "")
	if _, err := currentLeaseContext(); err == nil || !strings.Contains(err.Error(), "gfd start") {
		t.Fatalf("expected missing-lease error, got %v", err)
	}
	want := leaseContext{Issue: 7, Holder: "alice", Branch: "007/x"}
	if err := writeLeaseFile(want); err != nil {
		t.Fatal(err)
	}
	if got, err := currentLeaseContext(); err != nil || got != want {
		t.Fatalf("file lease: %+v %v", got, err)
	}
	t.Setenv("GFD_LEASE_ISSUE", "9")
	t.Setenv("GFD_LEASE_HOLDER", "bob")
	t.Setenv("GFD_LEASE_BRANCH", "009/y")
	if got, err := currentLeaseContext(); err != nil || got.Issue != 9 || got.Holder != "bob" {
		t.Fatalf("env lease should win: %+v %v", got, err)
	}
	t.Setenv("GFD_LEASE_BRANCH", "")
	if _, err := currentLeaseContext(); err == nil {
		t.Fatal("partial env lease must be denied")
	}
	t.Setenv("GFD_LEASE_ISSUE", "")
	t.Setenv("GFD_LEASE_HOLDER", "")
	removeLeaseFile(8)
	if _, err := readLeaseFile(); err != nil {
		t.Fatal("removing a different Issue's lease must keep the file")
	}
	removeLeaseFile(7)
	if _, err := readLeaseFile(); err == nil {
		t.Fatal("lease file should be removed")
	}
}

func TestAgentActorGivesEachSessionItsOwnIdentity(t *testing.T) {
	for agent, want := range map[string]string{
		"":                "octo",
		"claude":          "claude/octo",
		"claude/ab12cd34": "claude/octo/ab12cd34",
		"codex/a/b":       "codex/octo/a-b",
	} {
		got := agentActor("octo", agent)
		if got != want {
			t.Fatalf("agentActor(%q) = %q, want %q", agent, got, want)
		}
		// The Writer must still match every form to the comment author.
		if !writer.ActorMatchesLogin(got, "octo") {
			t.Fatalf("Writer would reject actor %q for login octo", got)
		}
	}
}

func TestResolveActorPrefersWorktreeLeaseThenEnv(t *testing.T) {
	dir := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s", output)
	}
	t.Chdir(dir)
	t.Setenv("GFD_ACTOR", "codex/octo/env")
	if err := writeLeaseFile(leaseContext{Issue: 7, Holder: "claude/octo/lease", Branch: "007/x"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolveActor("", 7); got != "claude/octo/lease" {
		t.Fatalf("lease holder should win for its Issue, got %q", got)
	}
	if got, _ := resolveActor("", 8); got != "codex/octo/env" {
		t.Fatalf("GFD_ACTOR should apply to other Issues, got %q", got)
	}
	if got, _ := resolveActor("explicit", 7); got != "explicit" {
		t.Fatalf("explicit actor should win, got %q", got)
	}
}

func TestNextWorkOrdersByPriorityThenNumber(t *testing.T) {
	contract := "<!-- work:v1 -->\n## Outcome\n## Scope\nIncludes:\nExcludes:\n## Acceptance criteria\n## Required evidence\nVerification boundary: CI\n## Documentation impact\n## Source and relationships\n"
	item := func(number int, priority string) workCandidate {
		return workCandidate{readyWork: readyWork{Number: number, Priority: priority, Kind: "Task"}, Body: contract, ParentID: "P", Status: "Ready"}
	}
	got := nextWork([]workCandidate{item(3, "P2"), item(9, "P0"), item(1, "P2"), item(5, "")}, time.Now())
	order := []int{}
	for _, work := range got {
		order = append(order, work.Number)
	}
	if fmt.Sprint(order) != "[9 1 3 5]" {
		t.Fatalf("order = %v", order)
	}
}

func TestPrintFlowResultJSONCarriesNextStep(t *testing.T) {
	output, err := captureStdout(t, func() error {
		return printFlowResult(flowResult{Issue: 4, Status: "In review", PR: "https://x/pr/1", Next: "wait"}, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded flowResult
	if err := json.Unmarshal([]byte(output), &decoded); err != nil || decoded.Next != "wait" || decoded.Issue != 4 {
		t.Fatalf("decoded %+v err %v from %s", decoded, err, output)
	}
}
