package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/infrastructure/checklist"
	"github.com/atdayev/submission-triage/internal/infrastructure/llm"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/repository"
	repomocks "github.com/atdayev/submission-triage/internal/repository/mocks"
)

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
	cl := store.All()[0]
	log := logrus.NewEntry(logrus.New())
	repo := &repository.Repository{Submissions: subs, Audit: aud, Outbox: newFakeOutbox()}
	return NewSubmissionsService(Dependencies{
		Config: &config.Config{Escalation: config.EscalationConfig{
			ThresholdHours:      72,
			AutoCloseAfterHours: 24,
			DigestIntervalHours: 24,
			DigestRecipient:     "ops@example.com",
		}},
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
	// No ListCompletedBefore expectation registered — failing it would surface here.
}

func TestSendEscalationDigest_SendsToConfiguredRecipient(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	escalated := []model.Submission{
		{ID: "esc-1", PolicyType: "cgl", FromAddress: "a@x", State: model.StateEscalated, LastActionAt: now.Add(-100 * time.Hour)},
		{ID: "esc-2", PolicyType: "cgl", FromAddress: "b@x", State: model.StateEscalated, LastActionAt: now.Add(-80 * time.Hour)},
	}
	subs.On("ListEscalatedSince", mock.Anything, mock.Anything, mock.Anything).Return(escalated, nil)

	digestSent := false
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		e := args.Get(1).(*model.AuditEntry)
		if e.EventType == model.EventDigestSent {
			digestSent = true
		}
	})

	svc := newSvcWith(t, subs, aud, mail, store, nil)
	svc.setClock(func() time.Time { return now })

	if err := svc.SendEscalationDigest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mail.sentCount() != 1 {
		t.Fatalf("expected 1 digest email, got %d", mail.sentCount())
	}
	if mail.sent[0].ToAddress != "ops@example.com" {
		t.Errorf("recipient: got %q, want ops@example.com", mail.sent[0].ToAddress)
	}
	if !contains(mail.sent[0].BodyText, "esc-1") || !contains(mail.sent[0].BodyText, "esc-2") {
		t.Errorf("digest body should list both submissions, got: %q", mail.sent[0].BodyText)
	}
	if !digestSent {
		t.Fatal("expected EventDigestSent audit entry")
	}
}

func TestSendEscalationDigest_NoRecipientIsNoOp(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}

	svc := newSvcWith(t, subs, aud, mail, store, nil)
	svc.cfg.Escalation.DigestRecipient = ""

	if err := svc.SendEscalationDigest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mail.sentCount() != 0 {
		t.Errorf("no recipient should mean no send, got %d", mail.sentCount())
	}
}

func TestSendEscalationDigest_NothingEscalatedIsNoOp(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cl := smallChecklist()
	store := &multiStore{byType: map[string]model.Checklist{"cgl": cl}}

	subs.On("ListEscalatedSince", mock.Anything, mock.Anything, mock.Anything).Return([]model.Submission{}, nil)

	svc := newSvcWith(t, subs, aud, mail, store, nil)

	if err := svc.SendEscalationDigest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mail.sentCount() != 0 {
		t.Errorf("expected 0 sends, got %d", mail.sentCount())
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
