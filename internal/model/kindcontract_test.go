package model

import (
	"strings"
	"testing"
)

const goodDecision = DecisionMarker + `

## Question

Should the Writer run on a schedule or only on events?

## Context

Events are missed when the runner sleeps.

## Decision drivers

- Receipts must not wait more than five minutes.

## Considered options

### Schedule every five minutes

Runs whether or not anything changed.

Consequences:
- Good, because nothing waits more than five minutes.
- Bad, because it spends runner minutes on idle repositories.

### Events only

Consequences:
- Good, because it costs nothing when idle.
- Bad, because a sleeping runner misses events.

## Recommended option

Schedule every five minutes, because missed receipts stall agents.

## Required approver

@octo

## Blocks

- #12
`

const goodEpic = EpicMarker + `

## Outcome

Quipu adopts private repositories.

## Non-goals

- Hosting runners for the owner.

## Standing constraints

- GitHub is the only record.

## Exit criteria

- [ ] A private repository runs the full loop.
- [x] Adoption audit lists every gap.

## Expected children

- Story: adopt audit

## Owner approval

Approved by @octo on 2026-11-18.
`

func TestDecisionContractAcceptsTwoOptionsWithoutOutcome(t *testing.T) {
	if strings.Contains(goodDecision, "## Outcome") {
		t.Fatal("fixture must not carry an Outcome")
	}
	if err := ValidateDecisionContract(goodDecision); err != nil {
		t.Fatal(err)
	}
	if err := ValidateContract("Decision", goodDecision); err != nil {
		t.Fatalf("dispatch by kind: %v", err)
	}
	if err := ValidateWorkContract(goodDecision); err == nil {
		t.Fatal("a decision:v1 body is not a work contract")
	}
}

