package main

// `quipu guide` prints the agent handbook. `--sync` writes it where agents
// read it: .agent/rules/quipu.md, and in the Quipu source repository the
// plugin skills. AGENTS.md and CLAUDE.md are created as short pointers only
// when missing; a project's own copies are never modified. TestGuidesInSync
// fails when a committed copy drifts from the handbook or the rule tables.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/isnakolah/quipu/internal/guide"
	"github.com/isnakolah/quipu/internal/model"
)

// guideRule is where the handbook lives in every Quipu repository.
const guideRule = ".agent/rules/quipu.md"

const agentsPointer = "# Agent instructions\n\nThis repository runs on Quipu. Before any change, read .agent/rules/quipu.md (or run `quipu guide`): take work with `quipu start --next --json`, open PRs whose body says `Refs #N`, finish with `quipu done N --json`, and follow each result's `next`. Everything off that flow is blocked.\n"

const claudePointer = "# Claude Code\n\nThis repository runs on Quipu; its agent rules are imported below.\n\n@.agent/rules/quipu.md\n"

const skillFrontmatter = `---
name: quipu
description: Work in a Quipu repository. Take GitHub Issues with quipu start, open PRs that say Refs #N, finish with quipu done, and follow the guard. Use whenever the repository has quipu.yml or .github/quipu/config.yaml, or the user mentions Quipu.
---

`

// flagRule is what the handbook adds when a flag is on, so each repository's
// agents read the rules that apply to them. With every flag off it adds nothing.
type flagRule struct {
	on   func(model.Config) bool
	text string
}

var flagRules = []flagRule{
	{model.Config.AgentsCreateIssues, "- **`issues_by: agents`:** you may create a Story, Task, or Defect under an open Epic with `quipu issue create --kind story --area AREA --parent EPIC --title TITLE --body-file CONTRACT.md --apply`. The body must be a valid work contract. New Epics stay proposals: use `quipu propose`.\n"},
	{model.Config.AgentsTriage, "- **`triage_by: agents`:** you may triage an Issue labeled `quipu:needs-triage` with `quipu triage N --json` and `--kind`, `--status`, `--priority`, `--area`, or `--parent`; it adds the Issue to the Project if needed. `--status` takes `backlog`, `ready`, or `blocked`, for a move the lifecycle allows, and `ready` needs `ready_by: agents`. `--parent` must be an open Epic. You may file a Decision or Gate under an open Epic with `quipu issue create`, but never decide one. Triage never sets Done or Cancelled and never closes anything.\n"},
	{model.Config.UpdatesBehind, "- **`update_behind: auto`:** when your PR is behind the default branch, `quipu done` merges the default branch in and pushes. The checks then run again, so rerun it. With `merge_without_ci`, it runs `local_check` on the new head and merges in the same call, and if the default branch keeps moving it stops with `base-moved`. A conflict stops with `merge-conflict` and leaves your branch as it was.\n"},
	{model.Config.MergesInBackground, "- **`merge_wait: background`:** when your evidence is complete and only checks are still running, `quipu done` returns at once and frees this worktree. The Writer holds your evidence and merges when the checks pass. If one fails, the Issue returns to Ready with its branch, and the receipt says why. So do not wait: take other work. Pushing to the branch again takes it out of the queue; rerun `quipu done` to queue it again.\n"},
	{model.Config.MergesWithoutCI, "- **`merge_without_ci`:** when required checks are pending, skipped, never ran, or were cancelled before starting, `quipu done` merges anyway, once this worktree is clean and at the PR's head, the policy rules pass, and `local_check` exits 0. Run it with `--boundary local`, never `CI`. A check that ran and failed still blocks. The evidence records that the merge skipped CI. If the merge lands and the Writer does not answer, rerun the same command.\n"},
	{model.Config.CleansUp, "- **`cleanup`:** do not run this repository's cleanup yourself. Quipu runs it in your worktree after the last step that needs the build, including when `quipu worktree remove` takes the worktree away. If it fails, the result's `warnings` says so, and the work still counts as finished.\n"},
	{model.Config.LimitsMachine, "- **`limits`:** this machine caps how much runs at once, across every repository on it. `quipu start` may return `\"status\": \"Busy\"` with a `reason` and claim nothing: finish any work you hold, and when none is left, report the reason and stop. Don't poll. Work you already hold always resumes, and `--next` first offers Ready work that needs no build slot. A long `local_check` holds a slot too, and stops with `machine-busy` when none is free. Your slot lasts while you make tool calls, and frees itself when you go quiet or your lease ends.\n"},
	{func(c model.Config) bool { return c.Workflows.SerialWriter() }, "- **`writer_mode: serial`:** one Writer job serves every request, so answers come faster while a runner is free, and wait while none is. Nothing changes in what you run. A Writer run that shows as cancelled or skipped is normal: a pending run was replaced, or a comment wasn't a request. Don't rerun it.\n"},
	{model.Config.MergesInQueue, "- **`merge: queue`:** `quipu done N --json` with every evidence flag and `--boundary local` queues your PR and returns at once, freeing this worktree and its build slot, so take other work. Run the check yourself first if you want to, but the queue runs the repository's check on the PR merged with the latest default branch and the PRs ahead of it. If it passes, the queue lands your PR and the Writer closes the Issue. If your PR is at fault or conflicts, it returns to In review with the failure attached: `quipu status --json` lists it under `queue.returned`. Run `quipu start N --json`, fix it, push, and run `quipu done` again; another worktree of the same login can take it over this way. Pushing after queueing takes the PR out of the queue. `quipu queue status --json` shows the queue. You never run the worker.\n"},
	{func(c model.Config) bool { return c.AutoReady }, "- **`auto_ready`:** work whose last blocker closed becomes Ready by itself at the Writer's next sweep, unless it is labelled `quipu:needs-owner`. You don't need to ask for it.\n"},
	{func(c model.Config) bool { return len(c.ScopedChecks) > 0 }, "- **`scoped_checks`:** the merge check runs only what the paths you changed call for, so a docs-only change may run none. A path no entry names runs the full check. The evidence says what ran and what didn't; you don't choose.\n"},
	{model.Config.Dispatches, "- **`dispatch`:** `quipu daemon` may have started you, with your lease already claimed. Your work is in `QUIPU_ISSUE`, on `QUIPU_BRANCH`, in `QUIPU_WORKTREE`, and `QUIPU_PROMPT` says what to do.\n"},
	{model.Config.GuardDenyList, "- **`guard: deny-list`:** the guard refuses only what is dangerous: force pushes and pushes to the default branch, merging, approving or closing outside Quipu, another agent's worktree or branch, Quipu's own files, commands this repository lists under `guard_deny`, and changes to a governed worktree's files without a lease (commits, edits, redirects and `rm`/`mv` into it). Reads, interpreters, loops, `--help`, disk checks and writes outside governed worktrees pass. A denial names the part of the line it refused (`denied: ...`); the rest of the line was fine.\n"},
	{model.Config.GuardReadsOpen, "- **`guard_reads: open`:** the guard also treats more read-only commands as reads, such as `git tag --sort`. Interpreters stay denied, and so does everything else in the handbook.\n"},
}

