package service

// processStartID returns the process creation time in milliseconds. On
// macOS this is the kernel's absolute start time, which is not affected by
// wall-clock steps, so an exact comparison is correct. It uses the same
// lookup as createTimeFn so the two cannot diverge.
func processStartID(pid int) (int64, error) {
	return createTimeFn(pid)
}

// startIDIsCreateTime reports whether the start ID is the creation time, so a
// record that already has CreateTime need not look the process up again.
const startIDIsCreateTime = true

// createTimeCanReject: on macOS CreateTime is the kernel's absolute start
// time, so a difference means a different process.
const createTimeCanReject = true