func TestDecisionContractRefusals(t *testing.T) {
	oneOption := goodDecision[:strings.Index(goodDecision, "### Events only")] + "## Recommended option" + strings.SplitN(goodDecision, "## Recommended option", 2)[1]
	cases := map[string]struct{ body, want string }{
		"one option":                  {oneOption, "at least two"},
		"question without ?":          {strings.Replace(goodDecision, "only on events?", "only on events.", 1), `must end with "?"`},
		"option without consequences": {strings.Replace(goodDecision, "Consequences:\n- Good, because it costs nothing when idle.\n- Bad, because a sleeping runner misses events.\n", "Cheaper.\n", 1), `option "Events only" has no consequences`},
		"empty consequences":          {strings.Replace(goodDecision, "Consequences:\n- Good, because it costs nothing when idle.\n- Bad, because a sleeping runner misses events.\n", "Consequences:\n- \n", 1), "has no consequences"},
		"no marker":                   {strings.Replace(goodDecision, DecisionMarker, "", 1), "missing " + DecisionMarker},
		"missing approver":            {strings.Replace(goodDecision, "## Required approver", "## Approver", 1), `missing required section "Required approver"`},
		"empty blocks":                {strings.Replace(goodDecision, "- #12", "- #", 1), `section "Blocks" is empty`},
		"work contract only":          {WorkBody("x", "x", "x", "x", "x", "None: x", "x", "CI"), "missing " + DecisionMarker},
	}
	for name, tc := range cases {
		err := ValidateDecisionContract(tc.body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}

func TestDecisionContractAcceptsMADRBullets(t *testing.T) {
	body := strings.ReplaceAll(goodDecision, "Consequences:\n", "")
	if err := ValidateDecisionContract(body); err != nil {
		t.Fatal(err)
	}
}

func TestEpicContract(t *testing.T) {
	if err := ValidateContract("epic", goodEpic); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ body, want string }{
		"no checkboxes":   {strings.Replace(strings.Replace(goodEpic, "- [ ] A private", "- A private", 1), "- [x] Adoption", "- Adoption", 1), "Exit criteria must be checkboxes"},
		"empty checkbox":  {strings.Replace(strings.Replace(goodEpic, "- [ ] A private repository runs the full loop.", "- [ ]", 1), "- [x] Adoption audit lists every gap.", "", 1), `section "Exit criteria" is empty`},
		"no non-goals":    {strings.Replace(goodEpic, "## Non-goals\n\n- Hosting runners for the owner.\n", "", 1), `missing required section "Non-goals"`},
		"empty non-goals": {strings.Replace(goodEpic, "- Hosting runners for the owner.", "-", 1), `section "Non-goals" is empty`},
		"no marker":       {strings.Replace(goodEpic, EpicMarker, "", 1), "missing " + EpicMarker},
	}
	for name, tc := range cases {
		err := ValidateEpicContract(tc.body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}

func TestContractTemplatesNameEverySectionAndFailUntilFilled(t *testing.T) {
	for kind, sections := range map[string][]string{"decision": RequiredDecisionSections, "epic": RequiredEpicSections} {
		body, err := ContractTemplate(kind)
		if err != nil {
			t.Fatal(err)
		}
		for _, section := range sections {
			if !strings.Contains(body, "## "+section+"\n") {
				t.Errorf("%s template lacks %q", kind, section)
			}
		}
		err = ValidateContract(kind, body)
		if err == nil {
			t.Fatalf("empty %s template passed validation", kind)
		}
		for _, section := range sections {
			if section == "Considered options" {
				// The option headings stand, but each needs a name and consequences.
				if !strings.Contains(err.Error(), "option 1 has no name") || !strings.Contains(err.Error(), "option 2 has no consequences") {
					t.Errorf("decision template: unfilled options not reported in %v", err)
				}
				continue
			}
			if !strings.Contains(err.Error(), `"`+section+`" is empty`) {
				t.Errorf("%s template: %q not reported empty in %v", kind, section, err)
			}
		}
	}
	if _, err := ContractTemplate("story"); err == nil {
		t.Fatal("story has no --template")
	}
}

func TestContractTemplatesPassOnceFilled(t *testing.T) {
	decision, _ := ContractTemplate("decision")
	for _, fill := range [][2]string{
		{`<!-- The one thing the owner must decide, as a question ending in "?". -->`, "Which runner?"},
		{"<!-- What makes the decision necessary now, and what is already known. -->", "Runners sleep."},
		{"- <!-- A force or constraint any answer must weigh. -->", "- Cost"},
		{"### <!-- First option -->", "### Hosted"},
		{"### <!-- Second option -->", "### Self-hosted"},
		{"<!-- The option you recommend and why. A recommendation only: the owner decides, and records the decision when closing this Decision. -->", "Hosted."},
		{"<!-- @login of the owner who must decide. -->", "@octo"},
		{`- <!-- #N, an Issue waiting on this decision, or "None: reason". -->`, "- #4"},
	} {
		decision = strings.Replace(decision, fill[0], fill[1], 1)
	}
	decision = strings.ReplaceAll(decision, "- <!-- Good, because ... / Bad, because ... -->", "- Good, because it is simple.")
	if err := ValidateDecisionContract(decision); err != nil {
		t.Fatalf("filled decision template: %v\n%s", err, decision)
	}
	epic, _ := ContractTemplate("epic")
	epic = EpicMarker + htmlComment.ReplaceAllString(strings.TrimPrefix(epic, EpicMarker), "filled")
	if err := ValidateEpicContract(epic); err != nil {
		t.Fatalf("filled epic template: %v\n%s", err, epic)
	}
}

func TestFormRenderedBodiesNeedNoMarker(t *testing.T) {
	rendered := strings.NewReplacer(DecisionMarker, "", "\n## ", "\n### ", "\n### Schedule", "\n#### Schedule", "\n### Events", "\n#### Events").Replace(goodDecision)
	if err := ValidateDecisionContract(rendered); err != nil {
		t.Fatalf("form-rendered decision: %v\n%s", err, rendered)
	}
	epic := strings.NewReplacer(EpicMarker, "", "\n## ", "\n### ").Replace(goodEpic)
	if err := ValidateEpicContract(epic); err != nil {
		t.Fatalf("form-rendered epic: %v", err)
	}
	noResponse := strings.Replace(epic, "Quipu adopts private repositories.", "_No response_", 1)
	if err := ValidateEpicContract(noResponse); err == nil || !strings.Contains(err.Error(), `"Outcome" is empty`) {
		t.Fatalf("_No response_ counts as empty: %v", err)
	}
}

func TestBoardKeepsTheCheckOlderIssuesHad(t *testing.T) {
	work := validBody
	cases := []struct {
		kind, body string
		valid      bool
	}{
		{"Decision", work, true},         // filed as work:v1 before decision:v1
		{"Decision", goodDecision, true}, // its own contract
		{"Decision", "free text", false}, // neither
		{"Decision", strings.Replace(goodDecision, "?", ".", 1), false},
		{"Epic", "Owner-approved goal: adopt.", true}, // Epics were never checked
		{"Epic", "## Outcome\n\nx\n\n## Exit criteria\n\nx\n", true},
		{"Epic", goodEpic, true},
		{"Epic", strings.Replace(goodEpic, "- [ ] A private", "- A private", 1), true},
		{"Epic", strings.Replace(strings.Replace(goodEpic, "- [ ] A private", "- A private", 1), "- [x] Adoption", "- Adoption", 1), false},
		{"Story", goodDecision, false},
		{"Gate", work, true},
	}
	for i, tc := range cases {
		err := boardContractProblem(tc.kind, tc.body)
		if (err == nil) != tc.valid {
			t.Errorf("case %d (%s): valid=%v, err=%v", i, tc.kind, tc.valid, err)
		}
	}
}

func TestValidateGraphChecksDecisionContract(t *testing.T) {
	bad := strings.Replace(goodDecision, "only on events?", "only on events.", 1)
	issues := []Issue{
		{ID: "a", Number: 1, Kind: "Epic", Status: "Ready", ProjectKind: "Epic", Labels: []string{"kind:epic"}},
		{ID: "b", Number: 2, Kind: "Decision", ParentID: "a", Status: "Backlog", ProjectKind: "Decision", Labels: []string{"kind:decision"}, Body: bad},
	}
	err := ValidateGraph(issues)
	if err == nil || !strings.Contains(err.Error(), `issue #2: the Question must end with "?"`) {
		t.Fatalf("got %v", err)
	}
	issues[1].Body = goodDecision
	if err := ValidateGraph(issues); err != nil && strings.Contains(err.Error(), "issue #2:") {
		t.Fatalf("valid decision flagged: %v", err)
	}
}
