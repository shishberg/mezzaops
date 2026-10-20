package service

import (
	"os"
	"testing"

	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/process"
)

func TestIsAliveCurrentProcess(t *testing.T) {
	pid := os.Getpid()
	if !IsAlive(pid) {
		t.Fatal("current process should be alive")
	}
}

func TestIsAliveDeadProcess(t *testing.T) {
	if IsAlive(99999999) {
		t.Fatal("bogus PID should not be alive")
	}
}

func TestVerifyProcessMatchingState(t *testing.T) {
	pid := os.Getpid()
	ps := processBackendState{
		PID:  pid,
		PGID: pid,
	}
	if bootTime, err := host.BootTime(); err == nil {
		ps.BootTime = int64(bootTime)
	}
	if proc, err := process.NewProcess(int32(pid)); err == nil {
		if ct, err := proc.CreateTime(); err == nil {
			ps.CreateTime = ct
		}
	}
	if !verifiedWith(ps, hostRules(), "") {
		t.Fatal("verifyProcess should verify the current process state")
	}
}

func TestVerifyProcessWrongBootTime(t *testing.T) {
	pid := os.Getpid()
	ps := processBackendState{
		PID:      pid,
		PGID:     pid,
		BootTime: 1, // impossibly old boot time
	}
	if verifiedWith(ps, hostRules(), "") {
		t.Fatal("verifyProcess should not verify when boot time mismatches")
	}
}

// verifiedWith reports whether a record is positively verified under rules.
func verifiedWith(ps processBackendState, rules platformRules, serviceBootID string) bool {
	result, _ := verifyProcess(ps, rules, serviceBootID)
	return result == verifyVerified
}
