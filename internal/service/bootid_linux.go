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

// legacyCreateTimeTolerance is how far (ms) a legacy record's CreateTime may
// differ from the live one: gopsutil derives the Linux value from a boot time
// it recomputes from /proc/uptime on every read, so it can shift by 1s.
const legacyCreateTimeTolerance = 1000
