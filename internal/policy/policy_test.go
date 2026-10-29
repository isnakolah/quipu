package policy

import "testing"

func TestValidatePR(t *testing.T) {
	if err := ValidatePR("001/bootstrap", "Refs #1", true, true); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePR("main", "Refs #1", true, true); err == nil {
		t.Fatal("wanted error")
	}
}

func TestReferencedIssuesRequiresExactlyOne(t *testing.T) {
	issues, err := ReferencedIssues("Refs #7")
	if err != nil || len(issues) != 1 || issues[0] != 7 {
		t.Fatalf("issues=%v err=%v", issues, err)
	}
	if _, err := ReferencedIssues("Refs #7\nRefs #8"); err == nil {
		t.Fatal("expected multiple Issue rejection")
	}
}

func TestReferencedIssuesRejectsClosingKeywords(t *testing.T) {
	for _, body := range []string{
		"Closes #7", "Refs #7\ncloses #8", "Refs #7\nfixed: #8", "Refs #7 Resolves owner/repo#9",
		"Refs #7\nfix https://github.com/o/r/issues/3",
	} {
		if _, err := ReferencedIssues(body); err == nil {
			t.Fatalf("expected rejection for %q", body)
		}
	}
	for _, body := range []string{"Refs #7", "refs #7 — fixes the flaky test", "Refs #7\nThis closes the gap in #docs"} {
		if _, err := ReferencedIssues(body); err != nil {
			t.Fatalf("%q: %v", body, err)
		}
	}
}

func TestBranchIssueAcceptsFourDigitIssues(t *testing.T) {
	if n, ok := BranchIssue("1042/big-repo"); !ok || n != 1042 {
		t.Fatalf("got %d %v", n, ok)
	}
	if n, ok := BranchIssue("007/x"); !ok || n != 7 {
		t.Fatalf("got %d %v", n, ok)
	}
	if _, ok := BranchIssue("feature/x"); ok {
		t.Fatal("non-Quipu branch accepted")
	}
}
