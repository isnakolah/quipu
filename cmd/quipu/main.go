package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/bootstrap"
	"github.com/isnakolah/quipu/internal/github"
	"github.com/isnakolah/quipu/internal/journal"
	"github.com/isnakolah/quipu/internal/model"
	policycheck "github.com/isnakolah/quipu/internal/policy"
	"github.com/isnakolah/quipu/internal/writer"
	"gopkg.in/yaml.v3"
)

const version = "0.0.0-dev"
const configPath = ".github/quipu/config.yaml"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "quipu:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "version":
		fmt.Println(version)
		return nil
	case "status":
		return statusCommand(args[1:])
	case "start":
		return startCommand(args[1:])
	case "done":
		return doneCommand(args[1:])
	case "init":
		return initCommand(args[1:])
	case "doctor":
		return doctor(args[1:])
	case "context":
		return contextCommand(args[1:])
	case "validate":
		return validateCommand(args[1:])
	case "request":
		return requestCommand(args[1:])
	case "work":
		return workCommand(args[1:])
	case "evidence":
		return evidenceCommand(args[1:])
	case "journal":
		return journalCommand(args[1:])
	case "configure":
		return configureCommand(args[1:])
	case "writer":
		return writerCommand(args[1:])
	case "adopt":
		return adoptCommand(args[1:])
	case "issue":
		return issueCommand(args[1:])
	case "pr":
		return prCommand(args[1:])
	case "policy":
		return policyCommand(args[1:])
	case "guard":
		return guardCommand(args[1:])
	default:
		return usage()
	}
}

