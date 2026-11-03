package main

// Merging without CI. With merge_without_ci on, `quipu done` may merge while
// required checks have not run, once local_check and the policy rules pass here.
// The flag, the command, and the ruleset bypass come only from the default
// branch. A check that ran and failed still blocks, and the evidence says plainly
// that CI was skipped and that the result is only what `quipu done` reported.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

const (
	noCIPrefix          = "None: merged without CI at "
	defaultEnvironments = "GitHub Actions CI"
	defaultArtifacts    = "None: CI run linked"
	// What the evidence says in place of those defaults, which name a CI run.
	noCIEnvironment = "This worktree only; no CI ran"
	noCIArtifacts   = "None: no CI ran"
)

// noCIAdminNote ends the CI field when the merge used the admin bypass.
const noCIAdminNote = "; merged with the admin bypass"

var noCIValue = regexp.MustCompile(`^None: merged without CI at ([0-9a-f]{40}); local_check (".*" exited 0|skipped, per scoped_checks|result not known to this rerun), as reported by quipu done(; merged with the admin bypass)?$`)

// noCIEvidence is the CI field of a merge without CI. The exit code is the
// client's report: the Writer cannot verify a run on another machine.
func noCIEvidence(head, command string, exit int) string {
	if command == "" {
		return noCIPrefix + head + "; local_check skipped, per scoped_checks, as reported by quipu done"
	}
	return fmt.Sprintf("%s%s; local_check %q exited %d, as reported by quipu done", noCIPrefix, head, command, exit)
}

// noCIEvidenceUnknown is the CI field when a rerun has no record of the check.
func noCIEvidenceUnknown(head string) string {
	return noCIPrefix + head + "; local_check result not known to this rerun, as reported by quipu done"
}

func isNoCIEvidence(ci string) bool { return strings.HasPrefix(ci, noCIPrefix) }

// checkNoCIEvidence is what the Writer asks of evidence that skipped CI. Its
// config comes from the default branch, so a PR cannot turn this on for itself.
// A check that had failed by the time of the merge means nobody skipped CI: it
// ran and said no. One that fails later is what skipping CI gives up.
func checkNoCIEvidence(c model.Config, e writer.Evidence, view prView) error {
	if strings.Contains(e.CIURL, "local_check skipped, per scoped_checks") && len(c.ScopedChecks) == 0 {
		return errors.New("evidence says scoped_checks skipped local_check, but the default branch has none")
	}
	if !c.MergesWithoutCI() {
		return errors.New("evidence says the merge skipped CI, but merge_without_ci is off on the default branch")
	}
	match := noCIValue.FindStringSubmatch(e.CIURL)
	if match == nil {
		return fmt.Errorf("CI value %q is not the form quipu done writes", e.CIURL)
	}
	if match[1] != view.HeadRefOid {
		return fmt.Errorf("evidence says the merge skipped CI at %s, but the PR's head is %s", match[1], view.HeadRefOid)
	}
	if e.Boundary != "local" {
		return fmt.Errorf("a merge without CI was verified only locally, so its boundary must be local, not %q", e.Boundary)
	}
	if _, err := waivedChecks(checksBefore(view.Checks, view.MergedAt), requiredChecks(c)); err != nil {
		return err
	}
	return nil
}

// checksBefore leaves out the checks that finished after a time.
func checksBefore(checks []prCheck, at string) []prCheck {
	limit, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return checks
	}
	var before []prCheck
	for _, check := range checks {
		completed, err := time.Parse(time.RFC3339, check.CompletedAt)
		if err != nil || !completed.After(limit) {
			before = append(before, check)
		}
	}
	return before
}

// noCISettings reads the flags that waive checks. A worktree's copy of the config
// never counts, including in a clone with no origin to read the default branch from.
func noCISettings(c model.Config, source string) (noCI, bypass bool) {
	if source == configSourceWorktree {
		return false, false
	}
	return c.MergesWithoutCI(), c.BypassesRulesets()
}

