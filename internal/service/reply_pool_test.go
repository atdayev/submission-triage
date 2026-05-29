package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/repository"
	repomocks "github.com/atdayev/submission-triage/internal/repository/mocks"
)

// blockingMail blocks SendThreadedReply on a release channel — useful for
// saturating the worker pool deterministically.
type blockingMail struct {
	release chan struct{}
}

func (b *blockingMail) SendThreadedReply(_ context.Context, _ model.Reply) (string, error) {
	<-b.release
	return "ok", nil
}

func newPoolSvc(t *testing.T, mail *blockingMail, workers, queue int) (*SubmissionsService, *repomocks.AuditRepository) {
	t.Helper()
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	cl := smallChecklist()

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	log := logrus.NewEntry(logrus.New())
	svc := NewSubmissionsService(Dependencies{
		Config: &config.Config{
			Escalation: config.EscalationConfig{ThresholdHours: 72},
			Reply:      config.ReplyConfig{Workers: workers, QueueSize: queue},
		},
		Repository:     &repository.Repository{Submissions: subs, Audit: aud},
		EmailSender:    mail,
		Classifier:     &filenameClassifier{checklist: cl},
		ChecklistStore: &fakeStore{cl: cl},
		TextExtractors: map[string]TextExtractor{"application/pdf": fakeExtractor{}},
		Log:            log,
	})
	return svc, aud
}

func TestReplyPool_SaturatedQueueDropsAndAudits(t *testing.T) {
	mail := &blockingMail{release: make(chan struct{})}
	defer close(mail.release)

	// 1 worker + queue size 1 = at most 2 jobs in flight (1 executing, 1 queued).
	svc, aud := newPoolSvc(t, mail, 1, 1)

	var dropAudits int
	var auditMu sync.Mutex
	aud.Calls = nil // reset prior expectations counter
	aud.ExpectedCalls = nil
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		e := args.Get(1).(*model.AuditEntry)
		if e.EventType == model.EventReplyFailed {
			auditMu.Lock()
			if reason, _ := e.Payload["error"].(string); reason == "reply queue full; reply dropped" {
				dropAudits++
			}
			auditMu.Unlock()
		}
	})

	const N = 8
	results := make([]IngestResult, N)
	for i := 0; i < N; i++ {
		r, err := svc.IngestEmail(context.Background(), IngestRequest{
			MessageID:   "msg-" + string(rune('a'+i)),
			FromAddress: "broker@example.com",
			Subject:     "New Submission - CGL",
			Attachments: []model.Attachment{{Filename: "ACORD_125.pdf", ContentType: "application/pdf"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		results[i] = r
	}

	queued, dropped := 0, 0
	for _, r := range results {
		if r.ReplyQueued {
			queued++
		} else {
			dropped++
		}
	}
	if queued > 2 {
		t.Errorf("queued=%d; pool of 1 worker + queue 1 should accept at most 2 concurrent enqueues", queued)
	}
	if dropped == 0 {
		t.Error("expected at least one drop with saturated pool")
	}
	auditMu.Lock()
	if dropAudits != dropped {
		t.Errorf("drop count mismatch: ReplyQueued=false %d times, audit drops %d", dropped, dropAudits)
	}
	auditMu.Unlock()
}

func TestReplyPool_ShutdownDrains(t *testing.T) {
	mail := &blockingMail{release: make(chan struct{})}
	svc, _ := newPoolSvc(t, mail, 2, 4)

	// Enqueue 3 jobs; all should accept (workers + queue = 6 capacity).
	for i := 0; i < 3; i++ {
		_, err := svc.IngestEmail(context.Background(), IngestRequest{
			MessageID:   "shutdown-" + string(rune('a'+i)),
			FromAddress: "broker@example.com",
			Subject:     "New Submission - CGL",
			Attachments: []model.Attachment{{Filename: "ACORD_125.pdf", ContentType: "application/pdf"}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Release sends, then Shutdown — must return after all 3 process.
	close(mail.release)
	done := make(chan struct{})
	go func() {
		svc.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return — workers leaked")
	}
}
