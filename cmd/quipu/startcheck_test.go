package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
)

// Calla #61: `quipu start 61` accepted a Story with sub-issues, and `policy`
// refused its PR 40 minutes later. Start now refuses what policy would.
func TestStartRefusesWhatPolicyWouldRefuse(t *testing.T) {
	good := liveWork{IssueID: "I_61", IssueState: "OPEN", Status: "Ready", Kind: "Story", ParentID: "E", Body: testContract}
	if problems := workProblems(61, good); len(problems) != 0 {
		t.Fatalf("a valid Ready leaf must start: %v", problems)
	}

	parent := good
	parent.Children = 3
	problems := workProblems(61, parent)
	var coded *flowError
	err := startRefusal(61, parent, problems, []int{278, 279})
	if !errors.As(err, &coded) || coded.Code != "not-leaf" {
		t.Fatalf("an Issue with sub-issues: %v", err)
	}
	if !strings.Contains(coded.Message, "#61 has sub-issues") || !strings.Contains(coded.Next, "#278, #279") || !strings.Contains(coded.Next, "quipu start 278") {
		t.Fatalf("the refusal names the problem and the Ready leaves: %+v", coded)
	}
	if err := startRefusal(61, parent, problems, nil); !errors.As(err, &coded) || coded.Next == "" {
		t.Fatalf("with no Ready leaf there is still a next step: %v", err)
	}

	for name, mutate := range map[string]func(*liveWork){
		"no parent":       func(s *liveWork) { s.ParentID = "" },
		"an Epic":         func(s *liveWork) { s.Kind = "Epic" },
		"a Decision":      func(s *liveWork) { s.Kind = "Decision" },
		"no contract":     func(s *liveWork) { s.Body = "just text" },
		"an open blocker": func(s *liveWork) { s.Blockers = []liveBlocker{{"B", "OPEN"}} },
		"a closed Issue":  func(s *liveWork) { s.IssueState = "CLOSED" },
	} {
		state := good
		mutate(&state)
		problems := workProblems(61, state)
		if len(problems) == 0 {
			t.Errorf("%s: start must refuse", name)
			continue
		}
		if err := startRefusal(61, state, problems, nil); !errors.As(err, &coded) || coded.Code != "not-implementable" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The words start refuses with are the words policy prints on the PR.
func TestStartAndPolicyShareTheWorkCheck(t *testing.T) {
	state := liveWork{IssueState: "OPEN", Status: "In progress", Kind: "Story", Children: 2, Body: testContract,
		Holder: "claude/octo/a", Branch: "061/x", Expiry: time.Now().Add(time.Hour).Format(time.RFC3339)}
	pull := pullFacts{Body: "Refs #61", Base: "main", Head: "061/x", Author: "octo"}
	violations := prViolations(model.Config{DefaultBranch: "main"}, pull, func(int) (liveWork, error) { return state, nil }, time.Now())
	for _, problem := range workProblems(61, state) {
		found := false
		for _, v := range violations {
			found = found || v == problem
		}
		if !found {
			t.Errorf("policy does not report %q: %v", problem, violations)
		}
	}
}
