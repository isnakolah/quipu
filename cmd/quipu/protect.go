package main

// Repository protection and setup checks. `quipu protect` makes GitHub refuse
// what the flow forbids: direct pushes and force pushes to the default
// branch, merging without the required checks, and branch names that do not
// map to an Issue. `quipu doctor` verifies all of it, plus the local guard.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
)

// actionsAppID is GitHub Actions; required checks must come from it, so no
// one can satisfy them by posting a commit status by hand.
const actionsAppID = 15368

const branchRulesetName = "quipu-branch-names"

func protectionPayload() map[string]any {
	return map[string]any{
		"required_status_checks": map[string]any{
			"strict": true,
			"checks": []map[string]any{{"context": "test", "app_id": actionsAppID}, {"context": "policy", "app_id": actionsAppID}},
		},
		"enforce_admins": true,
		"required_pull_request_reviews": map[string]any{
			"dismiss_stale_reviews": true, "required_approving_review_count": 0,
		},
		"restrictions":                     nil,
		"required_conversation_resolution": true,
		"allow_force_pushes":               false,
		"allow_deletions":                  false,
	}
}

// branchRuleset lets only NNN/slug work branches (and Dependabot) be created.
func branchRuleset(defaultBranch string) map[string]any {
	return map[string]any{
		"name":        branchRulesetName,
		"target":      "branch",
		"enforcement": "active",
		"conditions": map[string]any{"ref_name": map[string]any{
			"include": []string{"~ALL"},
			"exclude": []string{"refs/heads/" + defaultBranch, "refs/heads/[0-9][0-9][0-9]*/*", "refs/heads/dependabot/**"},
		}},
		"rules": []map[string]any{{"type": "creation"}},
	}
}

func protectCommand(args []string) error {
	fs := flag.NewFlagSet("protect", flag.ContinueOnError)
	apply := applyFlag(fs)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	plan := map[string]any{"repository": c.Owner + "/" + c.Repository, "branch_protection": protectionPayload(), "ruleset": branchRuleset(c.DefaultBranch), "apply": *apply}
	if !*apply {
		return printValue(plan, *asJSON)
	}
	if err := protectRepository(c); err != nil {
		return err
	}
	return printValue(plan, *asJSON)
}

// protectRepository applies main protection and the branch-name ruleset.
// It needs an admin token, so the owner runs it.
func protectRepository(c model.Config) error {
	repo := "repos/" + c.Owner + "/" + c.Repository
	if err := ghJSON("PUT", repo+"/branches/"+c.DefaultBranch+"/protection", protectionPayload()); err != nil {
		return fmt.Errorf("protect %s: %w", c.DefaultBranch, err)
	}
	id, err := rulesetID(repo)
	if err != nil {
		return err
	}
	if id == 0 {
		return ghJSON("POST", repo+"/rulesets", branchRuleset(c.DefaultBranch))
	}
	return ghJSON("PUT", fmt.Sprintf("%s/rulesets/%d", repo, id), branchRuleset(c.DefaultBranch))
}

func rulesetID(repo string) (int, error) {
	output, err := ghOutput("api", repo+"/rulesets")
	if err != nil {
		return 0, err
	}
	var rulesets []struct {
		ID          int    `json:"id"`
		Name        string `json:"name"`
		Enforcement string `json:"enforcement"`
	}
	if err := json.Unmarshal(output, &rulesets); err != nil {
		return 0, err
	}
	for _, ruleset := range rulesets {
		if ruleset.Name == branchRulesetName {
			return ruleset.ID, nil
		}
	}
	return 0, nil
}

func ghJSON(method, path string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	cmd := exec.Command("gh", "api", "-X", method, path, "--input", "-")
	cmd.Stdin = strings.NewReader(string(raw))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh api %s %s: %s", method, path, strings.TrimSpace(string(output)))
	}
	return nil
}

// ---- doctor ----

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// errReported marks a failure whose report is already printed.
var errReported = errors.New("reported")

func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	checks := doctorChecks()
	failed := 0
	for _, check := range checks {
		if check.Status == "fail" {
			failed++
		}
	}
	report := map[string]any{"quipu_version": version, "ok": failed == 0, "checks": checks}
	if *asJSON {
		if err := printValue(report, true); err != nil {
			return err
		}
	} else {
		for _, check := range checks {
			line := fmt.Sprintf("%-4s %s", strings.ToUpper(check.Status), check.Name)
			if check.Detail != "" {
				line += ": " + check.Detail
			}
			fmt.Println(line)
			if check.Fix != "" && check.Status != "ok" {
				fmt.Println("     fix: " + check.Fix)
			}
		}
	}
	if failed > 0 {
		return errReported
	}
	return nil
}

