package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/mock"

	"github.com/atdayev/submission-triage/internal/model"
	repomocks "github.com/atdayev/submission-triage/internal/repository/mocks"
)

// A redelivered header-less email must dedup, not spawn a second submission/reply.
func TestIngestEmail_DedupEmptyMessageID(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)

	var created *model.Submission
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		created = args.Get(1).(*model.Submission)
	})
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	// first lookup finds nothing (new); second returns the just-created submission
	subs.On("FindByDeterministicID", mock.Anything, mock.Anything).
		Return(nil, model.ErrSubmissionNotFound).Once()
	subs.On("FindByDeterministicID", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ string) *model.Submission { return created },
		func(_ context.Context, _ string) error { return nil },
	)

	svc := newSvc(t, subs, aud, mail, cl)

	req := IngestRequest{
		FromAddress: "broker@example.com",
		Subject:     "New Submission - CGL",
		BodyText:    "no message id on this one",
		// no MessageID, no In-Reply-To, no References
	}

	r1, err := svc.IngestEmail(context.Background(), req)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if r1.IsDuplicate {
		t.Fatal("first ingest should not be a duplicate")
	}

	r2, err := svc.IngestEmail(context.Background(), req)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if !r2.IsDuplicate {
		t.Fatal("second ingest of the identical header-less email should dedup")
	}
	if r2.SubmissionID != r1.SubmissionID {
		t.Errorf("dedup produced a different submission: %q vs %q", r2.SubmissionID, r1.SubmissionID)
	}

	svc.Wait()
	if mail.sentCount() != 1 {
		t.Errorf("expected exactly one reply (no duplicate), got %d", mail.sentCount())
	}
}
