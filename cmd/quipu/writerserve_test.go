package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

// serveFixture is the loop on a fake clock. requests are what each poll finds; once
// they run out, polls find nothing.
type serveFixture struct {
	now      time.Time
	polls    [][]int
	pollErr  error
	handled  []int
	failOn   map[int]int
	sweeps   []bool
	waiting  bool
	low      string
	sweepErr error
	// handleTakes is how long each Issue takes.
	handleTakes time.Duration
	dispatched  int
	polled      int
	onPoll      func(f *serveFixture)
}

func (f *serveFixture) ops() serveOps {
	return serveOps{
		poll: func() ([]int, error) {
			f.polled++
			if f.onPoll != nil {
				f.onPoll(f)
			}
			if f.pollErr != nil {
				return nil, f.pollErr
			}
			if len(f.polls) == 0 {
				return nil, nil
			}
			next := f.polls[0]
			f.polls = f.polls[1:]
			return next, nil
		},
		sweep: func(first bool) error {
			f.sweeps = append(f.sweeps, first)
			return f.sweepErr
		},
		handle: func(number int) error {
			f.now = f.now.Add(f.handleTakes)
			if f.failOn[number] > 0 {
				f.failOn[number]--
				return errors.New("GitHub said no")
			}
			f.handled = append(f.handled, number)
			return nil
		},
		yield:    func() (bool, error) { return f.waiting, nil },
		rate:     func() (string, error) { return f.low, nil },
		dispatch: func() error { f.dispatched++; return nil },
		now:      func() time.Time { return f.now },
		sleep:    func(d time.Duration) { f.now = f.now.Add(d) },
	}
}

func newServeFixture() *serveFixture {
	return &serveFixture{now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC), failOn: map[int]int{}}
}

func TestServeAppliesInOrderThenStopsWhenIdle(t *testing.T) {
	f := newServeFixture()
	f.polls = [][]int{{2, 1}, nil, {3}}
	result, err := serve(f.ops())
	if err != nil || result.Reason != serveStopIdle {
		t.Fatalf("result = %+v, err %v", result, err)
	}
	if fmt.Sprint(f.handled) != "[2 1 3]" {
		t.Fatalf("handled %v, want comment order", f.handled)
	}
	if fmt.Sprint(f.sweeps) != "[true]" || f.dispatched != 0 {
		t.Fatalf("sweeps %v, dispatched %d", f.sweeps, f.dispatched)
	}
}

func TestServeFinalPollCatchesARequestBeforeIdling(t *testing.T) {
	f := newServeFixture()
	f.onPoll = func(f *serveFixture) {
		// The first poll at or past the idle limit is the final one.
		if f.now.Sub(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)) >= serveIdle && len(f.handled) == 0 {
			f.polls = [][]int{{7}}
		}
	}
	result, err := serve(f.ops())
	if err != nil || result.Reason != serveStopIdle || fmt.Sprint(f.handled) != "[7]" {
		t.Fatalf("result = %+v, err %v, handled %v", result, err, f.handled)
	}
}

func TestServeSweepsEveryFiveMinutesWhileBusy(t *testing.T) {
	f := newServeFixture()
	// Work keeps coming, so idling never ends the run.
	f.onPoll = func(f *serveFixture) { f.polls = [][]int{{1}} }
	result, err := serve(f.ops())
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != serveStopLimit {
		t.Fatalf("reason %q", result.Reason)
	}
	if len(f.sweeps) < 2 || !f.sweeps[0] || f.sweeps[1] {
		t.Fatalf("sweeps %v: want the first, then at least one later", f.sweeps)
	}
}

func TestServeStopsAtTheLimitAfterTheCurrentIssue(t *testing.T) {
	f := newServeFixture()
	f.handleTakes = serveLimit
	f.polls = [][]int{{1, 2}}
	result, err := serve(f.ops())
	if err != nil || result.Reason != serveStopLimit {
		t.Fatalf("result = %+v, err %v", result, err)
	}
	if fmt.Sprint(f.handled) != "[1]" || f.dispatched != 1 || !result.Dispatched {
		t.Fatalf("handled %v, dispatched %d: want the Issue in hand finished and another run for the rest", f.handled, f.dispatched)
	}
}

