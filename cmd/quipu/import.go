package main

// `quipu import --manifest FILE` brings a repository's existing backlog and
// history into Quipu. It creates the Issues, Project fields, parents, and
// blockers itself, like `quipu issue create`, and asks the Writer to record
// every status beyond Backlog, so Done and Cancelled work closes with a
// receipt the watchdog trusts. Each Issue carries its manifest key, so
// importing again creates nothing twice.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/model"
	"github.com/isnakolah/quipu/internal/writer"
	"gopkg.in/yaml.v3"
)

const importSchema = "quipu-import/v1"

// maxSubIssues is GitHub's limit on sub-issues under one parent.
const maxSubIssues = 100

type importManifest struct {
	Schema string       `yaml:"schema"`
	Source string       `yaml:"source"`
	Items  []importItem `yaml:"items"`
}

type importItem struct {
	Key        string   `yaml:"key"`
	Kind       string   `yaml:"kind"`
	Title      string   `yaml:"title"`
	Parent     string   `yaml:"parent"`
	Area       string   `yaml:"area"`
	Priority   string   `yaml:"priority"`
	Status     string   `yaml:"status"`
	Blockers   []string `yaml:"blockers"`
	Branch     string   `yaml:"branch"`
	Commits    []string `yaml:"commits"`
	Source     string   `yaml:"source"`
	Reason     string   `yaml:"reason"`
	ApprovedBy string   `yaml:"approved_by"`
	Body       string   `yaml:"body"`
}

var (
	importKey     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	importMarker  = regexp.MustCompile(`<!-- quipu-import:v1 key=([A-Za-z0-9][A-Za-z0-9._-]{0,63}) -->`)
	commitSHA     = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	gitBranchName = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	importKinds   = map[string]bool{"epic": true, "contract": true, "story": true, "task": true, "defect": true, "gate": true, "decision": true}
	importStatus  = map[string]bool{"Backlog": true, "Ready": true, "Blocked": true, "In progress": true, "Done": true, "Cancelled": true}
)

func parseManifest(raw []byte) (importManifest, error) {
	var m importManifest
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&m); err != nil {
		return m, fmt.Errorf("manifest: %w", err)
	}
	return m, nil
}

// normalize fills defaults: the first configured area, P2, Backlog, and the
// manifest's source.
func (m *importManifest) normalize(c model.Config) {
	for i := range m.Items {
		item := &m.Items[i]
		if item.Area == "" && len(c.Areas) > 0 {
			item.Area = c.Areas[0]
		}
		if item.Priority == "" {
			item.Priority = "P2"
		}
		if item.Status == "" {
			item.Status = "Backlog"
		}
		if item.Source == "" {
			item.Source = m.Source
		}
	}
}

func closedImport(status string) bool { return status == "Done" || status == "Cancelled" }

