package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

func TestAutonomyFlagsValidate(t *testing.T) {
	base := model.Config{SchemaVersion: model.ConfigVersion, Owner: "o", Repository: "r", DefaultBranch: "main", WikiMode: "off", Areas: []string{"core"}, AuthorizedActors: []string{"o"}}
	for name, tc := range map[string]struct {
		set   func(*model.Config)
		valid bool
	}{
		"off": {func(*model.Config) {}, true},
		"explicit defaults": {func(c *model.Config) {
			c.IssuesBy, c.TriageBy, c.UpdateBehind, c.MergeWait, c.GuardReads = "owner", "owner", "off", "foreground", "closed"
		}, true},
		"issues_by":               {func(c *model.Config) { c.IssuesBy = "agents" }, true},
		"issues_by bad":           {func(c *model.Config) { c.IssuesBy = "anyone" }, false},
		"triage_by":               {func(c *model.Config) { c.TriageBy = "agents" }, true},
		"triage_by bad":           {func(c *model.Config) { c.TriageBy = "anyone" }, false},
		"guard_reads":             {func(c *model.Config) { c.GuardReads = "open" }, true},
		"guard_reads bad":         {func(c *model.Config) { c.GuardReads = "wide" }, false},
		"update_behind":           {func(c *model.Config) { c.Merge, c.UpdateBehind = "agent", "auto" }, true},
		"update_behind bad":       {func(c *model.Config) { c.Merge, c.UpdateBehind = "agent", "always" }, false},
		"update_behind, no merge": {func(c *model.Config) { c.UpdateBehind = "auto" }, false},
		"merge_wait":              {func(c *model.Config) { c.Merge, c.MergeWait = "agent", "background" }, true},
		"merge_wait bad":          {func(c *model.Config) { c.Merge, c.MergeWait = "agent", "later" }, false},
		"merge_wait, no merge":    {func(c *model.Config) { c.MergeWait = "background" }, false},
	} {
		c := base
		tc.set(&c)
		if err := c.Validate(); (err == nil) != tc.valid {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAutonomyFlagsAreRelaxations(t *testing.T) {
	c := model.Config{Merge: "agent", IssuesBy: "agents", TriageBy: "agents", UpdateBehind: "auto", MergeWait: "background", GuardReads: "open"}
	if !c.AgentsCreateIssues() || !c.AgentsTriage() || !c.UpdatesBehind() || !c.MergesInBackground() || !c.GuardReadsOpen() {
		t.Fatalf("flags on: %+v", c)
	}
	off := c.WithoutRelaxations()
	if off.AgentsCreateIssues() || off.AgentsTriage() || off.UpdatesBehind() || off.MergesInBackground() || off.GuardReadsOpen() {
		t.Fatalf("an untrusted config must grant nothing: %+v", off)
	}
}

// flaggedRepo is a checkout whose default branch's config adds extra.
func flaggedRepo(t *testing.T, extra string) (string, worktree) {
	t.Helper()
	return publishedRepo(t, rootConfigPath, append(repoConfig(t), extra...))
}

func TestGuardOpensOnlyWhatTheDefaultBranchFlagsOpen(t *testing.T) {
	commands := []string{"git tag --sort=-v:refname", "quipu triage 7 --priority P1 --json", "quipu issue create --kind story --parent 1"}
	_, closed := flaggedRepo(t, "")
	for _, command := range commands {
		if code := guardCode(t, closed.Top, "Bash", map[string]any{"command": command}); code == "" {
			t.Errorf("flags off: %q must be denied", command)
		}
	}
	// A worktree's own copy turns on nothing.
	raw := append(repoConfig(t), "guard_reads: open\ntriage_by: agents\nissues_by: agents\n"...)
	writeRepoFile(t, closed.Top, rootConfigPath, raw)
	for _, command := range commands {
		if code := guardCode(t, closed.Top, "Bash", map[string]any{"command": command}); code == "" {
			t.Errorf("worktree copy: %q must be denied", command)
		}
	}
}

func TestGuardReadsOpen(t *testing.T) {
	reads := []string{"git tag --sort=-v:refname", "git branch --list '1*'", "git symbolic-ref --short HEAD", "git notes list", "nproc", "strings a.bin", "pgrep go", "paste a b", "sw_vers"}
	t.Run("closed", func(t *testing.T) {
		_, closed := flaggedRepo(t, "")
		for _, command := range reads {
			if code := guardCode(t, closed.Top, "Bash", map[string]any{"command": command}); code == "" {
				t.Errorf("%q must stay denied", command)
			}
		}
	})
	_, open := flaggedRepo(t, "guard_reads: open\n")
	for _, command := range reads {
		if code := guardCode(t, open.Top, "Bash", map[string]any{"command": command}); code != "" {
			t.Errorf("open: %q denied with %s", command, code)
		}
	}
	for command, want := range map[string]string{
		"python3 -c 'print(1)'":    "no-lease",
		"perl -e 'print 1'":        "no-lease",
		"awk 'BEGIN{print 1}'":     "no-lease",
		"node -e 1":                "no-lease",
		"ruby -e 1":                "no-lease",
		"base64 -o out in":         "no-lease",
		"xxd in out":               "no-lease",
		"git tag v1":               "git-refs",
		"git tag --sort=x v1":      "git-refs",
		"git tag --delete v1":      "git-refs",
		"git branch --list x -d":   "branch-change",
		"git branch new":           "branch-change",
		"git symbolic-ref HEAD x":  "git-refs",
		"git symbolic-ref -d HEAD": "git-refs",
		"git notes add":            "git-refs",
		"gh pr merge 1":            "merge-approve",
		"nproc > out.txt":          "no-lease",
		"arch":                     "no-lease",
		"arch -arm64 python3 -c 1": "no-lease",
	} {
		if code := guardCode(t, open.Top, "Bash", map[string]any{"command": command}); code != want {
			t.Errorf("open: %q: want %s, got %q", command, want, code)
		}
	}
}

func TestGuardKeepsEpicsAndClosingOutOfTheFlags(t *testing.T) {
	_, on := flaggedRepo(t, "issues_by: agents\ntriage_by: agents\nready_by: agents\n")
	for _, command := range []string{
		"quipu issue create --kind epic --approved-by me --title t --body-file b --apply",
		"quipu issue create --kind story --kind epic --parent 1 --title t --body-file b --apply",
		"quipu issue create --kind=epic --parent 1 --apply",
		"quipu issue create -kind epic --parent 1 --apply",
		"quipu issue create --kind story --apply",
		"quipu issue link-parent --parent 1 --child 2 --apply",
		"quipu move 7 archived",
		"gh issue close 7",
	} {
		if code := guardCode(t, on.Top, "Bash", map[string]any{"command": command}); code == "" {
			t.Errorf("%q must be denied", command)
		}
	}
	if code := guardCode(t, on.Top, "Bash", map[string]any{"command": "quipu triage 7 --priority P1 --json"}); code != "" {
		t.Errorf("triage_by on: denied with %s", code)
	}
}

func TestAgentIssueProblem(t *testing.T) {
	kinds := map[int]string{1: "Epic", 2: "Story", 3: "a closed Issue"}
	parent := func(number int) (string, error) {
		if kind, ok := kinds[number]; ok {
			return kind, nil
		}
		return "", errors.New("not found")
	}
	literal := func(command string) []shellArg {
		var args []shellArg
		for _, word := range strings.Fields(command) {
			args = append(args, shellArg{word, true})
		}
		return args
	}
	issues := model.Config{IssuesBy: "agents"}
	triage := model.Config{TriageBy: "agents"}
	both := model.Config{IssuesBy: "agents", TriageBy: "agents"}
	for name, tc := range map[string]struct {
		c       model.Config
		command string
		want    string
	}{
		"off":                  {model.Config{}, "--kind story --parent 1", "issues_by"},
		"story under Epic":     {issues, "--kind story --parent 1", ""},
		"task, inline":         {issues, "--kind=task --parent=1", ""},
		"defect, single dash":  {issues, "-kind defect -parent 1", ""},
		"under a Story":        {issues, "--kind story --parent 2", "must be an open Epic"},
		"under a closed Epic":  {issues, "--kind story --parent 3", "must be an open Epic"},
		"parent unreadable":    {issues, "--kind story --parent 9", "could not read"},
		"no parent":            {issues, "--kind story", "--parent"},
		"Epic":                 {both, "--kind epic --parent 1", "may not create"},
		"contract":             {both, "--kind contract --parent 1", "may not create"},
		"last flag wins":       {both, "--kind story --kind epic --parent 1", "may not create"},
		"second parent":        {both, "--kind story --parent 1 --parent 2", "must be an open Epic"},
		"octal parent":         {issues, "--kind story --parent 010", "could not read parent #010"},
		"hex parent":           {issues, "--kind story --parent 0x2", "must be an open Epic"},
		"Decision, no triage":  {issues, "--kind decision --parent 1", "triage_by"},
		"Decision with triage": {triage, "--kind decision --parent 1", ""},
		"Gate with triage":     {triage, "--kind gate --parent 1", ""},
		"story, triage only":   {triage, "--kind story --parent 1", "issues_by"},
	} {
		got := agentIssueProblem(tc.c, literal(tc.command), parent)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: want %q, got %q", name, tc.want, got)
		}
	}
	computed := []shellArg{{"--kind", true}, {"", false}, {"--parent", true}, {"1", true}}
	if got := agentIssueProblem(issues, computed, parent); got == "" {
		t.Error("a computed argument must be refused")
	}
}

func TestTriageCannotBypassReadyOrClose(t *testing.T) {
	labels := []string{triageLabel, "kind:story", "area:core"}
	agents := model.Config{TriageBy: "agents", Areas: []string{"core"}}
	readyToo := agents
	readyToo.ReadyBy = "agents"
	parent := func(number int) (string, error) {
		if number == 1 {
			return "Epic", nil
		}
		return "a closed Issue", nil
	}
	for name, tc := range map[string]struct {
		c      model.Config
		o      triageOptions
		labels []string
		want   string
	}{
		"flag off":          {model.Config{Areas: []string{"core"}}, triageOptions{Priority: "P1"}, labels, "triage_by"},
		"priority":          {agents, triageOptions{Priority: "P1", Area: "core", Parent: 1}, labels, ""},
		"backlog":           {agents, triageOptions{Status: "backlog"}, labels, ""},
		"blocked":           {agents, triageOptions{Status: "Blocked"}, labels, ""},
		"Ready, no flag":    {agents, triageOptions{Status: "ready"}, labels, "ready_by"},
		"Ready with flag":   {readyToo, triageOptions{Status: "ready"}, labels, ""},
		"Done":              {readyToo, triageOptions{Status: "done"}, labels, "never"},
		"Cancelled":         {readyToo, triageOptions{Status: "cancelled"}, labels, "never"},
		"Archived":          {readyToo, triageOptions{Status: "archived"}, labels, "must be"},
		"In progress":       {readyToo, triageOptions{Status: "in progress"}, labels, "must be"},
		"not flagged":       {agents, triageOptions{Priority: "P1"}, []string{"kind:story"}, "needs no triage"},
		"bad priority":      {agents, triageOptions{Priority: "P9"}, labels, "priority"},
		"bad area":          {agents, triageOptions{Area: "other"}, labels, "not configured"},
		"own parent":        {agents, triageOptions{Parent: 7}, labels, "another Issue"},
		"file a Decision":   {agents, triageOptions{Kind: "decision"}, []string{triageLabel}, ""},
		"Epic kind":         {agents, triageOptions{Kind: "epic"}, []string{triageLabel}, "Epics stay"},
		"kind already set":  {agents, triageOptions{Kind: "gate"}, labels, "already has kind"},
		"same kind again":   {agents, triageOptions{Kind: "story"}, labels, ""},
		"parent not Epic":   {agents, triageOptions{Parent: 2}, labels, "must be an open Epic"},
		"Decision to story": {agents, triageOptions{Kind: "story"}, []string{triageLabel, "kind:decision"}, "already has kind"},
	} {
		err := checkTriage(tc.c, 7, tc.o, liveWork{Labels: tc.labels, Status: "Ready"}, true, parent)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
}

func TestTriageFailsBeforeChangingAnything(t *testing.T) {
	c := model.Config{TriageBy: "agents", Areas: []string{"core"}}
	epic := func(int) (string, error) { return "Epic", nil }
	flagged := []string{triageLabel}
	for name, tc := range map[string]struct {
		o         triageOptions
		state     liveWork
		inProject bool
		want      string
	}{
		"joining, no kind":        {triageOptions{Area: "core"}, liveWork{Labels: flagged}, false, "no kind"},
		"joining, no area":        {triageOptions{Kind: "story"}, liveWork{Labels: flagged}, false, "no area"},
		"joining, all known":      {triageOptions{Kind: "story", Area: "core", Parent: 1}, liveWork{Labels: flagged}, false, ""},
		"joining stays Backlog":   {triageOptions{Kind: "story", Area: "core", Status: "backlog"}, liveWork{Labels: flagged}, false, ""},
		"Backlog cannot block":    {triageOptions{Status: "blocked"}, liveWork{Labels: flagged, Status: "Backlog"}, true, "cannot move"},
		"Ready can go to Backlog": {triageOptions{Status: "backlog"}, liveWork{Labels: flagged, Status: "Ready"}, true, ""},
		"Ready can block":         {triageOptions{Status: "blocked"}, liveWork{Labels: flagged, Status: "Ready"}, true, ""},
		"held work stays":         {triageOptions{Status: "backlog"}, liveWork{Labels: flagged, Status: "In progress"}, true, "leaves work in progress alone"},
		"reviewed work stays":     {triageOptions{Status: "blocked"}, liveWork{Labels: flagged, Status: "In review"}, true, "leaves work in progress alone"},
	} {
		err := checkTriage(c, 7, tc.o, tc.state, tc.inProject, epic)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
}

func passing(names ...string) []prCheck {
	var checks []prCheck
	for _, name := range names {
		checks = append(checks, prCheck{Name: name, Status: "COMPLETED", Conclusion: "SUCCESS", DetailsURL: "https://github.com/o/r/actions/runs/1/job/" + name})
	}
	return checks
}

func TestDecideQueuedMerge(t *testing.T) {
	required := []string{"test", "policy"}
	green := mergeView{State: "OPEN", HeadRefOid: "abc", MergeStateStatus: "CLEAN", Checks: passing("test", "policy")}
	with := func(change func(*mergeView)) mergeView {
		view := green
		change(&view)
		return view
	}
	failed := []prCheck{{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"}, {Name: "policy", Status: "COMPLETED", Conclusion: "SUCCESS"}}
	notRun := with(func(v *mergeView) { v.Checks = passing("policy") })
	if got, reason := decideQueuedMerge(notRun, "abc", required, false, queueGrace); got != queueFail || !strings.Contains(reason, "never ran: test") {
		t.Errorf("a check that never appears cannot wait for ever: %d %q", got, reason)
	}
	running := with(func(v *mergeView) { v.Checks = append(passing("policy"), prCheck{Name: "test", Status: "QUEUED"}) })
	if got, _ := decideQueuedMerge(running, "abc", required, false, 48*time.Hour); got != queueWait {
		t.Errorf("a queued job waits, and GitHub ends it if no runner comes: %d", got)
	}
	for name, tc := range map[string]struct {
		view   mergeView
		update bool
		want   queueStep
		reason string
	}{
		"green":                 {green, false, queueMergeNow, ""},
		"unstable still merges": {with(func(v *mergeView) { v.MergeStateStatus = "UNSTABLE" }), false, queueMergeNow, ""},
		"failed check":          {with(func(v *mergeView) { v.Checks = failed }), false, queueFail, "test"},
		"failed beats behind":   {with(func(v *mergeView) { v.Checks = failed; v.MergeStateStatus = "BEHIND" }), true, queueFail, "test"},
		"cancelled check":       {with(func(v *mergeView) { v.Checks = []prCheck{{Name: "test", Status: "COMPLETED", Conclusion: "CANCELLED"}} }), false, queueFail, "test"},
		"failed policy": {with(func(v *mergeView) {
			v.Checks = append(passing("test"), prCheck{Name: "policy", Status: "COMPLETED", Conclusion: "FAILURE"})
		}), false, queueFail, "policy"},
		"running":               {with(func(v *mergeView) { v.Checks = append(passing("policy"), prCheck{Name: "test", Status: "IN_PROGRESS"}) }), false, queueWait, ""},
		"queued":                {with(func(v *mergeView) { v.Checks = append(passing("policy"), prCheck{Name: "test", Status: "QUEUED"}) }), false, queueWait, ""},
		"not run yet":           {with(func(v *mergeView) { v.Checks = passing("policy") }), false, queueWait, ""},
		"state not known yet":   {with(func(v *mergeView) { v.MergeStateStatus = "UNKNOWN" }), false, queueWait, ""},
		"behind":                {with(func(v *mergeView) { v.MergeStateStatus = "BEHIND" }), false, queueFail, "behind"},
		"behind, update":        {with(func(v *mergeView) { v.MergeStateStatus = "BEHIND" }), true, queueUpdate, ""},
		"conflict":              {with(func(v *mergeView) { v.MergeStateStatus = "DIRTY" }), true, queueFail, "conflicts"},
		"draft":                 {with(func(v *mergeView) { v.IsDraft = true }), false, queueFail, "draft"},
		"merged by someone":     {with(func(v *mergeView) { v.State = "MERGED"; v.HeadRefOid = "other" }), false, queueMerged, ""},
		"closed":                {with(func(v *mergeView) { v.State = "CLOSED" }), false, queueFail, "closed"},
		"pushed after queueing": {with(func(v *mergeView) { v.HeadRefOid = "new" }), false, queueDrop, "rerun quipu done"},
	} {
		got, reason := decideQueuedMerge(tc.view, "abc", required, tc.update, 0)
		if got != tc.want || !strings.Contains(reason, tc.reason) {
			t.Errorf("%s: step %d, reason %q", name, got, reason)
		}
	}
}

// queueFixture runs a Writer pass against fake GitHub operations and records
// what it did.
type queueFixture struct {
	w        *watch
	state    *liveWork
	view     mergeView
	merges   int
	updates  int
	parent   string
	moved    *writer.WorkState
	requests []writer.Request
	mu       sync.Mutex
	posted   []string
}

func newQueueFixture(t *testing.T, c model.Config, view mergeView) *queueFixture {
	t.Helper()
	f := &queueFixture{view: view, state: &liveWork{IssueID: "I_7", IssueState: "OPEN", ItemID: "item", Status: "In review", Holder: "claude/o/1", Branch: "007/work", Expiry: "2026-11-02T11:00:00Z"}}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.posted = append(f.posted, body.Body)
		f.mu.Unlock()
		rw.WriteHeader(http.StatusCreated)
		_, _ = rw.Write([]byte(`{"id":1}`))
	}))
	t.Cleanup(server.Close)
	queued := writer.Receipt{RequestID: "q1", Result: "accepted", At: time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC),
		Merge: &writer.QueuedMerge{PR: "https://github.com/o/r/pull/9", Head: "abc", Evidence: writer.Evidence{FinalSHA: pendingMerge, CIURL: pendingMerge, Commands: "go test", Environments: "CI", Criteria: "met", Artifacts: "None", Documentation: "None: x", Risks: "None", Boundary: "CI"}}}
	f.w = &watch{c: c, number: 7, apply: true, now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), ctx: context.Background(), receipts: []writer.Receipt{queued},
		poster: &github.Client{BaseURL: server.URL, Token: "t", HTTP: server.Client()}}
	f.w.ops = mergeOps{
		view: func(string) (mergeView, error) { return f.view, nil },
		merge: func(number int, view mergeView) error {
			f.merges++
			f.view.State = "MERGED"
			f.view.MergeCommit = &struct {
				OID string `json:"oid"`
			}{OID: "merge-sha"}
			return nil
		},
		update: func(string) error { f.updates++; f.view.HeadRefOid = "updated"; return nil },
		parent: func(string) (string, error) { return f.parent, nil },
		state:  func(int) (liveWork, error) { return *f.state, nil },
		move:   func(_ string, next writer.WorkState) error { f.moved = &next; return nil },
		apply: func(_ int, request writer.Request, _ []writer.Receipt) writer.Receipt {
			f.requests = append(f.requests, request)
			receipt := writer.Receipt{RequestID: request.ID, Result: "accepted", At: f.w.now}
			if request.Action == "evidence.submit" {
				receipt.Evidence = request.Evidence
				f.state.Status = "Evidence pending"
			}
			return receipt
		},
	}
	return f
}

var backgroundMerge = model.Config{Merge: "agent", MergeWait: "background"}

func greenView() mergeView {
	return mergeView{URL: "https://github.com/o/r/pull/9", State: "OPEN", HeadRefOid: "abc", MergeStateStatus: "CLEAN", Checks: passing("test", "policy")}
}

func TestQueuedMergeMergesOnGreenAndRecordsEvidence(t *testing.T) {
	f := newQueueFixture(t, backgroundMerge, greenView())
	if moved, err := f.w.queuedMerge(f.state); err != nil || !moved {
		t.Fatalf("%v %v", moved, err)
	}
	if f.merges != 1 || f.w.report.MergesDone != 1 {
		t.Fatalf("merges=%d report=%+v", f.merges, f.w.report)
	}
	if len(f.requests) != 2 || f.requests[0].Action != "evidence.submit" || f.requests[1].Status != "Done" {
		t.Fatalf("requests: %+v", f.requests)
	}
	evidence := f.requests[0].Evidence
	if evidence.FinalSHA != "merge-sha" || !strings.Contains(evidence.CIURL, "/actions/runs/1/job/test") || f.requests[0].Actor != "claude/o/1" {
		t.Fatalf("the Writer fills what only the merge knows: %+v", f.requests[0])
	}
	if _, queued := writer.LatestQueuedMerge(f.w.receipts); queued {
		t.Fatal("the queue must end")
	}
}

func TestFailedCheckBlocksTheQueuedMerge(t *testing.T) {
	view := greenView()
	view.Checks = append(passing("policy"), prCheck{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"})
	f := newQueueFixture(t, backgroundMerge, view)
	if _, err := f.w.queuedMerge(f.state); err != nil {
		t.Fatal(err)
	}
	if f.merges != 0 || len(f.requests) != 0 {
		t.Fatalf("a failed check must never merge: %d %+v", f.merges, f.requests)
	}
	if f.moved == nil || f.moved.Status != "Ready" || f.moved.Lease.Branch != "007/work" || f.moved.Lease.Holder != "" {
		t.Fatalf("the Issue returns to Ready on its branch: %+v", f.moved)
	}
	if len(f.posted) != 1 || !strings.Contains(f.posted[0], "required checks not passing: test") || !strings.Contains(f.posted[0], "007/work") {
		t.Fatalf("the receipt must give the reason: %v", f.posted)
	}
	if receipts := f.w.receipts; receipts[len(receipts)-1].State == nil || receipts[len(receipts)-1].State.Status != "Ready" {
		t.Fatalf("the receipt must record Ready, or the watchdog puts it back: %+v", receipts[len(receipts)-1])
	}
}

func TestQueuedMergeWaitsWithoutTouchingAnything(t *testing.T) {
	view := greenView()
	view.Checks = append(passing("policy"), prCheck{Name: "test", Status: "QUEUED"})
	f := newQueueFixture(t, backgroundMerge, view)
	if moved, err := f.w.queuedMerge(f.state); err != nil || moved {
		t.Fatalf("%v %v", moved, err)
	}
	if f.merges != 0 || f.moved != nil || len(f.posted) != 0 || f.w.report.MergesQueued != 1 {
		t.Fatalf("merges=%d moved=%+v posted=%v report=%+v", f.merges, f.moved, f.posted, f.w.report)
	}
	if f.state.Status != "In review" || f.state.Holder == "" {
		t.Fatalf("policy needs the holder while it waits: %+v", f.state)
	}
}

func TestQueuedMergeStopsWhenTheOwnerTurnsItOff(t *testing.T) {
	for _, c := range []model.Config{{Merge: "agent"}, {MergeWait: "background"}, {}} {
		f := newQueueFixture(t, c, greenView())
		if _, err := f.w.queuedMerge(f.state); err != nil {
			t.Fatal(err)
		}
		if f.merges != 0 || len(f.w.report.Warnings) != 1 {
			t.Errorf("%+v: merges=%d warnings=%v", c, f.merges, f.w.report.Warnings)
		}
	}
}

func TestQueuedMergeDryRunChangesNothing(t *testing.T) {
	f := newQueueFixture(t, backgroundMerge, greenView())
	f.w.apply = false
	if _, err := f.w.queuedMerge(f.state); err != nil {
		t.Fatal(err)
	}
	if f.merges != 0 || len(f.posted) != 0 || f.w.report.MergesQueued != 1 {
		t.Fatalf("merges=%d posted=%v report=%+v", f.merges, f.posted, f.w.report)
	}
}

func TestPushAfterQueueingDropsTheMergeAndKeepsTheHolder(t *testing.T) {
	view := greenView()
	view.HeadRefOid = "new"
	f := newQueueFixture(t, backgroundMerge, view)
	if _, err := f.w.queuedMerge(f.state); err != nil {
		t.Fatal(err)
	}
	if f.merges != 0 || f.moved != nil || !strings.Contains(strings.Join(f.posted, ""), "rerun quipu done") {
		t.Fatalf("merges=%d moved=%+v posted=%v", f.merges, f.moved, f.posted)
	}
	if _, queued := writer.LatestQueuedMerge(f.w.receipts); queued {
		t.Fatal("the queue must end")
	}
}

func TestQueuedMergeUpdatesABehindPROnlyWithTheFlag(t *testing.T) {
	view := greenView()
	view.MergeStateStatus = "BEHIND"
	withUpdate := model.Config{Merge: "agent", MergeWait: "background", UpdateBehind: "auto"}
	f := newQueueFixture(t, withUpdate, view)
	if _, err := f.w.queuedMerge(f.state); err != nil {
		t.Fatal(err)
	}
	queued, ok := writer.LatestQueuedMerge(f.w.receipts)
	if f.updates != 1 || f.merges != 0 || !ok || queued.Merge.Head != "" || queued.Merge.UpdatedFrom != "abc" || queued.Merge.Evidence.Criteria != "met" {
		t.Fatalf("updates=%d merges=%d queued=%+v", f.updates, f.merges, queued)
	}
	// GitHub still shows the old head: nothing changes, and the queue survives.
	f.view.HeadRefOid = "abc"
	if _, err := f.w.queuedMerge(f.state); err != nil {
		t.Fatal(err)
	}
	if queued, ok = writer.LatestQueuedMerge(f.w.receipts); !ok || queued.Merge.Head != "" {
		t.Fatalf("the Writer's own update must not drop its queue: %+v", queued)
	}
	// Once the new head shows, the queue pins it and merges on its checks.
	f.view.HeadRefOid, f.view.MergeStateStatus = "updated", "CLEAN"
	if _, err := f.w.queuedMerge(f.state); err != nil {
		t.Fatal(err)
	}
	if queued, ok = writer.LatestQueuedMerge(f.w.receipts); !ok || queued.Merge.Head != "updated" {
		t.Fatalf("queued=%+v", queued)
	}
	if _, err := f.w.queuedMerge(f.state); err != nil {
		t.Fatal(err)
	}
	if f.merges != 1 {
		t.Fatalf("merges=%d", f.merges)
	}
	without := newQueueFixture(t, backgroundMerge, view)
	if _, err := without.w.queuedMerge(without.state); err != nil {
		t.Fatal(err)
	}
	if without.updates != 0 || without.moved == nil || without.moved.Status != "Ready" {
		t.Fatalf("updates=%d moved=%+v", without.updates, without.moved)
	}
}

func TestRejectedQueuedEvidenceEndsTheQueueAfterTheMerge(t *testing.T) {
	f := newQueueFixture(t, backgroundMerge, greenView())
	f.w.ops.apply = func(_ int, request writer.Request, _ []writer.Receipt) writer.Receipt {
		return writer.Receipt{RequestID: request.ID, Result: "rejected", Detail: "CI run did not run on PR", At: f.w.now}
	}
	if _, err := f.w.queuedMerge(f.state); err != nil {
		t.Fatal(err)
	}
	if f.merges != 1 {
		t.Fatalf("merges=%d", f.merges)
	}
	if !strings.Contains(strings.Join(f.posted, ""), "quipu done 7") {
		t.Fatalf("the holder must be told to record it: %v", f.posted)
	}
	if _, queued := writer.LatestQueuedMerge(f.w.receipts); queued {
		t.Fatal("a merged PR must never merge again")
	}
}

func TestMergeFailureReturnsTheWorkWithTheReason(t *testing.T) {
	f := newQueueFixture(t, backgroundMerge, greenView())
	f.w.ops.merge = func(int, mergeView) error { return errors.New("GitHub refused: conversations unresolved") }
	if _, err := f.w.queuedMerge(f.state); err != nil {
		t.Fatal(err)
	}
	if f.moved == nil || f.moved.Status != "Ready" || !strings.Contains(strings.Join(f.posted, ""), "conversations unresolved") {
		t.Fatalf("moved=%+v posted=%v", f.moved, f.posted)
	}
}

func TestRefusedMergeRetriesUntilTheGraceEnds(t *testing.T) {
	f := newQueueFixture(t, backgroundMerge, greenView())
	f.w.now = f.w.receipts[0].At.Add(10 * time.Minute)
	f.w.ops.merge = func(int, mergeView) error { return errors.New("502") }
	if moved, err := f.w.queuedMerge(f.state); err != nil || moved {
		t.Fatalf("%v %v", moved, err)
	}
	if f.moved != nil || len(f.posted) != 0 || f.state.Holder == "" || len(f.w.report.Warnings) != 1 {
		t.Fatalf("a passing failure must not evict the holder: %+v %v %+v", f.moved, f.posted, f.w.report)
	}
	if _, queued := writer.LatestQueuedMerge(f.w.receipts); !queued {
		t.Fatal("the merge stays queued")
	}
}

func TestMergeThatErrorsButLandedRecordsEvidence(t *testing.T) {
	f := newQueueFixture(t, backgroundMerge, greenView())
	merged := f.w.ops.merge
	f.w.ops.merge = func(number int, view mergeView) error {
		_ = merged(number, view)
		return errors.New("502 after the merge landed")
	}
	if _, err := f.w.queuedMerge(f.state); err != nil {
		t.Fatal(err)
	}
	if f.moved != nil || len(f.requests) != 2 || f.requests[0].Evidence.FinalSHA != "merge-sha" {
		t.Fatalf("a merged PR must not go back to Ready: %+v %+v", f.moved, f.requests)
	}
}

func TestQueuedEvidenceCitesTheHeadThatMerged(t *testing.T) {
	c := model.Config{}
	current := passing("test", "policy")
	if got := queuedCIURL(c, "https://github.com/o/r/actions/runs/old", current); !strings.Contains(got, "/runs/1/job/test") {
		t.Fatalf("a URL from before an update must not survive: %s", got)
	}
	other := model.Config{RequiredChecks: []string{"unit"}}
	if got := queuedCIURL(other, pendingMerge, current); !strings.Contains(got, "/runs/1/") {
		t.Fatalf("any passing run is better than none: %s", got)
	}
	if got := queuedCIURL(c, "https://given", nil); got != "https://given" {
		t.Fatalf("no checks to read keeps what the agent gave: %s", got)
	}
	if got := queuedCIURL(c, pendingMerge, nil); got != "" {
		t.Fatalf("the placeholder is never evidence: %s", got)
	}
}

func TestQueuedEvidenceFinishesAfterAnInterruptedPass(t *testing.T) {
	f := newQueueFixture(t, backgroundMerge, greenView())
	f.state.Status = "Evidence pending"
	if _, err := f.w.queuedMerge(f.state); err != nil {
		t.Fatal(err)
	}
	if f.merges != 0 || len(f.requests) != 1 || f.requests[0].Status != "Done" {
		t.Fatalf("merges=%d requests=%+v", f.merges, f.requests)
	}
}

func TestQueuedMergeNeverCountsAsEvidence(t *testing.T) {
	queued := writer.Receipt{RequestID: "q1", Result: "accepted", Merge: &writer.QueuedMerge{Evidence: writer.Evidence{FinalSHA: "x", CIURL: "x", Commands: "x", Environments: "x", Criteria: "x", Artifacts: "x", Documentation: "x", Risks: "x", Boundary: "x"}}}
	if writer.AcceptedEvidenceSince([]writer.Receipt{queued}, time.Time{}) {
		t.Fatal("Done needs evidence validated against a merged PR")
	}
}

func TestChecksStillRunning(t *testing.T) {
	for code, want := range map[string]bool{"checks-pending": true, "checks-no-runner": true, "checks-failing": false, "merge-blocked": false} {
		if got := checksStillRunning(flowErr(code, "x")); got != want {
			t.Errorf("%s: %v", code, got)
		}
	}
	if checksStillRunning(nil) || checksStillRunning(errors.New("gh down")) {
		t.Fatal("only pending checks queue a merge")
	}
	// Right after a push GitHub has no checks yet, which must not read as failure.
	required := []string{"test", "policy"}
	if err := mergeGate(nil, required); !checksStillRunning(err) || !strings.Contains(err.Error(), "never ran") {
		t.Fatalf("checks not created yet: %v", err)
	}
	mixed := []prCheck{{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"}}
	if err := mergeGate(mixed, required); checksStillRunning(err) {
		t.Fatalf("a real failure must never queue, even with another check missing: %v", err)
	}
}

// behindBranch is a worktree on 001/a whose origin's main has moved on.
func behindBranch(t *testing.T, mainFile, branchFile string) (string, worktree) {
	t.Helper()
	top, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	writeRepoFile(t, agent.Top, branchFile, []byte("branch\n"))
	commitAll(t, agent.Top)
	gitIn(t, agent.Top, "push", "-q", "origin", "001/a")
	writeRepoFile(t, top, mainFile, []byte("main\n"))
	commitAll(t, top)
	gitIn(t, top, "push", "-q", "origin", "main")
	return top, agent
}

func TestMergeDefaultInUpdatesAndPushes(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	_, agent := behindBranch(t, "main.txt", "branch.txt")
	head, err := mergeDefaultIn(agent, "001/a", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(agent.Top, "main.txt")); err != nil {
		t.Fatalf("the default branch must be merged in: %v", err)
	}
	remote, err := agent.git("rev-parse", "origin/001/a")
	if err != nil || remote != head {
		t.Fatalf("the update must be pushed: local %s, remote %s, %v", head, remote, err)
	}
}

func TestMergeDefaultInReturnsConflictsToTheAgent(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	_, agent := behindBranch(t, "same.txt", "same.txt")
	before, _ := agent.git("rev-parse", "HEAD")
	_, err := mergeDefaultIn(agent, "001/a", "main")
	var coded *flowError
	if !errors.As(err, &coded) || coded.Code != "merge-conflict" || !strings.Contains(coded.Next, "resolve the conflicts") {
		t.Fatalf("want merge-conflict with a next step, got %v", err)
	}
	after, _ := agent.git("rev-parse", "HEAD")
	status, _ := agent.git("status", "--porcelain")
	if before != after || status != "" {
		t.Fatalf("the worktree must be as it was: %s %s %q", before, after, status)
	}
	if _, err := mergeDefaultIn(agent, "other/branch", "main"); !errors.As(err, &coded) || coded.Code != "merge-blocked" {
		t.Fatalf("another branch: %v", err)
	}
}

func TestGuideWithEveryFlagOffIsTheHandbook(t *testing.T) {
	// The handbook is rendered, not read from this repository's own rules
	// file: that file follows this repository's flags, which the owner may turn on.
	handbook := renderGuide()
	// Settings that predate the flags change nothing either.
	for _, c := range []model.Config{{}, {IssuesBy: "owner", TriageBy: "owner", UpdateBehind: "off", MergeWait: "foreground", GuardReads: "closed"}, {Merge: "agent", ReadyBy: "agents", CommitFormat: "issue-prefix"}} {
		if renderGuideFor(c) != handbook {
			t.Errorf("%+v: the guide differs from the handbook", c)
		}
	}
	for _, flagOnly := range []string{"merge-conflict", "issues_by", "triage_by", "update_behind", "merge_wait", "guard_reads", "quipu triage", "Flags on in this repository"} {
		if strings.Contains(handbook, flagOnly) {
			t.Errorf("with every flag off the handbook must not mention %q", flagOnly)
		}
	}
}

func TestGuideStatesTheRulesOfEachFlagOn(t *testing.T) {
	for name, tc := range map[string]struct {
		c    model.Config
		want []string
	}{
		"issues_by":     {model.Config{IssuesBy: "agents"}, []string{"quipu issue create", "New Epics stay proposals"}},
		"triage_by":     {model.Config{TriageBy: "agents"}, []string{"quipu triage N", "never decide", "never closes"}},
		"update_behind": {model.Config{Merge: "agent", UpdateBehind: "auto"}, []string{"merges the default branch in", "| `merge-conflict` |"}},
		"merge_wait":    {model.Config{Merge: "agent", MergeWait: "background"}, []string{"returns at once", "returns to Ready"}},
		"guard_reads":   {model.Config{GuardReads: "open"}, []string{"Interpreters stay denied"}},
	} {
		text := renderGuideFor(tc.c)
		if !strings.HasPrefix(text, renderGuide()) {
			t.Errorf("%s: a flag only adds rules", name)
		}
		for _, want := range tc.want {
			if !strings.Contains(text, want) {
				t.Errorf("%s: missing %q", name, want)
			}
		}
	}
	if strings.Contains(renderGuideFor(model.Config{UpdateBehind: "auto"}), "merge-conflict") {
		t.Error("update_behind does nothing without merge: agent, so the guide must not state it")
	}
}

func TestDoctorFlagsAStaleHandbook(t *testing.T) {
	on := model.Config{GuardReads: "open"}
	if got := handbookCheck(renderGuideFor(on), true, on); got.Status != "ok" {
		t.Errorf("current: %+v", got)
	}
	if got := handbookCheck(renderGuide(), true, on); got.Status != "warn" || !strings.Contains(got.Fix, "quipu guide --sync") {
		t.Errorf("a flag turned on: %+v", got)
	}
	if got := handbookCheck(renderGuideFor(on), true, model.Config{}); got.Status != "warn" {
		t.Errorf("a flag turned off: %+v", got)
	}
	if got := handbookCheck("", false, on); got.Status != "warn" {
		t.Errorf("missing: %+v", got)
	}
}

func TestSyncWritesTheEnforcedFlagsIntoTheHandbook(t *testing.T) {
	_, agent := flaggedRepo(t, "guard_reads: open\n")
	files := guideFilesFor(guideConfig(agent.Top), agent.Top, readUnder(agent.Top))
	if !strings.Contains(files[guideRule], "guard_reads: open") {
		t.Fatal("the repository's handbook states its flags")
	}
	t.Run("a worktree copy changes nothing", func(t *testing.T) {
		_, plain := flaggedRepo(t, "")
		writeRepoFile(t, plain.Top, rootConfigPath, append(repoConfig(t), "guard_reads: open\n"...))
		if strings.Contains(renderGuideFor(guideConfig(plain.Top)), "Flags on in this repository") {
			t.Fatal("a worktree copy must not change the rules agents read")
		}
	})
}

func TestMergeQueueRequestLifecycle(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	evidence := &writer.Evidence{FinalSHA: pendingMerge, CIURL: pendingMerge, Commands: "x", Environments: "x", Criteria: "x", Artifacts: "x", Documentation: "x", Risks: "x", Boundary: "x"}
	inReview := writer.WorkState{Status: "In review", Lease: writer.Lease{Holder: "a", Branch: "007/work", Expires: now.Add(-time.Hour)}}
	request := writer.Request{Action: "merge.queue", Actor: "a", PR: "url", Head: "abc", Evidence: evidence}
	next, err := writer.ApplyLifecycle(inReview, request, now)
	if err != nil {
		t.Fatalf("a lapsed lease in review can still queue: %v", err)
	}
	if next.Status != "In review" || next.Lease.Holder != "a" {
		t.Fatalf("queueing changes nothing on the board: %+v", next)
	}
	wrongHolder := request
	wrongHolder.Actor = "b"
	if _, err := writer.ApplyLifecycle(inReview, wrongHolder, now); err == nil {
		t.Fatal("only the holder queues")
	}
	if _, err := writer.ApplyLifecycle(writer.WorkState{Status: "In progress", Lease: inReview.Lease}, request, now); err == nil {
		t.Fatal("only work in review queues")
	}
	missing := request
	missing.Evidence = &writer.Evidence{FinalSHA: pendingMerge}
	if _, err := writer.ApplyLifecycle(inReview, missing, now); err == nil {
		t.Fatal("the Writer holds complete evidence")
	}
	noHead := request
	noHead.Head = ""
	if _, err := writer.ApplyLifecycle(inReview, noHead, now); err == nil {
		t.Fatal("a queued merge is pinned to a head")
	}
}
