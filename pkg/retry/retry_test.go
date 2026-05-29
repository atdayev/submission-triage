package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDo_SuccessFirstAttempt(t *testing.T) {
	calls := 0
	err := Do(context.Background(), 3, time.Millisecond, func(context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls: got %d, want 1", calls)
	}
}

func TestDo_SucceedsAfterRetries(t *testing.T) {
	calls := 0
	err := Do(context.Background(), 3, time.Millisecond, func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls: got %d, want 3", calls)
	}
}

func TestDo_ExhaustsAttempts(t *testing.T) {
	calls := 0
	err := Do(context.Background(), 2, time.Millisecond, func(context.Context) error {
		calls++
		return errors.New("always fails")
	})
	if err == nil {
		t.Fatal("expected error after exhaustion")
	}
	if calls != 2 {
		t.Fatalf("calls: got %d, want 2", calls)
	}
}

func TestDo_PermanentShortCircuits(t *testing.T) {
	calls := 0
	target := errors.New("nope")
	err := Do(context.Background(), 5, time.Millisecond, func(context.Context) error {
		calls++
		return MarkPermanent(target)
	})
	if calls != 1 {
		t.Fatalf("calls: got %d, want 1", calls)
	}
	if !errors.Is(err, target) {
		t.Fatalf("expected wrapped target err, got %v", err)
	}
}

func TestDo_ContextCancelInterruptsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	err := Do(ctx, 5, 100*time.Millisecond, func(context.Context) error {
		calls++
		return errors.New("transient")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
