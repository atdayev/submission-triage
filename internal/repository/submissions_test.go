package repository

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/atdayev/submission-triage/internal/database"
	"github.com/atdayev/submission-triage/internal/model"
)

func setupDB(t *testing.T) (*sql.DB, *SubmissionRepositoryImpl, *AuditRepositoryImpl) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	log := logrus.NewEntry(logrus.New())
	db, err := database.Open(context.Background(), path, log)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := database.Migrate(context.Background(), db, filepath.Join("..", "..", "migrations"), log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db, NewSubmissionRepository(db, log), NewAuditRepository(db, log)
}

func TestSubmissionRepo_UpsertAndGet(t *testing.T) {
	_, subs, _ := setupDB(t)

	now := time.Now().UTC().Truncate(time.Microsecond)
	in := &model.Submission{
		ID:           "sub-1",
		PolicyType:   "cgl",
		State:        model.StateOpen,
		SubjectLine:  "Test",
		FromAddress:  "x@y",
		FromName:     "X",
		ThreadKey:    "tk",
		CreatedAt:    now,
		UpdatedAt:    now,
		LastActionAt: now,
	}
	if err := subs.UpsertSubmission(context.Background(), in); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := subs.GetByID(context.Background(), "sub-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "sub-1" || got.State != model.StateOpen {
		t.Fatalf("unexpected: %+v", got)
	}

	_, err = subs.GetByID(context.Background(), "missing")
	if !errors.Is(err, model.ErrSubmissionNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestSubmissionRepo_FindByEmailReference(t *testing.T) {
	_, subs, _ := setupDB(t)

	now := time.Now().UTC()
	s := &model.Submission{
		ID: "sub-A", PolicyType: "cgl", State: model.StateOpen,
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
		Emails: []model.Email{
			{DeterministicID: "e1", SubmissionID: "sub-A", Direction: model.DirectionInbound, MessageID: "msg-1", ReceivedAt: now},
		},
	}
	if err := subs.UpsertSubmission(context.Background(), s); err != nil {
		t.Fatal(err)
	}

	got, ambiguous, err := subs.FindByEmailReference(context.Background(), []string{"msg-1"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.ID != "sub-A" {
		t.Fatalf("got %s, want sub-A", got.ID)
	}
	if ambiguous {
		t.Fatal("should not be ambiguous")
	}

	_, _, err = subs.FindByEmailReference(context.Background(), []string{"unknown"})
	if !errors.Is(err, model.ErrSubmissionNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestSubmissionRepo_ListStale(t *testing.T) {
	_, subs, _ := setupDB(t)
	now := time.Now().UTC()
	old := now.Add(-100 * time.Hour)
	s := &model.Submission{
		ID: "sub-old", PolicyType: "cgl", State: model.StateAwaiting,
		CreatedAt: old, UpdatedAt: old, LastActionAt: old,
	}
	if err := subs.UpsertSubmission(context.Background(), s); err != nil {
		t.Fatal(err)
	}

	stale, err := subs.ListStale(context.Background(), now.Add(-24*time.Hour).UnixNano(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ID != "sub-old" {
		t.Fatalf("got %+v", stale)
	}
}

func TestSubmissionRepo_FindByEmailReference_Ambiguous(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	earlier := time.Now().UTC().Add(-time.Hour)
	later := time.Now().UTC()

	older := &model.Submission{
		ID: "sub-old", PolicyType: "cgl", State: model.StateOpen,
		CreatedAt: earlier, UpdatedAt: earlier, LastActionAt: earlier,
		Emails: []model.Email{
			{DeterministicID: "e1", SubmissionID: "sub-old", Direction: model.DirectionInbound, MessageID: "shared-msg", ReceivedAt: earlier},
		},
	}
	if err := subs.UpsertSubmission(ctx, older); err != nil {
		t.Fatal(err)
	}

	newer := &model.Submission{
		ID: "sub-new", PolicyType: "cgl", State: model.StateOpen,
		CreatedAt: later, UpdatedAt: later, LastActionAt: later,
		Emails: []model.Email{
			{DeterministicID: "e2", SubmissionID: "sub-new", Direction: model.DirectionInbound, InReplyTo: "shared-msg", ReceivedAt: later},
		},
	}
	if err := subs.UpsertSubmission(ctx, newer); err != nil {
		t.Fatal(err)
	}

	got, ambiguous, err := subs.FindByEmailReference(ctx, []string{"shared-msg"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !ambiguous {
		t.Fatal("expected ambiguous=true with two submissions sharing a ref")
	}
	if got.ID != "sub-new" {
		t.Fatalf("expected most-recent (sub-new), got %s", got.ID)
	}
}

func TestSubmissionRepo_FindByEmailReference_EmptyInput(t *testing.T) {
	_, subs, _ := setupDB(t)
	_, _, err := subs.FindByEmailReference(context.Background(), []string{"  ", ""})
	if !errors.Is(err, model.ErrSubmissionNotFound) {
		t.Fatalf("expected not-found for whitespace-only refs, got %v", err)
	}
}

func TestSubmissionRepo_UpsertSubmission_FullAggregate(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	in := &model.Submission{
		ID: "sub-full", PolicyType: "cgl", State: model.StateAwaiting,
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
		Emails: []model.Email{
			{DeterministicID: "in-1", SubmissionID: "sub-full", Direction: model.DirectionInbound, MessageID: "m-1", ReceivedAt: now},
			{DeterministicID: "out-1", SubmissionID: "sub-full", Direction: model.DirectionOutbound, ReceivedAt: now, ProviderMsgID: "p-1"},
		},
		Documents: []model.Document{
			{ID: "doc-1", SubmissionID: "sub-full", EmailID: "in-1", Filename: "a.pdf", ClassifiedAs: "acord_125", Confidence: 0.95, ClassifiedBy: "heuristic", CreatedAt: now},
		},
		MissingItems: []model.MissingItem{
			{ID: "loss_runs", Description: "Loss runs", Reason: "document not provided"},
		},
	}
	if err := subs.UpsertSubmission(ctx, in); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := subs.GetByID(ctx, "sub-full")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Emails) != 2 {
		t.Fatalf("emails: got %d, want 2", len(got.Emails))
	}
	if len(got.Documents) != 1 {
		t.Fatalf("documents: got %d, want 1", len(got.Documents))
	}
	if got.Documents[0].ClassifiedAs != "acord_125" {
		t.Fatalf("document classified_as: got %q", got.Documents[0].ClassifiedAs)
	}
	if len(got.MissingItems) != 1 || got.MissingItems[0].ID != "loss_runs" {
		t.Fatalf("missing items: got %+v", got.MissingItems)
	}
	if got.MissingItems[0].Reason != "document not provided" {
		t.Errorf("reason: got %q", got.MissingItems[0].Reason)
	}
}

func TestSubmissionRepo_FindByEmailReference_MatchesInReplyTo(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := subs.UpsertSubmission(ctx, &model.Submission{
		ID: "sub-thread", PolicyType: "cgl", State: model.StateOpen,
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
		Emails: []model.Email{
			{DeterministicID: "e-followup", SubmissionID: "sub-thread", Direction: model.DirectionInbound, InReplyTo: "root-msg", ReceivedAt: now},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, _, err := subs.FindByEmailReference(ctx, []string{"root-msg"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.ID != "sub-thread" {
		t.Fatalf("got %s, want sub-thread", got.ID)
	}
}

func TestSubmissionRepo_UpsertSubmission_UpdatesExisting(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	t1 := time.Now().UTC().Truncate(time.Microsecond)

	original := &model.Submission{
		ID: "sub-upd", PolicyType: "cgl", State: model.StateOpen,
		SubjectLine: "first", CreatedAt: t1, UpdatedAt: t1, LastActionAt: t1,
	}
	if err := subs.UpsertSubmission(ctx, original); err != nil {
		t.Fatal(err)
	}

	t2 := t1.Add(time.Hour)
	updated := &model.Submission{
		ID: "sub-upd", PolicyType: "cgl", State: model.StateAwaiting,
		SubjectLine: "second", CreatedAt: t1, UpdatedAt: t2, LastActionAt: t2,
	}
	if err := subs.UpsertSubmission(ctx, updated); err != nil {
		t.Fatal(err)
	}

	got, err := subs.GetByID(ctx, "sub-upd")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateAwaiting {
		t.Errorf("state: got %s, want awaiting", got.State)
	}
	if got.SubjectLine != "second" {
		t.Errorf("subject: got %q", got.SubjectLine)
	}
	if !got.UpdatedAt.Equal(t2) {
		t.Errorf("updated_at: got %v, want %v", got.UpdatedAt, t2)
	}
}

func TestSubmissionRepo_ListStale_OnlyAwaitingState(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-200 * time.Hour)

	for id, state := range map[string]model.State{
		"stale-awaiting": model.StateAwaiting,
		"stale-complete": model.StateComplete,
		"stale-open":     model.StateOpen,
	} {
		if err := subs.UpsertSubmission(ctx, &model.Submission{
			ID: id, PolicyType: "cgl", State: state,
			CreatedAt: old, UpdatedAt: old, LastActionAt: old,
		}); err != nil {
			t.Fatal(err)
		}
	}

	stale, err := subs.ListStale(ctx, time.Now().UTC().Add(-time.Hour).UnixNano(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ID != "stale-awaiting" {
		t.Fatalf("expected only stale-awaiting, got %+v", stale)
	}
}

func TestSubmissionRepo_UpsertEmail_Standalone(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := subs.UpsertSubmission(ctx, &model.Submission{
		ID: "sub-e", PolicyType: "cgl", State: model.StateOpen,
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if err := subs.UpsertEmail(ctx, &model.Email{
		DeterministicID: "standalone-1",
		SubmissionID:    "sub-e",
		Direction:       model.DirectionOutbound,
		ProviderMsgID:   "pm-1",
		Subject:         "Re: hello",
		ReceivedAt:      now,
	}); err != nil {
		t.Fatalf("upsert email: %v", err)
	}

	got, err := subs.GetByID(ctx, "sub-e")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Emails) != 1 || got.Emails[0].DeterministicID != "standalone-1" {
		t.Fatalf("emails: got %+v", got.Emails)
	}
}

func TestSubmissionRepo_ListCompletedBefore(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-30 * 24 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	cases := []struct {
		id      string
		state   model.State
		updated time.Time
		wantHit bool
		wantPos int // index among returned (sorted asc by updated_at)
	}{
		{"old-complete", model.StateComplete, old, true, 0},
		{"recent-complete", model.StateComplete, recent, false, -1},
		{"old-awaiting", model.StateAwaiting, old, false, -1},
		{"old-escalated", model.StateEscalated, old, false, -1},
	}
	for _, tc := range cases {
		if err := subs.UpsertSubmission(ctx, &model.Submission{
			ID: tc.id, PolicyType: "cgl", State: tc.state,
			CreatedAt: tc.updated, UpdatedAt: tc.updated, LastActionAt: tc.updated,
		}); err != nil {
			t.Fatal(err)
		}
	}

	cutoff := now.Add(-7 * 24 * time.Hour).UnixNano()
	got, err := subs.ListCompletedBefore(ctx, cutoff, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "old-complete" {
		t.Fatalf("expected [old-complete], got %+v", got)
	}
}

func TestSubmissionRepo_ListEscalatedSince(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	long := now.Add(-100 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	subs.UpsertSubmission(ctx, &model.Submission{
		ID: "esc-recent", PolicyType: "cgl", State: model.StateEscalated,
		CreatedAt: long, UpdatedAt: recent, LastActionAt: recent,
		EscalatedAt: &recent,
	})
	subs.UpsertSubmission(ctx, &model.Submission{
		ID: "esc-old", PolicyType: "cgl", State: model.StateEscalated,
		CreatedAt: long, UpdatedAt: long, LastActionAt: long,
		EscalatedAt: &long,
	})
	subs.UpsertSubmission(ctx, &model.Submission{
		ID: "not-escalated", PolicyType: "cgl", State: model.StateAwaiting,
		CreatedAt: long, UpdatedAt: long, LastActionAt: long,
	})

	since := now.Add(-24 * time.Hour).UnixNano()
	got, err := subs.ListEscalatedSince(ctx, since, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "esc-recent" {
		t.Fatalf("expected [esc-recent], got %+v", got)
	}
}

func TestSubmissionRepo_UpsertEmail_DuplicatePreservesOriginalSubmission(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	subs.UpsertSubmission(ctx, &model.Submission{
		ID: "sub-A", PolicyType: "cgl", State: model.StateOpen,
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	})
	subs.UpsertSubmission(ctx, &model.Submission{
		ID: "sub-B", PolicyType: "cgl", State: model.StateOpen,
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	})

	original := &model.Email{
		DeterministicID: "shared-det-id",
		SubmissionID:    "sub-A",
		Direction:       model.DirectionInbound,
		MessageID:       "msg-x",
		ReceivedAt:      now,
	}
	if err := subs.UpsertEmail(ctx, original); err != nil {
		t.Fatal(err)
	}

	// a conflicting reassignment of the same deterministic_id must be rejected
	conflict := &model.Email{
		DeterministicID: "shared-det-id",
		SubmissionID:    "sub-B",
		Direction:       model.DirectionInbound,
		MessageID:       "msg-x",
		ReceivedAt:      now,
	}
	err := subs.UpsertEmail(ctx, conflict)
	if !errors.Is(err, model.ErrDuplicateEmail) {
		t.Fatalf("expected ErrDuplicateEmail, got %v", err)
	}

	// the original submission keeps ownership of the email
	got, err := subs.GetByID(ctx, "sub-A")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Emails) != 1 {
		t.Fatalf("sub-A should still own the email, got %d emails", len(got.Emails))
	}
	gotB, err := subs.GetByID(ctx, "sub-B")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotB.Emails) != 0 {
		t.Fatalf("sub-B should not have stolen the email, got %d", len(gotB.Emails))
	}
}

func TestSubmissionRepo_Documents_ExtractedFieldsRoundTrip(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	in := &model.Submission{
		ID: "sub-ef", PolicyType: "cgl", State: model.StateAwaiting,
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
		Documents: []model.Document{{
			ID: "doc-1", SubmissionID: "sub-ef", Filename: "loss_runs.pdf",
			ClassifiedAs: "loss_runs", Confidence: 0.9, ClassifiedBy: "llm",
			ExtractedFields: map[string]any{
				"years_covered": 7.0,
				"insurer":       "Sample Carrier",
			},
			CreatedAt: now,
		}},
	}
	if err := subs.UpsertSubmission(ctx, in); err != nil {
		t.Fatal(err)
	}

	got, err := subs.GetByID(ctx, "sub-ef")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Documents) != 1 {
		t.Fatalf("expected 1 document, got %d", len(got.Documents))
	}
	if got.Documents[0].ExtractedFields["years_covered"] != 7.0 {
		t.Errorf("years_covered: got %v, want 7.0", got.Documents[0].ExtractedFields["years_covered"])
	}
	if got.Documents[0].ExtractedFields["insurer"] != "Sample Carrier" {
		t.Errorf("insurer: got %v", got.Documents[0].ExtractedFields["insurer"])
	}
}

// legacy rows stored missing_items as ["a","b"]; the loader must accept both shapes
func TestSubmissionRepo_DecodeMissingItems_LegacyStringArray(t *testing.T) {
	db, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := subs.UpsertSubmission(ctx, &model.Submission{
		ID: "sub-legacy", PolicyType: "cgl", State: model.StateAwaiting,
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// overwrite the column with the legacy shape
	if _, err := db.Exec(`UPDATE submissions SET missing_items = ? WHERE id = ?`,
		`["acord_125","loss_runs"]`, "sub-legacy"); err != nil {
		t.Fatal(err)
	}

	got, err := subs.GetByID(ctx, "sub-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.MissingItems) != 2 {
		t.Fatalf("got %d items, want 2", len(got.MissingItems))
	}
	if got.MissingItems[0].ID != "acord_125" || got.MissingItems[1].ID != "loss_runs" {
		t.Errorf("items: %+v", got.MissingItems)
	}
}

func TestSubmissionRepo_UpsertSubmission_EmptyIDRejected(t *testing.T) {
	_, subs, _ := setupDB(t)
	err := subs.UpsertSubmission(context.Background(), &model.Submission{})
	if err == nil {
		t.Fatal("expected error for empty ID")
	}
}
