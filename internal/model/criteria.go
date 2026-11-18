package model

// Acceptance criteria are the checkboxes under a work contract's
// "## Acceptance criteria" heading. Agents tick them as they meet each one, so
// an Issue shows how far its work got even when the work stops part way.

import (
	"fmt"
	"regexp"
	"strings"
)

const criteriaHeading = "## Acceptance criteria"

// checkbox matches a task-list line: its indent and bullet, its box, its text.
var checkbox = regexp.MustCompile(`^(\s*[-*+]\s+\[)([ xX])(\]\s?)(.*)$`)

// Criterion is one acceptance-criteria checkbox.
type Criterion struct {
	Text string
	Done bool
	line int
}

// Criteria lists the checkboxes in a body's Acceptance criteria section, in order.
func Criteria(body string) []Criterion {
	var out []Criterion
	inSection := false
	for i, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "# ") {
			inSection = strings.EqualFold(trimmed, criteriaHeading)
			continue
		}
		if !inSection {
			continue
		}
		if m := checkbox.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			out = append(out, Criterion{Text: strings.TrimSpace(m[4]), Done: m[2] != " ", line: i})
		}
	}
	return out
}

// TickCriterion sets criterion k (counting from 1) done or not done and returns
// the new body. Nothing but that one box changes.
func TickCriterion(body string, k int, done bool) (string, error) {
	criteria := Criteria(body)
	if k < 1 || k > len(criteria) {
		return "", fmt.Errorf("criterion %d does not exist: the Issue has %d acceptance criteria", k, len(criteria))
	}
	lines := strings.Split(body, "\n")
	lines[criteria[k-1].line] = setBox(lines[criteria[k-1].line], done)
	return strings.Join(lines, "\n"), nil
}

// TickAll sets every acceptance criterion done and returns the new body.
func TickAll(body string) string {
	lines := strings.Split(body, "\n")
	for _, c := range Criteria(body) {
		lines[c.line] = setBox(lines[c.line], true)
	}
	return strings.Join(lines, "\n")
}

// CriteriaDone counts the ticked criteria and all criteria.
func CriteriaDone(body string) (done, total int) {
	for _, c := range Criteria(body) {
		total++
		if c.Done {
			done++
		}
	}
	return done, total
}

func setBox(line string, done bool) string {
	mark := " "
	if done {
		mark = "x"
	}
	return checkbox.ReplaceAllString(line, "${1}"+mark+"${3}${4}")
}
