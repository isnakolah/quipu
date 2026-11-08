package main

// One check per change. Each run of the merge check is kept under the clone's git
// directory, keyed by the tree it tested and the command's text, so `quipu check`,
// `quipu done` and the merge queue never run the same command on the same tree
// twice. A record is trusted only for that exact tree and command: a changed
// file, a changed command, or a merge from the default branch makes a new tree
// or a new command, and the check runs again. The directory is shared by the
// clone's worktrees, so the queue's worktree can see what an agent's passed;
// the guard keeps agents out of it.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/model"
)

// checkRecordsDir holds the records, in the clone's common git directory.
const checkRecordsDir = "quipu-checks"

// checkRecordAge is how long a record is kept. Reuse needs only the newest few.
const checkRecordAge = 14 * 24 * time.Hour

// checkRecord is one run of a check on one tree.
type checkRecord struct {
	Tree    string    `json:"tree"`
	Command string    `json:"command"`
	Result  string    `json:"result"` // pass or fail
	Exit    int       `json:"exit"`
	At      time.Time `json:"at"`
}

var treeID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func checkRecordPath(w worktree, tree, command string) (string, error) {
	if !treeID.MatchString(tree) {
		return "", fmt.Errorf("%q is not a tree", tree)
	}
	dir, err := w.commonPath(checkRecordsDir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(tree + "\x00" + command))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json"), nil
}

