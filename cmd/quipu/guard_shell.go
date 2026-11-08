package main

// Shell command analysis for the guard. Commands are parsed with a real bash
// parser, so quoting, spacing, pipelines, subshells, `$(...)`, redirections,
// `sh -c`, and `cd` cannot hide what runs. Every simple command is judged in
// the directory it runs in: read-only commands always pass, writes need the
// lease, and the never-allowed actions are denied outright.

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/isnakolah/quipu/internal/model"
	policycheck "github.com/isnakolah/quipu/internal/policy"
	"mvdan.cc/sh/v3/syntax"
)

// shellArg is one command word; ok is false when it is not a plain literal
// (it contains a variable, command substitution, or glob we cannot resolve).
type shellArg struct {
	s  string
	ok bool
	// prefix is the literal start of a computed word.
	prefix string
}

func (g *guard) shell(command, dir string) error {
	// The configured cleanup may be a compound line; in a finished worktree it
	// passes whole.
	if _, ok := g.finishing(dir); ok && cleanupLine(command, dir) {
		return nil
	}
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
		if hasConfig(w.Top) {
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
			target, ok, prefix := g.lit(redirect.Word)
			if !ok {
				if g.denyList(*dir) && g.outsideGoverned(prefix) {
					continue
				}
				return g.unparsable(*dir, "redirect to a computed path")
			}
			if target == "/dev/null" || target == "/dev/stdout" || target == "/dev/stderr" {
				continue
			}
			if err := g.writePath(g.resolve(*dir, target)); err != nil {
				return named(err, stmt)
			}
		}
	}
	if stmt.Cmd == nil {
		return nil
	}
	return g.command(stmt.Cmd, dir)
}

// named adds the statement to a denial that does not yet say what it refused.
func named(err error, stmt *syntax.Stmt) error {
	var d denial
	if !errors.As(err, &d) || d.Command != "" {
		return err
	}
	var b strings.Builder
	if syntax.NewPrinter(syntax.SingleLine(true)).Print(&b, stmt) == nil {
		d.Command = strings.TrimSpace(b.String())
	}
	return d
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
			g.assign(x.Assigns)
			return nil
		}
		args := make([]shellArg, len(x.Args))
		for i, word := range x.Args {
			if err := g.nested(word, *dir); err != nil {
				return err
			}
			args[i].s, args[i].ok, args[i].prefix = g.lit(word)
		}
		return g.call(args, dir)
	case *syntax.BinaryCmd:
		if x.Op == syntax.Pipe || x.Op == syntax.PipeAll {
			defer g.scope()()
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
		defer g.scope()()
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
	case *syntax.DeclClause:
		if err := g.nestedNode(x, *dir); err != nil {
			return err
		}
		g.assign(x.Args)
		return nil
	case *syntax.ArithmCmd, *syntax.TestClause, *syntax.LetClause:
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
			restore := g.scope()
			err = g.stmts(x.Stmts, &local)
			restore()
			return false
		case *syntax.ProcSubst:
			local := dir
			restore := g.scope()
			err = g.stmts(x.Stmts, &local)
			restore()
			return false
		}
		return true
	})
	return err
}

// wordLit returns a word's value when it is a plain literal, joining quoted parts.
func wordLit(word *syntax.Word) (string, bool) {
	s, ok, _ := (&guard{}).lit(word)
	return s, ok
}

