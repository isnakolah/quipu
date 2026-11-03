package main

// `quipu writer serve` is the serial Writer: one job that keeps applying requests
// while there are any, in place of a pile of runs that replace each other. GitHub
// events only wake it. It runs in Actions like any other Writer pass, so receipts
// still come from github-actions[bot] and nothing about trust changes.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

const (
	servePoll = 3 * time.Second
	// serveIdle is how long it stays with nothing to do, so a claim's second
	// request finds it still running.
	serveIdle = time.Minute
	// serveLimit bounds a run, so a busy repository can't hold a shared runner.
	serveLimit = 10 * time.Minute
	// serveSweepEvery covers what leaves no request: hand edits and expired leases.
	serveSweepEvery = 5 * time.Minute
	// serveYieldEvery and serveRateEvery space out the checks that cost calls.
	serveYieldEvery = 30 * time.Second
	serveRateEvery  = time.Minute
	// serveMaxErrors in a row means GitHub is not answering; failing shows it.
	serveMaxErrors = 5
	// serveMinPoints leaves agents something to read with.
	serveMinPoints = 200
	// serveReach is how far before the newest comment each poll looks, since a
	// comment can be listed later than one posted after it.
	serveReach = 2 * time.Minute
	// serveRunsReach is how far back a sweep looks for failed runs.
	serveRunsReach = 24 * time.Hour
)

// serveOps is what the loop does to the world.
type serveOps struct {
	// poll names the Issues with new requests, in comment order.
	poll     func() ([]int, error)
	sweep    func(first bool) error
	handle   func(number int) error
	yield    func() (bool, error)
	rate     func() (string, error)
	dispatch func() error
	now      func() time.Time
	sleep    func(time.Duration)
}

type serveResult struct {
	Reason     string   `json:"reason"`
	Polls      int      `json:"polls"`
	Unchanged  int      `json:"polls_unchanged"`
	Handled    []int    `json:"issues_handled"`
	Sweeps     int      `json:"sweeps"`
	Dispatched bool     `json:"dispatched"`
	Errors     []string `json:"errors"`
}

const (
	serveStopIdle  = "idle"
	serveStopLimit = "time limit"
	serveStopYield = "a build is waiting for this runner"
)

// serve runs until a stop condition, and returns an error for the ones that should
// show as a failed run.
func serve(ops serveOps) (serveResult, error) {
	result := serveResult{Handled: []int{}, Errors: []string{}}
	started := ops.now()
	lastWork, nextSweep, nextYield, nextRate := started, started.Add(serveSweepEvery), started, started
	failures := 0
	var waiting []int
	note := func(err error) error {
		result.Errors = append(result.Errors, err.Error())
		failures++
		if failures >= serveMaxErrors {
			result.Reason = "repeated GitHub errors"
			return fmt.Errorf("serve stopped after %d GitHub errors in a row: %w", failures, err)
		}
		return nil
	}
	if err := ops.sweep(true); err != nil {
		if err := note(err); err != nil {
			return result, err
		}
	}
	result.Sweeps++
	// stop decides between Issues, so an Issue in hand always finishes.
	stop := func(now time.Time) (string, error) {
		if !now.Before(started.Add(serveLimit)) {
			return serveStopLimit, nil
		}
		if !now.Before(nextRate) {
			nextRate = now.Add(serveRateEvery)
			low, err := ops.rate()
			if err != nil {
				return "", note(err)
			}
			if low != "" {
				result.Reason = "low rate limit"
				return "", errors.New("serve stopped: " + low)
			}
		}
		if !now.Before(nextYield) {
			nextYield = now.Add(serveYieldEvery)
			waitingBuild, err := ops.yield()
			if err != nil {
				return "", note(err)
			}
			if waitingBuild {
				return serveStopYield, nil
			}
		}
		return "", nil
	}
	// finish ends a run that stopped early. Another run takes what's left, including
	// what a failed last look can't rule out.
	finish := func(reason string) (serveResult, error) {
		result.Reason = reason
		fresh, err := ops.poll()
		waiting = appendNew(waiting, fresh...)
		if err == nil && len(waiting) == 0 {
			return result, nil
		}
		if err := ops.dispatch(); err != nil {
			result.Errors = append(result.Errors, "dispatch: "+err.Error())
			return result, err
		}
		result.Dispatched = true
		return result, nil
	}
	for {
		reason, err := stop(ops.now())
		if err != nil {
			return result, err
		}
		if reason != "" {
			return finish(reason)
		}
		fresh, err := ops.poll()
		if err != nil {
			if err := note(err); err != nil {
				return result, err
			}
		} else {
			failures = 0
			waiting = appendNew(waiting, fresh...)
		}
		for len(waiting) > 0 {
			number := waiting[0]
			if err := ops.handle(number); err != nil {
				if err := note(fmt.Errorf("#%d: %w", number, err)); err != nil {
					return result, err
				}
				// It keeps its turn; the same request is still unreceipted.
				break
			}
			failures = 0
			waiting = waiting[1:]
			if !slices.Contains(result.Handled, number) {
				result.Handled = append(result.Handled, number)
			}
			lastWork = ops.now()
			if reason, err := stop(lastWork); err != nil {
				return result, err
			} else if reason != "" {
				return finish(reason)
			}
		}
		now := ops.now()
		if !now.Before(nextSweep) {
			nextSweep = now.Add(serveSweepEvery)
			if err := ops.sweep(false); err != nil {
				if err := note(err); err != nil {
					return result, err
				}
			}
			result.Sweeps++
		}
		if len(waiting) == 0 && now.Sub(lastWork) >= serveIdle {
			// One last look: a request sent since the last poll would wait for a wake.
			fresh, err := ops.poll()
			if err == nil && len(fresh) == 0 {
				result.Reason = serveStopIdle
				return result, nil
			}
			if err == nil {
				waiting = appendNew(waiting, fresh...)
				continue
			}
			if err := note(err); err != nil {
				return result, err
			}
		}
		ops.sleep(servePoll)
	}
}

