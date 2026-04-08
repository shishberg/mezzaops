package service

import (
	"os"
	"testing"
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
	s := RunningState(pid, "/tmp/test.log")
	if !VerifyProcess(s) {
		t.Fatal("VerifyProcess should return true for current process state")
	}
}

func TestVerifyProcessWrongBootTime(t *testing.T) {
	pid := os.Getpid()
	s := RunningState(pid, "/tmp/test.log")
	s.BootTime = 1 // impossibly old boot time
	if VerifyProcess(s) {
		t.Fatal("VerifyProcess should return false when boot time mismatches")
	}
}
