package service

import (
	"testing"
)

func TestSystemctlBackend_Interface(t *testing.T) {
	b := NewSystemctlBackend("myapp.service", false)
	// Verify it implements Backend.
	var _ Backend = b
}

func TestSystemctlBackend_Fields(t *testing.T) {
	b := NewSystemctlBackend("myapp.service", true)
	if b.unit != "myapp.service" {
		t.Fatalf("unit: got %q, want %q", b.unit, "myapp.service")
	}
	if !b.userMode {
		t.Fatal("userMode should be true")
	}
}