// commentPoll finds new requests from the repository's comments. Asking with an
// ETag makes the usual answer a 304, which GitHub doesn't count against the limit.
type commentPoll struct {
	client           *github.Client
	path             string
	since            time.Time
	etag             string
	seen             map[int64]bool
	polls, unchanged int
}

func newCommentPoll(c model.Config, client *github.Client, since time.Time) *commentPoll {
	return &commentPoll{client: client, path: fmt.Sprintf("/repos/%s/%s/issues/comments", c.Owner, c.Repository), since: since, seen: map[int64]bool{}}
}

func (p *commentPoll) next(ctx context.Context) ([]int, error) {
	query := url.Values{"per_page": {"100"}, "sort": {"created"}, "direction": {"asc"}, "since": {p.since.UTC().Format(time.RFC3339)}}
	var comments []issueComment
	p.polls++
	etag, unchanged, err := p.client.GetConditional(ctx, p.path+"?"+query.Encode(), p.etag, &comments)
	if err != nil {
		return nil, err
	}
	if unchanged {
		p.unchanged++
		return nil, nil
	}
	p.etag = etag
	for page := 2; len(comments) == (page-1)*100; page++ {
		query.Set("page", fmt.Sprint(page))
		var more []issueComment
		if err := p.client.Do(ctx, http.MethodGet, p.path+"?"+query.Encode(), nil, &more); err != nil {
			return nil, err
		}
		comments = append(comments, more...)
	}
	numbers, newest := newRequests(comments, p.seen)
	if newest.Add(-serveReach).After(p.since) {
		// A different address has its own ETag; the next answer pays once.
		p.since, p.etag = newest.Add(-serveReach), ""
	}
	return numbers, nil
}

// newRequests names the Issues with requests not seen before, in comment ID order,
// and marks them seen. The Writer's pass over an Issue decides what's unreceipted.
func newRequests(comments []issueComment, seen map[int64]bool) ([]int, time.Time) {
	comments = slices.Clone(comments)
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].ID < comments[j].ID })
	var numbers []int
	var newest time.Time
	for _, comment := range comments {
		if created, err := time.Parse(time.RFC3339, comment.CreatedAt); err == nil && created.After(newest) {
			newest = created
		}
		if seen[comment.ID] {
			continue
		}
		seen[comment.ID] = true
		if !writer.IsRequest(comment.Body) || strings.Contains(comment.HTMLURL, "/pull/") {
			continue
		}
		if number, err := issueNumberFromURL(comment.IssueURL); err == nil {
			numbers = appendNew(numbers, number)
		}
	}
	return numbers, newest
}

// buildWaiting reports whether a job other than the Writer waits for this runner.
// A hosted Writer takes no one's runner, so it costs no calls.
func buildWaiting(labels []string, jobs func() ([]queuedJob, error)) (bool, error) {
	if len(labels) == 0 || hostedLabels(labels) {
		return false, nil
	}
	queued, err := jobs()
	if err != nil {
		return false, err
	}
	for _, job := range queued {
		if job.Name == "writer" || len(job.Labels) == 0 {
			continue
		}
		fits := true
		for _, label := range job.Labels {
			fits = fits && hasRunnerLabel(labels, label)
		}
		if fits {
			return true, nil
		}
	}
	return false, nil
}

// rateLow says what is nearly spent, or "".
func rateLow(core, graphql int) string {
	switch {
	case core < serveMinPoints:
		return fmt.Sprintf("only %d REST requests left this hour", core)
	case graphql < serveMinPoints:
		return fmt.Sprintf("only %d GraphQL points left this hour", graphql)
	}
	return ""
}

