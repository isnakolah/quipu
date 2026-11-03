package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

// daemonFixture takes real candidates through the real selection, so what it starts
// is what quipu start --next would.
type daemonFixture struct {
	candidates []workCandidate
	running    int
	started    []int
	order      []string
	low        bool
	finished   []worktreeEntry
	halted     string
	removeErr  error
	busy       bool
}

func (f *daemonFixture) ops() daemonOps {
	return daemonOps{
		running:  func() int { return f.running },
		halted:   func() string { return f.halted },
		finished: func() ([]worktreeEntry, error) { return f.finished, nil },
		lowDisk:  func() bool { return f.low },
		clear: func(entry worktreeEntry) error {
			f.order = append(f.order, "clear "+entry.Path)
			return nil
		},
		remove: func(entry worktreeEntry) error {
			f.order = append(f.order, "remove "+entry.Path)
			return f.removeErr
		},
		claim: func() (flowResult, error) {
			f.order = append(f.order, "claim")
			if f.busy {
				return flowResult{Status: "Busy", Reason: "all 1 build slots are in use"}, nil
			}
			now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
			for _, item := range nextWork(f.candidates, now) {
				taken := false
				for _, number := range f.started {
					taken = taken || number == item.Number
				}
				if !taken {
					return flowResult{Issue: item.Number, Status: "In progress"}, nil
				}
			}
			return flowResult{Status: "Idle"}, nil
		},
		launch: func(result flowResult) error {
			f.started = append(f.started, result.Issue)
			f.running++
			return nil
		},
		keepWorker: func() { f.order = append(f.order, "worker") },
	}
}

func daemonCandidate(number int, kind, priority string) workCandidate {
	return workCandidate{readyWork: readyWork{Number: number, Kind: kind, Priority: priority}, Status: "Ready", ParentID: "E", Body: testContract}
}

func TestDaemonRespectsSlotsAndPriority(t *testing.T) {
	f := &daemonFixture{candidates: []workCandidate{
		daemonCandidate(1, "Story", "P2"), daemonCandidate(2, "Task", "P0"), daemonCandidate(3, "Defect", "P1"),
	}}
	report := daemonPass(f.ops(), 2)
	if fmt.Sprint(f.started) != "[2 3]" || fmt.Sprint(report.Started) != "[2 3]" {
		t.Fatalf("started %v: want the two highest priorities", f.started)
	}
	if !strings.Contains(report.Waiting, "all 2 agents") {
		t.Fatalf("waiting %q", report.Waiting)
	}
	// A full house takes nothing.
	f = &daemonFixture{running: 2, candidates: []workCandidate{daemonCandidate(1, "Story", "P0")}}
	if report := daemonPass(f.ops(), 2); len(f.started) != 0 || len(report.Started) != 0 {
		t.Fatalf("started %v with no slot free", f.started)
	}
	// A machine out of build slots says so and takes nothing.
	f = &daemonFixture{busy: true, candidates: []workCandidate{daemonCandidate(1, "Story", "P0")}}
	if report := daemonPass(f.ops(), 2); len(f.started) != 0 || !strings.Contains(report.Waiting, "Busy") {
		t.Fatalf("started %v, waiting %q", f.started, report.Waiting)
	}
}

func TestDaemonNeverStartsWhatTheOwnerDecides(t *testing.T) {
	f := &daemonFixture{candidates: []workCandidate{
		daemonCandidate(1, "Epic", "P0"), daemonCandidate(2, "Decision", "P0"), daemonCandidate(3, "Gate", "P0"), daemonCandidate(4, "Story", "P2"),
	}}
	report := daemonPass(f.ops(), 5)
	if fmt.Sprint(f.started) != "[4]" {
		t.Fatalf("started %v", f.started)
	}
	if report.Waiting != "Idle" {
		t.Fatalf("waiting %q", report.Waiting)
	}
}

