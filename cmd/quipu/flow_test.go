package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
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
	status := prCheck{Context: "policy", State: "SUCCESS"}
	if got := passingCIURL(append([]prCheck{status}, ok...)); got != "https://ci/1" {
		t.Fatalf("a successful commit status is no failure, got %q", got)
	}
	status.State = "PENDING"
	if got := passingCIURL(append(ok, status)); got != "" {
		t.Fatalf("a pending commit status returned %q", got)
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
	receipts := []writer.Receipt{{RequestID: "r0", Result: "accepted"}, {RequestID: "r1", Result: "rejected", Detail: "stale", At: time.Now().UTC()}}
	receipt, ok := receiptFor(receipts, "r1")
	if !ok || receipt.Result != "rejected" {
		t.Fatalf("got %+v %v", receipt, ok)
	}
	if _, ok := receiptFor(receipts, "r2"); ok {
		t.Fatal("matched wrong request")
	}
}

// gitRepo makes a repository with one commit on main and returns its
// symlink-free root, since git reports resolved paths.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s", args, output)
	}
}

func TestWorktreeLeaseFile(t *testing.T) {
	w, err := openWorktree(gitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.readLease(); err == nil || !strings.Contains(err.Error(), "quipu start") {
		t.Fatalf("expected missing-lease error, got %v", err)
	}
	want := leaseContext{Issue: 7, Holder: "alice", Branch: "007/x", Expires: "2026-10-29T12:00:00Z"}
	if err := w.writeLease(want); err != nil {
		t.Fatal(err)
	}
	if got, err := w.readLease(); err != nil || got != want {
		t.Fatalf("file lease: %+v %v", got, err)
	}
	w.removeLease(8)
	if _, err := w.readLease(); err != nil {
		t.Fatal("removing a different Issue's lease must keep the file")
	}
	w.removeLease(7)
	if _, err := w.readLease(); err == nil {
		t.Fatal("lease file should be removed")
	}
}

func TestEachWorktreeHasItsOwnStableAgentID(t *testing.T) {
	top := gitRepo(t)
	gitIn(t, top, "worktree", "add", "-q", "-b", "001/a", filepath.Join(top, "..", "wt-a"))
	main, err := openWorktree(top)
	if err != nil {
		t.Fatal(err)
	}
	linked, err := openWorktree(filepath.Join(top, "..", "wt-a"))
	if err != nil {
		t.Fatal(err)
	}
	if main.linked() || !linked.linked() {
		t.Fatalf("linked() main=%v linked=%v", main.linked(), linked.linked())
	}
	first, err := linked.agentID()
	if err != nil || len(first) != 8 {
		t.Fatalf("agentID = %q, %v", first, err)
	}
	if again, _ := linked.agentID(); again != first {
		t.Fatalf("agentID changed: %q then %q", first, again)
	}
	if other, _ := main.agentID(); other == first {
		t.Fatal("two worktrees share an agent id")
	}
}

func TestAgentActorNamesProviderLoginAndWorktree(t *testing.T) {
	for agent, want := range map[string]string{
		"":               "agent/octo/ab12cd34",
		"claude":         "claude/octo/ab12cd34",
		"codex/old-sess": "codex/octo/ab12cd34",
		" /claude/ ":     "claude/octo/ab12cd34",
	} {
		got := agentActor("octo", agent, "ab12cd34")
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
	w, err := openWorktree(gitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUIPU_ACTOR", "codex/octo/env")
	if err := w.writeLease(leaseContext{Issue: 7, Holder: "claude/octo/lease", Branch: "007/x"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolveActor(w, "", 7); got != "claude/octo/lease" {
		t.Fatalf("lease holder should win for its Issue, got %q", got)
	}
	if got, _ := resolveActor(w, "", 8); got != "codex/octo/env" {
		t.Fatalf("QUIPU_ACTOR should apply to other Issues, got %q", got)
	}
	if got, _ := resolveActor(w, "explicit", 7); got != "explicit" {
		t.Fatalf("explicit actor should win, got %q", got)
	}
}

func TestLeaseFreshness(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	for name, tc := range map[string]struct {
		lease leaseContext
		want  bool
	}{
		"recent":         {leaseContext{VerifiedAt: stamp(-time.Minute), Expires: stamp(time.Hour)}, true},
		"stale check":    {leaseContext{VerifiedAt: stamp(-6 * time.Minute), Expires: stamp(time.Hour)}, false},
		"expired lease":  {leaseContext{VerifiedAt: stamp(-time.Minute), Expires: stamp(-time.Second)}, false},
		"never verified": {leaseContext{Expires: stamp(time.Hour)}, false},
		"future check":   {leaseContext{VerifiedAt: stamp(time.Hour), Expires: stamp(2 * time.Hour)}, false},
	} {
		if got := tc.lease.fresh(now); got != tc.want {
			t.Fatalf("%s: fresh = %v, want %v", name, got, tc.want)
		}
	}
}

func TestWorktreeDispatchAndBranchConflicts(t *testing.T) {
	top := gitRepo(t)
	here, err := openWorktree(top)
	if err != nil {
		t.Fatal(err)
	}
	path := dispatchPath(top, "", "quipu", 3)
	if want := filepath.Join(filepath.Dir(top), filepath.Base(top)+"-worktrees", "quipu-003"); path != want {
		t.Fatalf("dispatchPath = %q, want %q", path, want)
	}
	if got := dispatchPath(top, "/w", "quipu", 42); got != "/w/quipu-042" {
		t.Fatalf("dispatchPath with root = %q", got)
	}
	created, madeTree, madeBranch, err := here.addWorktree(path, "003/c", "main")
	if err != nil || !madeTree || !madeBranch || created.branch() != "003/c" {
		t.Fatalf("addWorktree = %+v %v %v %v", created, madeTree, madeBranch, err)
	}
	if again, madeTree, _, err := here.addWorktree(path, "003/c", "main"); err != nil || madeTree || again.Top != created.Top {
		t.Fatalf("existing worktree should be reused: %+v %v %v", again, madeTree, err)
	}
	if err := here.switchBranch("003/c", "main"); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("switching to a branch held by another worktree should name it, got %v", err)
	}
	if err := created.switchBranch("003/c", "main"); err != nil {
		t.Fatalf("already on the branch: %v", err)
	}
	here.removeWorktree(path, "003/c", true)
	if here.hasBranch("003/c") {
		t.Fatal("removeWorktree should delete the branch it created")
	}
	if err := here.switchBranch("004/d", "main"); err != nil || here.branch() != "004/d" {
		t.Fatalf("switchBranch create = %v on %q", err, here.branch())
	}
}

func TestBranchCheckedOutElsewhere(t *testing.T) {
	porcelain := "worktree /r\nHEAD 1\nbranch refs/heads/main\n\nworktree /r-wt/x\nHEAD 2\nbranch refs/heads/005/x\n\nworktree /r-wt/d\nHEAD 3\ndetached\n"
	if got := branchCheckedOutElsewhere(porcelain, "005/x", "/r"); got != "/r-wt/x" {
		t.Fatalf("got %q", got)
	}
	if got := branchCheckedOutElsewhere(porcelain, "005/x", "/r-wt/x"); got != "" {
		t.Fatalf("own worktree is not elsewhere: %q", got)
	}
	if got := branchCheckedOutElsewhere(porcelain, "005/xy", "/r"); got != "" {
		t.Fatalf("prefix must not match: %q", got)
	}
}

func TestReconcileIssuesPathSince(t *testing.T) {
	c := model.Config{Owner: "o", Repository: "r"}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	if got := reconcileIssuesPath(c, 0, now); got != "repos/o/r/issues?state=all&per_page=100" {
		t.Fatalf("full = %q", got)
	}
	if got := reconcileIssuesPath(c, 30*time.Minute, now); got != "repos/o/r/issues?state=all&per_page=100&since=2026-10-29T11:30:00Z" {
		t.Fatalf("since = %q", got)
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
