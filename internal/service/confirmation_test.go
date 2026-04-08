package service

import (
	"testing"
	"time"
)

func TestConfirmationTracker_AddAndConfirm(t *testing.T) {
	ct := NewConfirmationTracker(5 * time.Minute)
	ct.AddPending("svc", "main")

	if !ct.Confirm("svc") {
		t.Fatal("Confirm should return true after AddPending")
	}

	// Second confirm should return false (entry consumed)
	if ct.Confirm("svc") {
		t.Fatal("Confirm should return false after already confirmed")
	}
}

func TestConfirmationTracker_ConfirmWithoutPending(t *testing.T) {
	ct := NewConfirmationTracker(5 * time.Minute)

	if ct.Confirm("svc") {
		t.Fatal("Confirm should return false when nothing is pending")
	}
}

func TestConfirmationTracker_ExpiredEntry(t *testing.T) {
	ct := NewConfirmationTracker(1 * time.Millisecond)
	ct.AddPending("svc", "main")

	time.Sleep(5 * time.Millisecond)

	if ct.Confirm("svc") {
		t.Fatal("Confirm should return false for expired entry")
	}
}

func TestConfirmationTracker_IsPending(t *testing.T) {
	ct := NewConfirmationTracker(5 * time.Minute)

	if ct.IsPending("svc") {
		t.Fatal("IsPending should return false with no entry")
	}

	ct.AddPending("svc", "main")

	if !ct.IsPending("svc") {
		t.Fatal("IsPending should return true after AddPending")
	}

	// IsPending should not consume the entry
	if !ct.IsPending("svc") {
		t.Fatal("IsPending should still return true (not consumed)")
	}
}

func TestConfirmationTracker_IsPendingExpired(t *testing.T) {
	ct := NewConfirmationTracker(1 * time.Millisecond)
	ct.AddPending("svc", "main")

	time.Sleep(5 * time.Millisecond)

	if ct.IsPending("svc") {
		t.Fatal("IsPending should return false for expired entry")
	}
}

func TestConfirmationTracker_OverwritesPending(t *testing.T) {
	ct := NewConfirmationTracker(5 * time.Minute)
	ct.AddPending("svc", "branch-1")
	ct.AddPending("svc", "branch-2")

	// Should still confirm (latest write wins)
	if !ct.Confirm("svc") {
		t.Fatal("Confirm should return true after overwrite")
	}
}
