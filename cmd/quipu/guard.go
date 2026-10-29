package main

// The agent guard. Plugin and project hooks pipe every agent tool call through
// `quipu guard`, which denies anything outside the Quipu flow. Reads are
// always allowed. Writes need an active lease in the agent's own worktree on
// its leased branch. Some actions are never allowed for agents, because they
// bypass the Writer, the review, or Quipu's own state. Each denial names the
// command to use instead, so an agent can recover without guessing.
//
// The guard is a guardrail for agents, not a sandbox: the Writer, the
// required policy check, and branch protection enforce the flow on GitHub.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/model"
)

// guardRule is one reason the guard denies. The agent guide renders this
// table, so the documentation always matches what the guard enforces.
type guardRule struct {
	Code, Why, Instead string
}

var guardRules = []guardRule{
	{"no-lease", "changes need an active lease in this worktree", "run `quipu start --next --json` (or `quipu start N --json`), then retry"},
	{"wrong-branch", "this worktree is not on its leased branch", "run `quipu start N --json` with the leased Issue; it switches to the branch"},
	{"other-worktree", "agents change only the worktree that holds their own lease", "work inside your own worktree; dispatch other work with `quipu start --next --worktree --json`"},
	{"lease-inactive", "the live lease is expired, held by someone else, or no longer active", "run `quipu start N --json` to renew or re-take it; `quipu status --json` shows the holder"},
	{"git-internals", "files under .git hold git and Quipu state (lease, identity)", "use git and quipu commands; never edit .git directly"},
	{"quipu-config", "Quipu's config, workflows, and agent hooks are owner-only", "ask the owner in a comment on your PR"},
	{"branch-change", "the work branch is chosen by quipu start", "run `quipu start N --json`; restore files with `git restore <file>` or `git checkout -- <file>`"},
	{"git-refs", "tags, remotes, ref edits, and git config are owner-only", "none needed for the flow; ask the owner if you think it is"},
	{"worktree-add", "work worktrees are created by Quipu so each gets its own identity and lease", "run `quipu start --next --worktree --json`"},
	{"worktree-busy", "that worktree still holds a lease", "finish it with `quipu done N`, then remove the worktree"},
	{"push-target", "agents push only their leased branch to origin", "run `git push -u origin <leased branch>`"},
	{"force-push", "force pushes rewrite shared history", "use `git push --force-with-lease origin <leased branch>`"},
	{"no-verify", "git hooks must run", "rerun without --no-verify (or -n)"},
	{"merge-approve", "agents never merge, approve, or close PRs; the owner reviews", "run `quipu done N --json` and wait for review"},
	{"github-write", "GitHub state changes go through the Quipu Writer", "use `quipu start`, `quipu done`, or `quipu propose`; read with gh view/list commands"},
	{"pr-body", "a PR must be created from the leased branch with a body saying exactly `Refs #N` for the leased Issue and no closing keywords", "run `gh pr create --base <default> --head <leased branch> --title ... --body \"Refs #N ...\"`"},
	{"quipu-admin", "Quipu recovery and admin commands are owner-only", "use `quipu start`, `quipu done`, or `quipu propose`; ask the owner for recovery"},
	{"quipu-missing", "quipu is not installed, so no call can be checked", "ask the owner to install quipu (docs/source-install.md in the Quipu repository)"},
	{"unparsable", "the guard could not tell what this command does", "rewrite it as plain commands without eval, variables as command names, or computed paths"},
}

func ruleFor(code string) guardRule {
	for _, rule := range guardRules {
		if rule.Code == code {
			return rule
		}
	}
	return guardRule{Code: code, Why: code, Instead: "run `quipu guide`"}
}

// denial is a guard refusal. Its text is what the agent sees on stderr.
type denial struct {
	guardRule
	Detail string
}

func (d denial) Error() string {
	text := "QUIPU DENIED [" + d.Code + "]: " + d.Why
	if d.Detail != "" {
		text += " (" + d.Detail + ")"
	}
	return text + ". Do instead: " + d.Instead + ". Rules: `quipu guide`."
}

func deny(code, detail string) error { return denial{ruleFor(code), detail} }

