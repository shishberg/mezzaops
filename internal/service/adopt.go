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

// startOffsetTolerance is the slack (seconds) when comparing start offsets;
// gopsutil's boot time jitters by about a second between reads.
const startOffsetTolerance = 2

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
	// Prefer the boot ID, which is immune to clock steps; fall back to the
	// boot time (with tolerance) when either side lacks one.
	sameBootByID := false
	if cur := bootIDFn(); ps.BootID != "" && cur != "" {
		if ps.BootID != cur {
			return false
		}
		sameBootByID = true
	} else if ps.BootTime != 0 {
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
		if err == nil {
			// CreateTime is derived from the wall clock, so a clock step
			// on the same boot changes it. When the boot is known to be
			// the same and an offset since boot was recorded, compare that.
			if offsetVerification && sameBootByID && ps.StartOffset != 0 {
				if bt, berr := host.BootTime(); berr == nil {
					d := createTime/1000 - int64(bt) - ps.StartOffset
					if d < 0 {
						d = -d
					}
					return d <= startOffsetTolerance
				}
			}
			if createTime != ps.CreateTime {
				return false
			}
		}
	}
	return true
}
