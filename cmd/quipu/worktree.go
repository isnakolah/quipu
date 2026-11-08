package main

// Worktree-scoped local state. Parallel agents each work in their own git
// worktree, so the lease file, the agent identity, and branch checks are all
// resolved per worktree, never from shared repository state or the session.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const leaseFileName = "quipu-lease"

// finishedFileName marks a worktree whose work just left it, so its holder may
// tidy it up for a while with no lease. A new lease replaces it.
const finishedFileName = "quipu-finished"
const agentIDFileName = "quipu-agent-id"

// leaseVerifyTTL is how long the guard trusts a live lease check before it
// asks GitHub again, so many agents editing do not exhaust the API limit.
const leaseVerifyTTL = 5 * time.Minute

// worktree is one git working tree: Top is its root, GitDir its private git
// directory (.git for the main checkout, .git/worktrees/<name> for others).
type worktree struct{ Top, GitDir string }

func openWorktree(dir string) (worktree, error) {
	output, err := exec.Command("git", "-C", dir, "rev-parse", "--absolute-git-dir", "--show-toplevel").Output()
	if err != nil {
		return worktree{}, fmt.Errorf("%s is not inside a git worktree", dir)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 2 {
		return worktree{}, fmt.Errorf("%s has no git work tree", dir)
	}
	return worktree{GitDir: lines[0], Top: lines[1]}, nil
}

func currentWorktree() (worktree, error) { return openWorktree(".") }

// linked reports whether this is a secondary worktree rather than the main checkout.
func (w worktree) linked() bool {
	return filepath.Base(filepath.Dir(w.GitDir)) == "worktrees"
}

func (w worktree) git(args ...string) (string, error) {
	output, err := exec.Command("git", append([]string{"-C", w.Top}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

// branch is the branch this worktree works on. Mid-rebase HEAD is detached, so
// the branch being rebased comes from the rebase's own record of it.
func (w worktree) branch() string {
	if branch, _ := w.git("branch", "--show-current"); branch != "" {
		return branch
	}
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		if raw, err := os.ReadFile(filepath.Join(w.GitDir, dir, "head-name")); err == nil {
			if branch, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "refs/heads/"); ok {
				return branch
			}
		}
	}
	return ""
}

// gitOperation is the git operation this worktree is in the middle of, as the
// git subcommand that finishes or abandons it, or "" when there is none.
func (w worktree) gitOperation() string {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(w.GitDir, name))
		return err == nil
	}
	switch {
	case exists("rebase-merge"):
		return "rebase"
	case exists(filepath.Join("rebase-apply", "applying")):
		return "am"
	case exists("rebase-apply"):
		return "rebase"
	case exists("MERGE_HEAD"):
		return "merge"
	case exists("CHERRY_PICK_HEAD"):
		return "cherry-pick"
	case exists("REVERT_HEAD"):
		return "revert"
	}
	return ""
}

// requireIdle refuses to move this worktree to branch while a git operation
// is unfinished there, naming the command that backs out of it.
func (w worktree) requireIdle(branch string) error {
	op := w.gitOperation()
	if op == "" {
		return nil
	}
	e := flowErr("git-busy", "%s is in the middle of a git %s, so it cannot switch to %s", w.Top, op, branch)
	e.Next = fmt.Sprintf("in %s, finish it with `git %s --continue` or back out with `git %s --abort`, then rerun the same command", w.Top, op, op)
	return e
}

// agentID returns this worktree's stable identity, creating it on first use.
// A new agent session in the same worktree inherits it and so resumes the
// worktree's lease; two worktrees never share one, even on one login.
func (w worktree) agentID() (string, error) {
	path := filepath.Join(w.GitDir, agentIDFileName)
	if raw, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id, nil
		}
	}
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(id+"\n"), 0600); err != nil {
		return "", err
	}
	return id, nil
}

