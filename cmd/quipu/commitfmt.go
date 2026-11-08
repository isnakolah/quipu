package main

import (
	"flag"
	"fmt"
	"os"

	policycheck "github.com/isnakolah/quipu/internal/policy"
)

// verifyCommitCommand is for commit-msg hooks: `quipu verify-commit "$1"`.
// It passes every message unless the repository sets commit_format, and
// checks the subject names the Issue this worktree holds.
func verifyCommitCommand(args []string) error {
	fs := flag.NewFlagSet("verify-commit", flag.ContinueOnError)
	asJSON := jsonFlag(fs)
	files, err := parsePositional(fs, args)
	if err != nil || len(files) != 1 {
		return keepHelp(err, "usage: quipu verify-commit FILE [--json]  (from a commit-msg hook)")
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		return err
	}
	pass := func() error {
		if *asJSON {
			return printValue(map[string]any{"valid": true, "file": files[0]}, true)
		}
		return nil
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	c, err := loadConfigAt(here.Top)
	if err != nil || c.CommitFormat != policycheck.IssuePrefix {
		return pass()
	}
	issue := 0
	if lease, err := here.readLease(); err == nil {
		issue = lease.Issue
	}
	if _, err := policycheck.CheckIssuePrefix(string(raw), issue); err != nil {
		example := "#NNN feat(scope): summary\n\nRefs: #N"
		if issue != 0 {
			example = fmt.Sprintf("#%03d feat(scope): summary\n\nRefs: #%d", issue, issue)
		}
		return flowErr("commit-format", "%v. This repository writes commits as:\n\n%s", err, example)
	}
	return pass()
}
