package main

// merge: queue. `quipu done` validates a PR and queues it through the Writer,
// then returns at once. One local worker per clone takes a batch, replays its PRs
// in order onto the latest default branch in a reusable worktree, and runs the
// check once. A pass lands the PRs one at a time through GitHub, each pinned to
// its head and proved against the tree that was tested. A failure bisects to the
// PR at fault, lands the good ones, and returns only that one.
//
// GitHub says what landed and the Writer's receipts say who is queued, so a
// restart loses nothing. The journal only remembers what the check found.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

const (
	queuePrefix      = "None: merge queue at "
	queueEnvironment = "The merge queue's worktree on this machine; PR checks waived"
	queueArtifacts   = "None: no CI ran for this merge"
	// queueNoVerdicts is how many runs in a row may end with no verdict, such as a
	// timeout, before the queue stops and asks.
	queueNoVerdicts = 3
	// queueRefusals is how many times GitHub may refuse a landing before the same.
	queueRefusals = 5
	// queuePoll is how often an idle worker looks again.
	queuePoll = 30 * time.Second
	// queueQuietPass is the longest the worker goes without a full pass, for what
	// leaves no comment, such as a push to a queued PR or a hand-closed one.
	queueQuietPass = 5 * time.Minute
	// queueSettle is how long after a new comment it keeps passing, since the board
	// can lag the receipt that moved work on it.
	queueSettle = time.Minute
	// queueIdleExit outlasts a Writer that hasn't answered yet.
	queueIdleExit = 15 * time.Minute
	// queueLogTail is how much of a failed check's output the PR's Issue gets.
	queueLogTail = 2000
)

var queueValue = regexp.MustCompile(`^None: merge queue at ([0-9a-f]{40}); (check ".*" exited 0 on tree [0-9a-f]{40} \(base [0-9a-f]{40}, \d+ runs, \d+ flaky\)|check result not known to this rerun), as reported by quipu queue$`)

// queueEvidence is the CI field of a merge the queue tested. It is the worker's
// report: the Writer cannot verify a run on another machine.
func queueEvidence(head string, batch queueBatch, tree string) string {
	return fmt.Sprintf("%s%s; check %q exited 0 on tree %s (base %s, %d runs, %d flaky), as reported by quipu queue", queuePrefix, head, batch.Command, tree, batch.Base, batch.Runs, batch.Flakes)
}

// queueEvidenceUnknown is the CI field when the journal no longer has the result.
func queueEvidenceUnknown(head string) string {
	return queuePrefix + head + "; check result not known to this rerun, as reported by quipu queue"
}

func isQueueEvidence(ci string) bool { return strings.HasPrefix(ci, queuePrefix) }

// checkQueueEvidence is what the Writer asks of evidence from the queue. The
// default branch must have the queue on, and no check that ran may have failed.
func checkQueueEvidence(c model.Config, e writer.Evidence, view prView) error {
	if !c.MergesInQueue() {
		return errors.New("evidence from the merge queue needs merge: queue on the default branch")
	}
	match := queueValue.FindStringSubmatch(e.CIURL)
	if match == nil {
		return fmt.Errorf("CI value %q is not the form quipu queue writes", e.CIURL)
	}
	if match[1] != view.HeadRefOid {
		return fmt.Errorf("evidence says the queue tested %s, but the PR's head is %s", match[1], view.HeadRefOid)
	}
	if e.Boundary != "local" {
		return fmt.Errorf("the merge queue verified this locally, so its boundary must be local, not %q", e.Boundary)
	}
	_, err := waivedChecks(checksBefore(view.Checks, view.MergedAt), requiredChecks(c))
	return err
}

// queueMember is one PR waiting in the queue, as the Writer's receipts say.
type queueMember struct {
	Issue    int
	Holder   string
	Branch   string
	PR       string
	Head     string
	QueuedAt time.Time
	Evidence writer.Evidence
}

// batchStep is one PR replayed in a batch: Tree is the tree with it on top.
type batchStep struct {
	Issue int    `json:"issue"`
	PR    string `json:"pr"`
	Head  string `json:"head"`
	Tree  string `json:"tree"`
	// Note is what landing found that the evidence must say.
	Note string `json:"note,omitempty"`
}

// queueBatch is a tested result on its way onto the default branch.
type queueBatch struct {
	Base    string      `json:"base"`
	Command string      `json:"command"`
	Runs    int         `json:"runs"`
	Flakes  int         `json:"flakes"`
	Steps   []batchStep `json:"steps"`
}

// queueResult is what a landed PR's evidence needs.
type queueResult struct {
	Batch queueBatch `json:"batch"`
	Step  batchStep  `json:"step"`
}

// queueJournal is what only this machine knows. Losing it costs a retest and an
// honest "not known" in the evidence, never a PR.
type queueJournal struct {
	// Hold stops the queue until the owner runs quipu queue resume.
	Hold       string              `json:"hold,omitempty"`
	NoVerdicts int                 `json:"no_verdicts,omitempty"`
	Refusals   int                 `json:"refusals,omitempty"`
	Batch      *queueBatch         `json:"batch,omitempty"`
	Landed     map[int]queueResult `json:"landed,omitempty"`
	// Rush sends the next batch at once: its PRs already waited their turn in a
	// batch that couldn't finish.
	Rush bool `json:"rush,omitempty"`
	// Flaked names the last flaky run, for anyone reading status.
	Flaked string `json:"flaked,omitempty"`
}

// queueOps is what the worker does to GitHub, and to a machine slot. Git itself
// runs for real in the queue's worktree.
type queueOps struct {
	members    func() ([]queueMember, error)
	view       func(url string) (mergeView, error)
	violations func(m queueMember) ([]string, error)
	land       func(m batchStep, view mergeView, last bool) error
	record     func(m queueMember, e writer.Evidence) error
	giveBack   func(m queueMember, reason string) error
	hold       func() (func(), error)
	now        func() time.Time
}

