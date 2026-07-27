package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/infrastructure/checklist"
	"github.com/atdayev/submission-triage/internal/infrastructure/classifier"
	"github.com/atdayev/submission-triage/internal/infrastructure/llm"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/repository"
	repomocks "github.com/atdayev/submission-triage/internal/repository/mocks"
	"github.com/atdayev/submission-triage/pkg/glob"
)

// fakeOutbox is an in-memory OutboxRepository for tests.
type fakeOutbox struct {
	mu      sync.Mutex
	entries map[string]*model.OutboxEntry
	seq     int
	now     func() time.Time
}

func newFakeOutbox() *fakeOutbox {
	return &fakeOutbox{entries: map[string]*model.OutboxEntry{}, now: time.Now}
}

func (f *fakeOutbox) Enqueue(_ context.Context, e *model.OutboxEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// coalesce: one pending row per submission (mirrors the partial unique index)
	for _, ex := range f.entries {
		if ex.Status == model.OutboxPending && ex.SubmissionID == e.SubmissionID {
			ex.Reply = e.Reply
			ex.NotBefore = e.NotBefore
			ex.UpdatedAt = f.now()
			e.ID = ex.ID // RETURNING id
			return nil
		}
	}
	if e.ID == "" {
		f.seq++
		e.ID = fmt.Sprintf("ob-%d", f.seq)
	}
	if e.Status == "" {
		e.Status = model.OutboxPending
	}
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = f.now()
	}
	cp := *e
	f.entries[e.ID] = &cp
	return nil
}

// ListPending mirrors the SQL gate: due when not_before <= now, and for already-
// attempted rows only, when the retry backoff has elapsed (updated_at <= cutoff).
func (f *fakeOutbox) ListPending(_ context.Context, now, retryCutoff time.Time, _ int) ([]model.OutboxEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.OutboxEntry
	for _, e := range f.entries {
		if e.Status != model.OutboxPending {
			continue
		}
		if e.NotBefore.After(now) {
			continue
		}
		if e.Attempts > 0 && e.UpdatedAt.After(retryCutoff) {
			continue
		}
		out = append(out, *e)
	}
	return out, nil
}

func (f *fakeOutbox) Update(_ context.Context, id string, status model.OutboxStatus, attempts int, lastErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.entries[id]; ok {
		e.Status, e.Attempts, e.LastError = status, attempts, lastErr
		e.UpdatedAt = f.now()
	}
	return nil
}

// MarkSent marks the row sent only if unchanged since version (mirrors the CAS).
func (f *fakeOutbox) MarkSent(_ context.Context, id string, version time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.entries[id]; ok && e.Status == model.OutboxPending && e.UpdatedAt.Equal(version) {
		e.Status = model.OutboxSent
		e.UpdatedAt = f.now()
	}
	return nil
}

type fakeMail struct {
	mu         sync.Mutex
	sent       []model.Reply
	shouldFail bool
}

func (f *fakeMail) Name() string { return "fake" }

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
			return classifier.Result{CandidateID: item.ID, Confidence: 0.95, By: model.ClassifiedByFilename}, nil
		}
	}
	return classifier.Result{By: model.ClassifiedByHeuristic}, nil
}

// fixedConfidenceClassifier classifies every attachment to id at a fixed confidence.
type fixedConfidenceClassifier struct {
	id         string
	confidence float64
}

func (f *fixedConfidenceClassifier) Classify(_ context.Context, _ classifier.Input) (classifier.Result, error) {
	return classifier.Result{CandidateID: f.id, Confidence: f.confidence, By: "llm"}, nil
}

// cappedClassifier mimics the classifier degrading to heuristics when the LLM
// spend cap is reached: no candidate, Capped set.
type cappedClassifier struct{}

