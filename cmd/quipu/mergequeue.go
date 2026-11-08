package main

// merge_wait: background and update_behind: auto. `quipu done` hands a PR whose
// checks are still running to the Writer and returns at once. The Issue stays
// In review with its holder, so policy stays green, and each Writer pass merges
// the PR once its checks pass. A failed check returns the Issue to Ready with
// its branch and the reason. The merge goes through the same gate as
// `merge: agent`, and the evidence is validated after it, like any other.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	policycheck "github.com/isnakolah/quipu/internal/policy"
	"github.com/isnakolah/quipu/internal/writer"
)

// errPRBehind marks a merge refused because the PR is behind its base.
var errPRBehind = errors.New("pull request is behind its base")

// errChecksNeverRan marks a gate that failed only because required checks have
// not run, which is also how a PR looks right after a push.
var errChecksNeverRan = errors.New("required checks have not run")

// pendingMerge fills the evidence fields only the merge can answer.
const pendingMerge = "pending merge"

// queueGrace is how long a queued merge rides out a refused merge or a required
// check that never appears, before the work goes back to Ready.
const queueGrace = time.Hour

// mergeView is what a merge decision reads about a PR.
type mergeView struct {
	URL              string `json:"url"`
	State            string `json:"state"`
	Title            string `json:"title"`
	IsDraft          bool   `json:"isDraft"`
	MergeStateStatus string `json:"mergeStateStatus"`
	HeadRefOid       string `json:"headRefOid"`
	// BaseOID is the default branch's tip when the PR was compared with it.
	BaseOID     string `json:"-"`
	MergeCommit *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	Checks []prCheck `json:"statusCheckRollup"`
}

func loadMergeView(c model.Config, url string) (mergeView, error) {
	var view mergeView
	output, err := ghOutput("pr", "view", url, "--repo", c.Owner+"/"+c.Repository, "--json", "url,state,title,isDraft,mergeStateStatus,headRefOid,mergeCommit,statusCheckRollup")
	if err != nil {
		return view, err
	}
	return view, json.Unmarshal(output, &view)
}

// loadMergeState is the view a merge decision needs. GitHub reports BEHIND only
// under a rule that requires branches to be up to date, and a repository without
// one reports a stale PR as mergeable, so this asks git's own question. A PR that
// can't be compared doesn't merge.
func loadMergeState(c model.Config, url string) (mergeView, error) {
	view, err := loadMergeView(c, url)
	if err != nil || view.State != "OPEN" {
		return view, err
	}
	output, err := ghOutput("api", fmt.Sprintf("repos/%s/%s/compare/%s...%s?per_page=1", c.Owner, c.Repository, c.DefaultBranch, view.HeadRefOid), "--jq", "{behind_by: .behind_by, base: .base_commit.sha}")
	if err != nil {
		return view, flowErr("merge-blocked", "could not tell whether %s is behind %s: %v", url, c.DefaultBranch, err)
	}
	var compared struct {
		BehindBy int    `json:"behind_by"`
		Base     string `json:"base"`
	}
	if err := json.Unmarshal(output, &compared); err != nil {
		return view, flowErr("merge-blocked", "could not tell whether %s is behind %s: %v", url, c.DefaultBranch, err)
	}
	view.BaseOID, view.MergeStateStatus = compared.Base, behindState(view.MergeStateStatus, compared.BehindBy)
	return view, nil
}

// behindState merges GitHub's state with what the compare found. A conflict stays
// a conflict, and a BEHIND the compare disproves is GitHub still catching up.
func behindState(state string, behindBy int) string {
	switch {
	case state == "DIRTY":
		return state
	case behindBy > 0:
		return "BEHIND"
	case state == "BEHIND":
		return "UNKNOWN"
	}
	return state
}

// firstParent is the default branch's tip just before a merge commit.
func firstParent(c model.Config, sha string) (string, error) {
	output, err := ghOutput("api", fmt.Sprintf("repos/%s/%s/commits/%s", c.Owner, c.Repository, sha), "--jq", ".parents[0].sha")
	return strings.TrimSpace(string(output)), err
}

// baseRaceNote speaks up when the default branch moved between the last look at
// it and the merge. GitHub has no way to pin a merge to its base, so a merge from
// outside Quipu in that moment is reported, not prevented.
func baseRaceNote(tested, parent string) string {
	if tested == "" || parent == "" || tested == parent {
		return ""
	}
	return fmt.Sprintf("the default branch moved from %s to %s just before the merge, so this exact combination was not tested", tested, parent)
}

