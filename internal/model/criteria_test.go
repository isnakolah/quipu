package model

import (
	"strings"
	"testing"
)

const tickBody = `<!-- work:v1 -->

## Outcome

Something is true.

## Scope
Includes:
- [ ] a checkbox in Scope that is not a criterion

Excludes:
- other things

## Acceptance criteria
- [ ] Test: first
- [x] Test: second, already met
* [ ] Test: third

## Required evidence
- [ ] not a criterion either
- Verification boundary: CI
`

// changedLines lists the line numbers where two bodies differ.
func changedLines(a, b string) []int {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []int
	for i := range x {
		if i >= len(y) || x[i] != y[i] {
			out = append(out, i)
		}
	}
	if len(y) != len(x) {
		out = append(out, -1)
	}
	return out
}

func TestCriteriaReadsOnlyTheCriteriaSection(t *testing.T) {
	got := Criteria(tickBody)
	if len(got) != 3 || got[0].Text != "Test: first" || got[0].Done || !got[1].Done || got[2].Text != "Test: third" {
		t.Fatalf("criteria: %+v", got)
	}
	if done, total := CriteriaDone(tickBody); done != 1 || total != 3 {
		t.Fatalf("done %d of %d", done, total)
	}
}

func TestTickChangesOnlyThatBox(t *testing.T) {
	after, err := TickCriterion(tickBody, 3, true)
	if err != nil {
		t.Fatal(err)
	}
	changed := changedLines(tickBody, after)
	if len(changed) != 1 || strings.Split(after, "\n")[changed[0]] != "* [x] Test: third" {
		t.Fatalf("changed lines %v:\n%s", changed, after)
	}
	back, err := TickCriterion(after, 3, false)
	if err != nil || back != tickBody {
		t.Fatalf("untick did not restore the body: %v\n%s", err, back)
	}
	if same, _ := TickCriterion(tickBody, 2, true); same != tickBody {
		t.Fatal("ticking a met criterion changed the body")
	}
	for _, k := range []int{0, 4, -1} {
		if _, err := TickCriterion(tickBody, k, true); err == nil {
			t.Errorf("criterion %d was accepted", k)
		}
	}
}

func TestTickAllLeavesTheRestAlone(t *testing.T) {
	after := TickAll(tickBody)
	if done, total := CriteriaDone(after); done != 3 || total != 3 {
		t.Fatalf("done %d of %d", done, total)
	}
	if len(changedLines(tickBody, after)) != 2 {
		t.Fatalf("TickAll changed more than the two open criteria:\n%s", after)
	}
	if !strings.Contains(after, "- [ ] a checkbox in Scope") || !strings.Contains(after, "- [ ] not a criterion either") {
		t.Fatalf("TickAll ticked boxes outside the criteria:\n%s", after)
	}
}
