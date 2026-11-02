package main

import (
	"errors"
	"os"
	"os/exec"
)

// machineEnforced says whether machine limits work here. Without file locks two
// sessions could both take the last slot, so here limits don't apply.
const machineEnforced = false

func tryLockFile(*os.File) (bool, error) { return false, errors.New("no file locks here") }

func freeDiskBytes(string) (uint64, error) { return 0, errors.New("not supported here") }

// detach does nothing here: Windows does not signal a process group on exit.
func detach(*exec.Cmd) {}

// lockFile does nothing here, so two agents starting at once may both run
// runner_start.
func lockFile(*os.File) error { return nil }
