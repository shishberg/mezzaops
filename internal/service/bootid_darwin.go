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

// offsetVerification is off on macOS: CreateTime is the kernel's absolute
// process start time and does not move when the clock is stepped, whereas
// kern.boottime is adjusted, so an offset would be wrong. The exact
// CreateTime comparison is used instead.
var offsetVerification = false
