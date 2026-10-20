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
