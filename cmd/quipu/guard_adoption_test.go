package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// adoptionFixture is a clone just after `quipu adopt --apply`: quipu.yml is
// written but not committed, origin's main has no config yet, and other is a
// second governed worktree of the same clone.
type adoptionFixture struct{ top, other string }

func newAdoptionFixture(t *testing.T, opened time.Time) adoptionFixture {
	t.Helper()
	top := gitRepo(t)
	bare := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, top, "init", "-q", "--bare", "-b", "main", bare)
	gitIn(t, top, "remote", "add", "origin", bare)
	gitIn(t, top, "push", "-q", "origin", "main")
	gitIn(t, top, "remote", "set-head", "origin", "main")
	other := filepath.Join(filepath.Dir(top), "other")
	gitIn(t, top, "worktree", "add", "-q", "-b", "other", other)
	for _, dir := range []string{top, other} {
		writeRepoFile(t, dir, rootConfigPath, []byte(guardFixtureConfig))
	}
	w, err := openWorktree(top)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.recordBootstrap(opened, defaultBootstrapMinutes); err != nil {
		t.Fatal(err)
	}
	return adoptionFixture{top: top, other: other}
}

// land commits the config to main and pushes it, as the owner's merge of the
// adoption PR would, then fetches it into the clone.
func (f adoptionFixture) land(t *testing.T) {
	t.Helper()
	commitAll(t, f.top)
	gitIn(t, f.top, "push", "-q", "origin", "main")
	gitIn(t, f.top, "fetch", "-q", "origin")
}

func edit(path string) map[string]any { return map[string]any{"file_path": path} }

