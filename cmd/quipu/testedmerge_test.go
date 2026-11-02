package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/isnakolah/quipu/internal/writer"
)

func TestBehindState(t *testing.T) {
	for _, tc := range []struct {
		state  string
		behind int
		want   string
	}{
		{"CLEAN", 0, "CLEAN"},
		{"CLEAN", 2, "BEHIND"},    // no rule requires branches to be up to date
		{"UNSTABLE", 1, "BEHIND"}, // nor when a check that isn't required failed
		{"BLOCKED", 1, "BEHIND"},  // nor when checks are waiting
		{"UNKNOWN", 1, "BEHIND"},  // the compare knows even while GitHub doesn't
		{"BEHIND", 3, "BEHIND"},   // both agree
		{"DIRTY", 3, "DIRTY"},     // a conflict stays a conflict
		{"BEHIND", 0, "UNKNOWN"},  // GitHub catching up with a push
		{"BLOCKED", 0, "BLOCKED"}, // nothing to add
		{"HAS_HOOKS", 0, "HAS_HOOKS"},
	} {
		if got := behindState(tc.state, tc.behind); got != tc.want {
			t.Errorf("%s behind by %d: got %s, want %s", tc.state, tc.behind, got, tc.want)
		}
	}
}

func TestBaseRaceNote(t *testing.T) {
	for _, tc := range []struct {
		tested, parent string
		noted          bool
	}{
		{"same", "same", false},
		{"", "parent", false},
		{"tested", "", false},
		{"tested", "moved", true},
	} {
		if note := baseRaceNote(tc.tested, tc.parent); (note != "") != tc.noted {
			t.Errorf("%q %q: %q", tc.tested, tc.parent, note)
		}
	}
	e := writer.Evidence{Risks: "None"}
	addRisk(&e, "")
	if e.Risks != "None" {
		t.Fatalf("no note changes nothing: %q", e.Risks)
	}
	addRisk(&e, "first")
	addRisk(&e, "second")
	if e.Risks != "first; second" {
		t.Fatalf("got %q", e.Risks)
	}
}

// localPull is a PR from the agent's worktree against a real local origin, with
// only GitHub's answers faked. Its merge is a real merge commit on origin's main,
// and its check is a real shell command in the worktree.
type localPull struct {
	t      *testing.T
	top    string
	agent  worktree
	merged int
	checks int
	// during runs inside the check, for what moves while it takes its time.
	during func(run int)
}

func (p *localPull) ops() noCIOps {
	return noCIOps{
		view: func() (mergeView, error) {
			gitIn(p.t, p.agent.Top, "fetch", "-q", "origin")
			head, err := p.agent.git("rev-parse", "origin/001/a")
			if err != nil {
				return mergeView{}, err
			}
			base, err := p.agent.git("rev-parse", "origin/main")
			if err != nil {
				return mergeView{}, err
			}
			count, err := p.agent.git("rev-list", "--count", head+"..origin/main")
			if err != nil {
				return mergeView{}, err
			}
			behind, _ := strconv.Atoi(count)
			return mergeView{URL: "url", State: "OPEN", HeadRefOid: head, BaseOID: base, MergeStateStatus: behindState("CLEAN", behind),
				Checks: []prCheck{{Name: "test", Status: "QUEUED"}, {Name: "policy", Status: "COMPLETED", Conclusion: "SUCCESS"}}}, nil
		},
		update:     func() (string, error) { return mergeDefaultIn(p.agent, "001/a", "main") },
		pause:      func() {},
		lock:       p.agent.mergeLock,
		head:       func() (string, error) { return p.agent.git("rev-parse", "HEAD") },
		dirty:      func() (string, error) { return p.agent.git("status", "--porcelain") },
		violations: func() ([]string, error) { return nil, nil },
		check: func(command string) (int, error) {
			p.checks++
			if p.during != nil {
				p.during(p.checks)
			}
			return runLocalCheck(p.agent.Top, command)
		},
		record:        func(_, _ string) {},
		forget:        func(string) {},
		adminBlockers: func(mergeView) ([]string, error) { return nil, nil },
		merge: func(view mergeView, _ bool) error {
			p.merged++
			gitIn(p.t, p.top, "fetch", "-q", "origin")
			gitIn(p.t, p.top, "-c", "user.name=t", "-c", "user.email=t@example.com", "merge", "-q", "--no-ff", "--no-edit", view.HeadRefOid)
			gitIn(p.t, p.top, "push", "-q", "origin", "main")
			return nil
		},
	}
}

