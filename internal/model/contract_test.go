package model

import (
	"os"
	"strings"
	"testing"
)

// validBody is a complete work contract that raises no warning.
var validBody = WorkBody("Offline edits sync once the device reconnects.", "the sync queue", "conflict resolution",
	"Test: an edit made offline reaches the server after reconnect", "go test ./... output", "None: no user-facing docs change",
	"Owner request, 2026-11-18", "CI")

// templateBody is the Work Issue template as GitHub shows it, unfilled.
const templateBody = `<!-- work:v1 -->
## Outcome
## Scope
Includes:
-
Excludes:
-
## Acceptance criteria
- [ ]
## Required evidence
-
- Verification boundary: local | CI | target host | provider | staging | production | release
## Documentation impact
None: explain specific reason
## Source and relationships
- Native parent and blocker links are authoritative.
`

func TestWorkBodyValid(t *testing.T) {
	if err := ValidateWorkContract(validBody); err != nil {
		t.Fatal(err)
	}
	if warnings := WorkContractWarnings("Sync offline edits on reconnect", validBody); len(warnings) != 0 {
		t.Fatalf("a good contract raises no warning: %v", warnings)
	}
}

// replaceSection swaps the lines under heading for content.
func replaceSection(t *testing.T, body, heading, content string) string {
	t.Helper()
	start := strings.Index(body, heading+"\n")
	if start < 0 {
		t.Fatalf("no %s in body", heading)
	}
	rest := body[start+len(heading)+1:]
	end := strings.Index(rest, "\n## ")
	if end < 0 {
		end = len(rest)
	} else {
		end++
	}
	return body[:start] + heading + "\n" + content + "\n" + rest[end:]
}

func TestPlaceholderSectionsAreRefusedByName(t *testing.T) {
	err := ValidateWorkContract(templateBody)
	if err == nil || !strings.Contains(err.Error(), `"## Outcome"`) {
		t.Fatalf("the unfilled template must be refused, naming Outcome: %v", err)
	}
	for name, tc := range map[string]struct{ heading, content, want string }{
		"Outcome TBD":           {"## Outcome", "TBD", `"## Outcome" is empty`},
		"Outcome dots":          {"## Outcome", "...\n- TODO\nx", `"## Outcome" is empty`},
		"Includes bare bullet":  {"## Scope", "Includes:\n-\n\nExcludes:\n- conflict resolution", "Includes: lists nothing"},
		"no Excludes label":     {"## Scope", "Includes:\n- the sync queue", "scope requires Includes and Excludes"},
		"empty checkbox":        {"## Acceptance criteria", "- [ ]\n- [ ] TBD", "no checkbox criteria"},
		"criteria as prose":     {"## Acceptance criteria", "It works offline.", "no checkbox criteria"},
		"evidence placeholders": {"## Required evidence", "-\n- Verification boundary: local | CI | target host | provider | staging | production | release", `"## Required evidence" is empty`},
		"boundary choice text":  {"## Required evidence", "- go test output\n- Verification boundary: local | CI | target host | provider | staging | production | release", "verification boundary has no value"},
		"boundary blank":        {"## Required evidence", "- go test output\n- Verification boundary:", "verification boundary has no value"},
		"no boundary":           {"## Required evidence", "- go test output", "missing verification boundary"},
		"docs template text":    {"## Documentation impact", "None: explain specific reason", `"## Documentation impact" is empty`},
		"source boilerplate":    {"## Source and relationships", "- Native parent and blocker links are authoritative.", `"## Source and relationships" is empty`},
	} {
		err := ValidateWorkContract(replaceSection(t, validBody, tc.heading, tc.content))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error containing %q, got %v", name, tc.want, err)
		}
	}
	if err := ValidateWorkContract(strings.Replace(validBody, "## Outcome", "## Outcomes", 1)); err == nil || !strings.Contains(err.Error(), `missing required section "## Outcome"`) {
		t.Fatalf("a missing heading is still refused: %v", err)
	}
}

func TestExcludesNeedsARealItem(t *testing.T) {
	for _, excludes := range []string{"- None", "- none.", "-", "- None\n- TBD", ""} {
		body := replaceSection(t, validBody, "## Scope", "Includes:\n- the sync queue\n\nExcludes:\n"+excludes)
		err := ValidateWorkContract(body)
		if err == nil || !strings.Contains(err.Error(), "Excludes: has no real item") {
			t.Errorf("Excludes %q must be refused: %v", excludes, err)
		}
	}
	inline := replaceSection(t, validBody, "## Scope", "Includes: the sync queue.\nExcludes: conflict resolution.")
	if err := ValidateWorkContract(inline); err != nil {
		t.Fatalf("items on the label's own line count: %v", err)
	}
}

