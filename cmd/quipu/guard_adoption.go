package main

// What the guard allows in a bootstrap window (bootstrapwindow.go). Only the
// worktree that ran `quipu adopt --apply` gets it, only while it is open, and
// only for a session working in that worktree. Reads of every kind pass, as do
// help, the setup commands' dry runs and `quipu import --apply`. Until the
// adoption commit lands on the default branch, the agent may also write
// Quipu's own files, commit them, push them to a branch of their own and open
// the PR the owner merges. A window never pushes to the default branch, never
// force-pushes, and changes nothing else in the worktree without a lease.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	policycheck "github.com/isnakolah/quipu/internal/policy"
)

// windowState is what the guard learned about one worktree's window.
type windowState struct{ open, landed bool }

// window reads dir's window once per tool call. It counts only when the
// session itself works in that worktree.
func (g *guard) window(dir string) windowState {
	if state, ok := g.windows[dir]; ok {
		return state
	}
	var state windowState
	if w, err := openWorktree(existingDir(dir)); err == nil && g.session.GitDir != "" &&
		filepath.Clean(g.session.GitDir) == filepath.Clean(w.GitDir) && hasConfig(w.Top) {
		if _, open := w.bootstrapFor(g.now); open {
			state = windowState{open: true, landed: w.configLanded()}
		}
	}
	g.windows[dir] = state
	return state
}

// adopting reports whether dir lies in the session's own worktree while its
// bootstrap window is open.
func (g *guard) adopting(dir string) bool { return g.window(dir).open }

// landing reports whether dir is adopting and the adoption commit has not yet
// reached the default branch.
func (g *guard) landing(dir string) bool {
	state := g.window(dir)
	return state.open && !state.landed
}

// adoptionFile reports whether a path in the worktree is one of the files
// adopt writes: the enforcement files, the Issue forms, the handbook, and the
// pointers to it.
func adoptionFile(rel string) bool {
	switch {
	case protectedFile(rel), strings.HasPrefix(rel, ".github/ISSUE_TEMPLATE/"):
		return true
	}
	return rel == guideRule || rel == "AGENTS.md" || rel == "CLAUDE.md"
}

// adoptionQuipu allows the setup commands a window opens: adopt's audit,
// import with or without --apply, and, before the commit lands, rewriting
// the handbook and the hooks, which touch only Quipu's own files.
func (g *guard) adoptionQuipu(rest []shellArg, dir string) bool {
	if len(rest) == 0 || !g.adopting(dir) {
		return false
	}
	for _, arg := range rest {
		if !arg.ok {
			return false
		}
	}
	switch rest[0].s {
	case "adopt":
		return !hasFlag(rest, "--apply")
	case "import":
		return true
	case "guide", "hooks":
		return g.landing(dir)
	}
	return false
}

// landingWrite lets the adoption commit be made: a lease-less `git add` or
// `git commit` in the adopting worktree before the commit lands.
func (g *guard) landingWrite(err error, dir string) error {
	var d denial
	if errors.As(err, &d) && d.Code == "no-lease" && g.landing(dir) {
		return nil
	}
	return err
}

