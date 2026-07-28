package service

import (
	"context"
	"strings"
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

// emptyExtractor stands in for a document that yields no text (a scan or an
// encrypted PDF) without needing the real extractor.
type emptyExtractor struct{}

func (emptyExtractor) Extract([]byte) (string, error) { return "", nil }

func boolp(b bool) *bool           { return &b }
func timep(t time.Time) *time.Time { return &t }

// newSvcCfg builds a service with an explicit config, extractor set, and LLM so the
// Fix Spec 4 paths (kill switch, encryption, identity) can be exercised precisely.
func newSvcCfg(t *testing.T, subs *repomocks.SubmissionRepository, aud *repomocks.AuditRepository, mail *fakeMail, store checklist.Store, cfg *config.Config, ext map[string]TextExtractor, lm llm.Client) *SubmissionsService {
	t.Helper()
	subs.On("FindByDeterministicID", mock.Anything, mock.Anything).Return(nil, model.ErrSubmissionNotFound).Maybe()
	subs.On("SetLastReplyAt", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	subs.On("FilenameOnlyItems", mock.Anything, mock.Anything).Return(map[string][]string(nil), nil).Maybe()
	if ext == nil {
		ext = map[string]TextExtractor{"application/pdf": fakeExtractor{}}
	}
	return NewSubmissionsService(Dependencies{
		Config:         cfg,
		Repository:     &repository.Repository{Submissions: subs, Audit: aud, Outbox: newFakeOutbox()},
		EmailSender:    mail,
		Classifier:     &filenameClassifier{checklist: store.All()[0]},
		ChecklistStore: store,
		TextExtractors: ext,
		LLM:            lm,
		Log:            logrus.NewEntry(logrus.New()),
	})
}

func baseCfg() *config.Config {
	return &config.Config{
		Escalation: config.EscalationConfig{ThresholdHours: 72, BindWindowDays: 7, MinGapHours: 24},
		Classifier: config.ClassifierConfig{ConfidenceFloor: 0.80},
		Document:   config.DocumentConfig{UnreadableMinSizeBytes: 51200, UnreadableMinChars: 100},
	}
}

// A1 --------------------------------------------------------------------------

func TestIngestEmail_HardBounce_FlagsDeliveryFailedNoReply(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	var captured *model.Submission
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Run(func(a mock.Arguments) {
		captured = a.Get(1).(*model.Submission)
	})
	bounced, autoSuppressed := false, false
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(a mock.Arguments) {
		e := a.Get(1).(*model.AuditEntry)
		switch e.EventType {
		case model.EventReplyBounced:
			bounced = true
		case model.EventReplySuppressed:
			if e.Payload["reason"] == "auto_submitted" {
				autoSuppressed = true
			}
		}
	})
	svc := newSvcCfg(t, subs, aud, mail, &fakeStore{cl: smallChecklist()}, baseCfg(), nil, nil)

	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID:       "dsn-1",
		FromAddress:     "mailer-daemon@agency.example",
		Subject:         "Delivery Status Notification (Failure)",
		Bounce:          true,
		BouncePermanent: true,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Wait()
	if mail.sentCount() != 0 {
		t.Fatalf("a bounce must never be replied to, got %d sends", mail.sentCount())
	}
	if !res.NeedsReview {
		t.Error("a hard bounce must raise the review flag")
	}
	if captured == nil || !captured.DeliveryFailed {
		t.Error("a hard bounce must set delivery_failed")
	}
	if !bounced {
		t.Error("expected a reply.bounced audit")
	}
	if autoSuppressed {
		t.Error("a bounce must not be swallowed as auto-submitted")
	}
}

func TestIngestEmail_DeliveryFailed_SuppressesFurtherReplies(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	existing := &model.Submission{
		ID: "sub-df", State: model.StateAwaiting, PolicyType: "cgl", FromAddress: "broker@x", DeliveryFailed: true,
		Emails: []model.Email{{DeterministicID: "e0", MessageID: "orig"}},
	}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(existing, false, nil)
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	suppressed := false
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(a mock.Arguments) {
		e := a.Get(1).(*model.AuditEntry)
		if e.EventType == model.EventReplySuppressed && e.Payload["reason"] == "delivery_failed" {
			suppressed = true
		}
	})
	svc := newSvcCfg(t, subs, aud, mail, &fakeStore{cl: smallChecklist()}, baseCfg(), nil, nil)

	if _, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "m2", InReplyTo: "orig", FromAddress: "broker@x", Subject: "CGL",
		Attachments: []model.Attachment{{Filename: "ACORD_125.pdf", ContentType: "application/pdf", Content: []byte("ACORD 125")}},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Wait()
	if mail.sentCount() != 0 {
		t.Fatalf("delivery-failed submission must not be replied to, got %d", mail.sentCount())
	}
	if !suppressed {
		t.Error("expected a reply.suppressed(delivery_failed) audit")
	}
}

