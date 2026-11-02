package main

// triage_by: agents and issues_by: agents. Agents may bring Issues into the
// flow, but never finish work or decide anything: `quipu triage` cannot close
// an Issue, and Ready still needs ready_by: agents.

import (
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
)

// triageStatuses are the statuses triage may set. Done, Cancelled and the
// lifecycle in between stay with the flow and the owner.
var triageStatuses = map[string]string{"backlog": "Backlog", "ready": "Ready", "blocked": "Blocked"}

// triageKinds are the kinds triage may give an Issue that has none. An agent
// may file a Decision or Gate, and only the owner decides one.
var triageKinds = setOf("story", "task", "defect", "contract", "decision", "gate")

type triageOptions struct {
	Kind, Status, Priority, Area string
	Parent                       int
}

// checkTriage refuses what triage never does, before anything changes.
func checkTriage(c model.Config, number int, o triageOptions, labels []string) error {
	if !c.AgentsTriage() {
		return errors.New("triage needs triage_by: agents on the default branch")
	}
	if !hasLabel(labels, triageLabel) {
		return fmt.Errorf("#%d does not have the %s label, so it needs no triage", number, triageLabel)
	}
	if o.Kind != "" {
		if !triageKinds[o.Kind] {
			return fmt.Errorf("--kind must be story, task, defect, contract, decision or gate; Epics stay proposals")
		}
		if current := labelKind(labels); current != "" {
			return fmt.Errorf("#%d already has kind %s; only the owner changes it", number, current)
		}
	}
	if o.Status != "" {
		status, ok := triageStatuses[strings.ToLower(o.Status)]
		if !ok {
			return fmt.Errorf("--status must be backlog, ready or blocked; Done, Cancelled and closing are never triage")
		}
		if status == "Ready" && !c.AgentsMakeReady() {
			return errors.New("making work Ready needs ready_by: agents; only the owner makes work Ready here")
		}
	}
	if o.Priority != "" && !map[string]bool{"P0": true, "P1": true, "P2": true, "P3": true}[o.Priority] {
		return errors.New("--priority must be P0, P1, P2, or P3")
	}
	if o.Area != "" && !contains(c.Areas, o.Area) {
		return fmt.Errorf("area %q is not configured", o.Area)
	}
	if o.Parent < 0 || o.Parent == number {
		return errors.New("--parent must be another Issue")
	}
	return nil
}

// labelValue returns the value of the first label with prefix, without it.
func labelValue(labels []string, prefix string) string {
	for _, label := range labels {
		if strings.HasPrefix(label, prefix) {
			return strings.TrimPrefix(label, prefix)
		}
	}
	return ""
}