// saveCheckRecord keeps a run's result. A later run on the same tree and command
// replaces it, so a failure after a pass is never covered by the pass.
func saveCheckRecord(w worktree, tree, command string, exit int, at time.Time) error {
	path, err := checkRecordPath(w, tree, command)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	pruneCheckRecords(filepath.Dir(path), at)
	result := "pass"
	if exit != 0 {
		result = "fail"
	}
	raw, err := json.MarshalIndent(checkRecord{Tree: tree, Command: command, Result: result, Exit: exit, At: at.UTC()}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// passedCheck finds a passing run of exactly this command on exactly this tree.
func passedCheck(w worktree, tree, command string) (checkRecord, bool) {
	path, err := checkRecordPath(w, tree, command)
	if err != nil {
		return checkRecord{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return checkRecord{}, false
	}
	var record checkRecord
	if json.Unmarshal(raw, &record) != nil {
		return checkRecord{}, false
	}
	if record.Tree != tree || record.Command != command || record.Result != "pass" || record.Exit != 0 {
		return checkRecord{}, false
	}
	return record, true
}

func pruneCheckRecords(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > checkRecordAge {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

// workingTree is the tree of the worktree as it stands, untracked files included
// and ignored ones not: what `git add -A && git commit` would commit. It uses a
// copy of the index, so the real one is never touched.
func workingTree(w worktree) (string, error) {
	tmp, err := os.CreateTemp("", "quipu-index-")
	if err != nil {
		return "", err
	}
	index := tmp.Name()
	tmp.Close()
	defer os.Remove(index)
	if raw, err := os.ReadFile(filepath.Join(w.GitDir, "index")); err == nil {
		if err := os.WriteFile(index, raw, 0o600); err != nil {
			return "", err
		}
	} else {
		// git reads a missing index as empty, and an empty file as broken.
		_ = os.Remove(index)
	}
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", w.Top}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
		}
		return strings.TrimSpace(string(output)), nil
	}
	if _, err := git("add", "-A"); err != nil {
		return "", err
	}
	return git("write-tree")
}

// mergeCheck is the command the merge would run on tree: the queue's check under
// merge: queue, otherwise local_check, narrowed by scoped_checks from what the
// tree changes since it left the default branch. That is what `quipu done` and a
// one-PR batch compute, so their record lookups find this run.
func mergeCheck(c model.Config, w worktree, tree string) (model.ScopeChoice, error) {
	full := c.LocalCheck
	if c.MergesInQueue() {
		full = c.MergeQueue.Check
	}
	if full == "" {
		return model.ScopeChoice{}, flowErr("no-check", "this repository sets no local_check or merge_queue.check on its default branch")
	}
	if len(c.ScopedChecks) == 0 {
		return model.ScopeChoice{Command: full}, nil
	}
	base, err := w.git("merge-base", "origin/"+c.DefaultBranch, "HEAD")
	if err != nil {
		return model.ScopeChoice{Command: full, Reason: "scoped_checks: could not read what changed, so the full check ran"}, nil
	}
	changed, err := w.git("diff", "--name-only", base, tree)
	if err != nil {
		return model.ScopeChoice{Command: full, Reason: "scoped_checks: could not read what changed, so the full check ran"}, nil
	}
	return c.ScopedCommand(changedPaths(changed), full), nil
}

// checkResult is what `quipu check` reports.
type checkResult struct {
	Status   string   `json:"status"`
	Tree     string   `json:"tree"`
	Command  string   `json:"command,omitempty"`
	Scope    string   `json:"scope,omitempty"`
	Result   string   `json:"result,omitempty"`
	At       string   `json:"at,omitempty"`
	Reused   bool     `json:"reused,omitempty"`
	Recorded bool     `json:"recorded"`
	Warnings []string `json:"warnings,omitempty"`
	Next     string   `json:"next"`
}

// checkOps are the outside effects of `quipu check`.
type checkOps struct {
	hold func() (func(), error)
	run  func(command string) (int, error)
	now  func() time.Time
}

const checkReuseNext = "Commit exactly this state, push, and run quipu done: it reuses this result instead of running the check again, as long as what it would test is this same tree. A change, or a merge from the default branch, runs it again."

// runCheck runs the merge check on the worktree as it stands and records the
// result for its tree. A tree that already passed this command is not run again
// unless rerun is set.
func runCheck(c model.Config, w worktree, rerun bool, ops checkOps) (checkResult, error) {
	tree, err := workingTree(w)
	if err != nil {
		return checkResult{}, err
	}
	choice, err := mergeCheck(c, w, tree)
	if err != nil {
		return checkResult{}, err
	}
	result := checkResult{Tree: tree, Command: choice.Command, Scope: choice.Reason}
	if choice.Command == "" {
		result.Status, result.Next = "Nothing to run", "scoped_checks call for no check on what this tree changes, so quipu done runs none either. Commit, push, and run quipu done."
		return result, nil
	}
	if record, ok := passedCheck(w, tree, choice.Command); ok && !rerun {
		result.Status, result.Result, result.At, result.Reused, result.Recorded = "Passed", record.Result, record.At.Format(time.RFC3339), true, true
		result.Next = "This tree already passed this check, so it did not run again (--rerun runs it). " + checkReuseNext
		return result, nil
	}
	release, err := ops.hold()
	if err != nil {
		return checkResult{}, err
	}
	exit, err := ops.run(choice.Command)
	release()
	if err != nil {
		return checkResult{}, flowErr("check-failed", "the check could not run: %v", err)
	}
	at := ops.now()
	after, err := workingTree(w)
	switch {
	case err != nil:
		result.Warnings = append(result.Warnings, "could not read the worktree after the check, so its result was not recorded: "+err.Error())
	case after != tree:
		result.Warnings = append(result.Warnings, fmt.Sprintf("the check changed the worktree (tree %s, now %s), so its result was not recorded; ignore what it writes, or stop it writing tracked files", tree, after))
	default:
		if err := saveCheckRecord(w, tree, choice.Command, exit, at); err != nil {
			result.Warnings = append(result.Warnings, "could not record the result: "+err.Error())
		} else {
			result.Recorded = true
		}
	}
	if exit != 0 {
		return checkResult{}, flowErr("check-failed", "the check %q exited %d on tree %s", choice.Command, exit, tree)
	}
	result.Status, result.Result, result.At = "Passed", "pass", at.UTC().Format(time.RFC3339)
	result.Next = checkReuseNext
	if !result.Recorded {
		result.Next = "Passed, but not recorded, so quipu done runs the check again. Commit, push, and run quipu done."
	}
	return result, nil
}

// checkSlot holds a machine build slot for a check, where limits ask for one.
func checkSlot(c model.Config, w worktree, number int) (func(), error) {
	m, err := openMachine()
	if err != nil {
		if c.LimitsBuilds() {
			return nil, flowErr("machine-busy", "local_check did not run: could not read this machine's slots: %v", err)
		}
		return func() {}, nil
	}
	hold, err := m.holdCheck(c, w, number, time.Now())
	if err != nil {
		return nil, err
	}
	return hold.drop, nil
}

// checkCommand is `quipu check`.
func checkCommand(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	asJSON := jsonFlag(fs)
	rerun := fs.Bool("rerun", false, "run the check even when this tree already passed it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: quipu check [--rerun] [--json]")
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	// The command comes from the default branch, as it does for quipu done, so
	// both look up the same record.
	c, _, err := loadEnforcedConfig(here, true)
	if err != nil {
		return err
	}
	if !machineEnforced {
		c.Limits = model.Limits{}
	}
	number := 0
	if lease, err := here.readLease(); err == nil {
		number = lease.Issue
	}
	result, err := runCheck(c, here, *rerun, checkOps{
		hold: func() (func(), error) { return checkSlot(c, here, number) },
		run:  func(command string) (int, error) { return runLocalCheck(here.Top, command) },
		now:  time.Now,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return printValue(result, true)
	}
	line := fmt.Sprintf("%s: %q on tree %s", result.Status, result.Command, result.Tree)
	if result.Reused {
		line += " (reused)"
	}
	fmt.Println(line)
	for _, warning := range result.Warnings {
		fmt.Println("warning:", warning)
	}
	fmt.Println(result.Next)
	return nil
}
