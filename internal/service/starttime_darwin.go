package service

import "github.com/shirou/gopsutil/v3/process"

// processStartID returns the process creation time in milliseconds. On
// macOS this is the kernel's absolute start time, which is not affected by
// wall-clock steps, so an exact comparison is correct.
func processStartID(pid int) (int64, error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, err
	}
	return p.CreateTime()
}
