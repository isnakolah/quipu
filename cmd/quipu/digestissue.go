package main

// With `digest: issue` the Writer keeps one pinned "Quipu digest" Issue,
// labelled quipu:digest, and rewrites its body in place at most hourly. It
// posts it with the workflow's own token, as it does receipts, so only an
// Issue the Writer opened counts as the digest, and its edits wake no run.

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

const (
	digestLabel = "quipu:digest"
	digestTitle = "Quipu digest"
	// digestEvery is the most often the Writer rewrites the pinned Issue.
	digestEvery = time.Hour
)

var digestStampPattern = regexp.MustCompile(`<!-- quipu-digest:v1 at=([0-9TZ:.+-]+) -->`)

// digestIssue is the pinned Issue as GitHub lists it.
type digestIssue struct {
	Number int    `json:"number"`
	NodeID string `json:"node_id"`
	Body   string `json:"body"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
}

// isDigestIssue reports whether an Issue is the Writer's digest, which is
// outside the work flow: the watchdog never triages it and status never lists it.
func isDigestIssue(labels []string, body string) bool {
	return hasLabel(labels, digestLabel) && digestStampPattern.MatchString(body)
}

// digestIssueBody is the Issue body: a stamp the next pass reads, then the digest.
func digestIssueBody(d digestReport, now time.Time) string {
	return fmt.Sprintf("<!-- quipu-digest:v1 at=%s -->\n%s\n_The Quipu Writer rewrites this Issue at most hourly; nobody needs to answer it. `quipu digest --json` gives the same at any time._\n",
		now.UTC().Format(time.RFC3339), renderDigest(d))
}

// digestStamp is when the Writer last wrote body.
func digestStamp(body string) (time.Time, bool) {
	match := digestStampPattern.FindStringSubmatch(body)
	if match == nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, match[1])
	return at, err == nil
}

// digestIssueOps is what keeping the Issue does to GitHub; tests replace it.
type digestIssueOps struct {
	list   func() ([]digestIssue, error)
	create func(body string) (digestIssue, error)
	update func(number int, body string) error
	pin    func(issue digestIssue) error
	build  func() digestReport
}

// refreshDigestIssue updates the pinned Issue in place once an hour has passed
// since its last update, or opens and pins it the first time. It says what it
// did: "fresh", "updated" or "created".
func refreshDigestIssue(ops digestIssueOps, now time.Time) (string, error) {
	issues, err := ops.list()
	if err != nil {
		return "", err
	}
	var current *digestIssue
	for i := range issues {
		if issues[i].User.Login == writer.WriterLogin && digestStampPattern.MatchString(issues[i].Body) {
			if current == nil || issues[i].Number < current.Number {
				current = &issues[i]
			}
		}
	}
	if current != nil {
		if at, ok := digestStamp(current.Body); ok && now.Sub(at) < digestEvery {
			return "fresh", nil
		}
		return "updated", ops.update(current.Number, digestIssueBody(ops.build(), now))
	}
	created, err := ops.create(digestIssueBody(ops.build(), now))
	if err != nil {
		return "", err
	}
	if err := ops.pin(created); err != nil {
		return "created", fmt.Errorf("digest Issue #%d opened but not pinned: %w", created.Number, err)
	}
	return "created", nil
}

func liveDigestIssueOps(c model.Config, now time.Time) digestIssueOps {
	ctx := context.Background()
	repo := fmt.Sprintf("/repos/%s/%s", c.Owner, c.Repository)
	poster := receiptClient()
	return digestIssueOps{
		list: func() ([]digestIssue, error) {
			var issues []digestIssue
			err := github.NewClient().Do(ctx, http.MethodGet, repo+"/issues?state=open&per_page=20&labels="+digestLabel, nil, &issues)
			return issues, err
		},
		create: func(body string) (digestIssue, error) {
			var issue digestIssue
			if err := poster.Do(ctx, http.MethodPost, repo+"/issues", map[string]any{"title": digestTitle, "body": body}, &issue); err != nil {
				return issue, err
			}
			return issue, poster.AddLabels(ctx, c.Owner, c.Repository, issue.Number, digestLabel)
		},
		update: func(number int, body string) error {
			return poster.Do(ctx, http.MethodPatch, fmt.Sprintf("%s/issues/%d", repo, number), map[string]string{"body": body}, nil)
		},
		pin: func(issue digestIssue) error {
			_, err := runGraphQL(fmt.Sprintf("mutation { pinIssue(input:{issueId:%q}) { issue { number } } }", issue.NodeID))
			return err
		},
		build: func() digestReport {
			owner, err := boardOwnerQueue(c, now)
			d := collectDigest(c.Owner+"/"+c.Repository, liveDigestSources(c, true), owner, now.Add(-digestWindow), now)
			if err != nil {
				d.Warnings = append(d.Warnings, "board: "+strings.TrimSpace(err.Error()))
			}
			return d
		},
	}
}

// keepDigestIssue is the Writer's hourly step, when the config asks for it.
func keepDigestIssue(c model.Config, now time.Time) error {
	if !c.KeepsDigestIssue() {
		return nil
	}
	_, err := refreshDigestIssue(liveDigestIssueOps(c, now), now)
	return err
}