func policyCommand(args []string) error {
	if len(args) == 0 || args[0] != "pr" {
		return errors.New("usage: quipu policy pr --pr URL [--json]")
	}
	fs := flag.NewFlagSet("policy pr", flag.ContinueOnError)
	pr := fs.String("pr", "", "pull request URL")
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *pr == "" {
		return errors.New("policy pr requires --pr")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	output, err := ghOutput("pr", "view", *pr, "--repo", c.Owner+"/"+c.Repository, "--json", "url,headRefName,body")
	if err != nil {
		return err
	}
	var pull struct {
		URL         string `json:"url"`
		HeadRefName string `json:"headRefName"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(output, &pull); err != nil {
		return err
	}
	issues, err := policycheck.ReferencedIssues(pull.Body)
	if err != nil {
		return err
	}
	state, err := loadLiveWork(c, issues[0])
	if err != nil {
		return err
	}
	if err := validatePRWork(pull.HeadRefName, pull.Body, state, time.Now()); err != nil {
		return err
	}
	report := map[string]any{"valid": true, "pr": pull.URL, "issue_number": issues[0], "branch": pull.HeadRefName}
	return printValue(report, *asJSON)
}

func validatePRWork(branch, body string, state liveWork, now time.Time) error {
	issues, err := policycheck.ReferencedIssues(body)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(branch, fmt.Sprintf("%03d/", issues[0])) {
		return fmt.Errorf("PR branch %q does not map to referenced Issue #%d", branch, issues[0])
	}
	blockersResolved := true
	for _, blocker := range state.Blockers {
		if blocker.State != "CLOSED" {
			blockersResolved = false
		}
	}
	expires, err := time.Parse(time.RFC3339, state.Expiry)
	hasLease := err == nil && state.Holder != "" && state.Branch == branch && expires.After(now.UTC())
	if state.ParentID == "" {
		return errors.New("PR Issue has no native parent")
	}
	if err := model.ValidateWorkContract(state.Body); err != nil {
		return fmt.Errorf("PR Issue contract invalid: %w", err)
	}
	return policycheck.ValidatePR(branch, body, hasLease, blockersResolved)
}

func prCommand(args []string) error {
	if len(args) == 0 || args[0] != "link" {
		return errors.New("usage: quipu pr link --issue-number N --issue-id ID --fingerprint SHA --pr URL --apply")
	}
	return submitRequest("pr.link", args[1:], nil)
}

func issueCommand(args []string) error {
	if len(args) > 0 && args[0] == "link-parent" {
		return linkParentCommand(args[1:])
	}
	if len(args) > 0 && args[0] == "add-blocker" {
		return addBlockerCommand(args[1:])
	}
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: quipu issue {create|link-parent}")
	}
	fs := flag.NewFlagSet("issue create", flag.ContinueOnError)
	title := fs.String("title", "", "Issue title")
	kind := fs.String("kind", "", "epic|contract|story|task|defect|gate|decision")
	bodyFile := fs.String("body-file", "", "contract Markdown file")
	parent := fs.Int("parent", 0, "native parent Issue number; required except Epic")
	area := fs.String("area", "stable", "configured area")
	priority := fs.String("priority", "P2", "P0|P1|P2|P3")
	approvedBy := fs.String("approved-by", "", "owner approval identity; required for Epic")
	apply := applyFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !*apply {
		return errors.New("issue creation requires --apply")
	}
	if *title == "" || *bodyFile == "" {
		return errors.New("--title and --body-file are required")
	}
	allowed := map[string]bool{"epic": true, "contract": true, "story": true, "task": true, "defect": true, "gate": true, "decision": true}
	if !allowed[*kind] {
		return errors.New("invalid --kind")
	}
	body, err := os.ReadFile(*bodyFile)
	if err != nil {
		return err
	}
	if *kind != "epic" {
		if err := model.ValidateWorkContract(string(body)); err != nil {
			return fmt.Errorf("work Issue body does not satisfy work:v1 contract: %w", err)
		}
		if *parent < 1 {
			return errors.New("non-Epic Issue creation requires --parent")
		}
	} else if *parent != 0 {
		return errors.New("Epic cannot have a parent Issue")
	} else if strings.TrimSpace(*approvedBy) == "" {
		return errors.New("Epic creation requires --approved-by owner evidence")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if !contains(c.Areas, *area) {
		return fmt.Errorf("area %q is not configured", *area)
	}
	if !map[string]bool{"P0": true, "P1": true, "P2": true, "P3": true}[*priority] {
		return errors.New("priority must be P0, P1, P2, or P3")
	}
	createArgs := []string{"issue", "create", "--repo", c.Owner + "/" + c.Repository, "--title", *title, "--label", "kind:" + *kind, "--label", "area:" + *area, "--body-file", *bodyFile}
	output, err := ghOutput(createArgs...)
	if err != nil {
		return err
	}
	issueURL := strings.TrimSpace(string(output))
	number, err := issueNumberFromURL(issueURL)
	if err != nil {
		return err
	}
	if *kind != "epic" {
		if err := linkParent(c, *parent, number); err != nil {
			return fmt.Errorf("created Issue %s but could not link parent: %w", issueURL, err)
		}
	}
	if err := addIssueToProject(c, issueURL, titleCase(*kind), titleCase(*area), *priority); err != nil {
		return fmt.Errorf("created Issue %s but could not add configured Project fields: %w", issueURL, err)
	}
	if *kind == "epic" {
		if _, err := github.NewClient().CreateComment(context.Background(), c.Owner, c.Repository, number, "Owner approval evidence: "+strings.TrimSpace(*approvedBy)); err != nil {
			return fmt.Errorf("created Epic %s but could not record approval: %w", issueURL, err)
		}
	}
	fmt.Println(issueURL)
	return nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func titleCase(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

var issueURLPattern = regexp.MustCompile(`/issues/([0-9]+)$`)

func issueNumberFromURL(value string) (int, error) {
	match := issueURLPattern.FindStringSubmatch(strings.TrimSpace(value))
	if len(match) != 2 {
		return 0, fmt.Errorf("GitHub Issue create returned unexpected URL %q", value)
	}
	number, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, err
	}
	return number, nil
}

func addBlockerCommand(args []string) error {
	fs := flag.NewFlagSet("issue add-blocker", flag.ContinueOnError)
	issue := fs.Int("issue", 0, "blocked Issue number")
	blocker := fs.Int("blocker", 0, "blocking Issue number")
	apply := applyFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*apply {
		return errors.New("blocker link requires --apply")
	}
	if *issue < 1 || *blocker < 1 || *issue == *blocker {
		return errors.New("distinct --issue and --blocker are required")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	query := fmt.Sprintf("query { repository(owner:\"%s\",name:\"%s\") { i:issue(number:%d){id} b:issue(number:%d){id} } }", c.Owner, c.Repository, *issue, *blocker)
	output, err := ghOutput("api", "graphql", "-f", "query="+query)
	if err != nil {
		return err
	}
	var response struct {
		Data struct {
			Repository struct {
				I struct {
					ID string `json:"id"`
				} `json:"i"`
				B struct {
					ID string `json:"id"`
				} `json:"b"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return err
	}
	if response.Data.Repository.I.ID == "" || response.Data.Repository.B.ID == "" {
		return errors.New("Issue or blocker not found")
	}
	mutation := fmt.Sprintf("mutation { addBlockedBy(input:{issueId:\"%s\",blockingIssueId:\"%s\"}) { issue { number } } }", response.Data.Repository.I.ID, response.Data.Repository.B.ID)
	cmd := exec.Command("gh", "api", "graphql", "-f", "query="+mutation)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func linkParentCommand(args []string) error {
	fs := flag.NewFlagSet("issue link-parent", flag.ContinueOnError)
	parent := fs.Int("parent", 0, "parent Issue number")
	child := fs.Int("child", 0, "child Issue number")
	apply := applyFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*apply {
		return errors.New("parent link requires --apply")
	}
	if *parent < 1 || *child < 1 || *parent == *child {
		return errors.New("distinct --parent and --child are required")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	return linkParent(c, *parent, *child)
}

func linkParent(c model.Config, parent, child int) error {
	query := fmt.Sprintf("query { repository(owner:\"%s\",name:\"%s\") { p:issue(number:%d){id} c:issue(number:%d){id} } }", c.Owner, c.Repository, parent, child)
	output, err := exec.Command("gh", "api", "graphql", "-f", "query="+query).Output()
	if err != nil {
		return err
	}
	var response struct {
		Data struct {
			Repository struct {
				P struct {
					ID string `json:"id"`
				} `json:"p"`
				C struct {
					ID string `json:"id"`
				} `json:"c"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return err
	}
	if response.Data.Repository.P.ID == "" || response.Data.Repository.C.ID == "" {
		return errors.New("parent or child Issue not found")
	}
	mutation := fmt.Sprintf("mutation { addSubIssue(input:{issueId:\"%s\",subIssueId:\"%s\"}) { subIssue { number } } }", response.Data.Repository.P.ID, response.Data.Repository.C.ID)
	_, err = ghOutput("api", "graphql", "-f", "query="+mutation)
	return err
}

func addIssueToProject(c model.Config, issueURL, kind, area, priority string) error {
	output, err := ghOutput("project", "item-add", fmt.Sprint(c.Project.Number), "--owner", c.Owner, "--url", issueURL, "--format", "json")
	if err != nil {
		return err
	}
	var item struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(output, &item); err != nil {
		return err
	}
	if item.ID == "" {
		return errors.New("GitHub returned incomplete Project item identity")
	}
	fields, err := configuredProjectFields(c)
	if err != nil {
		return err
	}
	values := map[string]string{"Status": "Backlog", "Kind": kind, "Area": area, "Priority": priority, "Proof": "Not started"}
	for name, value := range values {
		field, ok := fields[name]
		if !ok || field.Options[value] == "" {
			return fmt.Errorf("configured Project lacks %s option %q", name, value)
		}
		if err := setProjectFieldValue(c.Project.ID, item.ID, field.ID, field.Options[value], false); err != nil {
			return fmt.Errorf("set Project %s: %w", name, err)
		}
	}
	return nil
}

func adoptCommand(args []string) error {
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	owner := fs.String("owner", "", "GitHub owner")
	repo := fs.String("repo", "", "repository")
	projectNumber := fs.Int("project-number", 0, "existing GitHub Project number")
	wiki := fs.String("wiki", "off", "off|journal")
	branch := fs.String("default-branch", "main", "default branch")
	areas := fs.String("areas", "stable", "comma-separated areas")
	apply := applyFlag(fs)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *owner == "" || *repo == "" {
		return errors.New("--owner and --repo are required")
	}
	output, err := exec.Command("gh", "api", "repos/"+*owner+"/"+*repo+"/issues?state=all&per_page=100").Output()
	if err != nil {
		return fmt.Errorf("audit repository: %w", err)
	}
	var issues []json.RawMessage
	if err := json.Unmarshal(output, &issues); err != nil {
		return err
	}
	report := map[string]any{"owner": *owner, "repository": *repo, "issues_found": len(issues), "safe_to_adopt": len(issues) == 0, "note": "adopt never reclassifies existing Issues"}
	if !*apply {
		return printValue(report, *asJSON)
	}
	if len(issues) != 0 {
		return errors.New("adopt refuses repository with existing Issues; audit and migrate explicitly")
	}
	if *projectNumber < 1 {
		return errors.New("adopt --apply requires --project-number for an existing Project")
	}
	if _, err := os.Stat(configPath); err == nil {
		return fmt.Errorf("%s already exists", configPath)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return err
	}
	projectInfo, err := readProject(*owner, *projectNumber)
	if err != nil {
		return err
	}
	fieldIDs, err := configuredFieldIDs(*owner, *projectNumber)
	if err != nil {
		return err
	}
	c := model.Config{SchemaVersion: model.ConfigVersion, Owner: *owner, Repository: *repo, WikiMode: *wiki, DefaultBranch: *branch, Areas: split(*areas), WriterVersion: version, AuthorizedActors: []string{*owner}, Policy: model.Policy{LeaseTTLMinutes: 120, ReconcileMins: 5}, Project: model.Project{ID: projectInfo.ID, Number: *projectNumber, Title: projectInfo.Title, Fields: fieldIDs}}
	if err := c.Validate(); err != nil {
		return err
	}
	raw, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, raw, 0644); err != nil {
		return err
	}
	return printValue(report, *asJSON)
}

func readProject(owner string, number int) (createdProject, error) {
	output, err := ghOutput("project", "view", fmt.Sprint(number), "--owner", owner, "--format", "json")
	if err != nil {
		return createdProject{}, fmt.Errorf("read Project #%d: %w", number, err)
	}
	var project struct {
		ID     string `json:"id"`
		Number int    `json:"number"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(output, &project); err != nil {
		return createdProject{}, err
	}
	if project.ID == "" || project.Number != number || project.Title == "" {
		return createdProject{}, fmt.Errorf("Project #%d returned incomplete identity", number)
	}
	return createdProject{ID: project.ID, Number: project.Number, Title: project.Title}, nil
}

func configuredFieldIDs(owner string, projectNumber int) (map[string]string, error) {
	fields, err := readProjectFields(owner, projectNumber)
	if err != nil {
		return nil, err
	}
	return configuredFieldIDsFromMap(projectNumber, fields)
}

func configuredFieldIDsFromMap(projectNumber int, fields map[string]projectField) (map[string]string, error) {
	ids := make(map[string]string, 8)
	for key, name := range map[string]string{
		"status": "Status", "kind": "Kind", "area": "Area", "priority": "Priority", "proof": "Proof",
		"lease_holder": "Lease holder", "lease_expires": "Lease expires", "branch": "Branch", "state_fingerprint": "State fingerprint",
	} {
		field, ok := fields[name]
		if !ok || field.ID == "" {
			return nil, fmt.Errorf("Project #%d missing required field %q", projectNumber, name)
		}
		ids[key] = field.ID
	}
	return ids, nil
}
func usage() error {
	return errors.New(`usage: quipu <command>

Everyday:
  status     what is in progress, in review, ready, and needs attention
  start N    claim Issue #N, create its branch, and record the lease locally
  start --next  claim the highest-priority Ready work (for agents)
  start --next --worktree  claim it into a new git worktree (dispatch a parallel agent)
  done N     link the PR, then after merge record evidence and close #N

Setup and advanced:
  init|adopt|doctor|context|validate|issue|work|pr|evidence|request|writer|journal|policy|configure|guard|version`)
}
func applyFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("apply", false, "perform state-changing action")
}
func jsonFlag(fs *flag.FlagSet) *bool { return fs.Bool("json", false, "JSON output") }

func initCommand(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	owner := fs.String("owner", "", "GitHub owner")
	repo := fs.String("repo", "", "repository")
	project := fs.String("project-name", "", "Project title")
	wiki := fs.String("wiki", "off", "off|journal")
	branch := fs.String("default-branch", "main", "default branch")
	areas := fs.String("areas", "stable", "comma-separated areas")
	visibility := fs.String("visibility", "public", "public|private")
	apply := applyFlag(fs)
	yes := fs.Bool("yes", false, "confirm bootstrap")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*apply || !*yes {
		return errors.New("init requires --apply --yes")
	}
	if *owner == "" || *repo == "" || *project == "" {
		return errors.New("--owner, --repo, and --project-name are required")
	}
	if *visibility != "public" && *visibility != "private" {
		return errors.New("visibility must be public or private")
	}
	if err := requireEmptyBootstrapRoot("."); err != nil {
		return err
	}
	c := model.Config{SchemaVersion: model.ConfigVersion, Owner: *owner, Repository: *repo, WikiMode: *wiki, DefaultBranch: *branch, Areas: split(*areas), WriterVersion: version, AuthorizedActors: []string{*owner}, Policy: model.Policy{LeaseTTLMinutes: 120, ReconcileMins: 5}, Project: model.Project{Title: *project, Fields: map[string]string{}}}
	if err := c.Validate(); err != nil {
		return err
	}
	if _, err := os.Stat(configPath); err == nil {
		return fmt.Errorf("%s already exists; use adopt for existing repositories", configPath)
	}
	projectInfo, err := provisionGitHub(*owner, *repo, *visibility, *project)
	if err != nil {
		return err
	}
	fieldIDs, err := provisionProjectContract(*owner, *repo, projectInfo.Number, c.Areas)
	if err != nil {
		return err
	}
	if err := provisionProjectViews(*owner, projectInfo.Number); err != nil {
		return err
	}
	c.Project.ID = projectInfo.ID
	c.Project.Number = projectInfo.Number
	c.Project.Fields = fieldIDs
	if err := bootstrap.Install(".", *owner, projectInfo.Number, *branch); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return err
	}
	raw, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, raw, 0644); err != nil {
		return err
	}
	if err := initializeBootstrapRepository(*owner, *repo, *branch); err != nil {
		return fmt.Errorf("GitHub repository and Project were provisioned, but local bootstrap push failed: %w", err)
	}
	fmt.Printf("provisioned %s/%s and Project #%d; wrote %s\n", *owner, *repo, projectInfo.Number, configPath)
	return nil
}

func requireEmptyBootstrapRoot(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("init requires an empty directory; use quipu adopt --apply after its audit for an existing repository")
	}
	return nil
}

func initializeBootstrapRepository(owner, repo, branch string) error {
	if output, err := exec.Command("git", "init", "--initial-branch", branch).CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %s", strings.TrimSpace(string(output)))
	}
	if output, err := exec.Command("git", "add", ".").CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %s", strings.TrimSpace(string(output)))
	}
	if output, err := exec.Command("git", "commit", "-m", "chore(quipu): bootstrap Quipu").CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %s", strings.TrimSpace(string(output)))
	}
	remote := "https://github.com/" + owner + "/" + repo + ".git"
	if output, err := exec.Command("git", "remote", "add", "origin", remote).CombinedOutput(); err != nil {
		return fmt.Errorf("git remote add: %s", strings.TrimSpace(string(output)))
	}
	if output, err := exec.Command("git", "push", "--set-upstream", "origin", branch).CombinedOutput(); err != nil {
		return fmt.Errorf("git push: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

type createdProject struct {
	ID     string `json:"id"`
	Number int    `json:"number"`
	Title  string `json:"title"`
}

func provisionGitHub(owner, repo, visibility, projectName string) (createdProject, error) {
	remote := owner + "/" + repo
	if err := exec.Command("gh", "repo", "view", remote).Run(); err == nil {
		return createdProject{}, fmt.Errorf("repository %s already exists; use quipu adopt after its audit", remote)
	}
	flag := "--public"
	if visibility == "private" {
		flag = "--private"
	}
	if output, err := exec.Command("gh", "repo", "create", remote, flag).CombinedOutput(); err != nil {
		return createdProject{}, fmt.Errorf("create repository %s: %s", remote, strings.TrimSpace(string(output)))
	}
	if output, err := exec.Command("gh", "repo", "edit", remote, "--enable-issues", "--enable-wiki").CombinedOutput(); err != nil {
		return createdProject{}, fmt.Errorf("enable repository features: %s", strings.TrimSpace(string(output)))
	}
	output, err := exec.Command("gh", "project", "create", "--owner", owner, "--title", projectName, "--format", "json").Output()
	if err != nil {
		return createdProject{}, fmt.Errorf("create Project: %w", err)
	}
	var project createdProject
	if err := json.Unmarshal(output, &project); err != nil {
		return createdProject{}, err
	}
	if project.ID == "" || project.Number == 0 {
		return createdProject{}, errors.New("GitHub returned incomplete Project identity")
	}
	project.Title = projectName
	return project, nil
}

func provisionProjectContract(owner, repo string, projectNumber int, areas []string) (map[string]string, error) {
	labels := []string{"kind:epic", "kind:contract", "kind:story", "kind:task", "kind:defect", "kind:gate", "kind:decision"}
	for _, area := range areas {
		labels = append(labels, "area:"+area)
	}
	for _, label := range labels {
		if output, err := exec.Command("gh", "label", "create", label, "--color", "1D76DB", "--force", "--repo", owner+"/"+repo).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("create label %s: %s", label, strings.TrimSpace(string(output)))
		}
	}
	areaOptions := make([]string, 0, len(areas))
	for _, area := range areas {
		areaOptions = append(areaOptions, strings.ToUpper(area[:1])+area[1:])
	}
	fields := []struct{ name, kind, options string }{
		{"Kind", "SINGLE_SELECT", "Epic,Contract,Story,Task,Defect,Gate,Decision"},
		{"Area", "SINGLE_SELECT", strings.Join(areaOptions, ",")},
		{"Priority", "SINGLE_SELECT", "P0,P1,P2,P3"},
		{"Proof", "SINGLE_SELECT", "Not started,Local verified,CI verified,Target environment pending,External/provider pending,Complete"},
		{"Lease holder", "TEXT", ""}, {"Lease expires", "TEXT", ""}, {"Branch", "TEXT", ""}, {"State fingerprint", "TEXT", ""},
	}
	for _, field := range fields {
		args := []string{"project", "field-create", fmt.Sprint(projectNumber), "--owner", owner, "--name", field.name, "--data-type", field.kind}
		if field.options != "" {
			args = append(args, "--single-select-options", field.options)
		}
		if output, err := exec.Command("gh", args...).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("create Project field %s: %s", field.name, strings.TrimSpace(string(output)))
		}
	}
	projectFields, err := readProjectFields(owner, projectNumber)
	if err != nil {
		return nil, err
	}
	status, ok := projectFields["Status"]
	if !ok {
		return nil, errors.New("Project default Status field not found")
	}
	if err := setStatusOptions(status.ID); err != nil {
		return nil, err
	}
	ids := map[string]string{}
	for key, name := range map[string]string{"status": "Status", "kind": "Kind", "area": "Area", "priority": "Priority", "proof": "Proof", "lease_holder": "Lease holder", "lease_expires": "Lease expires", "branch": "Branch", "state_fingerprint": "State fingerprint"} {
		field, ok := projectFields[name]
		if !ok {
			return nil, fmt.Errorf("Project field %q missing after provisioning", name)
		}
		ids[key] = field.ID
	}
	return ids, nil
}

