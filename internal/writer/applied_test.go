package writer

import (
	"testing"
	"time"
)

func TestAlreadyApplied(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	const me, other = "claude/octo/aaaa1111", "claude/octo/bbbb2222"
	held := func(status, holder string) WorkState {
		return WorkState{Status: status, Lease: Lease{Holder: holder, Branch: "001/a", Expires: now.Add(DefaultLeaseTTL)}}
	}
	free := func(status string) WorkState { return WorkState{Status: status, Lease: Lease{Branch: "001/a"}} }
	evidence := &Evidence{FinalSHA: "sha", CIURL: "c", Commands: "c", Environments: "e", Criteria: "c", Artifacts: "a", Documentation: "d", Risks: "r", Boundary: "CI"}
	claimed := Receipt{RequestID: "1", Result: "accepted", At: now.Add(-time.Hour), State: &ReceiptState{Status: "Claimed", Holder: me}}
	recorded := Receipt{RequestID: "2", Result: "accepted", At: now.Add(-time.Minute), Evidence: evidence}
	queued := Receipt{RequestID: "3", Result: "accepted", At: now.Add(-time.Minute), Merge: &QueuedMerge{PR: "pr", Head: "head"}}
	imported := Receipt{RequestID: "4", Result: "accepted", At: now, Detail: "imported K as Ready"}

	for name, tc := range map[string]struct {
		state    WorkState
		request  Request
		receipts []Receipt
		want     bool
	}{
		"claim, mine":                {held("Claimed", me), Request{Action: "claim", Actor: me, Branch: "001/a"}, nil, true},
		"claim, already started":     {held("In progress", me), Request{Action: "claim", Actor: me, Branch: "001/a"}, nil, true},
		"claim, other holder":        {held("Claimed", other), Request{Action: "claim", Actor: me, Branch: "001/a"}, nil, false},
		"claim, other branch":        {held("Claimed", me), Request{Action: "claim", Actor: me, Branch: "002/b"}, nil, false},
		"claim, Ready":               {free("Ready"), Request{Action: "claim", Actor: me, Branch: "001/a"}, nil, false},
		"claim, Done":                {held("Done", me), Request{Action: "claim", Actor: me, Branch: "001/a"}, nil, false},
		"start, mine":                {held("In progress", me), Request{Action: "start", Actor: me}, nil, true},
		"start, other holder":        {held("In progress", other), Request{Action: "start", Actor: me}, nil, false},
		"start, still Claimed":       {held("Claimed", me), Request{Action: "start", Actor: me}, nil, false},
		"renew, just renewed":        {held("In progress", me), Request{Action: "renew", Actor: me}, nil, true},
		"renew, other holder":        {held("In progress", other), Request{Action: "renew", Actor: me}, nil, false},
		"renew, running out":         {WorkState{Status: "In progress", Lease: Lease{Holder: me, Expires: now.Add(time.Minute)}}, Request{Action: "renew", Actor: me}, nil, false},
		"pr.link, in review":         {held("In review", me), Request{Action: "pr.link", Actor: me, PR: "pr"}, nil, true},
		"pr.link, done":              {held("Done", me), Request{Action: "pr.link", Actor: me, PR: "pr"}, nil, true},
		"pr.link, other holder":      {held("In review", other), Request{Action: "pr.link", Actor: me, PR: "pr"}, nil, false},
		"pr.link, in progress":       {held("In progress", me), Request{Action: "pr.link", Actor: me, PR: "pr"}, nil, false},
		"evidence, recorded":         {held("Evidence pending", me), Request{Action: "evidence.submit", Actor: me, Evidence: evidence}, []Receipt{claimed, recorded}, true},
		"evidence, already done":     {held("Done", me), Request{Action: "evidence.submit", Actor: me, Evidence: evidence}, []Receipt{claimed, recorded}, true},
		"evidence, other SHA":        {held("Evidence pending", me), Request{Action: "evidence.submit", Actor: me, Evidence: &Evidence{FinalSHA: "new"}}, []Receipt{claimed, recorded}, false},
		"evidence, earlier claim":    {held("Evidence pending", me), Request{Action: "evidence.submit", Actor: me, Evidence: evidence}, []Receipt{recorded, {RequestID: "9", Result: "accepted", At: now, State: &ReceiptState{Status: "Claimed", Holder: me}}}, false},
		"evidence, other holder":     {held("Evidence pending", other), Request{Action: "evidence.submit", Actor: me, Evidence: evidence}, []Receipt{claimed, recorded}, false},
		"evidence, none":             {held("Evidence pending", me), Request{Action: "evidence.submit", Actor: me}, []Receipt{claimed, recorded}, false},
		"merge.queue, same":          {held("In review", me), Request{Action: "merge.queue", Actor: me, PR: "pr", Head: "head"}, []Receipt{queued}, true},
		"merge.queue, new head":      {held("In review", me), Request{Action: "merge.queue", Actor: me, PR: "pr", Head: "moved"}, []Receipt{queued}, false},
		"merge.queue, none":          {held("In review", me), Request{Action: "merge.queue", Actor: me, PR: "pr", Head: "head"}, nil, false},
		"merge.queue, other holder":  {held("In review", other), Request{Action: "merge.queue", Actor: me, PR: "pr", Head: "head"}, []Receipt{queued}, false},
		"status, Ready to Ready":     {free("Ready"), Request{Action: "status", Status: "Ready", Actor: "octo"}, nil, true},
		"status, Done to Done":       {held("Done", me), Request{Action: "status", Status: "Done", Actor: me}, nil, true},
		"status, Done, other holder": {held("Done", other), Request{Action: "status", Status: "Done", Actor: me}, nil, false},
		"status, a real move":        {free("Backlog"), Request{Action: "status", Status: "Ready", Actor: "octo"}, nil, false},
		"status, never Claimed":      {held("Claimed", me), Request{Action: "status", Status: "Claimed", Actor: me}, nil, false},
		"release, released":          {free("Ready"), Request{Action: "release", Actor: me}, nil, true},
		"release, still held":        {held("In progress", me), Request{Action: "release", Actor: me}, nil, false},
		"resume, resumed":            {free("Ready"), Request{Action: "resume", Actor: me}, nil, true},
		"block, blocked":             {free("Blocked"), Request{Action: "block", Actor: me}, nil, true},
		"block, not yet":             {held("In progress", me), Request{Action: "block", Actor: me}, nil, false},
		"import, recorded":           {free("Ready"), Request{Action: "import", Actor: "octo", Import: &Import{Key: "K", Status: "Ready"}}, []Receipt{imported}, true},
		"import, another":            {free("Ready"), Request{Action: "import", Actor: "octo", Import: &Import{Key: "L", Status: "Ready"}}, []Receipt{imported}, false},
		"unknown action":             {free("Ready"), Request{Action: "nonsense", Actor: "octo"}, nil, false},
	} {
		if got := AlreadyApplied(tc.state, tc.request, tc.receipts, now); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func TestUnanswered(t *testing.T) {
	pending, _ := RenderRequest(Request{ID: "1", Action: "claim", IssueID: "I", Actor: "a", ExpectedFingerprint: "f"})
	answered, _ := RenderRequest(Request{ID: "2", Action: "claim", IssueID: "I", Actor: "a", ExpectedFingerprint: "f"})
	receipt, _ := RenderReceipt(Receipt{RequestID: "2", Result: "accepted"})
	got := Unanswered([]string{pending, answered, receipt, "just a comment"})
	if len(got) != 1 || got[0] != "1" {
		t.Fatalf("got %v", got)
	}
}

func TestMergeBatchLifecycle(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	e := &Evidence{FinalSHA: "p", CIURL: "p", Commands: "c", Environments: "e", Criteria: "c", Artifacts: "a", Documentation: "d", Risks: "r", Boundary: "local"}
	held := Lease{Holder: "claude/octo/x", Branch: "1/a", Expires: now.Add(time.Hour)}
	queue := Request{Action: "merge.batch", Actor: "claude/octo/x", PR: "u", Head: "h", Evidence: e}
	for _, status := range []string{"In progress", "In review"} {
		next, err := ApplyLifecycle(WorkState{Status: status, Lease: held}, queue, now)
		if err != nil || next.Status != "In review" || next.Lease.Holder != held.Holder {
			t.Fatalf("%s: %+v, %v", status, next, err)
		}
	}
	for name, tc := range map[string]struct {
		current WorkState
		request Request
	}{
		"not the holder": {WorkState{Status: "In progress", Lease: held}, Request{Action: "merge.batch", Actor: "claude/octo/y", PR: "u", Head: "h", Evidence: e}},
		"not started":    {WorkState{Status: "Ready"}, queue},
		"no evidence":    {WorkState{Status: "In progress", Lease: held}, Request{Action: "merge.batch", Actor: held.Holder, PR: "u", Head: "h"}},
		"no head":        {WorkState{Status: "In progress", Lease: held}, Request{Action: "merge.batch", Actor: held.Holder, PR: "u", Evidence: e}},
		"return, no why": {WorkState{Status: "In review", Lease: held}, Request{Action: "queue.return", Actor: held.Holder, PR: "u"}},
		"return, Ready":  {WorkState{Status: "Ready", Lease: held}, Request{Action: "queue.return", Actor: held.Holder, PR: "u", Reason: "x"}},
		"another login":  {WorkState{Status: "In review", Lease: held}, Request{Action: "takeover", Actor: "claude/other/x", Branch: "1/a"}},
		"another branch": {WorkState{Status: "In review", Lease: held}, Request{Action: "takeover", Actor: "codex/octo/y", Branch: "2/b"}},
		"not in review":  {WorkState{Status: "In progress", Lease: held}, Request{Action: "takeover", Actor: "codex/octo/y", Branch: "1/a"}},
	} {
		if _, err := ApplyLifecycle(tc.current, tc.request, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	next, err := ApplyLifecycle(WorkState{Status: "In review", Lease: held}, Request{Action: "takeover", Actor: "codex/octo/y", Branch: "1/a"}, now)
	if err != nil || next.Lease.Holder != "codex/octo/y" || next.Status != "In review" || !next.Lease.Expires.After(now) {
		t.Fatalf("%+v, %v", next, err)
	}
	if next, err := ApplyLifecycle(WorkState{Status: "In review", Lease: held}, Request{Action: "queue.return", Actor: held.Holder, PR: "u", Reason: "x"}, now); err != nil || next.Lease.Holder != held.Holder {
		t.Fatalf("%+v, %v", next, err)
	}
}

func TestQueueEntriesEndAndReturn(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	queued := Receipt{RequestID: "q", Result: "accepted", At: at, Merge: &QueuedMerge{PR: "u", Head: "h", Batch: true}}
	if _, ok := LatestQueuedMerge([]Receipt{queued}); !ok {
		t.Fatal("queued")
	}
	if _, returned := ReturnedFromQueue([]Receipt{queued}); returned {
		t.Fatal("not returned yet")
	}
	back := Receipt{RequestID: "r", Result: "accepted", At: at.Add(time.Minute), Detail: "why", EndsQueue: "q"}
	if _, ok := LatestQueuedMerge([]Receipt{queued, back}); ok {
		t.Fatal("it left the queue")
	}
	if got, returned := ReturnedFromQueue([]Receipt{queued, back}); !returned || got.Detail != "why" {
		t.Fatalf("%+v %v", got, returned)
	}
	again := Receipt{RequestID: "q2", Result: "accepted", At: at.Add(2 * time.Minute), Merge: &QueuedMerge{PR: "u", Head: "h2", Batch: true}}
	if got, ok := LatestQueuedMerge([]Receipt{queued, back, again}); !ok || got.Merge.Head != "h2" {
		t.Fatalf("queued again: %+v %v", got, ok)
	}
	if _, returned := ReturnedFromQueue([]Receipt{queued, back, again}); returned {
		t.Fatal("queued again means not returned")
	}
	if !SameLogin("claude/octo/a", "codex/octo/b") || !SameLogin("octo", "octo") || SameLogin("claude/octo/a", "claude/other/a") {
		t.Fatal("logins")
	}
	if !AlreadyApplied(WorkState{Status: "In review", Lease: Lease{Holder: "h"}}, Request{Action: "queue.return", Actor: "h"}, []Receipt{queued, back}, at) {
		t.Fatal("a repeated return changes nothing")
	}
}
