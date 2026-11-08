package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

var downCheck = doctorCheck{"runner: Writer", "fail", "needs [self-hosted, old-box]: runner old-box is offline", runnerFix}

func won() (runnerClaim, error) { return runnerClaim{won: true}, nil }

func TestRunnerStartRunsWhenARunnerIsDown(t *testing.T) {
	var ran []string
	note := runnerStarter{
		down:    func() []doctorCheck { return []doctorCheck{downCheck} },
		command: func() string { return "start-it" },
		claim:   won,
		run:     func(command string) error { ran = append(ran, command); return nil },
	}.ensure()
	if len(ran) != 1 || ran[0] != "start-it" {
		t.Fatalf("ran %v", ran)
	}
	for _, want := range []string{"old-box is offline", "without waiting", "quipu runner status"} {
		if !strings.Contains(note, want) {
			t.Fatalf("missing %q: %s", want, note)
		}
	}
}

func TestRunnerStartNeverRunsWhenRunnersAreUp(t *testing.T) {
	never := func(string) error { t.Fatal("no runner is down"); return nil }
	note := runnerStarter{down: func() []doctorCheck { return nil }, command: func() string { t.Fatal("no need to read it"); return "" }, claim: won, run: never}.ensure()
	if note != "" {
		t.Fatalf("nothing to say: %q", note)
	}
}

func TestRunnerStartUnsetOrFailingIsReported(t *testing.T) {
	never := func(string) error { t.Fatal("nothing to run"); return nil }
	unset := runnerStarter{down: func() []doctorCheck { return []doctorCheck{downCheck} }, command: func() string { return "" }, claim: won, run: never}.ensure()
	for _, want := range []string{"old-box is offline", "runner_start is unset", "owner:"} {
		if !strings.Contains(unset, want) {
			t.Errorf("unset: missing %q: %s", want, unset)
		}
	}
	failed := runnerStarter{down: func() []doctorCheck { return []doctorCheck{downCheck} }, command: func() string { return "x" }, claim: won, run: func(string) error { return errors.New("exit status 3") }}.ensure()
	for _, want := range []string{"runner_start failed", "exit status 3", "owner:"} {
		if !strings.Contains(failed, want) {
			t.Errorf("failed: missing %q: %s", want, failed)
		}
	}
}

func TestRunnerStartRunsOncePerClone(t *testing.T) {
	top, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	main, err := openWorktree(top)
	if err != nil {
		t.Fatal(err)
	}
	claimed := func(w worktree, at time.Time) bool {
		t.Helper()
		claim, err := claimRunnerStart(w, at)
		if err != nil {
			t.Fatal(err)
		}
		return claim.won
	}
	if !claimed(agent, now) {
		t.Fatal("the first start runs")
	}
	if claimed(main, now.Add(time.Minute)) {
		t.Fatal("another worktree of the clone must not start it again")
	}
	if claimed(agent, now.Add(runnerStartEvery-time.Second)) {
		t.Fatal("too soon")
	}
	if !claimed(agent, now.Add(runnerStartEvery)) {
		t.Fatal("a later need may start it again")
	}
	runs := 0
	starter := runnerStarter{down: func() []doctorCheck { return []doctorCheck{downCheck} }, command: func() string { return "x" },
		claim: func() (runnerClaim, error) { return claimRunnerStart(agent, now.Add(time.Hour)) }, run: func(string) error { runs++; return nil }}
	first, second := starter.ensure(), starter.ensure()
	if runs != 1 || !strings.Contains(first, "without waiting") || !strings.Contains(second, "not run again") {
		t.Fatalf("runs=%d first=%q second=%q", runs, first, second)
	}

	later := now.Add(2 * time.Hour)
	var wins atomic.Int32
	var racing sync.WaitGroup
	for range 20 {
		racing.Add(1)
		go func() {
			defer racing.Done()
			if claim, err := claimRunnerStart(agent, later); err == nil && claim.won {
				wins.Add(1)
			}
		}()
	}
	racing.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d agents started at once, and %d of them won the claim", 20, wins.Load())
	}
}