func loadRateLow(ctx context.Context, client *github.Client) (string, error) {
	var limits struct {
		Resources struct {
			Core    struct{ Remaining int } `json:"core"`
			GraphQL struct{ Remaining int } `json:"graphql"`
		} `json:"resources"`
	}
	if err := client.Do(ctx, http.MethodGet, "/rate_limit", nil, &limits); err != nil {
		return "", err
	}
	return rateLow(limits.Resources.Core.Remaining, limits.Resources.GraphQL.Remaining), nil
}

// runsNeedingDefects picks the failed runs with no Defect yet. Listing them means a
// failure still files one when the run that would have reported it was replaced.
func runsNeedingDefects(c model.Config, runs []runFacts, filed map[string]int) []runFacts {
	var missing []runFacts
	for _, run := range runs {
		if _, has := filed[fmt.Sprint(run.ID)]; !has && defectSkip(c, run) == "" {
			missing = append(missing, run)
		}
	}
	return missing
}

func sweepFailedRuns(c model.Config, apply bool, now time.Time) error {
	if len(c.Workflows.DefectWorkflows()) == 0 {
		return nil
	}
	repo := "repos/" + c.Owner + "/" + c.Repository
	query := url.Values{"branch": {c.DefaultBranch}, "status": {"completed"}, "per_page": {"100"}, "created": {">=" + now.Add(-serveRunsReach).UTC().Format(time.RFC3339)}}
	output, err := ghOutput("api", repo+"/actions/runs?"+query.Encode())
	if err != nil {
		return err
	}
	var listed struct {
		Runs []runFacts `json:"workflow_runs"`
	}
	if err := json.Unmarshal(output, &listed); err != nil {
		return err
	}
	if len(runsNeedingDefects(c, listed.Runs, nil)) == 0 {
		return nil
	}
	if output, err = ghOutput("api", "--paginate", repo+"/issues?state=all&per_page=100"); err != nil {
		return err
	}
	filed, err := markedIssues(output, defectMarker)
	if err != nil {
		return err
	}
	for _, run := range runsNeedingDefects(c, listed.Runs, filed) {
		if _, err := defectForRun(c, run, apply, now); err != nil {
			return fmt.Errorf("run %d: %w", run.ID, err)
		}
	}
	return nil
}

func dispatchWriter(c model.Config) error {
	_, err := ghOutput("workflow", "run", "quipu-writer.yml", "--repo", c.Owner+"/"+c.Repository, "--ref", c.DefaultBranch)
	return err
}

func writerServeCommand(args []string) error {
	fs := flag.NewFlagSet("writer serve", flag.ContinueOnError)
	apply := applyFlag(fs)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	trust, began := receiptTrust(c), time.Now()
	if !*apply {
		// Without --apply it reports what one pass would do and doesn't stay.
		numbers, err := reconcileTargets(c, 0, began)
		if err != nil {
			return err
		}
		var total issueReport
		failures := []string{}
		reconcileNumbers(c, numbers, false, trust, began, &total, &failures)
		return printValue(map[string]any{"apply": false, "issues_scanned": len(numbers), "requests_pending": len(total.Pending), "errors": failures}, *asJSON)
	}
	if err := requireWriterIdentity(trust, began); err != nil {
		return err
	}
	ctx := context.Background()
	client := github.NewClient()
	poll := newCommentPoll(c, client, began.Add(-serveReach))
	ops := serveOps{
		poll: func() ([]int, error) { return poll.next(ctx) },
		sweep: func(first bool) error {
			now := time.Now()
			window := time.Duration(0)
			// A wake from a comment needs only what changed since the last run: every
			// comment doing a full pass would spend the hour's points on one busy Issue.
			if first && os.Getenv("GITHUB_EVENT_NAME") == "issue_comment" {
				started, err := lastWriterRun(c)
				if err != nil {
					return err
				}
				window = drainWindow(started, now)
			}
			numbers, err := reconcileTargets(c, window, now)
			if err != nil {
				return err
			}
			var total issueReport
			failures := []string{}
			reconcileNumbers(c, numbers, true, trust, now, &total, &failures)
			if err := sweepFailedRuns(c, true, now); err != nil {
				failures = append(failures, "failed runs: "+err.Error())
			}
			if len(failures) > 0 {
				return errors.New(strings.Join(failures, "; "))
			}
			return nil
		},
		handle: func(number int) error {
			_, err := reconcileIssue(c, number, true, trust, time.Now())
			return err
		},
		yield: func() (bool, error) {
			return buildWaiting(c.Workflows.Runner("writer"), func() ([]queuedJob, error) { return loadQueuedJobs(c) })
		},
		rate:     func() (string, error) { return loadRateLow(ctx, client) },
		dispatch: func() error { return dispatchWriter(c) },
		now:      time.Now,
		sleep:    time.Sleep,
	}
	result, err := serve(ops)
	result.Polls, result.Unchanged = poll.polls, poll.unchanged
	if printErr := printValue(result, *asJSON); printErr != nil {
		return printErr
	}
	return err
}
