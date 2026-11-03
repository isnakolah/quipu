package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	policycheck "github.com/isnakolah/quipu/internal/policy"
	"github.com/isnakolah/quipu/internal/writer"
)

// batchFixture is the worker against a real local origin, with only GitHub's
// answers faked. A landing is a real merge commit on origin's main.
type batchFixture struct {
	t        *testing.T
	top      string
	run      *queueRun
	members  []queueMember
	views    map[string]*mergeView
	recorded map[int]writer.Evidence
	returned map[int]string
	landed   []int
	skipped  []int // landings told to skip CI
	now      time.Time
	// beforeLand runs just before a landing; failLand refuses one.
	beforeLand func(step batchStep)
	failLand   func(step batchStep) error
	// tamper changes what a landing merges, so it isn't what was tested.
	tamper func(step batchStep)
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := worktree{Top: dir}.git(args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func queueConfig(check string) model.Config {
	return model.Config{Owner: "o", Repository: "r", DefaultBranch: "main", Merge: "queue", MergeQueue: model.MergeQueue{Check: check, BatchSize: 4}}
}

func newBatchFixture(t *testing.T, check string) *batchFixture {
	t.Helper()
	gitIdentity(t)
	top, _ := publishedRepo(t, rootConfigPath, repoConfig(t))
	path := filepath.Join(t.TempDir(), "queue")
	gitIn(t, top, "fetch", "-q", "origin", "main")
	gitIn(t, top, "worktree", "add", "-q", "--detach", path, "origin/main")
	tree, err := openWorktree(path)
	if err != nil {
		t.Fatal(err)
	}
	f := &batchFixture{t: t, top: top, views: map[string]*mergeView{}, recorded: map[int]writer.Evidence{}, returned: map[int]string{}, now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	f.run = &queueRun{c: queueConfig(check), tree: tree, journal: filepath.Join(dir, "journal.json"), logs: filepath.Join(dir, "logs"), timeout: time.Minute}
	f.run.ops = queueOps{
		members: func() ([]queueMember, error) { return append([]queueMember{}, f.members...), nil },
		view: func(url string) (mergeView, error) {
			view, ok := f.views[url]
			if !ok {
				return mergeView{}, errors.New("no such PR")
			}
			return *view, nil
		},
		violations: func(queueMember) ([]string, error) { return nil, nil },
		land: func(step batchStep, view mergeView, last bool) error {
			if f.beforeLand != nil {
				f.beforeLand(step)
			}
			if f.failLand != nil {
				if err := f.failLand(step); err != nil {
					return err
				}
			}
			f.landed = append(f.landed, step.Issue)
			if !last {
				f.skipped = append(f.skipped, step.Issue)
			}
			gitIn(t, top, "pull", "-q", "--no-edit", "origin", "main")
			gitIn(t, top, "merge", "-q", "--no-ff", "--no-edit", step.Head)
			if f.tamper != nil {
				f.tamper(step)
			}
			gitIn(t, top, "push", "-q", "origin", "main")
			sha := gitOut(t, top, "rev-parse", "HEAD")
			mergedView := f.views[step.PR]
			mergedView.State = "MERGED"
			mergedView.MergeCommit = &struct {
				OID string `json:"oid"`
			}{sha}
			return nil
		},
		record: func(m queueMember, e writer.Evidence) error {
			f.recorded[m.Issue] = e
			f.remove(m.Issue)
			return nil
		},
		giveBack: func(m queueMember, reason string) error {
			f.returned[m.Issue] = reason
			f.remove(m.Issue)
			return nil
		},
		hold: func() (func(), error) { return func() {}, nil },
		now:  func() time.Time { return f.now },
	}
	return f
}

func (f *batchFixture) remove(issue int) {
	for i, m := range f.members {
		if m.Issue == issue {
			f.members = append(f.members[:i], f.members[i+1:]...)
			return
		}
	}
}

// pr pushes a branch with one file changed and queues it.
func (f *batchFixture) pr(issue int, name, content string) queueMember {
	f.t.Helper()
	branch := fmt.Sprintf("%03d/pr", issue)
	gitIn(f.t, f.top, "fetch", "-q", "origin", "main")
	gitIn(f.t, f.top, "switch", "-q", "-c", branch, "origin/main")
	writeRepoFile(f.t, f.top, name, []byte(content))
	commitAll(f.t, f.top)
	gitIn(f.t, f.top, "push", "-q", "origin", branch)
	head := gitOut(f.t, f.top, "rev-parse", "HEAD")
	gitIn(f.t, f.top, "switch", "-q", "main")
	url := fmt.Sprintf("https://example.test/pull/%d", issue)
	m := queueMember{Issue: issue, Holder: "claude/octo/x", Branch: branch, PR: url, Head: head, QueuedAt: f.now, Evidence: writer.Evidence{Criteria: "c", Commands: "cmd", Environments: defaultEnvironments, Artifacts: defaultArtifacts, Documentation: "d", Risks: "None", Boundary: "local"}}
	f.members = append(f.members, m)
	f.views[url] = &mergeView{URL: url, State: "OPEN", HeadRefOid: head}
	return m
}

// drain runs passes until the queue stops changing.
func (f *batchFixture) drain() queueOutcome {
	f.t.Helper()
	for range 20 {
		outcome, err := f.run.pass()
		if err != nil {
			f.t.Fatal(err)
		}
		if outcome != queueMoved {
			return outcome
		}
	}
	f.t.Fatal("the queue never settled")
	return queueIdle
}

func (f *batchFixture) mainHas(name string) bool {
	f.t.Helper()
	gitIn(f.t, f.top, "pull", "-q", "--no-edit", "origin", "main")
	_, err := os.Stat(filepath.Join(f.top, name))
	return err == nil
}

// counted is a check that notes each run, then does what command says.
func counted(t *testing.T, command string) (string, func() int) {
	t.Helper()
	counter := filepath.Join(t.TempDir(), "runs")
	return "echo x >> " + counter + "; " + command, func() int {
		raw, _ := os.ReadFile(counter)
		return strings.Count(string(raw), "x")
	}
}

func TestCleanBatchRunsTheCheckOnceAndLandsInOrder(t *testing.T) {
	check, runs := counted(t, "true")
	f := newBatchFixture(t, check)
	for i := 1; i <= 4; i++ {
		f.pr(i, fmt.Sprintf("f%d.txt", i), "x\n")
	}
	if outcome := f.drain(); outcome != queueIdle {
		t.Fatalf("outcome %d", outcome)
	}
	if runs() != 1 {
		t.Fatalf("runs %d", runs())
	}
	if fmt.Sprint(f.landed) != "[1 2 3 4]" {
		t.Fatalf("landed %v", f.landed)
	}
	if fmt.Sprint(f.skipped) != "[1 2 3]" {
		t.Fatalf("every merge but the last skips push CI: %v", f.skipped)
	}
	for i := 1; i <= 4; i++ {
		e, ok := f.recorded[i]
		if !ok {
			t.Fatalf("#%d has no evidence", i)
		}
		if !queueValue.MatchString(e.CIURL) || !strings.Contains(e.CIURL, "1 runs, 0 flaky") {
			t.Fatalf("evidence must say what ran: %q", e.CIURL)
		}
		if e.Boundary != "local" || e.Environments != queueEnvironment || e.Risks != "None" || e.FinalSHA == "" {
			t.Fatalf("%+v", e)
		}
	}
}

func TestLandedTreesAreExactlyWhatWasTested(t *testing.T) {
	f := newBatchFixture(t, "test -f f1.txt && test -f f2.txt")
	f.pr(1, "f1.txt", "x\n")
	f.pr(2, "f2.txt", "x\n")
	f.run.c.MergeQueue.BatchSize = 2
	if outcome := f.drain(); outcome != queueIdle {
		t.Fatalf("outcome %d, returned %v", outcome, f.returned)
	}
	if len(f.landed) != 2 {
		t.Fatalf("landed %v", f.landed)
	}
}

func TestBatchWaitsToFillThenGoes(t *testing.T) {
	check, runs := counted(t, "true")
	f := newBatchFixture(t, check)
	f.pr(1, "f1.txt", "x\n")
	if outcome := f.drain(); outcome != queueWaiting || runs() != 0 {
		t.Fatalf("a batch of 1 of 4 waits: %d, runs %d", outcome, runs())
	}
	f.now = f.now.Add(31 * time.Minute)
	if outcome := f.drain(); outcome != queueIdle || runs() != 1 || len(f.landed) != 1 {
		t.Fatalf("past the wait it goes: %d, runs %d, landed %v", outcome, runs(), f.landed)
	}
}

func TestAtMostOneBatchAtATime(t *testing.T) {
	check, runs := counted(t, "true")
	f := newBatchFixture(t, check)
	f.run.c.MergeQueue.BatchSize = 2
	for i := 1; i <= 3; i++ {
		f.pr(i, fmt.Sprintf("f%d.txt", i), "x\n")
	}
	f.now = f.now.Add(time.Hour)
	f.drain()
	if runs() != 2 || fmt.Sprint(f.landed) != "[1 2 3]" {
		t.Fatalf("runs %d, landed %v", runs(), f.landed)
	}
}

func TestConflictReturnsThatPRAndTheRestLand(t *testing.T) {
	check, runs := counted(t, "true")
	f := newBatchFixture(t, check)
	f.run.c.MergeQueue.BatchSize = 3
	f.pr(1, "same.txt", "one\n")
	f.pr(2, "same.txt", "two\n")
	f.pr(3, "other.txt", "x\n")
	if outcome := f.drain(); outcome != queueIdle {
		t.Fatalf("outcome %d", outcome)
	}
	if !strings.Contains(f.returned[2], "conflicts") || len(f.returned) != 1 {
		t.Fatalf("returned %v", f.returned)
	}
	if fmt.Sprint(f.landed) != "[1 3]" || runs() != 1 {
		t.Fatalf("landed %v, runs %d", f.landed, runs())
	}
}

func TestBisectReturnsOnlyTheCulprit(t *testing.T) {
	for culprit := 1; culprit <= 4; culprit++ {
		t.Run(fmt.Sprintf("PR %d of 4 is at fault", culprit), func(t *testing.T) {
			check, runs := counted(t, "test ! -f bad.txt")
			f := newBatchFixture(t, check)
			for i := 1; i <= 4; i++ {
				name := fmt.Sprintf("f%d.txt", i)
				if i == culprit {
					name = "bad.txt"
				}
				f.pr(i, name, "x\n")
			}
			if outcome := f.drain(); outcome != queueIdle {
				t.Fatalf("outcome %d, hold %q", outcome, f.run.current.Hold)
			}
			if len(f.returned) != 1 || !strings.Contains(f.returned[culprit], "failed with this PR") {
				t.Fatalf("returned %v", f.returned)
			}
			if !strings.Contains(f.returned[culprit], "Output of the last run") {
				t.Fatalf("the failure must be attached: %q", f.returned[culprit])
			}
			if f.mainHas("bad.txt") {
				t.Fatal("the culprit must not land")
			}
			if len(f.landed) != 3 {
				t.Fatalf("landed %v", f.landed)
			}
			t.Logf("%d runs", runs())
			for _, issue := range f.landed {
				if issue == culprit {
					t.Fatalf("landed the culprit: %v", f.landed)
				}
			}
		})
	}
}

func TestTwoPRsThatFailOnlyTogetherReturnTheLaterOne(t *testing.T) {
	f := newBatchFixture(t, "test ! -f a.txt || test ! -f b.txt")
	f.run.c.MergeQueue.BatchSize = 2
	f.pr(1, "a.txt", "x\n")
	f.pr(2, "b.txt", "x\n")
	if outcome := f.drain(); outcome != queueIdle {
		t.Fatalf("outcome %d", outcome)
	}
	if fmt.Sprint(f.landed) != "[1]" || len(f.returned) != 1 || f.returned[2] == "" {
		t.Fatalf("landed %v, returned %v", f.landed, f.returned)
	}
}

func TestFlakeLandsAndIsFlagged(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "flaked")
	// Fails the first time only.
	f := newBatchFixture(t, fmt.Sprintf("test -f %s || { touch %s; exit 1; }", marker, marker))
	f.run.c.MergeQueue.BatchSize = 1
	f.pr(1, "f1.txt", "x\n")
	if outcome := f.drain(); outcome != queueIdle {
		t.Fatalf("outcome %d", outcome)
	}
	if len(f.returned) != 0 || len(f.landed) != 1 {
		t.Fatalf("a flake ejects nobody: %v %v", f.returned, f.landed)
	}
	e := f.recorded[1]
	if !strings.Contains(e.CIURL, "2 runs, 1 flaky") || !strings.Contains(e.Risks, "flaky") {
		t.Fatalf("it can't pass silently: %+v", e)
	}
	f.run.loadJournal()
	if f.run.current.Flaked == "" {
		t.Fatal("status must be able to say so too")
	}
}

func TestAFailureNoPRReproducesEjectsNobody(t *testing.T) {
	// Fails on the first two runs, which is what convicts a batch, then passes.
	counter := filepath.Join(t.TempDir(), "n")
	check := fmt.Sprintf(`echo x >> %s; test "$(wc -l < %s)" -gt 2`, counter, counter)
	f := newBatchFixture(t, check)
	f.run.c.MergeQueue.BatchSize = 1
	f.pr(1, "f1.txt", "x\n")
	if outcome := f.drain(); outcome != queueHeld {
		t.Fatalf("outcome %d, returned %v, landed %v", outcome, f.returned, f.landed)
	}
	if len(f.returned) != 0 || len(f.landed) != 0 {
		t.Fatalf("returned %v, landed %v", f.returned, f.landed)
	}
	if !strings.Contains(f.run.current.Hold, "no PR reproduces") {
		t.Fatalf("hold %q", f.run.current.Hold)
	}
	// Held means held, until the owner says.
	if outcome := f.drain(); outcome != queueHeld || len(f.landed) != 0 {
		t.Fatalf("outcome %d, landed %v", outcome, f.landed)
	}
}

func TestARedDefaultBranchHoldsTheQueueAndBlamesNoPR(t *testing.T) {
	f := newBatchFixture(t, "false")
	f.run.c.MergeQueue.BatchSize = 2
	f.pr(1, "f1.txt", "x\n")
	f.pr(2, "f2.txt", "x\n")
	if outcome := f.drain(); outcome != queueHeld {
		t.Fatalf("outcome %d, returned %v", outcome, f.returned)
	}
	if len(f.returned) != 0 || len(f.landed) != 0 || !strings.Contains(f.run.current.Hold, "no PR is at fault") {
		t.Fatalf("returned %v, landed %v, hold %q", f.returned, f.landed, f.run.current.Hold)
	}
}

func TestATimeoutIsNoVerdict(t *testing.T) {
	f := newBatchFixture(t, "sleep 30")
	f.run.timeout = 50 * time.Millisecond
	f.run.c.MergeQueue.BatchSize = 1
	f.pr(1, "f1.txt", "x\n")
	if outcome := f.drain(); outcome != queueWaiting || len(f.returned) != 0 {
		t.Fatalf("outcome %d, returned %v", outcome, f.returned)
	}
	f.drain()
	if outcome := f.drain(); outcome != queueHeld || len(f.returned) != 0 || len(f.landed) != 0 {
		t.Fatalf("after 3 it asks: %d, %v, %v", outcome, f.returned, f.landed)
	}
}

func TestABusyMachineOnlyWaits(t *testing.T) {
	check, runs := counted(t, "true")
	f := newBatchFixture(t, check)
	f.run.c.MergeQueue.BatchSize = 1
	f.pr(1, "f1.txt", "x\n")
	f.run.ops.hold = func() (func(), error) { return nil, flowErr("machine-busy", "full") }
	for range 5 {
		if outcome := f.drain(); outcome != queueWaiting {
			t.Fatalf("outcome %d", outcome)
		}
	}
	if runs() != 0 || f.run.current.NoVerdicts != 0 {
		t.Fatalf("runs %d, no verdicts %d", runs(), f.run.current.NoVerdicts)
	}
	f.run.ops.hold = func() (func(), error) { return func() {}, nil }
	if outcome := f.drain(); outcome != queueIdle || runs() != 1 || len(f.landed) != 1 {
		t.Fatalf("outcome %d, runs %d, landed %v", outcome, runs(), f.landed)
	}
}

func TestPushAfterQueueingLeavesTheQueue(t *testing.T) {
	f := newBatchFixture(t, "true")
	f.run.c.MergeQueue.BatchSize = 1
	f.pr(1, "f1.txt", "x\n")
	f.views["https://example.test/pull/1"].HeadRefOid = "another"
	f.drain()
	if !strings.Contains(f.returned[1], "moved") || len(f.landed) != 0 {
		t.Fatalf("returned %v, landed %v", f.returned, f.landed)
	}
}

func TestPolicyFailedChecksAndDraftsLeaveTheQueue(t *testing.T) {
	for name, change := range map[string]func(f *batchFixture){
		"policy": func(f *batchFixture) {
			f.run.ops.violations = func(queueMember) ([]string, error) { return []string{"a rule"}, nil }
		},
		"failed check": func(f *batchFixture) {
			f.views["https://example.test/pull/1"].Checks = []prCheck{{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"}}
		},
		"draft":  func(f *batchFixture) { f.views["https://example.test/pull/1"].IsDraft = true },
		"closed": func(f *batchFixture) { f.views["https://example.test/pull/1"].State = "CLOSED" },
	} {
		t.Run(name, func(t *testing.T) {
			check, runs := counted(t, "true")
			f := newBatchFixture(t, check)
			f.run.c.MergeQueue.BatchSize = 1
			f.pr(1, "f1.txt", "x\n")
			change(f)
			f.drain()
			if f.returned[1] == "" || len(f.landed) != 0 || runs() != 0 {
				t.Fatalf("returned %v, landed %v, runs %d", f.returned, f.landed, runs())
			}
		})
	}
}

// crash is what a killed worker leaves: every effect so far, and nothing after.
var errCrash = errors.New("killed")

func TestCrashAfterTestingLandsWithoutTestingAgain(t *testing.T) {
	check, runs := counted(t, "true")
	f := newBatchFixture(t, check)
	f.run.c.MergeQueue.BatchSize = 2
	f.pr(1, "f1.txt", "x\n")
	f.pr(2, "f2.txt", "x\n")
	f.failLand = func(batchStep) error { return errCrash }
	if _, err := f.run.pass(); err != nil {
		t.Fatal(err)
	}
	if len(f.landed) != 0 || runs() != 1 {
		t.Fatalf("landed %v, runs %d", f.landed, runs())
	}
	f.failLand = nil
	if outcome := f.drain(); outcome != queueIdle {
		t.Fatalf("outcome %d", outcome)
	}
	if runs() != 1 || fmt.Sprint(f.landed) != "[1 2]" {
		t.Fatalf("the tested result still applies: runs %d, landed %v", runs(), f.landed)
	}
	if !strings.Contains(f.recorded[2].CIURL, "exited 0") {
		t.Fatalf("%q", f.recorded[2].CIURL)
	}
}

func TestCrashMidBatchNeverMergesTwice(t *testing.T) {
	check, runs := counted(t, "true")
	f := newBatchFixture(t, check)
	f.run.c.MergeQueue.BatchSize = 3
	for i := 1; i <= 3; i++ {
		f.pr(i, fmt.Sprintf("f%d.txt", i), "x\n")
	}
	f.failLand = func(step batchStep) error {
		if step.Issue == 2 {
			return errCrash
		}
		return nil
	}
	if _, err := f.run.pass(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(f.landed) != "[1]" {
		t.Fatalf("landed %v", f.landed)
	}
	f.failLand = nil
	// A new worker: only the journal and GitHub survive.
	revived := *f.run
	revived.current = queueJournal{}
	f.run = &revived
	if outcome := f.drain(); outcome != queueIdle {
		t.Fatalf("outcome %d", outcome)
	}
	if fmt.Sprint(f.landed) != "[1 2 3]" || runs() != 1 {
		t.Fatalf("landed %v, runs %d", f.landed, runs())
	}
	if len(f.recorded) != 3 {
		t.Fatalf("recorded %v", f.recorded)
	}
}

func TestLostJournalAfterAMergeStaysHonest(t *testing.T) {
	check, runs := counted(t, "true")
	f := newBatchFixture(t, check)
	f.run.c.MergeQueue.BatchSize = 2
	f.pr(1, "f1.txt", "x\n")
	f.pr(2, "f2.txt", "x\n")
	f.failLand = func(step batchStep) error {
		if step.Issue == 2 {
			return errCrash
		}
		return nil
	}
	if _, err := f.run.pass(); err != nil {
		t.Fatal(err)
	}
	f.failLand = nil
	if err := os.Remove(f.run.journal); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour)
	if outcome := f.drain(); outcome != queueIdle {
		t.Fatalf("outcome %d", outcome)
	}
	if !strings.Contains(f.recorded[1].CIURL, "not known") || !queueValue.MatchString(f.recorded[1].CIURL) {
		t.Fatalf("what it can't know it says: %q", f.recorded[1].CIURL)
	}
	if !strings.Contains(f.recorded[2].CIURL, "exited 0") || runs() != 2 {
		t.Fatalf("the other is tested again on the new tip: %q, runs %d", f.recorded[2].CIURL, runs())
	}
	if fmt.Sprint(f.landed) != "[1 2]" {
		t.Fatalf("landed %v", f.landed)
	}
}

func TestForeignCommitBeforeALandingRetestsTheRest(t *testing.T) {
	check, runs := counted(t, "true")
	f := newBatchFixture(t, check)
	f.run.c.MergeQueue.BatchSize = 3
	for i := 1; i <= 3; i++ {
		f.pr(i, fmt.Sprintf("f%d.txt", i), "x\n")
	}
	f.beforeLand = func(step batchStep) {
		if step.Issue == 1 {
			f.beforeLand = nil
		}
	}
	// It lands just as #1 finishes, so #2 finds the tip moved.
	inner := f.run.ops.land
	f.run.ops.land = func(step batchStep, view mergeView, last bool) error {
		if err := inner(step, view, last); err != nil {
			return err
		}
		if step.Issue == 1 {
			f.run.ops.land = inner
			landOnMain(t, f.top, "foreign.txt", "x\n")
		}
		return nil
	}
	f.now = f.now.Add(time.Hour)
	outcome := f.drain()
	if fmt.Sprint(f.landed) != "[1 2 3]" {
		t.Fatalf("outcome %d, landed %v, hold %q", outcome, f.landed, f.run.current.Hold)
	}
	if runs() != 2 {
		t.Fatalf("the rest are tested on the new tip: %d runs", runs())
	}
}

func TestLandingThatIsNotWhatWasTestedHolds(t *testing.T) {
	f := newBatchFixture(t, "true")
	f.run.c.MergeQueue.BatchSize = 2
	f.pr(1, "f1.txt", "x\n")
	f.pr(2, "f2.txt", "x\n")
	f.tamper = func(step batchStep) {
		writeRepoFile(t, f.top, "surprise.txt", []byte("x\n"))
		commitAll(t, f.top)
		f.tamper = nil
	}
	if outcome := f.drain(); outcome != queueHeld {
		t.Fatalf("outcome %d", outcome)
	}
	if fmt.Sprint(f.landed) != "[1]" {
		t.Fatalf("nothing lands after it: %v", f.landed)
	}
	if !strings.Contains(f.run.current.Hold, "was not tested") || !strings.Contains(f.run.current.Hold, "passes on main") {
		t.Fatalf("hold %q", f.run.current.Hold)
	}
	// While held, what landed still gets its evidence.
	if outcome := f.drain(); outcome != queueHeld {
		t.Fatalf("outcome %d", outcome)
	}
	if !strings.Contains(f.recorded[1].Risks, "was not tested") {
		t.Fatalf("the evidence says so: %+v", f.recorded[1])
	}
}

func TestRepeatedRefusalsHold(t *testing.T) {
	f := newBatchFixture(t, "true")
	f.run.c.MergeQueue.BatchSize = 1
	f.pr(1, "f1.txt", "x\n")
	f.failLand = func(batchStep) error { return errors.New("refused") }
	var outcome queueOutcome
	for range queueRefusals {
		outcome = f.drain()
	}
	if outcome != queueHeld || !strings.Contains(f.run.current.Hold, "refused") {
		t.Fatalf("outcome %d, hold %q", outcome, f.run.current.Hold)
	}
}

func TestQueueEvidenceIsWhatTheWriterAccepts(t *testing.T) {
	head := strings.Repeat("a", 40)
	c := queueConfig("make check")
	batch := queueBatch{Base: strings.Repeat("b", 40), Command: `make "check"`, Runs: 2, Flakes: 1}
	good := writer.Evidence{CIURL: queueEvidence(head, batch, strings.Repeat("c", 40)), Boundary: "local"}
	view := prView{HeadRefOid: head}
	if err := checkQueueEvidence(c, good, view); err != nil {
		t.Fatal(err)
	}
	if err := checkQueueEvidence(c, writer.Evidence{CIURL: queueEvidenceUnknown(head), Boundary: "local"}, view); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		c model.Config
		e writer.Evidence
		v prView
	}{
		"queue off":       {model.Config{Merge: "agent"}, good, view},
		"other head":      {c, good, prView{HeadRefOid: strings.Repeat("d", 40)}},
		"boundary":        {c, writer.Evidence{CIURL: good.CIURL, Boundary: "CI"}, view},
		"a made up value": {c, writer.Evidence{CIURL: queuePrefix + head + "; trust me", Boundary: "local"}, view},
		"a check failed":  {c, good, prView{HeadRefOid: head, Checks: []prCheck{{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"}}}},
	} {
		if err := checkQueueEvidence(tc.c, tc.e, tc.v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestQueueConfigValidates(t *testing.T) {
	retries := -1
	for name, c := range map[string]model.Config{
		"no check":         {Merge: "queue"},
		"two lines":        {Merge: "queue", MergeQueue: model.MergeQueue{Check: "a\nb"}},
		"negative":         {Merge: "queue", MergeQueue: model.MergeQueue{Check: "a", BatchSize: -1}},
		"negative retries": {Merge: "queue", MergeQueue: model.MergeQueue{Check: "a", FlakyRetries: &retries}},
		"background":       {Merge: "queue", MergeWait: "background", MergeQueue: model.MergeQueue{Check: "a"}},
		"without ci":       {Merge: "queue", MergeWithoutCI: true, MergeQueue: model.MergeQueue{Check: "a"}},
		"settings alone":   {Merge: "agent", MergeQueue: model.MergeQueue{Check: "a"}},
	} {
		if err := c.ValidateMergeQueue(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := queueConfig("make")
	if err := c.ValidateMergeQueue(); err != nil {
		t.Fatal(err)
	}
	empty := model.Config{Merge: "queue"}
	if empty.QueueBatchSize() != 4 || empty.QueueMaxWait() != 30*time.Minute || empty.QueueFlakyRetries() != 1 {
		t.Fatal("defaults")
	}
	if c.AgentsMerge() || !c.MergesInQueue() {
		t.Fatal("the queue is its own mode")
	}
	if off := c.WithoutRelaxations(); off.MergesInQueue() || off.MergeQueue.Check != "" {
		t.Fatal("a config that can't be trusted merges nothing")
	}
}

func TestProtectAndDoctorKnowTheQueue(t *testing.T) {
	c := queueConfig("make")
	rule := mainRuleset(c, false)
	var strict any
	for _, r := range rule["rules"].([]map[string]any) {
		if r["type"] == "required_status_checks" {
			strict = r["parameters"].(map[string]any)["strict_required_status_checks_policy"]
		}
	}
	if strict != false {
		t.Fatalf("strict %v", strict)
	}
	if on := mainRuleset(model.Config{}, false); fmt.Sprint(on["rules"]) == fmt.Sprint(rule["rules"]) {
		t.Fatal("every other mode stays strict")
	}
	strictRule := ruleset{Name: "main", Enforcement: "active", Target: "branch"}
	strictRule.Rules = append(strictRule.Rules, struct {
		Type       string `json:"type"`
		Parameters struct {
			Strict bool `json:"strict_required_status_checks_policy"`
			Checks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}{Type: "required_status_checks"})
	strictRule.Rules[0].Parameters.Strict = true
	if found := judgeRulesets(c, []ruleset{strictRule}); len(found) != 1 || found[0].Name != "merge: queue" {
		t.Fatalf("%+v", found)
	}
	if found := judgeRulesets(model.Config{MergeWithoutCI: true, Merge: "agent", LocalCheck: "x"}, []ruleset{strictRule}); len(found) != 0 {
		t.Fatalf("the other modes don't mind: %+v", found)
	}
}

func TestStatusFoldsTheQueueIn(t *testing.T) {
	report := statusReport{Attention: []string{}, OwnerQueue: []ownerItem{}}
	addQueueToReport(&report, nil)
	if report.Queue != nil {
		t.Fatal("not in queue mode")
	}
	addQueueToReport(&report, &queueReport{Returned: []queueReturned{{Issue: 3, Reason: "bad\nlong log"}}, Flaked: "once", Held: "stop\nlog"})
	if report.Queue == nil || len(report.Attention) != 2 || len(report.OwnerQueue) != 1 {
		t.Fatalf("%+v", report)
	}
	if strings.Contains(report.Attention[0], "long log") || !strings.Contains(report.OwnerQueue[0].Reason, "quipu queue resume") {
		t.Fatalf("%+v", report)
	}
	if text := renderQueue(&queueReport{Entries: []queueEntry{{Issue: 1, PR: "u"}}, Worker: "running", Held: "x"}); !strings.Contains(text, "1 waiting") || !strings.Contains(text, "held") {
		t.Fatal(text)
	}
}

func TestGuardOnQueueCommands(t *testing.T) {
	f := newGuardFixture(t)
	if code := guardCode(t, f.a, "Bash", map[string]any{"command": "quipu queue status --json"}); code != "" {
		t.Errorf("status: %s", code)
	}
	for _, command := range []string{"quipu queue run", "quipu queue resume"} {
		if code := guardCode(t, f.a, "Bash", map[string]any{"command": command}); code != "quipu-admin" {
			t.Errorf("%s: %s", command, code)
		}
	}
}

func TestMergeMessageKeepsTheCommitFormatFooter(t *testing.T) {
	view := mergeView{Title: "feat(queue): land batches"}
	prefixed := model.Config{CommitFormat: policycheck.IssuePrefix}
	message, err := mergeMessage(prefixed, 42, view, "[skip ci]")
	if err != nil {
		t.Fatal(err)
	}
	body := message[3]
	if message[2] != "--body" || !strings.Contains(body, "Refs: #42") || !strings.Contains(body, "[skip ci]") {
		t.Fatalf("message = %q, want the footer and the marker in one body", message)
	}
	if _, err := policycheck.CheckIssuePrefix("#042 feat(queue): land batches\n\n"+body, 42); err != nil {
		t.Fatalf("the marked merge message breaks the format: %v", err)
	}
	message, err = mergeMessage(prefixed, 42, view, "")
	if err != nil || strings.Contains(strings.Join(message, " "), "skip ci") {
		t.Fatalf("message = %q, %v, want no marker", message, err)
	}
	message, err = mergeMessage(model.Config{}, 42, view, "[skip ci]")
	if err != nil || strings.Join(message, " ") != "--body [skip ci]" {
		t.Fatalf("message = %q, %v", message, err)
	}
	if message, err = mergeMessage(model.Config{}, 42, view, ""); err != nil || len(message) != 0 {
		t.Fatalf("message = %q, %v, want GitHub's default message", message, err)
	}
}

func TestQueuePreparation(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	live, lapsed := now.Add(time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339)
	for _, test := range []struct {
		name, status, holder, expiry string
		want                         string
	}{
		{"in progress queues at once", "In progress", "me", live, ""},
		{"claimed starts first", "Claimed", "me", live, "start"},
		{"a lapsed lease renews", "In progress", "me", lapsed, "renew"},
		{"lapsed and claimed does both, in order", "Claimed", "me", lapsed, "renew start"},
		{"in review needs nothing", "In review", "me", lapsed, ""},
		{"another holder's lease is not ours to renew", "In progress", "other", lapsed, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := liveWork{Status: test.status, Holder: test.holder, Expiry: test.expiry}
			if got := strings.Join(queuePreparation(state, "me", now), " "); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestQueueStateReportsARejectedBatchRequest(t *testing.T) {
	body, err := writer.RenderRequest(writer.Request{ID: "1", IssueID: "I", Actor: "me", ExpectedFingerprint: "f", Action: "merge.batch", PR: "url", Head: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	comments := []github.Comment{{Body: body, CreatedAt: "2026-11-03T12:00:00Z"}}
	if got := queueStateOf(comments, nil); got.State != "queued" {
		t.Fatalf("unanswered = %+v, want queued", got)
	}
	rejected := []writer.Receipt{{RequestID: "1", Result: "rejected", Detail: "stale"}}
	if got := queueStateOf(comments, rejected); got.State != "rejected" || got.Detail != "stale" {
		t.Fatalf("rejected = %+v", got)
	}
}

func TestQueueGateSparesTheBoard(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	newest, stamp := int64(10), "a"
	var latestErr error
	gate := newQueueGate(func() (int64, error) { return newest, latestErr }, func() string { return stamp })
	gate.passed(now, queueWaiting, nil, time.Time{})

	if !gate.quiet(now.Add(time.Minute)) {
		t.Fatal("nothing new: want no pass")
	}
	if gate.quiet(now.Add(queueQuietPass)) != false {
		t.Fatal("the quiet limit must force a pass")
	}
	gate.passed(now, queueIdle, nil, now.Add(2*time.Minute))
	if !gate.quiet(now.Add(time.Minute)) {
		t.Fatal("before the batch timer: want no pass")
	}
	if gate.quiet(now.Add(2 * time.Minute)) {
		t.Fatal("the batch timer must force a pass")
	}

	gate.passed(now, queueWaiting, nil, time.Time{})
	newest = 11
	if gate.quiet(now.Add(time.Minute)) {
		t.Fatal("a new comment must force a pass")
	}
	gate.passed(now.Add(time.Minute), queueWaiting, nil, time.Time{})
	if gate.quiet(now.Add(time.Minute + 30*time.Second)) {
		t.Fatal("the board can lag the receipt: want another pass while it settles")
	}
	gate.passed(now.Add(time.Minute+30*time.Second), queueWaiting, nil, time.Time{})
	if !gate.quiet(now.Add(3 * time.Minute)) {
		t.Fatal("settled: want no pass")
	}

	stamp = "b"
	if gate.quiet(now.Add(3 * time.Minute)) {
		t.Fatal("queue resume wrote the journal: want a pass")
	}
	gate.passed(now.Add(3*time.Minute), queueHeld, nil, time.Time{})
	if !gate.quiet(now.Add(4 * time.Minute)) {
		t.Fatal("held with nothing new: want no pass")
	}

	latestErr = errors.New("GitHub is down")
	if gate.quiet(now.Add(4 * time.Minute)) {
		t.Fatal("not being able to tell must mean a pass")
	}
	latestErr = nil

	for _, test := range []struct {
		name    string
		outcome queueOutcome
		err     error
	}{{"moved", queueMoved, nil}, {"failed", queueWaiting, errors.New("rate limited")}} {
		gate.passed(now.Add(5*time.Minute), test.outcome, test.err, time.Time{})
		if gate.quiet(now.Add(5 * time.Minute)) {
			t.Fatalf("%s: want another pass at once", test.name)
		}
	}
}

func TestWaitingBatchNamesWhenItIsDue(t *testing.T) {
	check, _ := counted(t, "true")
	f := newBatchFixture(t, check)
	f.pr(1, "f1.txt", "x\n")
	if outcome, err := f.run.pass(); err != nil || outcome != queueWaiting {
		t.Fatalf("pass = %v, %v", outcome, err)
	}
	want := f.members[0].QueuedAt.Add(f.run.c.QueueMaxWait())
	if !f.run.wake.Equal(want) {
		t.Fatalf("wake = %v, want %v", f.run.wake, want)
	}
}
