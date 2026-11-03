package main

// `quipu daemon` keeps a machine busy without a person dispatching: it fills free
// slots from Ready work, starts the configured agent in each new worktree, and clears
// away what's finished. It is optional, the owner runs it, and it takes work only
// through quipu start, so it never starts an Epic, a Decision or a Gate.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/isnakolah/quipu/internal/model"
)

const (
	// daemonFastExit is how soon an agent's failure counts against its command.
	daemonFastExit = time.Minute
	// daemonMaxFastFails in a row means the command is broken, and starting more
	// agents would only claim and strand more work.
	daemonMaxFastFails = 3
	daemonPrompt       = "You work in this directory on Issue #%d of this Quipu repository. Your lease is already claimed for this worktree. First run `quipu start %d --json` here to resume it. Implement only #%d's acceptance criteria, run the tests, push branch %s, open a PR with `gh pr create --base %s --head %s` and a body that says exactly `Refs #%d`, then run `quipu done %d --json` and report the PR URL. Read `quipu guide` for the rules. Never merge, never switch branches, never edit outside this directory."
)

// daemonOps is what one pass does to the world.
type daemonOps struct {
	running    func() int
	halted     func() string
	finished   func() ([]worktreeEntry, error)
	lowDisk    func() bool
	clear      func(entry worktreeEntry) error
	remove     func(entry worktreeEntry) error
	claim      func() (flowResult, error)
	launch     func(result flowResult) error
	keepWorker func()
}

type daemonReport struct {
	Started []int    `json:"started"`
	Removed []string `json:"removed"`
	Cleared []string `json:"cleared"`
	// Waiting says why no more agents started: Idle, Busy, or the slots are full.
	Waiting  string   `json:"waiting,omitempty"`
	Warnings []string `json:"warnings"`
}

// daemonPass is one look: finished work goes first, since it frees what new work
// needs, and low disk clears build output before anything else is asked of it.
func daemonPass(ops daemonOps, maxAgents int) daemonReport {
	report := daemonReport{Started: []int{}, Removed: []string{}, Cleared: []string{}, Warnings: []string{}}
	warn := func(format string, args ...any) {
		report.Warnings = append(report.Warnings, fmt.Sprintf(format, args...))
	}
	ops.keepWorker()
	finished, err := ops.finished()
	if err != nil {
		warn("could not list finished worktrees: %v", err)
	}
	if ops.lowDisk() {
		for _, entry := range finished {
			if err := ops.clear(entry); err != nil {
				warn("could not clear %s: %v", entry.Path, err)
				continue
			}
			report.Cleared = append(report.Cleared, entry.Path)
		}
	}
	for _, entry := range finished {
		if err := ops.remove(entry); err != nil {
			warn("could not remove %s: %v", entry.Path, err)
			continue
		}
		report.Removed = append(report.Removed, entry.Path)
	}
	if reason := ops.halted(); reason != "" {
		report.Waiting = reason
		return report
	}
	for ops.running() < maxAgents {
		result, err := ops.claim()
		if err != nil {
			warn("could not take work: %v", err)
			return report
		}
		if result.Status == "Idle" || result.Status == "Busy" {
			report.Waiting = result.Status
			if result.Reason != "" {
				report.Waiting += ": " + result.Reason
			}
			return report
		}
		if err := ops.launch(result); err != nil {
			warn("#%d is claimed, but its agent would not start: %v", result.Issue, err)
			return report
		}
		report.Started = append(report.Started, result.Issue)
	}
	report.Waiting = fmt.Sprintf("all %d agents are running", maxAgents)
	return report
}

// finishedWorktrees picks linked worktrees whose work is over and whose agent has
// left. The first entry is the main checkout, which never goes.
func finishedWorktrees(entries []worktreeEntry, status func(number int) (string, error), agentAlive func(path string) bool) ([]worktreeEntry, error) {
	var finished []worktreeEntry
	var failure error
	for i, entry := range entries {
		number := issueOfBranch(entry.Branch)
		if i == 0 || number == 0 || agentAlive(entry.Path) {
			continue
		}
		state, err := status(number)
		if err != nil {
			failure = errors.Join(failure, fmt.Errorf("#%d: %w", number, err))
			continue
		}
		if terminalIssueStatus(state) {
			finished = append(finished, entry)
		}
	}
	return finished, failure
}

// agentLedger remembers this clone's agents across a daemon restart, so a new daemon
// doesn't start more than the limit, or remove a worktree something still runs in.
type agentLedger struct {
	mu        sync.Mutex
	path      string
	pids      map[string]int
	fastFails int
	alive     func(pid int) bool
	now       func() time.Time
}

func openAgentLedger(path string) *agentLedger {
	ledger := &agentLedger{path: path, pids: map[string]int{}, now: time.Now, alive: func(pid int) bool {
		// Signal 0 only asks whether the process exists.
		process, err := os.FindProcess(pid)
		return err == nil && process.Signal(syscall.Signal(0)) == nil
	}}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &ledger.pids)
	}
	return ledger
}

func (l *agentLedger) running() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for path, pid := range l.pids {
		if l.alive(pid) {
			count++
			continue
		}
		delete(l.pids, path)
	}
	l.save()
	return count
}

