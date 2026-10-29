package main

// Provisioning shared by `quipu init` and `quipu adopt`. Every step reads
// what GitHub already has and changes only what is missing, so adopting a
// repository twice changes nothing the second time.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/isnakolah/quipu/internal/bootstrap"
	"github.com/isnakolah/quipu/internal/model"
	"gopkg.in/yaml.v3"
)

func adoptCommand(args []string) error {
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	owner := fs.String("owner", "", "GitHub owner")
	repo := fs.String("repo", "", "repository")
	projectNumber := fs.Int("project-number", 0, "existing GitHub Project number")
	setup := addSetupFlags(fs)
	replace := fs.Bool("replace", false, "replace existing workflow and Issue form files that differ from Quipu's")
	apply := applyFlag(fs)
	asJSON := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	here, err := currentWorktree()
	if err != nil {
		return errors.New("run quipu adopt inside a clone of the repository to adopt")
	}
	top := here.Top
	c, adopted, err := existingConfig(top)
	if err != nil {
		return err
	}
	if adopted {
		if (*owner != "" && !strings.EqualFold(*owner, c.Owner)) || (*repo != "" && !strings.EqualFold(*repo, c.Repository)) {
			return fmt.Errorf("%s belongs to %s/%s, not %s/%s", configPath, c.Owner, c.Repository, *owner, *repo)
		}
		if *projectNumber != 0 && *projectNumber != c.Project.Number {
			return fmt.Errorf("%s uses Project #%d; it cannot move to #%d", configPath, c.Project.Number, *projectNumber)
		}
	} else if *owner == "" || *repo == "" {
		return errors.New("--owner and --repo are required")
	}
	report := map[string]any{"owner": c.Owner, "repository": c.Repository, "already_adopted": adopted, "apply": *apply}
	if !adopted {
		report["owner"], report["repository"] = *owner, *repo
		// The audit gates only the first adoption: imported work is fine later.
		count, err := realIssueCount(*owner, *repo)
		if err != nil {
			return err
		}
		report["issues_found"] = count
		report["safe_to_adopt"] = count == 0
		report["note"] = "adopt never reclassifies existing Issues; pull requests do not count"
		if *apply && count != 0 {
			return fmt.Errorf("adopt refuses a repository with %d existing Issues; import or migrate them explicitly", count)
		}
		if *apply && *projectNumber < 1 {
			return errors.New("adopt --apply requires --project-number for an existing Project")
		}
	}
	if !*apply {
		report["next"] = "rerun with --apply to provision; it changes only what is missing"
		return printValue(report, *asJSON)
	}
	if !adopted {
		info, err := readProject(*owner, *projectNumber)
		if err != nil {
			return err
		}
		c = newConfig(*owner, *repo, info)
	}
	if err := setup.apply(fs, &c); err != nil {
		return err
	}
	changes, kept, err := provision(top, c, *replace)
	report["changes"] = changes
	report["kept"] = kept
	if err != nil {
		report["error"] = err.Error()
		_ = printValue(report, *asJSON)
		return errReported
	}
	report["changed"] = len(changes) > 0
	if len(kept) > 0 {
		report["kept_note"] = "these files already existed and differ from Quipu's; review them, or rerun with --replace"
	}
	report["next"] = "commit and push the written files, set the QUIPU_WRITER_TOKEN secret, run `quipu protect --apply`, then `quipu doctor`"
	return printValue(report, *asJSON)
}

// existingConfig loads the config when the repository was adopted already.
func existingConfig(top string) (model.Config, bool, error) {
	if _, err := os.Stat(filepath.Join(top, configPath)); os.IsNotExist(err) {
		return model.Config{}, false, nil
	}
	c, err := loadConfigAt(top)
	if err != nil {
		return c, false, fmt.Errorf("%s: %w", configPath, err)
	}
	return c, true, nil
}

func newConfig(owner, repo string, info projectInfo) model.Config {
	return model.Config{
		SchemaVersion: model.ConfigVersion, Owner: owner, Repository: repo, WikiMode: "off", DefaultBranch: "main",
		Areas: []string{"stable"}, WriterVersion: version, AuthorizedActors: []string{owner},
		Policy:    model.Policy{LeaseTTLMinutes: 120, ReconcileMins: 5},
		Project:   model.Project{ID: info.ID, Number: info.Number, Title: info.Title, Fields: map[string]string{}},
		Workflows: model.Workflows{QuipuVersion: pinnable(version)},
	}
}

