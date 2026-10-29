package main

// `quipu writer defect --run-id ID` runs in the Writer workflow when a
// watched workflow fails on the default branch. It files one P0 Defect per
// run, under the Epic of the Issue whose PR the failing commit merged, and
// records it Ready so an agent picks it up first.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/model"
	policycheck "github.com/isnakolah/quipu/internal/policy"
	"github.com/isnakolah/quipu/internal/writer"
)

var defectMarker = regexp.MustCompile(`<!-- quipu-defect:v1 run=([0-9]+) -->`)

type runFacts struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	Event      string `json:"event"`
	URL        string `json:"html_url"`
	Attempt    int    `json:"run_attempt"`
}

// defectSkip says why a run gets no Defect, or "" when it gets one.
func defectSkip(c model.Config, run runFacts) string {
	switch {
	case run.Conclusion != "failure" && run.Conclusion != "timed_out" && run.Conclusion != "startup_failure":
		return fmt.Sprintf("run concluded %q, not a failure", run.Conclusion)
	case run.HeadBranch != c.DefaultBranch:
		return fmt.Sprintf("run is on %q, not the default branch %q", run.HeadBranch, c.DefaultBranch)
	case run.Event == "pull_request" || run.Event == "pull_request_target":
		return "run is a pull request run"
	case !contains(c.Workflows.DefectWorkflows(), run.Name):
		return fmt.Sprintf("workflow %q is not in workflows.defects_from", run.Name)
	}
	return ""
}

// issueNode is the part of an Issue that parent resolution needs.
type issueNode struct {
	Number int
	Kind   string
	Area   string
	Parent int
}

// defectParent walks up from the merged PR's Issue to its Epic.
func defectParent(start int, load func(int) (issueNode, error)) (issueNode, error) {
	node, err := load(start)
	if err != nil {
		return issueNode{}, err
	}
	for depth := 0; depth < 20; depth++ {
		if node.Kind == "epic" {
			return node, nil
		}
		if node.Parent == 0 {
			return issueNode{}, fmt.Errorf("#%d has no Epic above it", start)
		}
		if node, err = load(node.Parent); err != nil {
			return issueNode{}, err
		}
	}
	return issueNode{}, fmt.Errorf("#%d: parent chain too deep", start)
}