// queueRun is one worker on one clone.
type queueRun struct {
	c       model.Config
	tree    worktree
	journal string
	logs    string
	ops     queueOps
	// current is the journal as of this pass.
	current queueJournal
	timeout time.Duration
	// wake is when a waiting batch's timer runs out.
	wake time.Time
}

func (q *queueRun) loadJournal() {
	q.current = queueJournal{}
	if raw, err := os.ReadFile(q.journal); err == nil {
		// A journal that can't be read is a lost one.
		_ = json.Unmarshal(raw, &q.current)
	}
}

func (q *queueRun) saveJournal() error {
	raw, err := json.MarshalIndent(q.current, "", "  ")
	if err != nil {
		return err
	}
	tmp := q.journal + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, q.journal)
}

// queueOutcome says what a pass did, for the worker's pacing.
type queueOutcome int

const (
	queueIdle    queueOutcome = iota // nobody is queued
	queueWaiting                     // a batch isn't due, or the machine is busy
	queueHeld                        // the owner has to look
	queueMoved                       // something changed: look again at once
)

// pass does the next thing the queue needs, and it's safe to stop anywhere.
func (q *queueRun) pass() (queueOutcome, error) {
	q.loadJournal()
	members, err := q.ops.members()
	if err != nil {
		return queueWaiting, err
	}
	open, moved, err := q.settle(members)
	if err != nil {
		return queueWaiting, err
	}
	q.pruneResults(members)
	if q.current.Hold != "" {
		return queueHeld, q.saveJournal()
	}
	if q.current.Batch != nil {
		return q.resumeLanding(open)
	}
	if len(open) == 0 {
		if moved {
			return queueMoved, q.saveJournal()
		}
		return queueIdle, q.saveJournal()
	}
	size, wait := q.c.QueueBatchSize(), q.c.QueueMaxWait()
	if len(open) < size && q.ops.now().Sub(open[0].QueuedAt) < wait && !q.current.Rush {
		q.wake = open[0].QueuedAt.Add(wait)
		return queueWaiting, q.saveJournal()
	}
	q.current.Rush = false
	return q.runBatch(open[:min(size, len(open))])
}

// settle deals with whoever needs no test: merged PRs get their evidence, and a PR
// that changed under the queue goes back. What's left is ready to batch.
func (q *queueRun) settle(members []queueMember) ([]queueMember, bool, error) {
	var open []queueMember
	moved := false
	for _, m := range members {
		view, err := q.ops.view(m.PR)
		if err != nil {
			return nil, moved, err
		}
		reason := ""
		switch {
		case view.State == "MERGED":
			moved = true
			if err := q.ops.record(m, q.evidenceFor(m, view)); err != nil {
				return nil, moved, err
			}
			continue
		case view.State == "CLOSED":
			reason = "the PR was closed"
		case view.HeadRefOid != m.Head:
			reason = fmt.Sprintf("the PR moved from %s to %s after it was queued; rerun quipu done to queue it again", m.Head, view.HeadRefOid)
		case view.IsDraft:
			reason = "the PR is a draft"
		}
		if reason == "" {
			if _, err := waivedChecks(view.Checks, requiredChecks(q.c)); err != nil {
				reason = err.Error()
			}
		}
		if reason == "" {
			problems, err := q.ops.violations(m)
			if err != nil {
				return nil, moved, err
			}
			if len(problems) > 0 {
				reason = "the PR breaks the policy rules: " + strings.Join(problems, "; ")
			}
		}
		if reason != "" {
			moved = true
			if err := q.ops.giveBack(m, reason); err != nil {
				return nil, moved, err
			}
			continue
		}
		open = append(open, m)
	}
	return open, moved, nil
}

// evidenceFor states what actually ran for a merged PR.
func (q *queueRun) evidenceFor(m queueMember, view mergeView) writer.Evidence {
	e := m.Evidence
	if view.MergeCommit != nil {
		e.FinalSHA = view.MergeCommit.OID
	}
	e.CIURL = queueEvidenceUnknown(m.Head)
	result, known := q.current.Landed[m.Issue]
	if !known && q.current.Batch != nil {
		for _, step := range q.current.Batch.Steps {
			if step.Issue == m.Issue {
				result, known = queueResult{*q.current.Batch, step}, true
			}
		}
	}
	if known {
		e.CIURL = queueEvidence(m.Head, result.Batch, result.Step.Tree)
		if result.Batch.Flakes > 0 {
			addRisk(&e, fmt.Sprintf("the check failed and then passed %d time(s) in this batch: a flaky run, which landed", result.Batch.Flakes))
		}
		addRisk(&e, result.Step.Note)
	}
	e.Boundary = "local"
	if e.Environments == defaultEnvironments {
		e.Environments = queueEnvironment
	}
	if e.Artifacts == defaultArtifacts {
		e.Artifacts = queueArtifacts
	}
	return e
}

func (q *queueRun) pruneResults(members []queueMember) {
	for issue := range q.current.Landed {
		if !slicesContainsIssue(members, issue) {
			delete(q.current.Landed, issue)
		}
	}
}

func slicesContainsIssue(members []queueMember, issue int) bool {
	for _, m := range members {
		if m.Issue == issue {
			return true
		}
	}
	return false
}

// git runs git in the queue's worktree. Replay commits never leave this machine, so
// the repository's hooks and identity have no say in them.
func (q *queueRun) git(args ...string) (string, error) {
	return q.tree.git(append([]string{"-c", "core.hooksPath=/dev/null", "-c", "user.name=quipu queue", "-c", "user.email=queue@quipu.invalid"}, args...)...)
}

