package service

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/atdayev/submission-triage/internal/infrastructure/classifier"
	"github.com/atdayev/submission-triage/internal/model"
	repomocks "github.com/atdayev/submission-triage/internal/repository/mocks"
)

// blockingClassifier parks the singleflight leader inside Classify so the test
// can prove the other goroutines join the in-flight call before it returns.
type blockingClassifier struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingClassifier) Classify(_ context.Context, _ classifier.Input) (classifier.Result, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return classifier.Result{By: "heuristic"}, nil
}

// without singleflight, concurrent ingests of one email create N orphan rows;
// the gate collapses them so only the first executes.
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

	bc := &blockingClassifier{entered: make(chan struct{}), release: make(chan struct{})}
	svc := newSvcWithClassifier(t, subs, aud, mail, cl, bc)

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
	var launched atomic.Int32
	var wg sync.WaitGroup
	results := make([]IngestResult, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			launched.Add(1)
			results[i], errs[i] = svc.IngestEmail(context.Background(), req)
		}(i)
	}
	close(start)

	// leader holds the singleflight key; wait for all goroutines, then let them park
	<-bc.entered
	for launched.Load() < int32(N) {
		runtime.Gosched()
	}
	time.Sleep(20 * time.Millisecond)
	close(bc.release)

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
