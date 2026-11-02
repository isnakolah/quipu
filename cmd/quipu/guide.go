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
	{model.Config.AgentsTriage, "- **`triage_by: agents`:** you may triage an Issue labeled `quipu:needs-triage` with `quipu triage N --json` and `--kind`, `--status backlog|blocked`, `--priority`, `--area`, or `--parent`; it adds the Issue to the Project if needed. `--status ready` needs `ready_by: agents`. You may file a Decision or Gate under an open Epic with `quipu issue create`, but never decide one. Triage never sets Done or Cancelled and never closes anything.\n"},
	{model.Config.UpdatesBehind, "- **`update_behind: auto`:** when your PR is behind the default branch, `quipu done` merges the default branch in and pushes. The checks then run again, so rerun it. A conflict stops with `merge-conflict` and leaves your branch as it was.\n"},
	{model.Config.MergesInBackground, "- **`merge_wait: background`:** when your evidence is complete and only checks are still running, `quipu done` returns at once and frees this worktree. The Writer holds your evidence and merges when the checks pass. If one fails, the Issue returns to Ready with its branch, and the receipt says why. So do not wait: take other work. Pushing to the branch again takes it out of the queue; rerun `quipu done` to queue it again.\n"},
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
	if c.UpdatesBehind() {
		text += "\n| Code | What happened | Next |\n|---|---|---|\n"
		for _, kind := range flagErrors {
			text += fmt.Sprintf("| `%s` | %s | %s |\n", kind.Code, cell(kind.When), cell(kind.Next))
		}
	}
	return text
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