// didNotRun is a check that says nothing about the code: skipped, or GitHub
// ending a job nothing ever picked up, which its times show. Times it can't read
// count as a run, so an unknown shape blocks.
func didNotRun(check prCheck) bool {
	switch check.Conclusion {
	case "SKIPPED", "NEUTRAL":
		return true
	case "CANCELLED":
	default:
		return false
	}
	started, err := time.Parse(time.RFC3339, check.StartedAt)
	if err != nil {
		return false
	}
	if started.IsZero() {
		return true
	}
	completed, err := time.Parse(time.RFC3339, check.CompletedAt)
	return err == nil && !started.Before(completed)
}

// waivedChecks names the required checks a merge without CI goes ahead without:
// those pending, skipped, never run, or cancelled before starting. One that ran and failed
// is an error, whatever the flags.
func waivedChecks(checks []prCheck, required []string) ([]string, error) {
	var ran []prCheck
	for _, check := range checks {
		if !didNotRun(check) {
			ran = append(ran, check)
		}
	}
	verdicts := judgeChecks(ran, required)
	var waived, failed []string
	for _, name := range required {
		switch verdicts[name] {
		case checkFailing:
			failed = append(failed, name)
		case checkPending, checkNeverRan:
			waived = append(waived, name)
		}
	}
	if len(failed) > 0 {
		return nil, flowErr("checks-failing", "required checks failed: %s; merge_without_ci never waives a check that failed", strings.Join(failed, ", "))
	}
	return waived, nil
}

// noCIOps are the outside effects of a merge without CI.
type noCIOps struct {
	view       func() (mergeView, error)
	update     func() (string, error)
	pause      func()
	lock       func() (func(), error)
	hold       func() (func(), error)
	head       func() (string, error)
	dirty      func() (string, error)
	violations func() ([]string, error)
	check      func(command string) (int, error)
	// changed lists the paths this PR touches, for scoped_checks.
	changed       func() ([]string, error)
	record        func(head, value string)
	forget        func(head string)
	merge         func(view mergeView, admin bool) error
	adminBlockers func(view mergeView) ([]string, error)
}

// blockedState is a PR GitHub won't merge whatever the checks say.
func blockedState(view mergeView) error {
	if view.IsDraft || view.MergeStateStatus == "DIRTY" || view.MergeStateStatus == "BEHIND" {
		return flowErr("merge-blocked", "PR %s cannot merge yet (draft %v, state %s)", view.URL, view.IsDraft, view.MergeStateStatus)
	}
	return nil
}

// noCICommand is local_check, narrowed by scoped_checks where they apply. Not being
// able to tell what changed runs all of it.
func noCICommand(c model.Config, ops noCIOps) string {
	if len(c.ScopedChecks) == 0 || ops.changed == nil {
		return c.LocalCheck
	}
	paths, err := ops.changed()
	if err != nil {
		return c.LocalCheck
	}
	return c.ScopedCommand(paths, c.LocalCheck).Command
}

// noCIAttempts is how many times one `quipu done` tests against a moving base.
const noCIAttempts = 3

// errBaseMoved marks a check that passed against a default branch that has since
// moved, so what passed is no longer what would merge.
var errBaseMoved = errors.New("the default branch moved during local_check")

// mergeWithoutCI merges with required checks waived, having tested exactly what
// merges. It returns "" and no error when no check is waived, so the ordinary
// merge applies, and otherwise the CI field for the evidence.
func mergeWithoutCI(c model.Config, bypass bool, e writer.Evidence, ops noCIOps) (string, error) {
	for range noCIAttempts {
		value, err := mergeWithoutCIOnce(c, bypass, e, ops)
		if !errors.Is(err, errBaseMoved) {
			return value, err
		}
	}
	return "", flowErr("base-moved", "%s moved during each of %d runs of local_check, so nothing merged", c.DefaultBranch, noCIAttempts)
}

// noCIHeadPolls is how many times an update waits for GitHub to show its push.
const noCIHeadPolls = 10

// updateBranch brings a behind PR up to date and waits to see the new head, since
// the check must run on what GitHub would merge.
func updateBranch(c model.Config, view mergeView, ops noCIOps) (mergeView, error) {
	head, err := ops.update()
	if err != nil {
		return view, err
	}
	for range noCIHeadPolls {
		if view, err = ops.view(); err != nil {
			return view, err
		}
		if view.HeadRefOid == head {
			return view, nil
		}
		ops.pause()
	}
	return view, flowErr("checks-pending", "updated %s with %s, and GitHub does not show it yet; rerun quipu done to check and merge it", view.URL, c.DefaultBranch)
}

