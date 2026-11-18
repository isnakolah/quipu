package model

// Decisions and Epics are decided by the owner, not implemented, so they carry
// contracts of their own instead of work:v1. A Decision follows MADR
// (https://adr.github.io/madr/): the question, its context and drivers, the
// options with their consequences, and a recommendation kept apart from the
// owner's decision. An Epic states the outcome and how anyone will know it is
// reached. Each contract is accepted in two shapes: written with its marker and
// "## " sections, or rendered by the matching GitHub Issue form, which cannot
// add a marker and renders each field as a "### " heading.

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	DecisionMarker = "<!-- decision:v1 -->"
	EpicMarker     = "<!-- epic:v1 -->"
)

var (
	RequiredDecisionSections = []string{"Question", "Context", "Decision drivers", "Considered options", "Recommended option", "Required approver", "Blocks"}
	RequiredEpicSections     = []string{"Outcome", "Non-goals", "Standing constraints", "Exit criteria", "Expected children", "Owner approval"}
)

// ContractName names the contract an Issue of kind must satisfy.
func ContractName(kind string) string {
	switch strings.ToLower(kind) {
	case "decision":
		return "decision:v1"
	case "epic":
		return "epic:v1"
	}
	return "work:v1"
}

// ValidateContract checks body against the contract of its kind: decision:v1
// for a Decision, epic:v1 for an Epic, and work:v1 for every other kind, with
// a Defect's reproduction on top.
func ValidateContract(kind, body string) error {
	switch strings.ToLower(kind) {
	case "decision":
		return ValidateDecisionContract(body)
	case "epic":
		return ValidateEpicContract(body)
	}
	return ValidateWorkContractFor(kind, body)
}

// boardContractProblem is the contract check for an Issue already on the board.
// Issues filed before their kind had a contract keep the check they had: a
// Decision written as work:v1 is still checked as work:v1, and an Epic written
// to no contract is not checked, as before. Everything else meets its own.
func boardContractProblem(kind, body string) error {
	switch strings.ToLower(kind) {
	case "decision":
		if strings.Contains(body, WorkMarker) && !declaresContract(body, DecisionMarker, RequiredDecisionSections) {
			return ValidateWorkContract(body)
		}
	case "epic":
		if !declaresContract(body, EpicMarker, RequiredEpicSections) {
			return nil
		}
	}
	return ValidateContract(kind, body)
}

// ContractTemplate is the empty contract for kind, to fill in. Left unfilled,
// it fails validation.
func ContractTemplate(kind string) (string, error) {
	switch strings.ToLower(kind) {
	case "decision":
		return DecisionMarker + `

## Question

<!-- The one thing the owner must decide, as a question ending in "?". -->

## Context

<!-- What makes the decision necessary now, and what is already known. -->

## Decision drivers

- <!-- A force or constraint any answer must weigh. -->

## Considered options

### <!-- First option -->

<!-- What this option is. -->

Consequences:
- <!-- Good, because ... / Bad, because ... -->

### <!-- Second option -->

<!-- What this option is. -->

Consequences:
- <!-- Good, because ... / Bad, because ... -->

## Recommended option

<!-- The option you recommend and why. A recommendation only: the owner decides, and records the decision when closing this Decision. -->

## Required approver

<!-- @login of the owner who must decide. -->

## Blocks

- <!-- #N, an Issue waiting on this decision, or "None: reason". -->
`, nil
	case "epic":
		return EpicMarker + `

## Outcome

<!-- The long-lived result this Epic delivers. -->

## Non-goals

- <!-- What this Epic deliberately does not do. -->

## Standing constraints

- <!-- Rules every child Issue must keep. -->

## Exit criteria

- [ ] <!-- An observable condition that says the outcome is reached. -->

## Expected children

- <!-- The Stories, Tasks, Decisions or Gates expected under this Epic. -->

## Owner approval

<!-- Who approved this Epic, when, and where (an approval comment URL). -->
`, nil
	}
	return "", fmt.Errorf("--template prints the decision and epic contracts, not %q; a work:v1 body is in .github/ISSUE_TEMPLATE/work.yml", kind)
}

// ValidateDecisionContract checks a decision:v1 body: every section filled, a
// Question that ends in "?", and at least two considered options, each under
// its own heading with its consequences.
func ValidateDecisionContract(body string) error {
	sections, err := contractSections(body, DecisionMarker, RequiredDecisionSections)
	if err != nil {
		return err
	}
	var problems []string
	for _, name := range RequiredDecisionSections {
		if meaningful(sections[name]) == "" {
			problems = append(problems, fmt.Sprintf("section %q is empty", name))
		}
	}
	if question := meaningful(sections["Question"]); question != "" && !strings.HasSuffix(question, "?") {
		problems = append(problems, `the Question must end with "?"`)
	}
	problems = append(problems, optionProblems(sections["Considered options"])...)
	return joinProblems(problems)
}