// addRisk puts a note in the evidence's risks.
func addRisk(e *writer.Evidence, note string) {
	switch {
	case note == "":
	case e.Risks == "" || e.Risks == "None":
		e.Risks = note
	default:
		e.Risks += "; " + note
	}
}

func requiredChecks(c model.Config) []string {
	return append(append([]string{}, c.Checks()...), "policy")
}

// mergePull merges with a merge commit, pinned to the head the checks ran on.
func mergePull(c model.Config, number int, view mergeView, flags ...string) error {
	return mergePullMarked(c, number, view, "", flags...)
}

// mergeMessage is the flags that set a merge commit's message. A marker joins the
// body rather than replacing it, since gh keeps only the last --body and the
// repository's commit format needs its footer.
func mergeMessage(c model.Config, number int, view mergeView, marker string) ([]string, error) {
	if c.CommitFormat == policycheck.IssuePrefix {
		subject, body, err := policycheck.IssuePrefixMessage(number, view.Title)
		if err != nil {
			return nil, flowErr("commit-format", "%v", err)
		}
		if marker != "" {
			body += "\n\n" + marker
		}
		return []string{"--subject", subject, "--body", body}, nil
	}
	if marker != "" {
		return []string{"--body", marker}, nil
	}
	return nil, nil
}

// mergePullMarked is mergePull with one more line in the merge commit's body.
func mergePullMarked(c model.Config, number int, view mergeView, marker string, flags ...string) error {
	args := []string{"pr", "merge", view.URL, "--repo", c.Owner + "/" + c.Repository, "--merge", "--match-head-commit", view.HeadRefOid}
	message, err := mergeMessage(c, number, view, marker)
	if err != nil {
		return err
	}
	if _, err := ghOutput(append(append(args, message...), flags...)...); err != nil {
		return flowErr("merge-blocked", "GitHub refused to merge %s: %v", view.URL, err)
	}
	return nil
}

// requiredRunURL is the run URL of the first required check, for the evidence.
func requiredRunURL(c model.Config, checks []prCheck) string {
	for _, check := range checks {
		if check.Name == c.Checks()[0] && check.Conclusion == "SUCCESS" {
			return check.DetailsURL
		}
	}
	return ""
}

// checksStillRunning reports whether only unfinished checks stand in the way.
func checksStillRunning(err error) bool {
	var coded *flowError
	return errors.Is(err, errChecksNeverRan) || (errors.As(err, &coded) && (coded.Code == "checks-pending" || coded.Code == "checks-no-runner"))
}

// queuedCIURL is the run the evidence cites: one on the head that merged, read
// after the merge, since an update may have moved the head since it was queued.
func queuedCIURL(c model.Config, stored string, checks []prCheck) string {
	if url := requiredRunURL(c, checks); url != "" {
		return url
	}
	if url := passingCIURL(checks); url != "" {
		return url
	}
	if stored == pendingMerge {
		return ""
	}
	return stored
}

// mergeDefaultIn merges the default branch into the worktree's branch and pushes
// it, returning the new head. A conflict is left for the agent, with the
// worktree as it was.
func mergeDefaultIn(w worktree, branch, defaultBranch string) (string, error) {
	if w.branch() != branch {
		return "", flowErr("merge-blocked", "%s is behind %s; run quipu done in the worktree on that branch so it can be updated", branch, defaultBranch)
	}
	if _, err := w.git("fetch", "-q", "origin", defaultBranch); err != nil {
		return "", flowErr("merge-blocked", "could not fetch %s: %v", defaultBranch, err)
	}
	if _, err := w.git("merge", "-q", "--no-edit", "origin/"+defaultBranch); err != nil {
		if _, inProgress := w.git("rev-parse", "-q", "--verify", "MERGE_HEAD"); inProgress == nil {
			if _, abort := w.git("merge", "--abort"); abort != nil {
				return "", flowErr("merge-conflict", "%s conflicts with %s, and the merge could not be aborted: %v", branch, defaultBranch, abort)
			}
			return "", flowErr("merge-conflict", "%s conflicts with %s", branch, defaultBranch)
		}
		return "", flowErr("merge-blocked", "could not merge %s into %s: %v", defaultBranch, branch, err)
	}
	if _, err := w.git("push", "-q", "origin", branch); err != nil {
		return "", flowErr("merge-blocked", "could not push %s: %v", branch, err)
	}
	return w.git("rev-parse", "HEAD")
}

