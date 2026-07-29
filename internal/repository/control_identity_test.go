package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/atdayev/submission-triage/internal/model"
)

func idsOf(subs []model.Submission) []string {
	out := make([]string, len(subs))
	for i := range subs {
		out[i] = subs[i].ID
	}
	return out
}

func TestSubmissionRepo_Migration0008_RoundtripsNewFields(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	eff := now.Add(72 * time.Hour)
	lastEsc := now.Add(-time.Hour)
	sub := &model.Submission{
		ID: "s1", PolicyType: "cgl", State: model.StateAwaiting, FromAddress: "broker@x",
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
		NamedInsured: "Acme Manufacturing", EffectiveDate: &eff, OnHold: true, DeliveryFailed: true, LastEscalatedAt: &lastEsc,
		Emails:    []model.Email{{DeterministicID: "e1", SubmissionID: "s1", Direction: model.DirectionInbound, MessageID: "m1", ReplyTo: "producer@x", ReceivedAt: now}},
		Documents: []model.Document{{ID: "d1", SubmissionID: "s1", Filename: "a.pdf", ClassifiedAs: "acord_125", Encrypted: true, CreatedAt: now}},
	}
	if err := subs.UpsertSubmission(ctx, sub); err != nil {
		t.Fatal(err)
	}
	got, err := subs.GetByID(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.NamedInsured != "Acme Manufacturing" {
		t.Errorf("named_insured = %q", got.NamedInsured)
	}
	if got.EffectiveDate == nil || !got.EffectiveDate.Equal(eff) {
		t.Errorf("effective_date = %v, want %v", got.EffectiveDate, eff)
	}
	if !got.OnHold {
		t.Error("on_hold not persisted")
	}
	if !got.DeliveryFailed {
		t.Error("delivery_failed not persisted")
	}
	if got.LastEscalatedAt == nil || !got.LastEscalatedAt.Equal(lastEsc) {
		t.Errorf("last_escalated_at = %v", got.LastEscalatedAt)
	}
	if len(got.Documents) != 1 || !got.Documents[0].Encrypted {
		t.Errorf("document encrypted not persisted: %+v", got.Documents)
	}
	if len(got.Emails) != 1 || got.Emails[0].ReplyTo != "producer@x" {
		t.Errorf("email reply_to not persisted: %+v", got.Emails)
	}
}

func TestSubmissionRepo_ListBindSoon(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	mk := func(id string, state model.State, effOffset time.Duration, onHold, delivFailed bool) {
		eff := now.Add(effOffset)
		if err := subs.UpsertSubmission(ctx, &model.Submission{
			ID: id, PolicyType: "cgl", State: state, FromAddress: "b@x",
			CreatedAt: now, UpdatedAt: now, LastActionAt: now,
			EffectiveDate: &eff, OnHold: onHold, DeliveryFailed: delivFailed,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("soon2", model.StateAwaiting, 2*24*time.Hour, false, false)
	mk("soon5", model.StateAwaiting, 5*24*time.Hour, false, false)
	mk("far", model.StateAwaiting, 30*24*time.Hour, false, false)
	mk("held", model.StateAwaiting, 1*24*time.Hour, true, false)
	mk("bounced", model.StateAwaiting, 1*24*time.Hour, false, true)
	mk("complete", model.StateComplete, 1*24*time.Hour, false, false)
	if err := subs.UpsertSubmission(ctx, &model.Submission{
		ID: "noeff", PolicyType: "cgl", State: model.StateAwaiting, FromAddress: "b@x",
		CreatedAt: now, UpdatedAt: now, LastActionAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := subs.ListBindSoon(ctx, 0, now.Add(7*24*time.Hour).UnixNano(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsOf(got); !reflect.DeepEqual(ids, []string{"soon2", "soon5"}) {
		t.Fatalf("ListBindSoon = %v, want [soon2 soon5] (excl. far/held/bounced/complete/noeff, asc)", ids)
	}
}

func TestSubmissionRepo_FindContentMatch(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seed := func(id, insured, from string, state model.State, createdOffset time.Duration) {
		if err := subs.UpsertSubmission(ctx, &model.Submission{
			ID: id, PolicyType: "cgl", State: state, NamedInsured: insured, FromAddress: from,
			CreatedAt: now.Add(createdOffset), UpdatedAt: now, LastActionAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("m1", "Acme Manufacturing", "broker@x", model.StateAwaiting, -2*24*time.Hour)
	seed("old", "Acme Manufacturing", "broker@x", model.StateAwaiting, -30*24*time.Hour)
	seed("closed", "Acme Manufacturing", "broker@x", model.StateClosed, -1*24*time.Hour)
	seed("other", "Acme Manufacturing", "someoneelse@x", model.StateAwaiting, -1*24*time.Hour)

	cutoff := now.Add(-14 * 24 * time.Hour).UnixNano()
	got, err := subs.FindContentMatch(ctx, "  acme MANUFACTURING ", "broker@x", cutoff)
	if err != nil {
		t.Fatalf("expected a normalized case-insensitive match: %v", err)
	}
	if got.ID != "m1" {
		t.Errorf("matched %s, want m1 (in-window, awaiting, same sender)", got.ID)
	}
	if _, err := subs.FindContentMatch(ctx, "Beta Corp", "broker@x", cutoff); !errors.Is(err, model.ErrSubmissionNotFound) {
		t.Errorf("a different insured must not match, got %v", err)
	}
}

func TestSubmissionRepo_HoldQueries(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seed := func(id, msgID string) {
		if err := subs.UpsertSubmission(ctx, &model.Submission{
			ID: id, PolicyType: "cgl", State: model.StateAwaiting, FromAddress: "b@x",
			CreatedAt: now, UpdatedAt: now, LastActionAt: now,
			Emails: []model.Email{{DeterministicID: "det-" + id, SubmissionID: id, Direction: model.DirectionInbound, MessageID: msgID, ReceivedAt: now}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("A", "msg-A")
	seed("B", "msg-B")

	ids, err := subs.SubmissionIDsByMessageIDs(ctx, []string{"msg-A", "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"A"}) {
		t.Fatalf("SubmissionIDsByMessageIDs = %v, want [A]", ids)
	}

	if err := subs.SetOnHold(ctx, []string{"A"}, true); err != nil {
		t.Fatal(err)
	}
	held, err := subs.ListHeldIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(held, []string{"A"}) {
		t.Fatalf("ListHeldIDs = %v, want [A]", held)
	}

	if err := subs.SetOnHold(ctx, []string{"A"}, false); err != nil {
		t.Fatal(err)
	}
	if held, _ := subs.ListHeldIDs(ctx); len(held) != 0 {
		t.Fatalf("ListHeldIDs after release = %v, want none", held)
	}
}

func TestSubmissionRepo_ListStaleExcludesHeldAndDeliveryFailed(t *testing.T) {
	_, subs, _ := setupDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-100 * time.Hour)
	seed := func(id string, onHold, df bool) {
		if err := subs.UpsertSubmission(ctx, &model.Submission{
			ID: id, PolicyType: "cgl", State: model.StateAwaiting, FromAddress: "b@x",
			CreatedAt: old, UpdatedAt: old, LastActionAt: old, OnHold: onHold, DeliveryFailed: df,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("plain", false, false)
	seed("held", true, false)
	seed("bounced", false, true)

	got, err := subs.ListStale(ctx, time.Now().UTC().Add(-time.Hour).UnixNano(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsOf(got); !reflect.DeepEqual(ids, []string{"plain"}) {
		t.Fatalf("ListStale = %v, want [plain] (held and delivery-failed excluded)", ids)
	}
}