func TestRunnerStartFailureIsToldToThoseItHoldsBack(t *testing.T) {
	_, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if claim, err := claimRunnerStart(agent, now); err != nil || !claim.won {
		t.Fatalf("%+v %v", claim, err)
	}
	recordRunnerFailure(agent, errors.New("exit status 3\n(output in log)"))
	note := runnerStarter{down: func() []doctorCheck { return []doctorCheck{downCheck} }, command: func() string { return "x" },
		claim: func() (runnerClaim, error) { return claimRunnerStart(agent, now.Add(time.Minute)) },
		run:   func(string) error { t.Fatal("not due"); return nil }}.ensure()
	for _, want := range []string{"runner_start failed", "exit status 3 (output in log)", "owner:"} {
		if !strings.Contains(note, want) {
			t.Errorf("missing %q: %s", want, note)
		}
	}
	if claim, err := claimRunnerStart(agent, now.Add(runnerStartEvery)); err != nil || !claim.won {
		t.Fatalf("it may be tried again later: %+v %v", claim, err)
	}
}

func TestRunnerStartThatCannotClaimDoesNotRun(t *testing.T) {
	note := runnerStarter{down: func() []doctorCheck { return []doctorCheck{downCheck} }, command: func() string { return "x" },
		claim: func() (runnerClaim, error) { return runnerClaim{}, errors.New("read-only") },
		run:   func(string) error { t.Fatal("must not run"); return nil }}.ensure()
	if !strings.Contains(note, "read-only") || strings.Contains(note, "ran in the last") {
		t.Fatal(note)
	}
}

func TestRunnerStartComesOnlyFromTheDefaultBranch(t *testing.T) {
	t.Run("default branch", func(t *testing.T) {
		_, agent := flaggedRepo(t, "workflows:\n  runner_start: start-it\n")
		if got := enforcedRunnerStart(agent); got != "start-it" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("worktree copy", func(t *testing.T) {
		_, agent := flaggedRepo(t, "")
		writeRepoFile(t, agent.Top, rootConfigPath, append(repoConfig(t), "workflows:\n  runner_start: evil\n"...))
		if got := enforcedRunnerStart(agent); got != "" {
			t.Fatalf("a worktree's copy must never run: %q", got)
		}
	})
	t.Run("no origin", func(t *testing.T) {
		top := gitRepo(t)
		writeRepoFile(t, top, rootConfigPath, append(repoConfig(t), "workflows:\n  runner_start: evil\n"...))
		w, err := openWorktree(top)
		if err != nil {
			t.Fatal(err)
		}
		if got := enforcedRunnerStart(w); got != "" {
			t.Fatalf("with no default branch to read, nothing runs: %q", got)
		}
	})
	if got := (model.Config{Workflows: model.Workflows{RunnerStart: "x"}}).WithoutRelaxations().Workflows.RunnerStart; got != "" {
		t.Fatalf("an untrusted config cannot name a command: %q", got)
	}
}

func TestRunnerStartValidates(t *testing.T) {
	c := model.Config{SchemaVersion: model.ConfigVersion, Owner: "o", Repository: "r", DefaultBranch: "main", WikiMode: "off", Areas: []string{"core"}, AuthorizedActors: []string{"o"}}
	c.Workflows.RunnerStart = "ssh box 'svc start'"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Workflows.RunnerStart = "one\ntwo"
	if c.Validate() == nil {
		t.Fatal("a command must be one line")
	}
}

func TestRunDetachedDoesNotBlock(t *testing.T) {
	top := t.TempDir()
	log := filepath.Join(top, "runner.log")
	started := time.Now()
	if err := runDetached(top, "echo up; sleep 5", log, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("waited %s for a command that runs on", took)
	}
	if err := runDetached(top, "exit 3", log, time.Minute); err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), log) {
		t.Fatalf("a fast failure is reported, with where its output went: %v", err)
	}
	if err := runDetached(top, "touch ran-here", log, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(top, "ran-here")); err != nil {
		t.Fatalf("it runs in the directory it was given: %v", err)
	}
}

