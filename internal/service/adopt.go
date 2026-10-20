package service

import (
	"syscall"

	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/process"
)

// bootTimeTolerance is how far apart two boot times (seconds) may be and
// still count as the same boot, covering gopsutil's uptime-based jitter. It
// is only the fallback when no boot ID is available on both sides.
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
// process we originally started.
//
// When the record and the host both have a boot ID, they must match and the
// process start ID (start ticks since boot on Linux, creation time on
// macOS) must be identical; this is exact and unaffected by wall-clock
// steps. A legacy record (no boot ID or no start ID) falls back to the
// original check: boot time within a small tolerance, then an exact
// CreateTime comparison. A record with no identity data at all is accepted
// (graceful degradation when gopsutil failed during start). Any lookup error
// means the process is not verified.
func VerifyProcess(ps processBackendState) bool {
	if cur := bootIDFn(); ps.BootID != "" && cur != "" {
		if ps.BootID != cur {
			return false
		}
		if ps.StartID != 0 {
			id, err := processStartIDFn(ps.PID)
			return err == nil && id == ps.StartID
		}
	} else if ps.BootTime != 0 {
		bootTime, err := host.BootTime()
		if err != nil || !sameBoot(int64(bootTime), ps.BootTime) {
			return false
		}
	}
	if ps.CreateTime != 0 {
		p, err := process.NewProcess(int32(ps.PID))
		if err != nil {
			return false
		}
		createTime, err := p.CreateTime()
		return err == nil && createTime == ps.CreateTime
	}
	return true
}
