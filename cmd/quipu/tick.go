package main

// quipu tick marks an acceptance criterion met on the Issue the agent holds,
// so the Issue shows how far the work got even if it stops part way. The
// Writer flips that one checkbox and nothing else in the body.

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

func tickCommand(args []string) error {
	fs := flag.NewFlagSet("tick", flag.ContinueOnError)
	criterion := fs.Int("criterion", 0, "the acceptance criterion to mark met, counting from 1")
	note := fs.String("note", "", "what you did for it, or what comes next")
	undo := fs.Bool("undo", false, "mark the criterion not met again")
	wait := fs.Duration("wait", 3*time.Minute, "how long to wait for the Writer receipt")
	asJSON := jsonFlag(fs)
	number, err := parseIssueArg(fs, args, false)
	if err != nil || *criterion == 0 {
		return keepHelp(err, "usage: quipu tick N --criterion K [--note TEXT] [--undo] [--json]")
	}
	here, c, err := choreSetup()
	if err != nil {
		return err
	}
	state, err := loadLiveWork(c, number)
	if err != nil {
		return err
	}
	action, done := "tick", true
	if *undo {
		action, done = "untick", false
	}
	criteria := model.Criteria(state.Body)
	if *criterion < 1 || *criterion > len(criteria) {
		return flowErr("no-criterion", "#%d has %d acceptance criteria; there is no criterion %d", number, len(criteria), *criterion)
	}
	if !writer.TickableStatus(state.Status) {
		return flowErr("not-active", "#%d is %s; criteria are ticked while work is in progress or in review", number, state.Status)
	}
	actor, err := resolveActor(here, "", number)
	if err != nil {
		return err
	}
	if state.Holder != actor {
		return flowErr("rejected-holder", "#%d is held by %q, not %q", number, state.Holder, actor)
	}
	result := flowResult{Issue: number, Status: "Ticked"}
	if !done {
		result.Status = "Unticked"
	}
	if criteria[*criterion-1].Done != done || strings.TrimSpace(*note) != "" {
		r := writer.Request{Action: action, Criterion: *criterion, Note: strings.TrimSpace(*note)}
		if err := sendAndWait(c, number, actor, r, *wait); err != nil {
			return err
		}
	}
	after, _ := model.TickCriterion(state.Body, *criterion, done)
	met, total := model.CriteriaDone(after)
	verb := "met"
	if !done {
		verb = "not met"
	}
	result.Next = fmt.Sprintf("#%d: criterion %d %s; %d of %d met. Tick each criterion once its change is committed and pushed.", number, *criterion, verb, met, total)
	return printFlowResult(result, *asJSON)
}