// setupOptions are the init and adopt flags that shape the config.
type setupOptions struct {
	wiki, branch, areas, runsOn, quipuVersion, schedule, checks *string
	writerOnPR                                                  *bool
}

func addSetupFlags(fs *flag.FlagSet) setupOptions {
	return setupOptions{
		wiki:         fs.String("wiki", "off", "off|journal"),
		branch:       fs.String("default-branch", "main", "default branch"),
		areas:        fs.String("areas", "stable", "comma-separated areas"),
		runsOn:       fs.String("runs-on", "", "comma-separated runner labels for every job, e.g. self-hosted,macOS"),
		quipuVersion: fs.String("quipu-version", "", "tag or commit SHA the workflows install (default: this binary's)"),
		schedule:     fs.String("schedule", "", "Writer cron schedule, or off (default every five minutes)"),
		checks:       fs.String("required-checks", "", "comma-separated CI checks main requires besides policy (default test)"),
		writerOnPR:   fs.Bool("writer-on-pr", true, "run the Writer on pull request events"),
	}
}

// apply copies the flags given on the command line into c. Flags left out
// keep what the config already says.
func (o setupOptions) apply(fs *flag.FlagSet, c *model.Config) error {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if given["wiki"] {
		c.WikiMode = *o.wiki
	}
	if given["default-branch"] {
		c.DefaultBranch = *o.branch
	}
	if given["areas"] {
		c.Areas = split(*o.areas)
	}
	if given["runs-on"] {
		if c.Workflows.RunsOn == nil {
			c.Workflows.RunsOn = map[string][]string{}
		}
		c.Workflows.RunsOn["default"] = split(*o.runsOn)
	}
	if given["quipu-version"] {
		c.Workflows.QuipuVersion = *o.quipuVersion
	}
	if given["schedule"] {
		c.Workflows.Schedule = *o.schedule
	}
	if given["required-checks"] {
		c.RequiredChecks = split(*o.checks)
	}
	if given["writer-on-pr"] {
		on := *o.writerOnPR
		c.Workflows.WriterOnPullRequest = &on
	}
	if c.Workflows.QuipuVersion == "" {
		return fmt.Errorf("this quipu (%s) is a development build; pass --quipu-version with a tag like v0.2.0 or a pushed commit SHA", version)
	}
	return c.Validate()
}

// provision makes GitHub and the working tree match c. It returns what it
// changed and the existing files it kept.
func provision(top string, c model.Config, replace bool) ([]string, []string, error) {
	var changes []string
	note := func(more []string, err error) error {
		changes = append(changes, more...)
		return err
	}
	if err := note(ensureRepoFeatures(c.Owner, c.Repository, c.WikiMode == "journal")); err != nil {
		return changes, nil, err
	}
	if err := note(ensureLabels(c.Owner, c.Repository, c.Areas)); err != nil {
		return changes, nil, err
	}
	info, err := readProject(c.Owner, c.Project.Number)
	if err != nil {
		return changes, nil, err
	}
	ids, fieldChanges, err := ensureProjectFields(c.Owner, info, c.Areas)
	if err := note(fieldChanges, err); err != nil {
		return changes, nil, err
	}
	c.Project.ID, c.Project.Title, c.Project.Fields = info.ID, info.Title, ids
	if err := note(ensureProjectViews(c.Owner, info)); err != nil {
		return changes, nil, err
	}
	if err := note(writeConfig(top, c)); err != nil {
		return changes, nil, err
	}
	files, err := bootstrap.Install(top, workflowOptions(c), replace)
	if err := note(files.Written, err); err != nil {
		return changes, files.Kept, err
	}
	if err := note(bootstrap.InstallHooks(top, true)); err != nil {
		return changes, files.Kept, err
	}
	return changes, files.Kept, note(syncGuides(top, true))
}

func workflowOptions(c model.Config) bootstrap.Options {
	runners := map[string][]string{}
	for _, job := range model.WorkflowJobs {
		runners[job] = c.Workflows.Runner(job)
	}
	pin := c.Workflows.QuipuVersion
	if pin == "" {
		pin = "main"
	}
	return bootstrap.Options{
		Owner: c.Owner, ProjectNumber: c.Project.Number, DefaultBranch: c.DefaultBranch, RunsOn: runners,
		QuipuVersion: pin, Schedule: c.Workflows.Cron(), WriterOnPullRequest: c.Workflows.PullRequestTrigger(),
		DefectsFrom: c.Workflows.DefectWorkflows(),
	}
}

