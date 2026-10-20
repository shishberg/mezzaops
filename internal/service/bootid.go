package service

import "sync"

// bootIDRead reads the boot ID from the platform; tests may replace it.
var bootIDRead = bootID

var (
	bootIDMu    sync.Mutex
	bootIDValue string
)

// cachedBootID returns the platform boot ID. A successful (non-empty) read
// is cached for the life of the process; a failed read is retried next time.
func cachedBootID() string {
	bootIDMu.Lock()
	defer bootIDMu.Unlock()
	if bootIDValue == "" {
		bootIDValue = bootIDRead()
	}
	return bootIDValue
}

// bootIDFn returns the current boot ID. A variable so tests can substitute
// it. Unlike the wall-clock-derived boot time, the ID does not change when
// the clock is stepped (for example by NTP after boot).
var bootIDFn = cachedBootID
