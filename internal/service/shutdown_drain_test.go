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

// slowMail blocks the send on a release channel so the test controls its timing.
type slowMail struct {
	release chan struct{}
	sent    atomic.Int32
}

func (s *slowMail) Name() string { return "fake" }

func (s *slowMail) SendThreadedReply(_ context.Context, _ model.Reply) (string, error) {
	<-s.release
	s.sent.Add(1)
	return "msg-slow", nil
}

// canceling the inbound ctx must not stop the detached reply send.
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
		Repository:     &repository.Repository{Submissions: subs, Audit: aud, Outbox: newFakeOutbox()},
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

// ctxBlockingMail blocks the send until its context is canceled.
type ctxBlockingMail struct {
	started  chan struct{}
	canceled atomic.Bool
}

func (m *ctxBlockingMail) Name() string { return "fake" }

func (m *ctxBlockingMail) SendThreadedReply(ctx context.Context, _ model.Reply) (string, error) {
	close(m.started)
	<-ctx.Done()
	m.canceled.Store(true)
	return "", ctx.Err()
}

// Shutdown must return within the grace window even when a send is stuck.
func TestShutdown_BoundedWhenSendStuck(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &ctxBlockingMail{started: make(chan struct{})}
	cl := smallChecklist()

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	log := logrus.NewEntry(logrus.New())
	svc := NewSubmissionsService(Dependencies{
		Config: &config.Config{
			Escalation: config.EscalationConfig{ThresholdHours: 72},
			HTTP:       config.HTTPConfig{ShutdownTimeoutSec: 1}, // 1s drain grace
		},
		Repository:     &repository.Repository{Submissions: subs, Audit: aud, Outbox: newFakeOutbox()},
		EmailSender:    mail,
		Classifier:     &filenameClassifier{checklist: cl},
		ChecklistStore: &fakeStore{cl: cl},
		TextExtractors: map[string]TextExtractor{"application/pdf": fakeExtractor{}},
		Log:            log,
	})

	if _, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID:   "msg-stuck",
		FromAddress: "broker@example.com",
		Subject:     "New Submission - CGL",
		Attachments: []model.Attachment{
			{Filename: "ACORD_125.pdf", ContentType: "application/pdf"},
			{Filename: "ACORD_126.pdf", ContentType: "application/pdf"},
		},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	<-mail.started // ensure the send is in flight before we shut down

	start := time.Now()
	done := make(chan struct{})
	go func() {
		svc.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown hung past the grace window")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Shutdown took %v; should bail near the 1s grace", elapsed)
	}
	if !mail.canceled.Load() {
		t.Fatal("in-flight send was not canceled at shutdown")
	}
}
