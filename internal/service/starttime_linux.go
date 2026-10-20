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
