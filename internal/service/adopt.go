package service

import (
	"syscall"

	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/process"
)

func init() {
	// gopsutil derives boot time from now minus uptime, truncated to seconds,
	// so it can vary by about a second between calls. Cache the first read.
	host.EnableBootTimeCache(true)
}

// bootTimeTolerance is how far apart two boot times (seconds) may be and
// still count as the same boot, covering gopsutil's uptime-based jitter.
const bootTimeTolerance = 10

// sameBoot reports whether two host boot times (unix seconds) are the same
// boot, allowing for jitter.
func sameBoot(a, b int64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= bootTimeTolerance
}

// IsAlive checks whether a process with the given PID exists.
// Returns true even if the process is owned by another user (EPERM). A
// non-positive pid is never alive.
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false // kill(0, 0) and kill(-1, 0) address whole process groups
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// VerifyProcess checks that the PID in the state still refers to the same
// process we originally started, by comparing boot time and process create time.
// If both identity fields are zero (e.g. gopsutil failed during start),
// returns true as graceful degradation -- we accept the PID rather than
// incorrectly restarting a process we can't identify.
func VerifyProcess(ps processBackendState) bool {
	if ps.BootTime != 0 {
		bootTime, err := host.BootTime()
		if err == nil && !sameBoot(int64(bootTime), ps.BootTime) {
			return false
		}
	}
	if ps.CreateTime != 0 {
		p, err := process.NewProcess(int32(ps.PID))
		if err != nil {
			return false
		}
		createTime, err := p.CreateTime()
		if err == nil && createTime != ps.CreateTime {
			return false
		}
	}
	return true
}
