package main

// Report back without polling. `quipu status` lists each active PR with its check
// results and what to do next, so an agent can do other work and look once.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

type pullCheck struct {
	Name   string `json:"name"`
	Result string `json:"result"`
}

// pullReport is one active PR as `quipu status` reports it.
type pullReport struct {
	Issue  int         `json:"issue"`
	Holder string      `json:"holder,omitempty"`
	Yours  bool        `json:"yours,omitempty"`
	PR     string      `json:"pr,omitempty"`
	State  string      `json:"state,omitempty"`
	Checks []pullCheck `json:"checks,omitempty"`
	Queue  string      `json:"queue,omitempty"` // queued | rejected | stopped
	Next   string      `json:"next"`
	// owner is why this waits on the owner, for the owner queue.
	owner string
}

// queueState is what the receipts say about a PR's queued merge.
type queueState struct{ State, Detail string }

// queueStateOf reads a queued merge's state from an Issue's comments. Only
// trusted receipts count, and what they cover is the current claim.
func queueStateOf(comments []github.Comment, receipts []writer.Receipt) queueState {
	if _, queued := writer.LatestQueuedMerge(receipts); queued {
		return queueState{State: "queued"}
	}
	since := writer.CycleStart(receipts)
	var last queueState
	for _, comment := range comments {
		if !writer.IsRequest(comment.Body) {
			continue
		}
		if posted, err := time.Parse(time.RFC3339, comment.CreatedAt); err == nil && posted.Before(since) {
			continue
		}
		request, err := writer.ParseRequest(comment.Body)
		if err != nil || (request.Action != "merge.queue" && request.Action != "merge.batch") {
			continue
		}
		receipt, answered := receiptFor(receipts, request.ID)
		switch {
		case !answered:
			last = queueState{State: "queued", Detail: "waiting for the Writer's next run"}
		case receipt.Result != "accepted":
			last = queueState{State: "rejected", Detail: receipt.Detail}
		default:
			last = queueState{}
		}
	}
	if last.State != "" {
		return last
	}
	for i := len(receipts) - 1; i >= 0; i-- {
		r := receipts[i]
		if strings.HasPrefix(r.RequestID, "merge-result-") && !r.At.Before(since) {
			return queueState{State: "stopped", Detail: r.Detail}
		}
	}
	return queueState{}
}

// describePull says where a PR stands and what to do now.
func describePull(c model.Config, item statusItem, view mergeView, queue queueState, gate error) pullReport {
	report := pullReport{Issue: item.Number, Holder: item.LeaseHolder, PR: view.URL, State: view.State, Queue: queue.State}
	for _, check := range view.Checks {
		name, result := check.Name, check.Conclusion
		if name == "" {
			name, result = check.Context, check.State
		}
		if check.Status != "" && check.Status != "COMPLETED" {
			result = check.Status
		}
		report.Checks = append(report.Checks, pullCheck{name, result})
	}
	var coded *flowError
	errors.As(gate, &coded)
	inReview := item.Status == "In review"
	switch {
	case view.State == "MERGED" && !inReview:
		report.Next = "Merged before quipu done moved it to review. Tell the owner: they recover it with quipu pr link and quipu evidence submit."
		report.owner = "PR merged before it reached review; recover it with quipu pr link and quipu evidence submit"
	case view.State == "MERGED":
		report.Next = fmt.Sprintf("Merged, with no evidence recorded yet. Record it: quipu done %d --json --criteria ... --commands ... --documentation ... --risks ... --boundary ... (from another checkout, add --actor %s)", item.Number, item.LeaseHolder)
	case view.State == "CLOSED":
		report.Next = fmt.Sprintf("Closed without merging. Open a new PR from %s, then run: quipu done %d --json", item.Branch, item.Number)
	case queue.State == "rejected":
		report.Next = fmt.Sprintf("The Writer rejected the queued merge: %s. Rerun quipu done %d --json with the evidence flags.", queue.Detail, item.Number)
	case queue.State == "stopped":
		report.Next = fmt.Sprintf("The queued merge stopped: %s", queue.Detail)
	case coded != nil && coded.Code == "checks-failing" && !errors.Is(gate, errChecksNeverRan):
		report.Next = fmt.Sprintf("%s. Fix the cause, push to %s, then rerun quipu done %d.", coded.Message, item.Branch, item.Number)
	case coded != nil && coded.Code == "checks-no-runner":
		report.Next = fmt.Sprintf("%s. Do not poll: tell the owner, and take other work.", coded.Message)
		report.owner = "a required check cannot start; start its runner"
	case queue.State == "queued" && c.MergesInQueue():
		report.Next = "Queued: the merge queue tests it in a batch and lands it. Nothing to do; take other work."
	case queue.State == "queued":
		report.Next = "Queued: the Writer merges when the checks pass. Nothing to do; take other work."
	case c.MergesInQueue() && inReview:
		report.Next = fmt.Sprintf("Queue it with the evidence flags: quipu done %d --json --criteria ... --commands ... --documentation ... --risks ... --boundary local", item.Number)
	case !inReview && !c.AgentsMerge():
		report.Next = fmt.Sprintf("Not in review yet. Move it there: quipu done %d --json. The checks need not have finished.", item.Number)
	case gate != nil:
		report.Next = "Checks have not finished. Do other work and look again later; no need to wait."
	case c.AgentsMerge():
		report.Next = fmt.Sprintf("Checks passed. Merge it: quipu done %d --json --criteria ... --commands ... --documentation ... --risks ... --boundary ...", item.Number)
	default:
		report.Next = "Checks passed. The owner reviews and merges."
	}
	return report
}

