package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/model"
)

func TestUpsertSubmissionWithReply_CommitsSubmissionEmailAndReply(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	sub := &model.Submission{
		ID: "s-ok", PolicyType: "cgl", State: model.StateAwaiting,
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
		Emails: []model.Email{{
			DeterministicID: "det-ok", SubmissionID: "s-ok",
			Direction: model.DirectionInbound, MessageID: "m1", ReceivedAt: now,
		}},
	}
	reply := &model.OutboxEntry{SubmissionID: "s-ok", Reply: model.Reply{ToAddress: "b@x", Subject: "re", BodyText: "b"}}

	if err := subs.UpsertSubmissionWithReply(ctx, sub, reply); err != nil {
		t.Fatalf("upsert with reply: %v", err)
	}
	if reply.ID == "" {
		t.Fatal("reply id should be generated")
	}

	got, err := subs.GetByID(ctx, "s-ok")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Emails) != 1 {
		t.Fatalf("email not persisted: %+v", got.Emails)
	}

	ob := NewOutboxRepository(db, logrus.NewEntry(logrus.New()))
	pend, err := ob.ListPending(ctx, now.Add(time.Minute), now.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pend) != 1 || pend[0].SubmissionID != "s-ok" {
		t.Fatalf("reply not persisted alongside submission: %+v", pend)
	}
}

// Two ingests for one submission leave a single pending reply reflecting the
// latest evaluation (the coalescing upsert), not two colliding rows.
func TestUpsertSubmissionWithReply_SecondReplyCoalesces(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	sub := &model.Submission{
		ID: "s-co", PolicyType: "cgl", State: model.StateAwaiting,
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	}
	if err := subs.UpsertSubmissionWithReply(ctx, sub, &model.OutboxEntry{SubmissionID: "s-co", Reply: model.Reply{ToAddress: "b@x", BodyText: "first"}}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := subs.UpsertSubmissionWithReply(ctx, sub, &model.OutboxEntry{SubmissionID: "s-co", Reply: model.Reply{ToAddress: "b@x", BodyText: "second"}}); err != nil {
		t.Fatalf("second: %v", err)
	}

	ob := NewOutboxRepository(db, logrus.NewEntry(logrus.New()))
	pend, err := ob.ListPending(ctx, now.Add(time.Hour), now.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 1 {
		t.Fatalf("two ingests for one submission should leave one pending reply, got %d", len(pend))
	}
	if pend[0].Reply.BodyText != "second" {
		t.Errorf("pending reply should reflect the latest evaluation: %q", pend[0].Reply.BodyText)
	}
}

func TestUpsertSubmissionWithReply_RollsBackWhenReplyInsertFails(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// seed a submission and an outbox row whose id the next call will collide with
	seed := &model.Submission{
		ID: "seed", PolicyType: "cgl", State: model.StateOpen,
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	}
	if err := subs.UpsertSubmission(ctx, seed); err != nil {
		t.Fatalf("seed submission: %v", err)
	}
	ob := NewOutboxRepository(db, logrus.NewEntry(logrus.New()))
	if err := ob.Enqueue(ctx, &model.OutboxEntry{ID: "dup", SubmissionID: "seed", Reply: model.Reply{ToAddress: "x@y"}}); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}

	sub := &model.Submission{
		ID: "sub-atomic", PolicyType: "cgl", State: model.StateAwaiting,
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
		Emails: []model.Email{{
			DeterministicID: "det-atomic", SubmissionID: "sub-atomic",
			Direction: model.DirectionInbound, ReceivedAt: now,
		}},
	}
	reply := &model.OutboxEntry{ID: "dup", SubmissionID: "sub-atomic", Reply: model.Reply{ToAddress: "z@y"}}

	if err := subs.UpsertSubmissionWithReply(ctx, sub, reply); err == nil {
		t.Fatal("expected the duplicate outbox id to fail the transaction")
	}

	if _, err := subs.GetByID(ctx, "sub-atomic"); !errors.Is(err, model.ErrSubmissionNotFound) {
		t.Fatalf("submission must not persist when the reply insert fails: %v", err)
	}
	if _, err := subs.FindByDeterministicID(ctx, "det-atomic"); !errors.Is(err, model.ErrSubmissionNotFound) {
		t.Fatalf("inbound email must not persist when the reply insert fails: %v", err)
	}
}
