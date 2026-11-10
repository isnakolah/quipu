package main

// After `quipu done` frees a worktree, its holder may still tidy it up for
// done_grace_minutes: run the repository's cleanup or a well-known build
// cleaner, `git clean`, and remove files git does not track. Nothing that
// records or publishes work passes: commits, pushes and edits are judged as
// with no lease, as before. Deleting a merged local work branch is allowed at
// any time, because git itself refuses `-d` on unmerged work.

import (
	"path/filepath"
	"strings"

	policycheck "github.com/isnakolah/quipu/internal/policy"
)

// wellKnownCleaners free build output in any repository.
var wellKnownCleaners = setOf("cargo clean", "go clean", "make clean", "make distclean")

// finishing returns dir's worktree while its tidy-up window is open and no
// lease has replaced it.
func (g *guard) finishing(dir string) (worktree, finishedRecord, bool) {
	w, err := openWorktree(existingDir(dir))
	if err != nil {
		return worktree{}, finishedRecord{}, false
	}
	if _, err := w.readLease(); err == nil {
		return worktree{}, finishedRecord{}, false
	}
	f, ok := w.readFinished(g.now)
	return w, f, ok
}

// sameCommand compares two command lines word by word.
func sameCommand(a, b string) bool {
	return b != "" && strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ")
}

// cleanupLine reports whether line is the repository's configured cleanup.
func cleanupLine(line, dir string) bool {
	c, err := enforcedConfigIn(dir)
	return err == nil && sameCommand(line, c.Cleanup)
}

// tidyUp reports whether one simple command only tidies a finished worktree.
func (g *guard) tidyUp(args []shellArg, dir string) bool {
	w, finished, ok := g.finishing(dir)
	if !ok {
		return false
	}
	words := make([]string, len(args))
	for i, arg := range args {
		if !arg.ok {
			return false
		}
		words[i] = arg.s
	}
	words[0] = filepath.Base(words[0])
	line := strings.Join(words, " ")
	if wellKnownCleaners[line] || sameCommand(line, finished.Cleanup) || cleanupLine(line, dir) {
		return true
	}
	switch words[0] {
	case "rm", "rmdir":
		return g.untrackedOnly(w, dir, words[1:])
	case "git":
		return len(words) > 1 && words[1] == "clean"
	}
	return false
}

// untrackedOnly reports whether every path names something inside w that git
// does not track: build output, not work.
func (g *guard) untrackedOnly(w worktree, dir string, args []string) bool {
	paths := 0
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		path := g.resolve(dir, arg)
		rel, err := filepath.Rel(w.Top, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || insideGitDir(path) {
			return false
		}
		if _, err := w.git("ls-files", "--error-unmatch", "--", rel); err == nil {
			return false
		}
		paths++
	}
	return paths > 0
}

// safeBranchDelete reports whether `git branch` args only delete Quipu work
// branches with -d, none checked out in a worktree. git refuses -d on a branch
// that is not merged, so no work can be lost.
func safeBranchDelete(args []shellArg, dir string) bool {
	var names []string
	deleting := false
	for _, arg := range args {
		switch {
		case !arg.ok:
			return false
		case arg.s == "-d" || arg.s == "--delete":
			deleting = true
		case strings.HasPrefix(arg.s, "-"):
			return false
		default:
			if _, ok := policycheck.BranchIssue(arg.s); !ok {
				return false
			}
			names = append(names, arg.s)
		}
	}
	if !deleting || len(names) == 0 {
		return false
	}
	w, err := openWorktree(existingDir(dir))
	if err != nil {
		return false
	}
	porcelain, err := w.git("worktree", "list", "--porcelain")
	if err != nil {
		return false
	}
	for _, entry := range parseWorktreeList(porcelain) {
		for _, name := range names {
			if entry.Branch == name {
				return false
			}
		}
	}
	return true
}