// queueMerge hands the PR and its evidence to the Writer and frees this
// worktree. It does not wait: the request applies when the Writer next runs.
func queueMerge(c model.Config, w worktree, number int, actor, branch, url, head, cleanup string, e writer.Evidence, why error, respond func(flowResult) error) error {
	// The Writer checks this too, but by then this agent has moved on.
	if err := validateReviewPR(c, number, branch, url); err != nil {
		return flowErr("rejected", "PR %s cannot be queued: %v", url, err)
	}
	if head == "" {
		view, err := loadMergeView(c, url)
		if err != nil {
			return err
		}
		head = view.HeadRefOid
	}
	if _, err := postRequest(c, number, actor, writer.Request{Action: "merge.queue", PR: url, Head: head, Evidence: &e}); err != nil {
		return err
	}
	warnings := w.release(number, cleanup, c.DoneGrace())
	var coded *flowError
	reason, owner := why.Error(), ""
	if errors.As(why, &coded) {
		reason = coded.Message
		if coded.Code == "checks-no-runner" {
			owner = " Tell the owner what this names: it cannot merge until a runner takes the check."
		}
	}
	return respond(flowResult{Issue: number, Status: "In review", PR: url, Warnings: warnings, Next: fmt.Sprintf("Queued (%s). The Writer merges %s when its checks pass and closes #%d, or returns #%d to Ready with the reason.%s Do not wait: take other work with quipu start --next --json, and look again later with quipu status --json.", reason, url, number, number, owner)})
}

type queueStep int

const (
	queueWait queueStep = iota
	queueMerged
	queueMergeNow
	queueUpdate
	queueFail
	queueDrop
)

// decideQueuedMerge is what a Writer pass does with a queued PR, queued for age.
// A check that has not run yet waits, because GitHub creates checks after a
// push, but not past queueGrace. A failed one never merges.
func decideQueuedMerge(view mergeView, head string, required []string, update bool, age time.Duration) (queueStep, string) {
	switch view.State {
	case "MERGED":
		return queueMerged, ""
	case "CLOSED":
		return queueFail, "the PR was closed"
	}
	if view.HeadRefOid != head {
		return queueDrop, fmt.Sprintf("the PR moved from %s to %s after it was queued; rerun quipu done to queue it again", head, view.HeadRefOid)
	}
	verdicts := judgeChecks(view.Checks, required)
	var failing, neverRan []string
	waiting := false
	for _, name := range required {
		switch verdicts[name] {
		case checkFailing:
			failing = append(failing, name)
		case checkNeverRan:
			neverRan = append(neverRan, name)
		case checkPending:
			waiting = true
		}
	}
	switch {
	case len(failing) > 0:
		return queueFail, "required checks not passing: " + strings.Join(failing, ", ")
	case len(neverRan) > 0 && age >= queueGrace:
		return queueFail, "required checks never ran: " + strings.Join(neverRan, ", ")
	case waiting || len(neverRan) > 0:
		return queueWait, ""
	case view.IsDraft:
		return queueFail, "the PR is a draft"
	case view.MergeStateStatus == "DIRTY":
		return queueFail, "the PR conflicts with the default branch"
	case view.MergeStateStatus == "UNKNOWN":
		return queueWait, ""
	case view.MergeStateStatus == "BEHIND":
		if update {
			return queueUpdate, ""
		}
		return queueFail, "the PR is behind the default branch; merge it in and rerun quipu done"
	}
	return queueMergeNow, ""
}

// mergeOps is what a Writer pass does to GitHub for a queued merge.
type mergeOps struct {
	view   func(url string) (mergeView, error)
	merge  func(number int, view mergeView) error
	update func(url string) error
	parent func(sha string) (string, error)
	state  func(number int) (liveWork, error)
	move   func(itemID string, next writer.WorkState) error
	apply  func(number int, request writer.Request, receipts []writer.Receipt) writer.Receipt
}

