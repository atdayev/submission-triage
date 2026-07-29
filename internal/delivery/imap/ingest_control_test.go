package imap

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/service"
)

// 2.1 — mail older than the cutoff is never fetched -------------------------------

func TestPoller_AppliesConfiguredCutoff(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mb := &fakeMailbox{}
	p := testPoller(mb, &fakeIngester{})
	p.configuredCutoff = cutoff
	p.cutoff = p.resolveCutoff(context.Background())
	p.pollOnce(context.Background())

	if !mb.fetchSince.Equal(cutoff) {
		t.Errorf("fetch cutoff: got %v, want the configured %v", mb.fetchSince, cutoff)
	}
}

// With no configured cutoff the first-boot timestamp stands in, and it must come
// from storage: deriving it at boot would skip a week of real mail after a week of
// downtime.
func TestPoller_FallsBackToPersistedFirstBoot(t *testing.T) {
	booted := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	ctrl := &fakeControl{bootAt: booted, attempts: map[uint32]int{}}
	mb := &fakeMailbox{}
	p := testPollerWithControl(mb, &fakeIngester{}, ctrl)
	p.cutoff = p.resolveCutoff(context.Background())
	p.pollOnce(context.Background())

	if !mb.fetchSince.Equal(booted) {
		t.Errorf("fetch cutoff: got %v, want the persisted first boot %v", mb.fetchSince, booted)
	}
}

// 2.2 — a message that always fails is quarantined --------------------------------

func TestPoller_QuarantinesAfterMaxAttempts(t *testing.T) {
	ctrl := newFakeControl()
	ing := &fakeIngester{fn: func(service.IngestRequest) error { return errors.New("boom") }}

	// each tick is its own fetch of the same still-unread message
	var lastSeen []uint32
	for i := 1; i <= 5; i++ {
		mb := &fakeMailbox{msgs: []rawMessage{{UID: 11, Raw: []byte(validEML)}}}
		p := testPollerWithControl(mb, ing, ctrl)
		p.maxAttempts = 3
		p.pollOnce(context.Background())
		lastSeen = mb.seen
		if i < 3 && len(mb.seen) != 0 {
			t.Fatalf("attempt %d: a retryable failure must leave the message unread, got %v", i, mb.seen)
		}
	}
	if len(lastSeen) != 1 || lastSeen[0] != 11 {
		t.Errorf("past the attempt cap the message must be quarantined, got %v", lastSeen)
	}
	if ctrl.attempts[11] < 3 {
		t.Errorf("attempts should accumulate across ticks, got %d", ctrl.attempts[11])
	}
}

// A deterministic failure can only fail the same way next tick, so retrying it
// reprocesses the message — and re-pays its LLM cost — on every poll forever.
func TestPoller_MarksSeenOnDeterministicFailure(t *testing.T) {
	mb := &fakeMailbox{msgs: []rawMessage{{UID: 12, Raw: []byte(validEML)}}}
	ctrl := newFakeControl()
	ing := &fakeIngester{fn: func(service.IngestRequest) error {
		return model.ErrInvalidTransition
	}}
	p := testPollerWithControl(mb, ing, ctrl)
	p.pollOnce(context.Background())

	if len(mb.seen) != 1 || mb.seen[0] != 12 {
		t.Errorf("an invalid-transition failure must be marked read, got %v", mb.seen)
	}
	if ctrl.attempts[12] != 0 {
		t.Error("a deterministic failure should not consume retry attempts; it is not retried")
	}
}

func TestPoller_ClearsFailureCountOnSuccess(t *testing.T) {
	mb := &fakeMailbox{msgs: []rawMessage{{UID: 13, Raw: []byte(validEML)}}}
	ctrl := newFakeControl()
	testPollerWithControl(mb, &fakeIngester{}, ctrl).pollOnce(context.Background())

	if len(ctrl.cleared) != 1 || ctrl.cleared[0] != 13 {
		t.Errorf("a message that finally ingested should have its failure count cleared, got %v", ctrl.cleared)
	}
}

// 3.5 — one poll cycle is bounded --------------------------------------------------

func TestPoller_BoundsEachPollCycle(t *testing.T) {
	var got context.Context
	p := testPoller(&fakeMailbox{}, &fakeIngester{})
	p.timeout = time.Minute
	p.dial = func(ctx context.Context) (mailbox, error) {
		got = ctx
		return &fakeMailbox{}, nil
	}
	p.pollOnce(context.Background())

	if got == nil {
		t.Fatal("dial was not called")
	}
	if _, ok := got.Deadline(); !ok {
		t.Error("a poll cycle must carry a deadline; the dial timeout covers TCP connect only")
	}
}
