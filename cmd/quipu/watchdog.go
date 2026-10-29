package main

// The watchdog: every Writer pass applies pending requests, then puts back
// anything changed outside Quipu. Once a repository uses Quipu, Quipu is the
// only way work moves: hand-closed Issues reopen, hand-edited Project fields
// return to the last receipt, Issues created outside the flow are flagged for
// triage, and PR policy checks re-run when the work under them changes.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

const (
	triageLabel  = "quipu:needs-triage"
	triageMarker = "<!-- quipu-triage:v1 -->"
)

// ownerClosableKinds may be closed by the owner directly; they have no PR or
// evidence, so a close by an authorized actor records them Done.
var ownerClosableKinds = map[string]bool{"Epic": true, "Decision": true, "Gate": true}

type issueReport struct {
	Pending   []string
	Receipted int
	Reclaimed int
	Reopened  int
	Restored  int
	Triaged   int
	Rechecked int
	Warnings  []string
}

func writerReconcileCommand(args []string) error {
	fs := flag.NewFlagSet("writer reconcile", flag.ContinueOnError)
	apply := applyFlag(fs)
	asJSON := jsonFlag(fs)
	since := fs.Duration("since", 0, "only Issues updated within this window, such as 30m; all Issues when 0")
	sinceLastRun := fs.Bool("since-last-run", false, "only Issues updated since the last successful Writer run began; all Issues when there is none")
	if err := fs.Parse(args); err != nil {
		return err
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
	window := *since
	if *sinceLastRun {
		started, err := lastWriterRun(c)
		if err != nil {
			return err
		}
		window = drainWindow(started, now)
	}
	output, err := ghOutput("api", "--paginate", "--slurp", reconcileIssuesPath(c, window, now))
	if err != nil {
		return err
	}
	var pages [][]struct {
		Number      int             `json:"number"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	if err := json.Unmarshal(output, &pages); err != nil {
		return err
	}
	var numbers []int
	for _, page := range pages {
		for _, item := range page {
			// REST's Issue endpoint also returns pull requests. Writer state is
			// valid only for canonical Issues, never PR numbers.
			if len(item.PullRequest) == 0 || string(item.PullRequest) == "null" {
				numbers = append(numbers, item.Number)
			}
		}
	}
	var total issueReport
	failures := []string{}
	for _, number := range numbers {
		// One broken Issue must not stop the pass for every other Issue.
		report, err := reconcileIssue(c, number, *apply, trust, now)
		if err != nil {
			failures = append(failures, fmt.Sprintf("#%d: %v", number, err))
		}
		total.Receipted += report.Receipted
		total.Reclaimed += report.Reclaimed
		total.Reopened += report.Reopened
		total.Restored += report.Restored
		total.Triaged += report.Triaged
		total.Rechecked += report.Rechecked
		for _, warning := range report.Warnings {
			total.Warnings = append(total.Warnings, fmt.Sprintf("#%d: %s", number, warning))
		}
	}
	result := map[string]any{"window": window.String(), "issues_scanned": len(numbers), "requests_receipted": total.Receipted, "leases_reclaimed": total.Reclaimed,
		"issues_reopened": total.Reopened, "drift_restored": total.Restored, "issues_triaged": total.Triaged,
		"policy_rechecked": total.Rechecked, "warnings": append([]string{}, total.Warnings...), "errors": failures, "apply": *apply}
	if err := printValue(result, *asJSON); err != nil {
		return err
	}
	if len(failures) > 0 {
		return fmt.Errorf("reconcile failed for %d Issue(s): %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

// reconcileIssuesPath lists the Issues a reconcile pass scans. With since,
// only Issues updated in that window: event-triggered Writer runs scan back to
// the last successful run, which drains every pending request even though
// Actions keeps only one queued run per concurrency group.
func reconcileIssuesPath(c model.Config, since time.Duration, now time.Time) string {
	path := "repos/" + c.Owner + "/" + c.Repository + "/issues?state=all&per_page=100"
	if since > 0 {
		path += "&since=" + now.Add(-since).UTC().Format(time.RFC3339)
	}
	return path
}

// drainMargin covers clock skew between runners and GitHub.
const drainMargin = 10 * time.Minute

// drainWindow reaches back to the start of the last successful Writer run,
// so a runner that slept for hours, or a run GitHub cancelled in the
// concurrency queue, loses no request. Zero (every Issue) without one.
func drainWindow(lastStarted, now time.Time) time.Duration {
	if lastStarted.IsZero() || lastStarted.After(now) {
		return 0
	}
	return now.Sub(lastStarted) + drainMargin
}

// lastWriterRun is when the last successful Quipu Writer run started.
func lastWriterRun(c model.Config) (time.Time, error) {
	output, err := ghOutput("api", fmt.Sprintf("repos/%s/%s/actions/workflows/quipu-writer.yml/runs?status=success&per_page=1", c.Owner, c.Repository))
	if err != nil {
		if strings.Contains(err.Error(), "Not Found") {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	var runs struct {
		Runs []struct {
			Started string `json:"run_started_at"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(output, &runs); err != nil || len(runs.Runs) == 0 {
		return time.Time{}, err
	}
	started, _ := time.Parse(time.RFC3339, runs.Runs[0].Started)
	return started, nil
}

// watch is one reconcile pass over one Issue.
type watch struct {
	c        model.Config
	number   int
	apply    bool
	now      time.Time
	trust    writer.Trust
	ctx      context.Context
	client   *github.Client // Project, Issue state, labels: the Writer token
	poster   *github.Client // receipts and Writer comments: github-actions[bot]
	comments []github.Comment
	receipts []writer.Receipt
	report   issueReport
}

func reconcileIssue(c model.Config, number int, apply bool, trust writer.Trust, now time.Time) (issueReport, error) {
	w := &watch{c: c, number: number, apply: apply, now: now, trust: trust, ctx: context.Background(), client: github.NewClient(), poster: receiptClient()}
	err := w.run()
	return w.report, err
}

func (w *watch) run() error {
	comments, err := w.client.ListComments(w.ctx, w.c.Owner, w.c.Repository, w.number)
	if err != nil {
		return err
	}
	w.comments, w.receipts = comments, w.trust.Receipts(comments)
	pending, rejected := writer.Pending(comments, w.trust)
	for _, item := range pending {
		w.report.Pending = append(w.report.Pending, item.Request.ID)
	}
	var before liveWork
	if len(pending) > 0 {
		before, _ = loadLiveWork(w.c, w.number)
	}
	if w.apply {
		for _, item := range pending {
			receipt := applyAuthorizedWriterRequest(w.c, w.number, item, w.receipts)
			if err := w.post(publishEvidenceJournal(w.c, w.number, item.Request, receipt)); err != nil {
				return err
			}
		}
		for _, receipt := range rejected {
			if err := w.post(receipt); err != nil {
				return err
			}
		}
		if err := retryPendingWikiJournals(w.c, w.number, w.receipts, w.post); err != nil {
			return err
		}
	}
	changed := len(pending) > 0
	state, err := loadLiveWork(w.c, w.number)
	inProject := !errors.Is(err, errNotInProject)
	if err != nil && inProject {
		return err
	}
	if err := w.removeStatusLabels(state); err != nil {
		return err
	}
	if state.IssueState == "CLOSED" {
		if !inProject {
			return nil
		}
		reopened, err := w.closed(state)
		if err != nil || !reopened {
			return err
		}
		state.IssueState, changed = "OPEN", true
	}
	problems := triageProblems(state, inProject)
	if err := w.triage(state, problems); err != nil {
		return err
	}
	if !inProject || labelKind(state.Labels) == "" {
		return nil
	}
	if w.apply && (state.Status == "" || state.Kind == "" || state.Area == "" || state.Priority == "" || state.Proof == "") {
		if err := initializeProjectFields(w.c, w.number, state); err != nil {
			return err
		}
		if state, err = loadLiveWork(w.c, w.number); err != nil {
			return err
		}
	}
	// A request applied in this pass already moved the work; comparing with
	// the receipt it just posted would only race the Project's own update.
	if !changed {
		restored, err := w.drift(&state)
		if err != nil {
			return err
		}
		changed = restored
	}
	reclaimed, err := w.reclaim(&state)
	if err != nil {
		return err
	}
	if w.apply && (changed || reclaimed) {
		for _, branch := range uniqueBranches(before.Branch, state.Branch) {
			w.recheckPolicy(branch)
		}
	}
	return nil
}

// post publishes a receipt as the Writer and records it for the rest of the pass.
func (w *watch) post(receipt writer.Receipt) error {
	body, err := writer.RenderReceipt(receipt)
	if err != nil {
		return err
	}
	if _, err := w.poster.CreateComment(w.ctx, w.c.Owner, w.c.Repository, w.number, body); err != nil {
		return err
	}
	w.receipts = append(w.receipts, receipt)
	w.report.Receipted++
	return nil
}

func (w *watch) removeStatusLabels(state liveWork) error {
	for _, label := range state.Labels {
		if !strings.HasPrefix(label, "status:") {
			continue
		}
		w.report.Restored++
		if !w.apply {
			continue
		}
		if err := w.client.RemoveLabel(w.ctx, w.c.Owner, w.c.Repository, w.number, label); err != nil {
			return fmt.Errorf("remove %s: %w", label, err)
		}
	}
	return nil
}

// closedVerdict says what a closed Issue needs: "ok", a status the owner's
// close records ("Cancelled" or "Done"), or "reopen".
func closedVerdict(c model.Config, state liveWork, receipts []writer.Receipt, trust writer.Trust) string {
	if writer.Closed(receipts) {
		return "ok"
	}
	if closed, err := time.Parse(time.RFC3339, state.ClosedAt); err == nil && closed.Before(trust.LegacyBefore) {
		return "ok"
	}
	if authorizedActor(c.AuthorizedActors, state.ClosedBy) {
		if state.StateReason == "NOT_PLANNED" || state.StateReason == "DUPLICATE" {
			return "Cancelled"
		}
		if ownerClosableKinds[labelKind(state.Labels)] {
			return "Done"
		}
	}
	return "reopen"
}

// closed handles a closed Issue and reports whether it was reopened.
func (w *watch) closed(state liveWork) (bool, error) {
	verdict := closedVerdict(w.c, state, w.receipts, w.trust)
	switch verdict {
	case "ok":
		return false, nil
	case "Cancelled", "Done":
		if !w.apply {
			return false, nil
		}
		next := writer.WorkState{Status: verdict}
		if err := updateLiveWork(w.c, state.ItemID, next); err != nil {
			return false, err
		}
		return false, w.post(writer.Receipt{RequestID: "owner-close-" + state.IssueID + "-" + state.ClosedAt, Result: "accepted",
			Detail: "closed by " + state.ClosedBy + "; lifecycle state changed to " + verdict, At: w.now.UTC(), State: writer.StateOf(next)})
	}
	w.report.Reopened++
	if !w.apply {
		return true, nil
	}
	if err := w.client.SetIssueState(w.ctx, w.c.Owner, w.c.Repository, w.number, "open"); err != nil {
		return false, fmt.Errorf("reopen closed Issue: %w", err)
	}
	return true, w.post(writer.Receipt{RequestID: "premature-close-" + state.IssueID + "-" + state.ClosedAt, Result: "accepted",
		Detail: fmt.Sprintf("Issue reopened: only the Writer closes work. Finish it with `quipu done %d`; the owner cancels it by closing as not planned", w.number), At: w.now.UTC()})
}

// triageProblems lists why an Issue is outside the flow, so no agent can take it.
func triageProblems(state liveWork, inProject bool) []string {
	var problems []string
	if !inProject {
		problems = append(problems, "it is not in the Quipu Project")
	}
	kind := labelKind(state.Labels)
	if kind == "" {
		problems = append(problems, "it has no `kind:` label")
	} else if kind != "Epic" && state.ParentID == "" {
		problems = append(problems, "it has no native parent Issue")
	}
	if implementableKinds[kind] {
		if err := model.ValidateWorkContract(state.Body); err != nil {
			problems = append(problems, "its work contract is invalid: "+err.Error())
		}
	}
	return problems
}

func (w *watch) triage(state liveWork, problems []string) error {
	labeled := hasLabel(state.Labels, triageLabel)
	if len(problems) == 0 {
		if labeled && w.apply {
			return w.client.RemoveLabel(w.ctx, w.c.Owner, w.c.Repository, w.number, triageLabel)
		}
		return nil
	}
	w.report.Triaged++
	if !w.apply {
		return nil
	}
	if !labeled {
		if err := w.client.AddLabels(w.ctx, w.c.Owner, w.c.Repository, w.number, triageLabel); err != nil {
			return err
		}
	}
	for _, comment := range w.comments {
		if strings.Contains(comment.Body, triageMarker) && w.trust.Trusted(comment) {
			return nil
		}
	}
	body := triageMarker + "\nThis Issue is outside the Quipu flow, so no agent will take it:\n"
	for _, problem := range problems {
		body += "- " + problem + "\n"
	}
	body += "\nOwner: create work with `quipu issue create --kind story --area AREA --parent N --body-file CONTRACT.md --apply`, or fix the points above. The `" + triageLabel + "` label clears itself once the Issue is valid."
	_, err := w.poster.CreateComment(w.ctx, w.c.Owner, w.c.Repository, w.number, body)
	return err
}

// expectedState is what the Project must show: the snapshot in the latest
// trusted receipt. Issues without receipts may sit in Backlog, Ready, or
// Blocked with no lease; Issues whose receipts predate snapshots are not
// checked.
func expectedState(state liveWork, receipts []writer.Receipt, trust writer.Trust) *writer.ReceiptState {
	if last := writer.LastState(receipts); last != nil {
		return last
	}
	if len(receipts) > 0 {
		return nil
	}
	if created, err := time.Parse(time.RFC3339, state.CreatedAt); err != nil || created.Before(trust.LegacyBefore) {
		return nil
	}
	switch state.Status {
	case "", "Backlog", "Ready", "Blocked":
		if state.Holder == "" && state.Expiry == "" && state.Branch == "" {
			return nil
		}
	}
	return &writer.ReceiptState{Status: "Backlog"}
}

// driftFields lists every lifecycle field that differs from want.
func driftFields(state liveWork, want writer.ReceiptState) []string {
	var changed []string
	check := func(name, got, expected string) {
		if got != expected {
			changed = append(changed, fmt.Sprintf("%s %q, expected %q", name, got, expected))
		}
	}
	check("Status", state.Status, want.Status)
	check("Lease holder", state.Holder, want.Holder)
	check("Lease expires", state.Expiry, want.Expires)
	check("Branch", state.Branch, want.Branch)
	return changed
}

// drift puts back Project fields edited by hand.
func (w *watch) drift(state *liveWork) (bool, error) {
	want := expectedState(*state, w.receipts, w.trust)
	if want == nil {
		return false, nil
	}
	changed := driftFields(*state, *want)
	if len(changed) == 0 {
		return false, nil
	}
	w.report.Restored++
	if !w.apply {
		return false, nil
	}
	if err := updateLiveWork(w.c, state.ItemID, want.WorkState()); err != nil {
		return false, fmt.Errorf("restore Project fields: %w", err)
	}
	sum := sha256.Sum256([]byte(strings.Join(changed, "|") + w.now.String()))
	err := w.post(writer.Receipt{RequestID: fmt.Sprintf("drift-%s-%x", state.IssueID, sum[:6]), Result: "accepted",
		Detail: "Project edited outside Quipu (" + strings.Join(changed, "; ") + "); restored. Lifecycle fields change only through quipu: agents use `quipu start` and `quipu done`, the owner uses `quipu ready`",
		At:     w.now.UTC(), State: want})
	state.Status, state.Holder, state.Expiry, state.Branch = want.Status, want.Holder, want.Expires, want.Branch
	return true, err
}

func (w *watch) reclaim(state *liveWork) (bool, error) {
	expires, _ := time.Parse(time.RFC3339, state.Expiry)
	next, reclaimed := writer.ReclaimExpired(writer.WorkState{Status: state.Status, Lease: writer.Lease{Holder: state.Holder, Expires: expires, Branch: state.Branch}}, w.now)
	if !reclaimed {
		return false, nil
	}
	w.report.Reclaimed++
	if !w.apply {
		return false, nil
	}
	if err := updateLiveWork(w.c, state.ItemID, next); err != nil {
		return false, err
	}
	requestID := fmt.Sprintf("lease-expiry-%s-%s", state.IssueID, state.Expiry)
	state.Status, state.Holder, state.Expiry = next.Status, next.Lease.Holder, leaseText(next.Lease.Expires)
	if _, done := receiptFor(w.receipts, requestID); done {
		return true, nil
	}
	return true, w.post(writer.Receipt{RequestID: requestID, Result: "accepted", Detail: "expired lease cleared; Issue returned to Ready", At: w.now.UTC(), State: writer.StateOf(next)})
}

// recheckPolicy re-runs the policy check of open PRs from branch whose
// verdict changed with the work under them. Failures are warnings: the next
// PR event runs the check anyway.
func (w *watch) recheckPolicy(branch string) {
	output, err := ghOutput("pr", "list", "--repo", w.c.Owner+"/"+w.c.Repository, "--head", branch, "--state", "open", "--json", "number")
	if err != nil {
		w.report.Warnings = append(w.report.Warnings, "list PRs for "+branch+": "+err.Error())
		return
	}
	var pulls []struct {
		Number int `json:"number"`
	}
	_ = json.Unmarshal(output, &pulls)
	for _, pr := range pulls {
		pull, err := loadPullFacts(w.c, fmt.Sprint(pr.Number))
		if err != nil {
			w.report.Warnings = append(w.report.Warnings, err.Error())
			continue
		}
		valid := len(prViolations(w.c, pull, func(n int) (liveWork, error) { return loadLiveWork(w.c, n) }, w.now)) == 0
		passed, runID, err := policyRun(w.c, pull.HeadSHA)
		if err != nil || runID == "" || passed == valid {
			continue
		}
		path := fmt.Sprintf("/repos/%s/%s/actions/runs/%s/rerun", w.c.Owner, w.c.Repository, runID)
		if err := w.poster.Do(w.ctx, "POST", path, nil, nil); err != nil {
			if err := w.client.Do(w.ctx, "POST", path, nil, nil); err != nil {
				w.report.Warnings = append(w.report.Warnings, fmt.Sprintf("re-run policy for PR #%d: %v", pr.Number, err))
				continue
			}
		}
		w.report.Rechecked++
	}
}

// policyRun finds the latest completed policy check on sha and its run ID.
func policyRun(c model.Config, sha string) (bool, string, error) {
	output, err := ghOutput("api", fmt.Sprintf("repos/%s/%s/commits/%s/check-runs?check_name=policy", c.Owner, c.Repository, sha))
	if err != nil {
		return false, "", err
	}
	var result struct {
		CheckRuns []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			DetailsURL string `json:"details_url"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return false, "", err
	}
	for _, run := range result.CheckRuns {
		if run.Status != "completed" {
			continue
		}
		if match := runURL.FindStringSubmatch(run.DetailsURL); match != nil {
			return run.Conclusion == "success", match[3], nil
		}
	}
	return false, "", nil
}

// labelKind is the Issue's kind from its kind: label, title-cased.
func labelKind(labels []string) string {
	for _, label := range labels {
		if value, ok := strings.CutPrefix(label, "kind:"); ok && value != "" {
			return strings.ToUpper(value[:1]) + value[1:]
		}
	}
	return ""
}

func uniqueBranches(branches ...string) []string {
	var out []string
	for _, branch := range branches {
		if branch != "" && !contains(out, branch) {
			out = append(out, branch)
		}
	}
	return out
}