// landOnMain commits a change straight to origin's main, as another PR's merge would.
func landOnMain(t *testing.T, top, name, content string) {
	t.Helper()
	gitIn(t, top, "pull", "-q", "--no-edit", "origin", "main")
	writeRepoFile(t, top, name, []byte(content))
	commitAll(t, top)
	gitIn(t, top, "push", "-q", "origin", "main")
}

func gitIdentity(t *testing.T) {
	t.Helper()
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(name, "t")
	}
	for _, name := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(name, "t@example.com")
	}
}

// callsResolve is the semantic check: every name the callers use must exist.
const callsResolve = `while read -r name; do grep -qx "$name" lib.txt || exit 1; done < calls.txt`

// semanticConflict is two PRs that touch different files, merge cleanly, and each
// pass alone: one renames what the other starts calling.
func semanticConflict(t *testing.T) *localPull {
	t.Helper()
	gitIdentity(t)
	top, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	landOnMain(t, top, "lib.txt", "greet\n")
	landOnMain(t, top, "calls.txt", "")
	gitIn(t, agent.Top, "merge", "-q", "--no-edit", "origin/main")
	writeRepoFile(t, agent.Top, "calls.txt", []byte("greet\n"))
	commitAll(t, agent.Top)
	gitIn(t, agent.Top, "push", "-q", "origin", "001/a")
	if exit, err := runLocalCheck(agent.Top, callsResolve); err != nil || exit != 0 {
		t.Fatalf("this PR must pass alone: exit %d, %v", exit, err)
	}
	// The other PR lands first, and passes too: nothing on main calls greet yet.
	landOnMain(t, top, "lib.txt", "hello\n")
	if exit, err := runLocalCheck(top, callsResolve); err != nil || exit != 0 {
		t.Fatalf("the other PR must pass alone: exit %d, %v", exit, err)
	}
	return &localPull{t: t, top: top, agent: agent}
}

func TestTwoPRsThatEachPassAloneDoNotBreakMain(t *testing.T) {
	config := noCIConfig
	config.LocalCheck, config.UpdateBehind = callsResolve, "auto"

	t.Run("update_behind tests the combination and refuses it", func(t *testing.T) {
		p := semanticConflict(t)
		value, err := mergeWithoutCI(config, false, localEvidence, p.ops())
		if flowCode(err) != "local-check-failed" || value != "" {
			t.Fatalf("the combination must fail its check: %q, %v", value, err)
		}
		if p.merged != 0 {
			t.Fatalf("merged %d", p.merged)
		}
		gitIn(t, p.top, "pull", "-q", "--no-edit", "origin", "main")
		if exit, err := runLocalCheck(p.top, callsResolve); err != nil || exit != 0 {
			t.Fatalf("main must stay good: exit %d, %v", exit, err)
		}
	})

	t.Run("without update_behind nothing tests it, so nothing merges it", func(t *testing.T) {
		p := semanticConflict(t)
		off := config
		off.UpdateBehind = ""
		if _, err := mergeWithoutCI(off, false, localEvidence, p.ops()); flowCode(err) != "merge-blocked" || p.merged != 0 || p.checks != 0 {
			t.Fatalf("a behind PR is refused: %v, merged %d, checks %d", err, p.merged, p.checks)
		}
	})
}

