package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/writer"
)

// fakeDigestIssues is GitHub's open Issues labelled quipu:digest.
type fakeDigestIssues struct {
	issues  []digestIssue
	created int
	updated []int
	pinned  []int
	pinErr  error
}

func (f *fakeDigestIssues) ops(t *testing.T) digestIssueOps {
	return digestIssueOps{
		list: func() ([]digestIssue, error) { return append([]digestIssue{}, f.issues...), nil },
		create: func(body string) (digestIssue, error) {
			f.created++
			issue := digestIssue{Number: 500 + f.created, NodeID: "I_x", Body: body}
			issue.User.Login = writer.WriterLogin
			f.issues = append(f.issues, issue)
			return issue, nil
		},
		update: func(number int, body string) error {
			for i := range f.issues {
				if f.issues[i].Number == number {
					f.issues[i].Body = body
					f.updated = append(f.updated, number)
					return nil
				}
			}
			t.Fatalf("update of #%d, which is not listed", number)
			return nil
		},
		pin: func(issue digestIssue) error {
			f.pinned = append(f.pinned, issue.Number)
			return f.pinErr
		},
		build: func() digestReport {
			src, owner := digestFixture(t)
			return collectDigest("o/r", src, owner, digestNow.Add(-digestWindow), digestNow)
		},
	}
}

func TestDigestIssueIsUpdatedInPlace(t *testing.T) {
	f := &fakeDigestIssues{}
	// An Issue someone else opened with the label is not the Writer's digest.
	forged := digestIssue{Number: 7, Body: "<!-- quipu-digest:v1 at=2026-11-08T17:59:00Z -->"}
	forged.User.Login = "agent-login"
	f.issues = []digestIssue{forged}
	ops := f.ops(t)

	did, err := refreshDigestIssue(ops, digestNow)
	if err != nil || did != "created" || f.created != 1 || len(f.pinned) != 1 || f.pinned[0] != 501 {
		t.Fatalf("first pass: %s %v, created %d, pinned %v", did, err, f.created, f.pinned)
	}
	body := f.issues[1].Body
	if !strings.Contains(body, "Merged in the last 24h: 2: #10, #11") || !isDigestIssue([]string{digestLabel}, body) {
		t.Fatalf("body:\n%s", body)
	}
	if did, err = refreshDigestIssue(ops, digestNow.Add(59*time.Minute)); err != nil || did != "fresh" || len(f.updated) != 0 {
		t.Fatalf("within the hour nothing is rewritten: %s %v %v", did, err, f.updated)
	}
	later := digestNow.Add(61 * time.Minute)
	if did, err = refreshDigestIssue(ops, later); err != nil || did != "updated" || len(f.updated) != 1 || f.updated[0] != 501 {
		t.Fatalf("after the hour: %s %v %v", did, err, f.updated)
	}
	if at, ok := digestStamp(f.issues[1].Body); !ok || !at.Equal(later) {
		t.Fatalf("stamp = %v %v", at, ok)
	}
	if f.created != 1 || len(f.pinned) != 1 || f.issues[0].Body != forged.Body {
		t.Fatalf("never re-created or pinned again, and never touches another Issue: created %d, pinned %v", f.created, f.pinned)
	}
}

func TestDigestIssuePinFailureIsReported(t *testing.T) {
	f := &fakeDigestIssues{pinErr: errors.New("three Issues already pinned")}
	did, err := refreshDigestIssue(f.ops(t), digestNow)
	if did != "created" || err == nil || !strings.Contains(err.Error(), "#501") {
		t.Fatalf("%s %v", did, err)
	}
	if did, err = refreshDigestIssue(f.ops(t), digestNow.Add(2*time.Hour)); did != "updated" || err != nil || f.created != 1 {
		t.Fatalf("an unpinned digest is still updated, not re-created: %s %v", did, err)
	}
}

func TestDigestIssueIsOutsideTheFlow(t *testing.T) {
	body := digestIssueBody(digestReport{Repository: "o/r"}, digestNow)
	if !isDigestIssue([]string{digestLabel}, body) {
		t.Fatal("the Writer's digest is recognised")
	}
	if isDigestIssue(nil, body) || isDigestIssue([]string{digestLabel}, "a story") {
		t.Fatal("it needs both the label and the stamp")
	}
	if err := keepDigestIssue(watchConfig(), digestNow); err != nil {
		t.Fatalf("with digest unset the Writer keeps no Issue and calls nothing: %v", err)
	}
}
