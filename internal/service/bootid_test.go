package service

import (
	"sync/atomic"
	"testing"
)

// resetBootIDCache forgets the cached boot ID.
func resetBootIDCache() {
	bootIDMu.Lock()
	bootIDValue = ""
	bootIDMu.Unlock()
}

func TestBootID_FailedReadIsNotCached(t *testing.T) {
	var calls atomic.Int32
	orig := bootIDRead
	bootIDRead = func() string {
		if calls.Add(1) == 1 {
			return "" // first read fails
		}
		return "id-1"
	}
	resetBootIDCache()
	t.Cleanup(func() { bootIDRead = orig; resetBootIDCache() })

	if got := cachedBootID(); got != "" {
		t.Fatalf("first read: got %q", got)
	}
	if got := cachedBootID(); got != "id-1" {
		t.Fatalf("a failed read must be retried, got %q", got)
	}
	cachedBootID()
	if calls.Load() != 2 {
		t.Fatalf("a successful read must be cached: %d reads", calls.Load())
	}
}