func (cappedClassifier) Classify(_ context.Context, _ classifier.Input) (classifier.Result, error) {
	return classifier.Result{By: "heuristic", Capped: true}, nil
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

// oneItemChecklist has a single required item, so a single flagged document
// leaves nothing broker-actionable — the flag-only path.
func oneItemChecklist() model.Checklist {
	return model.Checklist{
		Name:       "Test",
		PolicyType: "cgl",
		Required: []model.RequiredItem{
			{ID: "acord_125", Description: "ACORD 125", Match: model.MatchRules{FilenamePatterns: []string{"*ACORD*125*"}}},
		},
	}
}

func newSvc(t *testing.T, subs *repomocks.SubmissionRepository, aud *repomocks.AuditRepository, mail *fakeMail, cl model.Checklist) *SubmissionsService {
	t.Helper()
	return newSvcWithClassifier(t, subs, aud, mail, cl, &filenameClassifier{checklist: cl})
}

func newSvcWithClassifier(t *testing.T, subs *repomocks.SubmissionRepository, aud *repomocks.AuditRepository, mail *fakeMail, cl model.Checklist, clf classifier.Classifier) *SubmissionsService {
	t.Helper()
	// the new-submission path dedups by deterministic id; default to "not found"
	subs.On("FindByDeterministicID", mock.Anything, mock.Anything).Return(nil, model.ErrSubmissionNotFound).Maybe()
	subs.On("SetLastReplyAt", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	subs.On("FilenameOnlyItems", mock.Anything, mock.Anything).Return(map[string][]string(nil), nil).Maybe()
	log := logrus.NewEntry(logrus.New())
	repo := &repository.Repository{Submissions: subs, Audit: aud, Outbox: newFakeOutbox()}
	return NewSubmissionsService(Dependencies{
		Config:         &config.Config{Escalation: config.EscalationConfig{ThresholdHours: 72}},
		Repository:     repo,
		EmailSender:    mail,
		Classifier:     clf,
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
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil)
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
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
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
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil)

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

// a persist failure must surface as an error so the poller retries, not mark seen
func TestIngestEmail_PersistFailureReturnsError(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(errors.New("db down"))
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()

	svc := newSvc(t, subs, aud, mail, cl)
	_, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "msg-persist-fail",
		Subject:   "New Submission - CGL",
		Attachments: []model.Attachment{
			{Filename: "ACORD_125.pdf", ContentType: "application/pdf", Content: []byte("a125")},
		},
	})
	if err == nil {
		t.Fatal("expected ingest to fail when persistence fails")
	}
	svc.Wait()
	if mail.sentCount() != 0 {
		t.Fatalf("no reply should be sent when persistence failed, got %d", mail.sentCount())
	}
	// a failed persist must leave no orphan audit rows; they would duplicate on retry
	aud.AssertNotCalled(t, "Append", mock.Anything, mock.Anything)
}

func TestIngestEmail_AutoSubmitted_SuppressesReply(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()

	priorAction := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	existing := &model.Submission{
		ID:           "sub-ooo",
		PolicyType:   "cgl",
		State:        model.StateAwaiting,
		LastActionAt: priorAction,
		UpdatedAt:    priorAction,
		Emails:       []model.Email{{DeterministicID: "first", MessageID: "first-msg"}},
		Documents:    []model.Document{{ID: "doc-125", ClassifiedAs: "acord_125"}},
		MissingItems: []model.MissingItem{{ID: "acord_126", Description: "ACORD 126"}},
	}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(existing, false, nil)

	var captured *model.Submission
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		captured = args.Get(1).(*model.Submission)
	})

	suppressed := false
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		if args.Get(1).(*model.AuditEntry).EventType == model.EventReplySuppressed {
			suppressed = true
		}
	})

	svc := newSvc(t, subs, aud, mail, cl)
	svc.setClock(func() time.Time { return time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC) })

	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID:           "ooo-msg",
		InReplyTo:           "first-msg",
		FromAddress:         "broker@example.com",
		Subject:             "Re: CGL Submission",
		AutoSubmitted:       true,
		AutoResponseHeaders: map[string]string{"auto-submitted": "auto-replied"},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.ReplyQueued {
		t.Error("auto-submitted mail must not queue a reply")
	}
	svc.Wait()
	if mail.sentCount() != 0 {
		t.Fatalf("no reply should be sent for auto-submitted mail, got %d", mail.sentCount())
	}
	if !suppressed {
		t.Fatal("expected reply.suppressed audit entry")
	}
	if captured == nil {
		t.Fatal("submission never persisted")
	}
	if !captured.LastActionAt.Equal(priorAction) {
		t.Errorf("last_action_at advanced on auto-submitted mail: got %v, want %v", captured.LastActionAt, priorAction)
	}
	subs.AssertNotCalled(t, "UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything)
}

func TestIngestEmail_AutoSubmitted_PersistFailureReturnsError(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(errors.New("db down"))
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()

	svc := newSvc(t, subs, aud, mail, cl)
	_, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID:           "auto-persist-fail",
		Subject:             "New Submission - CGL",
		AutoSubmitted:       true,
		AutoResponseHeaders: map[string]string{"precedence": "bulk"},
	})
	if err == nil {
		t.Fatal("expected a suppressed-persist failure to return an error so the poller retries")
	}
	svc.Wait()
	if mail.sentCount() != 0 {
		t.Fatalf("no reply should be sent for suppressed mail, got %d", mail.sentCount())
	}
	// a failed persist must leave no orphan audit rows; they would duplicate on retry
	aud.AssertNotCalled(t, "Append", mock.Anything, mock.Anything)
}

func TestIngestEmail_FlagOnly_NoReplyWhenNothingToAsk(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := oneItemChecklist()
	lowConf := &fixedConfidenceClassifier{id: "acord_125", confidence: 0.60}

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	var captured *model.Submission
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		captured = args.Get(1).(*model.Submission)
	})

	needsReviewAudited := false
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		if args.Get(1).(*model.AuditEntry).EventType == model.EventNeedsReview {
			needsReviewAudited = true
		}
	})

	svc := newSvcWithClassifier(t, subs, aud, mail, cl, lowConf)
	svc.cfg.Classifier.ConfidenceFloor = 0.80

	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "low-conf", Subject: "New Submission - CGL",
		Attachments: []model.Attachment{{Filename: "mystery.pdf", ContentType: "application/pdf", Content: []byte("x")}},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// review is a flag, not a state: the submission stays awaiting, flagged.
	if res.State != model.StateAwaiting {
		t.Fatalf("state: got %s, want awaiting", res.State)
	}
	if !res.NeedsReview {
		t.Error("expected the submission to be flagged for review")
	}
	if res.ReplyQueued {
		t.Error("a flag-only submission must not queue a broker reply")
	}
	svc.Wait()
	if mail.sentCount() != 0 {
		t.Fatalf("no reply expected when everything outstanding is flagged, got %d", mail.sentCount())
	}
	if !needsReviewAudited {
		t.Error("expected submission.needs_review audit entry")
	}
	if captured == nil || !captured.NeedsReview || captured.State != model.StateAwaiting {
		t.Fatalf("submission should persist awaiting+flagged, got %+v", captured)
	}
	subs.AssertNotCalled(t, "UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything)
}

