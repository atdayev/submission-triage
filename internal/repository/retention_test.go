package repository

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/model"
)

// insertSubmission persists a minimal submission with the given state and clocks.
func insertSubmission(t *testing.T, subs *SubmissionRepositoryImpl, s *model.Submission) {
	t.Helper()
	if s.PolicyType == "" {
		s.PolicyType = "cgl"
	}
	if err := subs.UpsertSubmission(context.Background(), s); err != nil {
		t.Fatalf("upsert %s: %v", s.ID, err)
	}
}

func TestSubmissionRepo_ListStalledBefore(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)

	for _, tc := range []struct {
		id     string
		state  model.State
		action time.Time
		onHold bool
	}{
		{"stale-awaiting", model.StateAwaiting, old, false},
		{"stale-open", model.StateOpen, old, false},
		{"stale-held", model.StateAwaiting, old, true}, // a human is working it
		{"fresh-awaiting", model.StateAwaiting, now, false},
		{"stale-complete", model.StateComplete, old, false}, // the completed sweep owns this
		{"stale-closed", model.StateClosed, old, false},
	} {
		insertSubmission(t, subs, &model.Submission{
			ID: tc.id, State: tc.state, CreatedAt: tc.action, UpdatedAt: tc.action,
			LastActionAt: tc.action, OnHold: tc.onHold,
		})
	}

	got, err := subs.ListStalledBefore(ctx, now.Add(-90*24*time.Hour).UnixNano(), 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(got))
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	if want := []string{"stale-awaiting", "stale-open"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("got %v, want %v", ids, want)
	}
}

