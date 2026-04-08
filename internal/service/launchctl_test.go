package service

import (
	"testing"
)

func TestLaunchctlBackend_Interface(t *testing.T) {
	b := NewLaunchctlBackend("com.example.myapp")
	// Verify it implements Backend.
	var _ Backend = b
}

func TestLaunchctlBackend_Label(t *testing.T) {
	b := NewLaunchctlBackend("com.example.myapp")
	if b.label != "com.example.myapp" {
		t.Fatalf("label: got %q, want %q", b.label, "com.example.myapp")
	}
}
