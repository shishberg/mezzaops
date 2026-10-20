package service

import (
	"fmt"
	"syscall"

	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/process"
)

// IsAlive checks whether a process with the given PID exists.
// Returns true even if the process is owned by another user (EPERM).
func IsAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// identity is the result of comparing a persisted process record with the
// live process.
type identity int

const (
	identityMismatch   identity = iota // known to be a different process
	identityVerified                   // create time recorded and matching
	identityUnverified                 // nothing recorded to compare
)

// checkIdentity compares ps with the live process, doing at most one
// process lookup. A boot time that differs from the current one means the
// process is gone (reboot); otherwise a recorded create time must match.
// For identityUnverified, why says whether nothing was recorded or the
// lookup failed.
func checkIdentity(ps processBackendState) (id identity, why string) {
	if ps.BootTime != 0 {
		bootTime, err := host.BootTime()
		if err == nil && int64(bootTime) != ps.BootTime {
			return identityMismatch, ""
		}
	}
	if ps.CreateTime == 0 {
		return identityUnverified, "no create time recorded"
	}
	p, err := process.NewProcess(int32(ps.PID))
	if err != nil {
		return identityMismatch, ""
	}
	createTime, err := p.CreateTime()
	if err != nil {
		return identityUnverified, fmt.Sprintf("create-time lookup failed: %v", err)
	}
	if createTime != ps.CreateTime {
		return identityMismatch, ""
	}
	return identityVerified, ""
}

// VerifyProcess checks that the PID in the state still refers to the same
// process we originally started, by comparing boot time and process create time.
// If both identity fields are zero (e.g. gopsutil failed during start),
// returns true as graceful degradation -- we accept the PID rather than
// incorrectly restarting a process we can't identify.
func VerifyProcess(ps processBackendState) bool {
	id, _ := checkIdentity(ps)
	return id != identityMismatch
}