// remoteTip is the default branch's tip, freshly fetched.
func (q *queueRun) remoteTip() (string, error) {
	if _, err := q.git("fetch", "-q", "origin", q.c.DefaultBranch); err != nil {
		return "", err
	}
	return q.git("rev-parse", "origin/"+q.c.DefaultBranch)
}

// replay puts members on base, in order, and reports who conflicted or moved. The
// worktree keeps ignored files, so a reused build stays warm.
func (q *queueRun) replay(base string, members []queueMember) (steps []batchStep, left map[int]string, err error) {
	if _, err := q.git("checkout", "-q", "--force", "--detach", base); err != nil {
		return nil, nil, err
	}
	if _, err := q.git("clean", "-fdq"); err != nil {
		return nil, nil, err
	}
	left = map[int]string{}
	for _, m := range members {
		if _, err := q.git("fetch", "-q", "origin", m.Branch); err != nil {
			left[m.Issue] = fmt.Sprintf("could not fetch %s: %v", m.Branch, err)
			continue
		}
		fetched, err := q.git("rev-parse", "FETCH_HEAD")
		if err != nil {
			return nil, nil, err
		}
		if fetched != m.Head {
			left[m.Issue] = fmt.Sprintf("the branch moved from %s to %s after it was queued; rerun quipu done to queue it again", m.Head, fetched)
			continue
		}
		if _, err := q.git("merge", "-q", "--no-ff", "--no-edit", m.Head); err != nil {
			if _, inProgress := q.git("rev-parse", "-q", "--verify", "MERGE_HEAD"); inProgress != nil {
				return nil, nil, err
			}
			if _, err := q.git("merge", "--abort"); err != nil {
				return nil, nil, err
			}
			left[m.Issue] = fmt.Sprintf("it conflicts with %s and the PRs ahead of it in this batch; merge the default branch in, push, and rerun quipu done", q.c.DefaultBranch)
			continue
		}
		tree, err := q.git("rev-parse", "HEAD^{tree}")
		if err != nil {
			return nil, nil, err
		}
		steps = append(steps, batchStep{Issue: m.Issue, PR: m.PR, Head: m.Head, Tree: tree})
	}
	return steps, left, nil
}

type queueVerdict int

const (
	verdictPass queueVerdict = iota
	verdictFail
	verdictNone // a timeout or a lost environment says nothing about the code
)

// runOnce runs the check a single time on what's checked out.
func (q *queueRun) runOnce(batch *queueBatch, log string) (queueVerdict, error) {
	release, err := q.ops.hold()
	if err != nil {
		return verdictNone, err
	}
	defer release()
	batch.Runs++
	exit, err := runQueueCheck(q.tree.Top, batch.Command, log, q.timeout)
	switch {
	case err != nil:
		return verdictNone, nil
	case exit == 0:
		return verdictPass, nil
	case exit < 0:
		return verdictNone, nil
	}
	return verdictFail, nil
}

// judge runs the check until it has a verdict. A failure is a non-zero exit that
// survives its retries. A pass after a failure is a flake: it counts as a pass, and
// the evidence and status both say so, so one can't go unnoticed.
func (q *queueRun) judge(batch *queueBatch, log string) (queueVerdict, error) {
	failed := false
	for range 1 + q.c.QueueFlakyRetries() {
		verdict, err := q.runOnce(batch, log)
		if err != nil || verdict == verdictNone {
			return verdictNone, err
		}
		if verdict == verdictPass {
			if failed {
				batch.Flakes++
				q.current.Flaked = fmt.Sprintf("the check failed and then passed on base %s, in a batch with %s", batch.Base, describeSteps(batch.Steps))
			}
			return verdictPass, nil
		}
		failed = true
	}
	return verdictFail, nil
}

func describeSteps(steps []batchStep) string {
	var names []string
	for _, step := range steps {
		names = append(names, fmt.Sprintf("#%d", step.Issue))
	}
	return strings.Join(names, " ")
}

// holdQueue stops the queue for the owner.
func (q *queueRun) holdQueue(reason string) (queueOutcome, error) {
	q.current.Hold = reason
	return queueHeld, q.saveJournal()
}

// noVerdict counts a run that said nothing, or a machine with no slot free.
func (q *queueRun) noVerdict(err error) (queueOutcome, error) {
	var coded *flowError
	if errors.As(err, &coded) && coded.Code == "machine-busy" {
		return queueWaiting, q.saveJournal()
	}
	q.current.NoVerdicts++
	if q.current.NoVerdicts >= queueNoVerdicts {
		return q.holdQueue(fmt.Sprintf("the check ended with no verdict %d times in a row, such as a timeout or a lost environment", q.current.NoVerdicts))
	}
	return queueWaiting, q.saveJournal()
}

// runBatch tests a batch, and lands whatever it can prove.
func (q *queueRun) runBatch(members []queueMember) (queueOutcome, error) {
	base, err := q.remoteTip()
	if err != nil {
		return queueWaiting, err
	}
	steps, left, err := q.replay(base, members)
	if err != nil {
		return queueWaiting, err
	}
	// Conflicts go back, and the rest carry on without them.
	var kept []queueMember
	for _, m := range members {
		if reason, gone := left[m.Issue]; gone {
			if err := q.ops.giveBack(m, reason); err != nil {
				return queueWaiting, err
			}
			continue
		}
		kept = append(kept, m)
	}
	if len(steps) == 0 {
		return queueMoved, q.saveJournal()
	}
	batch := queueBatch{Base: base, Command: q.c.MergeQueue.Check, Steps: steps}
	log := q.logFile(steps[len(steps)-1].Tree)
	verdict, err := q.judge(&batch, log)
	if err != nil || verdict == verdictNone {
		return q.noVerdict(err)
	}
	q.current.NoVerdicts = 0
	if verdict == verdictPass {
		return q.startLanding(batch)
	}
	return q.bisect(batch, kept, log)
}

