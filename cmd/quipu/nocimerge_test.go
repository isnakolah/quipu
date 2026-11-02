package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

const noCIHead = "0123456789abcdef0123456789abcdef01234567"

var noCIConfig = model.Config{Merge: "agent", MergeWithoutCI: true, LocalCheck: "make check", RequiredChecks: []string{"test"}, DefaultBranch: "main"}

// fakeNoCI records what a merge without CI did.
type fakeNoCI struct {
	view       mergeView
	moved      string // the head the PR moves to while the check runs
	changes    string
	dirtyAfter string
	head       string
	violations []string
	exit       int
	checkErr   error
	refuse     map[bool]bool // refuse[admin] makes GitHub refuse that merge
	blockers   []string
	blockerErr error
	forgotten  string
	ran        []string
	merges     []bool
	updates    int
	locks      int
	recorded   string
}

func (f *fakeNoCI) ops() noCIOps {
	return noCIOps{
		view: func() (mergeView, error) { return f.view, nil },
		update: func() (string, error) {
			f.updates++
			f.view.HeadRefOid, f.head, f.view.MergeStateStatus = "updated", "updated", "BLOCKED"
			return "updated", nil
		},
		pause: func() {},
		lock:  func() (func(), error) { f.locks++; return func() {}, nil },
		hold:  func() (func(), error) { return func() {}, nil },
		head:  func() (string, error) { return f.head, nil },
		dirty: func() (string, error) {
			if len(f.ran) > 0 {
				return f.dirtyAfter, nil
			}
			return f.changes, nil
		},
		violations: func() ([]string, error) { return f.violations, nil },
		check: func(command string) (int, error) {
			f.ran = append(f.ran, command)
			if f.moved != "" {
				f.view.HeadRefOid = f.moved
			}
			return f.exit, f.checkErr
		},
		record:        func(_, value string) { f.recorded = value },
		forget:        func(head string) { f.forgotten = head },
		adminBlockers: func(mergeView) ([]string, error) { return f.blockers, f.blockerErr },
		merge: func(_ mergeView, admin bool) error {
			f.merges = append(f.merges, admin)
			if f.refuse[admin] {
				return flowErr("merge-blocked", "GitHub refused")
			}
			return nil
		},
	}
}

func pendingPull() *fakeNoCI {
	return &fakeNoCI{head: noCIHead, view: mergeView{URL: "url", State: "OPEN", HeadRefOid: noCIHead, MergeStateStatus: "BLOCKED",
		Checks: []prCheck{{Name: "test", Status: "QUEUED"}, {Name: "policy", Status: "COMPLETED", Conclusion: "SUCCESS"}}}}
}

var localEvidence = writer.Evidence{Boundary: "local"}

func flowCode(err error) string {
	var coded *flowError
	if errors.As(err, &coded) {
		return coded.Code
	}
	return ""
}

func TestMergeWithoutCIRunsTheCheckThenMerges(t *testing.T) {
	f := pendingPull()
	value, err := mergeWithoutCI(noCIConfig, false, localEvidence, f.ops())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.ran) != 1 || f.ran[0] != "make check" || len(f.merges) != 1 || f.merges[0] {
		t.Fatalf("ran %v, merges %v", f.ran, f.merges)
	}
	for _, want := range []string{"None: merged without CI at " + noCIHead, `"make check"`, "exited 0", "as reported by quipu done"} {
		if !strings.Contains(value, want) {
			t.Errorf("missing %q: %s", want, value)
		}
	}
	if f.recorded != value {
		t.Fatalf("a rerun needs the result recorded before the merge: %q", f.recorded)
	}
	if !isNoCIEvidence(value) || checkNoCIEvidence(noCIConfig, writer.Evidence{CIURL: value, Boundary: "local"}, prView{HeadRefOid: noCIHead}) != nil {
		t.Fatalf("the Writer must accept what quipu done writes: %s", value)
	}
}

func TestMergeWithoutCIRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*fakeNoCI)
		e     writer.Evidence
		code  string
		ran   bool
	}{
		"local_check fails":       {func(f *fakeNoCI) { f.exit = 2 }, localEvidence, "local-check-failed", true},
		"local_check cannot run":  {func(f *fakeNoCI) { f.checkErr = errors.New("no sh") }, localEvidence, "local-check-failed", true},
		"dirty tree":              {func(f *fakeNoCI) { f.changes = " M a.go" }, localEvidence, "worktree-unclean", false},
		"dirtied by the check":    {func(f *fakeNoCI) { f.dirtyAfter = " M a.go" }, localEvidence, "worktree-unclean", true},
		"HEAD is not the PR head": {func(f *fakeNoCI) { f.head = "other" }, localEvidence, "worktree-unclean", false},
		"PR moved during check":   {func(f *fakeNoCI) { f.moved = "moved" }, localEvidence, "worktree-unclean", true},
		"policy rules":            {func(f *fakeNoCI) { f.violations = []string{"PR changes quipu.yml"} }, localEvidence, "policy-rules", false},
		"boundary CI":             {func(*fakeNoCI) {}, writer.Evidence{Boundary: "CI"}, "evidence-missing", false},
		"conflicts":               {func(f *fakeNoCI) { f.view.MergeStateStatus = "DIRTY" }, localEvidence, "merge-blocked", false},
		"draft":                   {func(f *fakeNoCI) { f.view.IsDraft = true }, localEvidence, "merge-blocked", false},
		"behind, updates off":     {func(f *fakeNoCI) { f.view.MergeStateStatus = "BEHIND" }, localEvidence, "merge-blocked", false},
	} {
		t.Run(name, func(t *testing.T) {
			f := pendingPull()
			tc.setup(f)
			value, err := mergeWithoutCI(noCIConfig, true, tc.e, f.ops())
			if flowCode(err) != tc.code || value != "" {
				t.Fatalf("value %q, err %v", value, err)
			}
			if len(f.merges) != 0 {
				t.Fatalf("merged: %v", f.merges)
			}
			if (len(f.ran) > 0) != tc.ran {
				t.Fatalf("ran %v", f.ran)
			}
		})
	}
}

