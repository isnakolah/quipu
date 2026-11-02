package main

// Machine-wide limits. A build slot is a file under the state directory, shared
// by every repository and worktree on this computer, and a short lock on that
// directory lets two sessions never both take the last one. The lock is never
// held across the network.
//
// A slot of work outlives the process that took it, so it clears when its lease
// or worktree goes, its lease lapses, or the guard stops seeing its agent. A slot
// held only for as long as a process runs carries a lock the kernel frees when
// that process dies, which a reused PID can't fool.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/isnakolah/quipu/internal/model"
)

const (
	slotWork     = "work"     // an agent's claim on an Issue in a worktree
	slotCheck    = "check"    // a long check running now
	slotReserved = "reserved" // a claim on its way to GitHub
)

// machineSlot is one slot's file.
type machineSlot struct {
	Kind       string `json:"kind"`
	Repository string `json:"repository"`
	Issue      int    `json:"issue"`
	Worktree   string `json:"worktree,omitempty"`
	GitDir     string `json:"git_dir,omitempty"`
	// IdleMinutes is how long a slot of work lasts without its agent touching it.
	IdleMinutes int       `json:"idle_minutes,omitempty"`
	Since       time.Time `json:"since"`
	path        string
}

// owner tells slots apart for counting: one worktree takes one slot, however many
// files say so.
func (s machineSlot) owner() string {
	if s.GitDir != "" {
		return s.GitDir
	}
	return s.path
}

// machine is this computer's slots.
type machine struct{ dir string }

// openMachine finds the state directory. QUIPU_STATE_DIR moves it.
func openMachine() (machine, error) {
	if dir := os.Getenv("QUIPU_STATE_DIR"); dir != "" {
		return machine{dir}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return machine{}, err
	}
	return machine{filepath.Join(home, ".quipu", "machine")}, nil
}

func (m machine) slots() string { return filepath.Join(m.dir, "slots") }