// validate reports every problem in the manifest at once.
func (m importManifest) validate(c model.Config) error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if m.Schema != importSchema {
		add("schema must be %q", importSchema)
	}
	if len(m.Items) == 0 {
		add("items is empty")
	}
	byKey := map[string]importItem{}
	for _, item := range m.Items {
		if !importKey.MatchString(item.Key) {
			add("key %q must be 1-64 letters, digits, dots, dashes, or underscores", item.Key)
		} else if _, dup := byKey[item.Key]; dup {
			add("key %s is used twice", item.Key)
		}
		byKey[item.Key] = item
	}
	for _, item := range m.Items {
		at := item.Key
		if !importKinds[item.Kind] {
			add("%s: kind must be one of epic, story, task, defect, contract, decision, gate", at)
		}
		if strings.TrimSpace(item.Title) == "" {
			add("%s: title is required", at)
		}
		if !contains(c.Areas, item.Area) {
			add("%s: area %q is not configured (%s)", at, item.Area, strings.Join(c.Areas, ", "))
		}
		if !map[string]bool{"P0": true, "P1": true, "P2": true, "P3": true}[item.Priority] {
			add("%s: priority must be P0, P1, P2, or P3", at)
		}
		if !importStatus[item.Status] {
			add("%s: status must be Backlog, Ready, Blocked, In progress, Done, or Cancelled", at)
		}
		switch {
		case item.Kind == "epic" && item.Parent != "":
			add("%s: an Epic has no parent", at)
		case item.Kind != "epic" && item.Parent == "":
			add("%s: every item but an Epic needs a parent", at)
		case item.Parent == item.Key:
			add("%s: an item cannot be its own parent", at)
		case item.Parent != "":
			if _, ok := byKey[item.Parent]; !ok {
				add("%s: parent %s is not in the manifest", at, item.Parent)
			}
		}
		for _, blocker := range item.Blockers {
			other, ok := byKey[blocker]
			switch {
			case blocker == item.Key:
				add("%s: an item cannot block itself", at)
			case !ok:
				add("%s: blocker %s is not in the manifest", at, blocker)
			case item.Status == "Done" && !closedImport(other.Status):
				add("%s: Done work cannot be blocked by open %s", at, blocker)
			}
		}
		if item.Branch != "" && (item.Status != "In progress" || !gitBranchName.MatchString(item.Branch)) {
			add("%s: branch is for In progress work and must be a plain branch name", at)
		}
		for _, sha := range item.Commits {
			if !commitSHA.MatchString(sha) {
				add("%s: commit %q is not a lowercase hex SHA", at, sha)
			}
		}
		if len(item.Commits) > 0 && item.Status != "Done" {
			add("%s: commits are evidence for Done work", at)
		}
		if item.Status == "Cancelled" && strings.TrimSpace(item.Reason) == "" {
			add("%s: Cancelled work needs a reason", at)
		}
		// Open work must carry a real contract; closed history needs none.
		if item.Kind != "epic" && !closedImport(item.Status) {
			if err := model.ValidateWorkContract(item.Body); err != nil {
				add("%s: open %s needs a work:v1 contract body: %v", at, item.Kind, err)
			}
		}
	}
	children := map[string]int{}
	for _, item := range m.Items {
		if item.Parent != "" {
			children[item.Parent]++
		}
	}
	for _, item := range m.Items {
		if n := children[item.Key]; n > maxSubIssues {
			add("%s: has %d children; GitHub allows %d sub-issues per parent, so split it", item.Key, n, maxSubIssues)
		}
	}
	if _, err := m.ordered(); err != nil {
		add("%v", err)
	}
	if len(problems) > 0 {
		return errors.New("invalid manifest:\n  " + strings.Join(problems, "\n  "))
	}
	return nil
}