func TestSubmissionRepo_ReplyBlockedIDs(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, tc := range []struct {
		id                     string
		onHold, deliveryFailed bool
	}{
		{"held", true, false},
		{"bounced", false, true},
		{"fine", false, false},
	} {
		insertSubmission(t, subs, &model.Submission{
			ID: tc.id, State: model.StateAwaiting, CreatedAt: now, UpdatedAt: now, LastActionAt: now,
			OnHold: tc.onHold, DeliveryFailed: tc.deliveryFailed,
		})
	}

	got, err := subs.ReplyBlockedIDs(ctx, []string{"held", "bounced", "fine", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if want := []string{"bounced", "held"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSubmissionRepo_ClearExtractedTextForClosed(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-200 * 24 * time.Hour)

	for _, tc := range []struct {
		id      string
		state   model.State
		updated time.Time
	}{
		{"long-closed", model.StateClosed, old},
		{"just-closed", model.StateClosed, now},
		{"still-open", model.StateAwaiting, old},
	} {
		insertSubmission(t, subs, &model.Submission{
			ID: tc.id, State: tc.state, CreatedAt: tc.updated, UpdatedAt: tc.updated, LastActionAt: tc.updated,
			Documents: []model.Document{{
				ID: tc.id + "-doc", SubmissionID: tc.id, Filename: "a.pdf",
				ExtractedText: "the full text of the document", CreatedAt: tc.updated,
			}},
		})
	}

	n, err := subs.ClearExtractedTextForClosed(ctx, now.Add(-90*24*time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("cleared %d documents, want 1", n)
	}
	for _, tc := range []struct {
		id        string
		wantEmpty bool
	}{
		{"long-closed", true},
		{"just-closed", false},
		{"still-open", false},
	} {
		var text string
		if err := db.QueryRowContext(ctx,
			`SELECT extracted_text FROM documents WHERE id = ?`, tc.id+"-doc").Scan(&text); err != nil {
			t.Fatal(err)
		}
		if (text == "") != tc.wantEmpty {
			t.Errorf("%s: extracted_text empty=%v, want empty=%v", tc.id, text == "", tc.wantEmpty)
		}
	}
}

func TestAuditRepo_PruneAndListByEvent(t *testing.T) {
	_, subs, aud := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-200 * 24 * time.Hour)

	insertSubmission(t, subs, &model.Submission{
		ID: "s1", State: model.StateAwaiting, CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	})
	for _, e := range []*model.AuditEntry{
		{SubmissionID: "s1", EventType: model.EventReplyDisabled, CreatedAt: now},
		{SubmissionID: "s1", EventType: model.EventEmailReceived, CreatedAt: now},
		{SubmissionID: "s1", EventType: model.EventEmailReceived, CreatedAt: old},
	} {
		if err := aud.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	ids, err := aud.ListSubmissionIDsByEvent(ctx, model.EventReplyDisabled, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"s1"}) {
		t.Errorf("withheld lookup: got %v, want [s1]", ids)
	}

	n, err := aud.Prune(ctx, now.Add(-90*24*time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1 (only the aged entry)", n)
	}
	rest, err := aud.ListBySubmission(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 2 {
		t.Errorf("%d entries survived, want 2", len(rest))
	}
}

func TestOutboxRepo_ExpireAndPrune(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	log := logrus.NewEntry(logrus.New())
	ob := NewOutboxRepository(db, log)
	now := time.Now().UTC()
	old := now.Add(-30 * 24 * time.Hour)

	for _, id := range []string{"s-old", "s-new", "s-rel"} {
		insertSubmission(t, subs, &model.Submission{
			ID: id, State: model.StateAwaiting, CreatedAt: now, UpdatedAt: now, LastActionAt: now,
		})
	}
	// a row whose not_before never came due: never handed to the sender, so its
	// attempt count never rises and it can never dead-letter on its own
	stuck := &model.OutboxEntry{SubmissionID: "s-old", Reply: model.Reply{ToAddress: "a@x"}, CreatedAt: old}
	if err := ob.Enqueue(ctx, stuck); err != nil {
		t.Fatal(err)
	}
	fresh := &model.OutboxEntry{SubmissionID: "s-new", Reply: model.Reply{ToAddress: "b@x"}, CreatedAt: now}
	if err := ob.Enqueue(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	released := &model.OutboxEntry{SubmissionID: "s-rel", Reply: model.Reply{ToAddress: "c@x"}, CreatedAt: now}
	if err := ob.Enqueue(ctx, released); err != nil {
		t.Fatal(err)
	}

	n, err := ob.ExpirePending(ctx, now.Add(-7*24*time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expired %d rows, want 1", n)
	}

	n, err = ob.ExpirePendingForSubmissions(ctx, []string{"s-rel"}, "stale: queued before hold")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expired %d rows for the released submission, want 1", n)
	}

	// only settled rows are pruned; the still-pending fresh row survives
	n, err = ob.Prune(ctx, now.Add(-7*24*time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1 (the aged expired row)", n)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Errorf("%d rows remain, want 2 (fresh pending + recently-expired)", remaining)
	}
}

func TestIngestControlRepo_FirstBootIsPersisted(t *testing.T) {
	db, _, _ := setupDB(t)
	ctx := context.Background()
	ctrl := NewIngestControlRepository(db, logrus.NewEntry(logrus.New()))

	first := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	got, err := ctrl.FirstBootAt(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(first) {
		t.Fatalf("got %v, want %v", got, first)
	}
	// a restart a week later must not move the cutoff, or it skips a week of real mail
	again, err := ctrl.FirstBootAt(ctx, first.Add(7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Equal(first) {
		t.Errorf("the cutoff moved on restart: got %v, want %v", again, first)
	}
}

func TestIngestControlRepo_FailureCounting(t *testing.T) {
	db, _, _ := setupDB(t)
	ctx := context.Background()
	ctrl := NewIngestControlRepository(db, logrus.NewEntry(logrus.New()))
	now := time.Now().UTC()

	for want := 1; want <= 3; want++ {
		got, err := ctrl.RecordFailure(ctx, 42, 7, "boom", now)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("attempt %d: got %d", want, got)
		}
	}
	// a UID is unique only within its UIDVALIDITY; a mailbox reset must not inherit
	// the old generation's count and quarantine an unrelated message
	other, err := ctrl.RecordFailure(ctx, 43, 7, "boom", now)
	if err != nil {
		t.Fatal(err)
	}
	if other != 1 {
		t.Errorf("a new UIDVALIDITY should start over, got %d", other)
	}

	if err := ctrl.ClearFailure(ctx, 42, 7); err != nil {
		t.Fatal(err)
	}
	after, err := ctrl.RecordFailure(ctx, 42, 7, "boom", now)
	if err != nil {
		t.Fatal(err)
	}
	if after != 1 {
		t.Errorf("clearing should reset the count, got %d", after)
	}
}

// A dead-lettered reply never reached the broker, so the repeat-suppression gate
// must be released or the submission is stranded with an ask nobody ever saw.
func TestSubmissionRepo_ClearReplyHash(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertSubmission(t, subs, &model.Submission{
		ID: "s1", State: model.StateAwaiting, CreatedAt: now, UpdatedAt: now, LastActionAt: now,
		LastReplyHash: "deadbeef",
	})
	if err := subs.ClearReplyHash(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	got, err := subs.GetByID(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastReplyHash != "" {
		t.Errorf("last_reply_hash: got %q, want empty", got.LastReplyHash)
	}
}

// GetPending is what stops the sweeper delivering from a stale ListPending snapshot:
// it must refuse a row that has since been sent, and hand back current content for
// one that has been superseded.
func TestOutboxRepo_GetPending(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	ob := NewOutboxRepository(db, logrus.NewEntry(logrus.New()))
	now := time.Now().UTC()

	insertSubmission(t, subs, &model.Submission{
		ID: "s1", State: model.StateAwaiting, CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	})
	e := &model.OutboxEntry{SubmissionID: "s1", Reply: model.Reply{ToAddress: "b@x", Subject: "first ask"}}
	if err := ob.Enqueue(ctx, e); err != nil {
		t.Fatal(err)
	}

	got, ok, err := ob.GetPending(ctx, e.ID)
	if err != nil || !ok {
		t.Fatalf("pending row should be readable: ok=%v err=%v", ok, err)
	}
	if got.Reply.Subject != "first ask" {
		t.Errorf("subject: got %q, want %q", got.Reply.Subject, "first ask")
	}

	// a follow-up supersedes the row's content; the sweeper must send the new text
	superseded := &model.OutboxEntry{SubmissionID: "s1", Reply: model.Reply{ToAddress: "b@x", Subject: "revised ask"}}
	if err := ob.Enqueue(ctx, superseded); err != nil {
		t.Fatal(err)
	}
	got, ok, err = ob.GetPending(ctx, e.ID)
	if err != nil || !ok {
		t.Fatalf("superseded row is still pending: ok=%v err=%v", ok, err)
	}
	if got.Reply.Subject != "revised ask" {
		t.Errorf("subject after supersede: got %q, want the current text", got.Reply.Subject)
	}

	// once sent it must not be handed out again
	if err := ob.MarkSent(ctx, e.ID, got.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ob.GetPending(ctx, e.ID); err != nil || ok {
		t.Errorf("a sent row must not be re-delivered: ok=%v err=%v", ok, err)
	}
	if _, ok, err := ob.GetPending(ctx, "no-such-row"); err != nil || ok {
		t.Errorf("a missing row should read as not-pending, not error: ok=%v err=%v", ok, err)
	}
}
