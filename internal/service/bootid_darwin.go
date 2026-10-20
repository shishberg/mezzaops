package service

import "golang.org/x/sys/unix"

// bootID returns the kernel's per-boot session UUID, or "" if unavailable.
func bootID() string {
	id, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return ""
	}
	return id
}

// legacyCreateTimeTolerance is 0 on macOS: CreateTime is the kernel's absolute
// start time and is compared exactly.
const legacyCreateTimeTolerance = 0