func TestDaemonClearsFinishedWorktreesFirstWhenDiskIsLow(t *testing.T) {
	done := []worktreeEntry{{Path: "/w/a", Branch: "1/a"}, {Path: "/w/b", Branch: "2/b"}}
	f := &daemonFixture{low: true, finished: done, candidates: []workCandidate{daemonCandidate(3, "Story", "P0")}}
	report := daemonPass(f.ops(), 1)
	want := "worker clear /w/a clear /w/b remove /w/a remove /w/b claim"
	if strings.Join(f.order, " ") != want {
		t.Fatalf("order %q, want %q", strings.Join(f.order, " "), want)
	}
	if len(report.Cleared) != 2 || len(report.Removed) != 2 {
		t.Fatalf("report %+v", report)
	}
	// With room on the disk, removal alone does it.
	f = &daemonFixture{finished: done}
	daemonPass(f.ops(), 1)
	if strings.Contains(strings.Join(f.order, " "), "clear") {
		t.Fatalf("order %v", f.order)
	}
	// A worktree it can't remove is a warning, not a stop.
	f = &daemonFixture{finished: done, removeErr: errors.New("worktree-dirty"), candidates: []workCandidate{daemonCandidate(3, "Story", "P0")}}
	report = daemonPass(f.ops(), 1)
	if len(report.Warnings) != 2 || len(report.Removed) != 0 || fmt.Sprint(f.started) != "[3]" {
		t.Fatalf("report %+v, started %v", report, f.started)
	}
}

func TestFinishedWorktreesOnlyWhenWorkIsOverAndItsAgentHasLeft(t *testing.T) {
	entries := []worktreeEntry{
		{Path: "/main", Branch: "main"},
		{Path: "/w/done", Branch: "1/a"},
		{Path: "/w/cancelled", Branch: "2/a"},
		{Path: "/w/active", Branch: "3/a"},
		{Path: "/w/agent", Branch: "4/a"},
		{Path: "/w/other", Branch: "wip"},
		{Path: "/w/unknown", Branch: "6/a"},
	}
	status := map[int]string{1: "Done", 2: "Cancelled", 3: "In progress", 4: "Done"}
	got, err := finishedWorktrees(entries, func(n int) (string, error) {
		if n == 6 {
			return "", errors.New("GitHub is down")
		}
		return status[n], nil
	}, func(path string) bool { return path == "/w/agent" })
	if err == nil || !strings.Contains(err.Error(), "#6") {
		t.Fatalf("err = %v", err)
	}
	var paths []string
	for _, entry := range got {
		paths = append(paths, entry.Path)
	}
	if strings.Join(paths, " ") != "/w/done /w/cancelled" {
		t.Fatalf("paths %v", paths)
	}
}

func TestAgentLedger(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "ledger.json")
	dead := map[int]bool{}
	open := func() *agentLedger {
		l := openAgentLedger(path)
		l.now = func() time.Time { return now }
		l.alive = func(pid int) bool { return !dead[pid] }
		return l
	}
	l := open()
	l.started("/w/a", 100)
	l.started("/w/b", 200)
	// A restarted daemon still counts agents it didn't start.
	l = open()
	if l.running() != 2 || !l.isAlive("/w/a") {
		t.Fatalf("running %d", l.running())
	}
	dead[100] = true
	if l.running() != 1 || l.isAlive("/w/a") {
		t.Fatalf("running %d after one died", l.running())
	}
	// Fast failures in a row stop it; anything else starts the count again.
	began := now.Add(-time.Second)
	l.exited("/w/1", began, true)
	l.exited("/w/2", began, true)
	l.exited("/w/3", began, false)
	l.exited("/w/4", began, true)
	l.exited("/w/5", began, true)
	if l.halted() != "" {
		t.Fatal("a success in between must reset the count")
	}
	l.exited("/w/6", began.Add(-time.Hour), true)
	if l.halted() != "" {
		t.Fatal("an agent that ran a while says nothing about the command")
	}
	l.exited("/w/7", began, true)
	if !strings.Contains(l.halted(), "agent_command looks broken") {
		t.Fatalf("halted %q", l.halted())
	}
	f := &daemonFixture{halted: l.halted(), candidates: []workCandidate{daemonCandidate(1, "Story", "P0")}, finished: []worktreeEntry{{Path: "/w/x", Branch: "1/a"}}}
	report := daemonPass(f.ops(), 2)
	if len(f.started) != 0 || len(report.Removed) != 1 || !strings.Contains(report.Waiting, "broken") {
		t.Fatalf("started %v, report %+v: it stops starting, and still clears up", f.started, report)
	}
}

