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

func TestWithGuideBlockKeepsRepositoryText(t *testing.T) {
	first := withGuideBlock("# Ours\n\nKeep me.\n", "handbook v1\n")
	if !strings.HasPrefix(first, "# Ours\n\nKeep me.\n\n"+guideBegin) {
		t.Fatalf("first=%q", first)
	}
	second := withGuideBlock(first+"\nAfter.\n", "handbook v2\n")
	if strings.Contains(second, "v1") || !strings.Contains(second, "handbook v2") || !strings.Contains(second, "Keep me.") || !strings.HasSuffix(second, "After.\n") {
		t.Fatalf("second=%q", second)
	}
	files := guideFiles(t.TempDir(), func(path string) (string, bool) {
		if path == "CLAUDE.md" {
			return "# Mine\n", true
		}
		return "", false
	})
	if files["CLAUDE.md"] != "# Mine\n\n@AGENTS.md\n" {
		t.Fatalf("CLAUDE.md=%q", files["CLAUDE.md"])
	}
}
