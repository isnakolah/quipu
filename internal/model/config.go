package model

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const ConfigVersion = 1

type Config struct {
	SchemaVersion    int      `yaml:"schema_version" json:"schema_version"`
	Owner            string   `yaml:"owner" json:"owner"`
	Repository       string   `yaml:"repository" json:"repository"`
	Project          Project  `yaml:"project" json:"project"`
	Areas            []string `yaml:"areas" json:"areas"`
	WikiMode         string   `yaml:"wiki_mode" json:"wiki_mode"`
	DefaultBranch    string   `yaml:"default_branch" json:"default_branch"`
	WriterVersion    string   `yaml:"writer_version" json:"writer_version"`
	AuthorizedActors []string `yaml:"authorized_actors" json:"authorized_actors"`
	Policy           Policy   `yaml:"policy" json:"policy"`
	// ReceiptsTrustedBefore keeps receipts posted before the Writer moved to
	// its bot identity trusted. Later receipts count only from the bot.
	ReceiptsTrustedBefore string `yaml:"receipts_trusted_before,omitempty" json:"receipts_trusted_before,omitempty"`
	// ReadyBy says who makes work Ready: "owner" (the default) or "agents".
	ReadyBy string `yaml:"ready_by,omitempty" json:"ready_by,omitempty"`
	// PolicyExemptAuthors are bot accounts, such as dependabot[bot], whose
	// PRs skip the work checks. Changes to Quipu's own files still need the
	// owner's approval label.
	PolicyExemptAuthors []string `yaml:"policy_exempt_authors,omitempty" json:"policy_exempt_authors,omitempty"`
	// RequiredChecks are the CI check names main requires besides policy.
	// Empty means ["test"], the job in Quipu's generated CI workflow.
	RequiredChecks []string `yaml:"required_checks,omitempty" json:"required_checks,omitempty"`
	// Merge says who merges work PRs: "owner" (the default) or "agent", in
	// which case `quipu done` merges once every required check is green.
	Merge string `yaml:"merge,omitempty" json:"merge,omitempty"`
	// CommitFormat "issue-prefix" requires `#NNN type(scope): summary`
	// commit subjects with a `Refs: #N` footer.
	CommitFormat string `yaml:"commit_format,omitempty" json:"commit_format,omitempty"`
	// IssuesBy "agents" lets agents create Stories, Tasks and Defects under an
	// existing Epic. New Epics stay proposals.
	IssuesBy string `yaml:"issues_by,omitempty" json:"issues_by,omitempty"`
	// TriageBy "agents" lets agents triage quipu:needs-triage Issues and file
	// Decisions and Gates, which they still never decide.
	TriageBy string `yaml:"triage_by,omitempty" json:"triage_by,omitempty"`
	// UpdateBehind "auto" makes `quipu done` merge the default branch into a
	// PR that is behind it. Needs merge: agent.
	UpdateBehind string `yaml:"update_behind,omitempty" json:"update_behind,omitempty"`
	// MergeWait "background" hands a PR whose checks are still running to the
	// Writer, which merges it once they pass. Needs merge: agent.
	MergeWait string `yaml:"merge_wait,omitempty" json:"merge_wait,omitempty"`
	// MergeWithoutCI lets `quipu done` merge while required checks have not run,
	// once LocalCheck and the policy rules pass in the worktree. A check that ran
	// and failed still blocks. Needs merge: agent and a LocalCheck.
	MergeWithoutCI bool `yaml:"merge_without_ci,omitempty" json:"merge_without_ci,omitempty"`
	// LocalCheck is the shell command merge_without_ci runs on the PR's head.
	LocalCheck string `yaml:"local_check,omitempty" json:"local_check,omitempty"`
	// MergeBypassRulesets lets a merge without CI use the admin bypass when a
	// ruleset requires the waived checks. Needs merge_without_ci.
	MergeBypassRulesets bool `yaml:"merge_bypass_rulesets,omitempty" json:"merge_bypass_rulesets,omitempty"`
	// Cleanup is a shell command that frees a linked worktree's disk. Quipu runs
	// it there after the last step that needs the build, so a repository's
	// agents never have to order it around local_check.
	Cleanup string `yaml:"cleanup,omitempty" json:"cleanup,omitempty"`
	// DoneGraceMinutes is how long, after `quipu done` frees a worktree, its
	// holder may still tidy it up: the cleanup, `git clean`, removing untracked
	// build output. Commits and pushes stay denied. Unset means 60; 0 turns it off.
	DoneGraceMinutes *int `yaml:"done_grace_minutes,omitempty" json:"done_grace_minutes,omitempty"`
	// AutoReady moves work to Ready when its last blocker closes, unless it is
	// labelled quipu:needs-owner. Decisions and Gates never move.
	AutoReady bool `yaml:"auto_ready,omitempty" json:"auto_ready,omitempty"`
	// Dispatch configures quipu daemon.
	Dispatch Dispatch `yaml:"dispatch,omitempty" json:"dispatch,omitempty"`
	// ScopedChecks choose what merge: queue and merge_without_ci run from the paths
	// a change touches.
	ScopedChecks []ScopedCheck `yaml:"scoped_checks,omitempty" json:"scoped_checks,omitempty"`
	// SlowChecks are workflows run on the default branch per batch or nightly.
	SlowChecks []SlowCheck `yaml:"slow_checks,omitempty" json:"slow_checks,omitempty"`
	// MergeQueue configures merge: queue.
	MergeQueue MergeQueue `yaml:"merge_queue,omitempty" json:"merge_queue,omitempty"`
	// Limits caps what agents may start on this machine. Unset, nothing is capped.
	Limits Limits `yaml:"limits,omitempty" json:"limits,omitempty"`
	// GuardReads "open" widens the commands the guard treats as read-only.
	GuardReads string    `yaml:"guard_reads,omitempty" json:"guard_reads,omitempty"`
	// Batches "epic" lets the leaf Issues of one Epic be worked on one rolling
	// epic-NNN/slug branch and merged together; unset or "none" keeps one
	// branch and PR per Issue.
	Batches string `yaml:"batches,omitempty" json:"batches,omitempty"`
	Workflows  Workflows `yaml:"workflows,omitempty" json:"workflows,omitempty"`
}

