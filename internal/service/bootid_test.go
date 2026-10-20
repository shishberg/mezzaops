package service

import (
	"bytes"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// resetBootIDCache forgets the cached boot ID.
func resetBootIDCache() {
	bootIDOnce = sync.Once{}
	bootIDValue = ""
}

func TestBootID_ReadOnceEvenWhenEmpty(t *testing.T) {
	var calls atomic.Int32
	orig := bootIDRead
	bootIDRead = func() string { calls.Add(1); return "" } // unavailable
	resetBootIDCache()
	t.Cleanup(func() { bootIDRead = orig; resetBootIDCache() })

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	for range 3 {
		if got := cachedBootID(); got != "" {
			t.Fatalf("got %q", got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("boot ID read %d times, want once even when empty", calls.Load())
	}
	if n := strings.Count(buf.String(), "boot ID"); n != 1 {
		t.Fatalf("an empty boot ID should be logged once, got %d:\n%s", n, buf.String())
	}
}

func TestProcessStartID_RealLookupIsStable(t *testing.T) {
	a, err := processStartID(os.Getpid())
	if err != nil || a == 0 {
		t.Fatalf("processStartID: %d, %v", a, err)
	}
	b, err := processStartID(os.Getpid())
	if err != nil || a != b {
		t.Fatalf("start ID must be stable: %d then %d (%v)", a, b, err)
	}
	if _, err := processStartID(99999999); err == nil {
		t.Fatal("a missing process must be a lookup error")
	}
}
