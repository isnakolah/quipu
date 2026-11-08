package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
)

const denyListConfig = "guard: deny-list\nguard_deny:\n  - fly deploy\n  - npm publish\n"

// guardError runs one tool call through the guard and returns the denial.
func guardError(t *testing.T, cwd, tool string, input map[string]any) error {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	g := newGuard(cwd, time.Now().UTC())
	g.live = func(model.Config, int) (liveWork, error) { return liveWork{}, errors.New("no network in tests") }
	return g.check(hookInput{ToolName: tool, Cwd: cwd, ToolInput: raw})
}

// harmlessCommands are the refusals the Calla write-up (2026-10-30..11-06)
// and this autonomy work counted as harmless: reads, disk checks, help, poll
// loops, scratch writes outside any governed worktree.
func harmlessCommands(scratch string) []string {
	return []string{
		"df -k /",
		"df -k / | awk 'NR==2 {print $4}'",
		"pgrep -fl cargo",
		"ps aux",
		"docker ps",
		"docker system df",
		"docker volume ls -q",
		"tmutil listlocalsnapshots /",
		"diskutil apfs listSnapshots /",
		"find . -name '*.rs' -exec ls -l {} +",
		"security find-identity -v -p codesigning",
		"git branch -r --contains abc1234",
		"gh pr view 12 --json state",
		"gh pr checks 12",
		"gh api repos/o/r/contents/README.md | jq -r .content | base64 -d | head",
		"jq -r .data.bytes_b64 f.json | base64 -d",
		"while ! gh pr checks 12; do sleep 30; done",
		"until [ \"$(df -k / | awk 'NR==2{print $4}')\" -gt 1000 ]; do sleep 60; done",
		"quipu worktree --help",
		"quipu issue create --help",
		"quipu adopt --help",
		"cargo build -h",
		"tail -n 50 " + filepath.Join(scratch, "task.output"),
		"python3 -I -c 'import json,sys; print(json.load(sys.stdin)[\"merge\"])' < cfg.json",
		"quipu config --json | python3 -I -c 'import sys; print(sys.stdin.read()[:600])'",
		"git fetch -q origin",
		"awk '{print $1}' notes.txt",
		"echo seen >> " + filepath.Join(scratch, "denials.log"),
		"S=" + scratch + "; echo seen >> \"$S/denials.log\"",
		"L=" + filepath.Join(scratch, "check.log") + "; make check > \"$L\" 2>&1",
		"git rev-parse HEAD; gh api repos/o/r/contents/a | base64 -d | head",
		"git worktree unlock ../other",
	}
}

func TestDenyListLetsHarmlessCommandsThrough(t *testing.T) {
	top, agent := flaggedRepo(t, denyListConfig)
	scratch := t.TempDir()
	now := time.Now().UTC()
	cases := map[string]func(){
		"without a lease": func() {},
		"with a lease": func() {
			writeRecord(t, agent.Top, leaseContext{Issue: 1, Holder: "claude/octo/aaaa1111", Branch: "001/a",
				Expires: now.Add(time.Hour).Format(time.RFC3339), VerifiedAt: now.Format(time.RFC3339)})
		},
		"with a stale lease": func() {
			writeRecord(t, agent.Top, leaseContext{Issue: 238, Holder: "agent/octo/1aef1808", Branch: "238/old",
				Expires: now.Add(-48 * time.Hour).Format(time.RFC3339), VerifiedAt: now.Add(-50 * time.Hour).Format(time.RFC3339)})
		},
	}
	for name, setup := range cases {
		setup()
		for _, dir := range []string{top, agent.Top} {
			for _, command := range harmlessCommands(scratch) {
				if err := guardError(t, dir, "Bash", map[string]any{"command": command}); err != nil {
					t.Errorf("%s, in %s: %q: %v", name, filepath.Base(dir), command, err)
				}
			}
		}
	}
}

