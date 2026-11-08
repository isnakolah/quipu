package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/writer"
)

var checkAt = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// checkHere is `quipu check` in a worktree, with no machine slots.
func checkHere(t *testing.T, w worktree, command string, queue bool) (checkResult, error) {
	t.Helper()
	c := noCIConfig
	c.LocalCheck = command
	if queue {
		c = queueConfig(command)
	}
	return runCheck(c, w, false, checkOps{
		hold: func() (func(), error) { return func() {}, nil },
		run:  func(command string) (int, error) { return runLocalCheck(w.Top, command) },
		now:  func() time.Time { return checkAt },
	})
}

// reusing is a PR's merge ops with the real record store of its clone.
func (p *localPull) reusing() noCIOps {
	ops := p.ops()
	ops.tree = func() (string, error) { return p.agent.git("rev-parse", "HEAD^{tree}") }
	ops.passed = func(tree, command string) bool {
		_, ok := passedCheck(p.agent, tree, command)
		return ok
	}
	ops.remember = func(tree, command string, exit int) { _ = saveCheckRecord(p.agent, tree, command, exit, checkAt) }
	return ops
}

// branchPull is a PR from the agent's worktree with one file changed, checked
// with `quipu check` before it was committed, as an agent would.
func branchPull(t *testing.T, check string) *localPull {
	t.Helper()
	gitIdentity(t)
	top, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	writeRepoFile(t, agent.Top, "branch.txt", []byte("branch\n"))
	result, err := checkHere(t, agent, check, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "Passed" || result.Reused || !result.Recorded {
		t.Fatalf("the first check runs and is recorded: %+v", result)
	}
	commitAll(t, agent.Top)
	if head, _ := agent.git("rev-parse", "HEAD^{tree}"); head != result.Tree {
		t.Fatalf("the check keys the tree a commit of the worktree makes: %s, committed %s", result.Tree, head)
	}
	gitIn(t, agent.Top, "push", "-q", "origin", "001/a")
	return &localPull{t: t, top: top, agent: agent}
}

func TestCheckThenDoneRunsTheCommandOnce(t *testing.T) {
	check, runs := counted(t, "true")
	p := branchPull(t, check)
	config := noCIConfig
	config.LocalCheck = check
	value, err := mergeWithoutCI(config, false, localEvidence, p.reusing())
	if err != nil {
		t.Fatal(err)
	}
	if runs() != 1 || p.checks != 0 || p.merged != 1 {
		t.Fatalf("the check runs once in all: runs %d, done ran it %d times, merged %d", runs(), p.checks, p.merged)
	}
	tree, _ := p.agent.git("rev-parse", "HEAD^{tree}")
	if !strings.Contains(value, "on tree "+tree+", reused from an earlier run") {
		t.Fatalf("the evidence must say the result was reused: %s", value)
	}
	head, _ := p.agent.git("rev-parse", "HEAD")
	if err := checkNoCIEvidence(config, writer.Evidence{CIURL: value, Boundary: "local"}, prView{HeadRefOid: head}); err != nil {
		t.Fatalf("the Writer must accept it: %v", err)
	}
	if result, err := checkHere(t, p.agent, check, false); err != nil || !result.Reused || runs() != 1 {
		t.Fatalf("checking the same tree again reuses it too: %+v, %v, runs %d", result, err, runs())
	}
}

func TestAChangedTreeCommandOrMergeRunsItAgain(t *testing.T) {
	t.Run("a changed tree", func(t *testing.T) {
		check, runs := counted(t, "true")
		p := branchPull(t, check)
		writeRepoFile(t, p.agent.Top, "branch.txt", []byte("changed\n"))
		commitAll(t, p.agent.Top)
		gitIn(t, p.agent.Top, "push", "-q", "origin", "001/a")
		config := noCIConfig
		config.LocalCheck = check
		value, err := mergeWithoutCI(config, false, localEvidence, p.reusing())
		if err != nil {
			t.Fatal(err)
		}
		if runs() != 2 || p.checks != 1 || strings.Contains(value, "reused") {
			t.Fatalf("runs %d, done ran it %d times: %s", runs(), p.checks, value)
		}
	})

	t.Run("a changed command", func(t *testing.T) {
		check, runs := counted(t, "true")
		p := branchPull(t, check)
		config := noCIConfig
		config.LocalCheck = check + " && true"
		value, err := mergeWithoutCI(config, false, localEvidence, p.reusing())
		if err != nil {
			t.Fatal(err)
		}
		if runs() != 2 || p.checks != 1 || strings.Contains(value, "reused") {
			t.Fatalf("runs %d, done ran it %d times: %s", runs(), p.checks, value)
		}
	})

	t.Run("a merge from the default branch", func(t *testing.T) {
		check, runs := counted(t, "true")
		p := branchPull(t, check)
		landOnMain(t, p.top, "other.txt", "other\n")
		config := noCIConfig
		config.LocalCheck, config.UpdateBehind = check, "auto"
		value, err := mergeWithoutCI(config, false, localEvidence, p.reusing())
		if err != nil {
			t.Fatal(err)
		}
		if runs() != 2 || p.checks != 1 || strings.Contains(value, "reused") {
			t.Fatalf("runs %d, done ran it %d times: %s", runs(), p.checks, value)
		}
		if _, err := os.Stat(filepath.Join(p.agent.Top, "other.txt")); err != nil {
			t.Fatalf("the run must have been on the merged tree: %v", err)
		}
	})

	t.Run("a failure", func(t *testing.T) {
		gitIdentity(t)
		_, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
		marker := filepath.Join(t.TempDir(), "pass")
		check, runs := counted(t, "test -f "+marker)
		if _, err := checkHere(t, agent, check, false); flowCode(err) != "check-failed" {
			t.Fatalf("a failing check fails: %v", err)
		}
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if result, err := checkHere(t, agent, check, false); err != nil || result.Reused || runs() != 2 {
			t.Fatalf("a recorded failure is never reused: %+v, %v, runs %d", result, err, runs())
		}
	})
}

func TestCheckRecordsOnlyWhatItTested(t *testing.T) {
	gitIdentity(t)
	_, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	result, err := checkHere(t, agent, "echo out > written.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Recorded || len(result.Warnings) == 0 {
		t.Fatalf("a check that changes the worktree is not recorded: %+v", result)
	}
	if _, ok := passedCheck(agent, result.Tree, "echo out > written.txt"); ok {
		t.Fatal("nor found")
	}
	tree, _ := agent.git("rev-parse", "HEAD^{tree}")
	if err := saveCheckRecord(agent, tree, "make check", 0, checkAt); err != nil {
		t.Fatal(err)
	}
	if _, ok := passedCheck(agent, tree, "make check"); !ok {
		t.Fatal("a pass is found for its tree and command")
	}
	if _, ok := passedCheck(agent, tree, "make check "); ok {
		t.Fatal("never for another command")
	}
	if _, ok := passedCheck(agent, strings.Repeat("0", 40), "make check"); ok {
		t.Fatal("never for another tree")
	}
	if err := saveCheckRecord(agent, "not a tree", "make check", 0, checkAt); err == nil {
		t.Fatal("only trees are keys")
	}
	if _, err := checkHere(t, agent, "", false); flowCode(err) != "no-check" {
		t.Fatalf("no command, nothing to run: %v", err)
	}
}

func TestThreeIssueBatchRunsTheCheckOnce(t *testing.T) {
	check, runs := counted(t, "true")
	f := newBatchFixture(t, check)
	f.run.c.MergeQueue.BatchSize = 3
	for i := 1; i <= 3; i++ {
		f.pr(i, fmt.Sprintf("f%d.txt", i), "x\n")
	}
	if outcome := f.drain(); outcome != queueIdle {
		t.Fatalf("outcome %d", outcome)
	}
	if runs() != 1 || fmt.Sprint(f.landed) != "[1 2 3]" {
		t.Fatalf("runs %d, landed %v", runs(), f.landed)
	}
	for i := 1; i <= 3; i++ {
		if e := f.recorded[i]; !strings.Contains(e.CIURL, "(base ") || !strings.Contains(e.CIURL, "1 runs, 0 flaky)") {
			t.Fatalf("#%d: %q", i, e.CIURL)
		}
	}
	tree := gitOut(t, f.top, "rev-parse", "origin/main^{tree}")
	if _, ok := passedCheck(f.run.tree, tree, check); !ok {
		t.Fatal("the merged tree's pass is recorded")
	}
}

func TestQueueReusesAPassForTheTreeItTests(t *testing.T) {
	check, runs := counted(t, "true")
	f := newBatchFixture(t, check)
	m := f.pr(1, "f1.txt", "x\n")
	gitIn(t, f.top, "switch", "-q", m.Branch)
	agent, err := openWorktree(f.top)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := checkHere(t, agent, check, true); err != nil || !result.Recorded {
		t.Fatalf("%+v, %v", result, err)
	}
	gitIn(t, f.top, "switch", "-q", "main")
	f.run.c.MergeQueue.BatchSize = 1
	if outcome := f.drain(); outcome != queueIdle {
		t.Fatalf("outcome %d", outcome)
	}
	if runs() != 1 || fmt.Sprint(f.landed) != "[1]" {
		t.Fatalf("runs %d, landed %v", runs(), f.landed)
	}
	e := f.recorded[1]
	if !strings.Contains(e.CIURL, "0 runs, 0 flaky, 1 reused)") || !queueValue.MatchString(e.CIURL) {
		t.Fatalf("the evidence says it reused the pass: %q", e.CIURL)
	}
	if err := checkQueueEvidence(f.run.c, e, prView{HeadRefOid: m.Head}); err != nil {
		t.Fatalf("the Writer must accept it: %v", err)
	}
}