// the headline Fix Spec 3 case: an unverifiable document flags the submission
// but must not suppress the request for the documents that are genuinely absent.
func TestIngestEmail_LowConfidence_FlagsButStillRequestsAbsent(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist() // acord_125 + acord_126
	lowConf := &fixedConfidenceClassifier{id: "acord_125", confidence: 0.60}

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	var captured *model.Submission
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		captured = args.Get(1).(*model.Submission)
	})
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	svc := newSvcWithClassifier(t, subs, aud, mail, cl, lowConf)
	svc.cfg.Classifier.ConfidenceFloor = 0.80

	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "low-conf-mixed", Subject: "New Submission - CGL", FromAddress: "b@x",
		Attachments: []model.Attachment{{Filename: "mystery.pdf", ContentType: "application/pdf", Content: []byte("x")}},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.State != model.StateAwaiting || !res.NeedsReview || !res.ReplyQueued {
		t.Fatalf("want awaiting+flagged+reply, got state=%s review=%v queued=%v", res.State, res.NeedsReview, res.ReplyQueued)
	}
	if captured == nil || !captured.NeedsReview {
		t.Fatalf("submission should persist flagged, got %+v", captured)
	}
	svc.Wait()
	if mail.sentCount() != 1 {
		t.Fatalf("expected the absent item to be requested, got %d replies", mail.sentCount())
	}
	body := mail.sent[0].BodyText
	if !contains(body, "ACORD 126") {
		t.Errorf("reply should ask for the genuinely-absent ACORD 126:\n%s", body)
	}
	if contains(body, "ACORD 125") {
		t.Errorf("reply must not mention the flagged (low-confidence) ACORD 125:\n%s", body)
	}
}

func TestIngestEmail_SpendCapped_AuditsOnceAndFallsBack(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()

	cappedCount := 0
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		if args.Get(1).(*model.AuditEntry).EventType == model.EventLLMCapped {
			cappedCount++
		}
	})

	svc := newSvcWithClassifier(t, subs, aud, mail, cl, cappedClassifier{})
	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "capped", Subject: "New Submission - CGL",
		Attachments: []model.Attachment{
			{Filename: "a.pdf", ContentType: "application/pdf", Content: []byte("a")},
			{Filename: "b.pdf", ContentType: "application/pdf", Content: []byte("b")},
			{Filename: "c.pdf", ContentType: "application/pdf", Content: []byte("c")},
		},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// classification degraded to heuristics: nothing classified, so items are missing
	if res.State != model.StateAwaiting {
		t.Fatalf("state: got %s, want awaiting", res.State)
	}
	if cappedCount != 1 {
		t.Fatalf("llm.capped should be audited exactly once across attachments, got %d", cappedCount)
	}
	svc.Wait()
}

// an escalated submission whose only outstanding item is flagged de-escalates to
// awaiting (broker has nothing to do) and sets the review flag — no reply.
func TestIngestEmail_EscalatedThenLowConfidence_FlagsAndDeEscalates(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := oneItemChecklist()
	lowConf := &fixedConfidenceClassifier{id: "acord_125", confidence: 0.60}

	existing := &model.Submission{
		ID: "esc-1", PolicyType: "cgl", State: model.StateEscalated,
		Emails: []model.Email{{DeterministicID: "first", MessageID: "first-msg"}},
	}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(existing, false, nil)
	var captured *model.Submission
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		captured = args.Get(1).(*model.Submission)
	})
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	svc := newSvcWithClassifier(t, subs, aud, mail, cl, lowConf)
	svc.cfg.Classifier.ConfidenceFloor = 0.80

	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID:   "reply-msg",
		InReplyTo:   "first-msg",
		FromAddress: "broker@example.com",
		Subject:     "Re: CGL",
		Attachments: []model.Attachment{{Filename: "mystery.pdf", ContentType: "application/pdf", Content: []byte("x")}},
	})
	if err != nil {
		t.Fatalf("ingest must not fail on escalated+low-confidence: %v", err)
	}
	if res.State != model.StateAwaiting || !res.NeedsReview {
		t.Fatalf("want awaiting+flagged, got state=%s review=%v", res.State, res.NeedsReview)
	}
	if captured == nil || captured.State != model.StateAwaiting || !captured.NeedsReview {
		t.Fatalf("submission must persist awaiting+flagged, got %+v", captured)
	}
	svc.Wait()
	subs.AssertNotCalled(t, "UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything)
}