func TestDenyListStillRefusesTheDangerous(t *testing.T) {
	_, agent := flaggedRepo(t, denyListConfig)
	now := time.Now().UTC()
	writeRecord(t, agent.Top, leaseContext{Issue: 1, Holder: "claude/octo/aaaa1111", Branch: "001/a",
		Expires: now.Add(time.Hour).Format(time.RFC3339), VerifiedAt: now.Format(time.RFC3339)})
	for _, test := range []struct{ command, code, denied string }{
		{"git push --force origin 001/a", "force-push", "git push --force origin 001/a"},
		{"git status && git push origin main", "push-target", "git push origin main"},
		{"gh pr merge 12 --merge", "merge-approve", "gh pr merge 12 --merge"},
		{"gh pr review 12 --approve", "merge-approve", "gh pr review 12 --approve"},
		{"gh issue close 5", "github-write", "gh issue close 5"},
		{"ls; echo x >> " + rootConfigPath, "quipu-config", ""},
		{"python3 edit.py .github/quipu/config.yaml", "quipu-config", "python3 edit.py .github/quipu/config.yaml"},
		{"fly deploy --remote-only", "guard-deny", "fly deploy --remote-only"},
		{"ls && env CI=1 npm publish --access public", "guard-deny", "npm publish --access public"},
		{"quipu protect --apply", "quipu-admin", "quipu protect --apply"},
	} {
		err := guardError(t, agent.Top, "Bash", map[string]any{"command": test.command})
		var d denial
		if !errors.As(err, &d) || d.Code != test.code {
			t.Errorf("%q: want %s, got %v", test.command, test.code, err)
			continue
		}
		if test.denied != "" && !strings.Contains(err.Error(), "denied: `"+test.denied+"`") {
			t.Errorf("%q must name the subcommand %q: %v", test.command, test.denied, err)
		}
	}
	if code := guardCode(t, agent.Top, "Edit", map[string]any{"file_path": filepath.Join(agent.Top, rootConfigPath)}); code != "quipu-config" {
		t.Errorf("an edit to the config: %q", code)
	}
}

func TestDenyListKeepsChangesBehindTheLease(t *testing.T) {
	top, agent := flaggedRepo(t, denyListConfig)
	for _, dir := range []string{top, agent.Top} {
		for _, command := range []string{
			"git commit -qm more",
			"echo x > notes.md",
			"rm -rf src",
			"mv a b",
			"sed -i '' s/a/b/ notes.md",
			"X=notes.md; echo y >> \"$X\"",
		} {
			err := guardError(t, dir, "Bash", map[string]any{"command": "ls; " + command})
			var d denial
			if !errors.As(err, &d) || d.Code != "no-lease" {
				t.Errorf("%s: %q without a lease: %v", filepath.Base(dir), command, err)
			} else if !strings.Contains(err.Error(), "denied: `") || strings.Contains(err.Error(), "denied: `ls`") {
				t.Errorf("%q must name its own subcommand: %v", command, err)
			}
		}
		if code := guardCode(t, dir, "Edit", map[string]any{"file_path": filepath.Join(dir, "x.go")}); code != "no-lease" {
			t.Errorf("an edit without a lease: %q", code)
		}
	}
	// A computed path that may point into the worktree is still judged there.
	if code := guardCode(t, agent.Top, "Bash", map[string]any{"command": "echo x > \"$UNSET/notes.md\""}); code == "" {
		t.Error("a redirect to an unknown path inside a governed checkout must be judged")
	}
}

func TestVariablesAssignedInTheLineAreFollowed(t *testing.T) {
	_, agent := flaggedRepo(t, "")
	scratch := t.TempDir()
	if code := guardCode(t, agent.Top, "Bash", map[string]any{"command": "N=" + scratch + "; cat \"$N/a\" > \"$N/b\""}); code != "" {
		t.Errorf("a literal variable resolves outside the worktree: %s", code)
	}
	if code := guardCode(t, agent.Top, "Bash", map[string]any{"command": "(N=" + scratch + "); echo x > \"$N/b\""}); code == "" {
		t.Error("a subshell's assignment does not reach the rest of the line")
	}
	if code := guardCode(t, agent.Top, "Bash", map[string]any{"command": "N=" + scratch + "; N=$(pwd); echo x > \"$N/b\""}); code == "" {
		t.Error("a computed reassignment forgets the variable")
	}
}

func TestHelpFormSkipsFileWriters(t *testing.T) {
	words := func(command string) []shellArg {
		var args []shellArg
		for _, word := range strings.Fields(command) {
			args = append(args, shellArg{s: word, ok: true})
		}
		return args
	}
	for command, want := range map[string]bool{
		"quipu worktree --help": true, "git commit -h": true, "gh help pr": true, "df -h": true,
		"rm build --help": false, "ln -sfh a b": false, "cp -h": false, "du -h /tmp": false,
	} {
		if got := helpForm(words(command)); got != want {
			t.Errorf("%q: %v", command, got)
		}
	}
}