func mergeWithoutCIOnce(c model.Config, bypass bool, e writer.Evidence, ops noCIOps) (string, error) {
	view, err := ops.view()
	if err != nil {
		return "", err
	}
	required := requiredChecks(c)
	behind := view.MergeStateStatus == "BEHIND" && !view.IsDraft && c.UpdatesBehind()
	if !behind && mergeGate(view.Checks, required) == nil {
		return "", nil
	}
	if _, err := waivedChecks(view.Checks, required); err != nil {
		return "", err
	}
	if e.Boundary != "local" {
		return "", flowErr("evidence-missing", "this merge skips CI, so --boundary must be local, not %q", e.Boundary)
	}
	if err := checkCleanAt(ops, view.HeadRefOid); err != nil {
		return "", err
	}
	if behind {
		if view, err = updateBranch(c, view, ops); err != nil {
			return "", err
		}
		if view.MergeStateStatus == "BEHIND" {
			return "", errBaseMoved
		}
		if mergeGate(view.Checks, required) == nil {
			return "", nil
		}
		if _, err := waivedChecks(view.Checks, required); err != nil {
			return "", err
		}
	}
	if err := blockedState(view); err != nil {
		return "", err
	}
	violations, err := ops.violations()
	if err != nil {
		return "", err
	}
	if len(violations) > 0 {
		return "", flowErr("policy-rules", "PR %s breaks the policy rules, which merge_without_ci never waives: %s", view.URL, strings.Join(violations, "; "))
	}
	command := noCICommand(c, ops)
	exit := 0
	if command != "" {
		release, err := ops.hold()
		if err != nil {
			return "", err
		}
		exit, err = ops.check(command)
		release()
		if err != nil {
			return "", flowErr("local-check-failed", "local_check could not run: %v", err)
		}
		if exit != 0 {
			return "", flowErr("local-check-failed", "local_check %q exited %d, so nothing merged", command, exit)
		}
	}
	// It may have taken a while, and it ran this repository's code.
	if err := checkCleanAt(ops, view.HeadRefOid); err != nil {
		return "", err
	}
	head := view.HeadRefOid
	// From the last look to the merge, no other agent on this clone can merge.
	unlock, err := ops.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if view, err = ops.view(); err != nil {
		return "", err
	}
	if view.HeadRefOid != head {
		return "", flowErr("worktree-unclean", "PR %s moved to %s while local_check ran on %s; rerun quipu done", view.URL, view.HeadRefOid, head)
	}
	if view.MergeStateStatus == "BEHIND" && c.UpdatesBehind() {
		return "", errBaseMoved
	}
	if err := blockedState(view); err != nil {
		return "", err
	}
	waived, err := waivedChecks(view.Checks, required)
	if err != nil {
		return "", err
	}
	value := noCIEvidence(head, command, exit)
	ops.record(head, value)
	err = ops.merge(view, false)
	if err != nil && bypass && len(waived) > 0 {
		// --admin skips every rule, and GitHub's refusal doesn't say which one
		// stopped it. So it is used only when nothing else could have.
		blockers, blockerErr := ops.adminBlockers(view)
		switch {
		case blockerErr != nil:
			err = flowErr("merge-blocked", "%v; and could not tell whether the admin bypass is safe: %v", err, blockerErr)
		case len(blockers) > 0:
			err = flowErr("merge-blocked", "%v; and the admin bypass only waives checks that never ran, not: %s", err, strings.Join(blockers, ", "))
		default:
			value += noCIAdminNote
			ops.record(head, value)
			err = ops.merge(view, true)
		}
	}
	if err != nil {
		ops.forget(head)
		return "", err
	}
	return value, nil
}

