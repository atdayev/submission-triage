package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/atdayev/submission-triage/internal/model"
)

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

	// Try to reassign the same deterministic_id to a different submission.
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

	// Original submission_id must be preserved.
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

// Legacy rows had missing_items stored as ["a","b"]; the new shape is
// [{id,...}]. The loader must accept both so an existing DB doesn't break
// on first read after the model change.
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
	// Stomp the column with the legacy shape.
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
