// Package reconnect provides a small, shared backoff helper used by the
// chat frontends (Discord, Matrix, Mattermost) to retry their initial
// connect and long-running Run bodies when a chat server is unreachable
// on boot (e.g. Tailscale MagicDNS not ready yet).
//
// The helper is intentionally tiny: one function, one Options struct, no
// package-level state. Callers wrap their Run body in a closure and call
// RunWithBackoff. The op's first ctx-aware return is reported as the
// terminal error; a nil return exits the loop successfully.
package reconnect

import (
	"context"
	"log"
	"math/rand"
	"time"
)

// Options control the backoff schedule. Zero values fall back to defaults
// defined in the const block below.
type Options struct {
	// InitialDelay is the wait before the second attempt (the first wait
	// is zero). Default 1s.
	InitialDelay time.Duration
	// MaxDelay caps the exponential growth. Default 60s.
	MaxDelay time.Duration
	// Factor is the multiplier between attempts. Default 2.0.
	Factor float64
	// JitterFraction is the maximum +/- fraction of the computed delay
	// to randomise, in [0, 1). Zero selects the default (0.2, i.e.
	// ±20%); a negative value disables jitter. Jittered delays are
	// clamped to MaxDelay so a positive jitter cannot push the delay
	// past the cap.
	JitterFraction float64
	// Logger is invoked once per failed attempt. If nil, log.Printf is
	// used. The name argument is the caller-supplied identifier (e.g.
	// "discord", "mattermost", "matrix").
	Logger func(name string, attempt int, err error, nextDelay time.Duration)
}

const (
	defaultInitialDelay   = 1 * time.Second
	defaultMaxDelay       = 60 * time.Second
	defaultFactor         = 2.0
	defaultJitterFraction = 0.2
)

func (o Options) initialDelay() time.Duration {
	if o.InitialDelay > 0 {
		return o.InitialDelay
	}
	return defaultInitialDelay
}

func (o Options) maxDelay() time.Duration {
	if o.MaxDelay > 0 {
		return o.MaxDelay
	}
	return defaultMaxDelay
}

func (o Options) factor() float64 {
	if o.Factor > 1 {
		return o.Factor
	}
	return defaultFactor
}

func (o Options) jitterFraction() float64 {
	if o.JitterFraction < 0 {
		return 0
	}
	if o.JitterFraction == 0 {
		// Zero is the zero-value default; the spec wants jitter, so
		// supply a reasonable default. Callers that want no jitter
		// can pass a small negative value (e.g. -1) to disable it.
		return defaultJitterFraction
	}
	if o.JitterFraction >= 1 {
		return 0.99
	}
	return o.JitterFraction
}

// RunWithBackoff calls op repeatedly until it returns nil or ctx is
// cancelled; it never gives up on its own. The op is called immediately for
// the first attempt; on each failure the delay is doubled (capped at
// MaxDelay) and jittered, then op runs again.
//
// If an invocation of op ran for at least MaxDelay before failing, it is
// treated as a healthy connection that later dropped rather than a failed
// connect: the delay resets to InitialDelay, so a sync that dies after days
// of uptime retries after about a second instead of waiting at the cap.
//
// When ctx is cancelled the loop exits and returns nil: a clean shutdown is
// not an error. Callers can tell cancellation apart from success by
// checking ctx.Err(). Any other error is logged via the configured Logger
// and retried.
//
// The first attempt has no preceding sleep, so a frontend that is healthy
// on first call goes through the loop with no observable delay.
func RunWithBackoff(ctx context.Context, name string, opts Options, op func(ctx context.Context) error) error {
	delay := time.Duration(0)
	attempt := 0
	for {
		attempt++

		if ctx.Err() != nil {
			return nil
		}

		started := nowFn()
		err := op(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}

		if nowFn().Sub(started) >= opts.maxDelay() {
			delay = 0
		}

		nextDelay := nextBackoff(delay, opts)
		logAttempt(opts, name, attempt, err, nextDelay)
		if err := sleepFn(ctx, nextDelay); err != nil {
			return nil // ctx cancelled while waiting
		}
		delay = nextDelay
	}
}

// Seams for tests: the clock and the sleep function.
var (
	nowFn   = time.Now
	sleepFn = sleep
)

// nextBackoff doubles delay and caps it at opts.maxDelay, with optional
// symmetric jitter. The result is always >= 1ms.
func nextBackoff(prev time.Duration, opts Options) time.Duration {
	if prev <= 0 {
		prev = opts.initialDelay()
	} else {
		prev = time.Duration(float64(prev) * opts.factor())
		if prev > opts.maxDelay() {
			prev = opts.maxDelay()
		}
	}

	jf := opts.jitterFraction()
	if jf > 0 {
		// Symmetric jitter in [-jf, +jf). rand.Float64 returns [0, 1).
		delta := (rand.Float64()*2 - 1) * jf
		shifted := time.Duration(float64(prev) * (1 + delta))
		if shifted < time.Millisecond {
			shifted = time.Millisecond
		}
		// Clamp to MaxDelay so a positive jitter can't push the delay
		// past the cap the caller asked for.
		if shifted > opts.maxDelay() {
			shifted = opts.maxDelay()
		}
		prev = shifted
	}

	return prev
}

// sleep blocks for d or until ctx is cancelled, returning ctx.Err() if so.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func logAttempt(opts Options, name string, attempt int, err error, nextDelay time.Duration) {
	if opts.Logger != nil {
		opts.Logger(name, attempt, err, nextDelay)
		return
	}
	log.Printf("%s: connect attempt %d failed: %v; retrying in %s", name, attempt, err, nextDelay)
}