func writeConfig(top string, c model.Config) ([]string, error) {
	raw, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(top, configPath)
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, raw) {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return []string{configPath}, os.WriteFile(path, raw, 0o644)
}

// realIssueCount counts the repository's Issues. The REST Issues list also
// returns pull requests, which an adoptable repository may have.
func realIssueCount(owner, repo string) (int, error) {
	output, err := ghOutput("api", "--paginate", "repos/"+owner+"/"+repo+"/issues?state=all&per_page=100")
	if err != nil {
		return 0, fmt.Errorf("audit repository: %w", err)
	}
	return countIssues(output)
}

// countIssues reads paginated REST pages, one JSON array after another.
func countIssues(raw []byte) (int, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	count := 0
	for {
		var page []struct {
			PullRequest json.RawMessage `json:"pull_request"`
		}
		if err := decoder.Decode(&page); err == io.EOF {
			return count, nil
		} else if err != nil {
			return 0, err
		}
		for _, item := range page {
			if len(item.PullRequest) == 0 || string(item.PullRequest) == "null" {
				count++
			}
		}
	}
}

func ensureRepoFeatures(owner, repo string, wiki bool) ([]string, error) {
	output, err := ghOutput("repo", "view", owner+"/"+repo, "--json", "hasIssuesEnabled,hasWikiEnabled")
	if err != nil {
		return nil, err
	}
	var features struct{ HasIssuesEnabled, HasWikiEnabled bool }
	if err := json.Unmarshal(output, &features); err != nil {
		return nil, err
	}
	var args, changes []string
	if !features.HasIssuesEnabled {
		args, changes = append(args, "--enable-issues"), append(changes, "Issues enabled")
	}
	if wiki && !features.HasWikiEnabled {
		args, changes = append(args, "--enable-wiki"), append(changes, "Wiki enabled")
	}
	if len(args) == 0 {
		return nil, nil
	}
	if _, err := ghOutput(append([]string{"repo", "edit", owner + "/" + repo}, args...)...); err != nil {
		return nil, fmt.Errorf("enable repository features: %w", err)
	}
	return changes, nil
}

type quipuLabel struct{ name, color, description string }

func quipuLabels(areas []string) []quipuLabel {
	var labels []quipuLabel
	for _, kind := range []string{"epic", "contract", "story", "task", "defect", "gate", "decision"} {
		labels = append(labels, quipuLabel{"kind:" + kind, "1D76DB", ""})
	}
	for _, area := range areas {
		labels = append(labels, quipuLabel{"area:" + area, "1D76DB", ""})
	}
	return append(labels,
		quipuLabel{triageLabel, "D93F0B", "Created outside the Quipu flow; the owner triages it"},
		quipuLabel{ownerApprovedLabel, "0E8A16", "The owner approves a PR that changes Quipu's own files"},
	)
}

func ensureLabels(owner, repo string, areas []string) ([]string, error) {
	output, err := ghOutput("label", "list", "--repo", owner+"/"+repo, "--limit", "1000", "--json", "name")
	if err != nil {
		return nil, err
	}
	var existing []struct{ Name string }
	if err := json.Unmarshal(output, &existing); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, label := range existing {
		have[strings.ToLower(label.Name)] = true
	}
	var changes []string
	for _, label := range quipuLabels(areas) {
		if have[label.name] {
			continue
		}
		args := []string{"label", "create", label.name, "--color", label.color, "--repo", owner + "/" + repo}
		if label.description != "" {
			args = append(args, "--description", label.description)
		}
		if _, err := ghOutput(args...); err != nil {
			return changes, fmt.Errorf("create label %s: %w", label.name, err)
		}
		changes = append(changes, "label "+label.name)
	}
	return changes, nil
}

type projectInfo struct {
	ID        string
	Number    int
	Title     string
	Items     int
	OwnerType string // User or Organization
}

func readProject(owner string, number int) (projectInfo, error) {
	output, err := ghOutput("project", "view", fmt.Sprint(number), "--owner", owner, "--format", "json")
	if err != nil {
		return projectInfo{}, fmt.Errorf("read Project #%d: %w", number, err)
	}
	return parseProject(output, number)
}

