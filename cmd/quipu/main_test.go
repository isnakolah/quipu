package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

func TestLiveWorkFingerprintIgnoresRequestCommentTimestamp(t *testing.T) {
	before := liveWork{IssueID: "I_1", IssueState: "OPEN", UpdatedAt: "2026-08-09T19:31:00Z", Status: "Ready"}
	after := before
	after.UpdatedAt = "2026-08-09T19:32:00Z"

	beforeHash, err := fingerprintLiveWork(before)
	if err != nil {
		t.Fatal(err)
	}
	afterHash, err := fingerprintLiveWork(after)
	if err != nil {
		t.Fatal(err)
	}
	if beforeHash != afterHash {
		t.Fatalf("request comment timestamp changed fingerprint: %s != %s", beforeHash, afterHash)
	}
}

func TestJournalRenderIsDeterministic(t *testing.T) {
	output, err := captureStdout(t, func() error {
		return journalCommand([]string{"render", "--request-id", "r1", "--date", "2026-08-09", "--issue", "#13", "--pr", "https://example.test/pr/1", "--outcome", "recorded", "--proof", "CI", "--boundary", "CI", "--next-blocker", "None"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "<!-- quipu-journal:v1 request=r1 -->") || !strings.Contains(output, "- Issue: #13") {
		t.Fatalf("unexpected journal output: %s", output)
	}
}

func TestPendingWikiJournalAllowsRepairedReceipt(t *testing.T) {
	receipts := []writer.Receipt{{RequestID: "evidence-1", Result: "accepted", Detail: "evidence recorded; Wiki journal pending", Evidence: &writer.Evidence{}}}
	if !pendingWikiJournal(receipts) {
		t.Fatal("expected pending wiki journal")
	}
	receipts = append(receipts, writer.Receipt{RequestID: "wiki-retry-evidence-1", Result: "accepted", Detail: "generated Wiki journal repaired"})
	if pendingWikiJournal(receipts) {
		t.Fatal("repaired wiki journal must not block completion")
	}
}

func TestRequireEmptyBootstrapRoot(t *testing.T) {
	root := t.TempDir()
	if err := requireEmptyBootstrapRoot(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "existing"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := requireEmptyBootstrapRoot(root); err == nil {
		t.Fatal("expected nonempty root refusal")
	}
}

func TestConfiguredFieldIDsRejectsMissingRequiredFields(t *testing.T) {
	if _, err := configuredFieldIDsFromMap(4, map[string]projectField{"Status": {ID: "PVTSSF_status"}}); err == nil {
		t.Fatal("expected missing field error")
	}
}

func TestRequestStatusReportJoinsReceipt(t *testing.T) {
	request := writer.Request{ID: "r1", Action: "claim", IssueID: "I1", Actor: "agent", ExpectedFingerprint: "fingerprint"}
	requestBody, err := writer.RenderRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	receiptBody, err := writer.RenderReceipt(writer.Receipt{RequestID: "r1", Fingerprint: "fresh", Result: "accepted", Detail: "claimed"})
	if err != nil {
		t.Fatal(err)
	}
	bot := github.Comment{Body: receiptBody}
	bot.User.Login = writer.WriterLogin
	report := requestStatusReport([]github.Comment{{Body: requestBody}, bot}, writer.Trust{}, "")
	if len(report) != 1 || report[0].State != "accepted" || report[0].Detail != "claimed" {
		t.Fatalf("report=%+v", report)
	}
	forged := github.Comment{Body: receiptBody}
	forged.User.Login = "isnakolah"
	if report := requestStatusReport([]github.Comment{{Body: requestBody}, forged}, writer.Trust{}, ""); report[0].State != "pending" {
		t.Fatalf("receipt from a non-Writer login counted: %+v", report)
	}
}

func TestStandardProjectViews(t *testing.T) {
	fields := map[string]int{}
	for i, name := range []string{"Status", "Kind", "Area", "Priority", "Proof", "Lease holder", "Lease expires", "Branch", "State fingerprint", "Parent issue"} {
		fields[name] = i + 1
	}
	views, err := standardProjectViews(fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 5 || views[0].Name != "Roadmap" || views[2].Layout != "board" || views[2].GroupBy[0] != fields["Status"] {
		t.Fatalf("views=%+v", views)
	}
}

func TestIssueNumberFromURL(t *testing.T) {
	number, err := issueNumberFromURL("https://github.com/octo/example/issues/42")
	if err != nil || number != 42 {
		t.Fatalf("number=%d err=%v", number, err)
	}
	if _, err := issueNumberFromURL("not an issue URL"); err == nil {
		t.Fatal("expected malformed URL rejection")
	}
}

func TestSelectReadyWorkRequiresValidContract(t *testing.T) {
	valid := model.WorkBody("test", "scope", "none", "pass", "test", "None: test", "source", "local")
	candidates := []workCandidate{
		{readyWork: readyWork{IssueID: "bad", Number: 1, Kind: "Story"}, Status: "Ready", ParentID: "parent", Body: "missing contract"},
		{readyWork: readyWork{IssueID: "good", Number: 2, Kind: "Story"}, Status: "Ready", ParentID: "parent", Body: valid},
	}
	ready := selectReadyWork(candidates, time.Now())
	if len(ready) != 1 || ready[0].Number != 2 {
		t.Fatalf("ready=%+v", ready)
	}
}

func TestAuthorizedActor(t *testing.T) {
	if !authorizedActor([]string{"isnakolah"}, "IsNaKoLaH") || authorizedActor([]string{"isnakolah"}, "other") {
		t.Fatal("unexpected authorization result")
	}
}

func TestContainsAndTitleCase(t *testing.T) {
	if !contains([]string{"delivery", "core"}, "core") || contains([]string{"delivery"}, "ops") {
		t.Fatal("configured area lookup is incorrect")
	}
	if got := titleCase("story"); got != "Story" {
		t.Fatalf("titleCase=%q", got)
	}
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	previous := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	err = fn()
	_ = writer.Close()
	os.Stdout = previous
	var output bytes.Buffer
	if _, copyErr := io.Copy(&output, reader); copyErr != nil && err == nil {
		err = copyErr
	}
	_ = reader.Close()
	return output.String(), err
}

func TestSelectReadyWorkExcludesNonLeafBlockedAndLeased(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	contract := model.WorkBody("test", "scope", "none", "pass", "test", "None: test", "source", "local")
	ready := selectReadyWork([]workCandidate{
		{readyWork: readyWork{IssueID: "I-1", Number: 1, Kind: "Story"}, ParentID: "P", Status: "Ready", Body: contract},
		{readyWork: readyWork{IssueID: "I-2", Number: 2, Kind: "Story"}, ParentID: "P", Status: "Ready", Body: contract, HasChildren: true},
		{readyWork: readyWork{IssueID: "I-3", Number: 3, Kind: "Story"}, ParentID: "P", Status: "Ready", Body: contract, BlockerStates: []string{"OPEN"}},
		{readyWork: readyWork{IssueID: "I-4", Number: 4, Kind: "Story"}, ParentID: "P", Status: "Ready", Body: contract, LeaseHolder: "agent", LeaseExpires: now.Add(time.Hour).Format(time.RFC3339)},
		{readyWork: readyWork{IssueID: "I-5", Number: 5, Kind: "Story"}, ParentID: "P", Status: "Ready", Body: contract, LeaseHolder: "agent", LeaseExpires: now.Add(-time.Hour).Format(time.RFC3339)},
	}, now)
	if len(ready) != 2 || ready[0].Number != 1 || ready[1].Number != 5 {
		t.Fatalf("ready work = %#v", ready)
	}
}
