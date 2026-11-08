package main

// `quipu done N --evidence-file FILE` reads evidence from a file, so none of it
// rides on the command line where a harness that inspects the whole line can
// mistake it for commands. A flag given as well overrides its field.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/isnakolah/quipu/internal/writer"
)

// evidenceTemplate is what `quipu done --evidence-template` prints. Comments
// are hints: a section left as only its hint counts as missing.
const evidenceTemplate = `## Criteria
<!-- each acceptance criterion and its result, one per line -->

## Commands
<!-- the exact commands you ran to verify, one per line -->

## Documentation
<!-- the docs you changed, or None: reason -->

## Risks
<!-- what could still go wrong, or None -->

## Boundary
<!-- local, CI, target host, provider, staging, production, or release -->

## Environments
<!-- optional; defaults to GitHub Actions CI -->

## Artifacts
<!-- optional URLs of logs or screenshots; defaults to None: CI run linked -->
`

// evidenceSection is one field of an evidence file: its Markdown heading,
// JSON key, flag, and whether a file must give it.
type evidenceSection struct {
	heading, key, flag string
	required           bool
	field              func(*writer.Evidence) *string
}

var evidenceSections = []evidenceSection{
	{"Criteria", "criteria", "criteria", true, func(e *writer.Evidence) *string { return &e.Criteria }},
	{"Commands", "commands", "commands", true, func(e *writer.Evidence) *string { return &e.Commands }},
	{"Documentation", "documentation", "documentation", true, func(e *writer.Evidence) *string { return &e.Documentation }},
	{"Risks", "risks", "risks", true, func(e *writer.Evidence) *string { return &e.Risks }},
	{"Boundary", "boundary", "boundary", true, func(e *writer.Evidence) *string { return &e.Boundary }},
	{"Environments", "environments", "environments", false, func(e *writer.Evidence) *string { return &e.Environments }},
	{"Artifacts", "artifacts", "artifacts", false, func(e *writer.Evidence) *string { return &e.Artifacts }},
	{"CI URL", "ci_url", "ci-url", false, func(e *writer.Evidence) *string { return &e.CIURL }},
}

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// parseEvidenceFile reads Markdown sections or a JSON object into evidence
// fields, keyed by JSON key. Unknown sections are errors, so a typo is caught.
func parseEvidenceFile(raw []byte) (map[string]string, error) {
	values := map[string]string{}
	known := map[string]string{}
	for _, s := range evidenceSections {
		known[strings.ToLower(s.heading)] = s.key
		known[s.key] = s.key
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '{' {
		var object map[string]string
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return nil, fmt.Errorf("evidence JSON: %v", err)
		}
		for key, value := range object {
			canonical, ok := known[strings.ToLower(key)]
			if !ok {
				return nil, fmt.Errorf("evidence JSON has unknown key %q", key)
			}
			values[canonical] = oneLine(value)
		}
		return values, nil
	}
	current := ""
	var body []string
	flush := func() {
		if current != "" {
			values[current] = oneLine(strings.Join(body, "\n"))
		}
		body = nil
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if heading, ok := strings.CutPrefix(line, "## "); ok {
			flush()
			canonical, found := known[strings.ToLower(strings.TrimSpace(heading))]
			if !found {
				return nil, fmt.Errorf("evidence file has unknown section %q", "## "+strings.TrimSpace(heading))
			}
			current = canonical
			continue
		}
		if current != "" {
			body = append(body, line)
		}
	}
	flush()
	return values, nil
}

// oneLine folds a section onto one line, as receipts and the journal table
// keep it: comments and list markers go, lines join with "; ".
func oneLine(text string) string {
	var parts []string
	for _, line := range strings.Split(htmlComment.ReplaceAllString(text, ""), "\n") {
		line = strings.TrimSpace(line)
		for _, marker := range []string{"- [x] ", "- [X] ", "- [ ] ", "- ", "* "} {
			line = strings.TrimPrefix(line, marker)
		}
		if line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, "; ")
}

// evidenceFlags registers the evidence fields of `quipu done` on fs.
func evidenceFlags(fs *flag.FlagSet, e *writer.Evidence) {
	fs.StringVar(&e.Criteria, "criteria", "", "acceptance-criteria result")
	fs.StringVar(&e.Commands, "commands", "", "exact verification commands")
	fs.StringVar(&e.Documentation, "documentation", "", "documentation changes; None: reason")
	fs.StringVar(&e.Risks, "risks", "", "known residual risks; None")
	fs.StringVar(&e.Boundary, "boundary", "", "local|CI|target host|provider|staging|production|release")
	fs.StringVar(&e.Environments, "environments", defaultEnvironments, "verified environments")
	fs.StringVar(&e.Artifacts, "artifacts", defaultArtifacts, "artifact, screenshot, or log URLs; None: reason")
	fs.StringVar(&e.CIURL, "ci-url", "", "CI run URL; read from the PR checks when omitted")
}

// applyEvidenceFile fills e from path, except fields whose flag was given.
func applyEvidenceFile(e *writer.Evidence, path string, fs *flag.FlagSet) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return flowErr("evidence-missing", "cannot read evidence file: %v", err)
	}
	values, err := parseEvidenceFile(raw)
	if err != nil {
		return flowErr("evidence-missing", "%s: %v", path, err)
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var missing []string
	for _, s := range evidenceSections {
		if given[s.flag] {
			continue
		}
		if value := values[s.key]; value != "" {
			*s.field(e) = value
		} else if s.required {
			missing = append(missing, "## "+s.heading)
		}
	}
	if len(missing) > 0 {
		return flowErr("evidence-missing", "%s has no %s; fill it in (quipu done --evidence-template prints every section)", path, strings.Join(missing, ", "))
	}
	return nil
}