// testPrefix replays the first n PRs of a batch and judges them.
func (q *queueRun) testPrefix(batch *queueBatch, members []queueMember, n int, log string) (queueVerdict, error) {
	if _, _, err := q.replay(batch.Base, members[:n]); err != nil {
		return verdictNone, err
	}
	return q.judge(batch, log)
}

// bisect finds the first prefix that fails. Every prefix but the last was tested
// here, so the search can't blame a PR for what came after it. Before anyone goes
// back, the failure must happen once more: if it won't, nothing is ejected.
func (q *queueRun) bisect(batch queueBatch, members []queueMember, log string) (queueOutcome, error) {
	good, bad := 0, len(members)
	for bad-good > 1 {
		mid := (good + bad) / 2
		verdict, err := q.testPrefix(&batch, members, mid, log)
		if err != nil || verdict == verdictNone {
			return q.noVerdict(err)
		}
		if verdict == verdictPass {
			good = mid
		} else {
			bad = mid
		}
	}
	if good == 0 {
		// The first PR looks guilty, so make sure it isn't the default branch.
		verdict, err := q.testPrefix(&batch, members, 0, log)
		if err != nil || verdict == verdictNone {
			return q.noVerdict(err)
		}
		if verdict != verdictPass {
			return q.holdQueue(fmt.Sprintf("the check fails on %s itself, at %s, with no PR on it, so no PR is at fault. Fix the default branch, then run quipu queue resume.\n%s", q.c.DefaultBranch, batch.Base, tailOf(log)))
		}
	}
	if _, _, err := q.replay(batch.Base, members[:bad]); err != nil {
		return queueWaiting, err
	}
	again, err := q.runOnce(&batch, log)
	if err != nil || again == verdictNone {
		return q.noVerdict(err)
	}
	if again == verdictPass {
		return q.holdQueue(fmt.Sprintf("the check failed on %s and then passed, so no PR reproduces the failure and nothing was ejected. Look at the check, then run quipu queue resume.\n%s", describeSteps(batch.Steps), tailOf(log)))
	}
	culprit := members[bad-1]
	reason := fmt.Sprintf("the check %q failed with this PR on top of %s and the PRs ahead of it. Fix it, push, and rerun quipu done.\n%s", batch.Command, batch.Base, tailOf(log))
	if err := q.ops.giveBack(culprit, reason); err != nil {
		return queueWaiting, err
	}
	q.current.Rush = true
	if good == 0 {
		return queueMoved, q.saveJournal()
	}
	batch.Steps = batch.Steps[:good]
	return q.startLanding(batch)
}

// startLanding journals the tested result before anything merges.
func (q *queueRun) startLanding(batch queueBatch) (queueOutcome, error) {
	q.current.Batch = &batch
	q.current.Refusals = 0
	if err := q.saveJournal(); err != nil {
		return queueWaiting, err
	}
	return q.land(batch)
}

// resumeLanding carries on after a restart, while what was tested still applies.
func (q *queueRun) resumeLanding(open []queueMember) (queueOutcome, error) {
	batch := *q.current.Batch
	for _, step := range batch.Steps {
		view, err := q.ops.view(step.PR)
		if err != nil {
			return queueWaiting, err
		}
		if view.State != "MERGED" && !slicesContainsIssue(open, step.Issue) {
			return q.abandonLanding(batch)
		}
	}
	return q.land(batch)
}

// abandonLanding gives up on what hasn't landed. Those PRs stay queued, and the
// next batch tests them on the new tip.
func (q *queueRun) abandonLanding(batch queueBatch) (queueOutcome, error) {
	q.current.Batch = nil
	q.current.Rush = true
	return queueMoved, q.saveJournal()
}

// land merges a tested batch in order, each onto the one before it. A PR already
// merged was this worker before a restart, so it never merges twice. Trees prove
// each landing is exactly what was tested, since GitHub's merge commit has its own
// SHA. Each merge is recorded on the next pass, from what GitHub says landed.
func (q *queueRun) land(batch queueBatch) (queueOutcome, error) {
	tip := batch.Base
	for i := range batch.Steps {
		step := batch.Steps[i]
		view, err := q.ops.view(step.PR)
		if err != nil {
			return queueWaiting, err
		}
		if view.State != "MERGED" {
			if view.State != "OPEN" || view.HeadRefOid != step.Head {
				return q.abandonLanding(batch)
			}
			current, err := q.remoteTip()
			if err != nil {
				return queueWaiting, err
			}
			if current != tip {
				// Someone else's commit got in first, so the rest are untested.
				return q.abandonLanding(batch)
			}
			if err := q.ops.land(step, view, i == len(batch.Steps)-1); err != nil {
				q.current.Refusals++
				if q.current.Refusals >= queueRefusals {
					return q.holdQueue(fmt.Sprintf("GitHub refused to land %s %d times: %v", step.PR, q.current.Refusals, err))
				}
				return queueWaiting, q.saveJournal()
			}
			if view, err = q.ops.view(step.PR); err != nil {
				return queueWaiting, err
			}
			if view.State != "MERGED" || view.MergeCommit == nil {
				return queueWaiting, q.saveJournal()
			}
		}
		if view.MergeCommit == nil {
			return queueWaiting, fmt.Errorf("%s is merged and GitHub names no merge commit", step.PR)
		}
		note, err := q.proveLanding(view.MergeCommit.OID, tip, step.Tree)
		if err != nil {
			return queueWaiting, err
		}
		batch.Steps[i].Note = note
		if q.current.Landed == nil {
			q.current.Landed = map[int]queueResult{}
		}
		q.current.Landed[step.Issue] = queueResult{batch, batch.Steps[i]}
		q.current.Refusals = 0
		tip = view.MergeCommit.OID
		if note != "" {
			q.current.Batch = nil
			return q.holdQueue(note + ". " + q.testTip())
		}
		if err := q.saveJournal(); err != nil {
			return queueWaiting, err
		}
	}
	q.current.Batch = nil
	return queueMoved, q.saveJournal()
}

