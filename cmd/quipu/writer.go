package main

// The Writer: the only code that changes lifecycle state. It runs in the
// Quipu Writer workflow, applies request comments, and answers each with a
// receipt posted under the workflow's own token, so receipts are authored by
// github-actions[bot]. Only those receipts count, which means no one sharing
// the owner's login can forge one.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/journal"
	"github.com/isnakolah/quipu/internal/model"
	policycheck "github.com/isnakolah/quipu/internal/policy"
	"github.com/isnakolah/quipu/internal/writer"
)

func receiptTrust(c model.Config) writer.Trust {
	before, _ := time.Parse(time.RFC3339, c.ReceiptsTrustedBefore)
	return writer.Trust{LegacyBefore: before}
}

// receiptClient posts receipts and Writer comments. The Writer workflow sets
// QUIPU_RECEIPT_TOKEN to its GITHUB_TOKEN; Project changes keep using the
// Writer's personal token, which GITHUB_TOKEN cannot replace.
func receiptClient() *github.Client {
	client := github.NewClient()
	if token := strings.TrimSpace(os.Getenv("QUIPU_RECEIPT_TOKEN")); token != "" {
		client.Token = token
	}
	return client
}

// requireWriterIdentity refuses --apply where the receipts it posts would not
// be trusted, since the same requests would then be applied again.
func requireWriterIdentity(trust writer.Trust, now time.Time) error {
	if os.Getenv("QUIPU_RECEIPT_TOKEN") != "" || trust.LegacyBefore.After(now) {
		return nil
	}
	return errors.New("the Writer applies requests only in the Quipu Writer workflow, where receipts are posted as " + writer.WriterLogin + ". Run `gh workflow run quipu-writer.yml` instead")
}

func writerCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: quipu writer {run|reconcile}")
	}
	if args[0] == "reconcile" {
		return writerReconcileCommand(args[1:])
	}
	if args[0] != "run" {
		return errors.New("usage: quipu writer {run|reconcile}")
	}
	fs := flag.NewFlagSet("writer run", flag.ContinueOnError)
	number := fs.Int("issue-number", 0, "issue number")
	apply := applyFlag(fs)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *number < 1 {
		return errors.New("--issue-number is required")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	trust, now := receiptTrust(c), time.Now()
	if *apply {
		if err := requireWriterIdentity(trust, now); err != nil {
			return err
		}
	}
	report, err := reconcileIssue(c, *number, *apply, trust, now)
	if err != nil {
		return err
	}
	ids := append([]string{}, report.Pending...)
	return printValue(map[string]any{"issue_number": *number, "pending_request_ids": ids, "receipts_applied": report.Receipted, "apply": *apply, "warnings": report.Warnings}, *asJSON)
}

func applyAuthorizedWriterRequest(c model.Config, number int, pending writer.PendingRequest, receipts []writer.Receipt) writer.Receipt {
	if !authorizedActor(c.AuthorizedActors, pending.Author) {
		return rejectedReceipt(pending.Request, fmt.Errorf("request author %q is not authorized by repository config", pending.Author))
	}
	return applyWriterRequest(c, number, pending.Request, receipts)
}

