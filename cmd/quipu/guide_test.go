package main

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestGuidesInSync fails when a committed agent guide differs from what
// `quipu guide --sync` generates. Fix it with `go run ./cmd/quipu guide --sync`.
func TestGuidesInSync(t *testing.T) {
	top, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := syncGuides(top, false)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(changed)
	if len(changed) > 0 {
		t.Fatalf("out of date: %v; run `go run ./cmd/quipu guide --sync`", changed)
	}
}

func TestGuideRendersEveryRuleAndError(t *testing.T) {
	text := renderGuide()
	if strings.Contains(text, "{{") {
		t.Fatal("unfilled placeholder")
	}
	for _, rule := range guardRules {
		if !strings.Contains(text, "| `"+rule.Code+"` |") {
			t.Errorf("guard rule %s missing", rule.Code)
		}
	}
	for _, kind := range flowErrors {
		if !strings.Contains(text, "| `"+kind.Code+"` |") {
			t.Errorf("error %s missing", kind.Code)
		}
	}
}

func TestGuideNeverModifiesProjectInstructions(t *testing.T) {
	own := map[string]string{"AGENTS.md": "# Ours\n\nKeep me.\n", "CLAUDE.md": "# Mine\n"}
	read := func(path string) (string, bool) { v, ok := own[path]; return v, ok }
	files := guideFiles(t.TempDir(), read)
	if files["AGENTS.md"] != own["AGENTS.md"] || files["CLAUDE.md"] != own["CLAUDE.md"] {
		t.Fatalf("project files changed: %q %q", files["AGENTS.md"], files["CLAUDE.md"])
	}
	if files[guideRule] != renderGuide() {
		t.Fatal("handbook must go to " + guideRule)
	}
	if hints := guideHints(read); len(hints) != 2 {
		t.Fatalf("hints=%v", hints)
	}
	missing := guideFiles(t.TempDir(), func(string) (string, bool) { return "", false })
	if missing["AGENTS.md"] != agentsPointer || missing["CLAUDE.md"] != claudePointer {
		t.Fatal("missing files get the pointers")
	}
	if hints := guideHints(func(p string) (string, bool) {
		return map[string]string{"AGENTS.md": agentsPointer, "CLAUDE.md": claudePointer}[p], true
	}); len(hints) != 0 {
		t.Fatalf("pointers need no hints: %v", hints)
	}
}