func TestRunnerStatusReport(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	c := model.Config{Owner: "o", Repository: "r"}
	queued := []queuedJob{
		{Name: "fmt", Labels: []string{"self-hosted", "calla-mac"}},
		{Name: "linux", Labels: []string{"self-hosted", "old-box"}},
		{Name: "gone", Labels: []string{"self-hosted", "calla-linux"}},
	}
	report := buildRunnerStatus(c, callaRunners, nil, queued, nil, now.Add(-2*time.Hour), nil, now)
	if report.State != "known" || len(report.Runners) != 2 || report.Runners[0].State != "online" || report.Runners[1].State != "offline" || report.Runners[1].Labels[2] != "old-box" {
		t.Fatalf("%+v", report)
	}
	if report.Queued[0].Verdict != "will start" || report.Queued[1].Verdict != "cannot start" || report.Queued[2].Verdict != "cannot start" || !strings.Contains(report.Queued[1].Detail, "offline") {
		t.Fatalf("queued: %+v", report.Queued)
	}
	if !strings.Contains(report.WriterRun, "2h0m0s ago") || report.RunnerStart != "unset" {
		t.Fatalf("%+v", report)
	}

	unknown := buildRunnerStatus(c, runnerFacts{Err: errors.New("403")}, nil, queued, errors.New("rate limited"), time.Time{}, errors.New("404"), now)
	if unknown.State != "unknown" || !strings.Contains(unknown.Detail, "403") || unknown.Queued[0].Verdict != "unknown" || unknown.QueuedError == "" || !strings.Contains(unknown.WriterRun, "unknown") {
		t.Fatalf("%+v", unknown)
	}
	partial := buildRunnerStatus(c, runnerFacts{Partial: true}, nil, nil, nil, time.Time{}, nil, now)
	if partial.State != "partial" || partial.WriterRun != "never" || partial.Queued == nil || partial.Runners == nil {
		t.Fatalf("%+v", partial)
	}
	c.Workflows.RunnerStart = "x"
	if got := buildRunnerStatus(c, callaRunners, nil, nil, nil, now, nil, now); got.RunnerStart != "set" {
		t.Fatalf("the report says it is set, not what it runs: %+v", got)
	}
}

func TestParseQueuedJobsKeepsOnlyTheWaiting(t *testing.T) {
	jobs, err := parseQueuedJobs([]byte(`{"jobs":[{"name":"a","status":"queued","html_url":"u","labels":["self-hosted"]},{"name":"b","status":"in_progress"},{"name":"c","status":"completed"}]}`))
	if err != nil || len(jobs) != 1 || jobs[0].Name != "a" || jobs[0].URL != "u" || jobs[0].Labels[0] != "self-hosted" {
		t.Fatalf("%v %+v", err, jobs)
	}
}

