package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestFlowErrorsPrintAsJSONWithNextStep(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := fmt.Errorf("wrapped: %w", flowErr("held-by", "#3 is held by %q", "x"))
	printError(err, []string{"start", "3", "--json"}, &stdout, &stderr)
	var decoded struct {
		Error flowError `json:"error"`
	}
	if json.Unmarshal(stdout.Bytes(), &decoded) != nil || decoded.Error.Code != "held-by" || decoded.Error.Next == "" || stderr.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	stdout.Reset()
	printError(errors.New("plain"), []string{"status"}, &stdout, &stderr)
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "quipu: plain") {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	stderr.Reset()
	printError(errReported, []string{"doctor", "--json"}, &stdout, &stderr)
	if stdout.Len()+stderr.Len() != 0 {
		t.Fatal("an already reported failure must print nothing more")
	}
}

func TestEveryFlowErrorCodeHasGuidance(t *testing.T) {
	for _, kind := range flowErrors {
		if kind.Code == "" || kind.When == "" || kind.Next == "" {
			t.Fatalf("incomplete %+v", kind)
		}
	}
	for detail, want := range map[string]string{
		"fingerprint mismatch":                       "rejected-stale",
		"issue already leased by x until y":          "rejected-taken",
		"unresolved blocker #4":                      "rejected-blocked",
		"Done requires generated Wiki journal":       "rejected-journal",
		"actor bob not authorized by repository":     "rejected-unauthorized",
		`work is held by "a", not "b"`:               "rejected-holder",
		"invalid lifecycle transition Ready -> Done": "rejected",
	} {
		if got := rejectionCode(detail); got != want {
			t.Fatalf("rejectionCode(%q) = %q, want %q", detail, got, want)
		}
		if flowErr(want, "x").Next == "" {
			t.Fatalf("code %s has no next step", want)
		}
	}
}
