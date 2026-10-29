package main

// Shell command analysis for the guard. Commands are parsed with a real bash
// parser, so quoting, spacing, pipelines, subshells, `$(...)`, redirections,
// `sh -c`, and `cd` cannot hide what runs. Every simple command is judged in
// the directory it runs in: read-only commands always pass, writes need the
// lease, and the never-allowed actions are denied outright.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	policycheck "github.com/isnakolah/quipu/internal/policy"
	"mvdan.cc/sh/v3/syntax"
)

// shellArg is one command word; ok is false when it is not a plain literal
// (it contains a variable, command substitution, or glob we cannot resolve).
type shellArg struct {
	s  string
	ok bool
}

func (g *guard) shell(command, dir string) error {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return g.unparsable(dir, err.Error())
	}
	return g.stmts(file.Stmts, &dir)
}

// unparsable denies a command the guard cannot understand, but only where
// Quipu has something to protect.
func (g *guard) unparsable(dir, detail string) error {
	if w, err := openWorktree(existingDir(dir)); err == nil {
		if _, err := os.Stat(filepath.Join(w.Top, configPath)); err == nil {
			return deny("unparsable", detail)
		}
	}
	return nil
}

func (g *guard) stmts(list []*syntax.Stmt, dir *string) error {
	for _, stmt := range list {
		if err := g.stmt(stmt, dir); err != nil {
			return err
		}
	}
	return nil
}

func (g *guard) stmt(stmt *syntax.Stmt, dir *string) error {
	for _, redirect := range stmt.Redirs {
		if err := g.nested(redirect.Word, *dir); err != nil {
			return err
		}
		switch redirect.Op {
		case syntax.RdrOut, syntax.AppOut, syntax.RdrInOut, syntax.ClbOut, syntax.RdrAll, syntax.AppAll:
			target, ok := wordLit(redirect.Word)
			if !ok {
				return g.unparsable(*dir, "redirect to a computed path")
			}
			if target == "/dev/null" || target == "/dev/stdout" || target == "/dev/stderr" {
				continue
			}
			if err := g.writePath(g.resolve(*dir, target)); err != nil {
				return err
			}
		}
	}
	if stmt.Cmd == nil {
		return nil
	}
	return g.command(stmt.Cmd, dir)
}

func (g *guard) command(cmd syntax.Command, dir *string) error {
	local := *dir
	switch x := cmd.(type) {
	case *syntax.CallExpr:
		for _, assign := range x.Assigns {
			if assign.Value != nil {
				if err := g.nested(assign.Value, *dir); err != nil {
					return err
				}
			}
		}
		if len(x.Args) == 0 {
			return nil
		}
		args := make([]shellArg, len(x.Args))
		for i, word := range x.Args {
			if err := g.nested(word, *dir); err != nil {
				return err
			}
			args[i].s, args[i].ok = wordLit(word)
		}
		return g.call(args, dir)
	case *syntax.BinaryCmd:
		if x.Op == syntax.Pipe || x.Op == syntax.PipeAll {
			if err := g.stmt(x.X, &local); err != nil {
				return err
			}
			right := *dir
			return g.stmt(x.Y, &right)
		}
		if err := g.stmt(x.X, dir); err != nil {
			return err
		}
		return g.stmt(x.Y, dir)
	case *syntax.Subshell:
		return g.stmts(x.Stmts, &local)
	case *syntax.Block:
		return g.stmts(x.Stmts, dir)
	case *syntax.IfClause:
		for clause := x; clause != nil; clause = clause.Else {
			if err := g.stmts(clause.Cond, &local); err != nil {
				return err
			}
			if err := g.stmts(clause.Then, &local); err != nil {
				return err
			}
		}
		return nil
	case *syntax.WhileClause:
		if err := g.stmts(x.Cond, &local); err != nil {
			return err
		}
		return g.stmts(x.Do, &local)
	case *syntax.ForClause:
		return g.stmts(x.Do, &local)
	case *syntax.CaseClause:
		for _, item := range x.Items {
			if err := g.stmts(item.Stmts, &local); err != nil {
				return err
			}
		}
		return nil
	case *syntax.FuncDecl:
		return g.stmt(x.Body, &local)
	case *syntax.TimeClause:
		if x.Stmt != nil {
			return g.stmt(x.Stmt, dir)
		}
		return nil
	case *syntax.CoprocClause:
		return g.stmt(x.Stmt, &local)
	case *syntax.DeclClause, *syntax.ArithmCmd, *syntax.TestClause, *syntax.LetClause:
		return g.nestedNode(x, *dir)
	default:
		return g.unparsable(*dir, "unsupported shell construct")
	}
}

