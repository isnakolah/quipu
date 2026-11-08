package main

// Running an import so that a GitHub 5xx neither duplicates nor orphans
// anything. Each Issue is created and then linked at once (Project and
// parent), so a stop leaves at most the Issue in hand unlinked. A call that
// meets a 5xx is tried again after a pause, and a create that failed is looked
// up by its key first, because GitHub often makes the Issue before the 502.
// What still fails stops the import; rerunning it matches every Issue by key
// and resumes from where it stopped.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

// importOps are the GitHub calls an import makes. Tests replace them.
type importOps struct {
	// recent returns the newest Issues as REST pages, to find what a failed
	// create made anyway.
	recent    func() ([]byte, error)
	create    func(item importItem, body string) (int, error)
	approved  func(number int) (bool, error)
	approve   func(number int, text string) error
	links     func(number int) (importLinks, error)
	project   func(number int, item importItem) error
	parent    func(parent, child int) error
	blocker   func(issue, blocker int) error
	requested func(number int) (bool, error)
	request   func(number int, item importItem) (string, error)
	sleep     func(time.Duration)
}

// importBackoff is the pause before each new try of a call that met a 5xx.
var importBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}

var serverErrorText = regexp.MustCompile(`(?i)\bHTTP 5[0-9][0-9]\b|\b50[0-9] (bad gateway|service unavailable|gateway time-?out|internal server error)|bad gateway|service unavailable|gateway time-?out|internal server error|"message":\s*"server error"`)

// serverError reports whether err is GitHub failing on its side, which a
// later try may not meet.
func serverError(err error) bool { return err != nil && serverErrorText.MatchString(err.Error()) }

// retry runs call, trying again after each 5xx until importBackoff runs out.
func (o importOps) retry(call func() error) error {
	err := call()
	for _, wait := range importBackoff {
		if !serverError(err) {
			return err
		}
		o.sleep(wait)
		err = call()
	}
	return err
}

// createItem creates item's Issue once. After a failed try it looks for the
// Issue by key before trying again.
func (o importOps) createItem(item importItem) (int, error) {
	body := importBody(item)
	number, failed := 0, false
	err := o.retry(func() error {
		if failed {
			raw, err := o.recent()
			if err != nil {
				return err
			}
			keys, err := importedKeys(raw)
			if err != nil {
				return err
			}
			if found, ok := keys[item.Key]; ok {
				number = found
				return nil
			}
		}
		created, err := o.create(item, body)
		failed, number = err != nil, created
		return err
	})
	return number, err
}

// runImport creates, links, and records every item. numbers holds the Issues
// earlier runs made, by key, and gains the ones this run makes.
func runImport(ops importOps, c model.Config, items []importItem, numbers map[string]int) ([]string, []string, error) {
	var changes, requests []string
	stop := func(key string, err error) ([]string, []string, error) {
		return changes, requests, fmt.Errorf("%s: %w; rerun the same command to resume: Issues are matched by key", key, err)
	}
	linked := map[string]importLinks{}
	for _, item := range items {
		number, ok := numbers[item.Key]
		if !ok {
			created, err := ops.createItem(item)
			if err != nil {
				return stop(item.Key, err)
			}
			number, numbers[item.Key] = created, created
			changes = append(changes, fmt.Sprintf("created #%d %s", number, item.Key))
			// GitHub limits how fast content is created.
			ops.sleep(time.Second)
		}
		var links importLinks
		if err := ops.retry(func() (err error) { links, err = ops.links(number); return err }); err != nil {
			return stop(item.Key, err)
		}
		if !links.InProject {
			if err := ops.retry(func() error { return ops.project(number, item) }); err != nil {
				return stop(item.Key, fmt.Errorf("add to Project: %w", err))
			}
			changes = append(changes, fmt.Sprintf("#%d added to the Project", number))
		}
		if item.Parent != "" && links.Parent != numbers[item.Parent] {
			if links.Parent != 0 {
				return changes, requests, fmt.Errorf("%s: #%d already has parent #%d, not #%d", item.Key, number, links.Parent, numbers[item.Parent])
			}
			if err := ops.retry(func() error { return ops.parent(numbers[item.Parent], number) }); err != nil {
				return stop(item.Key, fmt.Errorf("link parent: %w", err))
			}
			changes = append(changes, fmt.Sprintf("#%d parent #%d", number, numbers[item.Parent]))
		}
		if item.Kind == "epic" {
			var approved bool
			if err := ops.retry(func() (err error) { approved, err = ops.approved(number); return err }); err != nil {
				return stop(item.Key, err)
			}
			if !approved {
				approval := item.ApprovedBy
				if approval == "" {
					approval = "@" + c.Owner + " (import manifest)"
				}
				if err := ops.retry(func() error { return ops.approve(number, approvalPrefix+approval) }); err != nil {
					return stop(item.Key, fmt.Errorf("record approval: %w", err))
				}
			}
		}
		linked[item.Key] = links
	}
	for _, item := range items {
		number := numbers[item.Key]
		for _, blocker := range item.Blockers {
			if linked[item.Key].Blockers[numbers[blocker]] {
				continue
			}
			if err := ops.retry(func() error { return ops.blocker(number, numbers[blocker]) }); err != nil {
				return stop(item.Key, fmt.Errorf("blocker %s: %w", blocker, err))
			}
			changes = append(changes, fmt.Sprintf("#%d blocked by #%d", number, numbers[blocker]))
		}
	}
	for _, item := range items {
		if item.Status == "Backlog" {
			continue
		}
		number := numbers[item.Key]
		var asked bool
		if err := ops.retry(func() (err error) { asked, err = ops.requested(number); return err }); err != nil {
			return stop(item.Key, err)
		}
		if asked {
			continue
		}
		// After a failed post, look again: GitHub may have kept the comment.
		var id string
		failed := false
		if err := ops.retry(func() (err error) {
			if failed {
				if asked, err = ops.requested(number); err != nil || asked {
					return err
				}
			}
			id, err = ops.request(number, item)
			failed = err != nil
			return err
		}); err != nil {
			return stop(item.Key, err)
		}
		if asked {
			continue
		}
		changes = append(changes, fmt.Sprintf("#%d import as %s requested", number, item.Status))
		requests = append(requests, id)
		ops.sleep(time.Second)
	}
	return changes, requests, nil
}

