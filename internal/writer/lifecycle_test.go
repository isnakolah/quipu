package writer

import (
	"strings"
	"testing"
	"time"
)

func TestApplyLifecycleClaimStartRenewRelease(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	request := Request{Action: "claim", Actor: "codex/isnakolah/session", Branch: "011/writer", LeaseExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
	state, err := ApplyLifecycle(WorkState{Status: "Ready"}, request, now)
	if err != nil || state.Status != "Claimed" || state.Lease.Holder != request.Actor {
		t.Fatalf("claim state=%+v err=%v", state, err)
	}
	request.Action = "start"
	state, err = ApplyLifecycle(state, request, now.Add(time.Minute))
	if err != nil || state.Status != "In progress" {
		t.Fatalf("start state=%+v err=%v", state, err)
	}
	request.Action = "renew"
	request.LeaseExpiresAt = now.Add(90 * time.Minute).Format(time.RFC3339)
	state, err = ApplyLifecycle(state, request, now.Add(2*time.Minute))
	if err != nil || !state.Lease.Expires.Equal(now.Add(90*time.Minute)) {
		t.Fatalf("renew state=%+v err=%v", state, err)
	}
	request.Action = "release"
	state, err = ApplyLifecycle(state, request, now.Add(3*time.Minute))
	if err != nil || state.Status != "Ready" || state.Lease.Holder != "" {
		t.Fatalf("release state=%+v err=%v", state, err)
	}
}

func TestApplyLifecycleRejectsInvalidOrContendedState(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	_, err := ApplyLifecycle(WorkState{Status: "Backlog"}, Request{Action: "claim", Actor: "a", Branch: "001/a"}, now)
	if err == nil || !strings.Contains(err.Error(), "Backlog -> Claimed") {
		t.Fatalf("err=%v", err)
	}
	_, err = ApplyLifecycle(WorkState{Status: "Ready", Lease: Lease{Holder: "other", Expires: now.Add(time.Hour)}}, Request{Action: "claim", Actor: "a", Branch: "001/a"}, now)
	if err == nil || !strings.Contains(err.Error(), "already leased") {
		t.Fatalf("err=%v", err)
	}
}

func TestApplyLifecycleRenewRetakesOwnExpiredLease(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	state, err := ApplyLifecycle(WorkState{Status: "In review", Lease: Lease{Holder: "a", Expires: now.Add(-time.Hour), Branch: "001/a"}}, Request{Action: "renew", Actor: "a"}, now)
	if err != nil || !state.Lease.Expires.After(now) || state.Lease.Branch != "001/a" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	_, err = ApplyLifecycle(WorkState{Status: "Claimed", Lease: Lease{Holder: "a", Expires: now}}, Request{Action: "renew", Actor: "b"}, now)
	if err == nil || !strings.Contains(err.Error(), "held by") {
		t.Fatalf("err=%v", err)
	}
	if _, err := ApplyLifecycle(WorkState{Status: "Ready"}, Request{Action: "renew", Actor: "a"}, now); err == nil {
		t.Fatal("renew of unleased work must fail")
	}
}

func TestApplyLifecycleRequiresHolderAndRealActions(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	held := Lease{Holder: "a", Expires: now.Add(time.Hour), Branch: "001/a"}
	if _, err := ApplyLifecycle(WorkState{Status: "Ready"}, Request{Action: "status", Status: "Claimed", Actor: "a"}, now); err == nil {
		t.Fatal("status Claimed must use claim")
	}
	if _, err := ApplyLifecycle(WorkState{Status: "Claimed", Lease: held}, Request{Action: "status", Status: "In progress", Actor: "a"}, now); err == nil {
		t.Fatal("status In progress from Claimed must use start")
	}
	if _, err := ApplyLifecycle(WorkState{Status: "Evidence pending", Lease: held}, Request{Action: "status", Status: "Done", Actor: "b"}, now); err == nil {
		t.Fatal("only the holder may finish leased work")
	}
	if state, err := ApplyLifecycle(WorkState{Status: "Evidence pending", Lease: held}, Request{Action: "status", Status: "Done", Actor: "a"}, now); err != nil || state.Status != "Done" {
		t.Fatalf("holder Done: %+v %v", state, err)
	}
	if _, err := ApplyLifecycle(WorkState{Status: "In progress", Lease: held}, Request{Action: "pr.link", PR: "https://x/pr/1", Actor: "b"}, now); err == nil {
		t.Fatal("only the holder may link a PR")
	}
	evidence := &Evidence{FinalSHA: "sha", CIURL: "ci", Commands: "go test", Environments: "CI", Criteria: "pass", Artifacts: "none", Documentation: "none", Risks: "none", Boundary: "CI"}
	if _, err := ApplyLifecycle(WorkState{Status: "In review", Lease: held}, Request{Action: "evidence.submit", PR: "https://x/pr/1", Evidence: evidence, Actor: "b"}, now); err == nil {
		t.Fatal("only the holder may submit evidence")
	}
	if state, err := ApplyLifecycle(WorkState{Status: "Backlog"}, Request{Action: "status", Status: "Ready", Actor: "owner"}, now); err != nil || state.Status != "Ready" {
		t.Fatalf("unleased status change: %+v %v", state, err)
	}
}

func TestApplyLifecycleInitialBacklogStatus(t *testing.T) {
	state, err := ApplyLifecycle(WorkState{}, Request{Action: "status", Status: "Backlog"}, time.Now())
	if err != nil || state.Status != "Backlog" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	// The owner may send unstarted work back to Backlog, never work underway.
	if state, err := ApplyLifecycle(WorkState{Status: "Ready"}, Request{Action: "status", Status: "Backlog"}, time.Now()); err != nil || state.Status != "Backlog" {
		t.Fatalf("Ready -> Backlog: state=%+v err=%v", state, err)
	}
	if _, err := ApplyLifecycle(WorkState{Status: "In progress"}, Request{Action: "status", Status: "Backlog"}, time.Now()); err == nil {
		t.Fatal("expected In progress -> Backlog rejection")
	}
}

func TestReclaimExpiredPreservesBranch(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	next, reclaimed := ReclaimExpired(WorkState{Status: "In progress", Lease: Lease{Holder: "a", Expires: now, Branch: "011/work"}}, now)
	if !reclaimed || next.Status != "Ready" || next.Lease.Holder != "" || next.Lease.Branch != "011/work" {
		t.Fatalf("next=%+v reclaimed=%t", next, reclaimed)
	}
}

func TestApplyLifecycleEvidenceRequiresCompletePayload(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	evidence := &Evidence{FinalSHA: "sha", CIURL: "ci", Commands: "go test", Environments: "CI", Criteria: "pass", Artifacts: "none", Documentation: "none", Risks: "none", Boundary: "CI"}
	state, err := ApplyLifecycle(WorkState{Status: "In review"}, Request{Action: "evidence.submit", PR: "https://example.test/pr/1", Evidence: evidence}, now)
	if err != nil || state.Status != "Evidence pending" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	_, err = ApplyLifecycle(WorkState{Status: "In review"}, Request{Action: "evidence.submit", Evidence: evidence}, now)
	if err == nil || !strings.Contains(err.Error(), "pull request") {
		t.Fatalf("err=%v", err)
	}
}

func TestApplyLifecyclePRLinkMovesReview(t *testing.T) {
	now := time.Now()
	state, err := ApplyLifecycle(WorkState{Status: "In progress", Lease: Lease{Holder: "a", Expires: now.Add(time.Hour)}}, Request{Action: "pr.link", PR: "https://example.test/pr/1", Actor: "a"}, now)
	if err != nil || state.Status != "In review" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}
