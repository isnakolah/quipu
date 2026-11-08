package main

import (
	"encoding/json"
	"errors"
	"github.com/isnakolah/quipu/internal/model"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// guardFixture is a Quipu-managed repository with two linked worktrees:
// a holds a fresh lease for #1 on 001/a, b holds none, and top is the main
// checkout without a lease.
type guardFixture struct{ top, a, b string }

// guardFixtureConfig is the config every guard fixture starts from. It is
// fixed here, not read from this repository's own config, so the owner can
// change what Quipu's own agents may do without changing what the tests mean.
// Every agent permission is at its owner-only default.
const guardFixtureConfig = `schema_version: 1
owner: octo
repository: repo
project:
  id: PVT_test
  number: 1
  title: Test
  fields:
    status: F_status
    kind: F_kind
    area: F_area
    priority: F_priority
    proof: F_proof
    lease_holder: F_holder
    lease_expires: F_expires
    branch: F_branch
    state_fingerprint: F_fingerprint
areas: [stable]
wiki_mode: journal
default_branch: main
writer_version: 0.0.0-dev
authorized_actors: [octo]
policy:
  lease_ttl_minutes: 120
  reconcile_minutes: 5
merge: agent
`

func newGuardFixture(t *testing.T) guardFixture {
	t.Helper()
	config := []byte(guardFixtureConfig)
	top := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(top, filepath.Dir(configPath)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, configPath), config, 0644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, top, "add", ".")
	gitIn(t, top, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "config")
	f := guardFixture{top: top, a: filepath.Join(filepath.Dir(top), "wt-a"), b: filepath.Join(filepath.Dir(top), "wt-b")}
	gitIn(t, top, "worktree", "add", "-q", "-b", "001/a", f.a)
	gitIn(t, top, "worktree", "add", "-q", "-b", "002/b", f.b)
	a, err := openWorktree(f.a)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := a.writeLease(leaseContext{Issue: 1, Holder: "claude/octo/aaaa1111", Branch: "001/a",
		Expires: now.Add(time.Hour).Format(time.RFC3339), VerifiedAt: now.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	return f
}

// fixtureCaller is the GitHub login every guard test calls as: the login of
// the lease in the fixture's worktree a.
func fixtureCaller(time.Time) (string, error) { return "octo", nil }

// guardCode runs one tool call through the guard and returns the denial
// code, or "" when allowed.
func guardCode(t *testing.T, cwd, tool string, input map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	g := newGuard(cwd, time.Now().UTC())
	// Tests never reach GitHub. No answer means what it means in production.
	g.caller = fixtureCaller
	g.live = func(model.Config, int) (liveWork, error) { return liveWork{}, errors.New("no network in tests") }
	err = g.check(hookInput{ToolName: tool, Cwd: cwd, ToolInput: raw})
	if err == nil {
		return ""
	}
	var d denial
	if errors.As(err, &d) {
		if !strings.Contains(err.Error(), "Do instead:") {
			t.Fatalf("denial without remedy: %v", err)
		}
		return d.Code
	}
	return "error: " + err.Error()
}

func TestGuardShellCommands(t *testing.T) {
	f := newGuardFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(f.a, "body.md"), []byte("Refs #1\n\nSummary."), 0644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		cwd, command, want string
	}{
		// Leased worktree: everyday work is allowed.
		{f.a, "go test ./...", ""},
		{f.a, "git status && git diff | head", ""},
		{f.a, "echo hi > /dev/null 2>&1", ""},
		{f.a, "git add . && git commit -m 'feat: x'", ""},
		{f.a, "sed -i '' s/a/b/ file.go", ""},
		{f.a, "python3 -c 'print(1)'", ""},
		{f.a, "echo x>out.txt", ""},
		{f.a, "rm -f old.go && mkdir -p pkg/new", ""},
		{f.a, "git checkout -- file.go", ""},
		{f.a, "git push -u origin 001/a", ""},
		{f.a, "git push", ""},
		{f.a, "git push origin HEAD", ""},
		{f.a, "git push --force-with-lease origin 001/a", ""},
		{f.a, "bash -c 'go vet ./...'", ""},
		{f.a, "gh pr create --base main --head 001/a --title 'feat: x' --body 'Refs #1\n\nSummary.'", ""},
		{f.a, "gh pr create --base main --head 001/a --title t --body-file body.md", ""},
		{f.a, "gh pr view --json url && gh pr checks", ""},
		{f.a, "gh api repos/o/r/issues/1 --jq .title", ""},
		{f.a, "gh api graphql -f query='{ viewer { login } }'", ""},
		{f.a, "quipu status --json && quipu start 1 --json", ""},
		{f.a, "quipu done 1 --json --criteria x", ""},
		{f.a, "quipu import --manifest backlog.yaml --apply", "quipu-admin"},
		{f.a, "cd " + outside + " && rm -rf scratch", ""},
		{f.a, "echo x > " + filepath.Join(outside, "notes.txt"), ""},
		// Never allowed, even with a lease.
		{f.a, "git push origin main", "push-target"},
		{f.a, "git  push  origin  HEAD:main", "push-target"},
		{f.a, `\git push origin main`, "push-target"},
		{f.a, "git push upstream 001/a", "push-target"},
		{f.a, "git push origin :001/a", "push-target"},
		{f.a, "git push --tags", "push-target"},
		{f.a, "git push --force", "force-push"},
		{f.a, "git push -fu origin 001/a", "force-push"},
		{f.a, "git push origin +001/a", "force-push"},
		{f.a, "git push --no-verify", "no-verify"},
		{f.a, "git commit -nm x", "no-verify"},
		{f.a, "git switch main", "branch-change"},
		{f.a, "git checkout main", "branch-change"},
		{f.a, "git branch -D 002/b", "branch-change"},
		{f.a, "gh pr checkout 3", "branch-change"},
		{f.a, "git tag v1", "git-refs"},
		{f.a, "git config user.name x", "git-refs"},
		{f.a, "git fetch origin main:main", "git-refs"},
		{f.a, "git worktree add ../wt-c", "worktree-add"},
		{f.a, "git --git-dir=../x status", "git-internals"},
		{f.a, "gh pr merge 1 --squash", "merge-approve"},
		{f.a, "gh pr review 1 --approve", "merge-approve"},
		{f.a, "gh pr close 1", "merge-approve"},
		{f.a, "gh issue close 1", "github-write"},
		{f.a, "gh issue comment 1 -b hi", "github-write"},
		{f.a, "gh issue create -t x -b y", "github-write"},
		{f.a, "gh project item-edit --id x", "github-write"},
		{f.a, "gh label create x", "github-write"},
		{f.a, "gh api -X PATCH repos/o/r/issues/1", "github-write"},
		{f.a, "gh api -XDELETE repos/o/r/issues/1/labels", "github-write"},
		{f.a, "gh api repos/o/r/issues -f title=x", "github-write"},
		{f.a, "gh api graphql -f query='mutation { closeIssue }'", "github-write"},
		{f.a, "curl -X POST -d '{}' https://api.github.com/repos/o/r/issues", "github-write"},
		{f.a, "quipu writer run --issue-number 1 --apply", "quipu-admin"},
		{f.a, "quipu writer serve --apply --json", "quipu-admin"},
		{f.a, "quipu daemon", "quipu-admin"},
		{f.a, "quipu park 1 --on 2 --json", ""},
		{f.a, "quipu work claim --issue-number 1", "quipu-admin"},
		{f.a, "quipu issue create --kind epic", "quipu-admin"},
		{f.a, "quipu evidence submit", "quipu-admin"},
		{f.a, "quipu ready 3", "quipu-admin"},
		{f.a, "quipu protect --apply", "quipu-admin"},
		{f.a, "quipu propose --parent 14 --title x --body y", ""},
		{f.a, "quipu protect", ""},
		{f.a, "quipu doctor --json", ""},
		{f.a, "echo '{}' > .git/quipu-lease", "git-internals"},
		{f.a, `python3 -c "open('.git/quipu-lease','w').write('x')"`, "git-internals"},
		{f.a, "rm -rf .git", "git-internals"},
		{f.a, "echo x > .github/quipu/config.yaml", "quipu-config"},
		{f.a, "sed -i '' s/a/b/ .github/quipu/config.yaml", "quipu-config"},
		{f.a, "gh pr create --body 'Closes #1'", "pr-body"},
		{f.a, "gh pr create --fill", "pr-body"},
		{f.a, "gh pr edit --add-label quipu:owner-approved", "github-write"},
		{f.a, "rm .claude/settings.json", "quipu-config"},
		{f.a, "sed -i '' s/guard// .codex/hooks.json", "quipu-config"},
		{f.a, "echo x > .github/workflows/quipu-writer.yml", "quipu-config"},
		{f.a, "quipu hooks", ""},
		{f.a, "quipu guide", ""},
		{f.top, "quipu guide --sync", "quipu-admin"},
		{f.a, "quipu hooks --apply", "quipu-admin"},
		{f.a, "gh pr create --base main --head 001/a --title t --body 'Refs #1' --label quipu:owner-approved", "github-write"},
		{f.a, "gh pr create --title t", "pr-body"},
		{f.a, "gh pr create --base main --head 001/a --body 'Refs #2'", "pr-body"},
		{f.a, "gh pr create --head 002/b --body 'Refs #1'", "pr-body"},
		{f.a, "gh pr create --base dev --body 'Refs #1'", "pr-body"},
		{f.a, "gh pr create --title 'fixes #4' --body 'Refs #1'", "pr-body"},
		{f.a, `eval "$CMD"`, "unparsable"},
		{f.a, "$GIT push origin main", "unparsable"},
		{f.a, "echo x > $OUT", "unparsable"},
		{f.a, "git -C " + f.b + " commit -m x", "other-worktree"},
		{f.a, "cd " + f.b + " && touch y", "other-worktree"},
		{f.a, "echo x > " + filepath.Join(f.b, "f"), "other-worktree"},
		{f.a, "bash -c 'git push origin main'", "push-target"},
		{f.a, "echo $(git push origin main)", "push-target"},
		{f.a, "(cd sub; gh pr merge 1)", "merge-approve"},
		{f.a, "if true; then gh issue close 1; fi", "github-write"},
		{f.a, "ls | xargs gh issue close", "github-write"},
		{f.a, "env FOO=1 gh pr merge 1", "merge-approve"},
		// Main checkout without a lease: look, don't touch.
		{f.top, "git status && git log --oneline | head -5", ""},
		{f.top, "go test ./... && go vet ./...", ""},
		{f.top, "cat README.md | grep -n Quipu", ""},
		{f.top, "find . -name '*.go' | wc -l", ""},
		{f.top, "quipu start --next --worktree --json", ""},
		{f.top, "gh pr list && gh issue view 1", ""},
		{f.top, "git worktree remove " + f.b, ""},
		{f.top, "git worktree remove " + f.a, "worktree-busy"},
		{f.top, "touch x", "no-lease"},
		{f.top, "echo x>f", "no-lease"},
		{f.top, "sed -i '' s/a/b/ README.md", "no-lease"},
		{f.top, "git commit -m x", "no-lease"},
		{f.top, "python3 tool.py", "no-lease"},
		{f.top, "npm install", "no-lease"},
		{f.top, "find . -name '*.tmp' -delete", "no-lease"},
		{f.b, "git add . && git commit -m x", "no-lease"},
		// Outside any Quipu repository nothing is guarded.
		{outside, "rm -rf x && git init y && eval \"$X\"", ""},
	}
	for _, tc := range cases {
		if got := guardCode(t, tc.cwd, "Bash", map[string]any{"command": tc.command}); got != tc.want {
			t.Errorf("%s$ %s\n  got %q, want %q", filepath.Base(tc.cwd), tc.command, got, tc.want)
		}
	}
}

func TestGuardEditAndOtherTools(t *testing.T) {
	f := newGuardFixture(t)
	outside := t.TempDir()
	patch := func(path string) map[string]any {
		return map[string]any{"command": "*** Begin Patch\n*** Update File: " + path + "\n@@\n-a\n+b\n*** End Patch\n"}
	}
	cases := []struct {
		cwd, tool string
		input     map[string]any
		want      string
	}{
		{f.a, "Edit", map[string]any{"file_path": filepath.Join(f.a, "new", "dir", "file.go")}, ""},
		{f.a, "Write", map[string]any{"file_path": filepath.Join(outside, "scratch.txt")}, ""},
		{f.a, "MultiEdit", map[string]any{"file_path": filepath.Join(f.a, "x.go")}, ""},
		{f.a, "NotebookEdit", map[string]any{"notebook_path": filepath.Join(f.a, "n.ipynb")}, ""},
		{f.a, "Edit", map[string]any{"file_path": filepath.Join(f.b, "x.go")}, "other-worktree"},
		{f.a, "Edit", map[string]any{"file_path": filepath.Join(f.top, "x.go")}, "other-worktree"},
		{f.a, "Write", map[string]any{"file_path": filepath.Join(f.top, ".git", "worktrees", "wt-a", "quipu-lease")}, "git-internals"},
		{f.a, "Write", map[string]any{"file_path": filepath.Join(f.a, ".github", "quipu", "config.yaml")}, "quipu-config"},
		{f.a, "apply_patch", patch("x.go"), ""},
		{f.a, "apply_patch", patch(filepath.Join(f.b, "x.go")), "other-worktree"},
		// A dispatcher session without a lease may drive a leased worktree.
		{f.top, "Edit", map[string]any{"file_path": filepath.Join(f.a, "x.go")}, ""},
		{f.top, "Edit", map[string]any{"file_path": filepath.Join(f.top, "x.go")}, "no-lease"},
		{f.top, "Edit", map[string]any{"file_path": filepath.Join(f.b, "x.go")}, "no-lease"},
		{f.top, "apply_patch", patch("x.go"), "no-lease"},
		{f.top, "Read", map[string]any{"file_path": filepath.Join(f.top, "x.go")}, ""},
		{f.top, "Grep", map[string]any{"pattern": "x"}, ""},
		{f.a, "mcp__github__create_issue", map[string]any{}, "github-write"},
		{f.a, "mcp__github__merge_pull_request", map[string]any{}, "github-write"},
		{f.a, "mcp__github__get_issue", map[string]any{}, ""},
		{f.a, "mcp__notion__create_page", map[string]any{}, ""},
		{outside, "Write", map[string]any{"file_path": filepath.Join(outside, "a.txt")}, ""},
	}
	for _, tc := range cases {
		if got := guardCode(t, tc.cwd, tc.tool, tc.input); got != tc.want {
			t.Errorf("%s %s %v: got %q, want %q", filepath.Base(tc.cwd), tc.tool, tc.input, got, tc.want)
		}
	}
	gitIn(t, f.a, "switch", "-q", "-c", "001/other")
	if got := guardCode(t, f.a, "Edit", map[string]any{"file_path": filepath.Join(f.a, "x.go")}); got != "wrong-branch" {
		t.Fatalf("branch mismatch: got %q", got)
	}
}

// A subagent started in the main checkout holds no lease there. The guard
// judges its edits by the worktree they land in: one leased to its own login
// is allowed, one leased to another login is someone else's work.
func TestSubagentFromMainCheckoutEditsByTheTargetsLease(t *testing.T) {
	f := newGuardFixture(t)
	b, err := openWorktree(f.b)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := b.writeLease(leaseContext{Issue: 2, Holder: "claude/hubot/bbbb2222", Branch: "002/b",
		Expires: now.Add(time.Hour).Format(time.RFC3339), VerifiedAt: now.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		tool  string
		input map[string]any
		want  string
	}{
		{"Edit", map[string]any{"file_path": filepath.Join(f.a, "x.go")}, ""},
		{"Write", map[string]any{"file_path": filepath.Join(f.a, "pkg", "new.go")}, ""},
		{"Bash", map[string]any{"command": "cd " + f.a + " && touch y && git add -A"}, ""},
		{"Edit", map[string]any{"file_path": filepath.Join(f.b, "x.go")}, "other-worktree"},
		{"Bash", map[string]any{"command": "cd " + f.b + " && touch y"}, "other-worktree"},
		{"Bash", map[string]any{"command": "echo x > " + filepath.Join(f.b, "f")}, "other-worktree"},
	}
	for _, tc := range cases {
		if got := guardCode(t, f.top, tc.tool, tc.input); got != tc.want {
			t.Errorf("from main checkout, %s %v: got %q, want %q", tc.tool, tc.input, got, tc.want)
		}
	}

	// Not knowing who calls proves nothing, so the edit is denied.
	g := newGuard(f.top, now)
	g.caller = func(time.Time) (string, error) { return "", errors.New("gh is not signed in") }
	raw, _ := json.Marshal(map[string]any{"file_path": filepath.Join(f.a, "x.go")})
	var d denial
	if err := g.check(hookInput{ToolName: "Edit", Cwd: f.top, ToolInput: raw}); !errors.As(err, &d) || d.Code != "other-worktree" {
		t.Fatalf("unknown caller: %v, want other-worktree", err)
	}

	// The worktree's own session never needs to say who it is.
	g = newGuard(f.a, now)
	g.caller = func(time.Time) (string, error) {
		t.Fatal("asked who calls from the worktree's own session")
		return "", nil
	}
	if err := g.check(hookInput{ToolName: "Edit", Cwd: f.a, ToolInput: raw}); err != nil {
		t.Fatalf("own worktree: %v", err)
	}
}

func TestGuardRulesAreDocumented(t *testing.T) {
	seen := map[string]bool{}
	for _, rule := range guardRules {
		if seen[rule.Code] || rule.Why == "" || rule.Instead == "" {
			t.Fatalf("bad rule %+v", rule)
		}
		seen[rule.Code] = true
	}
	if msg := deny("no-lease", "x").Error(); !strings.HasPrefix(msg, "QUIPU DENIED [no-lease]:") || !strings.Contains(msg, "quipu start") {
		t.Fatalf("message %q", msg)
	}
}

func TestCommandTextAcceptsArgv(t *testing.T) {
	for raw, want := range map[string]string{
		`"git status"`:                          "git status",
		`["bash","-lc","git push origin main"]`: "git push origin main",
		`["git","commit","-m","it's"]`:          `'git' 'commit' '-m' 'it'\''s'`,
	} {
		if got := commandText(json.RawMessage(raw)); got != want {
			t.Fatalf("commandText(%s) = %q, want %q", raw, got, want)
		}
	}
}

// conflicted leaves worktree wt part way through `git <op> main`, stopped on a
// conflict in conflict.txt.
func conflicted(t *testing.T, f guardFixture, wt string, op ...string) {
	t.Helper()
	gitIdentity(t)
	writeRepoFile(t, f.top, "conflict.txt", []byte("main\n"))
	commitAll(t, f.top)
	writeRepoFile(t, wt, "conflict.txt", []byte("branch\n"))
	commitAll(t, wt)
	if output, err := exec.Command("git", append([]string{"-C", wt}, append(op, "main")...)...).CombinedOutput(); err == nil {
		t.Fatalf("git %v must stop on the conflict: %s", op, output)
	}
}

func bash(command string) map[string]any { return map[string]any{"command": command} }

func TestGuardLetsTheLeasedBranchFinishOrAbortARebase(t *testing.T) {
	f := newGuardFixture(t)
	conflicted(t, f, f.a, "rebase")
	a, err := openWorktree(f.a)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.branch(); got != "001/a" {
		t.Fatalf("mid-rebase the branch is still the one being rebased, got %q", got)
	}
	for _, command := range []string{"git rebase --abort", "git rebase --continue", "git rebase --skip", "git add conflict.txt"} {
		if got := guardCode(t, f.a, "Bash", bash(command)); got != "" {
			t.Errorf("%s: %s", command, got)
		}
	}
	if got := guardCode(t, f.a, "Edit", map[string]any{"file_path": filepath.Join(f.a, "conflict.txt")}); got != "" {
		t.Errorf("resolving the conflict: %s", got)
	}
}

func TestGuardLetsTheLeasedBranchFinishOrAbortAMerge(t *testing.T) {
	f := newGuardFixture(t)
	conflicted(t, f, f.a, "merge", "--no-edit")
	if got := guardCode(t, f.a, "Edit", map[string]any{"file_path": filepath.Join(f.a, "conflict.txt")}); got != "" {
		t.Errorf("resolving the conflict: %s", got)
	}
	for _, command := range []string{"git add conflict.txt", "git merge --continue", "git merge --abort"} {
		if got := guardCode(t, f.a, "Bash", bash(command)); got != "" {
			t.Errorf("%s: %s", command, got)
		}
	}
}

func TestGuardLetsAWorktreeWithNoLiveLeaseBackOut(t *testing.T) {
	f := newGuardFixture(t)
	conflicted(t, f, f.b, "rebase")
	cases := []struct{ cwd, command, want string }{
		{f.b, "git rebase --abort", ""},
		{f.b, "git rebase --continue", ""},
		{f.b, "git merge --abort", ""},
		{f.b, "git cherry-pick --abort", ""},
		// Only finishing or backing out; anything else still needs a lease.
		{f.b, "git commit -m x", "no-lease"},
		{f.b, "git rebase main", "no-lease"},
		{f.b, "git cherry-pick --continue", "no-lease"},
		// A leased agent may back another worktree out when nothing holds it.
		{f.a, "git -C " + f.b + " rebase --abort", ""},
	}
	for _, c := range cases {
		if got := guardCode(t, c.cwd, "Bash", bash(c.command)); got != c.want {
			t.Errorf("%s in %s: got %q, want %q", c.command, c.cwd, got, c.want)
		}
	}
	b, err := openWorktree(f.b)
	if err != nil {
		t.Fatal(err)
	}
	expired := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if err := b.writeLease(leaseContext{Issue: 3, Holder: "claude/octo/cccc3333", Branch: "003/c", Expires: expired, VerifiedAt: expired}); err != nil {
		t.Fatal(err)
	}
	if got := guardCode(t, f.b, "Bash", bash("git rebase --abort")); got != "" {
		t.Errorf("an expired record holds nothing: %s", got)
	}
}

func TestGuardKeepsALiveLeaseOnAnotherBranchFromBackingOut(t *testing.T) {
	f := newGuardFixture(t)
	conflicted(t, f, f.b, "rebase")
	b, err := openWorktree(f.b)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	live := leaseContext{Issue: 3, Holder: "claude/octo/cccc3333", Branch: "003/c",
		Expires: now.Add(time.Hour).Format(time.RFC3339), VerifiedAt: now.Format(time.RFC3339)}
	if err := b.writeLease(live); err != nil {
		t.Fatal(err)
	}
	if got := guardCode(t, f.b, "Bash", bash("git rebase --abort")); got != "wrong-branch" {
		t.Errorf("a live lease for another branch: %q", got)
	}
	live.Branch = "002/b"
	if err := b.writeLease(live); err != nil {
		t.Fatal(err)
	}
	if got := guardCode(t, f.a, "Bash", bash("git -C "+f.b+" rebase --abort")); got != "other-worktree" {
		t.Errorf("another agent's live worktree: %q", got)
	}
}

func TestStartRefusesToSwitchMidRebase(t *testing.T) {
	f := newGuardFixture(t)
	conflicted(t, f, f.a, "rebase")
	a, err := openWorktree(f.a)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.switchBranch("001/a", "main"); err != nil {
		t.Fatalf("already on the branch being rebased: %v", err)
	}
	err = a.switchBranch("009/other", "main")
	var coded *flowError
	if !errors.As(err, &coded) || coded.Code != "git-busy" || !strings.Contains(coded.Next, "git rebase --abort") {
		t.Fatalf("got %v", err)
	}
}
