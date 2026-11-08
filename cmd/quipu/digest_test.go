package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/writer"
)

var digestNow = time.Date(2026, 11, 8, 18, 0, 0, 0, time.UTC)

// receiptFixture is a receipt comment, posted by login.
func receiptFixture(t *testing.T, login string, receipt writer.Receipt) github.Comment {
	t.Helper()
	body, err := writer.RenderReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	comment := github.Comment{Body: body, CreatedAt: receipt.At.Format(time.RFC3339)}
	comment.User.Login = login
	return comment
}

func patchComment(t *testing.T, id int64, issue int, created time.Time) digestComment {
	t.Helper()
	patch := "diff --git a/.github/quipu/config.yaml b/.github/quipu/config.yaml\n--- a/.github/quipu/config.yaml\n+++ b/.github/quipu/config.yaml\n@@ -1 +1 @@\n-merge: owner\n+merge: agent\n"
	files, err := patchFiles(patch)
	if err != nil {
		t.Fatal(err)
	}
	comment := digestComment{ID: id, Body: "<!-- quipu-proposal:v1 -->\n**Proposed work:** let agents merge\n\n" + patchSection(patch, files),
		HTMLURL: "https://github.com/o/r/issues/106#issuecomment-1", IssueURL: "https://api.github.com/repos/o/r/issues/106", CreatedAt: created.Format(time.RFC3339)}
	comment.User.Login = "agent-login"
	return comment
}

func digestFixture(t *testing.T) (digestSources, []ownerItem) {
	t.Helper()
	since := digestNow.Add(-digestWindow)
	pulls := []digestPull{
		{Number: 201, URL: "https://github.com/o/r/pull/201", Title: "feat: digest", Body: "Refs #10", State: "MERGED", MergedAt: digestNow.Add(-2 * time.Hour), Files: []string{"cmd/quipu/digest.go", ".github/workflows/ci.yml"}},
		{Number: 202, URL: "https://github.com/o/r/pull/202", Title: "fix: policy", Body: "Refs #11", State: "MERGED", MergedAt: digestNow.Add(-time.Hour), Files: []string{"cmd/quipu/prpolicy.go"}},
		{Number: 203, URL: "https://github.com/o/r/pull/203", Title: "ci: lint", Body: "Refs #12", State: "OPEN", Files: []string{".github/workflows/lint.yml"}},
		{Number: 204, URL: "https://github.com/o/r/pull/204", Title: "ci: abandoned", Body: "Refs #13", State: "CLOSED", Files: []string{".github/workflows/old.yml"}},
		{Number: 205, URL: "https://github.com/o/r/pull/205", Title: "old", Body: "Refs #14", State: "MERGED", MergedAt: since.Add(-time.Minute), Files: []string{".github/workflows/ci.yml"}},
	}
	receipts := map[int][]github.Comment{
		10: {receiptFixture(t, writer.WriterLogin, writer.Receipt{RequestID: "a", Result: "accepted", At: digestNow.Add(-90 * time.Minute),
			Evidence: &writer.Evidence{Boundary: "CI", FinalSHA: "abc"}})},
		// A receipt anyone but the Writer posted is not evidence.
		11: {receiptFixture(t, "agent-login", writer.Receipt{RequestID: "b", Result: "accepted", At: digestNow.Add(-30 * time.Minute),
			Evidence: &writer.Evidence{Boundary: "production"}})},
	}
	trust := writer.Trust{}
	src := digestSources{
		pulls: func(time.Time) ([]digestPull, error) { return pulls, nil },
		comments: func(time.Time) ([]digestComment, error) {
			return []digestComment{patchComment(t, 9001, 106, digestNow.Add(-3*time.Hour)), patchComment(t, 8001, 106, since.Add(-time.Hour)),
				{ID: 9002, Body: "just a comment", IssueURL: "https://api.github.com/repos/o/r/issues/5", CreatedAt: digestNow.Format(time.RFC3339)}}, nil
		},
		receipts: func(issue int) ([]writer.Receipt, error) { return trust.Receipts(receipts[issue]), nil },
	}
	items := []statusItem{
		{Number: 30, Title: "Which database?", Kind: "Decision", Status: "Ready"},
		{Number: 31, Title: "Parked work", Kind: "Story", Status: "Blocked", WaitingOn: []int{30}},
		{Number: 32, Title: "Busy", Kind: "Story", Status: "In progress", LeaseHolder: "claude/o/w", LeaseExpires: digestNow.Add(time.Hour).Format(time.RFC3339)},
	}
	return src, ownerQueue(items, true, digestNow)
}

