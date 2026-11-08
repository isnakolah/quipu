package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

const finishedHolder = "claude/octo/aaaa1111"

func TestLeaseStillHeld(t *testing.T) {
	lease := leaseContext{Issue: 1, Holder: finishedHolder, Branch: "001/a"}
	for _, tc := range []struct {
		state liveWork
		want  bool
	}{
		{liveWork{Status: "In progress", Holder: finishedHolder, Branch: "001/a"}, true},
		{liveWork{Status: "Claimed", Holder: finishedHolder, Branch: "001/a"}, true},
		{liveWork{Status: "In review", Holder: finishedHolder, Branch: "001/a"}, true},
		{liveWork{Status: "Evidence pending", Holder: finishedHolder, Branch: "001/a"}, true},
		{liveWork{Status: "Done", Holder: finishedHolder, Branch: "001/a"}, false},
		{liveWork{Status: "Cancelled", Holder: finishedHolder, Branch: "001/a"}, false},
		{liveWork{Status: "Ready", Branch: "001/a"}, false},
		{liveWork{Status: "In progress", Holder: "claude/octo/other", Branch: "001/a"}, false},
		{liveWork{Status: "In progress", Holder: finishedHolder, Branch: "001/other"}, false},
	} {
		if got := leaseStillHeld(lease, tc.state); got != tc.want {
			t.Errorf("%+v: got %v", tc.state, got)
		}
	}
}

// removalCode runs `git worktree remove` for the lease in f.a through the guard.
func removalCode(t *testing.T, cwd, target string, live func(model.Config, int) (liveWork, error)) error {
	t.Helper()
	g := newGuard(cwd, time.Now().UTC())
	g.caller = fixtureCaller
	g.live = live
	raw, err := json.Marshal(map[string]any{"command": "git worktree remove " + target})
	if err != nil {
		t.Fatal(err)
	}
	return g.check(hookInput{ToolName: "Bash", Cwd: cwd, ToolInput: raw})
}

func TestGuardFreesAWorktreeWhoseLeaseIsDead(t *testing.T) {
	done := func(model.Config, int) (liveWork, error) {
		return liveWork{Status: "Done", Holder: finishedHolder, Branch: "001/a"}, nil
	}
	active := func(model.Config, int) (liveWork, error) {
		return liveWork{Status: "In review", Holder: "claude/octo/aaaa1111", Branch: "001/a"}, nil
	}
	unreachable := func(model.Config, int) (liveWork, error) { return liveWork{}, errors.New("no network") }
	unlisted := func(model.Config, int) (liveWork, error) { return liveWork{}, errNotInProject }

	for name, tc := range map[string]struct {
		live  func(model.Config, int) (liveWork, error)
		from  func(guardFixture) string
		code  string
		freed bool
	}{
		"Done, from the holder":                {done, func(f guardFixture) string { return f.a }, "", true},
		"Done, from a dispatcher":              {done, func(f guardFixture) string { return f.top }, "", true},
		"Done, from another agent's worktree":  {done, func(f guardFixture) string { return f.b }, "", true},
		"live, from the holder":                {active, func(f guardFixture) string { return f.a }, "worktree-busy", false},
		"live, from a dispatcher":              {active, func(f guardFixture) string { return f.top }, "worktree-busy", false},
		"GitHub unreachable fails closed":      {unreachable, func(f guardFixture) string { return f.top }, "worktree-busy", false},
		"left the Project, so nothing to hold": {unlisted, func(f guardFixture) string { return f.top }, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newGuardFixture(t)
			a, err := openWorktree(f.a)
			if err != nil {
				t.Fatal(err)
			}
			// The cache says the lease is fresh, and that must not settle it.
			lease, err := a.readLease()
			if err != nil || !lease.fresh(time.Now().UTC()) {
				t.Fatalf("the fixture's lease must look fresh: %+v, %v", lease, err)
			}
			err = removalCode(t, tc.from(f), f.a, tc.live)
			var d denial
			if errors.As(err, &d) != (tc.code != "") || (tc.code != "" && d.Code != tc.code) {
				t.Fatalf("got %v, want %q", err, tc.code)
			}
			if _, err := a.readLease(); (err != nil) != tc.freed {
				t.Fatalf("lease file freed: %v, want %v", err, tc.freed)
			}
		})
	}
}

