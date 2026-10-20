package service

import (
	"log"
	"sync"
)

// bootIDRead reads the boot ID from the platform; tests may replace it.
var bootIDRead = bootID

var (
	bootIDMu     sync.Mutex // guards bootIDLoaded and bootIDValue
	bootIDLoaded bool
	bootIDValue  string
)

// cachedBootID returns the platform boot ID, read once per process (even
// when unavailable, in which case it is logged once). The Manager and the
// process records all use this value.
func cachedBootID() string {
	bootIDMu.Lock()
	defer bootIDMu.Unlock()
	if !bootIDLoaded {
		bootIDValue = bootIDRead()
		bootIDLoaded = true
		if bootIDValue == "" {
			log.Printf("cannot read the host boot ID; adopt: false services that have run before will not be autostarted")
		}
	}
	return bootIDValue
}

// bootIDFn returns the current boot ID. A variable so tests can substitute
// it. Unlike the wall-clock-derived boot time, the ID does not change when
// the clock is stepped (for example by NTP after boot).
var bootIDFn = cachedBootID
