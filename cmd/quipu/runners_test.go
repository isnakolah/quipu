package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
)

var callaRunners = runnerFacts{Runners: []repoRunner{
	{Name: "calla-mac", Online: true, Labels: []string{"self-hosted", "macOS", "ARM64", "calla-mac"}},
	{Name: "old-box", Online: false, Labels: []string{"self-hosted", "Linux", "old-box"}},
}}

func TestJudgeRunners(t *testing.T) {
	for name, tc := range map[string]struct {
		labels []string
		facts  runnerFacts
		want   runnerVerdict
		detail string
	}{
		"online":          {[]string{"self-hosted", "calla-mac"}, callaRunners, runnerAvailable, "calla-mac is online"},
		"label case":      {[]string{"Self-Hosted", "MACOS"}, callaRunners, runnerAvailable, "calla-mac"},
		"offline":         {[]string{"self-hosted", "old-box"}, callaRunners, runnerOffline, "old-box is offline"},
		"missing":         {[]string{"self-hosted", "calla-linux"}, callaRunners, runnerMissing, "no runner has calla-linux"},
		"split labels":    {[]string{"calla-mac", "old-box"}, callaRunners, runnerMissing, "no single runner"},
		"hosted":          {[]string{"ubuntu-latest"}, runnerFacts{}, runnerAvailable, "GitHub-hosted"},
		"hosted, no read": {[]string{"macos-14"}, runnerFacts{Err: errors.New("403")}, runnerAvailable, "GitHub-hosted"},
		"cannot read":     {[]string{"self-hosted", "calla-linux"}, runnerFacts{Err: errors.New("403")}, runnerUnknown, "runner state unknown"},
		"org not visible": {[]string{"self-hosted", "calla-linux"}, runnerFacts{Partial: true}, runnerUnknown, "organization runners"},
		"org, online":     {[]string{"calla-mac"}, runnerFacts{Partial: true, Runners: callaRunners.Runners}, runnerAvailable, "online"},
		"no labels":       {nil, callaRunners, runnerUnknown, "unknown"},
	} {
		got, detail := judgeRunners(tc.labels, tc.facts)
		if got != tc.want || !strings.Contains(detail, tc.detail) {
			t.Errorf("%s: verdict %d, detail %q", name, got, detail)
		}
	}
}

func TestExplainPending(t *testing.T) {
	required := []string{"test-linux", "fmt", "policy"}
	checks := []prCheck{
		{Name: "test-linux", Status: "QUEUED", DetailsURL: "linux"},
		{Name: "fmt", Status: "COMPLETED", Conclusion: "SUCCESS", DetailsURL: "fmt"},
		{Name: "policy", Status: "IN_PROGRESS", DetailsURL: "policy"},
		{Name: "not-required", Status: "QUEUED", DetailsURL: "other"},
	}
	gate := mergeGate(checks, required)
	jobs := map[string]jobFacts{
		"linux":  {Queued: true, Labels: []string{"self-hosted", "calla-linux"}},
		"policy": {Labels: []string{"self-hosted", "calla-mac"}},
	}
	lookup := func(url string) (jobFacts, error) {
		job, ok := jobs[url]
		if !ok {
			t.Fatalf("looked up %q, which is not a pending required check", url)
		}
		return job, nil
	}
	code := func(err error) (string, string) {
		var coded *flowError
		if !errors.As(err, &coded) {
			t.Fatalf("not coded: %v", err)
		}
		return coded.Code, coded.Message
	}

	got, message := code(explainPending(gate, checks, required, func() runnerFacts { return callaRunners }, lookup))
	if got != "checks-no-runner" || !strings.Contains(message, "test-linux") || !strings.Contains(message, "calla-linux") {
		t.Fatalf("%s: %s", got, message)
	}
	if strings.Contains(message, "policy") {
		t.Fatalf("policy is running, not stuck: %s", message)
	}

	// A queued job with an online runner is only waiting its turn.
	jobs["linux"] = jobFacts{Queued: true, Labels: []string{"self-hosted", "calla-mac"}}
	if got, _ := code(explainPending(gate, checks, required, func() runnerFacts { return callaRunners }, lookup)); got != "checks-pending" {
		t.Fatalf("online runner: %s", got)
	}

	// A running job is not stuck even if its runner has since gone offline.
	jobs["linux"] = jobFacts{Labels: []string{"self-hosted", "old-box"}}
	if got, _ := code(explainPending(gate, checks, required, func() runnerFacts { return callaRunners }, lookup)); got != "checks-pending" {
		t.Fatalf("running: %s", got)
	}

	// Unknown state keeps today's code and says so.
	jobs["linux"] = jobFacts{Queued: true, Labels: []string{"self-hosted", "calla-linux"}}
	got, message = code(explainPending(gate, checks, required, func() runnerFacts { return runnerFacts{Err: errors.New("403")} }, lookup))
	if got != "checks-pending" || !strings.Contains(message, "still running") || !strings.Contains(message, "runner state unknown for test-linux") {
		t.Fatalf("%s: %s", got, message)
	}
	got, message = code(explainPending(gate, checks, required, func() runnerFacts { return callaRunners }, func(string) (jobFacts, error) { return jobFacts{}, errors.New("not Actions") }))
	if got != "checks-pending" || !strings.Contains(message, "runner state unknown") {
		t.Fatalf("job unreadable: %s: %s", got, message)
	}
}

