//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// detach puts cmd in its own session, so killing quipu's process group, or
// closing its terminal, leaves cmd running.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// lockFile holds an exclusive lock on f until f is closed.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}
