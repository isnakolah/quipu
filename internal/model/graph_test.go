package model

import (
	"strings"
	"testing"
)

func TestValidateGraphRejectsStatusAndDuplicateClassificationLabels(t *testing.T) {
	issues := []Issue{
		{ID: "epic", Number: 1, Kind: "Epic", Status: "Backlog", State: "OPEN", Area: "stable", ProjectKind: "Epic", ProjectArea: "stable", Labels: []string{"kind:epic", "area:stable"}},
		{ID: "work", Number: 2, Kind: "Story", Status: "Ready", State: "OPEN", Area: "stable", ProjectKind: "Story", ProjectArea: "stable", ParentID: "epic", Labels: []string{"kind:story", "kind:task", "area:stable", "status:ready"}, Body: validBody},
	}
	err := ValidateGraph(issues)
	if err == nil || !strings.Contains(err.Error(), "forbidden status label") || !strings.Contains(err.Error(), "exactly one kind:*") {
		t.Fatalf("error=%v", err)
	}
}

func TestValidateGraphRequiresApprovalForReadyEpic(t *testing.T) {
	err := ValidateGraph([]Issue{{ID: "epic", Number: 1, Kind: "Epic", Status: "Ready", State: "OPEN", Area: "stable", ProjectKind: "Epic", ProjectArea: "stable", Labels: []string{"kind:epic", "area:stable"}}})
	if err == nil || !strings.Contains(err.Error(), "owner approval") {
		t.Fatalf("error=%v", err)
	}
}
