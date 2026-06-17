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

func TestOutboxRepo_PoisonRowDoesNotBlockQueue(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	ob := NewOutboxRepository(db, logrus.NewEntry(logrus.New()))
	now := time.Now().UTC()

	if err := subs.UpsertSubmission(ctx, &model.Submission{
		ID: "s1", State: model.StateOpen, PolicyType: "cgl",
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// an undecodable row at the head of the queue (oldest) must not abort the batch
	old := now.Add(-2 * time.Hour).UnixNano()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO outbox (id, submission_id, reply_json, status, attempts, last_error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		"poison", "s1", "{not json", string(model.OutboxPending), 0, "", old, old,
	); err != nil {
		t.Fatal(err)
	}
	if err := ob.Enqueue(ctx, &model.OutboxEntry{
		ID: "good", SubmissionID: "s1", Reply: model.Reply{ToAddress: "x@y", Subject: "re", BodyText: "b"},
	}); err != nil {
		t.Fatal(err)
	}

	pend, err := ob.ListPending(ctx, now.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("listpending: %v", err)
	}
	if len(pend) != 1 || pend[0].ID != "good" {
		t.Fatalf("good row should survive a poison row: got %+v", pend)
	}

	// the poison row is dead-lettered so it leaves the pending set
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM outbox WHERE id = ?`, "poison").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(model.OutboxFailed) {
		t.Errorf("poison row status: got %q, want %q", status, model.OutboxFailed)
	}
}

func TestOutboxRepo_RetryWindowBacksOffOnUpdate(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	ob := NewOutboxRepository(db, logrus.NewEntry(logrus.New()))
	now := time.Now().UTC()

	if err := subs.UpsertSubmission(ctx, &model.Submission{
		ID: "s2", State: model.StateOpen, PolicyType: "cgl",
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// an old row is due for retry
	old := now.Add(-time.Hour)
	e := &model.OutboxEntry{ID: "r1", SubmissionID: "s2", Reply: model.Reply{ToAddress: "x@y", Subject: "re", BodyText: "b"}, CreatedAt: old}
	if err := ob.Enqueue(ctx, e); err != nil {
		t.Fatal(err)
	}
	// a failed attempt bumps updated_at to now; the retry window must exclude it again
	if err := ob.Update(ctx, "r1", model.OutboxPending, 1, "transient"); err != nil {
		t.Fatal(err)
	}
	if pend, _ := ob.ListPending(ctx, now.Add(-time.Minute), 10); len(pend) != 0 {
		t.Errorf("a just-retried row should back off, got %d pending", len(pend))
	}
}
