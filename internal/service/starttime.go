package service

import (
	"fmt"
	"strconv"
	"strings"
)

// processStartIDFn returns an exact process start identity that is
// independent of the wall clock where the platform allows (see
// processStartID). A variable so tests can substitute it.
var processStartIDFn = processStartID

// parseStartTicks extracts the process start time, in clock ticks since boot
// (field 22), from the contents of /proc/<pid>/stat. The command name in
// field 2 is parenthesised and may itself contain spaces and parentheses, so
// fields are counted from after the last ')'.
func parseStartTicks(stat string) (int64, error) {
	end := strings.LastIndex(stat, ")")
	if end < 0 {
		return 0, fmt.Errorf("malformed stat: no ')'")
	}
	// After the ')' the fields start at field 3 (state), so field 22 is
	// index 19.
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 20 {
		return 0, fmt.Errorf("malformed stat: %d fields after comm, need 20", len(fields))
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed stat start time: %w", err)
	}
	return ticks, nil
}
