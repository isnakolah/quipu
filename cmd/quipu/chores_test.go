package main

import (
	"errors"
	"strings"
	"testing"

	gh "github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

// stubIssueKind makes the guard read every Issue as kind, without GitHub.
func stubIssueKind(t *testing.T, kind string) {
	t.Helper()
	saved := guardIssueKind
	guardIssueKind = func(model.Config, int) (string, error) { return kind, nil }
	t.Cleanup(func() { guardIssueKind = saved })
}

// fakeChores records every write a chore makes.
type fakeChores struct {
	work   map[int]liveWork
	pr     prView
	merged map[int][]string
	writes []string
}

func (f *fakeChores) ops() choreOps {
	return choreOps{
		state: func(number int) (liveWork, error) {
			if work, ok := f.work[number]; ok {
				return work, nil
			}
			return liveWork{}, errNotInProject
		},
		assign: func(number int, login string) error {
			f.writes = append(f.writes, "assign "+login)
			return nil
		},
		edit: func(number int, body string) error {
			f.writes = append(f.writes, "edit")
			return nil
		},
		viewPR:    func(string) (prView, error) { return f.pr, nil },
		mergedFor: func(number int) ([]string, error) { return f.merged[number], nil },
		comment: func(pr int, body string) error {
			f.writes = append(f.writes, "comment "+body)
			return nil
		},
		closePR: func(url string) error {
			f.writes = append(f.writes, "close "+url)
			return nil
		},
	}
}

const choreActor = "claude/octo/a"

// writerAnswers runs one request through the Writer's path: posted as a comment,
// found pending, applied, and answered by one receipt that leaves nothing pending.
func writerAnswers(t *testing.T, c model.Config, state liveWork, r writer.Request, f *fakeChores) writer.Receipt {
	t.Helper()
	r.ID, r.IssueID, r.Actor, r.ExpectedFingerprint = "1", "I_7", choreActor, "f"
	body, err := writer.RenderRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	request := gh.Comment{ID: 1, Body: body}
	request.User.Login = "octo"
	pending, rejected := writer.Pending([]gh.Comment{request}, writer.Trust{})
	if len(pending) != 1 || len(rejected) != 0 {
		t.Fatalf("pending %v, rejected %v", pending, rejected)
	}
	receipt := applyChore(c, 7, state, pending[0].Request, "f", f.ops())
	rendered, err := writer.RenderReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	answer := gh.Comment{ID: 2, Body: rendered}
	answer.User.Login = writer.WriterLogin
	if pending, _ := writer.Pending([]gh.Comment{request, answer}, writer.Trust{}); len(pending) != 0 {
		t.Fatalf("still pending after the receipt: %v", pending)
	}
	if receipt.RequestID != "1" || receipt.State != nil {
		t.Fatalf("a chore's receipt answers its request and moves no state: %+v", receipt)
	}
	return receipt
}

func TestIssueChoresRefuseOutsideIssuesByAndSucceedInside(t *testing.T) {
	story := liveWork{IssueID: "I_7", IssueState: "OPEN", Status: "Ready", Labels: []string{"kind:story"}, Body: testContract}
	off, on := model.Config{}, model.Config{IssuesBy: "agents"}
	for name, r := range map[string]writer.Request{
		"assign": {Action: "assign", Assignee: "octo"},
		"edit":   {Action: "edit", Body: testContract},
	} {
		f := &fakeChores{}
		if got := writerAnswers(t, off, story, r, f); got.Result != "rejected" || !strings.Contains(got.Detail, "issues_by: agents") || len(f.writes) != 0 {
			t.Errorf("%s with issues_by off: %+v, wrote %v", name, got, f.writes)
		}
		f = &fakeChores{}
		if got := writerAnswers(t, on, story, r, f); got.Result != "accepted" || len(f.writes) != 1 {
			t.Errorf("%s with issues_by: agents: %+v, wrote %v", name, got, f.writes)
		}
	}
}

func TestIssueEditStillValidatesTheContract(t *testing.T) {
	on := model.Config{IssuesBy: "agents"}
	story := liveWork{IssueState: "OPEN", Status: "Ready", Labels: []string{"kind:story"}}
	held := story
	held.Status, held.Holder = "In progress", "claude/octo/b"
	for name, tc := range map[string]struct {
		state liveWork
		r     writer.Request
		want  string
	}{
		"not a contract":  {story, writer.Request{Action: "edit", Body: "just words"}, "not a valid work contract"},
		"an Epic":         {liveWork{IssueState: "OPEN", Labels: []string{"kind:epic"}}, writer.Request{Action: "edit", Body: testContract}, "only Stories, Tasks and Defects"},
		"a Decision":      {liveWork{IssueState: "OPEN", Labels: []string{"kind:decision"}}, writer.Request{Action: "edit", Body: testContract}, "only Stories, Tasks and Defects"},
		"closed":          {liveWork{IssueState: "CLOSED", Labels: []string{"kind:story"}}, writer.Request{Action: "edit", Body: testContract}, "closed"},
		"held by another": {held, writer.Request{Action: "edit", Body: testContract}, "held by"},
		"assign closed":   {liveWork{IssueState: "CLOSED"}, writer.Request{Action: "assign", Assignee: "octo"}, "closed"},
		"bad login":       {story, writer.Request{Action: "assign", Assignee: "not a login"}, "GitHub login"},
	} {
		f := &fakeChores{}
		got := writerAnswers(t, on, tc.state, tc.r, f)
		if got.Result != "rejected" || !strings.Contains(got.Detail, tc.want) || len(f.writes) != 0 {
			t.Errorf("%s: want %q, got %+v, wrote %v", name, tc.want, got, f.writes)
		}
	}
}

func TestPRCloseRefusesAPRAnIssueStillNeeds(t *testing.T) {
	const url = "https://github.com/o/r/pull/30"
	open := prView{Number: 30, URL: url, State: "OPEN", HeadRefName: "007/old", Body: "Refs #7\n\nRefs #8"}
	done := liveWork{Status: "Done", Branch: "007/new"}
	for name, tc := range map[string]struct {
		pr     prView
		seven  liveWork
		eight  liveWork
		merged map[int][]string
		want   string
	}{
		"In progress":         {open, liveWork{Status: "In progress", Branch: "007/new"}, done, nil, "#7 is still In progress"},
		"Claimed":             {open, liveWork{Status: "Claimed", Branch: "007/new"}, done, nil, "#7 is still Claimed"},
		"In review, no other": {open, liveWork{Status: "In review", Branch: "007/new"}, done, nil, "no other merged PR"},
		"only this PR merged": {open, liveWork{Status: "Ready"}, done, map[int][]string{7: {url}}, "no other merged PR"},
		"its live branch":     {open, liveWork{Status: "In review", Branch: "007/old"}, done, map[int][]string{7: {"https://github.com/o/r/pull/31"}}, "still In review"},
		"one of two needed":   {open, done, liveWork{Status: "Backlog"}, nil, "#8 is Backlog"},
		"merged PR":           {prView{Number: 30, URL: url, State: "MERGED", Body: "Refs #7"}, done, done, nil, "only an open PR"},
		"no reference":        {prView{Number: 30, URL: url, State: "OPEN", Body: "Fixes nothing"}, done, done, nil, "references no Issue"},
	} {
		f := &fakeChores{work: map[int]liveWork{7: tc.seven, 8: tc.eight}, pr: tc.pr, merged: tc.merged}
		got := writerAnswers(t, model.Config{}, tc.seven, writer.Request{Action: "pr.close", PR: url}, f)
		if got.Result != "rejected" || !strings.Contains(got.Detail, tc.want) || len(f.writes) != 0 {
			t.Errorf("%s: want %q, got %+v, wrote %v", name, tc.want, got, f.writes)
		}
	}
}

func TestPRCloseClosesAStrandedPRAndSaysWhy(t *testing.T) {
	const url = "https://github.com/o/r/pull/30"
	f := &fakeChores{
		work:   map[int]liveWork{7: {Status: "Done"}, 8: {Status: "Ready", Branch: "008/b"}},
		pr:     prView{Number: 30, URL: url, State: "OPEN", HeadRefName: "008/a", Body: "Refs #7\nRefs #8"},
		merged: map[int][]string{8: {url, "https://github.com/o/r/pull/31"}},
	}
	// pr close needs no setting: its conditions are its limit.
	got := writerAnswers(t, model.Config{}, f.work[7], writer.Request{Action: "pr.close", PR: url}, f)
	if got.Result != "accepted" || !strings.Contains(got.Detail, "#7 is Done; #8 has merged PR https://github.com/o/r/pull/31") {
		t.Fatalf("%+v", got)
	}
	if len(f.writes) != 2 || !strings.HasPrefix(f.writes[0], "comment ") || !strings.Contains(f.writes[0], "#7 is Done") || f.writes[1] != "close "+url {
		t.Fatalf("want the reason on the PR, then the close: %v", f.writes)
	}
	cancelled := &fakeChores{work: map[int]liveWork{7: {Status: "Cancelled"}}, pr: prView{Number: 30, URL: url, State: "OPEN", Body: "Refs #7"}}
	if got := writerAnswers(t, model.Config{}, cancelled.work[7], writer.Request{Action: "pr.close", PR: url}, cancelled); got.Result != "accepted" {
		t.Fatalf("Cancelled: %+v", got)
	}
	other := &fakeChores{pr: prView{Number: 30, URL: url, State: "OPEN", Body: "Refs #9"}}
	if got := writerAnswers(t, model.Config{}, liveWork{Status: "Done"}, writer.Request{Action: "pr.close", PR: url}, other); got.Result != "rejected" || !strings.Contains(got.Detail, "does not reference #7") {
		t.Fatalf("posted on an Issue the PR does not reference: %+v", got)
	}
}

func TestPRRefs(t *testing.T) {
	got := prRefs("Refs #7\nrefs #8 and Refs #7, Fixes #9")
	if len(got) != 2 || got[0] != 7 || got[1] != 8 {
		t.Fatalf("got %v", got)
	}
}

func TestAgentMoveProblem(t *testing.T) {
	kinds := map[int]string{1: "Epic", 2: "Story", 3: "a closed Issue", 4: "Decision", 5: "Gate"}
	kind := func(number int) (string, error) {
		if got, ok := kinds[number]; ok {
			return got, nil
		}
		return "", errors.New("not found")
	}
	literal := func(command string) []shellArg {
		var args []shellArg
		for _, word := range strings.Fields(command) {
			args = append(args, shellArg{s: word, ok: true})
		}
		return args
	}
	agents := model.Config{ReadyBy: "agents"}
	for name, tc := range map[string]struct {
		c       model.Config
		sub     string
		command string
		want    string
	}{
		"off":             {model.Config{}, "move", "2 ready", "ready_by: agents"},
		"ready off":       {model.Config{}, "ready", "2", "ready_by: agents"},
		"to Ready":        {agents, "move", "2 ready --json", ""},
		"to Backlog":      {agents, "move", "#2 Backlog", ""},
		"status first":    {agents, "move", "backlog 2 --wait 1m", ""},
		"ready":           {agents, "ready", "2 --json", ""},
		"to Blocked":      {agents, "move", "2 blocked", "only to backlog or ready"},
		"to Archived":     {agents, "move", "2 archived", "only to backlog or ready"},
		"two statuses":    {agents, "move", "2 ready backlog", "pass one Issue number"},
		"no status":       {agents, "move", "2", "pass one Issue number"},
		"two Issues":      {agents, "ready", "2 3", "pass one Issue number"},
		"an Epic":         {agents, "move", "1 ready", "owner moves Epics"},
		"a Decision":      {agents, "ready", "4", "owner moves Epics, Decisions and Gates"},
		"a Gate":          {agents, "move", "5 backlog", "owner moves"},
		"closed":          {agents, "move", "3 ready", "closed"},
		"unreadable":      {agents, "move", "9 ready", "could not read"},
		"wait value skip": {agents, "ready", "--wait 5 2", ""},
	} {
		got := agentMoveProblem(tc.c, tc.sub, literal(tc.command), kind)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: want %q, got %q", name, tc.want, got)
		}
	}
	computed := []shellArg{{s: "2", ok: true}, {}}
	if got := agentMoveProblem(agents, "move", computed, kind); got == "" {
		t.Error("a computed argument must be refused")
	}
}