// judgeAdminBlockers lists what the admin bypass would wrongly override.
func judgeAdminBlockers(checks []prCheck, reviewDecision string, unresolvedThreads int) []string {
	var blockers []string
	var names []string
	for _, check := range checks {
		name := check.Name
		if name == "" {
			name = check.Context
		}
		names = append(names, name)
	}
	if _, err := waivedChecks(checks, names); err != nil {
		blockers = append(blockers, err.Error())
	}
	if reviewDecision == "CHANGES_REQUESTED" {
		blockers = append(blockers, "a review requested changes")
	}
	if unresolvedThreads > 0 {
		blockers = append(blockers, fmt.Sprintf("%d unresolved review threads", unresolvedThreads))
	}
	return blockers
}

// loadAdminBlockers reads what judgeAdminBlockers needs from GitHub.
func loadAdminBlockers(c model.Config, view mergeView) ([]string, error) {
	output, err := ghOutput("pr", "view", view.URL, "--repo", c.Owner+"/"+c.Repository, "--json", "number,reviewDecision")
	if err != nil {
		return nil, err
	}
	var pull struct {
		Number         int    `json:"number"`
		ReviewDecision string `json:"reviewDecision"`
	}
	if err := json.Unmarshal(output, &pull); err != nil {
		return nil, err
	}
	output, err = ghOutput("api", "graphql", "-f", "query=query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100){pageInfo{hasNextPage} nodes{isResolved}}}}}",
		"-F", "owner="+c.Owner, "-F", "name="+c.Repository, "-F", fmt.Sprintf("number=%d", pull.Number))
	if err != nil {
		return nil, err
	}
	var threads struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool `json:"hasNextPage"`
						} `json:"pageInfo"`
						Nodes []struct {
							IsResolved bool `json:"isResolved"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &threads); err != nil {
		return nil, err
	}
	found := threads.Data.Repository.PullRequest.ReviewThreads
	if found.PageInfo.HasNextPage {
		return nil, errors.New("more than 100 review threads")
	}
	unresolved := 0
	for _, thread := range found.Nodes {
		if !thread.IsResolved {
			unresolved++
		}
	}
	return judgeAdminBlockers(view.Checks, pull.ReviewDecision, unresolved), nil
}

// checkCleanAt refuses a tree the check would not really be checking.
func checkCleanAt(ops noCIOps, head string) error {
	dirty, err := ops.dirty()
	if err != nil {
		return err
	}
	if dirty != "" {
		return flowErr("worktree-unclean", "merge_without_ci needs a clean worktree, and this one has changes:\n%s", dirty)
	}
	current, err := ops.head()
	if err != nil {
		return err
	}
	if current != head {
		return flowErr("worktree-unclean", "this worktree is at %s, but the PR's head is %s; the check must run on what merges", current, head)
	}
	return nil
}

// liveNoCIOps is noCIOps against this worktree and GitHub.
// tested receives the default branch's tip at the moment of the merge.
func liveNoCIOps(c model.Config, w worktree, number int, branch, url string, tested *string) noCIOps {
	return noCIOps{
		view:   func() (mergeView, error) { return loadMergeState(c, url) },
		update: func() (string, error) { return mergeDefaultIn(w, branch, c.DefaultBranch) },
		pause:  func() { time.Sleep(3 * time.Second) },
		lock:   w.mergeLock,
		hold: func() (func(), error) {
			m, err := openMachine()
			if err != nil {
				if c.LimitsBuilds() {
					return nil, flowErr("machine-busy", "local_check did not run: could not read this machine's slots: %v", err)
				}
				return func() {}, nil
			}
			hold, err := m.holdCheck(c, w, number, time.Now())
			if err != nil {
				return nil, err
			}
			return hold.drop, nil
		},
		head:  func() (string, error) { return w.git("rev-parse", "HEAD") },
		dirty: func() (string, error) { return w.git("status", "--porcelain") },
		violations: func() ([]string, error) {
			pull, err := loadPullFacts(c, url)
			if err != nil {
				return nil, err
			}
			return prViolations(c, pull, func(n int) (liveWork, error) { return loadLiveWork(c, n) }, time.Now()), nil
		},
		check: func(command string) (int, error) { return runLocalCheck(w.Top, command) },
		changed: func() ([]string, error) {
			output, err := w.git("diff", "--name-only", "origin/"+c.DefaultBranch+"...HEAD")
			return changedPaths(output), err
		},
		record:        func(head, value string) { recordLocalCheck(w, head, value) },
		forget:        func(head string) { _ = os.Remove(localCheckRecord(w, head)) },
		adminBlockers: func(view mergeView) ([]string, error) { return loadAdminBlockers(c, view) },
		merge: func(view mergeView, admin bool) error {
			*tested = view.BaseOID
			if admin {
				return mergePull(c, number, view, "--admin")
			}
			return mergePull(c, number, view)
		},
	}
}

// runLocalCheck runs the default branch's command on this worktree. Its output
// goes to stderr, because stdout is the command's JSON result.
func runLocalCheck(top, command string) (int, error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir, cmd.Stdout, cmd.Stderr = top, os.Stderr, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return 0, err
}

func localCheckRecord(w worktree, head string) string {
	return filepath.Join(w.GitDir, "quipu-local-check-"+head)
}

// recordLocalCheck keeps the result for a rerun, should the merge land and the
// evidence not reach the Writer.
func recordLocalCheck(w worktree, head, value string) {
	_ = os.WriteFile(localCheckRecord(w, head), []byte(value+"\n"), 0o644)
}

// noCIRerunEvidence is the CI field when `quipu done` runs after a merge and was
// given none: the record of the check, or else the passing run of a required
// check, or else a plain statement that neither is known.
func noCIRerunEvidence(w worktree, head, requiredRun string) string {
	if raw, err := os.ReadFile(localCheckRecord(w, head)); err == nil {
		if value := strings.TrimSpace(string(raw)); isNoCIEvidence(value) {
			return value
		}
	}
	if requiredRun != "" {
		return requiredRun
	}
	return noCIEvidenceUnknown(head)
}

// noCIAwaitingReceipt says what is safe when the merge landed and the Writer has
// not answered: its evidence is queued, and rerunning is how to finish.
func noCIAwaitingReceipt(url string, err error) error {
	var coded *flowError
	if errors.As(err, &coded) && coded.Code == "no-receipt" {
		return flowErr("no-receipt", "PR %s merged without CI, and its evidence is queued for the Writer. %s", url, coded.Message)
	}
	return err
}

// enforcedBypass reads merge_bypass_rulesets from the default branch, so a
// worktree's copy cannot open the ruleset.
func enforcedBypass(w worktree) bool {
	c, source, err := loadEnforcedConfig(w, true)
	if err != nil {
		return false
	}
	_, bypass := noCISettings(c, source)
	return bypass
}

// noCIDefaults swaps the evidence defaults that name a CI run.
func noCIDefaults(e *writer.Evidence) {
	if e.Environments == defaultEnvironments {
		e.Environments = noCIEnvironment
	}
	if e.Artifacts == defaultArtifacts {
		e.Artifacts = noCIArtifacts
	}
}

// ruleset is the part of a repository ruleset that decides whether GitHub refuses
// a merge without CI.
type ruleset struct {
	Name        string `json:"name"`
	Enforcement string `json:"enforcement"`
	Target      string `json:"target"`
	Conditions  struct {
		RefName struct {
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
		} `json:"ref_name"`
	} `json:"conditions"`
	// CurrentUserCanBypass is what GitHub says when this token can't list the actors.
	CurrentUserCanBypass string `json:"current_user_can_bypass"`
	BypassActors         []struct {
		ActorID   int    `json:"actor_id"`
		ActorType string `json:"actor_type"`
		Mode      string `json:"bypass_mode"`
	} `json:"bypass_actors"`
	Rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			Strict bool `json:"strict_required_status_checks_policy"`
			Checks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	} `json:"rules"`
}

func (r ruleset) adminsBypass() bool {
	if r.CurrentUserCanBypass == "always" || r.CurrentUserCanBypass == "pull_requests_only" {
		return true
	}
	for _, actor := range r.BypassActors {
		if actor.ActorType == "RepositoryRole" && actor.ActorID == adminRoleID && (actor.Mode == "always" || actor.Mode == "pull_request") {
			return true
		}
	}
	return false
}

// appliesTo reports whether this ruleset governs a branch.
func (r ruleset) appliesTo(branch string) bool {
	matches := func(patterns []string) bool {
		for _, pattern := range patterns {
			if pattern == "~ALL" || pattern == "~DEFAULT_BRANCH" {
				return true
			}
			pattern = strings.TrimPrefix(pattern, "refs/heads/")
			if matched, err := path.Match(strings.ReplaceAll(pattern, "**", "*"), branch); err == nil && matched {
				return true
			}
		}
		return false
	}
	if len(r.Conditions.RefName.Include) == 0 && len(r.Conditions.RefName.Exclude) == 0 {
		return true
	}
	return matches(r.Conditions.RefName.Include) && !matches(r.Conditions.RefName.Exclude)
}

// requiresUpToDate reports whether this ruleset wants branches up to date.
func (r ruleset) requiresUpToDate() bool {
	for _, rule := range r.Rules {
		if rule.Type == "required_status_checks" && rule.Parameters.Strict {
			return true
		}
	}
	return false
}

// requires names the checks in this ruleset that merge_without_ci may waive.
func (r ruleset) requires(required []string) []string {
	var found []string
	for _, rule := range r.Rules {
		if rule.Type != "required_status_checks" {
			continue
		}
		for _, check := range rule.Parameters.Checks {
			if contains(required, check.Context) {
				found = append(found, check.Context)
			}
		}
	}
	return found
}

// judgeRulesets fails when GitHub would refuse the merges merge_without_ci is for.
func judgeRulesets(c model.Config, rulesets []ruleset) []doctorCheck {
	var checks []doctorCheck
	for _, r := range rulesets {
		if r.Enforcement != "active" || r.Target != "branch" || !r.appliesTo(c.DefaultBranch) {
			continue
		}
		// The queue lands one PR on top of another, so each is behind for a moment.
		if c.MergesInQueue() && r.requiresUpToDate() {
			checks = append(checks, doctorCheck{"merge: queue", "fail",
				fmt.Sprintf("ruleset %q requires branches to be up to date, so GitHub refuses every landing after a batch's first", r.Name),
				"owner: run quipu protect --apply"})
		}
		found := r.requires(requiredChecks(c))
		if len(found) == 0 {
			continue
		}
		switch {
		case !c.BypassesRulesets():
			checks = append(checks, doctorCheck{"merge_without_ci", "fail",
				fmt.Sprintf("ruleset %q requires %s, so GitHub refuses a merge that skips them", r.Name, strings.Join(found, ", ")),
				"owner: set merge_bypass_rulesets: true and run quipu protect --apply, or take those checks out of the ruleset"})
		case !r.adminsBypass():
			checks = append(checks, doctorCheck{"merge_without_ci", "fail",
				fmt.Sprintf("merge_bypass_rulesets is on, but ruleset %q does not let admins bypass it", r.Name),
				"owner: run quipu protect --apply"})
		}
	}
	return checks
}

// noCIChecks is what doctor adds when merge_without_ci is on.
func noCIChecks(c model.Config) []doctorCheck {
	if !c.MergesWithoutCI() && !c.MergesInQueue() {
		return nil
	}
	rulesets, err := loadRulesets(c)
	if err != nil {
		return []doctorCheck{{"merge_without_ci", "warn", "could not read the repository's rulesets: " + err.Error(), "owner: check them by hand; a ruleset that requires the waived checks makes GitHub refuse these merges"}}
	}
	return judgeRulesets(c, rulesets)
}

func loadRulesets(c model.Config) ([]ruleset, error) {
	repo := "repos/" + c.Owner + "/" + c.Repository + "/rulesets"
	output, err := ghOutput("api", repo+"?per_page=100")
	if err != nil {
		return nil, err
	}
	var listed []struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(output, &listed); err != nil {
		return nil, err
	}
	var rulesets []ruleset
	for _, item := range listed {
		output, err := ghOutput("api", fmt.Sprintf("%s/%d", repo, item.ID))
		if err != nil {
			return nil, err
		}
		var parsed ruleset
		if err := json.Unmarshal(output, &parsed); err != nil {
			return nil, err
		}
		rulesets = append(rulesets, parsed)
	}
	return rulesets, nil
}