func parseProject(raw []byte, number int) (projectInfo, error) {
	var project struct {
		ID     string `json:"id"`
		Number int    `json:"number"`
		Title  string `json:"title"`
		Items  struct {
			TotalCount int `json:"totalCount"`
		} `json:"items"`
		Owner struct {
			Type string `json:"type"`
		} `json:"owner"`
	}
	if err := json.Unmarshal(raw, &project); err != nil {
		return projectInfo{}, err
	}
	if project.ID == "" || project.Number != number || project.Title == "" {
		return projectInfo{}, fmt.Errorf("Project #%d returned incomplete identity", number)
	}
	return projectInfo{ID: project.ID, Number: project.Number, Title: project.Title, Items: project.Items.TotalCount, OwnerType: project.Owner.Type}, nil
}

type selectOption struct{ name, color string }

var statusOptions = []selectOption{
	{"Backlog", "GRAY"}, {"Ready", "BLUE"}, {"Claimed", "YELLOW"}, {"In progress", "ORANGE"}, {"In review", "PURPLE"},
	{"Evidence pending", "PINK"}, {"Blocked", "RED"}, {"Done", "GREEN"}, {"Cancelled", "GRAY"}, {"Archived", "GRAY"},
}

type fieldSpec struct {
	name, kind string
	options    []string
}

func projectFieldSpecs(areas []string) []fieldSpec {
	areaOptions := make([]string, 0, len(areas))
	for _, area := range areas {
		areaOptions = append(areaOptions, strings.ToUpper(area[:1])+area[1:])
	}
	return []fieldSpec{
		{"Kind", "SINGLE_SELECT", []string{"Epic", "Contract", "Story", "Task", "Defect", "Gate", "Decision"}},
		{"Area", "SINGLE_SELECT", areaOptions},
		{"Priority", "SINGLE_SELECT", []string{"P0", "P1", "P2", "P3"}},
		{"Proof", "SINGLE_SELECT", []string{"Not started", "Local verified", "CI verified", "Target environment pending", "External/provider pending", "Complete"}},
		{"Lease holder", "TEXT", nil}, {"Lease expires", "TEXT", nil}, {"Branch", "TEXT", nil}, {"State fingerprint", "TEXT", nil},
	}
}

// optionPlan is what a single-select field needs: the options it lacks and,
// when it lacks any, the full list to set.
type optionPlan struct {
	field   string
	id      string
	missing []string
	set     []selectOption
}

// planOptions compares the Project's fields with Quipu's. Status gets
// exactly Quipu's options; other fields keep theirs and gain the missing.
func planOptions(fields map[string]projectField, specs []fieldSpec) ([]optionPlan, error) {
	var plans []optionPlan
	status, ok := fields["Status"]
	if !ok {
		return nil, errors.New("Project default Status field not found")
	}
	var missing []string
	for _, option := range statusOptions {
		if _, ok := status.Options[option.name]; !ok {
			missing = append(missing, option.name)
		}
	}
	if len(missing) > 0 {
		plans = append(plans, optionPlan{"Status", status.ID, missing, statusOptions})
	}
	for _, spec := range specs {
		field, ok := fields[spec.name]
		if !ok || spec.kind != "SINGLE_SELECT" {
			continue
		}
		if field.Type != "" && field.Type != "ProjectV2SingleSelectField" {
			return nil, fmt.Errorf("Project field %q exists but is not a single select; rename it so Quipu can create its own", spec.name)
		}
		plan := optionPlan{field: spec.name, id: field.ID}
		existing := make([]string, 0, len(field.Options))
		for name := range field.Options {
			existing = append(existing, name)
		}
		sort.Strings(existing)
		for _, name := range existing {
			plan.set = append(plan.set, selectOption{name, "GRAY"})
		}
		for _, name := range spec.options {
			if _, ok := field.Options[name]; !ok {
				plan.missing = append(plan.missing, name)
				plan.set = append(plan.set, selectOption{name, "GRAY"})
			}
		}
		if len(plan.missing) > 0 {
			plans = append(plans, plan)
		}
	}
	return plans, nil
}