// adoptionPush allows pushing the adoption commit to origin under any branch
// but the default one. It refuses what it cannot prove misses the default.
func (g *guard) adoptionPush(rest []shellArg, dir string) error {
	w, err := openWorktree(existingDir(dir))
	if err != nil {
		return deny("push-target", err.Error())
	}
	defaults := defaultBranches(w)
	if len(defaults) == 0 {
		return deny("push-target", "the default branch is unknown, so this push cannot be shown to miss it")
	}
	var positional []string
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		switch {
		case !arg.ok:
			return deny("push-target", "computed push argument")
		case arg.s == "--force" || strings.HasPrefix(arg.s, "--force-") || isShortFlag(arg.s, 'f'):
			return deny("force-push", "the adoption commit goes to a new branch; nothing needs forcing")
		case arg.s == "-d" || arg.s == "--delete" || arg.s == "--all" || arg.s == "--mirror" || arg.s == "--tags" ||
			arg.s == "--follow-tags" || arg.s == "--prune" || strings.HasPrefix(arg.s, "--repo") ||
			strings.HasPrefix(arg.s, "--receive-pack") || strings.HasPrefix(arg.s, "--exec"):
			return deny("push-target", arg.s)
		case arg.s == "-o" || arg.s == "--push-option":
			i++
		case strings.HasPrefix(arg.s, "-"):
		default:
			positional = append(positional, arg.s)
		}
	}
	example := "git push -u origin HEAD:quipu-adoption"
	if len(positional) == 0 || positional[0] != "origin" {
		return deny("push-target", "during adoption, name origin and the setup branch: "+example)
	}
	refspecs := positional[1:]
	if len(refspecs) == 0 {
		refspecs = []string{"HEAD"}
	}
	for _, refspec := range refspecs {
		if strings.HasPrefix(refspec, "+") {
			return deny("force-push", refspec)
		}
		src, dst, hasDst := strings.Cut(refspec, ":")
		if !hasDst {
			dst = src
		}
		if dst == "HEAD" {
			dst = w.branch()
		}
		dst = strings.TrimPrefix(dst, "refs/heads/")
		if dst == "" || strings.HasPrefix(dst, "refs/") || contains(defaults, dst) {
			return deny("push-target", "the adoption commit never goes to the default branch; push it to a branch of its own: "+example)
		}
	}
	return nil
}

// adoptionPR allows opening the adoption PR: from a branch that is not the
// default one, into the default branch, with a body and no closing keywords.
func (g *guard) adoptionPR(rest []shellArg, dir string) error {
	w, err := openWorktree(existingDir(dir))
	if err != nil {
		return deny("pr-body", err.Error())
	}
	defaults := defaultBranches(w)
	head, base, hasBody := "", "", false
	for i := 0; i < len(rest); i++ {
		flag, inline, hasInline := strings.Cut(rest[i].s, "=")
		value := func() (string, bool) {
			if hasInline {
				return inline, rest[i].ok
			}
			if i+1 >= len(rest) {
				return "", false
			}
			i++
			return rest[i].s, rest[i].ok
		}
		switch flag {
		case "-H", "--head", "-B", "--base", "-t", "--title", "-b", "--body":
			v, ok := value()
			if !ok {
				return deny("pr-body", flag+" is computed")
			}
			switch flag {
			case "-H", "--head":
				head = v
			case "-B", "--base":
				base = v
			default:
				if err := policycheck.NoClosingKeywords(v, "PR "+strings.TrimLeft(flag, "-")); err != nil {
					return deny("pr-body", err.Error())
				}
				hasBody = hasBody || flag == "-b" || flag == "--body"
			}
		case "-F", "--body-file":
			v, ok := value()
			if !ok || v == "-" {
				return deny("pr-body", "body file is computed or stdin")
			}
			raw, err := os.ReadFile(g.resolve(dir, v))
			if err != nil {
				return deny("pr-body", err.Error())
			}
			if err := policycheck.NoClosingKeywords(string(raw), "PR body"); err != nil {
				return deny("pr-body", err.Error())
			}
			hasBody = true
		case "-l", "--label", "--add-label", "--remove-label":
			return deny("github-write", "PR labels are set by the owner")
		case "-f", "--fill", "--fill-first", "--fill-verbose", "-w", "--web", "-e", "--editor":
			return deny("pr-body", flag+" leaves the body unchecked")
		}
	}
	switch {
	case len(defaults) == 0 || !contains(defaults, base):
		return deny("pr-body", "during adoption, open the setup PR with --base set to the default branch")
	case head == "" || contains(defaults, head):
		return deny("pr-body", "during adoption, open the setup PR with --head set to the branch you pushed")
	case !hasBody:
		return deny("pr-body", "pass --body or --body-file")
	}
	return nil
}

// defaultBranches names the default branch as git records it and as the
// worktree's config says, so a push to either is refused.
func defaultBranches(w worktree) []string {
	var names []string
	if branch := w.defaultBranch(); branch != "" {
		names = append(names, branch)
	}
	if c, err := loadConfigAt(w.Top); err == nil && c.DefaultBranch != "" && !contains(names, c.DefaultBranch) {
		names = append(names, c.DefaultBranch)
	}
	return names
}
