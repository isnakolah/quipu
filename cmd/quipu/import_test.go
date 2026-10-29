package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

const testContract = "<!-- work:v1 -->\n## Outcome\n## Scope\nIncludes:\nExcludes:\n## Acceptance criteria\n## Required evidence\nVerification boundary: CI\n## Documentation impact\n## Source and relationships\n"

func importConfig() model.Config {
	return model.Config{Owner: "octo", Repository: "calla", Areas: []string{"app", "infra"}}
}

func manifestFrom(t *testing.T, yaml string) importManifest {
	t.Helper()
	m, err := parseManifest([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	m.normalize(importConfig())
	return m
}

func validManifest(t *testing.T) importManifest {
	body := strings.ReplaceAll(testContract, "\n", "\n      ")
	return manifestFrom(t, `schema: quipu-import/v1
source: quipu-import/backlog.yaml
items:
  - key: STORY-2
    kind: story
    title: Offline mode
    parent: EPIC-1
    status: Ready
    blockers: [STORY-1]
    body: |
      `+body+`
  - key: STORY-1
    kind: story
    title: Sync engine
    parent: EPIC-1
    status: Done
    commits: [0123abc]
    source: docs/plan.md:10-40
  - key: EPIC-1
    kind: epic
    title: Calla v1
    priority: P1
  - key: TASK-3
    kind: task
    title: Old spike
    parent: STORY-2
    status: Cancelled
    reason: replaced by the sync engine
  - key: TASK-4
    kind: task
    title: Widgets
    parent: STORY-2
    status: In progress
    branch: feature/widgets
    area: infra
    body: |
      `+body+`
`)
}

func TestManifestDefaultsAndOrder(t *testing.T) {
	m := validManifest(t)
	if err := m.validate(importConfig()); err != nil {
		t.Fatal(err)
	}
	ordered, err := m.ordered()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, item := range ordered {
		keys = append(keys, item.Key)
	}
	if strings.Join(keys, ",") != "EPIC-1,STORY-2,STORY-1,TASK-3,TASK-4" {
		t.Fatalf("parents must come first, otherwise manifest order: %v", keys)
	}
	epic := ordered[0]
	if epic.Area != "app" || epic.Priority != "P1" || epic.Status != "Backlog" || epic.Source != "quipu-import/backlog.yaml" {
		t.Fatalf("defaults = %+v", epic)
	}
	if ordered[2].Source != "docs/plan.md:10-40" {
		t.Fatal("an item's own source wins over the manifest's")
	}
}

func TestManifestRejectsMoreSubIssuesThanGitHubAllows(t *testing.T) {
	m := validManifest(t)
	for i := 0; i < maxSubIssues; i++ {
		m.Items = append(m.Items, importItem{Key: fmt.Sprintf("S%03d", i), Kind: "task", Title: "Task", Parent: "EPIC-1",
			Area: "app", Priority: "P2", Status: "Cancelled", Reason: "proof"})
	}
	err := m.validate(importConfig())
	if err == nil || !strings.Contains(err.Error(), "EPIC-1: has 102 children; GitHub allows 100 sub-issues per parent") {
		t.Fatalf("err = %v", err)
	}
	m.Items = m.Items[:len(m.Items)-2]
	if err := m.validate(importConfig()); err != nil {
		t.Fatalf("exactly %d children is allowed: %v", maxSubIssues, err)
	}
}

func TestManifestValidationReportsEveryProblem(t *testing.T) {
	m := manifestFrom(t, `schema: quipu-import/v0
items:
  - key: A
    kind: story
    title: no parent, no contract
  - key: A
    kind: saga
    title: ""
    parent: GHOST
    area: nowhere
    priority: P9
    status: Doing
  - key: B
    kind: task
    title: done but blocked
    parent: E
    status: Done
    blockers: [A, B]
    commits: [NOT-A-SHA]
  - key: C
    kind: task
    title: cancelled without reason
    parent: E
    status: Cancelled
    branch: x
  - key: E
    kind: epic
    title: epic
    parent: B
`)
	err := m.validate(importConfig())
	if err == nil {
		t.Fatal("expected problems")
	}
	for _, want := range []string{
		`schema must be "quipu-import/v1"`,
		"key A is used twice",
		"A: every item but an Epic needs a parent",
		"A: open story needs a work:v1 contract body",
		"A: kind must be one of",
		"A: title is required",
		"A: parent GHOST is not in the manifest",
		`A: area "nowhere" is not configured`,
		"A: priority must be",
		"A: status must be",
		"B: Done work cannot be blocked by open A",
		"B: an item cannot block itself",
		`B: commit "NOT-A-SHA" is not a lowercase hex SHA`,
		"C: Cancelled work needs a reason",
		"C: branch is for In progress work",
		"E: an Epic has no parent",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	cycle := manifestFrom(t, `schema: quipu-import/v1
items:
  - {key: X, kind: story, title: x, parent: Y, status: Done}
  - {key: Y, kind: story, title: y, parent: X, status: Done}
`)
	if err := cycle.validate(importConfig()); err == nil || !strings.Contains(err.Error(), "parent cycle") {
		t.Fatalf("cycle: %v", err)
	}
	if _, err := parseManifest([]byte("schema: quipu-import/v1\nitems:\n  - key: A\n    blocked_by: [B]\n")); err == nil {
		t.Fatal("unknown fields must be refused, not ignored")
	}
}

func TestImportMatchesByKey(t *testing.T) {
	item := validManifest(t).Items[4]
	body := importBody(item)
	if !strings.Contains(body, "branch `feature/widgets`") || !strings.HasSuffix(body, "<!-- quipu-import:v1 key=TASK-4 -->\n") {
		t.Fatalf("body = %q", body)
	}
	if err := model.ValidateWorkContract(body); err != nil {
		t.Fatalf("the generated body must keep the contract valid: %v", err)
	}
	pages := `[{"number":7,"body":` + quote(body) + `},{"number":8,"body":"no marker"},{"number":9,"body":"<!-- quipu-import:v1 key=X -->","pull_request":{"url":"u"}}]
[{"number":12,"body":"<!-- quipu-import:v1 key=EPIC-1 -->"},{"number":3,"body":"<!-- quipu-import:v1 key=EPIC-1 -->"},{"number":4,"body":null}]`
	keys, err := importedKeys([]byte(pages))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys["TASK-4"] != 7 || keys["EPIC-1"] != 3 {
		t.Fatalf("keys = %v (PRs are skipped; a duplicate keeps the oldest Issue)", keys)
	}
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

func TestImportRequestedSkipsAnsweredRejections(t *testing.T) {
	render := func(id string) github.Comment {
		body, err := writer.RenderRequest(writer.Request{ID: id, Action: "import", IssueID: "I", Actor: "a", ExpectedFingerprint: "f", Import: &writer.Import{Key: "K", Status: "Done"}})
		if err != nil {
			t.Fatal(err)
		}
		return github.Comment{Body: body}
	}
	if importRequested(nil, nil) {
		t.Fatal("no comments, no request")
	}
	if !importRequested([]github.Comment{render("1")}, nil) {
		t.Fatal("an unanswered request counts")
	}
	rejected := []writer.Receipt{{RequestID: "1", Result: "rejected"}}
	if importRequested([]github.Comment{render("1")}, rejected) {
		t.Fatal("a rejected request is posted again")
	}
	if !importRequested([]github.Comment{render("1"), render("2")}, append(rejected, writer.Receipt{RequestID: "2", Result: "accepted"})) {
		t.Fatal("an accepted request counts")
	}
}

func TestImportBranchKeepsOnlyFlowBranches(t *testing.T) {
	if importBranch(42, "feature/widgets") != "" || importBranch(42, "041/widgets") != "" {
		t.Fatal("foreign branches stay off the board")
	}
	if importBranch(42, "042/widgets") != "042/widgets" {
		t.Fatal("a flow branch for this Issue is kept")
	}
}

func TestImportReceiptsAreTrustedHistory(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	done := writer.Import{Key: "STORY-1", Status: "Done", Source: "docs/plan.md:10-40", Commits: []string{"0123abc", "4567def"}}
	next, err := writer.ApplyLifecycle(writer.WorkState{Status: "Backlog"}, writer.Request{Action: "import", Import: &done}, now)
	if err != nil || next.Status != "Done" {
		t.Fatalf("next=%+v err=%v", next, err)
	}
	receipt := importReceipt(writer.Receipt{RequestID: "r", Result: "accepted", At: now, State: writer.StateOf(next)}, done)
	if receipt.Evidence == nil || receipt.Evidence.Validate() != nil {
		t.Fatalf("Done needs complete evidence: %+v", receipt.Evidence)
	}
	if receipt.Evidence.Boundary != "local, before Quipu" || receipt.Evidence.FinalSHA != "0123abc, 4567def" || receipt.Evidence.Artifacts != "docs/plan.md:10-40" {
		t.Fatalf("evidence = %+v", receipt.Evidence)
	}
	trust := writer.Trust{}
	comment, _ := writer.RenderReceipt(receipt)
	receipts := trust.Receipts([]github.Comment{{Body: comment, User: struct {
		Login string `json:"login"`
	}{writer.WriterLogin}}})
	closed := liveWork{ClosedAt: now.Format(time.RFC3339), ClosedBy: "someone", StateReason: "COMPLETED"}
	if verdict := closedVerdict(importConfig(), closed, receipts, trust); verdict != "ok" {
		t.Fatalf("the watchdog must leave imported Done closed, got %q", verdict)
	}
	forged := trust.Receipts([]github.Comment{{Body: comment, User: struct {
		Login string `json:"login"`
	}{"octo"}}})
	if verdict := closedVerdict(importConfig(), closed, forged, trust); verdict != "reopen" {
		t.Fatalf("a receipt not posted by the Writer is ignored, got %q", verdict)
	}

	cancelled := writer.Import{Key: "TASK-3", Status: "Cancelled", Reason: "replaced"}
	if next, err := writer.ApplyLifecycle(writer.WorkState{}, writer.Request{Action: "import", Import: &cancelled}, now); err != nil || next.Status != "Cancelled" {
		t.Fatalf("cancelled: %+v %v", next, err)
	}
	started := writer.Import{Key: "TASK-4", Status: "In progress"}
	next, err = writer.ApplyLifecycle(writer.WorkState{Status: "Backlog"}, writer.Request{Action: "import", Branch: "009/widgets", Import: &started}, now)
	if err != nil || next.Status != "Ready" || next.Lease.Branch != "009/widgets" || next.Lease.Holder != "" {
		t.Fatalf("In progress waits in Ready on its branch: %+v %v", next, err)
	}
	for name, tc := range map[string]struct {
		current writer.WorkState
		record  *writer.Import
	}{
		"not new":         {writer.WorkState{Status: "Ready"}, &done},
		"held":            {writer.WorkState{Status: "Backlog", Lease: writer.Lease{Holder: "a"}}, &done},
		"no record":       {writer.WorkState{Status: "Backlog"}, nil},
		"unknown status":  {writer.WorkState{Status: "Backlog"}, &writer.Import{Status: "Shipped"}},
		"cancel unreason": {writer.WorkState{Status: "Backlog"}, &writer.Import{Status: "Cancelled"}},
	} {
		if _, err := writer.ApplyLifecycle(tc.current, writer.Request{Action: "import", Import: tc.record}, now); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}
