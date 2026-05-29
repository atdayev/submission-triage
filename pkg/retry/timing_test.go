package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// retry.Do doubles the delay after each failed attempt. With baseDelay=10ms
// and attempts=4 the unjittered sleeps are 10ms + 20ms + 40ms = 70ms. We
// stub jitter to identity so the assertion isn't flaky against the ±20%
// randomization.
func TestDo_BackoffDoubles(t *testing.T) {
	origJitter := jitter
	jitter = func(d time.Duration) time.Duration { return d }
	t.Cleanup(func() { jitter = origJitter })

	const (
		attempts  = 4
		baseDelay = 10 * time.Millisecond
		minTotal  = 70 * time.Millisecond
		// Flat-no-doubling would land at 30ms; we want to fail on regression.
		// Upper bound allows scheduler slop on slow CI.
		maxTotal = 500 * time.Millisecond
	)

	start := time.Now()
	err := Do(context.Background(), attempts, baseDelay, func(context.Context) error {
		return errors.New("transient")
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	if elapsed < minTotal {
		t.Errorf("elapsed %v < min %v — delay isn't being doubled", elapsed, minTotal)
	}
	if elapsed > maxTotal {
		t.Errorf("elapsed %v > max %v — something is sleeping too long", elapsed, maxTotal)
	}
}

// Ensures that successful first attempt skips the backoff path entirely.
func TestDo_SuccessIsImmediate(t *testing.T) {
	start := time.Now()
	err := Do(context.Background(), 5, 100*time.Millisecond, func(context.Context) error {
		return nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("first-attempt success took %v — should be immediate", elapsed)
	}
}