// renderGuideFor is the handbook for a repository's effective config.
func renderGuideFor(c model.Config) string {
	var rules strings.Builder
	for _, rule := range flagRules {
		if rule.on(c) {
			rules.WriteString(rule.text)
		}
	}
	if rules.Len() == 0 {
		return renderGuide()
	}
	text := renderGuide() + "\n## Flags on in this repository\n\nThe owner turned these on in `quipu.yml`. Everything above still applies, except where they say otherwise.\n\n" + rules.String()
	var rows strings.Builder
	for _, kind := range flagErrors {
		if flagErrorOn[kind.Code](c) {
			fmt.Fprintf(&rows, "| `%s` | %s | %s |\n", kind.Code, cell(kind.When), cell(kind.Next))
		}
	}
	if rows.Len() > 0 {
		text += "\n| Code | What happened | Next |\n|---|---|---|\n" + rows.String()
	}
	return text
}

// flagErrorOn says which flag can cause each of flagErrors.
var flagErrorOn = map[string]func(model.Config) bool{
	"merge-conflict":     model.Config.UpdatesBehind,
	"machine-busy":       model.Config.LimitsBuilds,
	"base-moved":         model.Config.UpdatesBehind,
	"local-check-failed": model.Config.MergesWithoutCI,
	"worktree-unclean":   model.Config.MergesWithoutCI,
	"policy-rules":       func(c model.Config) bool { return c.MergesWithoutCI() || c.MergesInQueue() },
}

// handbookCheck compares the committed handbook with what the config in force
// calls for, so a flag turned on or off cannot leave agents reading old rules.
func handbookCheck(current string, found bool, c model.Config) doctorCheck {
	switch {
	case !found:
		return doctorCheck{"agent handbook", "warn", guideRule + " is missing", "owner: quipu guide --sync, then commit it"}
	case current != renderGuideFor(c):
		return doctorCheck{"agent handbook", "warn", guideRule + " does not match this quipu and the flags in force", "owner: quipu guide --sync, then commit it"}
	}
	return doctorCheck{"agent handbook", "ok", guideRule, ""}
}

