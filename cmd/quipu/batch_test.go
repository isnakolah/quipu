package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	policycheck "github.com/isnakolah/quipu/internal/policy"
)

const epicBranch = "epic-117/smoother-and-more-autonomous"

// leafOf is a leaf Issue of Epic epic, In progress and leased on branch.
func leafOf(epic int, branch string) liveWork {
	return liveWork{IssueState: "OPEN", Status: "In progress", Kind: "Story", ParentID: fmt.Sprintf("E%d", epic), ParentNumber: epic,
		Body: testContract, Holder: "claude/octo/a", Branch: branch, Expiry: time.Now().Add(time.Hour).Format(time.RFC3339)}
}

func epicPull(body string) pullFacts {
	return pullFacts{Body: body, Base: "main", Head: epicBranch, Author: "octo", Title: "feat(batch): three small things"}
}

func TestPolicyAcceptsOneEpicBatch(t *testing.T) {
	batches := model.Config{DefaultBranch: "main", Batches: "epic"}
	leaves := map[int]liveWork{130: leafOf(117, epicBranch), 133: leafOf(117, epicBranch), 134: leafOf(117, epicBranch)}
	load := func(n int) (liveWork, error) { return leaves[n], nil }
	pull := epicPull("Refs #130\nRefs #133\nRefs #134\n\nThree leaves of #117.")
	if v := prViolations(batches, pull, load, time.Now()); len(v) != 0 {
		t.Fatalf("one Epic's leaves on its rolling branch: %v", v)
	}

	leaves[106] = leafOf(106, epicBranch)
	v := prViolations(batches, epicPull("Refs #130\nRefs #106"), load, time.Now())
	if joined := strings.Join(v, "\n"); !strings.Contains(joined, "Epics #106 and #117") {
		t.Fatalf("a PR mixing two Epics names both: %v", v)
	}

	other := leafOf(117, "epic-117/other")
	leaves[135] = other
	if v := prViolations(batches, epicPull("Refs #130\nRefs #135"), load, time.Now()); len(v) == 0 {
		t.Fatal("every referenced leaf must be leased on this branch")
	}

	withPrefix := batches
	withPrefix.CommitFormat = policycheck.IssuePrefix
	pull.Commits = []string{"#130 feat(a): one\n\nRefs: #130", "#134 fix(b): two\n\nRefs: #134"}
	if v := prViolations(withPrefix, pull, load, time.Now()); len(v) != 0 {
		t.Fatalf("each commit may carry any referenced Issue's prefix: %v", v)
	}
	pull.Commits = append(pull.Commits, "#999 feat(c): stray\n\nRefs: #999")
	if v := prViolations(withPrefix, pull, load, time.Now()); len(v) == 0 {
		t.Fatal("a commit for an Issue outside the PR is refused")
	}
}

func TestEpicBranchesNeedTheSetting(t *testing.T) {
	plain := model.Config{DefaultBranch: "main"}
	load := func(int) (liveWork, error) { return leafOf(117, epicBranch), nil }
	v := prViolations(plain, epicPull("Refs #130\nRefs #133"), load, time.Now())
	joined := strings.Join(v, "\n")
	if !strings.Contains(joined, "exactly one Issue") || !strings.Contains(joined, "batches: epic") {
		t.Fatalf("without batches an epic branch and several Refs are refused: %v", v)
	}
	single := pullFacts{Body: "Refs #130", Base: "main", Head: "130/leases", Author: "octo"}
	state := leafOf(117, "130/leases")
	if v := prViolations(plain, single, func(int) (liveWork, error) { return state, nil }, time.Now()); len(v) != 0 {
		t.Fatalf("one Issue, one branch, one PR is unchanged: %v", v)
	}
}

func TestWriterLinksEachLeafOfABatch(t *testing.T) {
	c := model.Config{DefaultBranch: "main", Batches: "epic"}
	view := prView{Body: "Refs #130\nRefs #133", BaseRefName: "main", HeadRefName: epicBranch}
	for _, n := range []int{130, 133} {
		if err := checkPRWork(c, n, epicBranch, view); err != nil {
			t.Errorf("#%d: %v", n, err)
		}
	}
	if err := checkPRWork(c, 134, epicBranch, view); err == nil {
		t.Error("a leaf the PR does not reference is not linked")
	}
	if err := checkPRWork(model.Config{DefaultBranch: "main"}, 130, epicBranch, view); err == nil {
		t.Error("without batches the PR must reference exactly one Issue")
	}
}

func TestBatchDoneSplitsWhatLanded(t *testing.T) {
	numbers, flags, batch := doneIssues([]string{"130", "#133", "134", "--json", "--criteria", "42", "--boundary=CI"})
	if len(numbers) != 3 || numbers[1] != 133 || batch || strings.Join(flags, " ") != "--json --criteria 42 --boundary=CI" {
		t.Fatalf("issues %v flags %v", numbers, flags)
	}
	if _, _, batch := doneIssues([]string{"--batch", "--json"}); !batch {
		t.Fatal("--batch")
	}
	commits := []string{"feat(a): one\n\nRefs #130", "#133 fix(b): two\n\nRefs: #133", "chore: unrelated, mentions #134 in passing"}
	landed, left := landedIssues([]int{130, 133, 134}, commits)
	if fmt.Sprint(landed) != "[130 133]" || fmt.Sprint(left) != "[134]" {
		t.Fatalf("landed %v left %v", landed, left)
	}
}

func TestLeasesShareAnEpicWorktree(t *testing.T) {
	w := leasedWorktree(t, 130)
	first, err := w.readLease()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{133, 134} {
		lease := first
		lease.Issue = n
		if err := w.addLease(lease); err != nil {
			t.Fatal(err)
		}
	}
	leases, err := w.readLeases()
	if err != nil || len(leases) != 3 {
		t.Fatalf("three leases in one worktree: %v %v", leases, err)
	}
	if lease, ok := w.leaseFor(133); !ok || lease.Issue != 133 {
		t.Fatal("each lease is found by its Issue")
	}
	if warnings := w.release(130, "exit 3", time.Hour); warnings != nil {
		t.Fatalf("releasing one leaf runs no cleanup while others remain: %v", warnings)
	}
	if _, ok := w.readFinished(time.Now()); ok {
		t.Fatal("the worktree is not finished while leaves remain")
	}
	if _, ok := w.leaseFor(130); ok {
		t.Fatal("the released lease is gone")
	}
	stranger := first
	stranger.Issue, stranger.Branch = 200, "200/other"
	if err := w.addLease(stranger); err != nil {
		t.Fatal(err)
	}
	if leases, _ := w.readLeases(); len(leases) != 1 {
		t.Fatalf("a lease on another branch replaces the set: %v", leases)
	}
	if err := w.writeLease(first); err != nil {
		t.Fatal(err)
	}
	if leases, _ := w.readLeases(); len(leases) != 1 || leases[0].Issue != 130 {
		t.Fatalf("writeLease keeps one lease, as before: %v", leases)
	}
}
