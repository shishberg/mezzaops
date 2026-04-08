package service

import (
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

// VerifyProcess checks that the PID in the state still refers to the same
// process we originally started, by comparing boot time and process create time.
// If both identity fields are zero (e.g. gopsutil failed during start),
// returns true as graceful degradation -- we accept the PID rather than
// incorrectly restarting a process we can't identify.
func VerifyProcess(s State) bool {
	if s.BootTime != 0 {
		bootTime, err := host.BootTime()
		if err == nil && int64(bootTime) != s.BootTime {
			return false
		}
	}
	if s.CreateTime != 0 {
		p, err := process.NewProcess(int32(s.PID))
		if err != nil {
			return false
		}
		createTime, err := p.CreateTime()
		if err == nil && createTime != s.CreateTime {
			return false
		}
	}
	return true
}
