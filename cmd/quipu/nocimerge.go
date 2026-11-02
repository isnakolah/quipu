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
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

const (
	noCIPrefix = "None: merged without CI at "
	defaultEnvironments = "GitHub Actions CI"
	defaultArtifacts    = "None: CI run linked"
	// What the evidence says in place of those defaults, which name a CI run.
	noCIEnvironment = "This worktree only; no CI ran"
	noCIArtifacts   = "None: no CI ran"
)

var noCIValue = regexp.MustCompile(`^None: merged without CI at ([0-9a-f]{40}); `)

// noCIEvidence is the CI field of a merge without CI. The exit code is the
// client's report: the Writer cannot verify a run on another machine.
func noCIEvidence(head, command string, exit int) string {
	return fmt.Sprintf("%s%s; local_check %q exited %d, as reported by quipu done", noCIPrefix, head, command, exit)
}

// noCIEvidenceUnknown is the CI field when a rerun has no record of the check.
func noCIEvidenceUnknown(head string) string {
	return noCIPrefix + head + "; local_check result not known to this rerun, as reported by quipu done"
}

func isNoCIEvidence(ci string) bool { return strings.HasPrefix(ci, noCIPrefix) }

// checkNoCIEvidence is what the Writer asks of evidence that skipped CI. Its
// config comes from the default branch, so a PR cannot turn this on for itself.
func checkNoCIEvidence(c model.Config, e writer.Evidence, head string) error {
	if !c.MergesWithoutCI() {
		return errors.New("evidence says the merge skipped CI, but merge_without_ci is off on the default branch")
	}
	match := noCIValue.FindStringSubmatch(e.CIURL)
	if match == nil {
		return fmt.Errorf("CI value %q is not the form quipu done writes", e.CIURL)
	}
	if match[1] != head {
		return fmt.Errorf("evidence says the merge skipped CI at %s, but the PR's head is %s", match[1], head)
	}
	if e.Boundary != "local" {
		return fmt.Errorf("a merge without CI was verified only locally, so its boundary must be local, not %q", e.Boundary)
	}
	return nil
}

// noCISettings reads the flags that waive checks. A worktree's copy of the config
// never counts, including in a clone with no origin to read the default branch from.
func noCISettings(c model.Config, source string) (noCI, bypass bool) {
	if source == configSourceWorktree {
		return false, false
	}
	return c.MergesWithoutCI(), c.BypassesRulesets()
}

// cancelledBeforeStarting is GitHub ending a job nothing ever picked up: it
// never ran, so it says nothing about the code. The times are how it shows.
func cancelledBeforeStarting(check prCheck) bool {
	if check.Conclusion != "CANCELLED" {
		return false
	}
	started, err := time.Parse(time.RFC3339, check.StartedAt)
	if err != nil || started.IsZero() {
		return true
	}
	completed, err := time.Parse(time.RFC3339, check.CompletedAt)
	return err == nil && !started.Before(completed)
}