func TestGuardAllowsChoresOnlyInsideTheirSettings(t *testing.T) {
	stubIssueKind(t, "Story")
	chores := []string{
		"quipu issue assign 7 octo --json",
		"quipu issue edit 7 --body-file contract.md --json",
		"quipu move 7 ready --json",
		"quipu move 7 backlog --json",
		"quipu ready 7 --json",
	}
	ownerOnly := []string{
		"quipu move 7 blocked",
		"quipu move 7 archived",
		"quipu pr link --issue-number 7 --issue-id I --fingerprint f --pr u --apply",
		"quipu issue link-parent --parent 1 --child 7 --apply",
		"quipu issue add-blocker --issue 7 --blocker 8 --apply",
		"quipu evidence submit --issue-number 7 --apply",
		"quipu writer run --issue-number 7 --apply",
		"quipu work release --issue-number 7 --apply",
		"gh pr close 30",
		"gh issue edit 7 --body x",
		"gh pr merge 30",
	}
	t.Run("flags off", func(t *testing.T) {
		_, off := flaggedRepo(t, "")
		for _, command := range chores {
			if code := guardCode(t, off.Top, "Bash", map[string]any{"command": command}); code == "" {
				t.Errorf("%q must be denied", command)
			}
		}
		if code := guardCode(t, off.Top, "Bash", map[string]any{"command": "quipu pr close 30 --json"}); code != "" {
			t.Errorf("pr close needs no setting, denied with %s", code)
		}
	})
	_, on := flaggedRepo(t, "issues_by: agents\nready_by: agents\n")
	for _, command := range append(chores, "quipu pr close 30 --json") {
		if code := guardCode(t, on.Top, "Bash", map[string]any{"command": command}); code != "" {
			t.Errorf("flags on: %q denied with %s", command, code)
		}
	}
	for _, command := range ownerOnly {
		if code := guardCode(t, on.Top, "Bash", map[string]any{"command": command}); code == "" {
			t.Errorf("flags on: %q must stay denied", command)
		}
	}
	stubIssueKind(t, "Epic")
	for _, command := range []string{"quipu move 7 ready", "quipu ready 7", "quipu move 7 backlog"} {
		if code := guardCode(t, on.Top, "Bash", map[string]any{"command": command}); code != "quipu-admin" {
			t.Errorf("an Epic: %q: want quipu-admin, got %q", command, code)
		}
	}
}
