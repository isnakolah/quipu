package main

// On-demand runners. When a runner the repository needs is down, `quipu start`
// and `quipu done` run the owner's workflows.runner_start once and carry on, so
// agents never wait for it. `quipu runner` shows the state and how to set one up,
// and `quipu status` reports each PR's checks, so agents check back when free.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/model"
)

const (
	// runnerStartEvery keeps agents in several worktrees from each starting it.
	runnerStartEvery = 10 * time.Minute
	// runnerStartGrace is how long a start may fail fast and be reported.
	// After it the command keeps running, and nothing waits for it.
	runnerStartGrace = 2 * time.Second
)

// runnerStarter is what starting a runner needs, so each step can be faked.
type runnerStarter struct {
	down    func() []doctorCheck
	command func() string
	claim   func() (runnerClaim, error)
	run     func(command string) error
}

// runnerClaim is what the stamp says when a start is due.
type runnerClaim struct {
	won    bool
	failed string // why the last start failed, if it did and it was recent
}

// ensure starts a needed runner that is down and says what it did, or returns ""
// when nothing is down.
func (s runnerStarter) ensure() string {
	down := s.down()
	if len(down) == 0 {
		return ""
	}
	names := make([]string, len(down))
	for i, check := range down {
		names[i] = strings.TrimPrefix(check.Name, "runner: ") + " " + check.Detail
	}
	what := "needed runners are down: " + strings.Join(names, "; ")
	command := s.command()
	if command == "" {
		return what + ". workflows.runner_start is unset, so nothing started them; " + runnerFix
	}
	claim, err := s.claim()
	if err != nil {
		return what + ". Could not tell whether runner_start ran recently, so it was not run: " + err.Error() + "; " + runnerFix
	}
	if claim.failed != "" {
		return what + ". runner_start failed in the last " + runnerStartEvery.String() + ": " + claim.failed + "; " + runnerFix
	}
	if !claim.won {
		return what + ". runner_start ran in the last " + runnerStartEvery.String() + ", so it was not run again. Carry on; do not wait for it"
	}
	if err := s.run(command); err != nil {
		return what + ". runner_start failed: " + err.Error() + "; " + runnerFix
	}
	return what + ". Ran runner_start without waiting for it. Carry on; check later with quipu runner status --json"
}

// enforcedRunnerStart reads the command from the default branch as it is now. A
// worktree's copy never runs, and without an origin nothing is enforced.
func enforcedRunnerStart(w worktree) string {
	c, source, err := loadEnforcedConfig(w, true)
	if err != nil || source == configSourceWorktree {
		return ""
	}
	return c.Workflows.RunnerStart
}

// runnerStamp is the stamp shared by every worktree of the clone.
func runnerStamp(w worktree) (string, error) { return w.commonPath("quipu-runner-start") }

// claimRunnerStart lets one agent in the clone start the runner per
// runnerStartEvery. The lock is what makes it one when several start at once.
func claimRunnerStart(w worktree, now time.Time) (runnerClaim, error) {
	path, err := runnerStamp(w)
	if err != nil {
		return runnerClaim{}, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return runnerClaim{}, err
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		return runnerClaim{}, err
	}
	if raw, err := os.ReadFile(path); err == nil {
		when, failed, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
		if last, err := time.Parse(time.RFC3339, when); err == nil && now.Sub(last) < runnerStartEvery {
			return runnerClaim{failed: failed}, nil
		}
	}
	if err := os.WriteFile(path, []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return runnerClaim{}, err
	}
	return runnerClaim{won: true}, nil
}

// recordRunnerFailure adds why a start failed to the stamp, so the agents it
// holds back hear that instead of being told a start is under way.
func recordRunnerFailure(w worktree, failure error) {
	path, err := runnerStamp(w)
	if err != nil {
		return
	}
	stamp, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer stamp.Close()
	fmt.Fprintln(stamp, strings.ReplaceAll(failure.Error(), "\n", " "))
}

// runDetached starts command in dir and returns once it has failed, finished, or
// had the grace period. Past that it keeps running, in its own session, with its
// output in logPath.
func runDetached(dir, command, logPath string, grace time.Duration) error {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, log, log
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	select {
	case err := <-finished:
		if err != nil {
			return fmt.Errorf("%w (output in %s)", err, logPath)
		}
	case <-time.After(grace):
	}
	return nil
}

