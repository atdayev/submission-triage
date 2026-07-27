package repository

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/model"
)

func seedSubmission(t *testing.T, subs *SubmissionRepositoryImpl, id string) {
	t.Helper()
	now := time.Now().UTC()
	if err := subs.UpsertSubmission(context.Background(), &model.Submission{
		ID: id, State: model.StateOpen, PolicyType: "cgl",
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	}); err != nil {
		t.Fatalf("seed submission %s: %v", id, err)
	}
}

func TestOutboxRepo_EnqueueListUpdate(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	ob := NewOutboxRepository(db, logrus.NewEntry(logrus.New()))
	now := time.Now().UTC()
	seedSubmission(t, subs, "sub-ob")

	e := &model.OutboxEntry{SubmissionID: "sub-ob", Reply: model.Reply{ToAddress: "x@y", Subject: "re", BodyText: "b"}}
	if err := ob.Enqueue(ctx, e); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if e.ID == "" {
		t.Fatal("expected generated id")
	}

	// a never-attempted row is due immediately, even against a past retry cutoff
	pend, err := ob.ListPending(ctx, now, now.Add(-time.Minute), 10)
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
	if pend2, _ := ob.ListPending(ctx, now.Add(time.Hour), now.Add(time.Hour), 10); len(pend2) != 0 {
		t.Errorf("sent entry still pending: %d", len(pend2))
	}
}

