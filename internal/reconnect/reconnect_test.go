package reconnect

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunWithBackoff_SuccessFirstAttempt(t *testing.T) {
	var attempts atomic.Int32
	err := RunWithBackoff(context.Background(), "test", Options{}, func(ctx context.Context) error {
		attempts.Add(1)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), attempts.Load())
}

func TestRunWithBackoff_RetriesUntilSuccess(t *testing.T) {
	var attempts atomic.Int32
	err := RunWithBackoff(context.Background(), "test", Options{
		InitialDelay: 1 * time.Millisecond,
		MaxDelay:     5 * time.Millisecond,
	}, func(ctx context.Context) error {
		n := attempts.Add(1)
		if n < 3 {
			return errors.New("transient")
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, int32(3), attempts.Load(), "should have retried until 3rd attempt succeeded")
}

func TestRunWithBackoff_CtxCancelDuringBackoff(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after the first failure is logged, while the helper sleeps.
	go func() {
		// Wait for at least one attempt to start; then cancel.
		for attempts.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	err := RunWithBackoff(ctx, "test", Options{
		InitialDelay: 500 * time.Millisecond,
		MaxDelay:     time.Second,
	}, func(ctx context.Context) error {
		attempts.Add(1)
		return errors.New("transient")
	})
	assert.ErrorIs(t, err, context.Canceled)
	// The first attempt ran, then we cancelled during its backoff.
	assert.LessOrEqual(t, attempts.Load(), int32(2),
		"attempts should not have continued after cancel")
}

func TestRunWithBackoff_CtxCancelDuringOp(t *testing.T) {
	// Op blocks until ctx is cancelled. The helper should exit promptly
	// without busy-looping once it sees ctx.Err() between attempts.
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- RunWithBackoff(ctx, "test", Options{}, func(ctx context.Context) error {
			attempts.Add(1)
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	// Let the op run once
	require.Eventually(t, func() bool { return attempts.Load() >= 1 },
		time.Second, 10*time.Millisecond)

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("RunWithBackoff did not return after op returned ctx.Err()")
	}
	// No busy-loop: should be effectively instantaneous.
	assert.Less(t, time.Since(start), 500*time.Millisecond,
		"helper should not busy-loop after op returns ctx.Err()")
}

func TestNextBackoff_RespectsCap(t *testing.T) {
	opts := Options{
		InitialDelay:   10 * time.Millisecond,
		MaxDelay:       100 * time.Millisecond,
		Factor:         4.0,
		JitterFraction: -1, // disable jitter for deterministic test
	}
	// 10, 40, 100 (capped), 100, 100...
	d := time.Duration(0)
	expected := []time.Duration{
		10 * time.Millisecond,
		40 * time.Millisecond,
		100 * time.Millisecond,
		100 * time.Millisecond,
		100 * time.Millisecond,
	}
	for i, want := range expected {
		got := nextBackoff(d, opts)
		assert.Equal(t, want, got, "step %d", i)
		d = got
	}
}

func TestNextBackoff_JitterBounded(t *testing.T) {
	opts := Options{
		InitialDelay:   100 * time.Millisecond,
		MaxDelay:       10 * time.Second,
		Factor:         2.0,
		JitterFraction: 0.2, // ±20%
	}
	// Sample many times: result must be in [80ms, 120ms] for the first step.
	for i := 0; i < 200; i++ {
		got := nextBackoff(0, opts)
		assert.GreaterOrEqual(t, got, 80*time.Millisecond, "iter %d: %v", i, got)
		assert.LessOrEqual(t, got, 120*time.Millisecond, "iter %d: %v", i, got)
	}
}

func TestRunWithBackoff_CustomLogger(t *testing.T) {
	type call struct {
		name      string
		attempt   int
		err       error
		nextDelay time.Duration
	}
	var calls []call
	mu := make(chan struct{}, 1)
	mu <- struct{}{}

	logger := func(name string, attempt int, err error, nextDelay time.Duration) {
		<-mu
		calls = append(calls, call{name, attempt, err, nextDelay})
		mu <- struct{}{}
	}

	var attempts atomic.Int32
	err := RunWithBackoff(context.Background(), "custom", Options{
		InitialDelay: 1 * time.Millisecond,
		MaxDelay:     2 * time.Millisecond,
		Logger:       logger,
	}, func(ctx context.Context) error {
		n := attempts.Add(1)
		if n < 2 {
			return errors.New("boom")
		}
		return nil
	})
	require.NoError(t, err)
	assert.Len(t, calls, 1, "expected one log call (one failure before success)")
	assert.Equal(t, "custom", calls[0].name)
	assert.Equal(t, 1, calls[0].attempt)
	assert.EqualError(t, calls[0].err, "boom")
}

func TestRunWithBackoff_MinimumDelayFloor(t *testing.T) {
	// Even with extreme jitter, the computed delay must be at least 1ms,
	// so a misconfigured frontend never busy-loops.
	opts := Options{
		InitialDelay:   1 * time.Millisecond,
		MaxDelay:       1 * time.Millisecond,
		JitterFraction: 0.99,
	}
	for i := 0; i < 100; i++ {
		d := nextBackoff(0, opts)
		assert.GreaterOrEqual(t, d, time.Millisecond)
	}
}

func TestNextBackoff_ZeroJitterUsesDefault(t *testing.T) {
	// A zero JitterFraction selects the default (0.2). All sampled
	// delays must fall within ±20% of the input.
	opts := Options{
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     1 * time.Second,
		Factor:       2.0,
	}
	for i := 0; i < 200; i++ {
		got := nextBackoff(0, opts)
		assert.GreaterOrEqual(t, got, 80*time.Millisecond, "iter %d: %v", i, got)
		assert.LessOrEqual(t, got, 120*time.Millisecond, "iter %d: %v", i, got)
	}
}

func TestNextBackoff_PositiveJitterClampedToMaxDelay(t *testing.T) {
	// A positive jitter must not push the delay past MaxDelay. Sample
	// many times at the cap; the result must never exceed MaxDelay.
	opts := Options{
		InitialDelay:   100 * time.Millisecond,
		MaxDelay:       50 * time.Millisecond,
		Factor:         2.0,
		JitterFraction: 0.5, // +50% on the cap would be 75ms; must clamp
	}
	for i := 0; i < 500; i++ {
		got := nextBackoff(0, opts)
		// Initial delay is 100ms but MaxDelay is 50ms — prev starts at
		// 50ms, then jitter is applied. With +50% jitter the result
		// could be 75ms; the clamp must bring it back to 50ms.
		assert.LessOrEqual(t, got, opts.MaxDelay, "iter %d: %v", i, got)
	}
}