// A2 --------------------------------------------------------------------------

func TestIngestEmail_RepliesToReplyToAddress(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)
	svc := newSvcCfg(t, subs, aud, mail, &fakeStore{cl: smallChecklist()}, baseCfg(), nil, nil)

	if _, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "m1", FromAddress: "assistant@agency.example", ReplyTo: "producer@agency.example",
		Subject:     "New Submission - CGL",
		Attachments: []model.Attachment{{Filename: "ACORD_125.pdf", ContentType: "application/pdf", Content: []byte("ACORD 125")}},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Wait()
	if mail.sentCount() != 1 {
		t.Fatalf("expected a reply, got %d", mail.sentCount())
	}
	if got := mail.sent[0].ToAddress; got != "producer@agency.example" {
		t.Errorf("reply went to %q, want the Reply-To producer@agency.example", got)
	}
}

// A3 --------------------------------------------------------------------------

func TestIngestEmail_EncryptedPDF_AsksResendUnlocked_NoReviewFlag(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	var captured *model.Submission
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(a mock.Arguments) {
		captured = a.Get(1).(*model.Submission)
	})
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)
	ext := map[string]TextExtractor{"application/pdf": emptyExtractor{}}
	svc := newSvcCfg(t, subs, aud, mail, &fakeStore{cl: oneItemChecklist()}, baseCfg(), ext, nil)

	pdf := append([]byte("%PDF-1.4\n"), []byte("trailer << /Encrypt 5 0 R /Root 1 0 R >>")...)
	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "m1", FromAddress: "broker@x", Subject: "New Submission - CGL",
		Attachments: []model.Attachment{{Filename: "ACORD_125.pdf", ContentType: "application/pdf", Size: len(pdf), Content: pdf}},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Wait()
	if res.NeedsReview {
		t.Error("an encrypted (broker-actionable) PDF must not raise the review flag")
	}
	if mail.sentCount() != 1 {
		t.Fatalf("expected a resend-unlocked reply, got %d", mail.sentCount())
	}
	body := mail.sent[0].BodyText
	if !strings.Contains(body, "password-protected") || !strings.Contains(body, "unlocked") {
		t.Errorf("reply should ask the broker to resend unlocked:\n%s", body)
	}
	if captured != nil {
		var enc bool
		for _, d := range captured.Documents {
			if d.ClassifiedAs == "acord_125" && d.Encrypted {
				enc = true
			}
		}
		if !enc {
			t.Error("the acord_125 document should be persisted encrypted")
		}
	}
}

// A4 --------------------------------------------------------------------------

func TestIngestEmail_ShareLinkNoAttachments_AsksToAttach(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)
	cfg := baseCfg()
	cfg.Document.AttachmentLinkDomains = []string{"dropbox", "sharefile"}
	svc := newSvcCfg(t, subs, aud, mail, &fakeStore{cl: smallChecklist()}, cfg, nil, nil)

	if _, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "m1", FromAddress: "broker@x", Subject: "New Submission - CGL",
		BodyText: "Everything is here: https://www.dropbox.com/s/abc/submission",
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Wait()
	if mail.sentCount() != 1 {
		t.Fatalf("expected a reply, got %d", mail.sentCount())
	}
	body := mail.sent[0].BodyText
	if !strings.Contains(body, "didn't find any attachments") {
		t.Errorf("reply should ask the broker to attach directly:\n%s", body)
	}
	if strings.Contains(body, "ACORD 125") || strings.Contains(body, "ACORD 126") {
		t.Errorf("the attach-directly reply must not list every document:\n%s", body)
	}
}

// B1 --------------------------------------------------------------------------