// leaseContext is the work this worktree holds, mirrored from GitHub. Only
// `quipu start` writes it, to <git-dir>/quipu-lease, where agents cannot edit
// it. The guard re-verifies it against live GitHub at most every leaseVerifyTTL.
type leaseContext struct {
	Issue      int    `json:"issue"`
	Holder     string `json:"holder"`
	Branch     string `json:"branch"`
	Expires    string `json:"expires,omitempty"`
	VerifiedAt string `json:"verified_at,omitempty"`
}

// fresh reports whether a recent live check still vouches for this lease.
func (l leaseContext) fresh(now time.Time) bool {
	verified, err := time.Parse(time.RFC3339, l.VerifiedAt)
	if err != nil || now.Sub(verified) > leaseVerifyTTL || verified.After(now.Add(time.Minute)) {
		return false
	}
	expires, err := time.Parse(time.RFC3339, l.Expires)
	return err == nil && expires.After(now)
}

// expired reports whether the lease's own expiry has passed. A renewal rewrites
// the record, so a record past its expiry holds nothing.
func (l leaseContext) expired(now time.Time) bool {
	expires, err := time.Parse(time.RFC3339, l.Expires)
	return err == nil && !expires.After(now)
}

func (w worktree) readLease() (leaseContext, error) {
	leases, err := w.readLeases()
	if err != nil {
		return leaseContext{}, err
	}
	// With batches: epic a worktree holds one lease per leaf Issue, all on its
	// branch for one holder. The one that lasts longest stands for them.
	best := leases[0]
	for _, lease := range leases[1:] {
		if lease.Expires > best.Expires {
			best = lease
		}
	}
	return best, nil
}

// readLeases returns every lease recorded for this worktree. The file holds
// one lease, or a list when several leaf Issues share the worktree.
func (w worktree) readLeases() ([]leaseContext, error) {
	raw, err := os.ReadFile(filepath.Join(w.GitDir, leaseFileName))
	if err != nil {
		return nil, fmt.Errorf("quipu no active lease in worktree %s; run `quipu start N` there first; operation denied", w.Top)
	}
	var leases []leaseContext
	if trimmed := strings.TrimSpace(string(raw)); strings.HasPrefix(trimmed, "[") {
		err = json.Unmarshal(raw, &leases)
	} else {
		var lease leaseContext
		err = json.Unmarshal(raw, &lease)
		leases = []leaseContext{lease}
	}
	if err != nil || len(leases) == 0 {
		return nil, errors.New("quipu lease file is malformed; rerun `quipu start N`; operation denied")
	}
	for _, lease := range leases {
		if lease.Issue < 1 || lease.Holder == "" || lease.Branch == "" {
			return nil, errors.New("quipu lease file is malformed; rerun `quipu start N`; operation denied")
		}
	}
	return leases, nil
}

// leaseFor returns this worktree's lease on Issue number.
func (w worktree) leaseFor(number int) (leaseContext, bool) {
	leases, err := w.readLeases()
	if err != nil {
		return leaseContext{}, false
	}
	for _, lease := range leases {
		if lease.Issue == number {
			return lease, true
		}
	}
	return leaseContext{}, false
}

// writeLease records lease as this worktree's only lease.
func (w worktree) writeLease(lease leaseContext) error {
	return w.putLease(lease, false)
}

// addLease records lease beside the worktree's other leases on the same
// branch and holder, for leaf Issues sharing a rolling Epic branch. A lease on
// another branch or holder replaces them.
func (w worktree) addLease(lease leaseContext) error {
	return w.putLease(lease, true)
}

func (w worktree) putLease(lease leaseContext, join bool) error {
	leases := []leaseContext{lease}
	if existing, err := w.readLeases(); err == nil && join {
		for _, other := range existing {
			if other.Issue != lease.Issue && other.Branch == lease.Branch && other.Holder == lease.Holder {
				leases = append(leases, other)
			}
		}
	}
	if err := w.storeLeases(leases); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(w.GitDir, finishedFileName))
	return nil
}