func TestIngestEmail_LowConfidence_PersistFailureReturnsError(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := oneItemChecklist()
	lowConf := &fixedConfidenceClassifier{id: "acord_125", confidence: 0.60}

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(errors.New("db down"))
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()

	svc := newSvcWithClassifier(t, subs, aud, mail, cl, lowConf)
	svc.cfg.Classifier.ConfidenceFloor = 0.80

	_, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID:   "lc-fail",
		Subject:     "New Submission - CGL",
		Attachments: []model.Attachment{{Filename: "mystery.pdf", ContentType: "application/pdf", Content: []byte("x")}},
	})
	if err == nil {
		t.Fatal("expected a needs-review persist failure to return an error so the poller retries")
	}
	svc.Wait()
	if mail.sentCount() != 0 {
		t.Fatalf("no reply for needs_review, got %d", mail.sentCount())
	}
	// audits must not flush before a successful commit
	aud.AssertNotCalled(t, "Append", mock.Anything, mock.Anything)
}

func TestSendDigest_RendersOpenSubmissions(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	open := []model.Submission{
		{ID: "a1", PolicyType: "cgl", State: model.StateAwaiting, NeedsReview: true, SubjectLine: "New Sub A", FromAddress: "a@x",
			CreatedAt: now.Add(-2 * time.Hour), LastActionAt: now.Add(-time.Hour),
			MissingItems: []model.MissingItem{{ID: "acord_125", Description: "ACORD 125", Reason: "received but not confidently identified", Code: model.ReasonLowConfidence}}},
		{ID: "b1", PolicyType: "cgl", State: model.StateAwaiting, SubjectLine: "New Sub B", FromAddress: "b@x",
			CreatedAt: now.Add(-3 * time.Hour), LastActionAt: now.Add(-3 * time.Hour),
			MissingItems: []model.MissingItem{{ID: "acord_126", Description: "ACORD 126", Reason: "document not provided", Code: model.ReasonNotProvided}}},
	}
	subs.On("ListOpen", mock.Anything, mock.Anything).Return(open, nil)

	digestSent := false
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		if args.Get(1).(*model.AuditEntry).EventType == model.EventDigestSent {
			digestSent = true
		}
	})

	svc := newSvcWith(t, subs, aud, mail, store, nil)
	svc.setClock(func() time.Time { return now })

	if err := svc.SendDigest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mail.sentCount() != 1 {
		t.Fatalf("expected 1 digest email, got %d", mail.sentCount())
	}
	if mail.sent[0].ToAddress != "ops@example.com" {
		t.Errorf("recipient: got %q", mail.sent[0].ToAddress)
	}
	body := mail.sent[0].BodyText
	for _, want := range []string{"New Sub A", "New Sub B", "1 submission(s) need review", "[needs review]", "Awaiting"} {
		if !contains(body, want) {
			t.Errorf("digest missing %q:\n%s", want, body)
		}
	}
	if contains(body, "acord_125") || contains(body, "acord_126") {
		t.Errorf("digest leaked internal item ids:\n%s", body)
	}
	if !digestSent {
		t.Fatal("expected EventDigestSent audit entry")
	}
}

func TestSendDigest_FlagsFilenameOnlyMatches(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	open := []model.Submission{
		{ID: "done", PolicyType: "cgl", State: model.StateComplete, SubjectLine: "Complete Sub", FromAddress: "d@x",
			CreatedAt: now.Add(-2 * time.Hour), LastActionAt: now.Add(-time.Hour)},
	}
	subs.On("ListOpen", mock.Anything, mock.Anything).Return(open, nil)
	// the repo flags acord_125 as filename-only; the service resolves its human name
	subs.On("FilenameOnlyItems", mock.Anything, mock.Anything).Return(map[string][]string{"done": {"acord_125"}}, nil)
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()

	svc := newSvcWith(t, subs, aud, mail, store, nil)
	svc.setClock(func() time.Time { return now })

	if err := svc.SendDigest(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := mail.sent[0].BodyText
	if !contains(body, "Matched on filename only, content not verified: ACORD 125") {
		t.Errorf("digest missing the filename-only flag with the resolved item name:\n%s", body)
	}
	if contains(body, "acord_125") {
		t.Errorf("digest leaked the internal item id:\n%s", body)
	}
}

func TestIngestEmail_FirstReplyImmediateDespiteWindow(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	svc := newSvc(t, subs, aud, mail, cl)
	svc.coalesceWindow = 2 * time.Minute // a window is set, but this is the first reply

	_, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "m1", FromAddress: "broker@example.com", Subject: "New Submission - CGL",
		Attachments: []model.Attachment{
			{Filename: "ACORD_125.pdf", ContentType: "application/pdf", Content: []byte("a125")},
		},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Wait()
	if mail.sentCount() != 1 {
		t.Fatalf("the first reply must send immediately even with a coalesce window, got %d", mail.sentCount())
	}
}

func TestIngestEmail_RecordsLastReplyAtOnSend(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()

	// register a counting SetLastReplyAt BEFORE newSvc's .Maybe() so it wins
	var lastReplyCalls int32
	var gotID string
	subs.On("SetLastReplyAt", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		atomic.AddInt32(&lastReplyCalls, 1)
		gotID = args.Get(1).(string)
	})
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	svc := newSvc(t, subs, aud, mail, cl)
	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "m1", FromAddress: "broker@example.com", Subject: "New Submission - CGL",
		Attachments: []model.Attachment{{Filename: "ACORD_125.pdf", ContentType: "application/pdf", Content: []byte("a125")}},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Wait()
	if mail.sentCount() != 1 {
		t.Fatalf("expected the reply to send, got %d", mail.sentCount())
	}
	if atomic.LoadInt32(&lastReplyCalls) != 1 {
		t.Fatalf("a successful send must record last_reply_at exactly once, got %d", lastReplyCalls)
	}
	if gotID != res.SubmissionID {
		t.Errorf("last_reply_at recorded for %q, want submission %q", gotID, res.SubmissionID)
	}
}

