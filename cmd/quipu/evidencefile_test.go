package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isnakolah/quipu/internal/writer"
)

// evidenceFrom parses done's evidence arguments the way doneCommand does.
func evidenceFrom(t *testing.T, args ...string) (writer.Evidence, error) {
	t.Helper()
	fs := flag.NewFlagSet("done", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var e writer.Evidence
	evidenceFlags(fs, &e)
	file := fs.String("evidence-file", "", "")
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if *file != "" {
		return e, applyEvidenceFile(&e, *file, fs)
	}
	return e, nil
}

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

var flagEvidence = []string{
	"--criteria", "All 3 criteria met; login rejects bad tokens (TestLogin)",
	"--commands", "go test ./...; go vet ./...",
	"--documentation", "README Install section",
	"--risks", "None",
	"--boundary", "CI",
}

// Claude Code's isolation check refused `quipu done` six times because the
// evidence text on its command line looked like commands.
func TestEvidenceFileMatchesTheFlags(t *testing.T) {
	want, err := evidenceFrom(t, flagEvidence...)
	if err != nil {
		t.Fatal(err)
	}
	if want.Criteria != flagEvidence[1] || want.Environments != defaultEnvironments || want.Artifacts != defaultArtifacts {
		t.Fatalf("the flags work unchanged: %+v", want)
	}
	markdown := writeTemp(t, "evidence.md", `## Criteria
<!-- hint -->
- [x] All 3 criteria met
- [x] login rejects bad tokens (TestLogin)

## Commands
go test ./...
go vet ./...

## Documentation
README Install section

## Risks
None

## Boundary
CI
`)
	jsonFile := writeTemp(t, "evidence.json", `{"criteria": "All 3 criteria met; login rejects bad tokens (TestLogin)",
 "commands": "go test ./...; go vet ./...", "documentation": "README Install section", "risks": "None", "boundary": "CI"}`)
	for _, path := range []string{markdown, jsonFile} {
		got, err := evidenceFrom(t, "--evidence-file", path)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		if got != want {
			t.Errorf("%s:\n got %+v\nwant %+v", filepath.Base(path), got, want)
		}
	}

	overridden, err := evidenceFrom(t, "--evidence-file", markdown, "--risks", "Untested on Windows", "--environments", "macOS")
	if err != nil || overridden.Risks != "Untested on Windows" || overridden.Environments != "macOS" || overridden.Commands != want.Commands {
		t.Fatalf("a flag overrides its field only: %+v %v", overridden, err)
	}
	withOptional, err := evidenceFrom(t, "--evidence-file", writeTemp(t, "e.md", evidenceTemplate+"\n## CI URL\nhttps://github.com/o/r/actions/runs/1\n"),
		"--criteria", "c", "--commands", "x", "--documentation", "None: none", "--risks", "None", "--boundary", "CI")
	if err != nil || withOptional.CIURL != "https://github.com/o/r/actions/runs/1" || withOptional.Environments != defaultEnvironments {
		t.Fatalf("optional sections fill in, empty ones keep the default: %+v %v", withOptional, err)
	}
}

func TestEvidenceFileNamesWhatIsMissing(t *testing.T) {
	var coded *flowError
	_, err := evidenceFrom(t, "--evidence-file", writeTemp(t, "e.md", "## Criteria\nok\n## Commands\nmake\n## Documentation\nNone: x\n## Boundary\nCI\n"))
	if !errors.As(err, &coded) || coded.Code != "evidence-missing" || !strings.Contains(coded.Message, "## Risks") {
		t.Fatalf("a missing section is named: %v", err)
	}
	_, err = evidenceFrom(t, "--evidence-file", writeTemp(t, "e.md", evidenceTemplate))
	if !errors.As(err, &coded) || !strings.Contains(coded.Message, "## Criteria, ## Commands, ## Documentation, ## Risks, ## Boundary") {
		t.Fatalf("an unfilled template is missing every required section: %v", err)
	}
	if _, err := evidenceFrom(t, "--evidence-file", writeTemp(t, "e.md", "## Critera\nok\n")); err == nil || !strings.Contains(err.Error(), `"## Critera"`) {
		t.Fatalf("a misspelt section is named: %v", err)
	}
	if _, err := evidenceFrom(t, "--evidence-file", writeTemp(t, "e.json", `{"risk": "None"}`)); err == nil || !strings.Contains(err.Error(), `"risk"`) {
		t.Fatalf("a misspelt key is named: %v", err)
	}
	if _, err := evidenceFrom(t, "--evidence-file", filepath.Join(t.TempDir(), "absent.md")); !errors.As(err, &coded) {
		t.Fatalf("an unreadable file: %v", err)
	}
}