// hookInput is the PreToolUse payload from Claude Code or Codex.
type hookInput struct {
	ToolName  string          `json:"tool_name"`
	Cwd       string          `json:"cwd"`
	ToolInput json.RawMessage `json:"tool_input"`
}

type toolArgs struct {
	Command      json.RawMessage `json:"command"`
	FilePath     string          `json:"file_path"`
	NotebookPath string          `json:"notebook_path"`
	Path         string          `json:"path"`
}

func guardCommand(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: quipu guard < hook-input.json")
	}
	var input hookInput
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		return fmt.Errorf("read hook input: %w", err)
	}
	return newGuard(input.Cwd, time.Now().UTC()).check(input)
}

// guard judges one tool call. It caches per-worktree verdicts so a compound
// command checks each worktree once.
type guard struct {
	cwd     string
	now     time.Time
	session worktree
	// sessionLeased is set when the agent's own worktree holds a lease; such
	// an agent may then change no other worktree.
	sessionLeased bool
	verified      map[string]leaseContext
}

func newGuard(cwd string, now time.Time) *guard {
	if cwd == "" {
		cwd = "."
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	g := &guard{cwd: cwd, now: now, verified: map[string]leaseContext{}}
	if w, err := openWorktree(cwd); err == nil {
		g.session = w
		if _, err := os.Stat(filepath.Join(w.Top, configPath)); err == nil {
			_, err := w.readLease()
			g.sessionLeased = err == nil
		}
	}
	return g
}

var readOnlyTools = setOf("Read", "Grep", "Glob", "LS", "WebFetch", "WebSearch", "TodoWrite", "Task", "Agent",
	"NotebookRead", "ToolSearch", "AskUserQuestion", "ExitPlanMode", "EnterPlanMode", "Skill", "BashOutput",
	"KillShell", "TaskOutput", "TaskStop", "ListMcpResourcesTool", "ReadMcpResourceTool", "view_image", "update_plan")

var editTools = setOf("Edit", "Write", "MultiEdit", "NotebookEdit")

func (g *guard) check(input hookInput) error {
	var args toolArgs
	if len(input.ToolInput) > 0 {
		_ = json.Unmarshal(input.ToolInput, &args)
	}
	switch name := input.ToolName; {
	case readOnlyTools[name]:
		return nil
	case editTools[name]:
		for _, path := range []string{args.FilePath, args.NotebookPath, args.Path} {
			if path != "" {
				return g.writePath(g.resolve(g.cwd, path))
			}
		}
		return g.writeIn(g.cwd)
	case name == "apply_patch":
		paths := patchPaths(commandText(args.Command))
		if len(paths) == 0 {
			return g.writeIn(g.cwd)
		}
		for _, path := range paths {
			if err := g.writePath(g.resolve(g.cwd, path)); err != nil {
				return err
			}
		}
		return nil
	case name == "Bash" || name == "shell" || name == "local_shell" || name == "exec_command":
		return g.shell(commandText(args.Command), g.cwd)
	case strings.HasPrefix(name, "mcp__"):
		if githubMutation(name) {
			return deny("github-write", "MCP tool "+name)
		}
		return nil
	default:
		// Tools Quipu does not know write nothing Quipu tracks.
		return nil
	}
}

// commandText accepts a shell command as a string or an argv array.
func commandText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var argv []string
	if json.Unmarshal(raw, &argv) != nil {
		return ""
	}
	if len(argv) == 3 && (argv[1] == "-c" || argv[1] == "-lc") {
		return argv[2]
	}
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

var patchFile = regexp.MustCompile(`(?m)^\*\*\* (?:Add File|Update File|Delete File|Move to): (.+)$`)

func patchPaths(patch string) []string {
	var paths []string
	for _, match := range patchFile.FindAllStringSubmatch(patch, -1) {
		paths = append(paths, strings.TrimSpace(match[1]))
	}
	return paths
}

var githubMutationVerb = regexp.MustCompile(`(?i)(create|update|delete|merge|add|remove|push|close|edit|write|set|assign|request|submit|approve|fork|transfer|label|comment|lock|reopen|dismiss|rerun|cancel|run_workflow)`)

func githubMutation(tool string) bool {
	lower := strings.ToLower(tool)
	if !strings.Contains(lower, "github") && !strings.Contains(lower, "__git") {
		return false
	}
	parts := strings.Split(tool, "__")
	return githubMutationVerb.MatchString(parts[len(parts)-1])
}

// ---- paths and worktrees ----

func (g *guard) resolve(dir, path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path)
}

