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
	ran        []string
	merges     []bool
	updates    int
	recorded   string
}

func (f *fakeNoCI) ops() noCIOps {
	return noCIOps{
		view:   func() (mergeView, error) { return f.view, nil },
		update: func() error { f.updates++; f.view.HeadRefOid, f.head = "updated", "updated"; return nil },
		head:   func() (string, error) { return f.head, nil },
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
		record: func(_, value string) { f.recorded = value },
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
	if !isNoCIEvidence(value) || checkNoCIEvidence(noCIConfig, writer.Evidence{CIURL: value, Boundary: "local"}, noCIHead) != nil {
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
		"cancelled, no start time":  {{Name: "test", Status: "COMPLETED", Conclusion: "CANCELLED"}},
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

func TestBehindFollowsUpdateBehind(t *testing.T) {
	c := noCIConfig
	c.UpdateBehind = "auto"
	f := pendingPull()
	f.view.MergeStateStatus = "BEHIND"
	value, err := mergeWithoutCI(c, false, localEvidence, f.ops())
	if err != nil || f.updates != 1 || !strings.Contains(value, "at updated") && !strings.Contains(value, "updated") {
		t.Fatalf("value %q, err %v, updates %d", value, err, f.updates)
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
	if err := checkNoCIEvidence(noCIConfig, good, noCIHead); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		c    model.Config
		e    writer.Evidence
		head string
		want string
	}{
		"flag off":         {model.Config{Merge: "agent"}, good, noCIHead, "off on the default branch"},
		"no merge: agent":  {model.Config{MergeWithoutCI: true, LocalCheck: "x"}, good, noCIHead, "off on the default branch"},
		"without relaxing": {noCIConfig.WithoutRelaxations(), good, noCIHead, "off on the default branch"},
		"another head":     {noCIConfig, good, strings.Repeat("f", 40), "PR's head"},
		"boundary CI":      {noCIConfig, writer.Evidence{CIURL: value, Boundary: "CI"}, noCIHead, "boundary must be local"},
		"not our form":     {noCIConfig, writer.Evidence{CIURL: noCIPrefix + "abc; trust me", Boundary: "local"}, noCIHead, "not the form"},
	} {
		if err := checkNoCIEvidence(tc.c, tc.e, tc.head); err == nil || !strings.Contains(err.Error(), tc.want) {
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
		"no merge: agent":     func(c *model.Config) { c.Merge = "" },
		"no local_check":      func(c *model.Config) { c.LocalCheck = " " },
		"two lines":           func(c *model.Config) { c.LocalCheck = "a\nb" },
		"check without flag":  func(c *model.Config) { c.MergeWithoutCI = false },
		"bypass without flag": func(c *model.Config) { c.MergeWithoutCI, c.LocalCheck, c.MergeBypassRulesets = false, "", true },
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
	if got := noCIChecks(model.Config{}); got != nil {
		t.Fatalf("flag off: %+v", got)
	}
}

func TestRerunAfterTheMergeLands(t *testing.T) {
	_, agent := flaggedRepo(t, "")
	unknown := noCIRerunEvidence(agent, noCIHead, "")
	if !isNoCIEvidence(unknown) || !strings.Contains(unknown, "not known") || checkNoCIEvidence(noCIConfig, writer.Evidence{CIURL: unknown, Boundary: "local"}, noCIHead) != nil {
		t.Fatalf("no record: %s", unknown)
	}
	if got := noCIRerunEvidence(agent, noCIHead, "https://run"); got != "" {
		t.Fatalf("a real run is the evidence: %s", got)
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
