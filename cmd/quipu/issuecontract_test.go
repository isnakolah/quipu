package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isnakolah/quipu/internal/model"
)

func TestIssueCreateTemplatePrintsEachKindsContract(t *testing.T) {
	for kind, sections := range map[string][]string{"decision": model.RequiredDecisionSections, "epic": model.RequiredEpicSections} {
		out, err := captureStdout(t, func() error { return issueCommand([]string{"create", "--kind", kind, "--template"}) })
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		for _, section := range sections {
			if !strings.Contains(out, "## "+section) {
				t.Errorf("%s template lacks %q", kind, section)
			}
		}
		if model.ValidateContract(kind, out) == nil {
			t.Errorf("%s template passes validation unfilled", kind)
		}
		// Filed as printed, issue create refuses it before touching GitHub.
		file := filepath.Join(t.TempDir(), "body.md")
		if err := os.WriteFile(file, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
		err = issueCommand([]string{"create", "--kind", kind, "--title", "t", "--body-file", file, "--parent", "1", "--apply"})
		if err == nil || !strings.Contains(err.Error(), model.ContractName(kind)) {
			t.Errorf("%s: unfilled template accepted: %v", kind, err)
		}
	}
	if err := issueCommand([]string{"create", "--kind", "story", "--template"}); err == nil {
		t.Error("story has no --template")
	}
	if err := issueCommand([]string{"create", "--kind", "epic", "--template", "--apply"}); err == nil {
		t.Error("--template with --apply must refuse")
	}
}

func TestIssueCreateChecksDecisionsAgainstDecisionContract(t *testing.T) {
	file := filepath.Join(t.TempDir(), "body.md")
	write := func(body string) {
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A work:v1 body was how Decisions were filed; it no longer is.
	write(model.WorkBody("x", "x", "x", "x", "x", "None: x", "x", "CI"))
	err := issueCommand([]string{"create", "--kind", "decision", "--title", "t", "--body-file", file, "--parent", "1", "--apply"})
	if err == nil || !strings.Contains(err.Error(), "decision:v1") {
		t.Fatalf("work body filed as a Decision: %v", err)
	}
	// An Epic body is now checked too.
	write("Owner-approved goal: adopt.")
	err = issueCommand([]string{"create", "--kind", "epic", "--title", "t", "--body-file", file, "--approved-by", "me", "--apply"})
	if err == nil || !strings.Contains(err.Error(), "epic:v1") {
		t.Fatalf("free-text Epic accepted: %v", err)
	}
}
