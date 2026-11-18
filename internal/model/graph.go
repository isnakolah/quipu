package model

import (
	"fmt"
	"sort"
	"strings"
)

type Issue struct {
	ID           string   `json:"id"`
	Number       int      `json:"number"`
	Kind         string   `json:"kind"`
	Status       string   `json:"status"`
	State        string   `json:"state"`
	Area         string   `json:"area"`
	ProjectKind  string   `json:"project_kind"`
	ProjectArea  string   `json:"project_area"`
	Branch       string   `json:"branch"`
	ParentID     string   `json:"parent_id"`
	BlockerIDs   []string `json:"blocker_ids"`
	Labels       []string `json:"labels"`
	EpicApproved bool     `json:"epic_approved"`
	Body         string   `json:"body"`
}

var projectStatuses = map[string]bool{
	"Backlog": true, "Ready": true, "Claimed": true, "In progress": true,
	"In review": true, "Evidence pending": true, "Blocked": true,
	"Done": true, "Cancelled": true, "Archived": true,
}

// ValidationError carries all independently detected graph violations.
// Its deterministic output is safe to use in CI and receipts.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }

func (e *ValidationError) add(format string, args ...any) {
	e.Problems = append(e.Problems, fmt.Sprintf(format, args...))
}

func (e *ValidationError) err() error {
	if len(e.Problems) == 0 {
		return nil
	}
	sort.Strings(e.Problems)
	unique := e.Problems[:0]
	for _, problem := range e.Problems {
		if len(unique) == 0 || unique[len(unique)-1] != problem {
			unique = append(unique, problem)
		}
	}
	e.Problems = unique
	return e
}

func ValidateGraph(issues []Issue) error {
	byID := make(map[string]Issue, len(issues))
	problems := &ValidationError{}
	for _, issue := range issues {
		if issue.ID == "" {
			problems.add("issue id is required")
			continue
		}
		if _, ok := byID[issue.ID]; ok {
			problems.add("duplicate issue %s", issue.ID)
			continue
		}
		byID[issue.ID] = issue
	}
	if err := problems.err(); err != nil {
		return err
	}
	for _, issue := range issues {
		if issue.State == "CLOSED" {
			continue
		}
		live := issue.State != "CLOSED" && issue.Status != "Archived"
		if issue.Kind != "Epic" && live && issue.ParentID == "" {
			problems.add("issue #%d has no parent", issue.Number)
		}
		if issue.Kind != "Epic" {
			if err := ValidateWorkContractFor(issue.Kind, issue.Body); err != nil {
				problems.add("issue #%d: %v", issue.Number, err)
			}
		}
		if issue.Status == "" {
			problems.add("issue #%d has no Project Status", issue.Number)
		} else if !projectStatuses[issue.Status] {
			problems.add("issue #%d has unknown Project Status %q", issue.Number, issue.Status)
		}
		if issue.ProjectKind == "" {
			problems.add("issue #%d has no Project Kind", issue.Number)
		}
		kindLabels, areaLabels := 0, 0
		for _, label := range issue.Labels {
			switch {
			case strings.HasPrefix(label, "status:"):
				problems.add("issue #%d uses forbidden status label %q", issue.Number, label)
			case strings.HasPrefix(label, "kind:"):
				kindLabels++
			case strings.HasPrefix(label, "area:"):
				areaLabels++
			}
		}
		if issue.Kind != "" && kindLabels != 1 {
			problems.add("issue #%d must have exactly one kind:* label", issue.Number)
		}
		if issue.Area != "" && areaLabels != 1 {
			problems.add("issue #%d must have exactly one area:* label", issue.Number)
		}
		if issue.Kind == "Epic" && issue.Branch != "" {
			problems.add("Epic #%d cannot own implementation branch %q", issue.Number, issue.Branch)
		}
		if issue.Kind == "Epic" && issue.Status == "Ready" && !issue.EpicApproved {
			problems.add("Epic #%d cannot enter Ready without owner approval evidence", issue.Number)
		}
		if issue.ProjectKind != "" && !strings.EqualFold(issue.ProjectKind, issue.Kind) {
			problems.add("issue #%d Project Kind %q does not match kind:%s", issue.Number, issue.ProjectKind, strings.ToLower(issue.Kind))
		}
		if issue.Area != "" && issue.ProjectArea == "" {
			problems.add("issue #%d has no Project Area", issue.Number)
		} else if issue.ProjectArea != "" && !strings.EqualFold(issue.ProjectArea, issue.Area) {
			problems.add("issue #%d Project Area %q does not match area:%s", issue.Number, issue.ProjectArea, issue.Area)
		}
		for _, blocker := range issue.BlockerIDs {
			if _, ok := byID[blocker]; !ok {
				problems.add("issue #%d references unknown blocker %s", issue.Number, blocker)
				continue
			}
			blocking := byID[blocker]
			if requiresClearBlockers(issue.Status) && unresolved(blocking) {
				problems.add("issue #%d has unresolved blocker #%d while Status is %s", issue.Number, blocking.Number, issue.Status)
			}
		}
	}
	for _, issue := range issues {
		if issue.State == "CLOSED" {
			continue
		}
		if err := assertAcyclic(issue.ID, byID); err != nil {
			problems.add("%v", err)
		}
		if err := assertBlockerAcyclic(issue.ID, byID); err != nil {
			problems.add("%v", err)
		}
	}
	for _, parent := range issues {
		if parent.Status != "Done" {
			continue
		}
		for _, child := range issues {
			if child.ParentID == parent.ID && unresolved(child) {
				problems.add("parent #%d is Done while child #%d remains incomplete", parent.Number, child.Number)
			}
		}
	}
	return problems.err()
}

func requiresClearBlockers(status string) bool {
	return status == "Ready" || status == "Claimed" || status == "In progress" || status == "Done"
}

func unresolved(issue Issue) bool {
	return issue.State != "CLOSED" && issue.Status != "Done" && issue.Status != "Cancelled" && issue.Status != "Archived"
}

func assertAcyclic(start string, byID map[string]Issue) error {
	seen := map[string]bool{}
	for id := start; id != ""; {
		if seen[id] {
			return fmt.Errorf("parent cycle at issue %s", id)
		}
		seen[id] = true
		issue, ok := byID[id]
		if !ok {
			return fmt.Errorf("unknown parent issue %s", id)
		}
		id = issue.ParentID
	}
	return nil
}

func assertBlockerAcyclic(start string, byID map[string]Issue) error {
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var walk func(string) error
	walk = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("blocker cycle at issue %s", id)
		}
		if visited[id] {
			return nil
		}
		issue := byID[id]
		visiting[id] = true
		for _, blocker := range issue.BlockerIDs {
			if _, ok := byID[blocker]; ok {
				if err := walk(blocker); err != nil {
					return err
				}
			}
		}
		delete(visiting, id)
		visited[id] = true
		return nil
	}
	return walk(start)
}