// realPath resolves symlinks in the part of path that exists, so it compares
// equal to the paths git reports (macOS /var is /private/var, for example).
func realPath(path string) string {
	existing := path
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return path
		}
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return path
	}
	rest, err := filepath.Rel(existing, path)
	if err != nil {
		return path
	}
	return filepath.Join(real, rest)
}

// existingDir is the nearest existing directory at or above path.
func existingDir(path string) string {
	dir := realPath(path)
	for {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

func insideGitDir(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".git" {
			return true
		}
	}
	return false
}

// writePath allows a change to path only within the rules for its worktree.
func (g *guard) writePath(path string) error {
	if insideGitDir(path) {
		return deny("git-internals", path)
	}
	path = realPath(path)
	dir := existingDir(path)
	if w, err := openWorktree(dir); err == nil {
		if rel, err := filepath.Rel(w.Top, path); err == nil && protectedFile(filepath.ToSlash(rel)) {
			return deny("quipu-config", filepath.ToSlash(rel))
		}
	}
	return g.writeIn(dir)
}

// writeIn allows a change in dir when dir is outside any Quipu-managed
// repository, or is the agent's own worktree holding an active lease on its
// leased branch.
func (g *guard) writeIn(dir string) error {
	if insideGitDir(dir) {
		return deny("git-internals", dir)
	}
	w, err := openWorktree(existingDir(dir))
	if err != nil {
		return nil
	}
	if _, ok := g.verified[w.Top]; ok {
		return nil
	}
	c, err := loadConfigAt(w.Top)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("QUIPU DENIED [config]: configuration unavailable in %s: %v", w.Top, err)
	}
	if g.sessionLeased && g.session.Top != w.Top {
		return deny("other-worktree", fmt.Sprintf("target %s, yours %s", w.Top, g.session.Top))
	}
	lease, err := w.readLease()
	if err != nil {
		return deny("no-lease", "worktree "+w.Top)
	}
	if branch := w.branch(); branch != lease.Branch {
		return deny("wrong-branch", fmt.Sprintf("on %q, lease #%d is for %q", branch, lease.Issue, lease.Branch))
	}
	if !lease.fresh(g.now) {
		if err := g.verifyLive(c, w, lease); err != nil {
			return err
		}
	}
	g.verified[w.Top] = lease
	return nil
}

// verifyLive checks the lease against GitHub and caches the result in the
// lease file so the next edits within leaseVerifyTTL stay off the API.
func (g *guard) verifyLive(c model.Config, w worktree, lease leaseContext) error {
	state, err := loadLiveWork(c, lease.Issue)
	if err != nil {
		return deny("lease-inactive", "could not verify #"+fmt.Sprint(lease.Issue)+": "+err.Error())
	}
	if state.Holder != lease.Holder || state.Branch != lease.Branch {
		return deny("lease-inactive", fmt.Sprintf("#%d is held by %q on %q", lease.Issue, state.Holder, state.Branch))
	}
	expires, err := time.Parse(time.RFC3339, state.Expiry)
	if err != nil || !expires.After(g.now) {
		return deny("lease-inactive", fmt.Sprintf("#%d lease expired", lease.Issue))
	}
	switch state.Status {
	case "Claimed", "In progress", "In review", "Evidence pending":
	default:
		return deny("lease-inactive", fmt.Sprintf("#%d is %s", lease.Issue, state.Status))
	}
	lease.Expires, lease.VerifiedAt = state.Expiry, g.now.Format(time.RFC3339)
	_ = w.writeLease(lease)
	return nil
}

// leaseIn returns the verified lease of dir's worktree after writeIn passed.
func (g *guard) leaseIn(dir string) (leaseContext, worktree, bool) {
	w, err := openWorktree(existingDir(dir))
	if err != nil {
		return leaseContext{}, worktree{}, false
	}
	lease, ok := g.verified[w.Top]
	return lease, w, ok
}

func setOf(items ...string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}