func TestServeDispatchesOnlyWhenWorkRemains(t *testing.T) {
	f := newServeFixture()
	f.handleTakes = serveLimit
	f.polls = [][]int{{1}}
	if result, err := serve(f.ops()); err != nil || result.Reason != serveStopLimit || f.dispatched != 0 {
		t.Fatalf("result = %+v, err %v, dispatched %d", result, err, f.dispatched)
	}
}

func TestServeLeavesTheRunnerForAWaitingBuild(t *testing.T) {
	f := newServeFixture()
	f.polls = [][]int{{1, 2}}
	handled := 0
	ops := f.ops()
	handle := ops.handle
	ops.handle = func(number int) error {
		handled++
		f.waiting = true
		f.now = f.now.Add(serveYieldEvery)
		return handle(number)
	}
	result, err := serve(ops)
	if err != nil || result.Reason != serveStopYield {
		t.Fatalf("result = %+v, err %v", result, err)
	}
	if handled != 1 || f.dispatched != 1 {
		t.Fatalf("handled %d, dispatched %d", handled, f.dispatched)
	}
}

func TestServeFailsOnALowRateLimit(t *testing.T) {
	f := newServeFixture()
	f.low = "only 10 GraphQL points left this hour"
	result, err := serve(f.ops())
	if err == nil || !strings.Contains(err.Error(), "GraphQL") || result.Reason != "low rate limit" {
		t.Fatalf("result = %+v, err %v", result, err)
	}
}

func TestServeFailsOnRepeatedErrorsButNotOneBadIssue(t *testing.T) {
	f := newServeFixture()
	f.pollErr = errors.New("502")
	result, err := serve(f.ops())
	if err == nil || result.Reason != "repeated GitHub errors" {
		t.Fatalf("result = %+v, err %v", result, err)
	}

	f = newServeFixture()
	f.polls = [][]int{{1, 2}}
	f.failOn[1] = 2
	result, err = serve(f.ops())
	if err != nil || result.Reason != serveStopIdle {
		t.Fatalf("result = %+v, err %v", result, err)
	}
	if fmt.Sprint(f.handled) != "[1 2]" || len(result.Errors) != 2 {
		t.Fatalf("handled %v, errors %v: a failing Issue retries in its turn", f.handled, result.Errors)
	}
}

func TestNewRequestsAreOrderedAndSeenOnce(t *testing.T) {
	seen := map[int64]bool{}
	body := writer.RequestPrefix + " id=1 -->"
	comments := []issueComment{
		{ID: 30, Body: body, IssueURL: "https://api.github.com/repos/o/r/issues/2", CreatedAt: "2026-11-03T12:00:03Z"},
		{ID: 10, Body: body, IssueURL: "https://api.github.com/repos/o/r/issues/1", CreatedAt: "2026-11-03T12:00:01Z"},
		{ID: 20, Body: "a person talking", IssueURL: "https://api.github.com/repos/o/r/issues/9", CreatedAt: "2026-11-03T12:00:02Z"},
		{ID: 25, Body: body, IssueURL: "https://api.github.com/repos/o/r/issues/8", HTMLURL: "https://github.com/o/r/pull/8#issuecomment-25", CreatedAt: "2026-11-03T12:00:02Z"},
		{ID: 35, Body: body, IssueURL: "https://api.github.com/repos/o/r/issues/1", CreatedAt: "2026-11-03T12:00:04Z"},
	}
	numbers, newest := newRequests(comments, seen)
	if fmt.Sprint(numbers) != "[1 2]" {
		t.Fatalf("numbers = %v", numbers)
	}
	if !newest.Equal(time.Date(2026, 11, 3, 12, 0, 4, 0, time.UTC)) {
		t.Fatalf("newest = %v", newest)
	}
	if again, _ := newRequests(comments, seen); len(again) != 0 {
		t.Fatalf("seen comments came back: %v", again)
	}
	// One listed late, behind a higher ID, is still found.
	late := append(comments, issueComment{ID: 15, Body: body, IssueURL: "https://api.github.com/repos/o/r/issues/3"})
	if found, _ := newRequests(late, seen); fmt.Sprint(found) != "[3]" {
		t.Fatalf("late comment: %v", found)
	}
}

