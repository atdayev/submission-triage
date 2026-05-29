package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/mock"

	"github.com/atdayev/submission-triage/internal/model"
	repomocks "github.com/atdayev/submission-triage/internal/repository/mocks"
)

// Without singleflight, N concurrent webhooks for the same email each fall
// through FindByEmailReference → NotFound, each call createSubmission with
// a fresh UUID, and we'd persist N orphan submission rows. With the gate,
// only the first call executes; the rest receive the same IngestResult.
func TestIngestEmail_ConcurrentSameEmail_SingleSubmission(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)

	var upsertCalls atomic.Int32
	var capturedID atomic.Value // string
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		upsertCalls.Add(1)
		s := args.Get(1).(*model.Submission)
		capturedID.Store(s.ID)
	})
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	svc := newSvc(t, subs, aud, mail, cl)

	const N = 10
	req := IngestRequest{
		MessageID:   "msg-concurrent",
		FromAddress: "broker@example.com",
		Subject:     "New Submission - CGL",
		BodyText:    "same body",
		Attachments: []model.Attachment{
			{Filename: "ACORD_125.pdf", ContentType: "application/pdf", Content: []byte("acord")},
		},
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]IngestResult, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = svc.IngestEmail(context.Background(), req)
		}(i)
	}
	close(start)
	wg.Wait()
	svc.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	if upsertCalls.Load() != 1 {
		t.Fatalf("UpsertSubmission was called %d times; expected exactly 1 (singleflight should collapse the rest)", upsertCalls.Load())
	}

	wantID := capturedID.Load().(string)
	for i, r := range results {
		if r.SubmissionID != wantID {
			t.Errorf("goroutine %d got submission_id=%q, want %q", i, r.SubmissionID, wantID)
		}
	}
}
