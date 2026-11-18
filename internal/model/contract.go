package model

// A work contract is the body of a Story, Task, Defect, or Contract. Quipu
// judges what it says, not only which headings it has: a contract that is empty
// or can't be verified is refused (a hard rule), and weak wording is listed as
// warnings that never stop the Issue being filed.

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const WorkMarker = "<!-- work:v1 -->"

var RequiredWorkSections = []string{"## Outcome", "## Scope", "## Acceptance criteria", "## Required evidence", "## Documentation impact", "## Source and relationships"}

// DefectSections are the sections a Defect needs on top of the work contract.
var DefectSections = []string{"## Steps to reproduce", "## Expected", "## Actual", "## Environment"}

// Limits behind the warnings.
const (
	MaxCriteria      = 7
	MaxCriterionLen  = 200
	MaxTitleLen      = 70
	compoundAndCount = 3
)

// VagueWords are requirement smells (ISO 29148; Femmer et al.): words a
// stranger can't check. They raise warnings, never refusals, because the
// detection is imprecise.
var VagueWords = []string{"should work", "properly", "correctly", "as needed", "if possible", "as appropriate", "etc", "and/or", "user-friendly", "fast", "easy", "robust", "significant", "sufficient", "minimal", "improve", "better"}

var vaguePatterns = func() []*regexp.Regexp {
	forms := map[string]string{
		"etc":           `etc\b\.?`,
		"and/or":        `and/or\b`,
		"user-friendly": `user[- ]friendly\b`,
		"fast":          `fast(er|est)?\b`,
		"easy":          `eas(y|ier|iest|ily)\b`,
		"robust":        `robust(ly|ness)?\b`,
		"significant":   `significant(ly)?\b`,
		"sufficient":    `sufficient(ly)?\b`,
		"minimal":       `minimal(ly)?\b`,
		"improve":       `improv(e|es|ed|ing|ement|ements)\b`,
	}
	patterns := make([]*regexp.Regexp, len(VagueWords))
	for i, word := range VagueWords {
		form, ok := forms[word]
		if !ok {
			form = regexp.QuoteMeta(word) + `\b`
		}
		patterns[i] = regexp.MustCompile(`(?i)\b` + form)
	}
	return patterns
}()

var (
	htmlComment  = regexp.MustCompile(`(?s)<!--.*?-->`)
	codeSpan     = regexp.MustCompile("`[^`\n]*`")
	quoted       = regexp.MustCompile(`"[^"\n]*"|“[^”\n]*”`)
	bulletPrefix = regexp.MustCompile(`^(?:[-*+](?:\s*\[[ xX]?\])?|\d+[.)])(?:\s+|$)`)
	numberedItem = regexp.MustCompile(`^\s*\d+[.)]\s+\S`)
	sourceRef    = regexp.MustCompile(`https?://|#[0-9]+\b|\b[0-9]{4}-[0-9]{2}-[0-9]{2}\b`)
	// dependsLead is a line that opens by naming a dependency, or prose that
	// names an Issue it waits for (dependsIssue): "Depends on the brief Issue", "blocked by #12".
	dependsLead    = regexp.MustCompile(`(?i)^(depends on|blocked by|blocked on|waits? (?:on|for))\b`)
	dependsIssue   = regexp.MustCompile(`(?i)\b(depends on|blocked by|blocked on|waits? (?:on|for)|after) (?:[\w.-]+/[\w.-]+)?#[0-9]+`)
	activityOpener = regexp.MustCompile(`(?i)^(implement|refactor)\b`)
)

// placeholderText is template text that says nothing about this work.
var placeholderText = map[string]bool{
	"": true, "tbd": true, "todo": true, "tbc": true, "...": true, "…": true, "x": true, "?": true,
	"none: explain specific reason":                      true,
	"native parent and blocker links are authoritative.": true,
	"native parent and blocker links are authoritative":  true,
}

// section is one "## Heading" of a body and the lines under it.
type section struct {
	heading string
	lines   []string
}