func TestAutoReady(t *testing.T) {
	good := liveWork{IssueState: "OPEN", Status: "Backlog", Labels: []string{"kind:story"}, ParentID: "E", Body: testContract, Blockers: []liveBlocker{{"A", "CLOSED"}, {"B", "CLOSED"}}}
	with := func(edit func(*liveWork)) liveWork {
		state := good
		edit(&state)
		return state
	}
	for _, test := range []struct {
		name  string
		state liveWork
		moves bool
	}{
		{"last blocker closed", good, true},
		{"parked work too", with(func(s *liveWork) { s.Status, s.Branch = "Blocked", "1/a" }), true},
		{"a blocker is still open", with(func(s *liveWork) { s.Blockers = []liveBlocker{{"A", "CLOSED"}, {"B", "OPEN"}} }), false},
		{"never blocked: the owner decides", with(func(s *liveWork) { s.Blockers = nil }), false},
		{"labelled for the owner", with(func(s *liveWork) { s.Labels = []string{"kind:story", needsOwnerLabel} }), false},
		{"a Decision", with(func(s *liveWork) { s.Labels = []string{"kind:decision"} }), false},
		{"a Gate", with(func(s *liveWork) { s.Labels = []string{"kind:gate"} }), false},
		{"an Epic", with(func(s *liveWork) { s.Labels = []string{"kind:epic"} }), false},
		{"already moving", with(func(s *liveWork) { s.Status = "In progress" }), false},
		{"closed", with(func(s *liveWork) { s.IssueState = "CLOSED" }), false},
		{"no parent", with(func(s *liveWork) { s.ParentID = "" }), false},
		{"has children", with(func(s *liveWork) { s.Children = 1 }), false},
		{"no contract", with(func(s *liveWork) { s.Body = "do it" }), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := autoReadyProblem(test.state, true); (got == "") != test.moves {
				t.Fatalf("problem %q, want moves=%v", got, test.moves)
			}
		})
	}
	if autoReadyProblem(good, false) == "" {
		t.Fatal("work off the board stays")
	}
	// Off by default; on, a dry pass only counts it.
	state := good
	w := &watch{now: time.Now()}
	if moved, err := w.autoReady(&state, true); err != nil || moved || w.report.AutoReady != 0 {
		t.Fatalf("off: %v, %v, %d", moved, err, w.report.AutoReady)
	}
	w.c.AutoReady = true
	if moved, err := w.autoReady(&state, true); err != nil || moved || w.report.AutoReady != 1 || state.Status != "Backlog" {
		t.Fatalf("dry: %v, %v, %d, %s", moved, err, w.report.AutoReady, state.Status)
	}
}

func TestParkRejectsAnythingButAnOpenDecision(t *testing.T) {
	decision := liveWork{IssueState: "OPEN", Labels: []string{"kind:decision"}}
	if got := parkProblem(decision, 9); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := parkProblem(liveWork{IssueState: "OPEN", Labels: []string{"kind:story"}}, 9); !strings.Contains(got, "not a Decision") {
		t.Fatalf("got %q", got)
	}
	if got := parkProblem(liveWork{IssueState: "OPEN", Labels: []string{"kind:gate"}}, 9); !strings.Contains(got, "not a Decision") {
		t.Fatalf("got %q", got)
	}
	if got := parkProblem(liveWork{IssueState: "CLOSED", Labels: []string{"kind:decision"}}, 9); !strings.Contains(got, "already closed") {
		t.Fatalf("got %q", got)
	}
	if !hasBlocker(liveWork{Blockers: []liveBlocker{{"D", "OPEN"}}}, "D") || hasBlocker(liveWork{}, "D") {
		t.Fatal("a rerun must not add the blocker twice")
	}
}

func TestParkFreesTheLeaseAndSlotAndWorkReturnsWhenTheDecisionCloses(t *testing.T) {
	m := testMachine(t)
	c := limited(1)
	w := leasedWorktree(t, 5)
	now := machineNow
	if warnings := recordWorkSlot(c, w, 5, "app", now); len(warnings) > 0 {
		t.Fatal(warnings)
	}
	if got := len(liveSlots(t, m, now)); got != 1 {
		t.Fatalf("slots in use %d before parking", got)
	}
	releaseParked(c, w, 5)
	if _, err := w.readLease(); err == nil {
		t.Fatal("the lease is still here")
	}
	if got := len(liveSlots(t, m, now)); got != 0 {
		t.Fatalf("slots in use %d after parking", got)
	}
	parked := liveWork{IssueState: "OPEN", Status: "Blocked", Branch: "005/a", Labels: []string{"kind:story"}, ParentID: "E", Body: testContract, Blockers: []liveBlocker{{"D", "OPEN"}}}
	if autoReadyProblem(parked, true) == "" {
		t.Fatal("it waits while the Decision is open")
	}
	parked.Blockers[0].State = "CLOSED"
	if got := autoReadyProblem(parked, true); got != "" {
		t.Fatalf("the Decision closed: %q", got)
	}
}