func TestUpdatedPRMergesWhatItTested(t *testing.T) {
	gitIdentity(t)
	top, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	writeRepoFile(t, agent.Top, "branch.txt", []byte("branch\n"))
	commitAll(t, agent.Top)
	gitIn(t, agent.Top, "push", "-q", "origin", "001/a")
	landOnMain(t, top, "other.txt", "other\n")

	config := noCIConfig
	config.LocalCheck, config.UpdateBehind = "test -f other.txt && test -f branch.txt", "auto"
	p := &localPull{t: t, top: top, agent: agent}
	value, err := mergeWithoutCI(config, false, localEvidence, p.ops())
	if err != nil {
		t.Fatal(err)
	}
	if p.merged != 1 || p.checks != 1 {
		t.Fatalf("merged %d, checks %d", p.merged, p.checks)
	}
	head, _ := agent.git("rev-parse", "HEAD")
	if !strings.Contains(value, "at "+head+";") {
		t.Fatalf("the evidence must name the head it checked, %s: %s", head, value)
	}
	if err := checkNoCIEvidence(config, writer.Evidence{CIURL: value, Boundary: "local"}, prView{HeadRefOid: head}); err != nil {
		t.Fatalf("the Writer must accept it: %v", err)
	}
}

func TestBaseMovingDuringTheCheckRunsItAgain(t *testing.T) {
	gitIdentity(t)
	top, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	writeRepoFile(t, agent.Top, "branch.txt", []byte("branch\n"))
	commitAll(t, agent.Top)
	gitIn(t, agent.Top, "push", "-q", "origin", "001/a")

	config := noCIConfig
	config.LocalCheck, config.UpdateBehind = "true", "auto"
	p := &localPull{t: t, top: top, agent: agent, during: func(run int) {
		if run == 1 {
			landOnMain(t, top, "moved.txt", "moved\n")
		}
	}}
	if _, err := mergeWithoutCI(config, false, localEvidence, p.ops()); err != nil {
		t.Fatal(err)
	}
	if p.checks != 2 || p.merged != 1 {
		t.Fatalf("checks %d, merged %d", p.checks, p.merged)
	}
	if _, err := os.Stat(filepath.Join(agent.Top, "moved.txt")); err != nil {
		t.Fatalf("the second run must have checked the new base: %v", err)
	}
}

func TestABaseThatKeepsMovingStopsWithItsOwnCode(t *testing.T) {
	gitIdentity(t)
	top, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	writeRepoFile(t, agent.Top, "branch.txt", []byte("branch\n"))
	commitAll(t, agent.Top)
	gitIn(t, agent.Top, "push", "-q", "origin", "001/a")

	config := noCIConfig
	config.LocalCheck, config.UpdateBehind = "true", "auto"
	p := &localPull{t: t, top: top, agent: agent, during: func(run int) {
		landOnMain(t, top, "moved-"+strconv.Itoa(run)+".txt", "moved\n")
	}}
	_, err := mergeWithoutCI(config, false, localEvidence, p.ops())
	if flowCode(err) != "base-moved" {
		t.Fatalf("got %v", err)
	}
	if p.checks != noCIAttempts || p.merged != 0 {
		t.Fatalf("checks %d, merged %d", p.checks, p.merged)
	}
}

func TestLaggingHeadAsksForARerunWithoutChecking(t *testing.T) {
	c := noCIConfig
	c.UpdateBehind = "auto"
	f := pendingPull()
	f.view.MergeStateStatus = "BEHIND"
	ops := f.ops()
	ops.update = func() (string, error) { f.updates++; f.head = "updated"; return "updated", nil }
	if _, err := mergeWithoutCI(c, false, localEvidence, ops); flowCode(err) != "checks-pending" || len(f.ran) != 0 || len(f.merges) != 0 {
		t.Fatalf("GitHub never showed the new head, so nothing can be checked: %v, ran %v, merges %v", err, f.ran, f.merges)
	}
}

func TestMergeHoldsTheCloneLock(t *testing.T) {
	f := pendingPull()
	if _, err := mergeWithoutCI(noCIConfig, false, localEvidence, f.ops()); err != nil {
		t.Fatal(err)
	}
	if f.locks != 1 {
		t.Fatalf("locks %d", f.locks)
	}
}