func loadIssueNode(c model.Config, number int) (issueNode, error) {
	query := `query($owner:String!,$repo:String!,$number:Int!){ repository(owner:$owner,name:$repo){ issue(number:$number){ number parent{number} labels(first:50){nodes{name}} } } }`
	output, err := ghOutput("api", "graphql", "-f", "query="+query, "-f", "owner="+c.Owner, "-f", "repo="+c.Repository, "-F", fmt.Sprintf("number=%d", number))
	if err != nil {
		return issueNode{}, err
	}
	var response struct {
		Data struct {
			Repository struct {
				Issue *struct {
					Number int `json:"number"`
					Parent *struct {
						Number int `json:"number"`
					} `json:"parent"`
					Labels struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels"`
				} `json:"issue"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return issueNode{}, err
	}
	issue := response.Data.Repository.Issue
	if issue == nil {
		return issueNode{}, fmt.Errorf("#%d not found", number)
	}
	node := issueNode{Number: issue.Number}
	if issue.Parent != nil {
		node.Parent = issue.Parent.Number
	}
	for _, label := range issue.Labels.Nodes {
		if kind, ok := strings.CutPrefix(label.Name, "kind:"); ok {
			node.Kind = kind
		}
		if area, ok := strings.CutPrefix(label.Name, "area:"); ok {
			node.Area = area
		}
	}
	return node, nil
}

// mergedPull is the PR whose merge produced the failing commit.
type mergedPull struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	Body   string `json:"body"`
	Merged string `json:"merged_at"`
	SHA    string `json:"merge_commit_sha"`
}

// pullIssue returns the Issue the merged PR for sha references, if any.
func pullIssue(pulls []mergedPull, sha string) (mergedPull, int) {
	for _, pull := range pulls {
		if pull.Merged == "" || pull.SHA != sha {
			continue
		}
		if issues, err := policycheck.ReferencedIssues(pull.Body); err == nil && len(issues) == 1 {
			return pull, issues[0]
		}
		return pull, 0
	}
	return mergedPull{}, 0
}

func defectTitle(run runFacts) string {
	return fmt.Sprintf("Defect: %s failed on %s at %.7s", run.Name, run.HeadBranch, run.HeadSHA)
}

func defectBody(run runFacts, pull mergedPull, issue int) string {
	source := fmt.Sprintf("Failed run: %s (attempt %d) on commit %s", run.URL, run.Attempt, run.HeadSHA)
	if pull.URL != "" {
		source += fmt.Sprintf(", merged by PR %s", pull.URL)
		if issue != 0 {
			source += fmt.Sprintf(" for #%d", issue)
		}
	}
	body := model.WorkBody(
		fmt.Sprintf("The %s workflow passes on %s again.", run.Name, run.HeadBranch),
		fmt.Sprintf("The cause of the failure in %s.", run.URL),
		"Unrelated changes.",
		fmt.Sprintf("The cause of run %s is found and fixed, and %s passes on %s.", run.URL, run.Name, run.HeadBranch),
		fmt.Sprintf("A passing %s run that includes the fix.", run.Name),
		"None: a fix, unless the failure came from documented behaviour.",
		source, "CI")
	return body + fmt.Sprintf("\n<!-- quipu-defect:v1 run=%d -->\n", run.ID)
}

func writerDefectCommand(args []string) error {
	fs := flag.NewFlagSet("writer defect", flag.ContinueOnError)
	runID := fs.Int64("run-id", 0, "GitHub Actions run ID")
	apply := applyFlag(fs)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runID < 1 {
		return errors.New("usage: quipu writer defect --run-id ID [--apply] [--json]")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	trust, now := receiptTrust(c), time.Now()
	if *apply {
		if err := requireWriterIdentity(trust, now); err != nil {
			return err
		}
	}
	repo := "repos/" + c.Owner + "/" + c.Repository
	output, err := ghOutput("api", fmt.Sprintf("%s/actions/runs/%d", repo, *runID))
	if err != nil {
		return err
	}
	var run runFacts
	if err := json.Unmarshal(output, &run); err != nil {
		return err
	}
	report := map[string]any{"run": run.URL, "workflow": run.Name, "apply": *apply}
	if reason := defectSkip(c, run); reason != "" {
		report["skipped"] = reason
		return printValue(report, *asJSON)
	}
	output, err = ghOutput("api", "--paginate", repo+"/issues?state=all&per_page=100")
	if err != nil {
		return err
	}
	filed, err := markedIssues(output, defectMarker)
	if err != nil {
		return err
	}
	if number, ok := filed[fmt.Sprint(run.ID)]; ok {
		report["skipped"] = fmt.Sprintf("run already has Defect #%d", number)
		report["defect"] = number
		return printValue(report, *asJSON)
	}
	var pulls []mergedPull
	if output, err := ghOutput("api", fmt.Sprintf("%s/commits/%s/pulls", repo, run.HeadSHA)); err == nil {
		_ = json.Unmarshal(output, &pulls)
	}
	pull, issue := pullIssue(pulls, run.HeadSHA)
	area := c.Areas[0]
	var epic issueNode
	var warnings []string
	if issue == 0 {
		warnings = append(warnings, "no merged PR referencing an Issue produced this commit")
	} else if epic, err = defectParent(issue, func(n int) (issueNode, error) { return loadIssueNode(c, n) }); err != nil {
		warnings = append(warnings, err.Error())
	} else if node, err := loadIssueNode(c, issue); err == nil && contains(c.Areas, node.Area) {
		area = node.Area
	}
	report["issue"], report["epic"], report["warnings"] = issue, epic.Number, warnings
	if !*apply {
		report["title"] = defectTitle(run)
		return printValue(report, *asJSON)
	}
	number, err := fileDefect(c, run, pull, issue, epic.Number, area, now)
	if err != nil {
		return err
	}
	report["defect"] = number
	report["status"] = "Ready"
	if epic.Number == 0 {
		report["status"] = "Backlog"
		report["next"] = fmt.Sprintf("the owner links #%d under an Epic and runs quipu ready %d", number, number)
	}
	return printValue(report, *asJSON)
}

// fileDefect creates the Defect and, with a parent Epic, records it Ready.
func fileDefect(c model.Config, run runFacts, pull mergedPull, issue, epic int, area string, now time.Time) (int, error) {
	scratch, err := os.MkdirTemp("", "quipu-defect")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(scratch)
	bodyFile := filepath.Join(scratch, "defect.md")
	if err := os.WriteFile(bodyFile, []byte(defectBody(run, pull, issue)), 0o600); err != nil {
		return 0, err
	}
	output, err := ghOutput("issue", "create", "--repo", c.Owner+"/"+c.Repository, "--title", defectTitle(run), "--label", "kind:defect", "--label", "area:"+area, "--body-file", bodyFile)
	if err != nil {
		return 0, err
	}
	url := strings.TrimSpace(string(output))
	number, err := issueNumberFromURL(url)
	if err != nil {
		return 0, err
	}
	if err := addIssueToProject(c, url, "Defect", titleCase(area), "P0"); err != nil {
		return number, fmt.Errorf("Defect #%d: add to Project: %w", number, err)
	}
	if epic == 0 {
		return number, nil
	}
	if err := linkParent(c, epic, number); err != nil {
		return number, fmt.Errorf("Defect #%d: link Epic #%d: %w", number, epic, err)
	}
	state, err := loadLiveWork(c, number)
	if err != nil {
		return number, err
	}
	next := writer.WorkState{Status: "Ready"}
	if err := updateLiveWork(c, state.ItemID, next); err != nil {
		return number, err
	}
	receipt, err := writer.RenderReceipt(writer.Receipt{RequestID: fmt.Sprintf("defect-run-%d", run.ID), Result: "accepted",
		Detail: fmt.Sprintf("Defect filed for failed run %s; lifecycle state changed to Ready", run.URL), At: now.UTC(), State: writer.StateOf(next)})
	if err != nil {
		return number, err
	}
	_, err = receiptClient().CreateComment(context.Background(), c.Owner, c.Repository, number, receipt)
	return number, err
}