// storeLeases writes the lease file: one lease as an object, as before, so
// older Quipu versions still read it, and several as a list.
func (w worktree) storeLeases(leases []leaseContext) error {
	path := filepath.Join(w.GitDir, leaseFileName)
	if len(leases) == 0 {
		return os.Remove(path)
	}
	sort.Slice(leases, func(i, j int) bool { return leases[i].Issue < leases[j].Issue })
	var raw []byte
	var err error
	if len(leases) == 1 {
		raw, err = json.Marshal(leases[0])
	} else {
		raw, err = json.Marshal(leases)
	}
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// finishedRecord says which work left this worktree and until when its holder
// may still tidy up there.
type finishedRecord struct {
	Issue  int    `json:"issue"`
	Branch string `json:"branch"`
	Until  string `json:"until"`
}

// readFinished returns the worktree's finished record while its window is open.
func (w worktree) readFinished(now time.Time) (finishedRecord, bool) {
	raw, err := os.ReadFile(filepath.Join(w.GitDir, finishedFileName))
	if err != nil {
		return finishedRecord{}, false
	}
	var f finishedRecord
	if json.Unmarshal(raw, &f) != nil || f.Issue < 1 {
		return finishedRecord{}, false
	}
	until, err := time.Parse(time.RFC3339, f.Until)
	if err != nil || !until.After(now) {
		return finishedRecord{}, false
	}
	return f, true
}

// removeLease forgets this worktree's lease on Issue number, and reports whether
// it had one.
func (w worktree) removeLease(number int) bool {
	leases, err := w.readLeases()
	if err != nil {
		return false
	}
	var rest []leaseContext
	for _, lease := range leases {
		if lease.Issue != number {
			rest = append(rest, lease)
		}
	}
	if len(rest) == len(leases) {
		return false
	}
	_ = w.storeLeases(rest)
	return true
}

// release frees this worktree once its work on Issue number has left it, and runs
// the repository's cleanup there. For grace afterwards the holder may still tidy
// up (see guard_finish.go). A second call finds nothing to do. What cleanup
// couldn't do is a warning: the work itself is finished.
func (w worktree) release(number int, cleanup string, grace time.Duration) []string {
	lease, ok := w.leaseFor(number)
	if !ok || !w.removeLease(number) {
		return nil
	}
	// Other leaf Issues still being worked here keep the worktree and its build.
	if _, err := w.readLeases(); err == nil {
		return nil
	}
	if grace > 0 {
		if raw, err := json.Marshal(finishedRecord{Issue: number, Branch: lease.Branch,
			Until: time.Now().Add(grace).UTC().Format(time.RFC3339)}); err == nil {
			_ = os.WriteFile(filepath.Join(w.GitDir, finishedFileName), raw, 0600)
		}
	}
	if cleanup == "" || !w.linked() {
		return nil
	}
	return runCleanup(w.Top, cleanup)
}

// commonPath names a file shared by every worktree of the clone.
func (w worktree) commonPath(name string) (string, error) {
	dir, err := w.git("rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(w.Top, dir)
	}
	return filepath.Join(dir, name), nil
}

// mergeLock serializes the last look before a merge and the merge itself across
// this clone's worktrees. Closing the file releases it, as does the process dying.
func (w worktree) mergeLock() (func(), error) {
	path, err := w.commonPath("quipu-merge.lock")
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockFile(lock); err != nil {
		lock.Close()
		return nil, err
	}
	return func() { lock.Close() }, nil
}

// holding returns the worktree of this repository whose lease is for Issue
// number, preferring w itself, or w when none holds it.
func (w worktree) holding(number int) worktree {
	if _, ok := w.leaseFor(number); ok {
		return w
	}
	porcelain, err := w.git("worktree", "list", "--porcelain")
	if err != nil {
		return w
	}
	for _, line := range strings.Split(porcelain, "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		if other, err := openWorktree(path); err == nil {
			if _, ok := other.leaseFor(number); ok {
				return other
			}
		}
	}
	return w
}