func TestBuildWaitingIsAboutThisRunner(t *testing.T) {
	jobs := func(queued ...queuedJob) func() ([]queuedJob, error) {
		return func() ([]queuedJob, error) { return queued, nil }
	}
	selfHosted := []string{"self-hosted", "macOS"}
	for _, test := range []struct {
		name   string
		labels []string
		jobs   func() ([]queuedJob, error)
		want   bool
	}{
		{"a build for this runner", selfHosted, jobs(queuedJob{Name: "test", Labels: selfHosted}), true},
		{"a build needing fewer labels fits too", selfHosted, jobs(queuedJob{Name: "test", Labels: []string{"macOS"}}), true},
		{"the pending Writer doesn't count", selfHosted, jobs(queuedJob{Name: "writer", Labels: selfHosted}), false},
		{"a build for another runner", selfHosted, jobs(queuedJob{Name: "test", Labels: []string{"self-hosted", "linux"}}), false},
		{"a kept-free Writer runner yields to nobody", []string{"quipu-writer"}, jobs(queuedJob{Name: "test", Labels: selfHosted}), false},
		{"nothing queued", selfHosted, jobs(), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := buildWaiting(test.labels, test.jobs)
			if err != nil || got != test.want {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
	hosted, err := buildWaiting([]string{"ubuntu-latest"}, func() ([]queuedJob, error) {
		t.Fatal("a hosted Writer must cost no calls")
		return nil, nil
	})
	if err != nil || hosted {
		t.Fatalf("hosted: %v, %v", hosted, err)
	}
}

func TestRateLow(t *testing.T) {
	if got := rateLow(5000, 5000); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := rateLow(10, 5000); !strings.Contains(got, "REST") {
		t.Fatalf("got %q", got)
	}
	if got := rateLow(5000, 10); !strings.Contains(got, "GraphQL") {
		t.Fatalf("got %q", got)
	}
}

func TestAFailedRunFilesOneDefectByListing(t *testing.T) {
	c := model.Config{DefaultBranch: "main"}
	failed := runFacts{ID: 7, Name: "CI", Conclusion: "failure", HeadBranch: "main", Event: "push"}
	runs := []runFacts{
		failed,
		{ID: 8, Name: "CI", Conclusion: "success", HeadBranch: "main", Event: "push"},
		{ID: 9, Name: "CI", Conclusion: "failure", HeadBranch: "feature", Event: "push"},
		{ID: 10, Name: "CI", Conclusion: "failure", HeadBranch: "main", Event: "pull_request"},
		{ID: 11, Name: "Other", Conclusion: "failure", HeadBranch: "main", Event: "push"},
	}
	// No run reported it, because the pending run that would have was replaced.
	if got := runsNeedingDefects(c, runs, map[string]int{}); len(got) != 1 || got[0].ID != 7 {
		t.Fatalf("got %+v", got)
	}
	if got := runsNeedingDefects(c, runs, map[string]int{"7": 42}); len(got) != 0 {
		t.Fatalf("a filed run came back: %+v", got)
	}
}

func TestReadLagDoesNotLookLikeLosingAClaim(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	sleep := func(d time.Duration) { now = now.Add(d) }
	won := writer.Receipt{RequestID: "1", Result: "accepted", State: &writer.ReceiptState{Status: "Claimed", Holder: "me", Expires: "2026-11-03T14:00:00Z", Branch: "1/a"}}
	moved := liveWork{Status: "Claimed", Holder: "me", Expiry: "2026-11-03T14:00:00Z", Branch: "1/a"}
	reads := 0
	load := func() (liveWork, error) {
		reads++
		if reads < 4 {
			return liveWork{Status: "Ready"}, nil
		}
		return moved, nil
	}
	if err := awaitReceiptState(won, load, clock, sleep); err != nil || reads != 4 {
		t.Fatalf("err %v after %d reads", err, reads)
	}
	// Still lagging is no-receipt: rerun. It is never errRejected, which --next
	// reads as losing the race and moves on from.
	err := awaitReceiptState(won, func() (liveWork, error) { return liveWork{Status: "Ready"}, nil }, clock, sleep)
	if flowCode(err) != "no-receipt" || !errors.Is(err, errNoReceipt) || errors.Is(err, errRejected) {
		t.Fatalf("err = %v", err)
	}
	if err := awaitReceiptState(writer.Receipt{}, func() (liveWork, error) { t.Fatal("no state to wait for"); return liveWork{}, nil }, clock, sleep); err != nil {
		t.Fatal(err)
	}
}

func TestDriftWaitsOutReadLagOnlyWhenSerial(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	last := writer.ReceiptState{Status: "Claimed", Holder: "me", Expires: "2026-11-03T14:00:00Z", Branch: "1/a"}
	lagging := liveWork{Status: "Ready", CreatedAt: "2026-10-01T00:00:00Z"}
	serial := model.Config{Workflows: model.Workflows{WriterMode: "serial"}}
	w := &watch{c: serial, now: now, receipts: []writer.Receipt{{RequestID: "1", Result: "accepted", At: now.Add(-5 * time.Second), State: &last}}}
	if restored, err := w.drift(&lagging); err != nil || restored || w.report.Restored != 0 {
		t.Fatalf("restored %v, %d, err %v: a read just after a receipt can only be lag", restored, w.report.Restored, err)
	}
	// Past the window it is a real edit. Without --apply this only counts it.
	w.now = now.Add(time.Minute)
	if _, err := w.drift(&lagging); err != nil || w.report.Restored != 1 {
		t.Fatalf("restored %d, err %v", w.report.Restored, err)
	}
	// With the default Writer nothing changes.
	w = &watch{now: now, receipts: []writer.Receipt{{RequestID: "1", Result: "accepted", At: now.Add(-5 * time.Second), State: &last}}}
	if _, err := w.drift(&lagging); err != nil || w.report.Restored != 1 {
		t.Fatalf("default mode: restored %d, err %v", w.report.Restored, err)
	}
}

func TestDoctorWarnsWhenWriterWorkflowAndConfigDisagree(t *testing.T) {
	serial := model.Config{Workflows: model.Workflows{WriterMode: "serial"}}
	const events, served = "quipu writer reconcile --apply", "quipu writer serve --apply --json"
	for _, test := range []struct {
		name     string
		c        model.Config
		workflow string
		found    bool
		warns    bool
	}{
		{"flag set, not regenerated", serial, events, true, true},
		{"regenerated, flag gone", model.Config{}, served, true, true},
		{"both serial", serial, served, true, false},
		{"both default", model.Config{}, events, true, false},
		{"no workflow to compare", serial, "", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := writerModeCheck(test.c, test.workflow, test.found)
			if (len(got) == 1 && got[0].Status == "warn") != test.warns {
				t.Fatalf("got %+v, want warn=%v", got, test.warns)
			}
		})
	}
}

func TestRunnerSetupExplainsTheWriterRunner(t *testing.T) {
	c := model.Config{Owner: "o", Repository: "r", Workflows: model.Workflows{WriterMode: "serial", RunsOn: map[string][]string{"default": {"self-hosted", "macOS"}}}}
	joined := strings.Join(planRunnerSetup(c, nil).Steps, "\n")
	if !strings.Contains(joined, "--no-default-labels --labels quipu-writer") {
		t.Fatalf("steps:\n%s", joined)
	}
	c.Workflows.WriterMode = ""
	if strings.Contains(strings.Join(planRunnerSetup(c, nil).Steps, "\n"), "quipu-writer") {
		t.Fatal("only a serial Writer needs this")
	}
}
