package service

// processStartID returns the process creation time in milliseconds. On
// macOS this is the kernel's absolute start time, which is not affected by
// wall-clock steps, so an exact comparison is correct. It uses the same
// lookup as createTimeFn so the two cannot diverge.
func processStartID(pid int) (int64, error) {
	return createTimeFn(pid)
}

// hostRules are the verification rules for this platform: on macOS the start
// ID is the creation time, and CreateTime and the boot time are absolute
// kernel values, so a difference means a different process or boot.
func hostRules() platformRules { return rulesDarwin() }
