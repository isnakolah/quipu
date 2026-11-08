package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFlaggedWorkflowChangesPassPolicy(t *testing.T) {
	c := watchConfig()
	now := cutover
	work := liveWork{IssueState: "OPEN", Kind: "Story", ParentID: "P", Body: validContract(), Status: "In progress",
		Holder: "claude/owner/w1", Branch: "042/fix-it", Expiry: now.Add(time.Hour).Format(time.RFC3339)}
	load := func(int) (liveWork, error) { return work, nil }
	pull := pullFacts{Title: "Fix it", Body: "Refs #42", Author: "owner", Base: "main", Head: "042/fix-it", Commits: []string{"fix it\n"}}

	pull.Files = []string{"cmd/quipu/main.go", ".github/workflows/ci.yml"}
	if v := strings.Join(prViolations(c, pull, load, now), "\n"); !strings.Contains(v, ownerApprovedLabel) {
		t.Fatalf("by default a workflow change needs the owner's label: %q", v)
	}
	c.WorkflowChanges = "flagged"
	if v := prViolations(c, pull, load, now); len(v) != 0 {
		t.Fatalf("flagged: a change to ci.yml passes policy: %v", v)
	}
	if got := flaggedWorkflowFiles(c, pull.Files); len(got) != 1 || got[0] != ".github/workflows/ci.yml" {
		t.Fatalf("flagged files = %v", got)
	}
	for _, file := range []string{".github/quipu/config.yaml", rootConfigPath, ".github/workflows/quipu-writer.yml", ".github/workflows/quipu-policy.yml",
		".claude/settings.json", ".github/workflows/sub/x.yml"} {
		pull.Files = []string{".github/workflows/ci.yml", file}
		if v := strings.Join(prViolations(c, pull, load, now), "\n"); !strings.Contains(v, file) || !strings.Contains(v, ownerApprovedLabel) {
			t.Errorf("flagged: %s still needs the owner's label: %q", file, v)
		}
	}
	pull.Labels = []string{ownerApprovedLabel}
	if v := prViolations(c, pull, load, now); len(v) != 0 {
		t.Fatalf("the owner's label still allows it: %v", v)
	}
}

func TestFlaggedWorkflowBotPRs(t *testing.T) {
	c := watchConfig()
	c.PolicyExemptAuthors, c.WorkflowChanges = []string{"dependabot[bot]"}, "flagged"
	bot := pullFacts{Author: "app/dependabot", IsBot: true, Base: "main", Head: "dependabot/github_actions/x", Files: []string{".github/workflows/ci.yml"}}
	if v := prViolations(c, bot, nil, cutover); len(v) != 0 {
		t.Fatalf("an exempt bot's workflow bump is flagged, not failed: %v", v)
	}
	bot.Files = []string{".github/workflows/quipu-writer.yml"}
	if v := prViolations(c, bot, nil, cutover); len(v) != 1 {
		t.Fatalf("Quipu's own workflow stays owner-only: %v", v)
	}
}

func TestMentionsOwnerOnlyPath(t *testing.T) {
	c := watchConfig()
	if !mentionsOwnerOnlyPath(c, ".github/workflows/ci.yml") {
		t.Fatal("by default every workflow is owner-only")
	}
	c.WorkflowChanges = "flagged"
	for arg, ownerOnly := range map[string]bool{
		".github/workflows/ci.yml":            false,
		"/repo/wt/.github/workflows/lint.yml": false,
		".github/workflows/quipu-writer.yml":  true,
		".github/workflows":                   true,
		".github/workflows/":                  true,
		".github/workflows/*.yml":             true,
		".github/workflows/a/../quipu-x.yml":  true,
		".github/quipu/config.yaml":           true,
		".codex/x;.github/workflows/ci.yml":   true,
		"README.md":                           false,
	} {
		if got := mentionsOwnerOnlyPath(c, arg); got != ownerOnly {
			t.Errorf("%q: owner-only = %v, want %v", arg, got, ownerOnly)
		}
	}
}

func TestGuardOpensWorkflowsOnlyWhenTheDefaultBranchFlagsThem(t *testing.T) {
	lease := func(top string) {
		now := time.Now().UTC()
		writeRecord(t, top, leaseContext{Issue: 1, Holder: "claude/octo/aaaa1111", Branch: "001/a",
			Expires: now.Add(time.Hour).Format(time.RFC3339), VerifiedAt: now.Format(time.RFC3339)})
	}
	edit := func(top, file string) string {
		return guardCode(t, top, "Edit", map[string]any{"file_path": filepath.Join(top, filepath.FromSlash(file))})
	}
	t.Run("strict", func(t *testing.T) { strictWorkflows(t, lease, edit) })
	t.Run("flagged", func(t *testing.T) { flaggedWorkflows(t, lease, edit) })
}

func strictWorkflows(t *testing.T, lease func(string), edit func(string, string) string) {
	_, strict := flaggedRepo(t, "")
	lease(strict.Top)
	if code := edit(strict.Top, ".github/workflows/ci.yml"); code != "quipu-config" {
		t.Fatalf("by default a workflow edit is denied: %q", code)
	}
	writeRepoFile(t, strict.Top, rootConfigPath, append(repoConfig(t), "workflow_changes: flagged\n"...))
	if code := edit(strict.Top, ".github/workflows/ci.yml"); code != "quipu-config" {
		t.Fatalf("a worktree copy cannot flag workflows open: %q", code)
	}
}

func flaggedWorkflows(t *testing.T, lease func(string), edit func(string, string) string) {
	_, flagged := flaggedRepo(t, "workflow_changes: flagged\n")
	lease(flagged.Top)
	if code := edit(flagged.Top, ".github/workflows/ci.yml"); code != "" {
		t.Fatalf("flagged: an edit to ci.yml is allowed: %q", code)
	}
	if code := guardCode(t, flagged.Top, "Bash", map[string]any{"command": "echo x >> .github/workflows/ci.yml"}); code != "" {
		t.Fatalf("flagged: a shell write to ci.yml is allowed: %q", code)
	}
	for _, file := range []string{".github/workflows/quipu-writer.yml", ".github/quipu/config.yaml", rootConfigPath} {
		if code := edit(flagged.Top, file); code != "quipu-config" {
			t.Errorf("flagged: %s stays owner-only: %q", file, code)
		}
	}
	for _, command := range []string{"echo x > .github/workflows/quipu-policy.yml", "sed -i '' s/a/b/ .github/workflows/*.yml"} {
		if code := guardCode(t, flagged.Top, "Bash", map[string]any{"command": command}); code != "quipu-config" {
			t.Errorf("flagged: %q stays denied: %q", command, code)
		}
	}
}