// branchCheckedOutElsewhere names the worktree, other than self, that has
// branch checked out, using `git worktree list --porcelain` output.
func branchCheckedOutElsewhere(porcelain, branch, self string) string {
	path := ""
	for _, line := range strings.Split(porcelain, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/"+branch && filepath.Clean(path) != filepath.Clean(self):
			return path
		}
	}
	return ""
}

func (w worktree) requireBranchFree(branch string) error {
	porcelain, err := w.git("worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	if other := branchCheckedOutElsewhere(porcelain, branch, w.Top); other != "" {
		return flowErr("branch-elsewhere", "branch %s is checked out in worktree %s", branch, other)
	}
	return nil
}

// baseRef fetches the default branch and returns the ref new work starts from.
func (w worktree) baseRef(base string) string {
	_, _ = w.git("fetch", "--quiet", "origin", base)
	if _, err := w.git("rev-parse", "--verify", "--quiet", "origin/"+base); err == nil {
		return "origin/" + base
	}
	return base
}

func (w worktree) hasBranch(branch string) bool {
	_, err := w.git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// switchBranch moves this worktree onto branch, creating it from base.
func (w worktree) switchBranch(branch, base string) error {
	if w.branch() == branch {
		return nil
	}
	if err := w.requireIdle(branch); err != nil {
		return err
	}
	if err := w.requireBranchFree(branch); err != nil {
		return err
	}
	if w.hasBranch(branch) {
		_, err := w.git("switch", branch)
		return err
	}
	_, err := w.git("switch", "-c", branch, w.startPoint(branch, base))
	return err
}

// startPoint is where a branch missing from this clone starts: its copy on origin
// if there is one, since the work may already be there, and otherwise the default
// branch.
func (w worktree) startPoint(branch, base string) string {
	if _, err := w.git("fetch", "--quiet", "origin", branch); err == nil {
		if _, err := w.git("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
			return "origin/" + branch
		}
	}
	return w.baseRef(base)
}

// dispatchPath is where `start --worktree` puts the worktree for Issue N:
// <root>/<repository>-NNN, with root defaulting to a sibling of the checkout.
func dispatchPath(top, root, repository string, number int) string {
	if root == "" {
		root = filepath.Join(filepath.Dir(top), filepath.Base(top)+"-worktrees")
	}
	return filepath.Join(root, fmt.Sprintf("%s-%03d", repository, number))
}

// addWorktree creates (or reuses) a worktree at path on branch. It reports
// whether it created the worktree and the branch, so a lost claim can undo them.
func (w worktree) addWorktree(path, branch, base string) (worktree, bool, bool, error) {
	if existing, err := openWorktree(path); err == nil && existing.Top == path {
		if existing.branch() != branch {
			return worktree{}, false, false, fmt.Errorf("worktree %s exists on branch %s, not %s", path, existing.branch(), branch)
		}
		return existing, false, false, nil
	}
	if err := w.requireBranchFree(branch); err != nil {
		return worktree{}, false, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return worktree{}, false, false, err
	}
	createdBranch := !w.hasBranch(branch)
	args := []string{"worktree", "add", path, branch}
	if createdBranch {
		args = []string{"worktree", "add", "-b", branch, path, w.startPoint(branch, base)}
	}
	if _, err := w.git(args...); err != nil {
		return worktree{}, false, false, err
	}
	created, err := openWorktree(path)
	return created, true, createdBranch, err
}

func (w worktree) removeWorktree(path, branch string, deleteBranch bool) {
	_, _ = w.git("worktree", "remove", "--force", path)
	if deleteBranch {
		_, _ = w.git("branch", "-D", branch)
	}
}
