package main

import (
	"errors"
	"fmt"
	"os"

	policycheck "github.com/isnakolah/quipu/internal/policy"
)

// verifyCommitCommand is for commit-msg hooks: `quipu verify-commit "$1"`.
// It passes every message unless the repository sets commit_format, and
// checks the subject names the Issue this worktree holds.
func verifyCommitCommand(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: quipu verify-commit FILE  (from a commit-msg hook)")
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	c, err := loadConfigAt(here.Top)
	if err != nil || c.CommitFormat != policycheck.IssuePrefix {
		return nil
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
	return nil
}
