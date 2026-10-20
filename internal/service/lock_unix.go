//go:build linux || darwin

package service

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireStateLock takes an exclusive advisory lock on a file inside
// stateDir. Returns the locked file handle (so the lock is held for the
// lifetime of the Manager and the OS releases it on close / process exit)
// or an error if another live instance holds it.
//
// We use LOCK_EX | LOCK_NB so a second instance fails fast instead of
// blocking. The lock is per-process: when the holder exits, the kernel
// releases the lock automatically, so a crashed instance does not leave
// a stale lock that needs manual cleanup.
//
// Only Linux and macOS are supported platforms; on any other OS the
// package intentionally fails to build rather than skipping the lock.
func acquireStateLock(stateDir string) (*os.File, error) {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, fmt.Errorf("creating state dir for lock: %w", err)
	}
	lockPath := filepath.Join(stateDir, "mezzaops.lock")
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening state lock %q: %w", lockPath, err)
	}

	// Exclusive, non-blocking. flock(2) returns EWOULDBLOCK if another
	// process holds the lock.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("another mezzaops instance already holds the state lock at %q", lockPath)
		}
		return nil, fmt.Errorf("acquiring state lock: %w", err)
	}

	// Truncate so an existing file with stale contents is not read as
	// a "this PID owns the lock" by a human inspecting the file.
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("truncating state lock file: %w", err)
	}
	if _, err := fmt.Fprintf(f, "pid=%d\n", os.Getpid()); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("writing state lock file: %w", err)
	}
	return f, nil
}

// releaseStateLock releases the exclusive lock on the file handle. Safe
// to call with a nil handle.
func releaseStateLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
