package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/atdayev/submission-triage/pkg/logger"
)

const MaxDelay = 30 * time.Second // cap the doubling
const jitterFactor = 0.2          // ±20%, avoids a thundering herd

type Permanent struct {
	Err error
}

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

		sleep := jitter(delay)
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

func (p *Permanent) Error() string {
	if p.Err == nil {
		return "permanent error"
	}
	return p.Err.Error()
}

func (p *Permanent) Unwrap() error { return p.Err }