func liveMergeOps(c model.Config) mergeOps {
	return mergeOps{
		view:   func(url string) (mergeView, error) { return loadMergeState(c, url) },
		parent: func(sha string) (string, error) { return firstParent(c, sha) },
		merge:  func(number int, view mergeView) error { return mergePull(c, number, view) },
		update: func(url string) error {
			_, err := ghOutput("pr", "update-branch", url, "--repo", c.Owner+"/"+c.Repository)
			return err
		},
		state: func(number int) (liveWork, error) { return loadLiveWork(c, number) },
		move:  func(itemID string, next writer.WorkState) error { return updateLiveWork(c, itemID, next) },
		apply: func(number int, request writer.Request, receipts []writer.Receipt) writer.Receipt {
			return applyWriterRequest(c, number, request, receipts)
		},
	}
}

// queuedMerge advances a queued PR and reports whether the work moved. Only the
// default branch's config can turn this on, and turning it off stops the queue.
func (w *watch) queuedMerge(state *liveWork) (bool, error) {
	if state.Status != "In review" && state.Status != "Evidence pending" {
		return false, nil
	}
	queued, ok := writer.LatestQueuedMerge(w.receipts)
	if !ok || queued.Merge.Batch {
		return false, nil
	}
	if !w.c.MergesInBackground() {
		w.report.Warnings = append(w.report.Warnings, fmt.Sprintf("#%d has a queued merge, but merge_wait: background is off; rerun quipu done", w.number))
		return false, nil
	}
	w.report.MergesQueued++
	if !w.apply {
		return false, nil
	}
	if state.Status == "Evidence pending" {
		return true, w.finishQueued(queued)
	}
	view, err := w.ops.view(queued.Merge.PR)
	if err != nil {
		w.report.Warnings = append(w.report.Warnings, fmt.Sprintf("read queued PR for #%d: %v", w.number, err))
		return false, nil
	}
	if queued.Merge.Head == "" {
		return true, w.adoptUpdatedHead(queued, view)
	}
	age := w.now.Sub(queued.At)
	race := ""
	step, reason := decideQueuedMerge(view, queued.Merge.Head, requiredChecks(w.c), w.c.UpdatesBehind(), age)
	switch step {
	case queueWait:
		return false, nil
	case queueDrop:
		return false, w.endQueue(queued, reason)
	case queueFail:
		return true, w.returnQueued(state, queued, reason)
	case queueUpdate:
		return true, w.updateQueued(state, queued)
	case queueMergeNow:
		tested := view.BaseOID
		mergeErr := w.ops.merge(w.number, view)
		if mergeErr == nil {
			w.report.MergesDone++
		}
		if view, err = w.ops.view(queued.Merge.PR); err != nil {
			// The PR is merged. The next pass sees that and records the evidence.
			w.report.Warnings = append(w.report.Warnings, fmt.Sprintf("read merged PR for #%d: %v", w.number, err))
			return true, nil
		}
		if view.State != "MERGED" {
			if mergeErr == nil {
				mergeErr = fmt.Errorf("PR %s is %s after merging", queued.Merge.PR, view.State)
			}
			// A refusal may pass: a 502, or a base that just moved. Only one that
			// lasts sends the work back.
			if age >= queueGrace {
				return true, w.returnQueued(state, queued, mergeErr.Error())
			}
			w.report.Warnings = append(w.report.Warnings, fmt.Sprintf("merge #%d: %v; the next pass tries again", w.number, mergeErr))
			return false, nil
		}
		if view.MergeCommit != nil {
			if parent, err := w.ops.parent(view.MergeCommit.OID); err != nil {
				w.report.Warnings = append(w.report.Warnings, fmt.Sprintf("read the merge of #%d: %v", w.number, err))
			} else {
				race = baseRaceNote(tested, parent)
			}
		}
	}
	return true, w.recordQueued(state, queued, view, race)
}

// endQueue ends a queued merge without moving the work.
func (w *watch) endQueue(queued writer.Receipt, detail string) error {
	return w.post(writer.Receipt{RequestID: writer.MergeResultID(queued), Result: "accepted", Detail: detail, At: w.now.UTC()})
}

