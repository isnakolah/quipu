package main

import (
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

var cutover = time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)

func watchConfig() model.Config {
	return model.Config{Owner: "o", Repository: "r", DefaultBranch: "main", AuthorizedActors: []string{"owner"}}
}

func validContract() string {
	return model.WorkBody("outcome", "a", "b", "- [ ] works", "- CI", "None: test", "- US-1", "provider")
}

func TestClosedVerdict(t *testing.T) {
	c, trust := watchConfig(), writer.Trust{LegacyBefore: cutover}
	after := "2026-10-30T00:00:00Z"
	cases := []struct {
		name     string
		state    liveWork
		receipts []writer.Receipt
		want     string
	}{
		{"closed by Writer Done", liveWork{ClosedAt: after, ClosedBy: "owner"}, []writer.Receipt{{Result: "accepted", State: &writer.ReceiptState{Status: "Done"}}}, "ok"},
		{"legacy close", liveWork{ClosedAt: "2026-10-01T00:00:00Z"}, nil, "ok"},
		{"owner not planned", liveWork{ClosedAt: after, ClosedBy: "owner", StateReason: "NOT_PLANNED", Labels: []string{"kind:story"}}, nil, "Cancelled"},
		{"owner closes epic", liveWork{ClosedAt: after, ClosedBy: "owner", StateReason: "COMPLETED", Labels: []string{"kind:epic"}}, nil, "Done"},
		{"owner completes story by hand", liveWork{ClosedAt: after, ClosedBy: "owner", StateReason: "COMPLETED", Labels: []string{"kind:story"}}, nil, "reopen"},
		{"stranger not planned", liveWork{ClosedAt: after, ClosedBy: "someone", StateReason: "NOT_PLANNED", Labels: []string{"kind:story"}}, nil, "reopen"},
		{"forged Done receipt is not passed in", liveWork{ClosedAt: after, ClosedBy: "owner", Labels: []string{"kind:task"}}, nil, "reopen"},
	}
	for _, tc := range cases {
		if got := closedVerdict(c, tc.state, tc.receipts, trust); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestExpectedStateAndDrift(t *testing.T) {
	trust := writer.Trust{LegacyBefore: cutover}
	snapshot := &writer.ReceiptState{Status: "In progress", Holder: "claude/owner/w1", Expires: "2026-10-30T02:00:00Z", Branch: "042/x"}
	receipts := []writer.Receipt{{Result: "accepted", State: snapshot}, {Result: "rejected"}, {Result: "accepted", Detail: "wiki"}}
	live := liveWork{Status: "Done", Holder: "", Branch: "042/x", Expiry: "2026-10-30T02:00:00Z", CreatedAt: "2026-10-30T00:00:00Z"}
	want := expectedState(live, receipts, trust)
	if want != snapshot {
		t.Fatalf("want the last snapshot, got %+v", want)
	}
	changed := driftFields(live, *want)
	if len(changed) != 2 || !strings.HasPrefix(changed[0], "Status") || !strings.HasPrefix(changed[1], "Lease holder") {
		t.Fatalf("changed=%v", changed)
	}
	fresh := liveWork{Status: "Ready", CreatedAt: "2026-10-30T00:00:00Z"}
	if expectedState(fresh, nil, trust) != nil {
		t.Fatal("a new Issue may be made Ready before any receipt")
	}
	fresh.Status, fresh.Holder = "In progress", "someone"
	if got := expectedState(fresh, nil, trust); got == nil || got.Status != "Backlog" {
		t.Fatalf("a hand-started new Issue goes back to Backlog, got %+v", got)
	}
	legacy := liveWork{Status: "In progress", CreatedAt: "2026-09-01T00:00:00Z"}
	if expectedState(legacy, nil, trust) != nil {
		t.Fatal("Issues from before the cutover are not checked without snapshots")
	}
	if expectedState(liveWork{Status: "In progress", CreatedAt: "2026-10-30T00:00:00Z"}, []writer.Receipt{{Result: "accepted"}}, trust) != nil {
		t.Fatal("Issues with only pre-snapshot receipts are not checked")
	}
}

func TestTriageProblems(t *testing.T) {
	ok := liveWork{Labels: []string{"kind:story"}, ParentID: "P", Body: validContract()}
	if problems := triageProblems(ok, true); len(problems) != 0 {
		t.Fatalf("valid story: %v", problems)
	}
	if problems := triageProblems(liveWork{Labels: []string{"kind:epic"}}, true); len(problems) != 0 {
		t.Fatalf("epics need no parent or contract: %v", problems)
	}
	bad := triageProblems(liveWork{Labels: []string{"bug"}}, false)
	if len(bad) != 2 || !strings.Contains(bad[0], "Project") || !strings.Contains(bad[1], "kind:") {
		t.Fatalf("bad=%v", bad)
	}
	orphan := triageProblems(liveWork{Labels: []string{"kind:task"}, Body: "free text"}, true)
	if len(orphan) != 2 || !strings.Contains(orphan[0], "parent") || !strings.Contains(orphan[1], "contract") {
		t.Fatalf("orphan=%v", orphan)
	}
}

func TestRequireWriterIdentity(t *testing.T) {
	now := cutover.Add(time.Hour)
	t.Setenv("QUIPU_RECEIPT_TOKEN", "")
	if err := requireWriterIdentity(writer.Trust{LegacyBefore: cutover}, now); err == nil {
		t.Fatal("applying outside the Writer workflow would post untrusted receipts")
	}
	if err := requireWriterIdentity(writer.Trust{LegacyBefore: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUIPU_RECEIPT_TOKEN", "x")
	if err := requireWriterIdentity(writer.Trust{}, now); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyViolations(t *testing.T) {
	c := watchConfig()
	now := cutover
	work := liveWork{IssueState: "OPEN", Kind: "Story", ParentID: "P", Body: validContract(), Status: "In progress",
		Holder: "claude/owner/w1", Branch: "042/fix-it", Expiry: now.Add(time.Hour).Format(time.RFC3339)}
	good := pullFacts{Title: "Fix it", Body: "Refs #42", Author: "owner", Base: "main", Head: "042/fix-it", Commits: []string{"fix it\n"}, Files: []string{"cmd/quipu/main.go"}}
	load := func(w liveWork) func(int) (liveWork, error) { return func(int) (liveWork, error) { return w, nil } }
	if v := prViolations(c, good, load(work), now); len(v) != 0 {
		t.Fatalf("valid PR: %v", v)
	}
	expect := func(name string, pull pullFacts, w liveWork, fragment string) {
		t.Helper()
		v := prViolations(c, pull, load(w), now)
		if !strings.Contains(strings.Join(v, "\n"), fragment) {
			t.Errorf("%s: want %q in %v", name, fragment, v)
		}
	}
	pull := good
	pull.Body = "Closes #42"
	expect("closing keyword", pull, work, "Refs #N")
	pull = good
	pull.Commits = []string{"fix\n\nfixes #42"}
	expect("closing commit", pull, work, "reword the commit")
	pull = good
	pull.Title = "Resolve #42"
	expect("closing title", pull, work, "retitle")
	pull = good
	pull.Base = "dev"
	expect("base", pull, work, "base branch")
	pull = good
	pull.Head = "fix-it"
	expect("branch name", pull, work, "NNN/short-description")
	pull = good
	pull.Head, pull.Body = "1042/fix-it", "Refs #1042"
	w := work
	w.Branch = "1042/fix-it"
	if v := prViolations(c, pull, load(w), now); len(v) != 0 {
		t.Fatalf("Issues above 999 are valid: %v", v)
	}
	pull = good
	pull.Author = "stranger"
	expect("author", pull, work, "PR author")
	pull = good
	pull.Files = []string{".github/workflows/quipu-writer.yml"}
	expect("protected path", pull, work, ownerApprovedLabel)
	pull.Labels = []string{ownerApprovedLabel}
	if v := prViolations(c, pull, load(work), now); len(v) != 0 {
		t.Fatalf("owner-approved protected change: %v", v)
	}
	w = work
	w.Status = "Ready"
	expect("status", good, w, "In progress or In review")
	w = work
	w.Expiry = now.Add(-time.Minute).Format(time.RFC3339)
	expect("expired", good, w, "lease expired")
	w.Status = "In review"
	if v := prViolations(c, good, load(w), now); len(v) != 0 {
		t.Fatalf("In review leases do not expire for the policy: %v", v)
	}
	w = work
	w.Branch = "042/other"
	expect("lease branch", good, w, "not leased on branch")
	w = work
	w.Kind, w.Children = "Epic", 2
	expect("epic", good, w, "only Stories")
	expect("children", good, w, "sub-issues")
	w = work
	w.IssueState = "CLOSED"
	expect("closed", good, w, "is closed")
	w = work
	w.Blockers = []liveBlocker{{ID: "B", State: "OPEN"}}
	expect("blocked", good, w, "blocked")
}
