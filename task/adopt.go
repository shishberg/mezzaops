package task

import (
	"syscall"

	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/process"
)

func IsAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil
}

func VerifyProcess(s State) bool {
	if s.BootTime != 0 {
		bootTime, err := host.BootTime()
		if err == nil && int64(bootTime) != s.BootTime {
			return false
		}
	}
	if s.CreateTime != 0 {
		p, err := process.NewProcess(int32(s.PID))
		if err != nil {
			return false
		}
		createTime, err := p.CreateTime()
		if err == nil && createTime != s.CreateTime {
			return false
		}
	}
	return true
}
