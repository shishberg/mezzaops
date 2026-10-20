package service

// bootIDFn returns the current boot ID. A variable so tests can substitute
// it. Unlike the wall-clock-derived boot time, the ID does not change when
// the clock is stepped (for example by NTP after boot).
var bootIDFn = bootID