func TestIngestEmail_FollowupAfterWindowSendsImmediately(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	existing := &model.Submission{
		ID: "sub-1", PolicyType: "cgl", State: model.StateAwaiting,
		LastReplyAt:  now.Add(-3 * time.Minute), // replied 3 min ago; window is 2 min
		Emails:       []model.Email{{DeterministicID: "first", MessageID: "first-msg"}},
		Documents:    []model.Document{{ID: "d125", ClassifiedAs: "acord_125"}},
		MissingItems: []model.MissingItem{{ID: "acord_126", Description: "ACORD 126"}},
	}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(existing, false, nil)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	svc := newSvc(t, subs, aud, mail, cl)
	svc.coalesceWindow = 2 * time.Minute
	svc.setClock(func() time.Time { return now })

	if _, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "second-msg", InReplyTo: "first-msg", FromAddress: "broker@example.com", Subject: "Re: CGL",
		Attachments: []model.Attachment{{Filename: "ACORD_126.pdf", ContentType: "application/pdf", Content: []byte("a126")}},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Wait()
	// the previous reply was 3 min ago (> 2 min window), so spacing has released
	if mail.sentCount() != 1 {
		t.Fatalf("a follow-up after the coalesce window should send immediately, got %d", mail.sentCount())
	}
}

func TestIngestEmail_FollowupWithinWindowDefersToSweeper(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	existing := &model.Submission{
		ID: "sub-1", PolicyType: "cgl", State: model.StateAwaiting,
		LastReplyAt:  now.Add(-30 * time.Second), // replied 30s ago
		Emails:       []model.Email{{DeterministicID: "first", MessageID: "first-msg"}},
		Documents:    []model.Document{{ID: "d125", ClassifiedAs: "acord_125"}},
		MissingItems: []model.MissingItem{{ID: "acord_126", Description: "ACORD 126"}},
	}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(existing, false, nil)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	svc := newSvc(t, subs, aud, mail, cl)
	svc.coalesceWindow = 2 * time.Minute
	svc.setClock(func() time.Time { return now })

	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "second-msg", InReplyTo: "first-msg", FromAddress: "broker@example.com", Subject: "Re: CGL",
		Attachments: []model.Attachment{
			{Filename: "ACORD_126.pdf", ContentType: "application/pdf", Content: []byte("a126")},
		},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// the reply is durably queued but NOT sent online: last_reply_at + window is still in the future
	if !res.ReplyQueued {
		t.Fatal("a deferred reply must still be queued in the outbox")
	}
	svc.Wait()
	if mail.sentCount() != 0 {
		t.Fatalf("a follow-up within the coalesce window must defer to the sweeper, got %d online sends", mail.sentCount())
	}
}

func TestCheckEscalations(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()

	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	// escalatable: the broker owes a document
	broker := model.Submission{ID: "s1", State: model.StateAwaiting, LastActionAt: now.Add(-100 * time.Hour),
		MissingItems: []model.MissingItem{{ID: "acord_126", Description: "ACORD 126", Code: model.ReasonNotProvided}}}
	// blocked solely on an unreadable file: the agency's problem, must not escalate
	flagOnly := model.Submission{ID: "s2", State: model.StateAwaiting, NeedsReview: true, LastActionAt: now.Add(-100 * time.Hour),
		MissingItems: []model.MissingItem{{ID: "acord_125", Description: "ACORD 125", Code: model.ReasonUnreadable}}}

	subs.On("ListStale", mock.Anything, mock.Anything, mock.Anything).Return([]model.Submission{broker, flagOnly}, nil)
	var escalatedIDs []string
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		escalatedIDs = append(escalatedIDs, args.Get(1).(*model.Submission).ID)
	})
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	svc := newSvc(t, subs, aud, mail, cl)
	svc.setClock(func() time.Time { return now })

	if err := svc.CheckEscalations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(escalatedIDs) != 1 || escalatedIDs[0] != "s1" {
		t.Fatalf("only the broker-actionable case should escalate, got %v", escalatedIDs)
	}
}