func TestExplainPendingLeavesOtherFailuresAlone(t *testing.T) {
	never := func() runnerFacts { t.Fatal("no lookup needed"); return runnerFacts{} }
	noJob := func(string) (jobFacts, error) { t.Fatal("no lookup needed"); return jobFacts{}, nil }
	failed := mergeGate([]prCheck{{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"}, {Name: "policy", Status: "QUEUED"}}, []string{"test", "policy"})
	if got := explainPending(failed, nil, []string{"test", "policy"}, never, noJob); got != failed {
		t.Fatalf("a failed check must stay checks-failing: %v", got)
	}
	if got := explainPending(nil, nil, nil, never, noJob); got != nil {
		t.Fatalf("green stays green: %v", got)
	}
}

func TestNoRunnerCodeIsDocumented(t *testing.T) {
	e := flowErr("checks-no-runner", "x")
	if !strings.Contains(e.Next, "do not poll") || !strings.Contains(renderGuide(), "`checks-no-runner`") {
		t.Fatalf("next %q, or the guide lacks the code", e.Next)
	}
}

func TestDescribeWriter(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	labels := []string{"self-hosted", "old-box"}
	text := describeWriter(now.Add(-3*time.Hour), nil, labels, callaRunners, now)
	for _, want := range []string{"stays queued", "3h0m0s ago", "old-box is offline"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if text := describeWriter(time.Time{}, nil, []string{"ubuntu-latest"}, runnerFacts{}, now); !strings.Contains(text, "never run") || !strings.Contains(text, "GitHub-hosted") {
		t.Fatal(text)
	}
	text = describeWriter(time.Time{}, errors.New("403"), labels, runnerFacts{Err: errors.New("403")}, now)
	if !strings.Contains(text, "last run is unknown") || !strings.Contains(text, "runner state unknown") {
		t.Fatal(text)
	}
}

const callaCI = `
name: Core
on: pull_request
jobs:
  fmt:
    runs-on: [self-hosted, macOS, calla-mac]
    steps: []
  test-linux:
    runs-on: [self-hosted, calla-linux]
    steps: []
  site:
    name: Site build
    runs-on: ubuntu-latest
    steps: []
  matrix:
    name: desktop
    runs-on: ${{ matrix.os }}
    steps: []
  grouped:
    runs-on: {group: big}
    steps: []
`

func TestDoctorFailsOnRequiredCheckLabelsOnlyInAWorkflow(t *testing.T) {
	jobs, err := parseWorkflowJobs([]byte(callaCI))
	if err != nil {
		t.Fatal(err)
	}
	c := model.Config{RequiredChecks: []string{"fmt", "test-linux", "Site build", "desktop (ubuntu)", "grouped", "external"}}
	c.Workflows.RunsOn = map[string][]string{"default": {"self-hosted", "macOS", "calla-mac"}, "nightly": {"self-hosted", "old-box"}}
	statuses := map[string]doctorCheck{}
	for _, check := range judgeRunnerNeeds(c, jobs, func() runnerFacts { return callaRunners }) {
		statuses[check.Name] = check
	}
	for name, want := range map[string]string{
		"runner: Writer":                    "ok",
		"runner: policy":                    "ok",
		"runner: workflows.runs_on.nightly": "fail",
		"runner: check fmt":                 "ok",
		"runner: check test-linux":          "fail",
		"runner: check Site build":          "ok",
		"runner: check desktop (ubuntu)":    "warn",
		"runner: check grouped":             "warn",
		"runner: check external":            "warn",
	} {
		if statuses[name].Status != want {
			t.Errorf("%s: want %s, got %+v", name, want, statuses[name])
		}
	}
	if got := statuses["runner: check test-linux"]; !strings.Contains(got.Detail, "no runner has calla-linux") || got.Fix == "" {
		t.Fatalf("the failure must name the label and the fix: %+v", got)
	}
}

func TestDoctorWarnsWhenRunnersCannotBeRead(t *testing.T) {
	c := model.Config{}
	c.Workflows.RunsOn = map[string][]string{"default": {"self-hosted", "calla-mac"}}
	jobs := []workflowJob{{ID: "test", Labels: []string{"self-hosted", "calla-mac"}}}
	for _, check := range judgeRunnerNeeds(c, jobs, func() runnerFacts { return runnerFacts{Err: errors.New("403")} }) {
		if check.Status != "warn" || !strings.Contains(check.Detail, "runner state unknown") {
			t.Errorf("unknown runners must warn, not fail: %+v", check)
		}
	}
}

func TestDoctorSkipsRunnersForHostedJobs(t *testing.T) {
	jobs := []workflowJob{{ID: "test", Labels: []string{"ubuntu-latest"}}}
	never := func() runnerFacts { t.Fatal("hosted jobs need no runner lookup"); return runnerFacts{} }
	for _, check := range judgeRunnerNeeds(model.Config{}, jobs, never) {
		if check.Status != "ok" {
			t.Errorf("%+v", check)
		}
	}
}

func TestKeepsLeaseWithOpenPR(t *testing.T) {
	if !keepsLease(true, nil) {
		t.Fatal("an open PR keeps the lease")
	}
	if !keepsLease(false, errors.New("gh down")) {
		t.Fatal("a failed lookup must not hand the work to someone else")
	}
	if keepsLease(false, nil) {
		t.Fatal("no open PR: abandoned work must stay recoverable")
	}
	if open, err := branchHasOpenPR(model.Config{}, ""); open || err != nil {
		t.Fatalf("work with no branch has no PR: %v %v", open, err)
	}
}

func TestReclaimKeepsWorkWithAnOpenPR(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	expired := func() *liveWork {
		return &liveWork{Status: "In progress", Holder: "a", Branch: "007/work", Expiry: "2026-11-02T11:00:00Z"}
	}
	for name, tc := range map[string]struct {
		open      bool
		err       error
		reclaimed int
		kept      int
	}{
		"open PR":       {open: true, kept: 1},
		"lookup failed": {err: errors.New("gh down"), kept: 1},
		"no PR":         {reclaimed: 1},
	} {
		w := &watch{now: now, openPR: func(branch string) (bool, error) {
			if branch != "007/work" {
				t.Fatalf("looked up %q", branch)
			}
			return tc.open, tc.err
		}}
		state := expired()
		if _, err := w.reclaim(state); err != nil {
			t.Fatal(err)
		}
		if w.report.Reclaimed != tc.reclaimed || w.report.Kept != tc.kept {
			t.Errorf("%s: %+v", name, w.report)
		}
		if state.Holder != "a" {
			t.Errorf("%s: a dry run changed the holder to %q", name, state.Holder)
		}
	}
	// A live lease is not even looked up.
	live := &watch{now: now, openPR: func(string) (bool, error) { t.Fatal("no lookup for a live lease"); return false, nil }}
	state := expired()
	state.Expiry = "2026-11-02T13:00:00Z"
	if _, err := live.reclaim(state); err != nil || live.report.Reclaimed != 0 {
		t.Fatalf("%v %+v", err, live.report)
	}
}

func TestOwnerQueue(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	items := []statusItem{
		{Number: 1, Kind: "Epic", ChildTotal: 2, ChildCompleted: 2},
		{Number: 2, Kind: "Epic", ChildTotal: 2, ChildCompleted: 1},
		{Number: 3, Kind: "Decision", Status: "Ready"},
		{Number: 4, Kind: "Gate", Status: "Backlog"},
		{Number: 5, Kind: "Story", Status: "In review"},
		{Number: 6, Kind: "Story", Status: "Blocked"},
		{Number: 7, Kind: "Story", Status: "In progress", LeaseHolder: "a", LeaseExpires: "2026-11-02T11:00:00Z"},
		{Number: 8, Kind: "Story", Status: "In progress", LeaseHolder: "a", LeaseExpires: "2026-11-02T13:00:00Z"},
		{Number: 9, Kind: "Task", Status: "Ready"},
		{Number: 10, Kind: "Task", NeedsTriage: true},
		{Number: 11, Kind: "Decision", NeedsTriage: true},
	}
	reasons := map[int]string{}
	for _, item := range ownerQueue(items, false, now) {
		reasons[item.Number] = item.Reason
	}
	for number, want := range map[int]string{1: "close the goal", 3: "decides a Decision", 4: "decides a Gate", 5: "review and merge", 6: "blocked", 7: "close the PR to release", 10: "triage", 11: "triage"} {
		if !strings.Contains(reasons[number], want) {
			t.Errorf("#%d: want %q, got %q", number, want, reasons[number])
		}
	}
	for _, number := range []int{2, 8, 9} {
		if reason, queued := reasons[number]; queued {
			t.Errorf("#%d does not wait on the owner: %q", number, reason)
		}
	}
	for _, item := range ownerQueue(items, true, now) {
		if item.Number == 5 {
			t.Error("with merge: agent, review does not wait on the owner")
		}
	}

	report := buildStatusReport("o/r", items, now)
	if report.OwnerQueue == nil || len(report.OwnerQueue) != 0 {
		t.Fatalf("the queue starts as an empty list, for JSON: %#v", report.OwnerQueue)
	}
	report.OwnerQueue = ownerQueue(items, false, now)
	if output := renderStatus(report, now); !strings.Contains(output, "Waiting on the owner (8)") || !strings.Contains(output, "#10 #11") {
		t.Fatalf("render:\n%s", output)
	}
}
