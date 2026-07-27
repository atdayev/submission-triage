package service

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"

	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/repository"
	repomocks "github.com/atdayev/submission-triage/internal/repository/mocks"
)

func outboxSvc(ob repository.OutboxRepository, mail *fakeMail, subs *repomocks.SubmissionRepository, aud *repomocks.AuditRepository, maxAttempts int) *SubmissionsService {
	subs.On("SetLastReplyAt", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	return &SubmissionsService{
		repo:              &repository.Repository{Submissions: subs, Audit: aud, Outbox: ob},
		mail:              mail,
		now:               time.Now,
		log:               logrus.NewEntry(logrus.New()),
		outboxRetryAfter:  0,
		outboxMaxAttempts: maxAttempts,
		outboxBatch:       100,
	}
}

// listAllPending returns every pending row regardless of scheduling gates.
func listAllPending(t *testing.T, ob repository.OutboxRepository) []model.OutboxEntry {
	t.Helper()
	far := time.Now().Add(time.Hour)
	pend, err := ob.ListPending(context.Background(), far, far, 100)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	return pend
}

func TestRedeliverOutbox_SendsAndMarksSent(t *testing.T) {
	ctx := context.Background()
	ob := newFakeOutbox()
	subs := repomocks.NewSubmissionRepository(t)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud := repomocks.NewAuditRepository(t)
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()
	mail := &fakeMail{}
	svc := outboxSvc(ob, mail, subs, aud, 3)

	_ = ob.Enqueue(ctx, &model.OutboxEntry{SubmissionID: "s1", Reply: model.Reply{ToAddress: "broker@x", Subject: "re"}})

	if err := svc.RedeliverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if mail.sentCount() != 1 {
		t.Fatalf("sent: got %d, want 1", mail.sentCount())
	}
	if pend := listAllPending(t, ob); len(pend) != 0 {
		t.Errorf("entry still pending after success: %d", len(pend))
	}
}

func TestRedeliverOutbox_RetriesThenDeadLetters(t *testing.T) {
	ctx := context.Background()
	ob := newFakeOutbox()
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	var deadLettered bool
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		e := args.Get(1).(*model.AuditEntry)
		if e.EventType == model.EventReplyFailed {
			if v, _ := e.Payload["dead_lettered"].(bool); v {
				deadLettered = true
			}
		}
	}).Maybe()
	mail := &fakeMail{shouldFail: true}
	svc := outboxSvc(ob, mail, subs, aud, 2)

	_ = ob.Enqueue(ctx, &model.OutboxEntry{SubmissionID: "s1", Reply: model.Reply{ToAddress: "broker@x"}})

	_ = svc.RedeliverOutbox(ctx) // attempt 1 -> still pending
	if pend := listAllPending(t, ob); len(pend) != 1 {
		t.Fatalf("after one failure expected still pending, got %d", len(pend))
	}
	_ = svc.RedeliverOutbox(ctx) // attempt 2 -> hits max -> dead-lettered
	if pend := listAllPending(t, ob); len(pend) != 0 {
		t.Errorf("after max attempts expected dead-letter, still pending %d", len(pend))
	}
	if !deadLettered {
		t.Error("expected a dead-lettered reply.failed audit")
	}
}

// A never-attempted entry is due as soon as its coalesce window elapses; the
// retry backoff applies only to entries that have already failed a send.
func TestRedeliverOutbox_FreshEntryNotGatedByRetryWindow(t *testing.T) {
	ctx := context.Background()
	ob := newFakeOutbox()
	subs := repomocks.NewSubmissionRepository(t)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud := repomocks.NewAuditRepository(t)
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()
	mail := &fakeMail{}
	svc := outboxSvc(ob, mail, subs, aud, 3)

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	nowVal := base
	clk := func() time.Time { return nowVal }
	svc.setClock(clk)
	ob.now = clk
	svc.outboxRetryAfter = 2 * time.Minute

	_ = ob.Enqueue(ctx, &model.OutboxEntry{SubmissionID: "s1", Reply: model.Reply{ToAddress: "broker@x", Subject: "re"}})

	if err := svc.RedeliverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if mail.sentCount() != 1 {
		t.Fatalf("a never-attempted entry must send immediately, not wait out the retry window: got %d", mail.sentCount())
	}
}