const defectBody = `<!-- work:v1 -->

## Outcome

Saving a draft while offline keeps it, so nothing typed is lost.

## Scope
Includes:
- the draft store

Excludes:
- sync of drafts between devices

## Acceptance criteria
- [ ] Test: a draft saved offline is there after restart

## Required evidence
- go test ./... output
- Verification boundary: CI

## Documentation impact
None: a fix to documented behaviour

## Source and relationships
- Reported in #12 on 2026-11-02

## Steps to reproduce
1. Turn the network off.
2. Type a draft and press Save.
3. Restart the app.

## Expected
The draft is listed.

## Actual
The draft list is empty.

## Environment
macOS 15.1, app 2.4.0
`

func TestDefectNeedsItsReproduction(t *testing.T) {
	if err := ValidateWorkContractFor("defect", defectBody); err != nil {
		t.Fatalf("a complete Defect is accepted: %v", err)
	}
	if err := ValidateWorkContractFor("Defect", replaceSection(t, defectBody, "## Expected", "- The draft is listed")); err != nil {
		t.Fatalf("a Project Kind spelling is a Defect too: %v", err)
	}
	cut := func(heading string) string {
		start := strings.Index(defectBody, heading+"\n")
		rest := defectBody[start+len(heading):]
		end := strings.Index(rest, "\n## ")
		if end < 0 {
			return defectBody[:start]
		}
		return defectBody[:start] + rest[end+1:]
	}
	for name, tc := range map[string]struct{ body, want string }{
		"no steps":         {cut("## Steps to reproduce"), "Steps to reproduce"},
		"steps unnumbered": {replaceSection(t, defectBody, "## Steps to reproduce", "- Turn the network off\n- Save"), "numbered steps"},
		"steps only TBD":   {replaceSection(t, defectBody, "## Steps to reproduce", "1. TBD"), "numbered steps"},
		"no Expected":      {cut("## Expected"), `"## Expected"`},
		"no Actual":        {cut("## Actual"), `"## Actual"`},
		"no Environment":   {cut("## Environment"), `"## Environment"`},
		"empty Actual":     {replaceSection(t, defectBody, "## Actual", "TBD"), `"## Actual" is empty`},
		"same outcome":     {replaceSection(t, defectBody, "## Actual", "the draft is listed"), "Expected and Actual say the same thing"},
	} {
		err := ValidateWorkContractFor("kind:defect", tc.body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error containing %q, got %v", name, tc.want, err)
		}
	}
	if err := ValidateWorkContractFor("story", validBody); err != nil {
		t.Fatalf("only a Defect needs a reproduction: %v", err)
	}
	built := DefectBody(validBody, []string{"Run it", "Watch it fail"}, "It passes.", "It fails.", "GitHub Actions, ubuntu-latest")
	if err := ValidateWorkContractFor("defect", built); err != nil {
		t.Fatalf("DefectBody builds a valid Defect: %v", err)
	}
}

func TestEachWarningFiresAndIssue168StaysQuiet(t *testing.T) {
	raw, err := os.ReadFile("testdata/issue168.md")
	if err != nil {
		t.Fatal(err)
	}
	issue168 := string(raw)
	if err := ValidateWorkContract(issue168); err != nil {
		t.Fatalf("quipu#168 is a valid contract: %v", err)
	}
	if warnings := WorkContractWarnings("Done skips a cleanup command that lands in the PR it merges", issue168); len(warnings) != 0 {
		t.Fatalf("quipu#168 raises no warning: %v", warnings)
	}
	criteria := func(items ...string) string {
		return replaceSection(t, validBody, "## Acceptance criteria", "- [ ] "+strings.Join(items, "\n- [ ] "))
	}
	for name, tc := range map[string]struct{ title, body, want string }{
		"more than 7 criteria": {"", replaceSection(t, criteria("a", "b", "c", "d", "e", "f", "g", "h"), "## Scope", "Includes:\n- the queue\nExcludes:\n- conflicts"), "8 acceptance criteria"},
		"long criterion":       {"", criteria("Test: " + strings.Repeat("an offline edit reaches the server ", 6)), "criterion 1 is"},
		"semicolon":            {"", criteria("Test: offline edits sync; online edits stay put"), "criterion 1 joins several outcomes"},
		"chained and":          {"", criteria("Test: edits sync and drafts sync and photos sync and settings sync"), "criterion 1 joins several outcomes"},
		"vague criterion":      {"", criteria("Test: sync works properly"), `criterion 1 uses vague wording "properly"`},
		"vague outcome":        {"", replaceSection(t, validBody, "## Outcome", "Sync is fast, etc."), `Outcome uses vague wording "etc", "fast"`},
		"activity outcome":     {"", replaceSection(t, validBody, "## Outcome", "Implement the sync queue."), `Outcome opens with "Implement"`},
		"refactor outcome":     {"", replaceSection(t, validBody, "## Outcome", "refactor the store so drafts survive restart."), `Outcome opens with "refactor"`},
		"includes outnumber":   {"", replaceSection(t, validBody, "## Scope", "Includes:\n- the queue\n- the store\nExcludes:\n- conflicts"), "Scope Includes has 2 items but only 1 acceptance criteria"},
		"long title":           {strings.Repeat("Sync ", 15), validBody, "title is 74 characters"},
		"source without ref":   {"", replaceSection(t, validBody, "## Source and relationships", "- The owner asked for it."), "Source and relationships has no link, #N or date"},
		"depends on prose":     {"", replaceSection(t, validBody, "## Source and relationships", "- Owner, 2026-11-18\n- Depends on the brief Issue."), `the body says "Depends on"`},
		"blocked by an Issue":  {"", replaceSection(t, validBody, "## Outcome", "Edits sync, which is blocked by #12 today."), `the body says "blocked by #12"`},
	} {
		if err := ValidateWorkContract(tc.body); err != nil {
			t.Errorf("%s: warnings never refuse, but the fixture is invalid: %v", name, err)
			continue
		}
		warnings := WorkContractWarnings(tc.title, tc.body)
		found := false
		for _, w := range warnings {
			found = found || strings.Contains(w, tc.want)
		}
		if !found {
			t.Errorf("%s: want a warning containing %q, got %v", name, tc.want, warnings)
		}
	}
	// Words a contract mentions in code or quotes, or a dependency of a
	// package, are not wording to fix.
	quiet := replaceSection(t, validBody, "## Outcome", "The `--fast` flag and the word \"properly\" are refused; the .deb depends on libvulkan1.")
	if warnings := WorkContractWarnings("", quiet); len(warnings) != 0 {
		t.Fatalf("mentioned words raise no warning: %v", warnings)
	}
}

