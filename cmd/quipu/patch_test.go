package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchFiles(t *testing.T) {
	patch := "diff --git a/quipu.yml b/quipu.yml\n--- a/quipu.yml\n+++ b/quipu.yml\n@@ -1 +1 @@\n-a\n+b\n" +
		"--- /dev/null\n+++ b/.github/workflows/quipu-new.yml\n@@ -0,0 +1 @@\n+x\n" +
		"--- a/.codex/hooks.json\t2026-11-08\n+++ /dev/null\n@@ -1 +0,0 @@\n-y\n"
	files, err := patchFiles(patch)
	if err != nil || strings.Join(files, ",") != "quipu.yml,.github/workflows/quipu-new.yml,.codex/hooks.json" {
		t.Fatalf("files = %v, %v", files, err)
	}
	for name, bad := range map[string]string{
		"not a diff":       "just words\n",
		"outside":          "--- a/../x\n+++ b/../x\n",
		"absolute":         "--- /etc/hosts\n+++ /etc/hosts\n",
		"git internals":    "--- a/.git/config\n+++ b/.git/config\n",
		"request markers":  "--- a/x\n+++ b/x\n+<!-- quipu-request:v1 -->\n",
		"patch end marker": "--- a/x\n+++ b/x\n+" + patchEnd + "\n",
		"too big":          "--- a/x\n+++ b/x\n+" + strings.Repeat("x", patchMaxBytes) + "\n",
	} {
		if _, err := patchFiles(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPatchSurvivesTheComment(t *testing.T) {
	patch := "--- a/docs/x.md\n+++ b/docs/x.md\n@@ -1,3 +1,3 @@\n ````go\n-old\n+new\n ````\n"
	files, err := patchFiles(patch)
	if err != nil {
		t.Fatal(err)
	}
	section := patchSection(patch, files)
	if !strings.Contains(section, "`````diff\n") {
		t.Fatalf("the fence must outrun the patch's own backticks:\n%s", section)
	}
	body := "<!-- quipu-proposal:v1 -->\n**Proposed work:** x\n\nwhy\n\n" + section
	for _, comment := range []string{body, strings.ReplaceAll(body, "\n", "\r\n")} {
		got, err := extractPatch(comment)
		if err != nil || got != patch {
			t.Fatalf("round trip:\n%q\n%v", got, err)
		}
	}
	if _, err := extractPatch("<!-- quipu-proposal:v1 -->\nno patch"); err == nil {
		t.Fatal("a proposal without a patch has none to apply")
	}
	if _, err := extractPatch(strings.TrimSuffix(body, patchEnd+"\n")); err == nil {
		t.Fatal("a cut-short patch is refused")
	}
}

func TestExtractedPatchApplies(t *testing.T) {
	top := gitRepo(t)
	writeRepoFile(t, top, ".github/quipu/config.yaml", []byte("merge: owner\n"))
	commitAll(t, top)
	patch := "diff --git a/.github/quipu/config.yaml b/.github/quipu/config.yaml\n--- a/.github/quipu/config.yaml\n+++ b/.github/quipu/config.yaml\n@@ -1 +1 @@\n-merge: owner\n+merge: agent\n"
	files, _ := patchFiles(patch)
	got, err := extractPatch(patchSection(patch, files))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "p.diff")
	if err := os.WriteFile(file, []byte(got), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", top, "apply", file).CombinedOutput(); err != nil {
		t.Fatalf("git apply: %s", output)
	}
	if raw, _ := os.ReadFile(filepath.Join(top, ".github", "quipu", "config.yaml")); string(raw) != "merge: agent\n" {
		t.Fatalf("applied: %q", raw)
	}
}
