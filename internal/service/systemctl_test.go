package service

import (
	"testing"
)

func TestSystemctlBackend_Interface(t *testing.T) {
	b := NewSystemctlBackend("myapp.service", false, false)
	// Verify it implements Backend.
	var _ Backend = b
}

func TestSystemctlBackend_Fields(t *testing.T) {
	b := NewSystemctlBackend("myapp.service", true, false)
	if b.unit != "myapp.service" {
		t.Fatalf("unit: got %q, want %q", b.unit, "myapp.service")
	}
	if !b.userMode {
		t.Fatal("userMode should be true")
	}
	if b.sudo {
		t.Fatal("sudo should be false")
	}
}

func TestSystemctlBackend_Sudo(t *testing.T) {
	b := NewSystemctlBackend("myapp.service", false, true)
	if b.userMode {
		t.Fatal("userMode should be false")
	}
	if !b.sudo {
		t.Fatal("sudo should be true")
	}
}