func TestOutboxRepo_NotBeforeGatesDelivery(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	ob := NewOutboxRepository(db, logrus.NewEntry(logrus.New()))
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	seedSubmission(t, subs, "s-nb")

	if err := ob.Enqueue(ctx, &model.OutboxEntry{
		SubmissionID: "s-nb", Reply: model.Reply{ToAddress: "x@y"}, NotBefore: now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if pend, _ := ob.ListPending(ctx, now, now, 10); len(pend) != 0 {
		t.Fatalf("a row scheduled in the future must not be due: %d", len(pend))
	}
	if pend, _ := ob.ListPending(ctx, now.Add(3*time.Minute), now.Add(3*time.Minute), 10); len(pend) != 1 {
		t.Fatalf("a row past not_before must be due: %d", len(pend))
	}
}

func TestOutboxRepo_SecondReplySupersedes(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	ob := NewOutboxRepository(db, logrus.NewEntry(logrus.New()))
	now := time.Now().UTC()
	seedSubmission(t, subs, "s-coalesce")

	first := &model.OutboxEntry{SubmissionID: "s-coalesce", Reply: model.Reply{ToAddress: "x@y", BodyText: "first"}}
	if err := ob.Enqueue(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := &model.OutboxEntry{SubmissionID: "s-coalesce", Reply: model.Reply{ToAddress: "x@y", BodyText: "second"}}
	if err := ob.Enqueue(ctx, second); err != nil {
		t.Fatal(err)
	}
	// one pending row per submission: the second reply supersedes, reusing the row id
	if second.ID != first.ID {
		t.Errorf("coalesced reply should reuse the pending row id: %q vs %q", second.ID, first.ID)
	}
	pend, _ := ob.ListPending(ctx, now.Add(time.Hour), now.Add(time.Hour), 10)
	if len(pend) != 1 {
		t.Fatalf("expected exactly one pending row, got %d", len(pend))
	}
	if pend[0].Reply.BodyText != "second" {
		t.Errorf("pending reply should reflect the latest evaluation: %q", pend[0].Reply.BodyText)
	}
}

func TestOutboxRepo_PoisonRowDoesNotBlockQueue(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	ob := NewOutboxRepository(db, logrus.NewEntry(logrus.New()))
	now := time.Now().UTC()
	seedSubmission(t, subs, "s-poison")
	seedSubmission(t, subs, "s-good")

	// an undecodable row must not abort the batch (it is dead-lettered instead)
	old := now.Add(-2 * time.Hour).UnixNano()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO outbox (id, submission_id, reply_json, status, attempts, last_error, created_at, updated_at, not_before)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		"poison", "s-poison", "{not json", string(model.OutboxPending), 0, "", old, old, old,
	); err != nil {
		t.Fatal(err)
	}
	if err := ob.Enqueue(ctx, &model.OutboxEntry{
		ID: "good", SubmissionID: "s-good", Reply: model.Reply{ToAddress: "x@y", Subject: "re", BodyText: "b"},
	}); err != nil {
		t.Fatal(err)
	}

	pend, err := ob.ListPending(ctx, now, now, 10)
	if err != nil {
		t.Fatalf("listpending: %v", err)
	}
	if len(pend) != 1 || pend[0].ID != "good" {
		t.Fatalf("good row should survive a poison row: got %+v", pend)
	}

	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM outbox WHERE id = ?`, "poison").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(model.OutboxFailed) {
		t.Errorf("poison row status: got %q, want %q", status, model.OutboxFailed)
	}
}

func TestOutboxRepo_MarkSentGuardsAgainstSupersede(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	ob := NewOutboxRepository(db, logrus.NewEntry(logrus.New()))
	far := time.Now().Add(time.Hour)
	seedSubmission(t, subs, "s-cas")

	e := &model.OutboxEntry{SubmissionID: "s-cas", Reply: model.Reply{ToAddress: "x@y", BodyText: "first"}}
	if err := ob.Enqueue(ctx, e); err != nil {
		t.Fatal(err)
	}
	staleVersion := e.UpdatedAt

	// a follow-up supersedes the pending row (updated_at advances) before mark-sent
	time.Sleep(2 * time.Millisecond) // ensure a distinct updated_at
	if err := ob.Enqueue(ctx, &model.OutboxEntry{SubmissionID: "s-cas", Reply: model.Reply{ToAddress: "x@y", BodyText: "second"}}); err != nil {
		t.Fatal(err)
	}

	// marking sent with the STALE version must no-op: the superseding reply survives
	if err := ob.MarkSent(ctx, e.ID, staleVersion); err != nil {
		t.Fatal(err)
	}
	pend, _ := ob.ListPending(ctx, far, far, 10)
	if len(pend) != 1 || pend[0].Reply.BodyText != "second" {
		t.Fatalf("a superseded reply must survive a stale mark-sent: %+v", pend)
	}

	// marking sent with the CURRENT version clears the row
	if err := ob.MarkSent(ctx, pend[0].ID, pend[0].UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if pend2, _ := ob.ListPending(ctx, far, far, 10); len(pend2) != 0 {
		t.Fatalf("a current mark-sent should clear the row: %+v", pend2)
	}
}

func TestSubmissionRepo_PreservesLastReplyAtOnReUpsert(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	sub := &model.Submission{
		ID: "s-lr", PolicyType: "cgl", State: model.StateOpen,
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	}
	if err := subs.UpsertSubmission(ctx, sub); err != nil {
		t.Fatal(err)
	}
	// a send records last_reply_at out of band
	reply := now.Add(time.Minute)
	if err := subs.SetLastReplyAt(ctx, "s-lr", reply); err != nil {
		t.Fatal(err)
	}
	// a re-upsert whose in-memory struct carries LastReplyAt=0 must NOT clobber it
	sub.State = model.StateAwaiting
	if err := subs.UpsertSubmission(ctx, sub); err != nil {
		t.Fatal(err)
	}
	got, err := subs.GetByID(ctx, "s-lr")
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastReplyAt.Equal(reply) {
		t.Fatalf("last_reply_at must survive a re-upsert: got %v, want %v", got.LastReplyAt, reply)
	}
}

func TestOutboxRepo_RetryGateAppliesOnlyToAttempted(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	ob := NewOutboxRepository(db, logrus.NewEntry(logrus.New()))
	now := time.Now().UTC()
	seedSubmission(t, subs, "s-retry")

	if err := ob.Enqueue(ctx, &model.OutboxEntry{
		ID: "r1", SubmissionID: "s-retry", Reply: model.Reply{ToAddress: "x@y", Subject: "re", BodyText: "b"},
	}); err != nil {
		t.Fatal(err)
	}
	// attempts=0: due now regardless of the retry cutoff
	if pend, _ := ob.ListPending(ctx, now, now.Add(-time.Hour), 10); len(pend) != 1 {
		t.Fatalf("a never-attempted row must be due, got %d", len(pend))
	}
	// a failed attempt bumps updated_at + attempts; now the retry window applies
	if err := ob.Update(ctx, "r1", model.OutboxPending, 1, "transient"); err != nil {
		t.Fatal(err)
	}
	if pend, _ := ob.ListPending(ctx, now.Add(time.Hour), now.Add(-time.Hour), 10); len(pend) != 0 {
		t.Errorf("a just-retried row should back off, got %d pending", len(pend))
	}
}
