package main

import (
	"flag"

	"github.com/isnakolah/quipu/internal/bootstrap"
)

// hooksCommand installs the project-level guard hooks for Claude Code and
// Codex, so every agent session in the repository is guarded.
func hooksCommand(args []string) error {
	fs := flag.NewFlagSet("hooks", flag.ContinueOnError)
	apply := applyFlag(fs)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	changed, err := bootstrap.InstallHooks(here.Top, *apply)
	if err != nil {
		return err
	}
	next := "commit the files; each developer approves the hooks once in Claude Code (/hooks) and Codex"
	if len(changed) == 0 {
		next = "hooks already installed"
	} else if !*apply {
		next = "rerun with --apply to write them"
	}
	return printValue(map[string]any{"files": append([]string{}, changed...), "apply": *apply, "next": next}, *asJSON)
}