func TestOutboundEmail_DeterministicIDStableAcrossProviderIDs(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	r := model.Reply{SubmissionID: "s1", ToAddress: "x@y", Subject: "Re: a", BodyText: "body"}

	// a redelivery mints a fresh providerMsgID; the row's identity must not change
	a := outboundEmail(r, "msg-1@host", now)
	b := outboundEmail(r, "msg-2@host", now)
	if a.DeterministicID != b.DeterministicID {
		t.Fatalf("det id should be stable across providerMsgID: %q vs %q", a.DeterministicID, b.DeterministicID)
	}

	// a genuinely different reply (completion vs missing-items) stays distinct
	r2 := r
	r2.BodyText = "different body"
	if c := outboundEmail(r2, "msg-1@host", now); c.DeterministicID == a.DeterministicID {
		t.Error("different reply body should yield a different det id")
	}
}

// multiStore matches Get by policy_type; only known types return ok=true.
type multiStore struct {
	byType map[string]model.Checklist
}

func (m *multiStore) Get(policyType string) (model.Checklist, bool) {
	c, ok := m.byType[policyType]
	return c, ok
}

func (m *multiStore) All() []model.Checklist {
	out := make([]model.Checklist, 0, len(m.byType))
	for _, c := range m.byType {
		out = append(out, c)
	}
	return out
}

// fakeLLM canned-responds Classify and ExtractField. Errors short-circuit.
type fakeLLM struct {
	classifyResp llm.ClassificationResponse
	classifyErr  error
	extractResp  llm.FieldExtractionResponse
	extractErr   error
	extractCalls int
}

func (f *fakeLLM) Classify(_ context.Context, _ llm.ClassificationRequest) (llm.ClassificationResponse, error) {
	return f.classifyResp, f.classifyErr
}

func (f *fakeLLM) ExtractField(_ context.Context, _ llm.FieldExtractionRequest) (llm.FieldExtractionResponse, error) {
	f.extractCalls++
	return f.extractResp, f.extractErr
}

func cglChecklistWithLossRuns() model.Checklist {
	minVal := 5.0
	return model.Checklist{
		Name:       "CGL",
		PolicyType: "cgl",
		Required: []model.RequiredItem{
			{ID: "acord_125", Description: "ACORD 125", Match: model.MatchRules{FilenamePatterns: []string{"*ACORD*125*"}}},
			{ID: "loss_runs", Description: "Loss runs", Match: model.MatchRules{FilenamePatterns: []string{"*loss*"}},
				RequiresField: &model.RequiresField{Name: "years_covered", Type: model.FieldTypeNumber, MinValue: &minVal},
			},
		},
	}
}

func newSvcWith(t *testing.T, subs *repomocks.SubmissionRepository, aud *repomocks.AuditRepository,
	mail *fakeMail, store checklist.Store, llmClient llm.Client) *SubmissionsService {
	t.Helper()
	subs.On("FindByDeterministicID", mock.Anything, mock.Anything).Return(nil, model.ErrSubmissionNotFound).Maybe()
	subs.On("ListEscalatedSince", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	subs.On("ListOpen", mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	subs.On("CountOpen", mock.Anything).Return(0, nil).Maybe()
	subs.On("FilenameOnlyItems", mock.Anything, mock.Anything).Return(map[string][]string(nil), nil).Maybe()
	subs.On("SetLastReplyAt", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	cl := store.All()[0]
	log := logrus.NewEntry(logrus.New())
	repo := &repository.Repository{Submissions: subs, Audit: aud, Outbox: newFakeOutbox()}
	return NewSubmissionsService(Dependencies{
		Config: &config.Config{
			Escalation: config.EscalationConfig{ThresholdHours: 72, AutoCloseAfterHours: 24},
			Digest:     config.DigestConfig{IntervalHours: 24, Recipient: "ops@example.com", MaxRows: 500},
		},
		Repository:     repo,
		EmailSender:    mail,
		Classifier:     &filenameClassifier{checklist: cl},
		ChecklistStore: store,
		TextExtractors: map[string]TextExtractor{"application/pdf": fakeExtractor{}},
		LLM:            llmClient,
		Log:            log,
	})
}

func TestIngestEmail_UnknownPolicy_TransitionsToAwaitingAndSendsClarification(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cglChecklistWithLossRuns()}}

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()

	policyUnknownSeen := false
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		e := args.Get(1).(*model.AuditEntry)
		if e.EventType == model.EventPolicyUnknown {
			policyUnknownSeen = true
		}
	})

	svc := newSvcWith(t, subs, aud, mail, store, nil)
	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID:   "msg-unknown",
		FromAddress: "broker@example.com",
		Subject:     "Just some random subject with no policy hint",
		Attachments: []model.Attachment{{Filename: "random.pdf", ContentType: "application/pdf"}},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.State != model.StateAwaiting {
		t.Fatalf("state: got %s, want awaiting", res.State)
	}
	if !policyUnknownSeen {
		t.Fatal("expected EventPolicyUnknown audit entry")
	}
	svc.Wait()
	if mail.sentCount() != 1 {
		t.Fatalf("clarification reply expected, got %d sends", mail.sentCount())
	}
	body := mail.sent[0].BodyText
	if body == "" || len(body) < 20 || !contains(body, "policy type") {
		t.Errorf("clarification body should mention 'policy type', got: %q", body)
	}
}

