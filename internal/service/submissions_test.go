package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/infrastructure/classifier"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/repository"
	repomocks "github.com/atdayev/submission-triage/internal/repository/mocks"
	"github.com/atdayev/submission-triage/pkg/glob"
)

type fakeMail struct {
	mu         sync.Mutex
	sent       []model.Reply
	shouldFail bool
}

func (f *fakeMail) SendThreadedReply(_ context.Context, r model.Reply) (string, error) {
	if f.shouldFail {
		return "", context.DeadlineExceeded
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, r)
	return "msg-fake-1", nil
}

func (f *fakeMail) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type fakeStore struct {
	cl model.Checklist
}

func (f *fakeStore) Get(string) (model.Checklist, bool) { return f.cl, true }
func (f *fakeStore) All() []model.Checklist             { return []model.Checklist{f.cl} }

type fakeExtractor struct{}

func (fakeExtractor) Extract(b []byte) (string, error) { return string(b), nil }

type filenameClassifier struct {
	checklist model.Checklist
}

func (f *filenameClassifier) Classify(_ context.Context, in classifier.Input) (classifier.Result, error) {
	for _, item := range f.checklist.Required {
		if glob.MatchAny(item.Match.FilenamePatterns, in.Filename) {
			return classifier.Result{CandidateID: item.ID, Confidence: 0.95, By: "heuristic"}, nil
		}
	}
	return classifier.Result{By: "heuristic"}, nil
}

func smallChecklist() model.Checklist {
	return model.Checklist{
		Name:       "Test",
		PolicyType: "cgl",
		Required: []model.RequiredItem{
			{ID: "acord_125", Description: "ACORD 125", Match: model.MatchRules{FilenamePatterns: []string{"*ACORD*125*"}}},
			{ID: "acord_126", Description: "ACORD 126", Match: model.MatchRules{FilenamePatterns: []string{"*ACORD*126*"}}},
		},
	}
}

func newSvc(t *testing.T, subs *repomocks.SubmissionRepository, aud *repomocks.AuditRepository, mail *fakeMail, cl model.Checklist) *SubmissionsService {
	t.Helper()
	log := logrus.NewEntry(logrus.New())
	repo := &repository.Repository{Submissions: subs, Audit: aud}
	return NewSubmissionsService(Dependencies{
		Config:         &config.Config{Escalation: config.EscalationConfig{ThresholdHours: 72}},
		Repository:     repo,
		EmailSender:    mail,
		Classifier:     &filenameClassifier{checklist: cl},
		ChecklistStore: &fakeStore{cl: cl},
		TextExtractors: map[string]TextExtractor{
			"application/pdf": fakeExtractor{},
		},
		Log: log,
	})
}

func TestIngestEmail_NewSubmission_Complete(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	svc := newSvc(t, subs, aud, mail, cl)
	now := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	svc.setClock(func() time.Time { return now })

	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID:   "msg-1",
		FromAddress: "broker@example.com",
		FromName:    "Broker",
		Subject:     "New Submission - CGL",
		Attachments: []model.Attachment{
			{Filename: "ACORD_125_X.pdf", ContentType: "application/pdf", Content: []byte("ACORD 125")},
			{Filename: "ACORD_126_X.pdf", ContentType: "application/pdf", Content: []byte("ACORD 126")},
		},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.State != model.StateComplete {
		t.Fatalf("state: got %s, want complete", res.State)
	}
	if len(res.MissingItems) != 0 {
		t.Fatalf("missing: got %v, want none", res.MissingItems)
	}
	if !res.ReplyQueued {
		t.Fatal("expected completion reply to be queued")
	}
	svc.Wait()
	if mail.sentCount() != 1 {
		t.Fatalf("expected 1 sent reply, got %d", mail.sentCount())
	}
}

func TestIngestEmail_DuplicateSecondIngest(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()

	var stored *model.Submission

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ []string) *model.Submission { return stored },
		func(_ context.Context, _ []string) bool { return false },
		func(_ context.Context, _ []string) error {
			if stored == nil {
				return model.ErrSubmissionNotFound
			}
			return nil
		},
	)
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		s := args.Get(1).(*model.Submission)
		cp := *s
		cp.Emails = append([]model.Email{}, s.Emails...)
		cp.Documents = append([]model.Document{}, s.Documents...)
		stored = &cp
	})
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	svc := newSvc(t, subs, aud, mail, cl)
	svc.setClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })

	req := IngestRequest{
		MessageID:   "msg-dup",
		FromAddress: "x@y",
		Subject:     "Sub",
		Attachments: []model.Attachment{{Filename: "ACORD_125.pdf", ContentType: "application/pdf", Content: []byte("a125")}},
	}
	if _, err := svc.IngestEmail(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	res2, err := svc.IngestEmail(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	if !res2.IsDuplicate {
		t.Fatalf("expected duplicate, got %+v", res2)
	}
}

func TestIngestEmail_ReplyFailureKeepsState(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{shouldFail: true}
	cl := smallChecklist()

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil)

	foundReplyFailed := false
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		e := args.Get(1).(*model.AuditEntry)
		if e.EventType == model.EventReplyFailed {
			foundReplyFailed = true
		}
	})

	svc := newSvc(t, subs, aud, mail, cl)
	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "msg-X", Subject: "S",
		Attachments: []model.Attachment{{Filename: "no_match.pdf", ContentType: "application/pdf", Content: []byte("")}},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.State != model.StateAwaiting {
		t.Fatalf("state: got %s, want awaiting", res.State)
	}
	svc.Wait()
	if mail.sentCount() != 0 {
		t.Fatalf("expected 0 sent replies, got %d", mail.sentCount())
	}
	if !foundReplyFailed {
		t.Fatal("expected EventReplyFailed in audit log")
	}
}

func TestCheckEscalations(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	stale := model.Submission{ID: "s1", State: model.StateAwaiting, LastActionAt: now.Add(-100 * time.Hour)}

	subs.On("ListStale", mock.Anything, mock.Anything, mock.Anything).Return([]model.Submission{stale}, nil)
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil)

	foundEscalated := false
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		e := args.Get(1).(*model.AuditEntry)
		if e.EventType == model.EventEscalated {
			foundEscalated = true
		}
	})

	svc := newSvc(t, subs, aud, mail, cl)
	svc.setClock(func() time.Time { return now })

	if err := svc.CheckEscalations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !foundEscalated {
		t.Fatal("expected EventEscalated audit entry")
	}
}