// renderGuide is the handbook with the CLI's own rule tables filled in.
func renderGuide() string {
	var rules strings.Builder
	rules.WriteString("| Code | Why | Do instead |\n|---|---|---|\n")
	for _, rule := range guardRules {
		fmt.Fprintf(&rules, "| `%s` | %s | %s |\n", rule.Code, cell(rule.Why), cell(rule.Instead))
	}
	var failures strings.Builder
	failures.WriteString("| Code | What happened | Next |\n|---|---|---|\n")
	for _, kind := range flowErrors {
		fmt.Fprintf(&failures, "| `%s` | %s | %s |\n", kind.Code, cell(kind.When), cell(kind.Next))
	}
	text := strings.Replace(guide.Agent, "{{guard-rules}}\n", rules.String(), 1)
	return strings.Replace(text, "{{flow-errors}}\n", failures.String(), 1)
}

func cell(text string) string { return strings.ReplaceAll(text, "|", `\|`) }

// guideFiles returns every generated file for the repository at top, given
// the current contents read through read.
func guideFiles(top string, read func(string) (string, bool)) map[string]string {
	return guideFilesFor(model.Config{}, top, read)
}

// guideFilesFor is guideFiles for a config. The plugin skills ship to every
// repository, so only the repository's own handbook reflects its flags.
func guideFilesFor(c model.Config, top string, read func(string) (string, bool)) map[string]string {
	handbook := renderGuide()
	files := map[string]string{guideRule: renderGuideFor(c)}
	for path, pointer := range map[string]string{"AGENTS.md": agentsPointer, "CLAUDE.md": claudePointer} {
		if current, ok := read(path); ok {
			files[path] = current // the project's own file: never modified
		} else {
			files[path] = pointer
		}
	}
	for _, provider := range []string{"claude", "codex"} {
		skill := filepath.Join("plugins", provider, "quipu", "skills", "quipu", "SKILL.md")
		if _, err := os.Stat(filepath.Join(top, filepath.Dir(skill))); err == nil {
			files[skill] = skillFrontmatter + handbook
		}
	}
	return files
}

// guideHints names the project's own instruction files that do not point at
// the handbook, so the owner can add the pointer by hand.
func guideHints(read func(string) (string, bool)) []string {
	var hints []string
	if current, ok := read("AGENTS.md"); ok && !strings.Contains(current, guideRule) {
		hints = append(hints, "AGENTS.md does not mention "+guideRule+"; add a line telling agents to read it")
	}
	if current, ok := read("CLAUDE.md"); ok && !strings.Contains(current, "@"+guideRule) {
		hints = append(hints, "CLAUDE.md does not import the rules; add the line @"+guideRule)
	}
	return hints
}

func readUnder(top string) func(string) (string, bool) {
	return func(path string) (string, bool) {
		raw, err := os.ReadFile(filepath.Join(top, path))
		return string(raw), err == nil
	}
}

// guideConfig is the config in force at top. With none readable, the handbook
// states the defaults, which grant nothing extra.
func guideConfig(top string) model.Config {
	w, err := openWorktree(top)
	if err != nil {
		return model.Config{}
	}
	c, _, err := loadEnforcedConfig(w, false)
	if err != nil {
		return model.Config{}
	}
	return c
}

// syncGuides writes the generated copies under top and returns those changed.
func syncGuides(top string, write bool) ([]string, error) {
	read := readUnder(top)
	var changed []string
	for path, body := range guideFilesFor(guideConfig(top), top, read) {
		if current, _ := read(path); current == body {
			continue
		}
		changed = append(changed, path)
		if !write {
			continue
		}
		target := filepath.Join(top, path)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return changed, err
		}
		if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

func guideCommand(args []string) error {
	fs := flag.NewFlagSet("guide", flag.ContinueOnError)
	sync := fs.Bool("sync", false, "write the handbook to .agent/rules/quipu.md (and AGENTS.md/CLAUDE.md pointers when missing)")
	check := fs.Bool("check", false, "exit non-zero when a generated copy is out of date")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*sync && !*check {
		fmt.Print(renderGuideFor(guideConfig(".")))
		return nil
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	// Read the default branch as it is now, so a flag just merged is written.
	_, _, _ = loadEnforcedConfig(here, true)
	changed, err := syncGuides(here.Top, *sync)
	if err != nil {
		return err
	}
	for _, path := range changed {
		fmt.Println(path)
	}
	for _, hint := range guideHints(readUnder(here.Top)) {
		fmt.Println("note: " + hint)
	}
	if *check && !*sync && len(changed) > 0 {
		return errors.New("agent guides are out of date; the owner runs `quipu guide --sync`")
	}
	return nil
}