func TestDigestFromReceiptsAndMergedPRs(t *testing.T) {
	src, owner := digestFixture(t)
	d := collectDigest("o/r", src, owner, digestNow.Add(-digestWindow), digestNow)
	if len(d.Warnings) != 0 {
		t.Fatalf("warnings: %v", d.Warnings)
	}
	if len(d.Merged) != 2 || d.Merged[0].Issue != 10 || d.Merged[0].Boundary != "CI" || d.Merged[0].PR != "https://github.com/o/r/pull/201" {
		t.Fatalf("merged = %+v", d.Merged)
	}
	if d.Merged[1].Issue != 11 || d.Merged[1].Boundary != "evidence pending" {
		t.Fatalf("an untrusted receipt is no evidence: %+v", d.Merged[1])
	}
	if len(d.OwnerQueue) != 2 || d.OwnerQueue[0].Number != 30 || d.OwnerQueue[1].Number != 31 || !strings.Contains(d.OwnerQueue[1].Reason, "parked") {
		t.Fatalf("owner queue = %+v", d.OwnerQueue)
	}
	if len(d.Patches) != 1 || d.Patches[0].Comment != 9001 || d.Patches[0].Issue != 106 || d.Patches[0].Apply != "quipu patch apply 9001" ||
		len(d.Patches[0].Files) != 1 || d.Patches[0].Files[0] != ".github/quipu/config.yaml" {
		t.Fatalf("patches = %+v", d.Patches)
	}
	if len(d.WorkflowChanges) != 2 || d.WorkflowChanges[0].Number != 201 || d.WorkflowChanges[0].State != "merged" ||
		d.WorkflowChanges[1].Number != 203 || d.WorkflowChanges[1].State != "open" || d.WorkflowChanges[1].Files[0] != ".github/workflows/lint.yml" {
		t.Fatalf("workflow changes = %+v", d.WorkflowChanges)
	}
	want := []string{
		"Merged in the last 24h: 2: #10, #11",
		"Waits on you: 3 (2 on the board, 1 enforcement patch(es))",
		"Workflow changes in the last 24h: 2: PR #201 merged (.github/workflows/ci.yml); PR #203 open (.github/workflows/lint.yml)",
	}
	if strings.Join(d.Headline, "\n") != strings.Join(want, "\n") {
		t.Fatalf("headline:\n%s\nwant:\n%s", strings.Join(d.Headline, "\n"), strings.Join(want, "\n"))
	}
	text := renderDigest(d)
	for _, fragment := range []string{"| #10 feat: digest | https://github.com/o/r/pull/201 | CI |", "#30 Which database?: only the owner decides a Decision",
		"`quipu patch apply 9001`", "PR #203 (open) ci: lint: .github/workflows/lint.yml"} {
		if !strings.Contains(text, fragment) {
			t.Errorf("rendered digest lacks %q:\n%s", fragment, text)
		}
	}
	raw, err := json.Marshal(d)
	if err != nil || !strings.Contains(string(raw), `"workflow_changes":[`) || !strings.Contains(string(raw), `"headline":[`) {
		t.Fatalf("json: %s %v", raw, err)
	}
}

func TestEmptyDigestSaysSo(t *testing.T) {
	src := digestSources{pulls: func(time.Time) ([]digestPull, error) { return nil, nil }, comments: func(time.Time) ([]digestComment, error) { return nil, nil }}
	d := collectDigest("o/r", src, nil, digestNow.Add(-90*time.Minute), digestNow)
	want := "Merged in the last 90m: none\nWaits on you: nothing\nWorkflow changes in the last 90m: none"
	if got := strings.Join(d.Headline, "\n"); got != want {
		t.Fatalf("headline:\n%s", got)
	}
	raw, _ := json.Marshal(d)
	for _, field := range []string{`"merged":[]`, `"owner_queue":[]`, `"patches":[]`, `"workflow_changes":[]`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("empty lists stay lists in JSON: %s lacks %s", raw, field)
		}
	}
}

func TestLightDigestSkipsReceipts(t *testing.T) {
	src, owner := digestFixture(t)
	src.receipts = nil
	d := collectDigest("o/r", src, owner, digestNow.Add(-digestWindow), digestNow)
	if len(d.Merged) != 2 || d.Merged[0].Boundary != "" {
		t.Fatalf("a light digest does not look up boundaries: %+v", d.Merged)
	}
}

func TestStatusShowsTheDigestHeadline(t *testing.T) {
	report := buildStatusReport("o/r", nil, digestNow)
	report.Digest = []string{"Merged in the last 24h: 1: #10", "Waits on you: nothing", "Workflow changes in the last 24h: none"}
	text := renderStatus(report, digestNow)
	if !strings.Contains(text, "Digest (quipu digest for more)\n  Merged in the last 24h: 1: #10\n  Waits on you: nothing\n") {
		t.Fatalf("status:\n%s", text)
	}
}
