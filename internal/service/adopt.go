package service

import (
	"errors"
	"io/fs"
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

// verifyResult is the outcome of comparing a persisted process record with
// the live process.
type verifyResult int

const (
	// verifyMismatch: definitely a different process, or gone.
	verifyMismatch verifyResult = iota
	// verifyVerified: the same process (or a record with no identity data
	// to compare, accepted as graceful degradation, as on main).
	verifyVerified
	// verifyUnknown: the pid is alive but a lookup failed, so identity
	// could not be determined.
	verifyUnknown
)

// isGone reports whether err means the process no longer exists.
func isGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, process.ErrorProcessNotRunning)
}

// verifyProcessResult checks that the PID in the state still refers to the
// same process we originally started.
//
// When the record and the host both have a boot ID, they must match and the
// process start ID (start ticks since boot on Linux, creation time on
// macOS) must be identical; this is exact and unaffected by wall-clock
// steps. A legacy record (no boot ID or no start ID) falls back to main's
// approach: boot time within a small tolerance, then a CreateTime
// comparison that is exact on macOS and, on Linux, allows
// legacyCreateTimeTolerance because gopsutil recomputes the Linux boot time
// from /proc/uptime on every read, so CreateTime wobbles by about a second.
// A record with no identity data at all is accepted. A lookup failure for a
// process that still exists is verifyUnknown; a vanished process is a
// mismatch.
func verifyProcessResult(ps processBackendState) verifyResult {
	if cur := bootIDFn(); ps.BootID != "" && cur != "" {
		if ps.BootID != cur {
			return verifyMismatch
		}
		if ps.StartID != 0 {
			id, err := processStartIDFn(ps.PID)
			switch {
			case err == nil && id == ps.StartID:
				return verifyVerified
			case err == nil, isGone(err):
				return verifyMismatch
			default:
				return verifyUnknown
			}
		}
	} else if ps.BootTime != 0 {
		bootTime, err := host.BootTime()
		if err != nil {
			return verifyUnknown
		}
		if !sameBoot(int64(bootTime), ps.BootTime) {
			return verifyMismatch
		}
	}
	if ps.CreateTime != 0 {
		p, err := process.NewProcess(int32(ps.PID))
		if err != nil {
			if isGone(err) {
				return verifyMismatch
			}
			return verifyUnknown
		}
		createTime, err := p.CreateTime()
		if err != nil {
			if isGone(err) {
				return verifyMismatch
			}
			return verifyUnknown
		}
		delta := createTime - ps.CreateTime
		if delta < 0 {
			delta = -delta
		}
		if delta > legacyCreateTimeTolerance {
			return verifyMismatch
		}
	}
	return verifyVerified
}

// VerifyProcess reports whether the record is positively the live process.
// An unknown result is not verified (cleanOrphans, for one, must not kill on
// it).
func VerifyProcess(ps processBackendState) bool {
	return verifyProcessResult(ps) == verifyVerified
}
