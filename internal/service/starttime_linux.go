package service

import (
	"fmt"
	"os"
)

// processStartID returns the process start time in clock ticks since boot.
// Combined with the boot ID it identifies a process exactly and does not
// move when the wall clock is stepped.
func processStartID(pid int) (int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	return parseStartTicks(string(data))
}

// startIDIsCreateTime reports whether the start ID is the creation time, so a
// record that already has CreateTime need not look the process up again.
const startIDIsCreateTime = false

// createTimeCanReject: on Linux CreateTime is derived from the wall clock, so a
// clock step on the same boot moves it for the very same process. It can
// confirm a process but a difference proves nothing.
const createTimeCanReject = false