func TestValidateGraphRejectsParentCycle(t *testing.T) {
	issues := []Issue{{ID: "a", Number: 1, Kind: "Epic", ParentID: "b"}, {ID: "b", Number: 2, Kind: "Epic", ParentID: "a"}}
	if err := ValidateGraph(issues); err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestValidateGraphRejectsBlockerCycle(t *testing.T) {
	issues := []Issue{
		{ID: "a", Number: 1, Kind: "Epic"},
		{ID: "b", Number: 2, Kind: "Story", ParentID: "a", Body: validBody, BlockerIDs: []string{"c"}},
		{ID: "c", Number: 3, Kind: "Story", ParentID: "a", Body: validBody, BlockerIDs: []string{"b"}},
	}
	if err := ValidateGraph(issues); err == nil {
		t.Fatal("expected blocker cycle error")
	}
}

func TestValidateGraphRejectsReadyWorkWithBlocker(t *testing.T) {
	issues := []Issue{
		{ID: "a", Number: 1, Kind: "Epic"},
		{ID: "b", Number: 2, Kind: "Story", ParentID: "a", Body: validBody, Status: "Ready", BlockerIDs: []string{"c"}},
		{ID: "c", Number: 3, Kind: "Story", ParentID: "a", Body: validBody, Status: "Backlog"},
	}
	if err := ValidateGraph(issues); err == nil {
		t.Fatal("expected readiness blocker error")
	}
}

func TestValidateGraphRejectsDoneParentWithIncompleteChild(t *testing.T) {
	issues := []Issue{
		{ID: "a", Number: 1, Kind: "Epic", Status: "Done"},
		{ID: "b", Number: 2, Kind: "Story", ParentID: "a", Body: validBody, Status: "In progress"},
	}
	if err := ValidateGraph(issues); err == nil {
		t.Fatal("expected parent completion error")
	}
}

func TestValidateGraphRejectsClassificationMismatchAndEpicBranch(t *testing.T) {
	issues := []Issue{
		{ID: "a", Number: 1, Kind: "Epic", Branch: "001/nope"},
		{ID: "b", Number: 2, Kind: "Story", ParentID: "a", Body: validBody, Area: "core", ProjectKind: "Task", ProjectArea: "delivery"},
	}
	if err := ValidateGraph(issues); err == nil {
		t.Fatal("expected classification and Epic branch errors")
	}
}

func TestValidateGraphAcceptsCompleteGraph(t *testing.T) {
	issues := []Issue{
		{ID: "a", Number: 1, Kind: "Epic", Status: "Backlog", ProjectKind: "Epic", Labels: []string{"kind:epic"}},
		{ID: "b", Number: 2, Kind: "Story", ParentID: "a", Body: validBody, Status: "Backlog", Area: "stable", ProjectKind: "Story", ProjectArea: "stable", Labels: []string{"kind:story", "area:stable"}},
	}
	if err := ValidateGraph(issues); err != nil {
		t.Fatal(err)
	}
}
