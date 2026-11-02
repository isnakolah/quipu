package main

// Whether any runner can take a job. GitHub reports a job with no runner the
// same way as one about to start, so an agent would poll a check that can never
// finish. These helpers compare a job's labels with the registered runners.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	"gopkg.in/yaml.v3"
)

type repoRunner struct {
	Name   string
	Online bool
	Labels []string
}

// runnerFacts is what the token can see. Partial means organization-level
// runners may exist beyond those listed; Err means none could be read.
type runnerFacts struct {
	Runners []repoRunner
	Partial bool
	Err     error
}

func listRunners(path string) ([]repoRunner, error) {
	output, err := ghOutput("api", "--paginate", "--slurp", path)
	if err != nil {
		return nil, err
	}
	return parseRunnerPages(output)
}

// parseRunnerPages reads every page, since a runner past the first would
// otherwise look missing.
func parseRunnerPages(output []byte) ([]repoRunner, error) {
	var pages []struct {
		Runners []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		} `json:"runners"`
	}
	if err := json.Unmarshal(output, &pages); err != nil {
		return nil, err
	}
	runners := []repoRunner{}
	for _, page := range pages {
		for _, runner := range page.Runners {
			labels := make([]string, 0, len(runner.Labels))
			for _, label := range runner.Labels {
				labels = append(labels, label.Name)
			}
			runners = append(runners, repoRunner{Name: runner.Name, Online: runner.Status == "online", Labels: labels})
		}
	}
	return runners, nil
}

func loadRunnerFacts(c model.Config) runnerFacts {
	repo := "repos/" + c.Owner + "/" + c.Repository
	runners, err := listRunners(repo + "/actions/runners?per_page=100")
	if err != nil {
		return runnerFacts{Err: err}
	}
	facts := runnerFacts{Runners: runners}
	output, err := ghOutput("api", repo, "--jq", ".owner.type")
	if err != nil {
		facts.Partial = true
		return facts
	}
	if strings.TrimSpace(string(output)) == "Organization" {
		shared, err := listRunners("orgs/" + c.Owner + "/actions/runners?per_page=100")
		if err != nil {
			facts.Partial = true
			return facts
		}
		facts.Runners = append(facts.Runners, shared...)
	}
	return facts
}

type runnerVerdict int

const (
	runnerAvailable runnerVerdict = iota
	runnerOffline
	runnerMissing
	runnerUnknown
)

// hostedLabel matches GitHub's image names, such as ubuntu-latest, macos-15-xlarge
// or windows-11-arm. A custom label like ubuntu-gpu belongs to a self-hosted runner.
var hostedLabel = regexp.MustCompile(`(?i)^(ubuntu|macos|windows)-(latest|slim|[0-9][0-9.]*)(-[a-z0-9-]+)?$`)

// hostedLabels reports whether labels ask for a GitHub-hosted runner, which the
// runners API never lists.
func hostedLabels(labels []string) bool {
	for _, label := range labels {
		if !hostedLabel.MatchString(label) {
			return false
		}
	}
	return len(labels) > 0
}

func hasRunnerLabel(labels []string, want string) bool {
	for _, label := range labels {
		if strings.EqualFold(label, want) {
			return true
		}
	}
	return false
}

// judgeRunners decides whether a job with these labels can start. One runner
// must carry every label; a busy runner still counts, since the job only waits.
func judgeRunners(labels []string, facts runnerFacts) (runnerVerdict, string) {
	if hostedLabels(labels) {
		return runnerAvailable, "GitHub-hosted"
	}
	if len(labels) == 0 {
		return runnerUnknown, "its runner labels are unknown"
	}
	if facts.Err != nil {
		return runnerUnknown, "runner state unknown: this token cannot read the repository's runners"
	}
	var offline, seen []string
	for _, runner := range facts.Runners {
		seen = append(seen, runner.Labels...)
		matches := true
		for _, label := range labels {
			matches = matches && hasRunnerLabel(runner.Labels, label)
		}
		if !matches {
			continue
		}
		if runner.Online {
			return runnerAvailable, "runner " + runner.Name + " is online"
		}
		offline = append(offline, runner.Name)
	}
	if facts.Partial {
		return runnerUnknown, "runner state unknown: this token cannot read organization runners"
	}
	if len(offline) > 0 {
		return runnerOffline, "runner " + strings.Join(offline, ", ") + " is offline"
	}
	var missing []string
	for _, label := range labels {
		if !hasRunnerLabel(seen, label) {
			missing = append(missing, label)
		}
	}
	if len(missing) == 0 {
		return runnerMissing, "no single runner has all of " + strings.Join(labels, ", ")
	}
	return runnerMissing, "no runner has " + strings.Join(missing, ", ")
}