func applyWriterRequest(c model.Config, number int, request writer.Request, receipts []writer.Receipt) writer.Receipt {
	state, err := loadLiveWork(c, number)
	if err != nil {
		return rejectedReceipt(request, err)
	}
	if state.IssueID != request.IssueID {
		return rejectedReceipt(request, errors.New("request Issue ID does not match target Issue"))
	}
	actual, err := writer.RequireFreshFingerprint(request.ExpectedFingerprint, liveWorkFingerprint(state))
	if err != nil {
		return writer.Receipt{RequestID: request.ID, Fingerprint: actual, Result: "rejected", Detail: err.Error(), At: time.Now().UTC()}
	}
	if request.Action == "claim" || (request.Action == "status" && request.Status == "Done") {
		for _, blocker := range state.Blockers {
			if blocker.State != "CLOSED" {
				return rejectedReceipt(request, fmt.Errorf("unresolved blocker %s prevents %s", blocker.ID, request.Action))
			}
		}
	}
	if request.Action == "status" && request.Status == "Done" {
		if !writer.AcceptedEvidenceSince(receipts, writer.CycleStart(receipts)) {
			return rejectedReceipt(request, errors.New("Done requires accepted evidence from the current claim"))
		}
		if c.WikiMode == "journal" && pendingWikiJournal(receipts) {
			return rejectedReceipt(request, errors.New("Done requires generated Wiki journal to be synchronized"))
		}
	}
	if request.Action == "pr.link" {
		if err := validateReviewPR(c, number, state.Branch, request.PR); err != nil {
			return rejectedReceipt(request, err)
		}
	}
	if request.Action == "evidence.submit" {
		if err := validateEvidence(c, number, state.Branch, request.PR, request.Evidence); err != nil {
			return rejectedReceipt(request, err)
		}
	}
	current := writer.WorkState{Status: state.Status, Lease: writer.Lease{Holder: state.Holder, Branch: state.Branch}}
	if state.Expiry != "" {
		current.Lease.Expires, _ = time.Parse(time.RFC3339, state.Expiry)
	}
	next, err := writer.ApplyLifecycle(current, request, time.Now())
	if err != nil {
		return rejectedReceipt(request, err)
	}
	if err := updateLiveWork(c, state.ItemID, next); err != nil {
		return rejectedReceipt(request, err)
	}
	if next.Status == "Done" {
		if err := github.NewClient().SetIssueState(context.Background(), c.Owner, c.Repository, number, "closed"); err != nil {
			return rejectedReceipt(request, fmt.Errorf("close completed Issue: %w", err))
		}
	}
	receipt := writer.Receipt{RequestID: request.ID, Fingerprint: actual, Result: "accepted", Detail: "lifecycle state changed to " + next.Status, At: time.Now().UTC(), State: writer.StateOf(next)}
	if request.Action == "evidence.submit" {
		receipt.Detail = "evidence recorded; lifecycle state changed to Evidence pending"
		receipt.Evidence = request.Evidence
	}
	if next.Status == "Done" {
		receipt.Detail = "completion evidence verified; lifecycle state changed to Done and Issue closed"
	}
	return receipt
}

func pendingWikiJournal(receipts []writer.Receipt) bool {
	for _, receipt := range receipts {
		if receipt.Result == "accepted" && receipt.Evidence != nil && strings.Contains(receipt.Detail, "Wiki journal pending") {
			if _, repaired := receiptFor(receipts, "wiki-retry-"+receipt.RequestID); !repaired {
				return true
			}
		}
	}
	return false
}

func retryPendingWikiJournals(c model.Config, number int, receipts []writer.Receipt, post func(writer.Receipt) error) error {
	if c.WikiMode != "journal" {
		return nil
	}
	token := github.NewClient().Token
	for _, receipt := range append([]writer.Receipt{}, receipts...) {
		if receipt.Result != "accepted" || receipt.Evidence == nil || !strings.Contains(receipt.Detail, "Wiki journal pending") {
			continue
		}
		repairID := "wiki-retry-" + receipt.RequestID
		if _, done := receiptFor(receipts, repairID); done {
			continue
		}
		entry := journal.Entry{RequestID: receipt.RequestID, Date: receipt.At.UTC().Format("2006-01-02"), Issue: fmt.Sprintf("#%d", number), Outcome: "evidence journal repair", Proof: receipt.Evidence.Criteria, Boundary: receipt.Evidence.Boundary, NextBlocker: "None"}
		if _, err := journal.PublishWiki(context.Background(), c.Owner, c.Repository, token, entry); err != nil {
			continue
		}
		if err := post(writer.Receipt{RequestID: repairID, Result: "accepted", Detail: "generated Wiki journal repaired for evidence request " + receipt.RequestID, At: time.Now().UTC()}); err != nil {
			return err
		}
	}
	return nil
}