// addPullsToQueues folds what the PRs say into the owner queue and attention. A
// PR that can't start a check, or merged early, is the owner's whatever else was
// listed for its Issue. Merged and awaiting evidence is not: the holder records it.
func addPullsToQueues(queue []ownerItem, attention []string, pulls []pullReport, titles map[int]string) ([]ownerItem, []string) {
	for _, pull := range pulls {
		if pull.owner == "" && pull.State != "MERGED" {
			continue
		}
		queue = slices.DeleteFunc(queue, func(item ownerItem) bool { return item.Number == pull.Issue })
		if pull.owner == "" {
			attention = append(attention, fmt.Sprintf("#%d PR merged with no evidence recorded; its holder runs quipu done, or the owner does with --actor", pull.Issue))
			continue
		}
		queue = append(queue, ownerItem{Number: pull.Issue, Title: titles[pull.Issue], Reason: pull.owner})
		attention = append(attention, fmt.Sprintf("#%d %s", pull.Issue, pull.owner))
	}
	slices.SortStableFunc(queue, func(a, b ownerItem) int { return a.Number - b.Number })
	return queue, attention
}

// activePullStatus reports whether work in this status may have an open PR.
func activePullStatus(status string) bool {
	return status == "Claimed" || status == "In progress" || status == "In review"
}

// branchPulls lists the PRs from this repository's branch. A fork's PR with the
// same branch name is not this work.
func branchPulls(c model.Config, branch string) ([]struct{ URL, State string }, error) {
	output, err := ghOutput("pr", "list", "--repo", c.Owner+"/"+c.Repository, "--head", branch, "--state", "all", "--json", "url,state,isCrossRepository", "--limit", "20")
	if err != nil {
		return nil, err
	}
	var listed []struct {
		URL, State        string
		IsCrossRepository bool `json:"isCrossRepository"`
	}
	if err := json.Unmarshal(output, &listed); err != nil {
		return nil, err
	}
	var pulls []struct{ URL, State string }
	for _, pull := range listed {
		if !pull.IsCrossRepository {
			pulls = append(pulls, struct{ URL, State string }{pull.URL, pull.State})
		}
	}
	return pulls, nil
}

// branchPullURL finds the PR from this repository's branch, preferring an open
// or merged one over closed attempts.
func branchPullURL(c model.Config, branch string) (string, error) {
	pulls, err := branchPulls(c, branch)
	if err != nil {
		return "", err
	}
	if url := pickPullRequest(pulls); url != "" {
		return url, nil
	}
	if len(pulls) > 0 {
		return pulls[0].URL, nil
	}
	return "", nil
}

// loadPullReports reads each active PR once. A failed read is reported on that
// PR and never fails `quipu status`.
func loadPullReports(c model.Config, items []statusItem, actor string) []pullReport {
	reports := []pullReport{}
	required := requiredChecks(c)
	for _, item := range items {
		if !activePullStatus(item.Status) || item.Branch == "" || strings.EqualFold(item.Kind, "Epic") {
			continue
		}
		failed := func(err error) {
			reports = append(reports, pullReport{Issue: item.Number, Holder: item.LeaseHolder, Yours: item.LeaseHolder == actor, Next: "Could not read its PR: " + err.Error()})
		}
		url, err := branchPullURL(c, item.Branch)
		if err != nil {
			failed(err)
			continue
		}
		if url == "" {
			continue
		}
		view, err := loadMergeView(c, url)
		if err != nil {
			failed(err)
			continue
		}
		var queue queueState
		if item.Status == "In review" && c.MergesInBackground() {
			comments, err := github.NewClient().ListComments(context.Background(), c.Owner, c.Repository, item.Number)
			if err != nil {
				failed(err)
				continue
			}
			queue = queueStateOf(comments, receiptTrust(c).Receipts(comments))
		}
		gate := mergeGate(view.Checks, required)
		if view.State == "OPEN" {
			gate = explainPendingChecks(c, gate, view.Checks, required)
		}
		report := describePull(c, item, view, queue, gate)
		report.Yours = item.LeaseHolder == actor
		reports = append(reports, report)
	}
	return reports
}
