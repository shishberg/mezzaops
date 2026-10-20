package service

import (
	"os"
	"strings"
)

// bootID returns the kernel's per-boot UUID, or "" if unavailable.
func bootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// offsetVerification enables verifying a process by its start offset since
// boot when boot IDs match. On Linux gopsutil derives CreateTime and BootTime
// from the same wall-clock-minus-uptime value, so the offset is unchanged by
// a clock step while CreateTime itself is not.
var offsetVerification = true
