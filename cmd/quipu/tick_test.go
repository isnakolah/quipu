package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

const tickContract = `<!-- work:v1 -->

## Outcome

Something is true.

## Scope
Includes:
- a thing

Excludes:
- other things

## Acceptance criteria
- [ ] Test: first
- [ ] Test: second
- [ ] Test: third

## Required evidence
- go test ./...
- Verification boundary: CI

## Documentation impact
None: test

## Source and relationships
- test
`

func heldWork() liveWork {
	return liveWork{IssueID: "I_7", IssueState: "OPEN", Status: "In progress", Holder: choreActor, Labels: []string{"kind:task"}, Body: tickContract}
}

func TestTickFlipsOneBoxAndSaysSoInWords(t *testing.T) {
	f := &fakeChores{}
	// Ticking needs no issues_by setting: it is the holder's own progress.
	got := writerAnswers(t, model.Config{}, heldWork(), writer.Request{Action: "tick", Criterion: 2, Note: "parser done; Writer next"}, f)
	if got.Result != "accepted" || len(f.writes) != 1 {
		t.Fatalf("tick: %+v, wrote %v", got, f.writes)
	}
	want := strings.Replace(tickContract, "- [ ] Test: second", "- [x] Test: second", 1)
	if f.body != want {
		t.Fatalf("body after tick:\n%s", f.body)
	}
	rendered, err := writer.RenderReceipt(got)
	if err != nil {
		t.Fatal(err)
	}
	shown := rendered[strings.Index(rendered, "-->")+3:]
	for _, part := range []string{"criterion 2 met", "1 of 3 met", "Test: second", "parser done; Writer next"} {
		if !strings.Contains(shown, part) {
			t.Errorf("receipt summary lacks %q:\n%s", part, shown)
		}
	}
}

func TestTickRefusesOtherHoldersAndMissingCriteria(t *testing.T) {
	other := heldWork()
	other.Holder = "claude/octo/b"
	ready := heldWork()
	ready.Status, ready.Holder = "Ready", ""
	for name, tc := range map[string]struct {
		state liveWork
		r     writer.Request
		want  string
	}{
		"another holder":   {other, writer.Request{Action: "tick", Criterion: 1}, "only the lease holder"},
		"nobody holds it":  {ready, writer.Request{Action: "tick", Criterion: 1}, "only the lease holder"},
		"no such criteria": {heldWork(), writer.Request{Action: "tick", Criterion: 4}, "criterion 4 does not exist"},
	} {
		f := &fakeChores{}
		got := writerAnswers(t, model.Config{}, tc.state, tc.r, f)
		if got.Result != "rejected" || !strings.Contains(got.Detail, tc.want) || len(f.writes) != 0 {
			t.Errorf("%s: %+v, wrote %v", name, got, f.writes)
		}
	}
}

func TestDoneTicksWhatIsLeft(t *testing.T) {
	half, _ := model.TickCriterion(tickContract, 1, true)
	var set string
	detail := tickRemaining(half, func(body string) error { set = body; return nil })
	if want := model.TickAll(tickContract); set != want {
		t.Fatalf("Done set:\n%s", set)
	}
	if !strings.Contains(detail, "ticked the 2 acceptance criteria still open") {
		t.Fatalf("detail %q", detail)
	}
	if got := tickRemaining(model.TickAll(tickContract), func(string) error { t.Fatal("wrote a fully ticked body"); return nil }); got != "" {
		t.Fatalf("detail for a ticked body: %q", got)
	}
	if got := tickRemaining(half, func(string) error { return errors.New("boom") }); !strings.Contains(got, "could not be ticked: boom") {
		t.Fatalf("failure detail %q", got)
	}
}