// validateReviewPR accepts only an open PR into the default branch, from the
// leased branch, that references this Issue.
func validateReviewPR(c model.Config, number int, branch, pr string) error {
	if pr == "" {
		return errors.New("PR link requires pull request URL")
	}
	view, err := viewPR(c, pr)
	if err != nil {
		return err
	}
	if view.URL != pr || view.State != "OPEN" {
		return errors.New("PR link requires an open canonical pull request URL")
	}
	return checkPRWork(c, number, branch, view)
}

// validateEvidence accepts evidence only for this Issue's merged PR, with the
// final SHA its merge commit and the CI URL a successful run on that PR.
func validateEvidence(c model.Config, number int, branch, pr string, evidence *writer.Evidence) error {
	if evidence == nil || pr == "" || evidence.FinalSHA == "" {
		return errors.New("evidence requires PR URL and final SHA")
	}
	view, err := viewPR(c, pr)
	if err != nil {
		return err
	}
	if view.URL != pr || view.State != "MERGED" {
		return errors.New("evidence requires merged canonical pull request URL")
	}
	if view.MergeCommit.OID != evidence.FinalSHA {
		return fmt.Errorf("evidence final SHA %s does not match merged PR head %s", evidence.FinalSHA, view.MergeCommit.OID)
	}
	if err := checkPRWork(c, number, branch, view); err != nil {
		return err
	}
	return checkCIRun(c, evidence.CIURL, view)
}

type prView struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	State       string `json:"state"`
	Body        string `json:"body"`
	BaseRefName string `json:"baseRefName"`
	HeadRefName string `json:"headRefName"`
	HeadRefOid  string `json:"headRefOid"`
	MergeCommit struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
}

func viewPR(c model.Config, pr string) (prView, error) {
	var view prView
	output, err := ghOutput("pr", "view", pr, "--repo", c.Owner+"/"+c.Repository, "--json", "number,url,state,body,baseRefName,headRefName,headRefOid,mergeCommit")
	if err != nil {
		return view, fmt.Errorf("read pull request: %w", err)
	}
	return view, json.Unmarshal(output, &view)
}

func checkPRWork(c model.Config, number int, branch string, view prView) error {
	if branch == "" || view.HeadRefName != branch {
		return fmt.Errorf("PR head %q is not the leased branch %q", view.HeadRefName, branch)
	}
	if view.BaseRefName != c.DefaultBranch {
		return fmt.Errorf("PR targets %q, not %q", view.BaseRefName, c.DefaultBranch)
	}
	issues, err := policycheck.ReferencedIssues(view.Body)
	if err != nil {
		return err
	}
	if len(issues) != 1 || issues[0] != number {
		return fmt.Errorf("PR body must reference exactly Refs #%d", number)
	}
	return nil
}

var runURL = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/actions/runs/([0-9]+)`)

// checkCIRun requires a successful GitHub Actions run of this repository on
// the PR head or its merge commit.
func checkCIRun(c model.Config, ciURL string, view prView) error {
	match := runURL.FindStringSubmatch(ciURL)
	if match == nil || !strings.EqualFold(match[1], c.Owner) || !strings.EqualFold(match[2], c.Repository) {
		return fmt.Errorf("CI URL %q is not a GitHub Actions run of %s/%s", ciURL, c.Owner, c.Repository)
	}
	output, err := ghOutput("api", fmt.Sprintf("repos/%s/%s/actions/runs/%s", c.Owner, c.Repository, match[3]))
	if err != nil {
		return fmt.Errorf("read CI run: %w", err)
	}
	var run struct {
		Conclusion   string `json:"conclusion"`
		HeadSHA      string `json:"head_sha"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	}
	if err := json.Unmarshal(output, &run); err != nil {
		return err
	}
	if run.Conclusion != "success" {
		return fmt.Errorf("CI run %s concluded %q, not success", match[3], run.Conclusion)
	}
	if run.HeadSHA == view.HeadRefOid || run.HeadSHA == view.MergeCommit.OID {
		return nil
	}
	for _, pull := range run.PullRequests {
		if pull.Number == view.Number {
			return nil
		}
	}
	return fmt.Errorf("CI run %s did not run on PR #%d", match[3], view.Number)
}
