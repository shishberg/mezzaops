//go:build windows || solaris || aix

package service

import "os"

// On Windows, Solaris, AIX (and any other OS without syscall.Flock),
// the C5 single-instance lock is a no-op: the file handle is returned
// as nil and releaseStateLock is a no-op. The Linux build is the one
// that matters in the spec's production deployment (the operators run
// mezzaops under systemd on Linux). Solaris/AIX would need an
// fcntl-based implementation; Windows would need a named-mutex
// implementation. Both are tracked as a follow-up if those platforms
// become supported.

func acquireStateLock(stateDir string) (*os.File, error) {
	return nil, nil
}

func releaseStateLock(f *os.File) {}
