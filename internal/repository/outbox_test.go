package repository

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/model"
)

func TestOutboxRepo_EnqueueListUpdate(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	ob := NewOutboxRepository(db, logrus.NewEntry(logrus.New()))
	now := time.Now().UTC()

	if err := subs.UpsertSubmission(ctx, &model.Submission{
		ID: "sub-ob", State: model.StateOpen, PolicyType: "cgl",
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	e := &model.OutboxEntry{SubmissionID: "sub-ob", Reply: model.Reply{ToAddress: "x@y", Subject: "re", BodyText: "b"}}
	if err := ob.Enqueue(ctx, e); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if e.ID == "" {
		t.Fatal("expected generated id")
	}

	// a cutoff before the entry was created must exclude it (don't sweep fresh rows)
	if past, _ := ob.ListPending(ctx, now.Add(-time.Minute), 10); len(past) != 0 {
		t.Errorf("cutoff should exclude fresh entry, got %d", len(past))
	}

	// a later cutoff returns it, with the reply round-tripped intact
	pend, err := ob.ListPending(ctx, now.Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 1 || pend[0].Reply.ToAddress != "x@y" || pend[0].Reply.Subject != "re" {
		t.Fatalf("listpending: %+v", pend)
	}

	// once sent, it drops out of pending
	if err := ob.Update(ctx, e.ID, model.OutboxSent, 1, ""); err != nil {
		t.Fatal(err)
	}
	if pend2, _ := ob.ListPending(ctx, now.Add(time.Minute), 10); len(pend2) != 0 {
		t.Errorf("sent entry still pending: %d", len(pend2))
	}
}
