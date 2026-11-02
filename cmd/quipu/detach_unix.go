//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// detach puts cmd in its own session, so killing quipu's process group, or
// closing its terminal, leaves cmd running.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// machineEnforced says whether machine limits work here: they rest on file locks.
const machineEnforced = true

// tryLockFile takes the lock on f if nobody holds it, and reports whether it was
// free.
func tryLockFile(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

// freeDiskBytes is what an unprivileged process can still write at path.
func freeDiskBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

// lockFile holds an exclusive lock on f until f is closed.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}