type restProjectField struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type projectView struct {
	Name          string `json:"name"`
	Layout        string `json:"layout"`
	Filter        string `json:"filter"`
	VisibleFields []int  `json:"visible_fields,omitempty"`
	GroupBy       []int  `json:"group_by,omitempty"`
}

func provisionProjectViews(owner string, projectNumber int) error {
	endpoint, err := projectRESTEndpoint(owner, projectNumber)
	if err != nil {
		return err
	}
	output, err := ghOutput("api", endpoint+"/fields?per_page=100", "-H", "X-GitHub-Api-Version: 2026-03-10")
	if err != nil {
		return fmt.Errorf("list Project fields for views: %w", err)
	}
	var fields []restProjectField
	if err := json.Unmarshal(output, &fields); err != nil {
		return err
	}
	byName := make(map[string]int, len(fields))
	for _, field := range fields {
		byName[field.Name] = field.ID
	}
	views, err := standardProjectViews(byName)
	if err != nil {
		return err
	}
	for _, view := range views {
		raw, err := json.Marshal(view)
		if err != nil {
			return err
		}
		cmd := exec.Command("gh", "api", "--method", "POST", endpoint+"/views", "-H", "X-GitHub-Api-Version: 2026-03-10", "--input", "-")
		cmd.Stdin = strings.NewReader(string(raw))
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("create Project view %q: %s", view.Name, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func projectRESTEndpoint(owner string, projectNumber int) (string, error) {
	if _, err := ghOutput("api", "users/"+owner); err == nil {
		return fmt.Sprintf("users/%s/projectsV2/%d", owner, projectNumber), nil
	}
	if _, err := ghOutput("api", "orgs/"+owner); err == nil {
		return fmt.Sprintf("orgs/%s/projectsV2/%d", owner, projectNumber), nil
	}
	return "", fmt.Errorf("Project owner %q is neither a readable GitHub user nor organization", owner)
}

func standardProjectViews(fields map[string]int) ([]projectView, error) {
	need := func(name string) (int, error) {
		id, ok := fields[name]
		if !ok || id == 0 {
			return 0, fmt.Errorf("missing required Project field %q for standard views", name)
		}
		return id, nil
	}
	visibleNames := []string{"Status", "Kind", "Area", "Priority", "Proof", "Lease holder", "Lease expires", "Branch", "State fingerprint"}
	visible := make([]int, 0, len(visibleNames))
	for _, name := range visibleNames {
		id, err := need(name)
		if err != nil {
			return nil, err
		}
		visible = append(visible, id)
	}
	status, err := need("Status")
	if err != nil {
		return nil, err
	}
	parent, err := need("Parent issue")
	if err != nil {
		return nil, err
	}
	return []projectView{
		{Name: "Roadmap", Layout: "roadmap", Filter: "is:issue", GroupBy: []int{parent}},
		{Name: "Agent queue", Layout: "table", Filter: "is:issue status:Ready", VisibleFields: visible},
		{Name: "In flight", Layout: "board", Filter: "is:issue status:Claimed,\"In progress\",\"In review\"", VisibleFields: visible, GroupBy: []int{status}},
		{Name: "Proof and gates", Layout: "table", Filter: "is:issue status:\"Evidence pending\",Blocked", VisibleFields: visible},
		{Name: "Archive", Layout: "table", Filter: "is:issue status:Done,status:Cancelled,status:Archived", VisibleFields: visible},
	}, nil
}

func setStatusOptions(fieldID string) error {
	options := []struct{ name, color string }{
		{"Backlog", "GRAY"}, {"Ready", "BLUE"}, {"Claimed", "YELLOW"}, {"In progress", "ORANGE"}, {"In review", "PURPLE"}, {"Evidence pending", "PINK"}, {"Blocked", "RED"}, {"Done", "GREEN"}, {"Cancelled", "GRAY"}, {"Archived", "GRAY"},
	}
	parts := make([]string, 0, len(options))
	for _, option := range options {
		parts = append(parts, fmt.Sprintf(`{name:%q,color:%s,description:""}`, option.name, option.color))
	}
	mutation := fmt.Sprintf("mutation { updateProjectV2Field(input:{fieldId:%q,singleSelectOptions:[%s]}) { projectV2Field { ... on ProjectV2SingleSelectField { id } } } }", fieldID, strings.Join(parts, ","))
	if output, err := exec.Command("gh", "api", "graphql", "-f", "query="+mutation).CombinedOutput(); err != nil {
		return fmt.Errorf("configure Project Status options: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func readProjectFields(owner string, projectNumber int) (map[string]projectField, error) {
	output, err := exec.Command("gh", "project", "field-list", fmt.Sprint(projectNumber), "--owner", owner, "--format", "json").Output()
	if err != nil {
		return nil, err
	}
	var response struct {
		Fields []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Options []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"options"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, err
	}
	fields := make(map[string]projectField, len(response.Fields))
	for _, raw := range response.Fields {
		field := projectField{ID: raw.ID, Options: map[string]string{}}
		for _, option := range raw.Options {
			field.Options[option.Name] = option.ID
		}
		fields[raw.Name] = field
	}
	return fields, nil
}
func split(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// loadConfig reads the configuration of the repository containing the
// current directory, so commands work from any subdirectory or worktree.
func loadConfig() (model.Config, error) {
	top := "."
	if w, err := currentWorktree(); err == nil {
		top = w.Top
	}
	return loadConfigAt(top)
}

func loadConfigAt(top string) (model.Config, error) {
	var c model.Config
	raw, err := os.ReadFile(filepath.Join(top, configPath))
	if err != nil {
		return c, err
	}
	err = yaml.Unmarshal(raw, &c)
	if err != nil {
		return c, err
	}
	return c, c.Validate()
}
func printValue(v any, asJSON bool) error {
	if asJSON {
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	}
	fmt.Printf("%+v\n", v)
	return nil
}
func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	cachePath := ""
	if cache, cacheErr := github.NewMetadataCache(); cacheErr == nil {
		cachePath = cache.Root
	}
	report := map[string]any{"quipu_version": version, "config": err == nil, "github_token": github.NewClient().Token != "", "repository": "", "cache_path": cachePath, "cache_authority": "disposable metadata only; live GitHub wins"}
	if err == nil {
		report["repository"] = c.Owner + "/" + c.Repository
	}
	return printValue(report, *asJSON)
}
func contextCommand(args []string) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	asJSON := jsonFlag(fs)
	number := fs.Int("issue-number", 0, "Issue number for live state fingerprint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if *number > 0 {
		state, err := loadLiveWork(c, *number)
		if err != nil {
			return err
		}
		fingerprint, err := fingerprintLiveWork(state)
		if err != nil {
			return err
		}
		report := map[string]any{"config": c, "issue_number": *number, "issue_id": state.IssueID, "status": state.Status, "lease_holder": state.Holder, "lease_expires": state.Expiry, "branch": state.Branch, "state_fingerprint": fingerprint}
		storeMetadata("issues/"+strconv.Itoa(*number), report)
		return printValue(report, *asJSON)
	}
	storeMetadata("repository/config", c)
	return printValue(c, *asJSON)
}

func storeMetadata(key string, value any) {
	cache, err := github.NewMetadataCache()
	if err == nil {
		_ = cache.StoreJSON(key, value)
	}
}
func validateCommand(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	query := fmt.Sprintf(`query { repository(owner:%q,name:%q) { issues(first:100,states:[OPEN,CLOSED]) { nodes {
id number body state parent { id }
blockedBy(first:100) { nodes { id } }
labels(first:100) { nodes { name } }
comments(first:100) { nodes { body author { login } } }
projectItems(first:20) { nodes { project { id } fieldValues(first:30) { nodes {
... on ProjectV2ItemFieldSingleSelectValue { name field { ... on ProjectV2SingleSelectField { name } } }
... on ProjectV2ItemFieldTextValue { text field { ... on ProjectV2Field { name } } }
} } } }
} } } }`, c.Owner, c.Repository)
	output, err := exec.Command("gh", "api", "graphql", "-f", "query="+query).Output()
	if err != nil {
		return err
	}
	var response struct {
		Data struct {
			Repository struct {
				Issues struct {
					Nodes []struct {
						ID     string `json:"id"`
						Number int    `json:"number"`
						Body   string `json:"body"`
						State  string `json:"state"`
						Parent *struct {
							ID string `json:"id"`
						} `json:"parent"`
						BlockedBy struct {
							Nodes []struct {
								ID string `json:"id"`
							} `json:"nodes"`
						} `json:"blockedBy"`
						Labels struct {
							Nodes []struct {
								Name string `json:"name"`
							} `json:"nodes"`
						} `json:"labels"`
						Comments struct {
							Nodes []struct {
								Body   string `json:"body"`
								Author struct {
									Login string `json:"login"`
								} `json:"author"`
							} `json:"nodes"`
						} `json:"comments"`
						ProjectItems struct {
							Nodes []struct {
								Project struct {
									ID string `json:"id"`
								} `json:"project"`
								FieldValues struct {
									Nodes []struct {
										Name  string `json:"name"`
										Text  string `json:"text"`
										Field struct {
											Name string `json:"name"`
										} `json:"field"`
									} `json:"nodes"`
								} `json:"fieldValues"`
							} `json:"nodes"`
						} `json:"projectItems"`
					} `json:"nodes"`
				} `json:"issues"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return err
	}
	issues := make([]model.Issue, 0, len(response.Data.Repository.Issues.Nodes))
	for _, node := range response.Data.Repository.Issues.Nodes {
		kind, area := "", ""
		labels := make([]string, 0, len(node.Labels.Nodes))
		for _, label := range node.Labels.Nodes {
			labels = append(labels, label.Name)
			if strings.HasPrefix(label.Name, "kind:") {
				kind = strings.TrimPrefix(label.Name, "kind:")
				kind = strings.ToUpper(kind[:1]) + kind[1:]
			}
			if strings.HasPrefix(label.Name, "area:") {
				area = strings.TrimPrefix(label.Name, "area:")
			}
		}
		status, projectKind, projectArea, branch := "", "", "", ""
		for _, item := range node.ProjectItems.Nodes {
			if item.Project.ID != c.Project.ID {
				continue
			}
			for _, value := range item.FieldValues.Nodes {
				switch value.Field.Name {
				case "Status":
					status = value.Name
				case "Kind":
					projectKind = value.Name
				case "Area":
					projectArea = value.Name
				case "Branch":
					branch = value.Text
				}
			}
		}
		parent := ""
		if node.Parent != nil {
			parent = node.Parent.ID
		}
		blockers := make([]string, 0, len(node.BlockedBy.Nodes))
		for _, blocker := range node.BlockedBy.Nodes {
			blockers = append(blockers, blocker.ID)
		}
		issues = append(issues, model.Issue{ID: node.ID, Number: node.Number, Kind: kind, Status: status, State: node.State, Area: area, ProjectKind: projectKind, ProjectArea: projectArea, Branch: branch, ParentID: parent, BlockerIDs: blockers, Labels: labels, EpicApproved: epicApprovalEvidence(c.Owner, node.Comments.Nodes), Body: node.Body})
	}
	err = model.ValidateGraph(issues)
	openIssues := 0
	for _, issue := range issues {
		if issue.State != "CLOSED" {
			openIssues++
		}
	}
	report := map[string]any{"repository": c.Owner + "/" + c.Repository, "open_issues": openIssues, "valid": err == nil}
	if *asJSON {
		if err != nil {
			report["error"] = err.Error()
		}
		return printValue(report, true)
	}
	if err != nil {
		return err
	}
	fmt.Printf("valid: %d open Issues\n", openIssues)
	return nil
}

func epicApprovalEvidence(owner string, comments []struct {
	Body   string `json:"body"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
}) bool {
	for _, comment := range comments {
		if strings.EqualFold(comment.Author.Login, owner) && strings.HasPrefix(strings.TrimSpace(comment.Body), "Owner approval evidence:") {
			return true
		}
	}
	return false
}

func requestCommand(args []string) error {
	if len(args) == 0 || args[0] != "status" {
		return errors.New("usage: quipu request status --issue-number N [--request-id ID] [--json] | --issue-id ID --fingerprint SHA --status STATUS --apply")
	}
	if contains(args[1:], "--apply") {
		return submitRequest("status", args[1:], nil)
	}
	fs := flag.NewFlagSet("request status", flag.ContinueOnError)
	number := fs.Int("issue-number", 0, "Issue number")
	requestID := fs.String("request-id", "", "request ID")
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *number < 1 {
		return errors.New("request status requires --issue-number")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	comments, err := github.NewClient().ListComments(context.Background(), c.Owner, c.Repository, *number)
	if err != nil {
		return err
	}
	report := requestStatusReport(comments, *requestID)
	if len(report) == 0 && *requestID != "" {
		return fmt.Errorf("request %q not found on Issue #%d", *requestID, *number)
	}
	if *asJSON {
		return printValue(map[string]any{"issue_number": *number, "requests": report}, true)
	}
	for _, item := range report {
		fmt.Printf("%s %s %s %s\n", item.ID, item.Action, item.State, item.Detail)
	}
	return nil
}

type requestStatus struct {
	ID      string `json:"id"`
	Action  string `json:"action"`
	Actor   string `json:"actor"`
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
	Receipt string `json:"receipt_fingerprint,omitempty"`
}

func requestStatusReport(comments []github.Comment, onlyID string) []requestStatus {
	requests := make(map[string]requestStatus)
	order := make([]string, 0)
	for _, comment := range comments {
		request, err := writer.ParseRequest(comment.Body)
		if err != nil || (onlyID != "" && request.ID != onlyID) {
			continue
		}
		requests[request.ID] = requestStatus{ID: request.ID, Action: request.Action, Actor: request.Actor, State: "pending"}
		order = append(order, request.ID)
	}
	for _, comment := range comments {
		receipt, err := writer.ParseReceipt(comment.Body)
		if err != nil {
			continue
		}
		item, ok := requests[receipt.RequestID]
		if !ok {
			continue
		}
		item.State, item.Detail, item.Receipt = receipt.Result, receipt.Detail, receipt.Fingerprint
		requests[receipt.RequestID] = item
	}
	report := make([]requestStatus, 0, len(order))
	for _, id := range order {
		report = append(report, requests[id])
	}
	return report
}
func workCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: quipu work {list|claim|start|renew|release|block|resume}")
	}
	if args[0] == "list" {
		return workListCommand(args[1:])
	}
	return submitRequest(args[0], args[1:], nil)
}

type readyWork struct {
	IssueID  string `json:"issue_id"`
	Number   int    `json:"number"`
	Title    string `json:"title"`
	Kind     string `json:"kind"`
	Area     string `json:"area"`
	Priority string `json:"priority"`
}

type workCandidate struct {
	readyWork
	Body          string
	ParentID      string
	HasChildren   bool
	Status        string
	LeaseHolder   string
	LeaseExpires  string
	BlockerStates []string
}

func workListCommand(args []string) error {
	fs := flag.NewFlagSet("work list", flag.ContinueOnError)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	candidates, err := loadWorkCandidates(c)
	if err != nil {
		return err
	}
	ready := selectReadyWork(candidates, time.Now())
	if *asJSON {
		return printValue(map[string]any{"repository": c.Owner + "/" + c.Repository, "work": ready}, true)
	}
	for _, item := range ready {
		fmt.Printf("#%d [%s/%s/%s] %s\n", item.Number, item.Kind, item.Area, item.Priority, item.Title)
	}
	return nil
}

// loadWorkCandidates reads open Issues with the Project fields that decide
// whether work can be picked up.
func loadWorkCandidates(c model.Config) ([]workCandidate, error) {
	query := fmt.Sprintf(`query { repository(owner:%q,name:%q) { issues(first:100,states:OPEN) { nodes {
id number title body parent { id } subIssues(first:1) { nodes { id } } blockedBy(first:100) { nodes { state } }
projectItems(first:20) { nodes { project { id } fieldValues(first:30) { nodes {
... on ProjectV2ItemFieldSingleSelectValue { name field { ... on ProjectV2SingleSelectField { name } } }
... on ProjectV2ItemFieldTextValue { text field { ... on ProjectV2Field { name } } }
} } } }
} } } }`, c.Owner, c.Repository)
	output, err := ghOutput("api", "graphql", "-f", "query="+query)
	if err != nil {
		return nil, err
	}
	var response struct {
		Data struct {
			Repository struct {
				Issues struct {
					Nodes []struct {
						ID     string `json:"id"`
						Number int    `json:"number"`
						Title  string `json:"title"`
						Body   string `json:"body"`
						Parent *struct {
							ID string `json:"id"`
						} `json:"parent"`
						SubIssues struct {
							Nodes []struct {
								ID string `json:"id"`
							} `json:"nodes"`
						} `json:"subIssues"`
						BlockedBy struct {
							Nodes []struct {
								State string `json:"state"`
							} `json:"nodes"`
						} `json:"blockedBy"`
						ProjectItems struct {
							Nodes []struct {
								Project struct {
									ID string `json:"id"`
								} `json:"project"`
								FieldValues struct {
									Nodes []struct {
										Name  string `json:"name"`
										Text  string `json:"text"`
										Field struct {
											Name string `json:"name"`
										} `json:"field"`
									} `json:"nodes"`
								} `json:"fieldValues"`
							} `json:"nodes"`
						} `json:"projectItems"`
					} `json:"nodes"`
				} `json:"issues"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, err
	}
	candidates := make([]workCandidate, 0, len(response.Data.Repository.Issues.Nodes))
	for _, issue := range response.Data.Repository.Issues.Nodes {
		candidate := workCandidate{readyWork: readyWork{IssueID: issue.ID, Number: issue.Number, Title: issue.Title}, Body: issue.Body, HasChildren: len(issue.SubIssues.Nodes) != 0}
		if issue.Parent != nil {
			candidate.ParentID = issue.Parent.ID
		}
		for _, blocker := range issue.BlockedBy.Nodes {
			candidate.BlockerStates = append(candidate.BlockerStates, blocker.State)
		}
		for _, item := range issue.ProjectItems.Nodes {
			if item.Project.ID != c.Project.ID {
				continue
			}
			for _, value := range item.FieldValues.Nodes {
				switch value.Field.Name {
				case "Status":
					candidate.Status = value.Name
				case "Lease holder":
					candidate.LeaseHolder = value.Text
				case "Lease expires":
					candidate.LeaseExpires = value.Text
				case "Kind":
					candidate.Kind = value.Name
				case "Area":
					candidate.Area = value.Name
				case "Priority":
					candidate.Priority = value.Name
				}
			}
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func selectReadyWork(candidates []workCandidate, now time.Time) []readyWork {
	ready := make([]readyWork, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Status != "Ready" || candidate.ParentID == "" || candidate.HasChildren || strings.EqualFold(candidate.Kind, "Epic") {
			continue
		}
		if err := model.ValidateWorkContract(candidate.Body); err != nil {
			continue
		}
		blocked := false
		for _, state := range candidate.BlockerStates {
			if state != "CLOSED" {
				blocked = true
				break
			}
		}
		if blocked || hasActiveLease(candidate.LeaseHolder, candidate.LeaseExpires, now) {
			continue
		}
		ready = append(ready, candidate.readyWork)
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Number < ready[j].Number })
	return ready
}

func hasActiveLease(holder, expires string, now time.Time) bool {
	if holder == "" || expires == "" {
		return false
	}
	expiry, err := time.Parse(time.RFC3339, expires)
	return err == nil && expiry.After(now.UTC())
}
func evidenceCommand(args []string) error {
	if len(args) == 0 || args[0] != "submit" {
		return errors.New("usage: quipu evidence submit")
	}
	e := &writer.Evidence{}
	return submitRequest("evidence.submit", args[1:], e)
}
func submitRequest(action string, args []string, evidence *writer.Evidence) error {
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	number := fs.Int("issue-number", 0, "issue number")
	id := fs.String("issue-id", "", "issue node ID")
	actor := fs.String("actor", os.Getenv("USER"), "actor identity")
	fingerprint := fs.String("fingerprint", "", "observed fingerprint")
	apply := applyFlag(fs)
	lease := fs.String("lease-expires", "", "RFC3339 expiry")
	branch := fs.String("branch", "", "NNN/short-description branch")
	status := fs.String("status", "", "requested Project Status")
	pr := fs.String("pr", "", "pull request URL")
	var finalSHA, ciURL, commands, environments, criteria, artifacts, documentation, risks, boundary *string
	if evidence != nil {
		finalSHA = fs.String("final-sha", "", "final merged commit SHA")
		ciURL = fs.String("ci-url", "", "CI run URL")
		commands = fs.String("commands", "", "exact verification commands")
		environments = fs.String("environments", "", "verified environments")
		criteria = fs.String("criteria", "", "acceptance-criteria result")
		artifacts = fs.String("artifacts", "", "artifact, screenshot, or log URLs; None: reason")
		documentation = fs.String("documentation", "", "documentation changes; None: reason")
		risks = fs.String("risks", "", "known residual risks; None")
		boundary = fs.String("boundary", "", "local|CI|target host|provider|staging|production|release")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*apply {
		return errors.New("changing commands require --apply")
	}
	if *number < 1 || *id == "" || *fingerprint == "" {
		return errors.New("--issue-number, --issue-id, and --fingerprint are required")
	}
	if evidence != nil {
		*evidence = writer.Evidence{FinalSHA: *finalSHA, CIURL: *ciURL, Commands: *commands, Environments: *environments, Criteria: *criteria, Artifacts: *artifacts, Documentation: *documentation, Risks: *risks, Boundary: *boundary}
		if err := evidence.Validate(); err != nil {
			return err
		}
		if *pr == "" {
			return errors.New("evidence submit requires --pr")
		}
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	r := writer.Request{ID: fmt.Sprintf("%d", time.Now().UnixNano()), Action: action, IssueID: *id, Actor: *actor, ExpectedFingerprint: *fingerprint, LeaseExpiresAt: *lease, Branch: *branch, Status: *status, PR: *pr, Evidence: evidence}
	body, err := writer.RenderRequest(r)
	if err != nil {
		return err
	}
	comment, err := github.NewClient().CreateComment(context.Background(), c.Owner, c.Repository, *number, body)
	if err != nil {
		return err
	}
	fmt.Printf("request %s posted as comment %d\n", r.ID, comment.ID)
	return nil
}
func journalCommand(args []string) error {
	if len(args) == 0 || args[0] != "render" {
		return errors.New("usage: quipu journal render --request-id ID --date YYYY-MM-DD --issue '#N' --pr URL --outcome TEXT --proof TEXT --boundary TEXT --next-blocker TEXT")
	}
	fs := flag.NewFlagSet("journal render", flag.ContinueOnError)
	requestID := fs.String("request-id", "", "Writer request ID")
	date := fs.String("date", "", "UTC date, YYYY-MM-DD")
	issue := fs.String("issue", "", "Issue reference")
	pr := fs.String("pr", "", "pull request URL or None")
	outcome := fs.String("outcome", "", "outcome summary")
	proof := fs.String("proof", "", "proof summary")
	boundary := fs.String("boundary", "", "proof boundary")
	nextBlocker := fs.String("next-blocker", "", "next blocker or None")
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *requestID == "" || *date == "" || *issue == "" || *outcome == "" || *proof == "" || *boundary == "" || *nextBlocker == "" {
		return errors.New("journal render requires request-id, date, issue, outcome, proof, boundary, and next-blocker")
	}
	entry := journal.Entry{RequestID: *requestID, Date: *date, Issue: *issue, PR: *pr, Outcome: *outcome, Proof: *proof, Boundary: *boundary, NextBlocker: *nextBlocker}
	if *asJSON {
		return printValue(map[string]any{"entry": entry, "markdown": journal.Render(entry)}, true)
	}
	fmt.Print(journal.Render(entry))
	return nil
}

func configureCommand(args []string) error {
	if len(args) == 0 || args[0] != "writer-token" {
		return errors.New("usage: quipu configure writer-token --apply < token")
	}
	fs := flag.NewFlagSet("configure writer-token", flag.ContinueOnError)
	apply := applyFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !*apply {
		return errors.New("writer-token configuration requires --apply")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	cmd := exec.Command("gh", "secret", "set", "QUIPU_WRITER_TOKEN", "--repo", c.Owner+"/"+c.Repository)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("set QUIPU_WRITER_TOKEN: %w", err)
	}
	fmt.Println("QUIPU_WRITER_TOKEN configured; token was not read, printed, or persisted locally")
	return nil
}

func writerCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: quipu writer {run|reconcile}")
	}
	if args[0] == "reconcile" {
		return writerReconcileCommand(args[1:])
	}
	if args[0] != "run" {
		return errors.New("usage: quipu writer {run|reconcile}")
	}
	fs := flag.NewFlagSet("writer run", flag.ContinueOnError)
	number := fs.Int("issue-number", 0, "issue number")
	apply := applyFlag(fs)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *number < 1 {
		return errors.New("--issue-number is required")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	client := github.NewClient()
	comments, err := client.ListComments(context.Background(), c.Owner, c.Repository, *number)
	if err != nil {
		return err
	}
	pending, rejected := writer.Pending(comments)
	receiptsApplied := 0
	if *apply {
		for _, item := range pending {
			receipt := applyAuthorizedWriterRequest(c, *number, item, comments)
			receipt = publishEvidenceJournal(c, *number, item.Request, receipt)
			rejected = append(rejected, receipt)
		}
		for _, receipt := range rejected {
			body, err := writer.RejectionComment(receipt)
			if err != nil {
				return err
			}
			if _, err := client.CreateComment(context.Background(), c.Owner, c.Repository, *number, body); err != nil {
				return err
			}
			receiptsApplied++
		}
	}
	ids := make([]string, 0, len(pending))
	for _, item := range pending {
		ids = append(ids, item.Request.ID)
	}
	return printValue(map[string]any{"issue_number": *number, "pending_request_ids": ids, "receipts_applied": receiptsApplied, "apply": *apply, "note": "lifecycle requests require fresh fingerprint and configured Project membership"}, *asJSON)
}

func writerReconcileCommand(args []string) error {
	fs := flag.NewFlagSet("writer reconcile", flag.ContinueOnError)
	apply := applyFlag(fs)
	asJSON := jsonFlag(fs)
	since := fs.Duration("since", 0, "only Issues updated within this window, such as 30m; all Issues when 0")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	output, err := ghOutput("api", "--paginate", "--slurp", reconcileIssuesPath(c, *since, time.Now()))
	if err != nil {
		return err
	}
	var pages [][]struct {
		Number      int             `json:"number"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	if err := json.Unmarshal(output, &pages); err != nil {
		return err
	}
	issues := make([]struct {
		Number      int             `json:"number"`
		PullRequest json.RawMessage `json:"pull_request"`
	}, 0)
	for _, page := range pages {
		for _, item := range page {
			// REST's Issue endpoint also returns pull requests. Writer state is
			// valid only for canonical Issues, never PR numbers.
			if len(item.PullRequest) == 0 || string(item.PullRequest) == "null" {
				issues = append(issues, item)
			}
		}
	}
	report := map[string]any{"issues_scanned": len(issues), "requests_receipted": 0, "leases_reclaimed": 0, "apply": *apply}
	for _, issue := range issues {
		receipted, reclaimed, err := reconcileIssue(c, issue.Number, *apply)
		if err != nil {
			return fmt.Errorf("reconcile Issue #%d: %w", issue.Number, err)
		}
		report["requests_receipted"] = report["requests_receipted"].(int) + receipted
		report["leases_reclaimed"] = report["leases_reclaimed"].(int) + reclaimed
	}
	return printValue(report, *asJSON)
}

// reconcileIssuesPath lists the Issues a reconcile pass scans. With since,
// only recently updated Issues: the comment-triggered Writer run uses this to
// drain every pending request, because Actions cancels queued runs in its
// concurrency group when many agents post requests at once.
func reconcileIssuesPath(c model.Config, since time.Duration, now time.Time) string {
	path := "repos/" + c.Owner + "/" + c.Repository + "/issues?state=all&per_page=100"
	if since > 0 {
		path += "&since=" + now.Add(-since).UTC().Format(time.RFC3339)
	}
	return path
}

func reconcileIssue(c model.Config, number int, apply bool) (int, int, error) {
	client := github.NewClient()
	comments, err := client.ListComments(context.Background(), c.Owner, c.Repository, number)
	if err != nil {
		return 0, 0, err
	}
	pending, rejected := writer.Pending(comments)
	receipted := 0
	if apply {
		for _, item := range pending {
			receipt := applyAuthorizedWriterRequest(c, number, item, comments)
			rejected = append(rejected, publishEvidenceJournal(c, number, item.Request, receipt))
		}
		for _, receipt := range rejected {
			body, err := writer.RejectionComment(receipt)
			if err != nil {
				return 0, 0, err
			}
			if _, err := client.CreateComment(context.Background(), c.Owner, c.Repository, number, body); err != nil {
				return 0, 0, err
			}
			receipted++
		}
	}
	if apply {
		repaired, err := retryPendingWikiJournals(c, number, comments)
		if err != nil {
			return receipted, 0, err
		}
		receipted += repaired
	}
	state, err := loadLiveWork(c, number)
	if err != nil {
		return receipted, 0, err
	}
	if state.IssueState == "CLOSED" && !terminalIssueStatus(state.Status) && hasWriterReceipt(comments) {
		if !apply {
			return receipted, 0, nil
		}
		if err := client.SetIssueState(context.Background(), c.Owner, c.Repository, number, "open"); err != nil {
			return receipted, 0, fmt.Errorf("reopen prematurely closed Issue: %w", err)
		}
		requestID := "premature-close-" + state.IssueID + "-" + state.UpdatedAt
		body, err := writer.RenderReceipt(writer.Receipt{RequestID: requestID, Result: "accepted", Detail: "manual close denied; Issue reopened because completion evidence is incomplete", At: time.Now().UTC()})
		if err != nil {
			return receipted, 0, err
		}
		if _, err := client.CreateComment(context.Background(), c.Owner, c.Repository, number, body); err != nil {
			return receipted, 0, err
		}
		return receipted + 1, 0, nil
	}
	if state.IssueState == "CLOSED" {
		return receipted, 0, nil
	}
	if apply {
		if err := initializeProjectFields(c, number, state); err != nil {
			return receipted, 0, err
		}
		state, err = loadLiveWork(c, number)
		if err != nil {
			return receipted, 0, err
		}
	}
	expires, _ := time.Parse(time.RFC3339, state.Expiry)
	next, reclaimed := writer.ReclaimExpired(writer.WorkState{Status: state.Status, Lease: writer.Lease{Holder: state.Holder, Expires: expires, Branch: state.Branch}}, time.Now())
	if !reclaimed {
		return receipted, 0, nil
	}
	if !apply {
		return receipted, 1, nil
	}
	if err := updateLiveWork(c, state.ItemID, next); err != nil {
		return receipted, 0, err
	}
	requestID := fmt.Sprintf("lease-expiry-%s-%s", state.IssueID, state.Expiry)
	for _, comment := range comments {
		if strings.Contains(comment.Body, "request="+requestID) {
			return receipted, 1, nil
		}
	}
	body, err := writer.RenderReceipt(writer.Receipt{RequestID: requestID, Result: "accepted", Detail: "expired lease cleared; Issue returned to Ready", At: time.Now().UTC()})
	if err != nil {
		return receipted, 0, err
	}
	if _, err := client.CreateComment(context.Background(), c.Owner, c.Repository, number, body); err != nil {
		return receipted, 0, err
	}
	return receipted + 1, 1, nil
}

func applyAuthorizedWriterRequest(c model.Config, number int, pending writer.PendingRequest, comments []github.Comment) writer.Receipt {
	if !authorizedActor(c.AuthorizedActors, pending.Author) {
		return rejectedReceipt(pending.Request, fmt.Errorf("request author %q is not authorized by repository config", pending.Author))
	}
	return applyWriterRequest(c, number, pending.Request, comments)
}

func authorizedActor(allowed []string, actor string) bool {
	for _, candidate := range allowed {
		if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(actor)) {
			return true
		}
	}
	return false
}

func terminalIssueStatus(status string) bool {
	return status == "Done" || status == "Cancelled" || status == "Archived"
}

func hasWriterReceipt(comments []github.Comment) bool {
	for _, comment := range comments {
		if _, err := writer.ParseReceipt(comment.Body); err == nil {
			return true
		}
	}
	return false
}

type liveWork struct {
	IssueID    string
	IssueState string
	Body       string
	UpdatedAt  string
	ParentID   string
	Blockers   []liveBlocker
	ItemID     string
	Status     string
	Holder     string
	Expiry     string
	Branch     string
	Kind       string
	Area       string
	Priority   string
	Proof      string
}

type liveBlocker struct{ ID, State string }

func applyWriterRequest(c model.Config, number int, request writer.Request, comments []github.Comment) writer.Receipt {
	state, err := loadLiveWork(c, number)
	if err != nil {
		return rejectedReceipt(request, err)
	}
	if state.IssueID != request.IssueID {
		return rejectedReceipt(request, errors.New("request Issue ID does not match target Issue"))
	}
	actual, err := writer.RequireFreshFingerprint(request.ExpectedFingerprint, liveWorkFingerprint(state))
	if err != nil {
		return writer.Receipt{RequestID: request.ID, Fingerprint: actual, Result: "rejected", Detail: err.Error(), At: time.Now().UTC()}
	}
	if request.Action == "claim" || (request.Action == "status" && request.Status == "Done") {
		for _, blocker := range state.Blockers {
			if blocker.State != "CLOSED" {
				return rejectedReceipt(request, fmt.Errorf("unresolved blocker %s prevents %s", blocker.ID, request.Action))
			}
		}
	}
	if request.Action == "status" && request.Status == "Done" {
		if !acceptedEvidence(comments) {
			return rejectedReceipt(request, errors.New("Done requires a prior accepted evidence receipt"))
		}
		if c.WikiMode == "journal" && pendingWikiJournal(comments) {
			return rejectedReceipt(request, errors.New("Done requires generated Wiki journal to be synchronized"))
		}
	}
	if request.Action == "pr.link" {
		if err := validateReviewPR(request.PR); err != nil {
			return rejectedReceipt(request, err)
		}
	}
	if request.Action == "evidence.submit" {
		if err := validateMergedEvidencePR(request.PR, request.Evidence.FinalSHA); err != nil {
			return rejectedReceipt(request, err)
		}
	}
	current := writer.WorkState{Status: state.Status, Lease: writer.Lease{Holder: state.Holder, Branch: state.Branch}}
	if state.Expiry != "" {
		current.Lease.Expires, _ = time.Parse(time.RFC3339, state.Expiry)
	}
	next, err := writer.ApplyLifecycle(current, request, time.Now())
	if err != nil {
		return rejectedReceipt(request, err)
	}
	if err := updateLiveWork(c, state.ItemID, next); err != nil {
		return rejectedReceipt(request, err)
	}
	if next.Status == "Done" {
		if err := github.NewClient().SetIssueState(context.Background(), c.Owner, c.Repository, number, "closed"); err != nil {
			return rejectedReceipt(request, fmt.Errorf("close completed Issue: %w", err))
		}
	}
	receipt := writer.Receipt{RequestID: request.ID, Fingerprint: actual, Result: "accepted", Detail: "lifecycle state changed to " + next.Status, At: time.Now().UTC()}
	if request.Action == "evidence.submit" {
		receipt.Detail = "evidence recorded; lifecycle state changed to Evidence pending"
		receipt.Evidence = request.Evidence
	}
	if next.Status == "Done" {
		receipt.Detail = "completion evidence verified; lifecycle state changed to Done and Issue closed"
	}
	return receipt
}

func acceptedEvidence(comments []github.Comment) bool {
	bodies := make([]string, 0, len(comments))
	for _, comment := range comments {
		bodies = append(bodies, comment.Body)
	}
	return writer.HasAcceptedEvidence(bodies)
}

func pendingWikiJournal(comments []github.Comment) bool {
	for _, comment := range comments {
		receipt, err := writer.ParseReceipt(comment.Body)
		if err == nil && receipt.Result == "accepted" && receipt.Evidence != nil && strings.Contains(receipt.Detail, "Wiki journal pending") {
			if !hasReceipt(comments, "wiki-retry-"+receipt.RequestID) {
				return true
			}
		}
	}
	return false
}

func publishEvidenceJournal(c model.Config, number int, request writer.Request, receipt writer.Receipt) writer.Receipt {
	if c.WikiMode != "journal" || receipt.Result != "accepted" || receipt.Evidence == nil {
		return receipt
	}
	entry := journal.Entry{RequestID: receipt.RequestID, Date: receipt.At.UTC().Format("2006-01-02"), Issue: fmt.Sprintf("#%d", number), PR: request.PR, Outcome: receipt.Detail, Proof: receipt.Evidence.Criteria, Boundary: receipt.Evidence.Boundary, NextBlocker: "None"}
	published, err := journal.PublishWiki(context.Background(), c.Owner, c.Repository, github.NewClient().Token, entry)
	if err != nil {
		receipt.Detail += "; Wiki journal pending: " + strings.ReplaceAll(err.Error(), "\n", " ")
		return receipt
	}
	if published {
		receipt.Detail += "; Wiki journal appended"
	}
	return receipt
}

func retryPendingWikiJournals(c model.Config, number int, comments []github.Comment) (int, error) {
	if c.WikiMode != "journal" {
		return 0, nil
	}
	client := github.NewClient()
	repaired := 0
	for _, comment := range comments {
		receipt, err := writer.ParseReceipt(comment.Body)
		if err != nil || receipt.Result != "accepted" || receipt.Evidence == nil || !strings.Contains(receipt.Detail, "Wiki journal pending") {
			continue
		}
		repairID := "wiki-retry-" + receipt.RequestID
		if hasReceipt(comments, repairID) {
			continue
		}
		entry := journal.Entry{RequestID: receipt.RequestID, Date: receipt.At.UTC().Format("2006-01-02"), Issue: fmt.Sprintf("#%d", number), Outcome: "evidence journal repair", Proof: receipt.Evidence.Criteria, Boundary: receipt.Evidence.Boundary, NextBlocker: "None"}
		if _, err := journal.PublishWiki(context.Background(), c.Owner, c.Repository, client.Token, entry); err != nil {
			continue
		}
		body, err := writer.RenderReceipt(writer.Receipt{RequestID: repairID, Result: "accepted", Detail: "generated Wiki journal repaired for evidence request " + receipt.RequestID, At: time.Now().UTC()})
		if err != nil {
			return repaired, err
		}
		if _, err := client.CreateComment(context.Background(), c.Owner, c.Repository, number, body); err != nil {
			return repaired, err
		}
		repaired++
	}
	return repaired, nil
}

func hasReceipt(comments []github.Comment, requestID string) bool {
	for _, comment := range comments {
		receipt, err := writer.ParseReceipt(comment.Body)
		if err == nil && receipt.RequestID == requestID {
			return true
		}
	}
	return false
}

func validateReviewPR(pr string) error {
	if pr == "" {
		return errors.New("PR link requires pull request URL")
	}
	output, err := exec.Command("gh", "pr", "view", pr, "--json", "state,url").Output()
	if err != nil {
		return fmt.Errorf("read pull request: %w", err)
	}
	var result struct {
		State string `json:"state"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return err
	}
	if result.URL != pr || result.State != "OPEN" {
		return errors.New("PR link requires an open canonical pull request URL")
	}
	return nil
}

func validateMergedEvidencePR(pr, finalSHA string) error {
	if pr == "" || finalSHA == "" {
		return errors.New("evidence requires PR URL and final SHA")
	}
	output, err := exec.Command("gh", "pr", "view", pr, "--json", "state,url,mergeCommit").Output()
	if err != nil {
		return fmt.Errorf("read pull request: %w", err)
	}
	var result struct {
		State       string `json:"state"`
		URL         string `json:"url"`
		MergeCommit struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return err
	}
	if result.URL != pr || result.State != "MERGED" {
		return errors.New("evidence requires merged canonical pull request URL")
	}
	if result.MergeCommit.OID != finalSHA {
		return fmt.Errorf("evidence final SHA %s does not match merged PR head %s", finalSHA, result.MergeCommit.OID)
	}
	return nil
}

func fingerprintLiveWork(state liveWork) (string, error) {
	return writer.StateFingerprint(liveWorkFingerprint(state))
}

func liveWorkFingerprint(state liveWork) writer.Fingerprint {
	// GitHub updates Issue.updatedAt when quipu posts its durable request comment.
	// Including that value makes every request stale before the Writer can read
	// it. The fingerprint therefore covers authoritative graph and Project state,
	// while request parsing separately rejects edited request comments.
	fingerprint := writer.Fingerprint{IssueID: state.IssueID, IssueState: state.IssueState, ParentID: state.ParentID, Project: map[string]string{"status": state.Status, "lease_holder": state.Holder, "lease_expires": state.Expiry, "branch": state.Branch}}
	for _, blocker := range state.Blockers {
		fingerprint.DependencyIDs = append(fingerprint.DependencyIDs, blocker.ID)
	}
	return fingerprint
}

func rejectedReceipt(request writer.Request, err error) writer.Receipt {
	return writer.Receipt{RequestID: request.ID, Fingerprint: request.ExpectedFingerprint, Result: "rejected", Detail: err.Error(), At: time.Now().UTC()}
}

func loadLiveWork(c model.Config, number int) (liveWork, error) {
	query := fmt.Sprintf(`query { repository(owner:%q,name:%q) { issue(number:%d) {
id state body updatedAt parent { id } blockedBy(first:100) { nodes { id state } }
projectItems(first:20) { nodes { id project { id } fieldValues(first:30) { nodes {
... on ProjectV2ItemFieldSingleSelectValue { name field { ... on ProjectV2SingleSelectField { name } } }
... on ProjectV2ItemFieldTextValue { text field { ... on ProjectV2Field { name } } }
} } } }
} } }`, c.Owner, c.Repository, number)
	output, err := ghOutput("api", "graphql", "-f", "query="+query)
	if err != nil {
		return liveWork{}, err
	}
	var response struct {
		Data struct {
			Repository struct {
				Issue struct {
					ID        string `json:"id"`
					State     string `json:"state"`
					Body      string `json:"body"`
					UpdatedAt string `json:"updatedAt"`
					Parent    *struct {
						ID string `json:"id"`
					} `json:"parent"`
					BlockedBy struct {
						Nodes []struct {
							ID    string `json:"id"`
							State string `json:"state"`
						} `json:"nodes"`
					} `json:"blockedBy"`
					ProjectItems struct {
						Nodes []struct {
							ID      string `json:"id"`
							Project struct {
								ID string `json:"id"`
							} `json:"project"`
							FieldValues struct {
								Nodes []struct {
									Name  string `json:"name"`
									Text  string `json:"text"`
									Field struct {
										Name string `json:"name"`
									} `json:"field"`
								} `json:"nodes"`
							} `json:"fieldValues"`
						} `json:"nodes"`
					} `json:"projectItems"`
				} `json:"issue"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return liveWork{}, err
	}
	issue := response.Data.Repository.Issue
	if issue.ID == "" {
		return liveWork{}, errors.New("Issue not found")
	}
	state := liveWork{IssueID: issue.ID, IssueState: issue.State, Body: issue.Body, UpdatedAt: issue.UpdatedAt}
	if issue.Parent != nil {
		state.ParentID = issue.Parent.ID
	}
	for _, blocker := range issue.BlockedBy.Nodes {
		state.Blockers = append(state.Blockers, liveBlocker{ID: blocker.ID, State: blocker.State})
	}
	for _, item := range issue.ProjectItems.Nodes {
		if item.Project.ID != c.Project.ID {
			continue
		}
		state.ItemID = item.ID
		for _, value := range item.FieldValues.Nodes {
			switch value.Field.Name {
			case "Status":
				state.Status = value.Name
			case "Lease holder":
				state.Holder = value.Text
			case "Lease expires":
				state.Expiry = value.Text
			case "Branch":
				state.Branch = value.Text
			case "Kind":
				state.Kind = value.Name
			case "Area":
				state.Area = value.Name
			case "Priority":
				state.Priority = value.Name
			case "Proof":
				state.Proof = value.Name
			}
		}
	}
	if state.ItemID == "" {
		return liveWork{}, errors.New("Issue is not a member of configured Project")
	}
	return state, nil
}

type projectField struct {
	ID      string
	Options map[string]string
}

func updateLiveWork(c model.Config, itemID string, state writer.WorkState) error {
	fields, err := configuredProjectFields(c)
	if err != nil {
		return err
	}
	values := []struct {
		name  string
		value string
		text  bool
	}{
		{"Lease holder", state.Lease.Holder, true},
		{"Lease expires", leaseText(state.Lease.Expires), true},
		{"Branch", state.Lease.Branch, true},
		{"Status", state.Status, false},
	}
	for _, value := range values {
		field, ok := fields[value.name]
		if !ok {
			return fmt.Errorf("configured Project lacks field %q", value.name)
		}
		if !value.text {
			option, ok := field.Options[value.value]
			if !ok {
				return fmt.Errorf("Project field %s has no option %q", value.name, value.value)
			}
			if err := setProjectFieldValue(c.Project.ID, itemID, field.ID, option, false); err != nil {
				return fmt.Errorf("update %s: %w", value.name, err)
			}
			continue
		}
		if err := setProjectFieldValue(c.Project.ID, itemID, field.ID, value.value, true); err != nil {
			return fmt.Errorf("update %s: %w", value.name, err)
		}
	}
	return nil
}

// initializeProjectFields fills only blank bootstrap fields. Existing values are
// never overwritten; labels remain source for Kind and Area classification.
func initializeProjectFields(c model.Config, number int, state liveWork) error {
	output, err := ghOutput("issue", "view", fmt.Sprint(number), "--repo", c.Owner+"/"+c.Repository, "--json", "labels")
	if err != nil {
		return err
	}
	var result struct {
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return err
	}
	kind, area := "", ""
	for _, label := range result.Labels {
		if strings.HasPrefix(label.Name, "kind:") {
			value := strings.TrimPrefix(label.Name, "kind:")
			kind = strings.ToUpper(value[:1]) + value[1:]
		}
		if strings.HasPrefix(label.Name, "area:") {
			value := strings.TrimPrefix(label.Name, "area:")
			area = strings.ToUpper(value[:1]) + value[1:]
		}
	}
	if kind == "" {
		return errors.New("cannot initialize Project Kind without kind label")
	}
	if area == "" {
		if output, err := exec.Command("gh", "issue", "edit", fmt.Sprint(number), "--repo", c.Owner+"/"+c.Repository, "--add-label", "area:stable").CombinedOutput(); err != nil {
			return fmt.Errorf("initialize area label: %s", strings.TrimSpace(string(output)))
		}
		area = "Stable"
	}
	values := map[string]string{}
	if state.Status == "" {
		values["Status"] = "Backlog"
	}
	if state.Kind == "" {
		values["Kind"] = kind
	}
	if state.Area == "" {
		values["Area"] = area
	}
	if state.Priority == "" {
		values["Priority"] = "P2"
	}
	if state.Proof == "" {
		values["Proof"] = "Not started"
	}
	if len(values) == 0 {
		return nil
	}
	fields, err := configuredProjectFields(c)
	if err != nil {
		return err
	}
	for name, value := range values {
		field, ok := fields[name]
		if !ok || field.Options[value] == "" {
			return fmt.Errorf("configured Project lacks %s option %q", name, value)
		}
		if err := setProjectFieldValue(c.Project.ID, state.ItemID, field.ID, field.Options[value], false); err != nil {
			return fmt.Errorf("initialize %s: %w", name, err)
		}
	}
	return nil
}

func leaseText(expiry time.Time) string {
	if expiry.IsZero() {
		return ""
	}
	return expiry.UTC().Format(time.RFC3339)
}

func configuredProjectFields(c model.Config) (map[string]projectField, error) {
	// Resolve fields from the configured Project node instead of `gh project
	// field-list --owner`. The latter resolves a user account first, which is
	// unavailable to a least-privilege Writer token even when it can mutate the
	// Project by ID.
	query := fmt.Sprintf(`query { node(id:%q) { ... on ProjectV2 { fields(first:100) { nodes {
... on ProjectV2Field { id name }
... on ProjectV2SingleSelectField { id name options { id name } }
} } } } }`, c.Project.ID)
	output, err := ghOutput("api", "graphql", "-f", "query="+query)
	if err != nil {
		return nil, err
	}
	var response struct {
		Data struct {
			Node struct {
				Fields struct {
					Nodes []struct {
						ID      string `json:"id"`
						Name    string `json:"name"`
						Options []struct {
							ID   string `json:"id"`
							Name string `json:"name"`
						} `json:"options"`
					} `json:"nodes"`
				} `json:"fields"`
			} `json:"node"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, err
	}
	fields := make(map[string]projectField, len(response.Data.Node.Fields.Nodes))
	for _, raw := range response.Data.Node.Fields.Nodes {
		field := projectField{ID: raw.ID, Options: map[string]string{}}
		for _, option := range raw.Options {
			field.Options[option.Name] = option.ID
		}
		fields[raw.Name] = field
	}
	return fields, nil
}

// ghOutput preserves GitHub CLI diagnostics. Writer reconciliation must expose
// an authorization or schema failure rather than only returning exit status 1.
func ghOutput(args ...string) ([]byte, error) {
	output, err := exec.Command("gh", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return output, nil
}

// setProjectFieldValue uses the Project V2 GraphQL mutations directly. The gh
// project's item-edit command asks for unrelated read:org and read:discussion
// scopes before issuing its API call, while the Writer needs only Project
// mutation authority for this configured Project ID.
func setProjectFieldValue(projectID, itemID, fieldID, value string, text bool) error {
	var mutation string
	if text && value == "" {
		mutation = fmt.Sprintf(`mutation { clearProjectV2ItemFieldValue(input:{projectId:%q,itemId:%q,fieldId:%q}) { projectV2Item { id } } }`, projectID, itemID, fieldID)
	} else if text {
		mutation = fmt.Sprintf(`mutation { updateProjectV2ItemFieldValue(input:{projectId:%q,itemId:%q,fieldId:%q,value:{text:%q}}) { projectV2Item { id } } }`, projectID, itemID, fieldID, value)
	} else {
		mutation = fmt.Sprintf(`mutation { updateProjectV2ItemFieldValue(input:{projectId:%q,itemId:%q,fieldId:%q,value:{singleSelectOptionId:%q}}) { projectV2Item { id } } }`, projectID, itemID, fieldID, value)
	}
	if _, err := ghOutput("api", "graphql", "-f", "query="+mutation); err != nil {
		return err
	}
	return nil
}