func TestBootstrapWindowLetsTheAdoptingWorktreeFinishSetup(t *testing.T) {
	f := newAdoptionFixture(t, time.Now())
	pr := "gh pr create --base main --head quipu-adoption --title 'chore: adopt Quipu' --body 'Sets up Quipu.'"
	cases := []struct {
		cwd, tool string
		input     map[string]any
		want      string
	}{
		// The setup commands, their help and dry runs, and import.
		{f.top, "Bash", bash("quipu adopt --owner octo --repo repo"), ""},
		{f.top, "Bash", bash("quipu adopt --help && quipu import --help && quipu protect --help && quipu hooks --help"), ""},
		{f.top, "Bash", bash("quipu import --manifest backlog.yaml"), ""},
		{f.top, "Bash", bash("quipu import --manifest backlog.yaml --apply --json"), ""},
		{f.top, "Bash", bash("quipu runner setup && quipu hooks && quipu protect --json"), ""},
		{f.top, "Bash", bash("quipu doctor --json"), ""},
		// Reads of every kind.
		{f.top, "Bash", bash("strings quipu.yml | paste -s - && git tag --sort=-creatordate"), ""},
		{f.top, "Bash", bash("make --help"), ""},
		{f.top, "Read", map[string]any{"file_path": filepath.Join(f.top, "quipu.yml")}, ""},
		// Quipu's own files, until the commit lands, and the commit itself.
		{f.top, "Edit", edit(filepath.Join(f.top, "quipu.yml")), ""},
		{f.top, "Write", edit(filepath.Join(f.top, ".github/workflows/ci.yml")), ""},
		{f.top, "Write", edit(filepath.Join(f.top, ".github/ISSUE_TEMPLATE/work.yml")), ""},
		{f.top, "Edit", edit(filepath.Join(f.top, "CLAUDE.md")), ""},
		{f.top, "Bash", bash("quipu guide --sync && quipu hooks --apply"), ""},
		{f.top, "Bash", bash("git add -A && git commit -m 'chore: adopt Quipu'"), ""},
		// Pushed to a branch of its own, with a PR the owner merges.
		{f.top, "Bash", bash("git push -u origin HEAD:quipu-adoption"), ""},
		{f.top, "Bash", bash("git push origin main:refs/heads/quipu-adoption"), ""},
		{f.top, "Bash", bash(pr), ""},
		// What stays strict: the default branch, force, other files, admin.
		{f.top, "Bash", bash("git push origin main"), "push-target"},
		{f.top, "Bash", bash("git push origin HEAD"), "push-target"},
		{f.top, "Bash", bash("git push origin HEAD:main"), "push-target"},
		{f.top, "Bash", bash("git push origin quipu-adoption:refs/heads/main"), "push-target"},
		{f.top, "Bash", bash("git push origin"), "push-target"},
		{f.top, "Bash", bash("git push"), "push-target"},
		{f.top, "Bash", bash("git push upstream HEAD:quipu-adoption"), "push-target"},
		{f.top, "Bash", bash("git push origin HEAD:refs/tags/v1"), "push-target"},
		{f.top, "Bash", bash("git push --tags origin HEAD:quipu-adoption"), "push-target"},
		{f.top, "Bash", bash("git push --force origin HEAD:quipu-adoption"), "force-push"},
		{f.top, "Bash", bash("git push --force-with-lease origin HEAD:quipu-adoption"), "force-push"},
		{f.top, "Bash", bash("git push origin +HEAD:quipu-adoption"), "force-push"},
		{f.top, "Bash", bash("gh pr create --base main --head main --title t --body b"), "pr-body"},
		{f.top, "Bash", bash("gh pr create --base other --head quipu-adoption --title t --body b"), "pr-body"},
		{f.top, "Bash", bash("gh pr create --base main --head quipu-adoption --title t --body 'Closes #1'"), "pr-body"},
		{f.top, "Bash", bash("gh pr create --base main --head quipu-adoption --title t --fill"), "pr-body"},
		{f.top, "Bash", bash("gh pr create --base main --head quipu-adoption --title t --body b --label quipu:owner-approved"), "github-write"},
		{f.top, "Bash", bash("gh pr merge 1 --merge"), "merge-approve"},
		{f.top, "Bash", bash("gh pr edit 1 --body b"), "no-lease"},
		{f.top, "Bash", bash("quipu adopt --owner octo --repo repo --project-number 1 --apply"), "quipu-admin"},
		{f.top, "Bash", bash("quipu protect --apply"), "quipu-admin"},
		{f.top, "Bash", bash("quipu writer --apply"), "quipu-admin"},
		{f.top, "Edit", edit(filepath.Join(f.top, "main.go")), "no-lease"},
		{f.top, "Bash", bash("echo x > main.go"), "no-lease"},
		{f.top, "Bash", bash("echo {} > .git/quipu-bootstrap"), "git-internals"},
		{f.top, "Bash", bash("git checkout -b quipu-adoption"), "branch-change"},
		// Only the adopting worktree: the clone's other worktree, or a change
		// reaching into it, gets the normal rules.
		{f.other, "Bash", bash("quipu import --manifest backlog.yaml --apply"), "quipu-admin"},
		{f.other, "Bash", bash("git add -A && git commit -m x"), "no-lease"},
		{f.other, "Bash", bash("git push -u origin HEAD:quipu-adoption"), "no-lease"},
		{f.other, "Edit", edit(filepath.Join(f.other, "quipu.yml")), "quipu-config"},
		{f.top, "Edit", edit(filepath.Join(f.other, ".github/workflows/ci.yml")), "quipu-config"},
		{f.top, "Bash", bash("git -C ../other commit -m x"), "no-lease"},
	}
	for _, c := range cases {
		if got := guardCode(t, c.cwd, c.tool, c.input); got != c.want {
			t.Errorf("%s in %s: %v: got %q, want %q", c.tool, filepath.Base(c.cwd), c.input, got, c.want)
		}
	}
}