// proveLanding compares a merge commit with what was tested. Any difference comes
// back as a note for the evidence.
func (q *queueRun) proveLanding(sha, tip, tree string) (string, error) {
	if _, err := q.remoteTip(); err != nil {
		return "", err
	}
	parent, err := q.git("rev-parse", sha+"^1")
	if err != nil {
		return "", err
	}
	landed, err := q.git("rev-parse", sha+"^{tree}")
	if err != nil {
		return "", err
	}
	switch {
	case parent != tip:
		return fmt.Sprintf("merge commit %s landed on %s, not on the tested %s, so this exact result was not tested", sha, parent, tip), nil
	case landed != tree:
		return fmt.Sprintf("merge commit %s has tree %s, not the tested %s, so this exact result was not tested", sha, landed, tree), nil
	}
	return "", nil
}

// testTip says whether the default branch is good after a landing went wrong.
func (q *queueRun) testTip() string {
	tip, err := q.remoteTip()
	if err != nil {
		return "Could not read the default branch to test it: " + err.Error()
	}
	if _, _, err := q.replay(tip, nil); err != nil {
		return "Could not check out the default branch to test it: " + err.Error()
	}
	batch := queueBatch{Base: tip, Command: q.c.MergeQueue.Check}
	log := q.logFile(tip)
	switch verdict, err := q.runOnce(&batch, log); {
	case err != nil || verdict == verdictNone:
		return "Could not test the default branch."
	case verdict == verdictPass:
		return fmt.Sprintf("The check passes on %s at %s. Look, then run quipu queue resume.", q.c.DefaultBranch, tip)
	}
	return fmt.Sprintf("The check FAILS on %s at %s. Fix it, then run quipu queue resume.\n%s", q.c.DefaultBranch, tip, tailOf(log))
}

func (q *queueRun) logFile(name string) string {
	_ = os.MkdirAll(q.logs, 0o755)
	return filepath.Join(q.logs, name+".log")
}

// tailOf is the end of a log, for a person reading the Issue.
func tailOf(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(raw) > queueLogTail {
		raw = raw[len(raw)-queueLogTail:]
	}
	return "Output of the last run:\n" + strings.TrimSpace(string(raw))
}