// lit returns a word's value when it is a plain literal or uses only
// variables assigned a literal earlier in the same command. Otherwise it
// returns the literal start of the word as prefix.
func (g *guard) lit(word *syntax.Word) (string, bool, string) {
	var b strings.Builder
	param := func(p *syntax.ParamExp) bool {
		if p.Param == nil || p.Excl || p.Length || p.Width || p.Index != nil || p.Slice != nil || p.Repl != nil || p.Exp != nil || p.Names != 0 {
			return false
		}
		value, ok := g.vars[p.Param.Value]
		b.WriteString(value)
		return ok
	}
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(strings.ReplaceAll(p.Value, `\`, ""))
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				switch q := inner.(type) {
				case *syntax.Lit:
					b.WriteString(q.Value)
				case *syntax.ParamExp:
					if !param(q) {
						return "", false, b.String()
					}
				default:
					return "", false, b.String()
				}
			}
		case *syntax.ParamExp:
			if !param(p) {
				return "", false, b.String()
			}
		default:
			return "", false, b.String()
		}
	}
	return b.String(), true, ""
}

// assign records shell variables set to a literal, so later words that use
// them can be judged; any other assignment forgets the variable.
func (g *guard) assign(assigns []*syntax.Assign) {
	if g.vars == nil {
		g.vars = map[string]string{}
	}
	for _, a := range assigns {
		if a.Name == nil || a.Naked {
			continue
		}
		if a.Value == nil || a.Append || a.Index != nil || a.Array != nil {
			delete(g.vars, a.Name.Value)
			continue
		}
		if value, ok, _ := g.lit(a.Value); ok {
			g.vars[a.Name.Value] = value
		} else {
			delete(g.vars, a.Name.Value)
		}
	}
}

// scope saves the variables; the returned func restores them, for a subshell
// whose assignments never reach the rest of the line.
func (g *guard) scope() func() {
	saved := make(map[string]string, len(g.vars))
	for k, v := range g.vars {
		saved[k] = v
	}
	return func() { g.vars = saved }
}

// ---- command classification ----

// readOnlyCommands never change files or GitHub state, whatever their arguments.
var readOnlyCommands = setOf("ls", "cat", "head", "tail", "wc", "grep", "egrep", "fgrep", "rg", "ag", "ack", "jq",
	"diff", "cmp", "comm", "cut", "tr", "stat", "file", "tree", "pwd", "echo", "printf", "which", "type", "whereis",
	"date", "test", "[", "true", "false", "basename", "dirname", "realpath", "readlink", "du", "df", "printenv",
	"id", "whoami", "hostname", "uname", "nl", "column", "fold", "od", "hexdump", "md5", "md5sum", "shasum",
	"sha1sum", "sha256sum", "seq", "sleep", "less", "more", "man", "exit", "return", "set", "unset", "shopt",
	"read", "wait", "jobs", "trap", "ps", "lsof", "tput", "colordiff", "bat", "fd", "uniq", ":")

// openReadCommands are also read-only, and pass with guard_reads: open. Only
// commands that cannot write qualify: interpreters such as awk, python or perl
// can do anything, xxd and base64 can write a file, and macOS arch runs any
// program, so they stay out.
var openReadCommands = setOf("b2sum", "cal", "cksum", "expand", "getconf", "groups", "gzcat", "locale", "look",
	"nm", "nproc", "otool", "paste", "pgrep", "rev", "sha224sum", "sha384sum", "sha512sum", "strings", "sum", "sw_vers",
	"tac", "unexpand", "uptime", "who", "zcat")

// fileArgCommands write to their path arguments, so each path is judged.
var fileArgCommands = setOf("rm", "rmdir", "mv", "cp", "touch", "mkdir", "chmod", "chown", "ln", "tee",
	"truncate", "install", "unlink", "rsync", "patch")

func (g *guard) call(args []shellArg, dir *string) error {
	err := g.judge(args, dir)
	var d denial
	if !errors.As(err, &d) {
		return err
	}
	if d.Code == "no-lease" && g.tidyUp(args, *dir) {
		return nil
	}
	if d.Command == "" {
		d.Command = commandLine(args)
	}
	return d
}

// judge decides one simple command.
func (g *guard) judge(args []shellArg, dir *string) error {
	if err := g.declaredDeny(args, *dir); err != nil {
		return err
	}
	open := g.denyList(*dir)
	if open && helpForm(args) {
		return nil
	}
	if !args[0].ok {
		if open {
			return g.unknownCommand(args)
		}
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
		if open {
			return g.unknownCommand(rest)
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
		return g.quipu(rest, *dir)
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
		if commands, ok := findExec(rest); open && ok {
			for _, command := range commands {
				if err := g.call(command, dir); err != nil {
					return err
				}
			}
			return nil
		}
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
	if readOnlyCommands[name] || (openReadCommands[name] && g.readsOpen(*dir)) {
		return nil
	}
	if fileArgCommands[name] {
		return g.writeArgs(rest, *dir)
	}
	if open {
		return g.unknownCommand(rest)
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

// mentionsProtectedPath reports whether arg names one of Quipu's own
// enforcement files.
func mentionsProtectedPath(arg string) bool {
	for _, path := range protectedPaths {
		if strings.Contains(arg, strings.TrimSuffix(path, "/")) {
			return true
		}
	}
	return false
}

// genericWrite judges a command that may write anything: it runs where it
// is, and must not name git internals.
func (g *guard) genericWrite(args []shellArg, dir string) error {
	for _, arg := range args {
		if arg.ok && mentionsQuipuState(arg.s) {
			return deny("git-internals", arg.s)
		}
		if arg.ok && mentionsProtectedPath(arg.s) {
			return deny("quipu-config", arg.s)
		}
	}
	return g.writeIn(dir)
}

// writeArgs judges a command that writes its path arguments.
func (g *guard) writeArgs(args []shellArg, dir string) error {
	paths := 0
	for _, arg := range args {
		if !arg.ok {
			if g.denyList(dir) && g.outsideGoverned(arg.prefix) {
				paths++
				continue
			}
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
	case openGitRead(sub, rest) && g.readsOpen(dir):
		return nil
	case sub == "branch" && g.denyList(dir) && gitBranchQuery(rest):
		return nil
	case sub == "branch" && safeBranchDelete(rest, dir):
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
	case gitUnwinding(sub, rest):
		return g.gitUnwind(dir)
	case sub == "worktree":
		return g.gitWorktree(rest, dir)
	case sub == "push":
		return g.gitPush(rest, dir)
	default:
		return g.writeIn(dir)
	}
}

// gitUnwinding recognizes the commands that finish or back out of a rebase,
// merge, or cherry-pick already under way.
func gitUnwinding(sub string, rest []shellArg) bool {
	if len(rest) != 1 || !rest[0].ok {
		return false
	}
	switch flag := rest[0].s; sub {
	case "rebase":
		return flag == "--abort" || flag == "--continue" || flag == "--skip"
	case "merge":
		return flag == "--abort" || flag == "--continue"
	case "cherry-pick":
		return flag == "--abort"
	}
	return false
}

// gitUnwind lets an agent finish or back out of a git operation in a worktree
// whose recorded lease names the branch under way, or that holds no live lease.
// Otherwise a worktree stuck mid-rebase could be neither fixed nor switched.
func (g *guard) gitUnwind(dir string) error {
	err := g.writeIn(dir)
	if err == nil {
		return nil
	}
	w, openErr := openWorktree(existingDir(dir))
	if openErr != nil {
		return err
	}
	// writeIn clears a record that holds nothing, so one still here is live
	// or could not be checked.
	lease, leaseErr := w.readLease()
	if leaseErr != nil || lease.expired(g.now) {
		return nil
	}
	// A live lease is its holder's: another leased agent stays out.
	if lease.Branch == w.branch() && (!g.sessionLeased || g.session.Top == w.Top) {
		return nil
	}
	return err
}

// openGitRead recognizes the listing forms of git commands whose other forms
// write, which pass with guard_reads: open.
func openGitRead(sub string, rest []shellArg) bool {
	var positional []string
	var flags []shellArg
	for _, arg := range rest {
		if !arg.ok {
			return false
		}
		if strings.HasPrefix(arg.s, "-") {
			flags = append(flags, arg)
		} else {
			positional = append(positional, arg.s)
		}
	}
	switch sub {
	case "tag":
		return len(positional) == 0 && len(flags) > 0 && gitListing(flags, "--sort", "--format", "--contains", "--no-contains", "--merged", "--no-merged", "--points-at", "--color", "--no-color", "--column", "--no-column", "-i", "--ignore-case")
	case "branch":
		return hasFlag(flags, "--list") && gitListing(flags, "--list", "-a", "-r", "-v", "-vv", "--all", "--remotes", "--verbose", "--sort", "--format", "--color", "--no-color", "--contains", "--no-contains", "--merged", "--no-merged", "--ignore-case", "-i")
	case "symbolic-ref":
		return len(positional) == 1 && gitListing(flags, "--short", "-q", "--quiet")
	case "notes":
		return len(flags) == 0 && len(positional) > 0 && (positional[0] == "list" || positional[0] == "show")
	}
	return false
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

// gitBranchQuery reports, for deny-list mode, whether git branch arguments
// only list: a commit after --contains and friends, and patterns in list mode.
func gitBranchQuery(args []shellArg) bool {
	takesValue := setOf("--contains", "--no-contains", "--merged", "--no-merged", "--points-at", "--format", "--sort")
	flags := setOf("-a", "-r", "-v", "-vv", "--list", "-l", "--show-current", "--all", "--remotes", "--verbose",
		"--contains", "--no-contains", "--merged", "--no-merged", "--points-at", "--format", "--sort", "--color", "--no-color", "--column", "--no-column")
	listMode := hasAnyFlag(args, "--list", "-l", "--contains", "--no-contains", "--merged", "--no-merged", "--points-at")
	for i := 0; i < len(args); i++ {
		if !args[i].ok {
			return false
		}
		name, _, hasValue := strings.Cut(args[i].s, "=")
		switch {
		case takesValue[name] && !hasValue && i+1 < len(args) && !strings.HasPrefix(args[i+1].s, "-"):
			i++
		case flags[name]:
		case listMode && !strings.HasPrefix(name, "-"):
		default:
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
	if (rest[0].s == "unlock" || rest[0].s == "lock") && g.denyList(dir) {
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
				if lease, err := target.readLease(); err == nil {
					if err := g.checkRemovable(target, lease); err != nil {
						return err
					}
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
	if _, onEpic := policycheck.BranchEpic(lease.Branch); onEpic && c.BatchesByEpic() {
		// A rolling Epic branch's PR names any of the leaves leased here.
		issues, err := policycheck.ReferencedIssueSet(body)
		if err != nil {
			return deny("pr-body", err.Error())
		}
		for _, issue := range issues {
			if _, ok := w.leaseFor(issue); !ok {
				return deny("pr-body", "body references #"+strconv.Itoa(issue)+", which is not leased in this worktree")
			}
		}
		return nil
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

var quipuAgentCommands = setOf("status", "start", "done", "doctor", "config", "context", "validate", "guide", "version",
	"help", "-h", "--help", "propose", "park", "guard", "policy", "verify-commit")

func (g *guard) quipu(rest []shellArg, dir string) error {
	// Help prints usage and runs nothing; the CLI answers exactly these shapes.
	if quipuHelp(rest) {
		return nil
	}
	if len(rest) > 0 && rest[0].s == "guide" && hasFlag(rest, "--sync") {
		return deny("quipu-admin", "quipu guide --sync")
	}
	if len(rest) == 0 || quipuAgentCommands[rest[0].s] {
		return nil
	}
	switch sub := rest[0].s; {
	case sub == "work" && len(rest) > 1 && rest[1].s == "list":
		return nil
	case sub == "runner" && len(rest) > 1 && (rest[1].s == "status" || rest[1].s == "setup"):
		return nil
	case sub == "worktree" && len(rest) > 1 && rest[1].s == "remove":
		return nil
	case sub == "lease" && len(rest) > 1 && rest[1].s == "release":
		return nil
	case sub == "queue" && len(rest) > 1 && rest[1].s == "status":
		return nil
	case sub == "ready" && agentsMakeReady(dir):
		return nil
	case sub == "triage" && agentsTriage(dir):
		return nil
	case sub == "issue" && len(rest) > 1 && rest[1].s == "create":
		c, err := enforcedConfigIn(dir)
		if err != nil {
			return deny("quipu-admin", "quipu issue create: "+err.Error())
		}
		problem := agentIssueProblem(c, rest[2:], func(number int) (string, error) { return openIssueKind(c, number) })
		if problem != "" {
			return deny("quipu-admin", "quipu issue create: "+problem)
		}
		return nil
	case (sub == "writer" || sub == "journal" || sub == "protect" || sub == "hooks") && !hasFlag(rest, "--apply"):
		return nil
	default:
		return deny("quipu-admin", "quipu "+sub)
	}
}

// quipuHelp reports whether the literal arguments only ask quipu for help.
func quipuHelp(rest []shellArg) bool {
	words := make([]string, len(rest))
	for i, arg := range rest {
		if !arg.ok {
			return false
		}
		words[i] = arg.s
	}
	_, ok := helpRequest(words)
	return ok
}

// enforcedConfigIn reads the config in force for dir's repository.
func enforcedConfigIn(dir string) (model.Config, error) {
	w, err := openWorktree(existingDir(dir))
	if err != nil {
		return model.Config{}, err
	}
	c, _, err := loadEnforcedConfig(w, false)
	return c, err
}

// agentsMakeReady reports whether dir's repository lets agents make work Ready.
func agentsMakeReady(dir string) bool {
	c, err := enforcedConfigIn(dir)
	return err == nil && c.AgentsMakeReady()
}

// agentsTriage reports whether dir's repository lets agents triage Issues.
func agentsTriage(dir string) bool {
	c, err := enforcedConfigIn(dir)
	return err == nil && c.AgentsTriage()
}

// readsOpen reports whether dir's repository sets guard_reads: open.
func (g *guard) readsOpen(dir string) bool {
	c, ok := g.configFor(dir)
	return ok && (c.GuardReadsOpen() || c.GuardDenyList())
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