// A reply scheduled in the future (coalesce window not elapsed) is held by the
// sweeper until not_before passes — e.g. a shutdown mid-window leaves it pending.
func TestRedeliverOutbox_RespectsNotBefore(t *testing.T) {
	ctx := context.Background()
	ob := newFakeOutbox()
	subs := repomocks.NewSubmissionRepository(t)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud := repomocks.NewAuditRepository(t)
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()
	mail := &fakeMail{}
	svc := outboxSvc(ob, mail, subs, aud, 3)

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	nowVal := base
	clk := func() time.Time { return nowVal }
	svc.setClock(clk)
	ob.now = clk

	_ = ob.Enqueue(ctx, &model.OutboxEntry{SubmissionID: "s1", Reply: model.Reply{ToAddress: "b@x"}, NotBefore: base.Add(2 * time.Minute)})

	_ = svc.RedeliverOutbox(ctx) // before not_before
	if mail.sentCount() != 0 {
		t.Fatalf("a reply scheduled in the future must not send yet: %d", mail.sentCount())
	}

	nowVal = base.Add(3 * time.Minute) // past not_before
	_ = svc.RedeliverOutbox(ctx)
	if mail.sentCount() != 1 {
		t.Fatalf("the sweeper should send once not_before passes: %d", mail.sentCount())
	}
}

// A failed entry backs off for outboxRetryAfter before it is retried.
func TestRedeliverOutbox_AttemptedEntryBacksOffThenRetries(t *testing.T) {
	ctx := context.Background()
	ob := newFakeOutbox()
	subs := repomocks.NewSubmissionRepository(t)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud := repomocks.NewAuditRepository(t)
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()
	mail := &fakeMail{shouldFail: true}
	svc := outboxSvc(ob, mail, subs, aud, 5)

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	nowVal := base
	clk := func() time.Time { return nowVal }
	svc.setClock(clk)
	ob.now = clk
	svc.outboxRetryAfter = 2 * time.Minute

	_ = ob.Enqueue(ctx, &model.OutboxEntry{SubmissionID: "s1", Reply: model.Reply{ToAddress: "broker@x", Subject: "re"}})

	_ = svc.RedeliverOutbox(ctx) // attempt 1 fails -> attempts=1, updated_at=now
	mail.shouldFail = false      // it would succeed if retried

	_ = svc.RedeliverOutbox(ctx) // within the window -> held back
	if mail.sentCount() != 0 {
		t.Fatalf("an attempted entry should back off within the retry window: got %d", mail.sentCount())
	}

	nowVal = base.Add(3 * time.Minute) // past the window
	_ = svc.RedeliverOutbox(ctx)
	if mail.sentCount() != 1 {
		t.Fatalf("an attempted entry should retry after the window: got %d", mail.sentCount())
	}
}

// The sweeper must not re-send an entry the online worker still has in flight.
func TestRedeliverOutbox_SkipsInFlight(t *testing.T) {
	ctx := context.Background()
	ob := newFakeOutbox()
	subs := repomocks.NewSubmissionRepository(t)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud := repomocks.NewAuditRepository(t)
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()
	mail := &fakeMail{}
	svc := outboxSvc(ob, mail, subs, aud, 3)

	e := &model.OutboxEntry{SubmissionID: "s1", Reply: model.Reply{ToAddress: "broker@x"}}
	_ = ob.Enqueue(ctx, e)

	svc.claimDispatch(e.ID) // online worker is sending it
	if err := svc.RedeliverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if mail.sentCount() != 0 {
		t.Fatalf("sweeper double-sent an in-flight entry: %d", mail.sentCount())
	}

	svc.releaseDispatch(e.ID) // online send finished (here, without marking sent)
	if err := svc.RedeliverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if mail.sentCount() != 1 {
		t.Fatalf("after release the sweeper should deliver: got %d", mail.sentCount())
	}
}
