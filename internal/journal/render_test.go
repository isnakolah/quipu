package journal

import (
	"os"
	"testing"
)

func TestRenderIsIdempotencyKeyed(t *testing.T) {
	e := Render(Entry{RequestID: "x", Date: "2026-08-08", Issue: "#1"})
	if !HasRequest(e, "x") || HasRequest(e, "y") {
		t.Fatal("bad journal key")
	}
}

func TestGitAuthEnvDoesNotUseTokenURL(t *testing.T) {
	const token = "secret-token"
	for _, value := range gitAuthEnv(token) {
		if value == token || value == "GIT_REMOTE=secret-token" {
			t.Fatalf("raw token placed in git environment: %q", value)
		}
	}
	if _, err := os.Stat(".quipu-wiki-token"); !os.IsNotExist(err) {
		t.Fatalf("wiki token cache was created: %v", err)
	}
}

func TestAppendDoesNotDuplicateReceipt(t *testing.T) {
	entry := Entry{RequestID: "x", Date: "2026-08-08", Issue: "#1"}
	log := Append("", entry)
	if got := Append(log, entry); got != log {
		t.Fatal("duplicate journal entry")
	}
}

func TestHasRequestRecognizesLegacyGfdEntries(t *testing.T) {
	log := "<!-- gfd-journal:v1 request=old -->\n## entry\n"
	if !HasRequest(log, "old") {
		t.Fatal("legacy journal entry not recognized")
	}
	if got := Append(log, Entry{RequestID: "old"}); got != log {
		t.Fatal("legacy entry duplicated")
	}
}