func TestGuardAllowsQuipuWorktreeRemove(t *testing.T) {
	f := newGuardFixture(t)
	for _, cwd := range []string{f.a, f.b, f.top} {
		if code := guardCode(t, cwd, "Bash", map[string]any{"command": "quipu worktree remove 1 --json"}); code != "" {
			t.Errorf("%s: %s", cwd, code)
		}
	}
	if code := guardCode(t, f.top, "Bash", map[string]any{"command": "quipu worktree add x"}); code != "quipu-admin" {
		t.Errorf("only remove is an agent's: %s", code)
	}
}

func TestParseWorktreeList(t *testing.T) {
	entries := parseWorktreeList("worktree /repo\nHEAD abc\nbranch refs/heads/main\n\nworktree /wt\nHEAD def\nbranch refs/heads/001/a\n\nworktree /detached\nHEAD ghi\ndetached\n")
	if len(entries) != 3 || entries[0] != (worktreeEntry{"/repo", "main"}) || entries[1] != (worktreeEntry{"/wt", "001/a"}) || entries[2].Branch != "" {
		t.Fatalf("got %+v", entries)
	}
}

func TestIssueOfBranch(t *testing.T) {
	for branch, want := range map[string]int{"118/fix": 118, "007/x": 7, "main": 0, "feat/x": 0, "": 0} {
		if got := issueOfBranch(branch); got != want {
			t.Errorf("%q: %d", branch, got)
		}
	}
}

func TestFindRemovable(t *testing.T) {
	dir := t.TempDir()
	main, wt, other := filepath.Join(dir, "main"), filepath.Join(dir, "wt"), filepath.Join(dir, "other")
	entries := []worktreeEntry{{main, "main"}, {wt, "118/fix"}, {other, "feature"}}
	leases := func(path string) int {
		if path == other {
			return 9
		}
		return 0
	}
	for target, want := range map[string]string{"118": wt, "#118": wt, "9": other, wt: wt} {
		entry, err := findRemovable(entries, target, leases)
		if err != nil || entry.Path != want {
			t.Errorf("%s: %+v, %v", target, entry, err)
		}
	}
	for _, target := range []string{"7", main, filepath.Join(dir, "nowhere")} {
		if _, err := findRemovable(entries, target, leases); flowCode(err) != "worktree-missing" {
			t.Errorf("%s: %v", target, err)
		}
	}
}

// finishedWorktree is a linked worktree on 001/a with a pushed commit.
func finishedWorktree(t *testing.T) (worktree, worktree, worktreeEntry) {
	t.Helper()
	gitIdentity(t)
	top, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	writeRepoFile(t, agent.Top, "work.txt", []byte("work\n"))
	commitAll(t, agent.Top)
	gitIn(t, agent.Top, "push", "-q", "origin", "001/a")
	root, err := openWorktree(top)
	if err != nil {
		t.Fatal(err)
	}
	return root, agent, worktreeEntry{agent.Top, "001/a"}
}

func liveStatus(status string) func(int) (liveWork, error) {
	return func(int) (liveWork, error) {
		return liveWork{Status: status, Holder: finishedHolder, Branch: "001/a"}, nil
	}
}