// jobFacts is what GitHub says about the job behind a check.
type jobFacts struct {
	Queued bool
	Labels []string
}

var jobURL = regexp.MustCompile(`/actions/runs/[0-9]+/job/([0-9]+)`)

func loadJobFacts(c model.Config, detailsURL string) (jobFacts, error) {
	match := jobURL.FindStringSubmatch(detailsURL)
	if match == nil {
		return jobFacts{}, fmt.Errorf("%q is not an Actions job", detailsURL)
	}
	output, err := ghOutput("api", fmt.Sprintf("repos/%s/%s/actions/jobs/%s", c.Owner, c.Repository, match[1]))
	if err != nil {
		return jobFacts{}, err
	}
	var job struct {
		Status string   `json:"status"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(output, &job); err != nil {
		return jobFacts{}, err
	}
	return jobFacts{Queued: job.Status == "queued", Labels: job.Labels}, nil
}

// explainPending turns checks-pending into checks-no-runner when a required
// check is queued with no runner able to take it. When it cannot tell, it keeps
// checks-pending and says the runner state is unknown.
func explainPending(gate error, checks []prCheck, required []string, facts func() runnerFacts, job func(string) (jobFacts, error)) error {
	var coded *flowError
	if !errors.As(gate, &coded) || coded.Code != "checks-pending" {
		return gate
	}
	var stuck, unknown []string
	for _, check := range checks {
		if !contains(required, check.Name) || check.Status == "" || check.Status == "COMPLETED" {
			continue
		}
		state, err := job(check.DetailsURL)
		if err != nil {
			unknown = append(unknown, check.Name)
			continue
		}
		if !state.Queued {
			continue
		}
		verdict, detail := judgeRunners(state.Labels, facts())
		switch verdict {
		case runnerOffline, runnerMissing:
			stuck = append(stuck, fmt.Sprintf("%s needs [%s]: %s", check.Name, strings.Join(state.Labels, ", "), detail))
		case runnerUnknown:
			unknown = append(unknown, check.Name)
		}
	}
	if len(stuck) > 0 {
		return flowErr("checks-no-runner", "required checks cannot start: %s", strings.Join(stuck, "; "))
	}
	if len(unknown) > 0 {
		return flowErr("checks-pending", "%s (runner state unknown for %s)", coded.Message, strings.Join(unknown, ", "))
	}
	return gate
}

// explainPendingChecks is explainPending against live GitHub.
func explainPendingChecks(c model.Config, gate error, checks []prCheck, required []string) error {
	facts := sync.OnceValue(func() runnerFacts { return loadRunnerFacts(c) })
	return explainPending(gate, checks, required, facts, func(url string) (jobFacts, error) { return loadJobFacts(c, url) })
}

// describeWriter reports what is known about the Writer when a request gets no
// receipt. The request stays on the Issue, so nothing has to be sent again.
func describeWriter(lastRun time.Time, lastErr error, labels []string, facts runnerFacts, now time.Time) string {
	parts := []string{"the request stays queued, and the Writer applies it when it next runs"}
	switch {
	case lastErr != nil:
		parts = append(parts, "its last run is unknown")
	case lastRun.IsZero():
		parts = append(parts, "it has never run successfully")
	default:
		parts = append(parts, "its last successful run started "+now.Sub(lastRun).Round(time.Minute).String()+" ago; if that is recent, check the QUIPU_WRITER_TOKEN secret, since a scheduled run without it succeeds and does nothing")
	}
	_, detail := judgeRunners(labels, facts)
	return strings.Join(parts, "; ") + "; its runner [" + strings.Join(labels, ", ") + "]: " + detail
}

func writerDiagnosis(c model.Config, now time.Time) string {
	labels := c.Workflows.Runner("writer")
	var facts runnerFacts
	if !hostedLabels(labels) {
		facts = loadRunnerFacts(c)
	}
	lastRun, err := lastWriterRun(c)
	return describeWriter(lastRun, err, labels, facts, now)
}

// workflowJob is one job in a workflow file. Computed means its runner comes
// from an expression or a runner group, which only GitHub can resolve.
type workflowJob struct {
	ID, Name string
	Labels   []string
	Computed bool
	// Workflow is the name of the workflow the job is in.
	Workflow string
}

func parseWorkflowJobs(raw []byte) ([]workflowJob, error) {
	var file struct {
		Name string `yaml:"name"`
		Jobs map[string]struct {
			Name   string    `yaml:"name"`
			RunsOn yaml.Node `yaml:"runs-on"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	jobs := make([]workflowJob, 0, len(file.Jobs))
	for id, job := range file.Jobs {
		parsed := workflowJob{ID: id, Name: job.Name, Workflow: file.Name}
		node := &job.RunsOn
		switch node.Kind {
		case yaml.ScalarNode:
			parsed.Labels = []string{node.Value}
		case yaml.SequenceNode:
			for _, item := range node.Content {
				parsed.Labels = append(parsed.Labels, item.Value)
			}
		default:
			parsed.Computed = true
		}
		for _, label := range parsed.Labels {
			if strings.Contains(label, "${{") {
				parsed.Computed = true
			}
		}
		jobs = append(jobs, parsed)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return jobs, nil
}

// checkJob finds the job behind a required check. Its check name is the job's
// name, or its ID when unnamed, with any matrix values in parentheses after it.
// Jobs in several workflows may share a name; when their runners differ, which
// one the check means is unknown, so the result is Computed.
func checkJob(check string, jobs []workflowJob) (workflowJob, bool) {
	var found workflowJob
	matched := false
	for _, job := range jobs {
		if !jobMatchesCheck(job, check) {
			continue
		}
		if matched && (job.Computed || strings.Join(job.Labels, "\n") != strings.Join(found.Labels, "\n")) {
			return workflowJob{ID: job.ID, Name: job.Name, Computed: true, Workflow: job.Workflow}, true
		}
		found, matched = job, true
	}
	return found, matched
}

// jobMatchesCheck reports whether a check is this job, or a matrix leg of it.
func jobMatchesCheck(job workflowJob, check string) bool {
	name := job.Name
	if name == "" {
		name = job.ID
	}
	return check == name || strings.HasPrefix(check, name+" (")
}

// readWorkflowJobs reads the default branch's workflow files, or the worktree's
// when this clone has no copy of that branch.
func readWorkflowJobs(c model.Config, w worktree) []workflowJob {
	var jobs []workflowJob
	add := func(raw []byte) {
		if parsed, err := parseWorkflowJobs(raw); err == nil {
			jobs = append(jobs, parsed...)
		}
	}
	ref := "origin/" + c.DefaultBranch
	if names, err := w.git("ls-tree", "--name-only", ref, ".github/workflows/"); err == nil && names != "" {
		for _, name := range strings.Split(names, "\n") {
			if raw, err := w.git("show", ref+":"+name); err == nil {
				add([]byte(raw))
			}
		}
		return jobs
	}
	paths, _ := filepath.Glob(filepath.Join(w.Top, ".github", "workflows", "*.y*ml"))
	for _, path := range paths {
		if raw, err := os.ReadFile(path); err == nil {
			add(raw)
		}
	}
	return jobs
}

const runnerFix = "owner: start that runner, register one with those labels, change the job's runs-on, or take the check out of required_checks"

func runnerCheck(name string, labels []string, facts runnerFacts) doctorCheck {
	verdict, detail := judgeRunners(labels, facts)
	detail = "needs [" + strings.Join(labels, ", ") + "]: " + detail
	switch verdict {
	case runnerAvailable:
		return doctorCheck{name, "ok", detail, ""}
	case runnerUnknown:
		return doctorCheck{name, "warn", detail, "owner: run quipu doctor with a token that can read the repository's runners"}
	}
	return doctorCheck{name, "fail", detail, runnerFix}
}

// runnerNeed is one runner the repository depends on. Note says why its labels
// cannot be read.
type runnerNeed struct {
	name   string
	labels []string
	note   string
}

// runnerNeeds lists what the config and required checks ask for. The labels of a
// required check usually live only in the repository's own workflow file, so the
// config alone cannot show them.
func runnerNeeds(c model.Config, jobs []workflowJob) []runnerNeed {
	type need = runnerNeed
	needs := []need{{name: "runner: Writer", labels: c.Workflows.Runner("writer")}, {name: "runner: policy", labels: c.Workflows.Runner("policy")}}
	var configured []string
	for job := range c.Workflows.RunsOn {
		if job != "default" && job != "writer" && job != "policy" {
			configured = append(configured, job)
		}
	}
	sort.Strings(configured)
	for _, job := range configured {
		needs = append(needs, need{name: "runner: workflows.runs_on." + job, labels: c.Workflows.Runner(job)})
	}
	for _, check := range c.Checks() {
		job, found := checkJob(check, jobs)
		switch {
		case !found:
			needs = append(needs, need{name: "runner: check " + check, note: "no workflow job has this name, so its runner is unknown"})
		case job.Computed:
			needs = append(needs, need{name: "runner: check " + check, note: "its runner comes from an expression, a runner group, or several workflows, so it is unknown"})
		default:
			needs = append(needs, need{name: "runner: check " + check, labels: job.Labels})
		}
	}
	return needs
}

// judgeRunnerNeeds compares what the repository needs with the runners.
func judgeRunnerNeeds(c model.Config, jobs []workflowJob, facts func() runnerFacts) []doctorCheck {
	needs := runnerNeeds(c, jobs)
	checks := make([]doctorCheck, 0, len(needs))
	for _, item := range needs {
		if item.note != "" {
			checks = append(checks, doctorCheck{item.name, "warn", item.note, "owner: confirm a runner can take this check"})
			continue
		}
		var known runnerFacts
		if !hostedLabels(item.labels) {
			known = facts()
		}
		checks = append(checks, runnerCheck(item.name, item.labels, known))
	}
	return checks
}

func runnerChecks(c model.Config, w worktree) []doctorCheck {
	facts := sync.OnceValue(func() runnerFacts { return loadRunnerFacts(c) })
	return judgeRunnerNeeds(c, readWorkflowJobs(c, w), facts)
}

// ownerItem is one thing only the owner can move.
type ownerItem struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// ownerQueue lists what waits on the owner and why. Agents leave these alone
// and take other work.
func ownerQueue(items []statusItem, agentsMerge bool, now time.Time) []ownerItem {
	queue := []ownerItem{}
	for _, item := range items {
		reason := ""
		switch {
		case item.NeedsTriage:
			reason = "outside the Quipu flow; triage it"
		case item.Kind == "Decision" || item.Kind == "Gate":
			reason = "only the owner decides a " + item.Kind
		case item.Kind == "Epic":
			if item.ChildTotal > 0 && item.ChildCompleted == item.ChildTotal {
				reason = "all children are done; close the goal or add work"
			}
		case item.Status == "In review":
			if !agentsMerge {
				reason = "review and merge its PR"
			}
		case item.Status == "Blocked":
			reason = "blocked; decide how to unblock it"
		case item.Status == "Claimed" || item.Status == "In progress":
			if !hasActiveLease(item.LeaseHolder, item.LeaseExpires, now) {
				reason = "lease expired; the holder can renew it with quipu start, and the Writer returns it to Ready unless its branch has an open PR. If the agent is gone, close the PR to release it"
			}
		}
		if reason != "" {
			queue = append(queue, ownerItem{Number: item.Number, Title: item.Title, Reason: reason})
		}
	}
	sort.Slice(queue, func(i, j int) bool { return queue[i].Number < queue[j].Number })
	return queue
}