// waivedChecks names the required checks a merge without CI goes ahead without:
// those pending, never run, or cancelled before starting. One that ran and failed
// is an error, whatever the flags.
func waivedChecks(checks []prCheck, required []string) ([]string, error) {
	var ran []prCheck
	for _, check := range checks {
		if !cancelledBeforeStarting(check) {
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
	update     func() error
	head       func() (string, error)
	dirty      func() (string, error)
	violations func() ([]string, error)
	check      func(command string) (int, error)
	record     func(head, value string)
	merge      func(view mergeView, admin bool) error
}

// mergeWithoutCI merges with required checks waived. It returns "" and no error
// when no check is waived, so the ordinary merge applies, and otherwise the CI
// field for the evidence.
func mergeWithoutCI(c model.Config, bypass bool, e writer.Evidence, ops noCIOps) (string, error) {
	view, err := ops.view()
	if err != nil {
		return "", err
	}
	required := requiredChecks(c)
	if mergeGate(view.Checks, required) == nil {
		return "", nil
	}
	if _, err := waivedChecks(view.Checks, required); err != nil {
		return "", err
	}
	if e.Boundary != "local" {
		return "", flowErr("evidence-missing", "this merge skips CI, so --boundary must be local, not %q", e.Boundary)
	}
	if view.IsDraft || view.MergeStateStatus == "DIRTY" {
		return "", flowErr("merge-blocked", "PR %s cannot merge yet (draft %v, state %s)", view.URL, view.IsDraft, view.MergeStateStatus)
	}
	if view.MergeStateStatus == "BEHIND" {
		if !c.UpdatesBehind() {
			return "", flowErr("merge-blocked", "PR %s is behind %s", view.URL, c.DefaultBranch)
		}
		if err := ops.update(); err != nil {
			return "", err
		}
		if view, err = ops.view(); err != nil {
			return "", err
		}
	}
	if err := checkCleanAt(ops, view.HeadRefOid); err != nil {
		return "", err
	}
	violations, err := ops.violations()
	if err != nil {
		return "", err
	}
	if len(violations) > 0 {
		return "", flowErr("policy-rules", "PR %s breaks the policy rules, which merge_without_ci never waives: %s", view.URL, strings.Join(violations, "; "))
	}
	exit, err := ops.check(c.LocalCheck)
	if err != nil {
		return "", flowErr("local-check-failed", "local_check could not run: %v", err)
	}
	if exit != 0 {
		return "", flowErr("local-check-failed", "local_check %q exited %d, so nothing merged", c.LocalCheck, exit)
	}
	// It may have taken a while, and it ran this repository's code.
	if err := checkCleanAt(ops, view.HeadRefOid); err != nil {
		return "", err
	}
	head := view.HeadRefOid
	if view, err = ops.view(); err != nil {
		return "", err
	}
	if view.HeadRefOid != head {
		return "", flowErr("worktree-unclean", "PR %s moved to %s while local_check ran on %s; rerun quipu done", view.URL, view.HeadRefOid, head)
	}
	waived, err := waivedChecks(view.Checks, required)
	if err != nil {
		return "", err
	}
	value := noCIEvidence(head, c.LocalCheck, exit)
	ops.record(head, value)
	err = ops.merge(view, false)
	if err != nil && bypass && len(waived) > 0 {
		err = ops.merge(view, true)
	}
	if err != nil {
		return "", err
	}
	return value, nil
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
func liveNoCIOps(c model.Config, w worktree, number int, branch, url string) noCIOps {
	return noCIOps{
		view: func() (mergeView, error) { return loadMergeView(c, url) },
		update: func() error {
			_, err := mergeDefaultIn(w, branch, c.DefaultBranch)
			return err
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
		check:  func(command string) (int, error) { return runLocalCheck(w.Top, command) },
		record: func(head, value string) { recordLocalCheck(w, head, value) },
		merge: func(view mergeView, admin bool) error {
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

// noCIRerunEvidence is the CI field when `quipu done` runs again after a merge
// without CI. A real run wins over having no record of the check. It returns ""
// when the PR's own CI is the evidence.
func noCIRerunEvidence(w worktree, head, passingRun string) string {
	if raw, err := os.ReadFile(localCheckRecord(w, head)); err == nil {
		if value := strings.TrimSpace(string(raw)); isNoCIEvidence(value) {
			return value
		}
	}
	if passingRun != "" {
		return ""
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
	Name         string `json:"name"`
	Enforcement  string `json:"enforcement"`
	Target       string `json:"target"`
	BypassActors []struct {
		ActorID   int    `json:"actor_id"`
		ActorType string `json:"actor_type"`
		Mode      string `json:"bypass_mode"`
	} `json:"bypass_actors"`
	Rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			Checks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	} `json:"rules"`
}

func (r ruleset) adminsBypass() bool {
	for _, actor := range r.BypassActors {
		if actor.ActorType == "RepositoryRole" && actor.ActorID == adminRoleID && actor.Mode == "always" {
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
		if r.Enforcement != "active" || r.Target != "branch" {
			continue
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
	if !c.MergesWithoutCI() {
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