// Workflows shapes the generated GitHub Actions workflows.
type Workflows struct {
	// RunsOn maps a job (writer, policy, test) or "default" to runner labels,
	// such as [self-hosted, macOS]. Unset jobs use default, then ubuntu-latest.
	RunsOn map[string][]string `yaml:"runs_on,omitempty" json:"runs_on,omitempty"`
	// QuipuVersion is the tag or commit the workflows install. A runner that
	// already has quipu on its PATH uses that binary instead.
	QuipuVersion string `yaml:"quipu_version,omitempty" json:"quipu_version,omitempty"`
	// WriterMode "serial" has one Writer job apply requests in order, with events
	// only waking it. Empty keeps a run per event.
	WriterMode string `yaml:"writer_mode,omitempty" json:"writer_mode,omitempty"`
	// Schedule is the Writer's cron schedule, or "off". Empty means every
	// five minutes, or every fifteen with a serial Writer.
	Schedule string `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	// WriterOnPullRequest runs the Writer on PR events (default true). Turn
	// it off to save runner time; the schedule still rechecks PR policy.
	WriterOnPullRequest *bool `yaml:"writer_on_pull_request,omitempty" json:"writer_on_pull_request,omitempty"`
	// DefectsFrom names the workflows whose failed runs on the default
	// branch become Ready P0 Defects. Unset means [CI]; [] turns it off.
	DefectsFrom *[]string `yaml:"defects_from,omitempty" json:"defects_from,omitempty"`
	// RunnerStart is a shell command that starts the repository's runner. When a
	// needed runner is down, `quipu start` and `quipu done` run it from the home
	// directory of the agent's machine, without waiting. Only the default branch's
	// copy ever runs.
	RunnerStart string `yaml:"runner_start,omitempty" json:"runner_start,omitempty"`
}

// DefectWorkflows returns the workflows the Writer files Defects for.
func (w Workflows) DefectWorkflows() []string {
	if w.DefectsFrom == nil {
		return []string{"CI"}
	}
	return *w.DefectsFrom
}

// WorkflowJobs are the jobs whose runner the config may choose.
var WorkflowJobs = []string{"writer", "policy", "test"}

const DefaultSchedule = "*/5 * * * *"

// SerialSchedule is rarer: requests wake a serial Writer, so the schedule only
// sweeps for what leaves no request.
const SerialSchedule = "*/15 * * * *"

// Runner returns the runner labels for job.
func (w Workflows) Runner(job string) []string {
	if labels := w.RunsOn[job]; len(labels) > 0 {
		return labels
	}
	if labels := w.RunsOn["default"]; len(labels) > 0 {
		return labels
	}
	return []string{"ubuntu-latest"}
}

// Cron returns the Writer schedule, or "" when it is off.
func (w Workflows) Cron() string {
	switch w.Schedule {
	case "":
		if w.SerialWriter() {
			return SerialSchedule
		}
		return DefaultSchedule
	case "off":
		return ""
	}
	return w.Schedule
}

// SerialWriter reports whether one Writer job serves every request.
func (w Workflows) SerialWriter() bool { return w.WriterMode == "serial" }

// PullRequestTrigger reports whether the Writer runs on PR events.
func (w Workflows) PullRequestTrigger() bool {
	return w.WriterOnPullRequest == nil || *w.WriterOnPullRequest
}

// Checks returns the required CI checks, without policy.
func (c Config) Checks() []string {
	if len(c.RequiredChecks) == 0 {
		return []string{"test"}
	}
	return c.RequiredChecks
}

var (
	runnerLabel  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	cronSchedule = regexp.MustCompile(`^[0-9*/,-]+( [0-9*/,A-Za-z-]+){4}$`)
	quipuVersion = regexp.MustCompile(`^(v[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*|[0-9a-f]{7,40}|main)$`)
	checkName    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._/()-]*$`)
	workflowName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._/()-]*$`)
)

func (w Workflows) validate() error {
	if w.WriterMode != "" && w.WriterMode != "serial" {
		return fmt.Errorf("workflows.writer_mode must be serial or unset, not %q", w.WriterMode)
	}
	for job, labels := range w.RunsOn {
		known := job == "default"
		for _, name := range WorkflowJobs {
			known = known || job == name
		}
		if !known {
			return fmt.Errorf("workflows.runs_on: unknown job %q (use default, %s)", job, strings.Join(WorkflowJobs, ", "))
		}
		if len(labels) == 0 {
			return fmt.Errorf("workflows.runs_on.%s needs at least one runner label", job)
		}
		for _, label := range labels {
			if !runnerLabel.MatchString(label) {
				return fmt.Errorf("workflows.runs_on.%s: invalid runner label %q", job, label)
			}
		}
	}
	if w.QuipuVersion != "" && !quipuVersion.MatchString(w.QuipuVersion) {
		return fmt.Errorf("workflows.quipu_version must be a tag like v0.2.0 or a commit SHA, not %q", w.QuipuVersion)
	}
	for _, name := range w.DefectWorkflows() {
		if !workflowName.MatchString(name) {
			return fmt.Errorf("workflows.defects_from: invalid workflow name %q", name)
		}
	}
	if strings.ContainsAny(w.RunnerStart, "\n\r") {
		return fmt.Errorf("workflows.runner_start must be one line; put anything longer in a script outside the repository")
	}
	if w.Schedule != "" && w.Schedule != "off" && !cronSchedule.MatchString(w.Schedule) {
		return fmt.Errorf("workflows.schedule must be a five-field cron expression or off, not %q", w.Schedule)
	}
	return nil
}

// AgentsMerge reports whether `quipu done` merges green PRs.
func (c Config) AgentsMerge() bool { return c.Merge == "agent" }

// AgentsMakeReady reports whether agents may run `quipu ready`.
func (c Config) AgentsMakeReady() bool { return c.ReadyBy == "agents" }

// AgentsCreateIssues reports whether agents may run `quipu issue create`.
func (c Config) AgentsCreateIssues() bool { return c.IssuesBy == "agents" }

// AgentsTriage reports whether agents may run `quipu triage`.
func (c Config) AgentsTriage() bool { return c.TriageBy == "agents" }

// UpdatesBehind reports whether `quipu done` brings a behind PR up to date.
func (c Config) UpdatesBehind() bool { return c.AgentsMerge() && c.UpdateBehind == "auto" }

// MergesInBackground reports whether the Writer merges PRs whose checks are
// still running when `quipu done` runs.
func (c Config) MergesInBackground() bool { return c.AgentsMerge() && c.MergeWait == "background" }

// MergesWithoutCI reports whether `quipu done` may waive required checks that
// have not run.
func (c Config) MergesWithoutCI() bool { return c.AgentsMerge() && c.MergeWithoutCI }

// BypassesRulesets reports whether a merge without CI may use the admin bypass.
func (c Config) BypassesRulesets() bool {
	return (c.MergesWithoutCI() || c.MergesInQueue()) && c.MergeBypassRulesets
}

// Dispatch configures quipu daemon.
type Dispatch struct {
	// AgentCommand runs through sh in each new worktree. It gets QUIPU_ISSUE,
	// QUIPU_BRANCH, QUIPU_WORKTREE and QUIPU_PROMPT.
	AgentCommand string `yaml:"agent_command,omitempty" json:"agent_command,omitempty"`
	// MaxAgents caps agents at once on this clone; limits.build_slots caps builds
	// across the machine. Zero means 2.
	MaxAgents int `yaml:"max_agents,omitempty" json:"max_agents,omitempty"`
	// PollSeconds is how often it looks. Zero means 60.
	PollSeconds int `yaml:"poll_seconds,omitempty" json:"poll_seconds,omitempty"`
}

// Dispatches reports whether quipu daemon has a command to run.
func (c Config) Dispatches() bool { return c.Dispatch.AgentCommand != "" }

// DispatchMaxAgents is the agents quipu daemon runs at once.
func (c Config) DispatchMaxAgents() int {
	if c.Dispatch.MaxAgents > 0 {
		return c.Dispatch.MaxAgents
	}
	return 2
}

// DispatchPoll is how often quipu daemon looks.
func (c Config) DispatchPoll() time.Duration {
	if c.Dispatch.PollSeconds > 0 {
		return time.Duration(c.Dispatch.PollSeconds) * time.Second
	}
	return time.Minute
}

// ScopedCheck runs Command when a change touches Paths. With no Command, those paths
// need no check at all, which is how docs skip builds.
type ScopedCheck struct {
	Command string   `yaml:"command,omitempty" json:"command,omitempty"`
	Paths   []string `yaml:"paths" json:"paths"`
}

// SlowCheck is a workflow too slow for every change.
type SlowCheck struct {
	Workflow string `yaml:"workflow" json:"workflow"`
	When     string `yaml:"when" json:"when"` // batch | nightly
}

// ScopeChoice is what to run for a change, and why, for the evidence.
type ScopeChoice struct {
	// Command is what to run. Empty means nothing needs to.
	Command string
	Reason  string
}

// ScopedCommand picks what a change runs. A path no entry names runs the full check:
// forgetting to list a directory must cost a build, never skip one.
func (c Config) ScopedCommand(paths []string, full string) ScopeChoice {
	if len(c.ScopedChecks) == 0 {
		return ScopeChoice{Command: full}
	}
	if len(paths) == 0 {
		return ScopeChoice{Command: full, Reason: "scoped_checks: no changed paths to go on, so the full check ran"}
	}
	chosen := map[int]bool{}
	for _, path := range paths {
		named := false
		for i, check := range c.ScopedChecks {
			for _, glob := range check.Paths {
				if GlobMatches(glob, path) {
					named, chosen[i] = true, true
				}
			}
		}
		if !named {
			return ScopeChoice{Command: full, Reason: fmt.Sprintf("scoped_checks: %s is named by no entry, so the full check ran", path)}
		}
	}
	var run, skipped []string
	for i, check := range c.ScopedChecks {
		if check.Command == "" || slices.Contains(run, check.Command) {
			continue
		}
		if chosen[i] {
			run = append(run, check.Command)
		}
	}
	for i, check := range c.ScopedChecks {
		if check.Command != "" && !chosen[i] && !slices.Contains(run, check.Command) && !slices.Contains(skipped, check.Command) {
			skipped = append(skipped, check.Command)
		}
	}
	shown := paths
	if len(shown) > 5 {
		shown = shown[:5]
	}
	changed := strings.Join(shown, ", ")
	if len(run) == 0 {
		return ScopeChoice{Reason: fmt.Sprintf("scoped_checks: nothing ran, because the changed paths need no check: %s", changed)}
	}
	reason := "scoped_checks: ran " + strings.Join(run, " and ") + " for " + changed
	if len(skipped) > 0 {
		reason += "; skipped " + strings.Join(skipped, " and ") + ", because none of its paths changed"
	}
	return ScopeChoice{Command: strings.Join(run, " && "), Reason: reason}
}

// GlobMatches matches a repository path: * and ? stay within a directory, and **
// crosses them.
func GlobMatches(glob, path string) bool {
	pattern, err := globPattern(glob)
	return err == nil && pattern.MatchString(path)
}

func globPattern(glob string) (*regexp.Regexp, error) {
	if glob == "" || strings.HasPrefix(glob, "/") || slices.Contains(strings.Split(glob, "/"), "..") {
		return nil, fmt.Errorf("%q must be a path relative to the repository root", glob)
	}
	var out strings.Builder
	out.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch {
		case strings.HasPrefix(glob[i:], "**/"):
			out.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			out.WriteString(".*")
			i++
		case glob[i] == '*':
			out.WriteString("[^/]*")
		case glob[i] == '?':
			out.WriteString("[^/]")
		default:
			out.WriteString(regexp.QuoteMeta(string(glob[i])))
		}
	}
	out.WriteString("$")
	return regexp.Compile(out.String())
}

// ValidateAutonomy checks dispatch, scoped_checks and slow_checks.
func (c Config) ValidateAutonomy() error {
	if g := c.DoneGraceMinutes; g != nil && (*g < 0 || *g > 24*60) {
		return fmt.Errorf("done_grace_minutes must be between 0 and 1440")
	}
	d := c.Dispatch
	if d.AgentCommand == "" && (d.MaxAgents != 0 || d.PollSeconds != 0) {
		return fmt.Errorf("dispatch needs agent_command")
	}
	if d.MaxAgents < 0 || d.PollSeconds < 0 {
		return fmt.Errorf("dispatch.max_agents and dispatch.poll_seconds cannot be negative")
	}
	if len(c.ScopedChecks) > 0 && !c.MergesInQueue() && !c.MergesWithoutCI() {
		return fmt.Errorf("scoped_checks choose what merge: queue or merge_without_ci runs; neither is on")
	}
	for i, check := range c.ScopedChecks {
		if len(check.Paths) == 0 {
			return fmt.Errorf("scoped_checks[%d] needs paths", i)
		}
		for _, glob := range check.Paths {
			if _, err := globPattern(glob); err != nil {
				return fmt.Errorf("scoped_checks[%d]: %w", i, err)
			}
		}
	}
	for i, check := range c.SlowChecks {
		switch check.When {
		case "nightly":
		case "batch":
			if !c.MergesInQueue() {
				return fmt.Errorf("slow_checks[%d]: when: batch needs merge: queue", i)
			}
		default:
			return fmt.Errorf("slow_checks[%d]: when must be batch or nightly, not %q", i, check.When)
		}
		if !workflowName.MatchString(check.Workflow) {
			return fmt.Errorf("slow_checks[%d]: invalid workflow %q", i, check.Workflow)
		}
		// A failure no one files is a check no one runs.
		if !slices.Contains(c.Workflows.DefectWorkflows(), check.Workflow) {
			return fmt.Errorf("slow_checks[%d]: %q must be in workflows.defects_from, so that its failure opens a Defect", i, check.Workflow)
		}
	}
	return nil
}

// MergeQueue configures merge: queue: one worker lands batches of PRs, tested
// together. Empty numbers take their defaults.
type MergeQueue struct {
	// Check is the shell command a batch must pass.
	Check string `yaml:"check,omitempty" json:"check,omitempty"`
	// BatchSize is how many PRs a batch takes. Default 4.
	BatchSize int `yaml:"batch_size,omitempty" json:"batch_size,omitempty"`
	// MaxWaitMinutes is how long a PR waits for a batch to fill. Default 30.
	MaxWaitMinutes int `yaml:"max_wait_minutes,omitempty" json:"max_wait_minutes,omitempty"`
	// FlakyRetries is how many more times a failed check runs before it counts as
	// a failure. Nil means 1.
	FlakyRetries *int `yaml:"flaky_retries,omitempty" json:"flaky_retries,omitempty"`
	// CheckTimeoutMinutes ends a check that hangs, which says nothing about the
	// code. Default 120.
	CheckTimeoutMinutes int `yaml:"check_timeout_minutes,omitempty" json:"check_timeout_minutes,omitempty"`
}

// MergesInQueue reports whether a local worker lands PRs in tested batches.
func (c Config) MergesInQueue() bool { return c.Merge == "queue" }

// QueueBatchSize is how many PRs a batch takes.
func (c Config) QueueBatchSize() int {
	if c.MergeQueue.BatchSize > 0 {
		return c.MergeQueue.BatchSize
	}
	return 4
}

// QueueMaxWait is how long a PR waits for a batch to fill.
func (c Config) QueueMaxWait() time.Duration {
	if c.MergeQueue.MaxWaitMinutes > 0 {
		return time.Duration(c.MergeQueue.MaxWaitMinutes) * time.Minute
	}
	return 30 * time.Minute
}

// QueueFlakyRetries is how many more runs a failed check gets.
func (c Config) QueueFlakyRetries() int {
	if c.MergeQueue.FlakyRetries != nil {
		return *c.MergeQueue.FlakyRetries
	}
	return 1
}

// QueueCheckTimeout is how long a batch's check may run.
func (c Config) QueueCheckTimeout() time.Duration {
	if c.MergeQueue.CheckTimeoutMinutes > 0 {
		return time.Duration(c.MergeQueue.CheckTimeoutMinutes) * time.Minute
	}
	return 2 * time.Hour
}

// ValidateMergeQueue checks merge: queue and its settings.
func (c Config) ValidateMergeQueue() error {
	q := c.MergeQueue
	if !c.MergesInQueue() {
		if q.Check != "" || q.BatchSize != 0 || q.MaxWaitMinutes != 0 || q.FlakyRetries != nil || q.CheckTimeoutMinutes != 0 {
			return fmt.Errorf("merge_queue needs merge: queue")
		}
		return nil
	}
	if strings.TrimSpace(q.Check) == "" {
		return fmt.Errorf("merge: queue needs merge_queue.check")
	}
	if strings.ContainsAny(q.Check, "\n\r") {
		return fmt.Errorf("merge_queue.check must be one line")
	}
	if q.BatchSize < 0 || q.MaxWaitMinutes < 0 || q.CheckTimeoutMinutes < 0 || (q.FlakyRetries != nil && *q.FlakyRetries < 0) {
		return fmt.Errorf("merge_queue must not be negative")
	}
	if c.MergeWait == "background" {
		return fmt.Errorf("merge: queue and merge_wait: background disagree: pick one")
	}
	if c.MergeWithoutCI {
		return fmt.Errorf("merge: queue already skips CI before merge, so it can't be combined with merge_without_ci")
	}
	return nil
}

// Limits caps work across every repository and worktree on one machine. Each
// repository's own numbers apply to claims in that repository.
type Limits struct {
	// BuildSlots is how many worktrees may build at once. 0 is no limit.
	BuildSlots int `yaml:"build_slots,omitempty" json:"build_slots,omitempty"`
	// BuildAreas are the areas whose work counts as a build. Empty means all.
	BuildAreas []string `yaml:"build_areas,omitempty" json:"build_areas,omitempty"`
	// MinFreeDiskGB stops new claims when the disk falls under it. 0 is no floor.
	MinFreeDiskGB int `yaml:"min_free_disk_gb,omitempty" json:"min_free_disk_gb,omitempty"`
	// SlotIdleMinutes is how long a slot lasts without its agent making a tool
	// call. Empty means DefaultSlotIdleMinutes.
	SlotIdleMinutes int `yaml:"slot_idle_minutes,omitempty" json:"slot_idle_minutes,omitempty"`
}

// DefaultSlotIdleMinutes outlasts an agent thinking, and most single builds.
const DefaultSlotIdleMinutes = 45

// LimitsBuilds reports whether build slots are capped.
func (c Config) LimitsBuilds() bool { return c.Limits.BuildSlots > 0 }

// LimitsMachine reports whether anything is capped on this machine.
func (c Config) LimitsMachine() bool { return c.LimitsBuilds() || c.Limits.MinFreeDiskGB > 0 }

// CountsAsBuild reports whether work in this area takes a build slot. Work with no
// area does, since nothing says it doesn't. The Project titles its area values, so
// "app" in the config is "App" on the board.
func (c Config) CountsAsBuild(area string) bool {
	if len(c.Limits.BuildAreas) == 0 || area == "" {
		return true
	}
	return slices.ContainsFunc(c.Limits.BuildAreas, func(build string) bool { return strings.EqualFold(build, area) })
}

// SlotIdleMinutes is how long a quiet agent keeps its slot.
func (c Config) SlotIdleMinutes() int {
	if c.Limits.SlotIdleMinutes > 0 {
		return c.Limits.SlotIdleMinutes
	}
	return DefaultSlotIdleMinutes
}

// ValidateLimits checks the limits section.
func (c Config) ValidateLimits() error {
	l := c.Limits
	if l.BuildSlots < 0 || l.MinFreeDiskGB < 0 || l.SlotIdleMinutes < 0 {
		return fmt.Errorf("limits must not be negative")
	}
	if l.BuildSlots == 0 && (len(l.BuildAreas) > 0 || l.SlotIdleMinutes > 0) {
		return fmt.Errorf("limits.build_areas and limits.slot_idle_minutes need limits.build_slots")
	}
	for _, area := range l.BuildAreas {
		if !slices.Contains(c.Areas, area) {
			return fmt.Errorf("limits.build_areas names %q, which is not in areas", area)
		}
	}
	return nil
}

// DefaultDoneGraceMinutes is the tidy-up window after done when none is set.
const DefaultDoneGraceMinutes = 60

// DoneGrace is how long a finished worktree may still be tidied up.
func (c Config) DoneGrace() time.Duration {
	if c.DoneGraceMinutes == nil {
		return DefaultDoneGraceMinutes * time.Minute
	}
	return time.Duration(*c.DoneGraceMinutes) * time.Minute
}

// CleansUp reports whether Quipu frees a worktree's disk when it is finished.
func (c Config) CleansUp() bool { return c.Cleanup != "" }

// BatchesByEpic reports whether leaf Issues share their Epic's rolling branch.
func (c Config) BatchesByEpic() bool { return c.Batches == "epic" }

// GuardReadsOpen reports whether the guard allows its wider read-only list.
func (c Config) GuardReadsOpen() bool { return c.GuardReads == "open" }

// WithoutRelaxations turns off every setting that relaxes enforcement, for a
// config that cannot be trusted to have come from the default branch.
func (c Config) WithoutRelaxations() Config {
	c.ReadyBy, c.Merge, c.PolicyExemptAuthors = "", "", nil
	c.IssuesBy, c.TriageBy, c.UpdateBehind, c.MergeWait, c.GuardReads = "", "", "", "", ""
	c.MergeWithoutCI, c.LocalCheck, c.MergeBypassRulesets = false, "", false
	c.Cleanup, c.DoneGraceMinutes = "", nil
	c.MergeQueue = MergeQueue{}
	c.AutoReady, c.Dispatch, c.ScopedChecks, c.SlowChecks = false, Dispatch{}, nil, nil
	c.Workflows.RunnerStart = ""
	return c
}

type Project struct {
	ID     string            `yaml:"id" json:"id"`
	Number int               `yaml:"number" json:"number"`
	Title  string            `yaml:"title" json:"title"`
	Fields map[string]string `yaml:"fields" json:"fields"`
}

type Policy struct {
	LeaseTTLMinutes int `yaml:"lease_ttl_minutes" json:"lease_ttl_minutes"`
	ReconcileMins   int `yaml:"reconcile_minutes" json:"reconcile_minutes"`
}

func (c Config) Validate() error {
	if c.SchemaVersion != ConfigVersion {
		return fmt.Errorf("unsupported schema_version %d", c.SchemaVersion)
	}
	if strings.TrimSpace(c.Owner) == "" || strings.TrimSpace(c.Repository) == "" {
		return fmt.Errorf("owner and repository are required")
	}
	if c.DefaultBranch == "" {
		return fmt.Errorf("default_branch is required")
	}
	if c.WikiMode != "off" && c.WikiMode != "journal" {
		return fmt.Errorf("wiki_mode must be off or journal")
	}
	if len(c.Areas) == 0 {
		return fmt.Errorf("at least one area is required")
	}
	if len(c.AuthorizedActors) == 0 {
		return fmt.Errorf("at least one authorized actor is required")
	}
	actors := map[string]bool{}
	for _, actor := range c.AuthorizedActors {
		actor = strings.ToLower(strings.TrimSpace(actor))
		if actor == "" || actors[actor] {
			return fmt.Errorf("authorized actors must be nonempty and unique")
		}
		actors[actor] = true
	}
	if c.ReadyBy != "" && c.ReadyBy != "owner" && c.ReadyBy != "agents" {
		return fmt.Errorf("ready_by must be owner or agents")
	}
	for _, author := range c.PolicyExemptAuthors {
		// Only bots: a person's login is shared with their agents.
		if !strings.HasSuffix(author, "[bot]") {
			return fmt.Errorf("policy_exempt_authors accepts only bot accounts like dependabot[bot], not %q", author)
		}
	}
	if c.Merge != "" && c.Merge != "owner" && c.Merge != "agent" && c.Merge != "queue" {
		return fmt.Errorf("merge must be owner, agent, or queue")
	}
	if c.CommitFormat != "" && c.CommitFormat != "issue-prefix" {
		return fmt.Errorf("commit_format must be issue-prefix or unset")
	}
	if c.IssuesBy != "" && c.IssuesBy != "owner" && c.IssuesBy != "agents" {
		return fmt.Errorf("issues_by must be owner or agents")
	}
	if c.TriageBy != "" && c.TriageBy != "owner" && c.TriageBy != "agents" {
		return fmt.Errorf("triage_by must be owner or agents")
	}
	if c.UpdateBehind != "" && c.UpdateBehind != "off" && c.UpdateBehind != "auto" {
		return fmt.Errorf("update_behind must be off or auto")
	}
	if c.UpdateBehind == "auto" && !c.AgentsMerge() {
		return fmt.Errorf("update_behind: auto needs merge: agent")
	}
	if c.MergeWait != "" && c.MergeWait != "foreground" && c.MergeWait != "background" {
		return fmt.Errorf("merge_wait must be foreground or background")
	}
	if c.MergeWait == "background" && !c.AgentsMerge() {
		return fmt.Errorf("merge_wait: background needs merge: agent")
	}
	if c.MergeWithoutCI && !c.AgentsMerge() {
		return fmt.Errorf("merge_without_ci needs merge: agent")
	}
	if c.MergeWithoutCI && strings.TrimSpace(c.LocalCheck) == "" {
		return fmt.Errorf("merge_without_ci needs a local_check")
	}
	if c.MergeWithoutCI && c.MergeWait == "background" {
		return fmt.Errorf("merge_without_ci and merge_wait: background disagree: one waits for the checks and the other doesn't; pick one")
	}
	if c.LocalCheck != "" && !c.MergeWithoutCI {
		return fmt.Errorf("local_check needs merge_without_ci")
	}
	if strings.ContainsAny(c.LocalCheck, "\n\r") {
		return fmt.Errorf("local_check must be one line")
	}
	if c.MergeBypassRulesets && !c.MergeWithoutCI && !c.MergesInQueue() {
		return fmt.Errorf("merge_bypass_rulesets needs merge_without_ci or merge: queue")
	}
	if strings.ContainsAny(c.Cleanup, "\n\r") {
		return fmt.Errorf("cleanup must be one line")
	}
	if err := c.ValidateLimits(); err != nil {
		return err
	}
	if err := c.ValidateAutonomy(); err != nil {
		return err
	}
	if err := c.ValidateMergeQueue(); err != nil {
		return err
	}
	if c.Batches != "" && c.Batches != "none" && c.Batches != "epic" {
		return fmt.Errorf("batches must be none or epic")
	}
	if c.GuardReads != "" && c.GuardReads != "closed" && c.GuardReads != "open" {
		return fmt.Errorf("guard_reads must be closed or open")
	}
	if err := c.Workflows.validate(); err != nil {
		return err
	}
	seenCheck := map[string]bool{}
	for _, check := range c.RequiredChecks {
		if !checkName.MatchString(check) || check == "policy" || seenCheck[check] {
			return fmt.Errorf("required_checks must be unique CI check names other than policy, not %q", check)
		}
		seenCheck[check] = true
	}
	if c.ReceiptsTrustedBefore != "" {
		if _, err := time.Parse(time.RFC3339, c.ReceiptsTrustedBefore); err != nil {
			return fmt.Errorf("receipts_trusted_before must be RFC3339")
		}
	}
	seen := map[string]bool{}
	for _, area := range c.Areas {
		if area == "" || seen[area] {
			return fmt.Errorf("areas must be nonempty and unique")
		}
		seen[area] = true
	}
	return nil
}