// ordered returns the items with every parent before its children,
// otherwise in manifest order.
func (m importManifest) ordered() ([]importItem, error) {
	byKey := map[string]importItem{}
	for _, item := range m.Items {
		byKey[item.Key] = item
	}
	var out []importItem
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(item importItem) error
	visit = func(item importItem) error {
		switch state[item.Key] {
		case 1:
			return fmt.Errorf("parent cycle through %s", item.Key)
		case 2:
			return nil
		}
		state[item.Key] = 1
		if parent, ok := byKey[item.Parent]; ok && item.Parent != item.Key {
			if err := visit(parent); err != nil {
				return err
			}
		}
		state[item.Key] = 2
		out = append(out, item)
		return nil
	}
	for _, item := range m.Items {
		if err := visit(item); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// importBody is the Issue body: the manifest body, a note about a branch
// started before Quipu, and the key marker that makes re-imports match.
func importBody(item importItem) string {
	body := strings.TrimSpace(item.Body)
	if body == "" {
		body = item.Title
	}
	if item.Branch != "" {
		body += fmt.Sprintf("\n\nWork started before Quipu on branch `%s`. `quipu start` gives this Issue its own `NNN/short-description` branch; bring the commits over from `%s`.", item.Branch, item.Branch)
	}
	if item.Source != "" {
		body += "\n\nImported from " + item.Source + "."
	}
	return body + fmt.Sprintf("\n\n<!-- quipu-import:v1 key=%s -->\n", item.Key)
}

// importedKeys maps manifest keys to Issue numbers.
func importedKeys(raw []byte) (map[string]int, error) {
	return markedIssues(raw, importMarker)
}

// markedIssues maps the first capture of marker in each Issue body to the
// oldest Issue carrying it. raw is paginated REST Issue pages; pull
// requests are skipped.
func markedIssues(raw []byte, marker *regexp.Regexp) (map[string]int, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	keys := map[string]int{}
	for decoder.More() {
		var page []struct {
			Number      int             `json:"number"`
			Body        string          `json:"body"`
			PullRequest json.RawMessage `json:"pull_request"`
		}
		if err := decoder.Decode(&page); err != nil {
			return nil, err
		}
		for _, issue := range page {
			if len(issue.PullRequest) > 0 && string(issue.PullRequest) != "null" {
				continue
			}
			if match := marker.FindStringSubmatch(issue.Body); match != nil {
				if earlier, ok := keys[match[1]]; !ok || issue.Number < earlier {
					keys[match[1]] = issue.Number
				}
			}
		}
	}
	return keys, nil
}

// importLinks is what GitHub already has for an imported Issue.
type importLinks struct {
	ID        string
	Parent    int
	Blockers  map[int]bool
	InProject bool
}

func readImportLinks(c model.Config, number int) (importLinks, error) {
	query := `query($owner:String!,$repo:String!,$number:Int!){ repository(owner:$owner,name:$repo){ issue(number:$number){ id parent{number} blockedBy(first:100){nodes{number}} projectItems(first:50){nodes{project{id}}} } } }`
	output, err := ghOutput("api", "graphql", "-f", "query="+query, "-f", "owner="+c.Owner, "-f", "repo="+c.Repository, "-F", fmt.Sprintf("number=%d", number))
	if err != nil {
		return importLinks{}, err
	}
	var response struct {
		Data struct {
			Repository struct {
				Issue struct {
					ID     string `json:"id"`
					Parent *struct {
						Number int `json:"number"`
					} `json:"parent"`
					BlockedBy struct {
						Nodes []struct {
							Number int `json:"number"`
						} `json:"nodes"`
					} `json:"blockedBy"`
					ProjectItems struct {
						Nodes []struct {
							Project struct {
								ID string `json:"id"`
							} `json:"project"`
						} `json:"nodes"`
					} `json:"projectItems"`
				} `json:"issue"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return importLinks{}, err
	}
	issue := response.Data.Repository.Issue
	links := importLinks{ID: issue.ID, Blockers: map[int]bool{}}
	if issue.Parent != nil {
		links.Parent = issue.Parent.Number
	}
	for _, node := range issue.BlockedBy.Nodes {
		links.Blockers[node.Number] = true
	}
	for _, node := range issue.ProjectItems.Nodes {
		links.InProject = links.InProject || node.Project.ID == c.Project.ID
	}
	return links, nil
}

// importRequested reports whether the Issue already has an import request
// that the Writer accepted or has not answered yet.
func importRequested(comments []github.Comment, receipts []writer.Receipt) bool {
	for _, comment := range comments {
		request, err := writer.ParseRequest(comment.Body)
		if err != nil || request.Action != "import" {
			continue
		}
		if receipt, ok := receiptFor(receipts, request.ID); !ok || receipt.Result == "accepted" {
			return true
		}
	}
	return false
}

// importBranch keeps a pre-Quipu branch on the board only when it already
// follows the NNN/short-description rule for this Issue.
func importBranch(number int, branch string) string {
	if strings.HasPrefix(branch, fmt.Sprintf("%03d/", number)) && validBranch(branch) {
		return branch
	}
	return ""
}

func validBranch(branch string) bool {
	return regexp.MustCompile(`^[0-9]{3,}/[a-z0-9-]+$`).MatchString(branch)
}

func importCommand(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "manifest YAML file (schema quipu-import/v1)")
	apply := applyFlag(fs)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *manifestPath == "" {
		return errors.New("usage: quipu import --manifest FILE [--apply] [--json]")
	}
	raw, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	m, err := parseManifest(raw)
	if err != nil {
		return err
	}
	m.normalize(c)
	if err := m.validate(c); err != nil {
		return err
	}
	items, _ := m.ordered()
	output, err := ghOutput("api", "--paginate", "repos/"+c.Owner+"/"+c.Repository+"/issues?state=all&per_page=100")
	if err != nil {
		return err
	}
	numbers, err := importedKeys(output)
	if err != nil {
		return err
	}
	report := map[string]any{"manifest": *manifestPath, "items": len(items), "apply": *apply}
	if !*apply {
		var create, existing []string
		for _, item := range items {
			if number, ok := numbers[item.Key]; ok {
				existing = append(existing, fmt.Sprintf("%s #%d", item.Key, number))
			} else {
				create = append(create, item.Key)
			}
		}
		report["create"], report["existing"] = create, existing
		report["next"] = "rerun with --apply; existing Issues are matched by key and only repaired"
		return printValue(report, *asJSON)
	}
	here, err := currentWorktree()
	if err != nil {
		return err
	}
	ops, cleanup, err := liveImportOps(c, here)
	if err != nil {
		return err
	}
	defer cleanup()
	changes, requests, err := runImport(ops, c, items, numbers)
	report["changes"], report["import_requests"] = changes, requests
	report["changed"] = len(changes) > 0
	if err != nil {
		report["error"] = err.Error()
		report["next"] = "rerun the same command: it matches every Issue by key, so it creates nothing twice and links what this run left unlinked"
		_ = printValue(report, *asJSON)
		return errReported
	}
	if len(requests) > 0 {
		report["next"] = "the Writer records each status beyond Backlog and closes Done and Cancelled work; watch with quipu status, or run gh workflow run quipu-writer.yml"
	}
	return printValue(report, *asJSON)
}
