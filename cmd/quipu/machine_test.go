package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/isnakolah/quipu/internal/model"
)

var machineNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func limited(slots int, areas ...string) model.Config {
	return model.Config{Owner: "o", Repository: "r", Limits: model.Limits{BuildSlots: slots, BuildAreas: areas}}
}

func testMachine(t *testing.T) machine {
	t.Helper()
	if !machineEnforced {
		t.Skip("machine limits need file locks")
	}
	dir := t.TempDir()
	t.Setenv("QUIPU_STATE_DIR", dir)
	m, err := openMachine()
	if err != nil || m.dir != dir {
		t.Fatalf("%+v, %v", m, err)
	}
	return m
}

// leasedWorktree is a worktree whose lease runs for an hour, as of machineNow.
func leasedWorktree(t *testing.T, number int) worktree {
	t.Helper()
	dir := t.TempDir()
	w := worktree{Top: dir, GitDir: filepath.Join(dir, "gitdir")}
	if err := os.Mkdir(w.GitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lease := leaseContext{Issue: number, Holder: "h", Branch: "b", Expires: machineNow.Add(time.Hour).Format(time.RFC3339)}
	if err := w.writeLease(lease); err != nil {
		t.Fatal(err)
	}
	return w
}

func liveSlots(t *testing.T, m machine, now time.Time) []machineSlot {
	t.Helper()
	var slots []machineSlot
	if err := m.locked(func() (err error) {
		slots, err = m.live(now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return slots
}

func TestLimitsUnsetLeaveEverythingAlone(t *testing.T) {
	t.Setenv("QUIPU_STATE_DIR", filepath.Join(t.TempDir(), "never-made"))
	c := model.Config{Owner: "o", Repository: "r"}
	gate := openClaimGate(c, worktree{Top: "/nowhere", GitDir: "/nowhere/git"}, false, machineNow)
	hold, reason := gate.enter(1, "app")
	if hold != nil || reason != "" {
		t.Fatalf("%v %q", hold, reason)
	}
	ready := []readyWork{{Number: 1, Area: "app"}}
	if kept, reason := gate.outsideBuilds(ready); len(kept) != 1 || reason != "" {
		t.Fatalf("%v %q", kept, reason)
	}
	if loadMachineReport(c, "/nowhere", machineNow) != nil {
		t.Fatal("status says nothing")
	}
	if warnings := recordWorkSlot(c, worktree{}, 1, "app", machineNow); warnings != nil {
		t.Fatal(warnings)
	}
	if held, err := (machine{}).holdCheck(c, worktree{}, 1, machineNow); held != nil || err != nil {
		t.Fatalf("%v, %v", held, err)
	}
	if _, err := os.Stat(os.Getenv("QUIPU_STATE_DIR")); !os.IsNotExist(err) {
		t.Fatalf("nothing written: %v", err)
	}
}

func TestSlotsAreCountedAcrossWorktreesAndRepositories(t *testing.T) {
	m := testMachine(t)
	first, second := leasedWorktree(t, 1), leasedWorktree(t, 2)
	c := limited(2)
	other := limited(2)
	other.Repository = "another"
	if err := m.recordWork(c, first, 1, machineNow); err != nil {
		t.Fatal(err)
	}
	if err := m.recordWork(other, second, 2, machineNow); err != nil {
		t.Fatal(err)
	}
	hold, reason, err := m.reserve(c, "", 3, machineNow)
	if err != nil || hold != nil {
		t.Fatalf("%v %v", hold, err)
	}
	if !strings.Contains(reason, "all 2") || !strings.Contains(reason, "o/r#1") || !strings.Contains(reason, "o/another#2") {
		t.Fatalf("the reason names who: %q", reason)
	}
	// The worktree that has one needs no other.
	again, reason, err := m.reserve(c, first.GitDir, 3, machineNow)
	if err != nil || reason != "" || again == nil {
		t.Fatalf("%v %q %v", again, reason, err)
	}
	again.drop()
}

func TestAWorktreeTakesOneSlotHoweverManyFilesSaySo(t *testing.T) {
	m := testMachine(t)
	w := leasedWorktree(t, 1)
	c := limited(2)
	if err := m.recordWork(c, w, 1, machineNow); err != nil {
		t.Fatal(err)
	}
	check, err := m.holdCheck(c, w, 1, machineNow)
	if err != nil {
		t.Fatal(err)
	}
	defer check.drop()
	if report := machineUsage(t, m, c); report.Used != 1 || len(report.Holders) != 2 {
		t.Fatalf("%+v", report)
	}
	hold, reason, err := m.reserve(c, "", 2, machineNow)
	if err != nil || reason != "" {
		t.Fatalf("one is free: %q, %v", reason, err)
	}
	hold.drop()
}

func machineUsage(t *testing.T, m machine, c model.Config) machineReport {
	t.Helper()
	report := loadMachineReport(c, m.dir, machineNow)
	if report == nil {
		t.Fatal("limits are set")
	}
	return *report
}

func TestWorkSlotsClearThemselves(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(t *testing.T, w worktree)
		now    time.Time
	}{
		"worktree gone": {func(t *testing.T, w worktree) {
			if err := os.RemoveAll(w.Top); err != nil {
				t.Fatal(err)
			}
		}, machineNow},
		"lease file gone": {func(t *testing.T, w worktree) { w.removeLease(1) }, machineNow},
		"lease for other work": {func(t *testing.T, w worktree) {
			if err := w.writeLease(leaseContext{Issue: 2, Holder: "h", Branch: "b", Expires: machineNow.Add(time.Hour).Format(time.RFC3339)}); err != nil {
				t.Fatal(err)
			}
		}, machineNow},
		"lease expired":    {func(t *testing.T, w worktree) {}, machineNow.Add(2 * time.Hour)},
		"agent gone quiet": {func(t *testing.T, w worktree) {}, machineNow.Add(46 * time.Minute)},
	} {
		t.Run(name, func(t *testing.T) {
			m := testMachine(t)
			w := leasedWorktree(t, 1)
			if err := m.recordWork(limited(1), w, 1, machineNow); err != nil {
				t.Fatal(err)
			}
			m.touchWork(w.GitDir, machineNow)
			if len(liveSlots(t, m, machineNow)) != 1 {
				t.Fatal("it starts in use")
			}
			tc.change(t, w)
			if slots := liveSlots(t, m, tc.now); len(slots) != 0 {
				t.Fatalf("%+v", slots)
			}
			if _, err := os.Stat(m.workSlotPath(w.GitDir)); !os.IsNotExist(err) {
				t.Fatalf("the file goes: %v", err)
			}
		})
	}
}

func TestGuardKeepsAWorkSlotAlive(t *testing.T) {
	m := testMachine(t)
	w := leasedWorktree(t, 1)
	if err := m.recordWork(limited(1), w, 1, machineNow); err != nil {
		t.Fatal(err)
	}
	later := machineNow.Add(10 * time.Minute)
	m.touchWork(w.GitDir, later)
	if len(liveSlots(t, m, later.Add(44*time.Minute))) != 1 {
		t.Fatal("the agent was seen 44 minutes ago")
	}
	m.touchWork(filepath.Join(t.TempDir(), "no-slot"), later)
	if entries, _ := os.ReadDir(m.slots()); len(entries) != 1 {
		t.Fatalf("touching creates nothing: %v", entries)
	}
}

func TestDroppedWorkFreesItsSlot(t *testing.T) {
	m := testMachine(t)
	w := leasedWorktree(t, 1)
	c := limited(1)
	if err := m.recordWork(c, w, 1, machineNow); err != nil {
		t.Fatal(err)
	}
	freeWorkSlot(c, w)
	if len(liveSlots(t, m, machineNow)) != 0 {
		t.Fatal("In review frees the slot, though the lease stays")
	}
}

func TestResumingIsNeverRefused(t *testing.T) {
	m := testMachine(t)
	c := limited(1)
	first, second := leasedWorktree(t, 1), leasedWorktree(t, 2)
	if warnings := recordWorkSlot(c, first, 1, "", machineNow); warnings != nil {
		t.Fatal(warnings)
	}
	if warnings := recordWorkSlot(c, second, 2, "", machineNow); warnings != nil {
		t.Fatal(warnings)
	}
	if report := machineUsage(t, m, c); report.Used != 2 {
		t.Fatalf("over the limit, and honest about it: %+v", report)
	}
}

func TestOutsideBuildAreasTakesNoSlot(t *testing.T) {
	m := testMachine(t)
	c := limited(1, "backend")
	w := leasedWorktree(t, 1)
	if err := m.recordWork(c, w, 1, machineNow); err != nil {
		t.Fatal(err)
	}
	gate := openClaimGate(c, worktree{Top: t.TempDir(), GitDir: t.TempDir()}, true, machineNow)

	if hold, reason := gate.enter(2, "docs"); hold != nil || reason != "" {
		t.Fatalf("docs: %v %q", hold, reason)
	}
	if _, reason := gate.enter(2, "backend"); reason == "" {
		t.Fatal("a build must wait")
	}
	if _, reason := gate.enter(2, ""); reason == "" {
		t.Fatal("work with no area counts as a build")
	}

	kept, reason := gate.outsideBuilds([]readyWork{{Number: 2, Area: "backend"}, {Number: 3, Area: "docs"}})
	if len(kept) != 1 || kept[0].Number != 3 || reason == "" {
		t.Fatalf("%v %q", kept, reason)
	}
	if warnings := recordWorkSlot(c, leasedWorktree(t, 3), 3, "docs", machineNow); warnings != nil {
		t.Fatal(warnings)
	}
	if report := machineUsage(t, m, c); report.Used != 1 {
		t.Fatalf("%+v", report)
	}
}

func TestDiskFloorStopsNewClaimsOfEveryArea(t *testing.T) {
	testMachine(t)
	top := t.TempDir()
	free, err := freeDiskBytes(top)
	if err != nil {
		t.Fatal(err)
	}
	c := limited(0)
	c.Limits.MinFreeDiskGB = int(free/(1<<30)) + 1
	gate := openClaimGate(c, worktree{Top: top, GitDir: top}, false, machineNow)
	if _, reason := gate.enter(1, "docs"); !strings.Contains(reason, "min_free_disk_gb") {
		t.Fatalf("%q", reason)
	}
	if kept, reason := gate.outsideBuilds([]readyWork{{Number: 1, Area: "docs"}}); len(kept) != 0 || reason == "" {
		t.Fatalf("%v %q", kept, reason)
	}
	report := loadMachineReport(c, top, machineNow)
	if report == nil || report.Disk == nil || !report.Disk.Short {
		t.Fatalf("%+v", report)
	}

	c.Limits.MinFreeDiskGB = 1
	if free >= 1<<30 {
		if _, reason := openClaimGate(c, worktree{Top: top, GitDir: top}, false, machineNow).enter(1, "docs"); reason != "" {
			t.Fatalf("%q", reason)
		}
	}
}

func TestCheckHoldsASlotAndGivesItBack(t *testing.T) {
	m := testMachine(t)
	c := limited(1)
	w := worktree{Top: t.TempDir(), GitDir: t.TempDir()}
	hold, err := m.holdCheck(c, w, 1, machineNow)
	if err != nil || hold == nil {
		t.Fatalf("%v %v", hold, err)
	}
	if _, reason, _ := m.reserve(c, "", 2, machineNow); reason == "" {
		t.Fatal("a running check counts")
	}
	if _, err := m.holdCheck(c, worktree{Top: t.TempDir(), GitDir: t.TempDir()}, 2, machineNow); flowCode(err) != "machine-busy" {
		t.Fatalf("got %v", err)
	}
	hold.drop()
	hold.drop()
	if len(liveSlots(t, m, machineNow)) != 0 {
		t.Fatal("the slot is free again")
	}
}

func TestNoCIHoldsASlotOnlyForTheCheck(t *testing.T) {
	f := pendingPull()
	ops := f.ops()
	held := 0
	inner := ops.check
	ops.hold = func() (func(), error) { held++; return func() { held-- }, nil }
	ops.check = func(command string) (int, error) {
		if held != 1 {
			t.Fatalf("the check needs its slot: %d", held)
		}
		return inner(command)
	}
	if _, err := mergeWithoutCI(noCIConfig, false, localEvidence, ops); err != nil {
		t.Fatal(err)
	}
	if held != 0 {
		t.Fatalf("it is given back: %d", held)
	}

	f = pendingPull()
	ops = f.ops()
	ops.hold = func() (func(), error) { return nil, flowErr("machine-busy", "full") }
	if _, err := mergeWithoutCI(noCIConfig, false, localEvidence, ops); flowCode(err) != "machine-busy" || len(f.ran) != 0 || len(f.merges) != 0 {
		t.Fatalf("no slot, no check, no merge: %v, %v, %v", err, f.ran, f.merges)
	}
}

// TestMachineHelper is a separate process for the tests below.
func TestMachineHelper(t *testing.T) {
	if os.Getenv("QUIPU_MACHINE_HELPER") == "" {
		t.Skip("run by the tests below")
	}
	m, err := openMachine()
	if err != nil {
		t.Fatal(err)
	}
	number, _ := strconv.Atoi(os.Getenv("QUIPU_MACHINE_HELPER"))
	hold, reason, err := m.reserve(limited(1), "", number, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		io.WriteString(os.Stdout, "BUSY\n")
		return
	}
	io.WriteString(os.Stdout, "HOLDING\n")
	// Hold it until the test closes stdin, or kills this process.
	_, _ = io.Copy(io.Discard, os.Stdin)
	hold.drop()
}

// helperProcess starts a process racing for the slot and returns its first line.
func helperProcess(t *testing.T, number int) (*exec.Cmd, io.WriteCloser, *bufio.Reader) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMachineHelper$")
	cmd.Env = append(os.Environ(), "QUIPU_MACHINE_HELPER="+strconv.Itoa(number))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd, stdin, bufio.NewReader(stdout)
}

func TestOnlyOneProcessTakesTheLastSlot(t *testing.T) {
	testMachine(t)
	const racers = 8
	type racer struct {
		stdin io.WriteCloser
		lines *bufio.Reader
	}
	var all []racer
	for i := range racers {
		_, stdin, lines := helperProcess(t, i+1)
		all = append(all, racer{stdin, lines})
	}
	holding, busy := 0, 0
	for _, r := range all {
		line, err := r.lines.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		switch strings.TrimSpace(line) {
		case "HOLDING":
			holding++
		case "BUSY":
			busy++
		default:
			t.Fatalf("got %q", line)
		}
	}
	if holding != 1 || busy != racers-1 {
		t.Fatalf("holding %d, busy %d", holding, busy)
	}
}

func TestAKilledHolderFreesItsSlot(t *testing.T) {
	m := testMachine(t)
	cmd, _, lines := helperProcess(t, 1)
	if line, err := lines.ReadString('\n'); err != nil || strings.TrimSpace(line) != "HOLDING" {
		t.Fatalf("%q, %v", line, err)
	}
	if _, reason, err := m.reserve(limited(1), "", 2, machineNow); err != nil || reason == "" {
		t.Fatalf("while it lives it holds: %q, %v", reason, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	hold, reason, err := m.reserve(limited(1), "", 2, machineNow)
	if err != nil || reason != "" || hold == nil {
		t.Fatalf("%v %q %v", hold, reason, err)
	}
	hold.drop()
	if entries, _ := os.ReadDir(m.slots()); len(entries) != 0 {
		t.Fatalf("its file went too: %v", entries)
	}
}

func TestStatusReportsUsage(t *testing.T) {
	m := testMachine(t)
	c := limited(2)
	c.Limits.MinFreeDiskGB = 1
	w := leasedWorktree(t, 7)
	if err := m.recordWork(c, w, 7, machineNow); err != nil {
		t.Fatal(err)
	}
	report := loadMachineReport(c, w.Top, machineNow)
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var seen struct {
		Slots, Used int
		Holders     []struct {
			Kind, Repository, Worktree string
			Issue                      int
		}
		Disk *struct {
			MinGB int `json:"min_gb"`
		}
	}
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatal(err)
	}
	if seen.Slots != 2 || seen.Used != 1 || len(seen.Holders) != 1 || seen.Holders[0].Issue != 7 || seen.Holders[0].Kind != slotWork || seen.Holders[0].Repository != "o/r" || seen.Disk == nil || seen.Disk.MinGB != 1 {
		t.Fatalf("%s", raw)
	}
	text := renderMachine(report)
	if !strings.Contains(text, "1 of 2") || !strings.Contains(text, "o/r#7") {
		t.Fatalf("%s", text)
	}
	if renderMachine(nil) != "" {
		t.Fatal("nothing without limits")
	}
}

func TestBusyResultSaysWhyAndPrintsIt(t *testing.T) {
	result := busyResult(9, "no room")
	if result.Status != "Busy" || result.Reason != "no room" || result.Issue != 9 || result.Next == "" {
		t.Fatalf("%+v", result)
	}
	if result := busyResult(0, "x"); result.Issue != 0 {
		t.Fatalf("%+v", result)
	}
}

func TestAnUnreadableMachineTurnsClaimsDown(t *testing.T) {
	if !machineEnforced {
		t.Skip("machine limits need file locks")
	}
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("permissions don't bind here")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, "file")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUIPU_STATE_DIR", filepath.Join(blocked, "under-a-file"))
	gate := openClaimGate(limited(1), worktree{Top: dir, GitDir: dir}, true, machineNow)
	if _, reason := gate.enter(1, "app"); !strings.Contains(reason, "could not read") {
		t.Fatalf("%q", reason)
	}
	if kept, reason := gate.outsideBuilds([]readyWork{{Number: 1, Area: "app"}}); len(kept) != 0 || !strings.Contains(reason, "could not read") {
		t.Fatalf("%v %q", kept, reason)
	}
}

func TestDoctorSaysWhetherLimitsCanWork(t *testing.T) {
	testMachine(t)
	if check := machineDoctorCheck(); check.Status != "ok" {
		t.Fatalf("%+v", check)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUIPU_STATE_DIR", filepath.Join(blocked, "under-a-file"))
	if check := machineDoctorCheck(); check.Status != "fail" || check.Fix == "" {
		t.Fatalf("%+v", check)
	}
}

func TestLimitsValidation(t *testing.T) {
	base := model.Config{}
	for name, limits := range map[string]model.Limits{
		"negative slots":  {BuildSlots: -1},
		"negative disk":   {MinFreeDiskGB: -1},
		"negative idle":   {BuildSlots: 1, SlotIdleMinutes: -1},
		"areas no slots":  {BuildAreas: []string{"app"}},
		"idle no slots":   {SlotIdleMinutes: 5},
		"area not listed": {BuildSlots: 1, BuildAreas: []string{"elsewhere"}},
	} {
		c := base
		c.Areas = []string{"app"}
		c.Limits = limits
		if err := c.ValidateLimits(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := base
	good.Areas = []string{"app"}
	good.Limits = model.Limits{BuildSlots: 2, BuildAreas: []string{"app"}, MinFreeDiskGB: 20, SlotIdleMinutes: 10}
	if err := good.ValidateLimits(); err != nil {
		t.Fatal(err)
	}
	if good.SlotIdleMinutes() != 10 || base.SlotIdleMinutes() != 45 {
		t.Fatal("idle defaults to 45")
	}
	if !good.CountsAsBuild("") || !good.CountsAsBuild("app") || !good.CountsAsBuild("App") || good.CountsAsBuild("Docs") {
		t.Fatal("build areas")
	}
	if !base.CountsAsBuild("docs") {
		t.Fatal("with none listed, everything is a build")
	}
	if good.WithoutRelaxations().Limits.BuildSlots != 2 {
		t.Fatal("limits tighten, so an untrusted config keeps them")
	}
}

func TestLimitsComeFromTheDefaultBranch(t *testing.T) {
	gitIdentity(t)
	_, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	here := model.Config{Limits: model.Limits{BuildSlots: 9}}
	got := enforcedLimits(here, agent, false)
	if !machineEnforced {
		if got.LimitsMachine() {
			t.Fatal("off where locks don't work")
		}
		return
	}
	if got.Limits.BuildSlots != 0 {
		t.Fatalf("the default branch sets none: %+v", got.Limits)
	}
}

func TestWorkSlotOnARealWorktree(t *testing.T) {
	m := testMachine(t)
	gitIdentity(t)
	_, agent := publishedRepo(t, rootConfigPath, repoConfig(t))
	now := time.Now().UTC()
	if err := agent.writeLease(leaseContext{Issue: 1, Holder: "h", Branch: "001/a", Expires: now.Add(time.Hour).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if warnings := recordWorkSlot(limited(1), agent, 1, "", now); warnings != nil {
		t.Fatal(warnings)
	}
	if slots := liveSlots(t, m, now); len(slots) != 1 {
		entries, _ := os.ReadDir(m.slots())
		t.Fatalf("%v, files %v, %+v", slots, entries, agent)
	}
}