func (l *agentLedger) isAlive(path string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	pid, ok := l.pids[path]
	return ok && l.alive(pid)
}

func (l *agentLedger) started(path string, pid int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pids[path] = pid
	l.save()
}

// exited notes how an agent ended, and forgets it.
func (l *agentLedger) exited(path string, began time.Time, failed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pids, path)
	l.save()
	switch {
	case failed && l.now().Sub(began) < daemonFastExit:
		l.fastFails++
	case !failed:
		l.fastFails = 0
	}
}

func (l *agentLedger) halted() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fastFails >= daemonMaxFastFails {
		return fmt.Sprintf("stopped starting agents: %d in a row failed within %s, so dispatch.agent_command looks broken. Fix it and restart the daemon", l.fastFails, daemonFastExit)
	}
	return ""
}

func (l *agentLedger) save() {
	if raw, err := json.Marshal(l.pids); err == nil {
		_ = os.WriteFile(l.path, raw, 0o644)
	}
}

// launchAgent starts the owner's command in the worktree, detached so it outlives
// the daemon. It reads its Issue from the environment.
func launchAgent(c model.Config, root worktree, ledger *agentLedger, result flowResult) error {
	logs, err := root.commonPath("quipu-daemon-logs")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logs, 0o755); err != nil {
		return err
	}
	log, err := os.OpenFile(fmt.Sprintf("%s/%d.log", logs, result.Issue), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command("sh", "-c", c.Dispatch.AgentCommand)
	cmd.Dir, cmd.Stdout, cmd.Stderr = result.Worktree, log, log
	n, branch := result.Issue, result.Branch
	cmd.Env = append(os.Environ(), fmt.Sprintf("QUIPU_ISSUE=%d", n), "QUIPU_BRANCH="+branch, "QUIPU_WORKTREE="+result.Worktree,
		"QUIPU_PROMPT="+fmt.Sprintf(daemonPrompt, n, n, n, branch, c.DefaultBranch, branch, n, n))
	detach(cmd)
	if err := cmd.Start(); err != nil {
		log.Close()
		return err
	}
	began := time.Now()
	ledger.started(result.Worktree, cmd.Process.Pid)
	go func() {
		defer log.Close()
		ledger.exited(result.Worktree, began, cmd.Wait() != nil)
	}()
	return nil
}

func daemonCommand(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	once := fs.Bool("once", false, "run one pass and exit")
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	root, err := mainCheckout(here)
	if err != nil {
		return err
	}
	lockPath, err := root.commonPath("quipu-daemon.lock")
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if free, err := tryLockFile(lock); err != nil {
		return err
	} else if !free {
		return errors.New("another quipu daemon is running on this clone")
	}
	ledgerPath, err := root.commonPath("quipu-daemon.json")
	if err != nil {
		return err
	}
	ledger := openAgentLedger(ledgerPath)
	for {
		// The default branch decides every pass, so an agent's copy can't choose the
		// command, and turning it off stops it.
		c, source, err := loadEnforcedConfig(root, true)
		if err != nil {
			return err
		}
		if !c.Dispatches() || source == configSourceWorktree {
			return errors.New("dispatch.agent_command is not set on the default branch")
		}
		report := daemonPass(liveDaemonOps(c, root, ledger), c.DispatchMaxAgents())
		if *once {
			return printValue(report, *asJSON)
		}
		if len(report.Started)+len(report.Removed)+len(report.Warnings) > 0 {
			if err := printValue(report, true); err != nil {
				return err
			}
		}
		time.Sleep(c.DispatchPoll())
	}
}

func liveDaemonOps(c model.Config, root worktree, ledger *agentLedger) daemonOps {
	live := func(number int) (liveWork, error) { return loadLiveWork(c, number) }
	cleanup := c.Cleanup
	return daemonOps{
		running: ledger.running,
		halted:  ledger.halted,
		finished: func() ([]worktreeEntry, error) {
			porcelain, err := root.git("worktree", "list", "--porcelain")
			if err != nil {
				return nil, err
			}
			return finishedWorktrees(parseWorktreeList(porcelain), func(number int) (string, error) {
				state, err := live(number)
				return state.Status, err
			}, ledger.isAlive)
		},
		lowDisk: func() bool {
			if c.Limits.MinFreeDiskGB <= 0 {
				return false
			}
			free, err := freeDiskBytes(root.Top)
			return err == nil && free < uint64(c.Limits.MinFreeDiskGB)<<30
		},
		clear: func(entry worktreeEntry) error {
			if cleanup == "" {
				return nil
			}
			if warnings := runCleanup(entry.Path, cleanup); len(warnings) > 0 {
				return errors.New(strings.Join(warnings, "; "))
			}
			return nil
		},
		remove: func(entry worktreeEntry) error {
			_, err := removeWorktreeTree(root, entry, cleanup, live)
			return err
		},
		claim: func() (flowResult, error) {
			return startNextWork(enforcedLimits(c, root, true), root, startOptions{Wait: 3 * time.Minute, Worktree: true, SkipElsewhere: true})
		},
		launch:     func(result flowResult) error { return launchAgent(c, root, ledger, result) },
		keepWorker: func() { ensureQueueWorker(c, root) },
	}
}
