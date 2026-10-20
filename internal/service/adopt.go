package service

import (
	"syscall"
	"time"

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

// createTimeJitter is how far CreateTime may differ and still be the same
// process: on Linux it is derived from a boot time that gopsutil recomputes
// (now minus uptime) and so wobbles by about a second between reads.
const createTimeJitter = 2 * time.Second

// clockStepMinimum is how far CreateTime must differ before a matching start
// offset is taken as evidence of a clock step rather than a different process.
const clockStepMinimum = 60 * time.Second

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
		delta := createTime - ps.CreateTime
		if delta < 0 {
			delta = -delta
		}
		if err == nil && delta > createTimeJitter.Milliseconds() {
			// A CreateTime match (within gopsutil's boot-time jitter) is
			// accepted. A larger mismatch is normally PID reuse. The exception is a wall-clock step on
			// the same boot (Linux, see offsetVerification): CreateTime
			// moves by the size of the step but the start offset since
			// boot does not. The offset alone is ambiguous for a process
			// that merely started a few seconds apart, so it is trusted
			// only when CreateTime differs by more than clockStepMinimum.
			if !offsetVerification || !sameBootByID || ps.StartOffset == 0 {
				return false
			}
			if delta <= clockStepMinimum.Milliseconds() {
				return false
			}
			bt, berr := host.BootTime()
			if berr != nil {
				return false
			}
			d := createTime/1000 - int64(bt) - ps.StartOffset
			if d < 0 {
				d = -d
			}
			return d <= startOffsetTolerance
		}
	}
	return true
}
