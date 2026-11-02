package model

import (
	"fmt"
	"regexp"
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
	// GuardReads "open" widens the commands the guard treats as read-only.
	GuardReads string    `yaml:"guard_reads,omitempty" json:"guard_reads,omitempty"`
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
	// Schedule is the Writer's cron schedule, or "off". Empty means every
	// five minutes.
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
		return DefaultSchedule
	case "off":
		return ""
	}
	return w.Schedule
}

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
func (c Config) BypassesRulesets() bool { return c.MergesWithoutCI() && c.MergeBypassRulesets }

// GuardReadsOpen reports whether the guard allows its wider read-only list.
func (c Config) GuardReadsOpen() bool { return c.GuardReads == "open" }

// WithoutRelaxations turns off every setting that relaxes enforcement, for a
// config that cannot be trusted to have come from the default branch.
func (c Config) WithoutRelaxations() Config {
	c.ReadyBy, c.Merge, c.PolicyExemptAuthors = "", "", nil
	c.IssuesBy, c.TriageBy, c.UpdateBehind, c.MergeWait, c.GuardReads = "", "", "", "", ""
	c.MergeWithoutCI, c.LocalCheck, c.MergeBypassRulesets = false, "", false
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
	if c.Merge != "" && c.Merge != "owner" && c.Merge != "agent" {
		return fmt.Errorf("merge must be owner or agent")
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
	if c.MergeBypassRulesets && !c.MergeWithoutCI {
		return fmt.Errorf("merge_bypass_rulesets needs merge_without_ci")
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