// approvalPrefix starts the comment that records an imported Epic's approval.
const approvalPrefix = "Owner approval evidence: "

// liveImportOps are the real GitHub calls. The returned func removes the
// scratch directory the Issue bodies are written to.
func liveImportOps(c model.Config, here worktree) (importOps, func(), error) {
	scratch, err := os.MkdirTemp("", "quipu-import")
	if err != nil {
		return importOps{}, nil, err
	}
	ctx := context.Background()
	client := github.NewClient()
	repo := c.Owner + "/" + c.Repository
	ops := importOps{
		recent: func() ([]byte, error) {
			return ghOutput("api", "repos/"+repo+"/issues?state=all&sort=created&direction=desc&per_page=50")
		},
		create: func(item importItem, body string) (int, error) {
			bodyFile := filepath.Join(scratch, item.Key+".md")
			if err := os.WriteFile(bodyFile, []byte(body), 0o600); err != nil {
				return 0, err
			}
			output, err := ghOutput("issue", "create", "--repo", repo, "--title", item.Title, "--label", "kind:"+item.Kind, "--label", "area:"+item.Area, "--body-file", bodyFile)
			if err != nil {
				return 0, err
			}
			return issueNumberFromURL(strings.TrimSpace(string(output)))
		},
		approved: func(number int) (bool, error) {
			comments, err := client.ListComments(ctx, c.Owner, c.Repository, number)
			if err != nil {
				return false, err
			}
			for _, comment := range comments {
				if strings.HasPrefix(comment.Body, approvalPrefix) {
					return true, nil
				}
			}
			return false, nil
		},
		approve: func(number int, text string) error {
			_, err := client.CreateComment(ctx, c.Owner, c.Repository, number, text)
			return err
		},
		links: func(number int) (importLinks, error) { return readImportLinks(c, number) },
		project: func(number int, item importItem) error {
			url := fmt.Sprintf("https://github.com/%s/issues/%d", repo, number)
			return addIssueToProject(c, url, titleCase(item.Kind), titleCase(item.Area), item.Priority)
		},
		parent:  func(parent, child int) error { return linkParent(c, parent, child) },
		blocker: func(issue, blocker int) error { return addBlocker(c, issue, blocker) },
		requested: func(number int) (bool, error) {
			comments, err := client.ListComments(ctx, c.Owner, c.Repository, number)
			if err != nil {
				return false, err
			}
			return importRequested(comments, receiptTrust(c).Receipts(comments)), nil
		},
		request: func(number int, item importItem) (string, error) {
			actor, err := resolveActor(here, "", number)
			if err != nil {
				return "", err
			}
			record := writer.Import{Key: item.Key, Status: item.Status, Source: item.Source, Commits: item.Commits, Reason: item.Reason}
			return postRequest(c, number, actor, writer.Request{Action: "import", Branch: importBranch(number, item.Branch), Import: &record})
		},
		sleep: time.Sleep,
	}
	return ops, func() { os.RemoveAll(scratch) }, nil
}