func TestBootstrapWindowStopsWritingOnceTheConfigLands(t *testing.T) {
	f := newAdoptionFixture(t, time.Now())
	f.land(t)
	cases := []struct {
		tool  string
		input map[string]any
		want  string
	}{
		// The window still runs import and the dry runs.
		{"Bash", bash("quipu import --manifest backlog.yaml --apply"), ""},
		{"Bash", bash("quipu adopt --owner octo --repo repo"), ""},
		// Writing, committing and pushing follow the normal rules again.
		{"Edit", edit(filepath.Join(f.top, "quipu.yml")), "quipu-config"},
		{"Bash", bash("quipu guide --sync"), "quipu-admin"},
		{"Bash", bash("quipu hooks --apply"), "quipu-admin"},
		{"Bash", bash("git commit --allow-empty -m x"), "no-lease"},
		{"Bash", bash("git push -u origin HEAD:quipu-adoption"), "no-lease"},
		{"Bash", bash("gh pr create --base main --head quipu-adoption --title t --body b"), "no-lease"},
	}
	for _, c := range cases {
		if got := guardCode(t, f.top, c.tool, c.input); got != c.want {
			t.Errorf("%s %v: got %q, want %q", c.tool, c.input, got, c.want)
		}
	}
}

func TestNormalRulesApplyAfterTheBootstrapWindow(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T) adoptionFixture{
		"expired": func(t *testing.T) adoptionFixture {
			return newAdoptionFixture(t, time.Now().Add(-3*time.Hour))
		},
		"never opened": func(t *testing.T) adoptionFixture {
			f := newAdoptionFixture(t, time.Now())
			w, _ := openWorktree(f.top)
			if err := os.Remove(filepath.Join(w.commonDir(), bootstrapFileName)); err != nil {
				t.Fatal(err)
			}
			return f
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			cases := []struct {
				tool  string
				input map[string]any
				want  string
			}{
				{"Bash", bash("quipu import --manifest backlog.yaml --apply"), "quipu-admin"},
				{"Bash", bash("quipu import --manifest backlog.yaml"), "quipu-admin"},
				{"Bash", bash("quipu adopt --owner octo --repo repo"), "quipu-admin"},
				{"Bash", bash("strings notes.txt"), "no-lease"},
				{"Edit", edit(filepath.Join(f.top, "quipu.yml")), "quipu-config"},
				{"Bash", bash("git add -A"), "no-lease"},
				{"Bash", bash("git push -u origin HEAD:quipu-adoption"), "no-lease"},
				{"Bash", bash("quipu adopt --help && quipu status --json"), ""},
			}
			for _, c := range cases {
				if got := guardCode(t, f.top, c.tool, c.input); got != c.want {
					t.Errorf("%s %v: got %q, want %q", c.tool, c.input, got, c.want)
				}
			}
		})
	}
}

func TestBootstrapWindowRecord(t *testing.T) {
	top := gitRepo(t)
	linked := filepath.Join(filepath.Dir(top), "linked")
	gitIn(t, top, "worktree", "add", "-q", "-b", "x", linked)
	main, _ := openWorktree(top)
	other, _ := openWorktree(linked)
	if main.commonDir() != other.commonDir() {
		t.Fatalf("worktrees of one clone share a common dir: %q %q", main.commonDir(), other.commonDir())
	}
	now := time.Now()
	window, err := other.recordBootstrap(now, 30)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(main.GitDir, bootstrapFileName)); err != nil {
		t.Fatalf("the record lives in the common git dir: %v", err)
	}
	if got, open := other.bootstrapFor(now.Add(29 * time.Minute)); !open || got != window {
		t.Fatalf("open for the adopting worktree: %+v %v", got, open)
	}
	if _, open := other.bootstrapFor(now.Add(31 * time.Minute)); open {
		t.Fatal("closed after its minutes")
	}
	if _, open := main.bootstrapFor(now); open {
		t.Fatal("a window is for the worktree that adopted, not the clone")
	}
	for _, minutes := range []int{-1, maxBootstrapMinutes + 1} {
		if validBootstrapMinutes(minutes) == nil {
			t.Errorf("%d minutes must be refused", minutes)
		}
	}
	if validBootstrapMinutes(0) != nil || validBootstrapMinutes(defaultBootstrapMinutes) != nil {
		t.Error("0 (none) and the default are valid")
	}
}
