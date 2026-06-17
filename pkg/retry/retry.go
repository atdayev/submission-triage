package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/atdayev/submission-triage/pkg/logger"
)

// MaxDelay caps the backoff doubling.
const MaxDelay = 30 * time.Second

const jitterFactor = 0.2 // ±20%, avoids a thundering herd

// Permanent wraps an error that retries must not be applied to.
type Permanent struct {
	Err error
}

// MarkPermanent wraps err so Do returns it immediately instead of retrying.
func MarkPermanent(err error) error {
	if err == nil {
		return nil
	}
	return &Permanent{Err: err}
}

// a var, not a func, so tests can stub it
var jitter = func(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := float64(d) * jitterFactor
	offset := (rand.Float64()*2 - 1) * spread
	return d + time.Duration(offset)
}

func capDelay(d time.Duration) time.Duration {
	if d > MaxDelay {
		return MaxDelay
	}
	return d
}

// Do calls fn until it succeeds, ctx ends, or attempts are exhausted, backing off with jitter between tries.
func Do(ctx context.Context, attempts int, baseDelay time.Duration, fn func(ctx context.Context) error) error {
	if attempts <= 0 {
		attempts = 1
	}
	if baseDelay <= 0 {
		baseDelay = 100 * time.Millisecond
	}

	log := logger.GetLoggerFromContext(ctx)

	var lastErr error
	delay := baseDelay
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn(ctx)
		if err == nil {
			return nil
		}

		var perm *Permanent
		if errors.As(err, &perm) {
			return fmt.Errorf("retry permanent: %w", err)
		}
		lastErr = err

		if i == attempts-1 {
			break
		}

		sleep := capDelay(jitter(delay))
		log.WithError(err).
			WithField("attempt", i+1).
			WithField("delay", sleep.String()).
			Warn("retryable error, backing off")

		t := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		delay *= 2
		if delay > MaxDelay {
			delay = MaxDelay
		}
	}
	return fmt.Errorf("retry exhausted after %d attempts: %w", attempts, lastErr)
}

// Error returns the wrapped error's message.
func (p *Permanent) Error() string {
	if p.Err == nil {
		return "permanent error"
	}
	return p.Err.Error()
}

// Unwrap returns the wrapped error.
func (p *Permanent) Unwrap() error { return p.Err }
