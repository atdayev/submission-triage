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
	if pend, _ := ob.ListPending(ctx, time.Now(), 10); len(pend) != 0 {
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
	if pend, _ := ob.ListPending(ctx, time.Now(), 10); len(pend) != 1 {
		t.Fatalf("after one failure expected still pending, got %d", len(pend))
	}
	_ = svc.RedeliverOutbox(ctx) // attempt 2 -> hits max -> dead-lettered
	if pend, _ := ob.ListPending(ctx, time.Now(), 10); len(pend) != 0 {
		t.Errorf("after max attempts expected dead-letter, still pending %d", len(pend))
	}
	if !deadLettered {
		t.Error("expected a dead-lettered reply.failed audit")
	}
}

// The sweeper backs off entries updated within outboxRetryAfter so it can't
// double-dispatch one the online sender just enqueued.
func TestRedeliverOutbox_RetryWindowBacksOffRecentEntry(t *testing.T) {
	ctx := context.Background()
	ob := newFakeOutbox()
	subs := repomocks.NewSubmissionRepository(t)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud := repomocks.NewAuditRepository(t)
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()
	mail := &fakeMail{}
	svc := outboxSvc(ob, mail, subs, aud, 3)

	// a controlled clock shared by the service and the fake outbox
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
	if mail.sentCount() != 0 {
		t.Fatalf("sweeper sent a freshly-enqueued entry inside the retry window: %d", mail.sentCount())
	}

	nowVal = base.Add(3 * time.Minute) // past the window
	if err := svc.RedeliverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if mail.sentCount() != 1 {
		t.Fatalf("sweeper should deliver after the retry window: got %d", mail.sentCount())
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
