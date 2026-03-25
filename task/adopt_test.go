package task

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