// locked runs fn holding the machine's lock.
func (m machine) locked(fn func() error) error {
	if err := os.MkdirAll(m.slots(), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(m.dir, "lock"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		return err
	}
	return fn()
}

// live reads the slots still in use and clears the rest. Hold the lock.
func (m machine) live(now time.Time) ([]machineSlot, error) {
	entries, err := os.ReadDir(m.slots())
	if err != nil {
		return nil, err
	}
	var slots []machineSlot
	for _, entry := range entries {
		path := filepath.Join(m.slots(), entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var slot machineSlot
		if json.Unmarshal(raw, &slot) != nil {
			_ = os.Remove(path)
			continue
		}
		slot.path = path
		info, err := entry.Info()
		if err != nil {
			continue
		}
		alive := slotProcessAlive(path)
		if slot.Kind == slotWork {
			alive = slotWorkAlive(slot, info.ModTime(), now)
		}
		if !alive {
			_ = os.Remove(path)
			continue
		}
		slots = append(slots, slot)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Since.Before(slots[j].Since) })
	return slots, nil
}

// slotProcessAlive asks the kernel whether the process that took a slot still
// holds its lock.
func slotProcessAlive(path string) bool {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	free, err := tryLockFile(file)
	return err == nil && !free
}

// slotWorkAlive reports whether anyone can still be working in a slot of work.
func slotWorkAlive(slot machineSlot, touched, now time.Time) bool {
	if slot.IdleMinutes > 0 && now.Sub(touched) > time.Duration(slot.IdleMinutes)*time.Minute {
		return false
	}
	if _, err := os.Stat(slot.Worktree); err != nil {
		return false
	}
	lease, err := (worktree{Top: slot.Worktree, GitDir: slot.GitDir}).readLease()
	if err != nil || lease.Issue != slot.Issue {
		return false
	}
	expires, err := time.Parse(time.RFC3339, lease.Expires)
	return err == nil && expires.After(now)
}

// slotsShort says why no build slot is free for gitDir, if none is. A worktree that
// already has one needs no other; a new worktree passes "".
func slotsShort(c model.Config, slots []machineSlot, gitDir string) string {
	if !c.LimitsBuilds() {
		return ""
	}
	owners := map[string]bool{}
	var names []string
	for _, slot := range slots {
		if !owners[slot.owner()] {
			names = append(names, fmt.Sprintf("%s#%d", slot.Repository, slot.Issue))
		}
		owners[slot.owner()] = true
	}
	if gitDir != "" && owners[realPath(gitDir)] {
		return ""
	}
	if len(owners) < c.Limits.BuildSlots {
		return ""
	}
	return fmt.Sprintf("all %d build slots on this machine are in use, by %s", c.Limits.BuildSlots, strings.Join(names, ", "))
}

// slotHold is a slot that lasts as long as this process keeps its lock.
type slotHold struct {
	file *os.File
	path string
}

// drop gives the slot back. It's safe on no slot, and to repeat.
func (h *slotHold) drop() {
	if h == nil || h.file == nil {
		return
	}
	_ = os.Remove(h.path)
	_ = h.file.Close()
	h.file = nil
}

// acquire writes a slot this process holds. Hold the machine's lock, so nobody
// reads the file half written.
func (m machine) acquire(slot machineSlot) (*slotHold, error) {
	file, err := os.CreateTemp(m.slots(), slot.Kind+"-*.json")
	if err != nil {
		return nil, err
	}
	hold := &slotHold{file, file.Name()}
	if err := lockFile(file); err != nil {
		hold.drop()
		return nil, err
	}
	raw, err := json.Marshal(slot)
	if err == nil {
		_, err = file.Write(raw)
	}
	if err != nil {
		hold.drop()
		return nil, err
	}
	return hold, nil
}

// reserve takes a slot for a claim, or says why there is none. What it returns
// lasts until the claim has been recorded or lost. gitDir is "" for a new worktree.
func (m machine) reserve(c model.Config, gitDir string, number int, now time.Time) (hold *slotHold, reason string, err error) {
	err = m.locked(func() error {
		slots, err := m.live(now)
		if err != nil {
			return err
		}
		if reason = slotsShort(c, slots, gitDir); reason != "" {
			return nil
		}
		hold, err = m.acquire(machineSlot{Kind: slotReserved, Repository: c.Owner + "/" + c.Repository, Issue: number, GitDir: realPathIfSet(gitDir), Since: now})
		return err
	})
	return hold, reason, err
}

// holdCheck takes a slot while a long check runs, so a merge can't start a second
// build on a full machine. A worktree that has a slot uses it, and is covered
// even if its agent's slot lapses while the check takes its time.
func (m machine) holdCheck(c model.Config, w worktree, number int, now time.Time) (*slotHold, error) {
	if !c.LimitsBuilds() {
		return nil, nil
	}
	var hold *slotHold
	err := m.locked(func() error {
		slots, err := m.live(now)
		if err != nil {
			return err
		}
		if reason := slotsShort(c, slots, w.GitDir); reason != "" {
			return flowErr("machine-busy", "local_check did not run: %s", reason)
		}
		hold, err = m.acquire(machineSlot{Kind: slotCheck, Repository: c.Owner + "/" + c.Repository, Issue: number, Worktree: w.Top, GitDir: realPath(w.GitDir), Since: now})
		return err
	})
	return hold, err
}

func realPathIfSet(path string) string {
	if path == "" {
		return ""
	}
	return realPath(path)
}

// workSlotPath is where a worktree's slot of work lives. A worktree has at most one.
func (m machine) workSlotPath(gitDir string) string {
	sum := sha256.Sum256([]byte(realPath(gitDir)))
	return filepath.Join(m.slots(), slotWork+"-"+hex.EncodeToString(sum[:8])+".json")
}

// recordWork writes the slot of work for what a worktree now holds. Resuming goes
// through here too, and is never refused, so it can put a machine over its limit.
func (m machine) recordWork(c model.Config, w worktree, number int, now time.Time) error {
	return m.locked(func() error {
		raw, err := json.Marshal(machineSlot{Kind: slotWork, Repository: c.Owner + "/" + c.Repository, Issue: number, Worktree: w.Top, GitDir: realPath(w.GitDir), IdleMinutes: c.SlotIdleMinutes(), Since: now})
		if err != nil {
			return err
		}
		path := m.workSlotPath(w.GitDir)
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, raw, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
		return os.Chtimes(path, now, now)
	})
}

// dropWork frees a worktree's slot of work: its agent has finished building.
func (m machine) dropWork(w worktree) { _ = os.Remove(m.workSlotPath(w.GitDir)) }

// touchWork shows its agent is still there. The guard calls this on every tool
// call, so it does nothing but set a time, and nothing at all without a slot.
func (m machine) touchWork(gitDir string, now time.Time) {
	_ = os.Chtimes(m.workSlotPath(gitDir), now, now)
}

// diskShort says why new work can't start on this disk, if it can't.
func diskShort(c model.Config, path string) string {
	if c.Limits.MinFreeDiskGB <= 0 {
		return ""
	}
	free, err := freeDiskBytes(path)
	if err != nil {
		return fmt.Sprintf("could not read the free disk at %s, and limits.min_free_disk_gb needs it: %v", path, err)
	}
	if gb := free / (1 << 30); gb < uint64(c.Limits.MinFreeDiskGB) {
		return fmt.Sprintf("%d GB free on this disk, under limits.min_free_disk_gb of %d", gb, c.Limits.MinFreeDiskGB)
	}
	return ""
}

// claimGate is what stands between a claim and this machine. It settles disk
// first, since work outside the build areas needs that too.
type claimGate struct {
	c    model.Config
	m    machine
	top  string
	own  string
	now  time.Time
	fail error
}

func openClaimGate(c model.Config, here worktree, newWorktree bool, now time.Time) claimGate {
	gate := claimGate{c: c, top: here.Top, now: now}
	if !newWorktree {
		gate.own = here.GitDir
	}
	if c.LimitsMachine() {
		gate.m, gate.fail = openMachine()
	}
	return gate
}

// enter takes what a claim on this Issue needs, or says why it can't start.
func (g claimGate) enter(number int, area string) (*slotHold, string) {
	if !g.c.LimitsMachine() {
		return nil, ""
	}
	if reason := diskShort(g.c, g.top); reason != "" {
		return nil, reason
	}
	if !g.c.CountsAsBuild(area) || !g.c.LimitsBuilds() {
		return nil, ""
	}
	if g.fail != nil {
		return nil, "could not read this machine's slots: " + g.fail.Error()
	}
	hold, reason, err := g.m.reserve(g.c, g.own, number, g.now)
	if err != nil {
		return nil, "could not read this machine's slots: " + err.Error()
	}
	return hold, reason
}

// outsideBuilds keeps what can still start when no slot is free. reason is empty
// when every candidate can.
func (g claimGate) outsideBuilds(ready []readyWork) (kept []readyWork, reason string) {
	if !g.c.LimitsMachine() {
		return ready, ""
	}
	if reason := diskShort(g.c, g.top); reason != "" {
		return nil, reason
	}
	if !g.c.LimitsBuilds() {
		return ready, ""
	}
	if g.fail != nil {
		return nil, "could not read this machine's slots: " + g.fail.Error()
	}
	var slots []machineSlot
	err := g.m.locked(func() (err error) {
		slots, err = g.m.live(g.now)
		return err
	})
	if err != nil {
		return nil, "could not read this machine's slots: " + err.Error()
	}
	reason = slotsShort(g.c, slots, g.own)
	if reason == "" {
		return ready, ""
	}
	for _, item := range ready {
		if !g.c.CountsAsBuild(item.Area) {
			kept = append(kept, item)
		}
	}
	return kept, reason
}

// busyResult is `quipu start` turning work down without claiming it.
func busyResult(number int, reason string) flowResult {
	return flowResult{Issue: number, Status: "Busy", Reason: reason, Next: "Nothing was claimed. Finish any work you hold; when none is left, report this reason and stop. Rerun quipu start later."}
}

// recordWorkSlot notes what a worktree now holds, and answers with any warning.
func recordWorkSlot(c model.Config, w worktree, number int, area string, now time.Time) []string {
	if !c.LimitsBuilds() || !c.CountsAsBuild(area) {
		return nil
	}
	m, err := openMachine()
	if err == nil {
		err = m.recordWork(c, w, number, now)
	}
	if err != nil {
		return []string{"could not record this worktree's build slot: " + err.Error()}
	}
	return nil
}

// freeWorkSlot gives back a worktree's slot of work, if it has one.
func freeWorkSlot(c model.Config, w worktree) {
	if !c.LimitsBuilds() {
		return
	}
	if m, err := openMachine(); err == nil {
		m.dropWork(w)
	}
}

// enforcedLimits reads the limits in force: the default branch's, where there is
// one, as with every setting that decides what an agent may do. They only tighten,
// so a config that can't be trusted keeps its own.
func enforcedLimits(c model.Config, here worktree, refresh bool) model.Config {
	if enforced, _, err := loadEnforcedConfig(here, refresh); err == nil {
		c.Limits = enforced.Limits
	}
	if !machineEnforced {
		c.Limits = model.Limits{}
	}
	return c
}

// machineDoctorCheck says whether this session can use the limits. An agent's
// sandbox may not let it write outside the repository.
func machineDoctorCheck() doctorCheck {
	if !machineEnforced {
		return doctorCheck{"machine limits", "warn", "limits are set, and aren't enforced on this system, which has no file locks", ""}
	}
	m, err := openMachine()
	if err == nil {
		err = m.locked(func() error { return nil })
	}
	if err != nil {
		return doctorCheck{"machine limits", "fail", "can't write this machine's slots, so new builds are turned down: " + err.Error(), "let agents write to ~/.quipu/machine, or set QUIPU_STATE_DIR to a directory they can"}
	}
	return doctorCheck{"machine limits", "ok", "", ""}
}

// machineHolder is one slot in `quipu status`.
type machineHolder struct {
	Kind       string    `json:"kind"`
	Repository string    `json:"repository"`
	Issue      int       `json:"issue"`
	Worktree   string    `json:"worktree,omitempty"`
	Since      time.Time `json:"since"`
}

// machineDisk is the free disk in `quipu status`.
type machineDisk struct {
	FreeGB uint64 `json:"free_gb"`
	MinGB  int    `json:"min_gb"`
	Short  bool   `json:"short"`
}

// machineReport is this machine's usage in `quipu status`, when limits are set.
type machineReport struct {
	Slots   int             `json:"slots"`
	Used    int             `json:"used"`
	Holders []machineHolder `json:"holders"`
	Disk    *machineDisk    `json:"disk,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// loadMachineReport reads usage, clearing what has gone stale on the way.
func loadMachineReport(c model.Config, top string, now time.Time) *machineReport {
	if !c.LimitsMachine() {
		return nil
	}
	report := &machineReport{Slots: c.Limits.BuildSlots, Holders: []machineHolder{}}
	if c.Limits.MinFreeDiskGB > 0 {
		free, err := freeDiskBytes(top)
		if err != nil {
			report.Error = err.Error()
		} else {
			gb := free / (1 << 30)
			report.Disk = &machineDisk{FreeGB: gb, MinGB: c.Limits.MinFreeDiskGB, Short: gb < uint64(c.Limits.MinFreeDiskGB)}
		}
	}
	m, err := openMachine()
	var slots []machineSlot
	if err == nil {
		err = m.locked(func() (err error) {
			slots, err = m.live(now)
			return err
		})
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		report.Error = err.Error()
	}
	owners := map[string]bool{}
	for _, slot := range slots {
		if !owners[slot.owner()] {
			report.Used++
		}
		owners[slot.owner()] = true
		report.Holders = append(report.Holders, machineHolder{slot.Kind, slot.Repository, slot.Issue, slot.Worktree, slot.Since})
	}
	return report
}

// renderMachine is the same report for a person.
func renderMachine(report *machineReport) string {
	if report == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nThis machine\n")
	if report.Slots > 0 {
		fmt.Fprintf(&b, "  build slots: %d of %d in use\n", report.Used, report.Slots)
	}
	for _, holder := range report.Holders {
		fmt.Fprintf(&b, "    %s#%d (%s) %s\n", holder.Repository, holder.Issue, holder.Kind, holder.Worktree)
	}
	if report.Disk != nil {
		fmt.Fprintf(&b, "  disk: %d GB free, %d GB required\n", report.Disk.FreeGB, report.Disk.MinGB)
	}
	if report.Error != "" {
		fmt.Fprintf(&b, "  ! %s\n", report.Error)
	}
	return b.String()
}