// contractSections splits a body into its "## " sections, keyed by the
// heading's lower-case text. HTML comments, the marker included, are dropped.
func contractSections(body string) map[string]section {
	body = htmlComment.ReplaceAllString(strings.ReplaceAll(body, "\r\n", "\n"), "")
	sections := map[string]section{}
	key := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ") {
			key = ""
			if text, ok := strings.CutPrefix(trimmed, "## "); ok {
				key = strings.ToLower(strings.TrimSpace(text))
				if _, seen := sections[key]; !seen {
					sections[key] = section{heading: "## " + strings.TrimSpace(text)}
				}
			}
			continue
		}
		if key != "" {
			s := sections[key]
			s.lines = append(s.lines, line)
			sections[key] = s
		}
	}
	return sections
}

// itemText is a line without its bullet, checkbox or number.
func itemText(line string) string {
	text := strings.TrimSpace(line)
	for {
		stripped := strings.TrimSpace(bulletPrefix.ReplaceAllString(text, ""))
		if stripped == text {
			return text
		}
		text = stripped
	}
}

// placeholder reports whether a line says nothing: a bare bullet or checkbox,
// TBD, TODO, "...", "x", or the template's own text.
func placeholder(line string) bool {
	text := itemText(line)
	lower := strings.ToLower(text)
	if placeholderText[lower] || placeholderText[strings.TrimRight(lower, ".!")] {
		return true
	}
	// The template's choice list, such as "local | CI | target host | …".
	return strings.Count(text, "|") >= 2 && strings.Contains(lower, "local") && strings.Contains(lower, "ci")
}

