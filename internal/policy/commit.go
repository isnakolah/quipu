package policy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// IssuePrefix is the commit_format that starts every commit subject with
// its Issue: `#NNN type(scope): summary`, with a `Refs: #N` footer.
const IssuePrefix = "issue-prefix"

var (
	prefixedSubject = regexp.MustCompile(`^#([0-9]+) ([a-z]+(?:\([a-z0-9._/-]+\))?!?: \S.*)$`)
	conventional    = regexp.MustCompile(`^[a-z]+(?:\([a-z0-9._/-]+\))?!?: \S.*$`)
	refsFooter      = regexp.MustCompile(`(?m)^Refs: #([0-9]+)\s*$`)
)

// CommitLines drops what git strips from a message: comment lines and
// everything below the scissors line of `git commit -v`.
func CommitLines(message string) []string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "# ------------------------ >8") {
			break
		}
		if line == "#" || strings.HasPrefix(line, "# ") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// IsMergeSubject reports a merge commit that git or GitHub wrote.
func IsMergeSubject(subject string) bool {
	return strings.HasPrefix(subject, "Merge ")
}

// CheckIssuePrefix validates one commit message in the issue-prefix format
// and returns the Issue it names. issue, when nonzero, must match it.
func CheckIssuePrefix(message string, issue int) (int, error) {
	lines := CommitLines(message)
	subject := ""
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			subject = strings.TrimSpace(line)
			break
		}
	}
	if IsMergeSubject(subject) {
		return issue, nil
	}
	match := prefixedSubject.FindStringSubmatch(subject)
	if match == nil {
		return 0, fmt.Errorf("commit subject %q is not `#NNN type(scope): summary`", subject)
	}
	named, _ := strconv.Atoi(match[1])
	if issue != 0 && named != issue {
		return named, fmt.Errorf("commit subject names #%d, but the work is #%d", named, issue)
	}
	footer := refsFooter.FindStringSubmatch(strings.Join(lines, "\n"))
	if footer == nil {
		return named, fmt.Errorf("commit message lacks a `Refs: #%d` footer line", named)
	}
	if n, _ := strconv.Atoi(footer[1]); n != named {
		return named, fmt.Errorf("commit footer says Refs: #%d, but the subject names #%d", n, named)
	}
	return named, nil
}

// IssuePrefixMessage turns a PR title into the merge commit subject and
// body for Issue number. The title must be `type(scope): summary`,
// optionally already prefixed with the Issue.
func IssuePrefixMessage(number int, title string) (subject, body string, err error) {
	title = strings.TrimSpace(title)
	if match := prefixedSubject.FindStringSubmatch(title); match != nil {
		title = match[2]
	}
	if !conventional.MatchString(title) {
		return "", "", fmt.Errorf("PR title %q is not `type(scope): summary`", title)
	}
	return fmt.Sprintf("#%03d %s", number, title), fmt.Sprintf("Refs: #%d", number), nil
}