func TestDigestListsEachOwnerWaitOnce(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	items := []statusItem{
		{Number: 1, Title: "Which?", Kind: "Decision", Status: "Ready"},
		{Number: 2, Title: "Ship?", Kind: "Gate", Status: "Backlog"},
		{Number: 3, Title: "Parked", Kind: "Story", Status: "Blocked", WaitingOn: []int{1}},
		{Number: 4, Title: "Stuck", Kind: "Story", Status: "Blocked"},
		{Number: 5, Title: "Approve", Kind: "Story", Status: "In review"},
		{Number: 6, Title: "Taken", Kind: "Story", Status: "In progress", LeaseHolder: "a", LeaseExpires: now.Add(time.Hour).Format(time.RFC3339)},
	}
	report := statusReport{OwnerQueue: ownerQueue(items, false, now)}
	report.OwnerQueue, report.Attention = addPullsToQueues(report.OwnerQueue, report.Attention, []pullReport{{Issue: 5, owner: "review and merge its PR", State: "OPEN"}}, map[int]string{5: "Approve"})
	addQueueToReport(&report, &queueReport{Held: "the batch failed and no PR reproduces it\nmore"})
	reasons := map[int]string{}
	for _, item := range report.OwnerQueue {
		if _, twice := reasons[item.Number]; twice {
			t.Fatalf("#%d listed twice: %+v", item.Number, report.OwnerQueue)
		}
		reasons[item.Number] = item.Reason
	}
	for number, want := range map[int]string{1: "Decision", 2: "Gate", 3: "parked, waiting on #1", 4: "blocked", 5: "review and merge", 0: "merge queue is held"} {
		if !strings.Contains(reasons[number], want) {
			t.Errorf("#%d: reason %q, want %q", number, reasons[number], want)
		}
	}
	if _, listed := reasons[6]; listed {
		t.Error("work in hand doesn't wait on the owner")
	}
}

func TestSlowChecks(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	checks := []model.SlowCheck{
		{Workflow: "Fresh", When: "nightly"},
		{Workflow: "Stale", When: "nightly"},
		{Workflow: "Never", When: "nightly"},
		{Workflow: "Broken", When: "nightly"},
		{Workflow: "Linux", When: "batch"},
	}
	ran := map[string]time.Time{"Fresh": now.Add(-time.Hour), "Stale": now.Add(-25 * time.Hour)}
	due, err := dueSlowChecks(checks, func(workflow string) (time.Time, error) {
		if workflow == "Broken" {
			return time.Time{}, errors.New("no such workflow")
		}
		return ran[workflow], nil
	}, now)
	if fmt.Sprint(due) != "[Stale Never]" || err == nil || !strings.Contains(err.Error(), "Broken") {
		t.Fatalf("due %v, err %v", due, err)
	}
	if got := batchSlowChecks(model.Config{SlowChecks: checks}); fmt.Sprint(got) != "[Linux]" {
		t.Fatalf("batch %v", got)
	}
}

func TestSlowChecksRunAfterABatchLands(t *testing.T) {
	check, runs := counted(t, "true")
	f := newBatchFixture(t, check)
	f.run.c.MergeQueue.BatchSize = 1
	slow := 0
	f.run.ops.slow = func() { slow++ }
	f.pr(1, "f1.txt", "x\n")
	if outcome := f.drain(); outcome != queueIdle || runs() != 1 || slow != 1 {
		t.Fatalf("outcome %d, runs %d, slow %d", outcome, runs(), slow)
	}
}