// realLines are a section's lines that say something.
func realLines(lines []string) []string {
	var out []string
	for _, line := range lines {
		if !placeholder(line) {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// scopeParts splits the Scope section into its Includes and Excludes items.
// ok is false when either label is missing.
func scopeParts(lines []string) (includes, excludes []string, ok bool) {
	var part *[]string
	seenIn, seenEx := false, false
	for _, line := range lines {
		label := strings.Trim(strings.TrimSpace(line), "*_ ")
		lower := strings.ToLower(label)
		switch {
		case strings.HasPrefix(lower, "includes:"):
			part, seenIn = &includes, true
			line = strings.TrimSpace(label[len("includes:"):])
		case strings.HasPrefix(lower, "excludes:"):
			part, seenEx = &excludes, true
			line = strings.TrimSpace(label[len("excludes:"):])
		}
		if part != nil && !placeholder(line) {
			*part = append(*part, strings.TrimSpace(line))
		}
	}
	return includes, excludes, seenIn && seenEx
}

// realCriteria are the acceptance-criteria checkboxes that say something.
func realCriteria(body string) []string {
	var out []string
	for _, c := range Criteria(body) {
		if !placeholder(c.Text) {
			out = append(out, c.Text)
		}
	}
	return out
}

func emptySection(heading string) error {
	return fmt.Errorf("section %q is empty: write what it asks for (placeholders such as \"-\", \"TBD\", \"TODO\", \"...\" and the template's text do not count)", heading)
}

// ValidateWorkContract applies the hard rules every work contract meets. The
// error names the first rule the body breaks.
func ValidateWorkContract(body string) error {
	if !strings.Contains(body, WorkMarker) {
		return fmt.Errorf("missing %s", WorkMarker)
	}
	sections := contractSections(body)
	for _, heading := range RequiredWorkSections {
		if _, ok := sections[strings.ToLower(strings.TrimPrefix(heading, "## "))]; !ok {
			return fmt.Errorf("missing required section %q", heading)
		}
	}
	get := func(heading string) []string {
		return sections[strings.ToLower(strings.TrimPrefix(heading, "## "))].lines
	}
	if len(realLines(get("## Outcome"))) == 0 {
		return emptySection("## Outcome")
	}
	includes, excludes, ok := scopeParts(get("## Scope"))
	if !ok {
		return fmt.Errorf("scope requires Includes and Excludes")
	}
	if len(includes) == 0 {
		return fmt.Errorf("scope Includes: lists nothing: name what this work changes")
	}
	realExcludes := 0
	for _, item := range excludes {
		if strings.ToLower(strings.TrimRight(itemText(item), ".")) != "none" {
			realExcludes++
		}
	}
	if realExcludes == 0 {
		return fmt.Errorf("scope Excludes: has no real item: name at least one thing this work leaves out (\"None\" does not count)")
	}
	if len(realCriteria(body)) == 0 {
		return fmt.Errorf("section \"## Acceptance criteria\" has no checkbox criteria: list each as \"- [ ] ...\"")
	}
	if len(realLines(get("## Required evidence"))) == 0 {
		return emptySection("## Required evidence")
	}
	if err := checkEvidence(get("## Required evidence")); err != nil {
		return err
	}
	if len(realLines(get("## Documentation impact"))) == 0 {
		return emptySection("## Documentation impact")
	}
	if len(realLines(get("## Source and relationships"))) == 0 {
		return emptySection("## Source and relationships")
	}
	return nil
}

// checkEvidence needs a verification boundary with a value.
func checkEvidence(lines []string) error {
	for _, line := range lines {
		if value, ok := cutPrefixFold(itemText(line), "Verification boundary:"); ok {
			if placeholder(value) {
				return fmt.Errorf("verification boundary has no value: pick one of local, CI, target host, provider, staging, production, release")
			}
			return nil
		}
	}
	return fmt.Errorf("missing verification boundary")
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// ValidateWorkContractFor applies the hard rules for an Issue of kind (a
// label value such as "defect" or a Project Kind such as "Defect"): a Defect
// also needs its reproduction.
func ValidateWorkContractFor(kind, body string) error {
	if err := ValidateWorkContract(body); err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimPrefix(strings.ToLower(kind), "kind:"), "defect") {
		return ValidateDefectContract(body)
	}
	return nil
}

// ValidateDefectContract needs numbered Steps to reproduce, Expected, Actual
// and Environment sections, and Expected and Actual that differ.
func ValidateDefectContract(body string) error {
	sections := contractSections(body)
	find := func(heading string) (section, bool) {
		name := strings.ToLower(strings.TrimPrefix(heading, "## "))
		if s, ok := sections[name]; ok {
			return s, true
		}
		// "## Expected behaviour" and "## Actual result" count too.
		for key, s := range sections {
			if strings.HasPrefix(key, name+" ") {
				return s, true
			}
		}
		return section{}, false
	}
	steps, ok := find("## Steps to reproduce")
	if !ok {
		return fmt.Errorf("a Defect needs a \"## Steps to reproduce\" section with numbered steps (1. 2. 3.)")
	}
	numbered := 0
	for _, line := range steps.lines {
		if numberedItem.MatchString(line) && !placeholder(line) {
			numbered++
		}
	}
	if numbered == 0 {
		return fmt.Errorf("a Defect's \"## Steps to reproduce\" needs numbered steps (1. 2. 3.)")
	}
	texts := map[string]string{}
	for _, heading := range DefectSections[1:] {
		s, ok := find(heading)
		if !ok {
			return fmt.Errorf("a Defect needs a %q section", heading)
		}
		lines := realLines(s.lines)
		if len(lines) == 0 {
			return emptySection(s.heading)
		}
		texts[heading] = normalized(lines)
	}
	if texts["## Expected"] == texts["## Actual"] {
		return fmt.Errorf("a Defect's Expected and Actual say the same thing: Actual is what happens instead")
	}
	return nil
}

func normalized(lines []string) string {
	words := make([]string, 0, len(lines))
	for _, line := range lines {
		words = append(words, strings.Fields(strings.ToLower(itemText(line)))...)
	}
	return strings.TrimRight(strings.Join(words, " "), ".")
}

// prose is text with code spans and quoted phrases taken out, so words a
// contract mentions rather than uses raise no warning.
func prose(text string) string {
	return quoted.ReplaceAllString(codeSpan.ReplaceAllString(text, ""), "")
}

func vagueIn(text string) []string {
	text = prose(text)
	var found []string
	for i, pattern := range vaguePatterns {
		if pattern.MatchString(text) {
			found = append(found, fmt.Sprintf("%q", VagueWords[i]))
		}
	}
	return found
}

// WorkContractWarnings lists weak wording in a work contract: things worth
// fixing that never stop the Issue being filed. title may be empty when the
// caller doesn't have it. Native blocker links are not checked here: prose
// that names a dependency warns on its own.
func WorkContractWarnings(title, body string) []string {
	var warnings []string
	add := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	if n := utf8.RuneCountInString(strings.TrimSpace(title)); n > MaxTitleLen {
		add("title is %d characters; keep it to %d or fewer", n, MaxTitleLen)
	}
	sections := contractSections(body)
	criteria := realCriteria(body)
	if len(criteria) > MaxCriteria {
		add("%d acceptance criteria; more than %d usually means the Issue should be split", len(criteria), MaxCriteria)
	}
	for i, criterion := range criteria {
		if n := utf8.RuneCountInString(criterion); n > MaxCriterionLen {
			add("criterion %d is %d characters; keep each to %d or fewer", i+1, n, MaxCriterionLen)
		}
		text := prose(criterion)
		if strings.Contains(text, ";") || strings.Count(strings.ToLower(text), " and ") >= compoundAndCount {
			add("criterion %d joins several outcomes (\";\" or a chain of \"and\"); give each outcome its own checkbox", i+1)
		}
		if words := vagueIn(criterion); len(words) > 0 {
			add("criterion %d uses vague wording %s; say what a stranger can check", i+1, strings.Join(words, ", "))
		}
	}
	outcome := realLines(sections["outcome"].lines)
	if len(outcome) > 0 {
		if words := vagueIn(strings.Join(outcome, " ")); len(words) > 0 {
			add("Outcome uses vague wording %s; say what a stranger can check", strings.Join(words, ", "))
		}
		if opener := activityOpener.FindString(strings.TrimLeft(itemText(outcome[0]), "*_ ")); opener != "" {
			add("Outcome opens with %q; describe the result once the work is done, not the activity", opener)
		}
	}
	includes, _, _ := scopeParts(sections["scope"].lines)
	if len(criteria) > 0 && len(includes) > len(criteria) {
		add("Scope Includes has %d items but only %d acceptance criteria; check each included item with a criterion", len(includes), len(criteria))
	}
	if source := realLines(sections["source and relationships"].lines); len(source) > 0 && !sourceRef.MatchString(strings.Join(source, " ")) {
		add("Source and relationships has no link, #N or date; say where the work came from")
	}
	if m := dependencyProse(body); m != "" {
		add("the body says %q, but Quipu orders work only by native blocker links; add one with quipu issue add-blocker (this warning reads the prose alone)", m)
	}
	return warnings
}

// dependencyProse returns the first words that name a dependency in prose.
func dependencyProse(body string) string {
	for _, line := range strings.Split(prose(htmlComment.ReplaceAllString(body, "")), "\n") {
		text := itemText(line)
		if m := dependsIssue.FindString(text); m != "" {
			return m
		}
		if m := dependsLead.FindString(text); m != "" {
			return m
		}
	}
	return ""
}

func WorkBody(outcome, includes, excludes, criteria, evidence, docs, source, boundary string) string {
	return fmt.Sprintf(`%s
## Outcome

%s

## Scope
Includes:
- %s

Excludes:
- %s

## Acceptance criteria
- [ ] %s

## Required evidence
- %s
- Verification boundary: %s

## Documentation impact
%s

## Source and relationships
- %s
- Native parent and blocker links are authoritative.
`, WorkMarker, outcome, includes, excludes, criteria, evidence, boundary, docs, source)
}

// DefectBody appends a Defect's reproduction to a work contract.
func DefectBody(contract string, steps []string, expected, actual, environment string) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(contract, "\n"))
	b.WriteString("\n\n## Steps to reproduce\n")
	for i, step := range steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, step)
	}
	fmt.Fprintf(&b, "\n## Expected\n%s\n\n## Actual\n%s\n\n## Environment\n%s\n", expected, actual, environment)
	return b.String()
}