func TestReconcileHold_HoldsFolderMembersAndReleasesTheRest(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	subs.On("SubmissionIDsByMessageIDs", mock.Anything, []string{"m-held"}).Return([]string{"sub-A"}, nil)
	subs.On("ListHeldIDs", mock.Anything).Return([]string{"sub-B"}, nil)
	var held, released []string
	subs.On("SetOnHold", mock.Anything, mock.Anything, true).Return(nil).Run(func(a mock.Arguments) {
		held = a.Get(1).([]string)
	})
	subs.On("SetOnHold", mock.Anything, mock.Anything, false).Return(nil).Run(func(a mock.Arguments) {
		released = a.Get(1).([]string)
	})
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()
	svc := newSvcCfg(t, subs, aud, &fakeMail{}, &fakeStore{cl: smallChecklist()}, baseCfg(), nil, nil)

	if err := svc.ReconcileHold(context.Background(), []string{"m-held"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(held) != 1 || held[0] != "sub-A" {
		t.Errorf("sub-A (in folder) should be newly held, got %v", held)
	}
	if len(released) != 1 || released[0] != "sub-B" {
		t.Errorf("sub-B (no longer in folder) should be released, got %v", released)
	}
}

func TestIngestEmail_OnHold_SuppressesReply(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	existing := &model.Submission{
		ID: "sub-hold", State: model.StateAwaiting, PolicyType: "cgl", FromAddress: "broker@x", OnHold: true,
		Emails: []model.Email{{DeterministicID: "e0", MessageID: "orig"}},
	}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(existing, false, nil)
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)
	svc := newSvcCfg(t, subs, aud, mail, &fakeStore{cl: smallChecklist()}, baseCfg(), nil, nil)

	if _, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "m2", InReplyTo: "orig", FromAddress: "broker@x", Subject: "CGL",
		Attachments: []model.Attachment{{Filename: "ACORD_125.pdf", ContentType: "application/pdf", Content: []byte("ACORD 125")}},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Wait()
	if mail.sentCount() != 0 {
		t.Fatalf("a held submission must not be replied to, got %d", mail.sentCount())
	}
}

// B2 --------------------------------------------------------------------------

func TestIngestEmail_RepliesDisabled_NoOutboundAuditsOnce(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	disabled := 0
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(a mock.Arguments) {
		if a.Get(1).(*model.AuditEntry).EventType == model.EventReplyDisabled {
			disabled++
		}
	})
	cfg := baseCfg()
	cfg.Reply.RepliesEnabled = boolp(false)
	svc := newSvcCfg(t, subs, aud, mail, &fakeStore{cl: smallChecklist()}, cfg, nil, nil)

	for _, id := range []string{"m1", "m2"} {
		if _, err := svc.IngestEmail(context.Background(), IngestRequest{
			MessageID: id, FromAddress: "broker@x", Subject: "New Submission - CGL",
			Attachments: []model.Attachment{{Filename: "ACORD_125.pdf", ContentType: "application/pdf", Content: []byte("ACORD 125")}},
		}); err != nil {
			t.Fatalf("ingest %s: %v", id, err)
		}
	}
	svc.Wait()
	if mail.sentCount() != 0 {
		t.Fatalf("kill switch must queue no replies, got %d", mail.sentCount())
	}
	if disabled != 1 {
		t.Errorf("reply.disabled should be audited once per process, got %d", disabled)
	}
}

// C1 --------------------------------------------------------------------------

func TestIngestEmail_ExtractsNamedInsuredAndEffectiveDate(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("FindContentMatch", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, model.ErrSubmissionNotFound).Maybe()
	var captured *model.Submission
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(a mock.Arguments) {
		captured = a.Get(1).(*model.Submission)
	})
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)
	lm := &fakeLLM{identityResp: llm.IdentityResponse{NamedInsured: "Acme Manufacturing", EffectiveDate: "2026-08-01"}}
	svc := newSvcCfg(t, subs, aud, mail, &fakeStore{cl: smallChecklist()}, baseCfg(), nil, lm)

	if _, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "m1", FromAddress: "broker@x", Subject: "New Business - CGL",
		Attachments: []model.Attachment{{Filename: "ACORD_125.pdf", ContentType: "application/pdf", Content: []byte("ACORD 125 application")}},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Wait()
	if lm.identityCalls != 1 {
		t.Errorf("expected exactly one identity extraction, got %d", lm.identityCalls)
	}
	if captured == nil || captured.NamedInsured != "Acme Manufacturing" {
		t.Fatalf("named insured not extracted: %+v", captured)
	}
	if captured.EffectiveDate == nil || captured.EffectiveDate.Format("2006-01-02") != "2026-08-01" {
		t.Errorf("effective date not parsed, got %v", captured.EffectiveDate)
	}
}

// C2 --------------------------------------------------------------------------

func TestIngestEmail_ContentMatch_AttachesToExisting(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	existing := &model.Submission{
		ID: "sub-1", State: model.StateAwaiting, PolicyType: "cgl", FromAddress: "broker@x", NamedInsured: "Acme Manufacturing",
		Emails:    []model.Email{{DeterministicID: "e0", MessageID: "orig"}},
		Documents: []model.Document{{ID: "d0", ClassifiedAs: "acord_125", Confidence: 0.95}},
	}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("FindContentMatch", mock.Anything, "Acme Manufacturing", "broker@x", mock.Anything).Return(existing, nil)
	var persisted *model.Submission
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(a mock.Arguments) {
		persisted = a.Get(1).(*model.Submission)
	})
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Maybe()
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	matched := false
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Run(func(a mock.Arguments) {
		if a.Get(1).(*model.AuditEntry).EventType == model.EventThreadMatchedByContent {
			matched = true
		}
	})
	lm := &fakeLLM{identityResp: llm.IdentityResponse{NamedInsured: "Acme Manufacturing"}}
	svc := newSvcCfg(t, subs, aud, mail, &fakeStore{cl: smallChecklist()}, baseCfg(), nil, lm)

	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "fresh", FromAddress: "broker@x", Subject: "New Business - CGL",
		Attachments: []model.Attachment{{Filename: "ACORD_125.pdf", ContentType: "application/pdf", Content: []byte("ACORD 125 application")}},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Wait()
	if res.SubmissionID != "sub-1" {
		t.Errorf("fresh email should attach to sub-1, got %s", res.SubmissionID)
	}
	if !matched {
		t.Error("expected a thread_matched_by_content audit")
	}
	if persisted == nil || persisted.ID != "sub-1" {
		t.Errorf("no separate submission should be created; persisted %+v", persisted)
	}
}

