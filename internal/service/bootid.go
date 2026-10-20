package service

import "sync"

// bootIDRead reads the boot ID from the platform; tests may replace it.
var bootIDRead = bootID

var (
	bootIDOnce  sync.Once
	bootIDValue string
)

// cachedBootID returns the platform boot ID, read once per process.
func cachedBootID() string {
	bootIDOnce.Do(func() { bootIDValue = bootIDRead() })
	return bootIDValue
}

// resetBootIDCache forgets the cached boot ID (for tests).
func resetBootIDCache() {
	bootIDOnce = sync.Once{}
	bootIDValue = ""
}

// bootIDFn returns the current boot ID. A variable so tests can substitute
// it. Unlike the wall-clock-derived boot time, the ID does not change when
// the clock is stepped (for example by NTP after boot).
var bootIDFn = cachedBootID