// runQueueCheck runs the check. A timeout returns exit -1, which says nothing
// about the code, and it takes the check's children with it.
func runQueueCheck(top, command, log string, timeout time.Duration) (int, error) {
	output, err := os.Create(log)
	if err != nil {
		return 0, err
	}
	defer output.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir, cmd.Stdout, cmd.Stderr = top, output, output
	detach(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	if ctx.Err() != nil {
		return -1, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return 0, err
}

// queuedMembers reads who is queued from the Writer's trusted receipts.
func queuedMembers(c model.Config) ([]queueMember, []queueReturned, error) {
	items, err := loadStatusItems(c)
	if err != nil {
		return nil, nil, err
	}
	return queuedAmong(c, items)
}

// queuedAmong is queuedMembers on a board already read.
func queuedAmong(c model.Config, items []statusItem) ([]queueMember, []queueReturned, error) {
	client := github.NewClient()
	var members []queueMember
	var returned []queueReturned
	for _, item := range items {
		if item.Status != "In review" && item.Status != "Evidence pending" {
			continue
		}
		comments, err := client.ListComments(context.Background(), c.Owner, c.Repository, item.Number)
		if err != nil {
			return nil, nil, err
		}
		receipts := receiptTrust(c).Receipts(comments)
		if queued, ok := writer.LatestQueuedMerge(receipts); ok && queued.Merge.Batch {
			members = append(members, queueMember{Issue: item.Number, Holder: item.LeaseHolder, Branch: item.Branch, PR: queued.Merge.PR, Head: queued.Merge.Head, QueuedAt: queued.At, Evidence: queued.Merge.Evidence})
			continue
		}
		if back, ok := writer.ReturnedFromQueue(receipts); ok {
			returned = append(returned, queueReturned{Issue: item.Number, Reason: back.Detail})
		}
	}
	sort.SliceStable(members, func(i, j int) bool { return members[i].QueuedAt.Before(members[j].QueuedAt) })
	return members, returned, nil
}

// landQueued merges one PR with a merge commit, pinned to its tested head. Each
// merge but the last skips the CI GitHub would run on push: the batch's check has
// already covered them, and the last merge's run covers the same code.
func landQueued(c model.Config, step batchStep, view mergeView, last bool) error {
	marker := ""
	if !last {
		marker = "[skip ci]"
	}
	err := mergePullMarked(c, step.Issue, view, marker)
	if err == nil || !c.BypassesRulesets() {
		return err
	}
	blockers, blockerErr := loadAdminBlockers(c, view)
	if blockerErr != nil {
		return fmt.Errorf("%v; and could not tell whether the admin bypass is safe: %w", err, blockerErr)
	}
	if len(blockers) > 0 {
		return fmt.Errorf("%v; and the admin bypass only waives checks that never ran, not: %s", err, strings.Join(blockers, ", "))
	}
	return mergePullMarked(c, step.Issue, view, marker, "--admin")
}

// liveQueueOps is queueOps against GitHub.
func liveQueueOps(c model.Config, tree worktree) queueOps {
	return queueOps{
		members: func() ([]queueMember, error) {
			members, _, err := queuedMembers(c)
			return members, err
		},
		view: func(url string) (mergeView, error) { return loadMergeView(c, url) },
		violations: func(m queueMember) ([]string, error) {
			pull, err := loadPullFacts(c, m.PR)
			if err != nil {
				return nil, err
			}
			return prViolations(c, pull, func(n int) (liveWork, error) { return loadLiveWork(c, n) }, time.Now()), nil
		},
		land: func(step batchStep, view mergeView, last bool) error { return landQueued(c, step, view, last) },
		record: func(m queueMember, e writer.Evidence) error {
			state, err := loadLiveWork(c, m.Issue)
			if err != nil {
				return err
			}
			if state.Status == "In review" {
				if err := sendAndWait(c, m.Issue, m.Holder, writer.Request{Action: "evidence.submit", PR: m.PR, Evidence: &e}, 3*time.Minute); err != nil {
					return err
				}
			}
			return sendAndWait(c, m.Issue, m.Holder, writer.Request{Action: "status", Status: "Done"}, 3*time.Minute)
		},
		giveBack: func(m queueMember, reason string) error {
			return sendAndWait(c, m.Issue, m.Holder, writer.Request{Action: "queue.return", PR: m.PR, Reason: reason}, 3*time.Minute)
		},
		hold: func() (func(), error) {
			m, err := openMachine()
			if err != nil {
				if c.LimitsBuilds() {
					return nil, flowErr("machine-busy", "the queue's check did not run: could not read this machine's slots: %v", err)
				}
				return func() {}, nil
			}
			hold, err := m.holdCheck(c, tree, 0, time.Now())
			if err != nil {
				return nil, err
			}
			return hold.drop, nil
		},
		now: time.Now,
	}
}

// queueWorktree makes or reuses the worker's worktree beside the main checkout.
func queueWorktree(root worktree, c model.Config) (worktree, error) {
	path := filepath.Join(filepath.Dir(root.Top), filepath.Base(root.Top)+"-worktrees", c.Repository+"-queue")
	if existing, err := openWorktree(path); err == nil && existing.Top == realPath(path) {
		return existing, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return worktree{}, err
	}
	if _, err := root.git("fetch", "-q", "origin", c.DefaultBranch); err != nil {
		return worktree{}, err
	}
	if _, err := root.git("worktree", "add", "-q", "--detach", path, "origin/"+c.DefaultBranch); err != nil {
		return worktree{}, err
	}
	return openWorktree(path)
}

// mainCheckout is this clone's first worktree.
func mainCheckout(here worktree) (worktree, error) {
	porcelain, err := here.git("worktree", "list", "--porcelain")
	if err != nil {
		return worktree{}, err
	}
	entries := parseWorktreeList(porcelain)
	if len(entries) == 0 {
		return worktree{}, errors.New("git lists no worktree")
	}
	return openWorktree(entries[0].Path)
}

// queueRunCommand is the worker: one per clone, until nobody has been queued for a
// while. A second one finds the lock taken and leaves.
func queueRunCommand(here worktree) error {
	lockPath, err := here.commonPath("quipu-queue.lock")
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	free, err := tryLockFile(lock)
	if err != nil {
		return err
	}
	if !free {
		fmt.Println("another worker is running on this clone")
		return nil
	}
	root, err := mainCheckout(here)
	if err != nil {
		return err
	}
	journal, err := here.commonPath("quipu-queue.json")
	if err != nil {
		return err
	}
	logs, err := here.commonPath("quipu-queue-logs")
	if err != nil {
		return err
	}
	idleSince := time.Now()
	var gate *queueGate
	for {
		if gate != nil && gate.quiet(time.Now()) {
			time.Sleep(queuePoll)
			continue
		}
		// The default branch decides every pass, so turning the queue off stops it.
		c, source, err := loadEnforcedConfig(root, true)
		if err != nil {
			return err
		}
		if !c.MergesInQueue() || source == configSourceWorktree {
			fmt.Println("merge: queue is off on the default branch")
			return nil
		}
		tree, err := queueWorktree(root, c)
		if err != nil {
			return err
		}
		if gate == nil {
			gate = newQueueGate(func() (int64, error) { return latestCommentID(c) }, func() string { return fileStamp(journal) })
		}
		run := &queueRun{c: c, tree: tree, journal: journal, logs: logs, ops: liveQueueOps(c, tree), timeout: c.QueueCheckTimeout()}
		outcome, err := run.pass()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s pass: %v\n", time.Now().Format(time.RFC3339), err)
		}
		gate.passed(time.Now(), outcome, err, run.wake)
		switch {
		case outcome == queueMoved && err == nil:
			idleSince = time.Now()
			continue
		case outcome == queueWaiting:
			idleSince = time.Now()
		case time.Since(idleSince) > queueIdleExit:
			// Held work still gets its evidence on the way, and then there's
			// nothing to do until the owner resumes.
			return nil
		}
		time.Sleep(queuePoll)
	}
}

// queueGate spares the board between passes. A pass reads the whole board and every
// queued PR, about 23 GraphQL points a page before the PRs, and the 5,000 an hour
// are shared with every agent on the login. A batch waiting 30 minutes for its timer
// would spend half of them finding nothing. Only a receipt changes who is queued, and
// a receipt is a new comment, so the newest comment's ID, a REST call, says whether a
// pass could find anything.
type queueGate struct {
	latest  func() (int64, error)
	journal func() string
	seen    int64
	stamp   string
	changed time.Time
	due     time.Time
}

func newQueueGate(latest func() (int64, error), journal func() string) *queueGate {
	g := &queueGate{latest: latest, journal: journal}
	// Read before the first pass, so a comment during it is still news afterwards.
	if id, err := latest(); err == nil {
		g.seen = id
	}
	return g
}

// quiet reports whether no pass is needed yet. Not being able to tell means a pass.
func (g *queueGate) quiet(now time.Time) bool {
	id, err := g.latest()
	if err != nil {
		return false
	}
	if id != g.seen {
		g.seen, g.changed = id, now
	}
	return now.Before(g.due) && now.Sub(g.changed) >= queueSettle && g.journal() == g.stamp
}

// passed notes a finished pass. Work that moved or failed gets another at once;
// otherwise the next is due at the batch timer or the quiet limit, whichever is first.
func (g *queueGate) passed(now time.Time, outcome queueOutcome, err error, wake time.Time) {
	g.stamp = g.journal()
	g.due = now.Add(queueQuietPass)
	if outcome == queueMoved || err != nil {
		g.due = now
	}
	if !wake.IsZero() && wake.Before(g.due) {
		g.due = wake
	}
}

// latestCommentID is the newest comment anywhere in the repository.
func latestCommentID(c model.Config) (int64, error) {
	var comments []github.Comment
	path := fmt.Sprintf("/repos/%s/%s/issues/comments?sort=created&direction=desc&per_page=1", c.Owner, c.Repository)
	if err := github.NewClient().Do(context.Background(), http.MethodGet, path, nil, &comments); err != nil {
		return 0, err
	}
	if len(comments) == 0 {
		return 0, nil
	}
	return comments[0].ID, nil
}

// fileStamp changes when a file does; quipu queue resume writes the journal.
func fileStamp(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return info.ModTime().String()
}

// queueWorkerRunning asks the kernel whether a worker holds this clone's lock.
func queueWorkerRunning(w worktree) bool {
	path, err := w.commonPath("quipu-queue.lock")
	if err != nil {
		return false
	}
	return slotProcessAlive(path)
}

// ensureQueueWorker starts the worker if none is running. The command, not the
// agent, starts it, so nothing here needs an agent to stay around.
func ensureQueueWorker(c model.Config, w worktree) {
	if !c.MergesInQueue() || queueWorkerRunning(w) {
		return
	}
	root, err := mainCheckout(w)
	if err != nil {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	logPath, err := w.commonPath("quipu-queue.log")
	if err != nil {
		return
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer log.Close()
	cmd := exec.Command(self, "queue", "run")
	cmd.Dir, cmd.Stdout, cmd.Stderr = root.Top, log, log
	detach(cmd)
	if err := cmd.Start(); err == nil {
		_ = cmd.Process.Release()
	}
}

// queueReturned is a PR the queue gave back.
type queueReturned struct {
	Issue  int    `json:"issue"`
	Reason string `json:"reason"`
}

// queueEntry is one PR waiting in status.
type queueEntry struct {
	Issue    int       `json:"issue"`
	PR       string    `json:"pr"`
	Head     string    `json:"head"`
	QueuedAt time.Time `json:"queued_at"`
}

// queueReport is the queue in `quipu status`.
type queueReport struct {
	Worker    string          `json:"worker"`
	BatchSize int             `json:"batch_size"`
	Entries   []queueEntry    `json:"entries"`
	Landing   []batchStep     `json:"landing,omitempty"`
	Returned  []queueReturned `json:"returned"`
	Held      string          `json:"held,omitempty"`
	Flaked    string          `json:"flaked,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// loadQueueReport reads the queue, in queue mode only: it costs a call per PR.
func loadQueueReport(c model.Config, w worktree, list func() ([]queueMember, []queueReturned, error)) *queueReport {
	if !c.MergesInQueue() {
		return nil
	}
	report := &queueReport{Worker: "stopped", BatchSize: c.QueueBatchSize(), Entries: []queueEntry{}, Returned: []queueReturned{}}
	if queueWorkerRunning(w) {
		report.Worker = "running"
	}
	members, returned, err := list()
	if err != nil {
		report.Error = err.Error()
	}
	for _, m := range members {
		report.Entries = append(report.Entries, queueEntry{m.Issue, m.PR, m.Head, m.QueuedAt})
	}
	if returned != nil {
		report.Returned = returned
	}
	if path, err := w.commonPath("quipu-queue.json"); err == nil {
		var state queueJournal
		if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, &state) == nil {
			report.Held, report.Flaked = state.Hold, state.Flaked
			if state.Batch != nil {
				report.Landing = state.Batch.Steps
			}
		}
	}
	return report
}

// addQueueToReport folds the queue into the board's attention and owner lists.
func addQueueToReport(report *statusReport, queue *queueReport) {
	if queue == nil {
		return
	}
	report.Queue = queue
	for _, back := range queue.Returned {
		report.Attention = append(report.Attention, fmt.Sprintf("#%d returned by the merge queue: fix it and rerun quipu done, after quipu start %d. %s", back.Issue, back.Issue, firstLine(back.Reason)))
	}
	if queue.Flaked != "" {
		report.Attention = append(report.Attention, "the merge queue saw a flaky run: "+queue.Flaked)
	}
	if queue.Held != "" {
		report.OwnerQueue = append(report.OwnerQueue, ownerItem{Reason: "the merge queue is held: " + firstLine(queue.Held) + " Run quipu queue resume once it is safe"})
	}
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// renderQueue is the same report for a person.
func renderQueue(report *queueReport) string {
	if report == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nMerge queue (%d waiting, worker %s)\n", len(report.Entries), report.Worker)
	for _, entry := range report.Entries {
		fmt.Fprintf(&b, "  #%d %s\n", entry.Issue, entry.PR)
	}
	if report.Held != "" {
		fmt.Fprintf(&b, "  ! held: %s\n", firstLine(report.Held))
	}
	return b.String()
}

// queueForBatch is `quipu done` in queue mode: check what can be checked now, queue
// the PR through the Writer, and free this worktree at once.
func queueForBatch(c model.Config, w worktree, number int, actor string, pull pullRequest, state liveWork, cleanup string, e writer.Evidence, wait time.Duration, respond func(flowResult) error) error {
	e.FinalSHA, e.CIURL = pendingMerge, pendingMerge
	if e.Environments == defaultEnvironments {
		e.Environments = queueEnvironment
	}
	if e.Artifacts == defaultArtifacts {
		e.Artifacts = queueArtifacts
	}
	if err := promptMissingEvidence(&e, os.Stdin, os.Stderr); err != nil {
		return err
	}
	if e.Boundary != "local" {
		return flowErr("evidence-missing", "the merge queue tests this on this machine, so --boundary must be local, not %q", e.Boundary)
	}
	for _, action := range queuePreparation(state, actor, time.Now()) {
		if err := sendAndWait(c, number, actor, writer.Request{Action: action}, wait); err != nil {
			return err
		}
	}
	view, err := loadMergeView(c, pull.URL)
	if err != nil {
		return err
	}
	if view.IsDraft {
		return flowErr("merge-blocked", "PR %s is a draft", pull.URL)
	}
	if _, err := waivedChecks(view.Checks, requiredChecks(c)); err != nil {
		return err
	}
	facts, err := loadPullFacts(c, pull.URL)
	if err != nil {
		return err
	}
	problems := prViolations(c, facts, func(n int) (liveWork, error) { return loadLiveWork(c, n) }, time.Now())
	if len(problems) > 0 {
		return flowErr("policy-rules", "PR %s breaks the policy rules, which the merge queue never waives: %s", pull.URL, strings.Join(problems, "; "))
	}
	if _, err := postRequest(c, number, actor, writer.Request{Action: "merge.batch", PR: pull.URL, Head: view.HeadRefOid, Evidence: &e}); err != nil {
		return err
	}
	warnings := w.release(number, cleanup)
	freeWorkSlot(c, w)
	ensureQueueWorker(c, w)
	return respond(flowResult{Issue: number, Status: "In review", PR: pull.URL, Warnings: warnings, Next: fmt.Sprintf("Queued. The merge queue tests %s in a batch, lands it, and the Writer closes #%d; or it returns #%d to In review with the failure attached. Do not wait: take other work with quipu start --next --json, and look again later with quipu status --json.", pull.URL, number, number)})
}

// queuePreparation is what work needs before it can be queued: a lease that lapsed
// while the Writer or CI was down, and the start of work only claimed, since Claimed
// cannot move straight to In review. Work In progress needs neither, so queueing it
// waits for nothing.
func queuePreparation(state liveWork, actor string, now time.Time) []string {
	var actions []string
	if state.Status == "In review" {
		return nil
	}
	if state.Holder == actor && !hasActiveLease(state.Holder, state.Expiry, now) {
		actions = append(actions, "renew")
	}
	if state.Status == "Claimed" {
		actions = append(actions, "start")
	}
	return actions
}

// takeOverReturned lets another worktree of the same login pick up work the queue
// returned, since the worktree that queued it may be gone.
func takeOverReturned(c model.Config, number int, state liveWork, actor string, wait time.Duration) (bool, error) {
	if !c.MergesInQueue() || state.Status != "In review" || state.Holder == actor || !writer.SameLogin(state.Holder, actor) {
		return false, nil
	}
	comments, err := github.NewClient().ListComments(context.Background(), c.Owner, c.Repository, number)
	if err != nil {
		return false, err
	}
	if _, returned := writer.ReturnedFromQueue(receiptTrust(c).Receipts(comments)); !returned {
		return false, nil
	}
	return true, sendAndWait(c, number, actor, writer.Request{Action: "takeover", Branch: state.Branch}, wait)
}

// queueCommand is `quipu queue`.
func queueCommand(args []string) error {
	usage := errors.New("usage: quipu queue (status [--json] | run | resume)")
	if len(args) == 0 {
		return usage
	}
	fs := flag.NewFlagSet("queue "+args[0], flag.ContinueOnError)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	switch args[0] {
	case "run":
		return queueRunCommand(here)
	case "resume":
		c, _, err := loadEnforcedConfig(here, true)
		if err != nil {
			return err
		}
		path, err := here.commonPath("quipu-queue.json")
		if err != nil {
			return err
		}
		q := &queueRun{journal: path}
		q.loadJournal()
		q.current.Hold, q.current.NoVerdicts, q.current.Refusals = "", 0, 0
		if err := q.saveJournal(); err != nil {
			return err
		}
		ensureQueueWorker(c, here)
		return printValue(map[string]any{"status": "Resumed"}, *asJSON)
	case "status":
		c, err := loadConfig()
		if err != nil {
			return err
		}
		enforced, _, err := loadEnforcedConfig(here, false)
		if err != nil {
			return err
		}
		c.Merge, c.MergeQueue = enforced.Merge, enforced.MergeQueue
		report := loadQueueReport(c, here, func() ([]queueMember, []queueReturned, error) { return queuedMembers(c) })
		if report == nil {
			return errors.New("merge: queue is off on the default branch")
		}
		if *asJSON {
			return printValue(report, true)
		}
		fmt.Print(renderQueue(report))
		return nil
	}
	return usage
}
