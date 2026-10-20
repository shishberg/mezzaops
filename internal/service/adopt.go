package service

import (
	"errors"
	"io/fs"
	"sync/atomic"
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
	// verifyVerified: the same process, or a record with no identity fields
	// at all (accepted as graceful degradation, as on main).
	verifyVerified
	// verifyUnknown: the pid is alive but no per-process comparison could be
	// made (a lookup failed, or the record offers nothing to compare).
	verifyUnknown
)

// isGone reports whether err means the process no longer exists.
func isGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, process.ErrorProcessNotRunning)
}

// verifyProcessResult checks that the PID in the state still refers to the
// same process we originally started. The steps, in order:
//
//  1. A non-positive pid, or one that is not alive, is a mismatch.
//  2. A record with no identity fields at all (BootID, StartID, BootTime and
//     CreateTime all zero) is verified, as on main.
//  3. The boot-level check can only rule a process out. If the record's boot
//     ID and the current boot ID are both known and differ, it is a mismatch.
//     If they are not both known, a recorded BootTime that differs (beyond
//     sameBoot's tolerance) from a readable current boot time is a mismatch.
//     (With matching boot IDs the boot time is not consulted: it is derived
//     from the wall clock and moves when the clock is stepped.)
//  4. If the record has a StartID and the boot IDs are both known and equal,
//     the start ID (start ticks since boot on Linux, creation time on macOS;
//     exact and clock-independent) is looked up: equal is verified, different
//     is a mismatch. If the lookup fails, a process that has exited is a
//     mismatch. Otherwise it is unknown on macOS (the CreateTime lookup would
//     be the same call); on Linux the CreateTime check is tried, but because
//     CreateTime moves when the clock is stepped it can then only verify the
//     process, never reject it (a difference is unknown).
//  5. If the record has a CreateTime it is looked up and compared with
//     createTimeMatches (exact on macOS, legacyCreateTimeTolerance on Linux,
//     where gopsutil recomputes the boot time and CreateTime wobbles by about a
//     second). A match is verified; a difference is a mismatch, except after a
//     failed start ID lookup (see step 4); a failed lookup is a mismatch if
//     the process has exited and unknown if it is alive.
//  6. Otherwise no per-process comparison was possible: unknown.
func verifyProcessResult(ps processBackendState) verifyResult {
	if ps.PID <= 0 || !isAliveFn(ps.PID) {
		return verifyMismatch
	}
	if ps.BootID == "" && ps.StartID == 0 && ps.BootTime == 0 && ps.CreateTime == 0 {
		return verifyVerified
	}

	cur := bootIDFn()
	sameBootID := false
	if ps.BootID != "" && cur != "" {
		if ps.BootID != cur {
			return verifyMismatch
		}
		sameBootID = true
	} else if ps.BootTime != 0 {
		if bt, err := bootTimeFn(); err == nil && !sameBoot(bt, ps.BootTime) {
			return verifyMismatch
		}
	}

	startIDApplied := false
	if ps.StartID != 0 && sameBootID {
		startIDApplied = true
		id, err := processStartIDFn(ps.PID)
		if err == nil {
			if id == ps.StartID {
				return verifyVerified
			}
			return verifyMismatch
		}
		if goneAfter(ps.PID, err) {
			return verifyMismatch
		}
		// Alive, but the start ID could not be read.
		if startIDIsCreateTime {
			// The CreateTime lookup would be the same call.
			return verifyUnknown
		}
		// On Linux CreateTime moves when the clock is stepped, so below it
		// can confirm the process but never reject it.
	}

	if ps.CreateTime != 0 {
		live, err := createTimeFn(ps.PID)
		if err != nil {
			if goneAfter(ps.PID, err) {
				return verifyMismatch
			}
			return verifyUnknown
		}
		if createTimeMatches(live, ps.CreateTime, legacyCreateTimeTolerance) {
			return verifyVerified
		}
		if startIDApplied {
			return verifyUnknown
		}
		return verifyMismatch
	}
	return verifyUnknown
}

// goneAfter reports whether a failed lookup means the process no longer
// exists: the error says so, or the pid is no longer alive. Otherwise the
// process is alive and its identity simply could not be read.
func goneAfter(pid int, err error) bool {
	return isGone(err) || !isAliveFn(pid)
}

// createTimeMatches reports whether two creation times (ms) are within tol of
// each other. The tolerance is a parameter so both platform values can be
// tested directly.
func createTimeMatches(live, recorded, tol int64) bool {
	d := live - recorded
	if d < 0 {
		d = -d
	}
	return d <= tol
}

// Seams for the platform lookups, so tests can drive verification
// deterministically.
var (
	// createTimeFn returns the process creation time in milliseconds.
	createTimeFn = func(pid int) (int64, error) {
		p, err := process.NewProcess(int32(pid))
		if err != nil {
			return 0, err
		}
		return p.CreateTime()
	}
	// bootTimeFn returns the host boot time in unix seconds.
	bootTimeFn = func() (int64, error) {
		bt, err := host.BootTime()
		return int64(bt), err
	}
)

// isAliveOverride replaces IsAlive in tests. It is atomic because adopted
// processes are polled from background goroutines that outlive a test.
var isAliveOverride atomic.Pointer[func(pid int) bool]

// isAliveFn reports whether a pid exists (IsAlive, unless a test overrides it).
func isAliveFn(pid int) bool {
	if f := isAliveOverride.Load(); f != nil {
		return (*f)(pid)
	}
	return IsAlive(pid)
}

// VerifyProcess reports whether the record is positively the live process.
// An unknown result is not verified (cleanOrphans, for one, must not kill on
// it).
func VerifyProcess(ps processBackendState) bool {
	return verifyProcessResult(ps) == verifyVerified
}