func TestScopedBatchSkipsDocsWithHonestEvidence(t *testing.T) {
	check, runs := counted(t, "true")
	f := newBatchFixture(t, check)
	f.run.c.MergeQueue.BatchSize = 1
	f.run.c.ScopedChecks = []model.ScopedCheck{{Paths: []string{"docs/**"}}}
	f.pr(1, "docs/a.txt", "x\n")
	if outcome := f.drain(); outcome != queueIdle || runs() != 0 || len(f.landed) != 1 {
		t.Fatalf("outcome %d, runs %d, landed %v", outcome, runs(), f.landed)
	}
	e := f.recorded[1]
	if !strings.Contains(e.CIURL, "no check ran") || strings.Contains(e.CIURL, "exited 0") {
		t.Fatalf("evidence claims a run: %s", e.CIURL)
	}
	if !strings.Contains(e.Risks, "nothing ran") || !strings.Contains(e.Risks, "docs/a.txt") {
		t.Fatalf("evidence doesn't say why: %s", e.Risks)
	}
	view := prView{HeadRefOid: strings.Fields(strings.TrimPrefix(e.CIURL, queuePrefix))[0]}
	view.HeadRefOid = strings.TrimSuffix(view.HeadRefOid, ";")
	if err := checkQueueEvidence(f.run.c, e, view); err != nil {
		t.Fatalf("the Writer must accept it: %v", err)
	}
	// The Writer refuses the same claim where the default branch has no scoped_checks.
	off := f.run.c
	off.ScopedChecks = nil
	if err := checkQueueEvidence(off, e, view); err == nil || !strings.Contains(err.Error(), "scoped_checks") {
		t.Fatalf("err = %v", err)
	}
}

func TestScopedBatchRunsWhatAChangedPathNames(t *testing.T) {
	check, runs := counted(t, "true")
	// The full check fails only on the path nobody named, so the base stays green.
	f := newBatchFixture(t, "test ! -f scripts/x.sh")
	f.run.c.MergeQueue.BatchSize = 1
	f.run.c.ScopedChecks = []model.ScopedCheck{{Command: check, Paths: []string{"src/**"}}, {Command: "false", Paths: []string{"web/**"}}, {Paths: []string{"docs/**"}}}
	f.pr(1, "src/a.txt", "x\n")
	if outcome := f.drain(); outcome != queueIdle || runs() != 1 || len(f.landed) != 1 {
		t.Fatalf("outcome %d, runs %d, landed %v", outcome, runs(), f.landed)
	}
	e := f.recorded[1]
	if !strings.Contains(e.CIURL, "exited 0") || !strings.Contains(e.Risks, "skipped false") {
		t.Fatalf("evidence: %s / %s", e.CIURL, e.Risks)
	}
	// A path nobody named runs the full check, which here fails, so it goes back.
	f.pr(2, "scripts/x.sh", "x\n")
	f.drain()
	if len(f.landed) != 1 || f.returned[2] == "" {
		t.Fatalf("landed %v, returned %v: an unnamed path must not skip the build", f.landed, f.returned)
	}
}

func TestScopedLocalCheck(t *testing.T) {
	c := noCIConfig
	c.ScopedChecks = []model.ScopedCheck{{Command: "go test", Paths: []string{"src/**"}}, {Paths: []string{"docs/**"}}}
	run := func(paths []string, err error) (*fakeNoCI, string, error) {
		f := pendingPull()
		ops := f.ops()
		ops.changed = func() ([]string, error) { return paths, err }
		value, mergeErr := mergeWithoutCI(c, false, localEvidence, ops)
		return f, value, mergeErr
	}
	f, value, err := run([]string{"docs/a.md"}, nil)
	if err != nil || len(f.ran) != 0 || len(f.merges) != 1 {
		t.Fatalf("err %v, ran %v, merges %v", err, f.ran, f.merges)
	}
	if !strings.Contains(value, "local_check skipped, per scoped_checks") || strings.Contains(value, "exited") {
		t.Fatalf("evidence claims a run: %s", value)
	}
	good := writer.Evidence{CIURL: value, Boundary: "local"}
	view := prView{HeadRefOid: noCIHead, Checks: nil}
	if err := checkNoCIEvidence(c, good, view); err != nil {
		t.Fatalf("the Writer must accept it: %v", err)
	}
	if err := checkNoCIEvidence(noCIConfig, good, view); err == nil || !strings.Contains(err.Error(), "scoped_checks") {
		t.Fatalf("err = %v", err)
	}
	f, value, err = run([]string{"src/a.go"}, nil)
	if err != nil || fmt.Sprint(f.ran) != "[go test]" || !strings.Contains(value, `"go test" exited 0`) {
		t.Fatalf("err %v, ran %v, value %s", err, f.ran, value)
	}
	for _, paths := range [][]string{{"elsewhere/a"}, nil} {
		if f, _, err = run(paths, nil); err != nil || fmt.Sprint(f.ran) != "[make check]" {
			t.Fatalf("%v: err %v, ran %v: want the full check", paths, err, f.ran)
		}
	}
	if f, _, err = run(nil, errors.New("git failed")); err != nil || fmt.Sprint(f.ran) != "[make check]" {
		t.Fatalf("err %v, ran %v: not knowing must run the full check", err, f.ran)
	}
}