// ensureRunner is runnerStarter against live GitHub. It reports on stderr too,
// because a failing command prints only its error on stdout.
func ensureRunner(c model.Config, w worktree) string {
	note := runnerStarter{
		down: func() []doctorCheck {
			var down []doctorCheck
			for _, check := range runnerChecks(c, w) {
				if check.Status == "fail" {
					down = append(down, check)
				}
			}
			return down
		},
		command: func() string { return enforcedRunnerStart(w) },
		claim:   func() (runnerClaim, error) { return claimRunnerStart(w, time.Now()) },
		run: func(command string) error {
			// Not the worktree: there a relative path would run the agent's copy
			// of a script, and only the default branch decides what runs.
			home, err := os.UserHomeDir()
			if err == nil {
				err = runDetached(home, command, filepath.Join(w.GitDir, "quipu-runner-start.log"), runnerStartGrace)
			}
			if err != nil {
				recordRunnerFailure(w, err)
			}
			return err
		},
	}.ensure()
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	return note
}

// queuedJob is a job waiting for a runner.
type queuedJob struct {
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Labels  []string `json:"labels"`
	Verdict string   `json:"verdict"`
	Detail  string   `json:"detail"`
}

type runnerState struct {
	Name   string   `json:"name"`
	State  string   `json:"state"`
	Labels []string `json:"labels"`
}

// runnerStatus is the report of `quipu runner status`.
type runnerStatus struct {
	Repository  string        `json:"repository"`
	State       string        `json:"state"` // known | partial | unknown
	Detail      string        `json:"detail,omitempty"`
	Runners     []runnerState `json:"runners"`
	Needs       []doctorCheck `json:"needs"`
	Queued      []queuedJob   `json:"queued"`
	QueuedError string        `json:"queued_error,omitempty"`
	WriterRun   string        `json:"writer_last_success"`
	RunnerStart string        `json:"runner_start"`
}

func buildRunnerStatus(c model.Config, facts runnerFacts, needs []doctorCheck, queued []queuedJob, queuedErr error, lastRun time.Time, lastErr error, now time.Time) runnerStatus {
	report := runnerStatus{Repository: c.Owner + "/" + c.Repository, State: "known", Runners: []runnerState{}, Needs: needs, Queued: []queuedJob{}, RunnerStart: "unset"}
	switch {
	case facts.Err != nil:
		report.State, report.Detail = "unknown", "this token cannot read the repository's runners: "+facts.Err.Error()
	case facts.Partial:
		report.State, report.Detail = "partial", "this token cannot read organization runners"
	}
	for _, runner := range facts.Runners {
		state := "offline"
		if runner.Online {
			state = "online"
		}
		report.Runners = append(report.Runners, runnerState{runner.Name, state, runner.Labels})
	}
	for _, job := range queued {
		verdict, detail := judgeRunners(job.Labels, facts)
		job.Detail = detail
		switch verdict {
		case runnerAvailable:
			job.Verdict = "will start"
		case runnerUnknown:
			job.Verdict = "unknown"
		default:
			job.Verdict = "cannot start"
		}
		report.Queued = append(report.Queued, job)
	}
	if queuedErr != nil {
		report.QueuedError = queuedErr.Error()
	}
	switch {
	case lastErr != nil:
		report.WriterRun = "unknown: " + lastErr.Error()
	case lastRun.IsZero():
		report.WriterRun = "never"
	default:
		report.WriterRun = lastRun.UTC().Format(time.RFC3339) + " (" + now.Sub(lastRun).Round(time.Minute).String() + " ago)"
	}
	if c.Workflows.RunnerStart != "" {
		report.RunnerStart = "set"
	}
	return report
}

