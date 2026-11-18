package writer

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// visible is what GitHub and its emails show of a comment.
func visible(comment string) string { return htmlComment.ReplaceAllString(comment, "") }

func fullEvidence() *Evidence {
	return &Evidence{FinalSHA: "4a4180d", CIURL: "https://ci/1", Commands: "go test ./...", Environments: "GitHub Actions CI",
		Criteria: "all 3 met", Artifacts: "None: CI run linked", Documentation: "README", Risks: "Untested on Windows", Boundary: "CI"}
}

func TestCommentsHideTheirRecord(t *testing.T) {
	request := Request{ID: "r1", Action: "claim", IssueID: "I1", Actor: "claude/me/abc", ExpectedFingerprint: "f", Branch: "7/x"}
	body, err := RenderRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseRequest(body)
	if err != nil || !reflect.DeepEqual(got, request) {
		t.Fatalf("request round trip: %+v %v\n%s", got, err, body)
	}
	receipt := Receipt{RequestID: "r1", Fingerprint: "f", Result: "accepted", Detail: "lifecycle state changed to Claimed",
		At: time.Date(2026, 11, 18, 13, 20, 0, 0, time.UTC), State: &ReceiptState{Status: "Claimed", Holder: "claude/me/abc", Branch: "7/x", Expires: "2026-11-18T15:20:24Z"},
		Evidence: fullEvidence()}
	rbody, err := RenderReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	gotReceipt, err := ParseReceipt(rbody)
	if err != nil || !reflect.DeepEqual(gotReceipt, receipt) {
		t.Fatalf("receipt round trip: %+v %v\n%s", gotReceipt, err, rbody)
	}
	for _, comment := range []string{body, rbody} {
		shown := visible(comment)
		if strings.Contains(shown, "{") || strings.Contains(shown, `"request_id"`) || strings.Contains(shown, `"action"`) || strings.Contains(shown, "```") {
			t.Fatalf("visible text holds the record:\n%s", shown)
		}
		if strings.TrimSpace(shown) == "" {
			t.Fatalf("nothing visible:\n%s", comment)
		}
	}
}

func TestEarlierCommentFormStillParses(t *testing.T) {
	request := Request{ID: "r1", Action: "claim", IssueID: "I1", Actor: "agent", ExpectedFingerprint: "f"}
	hash, _ := HashRequest(request)
	payload, _ := json.Marshal(request)
	old := fmt.Sprintf("<!-- quipu-request:v1 id=%s sha256=%s -->\n```json\n%s\n```", request.ID, hash, payload)
	for _, comment := range []string{old, strings.Replace(old, RequestPrefix, legacyRequestPrefix, 1)} {
		if got, err := ParseRequest(comment); err != nil || !reflect.DeepEqual(got, request) {
			t.Fatalf("old request: %+v %v", got, err)
		}
	}
	if _, err := ParseRequest(strings.Replace(old, "sha256=", "sha256=bad", 1)); err == nil {
		t.Fatal("old request with a bad hash parsed")
	}
	receipt := Receipt{RequestID: "r1", Fingerprint: "f", Result: "accepted", Evidence: fullEvidence(), At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	raw, _ := json.Marshal(receipt)
	oldReceipt := fmt.Sprintf("<!-- quipu-receipt:v1 request=%s fingerprint=%s -->\n```json\n%s\n```", receipt.RequestID, receipt.Fingerprint, raw)
	for _, comment := range []string{oldReceipt, strings.Replace(oldReceipt, ReceiptPrefix, legacyReceiptPrefix, 1)} {
		if got, err := ParseReceipt(comment); err != nil || !reflect.DeepEqual(got, receipt) {
			t.Fatalf("old receipt: %+v %v", got, err)
		}
	}
	if !HasAcceptedEvidence([]string{oldReceipt}) {
		t.Fatal("old evidence receipt no longer counts")
	}
}

func TestAgentTextCannotBreakTheRecord(t *testing.T) {
	hostile := "done --> ```json {\"request_id\":\"x\"} ``` " + ReceiptPrefix + " request=x --> <b>@owner</b>"
	evidence := fullEvidence()
	evidence.Criteria, evidence.Risks = hostile, "line one\n- line two -->"
	request := Request{ID: "r2", Action: "evidence.submit", IssueID: "I", Actor: "a", ExpectedFingerprint: "f", PR: "https://github.com/o/r/pull/1", Evidence: evidence}
	body, err := RenderRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseRequest(body); err != nil || !reflect.DeepEqual(got, request) {
		t.Fatalf("request: %+v %v\n%s", got, err, body)
	}
	receipt := Receipt{RequestID: "r2", Fingerprint: "f", Result: "accepted", Evidence: evidence, At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	rbody, err := RenderReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseReceipt(rbody); err != nil || !reflect.DeepEqual(got, receipt) {
		t.Fatalf("receipt: %+v %v\n%s", got, err, rbody)
	}
	for _, comment := range []string{body, rbody} {
		summary := comment[strings.Index(comment, "-->")+3:]
		if strings.Contains(summary, "<") {
			t.Fatalf("summary carries HTML or a marker:\n%s", summary)
		}
		if strings.Contains(summary, "@owner") {
			t.Fatalf("summary would notify someone:\n%s", summary)
		}
	}
}

func TestSummariesSayWhatHappened(t *testing.T) {
	cases := []struct {
		name string
		body func() (string, error)
		want []string
	}{
		{"claim", func() (string, error) {
			return RenderRequest(Request{ID: "1", Action: "claim", IssueID: "I", Actor: "claude/me/abc", ExpectedFingerprint: "f", Branch: "7/x"})
		}, []string{"Claim", "claude/me/abc", "7/x"}},
		{"status change", func() (string, error) {
			return RenderReceipt(Receipt{RequestID: "1", Result: "accepted", Detail: "lifecycle state changed to In progress",
				State: &ReceiptState{Status: "In progress", Holder: "claude/me/abc", Branch: "7/x", Expires: "2026-11-18T15:20:24Z"}})
		}, []string{"In progress", "claude/me/abc", "7/x", "2026-11-18 15:20 UTC"}},
		{"rejection", func() (string, error) {
			return RenderReceipt(Receipt{RequestID: "1", Result: "rejected", Detail: "stale request fingerprint"})
		}, []string{"Rejected", "stale request fingerprint"}},
		{"evidence", func() (string, error) {
			return RenderReceipt(Receipt{RequestID: "1", Result: "accepted", Detail: "evidence recorded", Evidence: fullEvidence(),
				State: &ReceiptState{Status: "Evidence pending"}})
		}, []string{"Evidence pending", "Criteria:** all 3 met", "Commands:** go test ./...", "Documentation:** README",
			"Risks:** Untested on Windows", "Boundary:** CI", "Environments:** GitHub Actions CI", "Final SHA:** 4a4180d",
			"CI:** https://ci/1", "Artifacts:** None: CI run linked"}},
	}
	for _, c := range cases {
		body, err := c.body()
		if err != nil {
			t.Fatal(err)
		}
		shown := visible(body)
		for _, want := range c.want {
			if !strings.Contains(shown, want) {
				t.Errorf("%s: summary lacks %q:\n%s", c.name, want, shown)
			}
		}
	}
}