func doctorChecks() []doctorCheck {
	var checks []doctorCheck
	add := func(name, status, detail, fix string) {
		checks = append(checks, doctorCheck{name, status, detail, fix})
	}
	if _, err := exec.LookPath("quipu"); err != nil {
		add("quipu on PATH", "fail", "the agent hooks call `quipu`", "GOBIN=\"$HOME/.local/bin\" go install ./cmd/quipu")
	} else {
		add("quipu on PATH", "ok", "", "")
	}
	if github.NewClient().Token == "" {
		add("GitHub auth", "fail", "no GITHUB_TOKEN and no gh login", "gh auth login")
	} else {
		add("GitHub auth", "ok", "", "")
	}
	here, err := currentWorktree()
	if err != nil {
		add("git worktree", "fail", err.Error(), "run quipu inside the repository")
		return checks
	}
	c, err := loadConfigAt(here.Top)
	if err != nil {
		add("config", "fail", err.Error(), "quipu init or quipu adopt")
		return checks
	}
	add("config", "ok", c.Owner+"/"+c.Repository, "")
	for _, name := range []string{"QUIPU_LEASE_ISSUE", "QUIPU_LEASE_HOLDER", "QUIPU_LEASE_BRANCH", "QUIPU_BOOTSTRAP_ADMIN"} {
		if os.Getenv(name) != "" {
			add("environment", "fail", name+" is no longer used", "unset "+name)
		}
	}
	if os.Getenv("QUIPU_ACTOR") != "" {
		add("environment", "warn", "QUIPU_ACTOR gives every worktree the same lease identity", "unset QUIPU_ACTOR unless one agent works alone")
	}
	checks = append(checks, hookChecks(here.Top)...)
	if lease, err := here.readLease(); err == nil {
		add("this worktree", "ok", fmt.Sprintf("holds #%d on %s as %s", lease.Issue, lease.Branch, lease.Holder), "")
	} else {
		add("this worktree", "ok", "no lease; start work with quipu start --next --json", "")
	}
	checks = append(checks, githubChecks(c)...)
	return checks
}

// hookChecks verifies the project-level guard hooks, which apply to every
// Claude Code and Codex session in the repository, plugin or not.
func hookChecks(top string) []doctorCheck {
	var checks []doctorCheck
	for _, file := range []string{".claude/settings.json", ".codex/hooks.json"} {
		raw, err := os.ReadFile(filepath.Join(top, file))
		switch {
		case err != nil:
			checks = append(checks, doctorCheck{"guard hooks", "fail", file + " missing", "quipu hooks --apply"})
		case !strings.Contains(string(raw), "quipu guard"):
			checks = append(checks, doctorCheck{"guard hooks", "fail", file + " does not run quipu guard", "quipu hooks --apply"})
		default:
			checks = append(checks, doctorCheck{"guard hooks", "ok", file, ""})
		}
	}
	return checks
}

func githubChecks(c model.Config) []doctorCheck {
	var checks []doctorCheck
	repo := "repos/" + c.Owner + "/" + c.Repository
	output, err := ghOutput("api", repo+"/branches/"+c.DefaultBranch+"/protection")
	switch {
	case err != nil && strings.Contains(err.Error(), "Branch not protected"):
		checks = append(checks, doctorCheck{"branch protection", "fail", c.DefaultBranch + " is not protected", "owner: quipu protect --apply"})
	case err != nil:
		checks = append(checks, doctorCheck{"branch protection", "warn", "cannot read protection (needs admin)", "owner: quipu doctor"})
	default:
		var p struct {
			Checks struct {
				Contexts []string `json:"contexts"`
			} `json:"required_status_checks"`
			Admins struct {
				Enabled bool `json:"enabled"`
			} `json:"enforce_admins"`
			Force struct {
				Enabled bool `json:"enabled"`
			} `json:"allow_force_pushes"`
		}
		_ = json.Unmarshal(output, &p)
		contexts := strings.Join(p.Checks.Contexts, ",")
		if !strings.Contains(contexts, "test") || !strings.Contains(contexts, "policy") || !p.Admins.Enabled || p.Force.Enabled {
			checks = append(checks, doctorCheck{"branch protection", "fail", "required checks " + contexts + ", enforce admins " + fmt.Sprint(p.Admins.Enabled), "owner: quipu protect --apply"})
		} else {
			checks = append(checks, doctorCheck{"branch protection", "ok", "checks " + contexts + ", admins enforced", ""})
		}
	}
	if id, err := rulesetID(repo); err != nil {
		checks = append(checks, doctorCheck{"branch names", "warn", "cannot read rulesets", "owner: quipu doctor"})
	} else if id == 0 {
		checks = append(checks, doctorCheck{"branch names", "fail", "no " + branchRulesetName + " ruleset", "owner: quipu protect --apply"})
	} else {
		checks = append(checks, doctorCheck{"branch names", "ok", branchRulesetName, ""})
	}
	output, err = ghOutput("api", repo+"/actions/secrets", "--jq", "[.secrets[].name]")
	switch {
	case err != nil:
		checks = append(checks, doctorCheck{"Writer token", "warn", "cannot list secrets (needs admin)", "owner: quipu doctor"})
	case !strings.Contains(string(output), "QUIPU_WRITER_TOKEN"):
		checks = append(checks, doctorCheck{"Writer token", "fail", "QUIPU_WRITER_TOKEN secret missing; no work can be claimed", "owner: create a classic PAT with public_repo and project, then gh secret set QUIPU_WRITER_TOKEN"})
	default:
		checks = append(checks, doctorCheck{"Writer token", "ok", "", ""})
	}
	return checks
}