func TestIngestEmail_RequiresField_ExtractedAndStoredOnDoc(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := cglChecklistWithLossRuns()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}
	lm := &fakeLLM{
		extractResp: llm.FieldExtractionResponse{
			Value: 7.0, Confidence: 0.9, Reason: "header reads '7 years'",
		},
	}

	var captured *model.Submission
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		captured = args.Get(1).(*model.Submission)
	})
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	svc := newSvcWith(t, subs, aud, mail, store, lm)
	_, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID:   "msg-extract",
		FromAddress: "broker@example.com",
		Subject:     "New Submission - CGL",
		Attachments: []model.Attachment{
			{Filename: "ACORD_125_X.pdf", ContentType: "application/pdf"},
			{Filename: "loss_runs_X.pdf", ContentType: "application/pdf"},
		},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Wait()

	if lm.extractCalls != 1 {
		t.Fatalf("ExtractField should be called once (only for loss_runs), got %d", lm.extractCalls)
	}
	if captured == nil {
		t.Fatal("submission never captured")
	}
	var lossRunDoc *model.Document
	for i := range captured.Documents {
		if captured.Documents[i].ClassifiedAs == "loss_runs" {
			lossRunDoc = &captured.Documents[i]
		}
	}
	if lossRunDoc == nil {
		t.Fatal("loss_runs document not captured")
	}
	if lossRunDoc.ExtractedFields["years_covered"] != 7.0 {
		t.Errorf("ExtractedFields[years_covered]: got %v, want 7.0", lossRunDoc.ExtractedFields["years_covered"])
	}
}

func TestIngestEmail_RequiresField_BelowMinFailsChecklist(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := cglChecklistWithLossRuns()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}
	lm := &fakeLLM{
		extractResp: llm.FieldExtractionResponse{Value: 2.0, Confidence: 0.9},
	}

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	svc := newSvcWith(t, subs, aud, mail, store, lm)
	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID:   "msg-below",
		FromAddress: "broker@example.com",
		Subject:     "New Submission - CGL",
		Attachments: []model.Attachment{
			{Filename: "ACORD_125_X.pdf", ContentType: "application/pdf"},
			{Filename: "loss_runs_X.pdf", ContentType: "application/pdf"},
		},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.State != model.StateAwaiting {
		t.Fatalf("state: got %s, want awaiting (2 < 5 years)", res.State)
	}
	if len(res.MissingItems) != 1 || res.MissingItems[0].ID != "loss_runs" {
		t.Fatalf("expected loss_runs missing, got %+v", res.MissingItems)
	}
}

func TestIngestEmail_RequiresField_LLMErrorSoftPasses(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := cglChecklistWithLossRuns()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}
	lm := &fakeLLM{extractErr: errors.New("llm down")}

	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()

	llmFailed := false
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		e := args.Get(1).(*model.AuditEntry)
		if e.EventType == model.EventLLMFailed {
			llmFailed = true
		}
	})

	svc := newSvcWith(t, subs, aud, mail, store, lm)
	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID:   "msg-llm-down",
		FromAddress: "broker@example.com",
		Subject:     "New Submission - CGL",
		Attachments: []model.Attachment{
			{Filename: "ACORD_125_X.pdf", ContentType: "application/pdf"},
			{Filename: "loss_runs_X.pdf", ContentType: "application/pdf"},
		},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.State != model.StateComplete {
		t.Fatalf("state: got %s, want complete (LLM failure should soft-pass)", res.State)
	}
	if !llmFailed {
		t.Fatal("expected EventLLMFailed audit entry")
	}
}

func TestIngestEmail_ThreadedFollowUp_TransitionsToComplete(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}

	existing := &model.Submission{
		ID:         "sub-existing",
		PolicyType: "cgl",
		State:      model.StateAwaiting,
		Emails: []model.Email{
			{DeterministicID: "first", MessageID: "first-msg"},
		},
		Documents: []model.Document{
			{ID: "doc-125", ClassifiedAs: "acord_125"},
		},
		MissingItems: []model.MissingItem{{ID: "acord_126", Description: "ACORD 126"}},
	}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(existing, false, nil)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	svc := newSvcWith(t, subs, aud, mail, store, nil)
	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID:   "second-msg",
		InReplyTo:   "first-msg",
		FromAddress: "broker@example.com",
		Subject:     "Re: CGL Submission",
		Attachments: []model.Attachment{
			{Filename: "ACORD_126_X.pdf", ContentType: "application/pdf"},
		},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.State != model.StateComplete {
		t.Fatalf("state: got %s, want complete after follow-up", res.State)
	}
	if res.SubmissionID != "sub-existing" {
		t.Errorf("submission id: got %s, want sub-existing", res.SubmissionID)
	}
}

