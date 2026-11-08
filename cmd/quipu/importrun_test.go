package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeIssue is what the fake GitHub keeps for one imported Issue.
type fakeIssue struct {
	body      string
	parent    int
	inProject bool
	blockers  map[int]bool
	comments  []string
	requests  int
}

// fakeImportHub stands in for GitHub. failing names keys whose create fails
// with a 502 that many more times; madeAnyway makes such a create still leave
// the Issue behind, as GitHub often does. down fails every call while set;
// outageAt makes that key's create leave its Issue and then take GitHub down.
type fakeImportHub struct {
	issues     map[int]*fakeIssue
	next       int
	failing    map[string]int
	madeAnyway bool
	down       bool
	outageAt   string
	sleeps     int
}

var errBadGateway = errors.New("gh issue create: HTTP 502: Bad Gateway (https://api.github.com/repos/octo/calla/issues)")

func newFakeImportHub() *fakeImportHub {
	return &fakeImportHub{issues: map[int]*fakeIssue{}, next: 100, failing: map[string]int{}}
}

func (h *fakeImportHub) ops() importOps {
	up := func() error {
		if h.down {
			return errBadGateway
		}
		return nil
	}
	return importOps{
		recent: func() ([]byte, error) {
			if err := up(); err != nil {
				return nil, err
			}
			return h.pages(), nil
		},
		create: func(item importItem, body string) (int, error) {
			if err := up(); err != nil {
				return 0, err
			}
			if item.Key == h.outageAt {
				h.add(body)
				h.down = true
				return 0, errBadGateway
			}
			if h.failing[item.Key] > 0 {
				h.failing[item.Key]--
				if h.madeAnyway {
					h.add(body)
				}
				return 0, errBadGateway
			}
			return h.add(body), nil
		},
		approved: func(number int) (bool, error) {
			for _, comment := range h.issues[number].comments {
				if strings.HasPrefix(comment, approvalPrefix) {
					return true, up()
				}
			}
			return false, up()
		},
		approve: func(number int, text string) error {
			if err := up(); err != nil {
				return err
			}
			h.issues[number].comments = append(h.issues[number].comments, text)
			return nil
		},
		links: func(number int) (importLinks, error) {
			issue := h.issues[number]
			blockers := map[int]bool{}
			for blocker := range issue.blockers {
				blockers[blocker] = true
			}
			return importLinks{Parent: issue.parent, InProject: issue.inProject, Blockers: blockers}, up()
		},
		project: func(number int, item importItem) error {
			if err := up(); err != nil {
				return err
			}
			h.issues[number].inProject = true
			return nil
		},
		parent: func(parent, child int) error {
			if err := up(); err != nil {
				return err
			}
			h.issues[child].parent = parent
			return nil
		},
		blocker: func(issue, blocker int) error {
			if err := up(); err != nil {
				return err
			}
			h.issues[issue].blockers[blocker] = true
			return nil
		},
		requested: func(number int) (bool, error) { return h.issues[number].requests > 0, up() },
		request: func(number int, item importItem) (string, error) {
			if err := up(); err != nil {
				return "", err
			}
			h.issues[number].requests++
			return "req-" + item.Key, nil
		},
		sleep: func(time.Duration) { h.sleeps++ },
	}
}

func (h *fakeImportHub) add(body string) int {
	h.next++
	h.issues[h.next] = &fakeIssue{body: body, blockers: map[int]bool{}}
	return h.next
}

// pages is the Issues list as one REST page.
func (h *fakeImportHub) pages() []byte {
	var page []map[string]any
	for number, issue := range h.issues {
		page = append(page, map[string]any{"number": number, "body": issue.body})
	}
	raw, _ := json.Marshal(page)
	return raw
}