// loadQueuedJobs lists the jobs waiting for a runner. A run with a job running
// can have others waiting, so in-progress runs are read too.
func loadQueuedJobs(c model.Config) ([]queuedJob, error) {
	repo := "repos/" + c.Owner + "/" + c.Repository
	jobs := []queuedJob{}
	for _, status := range []string{"queued", "in_progress"} {
		output, err := ghOutput("api", repo+"/actions/runs?per_page=30&status="+status)
		if err != nil {
			return jobs, err
		}
		var runs struct {
			Runs []struct {
				ID int64 `json:"id"`
			} `json:"workflow_runs"`
		}
		if err := json.Unmarshal(output, &runs); err != nil {
			return jobs, err
		}
		for _, run := range runs.Runs {
			output, err := ghOutput("api", fmt.Sprintf("%s/actions/runs/%d/jobs?per_page=100", repo, run.ID))
			if err != nil {
				return jobs, err
			}
			parsed, err := parseQueuedJobs(output)
			if err != nil {
				return jobs, err
			}
			jobs = append(jobs, parsed...)
		}
	}
	return jobs, nil
}

func parseQueuedJobs(output []byte) ([]queuedJob, error) {
	var response struct {
		Jobs []struct {
			Name    string   `json:"name"`
			Status  string   `json:"status"`
			HTMLURL string   `json:"html_url"`
			Labels  []string `json:"labels"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, err
	}
	var jobs []queuedJob
	for _, job := range response.Jobs {
		if job.Status == "queued" {
			jobs = append(jobs, queuedJob{Name: job.Name, URL: job.HTMLURL, Labels: job.Labels})
		}
	}
	return jobs, nil
}

// writerRunnerLabel names a runner kept free for the serial Writer.
const writerRunnerLabel = "quipu-writer"

// runnerSetup is what a repository's self-hosted runners need.
type runnerSetup struct {
	Labels [][]string `json:"labels"`
	Steps  []string   `json:"steps"`
}

// planRunnerSetup lists the steps to keep the runners this config needs well
// set up. It only prints them: a runner's machine may not be this one.
func planRunnerSetup(c model.Config, jobs []workflowJob) runnerSetup {
	setup := runnerSetup{Labels: [][]string{}, Steps: []string{}}
	seen := map[string]bool{}
	for _, need := range runnerNeeds(c, jobs) {
		key := strings.Join(need.labels, ",")
		if len(need.labels) == 0 || hostedLabels(need.labels) || seen[key] {
			continue
		}
		seen[key] = true
		setup.Labels = append(setup.Labels, need.labels)
	}
	if len(setup.Labels) == 0 {
		setup.Steps = append(setup.Steps, "This repository needs no self-hosted runner.")
		return setup
	}
	pin := c.Workflows.QuipuVersion
	if pin == "" {
		pin = "main"
	}
	url := "https://github.com/" + c.Owner + "/" + c.Repository
	for _, labels := range setup.Labels {
		setup.Steps = append(setup.Steps, fmt.Sprintf("Register a runner with these labels. Get a token under %s/settings/actions/runners/new, then run: ./config.sh --url %s --token TOKEN --labels %s", url, url, strings.Join(labels, ",")))
	}
	setup.Steps = append(setup.Steps,
		"Install it as a service, so it survives a reboot: ./svc.sh install && ./svc.sh start",
		fmt.Sprintf("Put quipu at the pinned version on the runner's PATH, because a quipu on the PATH wins over the pin: GOBIN=\"$HOME/.local/bin\" go install github.com/isnakolah/quipu/cmd/quipu@%s. Add that directory to the runner's .path file, then restart the service.", pin),
		"Install git and gh on the runner.",
	)
	if c.Workflows.RunnerStart == "" {
		setup.Steps = append(setup.Steps, "Optional: set workflows.runner_start in quipu.yml to a command that starts this runner from an agent's machine, so agents can start it when it is down.")
	}
	if c.Workflows.SerialWriter() && !hasRunnerLabel(c.Workflows.Runner("writer"), writerRunnerLabel) {
		setup.Steps = append(setup.Steps, fmt.Sprintf("The serial Writer answers quickly only while a runner is free, and a build holding the only runner makes claims wait. To answer during builds, add a second runner that only the Writer can use: ./config.sh --url %s --no-default-labels --labels %s, then set workflows.runs_on.writer: [%s] in quipu.yml and regenerate the workflows.", url, writerRunnerLabel, writerRunnerLabel))
	}
	setup.Steps = append(setup.Steps, "Check it: quipu doctor, then quipu runner status --json.")
	return setup
}

// runnerCommand is `quipu runner status|setup`. Both only read and print.
func runnerCommand(args []string) error {
	if len(args) == 0 || (args[0] != "status" && args[0] != "setup") {
		return errors.New("usage: quipu runner {status|setup} [--json]")
	}
	fs := flag.NewFlagSet("runner "+args[0], flag.ContinueOnError)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	c, _, err := loadEnforcedConfig(here, true)
	if err != nil {
		return err
	}
	if args[0] == "setup" {
		setup := planRunnerSetup(c, readWorkflowJobs(c, here))
		if *asJSON {
			return printValue(setup, true)
		}
		for i, step := range setup.Steps {
			fmt.Printf("%d. %s\n", i+1, step)
		}
		return nil
	}
	now := time.Now()
	facts := loadRunnerFacts(c)
	queued, queuedErr := loadQueuedJobs(c)
	lastRun, lastErr := lastWriterRun(c)
	report := buildRunnerStatus(c, facts, judgeRunnerNeeds(c, readWorkflowJobs(c, here), func() runnerFacts { return facts }), queued, queuedErr, lastRun, lastErr, now)
	if *asJSON {
		return printValue(report, true)
	}
	fmt.Printf("Runners for %s (%s)\n", report.Repository, report.State)
	if report.Detail != "" {
		fmt.Println("  " + report.Detail)
	}
	for _, runner := range report.Runners {
		fmt.Printf("  %s: %s [%s]\n", runner.Name, runner.State, strings.Join(runner.Labels, ", "))
	}
	fmt.Println("Needed")
	for _, need := range report.Needs {
		fmt.Printf("  %s %s: %s\n", strings.ToUpper(need.Status), need.Name, need.Detail)
	}
	if len(report.Queued) > 0 {
		fmt.Println("Queued")
		for _, job := range report.Queued {
			fmt.Printf("  %s: %s, %s\n", job.Name, job.Verdict, job.Detail)
		}
	}
	fmt.Println("Writer's last successful run: " + report.WriterRun)
	return nil
}

// runnerConfigChecks are what doctor adds for a repository's runners beyond
// whether they are up: the checks `quipu runner setup` steps lead to.
func runnerConfigChecks(c model.Config, pathVersion string) []doctorCheck {
	var checks []doctorCheck
	pin := c.Workflows.QuipuVersion
	if pathVersion != "" && pin != "" && pin != "main" && pathVersion != pin {
		checks = append(checks, doctorCheck{"quipu version", "warn", "quipu on this machine's PATH is " + pathVersion + " but workflows.quipu_version is " + pin + "; if this machine runs a runner, the PATH copy wins", "owner: see quipu runner setup"})
	}
	if c.MergesInBackground() && c.Workflows.Cron() == "" {
		checks = append(checks, doctorCheck{"merge_wait", "warn", "merge_wait: background with workflows.schedule off: unless the Writer workflow has been regenerated to run when checks finish, a queued merge waits for an Issue event", "owner: quipu adopt --apply --replace, or set a schedule"})
	}
	return checks
}

// writerModeCheck warns when the committed Writer workflow isn't the one the config
// asks for, which is the state between setting the flag and regenerating.
func writerModeCheck(c model.Config, workflow string, found bool) []doctorCheck {
	if !found {
		return nil
	}
	serial := strings.Contains(workflow, "quipu writer serve")
	switch {
	case c.Workflows.SerialWriter() && !serial:
		return []doctorCheck{{"writer mode", "warn", "workflows.writer_mode is serial, but the Writer workflow still starts a run per event", "owner: update workflows.quipu_version and the runner's quipu first, then regenerate quipu-writer.yml with quipu adopt --apply --replace and merge it through a PR"}}
	case !c.Workflows.SerialWriter() && serial:
		return []doctorCheck{{"writer mode", "warn", "the Writer workflow is serial, but workflows.writer_mode is not set", "owner: set workflows.writer_mode: serial, or regenerate the workflow to go back"}}
	}
	return nil
}

// pathQuipuVersion is the version of the quipu that hooks and runners find.
func pathQuipuVersion() string {
	output, err := exec.Command("quipu", "version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}
