package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isnakolah/quipu/internal/model"
)

// noneExcludes is testContract with Excludes holding only "None".
var noneExcludes = strings.Replace(testContract, "Excludes:\n- conflict resolution", "Excludes:\n- None", 1)

func TestStartRefusesABrokenContractAsNotReady(t *testing.T) {
	previous := loadStartWork
	t.Cleanup(func() { loadStartWork = previous })
	ready := liveWork{IssueID: "I_61", IssueState: "OPEN", Status: "Ready", Kind: "Story", ParentID: "E", Body: noneExcludes}
	for name, tc := range map[string]struct {
		state      liveWork
		code, want string
	}{
		"Excludes only None": {ready, "not-ready", "Excludes: has no real item"},
		"placeholder Outcome": {func() liveWork {
			s := ready
			s.Body = strings.Replace(testContract, "Offline edits sync once the device reconnects.", "TBD", 1)
			return s
		}(), "not-ready", `"## Outcome" is empty`},
		"Defect without steps":  {func() liveWork { s := ready; s.Kind, s.Body = "Defect", testContract; return s }(), "not-ready", "Steps to reproduce"},
		"an Epic is not work":   {func() liveWork { s := ready; s.Kind = "Epic"; return s }(), "not-implementable", "only Stories"},
		"a parent names leaves": {func() liveWork { s := ready; s.Children = 2; return s }(), "not-leaf", "has sub-issues"},
	} {
		loadStartWork = func(model.Config, int) (liveWork, error) { return tc.state, nil }
		_, err := startIssue(model.Config{}, worktree{}, 61, startOptions{})
		var coded *flowError
		if !errors.As(err, &coded) || coded.Code != tc.code || !strings.Contains(coded.Message, tc.want) {
			t.Errorf("%s: want %s naming %q, got %v", name, tc.code, tc.want, err)
			continue
		}
		if tc.code == "not-ready" && (!strings.Contains(coded.Message, "fails a hard rule") || !strings.Contains(coded.Next, "quipu start --next")) {
			t.Errorf("%s: the refusal says it is the contract and what to do: %+v", name, coded)
		}
	}
	// Work already held resumes whatever its body: only a new claim is refused.
	held := ready
	held.Status = "In progress"
	if err := contractRefusal(61, held); err == nil {
		t.Fatal("contractRefusal judges the body; startIssue only asks it for Ready work")
	}
	if err := contractRefusal(61, liveWork{Kind: "Story", Body: testContract}); err != nil {
		t.Fatalf("a valid contract starts: %v", err)
	}
}

// fixtureOpenIssues are the open Issues issueCreateFixture's duplicate check sees.
var fixtureOpenIssues []model.OpenIssue

// issueCreateFixture runs `quipu issue create` in a directory with this
// repository's config, with GitHub replaced.
func issueCreateFixture(t *testing.T, config []byte, body string) (output string, filed []issueFiling, err error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(configPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, configPath), config, 0o644); err != nil {
		t.Fatal(err)
	}
	bodyFile := filepath.Join(dir, "contract.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	previous, previousOpen := fileIssue, openIssuesForDuplicates
	t.Cleanup(func() { fileIssue, openIssuesForDuplicates = previous, previousOpen })
	openIssuesForDuplicates = func(model.Config) ([]model.OpenIssue, error) { return fixtureOpenIssues, nil }
	fileIssue = func(_ model.Config, f issueFiling) (int, string, error) {
		filed = append(filed, f)
		return 42, "https://github.com/octo/repo/issues/42", nil
	}
	output, err = captureStdout(t, func() error {
		return issueCommand([]string{"create", "--kind", "story", "--parent", "14", "--area", "stable", "--title", "Sync offline edits on reconnect", "--body-file", bodyFile, "--apply", "--json"})
	})
	return output, filed, err
}

func TestIssueCreateReportsWarningsAndStillFiles(t *testing.T) {
	config, err := os.ReadFile(filepath.Join("..", "..", configPath))
	if err != nil {
		t.Fatal(err)
	}
	weak := strings.Replace(testContract, "Offline edits sync once the device reconnects.", "Implement sync so it works properly.", 1)
	output, filed, err := issueCreateFixture(t, config, weak)
	if err != nil {
		t.Fatal(err)
	}
	if len(filed) != 1 || filed[0].Kind != "story" || filed[0].Parent != 14 {
		t.Fatalf("warnings never stop the Issue being filed: %+v", filed)
	}
	var result struct {
		Issue    int      `json:"issue"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	joined := strings.Join(result.Warnings, "\n")
	if result.Issue != 42 || !strings.Contains(joined, `Outcome opens with "Implement"`) || !strings.Contains(joined, `"properly"`) {
		t.Fatalf("--json lists the warnings: %s", output)
	}

	output, _, err = issueCreateFixture(t, config, testContract)
	if err != nil || !strings.Contains(output, `"warnings": []`) {
		t.Fatalf("a clean contract still reports an empty warnings array: %v %s", err, output)
	}

	_, filed, err = issueCreateFixture(t, config, noneExcludes)
	if err == nil || !strings.Contains(err.Error(), "Excludes: has no real item") || len(filed) != 0 {
		t.Fatalf("a hard rule refuses before anything is filed: %v, filed %+v", err, filed)
	}
}

func TestProposalsMeetTheContractRules(t *testing.T) {
	if _, err := proposalWarnings("Sync offline edits", noneExcludes); err == nil || !strings.Contains(err.Error(), "Excludes") {
		t.Fatalf("a proposal written as a contract meets its hard rules: %v", err)
	}
	warnings, err := proposalWarnings("Sync offline edits", testContract)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("a good contract proposal is quiet: %v %v", warnings, err)
	}
	warnings, err = proposalWarnings(strings.Repeat("long ", 20), "We should sync offline edits.")
	if err != nil || len(warnings) != 2 || !strings.Contains(warnings[1], "not a work:v1 contract") {
		t.Fatalf("a prose proposal is posted with warnings: %v %v", warnings, err)
	}
	result := withWarnings(flowResult{Status: "Proposed"}, warnings, true)
	if len(result.Warnings) != 2 {
		t.Fatalf("--json carries the warnings: %+v", result)
	}
}
