package service

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// childPIDs returns the direct children of the test process.
func childPIDs() map[int]bool {
	out, _ := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid())).Output()
	pids := map[int]bool{}
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids[pid] = true
		}
	}
	return pids
}

// killChildren kills (with their process groups) the test process's children
// that are not in keep. Managers and backends started by tests do not stop
// their service processes when a test ends; this keeps a run from leaving
// them behind.
func killChildren(keep map[int]bool) {
	for pid := range childPIDs() {
		if keep[pid] {
			continue
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// cleanupChildren registers a cleanup that kills children started during the
// test. Tests here do not run in parallel, so children can be attributed by
// diffing before and after.
func cleanupChildren(t *testing.T) {
	t.Helper()
	before := childPIDs()
	t.Cleanup(func() { killChildren(before) })
}

// TestMain sweeps anything a test still left running at the end of the run.
func TestMain(m *testing.M) {
	code := m.Run()
	killChildren(nil)
	os.Exit(code)
}
