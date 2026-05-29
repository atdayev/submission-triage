package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/repository"
	repomocks "github.com/atdayev/submission-triage/internal/repository/mocks"
)

// slowMail blocks SendThreadedReply on a release channel so the test can
// orchestrate "request context canceled before send completes" without
// relying on wall-clock timing.
type slowMail struct {
	release chan struct{}
	sent    atomic.Int32
}

func (s *slowMail) SendThreadedReply(_ context.Context, _ model.Reply) (string, error) {
	<-s.release
	s.sent.Add(1)
	return "msg-slow", nil
}

// If sendAndRecordReply ran on the inbound ctx instead of a detached one, the
// cancelation propagated below would short-circuit the send before release.
// We assert the send DID complete after release, proving context.WithoutCancel
// is in place and Wait() actually joins the in-flight goroutine.
func TestIngestEmail_ReplyGoroutineSurvivesCtxCancel(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &slowMail{release: make(chan struct{})}
	cl := smallChecklist()

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()

	replySentSeen := atomic.Bool{}
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		e := args.Get(1).(*model.AuditEntry)
		if e.EventType == model.EventReplySent {
			replySentSeen.Store(true)
		}
	})

	log := logrus.NewEntry(logrus.New())
	svc := NewSubmissionsService(Dependencies{
		Config:         &config.Config{Escalation: config.EscalationConfig{ThresholdHours: 72}},
		Repository:     &repository.Repository{Submissions: subs, Audit: aud},
		EmailSender:    mail,
		Classifier:     &filenameClassifier{checklist: cl},
		ChecklistStore: &fakeStore{cl: cl},
		TextExtractors: map[string]TextExtractor{"application/pdf": fakeExtractor{}},
		Log:            log,
	})

	ctx, cancel := context.WithCancel(context.Background())
	if _, err := svc.IngestEmail(ctx, IngestRequest{
		MessageID:   "msg-drain",
		FromAddress: "broker@example.com",
		Subject:     "New Submission - CGL",
		Attachments: []model.Attachment{
			{Filename: "ACORD_125.pdf", ContentType: "application/pdf"},
			{Filename: "ACORD_126.pdf", ContentType: "application/pdf"},
		},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// Cancel inbound ctx while the goroutine is still blocked in mail.Send.
	cancel()
	if mail.sent.Load() != 0 {
		t.Fatalf("send happened before release: got %d", mail.sent.Load())
	}

	// Release the send and drain.
	close(mail.release)
	doneWait := make(chan struct{})
	go func() {
		svc.Wait()
		close(doneWait)
	}()
	select {
	case <-doneWait:
	case <-time.After(2 * time.Second):
		t.Fatal("svc.Wait() did not return — goroutine leaked")
	}

	if mail.sent.Load() != 1 {
		t.Fatalf("send count: got %d, want 1 (ctx cancel should not have stopped detached send)", mail.sent.Load())
	}
	if !replySentSeen.Load() {
		t.Fatal("EventReplySent audit not written — goroutine bailed out instead of recording")
	}
}