func TestIngestEmail_ContentMatch_DifferentInsured_SeparateSubmission(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound)
	subs.On("FindContentMatch", mock.Anything, "Beta Corp", "broker@x", mock.Anything).Return(nil, model.ErrSubmissionNotFound)
	var persisted *model.Submission
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(a mock.Arguments) {
		persisted = a.Get(1).(*model.Submission)
	})
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)
	lm := &fakeLLM{identityResp: llm.IdentityResponse{NamedInsured: "Beta Corp"}}
	svc := newSvcCfg(t, subs, aud, mail, &fakeStore{cl: smallChecklist()}, baseCfg(), nil, lm)

	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "fresh2", FromAddress: "broker@x", Subject: "New Business - CGL",
		Attachments: []model.Attachment{{Filename: "ACORD_125.pdf", ContentType: "application/pdf", Content: []byte("ACORD 125 application")}},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	svc.Wait()
	if persisted == nil || persisted.NamedInsured != "Beta Corp" {
		t.Fatalf("a different insured should create its own submission: %+v", persisted)
	}
	if res.SubmissionID == "sub-1" {
		t.Error("must not attach to an unrelated submission")
	}
}

// C3 --------------------------------------------------------------------------

func TestCheckEscalations_BindWindow_EscalatesImmediately(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	now := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	sub := model.Submission{
		ID: "s1", State: model.StateAwaiting, PolicyType: "cgl", LastActionAt: now, // active, not quiet
		EffectiveDate: timep(now.Add(3 * 24 * time.Hour)),
		MissingItems:  []model.MissingItem{{ID: "acord_126", Code: model.ReasonNotProvided}},
	}
	subs.On("ListStale", mock.Anything, mock.Anything, mock.Anything).Return([]model.Submission{}, nil)
	subs.On("ListBindSoon", mock.Anything, mock.Anything, mock.Anything).Return([]model.Submission{sub}, nil)
	var escalated *model.Submission
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Run(func(a mock.Arguments) {
		escalated = a.Get(1).(*model.Submission)
	})
	aud.On("Append", mock.Anything, mock.Anything).Return(nil)
	svc := newSvcCfg(t, subs, aud, &fakeMail{}, &fakeStore{cl: smallChecklist()}, baseCfg(), nil, nil)
	svc.setClock(func() time.Time { return now })

	if err := svc.CheckEscalations(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if escalated == nil || escalated.State != model.StateEscalated {
		t.Fatalf("a bind-soon submission should escalate immediately, got %+v", escalated)
	}
	if escalated.LastEscalatedAt == nil {
		t.Error("escalation should stamp LastEscalatedAt")
	}
}

func TestCheckEscalations_BindWindow_RateLimitedWithinMinGap(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	now := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	recent := now.Add(-1 * time.Hour) // escalated well within the 24h MinGap
	sub := model.Submission{
		ID: "s1", State: model.StateAwaiting, PolicyType: "cgl", LastActionAt: now,
		EffectiveDate:   timep(now.Add(2 * 24 * time.Hour)),
		LastEscalatedAt: timep(recent),
		MissingItems:    []model.MissingItem{{ID: "acord_126", Code: model.ReasonNotProvided}},
	}
	subs.On("ListStale", mock.Anything, mock.Anything, mock.Anything).Return([]model.Submission{}, nil)
	subs.On("ListBindSoon", mock.Anything, mock.Anything, mock.Anything).Return([]model.Submission{sub}, nil)
	escalatedAudit := false
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe().Run(func(a mock.Arguments) {
		if a.Get(1).(*model.AuditEntry).EventType == model.EventEscalated {
			escalatedAudit = true
		}
	})
	svc := newSvcCfg(t, subs, aud, &fakeMail{}, &fakeStore{cl: smallChecklist()}, baseCfg(), nil, nil)
	svc.setClock(func() time.Time { return now })

	if err := svc.CheckEscalations(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if escalatedAudit {
		t.Error("a submission escalated within MinGap must not be re-nudged")
	}
}