func ensureProjectFields(owner string, info projectInfo, areas []string) (map[string]string, []string, error) {
	fields, err := readProjectFields(owner, info.Number)
	if err != nil {
		return nil, nil, err
	}
	specs := projectFieldSpecs(areas)
	var changes []string
	for _, spec := range specs {
		if _, ok := fields[spec.name]; ok {
			continue
		}
		args := []string{"project", "field-create", fmt.Sprint(info.Number), "--owner", owner, "--name", spec.name, "--data-type", spec.kind}
		if len(spec.options) > 0 {
			args = append(args, "--single-select-options", strings.Join(spec.options, ","))
		}
		if _, err := ghOutput(args...); err != nil {
			return nil, changes, fmt.Errorf("create Project field %s: %w", spec.name, err)
		}
		changes = append(changes, "Project field "+spec.name)
	}
	if len(changes) > 0 {
		if fields, err = readProjectFields(owner, info.Number); err != nil {
			return nil, changes, err
		}
	}
	plans, err := planOptions(fields, specs)
	if err != nil {
		return nil, changes, err
	}
	for _, plan := range plans {
		// Setting options replaces them all, which clears the field on every
		// item. Only an empty Project is safe to change that way.
		if info.Items > 0 {
			return nil, changes, fmt.Errorf("Project #%d field %s lacks options %s; add them in the Project's field settings (Quipu does not rewrite options of a Project with items), then rerun", info.Number, plan.field, strings.Join(plan.missing, ", "))
		}
		if err := setSelectOptions(plan.id, plan.set); err != nil {
			return nil, changes, fmt.Errorf("configure Project %s options: %w", plan.field, err)
		}
		changes = append(changes, "Project "+plan.field+" options "+strings.Join(plan.missing, ", "))
	}
	if len(plans) > 0 {
		if fields, err = readProjectFields(owner, info.Number); err != nil {
			return nil, changes, err
		}
	}
	ids, err := configuredFieldIDsFromMap(info.Number, fields)
	return ids, changes, err
}

func setSelectOptions(fieldID string, options []selectOption) error {
	parts := make([]string, 0, len(options))
	for _, option := range options {
		parts = append(parts, fmt.Sprintf(`{name:%q,color:%s,description:""}`, option.name, option.color))
	}
	mutation := fmt.Sprintf("mutation { updateProjectV2Field(input:{fieldId:%q,singleSelectOptions:[%s]}) { projectV2Field { ... on ProjectV2SingleSelectField { id } } } }", fieldID, strings.Join(parts, ","))
	_, err := ghOutput("api", "graphql", "-f", "query="+mutation)
	return err
}

func projectViewNames(owner string, info projectInfo) (map[string]bool, error) {
	root := "user"
	if info.OwnerType == "Organization" {
		root = "organization"
	}
	query := fmt.Sprintf(`query($owner:String!,$number:Int!){ %s(login:$owner){ projectV2(number:$number){ views(first:100){ nodes{ name } } } } }`, root)
	output, err := ghOutput("api", "graphql", "-f", "query="+query, "-f", "owner="+owner, "-F", fmt.Sprintf("number=%d", info.Number), "--jq", "."+"data."+root+".projectV2.views.nodes[].name")
	if err != nil {
		return nil, fmt.Errorf("list Project views: %w", err)
	}
	names := map[string]bool{}
	for _, name := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if name != "" {
			names[name] = true
		}
	}
	return names, nil
}

func ensureProjectViews(owner string, info projectInfo) ([]string, error) {
	have, err := projectViewNames(owner, info)
	if err != nil {
		return nil, err
	}
	endpoint, err := projectRESTEndpoint(owner, info.Number)
	if err != nil {
		return nil, err
	}
	output, err := ghOutput("api", endpoint+"/fields?per_page=100", "-H", "X-GitHub-Api-Version: 2026-03-10")
	if err != nil {
		return nil, fmt.Errorf("list Project fields for views: %w", err)
	}
	var fields []restProjectField
	if err := json.Unmarshal(output, &fields); err != nil {
		return nil, err
	}
	byName := make(map[string]int, len(fields))
	for _, field := range fields {
		byName[field.Name] = field.ID
	}
	views, err := standardProjectViews(byName)
	if err != nil {
		return nil, err
	}
	var changes []string
	for _, view := range views {
		if have[view.Name] {
			continue
		}
		raw, err := json.Marshal(view)
		if err != nil {
			return changes, err
		}
		cmd := exec.Command("gh", "api", "--method", "POST", endpoint+"/views", "-H", "X-GitHub-Api-Version: 2026-03-10", "--input", "-")
		cmd.Stdin = bytes.NewReader(raw)
		if output, err := cmd.CombinedOutput(); err != nil {
			return changes, fmt.Errorf("create Project view %q: %s", view.Name, strings.TrimSpace(string(output)))
		}
		changes = append(changes, "Project view "+view.Name)
	}
	return changes, nil
}