func TestFailedCheckBlocksWithOrWithoutTheBypass(t *testing.T) {
	failed := map[string]prCheck{
		"failure":          {Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"},
		"timed out":        {Name: "test", Status: "COMPLETED", Conclusion: "TIMED_OUT"},
		"cancelled midway": {Name: "test", Status: "COMPLETED", Conclusion: "CANCELLED", StartedAt: "2026-11-02T10:00:00Z", CompletedAt: "2026-11-02T10:05:00Z"},
		"commit status":    {Context: "test", State: "FAILURE"},
	}
	for name, check := range failed {
		for _, bypass := range []bool{false, true} {
			f := pendingPull()
			f.view.Checks = []prCheck{check}
			if _, err := mergeWithoutCI(noCIConfig, bypass, localEvidence, f.ops()); flowCode(err) != "checks-failing" {
				t.Errorf("%s, bypass %v: %v", name, bypass, err)
			}
			if len(f.merges) != 0 || len(f.ran) != 0 {
				t.Errorf("%s: ran %v, merges %v", name, f.ran, f.merges)
			}
		}
	}
}

func TestCheckThatFailsDuringLocalCheckStillBlocks(t *testing.T) {
	f := pendingPull()
	ops := f.ops()
	check := ops.check
	ops.check = func(command string) (int, error) {
		f.view.Checks = []prCheck{{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"}}
		return check(command)
	}
	if _, err := mergeWithoutCI(noCIConfig, true, localEvidence, ops); flowCode(err) != "checks-failing" || len(f.merges) != 0 {
		t.Fatalf("%v, merges %v", err, f.merges)
	}
}

func TestChecksThatNeverRanAreWaived(t *testing.T) {
	for name, checks := range map[string][]prCheck{
		"never created":             nil,
		"queued":                    {{Name: "test", Status: "QUEUED"}},
		"skipped":                   {{Name: "test", Status: "COMPLETED", Conclusion: "SKIPPED"}},
		"cancelled, zero start":     {{Name: "test", Status: "COMPLETED", Conclusion: "CANCELLED", StartedAt: "0001-01-01T00:00:00Z", CompletedAt: "2026-11-02T10:05:00Z"}},
		"cancelled as it was ended": {{Name: "test", Status: "COMPLETED", Conclusion: "CANCELLED", StartedAt: "2026-11-02T10:05:00Z", CompletedAt: "2026-11-02T10:05:00Z"}},
	} {
		f := pendingPull()
		f.view.Checks = checks
		value, err := mergeWithoutCI(noCIConfig, false, localEvidence, f.ops())
		if err != nil || value == "" || len(f.merges) != 1 {
			t.Errorf("%s: value %q, err %v, merges %v", name, value, err, f.merges)
		}
	}
}

func TestGreenChecksTakeTheOrdinaryMerge(t *testing.T) {
	f := pendingPull()
	f.view.Checks = passing("test", "policy")
	value, err := mergeWithoutCI(noCIConfig, true, writer.Evidence{Boundary: "CI"}, f.ops())
	if err != nil || value != "" || len(f.ran) != 0 || len(f.merges) != 0 {
		t.Fatalf("value %q, err %v, ran %v, merges %v", value, err, f.ran, f.merges)
	}
}

func TestUnreadableCancelledCheckBlocks(t *testing.T) {
	for name, check := range map[string]prCheck{
		"no times":      {Name: "test", Status: "COMPLETED", Conclusion: "CANCELLED"},
		"garbled start": {Name: "test", Status: "COMPLETED", Conclusion: "CANCELLED", StartedAt: "soon", CompletedAt: "2026-11-02T10:05:00Z"},
	} {
		f := pendingPull()
		f.view.Checks = []prCheck{check}
		if _, err := mergeWithoutCI(noCIConfig, true, localEvidence, f.ops()); flowCode(err) != "checks-failing" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestBehindFollowsUpdateBehind(t *testing.T) {
	c := noCIConfig
	c.UpdateBehind = "auto"
	f := pendingPull()
	f.view.MergeStateStatus = "BEHIND"
	value, err := mergeWithoutCI(c, false, localEvidence, f.ops())
	if err != nil || f.updates != 1 || len(f.ran) != 1 || len(f.merges) != 1 {
		t.Fatalf("it updates, checks the new head, and merges in one call: %v, updates %d, ran %v, merges %v", err, f.updates, f.ran, f.merges)
	}
	if !strings.Contains(value, "at updated;") {
		t.Fatalf("the evidence must name the head that was checked: %s", value)
	}

	dirty := pendingPull()
	dirty.view.MergeStateStatus, dirty.changes = "BEHIND", " M a.go"
	if _, err := mergeWithoutCI(c, false, localEvidence, dirty.ops()); flowCode(err) != "worktree-unclean" || dirty.updates != 0 {
		t.Fatalf("a dirty tree must not be pushed first: %v, updates %d", err, dirty.updates)
	}

	unpushed := pendingPull()
	unpushed.view.MergeStateStatus, unpushed.head = "BEHIND", "unpushed"
	if _, err := mergeWithoutCI(c, false, localEvidence, unpushed.ops()); flowCode(err) != "worktree-unclean" || unpushed.updates != 0 {
		t.Fatalf("unpushed commits must not be pushed first: %v, updates %d", err, unpushed.updates)
	}
}

func TestBaseMovingDuringTheCheckStopsTheMerge(t *testing.T) {
	for _, state := range []string{"BEHIND", "DIRTY"} {
		f := pendingPull()
		ops := f.ops()
		check := ops.check
		ops.check = func(command string) (int, error) {
			f.view.MergeStateStatus = state
			return check(command)
		}
		if _, err := mergeWithoutCI(noCIConfig, true, localEvidence, ops); flowCode(err) != "merge-blocked" || len(f.merges) != 0 {
			t.Errorf("%s: %v, merges %v", state, err, f.merges)
		}
	}
}

func TestAdminOnlyAfterRefusalAndOnlyWithTheBypass(t *testing.T) {
	refused := map[bool]bool{false: true}

	off := pendingPull()
	off.refuse = refused
	if _, err := mergeWithoutCI(noCIConfig, false, localEvidence, off.ops()); flowCode(err) != "merge-blocked" {
		t.Fatalf("bypass off: GitHub's refusal is merge-blocked: %v", err)
	}
	if len(off.merges) != 1 || off.merges[0] {
		t.Fatalf("bypass off must never use --admin: %v", off.merges)
	}

	on := pendingPull()
	on.refuse = refused
	if _, err := mergeWithoutCI(noCIConfig, true, localEvidence, on.ops()); err != nil {
		t.Fatal(err)
	}
	if len(on.merges) != 2 || on.merges[0] || !on.merges[1] {
		t.Fatalf("plain first, then --admin: %v", on.merges)
	}
	if !strings.HasSuffix(on.recorded, noCIAdminNote) {
		t.Fatalf("the evidence says the bypass was used: %s", on.recorded)
	}
	if err := checkNoCIEvidence(noCIConfig, writer.Evidence{CIURL: on.recorded, Boundary: "local"}, prView{HeadRefOid: noCIHead}); err != nil {
		t.Fatal(err)
	}

	for name, setup := range map[string]func(*fakeNoCI){
		"something else stopped it": func(f *fakeNoCI) { f.blockers = []string{"2 unresolved review threads"} },
		"can't tell":                func(f *fakeNoCI) { f.blockerErr = errors.New("502") },
	} {
		f := pendingPull()
		f.refuse = refused
		setup(f)
		if _, err := mergeWithoutCI(noCIConfig, true, localEvidence, f.ops()); flowCode(err) != "merge-blocked" {
			t.Errorf("%s: %v", name, err)
		}
		if len(f.merges) != 1 || f.merges[0] {
			t.Errorf("%s must not use --admin: %v", name, f.merges)
		}
		if f.forgotten != noCIHead {
			t.Errorf("%s: a refused merge must leave no record of one", name)
		}
	}

	plain := pendingPull()
	if _, err := mergeWithoutCI(noCIConfig, true, localEvidence, plain.ops()); err != nil || len(plain.merges) != 1 || plain.merges[0] {
		t.Fatalf("no refusal, no --admin: %v %v", plain.merges, err)
	}

	both := pendingPull()
	both.refuse = map[bool]bool{false: true, true: true}
	if _, err := mergeWithoutCI(noCIConfig, true, localEvidence, both.ops()); flowCode(err) != "merge-blocked" {
		t.Fatalf("%v", err)
	}
}

func TestAdminBlockers(t *testing.T) {
	queued := []prCheck{{Name: "test", Status: "QUEUED"}, {Name: "policy", Status: "COMPLETED", Conclusion: "SUCCESS"}}
	if got := judgeAdminBlockers(queued, "", 0); len(got) != 0 {
		t.Fatalf("only checks that never ran: %v", got)
	}
	failedOutsideRequired := append(queued[:2:2], prCheck{Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"})
	if got := judgeAdminBlockers(failedOutsideRequired, "", 0); len(got) != 1 || !strings.Contains(got[0], "lint") {
		t.Fatalf("a ruleset may require a check required_checks doesn't list: %v", got)
	}
	if got := judgeAdminBlockers(queued, "CHANGES_REQUESTED", 2); len(got) != 2 {
		t.Fatalf("%v", got)
	}
}

func TestPolicyRulesAreTheRealOnes(t *testing.T) {
	c := noCIConfig
	c.Owner, c.Repository = "o", "r"
	pull := pullFacts{Number: 1, Title: "feat: x", Body: "Refs #7", Author: "o", Base: "main", Head: "007/work", State: "OPEN", Files: []string{rootConfigPath}}
	violations := prViolations(c, pull, func(int) (liveWork, error) { return liveWork{}, errors.New("no work") }, time.Now())
	if !strings.Contains(strings.Join(violations, "\n"), ownerApprovedLabel) {
		t.Fatalf("the owner-approval rule must apply: %v", violations)
	}
	f := pendingPull()
	f.violations = violations
	if _, err := mergeWithoutCI(c, true, localEvidence, f.ops()); flowCode(err) != "policy-rules" || len(f.merges) != 0 {
		t.Fatalf("%v, merges %v", err, f.merges)
	}
}

func TestFlagsComeOnlyFromTheDefaultBranch(t *testing.T) {
	flags := "merge: agent\nmerge_without_ci: true\nlocal_check: make check\nmerge_bypass_rulesets: true\n"
	settings := func(t *testing.T, w worktree) (bool, bool) {
		t.Helper()
		c, source, err := loadEnforcedConfig(w, true)
		if err != nil {
			t.Fatal(err)
		}
		return noCISettings(c, source)
	}
	t.Run("default branch", func(t *testing.T) {
		_, agent := flaggedRepo(t, flags)
		if noCI, bypass := settings(t, agent); !noCI || !bypass || !enforcedBypass(agent) {
			t.Fatalf("noCI %v, bypass %v", noCI, bypass)
		}
	})
	t.Run("worktree copy", func(t *testing.T) {
		_, agent := flaggedRepo(t, "")
		writeRepoFile(t, agent.Top, rootConfigPath, append(repoConfig(t), flags...))
		if noCI, bypass := settings(t, agent); noCI || bypass || enforcedBypass(agent) {
			t.Fatalf("noCI %v, bypass %v", noCI, bypass)
		}
	})
	t.Run("no origin", func(t *testing.T) {
		top := gitRepo(t)
		writeRepoFile(t, top, rootConfigPath, append(repoConfig(t), flags...))
		w, err := openWorktree(top)
		if err != nil {
			t.Fatal(err)
		}
		if noCI, bypass := settings(t, w); noCI || bypass || enforcedBypass(w) {
			t.Fatalf("noCI %v, bypass %v", noCI, bypass)
		}
	})
}

func TestGuardStillDeniesARawMerge(t *testing.T) {
	_, agent := flaggedRepo(t, "merge: agent\nmerge_without_ci: true\nlocal_check: make check\nmerge_bypass_rulesets: true\n")
	for _, command := range []string{"gh pr merge 7 --admin", "gh pr merge 7 --merge --admin --match-head-commit abc", "gh pr merge 7"} {
		if code := guardCode(t, agent.Top, "Bash", map[string]any{"command": command}); code != "merge-approve" {
			t.Errorf("%q: %q", command, code)
		}
	}
}

func TestNoCLIFlagWaivesTheGate(t *testing.T) {
	for _, flag := range []string{"--merge-without-ci", "--admin", "--bypass"} {
		if err := doneCommand([]string{"7", flag}); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%s must not be a flag: %v", flag, err)
		}
	}
}

func TestWriterAcceptsSkippedCIOnlyWithTheFlag(t *testing.T) {
	value := noCIEvidence(noCIHead, "make check", 0)
	good := writer.Evidence{CIURL: value, Boundary: "local"}
	onHead := prView{HeadRefOid: noCIHead}
	if err := checkNoCIEvidence(noCIConfig, good, onHead); err != nil {
		t.Fatal(err)
	}
	failed := prCheck{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE", CompletedAt: "2026-11-02T10:00:00Z"}
	mergedAfter := prView{HeadRefOid: noCIHead, MergedAt: "2026-11-02T11:00:00Z", Checks: []prCheck{failed}}
	if err := checkNoCIEvidence(noCIConfig, good, mergedAfter); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("a check that failed before the merge was not skipped: %v", err)
	}
	mergedBefore := prView{HeadRefOid: noCIHead, MergedAt: "2026-11-02T09:00:00Z", Checks: []prCheck{failed}}
	if err := checkNoCIEvidence(noCIConfig, good, mergedBefore); err != nil {
		t.Fatalf("one that failed after the merge must not strand the Issue: %v", err)
	}
	for name, tc := range map[string]struct {
		c    model.Config
		e    writer.Evidence
		view prView
		want string
	}{
		"flag off":         {model.Config{Merge: "agent"}, good, onHead, "off on the default branch"},
		"no merge: agent":  {model.Config{MergeWithoutCI: true, LocalCheck: "x"}, good, onHead, "off on the default branch"},
		"without relaxing": {noCIConfig.WithoutRelaxations(), good, onHead, "off on the default branch"},
		"another head":     {noCIConfig, good, prView{HeadRefOid: strings.Repeat("f", 40)}, "PR's head"},
		"boundary CI":      {noCIConfig, writer.Evidence{CIURL: value, Boundary: "CI"}, onHead, "boundary must be local"},
		"not our form":     {noCIConfig, writer.Evidence{CIURL: noCIPrefix + "abc; trust me", Boundary: "local"}, onHead, "not the form"},
		"free text":        {noCIConfig, writer.Evidence{CIURL: noCIPrefix + noCIHead + "; anything", Boundary: "local"}, onHead, "not the form"},
		"check failed":     {noCIConfig, writer.Evidence{CIURL: noCIEvidence(noCIHead, "make check", 2), Boundary: "local"}, onHead, "not the form"},
	} {
		if err := checkNoCIEvidence(tc.c, tc.e, tc.view); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Anything else that isn't a run of this repository still fails as before.
	c := noCIConfig
	c.Owner, c.Repository = "o", "r"
	if err := checkCIRun(c, "None: skipped it", prView{}); err == nil {
		t.Fatal("with or without the flag, only the exact form skips the run check")
	}
}

func TestMergeWithoutCIConfigIsValidated(t *testing.T) {
	valid := func() model.Config {
		return model.Config{SchemaVersion: model.ConfigVersion, Owner: "o", Repository: "r", DefaultBranch: "main", WikiMode: "off", Areas: []string{"core"}, AuthorizedActors: []string{"o"},
			Merge: "agent", MergeWithoutCI: true, LocalCheck: "make check"}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	withBypass := valid()
	withBypass.MergeBypassRulesets = true
	if err := withBypass.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*model.Config){
		"no merge: agent":      func(c *model.Config) { c.Merge = "" },
		"no local_check":       func(c *model.Config) { c.LocalCheck = " " },
		"two lines":            func(c *model.Config) { c.LocalCheck = "a\nb" },
		"check without flag":   func(c *model.Config) { c.MergeWithoutCI = false },
		"bypass without flag":  func(c *model.Config) { c.MergeWithoutCI, c.LocalCheck, c.MergeBypassRulesets = false, "", true },
		"with the merge queue": func(c *model.Config) { c.MergeWait = "background" },
	} {
		c := valid()
		change(&c)
		if c.Validate() == nil {
			t.Errorf("%s must not validate", name)
		}
	}
	cleared := withBypass.WithoutRelaxations()
	if cleared.MergesWithoutCI() || cleared.BypassesRulesets() || cleared.LocalCheck != "" {
		t.Fatalf("%+v", cleared)
	}
}

func TestDoctorFailsOnARulesetThatWouldRefuse(t *testing.T) {
	requiring := func(bypass bool) ruleset {
		r := ruleset{Name: "main", Enforcement: "active", Target: "branch"}
		r.Rules = append(r.Rules, struct {
			Type       string `json:"type"`
			Parameters struct {
				Checks []struct {
					Context string `json:"context"`
				} `json:"required_status_checks"`
			} `json:"parameters"`
		}{Type: "required_status_checks"})
		r.Rules[0].Parameters.Checks = append(r.Rules[0].Parameters.Checks, struct {
			Context string `json:"context"`
		}{"test"})
		if bypass {
			r.BypassActors = append(r.BypassActors, struct {
				ActorID   int    `json:"actor_id"`
				ActorType string `json:"actor_type"`
				Mode      string `json:"bypass_mode"`
			}{adminRoleID, "RepositoryRole", "always"})
		}
		return r
	}
	on := noCIConfig
	on.MergeBypassRulesets = true

	if got := judgeRulesets(noCIConfig, []ruleset{requiring(false)}); len(got) != 1 || got[0].Status != "fail" || !strings.Contains(got[0].Fix, "merge_bypass_rulesets") {
		t.Fatalf("bypass off: %+v", got)
	}
	if got := judgeRulesets(on, []ruleset{requiring(false)}); len(got) != 1 || got[0].Status != "fail" || !strings.Contains(got[0].Fix, "quipu protect") {
		t.Fatalf("bypass on but not in place: %+v", got)
	}
	if got := judgeRulesets(on, []ruleset{requiring(true)}); len(got) != 0 {
		t.Fatalf("in place: %+v", got)
	}
	inactive := requiring(false)
	inactive.Enforcement = "disabled"
	if got := judgeRulesets(noCIConfig, []ruleset{inactive, {Name: "branches", Enforcement: "active", Target: "branch"}}); len(got) != 0 {
		t.Fatalf("nothing requires the checks: %+v", got)
	}
	release := requiring(false)
	release.Conditions.RefName.Include = []string{"refs/heads/release/*"}
	if got := judgeRulesets(noCIConfig, []ruleset{release}); len(got) != 0 {
		t.Fatalf("it doesn't govern the default branch: %+v", got)
	}
	excluded := requiring(false)
	excluded.Conditions.RefName.Include, excluded.Conditions.RefName.Exclude = []string{"~ALL"}, []string{"refs/heads/main"}
	if got := judgeRulesets(noCIConfig, []ruleset{excluded}); len(got) != 0 {
		t.Fatalf("it excludes the default branch: %+v", got)
	}
	governing := requiring(false)
	governing.Conditions.RefName.Include = []string{"~DEFAULT_BRANCH"}
	if got := judgeRulesets(noCIConfig, []ruleset{governing}); len(got) != 1 {
		t.Fatalf("%+v", got)
	}
	unlisted := requiring(false)
	unlisted.CurrentUserCanBypass = "always"
	if got := judgeRulesets(on, []ruleset{unlisted}); len(got) != 0 {
		t.Fatalf("a token that can't list actors still hears it can bypass: %+v", got)
	}
	if got := noCIChecks(model.Config{}); got != nil {
		t.Fatalf("flag off: %+v", got)
	}
}

func TestRerunAfterTheMergeLands(t *testing.T) {
	_, agent := flaggedRepo(t, "")
	unknown := noCIRerunEvidence(agent, noCIHead, "")
	if !isNoCIEvidence(unknown) || !strings.Contains(unknown, "not known") || checkNoCIEvidence(noCIConfig, writer.Evidence{CIURL: unknown, Boundary: "local"}, prView{HeadRefOid: noCIHead}) != nil {
		t.Fatalf("no record: %s", unknown)
	}
	if got := noCIRerunEvidence(agent, noCIHead, "https://run"); got != "https://run" {
		t.Fatalf("a required check's real run is the evidence: %s", got)
	}
	value := noCIEvidence(noCIHead, "make check", 0)
	recordLocalCheck(agent, noCIHead, value)
	if got := noCIRerunEvidence(agent, noCIHead, "https://run"); got != value {
		t.Fatalf("the record says what happened: %s", got)
	}

	awaiting := noCIAwaitingReceipt("url", flowErr("no-receipt", "the Writer is asleep"))
	if flowCode(awaiting) != "no-receipt" || !strings.Contains(awaiting.Error(), "merged without CI") || !strings.Contains(awaiting.Error(), "queued") {
		t.Fatalf("%v", awaiting)
	}
	rejected := flowErr("rejected", "no")
	if got := noCIAwaitingReceipt("url", rejected); !errors.Is(got, rejected) {
		t.Fatalf("%v", got)
	}

	e := writer.Evidence{Environments: defaultEnvironments, Artifacts: defaultArtifacts}
	noCIDefaults(&e)
	if strings.Contains(e.Environments, "GitHub Actions") || strings.Contains(e.Artifacts, "CI run linked") {
		t.Fatalf("%+v", e)
	}
	mine := writer.Evidence{Environments: "my mac", Artifacts: "https://log"}
	noCIDefaults(&mine)
	if mine.Environments != "my mac" || mine.Artifacts != "https://log" {
		t.Fatalf("%+v", mine)
	}
}

func TestRunLocalCheckReportsItsExitCode(t *testing.T) {
	top := t.TempDir()
	if exit, err := runLocalCheck(top, "exit 7"); err != nil || exit != 7 {
		t.Fatalf("%d %v", exit, err)
	}
	if exit, err := runLocalCheck(top, "touch here"); err != nil || exit != 0 {
		t.Fatalf("%d %v", exit, err)
	}
	if _, err := os.Stat(filepath.Join(top, "here")); err != nil {
		t.Fatalf("it runs in the worktree: %v", err)
	}
}

func TestEveryFlagErrorNamesItsFlag(t *testing.T) {
	for _, kind := range flagErrors {
		if flagErrorOn[kind.Code] == nil {
			t.Errorf("%s is missing from flagErrorOn, and renderGuideFor would panic", kind.Code)
		}
	}
}

func TestHandbookStatesTheFlagOnlyWhenOn(t *testing.T) {
	if text := renderGuideFor(noCIConfig); !strings.Contains(text, "`merge_without_ci`:") || !strings.Contains(text, "local-check-failed") {
		t.Fatal("flag on: the handbook must say what it does")
	}
	for _, absent := range []string{"merge_without_ci", "local-check-failed", "worktree-unclean", "policy-rules"} {
		if strings.Contains(renderGuideFor(model.Config{}), absent) {
			t.Errorf("flag off: %q must not appear", absent)
		}
	}
}
