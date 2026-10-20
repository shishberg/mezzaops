package service

import (
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

// mismatchReason says why verifyMismatch was returned.
type mismatchReason int

const (
	reasonNone             mismatchReason = iota
	reasonBadPID                          // pid <= 0
	reasonDead                            // the process is gone
	reasonDifferentBoot                   // recorded under a different boot
	reasonDifferentProcess                // alive, but not the recorded process
)

// platformRules are the per-platform verification rules, as named fields. The
// struct accepts any combination of its fields; only rulesLinux() and
// rulesDarwin() are used, and being functions they cannot be reassigned.
// Production uses hostRules(); tests pass either value explicitly.
type platformRules struct {
	// startIDIsCreateTime: the start ID is the creation time, so a failed
	// start ID lookup leaves nothing more to try and a record that already
	// has CreateTime need not look the process up again.
	startIDIsCreateTime bool
	// createTimeCanReject: a CreateTime difference proves a different
	// process. On Linux it does not: a clock step moves CreateTime.
	createTimeCanReject bool
	// bootTimeCanReject: a BootTime difference (no boot ID available) proves
	// a different boot. On Linux it does not: BootTime is now minus uptime,
	// so a clock step moves it.
	bootTimeCanReject bool
	// createTimeTolerance is how far (ms) two CreateTime values may differ
	// and still match. On Linux gopsutil recomputes the boot time from
	// /proc/uptime on every read, so CreateTime wobbles by about a second;
	// on macOS it is the kernel's absolute start time and compared exactly.
	createTimeTolerance int64
}

// rulesLinux: CreateTime and BootTime are derived from the wall clock, and
// CreateTime wobbles by about a second.
func rulesLinux() platformRules {
	return platformRules{createTimeTolerance: 1000}
}

// rulesDarwin: the start ID is CreateTime, and CreateTime and BootTime are
// absolute kernel values compared exactly.
func rulesDarwin() platformRules {
	return platformRules{startIDIsCreateTime: true, createTimeCanReject: true, bootTimeCanReject: true}
}

// verifyProcess checks that the PID in the state still refers to the same
// process we originally started. rules are the platform's verification rules
// (hostRules() in production); serviceBootID is the boot ID in the service state
// file, or "" if unknown. The steps, in order:
//
//  1. A non-positive pid, or one that is not alive, is a mismatch.
//  2. A record with no identity fields at all (BootID, StartID, BootTime and
//     CreateTime all zero) is verified, as on main.
//  3. The boot-level check can only rule a process out. If the record has no
//     boot ID of its own, a serviceBootID that differs from the current boot
//     ID is a mismatch (the file was written under another boot, so no
//     process recorded in it can be alive); a matching one proves nothing. If the record's boot
//     ID and the current boot ID are both known and differ, it is a mismatch.
//     If they are not both known, a recorded BootTime that differs (beyond
//     sameBoot's tolerance) from a readable current boot time is a mismatch
//     where bootTimeCanReject (macOS) and unknown on Linux, where BootTime
//     moves with the wall clock. (With matching boot IDs the boot time is not
//     consulted.)
//  4. If the record has a StartID and the boot IDs are both known and equal,
//     the start ID (start ticks since boot on Linux, creation time on macOS;
//     exact and clock-independent) is looked up: equal is verified, different
//     is a mismatch. If the lookup fails, a process that is no longer alive is
//     a mismatch; otherwise it is unknown on macOS (startIDIsCreateTime: the
//     CreateTime lookup would be the same call) and on Linux step 5 is tried.
//  5. If the record has a CreateTime it is looked up and compared with
//     createTimeMatches (rules.createTimeTolerance: exact on macOS, about a
//     second on Linux).
//     A match is verified. A difference is a mismatch only where
//     createTimeCanReject (macOS, where CreateTime is absolute); on Linux
//     CreateTime moves when the clock is stepped, so a difference is unknown.
//     A failed lookup is a mismatch if the process has exited and unknown if
//     it is alive.
//  6. Otherwise no per-process comparison was possible: unknown.
//
// Trade-off: on Linux a legacy record (no start ID) whose pid was reused on
// the same boot is unknown, not a mismatch, so the service is not autostarted
// that run (and this is logged) rather than risking a duplicate. Records with
// a start ID detect pid reuse exactly.
func verifyProcess(
	ps processBackendState, rules platformRules, serviceBootID string,
) (verifyResult, mismatchReason) {
	if ps.PID <= 0 {
		return verifyMismatch, reasonBadPID
	}
	if !isAliveFn(ps.PID) {
		return verifyMismatch, reasonDead
	}
	if ps.BootID == "" && ps.StartID == 0 && ps.BootTime == 0 && ps.CreateTime == 0 {
		return verifyVerified, reasonNone
	}

	cur := bootIDFn()
	sameBootID := false // the record's own boot ID matches (needed for the start ID)
	knownSameBoot := false
	if cur != "" {
		switch {
		case ps.BootID != "":
			if ps.BootID != cur {
				return verifyMismatch, reasonDifferentBoot
			}
			sameBootID, knownSameBoot = true, true
		case serviceBootID != "" && serviceBootID != cur:
			// The service state file's boot ID is the boot during which the
			// file was last written, and any process recorded in it was
			// started at or before that point. If it differs from the
			// current boot, that process cannot be alive. A match proves
			// nothing about the record (the file may have been re-stamped
			// without rewriting it), so it is never used to confirm a boot.
			return verifyMismatch, reasonDifferentBoot
		}
	}
	if !knownSameBoot && ps.BootTime != 0 {
		if bt, err := bootTimeFn(); err == nil && !sameBoot(bt, ps.BootTime) {
			if rules.bootTimeCanReject {
				return verifyMismatch, reasonDifferentBoot
			}
			// Linux: BootTime moves with the wall clock, so the same boot
			// can look different after a clock step.
			return verifyUnknown, reasonNone
		}
	}

	if ps.StartID != 0 && sameBootID {
		id, err := processStartIDFn(ps.PID)
		if err == nil {
			if id == ps.StartID {
				return verifyVerified, reasonNone
			}
			return verifyMismatch, reasonDifferentProcess
		}
		if goneAfter(ps.PID) {
			return verifyMismatch, reasonDead
		}
		if rules.startIDIsCreateTime {
			return verifyUnknown, reasonNone
		}
	}

	if ps.CreateTime != 0 {
		return checkCreateTime(ps, rules)
	}
	return verifyUnknown, reasonNone
}

// checkCreateTime compares the record's CreateTime with the live one.
func checkCreateTime(ps processBackendState, rules platformRules) (verifyResult, mismatchReason) {
	live, err := createTimeFn(ps.PID)
	if err != nil {
		if goneAfter(ps.PID) {
			return verifyMismatch, reasonDead
		}
		return verifyUnknown, reasonNone
	}
	switch {
	case createTimeMatches(live, ps.CreateTime, rules.createTimeTolerance):
		return verifyVerified, reasonNone
	case rules.createTimeCanReject:
		return verifyMismatch, reasonDifferentProcess
	default:
		return verifyUnknown, reasonNone
	}
}

// goneAfter reports whether a failed lookup means the process no longer
// exists. Only "the pid is no longer alive" counts: an error that merely looks
// like "gone" (ENOENT under hidepid or in another pid namespace) for a pid
// that is alive leaves the identity unknown.
func goneAfter(pid int) bool {
	return !isAliveFn(pid)
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
