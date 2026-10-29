package policy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Branch is NNN/short-description, with at least three digits so Issues
// numbered 1000 and up still map to a branch.
var Branch = regexp.MustCompile(`^[0-9]{3,}/[a-z0-9][a-z0-9-]*$`)

// IssueRef is the one reference form a PR may use. Closing keywords are not
// accepted: GitHub would close the Issue on merge, before evidence exists.
var IssueRef = regexp.MustCompile(`(?mi)\bRefs\s+#([0-9]+)\b`)

// ClosingRef matches every GitHub closing keyword form, in any case, with
// optional colon, cross-repository references, and Issue URLs.
var ClosingRef = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b:?\s+(?:[\w.-]+/[\w.-]+#[0-9]+|#[0-9]+|https?://github\.com/[\w.-]+/[\w.-]+/issues/[0-9]+)`)

// ReferencedIssues accepts exactly one `Refs #N` reference and no closing
// keyword. Policy needs one unambiguous implementation leaf per branch and PR.
func ReferencedIssues(body string) ([]int, error) {
	if err := NoClosingKeywords(body, "PR body"); err != nil {
		return nil, err
	}
	matches := IssueRef.FindAllStringSubmatch(body, -1)
	if len(matches) != 1 {
		return nil, fmt.Errorf("PR body must reference exactly one Issue as `Refs #N`")
	}
	number, err := strconv.Atoi(matches[0][1])
	if err != nil || number < 1 {
		return nil, fmt.Errorf("PR body has invalid Issue reference")
	}
	return []int{number}, nil
}

// NoClosingKeywords rejects text that would make GitHub close an Issue on
// merge. Quipu closes Issues only after the Writer records evidence.
func NoClosingKeywords(text, where string) error {
	if match := ClosingRef.FindString(text); match != "" {
		return fmt.Errorf("%s contains closing keyword %q; use `Refs #N` so the Writer closes the Issue after evidence", where, match)
	}
	return nil
}

// BranchIssue returns the Issue number a NNN/slug branch maps to.
func BranchIssue(branch string) (int, bool) {
	if !Branch.MatchString(branch) {
		return 0, false
	}
	prefix, _, _ := strings.Cut(branch, "/")
	number, err := strconv.Atoi(prefix)
	return number, err == nil && number > 0
}

func ValidatePR(branch, body string, hasLease, blockersResolved bool) error {
	if !Branch.MatchString(branch) {
		return fmt.Errorf("branch must match NNN/short-description")
	}
	if !hasLease {
		return fmt.Errorf("active lease required")
	}
	if !blockersResolved {
		return fmt.Errorf("unresolved blocker")
	}
	if _, err := ReferencedIssues(body); err != nil {
		return err
	}
	return nil
}
