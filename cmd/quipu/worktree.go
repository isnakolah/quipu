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

func (w worktree) branch() string {
	branch, _ := w.git("branch", "--show-current")
	return branch
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
	raw, err := os.ReadFile(filepath.Join(w.GitDir, leaseFileName))
	if err != nil {
		return leaseContext{}, fmt.Errorf("quipu no active lease in worktree %s; run `quipu start N` there first; operation denied", w.Top)
	}
	var lease leaseContext
	if err := json.Unmarshal(raw, &lease); err != nil || lease.Issue < 1 || lease.Holder == "" || lease.Branch == "" {
		return leaseContext{}, errors.New("quipu lease file is malformed; rerun `quipu start N`; operation denied")
	}
	return lease, nil
}

func (w worktree) writeLease(lease leaseContext) error {
	raw, err := json.Marshal(lease)
	if err != nil {
		return err
	}
	path := filepath.Join(w.GitDir, leaseFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(w.GitDir, finishedFileName))
	return nil
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
	lease, err := w.readLease()
	if err != nil || lease.Issue != number {
		return false
	}
	_ = os.Remove(filepath.Join(w.GitDir, leaseFileName))
	return true
}

// release frees this worktree once its work on Issue number has left it, and runs
// the repository's cleanup there. For grace afterwards the holder may still tidy
// up (see guard_finish.go). A second call finds nothing to do. What cleanup
// couldn't do is a warning: the work itself is finished.
func (w worktree) release(number int, cleanup string, grace time.Duration) []string {
	lease, err := w.readLease()
	if err != nil || lease.Issue != number || !w.removeLease(number) {
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
	if lease, err := w.readLease(); err == nil && lease.Issue == number {
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
			if lease, err := other.readLease(); err == nil && lease.Issue == number {
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