// returnQueued sends the work back to Ready on its branch, with the reason.
func (w *watch) returnQueued(state *liveWork, queued writer.Receipt, reason string) error {
	next := writer.WorkState{Status: "Ready", Lease: writer.Lease{Branch: state.Branch}}
	if err := w.ops.move(state.ItemID, next); err != nil {
		return err
	}
	state.Status, state.Holder, state.Expiry = next.Status, "", ""
	return w.post(writer.Receipt{RequestID: writer.MergeResultID(queued), Result: "accepted", Detail: "queued merge stopped: " + reason + "; Issue returned to Ready on branch " + state.Branch, At: w.now.UTC(), State: writer.StateOf(next)})
}

// updateQueued brings a behind PR up to date. GitHub may not show the new head
// yet, so the queue records the old one and adopts whatever replaces it.
func (w *watch) updateQueued(state *liveWork, queued writer.Receipt) error {
	if err := w.ops.update(queued.Merge.PR); err != nil {
		return w.returnQueued(state, queued, "could not update the PR with the default branch: "+err.Error())
	}
	requeued := *queued.Merge
	requeued.Head, requeued.UpdatedFrom = "", queued.Merge.Head
	return w.post(writer.Receipt{RequestID: "merge-queue-" + queued.RequestID + "-updated", Result: "accepted", Detail: "updated with the default branch; merge stays queued on the new head", At: w.now.UTC(), Merge: &requeued})
}

// adoptUpdatedHead pins the queue to the head the Writer's update produced.
func (w *watch) adoptUpdatedHead(queued writer.Receipt, view mergeView) error {
	if view.HeadRefOid == queued.Merge.UpdatedFrom {
		return nil
	}
	requeued := *queued.Merge
	requeued.Head, requeued.UpdatedFrom = view.HeadRefOid, ""
	return w.post(writer.Receipt{RequestID: "merge-queue-" + queued.RequestID + "-" + view.HeadRefOid, Result: "accepted", Detail: "merge stays queued on updated head " + view.HeadRefOid, At: w.now.UTC(), Merge: &requeued})
}

// recordQueued records the merged PR's evidence through the same checks as
// evidence.submit, then closes the work. Rejected evidence ends the queue, and
// the holder records it with quipu done.
func (w *watch) recordQueued(state *liveWork, queued writer.Receipt, view mergeView, race string) error {
	fingerprint, err := fingerprintLiveWork(*state)
	if err != nil {
		return err
	}
	evidence := queued.Merge.Evidence
	if view.MergeCommit != nil {
		evidence.FinalSHA = view.MergeCommit.OID
	}
	evidence.CIURL = queuedCIURL(w.c, evidence.CIURL, view.Checks)
	addRisk(&evidence, race)
	request := writer.Request{ID: "merge-evidence-" + queued.RequestID, Action: "evidence.submit", IssueID: state.IssueID, Actor: state.Holder, ExpectedFingerprint: fingerprint, PR: queued.Merge.PR, Evidence: &evidence}
	receipt := w.ops.apply(w.number, request, w.receipts)
	if err := w.post(publishEvidenceJournal(w.c, w.number, request, receipt)); err != nil {
		return err
	}
	if receipt.Result != "accepted" {
		return w.endQueue(queued, fmt.Sprintf("PR %s merged, but its evidence was rejected: %s. Record it from any checkout with: quipu done %d --actor %s --json --criteria ... --commands ... --documentation ... --risks ... --boundary ... --ci-url ...", queued.Merge.PR, receipt.Detail, w.number, state.Holder))
	}
	return w.finishQueued(queued)
}

// finishQueued closes work whose evidence is accepted. A rejection here, such
// as a Wiki journal still pending, is retried on the next pass.
func (w *watch) finishQueued(queued writer.Receipt) error {
	state, err := w.ops.state(w.number)
	if err != nil {
		return err
	}
	fingerprint, err := fingerprintLiveWork(state)
	if err != nil {
		return err
	}
	request := writer.Request{ID: fmt.Sprintf("merge-done-%s-%d", queued.RequestID, w.now.Unix()), Action: "status", Status: "Done", IssueID: state.IssueID, Actor: state.Holder, ExpectedFingerprint: fingerprint}
	receipt := w.ops.apply(w.number, request, w.receipts)
	if receipt.Result != "accepted" {
		w.report.Warnings = append(w.report.Warnings, fmt.Sprintf("close #%d after its queued merge: %s", w.number, receipt.Detail))
		return nil
	}
	if err := w.post(receipt); err != nil {
		return err
	}
	return w.endQueue(queued, "merged and closed by the Writer")
}