func TestWorktreeCopyCannotScopeOrDispatch(t *testing.T) {
	config := repoConfig(t)
	_, agent := publishedRepo(t, rootConfigPath, config)
	tampered := append(append([]byte{}, config...), "merge: queue\nmerge_queue:\n  check: make\nauto_ready: true\ndispatch:\n  agent_command: curl evil\nscoped_checks:\n  - paths: ['**']\n"...)
	writeRepoFile(t, agent.Top, rootConfigPath, tampered)
	local, err := loadConfigAt(agent.Top)
	if err != nil || len(local.ScopedChecks) != 1 {
		t.Fatalf("fixture: the worktree copy has them on: %v", err)
	}
	c, source, err := loadEnforcedConfig(agent, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ScopedChecks) != 0 || c.Dispatches() || c.AutoReady {
		t.Fatalf("%s: a worktree copy turned them on: %+v", source, c)
	}
	if source == configSourceWorktree {
		t.Fatal("the source must be the default branch")
	}
}

func TestDaemonPromptNamesItsWork(t *testing.T) {
	prompt := fmt.Sprintf(daemonPrompt, 7, 7, 7, "007/a", "main", "007/a", 7, 7)
	for _, want := range []string{"Issue #7", "quipu start 7 --json", "Refs #7", "quipu done 7 --json", "Never merge"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("missing %q: %s", want, prompt)
		}
	}
	if strings.Contains(prompt, "%!") {
		t.Fatalf("bad format: %s", prompt)
	}
}

func TestLaunchAgentRunsTheCommandInItsWorktreeAndTracksIt(t *testing.T) {
	top := gitRepo(t)
	root, err := openWorktree(top)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ledger := openAgentLedger(filepath.Join(dir, "ledger.json"))
	c := model.Config{DefaultBranch: "main", Dispatch: model.Dispatch{AgentCommand: `pwd > where; printf '%s %s %s' "$QUIPU_ISSUE" "$QUIPU_BRANCH" "$QUIPU_PROMPT" > what; exit 3`}}
	result := flowResult{Issue: 7, Branch: "007/a", Worktree: dir}
	if err := launchAgent(c, root, ledger, result); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for ledger.isAlive(dir) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ledger.isAlive(dir) {
		t.Fatal("the agent never finished")
	}
	what, err := os.ReadFile(filepath.Join(dir, "what"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(what), "7 007/a You work in this directory on Issue #7") {
		t.Fatalf("what = %s", what)
	}
	if _, err := os.Stat(filepath.Join(dir, "where")); err != nil {
		t.Fatalf("it didn't run in the worktree: %v", err)
	}
	// Its exit code was seen: this failure counts toward stopping.
	ledger.mu.Lock()
	fails := ledger.fastFails
	ledger.mu.Unlock()
	if fails != 1 {
		t.Fatalf("fast failures %d", fails)
	}
}

func TestWorkParkedInAnotherCheckoutDoesNotStopTheDaemon(t *testing.T) {
	top := gitRepo(t)
	root, err := openWorktree(top)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.git("branch", "005/parked"); err != nil {
		t.Fatal(err)
	}
	other, _, _, err := root.addWorktree(filepath.Join(t.TempDir(), "elsewhere"), "005/parked", "main")
	if err != nil {
		t.Fatal(err)
	}
	// The daemon's own dispatch path differs, so git would refuse the branch.
	_, _, _, err = root.addWorktree(filepath.Join(t.TempDir(), "dispatch"), "005/parked", "main")
	if !branchElsewhere(err) {
		t.Fatalf("addWorktree from %s = %v", other.Top, err)
	}
	if branchElsewhere(errors.New("branch-elsewhere")) {
		t.Fatal("only the coded failure counts")
	}
}