// triageCommand is `quipu triage N`. It reads its config from the default
// branch, so a worktree's copy cannot turn it on.
func triageCommand(args []string) error {
	fs := flag.NewFlagSet("triage", flag.ContinueOnError)
	o := triageOptions{}
	fs.StringVar(&o.Kind, "kind", "", "story|task|defect|contract|decision|gate, for an Issue with no kind")
	fs.StringVar(&o.Status, "status", "", "backlog|ready|blocked")
	fs.StringVar(&o.Priority, "priority", "", "P0|P1|P2|P3")
	fs.StringVar(&o.Area, "area", "", "configured area")
	fs.IntVar(&o.Parent, "parent", 0, "native parent Issue number")
	wait := fs.Duration("wait", 3*time.Minute, "how long to wait for the Writer receipt")
	asJSON := jsonFlag(fs)
	number, err := parseIssueArg(fs, args, false)
	if err != nil {
		return errors.New("usage: quipu triage N [--kind K] [--status S] [--priority P] [--area A] [--parent M] [--json]")
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	c, _, err := loadEnforcedConfig(here, true)
	if err != nil {
		return err
	}
	state, err := loadLiveWork(c, number)
	inProject := !errors.Is(err, errNotInProject)
	if err != nil && inProject {
		return err
	}
	if err := checkTriage(c, number, o, state.Labels); err != nil {
		return err
	}
	repo := c.Owner + "/" + c.Repository
	edit := []string{"issue", "edit", strconv.Itoa(number), "--repo", repo}
	if o.Kind != "" {
		edit = append(edit, "--add-label", "kind:"+o.Kind)
	}
	currentArea := labelValue(state.Labels, "area:")
	if o.Area != "" && o.Area != currentArea {
		edit = append(edit, "--add-label", "area:"+o.Area)
		if currentArea != "" {
			edit = append(edit, "--remove-label", "area:"+currentArea)
		}
	}
	if len(edit) > 5 {
		if _, err := ghOutput(edit...); err != nil {
			return err
		}
	}
	if o.Parent > 0 {
		if err := linkParent(c, o.Parent, number); err != nil {
			return fmt.Errorf("link #%d under #%d: %w", number, o.Parent, err)
		}
	}
	if !inProject {
		kind, area, priority := o.Kind, o.Area, o.Priority
		if kind == "" {
			if kind = strings.ToLower(labelKind(state.Labels)); kind == "" {
				return fmt.Errorf("#%d has no kind; rerun with --kind so it can join the Project", number)
			}
		}
		if area == "" {
			if area = currentArea; area == "" {
				return fmt.Errorf("#%d has no area; rerun with --area so it can join the Project", number)
			}
		}
		if priority == "" {
			priority = "P2"
		}
		url := fmt.Sprintf("https://github.com/%s/issues/%d", repo, number)
		if err := addIssueToProject(c, url, titleCase(kind), titleCase(area), priority); err != nil {
			return fmt.Errorf("add #%d to the Project: %w", number, err)
		}
	} else if o.Priority != "" || o.Area != "" {
		values := map[string]string{}
		if o.Priority != "" {
			values["Priority"] = o.Priority
		}
		if o.Area != "" {
			values["Area"] = titleCase(o.Area)
		}
		if err := setProjectOptions(c, state.ItemID, values); err != nil {
			return err
		}
	}
	status := state.Status
	if o.Status != "" {
		actor, err := resolveActor(here, "", number)
		if err != nil {
			return err
		}
		status = triageStatuses[strings.ToLower(o.Status)]
		if err := sendAndWait(c, number, actor, writer.Request{Action: "status", Status: status}, *wait); err != nil {
			return err
		}
	}
	return printFlowResult(flowResult{Issue: number, Status: status, Next: fmt.Sprintf("The Writer removes %s on its next pass if #%d is now valid. A work contract is still the Issue author's to fix.", triageLabel, number)}, *asJSON)
}

// agentIssueProblem says why an agent may not run this `quipu issue create`, or
// "" when it may. Every occurrence of a flag is judged, and anything computed
// is refused, so what the command finally reads was checked here.
func agentIssueProblem(c model.Config, args []shellArg, parentKind func(int) (string, error)) string {
	if !c.AgentsCreateIssues() && !c.AgentsTriage() {
		return "agents create Issues only with issues_by: agents on the default branch"
	}
	var kinds, parents []string
	for i, arg := range args {
		if !arg.ok {
			return "a computed argument cannot be checked"
		}
		name, value, inline := strings.Cut(strings.TrimLeft(arg.s, "-"), "=")
		if !strings.HasPrefix(arg.s, "-") || (name != "kind" && name != "parent") {
			continue
		}
		if !inline {
			if i+1 >= len(args) || !args[i+1].ok {
				return "--" + name + " needs a literal value"
			}
			value = args[i+1].s
		}
		if name == "kind" {
			kinds = append(kinds, value)
		} else {
			parents = append(parents, value)
		}
	}
	if len(kinds) == 0 || len(parents) == 0 {
		return "pass --kind and --parent"
	}
	for _, kind := range kinds {
		switch kind {
		case "story", "task", "defect":
			if !c.AgentsCreateIssues() {
				return kind + " needs issues_by: agents"
			}
		case "decision", "gate":
			if !c.AgentsTriage() {
				return kind + " needs triage_by: agents"
			}
		default:
			return "agents may not create a " + kind + "; suggest it with quipu propose"
		}
	}
	for _, value := range parents {
		parent, err := strconv.Atoi(value)
		if err != nil || parent < 1 {
			return "--parent must be an Issue number"
		}
		kind, err := parentKind(parent)
		if err != nil {
			return "could not read parent #" + value + ": " + err.Error()
		}
		if kind != "Epic" {
			return "parent #" + value + " must be an open Epic, not " + kind
		}
	}
	return ""
}

// openIssueKind returns an open Issue's kind, and refuses a closed one.
func openIssueKind(c model.Config, number int) (string, error) {
	output, err := ghOutput("issue", "view", strconv.Itoa(number), "--repo", c.Owner+"/"+c.Repository, "--json", "state,labels", "--jq", `.state + " " + ([.labels[].name] | join(","))`)
	if err != nil {
		return "", err
	}
	state, names, _ := strings.Cut(strings.TrimSpace(string(output)), " ")
	if state != "OPEN" {
		return "a closed Issue", nil
	}
	if kind := labelKind(strings.Split(names, ",")); kind != "" {
		return kind, nil
	}
	return "an Issue with no kind", nil
}