func TestRunnerSetupSteps(t *testing.T) {
	jobs, err := parseWorkflowJobs([]byte(callaCI))
	if err != nil {
		t.Fatal(err)
	}
	c := model.Config{Owner: "o", Repository: "r", RequiredChecks: []string{"fmt", "test-linux", "Site build"}}
	c.Workflows.QuipuVersion = "v0.3.0"
	c.Workflows.RunsOn = map[string][]string{"default": {"self-hosted", "macOS", "calla-mac"}}
	setup := planRunnerSetup(c, jobs)
	if len(setup.Labels) != 2 {
		t.Fatalf("each distinct self-hosted set once, no hosted ones: %v", setup.Labels)
	}
	text := strings.Join(setup.Steps, "\n")
	for _, want := range []string{"--labels self-hosted,macOS,calla-mac", "--labels self-hosted,calla-linux", "./svc.sh install", "quipu@v0.3.0", ".path", "runner_start", "quipu doctor"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	hosted := planRunnerSetup(model.Config{}, nil)
	if len(hosted.Labels) != 0 || !strings.Contains(hosted.Steps[0], "no self-hosted") {
		t.Fatalf("%+v", hosted)
	}
}

func TestDoctorChecksWhatSetupAsksFor(t *testing.T) {
	pinned := model.Config{}
	pinned.Workflows.QuipuVersion = "v0.3.0"
	if got := runnerConfigChecks(pinned, "v0.2.0"); len(got) != 1 || got[0].Status != "warn" || !strings.Contains(got[0].Detail, "v0.2.0") {
		t.Fatalf("%+v", got)
	}
	if got := runnerConfigChecks(pinned, "v0.3.0"); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	if got := runnerConfigChecks(model.Config{}, "v0.2.0"); len(got) != 0 {
		t.Fatalf("no pin, nothing to compare: %+v", got)
	}
	if got := runnerConfigChecks(pinned, ""); len(got) != 0 {
		t.Fatalf("no quipu on the PATH is its own doctor check: %+v", got)
	}
	queue := model.Config{Merge: "agent", MergeWait: "background"}
	queue.Workflows.Schedule = "off"
	if got := runnerConfigChecks(queue, ""); len(got) != 1 || !strings.Contains(got[0].Detail, "queued merge") {
		t.Fatalf("%+v", got)
	}
}

func TestWakeWorkflowsNameTheRequiredChecks(t *testing.T) {
	jobs, err := parseWorkflowJobs([]byte(callaCI))
	if err != nil {
		t.Fatal(err)
	}
	got := wakeWorkflows(model.Config{RequiredChecks: []string{"fmt", "test-linux"}}, jobs)
	if strings.Join(got, ",") != "Quipu policy,Core" {
		t.Fatalf("%v", got)
	}
	both := []workflowJob{{ID: "test", Workflow: "One"}, {ID: "test", Workflow: "Two", Labels: []string{"ubuntu-latest"}}}
	if got := wakeWorkflows(model.Config{RequiredChecks: []string{"test"}}, both); strings.Join(got, ",") != "Quipu policy,One,Two" {
		t.Fatalf("either may be the one that runs: %v", got)
	}
	if got := wakeWorkflows(model.Config{RequiredChecks: []string{"gone"}}, both); strings.Join(got, ",") != "Quipu policy" {
		t.Fatalf("%v", got)
	}
}

func TestJobsAfterInstallReadsWhatInstallLeaves(t *testing.T) {
	top := t.TempDir()
	writeRepoFile(t, top, ".github/workflows/ci.yml", []byte("name: Build\njobs:\n  test:\n    runs-on: ubuntu-latest\n"))
	writeRepoFile(t, top, ".github/workflows/nameless.yml", []byte("jobs:\n  lint:\n    runs-on: ubuntu-latest\n"))
	rendered := map[string]string{
		".github/workflows/ci.yml":     "name: CI\njobs:\n  test:\n    runs-on: ubuntu-latest\n",
		".github/workflows/fresh.yml":  "name: Fresh\njobs:\n  new:\n    runs-on: ubuntu-latest\n",
		".github/ISSUE_TEMPLATE/x.yml": "jobs:\n  form:\n    runs-on: ubuntu-latest\n",
	}
	c := model.Config{RequiredChecks: []string{"test", "lint", "new", "form"}}
	kept := wakeWorkflows(c, jobsAfterInstall(top, rendered, false))
	if strings.Join(kept, ",") != "Quipu policy,Build,.github/workflows/nameless.yml,Fresh" {
		t.Fatalf("without replace an existing file stays: %v", kept)
	}
	replaced := wakeWorkflows(c, jobsAfterInstall(top, rendered, true))
	if strings.Join(replaced, ",") != "Quipu policy,CI,.github/workflows/nameless.yml,Fresh" {
		t.Fatalf("with replace the generated file wins: %v", replaced)
	}
}

func TestWakingTheWriterFilesNoDefects(t *testing.T) {
	c := model.Config{DefaultBranch: "main"}
	if reason := defectSkip(c, runFacts{Name: "CI", Conclusion: "failure", HeadBranch: "007/work"}); reason == "" {
		t.Fatal("a failure on a work branch files no Defect, now that it can wake the Writer")
	}
	if reason := defectSkip(c, runFacts{Name: "Quipu policy", Conclusion: "failure", HeadBranch: "main"}); reason == "" {
		t.Fatal("a workflow outside defects_from files no Defect")
	}
}

func TestGuardAllowsOnlyTheRunnerReads(t *testing.T) {
	f := newGuardFixture(t)
	for _, command := range []string{"quipu runner status --json", "quipu runner setup"} {
		if code := guardCode(t, f.b, "Bash", map[string]any{"command": command}); code != "" {
			t.Errorf("%q denied with %s", command, code)
		}
	}
	for _, command := range []string{"quipu runner start", "quipu runner"} {
		if code := guardCode(t, f.b, "Bash", map[string]any{"command": command}); code != "quipu-admin" {
			t.Errorf("%q: %q", command, code)
		}
	}
}

func TestStatusReportsPullRequestsWithoutWaiting(t *testing.T) {
	item := statusItem{Number: 7, Status: "In review", LeaseHolder: "claude/o/1", Branch: "007/work"}
	c := model.Config{Merge: "agent", MergeWait: "background"}
	required := []string{"test", "policy"}
	green := mergeView{URL: "url", State: "OPEN", Checks: passing("test", "policy")}
	red := mergeView{URL: "url", State: "OPEN", Checks: append(passing("policy"), prCheck{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"})}
	running := mergeView{URL: "url", State: "OPEN", Checks: append(passing("policy"), prCheck{Name: "test", Status: "IN_PROGRESS"})}
	for name, tc := range map[string]struct {
		c     model.Config
		view  mergeView
		queue queueState
		gate  error
		want  string
		owner string
	}{
		"green, agents merge": {c, green, queueState{}, nil, "Merge it: quipu done 7", ""},
		"green, owner merges": {model.Config{}, green, queueState{}, nil, "owner reviews", ""},
		"failed":              {c, red, queueState{}, mergeGate(red.Checks, required), "Fix the cause", ""},
		"running":             {c, running, queueState{}, mergeGate(running.Checks, required), "no need to wait", ""},
		"not created yet":     {c, mergeView{URL: "url", State: "OPEN"}, queueState{}, mergeGate(nil, required), "no need to wait", ""},
		"no runner":           {c, running, queueState{}, flowErr("checks-no-runner", "required checks cannot start: test"), "Do not poll", "cannot start"},
		"queued":              {c, running, queueState{State: "queued"}, mergeGate(running.Checks, required), "Nothing to do", ""},
		"queued and failed":   {c, red, queueState{State: "queued"}, mergeGate(red.Checks, required), "Fix the cause", ""},
		"queue rejected":      {c, running, queueState{State: "rejected", Detail: "stale"}, nil, "rejected the queued merge: stale", ""},
		"queue stopped":       {c, running, queueState{State: "stopped", Detail: "head moved"}, nil, "stopped: head moved", ""},
		"merged, no evidence": {c, mergeView{URL: "url", State: "MERGED"}, queueState{}, nil, "--actor claude/o/1", ""},
		"closed":              {c, mergeView{URL: "url", State: "CLOSED"}, queueState{}, nil, "Open a new PR from 007/work", ""},
	} {
		got := describePull(tc.c, item, tc.view, tc.queue, tc.gate)
		if !strings.Contains(got.Next, tc.want) || got.PR != "url" || got.Holder != "claude/o/1" {
			t.Errorf("%s: %+v", name, got)
		}
		if (tc.owner == "") != (got.owner == "") || !strings.Contains(got.owner, tc.owner) {
			t.Errorf("%s: owner reason %q", name, got.owner)
		}
	}
	report := describePull(c, item, red, queueState{}, mergeGate(red.Checks, required))
	results := map[string]string{}
	for _, check := range report.Checks {
		results[check.Name] = check.Result
	}
	if results["test"] != "FAILURE" || results["policy"] != "SUCCESS" {
		t.Fatalf("each check's result is reported: %+v", report.Checks)
	}
	if got := describePull(c, item, running, queueState{}, mergeGate(running.Checks, required)); got.Checks[1].Result != "IN_PROGRESS" {
		t.Fatalf("a running check says so: %+v", got.Checks)
	}
	early := item
	early.Status = "In progress"
	for name, tc := range map[string]struct {
		c    model.Config
		view mergeView
		gate error
		want string
	}{
		"green, owner merges":   {model.Config{}, green, nil, "Move it there: quipu done 7 --json"},
		"running, owner merges": {model.Config{}, running, mergeGate(running.Checks, required), "Move it there: quipu done 7 --json"},
		"failed, owner merges":  {model.Config{}, red, mergeGate(red.Checks, required), "Fix the cause"},
		"merged early":          {c, mergeView{URL: "url", State: "MERGED"}, nil, "Tell the owner"},
		"running, agents merge": {c, running, mergeGate(running.Checks, required), "no need to wait"},
	} {
		if got := describePull(tc.c, early, tc.view, queueState{}, tc.gate); !strings.Contains(got.Next, tc.want) {
			t.Errorf("not in review yet, %s: %+v", name, got)
		}
	}
	status := buildStatusReport("o/r", nil, time.Now())
	if status.PullRequests == nil {
		t.Fatal("an empty list, for JSON")
	}
}

func TestPullsReplaceWhatTheOwnerQueueSaidOfThem(t *testing.T) {
	queue := []ownerItem{{Number: 3, Reason: "review and merge its PR"}, {Number: 5, Reason: "review and merge its PR"}, {Number: 9, Reason: "review and merge its PR"}}
	pulls := []pullReport{
		{Issue: 3, State: "OPEN", owner: "a required check cannot start; start its runner"},
		{Issue: 5, State: "MERGED"},
		{Issue: 7, State: "MERGED", owner: "PR merged before it reached review"},
		{Issue: 9, State: "OPEN"},
	}
	got, attention := addPullsToQueues(queue, nil, pulls, map[int]string{7: "seven"})
	want := []ownerItem{
		{Number: 3, Reason: "a required check cannot start; start its runner"},
		{Number: 7, Title: "seven", Reason: "PR merged before it reached review"},
		{Number: 9, Reason: "review and merge its PR"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%+v", got)
	}
	if len(attention) != 3 || !strings.Contains(attention[1], "#5 PR merged with no evidence") {
		t.Fatalf("%v", attention)
	}
}

func TestQueueStateOfReadsTheReceipts(t *testing.T) {
	request := func(id string, action string) github.Comment {
		body, err := writer.RenderRequest(writer.Request{ID: id, Action: action, IssueID: "I", Actor: "a", ExpectedFingerprint: "f"})
		if err != nil {
			t.Fatal(err)
		}
		return github.Comment{Body: body}
	}
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if got := queueStateOf(nil, nil); got.State != "" {
		t.Fatalf("%+v", got)
	}
	// Posted, and the Writer has not answered.
	if got := queueStateOf([]github.Comment{request("q", "merge.queue")}, nil); got.State != "queued" {
		t.Fatalf("%+v", got)
	}
	rejected := []writer.Receipt{{RequestID: "q", Result: "rejected", Detail: "stale request", At: at}}
	if got := queueStateOf([]github.Comment{request("q", "merge.queue")}, rejected); got.State != "rejected" || got.Detail != "stale request" {
		t.Fatalf("%+v", got)
	}
	accepted := writer.Receipt{RequestID: "q", Result: "accepted", At: at, Merge: &writer.QueuedMerge{PR: "url", Head: "h"}}
	if got := queueStateOf([]github.Comment{request("q", "merge.queue")}, []writer.Receipt{accepted}); got.State != "queued" {
		t.Fatalf("%+v", got)
	}
	ended := []writer.Receipt{accepted, {RequestID: writer.MergeResultID(accepted), Result: "accepted", Detail: "required checks not passing: test", At: at}}
	if got := queueStateOf([]github.Comment{request("q", "merge.queue")}, ended); got.State != "stopped" || !strings.Contains(got.Detail, "not passing") {
		t.Fatalf("%+v", got)
	}
	// Queueing again after a rejection is what counts.
	again := []writer.Receipt{{RequestID: "q", Result: "rejected", Detail: "stale", At: at}, {RequestID: "q2", Result: "accepted", At: at, Merge: &writer.QueuedMerge{PR: "url", Head: "h"}}}
	if got := queueStateOf([]github.Comment{request("q", "merge.queue"), request("q2", "merge.queue")}, again); got.State != "queued" {
		t.Fatalf("%+v", got)
	}
	// A result or a request from an earlier claim says nothing about this one.
	claimed := at.Add(time.Hour)
	stale := []writer.Receipt{{RequestID: writer.MergeResultID(accepted), Result: "accepted", Detail: "old", At: at}, {RequestID: "c", Result: "accepted", At: claimed, State: &writer.ReceiptState{Status: "Claimed"}}}
	if got := queueStateOf(nil, stale); got.State != "" {
		t.Fatalf("%+v", got)
	}
	old := request("q", "merge.queue")
	old.CreatedAt = at.Format(time.RFC3339)
	lastClaim := []writer.Receipt{{RequestID: "q", Result: "rejected", Detail: "stale", At: at}, {RequestID: "c", Result: "accepted", At: claimed, State: &writer.ReceiptState{Status: "Claimed"}}}
	if got := queueStateOf([]github.Comment{old}, lastClaim); got.State != "" {
		t.Fatalf("an old rejection must not hide what this claim is doing: %+v", got)
	}
	current := request("q", "merge.queue")
	current.CreatedAt = claimed.Add(time.Minute).Format(time.RFC3339)
	if got := queueStateOf([]github.Comment{current}, lastClaim[1:]); got.State != "queued" {
		t.Fatalf("%+v", got)
	}
}

func TestRunnerWarningPrintsOncePerWorktreeEverySixHours(t *testing.T) {
	w := worktree{GitDir: t.TempDir()}
	down := runnerStarter{down: func() []doctorCheck { return []doctorCheck{downCheck} }, command: func() string { return "" }, claim: won, run: func(string) error { return nil }}
	start := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	var stderr strings.Builder
	// Two `quipu start` calls within six hours: the warning is printed once,
	// and the runner field carries it both times.
	for i, now := range []time.Time{start, start.Add(5 * time.Hour)} {
		note := announceRunner(down, w, now, &stderr)
		raw, err := json.Marshal(flowResult{Issue: 7, Status: "In progress", Runner: note})
		if err != nil || !strings.Contains(string(raw), `"runner":"needed runners are down`) {
			t.Fatalf("call %d: runner field missing: %s", i+1, raw)
		}
	}
	if got := strings.Count(stderr.String(), "needed runners are down"); got != 1 {
		t.Fatalf("warned %d times within six hours: %q", got, stderr.String())
	}
	announceRunner(down, w, start.Add(7*time.Hour), &stderr)
	if got := strings.Count(stderr.String(), "needed runners are down"); got != 2 {
		t.Fatalf("after six hours it is due again; warned %d times", got)
	}
	// Another worktree has its own marker.
	announceRunner(down, worktree{GitDir: t.TempDir()}, start.Add(7*time.Hour), &stderr)
	if got := strings.Count(stderr.String(), "needed runners are down"); got != 3 {
		t.Fatalf("another worktree hears it too; warned %d times", got)
	}
	// Nothing down: nothing printed, no field.
	quiet := runnerStarter{down: func() []doctorCheck { return nil }}
	var none strings.Builder
	if note := announceRunner(quiet, worktree{GitDir: t.TempDir()}, start, &none); note != "" || none.Len() != 0 {
		t.Fatalf("note %q stderr %q", note, none.String())
	}
}