// nested judges commands hidden in `$(...)` and `<(...)` inside a word.
func (g *guard) nested(word *syntax.Word, dir string) error {
	if word == nil {
		return nil
	}
	return g.nestedNode(word, dir)
}

func (g *guard) nestedNode(node syntax.Node, dir string) error {
	var err error
	syntax.Walk(node, func(n syntax.Node) bool {
		if err != nil {
			return false
		}
		switch x := n.(type) {
		case *syntax.CmdSubst:
			local := dir
			err = g.stmts(x.Stmts, &local)
			return false
		case *syntax.ProcSubst:
			local := dir
			err = g.stmts(x.Stmts, &local)
			return false
		}
		return true
	})
	return err
}

// wordLit returns a word's value when it is a plain literal, joining quoted parts.
func wordLit(word *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(strings.ReplaceAll(p.Value, `\`, ""))
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// ---- command classification ----

// readOnlyCommands never change files or GitHub state, whatever their arguments.
var readOnlyCommands = setOf("ls", "cat", "head", "tail", "wc", "grep", "egrep", "fgrep", "rg", "ag", "ack", "jq",
	"diff", "cmp", "comm", "cut", "tr", "stat", "file", "tree", "pwd", "echo", "printf", "which", "type", "whereis",
	"date", "test", "[", "true", "false", "basename", "dirname", "realpath", "readlink", "du", "df", "printenv",
	"id", "whoami", "hostname", "uname", "nl", "column", "fold", "od", "hexdump", "md5", "md5sum", "shasum",
	"sha1sum", "sha256sum", "seq", "sleep", "less", "more", "man", "exit", "return", "set", "unset", "shopt",
	"read", "wait", "jobs", "trap", "ps", "lsof", "tput", "colordiff", "bat", "fd", "uniq", ":")

// fileArgCommands write to their path arguments, so each path is judged.
var fileArgCommands = setOf("rm", "rmdir", "mv", "cp", "touch", "mkdir", "chmod", "chown", "ln", "tee",
	"truncate", "install", "unlink", "rsync", "patch")

func (g *guard) call(args []shellArg, dir *string) error {
	if !args[0].ok {
		return g.unparsable(*dir, "command name is not a literal")
	}
	name, rest := filepath.Base(args[0].s), args[1:]
	switch name {
	case "cd", "pushd":
		if len(rest) == 0 {
			if home, err := os.UserHomeDir(); err == nil {
				*dir = home
			}
			return nil
		}
		if !rest[0].ok {
			return g.unparsable(*dir, "cd to a computed path")
		}
		if rest[0].s != "-" {
			*dir = g.resolve(*dir, rest[0].s)
		}
		return nil
	case "popd":
		*dir = g.cwd
		return nil
	case "env":
		for len(rest) > 0 && rest[0].ok && (strings.Contains(rest[0].s, "=") || strings.HasPrefix(rest[0].s, "-")) {
			if rest[0].s == "-u" || rest[0].s == "-C" || rest[0].s == "-S" {
				rest = rest[1:]
			}
			if len(rest) > 0 {
				rest = rest[1:]
			}
		}
		if len(rest) == 0 {
			return nil
		}
		return g.call(rest, dir)
	case "command", "builtin", "exec", "nohup", "time", "nice", "timeout", "xargs", "caffeinate":
		if name == "command" && len(rest) > 0 && (rest[0].s == "-v" || rest[0].s == "-V") {
			return nil
		}
		rest = skipWrapperFlags(name, rest)
		if len(rest) == 0 {
			return nil
		}
		return g.call(rest, dir)
	case "sh", "bash", "zsh", "dash", "ksh":
		for i, arg := range rest {
			if arg.ok && strings.HasPrefix(arg.s, "-") && !strings.HasPrefix(arg.s, "--") && strings.Contains(arg.s, "c") {
				if i+1 >= len(rest) || !rest[i+1].ok {
					return g.unparsable(*dir, name+" -c with a computed script")
				}
				return g.shell(rest[i+1].s, *dir)
			}
		}
		return g.writeIn(*dir)
	case "eval":
		parts := make([]string, len(rest))
		for i, arg := range rest {
			if !arg.ok {
				return g.unparsable(*dir, "eval of a computed string")
			}
			parts[i] = arg.s
		}
		return g.shell(strings.Join(parts, " "), *dir)
	case "git":
		return g.git(rest, *dir)
	case "gh":
		return g.gh(rest, *dir)
	case "quipu":
		return g.quipu(rest)
	case "go":
		return g.goCommand(rest, *dir)
	case "gofmt":
		if hasFlag(rest, "-w") {
			return g.writeArgs(rest, *dir)
		}
		return nil
	case "sed":
		for _, arg := range rest {
			if arg.ok && (strings.HasPrefix(arg.s, "-i") || strings.HasPrefix(arg.s, "--in-place") ||
				(strings.HasPrefix(arg.s, "-") && !strings.HasPrefix(arg.s, "--") && strings.Contains(arg.s, "i"))) {
				return g.writeArgs(rest, *dir)
			}
		}
		return nil
	case "find":
		for _, arg := range rest {
			switch arg.s {
			case "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls":
				return g.writeIn(*dir)
			}
		}
		return nil
	case "sort":
		if hasFlag(rest, "-o") || hasPrefixFlag(rest, "--output") {
			return g.writeIn(*dir)
		}
		return nil
	case "curl":
		return g.curl(rest, *dir)
	}
	if readOnlyCommands[name] {
		return nil
	}
	if fileArgCommands[name] {
		return g.writeArgs(rest, *dir)
	}
	return g.genericWrite(rest, *dir)
}

func skipWrapperFlags(name string, rest []shellArg) []shellArg {
	valued := map[string]bool{"-n": name == "nice" || name == "xargs", "-I": name == "xargs", "-L": name == "xargs",
		"-P": name == "xargs", "-d": name == "xargs", "-s": name == "xargs" || name == "timeout", "-k": name == "timeout"}
	for len(rest) > 0 && rest[0].ok && strings.HasPrefix(rest[0].s, "-") {
		if valued[rest[0].s] && len(rest) > 1 {
			rest = rest[1:]
		}
		rest = rest[1:]
	}
	if name == "timeout" && len(rest) > 0 {
		rest = rest[1:] // the duration
	}
	return rest
}

func hasFlag(args []shellArg, flags ...string) bool {
	for _, arg := range args {
		for _, flag := range flags {
			if arg.s == flag {
				return true
			}
		}
	}
	return false
}

func hasPrefixFlag(args []shellArg, prefix string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg.s, prefix) {
			return true
		}
	}
	return false
}

// mentionsQuipuState reports whether a literal argument names git internals
// or Quipu's local state, which no command may write.
func mentionsQuipuState(arg string) bool {
	return insideGitDir(arg) || strings.Contains(arg, "quipu-lease") || strings.Contains(arg, "quipu-agent-id")
}

// genericWrite judges a command that may write anything: it runs where it
// is, and must not name git internals.
func (g *guard) genericWrite(args []shellArg, dir string) error {
	for _, arg := range args {
		if arg.ok && mentionsQuipuState(arg.s) {
			return deny("git-internals", arg.s)
		}
		if arg.ok && strings.Contains(arg.s, configPath) {
			return deny("quipu-config", "")
		}
	}
	return g.writeIn(dir)
}

// writeArgs judges a command that writes its path arguments.
func (g *guard) writeArgs(args []shellArg, dir string) error {
	paths := 0
	for _, arg := range args {
		if !arg.ok {
			return g.writeIn(dir)
		}
		if strings.HasPrefix(arg.s, "-") || arg.s == "" {
			continue
		}
		paths++
		if err := g.writePath(g.resolve(dir, arg.s)); err != nil {
			return err
		}
	}
	if paths == 0 {
		return g.writeIn(dir)
	}
	return nil
}

// ---- git ----

var gitReadOnly = setOf("status", "log", "diff", "show", "blame", "rev-parse", "ls-files", "ls-tree", "ls-remote",
	"cat-file", "shortlog", "describe", "grep", "reflog", "help", "version", "merge-base", "rev-list", "name-rev",
	"show-ref", "for-each-ref", "check-ignore", "var", "whatchanged", "range-diff", "count-objects", "fsck", "cherry")

var gitRefWrites = setOf("update-ref", "symbolic-ref", "replace", "filter-branch", "filter-repo", "notes", "remote",
	"config", "tag", "branch")

func (g *guard) git(args []shellArg, dir string) error {
	i := 0
	for i < len(args) {
		arg := args[i]
		if !arg.ok {
			return g.unparsable(dir, "git option is not a literal")
		}
		switch {
		case arg.s == "-C" && i+1 < len(args):
			if !args[i+1].ok {
				return g.unparsable(dir, "git -C a computed path")
			}
			dir = g.resolve(dir, args[i+1].s)
			i += 2
		case arg.s == "-c" && i+1 < len(args):
			i += 2
		case strings.HasPrefix(arg.s, "--git-dir") || strings.HasPrefix(arg.s, "--work-tree"):
			return deny("git-internals", arg.s)
		case strings.HasPrefix(arg.s, "-"):
			i++
		default:
			goto subcommand
		}
	}
	return nil
subcommand:
	sub, rest := args[i].s, args[i+1:]
	for _, arg := range rest {
		if arg.s == "--no-verify" || (sub == "commit" && isShortFlag(arg.s, 'n')) {
			return deny("no-verify", "git "+sub)
		}
	}
	switch {
	case gitReadOnly[sub]:
		return nil
	case sub == "fetch":
		for _, arg := range rest {
			if strings.Contains(arg.s, ":") {
				return deny("git-refs", "fetch into a local ref")
			}
		}
		return nil
	case sub == "stash" && len(rest) > 0 && (rest[0].s == "list" || rest[0].s == "show"):
		return nil
	case sub == "branch" && gitListing(rest, "-a", "-r", "-v", "-vv", "--list", "-l", "--show-current", "--all",
		"--remotes", "--verbose", "--contains", "--no-contains", "--merged", "--no-merged", "--format", "--sort", "--color", "--no-color", "--column"):
		return nil
	case sub == "tag" && (len(rest) == 0 || hasFlag(rest, "-l", "--list")):
		return nil
	case sub == "remote" && (len(rest) == 0 || rest[0].s == "-v" || rest[0].s == "show" || rest[0].s == "get-url"):
		return nil
	case sub == "config" && hasAnyFlag(rest, "--get", "--get-all", "--get-regexp", "--list", "-l", "--show-origin", "--show-scope"):
		return nil
	case sub == "branch":
		return deny("branch-change", "git branch "+joinArgs(rest))
	case gitRefWrites[sub]:
		return deny("git-refs", "git "+sub)
	case sub == "switch":
		return deny("branch-change", "git switch")
	case sub == "checkout":
		if hasFlag(rest, "--", "-p", "--patch") {
			return g.writeIn(dir)
		}
		return deny("branch-change", "git checkout "+joinArgs(rest))
	case sub == "worktree":
		return g.gitWorktree(rest, dir)
	case sub == "push":
		return g.gitPush(rest, dir)
	default:
		return g.writeIn(dir)
	}
}

func isShortFlag(arg string, flag byte) bool {
	return len(arg) > 1 && arg[0] == '-' && arg[1] != '-' && strings.IndexByte(arg[1:], flag) >= 0
}

// gitListing reports whether git branch arguments only list branches.
func gitListing(args []shellArg, allowed ...string) bool {
	set := setOf(allowed...)
	for _, arg := range args {
		name, _, _ := strings.Cut(arg.s, "=")
		if !set[name] {
			return false
		}
	}
	return true
}

func hasAnyFlag(args []shellArg, flags ...string) bool {
	for _, arg := range args {
		name, _, _ := strings.Cut(arg.s, "=")
		for _, flag := range flags {
			if name == flag {
				return true
			}
		}
	}
	return false
}

func joinArgs(args []shellArg) string {
	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = arg.s
	}
	return strings.Join(parts, " ")
}

func (g *guard) gitWorktree(rest []shellArg, dir string) error {
	if len(rest) == 0 || rest[0].s == "list" || rest[0].s == "prune" {
		return nil
	}
	switch rest[0].s {
	case "add":
		return deny("worktree-add", "")
	case "remove":
		for _, arg := range rest[1:] {
			if !arg.ok {
				return g.unparsable(dir, "worktree path is computed")
			}
			if strings.HasPrefix(arg.s, "-") {
				continue
			}
			target, err := openWorktree(g.resolve(dir, arg.s))
			if err == nil {
				if _, err := target.readLease(); err == nil {
					return deny("worktree-busy", target.Top)
				}
			}
		}
		return nil
	default:
		return deny("git-refs", "git worktree "+rest[0].s)
	}
}

// gitPush allows pushing only the leased branch to origin.
func (g *guard) gitPush(rest []shellArg, dir string) error {
	if err := g.writeIn(dir); err != nil {
		return err
	}
	lease, _, managed := g.leaseIn(dir)
	if !managed {
		return nil
	}
	var positional []string
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		if !arg.ok {
			return deny("push-target", "computed push argument")
		}
		switch {
		case arg.s == "--force-with-lease" || strings.HasPrefix(arg.s, "--force-with-lease=") || arg.s == "--force-if-includes":
		case arg.s == "--force" || (isShortFlag(arg.s, 'f')):
			return deny("force-push", "")
		case arg.s == "-d" || arg.s == "--delete" || arg.s == "--all" || arg.s == "--mirror" || arg.s == "--tags" ||
			arg.s == "--follow-tags" || arg.s == "--prune" || strings.HasPrefix(arg.s, "--repo"):
			return deny("push-target", arg.s)
		case arg.s == "-o" || arg.s == "--push-option" || arg.s == "--receive-pack" || arg.s == "--exec":
			i++
		case strings.HasPrefix(arg.s, "-"):
		default:
			positional = append(positional, arg.s)
		}
	}
	if len(positional) > 0 && positional[0] != "origin" {
		return deny("push-target", "remote "+positional[0])
	}
	for _, refspec := range positional[min(1, len(positional)):] {
		if strings.HasPrefix(refspec, "+") {
			return deny("force-push", refspec)
		}
		src, dst, hasDst := strings.Cut(refspec, ":")
		if !hasDst {
			dst = src
		}
		branchOf := func(ref string) string { return strings.TrimPrefix(ref, "refs/heads/") }
		okSrc := src == "HEAD" || branchOf(src) == lease.Branch
		okDst := branchOf(dst) == lease.Branch || (!hasDst && src == "HEAD")
		if !okSrc || !okDst {
			return deny("push-target", "refspec "+refspec+"; leased branch is "+lease.Branch)
		}
	}
	return nil
}

// ---- gh ----

var ghReadActions = setOf("view", "list", "status", "checks", "diff", "search", "watch", "item-list", "field-list", "download")

func (g *guard) gh(args []shellArg, dir string) error {
	var words []shellArg
	for i := 0; i < len(args); i++ {
		if args[i].s == "-R" || args[i].s == "--repo" || args[i].s == "--hostname" {
			i++
			continue
		}
		if strings.HasPrefix(args[i].s, "--repo=") {
			continue
		}
		words = append(words, args[i])
	}
	if len(words) == 0 {
		return nil
	}
	group, rest := words[0].s, words[1:]
	action := ""
	if len(rest) > 0 {
		action = rest[0].s
	}
	switch group {
	case "help", "version", "browse", "search", "status", "completion":
		return nil
	case "auth":
		if action == "status" || action == "token" {
			return nil
		}
		return deny("github-write", "gh auth "+action)
	case "api":
		return g.ghAPI(rest)
	case "pr":
		switch action {
		case "view", "list", "status", "checks", "diff":
			return nil
		case "create", "edit":
			return g.ghPRBody(action, rest[1:], dir)
		case "comment":
			if err := g.writeIn(dir); err != nil {
				return err
			}
			for _, arg := range rest {
				if strings.Contains(arg.s, "quipu-request") || strings.Contains(arg.s, "quipu-receipt") {
					return deny("github-write", "Quipu markers in a comment")
				}
			}
			return nil
		case "ready", "update-branch":
			return g.writeIn(dir)
		case "checkout":
			return deny("branch-change", "gh pr checkout")
		case "merge", "review", "close", "reopen":
			return deny("merge-approve", "gh pr "+action)
		}
		return deny("github-write", "gh pr "+action)
	case "run", "workflow", "release", "label", "issue", "project", "repo", "secret", "variable", "ruleset", "cache", "gist":
		if ghReadActions[action] {
			return nil
		}
		return deny("github-write", "gh "+group+" "+action)
	}
	return deny("github-write", "gh "+group)
}

func (g *guard) ghAPI(rest []shellArg) error {
	method, fields, endpoint, query := "", false, "", ""
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		value := func() string {
			if i+1 < len(rest) {
				i++
				if !rest[i].ok {
					return "\x00"
				}
				return rest[i].s
			}
			return ""
		}
		switch {
		case arg.s == "-X" || arg.s == "--method":
			method = strings.ToUpper(value())
		case strings.HasPrefix(arg.s, "-X") && len(arg.s) > 2:
			method = strings.ToUpper(arg.s[2:])
		case strings.HasPrefix(arg.s, "--method="):
			method = strings.ToUpper(strings.TrimPrefix(arg.s, "--method="))
		case arg.s == "-f" || arg.s == "-F" || arg.s == "--field" || arg.s == "--raw-field":
			fields = true
			if v := value(); strings.HasPrefix(v, "query=") {
				query = strings.TrimPrefix(v, "query=")
			} else if v == "\x00" {
				query = "\x00"
			}
		case arg.s == "--input":
			value()
			fields = true
		case arg.s == "-H" || arg.s == "--header" || arg.s == "-q" || arg.s == "--jq" || arg.s == "-t" || arg.s == "--template" || arg.s == "--cache" || arg.s == "-p" || arg.s == "--preview":
			value()
		case strings.HasPrefix(arg.s, "-"):
		case endpoint == "":
			endpoint = arg.s
		}
	}
	if endpoint == "graphql" {
		if query == "\x00" || strings.HasPrefix(query, "@") || strings.Contains(strings.ToLower(query), "mutation") {
			return deny("github-write", "GraphQL mutation")
		}
		return nil
	}
	if method == "GET" || method == "HEAD" || (method == "" && !fields) {
		return nil
	}
	return deny("github-write", "gh api "+endpoint)
}

// ghPRBody allows creating or editing a PR only from the leased branch with
// a compliant body, so the policy check never has to reject it.
func (g *guard) ghPRBody(action string, rest []shellArg, dir string) error {
	if err := g.writeIn(dir); err != nil {
		return err
	}
	lease, w, managed := g.leaseIn(dir)
	if !managed {
		return nil
	}
	c, err := loadConfigAt(w.Top)
	if err != nil {
		return deny("pr-body", err.Error())
	}
	body, hasBody := "", false
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		next := func() (string, bool) {
			if i+1 >= len(rest) {
				return "", false
			}
			i++
			return rest[i].s, rest[i].ok
		}
		flag, inline, hasInline := strings.Cut(arg.s, "=")
		value := func() (string, bool) {
			if hasInline {
				return inline, true
			}
			return next()
		}
		switch flag {
		case "-b", "--body":
			v, ok := value()
			if !ok {
				return deny("pr-body", "body is computed")
			}
			body, hasBody = v, true
		case "-F", "--body-file":
			v, ok := value()
			if !ok || v == "-" {
				return deny("pr-body", "body file is computed or stdin")
			}
			raw, err := os.ReadFile(g.resolve(dir, v))
			if err != nil {
				return deny("pr-body", err.Error())
			}
			body, hasBody = string(raw), true
		case "-H", "--head":
			if v, ok := value(); !ok || v != lease.Branch {
				return deny("pr-body", "head must be "+lease.Branch)
			}
		case "-B", "--base":
			if v, ok := value(); !ok || v != c.DefaultBranch {
				return deny("pr-body", "base must be "+c.DefaultBranch)
			}
		case "-t", "--title":
			v, ok := value()
			if !ok {
				return deny("pr-body", "title is computed")
			}
			if err := policycheck.NoClosingKeywords(v, "PR title"); err != nil {
				return deny("pr-body", err.Error())
			}
		case "-l", "--label", "--add-label", "--remove-label":
			// Labels such as quipu:owner-approved are the owner's decision.
			return deny("github-write", "PR labels are set by the owner")
		case "-f", "--fill", "--fill-first", "--fill-verbose", "-w", "--web", "-e", "--editor":
			return deny("pr-body", flag+" leaves the body unchecked")
		}
	}
	if !hasBody {
		if action == "edit" {
			return nil
		}
		return deny("pr-body", "pass --body or --body-file")
	}
	issues, err := policycheck.ReferencedIssues(body)
	if err != nil {
		return deny("pr-body", err.Error())
	}
	if issues[0] != lease.Issue {
		return deny("pr-body", "body references #"+strconv.Itoa(issues[0])+" but the lease is #"+strconv.Itoa(lease.Issue))
	}
	return nil
}

// ---- quipu, go, curl ----

var quipuAgentCommands = setOf("status", "start", "done", "doctor", "context", "validate", "guide", "version",
	"help", "-h", "--help", "propose", "guard", "policy")

func (g *guard) quipu(rest []shellArg) error {
	if len(rest) == 0 || quipuAgentCommands[rest[0].s] {
		return nil
	}
	switch sub := rest[0].s; {
	case sub == "work" && len(rest) > 1 && rest[1].s == "list":
		return nil
	case (sub == "writer" || sub == "journal" || sub == "protect") && !hasFlag(rest, "--apply"):
		return nil
	default:
		return deny("quipu-admin", "quipu "+sub)
	}
}

var goReadOnly = setOf("test", "vet", "build", "list", "version", "doc", "help", "bug")

func (g *guard) goCommand(rest []shellArg, dir string) error {
	if len(rest) == 0 {
		return nil
	}
	sub := rest[0].s
	switch {
	case sub == "build" && (hasFlag(rest, "-o") || hasPrefixFlag(rest, "-o=")):
		return g.writeIn(dir)
	case goReadOnly[sub]:
		return nil
	case sub == "env" && !hasFlag(rest, "-w", "-u"):
		return nil
	case sub == "mod" && len(rest) > 1 && (rest[1].s == "graph" || rest[1].s == "why" || rest[1].s == "verify" || rest[1].s == "download"):
		return nil
	}
	return g.genericWrite(rest, dir)
}

// curl reads unless it sends data or writes a file; writes to GitHub's API
// must go through the Writer.
func (g *guard) curl(rest []shellArg, dir string) error {
	sends, writesFile, github := false, false, false
	for _, arg := range rest {
		switch {
		case arg.s == "-o" || arg.s == "-O" || arg.s == "--output" || arg.s == "--remote-name" || arg.s == "-J":
			writesFile = true
		case arg.s == "-d" || strings.HasPrefix(arg.s, "--data") || arg.s == "-F" || arg.s == "--form" || arg.s == "--json" ||
			arg.s == "-T" || arg.s == "--upload-file" || strings.HasPrefix(arg.s, "-X") || arg.s == "--request":
			sends = true
		case strings.Contains(arg.s, "github.com"):
			github = true
		}
	}
	if sends && github {
		return deny("github-write", "curl to GitHub")
	}
	if sends || writesFile {
		return g.genericWrite(rest, dir)
	}
	return nil
}
