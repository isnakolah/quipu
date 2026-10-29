package policy

import (
	"strings"
	"testing"
)

func TestCheckIssuePrefix(t *testing.T) {
	ok := []struct {
		message string
		issue   int
	}{
		{"#095 feat(merge): agents merge green PRs\n\nWhy.\n\nRefs: #95\n", 95},
		{"#95 fix: typo\n\nRefs: #95", 95},
		{"#1203 refactor(core/api)!: rename\n\nRefs: #1203\n# Please enter the commit message\n", 0},
		{"Merge branch 'main' into 095/x", 95},
		{"#7 docs: x\n\nRefs: #7\n# ------------------------ >8 ------------------------\nRefs: #8\n", 7},
	}
	for _, tc := range ok {
		if _, err := CheckIssuePrefix(tc.message, tc.issue); err != nil {
			t.Errorf("%q: %v", tc.message, err)
		}
	}
	bad := map[string]string{
		"feat: no prefix\n\nRefs: #95":           "is not `#NNN type(scope): summary`",
		"#95 Feat: capital\n\nRefs: #95":         "is not",
		"#95 feat:no space\n\nRefs: #95":         "is not",
		"#96 feat: other issue\n\nRefs: #96":     "names #96, but the work is #95",
		"#95 feat: no footer":                    "lacks a `Refs: #95` footer",
		"#95 feat: wrong footer\n\nRefs: #9":     "footer says Refs: #9",
		"#95 feat: footer hidden\n\n# Refs: #95": "lacks",
	}
	for message, want := range bad {
		if _, err := CheckIssuePrefix(message, 95); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", message, err, want)
		}
	}
}

func TestIssuePrefixMessage(t *testing.T) {
	subject, body, err := IssuePrefixMessage(95, "feat(merge): agents merge green PRs")
	if err != nil || subject != "#095 feat(merge): agents merge green PRs" || body != "Refs: #95" {
		t.Fatalf("%q %q %v", subject, body, err)
	}
	if subject, _, _ := IssuePrefixMessage(1203, "#1203 fix: x"); subject != "#1203 fix: x" {
		t.Fatalf("an already prefixed title is not prefixed twice: %q", subject)
	}
	if _, _, err := IssuePrefixMessage(95, "Agents merge green PRs"); err == nil {
		t.Fatal("expected a non-conventional title to be refused")
	}
	if _, err := CheckIssuePrefix(subject+"\n\n"+body, 95); err != nil {
		t.Fatalf("the generated merge message must pass its own check: %v", err)
	}
}