// keys is what a fresh run reads before it starts.
func (h *fakeImportHub) keys(t *testing.T) map[string]int {
	t.Helper()
	keys, err := importedKeys(h.pages())
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// assertWhole checks that every item has exactly one Issue, in the Project,
// under its parent, with its blockers, approval and status request.
func (h *fakeImportHub) assertWhole(t *testing.T, items []importItem) {
	t.Helper()
	if len(h.issues) != len(items) {
		t.Fatalf("%d Issues for %d items: duplicates", len(h.issues), len(items))
	}
	keys := h.keys(t)
	for _, item := range items {
		issue := h.issues[keys[item.Key]]
		if issue == nil {
			t.Fatalf("%s has no Issue", item.Key)
		}
		if !issue.inProject || (item.Parent != "" && issue.parent != keys[item.Parent]) {
			t.Errorf("%s is orphaned: in Project %v, parent #%d", item.Key, issue.inProject, issue.parent)
		}
		for _, blocker := range item.Blockers {
			if !issue.blockers[keys[blocker]] {
				t.Errorf("%s lacks blocker %s", item.Key, blocker)
			}
		}
		if want := map[bool]int{true: 0, false: 1}[item.Status == "Backlog"]; issue.requests != want {
			t.Errorf("%s has %d status requests, want %d", item.Key, issue.requests, want)
		}
		if item.Kind == "epic" && len(issue.comments) != 1 {
			t.Errorf("%s has approval comments %q", item.Key, issue.comments)
		}
	}
}

func TestImportRetriesA502WithoutDuplicates(t *testing.T) {
	for _, madeAnyway := range []bool{true, false} {
		items, _ := validManifest(t).ordered()
		hub := newFakeImportHub()
		hub.madeAnyway = madeAnyway
		hub.failing["STORY-1"] = 2
		_, requests, err := runImport(hub.ops(), importConfig(), items, map[string]int{})
		if err != nil {
			t.Fatalf("made anyway %v: a passing 502 is retried: %v", madeAnyway, err)
		}
		hub.assertWhole(t, items)
		if len(requests) != 4 || hub.sleeps == 0 {
			t.Fatalf("requests %v, sleeps %d", requests, hub.sleeps)
		}
	}
}

func TestImportInterruptedByA502ResumesWithoutDuplicatesOrOrphans(t *testing.T) {
	items, _ := validManifest(t).ordered()
	hub := newFakeImportHub()
	// GitHub makes the third Issue, answers 502, and stays down past every
	// retry.
	hub.outageAt = "STORY-1"
	changes, _, err := runImport(hub.ops(), importConfig(), items, map[string]int{})
	if err == nil || !strings.Contains(err.Error(), "STORY-1") || !strings.Contains(err.Error(), "rerun the same command") {
		t.Fatalf("a lasting 502 stops the import and says how to resume: %v", err)
	}
	if strings.Join(changes, ";") == "" || len(hub.issues) != 3 {
		t.Fatalf("changes %v, %d Issues", changes, len(hub.issues))
	}
	// Each Issue made before the stop is already linked.
	for key, number := range hub.keys(t) {
		if key != "STORY-1" && !hub.issues[number].inProject {
			t.Fatalf("%s was left out of the Project", key)
		}
	}
	// GitHub recovers; the owner or agent reruns the same command.
	hub.down, hub.outageAt = false, ""
	if _, _, err := runImport(hub.ops(), importConfig(), items, hub.keys(t)); err != nil {
		t.Fatal(err)
	}
	hub.assertWhole(t, items)
	// A third run changes nothing.
	changes, requests, err := runImport(hub.ops(), importConfig(), items, hub.keys(t))
	if err != nil || len(changes) != 0 || len(requests) != 0 {
		t.Fatalf("a finished import is idempotent: %v %v %v", changes, requests, err)
	}
}

func TestImportStopsAtOnceOnAnErrorThatIsNotA5xx(t *testing.T) {
	items, _ := validManifest(t).ordered()
	hub := newFakeImportHub()
	ops := hub.ops()
	ops.create = func(importItem, string) (int, error) {
		return 0, errors.New("gh issue create: HTTP 422: Validation Failed")
	}
	if _, _, err := runImport(ops, importConfig(), items, map[string]int{}); err == nil || hub.sleeps != 0 {
		t.Fatalf("a 422 is not retried: %v, %d sleeps", err, hub.sleeps)
	}
	for _, text := range []string{"HTTP 502: Bad Gateway", "HTTP 503", "gh: Server Error (HTTP 500)", "504 Gateway Timeout", `{"message": "Server Error"}`} {
		if !serverError(errors.New(text)) {
			t.Errorf("%q is a server error", text)
		}
	}
	for _, text := range []string{"HTTP 404: Not Found", "HTTP 422", "issue 502 not found"} {
		if serverError(errors.New(text)) {
			t.Errorf("%q is not a server error", text)
		}
	}
}