func TestCheckClosures_TransitionsCompleteSubmissionsToClosed(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	stale := model.Submission{ID: "old-complete", State: model.StateComplete, UpdatedAt: now.Add(-30 * 24 * time.Hour)}
	subs.On("ListCompletedBefore", mock.Anything, mock.Anything, mock.Anything).Return([]model.Submission{stale}, nil)

	var updated *model.Submission
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		updated = args.Get(1).(*model.Submission)
	})

	closedEvents := 0
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		e := args.Get(1).(*model.AuditEntry)
		if e.EventType == model.EventClosed {
			closedEvents++
		}
	})

	svc := newSvcWith(t, subs, aud, mail, store, nil)
	svc.setClock(func() time.Time { return now })

	if err := svc.CheckClosures(context.Background()); err != nil {
		t.Fatal(err)
	}
	if updated == nil || updated.State != model.StateClosed {
		t.Fatalf("expected submission transitioned to Closed, got %+v", updated)
	}
	if closedEvents != 1 {
		t.Fatalf("expected 1 EventClosed audit, got %d", closedEvents)
	}
}

func TestCheckClosures_RetiresQuietEscalatedSubmissions(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	subs.On("ListCompletedBefore", mock.Anything, mock.Anything, mock.Anything).Return([]model.Submission(nil), nil)

	// one escalated case gone quiet past the 24h window, one still fresh
	stale := model.Submission{ID: "stale-esc", State: model.StateEscalated, LastActionAt: now.Add(-48 * time.Hour)}
	fresh := model.Submission{ID: "fresh-esc", State: model.StateEscalated, LastActionAt: now.Add(-1 * time.Hour)}
	subs.On("ListEscalatedSince", mock.Anything, mock.Anything, mock.Anything).Return([]model.Submission{stale, fresh}, nil)

	var closed []string
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		closed = append(closed, args.Get(1).(*model.Submission).ID)
	})
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)

	svc := newSvcWith(t, subs, aud, mail, store, nil)
	svc.setClock(func() time.Time { return now })

	if err := svc.CheckClosures(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0] != "stale-esc" {
		t.Fatalf("only the stale escalated case should close, got %v", closed)
	}
}

func TestCheckClosures_DisabledWhenAutoCloseIsZero(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}

	svc := newSvcWith(t, subs, aud, mail, store, nil)
	svc.cfg.Escalation.AutoCloseAfterHours = 0

	if err := svc.CheckClosures(context.Background()); err != nil {
		t.Fatal(err)
	}
	// no ListCompletedBefore expectation registered — failing it would surface here
}

func TestSendDigest_NoRecipientIsNoOp(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}

	svc := newSvcWith(t, subs, aud, mail, store, nil)
	svc.cfg.Digest.Recipient = ""

	if err := svc.SendDigest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mail.sentCount() != 0 {
		t.Errorf("no recipient should mean no send, got %d", mail.sentCount())
	}
}

func TestSendDigest_NothingOpenIsNoOp(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}

	subs.On("ListOpen", mock.Anything, mock.Anything).Return([]model.Submission{}, nil)

	svc := newSvcWith(t, subs, aud, mail, store, nil)

	if err := svc.SendDigest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mail.sentCount() != 0 {
		t.Errorf("expected 0 sends when nothing is open, got %d", mail.sentCount())
	}
}

func TestInferPolicyType(t *testing.T) {
	cgl := model.Checklist{Name: "Commercial General Liability", PolicyType: "cgl"}
	bop := model.Checklist{Name: "Business Owners Policy", PolicyType: "bop"}
	wc := model.Checklist{Name: "Workers Compensation", PolicyType: "workers_compensation"}
	all := []model.Checklist{cgl, bop, wc}

	cases := []struct {
		subject string
		want    string
	}{
		{"New Submission - Commercial General Liability", "cgl"},
		{"new sub - CGL", "cgl"},
		{"Re: workers comp renewal", "workers_compensation"},
		{"Re: Workers' Comp Renewal", "workers_compensation"},
		{"BOP for ACME", "bop"},
		{"general liability for ACME", "cgl"},
		{"something else entirely", model.PolicyTypeUnknown},
		{"", model.PolicyTypeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.subject, func(t *testing.T) {
			got := inferPolicyType(tc.subject, all)
			if got != tc.want {
				t.Errorf("inferPolicyType(%q) = %q, want %q", tc.subject, got, tc.want)
			}
		})
	}
}

func TestComputeEmailID_DeterministicAndOrderIndependent(t *testing.T) {
	a := []model.Attachment{
		{SHA256: "aaa"},
		{SHA256: "bbb"},
	}
	b := []model.Attachment{
		{SHA256: "bbb"},
		{SHA256: "aaa"},
	}
	first := computeEmailID("msg-id", "hello", a)
	second := computeEmailID("msg-id", "hello", b)
	if first != second {
		t.Errorf("attachment order should not matter; %s vs %s", first, second)
	}

	different := computeEmailID("other-msg", "hello", a)
	if different == first {
		t.Error("different message-id should produce different id")
	}
}

func TestCleanThreadRefs_DedupesAndTrims(t *testing.T) {
	got := cleanThreadRefs("msg-1", " msg-2 ", []string{"msg-2", "", "msg-3"})
	want := []string{"msg-1", "msg-2", "msg-3"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got[i], want[i])
		}
	}
}

// contains is a case-insensitive substring helper for assertions.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			h := haystack[i+j]
			n := needle[j]
			if h >= 'A' && h <= 'Z' {
				h += 'a' - 'A'
			}
			if n >= 'A' && n <= 'Z' {
				n += 'a' - 'A'
			}
			if h != n {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
