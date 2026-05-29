package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/atdayev/submission-triage/internal/model"
)

func TestAuditRepo_Append_NilEntryRejected(t *testing.T) {
	_, _, aud := setupDB(t)
	err := aud.Append(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "nil entry") {
		t.Fatalf("expected nil-entry error, got %v", err)
	}
}

func TestAuditRepo_Append_GeneratesIDAndCreatedAt(t *testing.T) {
	_, subs, aud := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_ = subs.UpsertSubmission(ctx, &model.Submission{
		ID: "sub-gen", State: model.StateOpen, PolicyType: "cgl",
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	})

	entry := &model.AuditEntry{
		SubmissionID: "sub-gen",
		EventType:    model.EventEmailReceived,
		Payload:      map[string]any{"k": "v"},
	}
	if err := aud.Append(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if entry.ID == "" {
		t.Error("expected generated ID")
	}
	if entry.CreatedAt.IsZero() {
		t.Error("expected generated CreatedAt")
	}
}

func TestAuditRepo_Append_PreservesExplicitIDAndCreatedAt(t *testing.T) {
	_, subs, aud := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_ = subs.UpsertSubmission(ctx, &model.Submission{
		ID: "sub-keep", State: model.StateOpen, PolicyType: "cgl",
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	})

	explicitTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	entry := &model.AuditEntry{
		ID:           "explicit-id",
		SubmissionID: "sub-keep",
		EventType:    model.EventEmailReceived,
		CreatedAt:    explicitTime,
	}
	if err := aud.Append(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if entry.ID != "explicit-id" {
		t.Errorf("ID overwritten: got %s", entry.ID)
	}
	if !entry.CreatedAt.Equal(explicitTime) {
		t.Errorf("CreatedAt overwritten: got %v", entry.CreatedAt)
	}

	list, _ := aud.ListBySubmission(ctx, "sub-keep")
	if len(list) != 1 || list[0].ID != "explicit-id" {
		t.Fatalf("persisted: got %+v", list)
	}
}

func TestAuditRepo_ListBySubmission_OrdersAscending(t *testing.T) {
	_, subs, aud := setupDB(t)
	ctx := context.Background()
	base := time.Now().UTC()
	_ = subs.UpsertSubmission(ctx, &model.Submission{
		ID: "sub-ord", State: model.StateOpen, PolicyType: "cgl",
		CreatedAt: base, UpdatedAt: base, LastActionAt: base,
	})

	for i, evt := range []model.EventType{
		model.EventChecklistEvaluated,
		model.EventEmailReceived,
		model.EventReplySent,
	} {
		_ = aud.Append(ctx, &model.AuditEntry{
			SubmissionID: "sub-ord",
			EventType:    evt,
			CreatedAt:    base.Add(time.Duration(i) * time.Second),
		})
	}

	list, err := aud.ListBySubmission(ctx, "sub-ord")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i].CreatedAt.Before(list[i-1].CreatedAt) {
			t.Fatalf("entry %d (%v) earlier than entry %d (%v)", i, list[i].CreatedAt, i-1, list[i-1].CreatedAt)
		}
	}
}

func TestAuditRepo_ListBySubmission_UnknownReturnsEmpty(t *testing.T) {
	_, _, aud := setupDB(t)
	list, err := aud.ListBySubmission(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %d", len(list))
	}
}

func TestAuditRepo_Append_PersistsPayloadAsJSON(t *testing.T) {
	_, subs, aud := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_ = subs.UpsertSubmission(ctx, &model.Submission{
		ID: "sub-p", State: model.StateOpen, PolicyType: "cgl",
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	})

	payload := map[string]any{
		"missing_count": 2,
		"missing_ids":   []string{"acord_125", "loss_runs"},
	}
	if err := aud.Append(ctx, &model.AuditEntry{
		SubmissionID: "sub-p",
		EventType:    model.EventChecklistEvaluated,
		Payload:      payload,
	}); err != nil {
		t.Fatal(err)
	}

	list, _ := aud.ListBySubmission(ctx, "sub-p")
	if len(list) != 1 {
		t.Fatalf("got %d", len(list))
	}
	if list[0].Payload["missing_count"].(float64) != 2 {
		t.Errorf("missing_count: %+v", list[0].Payload["missing_count"])
	}
}