// ValidateEpicContract checks an epic:v1 body: every section filled, and Exit
// criteria written as checkboxes.
func ValidateEpicContract(body string) error {
	sections, err := contractSections(body, EpicMarker, RequiredEpicSections)
	if err != nil {
		return err
	}
	var problems []string
	for _, name := range RequiredEpicSections {
		if meaningful(sections[name]) == "" {
			problems = append(problems, fmt.Sprintf("section %q is empty", name))
		}
	}
	if meaningful(sections["Exit criteria"]) != "" && !exitCheckbox.MatchString(stripComments(sections["Exit criteria"])) {
		problems = append(problems, `Exit criteria must be checkboxes, one "- [ ] criterion" per line`)
	}
	return joinProblems(problems)
}

var (
	headingLine  = regexp.MustCompile(`^(#{2,6})\s+(.*?)\s*#*\s*$`)
	htmlComment  = regexp.MustCompile(`(?s)<!--.*?-->`)
	exitCheckbox = regexp.MustCompile(`(?m)^\s*[-*]\s+\[[ xX]\]\s+\S`)
	placeholder  = regexp.MustCompile(`(?m)^\s*(?:[-*](?:\s+\[[ xX]\])?|[-*]\s+#|_No response_)?\s*$`)
	consequences = regexp.MustCompile(`(?im)^[\s#*>_-]*consequences\b[*_]*\s*:?[*_]*\s*(.*)$`)
	madrBullet   = regexp.MustCompile(`(?im)^\s*[-*]\s+(?:good|bad|neutral),\s+because\s+\S`)
)

// declaresContract reports whether body is written to a contract: it carries
// the marker, or it is the Issue form's rendering, whose first field is a
// "### " heading.
func declaresContract(body, marker string, sections []string) bool {
	if strings.Contains(body, marker) {
		return true
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "### "+sections[0]) {
			return true
		}
	}
	return false
}

// contractSections splits body into the named sections. A section starts at a
// "## Name" or "### Name" line and runs to the next such line; headings with
// other names belong to the section they are in.
func contractSections(body, marker string, names []string) (map[string]string, error) {
	if !declaresContract(body, marker, names) {
		return nil, fmt.Errorf("missing %s (or a body rendered by the Issue form)", marker)
	}
	known := make(map[string]string, len(names))
	for _, name := range names {
		known[strings.ToLower(name)] = name
	}
	sections := map[string]string{}
	seen := map[string]bool{}
	current := ""
	for _, line := range strings.Split(body, "\n") {
		if m := headingLine.FindStringSubmatch(line); m != nil && len(m[1]) <= 3 {
			if name, ok := known[strings.ToLower(m[2])]; ok {
				current, seen[name] = name, true
				continue
			}
		}
		if current != "" {
			sections[current] += line + "\n"
		}
	}
	var missing []string
	for _, name := range names {
		if !seen[name] {
			missing = append(missing, fmt.Sprintf("missing required section %q", name))
		}
	}
	return sections, joinProblems(missing)
}

// optionProblems checks the Considered options section: two or more options,
// each a heading with a name, and each with its consequences.
func optionProblems(section string) []string {
	if meaningful(section) == "" {
		return nil
	}
	type option struct{ name, text string }
	var options []option
	for _, line := range strings.Split(section, "\n") {
		if m := headingLine.FindStringSubmatch(line); m != nil && !consequences.MatchString(m[2]) {
			options = append(options, option{name: strings.TrimSpace(stripComments(m[2]))})
			continue
		}
		if len(options) > 0 {
			options[len(options)-1].text += line + "\n"
		}
	}
	if len(options) < 2 {
		return []string{fmt.Sprintf("Considered options lists %d option(s) under headings; a Decision needs at least two, each under its own heading with its consequences", len(options))}
	}
	var problems []string
	for i, o := range options {
		label := fmt.Sprintf("option %d", i+1)
		if o.name == "" {
			problems = append(problems, label+" has no name in its heading")
		} else {
			label = fmt.Sprintf("option %q", o.name)
		}
		if !hasConsequences(o.text) {
			problems = append(problems, label+` has no consequences; add "Consequences:" with what follows from choosing it`)
		}
	}
	return problems
}

// hasConsequences reports whether an option's text states its consequences:
// a "Consequences" line with something after it, or MADR's "Good, because"
// and "Bad, because" bullets.
func hasConsequences(text string) bool {
	text = stripComments(text)
	if madrBullet.MatchString(text) {
		return true
	}
	loc := consequences.FindStringSubmatchIndex(text)
	return loc != nil && meaningful(text[loc[2]:]) != ""
}

func stripComments(text string) string { return htmlComment.ReplaceAllString(text, "") }

// meaningful is what a section says once comments, empty list items, empty
// checkboxes, and an Issue form's "_No response_" are taken out.
func meaningful(text string) string {
	return strings.TrimSpace(placeholder.ReplaceAllString(stripComments(text), ""))
}

func joinProblems(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}