func TestMergeLockIsReleased(t *testing.T) {
	_, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	for range 2 {
		unlock, err := agent.mergeLock()
		if err != nil {
			t.Fatal(err)
		}
		unlock()
	}
}

func TestCleanupRunsAfterTheCheckNeedsIt(t *testing.T) {
	gitIdentity(t)
	top, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	writeRepoFile(t, agent.Top, ".gitignore", []byte("node_modules/\n"))
	commitAll(t, agent.Top)
	gitIn(t, agent.Top, "push", "-q", "origin", "001/a")
	writeRepoFile(t, agent.Top, "node_modules/dep.txt", []byte("dep\n"))

	config := noCIConfig
	config.LocalCheck = "test -f node_modules/dep.txt"
	p := &localPull{t: t, top: top, agent: agent}
	if _, err := mergeWithoutCI(config, false, localEvidence, p.ops()); err != nil {
		t.Fatalf("the check needs what cleanup removes: %v", err)
	}
	if err := agent.writeLease(leaseContext{Issue: 1, Holder: "h", Branch: "001/a"}); err != nil {
		t.Fatal(err)
	}
	if warnings := agent.release(1, "rm -rf node_modules"); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	if _, err := os.Stat(filepath.Join(agent.Top, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("cleanup must have run: %v", err)
	}
	if _, err := agent.readLease(); err == nil {
		t.Fatal("the lease must be gone")
	}
}

func TestReleaseRunsCleanupOnce(t *testing.T) {
	top, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	counter := filepath.Join(t.TempDir(), "ran")
	command := "echo x >> " + counter
	lease := leaseContext{Issue: 1, Holder: "h", Branch: "001/a"}

	if err := agent.writeLease(lease); err != nil {
		t.Fatal(err)
	}
	agent.release(2, command)
	if _, err := agent.readLease(); err != nil {
		t.Fatal("another Issue's release must leave this lease alone")
	}
	agent.release(1, command)
	agent.release(1, command) // a rerun of quipu done
	raw, err := os.ReadFile(counter)
	if err != nil || strings.Count(string(raw), "x") != 1 {
		t.Fatalf("got %q, %v", raw, err)
	}

	if err := agent.writeLease(lease); err != nil {
		t.Fatal(err)
	}
	if warnings := agent.release(1, "exit 3"); len(warnings) != 1 || !strings.Contains(warnings[0], "exited 3") {
		t.Fatalf("a failure is a warning: %v", warnings)
	}
	if _, err := agent.readLease(); err == nil {
		t.Fatal("a failed cleanup must not keep the lease")
	}

	main, err := openWorktree(top)
	if err != nil {
		t.Fatal(err)
	}
	if err := main.writeLease(lease); err != nil {
		t.Fatal(err)
	}
	if warnings := main.release(1, "exit 3"); warnings != nil {
		t.Fatalf("the main checkout is never cleaned: %v", warnings)
	}
	if _, err := main.readLease(); err == nil {
		t.Fatal("its lease still goes")
	}
}

func TestCleanupComesOnlyFromTheDefaultBranch(t *testing.T) {
	c := noCIConfig
	c.Cleanup = "rm -rf target"
	if enforcedCleanup(c, "origin/main:quipu.yml") != "rm -rf target" {
		t.Fatal("the default branch's copy runs")
	}
	if enforcedCleanup(c, configSourceWorktree) != "" {
		t.Fatal("a worktree's copy never runs")
	}
	if c.WithoutRelaxations().Cleanup != "" {
		t.Fatal("a config that can't be trusted runs nothing")
	}
}

func TestQueuedMergeRecordsARaceInTheRisks(t *testing.T) {
	view := greenView()
	view.BaseOID = "tested"
	f := newQueueFixture(t, backgroundMerge, view)
	f.parent = "moved"
	if moved, err := f.w.queuedMerge(f.state); err != nil || !moved {
		t.Fatalf("%v %v", moved, err)
	}
	if len(f.requests) == 0 || f.requests[0].Evidence == nil || !strings.Contains(f.requests[0].Evidence.Risks, "not tested") {
		t.Fatalf("requests %+v", f.requests)
	}
}
