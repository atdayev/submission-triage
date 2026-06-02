package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDo_MaxDelayCapsExponentialGrowth(t *testing.T) {
	// Replace jitter with identity so timing is deterministic.
	origJitter := jitter
	jitter = func(d time.Duration) time.Duration { return d }
	t.Cleanup(func() { jitter = origJitter })

	// Track each sleep the loop tried.
	var sleeps []time.Duration
	recordSleep := func(d time.Duration) time.Duration {
		sleeps = append(sleeps, d)
		return time.Microsecond // short-circuit actual sleep so the test is fast
	}
	jitter = recordSleep

	// 20 attempts, base 1s → without cap the largest delay would be 2^18 ≈ 262144s.
	// With MaxDelay=30s we should hit the cap quickly.
	_ = Do(context.Background(), 20, 1*time.Second, func(context.Context) error {
		return errors.New("always fails")
	})

	if len(sleeps) == 0 {
		t.Fatal("no sleeps recorded")
	}
	for i, d := range sleeps {
		if d > MaxDelay {
			t.Errorf("sleep #%d = %v exceeds MaxDelay %v", i, d, MaxDelay)
		}
	}
	// The last few should all be exactly MaxDelay.
	if last := sleeps[len(sleeps)-1]; last != MaxDelay {
		t.Errorf("expected last sleep to hit MaxDelay (%v), got %v", MaxDelay, last)
	}
}

func TestJitter_StaysWithinBand(t *testing.T) {
	const d = 100 * time.Millisecond
	const margin = float64(d) * jitterFactor

	for i := 0; i < 200; i++ {
		got := jitter(d)
		if float64(got) < float64(d)-margin {
			t.Errorf("jitter %v < lower bound %v", got, time.Duration(float64(d)-margin))
		}
		if float64(got) > float64(d)+margin {
			t.Errorf("jitter %v > upper bound %v", got, time.Duration(float64(d)+margin))
		}
	}
}

func TestJitter_ZeroOrNegativeReturnsUnchanged(t *testing.T) {
	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %v, want 0", got)
	}
	if got := jitter(-1); got != -1 {
		t.Errorf("jitter(-1) = %v, want -1", got)
	}
}

func TestJitter_VariesAcrossCalls(t *testing.T) {
	// With 200 calls the chance of every value being identical is
	// vanishingly small unless jitter is broken.
	const d = 100 * time.Millisecond
	seen := map[time.Duration]int{}
	for i := 0; i < 200; i++ {
		seen[jitter(d)]++
	}
	if len(seen) < 10 {
		t.Errorf("jitter produced %d distinct values across 200 calls; expected many more", len(seen))
	}
}