func TestRemoveDoneWorktreeTakesEverything(t *testing.T) {
	root, agent, entry := finishedWorktree(t)
	if err := agent.writeLease(leaseContext{Issue: 1, Holder: finishedHolder, Branch: "001/a"}); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, agent.Top, "target/huge.bin", []byte("build output\n"))
	writeRepoFile(t, agent.Top, "stray.txt", []byte("left behind\n"))
	result, err := removeWorktreeTree(root, entry, "", liveStatus("Done"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "Removed" || result.Issue != 1 || result.Worktree != agent.Top {
		t.Fatalf("got %+v", result)
	}
	if _, err := os.Stat(agent.Top); !os.IsNotExist(err) {
		t.Fatalf("the worktree must be gone: %v", err)
	}
	if root.hasBranch("001/a") {
		t.Fatal("a merged local branch goes with it")
	}
	if listed, _ := root.git("worktree", "list"); strings.Contains(listed, "agent-worktree") {
		t.Fatalf("must be pruned: %s", listed)
	}
}

func TestRemoveRunsCleanupFirst(t *testing.T) {
	root, agent, entry := finishedWorktree(t)
	proof := filepath.Join(t.TempDir(), "cleaned")
	if _, err := removeWorktreeTree(root, entry, "pwd > "+proof, liveStatus("Done")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(proof)
	if err != nil || strings.TrimSpace(string(raw)) != agent.Top {
		t.Fatalf("cleanup must run in the worktree: %q, %v", raw, err)
	}
}

func TestRemoveWarnsWhenCleanupFails(t *testing.T) {
	root, agent, entry := finishedWorktree(t)
	result, err := removeWorktreeTree(root, entry, "exit 4", liveStatus("Done"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "exited 4") {
		t.Fatalf("got %+v", result)
	}
	if _, err := os.Stat(agent.Top); !os.IsNotExist(err) {
		t.Fatal("the worktree still goes")
	}
}

func TestRemoveProtectsWorkThatIsNotDone(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(t *testing.T, agent worktree)
		live  func(int) (liveWork, error)
		code  string
	}{
		"live lease": {func(t *testing.T, agent worktree) {
			if err := agent.writeLease(leaseContext{Issue: 1, Holder: finishedHolder, Branch: "001/a"}); err != nil {
				t.Fatal(err)
			}
		}, liveStatus("In progress"), "worktree-held"},
		"tracked change": {func(t *testing.T, agent worktree) {
			writeRepoFile(t, agent.Top, "work.txt", []byte("changed\n"))
		}, liveStatus("Ready"), "worktree-dirty"},
		"new file": {func(t *testing.T, agent worktree) {
			writeRepoFile(t, agent.Top, "new.txt", []byte("new\n"))
		}, liveStatus("Ready"), "worktree-dirty"},
		"unpushed commit": {func(t *testing.T, agent worktree) {
			writeRepoFile(t, agent.Top, "new.txt", []byte("new\n"))
			commitAll(t, agent.Top)
		}, liveStatus("In review"), "worktree-dirty"},
	} {
		t.Run(name, func(t *testing.T) {
			root, agent, entry := finishedWorktree(t)
			tc.setup(t, agent)
			if _, err := removeWorktreeTree(root, entry, "", tc.live); flowCode(err) != tc.code {
				t.Fatalf("got %v, want %s", err, tc.code)
			}
			if _, err := os.Stat(agent.Top); err != nil {
				t.Fatalf("it must stay: %v", err)
			}
		})
	}

	t.Run("no answer from GitHub", func(t *testing.T) {
		root, agent, entry := finishedWorktree(t)
		if _, err := removeWorktreeTree(root, entry, "", func(int) (liveWork, error) { return liveWork{}, errors.New("no network") }); err == nil {
			t.Fatal("no answer, no removal")
		}
		if _, err := os.Stat(agent.Top); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("clean and pushed keeps its branch", func(t *testing.T) {
		root, agent, entry := finishedWorktree(t)
		result, err := removeWorktreeTree(root, entry, "", liveStatus("In review"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(agent.Top); !os.IsNotExist(err) {
			t.Fatal("a queued PR's worktree is free to go")
		}
		if !root.hasBranch("001/a") || result.Branch != "001/a" {
			t.Fatalf("work that isn't Done keeps its local branch: %+v", result)
		}
	})
}

func TestRemovingAFinishedWorktreeFromInsideIt(t *testing.T) {
	root, agent, entry := finishedWorktree(t)
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(before) })
	if err := os.Chdir(agent.Top); err != nil {
		t.Fatal(err)
	}
	if _, err := removeWorktreeTree(root, entry, "", liveStatus("Done")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(agent.Top); !os.IsNotExist(err) {
		t.Fatalf("got %v", err)
	}
}

func TestDoneResultNamesTheWayToRemoveTheWorktree(t *testing.T) {
	_, agent, _ := finishedWorktree(t)
	result := doneResult(agent, 1, "pr", []string{"note"})
	if result.Status != "Done" || result.PR != "pr" || result.Worktree != agent.Top || !strings.Contains(result.Next, "quipu worktree remove "+agent.Top) || len(result.Warnings) != 1 {
		t.Fatalf("got %+v", result)
	}
}

func TestStartPointKeepsWorkThatIsOnlyOnOrigin(t *testing.T) {
	gitIdentity(t)
	top, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	writeRepoFile(t, agent.Top, "work.txt", []byte("work\n"))
	commitAll(t, agent.Top)
	gitIn(t, agent.Top, "push", "-q", "origin", "001/a")
	want, _ := agent.git("rev-parse", "HEAD")
	root, err := openWorktree(top)
	if err != nil {
		t.Fatal(err)
	}
	// The worktree and branch are gone from this clone, as after a removal.
	gitIn(t, top, "worktree", "remove", "--force", agent.Top)
	gitIn(t, top, "branch", "-D", "001/a")

	path := filepath.Join(filepath.Dir(top), "taken-over")
	created, _, createdBranch, err := root.addWorktree(path, "001/a", "main")
	if err != nil {
		t.Fatal(err)
	}
	if head, _ := created.git("rev-parse", "HEAD"); head != want || !createdBranch {
		t.Fatalf("a new worktree must keep the PR's commits: %s, want %s", head, want)
	}
	gitIn(t, top, "worktree", "remove", "--force", path)
	gitIn(t, top, "branch", "-D", "001/a")

	if err := root.switchBranch("001/a", "main"); err != nil {
		t.Fatal(err)
	}
	if head, _ := root.git("rev-parse", "HEAD"); head != want {
		t.Fatalf("switching must keep them too: %s, want %s", head, want)
	}
	gitIn(t, top, "switch", "-q", "main")
	mainHead, _ := root.git("rev-parse", "origin/main")
	if point := root.startPoint("002/new", "main"); point != "origin/main" {
		t.Fatalf("new work starts from the default branch: %s (%s)", point, mainHead)
	}
}

func TestWriterAcceptsARepeatBeforeCallingItStale(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	state := liveWork{IssueID: "I", IssueState: "OPEN", Status: "Done", Holder: finishedHolder, Branch: "001/a"}
	request := writer.Request{ID: "2", Action: "status", Status: "Done", IssueID: "I", Actor: finishedHolder, ExpectedFingerprint: "from-before-the-first-one-landed"}

	receipt, applied := alreadyAppliedReceipt(state, request, nil, now)
	if !applied || receipt.Result != "accepted" || receipt.RequestID != "2" || !strings.Contains(receipt.Detail, "Done") {
		t.Fatalf("got %+v, %v", receipt, applied)
	}
	if receipt.State != nil {
		t.Fatal("it changes nothing, so it is no record of state")
	}
	if writer.Closed([]writer.Receipt{{Result: "accepted", Detail: "lifecycle state changed to Ready", State: &writer.ReceiptState{Status: "Ready"}}, receipt}) {
		t.Fatal("a repeat must not read as closing the work")
	}

	other := request
	other.Actor = "claude/octo/other"
	if _, applied := alreadyAppliedReceipt(state, other, nil, now); applied {
		t.Fatal("never for another holder")
	}

	ready := liveWork{IssueID: "I", IssueState: "OPEN", Status: "Ready"}
	again := writer.Request{ID: "3", Action: "status", Status: "Ready", IssueID: "I", Actor: "octo", ExpectedFingerprint: "old"}
	if _, applied := alreadyAppliedReceipt(ready, again, nil, now); !applied {
		t.Fatal("Ready to Ready is no error")
	}
}

func TestRequestIssues(t *testing.T) {
	render := func(id string) string {
		body, err := writer.RenderRequest(writer.Request{ID: id, Action: "claim", IssueID: "I", Actor: "a", ExpectedFingerprint: "f"})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	receipt, err := writer.RenderReceipt(writer.Receipt{RequestID: "2", Result: "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	comment := func(number, kind, body string) issueComment {
		return issueComment{Body: body, IssueURL: "https://api.github.com/repos/o/r/issues/" + number, HTMLURL: "https://github.com/o/r/" + kind + "/" + number + "#issuecomment-1"}
	}
	got := requestIssues([]issueComment{
		comment("1", "issues", render("1")),   // waiting
		comment("2", "issues", render("2")),   // answered below
		comment("2", "issues", receipt),       //
		comment("3", "issues", "hello"),       // not a request
		comment("4", "pull", render("4")),     // on a pull request
		comment("5", "issues", "hello"),       // a request after other talk
		comment("5", "issues", render("5")),   //
		{Body: render("6"), IssueURL: "junk"}, // unreadable
	})
	if len(got) != 2 || got[0] != 1 || got[1] != 5 {
		t.Fatalf("got %v", got)
	}

	raw, err := json.Marshal([][]issueComment{{comment("7", "issues", render("7"))}, {comment("8", "issues", render("8"))}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := parseRequestIssues([]byte(strings.ReplaceAll(string(raw), `"Body"`, `"body"`))); err != nil || len(got) != 2 {
		t.Fatalf("every page counts: %v, %v", got, err)
	}
}

func TestAppendNew(t *testing.T) {
	got := appendNew([]int{1, 2}, 2, 3, 0, 3)
	if len(got) != 3 || got[2] != 3 {
		t.Fatalf("got %v", got)
	}
}

func TestEventIssue(t *testing.T) {
	write := func(body string) string {
		path := filepath.Join(t.TempDir(), "event.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for name, tc := range map[string]struct {
		path string
		want int
	}{
		"not in Actions":     {"", 0},
		"missing file":       {filepath.Join(t.TempDir(), "gone"), 0},
		"junk":               {write("{"), 0},
		"comment on Issue":   {write(`{"action":"created","issue":{"number":12}}`), 12},
		"comment on PR":      {write(`{"issue":{"number":12,"pull_request":{"url":"x"}}}`), 0},
		"event with neither": {write(`{"schedule":"*/5 * * * *"}`), 0},
	} {
		if got := eventIssue(tc.path); got != tc.want {
			t.Errorf("%s: %d", name, got)
		}
	}
}

func TestRescanLooksAgainAfterGitHubCatchesUp(t *testing.T) {
	began := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var slept time.Duration
	var since time.Time
	list := func(from time.Time) ([]int, error) { since = from; return []int{7}, nil }

	got, err := rescanRequests(5*time.Second, func(d time.Duration) { slept = d }, list, began)
	if err != nil || len(got) != 1 || got[0] != 7 {
		t.Fatalf("got %v, %v", got, err)
	}
	if slept != rescanAfter-5*time.Second {
		t.Fatalf("slept %s", slept)
	}
	if !since.Equal(began.Add(-rescanReach)) {
		t.Fatalf("since %s", since)
	}

	slept = 0
	if _, err := rescanRequests(time.Minute, func(d time.Duration) { slept = d }, list, began); err != nil || slept != 0 {
		t.Fatalf("a long pass has already waited: slept %s, %v", slept, err)
	}

	wantErr := errors.New("boom")
	if _, err := rescanRequests(time.Minute, func(time.Duration) {}, func(time.Time) ([]int, error) { return nil, wantErr }, began); !errors.Is(err, wantErr) {
		t.Fatalf("got %v", err)
	}
}

func TestFindRemovableTakesAnyPathForm(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "repo")
	wt := filepath.Join(dir, "repo-worktrees", "repo-042")
	x := filepath.Join(main, "x")
	for _, path := range []string{main, wt, x} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "repo-worktrees"), link); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(wt, filepath.Join(home, "wt")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(before) })
	if err := os.Chdir(main); err != nil {
		t.Fatal(err)
	}
	entries := []worktreeEntry{{main, "main"}, {wt, "042/a"}, {x, "043/x"}}
	none := func(string) int { return 0 }
	for target, want := range map[string]string{
		wt:                              wt,
		"../repo-worktrees/repo-042":    wt,
		"../repo-worktrees/repo-042/":   wt,
		wt + "/":                        wt,
		filepath.Join(link, "repo-042"): wt,
		"~/wt":                          wt,
		"./x/":                          x,
		"x":                             x,
		x:                               x,
	} {
		entry, err := findRemovable(entries, target, none)
		if err != nil || entry.Path != want {
			t.Errorf("%s: %+v, %v", target, entry, err)
		}
	}
	if _, err := findRemovable(entries, ".", none); flowCode(err) != "worktree-missing" {
		t.Errorf("the main checkout, named relatively, is never removed: %v", err)
	}
}

func TestRemoveTakesALockedWorktree(t *testing.T) {
	root, agent, entry := finishedWorktree(t)
	gitIn(t, root.Top, "worktree", "lock", "--reason", "claude agent session", agent.Top)
	if _, err := removeWorktreeTree(root, entry, "", liveStatus("Done")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(agent.Top); !os.IsNotExist(err) {
		t.Fatalf("a locked worktree whose work is Done goes too: %v", err)
	}
}
