package main

// Freeing a finished worktree. Done releases the lease, and `quipu worktree
// remove` takes the worktree away for the agent that held it or a dispatcher:
// the guard denies agents the `rm -rf` and `git branch -D` this needs, and the
// repository's own cleanup command can't run under the guard once the lease is
// gone. Cleanup comes only from the default branch.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/isnakolah/quipu/internal/model"
)

// enforcedCleanup reads the cleanup command. A worktree's copy never runs.
func enforcedCleanup(c model.Config, source string) string {
	if source == configSourceWorktree {
		return ""
	}
	return c.Cleanup
}

// runCleanup runs the repository's cleanup in a worktree.
func runCleanup(top, command string) []string {
	exit, err := runLocalCheck(top, command)
	switch {
	case err != nil:
		return []string{fmt.Sprintf("cleanup %q could not run: %v", command, err)}
	case exit != 0:
		return []string{fmt.Sprintf("cleanup %q exited %d", command, exit)}
	}
	return nil
}

// leaseStillHeld reports whether a worktree's lease file still means something:
// the Issue is active, on this branch, under this holder. An expired lease kept by
// an open PR still counts, since its holder may renew it.
func leaseStillHeld(lease leaseContext, state liveWork) bool {
	if state.Holder != lease.Holder || state.Branch != lease.Branch {
		return false
	}
	switch state.Status {
	case "Claimed", "In progress", "In review", "Evidence pending":
		return true
	}
	return false
}

// worktreeEntry is one line of `git worktree list`.
type worktreeEntry struct{ Path, Branch string }

// parseWorktreeList reads `git worktree list --porcelain`. The main checkout
// comes first.
func parseWorktreeList(porcelain string) []worktreeEntry {
	var entries []worktreeEntry
	for _, line := range strings.Split(porcelain, "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			entries = append(entries, worktreeEntry{Path: path})
		} else if branch, ok := strings.CutPrefix(line, "branch refs/heads/"); ok && len(entries) > 0 {
			entries[len(entries)-1].Branch = branch
		}
	}
	return entries
}

var branchIssue = regexp.MustCompile(`^([0-9]+)/`)

// issueOfBranch is the Issue a work branch belongs to, or 0.
func issueOfBranch(branch string) int {
	match := branchIssue.FindStringSubmatch(branch)
	if match == nil {
		return 0
	}
	number, _ := strconv.Atoi(match[1])
	return number
}

// findRemovable resolves "N", "#N", or a path to a linked worktree of this clone.
func findRemovable(entries []worktreeEntry, target string, leaseIssue func(path string) int) (worktreeEntry, error) {
	if number, err := strconv.Atoi(strings.TrimPrefix(target, "#")); err == nil {
		for _, entry := range entries[min(1, len(entries)):] {
			if leaseIssue(entry.Path) == number || issueOfBranch(entry.Branch) == number {
				return entry, nil
			}
		}
		return worktreeEntry{}, flowErr("worktree-missing", "no linked worktree holds #%d", number)
	}
	wanted := realPath(target)
	for i, entry := range entries {
		if realPath(entry.Path) != wanted {
			continue
		}
		if i == 0 {
			return worktreeEntry{}, flowErr("worktree-missing", "%s is the main checkout, which is never removed", target)
		}
		return entry, nil
	}
	return worktreeEntry{}, flowErr("worktree-missing", "%s is not a worktree of this clone", target)
}

// removeWorktreeTree takes one linked worktree away. Work that isn't Done keeps
// whatever exists only here. Done work is on the default branch, so what's left
// is build output and the local branch.
func removeWorktreeTree(root worktree, entry worktreeEntry, cleanup string, live func(number int) (liveWork, error)) (flowResult, error) {
	target, err := openWorktree(entry.Path)
	if err != nil {
		return flowResult{}, err
	}
	number := issueOfBranch(entry.Branch)
	lease, leaseErr := target.readLease()
	if leaseErr == nil {
		number = lease.Issue
	}
	status := ""
	if number > 0 {
		state, err := live(number)
		if err != nil && !errors.Is(err, errNotInProject) {
			return flowResult{}, fmt.Errorf("could not read #%d, so %s stays: %w", number, target.Top, err)
		}
		status = state.Status
		if leaseErr == nil && leaseStillHeld(lease, state) {
			return flowResult{}, flowErr("worktree-held", "%s holds #%d, which is %s", target.Top, number, state.Status)
		}
	}
	if status != "Done" {
		if changes, err := target.git("status", "--porcelain"); err != nil {
			return flowResult{}, err
		} else if changes != "" {
			return flowResult{}, flowErr("worktree-dirty", "%s has changes, and #%d is not Done:\n%s", target.Top, number, changes)
		}
		if unpushed, err := target.git("rev-list", "HEAD", "--not", "--remotes=origin"); err != nil {
			return flowResult{}, err
		} else if unpushed != "" {
			return flowResult{}, flowErr("worktree-dirty", "%s has commits that aren't on origin, and #%d is not Done", target.Top, number)
		}
	}
	var warnings []string
	if cleanup != "" {
		warnings = runCleanup(target.Top, cleanup)
	}
	if _, err := root.git("worktree", "remove", "--force", target.Top); err != nil {
		return flowResult{}, err
	}
	if _, err := root.git("worktree", "prune"); err != nil {
		warnings = append(warnings, err.Error())
	}
	result := flowResult{Issue: number, Status: "Removed", Worktree: target.Top, Warnings: warnings, Next: "Pick the next task: quipu start --next --json"}
	if status == "Done" && entry.Branch != "" {
		if _, err := root.git("branch", "-D", entry.Branch); err != nil {
			result.Warnings = append(result.Warnings, err.Error())
		}
		return result, nil
	}
	result.Branch = entry.Branch
	return result, nil
}

func worktreeCommand(args []string) error {
	usage := errors.New("usage: quipu worktree remove (N | PATH) [--json]")
	if len(args) == 0 || args[0] != "remove" {
		return usage
	}
	fs := flag.NewFlagSet("worktree remove", flag.ContinueOnError)
	asJSON := jsonFlag(fs)
	var positional []string
	for rest := args[1:]; ; {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		positional, rest = append(positional, fs.Arg(0)), fs.Args()[1:]
	}
	if len(positional) != 1 {
		return usage
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	c, source, err := loadEnforcedConfig(here, true)
	if err != nil {
		return err
	}
	porcelain, err := here.git("worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	entries := parseWorktreeList(porcelain)
	entry, err := findRemovable(entries, positional[0], func(path string) int {
		if other, err := openWorktree(path); err == nil {
			if lease, err := other.readLease(); err == nil {
				return lease.Issue
			}
		}
		return 0
	})
	if err != nil {
		return err
	}
	root, err := openWorktree(entries[0].Path)
	if err != nil {
		return err
	}
	result, err := removeWorktreeTree(root, entry, enforcedCleanup(c, source), func(number int) (liveWork, error) { return loadLiveWork(c, number) })
	if err != nil {
		return err
	}
	// The process may have been standing in what it just removed.
	_ = os.Chdir(root.Top)
	return printFlowResult(result, *asJSON)
}
