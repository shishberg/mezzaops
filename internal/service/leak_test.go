package service

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// childPIDs returns the direct children of the test process. pgrep exits 1
// when there are none; any other failure (for example pgrep missing) is
// returned so callers can report that children could not be listed.
func childPIDs() (map[int]bool, error) {
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
			return map[int]bool{}, err
		}
	}
	pids := map[int]bool{}
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids[pid] = true
		}
	}
	return pids, nil
}

// killChildren kills (with their process groups) the test process's children
// that are not in keep. Managers and backends started by tests do not stop
// their service processes when a test ends; this keeps a run from leaving
// them behind.
//
// A pid listed by pgrep could in theory exit and be reused before the kill;
// that race is accepted in a test helper.
func killChildren(keep map[int]bool) error {
	children, err := childPIDs()
	if err != nil {
		return err
	}
	for pid := range children {
		if keep[pid] {
			continue
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return nil
}

// cleanupChildren registers a cleanup that kills children started during the
// test. Tests here do not run in parallel, so children can be attributed by
// diffing before and after.
func cleanupChildren(t *testing.T) {
	t.Helper()
	before, err := childPIDs()
	if err != nil {
		t.Logf("cannot list child processes (is pgrep installed?): %v", err)
	}
	t.Cleanup(func() {
		if err := killChildren(before); err != nil {
			t.Logf("cannot clean up child processes: %v", err)
		}
	})
}

// TestMain sweeps anything a test still left running at the end of the run.
func TestMain(m *testing.M) {
	code := m.Run()
	if err := killChildren(nil); err != nil {
		fmt.Fprintf(os.Stderr, "cannot clean up leftover child processes (is pgrep installed?): %v\n", err)
	}
	os.Exit(code)
}
