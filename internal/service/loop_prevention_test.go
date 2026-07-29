package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/model"
	repomocks "github.com/atdayev/submission-triage/internal/repository/mocks"
)

// acord125 is a submission attachment that satisfies the first checklist item,
// leaving ACORD 126 outstanding — the standard "one thing still missing" shape.
func acord125() []model.Attachment {
	return []model.Attachment{{Filename: "ACORD_125.pdf", ContentType: "application/pdf", Content: []byte("ACORD 125")}}
}

// 0.1 — policy type re-inference and the clarification cap -----------------------

func TestResolvePolicyType_ReInfersFromReplyBody(t *testing.T) {
	store := &multiStore{byType: map[string]model.Checklist{"workers_compensation": {
		Name: "Workers Compensation", PolicyType: "workers_compensation",
		Required: []model.RequiredItem{{ID: "acord_130", Description: "ACORD 130"}},
	}}}
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()
	svc := newSvcCfg(t, subs, aud, &fakeMail{}, store, baseCfg(), nil, nil)

	sub := &model.Submission{ID: "s1", PolicyType: model.PolicyTypeUnknown}
	svc.resolvePolicyType(context.Background(), sub, model.Email{
		Subject:  "RE: New Business Submission",
		BodyText: "it's workers comp, sorry for the confusion.\n\n> Could you reply with the line of business",
	})
	if sub.PolicyType != "workers_compensation" {
		t.Errorf("policy type: got %q, want workers_compensation", sub.PolicyType)
	}
}

// The quoted thread carries our own clarification, which names every line of
// business we support; matching below the quote line would resolve to whichever
// appears there first rather than to what the broker actually wrote.
func TestResolvePolicyType_IgnoresQuotedThread(t *testing.T) {
	store := &multiStore{byType: map[string]model.Checklist{"cgl": smallChecklist()}}
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()
	svc := newSvcCfg(t, subs, aud, &fakeMail{}, store, baseCfg(), nil, nil)

	sub := &model.Submission{ID: "s1", PolicyType: model.PolicyTypeUnknown}
	svc.resolvePolicyType(context.Background(), sub, model.Email{
		Subject:  "RE: Submission",
		BodyText: "Checking on this.\n\n> Could you reply with the line of business (e.g., Test)?",
	})
	if sub.PolicyType != model.PolicyTypeUnknown {
		t.Errorf("a match inside the quoted thread must not resolve the policy type, got %q", sub.PolicyType)
	}
}

// Earliest match wins so a broker's answer beats their own signature block.
func TestInferPolicyType_EarliestMatchWins(t *testing.T) {
	all := []model.Checklist{
		{Name: "Commercial Property", PolicyType: "commercial_property"},
		{Name: "Workers Compensation", PolicyType: "workers_compensation"},
	}
	got := inferPolicyType("it's workers comp\n\nJane Doe\nCommercial Property Specialist", all)
	if got != "workers_compensation" {
		t.Errorf("got %q, want workers_compensation (the answer precedes the signature)", got)
	}
}

func TestIngestEmail_PolicyUnknown_StopsAskingAtCapAndFlags(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	store := &multiStore{byType: map[string]model.Checklist{"cgl": smallChecklist()}}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound).Maybe()
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Maybe()
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()

	cfg := baseCfg()
	cfg.Reply.PolicyClarifyMaxAsks = 2
	svc := newSvcCfg(t, subs, aud, mail, store, cfg, nil, nil)

	// the counter is incremented before the reply is built, so an ask is still due
	// while it sits at the cap
	atCap := &model.Submission{ID: "s1", PolicyType: model.PolicyTypeUnknown, PolicyClarifyAsks: 2}
	if _, _, send := svc.buildReply(atCap, []model.MissingItem{model.PolicyUnknownItem()}, model.Email{}, true); !send {
		t.Error("the last permitted ask should still be sent")
	}
	if svc.policyClarifyExhausted(atCap) {
		t.Error("sitting at the cap is not yet exhausted")
	}

	past := &model.Submission{ID: "s2", PolicyType: model.PolicyTypeUnknown, PolicyClarifyAsks: 3}
	_, fingerprint, send := svc.buildReply(past, []model.MissingItem{model.PolicyUnknownItem()}, model.Email{}, true)
	if send {
		t.Error("past the cap the clarification must stop being asked")
	}
	if fingerprint != "" {
		t.Error("a withheld reply has no fingerprint to record")
	}
	if !svc.policyClarifyExhausted(past) {
		t.Error("past the cap the submission should read as exhausted so a human is flagged")
	}
}

// The synthetic item must carry a reason code: with an empty Code it is invisible to
// BrokerActionable, so the submission can never escalate and never close.
func TestPolicyUnknownItem_IsBrokerActionable(t *testing.T) {
	items := []model.MissingItem{model.PolicyUnknownItem()}
	if len(model.BrokerActionable(items)) != 1 {
		t.Error("policy_unknown must be broker-actionable, else escalation and closure both skip it")
	}
	if model.RequiresReview(items) {
		t.Error("policy_unknown is the broker's to answer, not a review flag on its own")
	}
}

// 0.2 — a repeat ask is not sent -------------------------------------------------

func TestIngestEmail_ContentFreeAcksDrawOneReply(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(nil, false, model.ErrSubmissionNotFound).Maybe()
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Maybe()
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()

	// the submission the follow-ups thread into, carrying the first reply's fingerprint
	var stored *model.Submission
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil).Run(func(a mock.Arguments) {
		stored = a.Get(1).(*model.Submission)
	})
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()
	svc := newSvcCfg(t, subs, aud, mail, &fakeStore{cl: smallChecklist()}, baseCfg(), nil, nil)

	if _, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "m1", FromAddress: "broker@x", Subject: "New Submission - CGL", Attachments: acord125(),
	}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	svc.Wait()
	if mail.sentCount() != 1 {
		t.Fatalf("the first ask should send, got %d", mail.sentCount())
	}
	if stored == nil || stored.LastReplyHash == "" {
		t.Fatal("the queued reply's fingerprint should be recorded on the submission")
	}

	// three "thanks, will send tomorrow" replies: same outstanding set every time
	existing := *stored
	subs.ExpectedCalls = nil
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(&existing, false, nil)
	subs.On("FindByDeterministicID", mock.Anything, mock.Anything).Return(nil, model.ErrSubmissionNotFound).Maybe()
	subs.On("SetLastReplyAt", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	for _, id := range []string{"ack1", "ack2", "ack3"} {
		if _, err := svc.IngestEmail(context.Background(), IngestRequest{
			MessageID: id, InReplyTo: "m1", FromAddress: "broker@x", Subject: "Re: New Submission - CGL",
			BodyText: "thanks, will send tomorrow",
		}); err != nil {
			t.Fatalf("ack %s: %v", id, err)
		}
	}
	svc.Wait()
	if mail.sentCount() != 1 {
		t.Errorf("three content-free acks must not re-send the same list: got %d replies total", mail.sentCount())
	}
}

// A complete submission thanks the broker once, not on every later message.
func TestReplyFingerprint_CompletionIsStable(t *testing.T) {
	a := model.ReplyFingerprint(model.ReplyKindCompletion, nil)
	b := model.ReplyFingerprint(model.ReplyKindCompletion, nil)
	if a != b {
		t.Error("the completion note must fingerprint identically so it is sent once")
	}
	if c := model.ReplyFingerprint(model.ReplyKindMissingItems, nil); c == a {
		t.Error("a different reply kind must fingerprint differently")
	}
}

func TestReplyFingerprint_TracksItemsAndCodes(t *testing.T) {
	base := []model.MissingItem{
		{ID: "acord_125", Code: model.ReasonNotProvided},
		{ID: "acord_126", Code: model.ReasonNotProvided},
	}
	reordered := []model.MissingItem{base[1], base[0]}
	if model.ReplyFingerprint(model.ReplyKindMissingItems, base) != model.ReplyFingerprint(model.ReplyKindMissingItems, reordered) {
		t.Error("item order must not change the fingerprint")
	}
	fewer := base[:1]
	if model.ReplyFingerprint(model.ReplyKindMissingItems, base) == model.ReplyFingerprint(model.ReplyKindMissingItems, fewer) {
		t.Error("a shorter outstanding list is a new ask and must send")
	}
	recoded := []model.MissingItem{{ID: "acord_125", Code: model.ReasonEncrypted}, base[1]}
	if model.ReplyFingerprint(model.ReplyKindMissingItems, base) == model.ReplyFingerprint(model.ReplyKindMissingItems, recoded) {
		t.Error("the same item for a different reason is a different ask")
	}
}

// 1.1 — a closed thread reopens instead of looping --------------------------------

func TestIngestEmail_ReplyToClosedSubmissionReopensIt(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	closed := &model.Submission{
		ID: "s-closed", State: model.StateClosed, PolicyType: "cgl", FromAddress: "broker@x",
		Emails: []model.Email{{DeterministicID: "e0", MessageID: "orig"}},
	}
	subs.On("FindByEmailReference", mock.Anything, mock.Anything).Return(closed, false, nil)
	subs.On("UpsertSubmissionWithReply", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Maybe()
	subs.On("UpsertEmail", mock.Anything, mock.Anything).Return(nil).Maybe()
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()
	svc := newSvcCfg(t, subs, aud, mail, &fakeStore{cl: smallChecklist()}, baseCfg(), nil, nil)

	res, err := svc.IngestEmail(context.Background(), IngestRequest{
		MessageID: "m2", InReplyTo: "orig", FromAddress: "broker@x", Subject: "Re: CGL",
		Attachments: acord125(),
	})
	if err != nil {
		t.Fatalf("a reply into a closed thread must not error — it would never be marked read: %v", err)
	}
	if res.State != model.StateAwaiting {
		t.Errorf("state: got %s, want awaiting (the case reopens)", res.State)
	}
}

// The poller's safety net: any deterministic transition failure is recognisable, so
// the message is marked read rather than reprocessed on every tick forever.
func TestTransitionState_InvalidTransitionIsIdentifiable(t *testing.T) {
	sub := &model.Submission{ID: "s1", State: model.StateClosed}
	err := sub.TransitionTo(model.StateEscalated, time.Now())
	if !errors.Is(err, model.ErrInvalidTransition) {
		t.Fatalf("got %v, want an error the poller can match on ErrInvalidTransition", err)
	}
}

// 1.2 — the close clock is not reset by inbound mail ------------------------------

func TestTransitionTo_CompletedAtIsNotMovedBySelfTransition(t *testing.T) {
	first := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	later := first.Add(72 * time.Hour)

	sub := &model.Submission{ID: "s1", State: model.StateAwaiting}
	if err := sub.TransitionTo(model.StateComplete, first); err != nil {
		t.Fatal(err)
	}
	if sub.CompletedAt == nil || !sub.CompletedAt.Equal(first) {
		t.Fatalf("completed_at: got %v, want %v", sub.CompletedAt, first)
	}
	// a thank-you reply lands: complete -> complete
	if err := sub.TransitionTo(model.StateComplete, later); err != nil {
		t.Fatal(err)
	}
	if !sub.CompletedAt.Equal(first) {
		t.Errorf("inbound mail must not defer the close clock: completed_at moved to %v", sub.CompletedAt)
	}
	if !sub.UpdatedAt.Equal(later) {
		t.Errorf("updated_at should still track activity, got %v", sub.UpdatedAt)
	}
	// reopening clears it, so a reopened case isn't closed on its old stamp
	if err := sub.TransitionTo(model.StateAwaiting, later); err != nil {
		t.Fatal(err)
	}
	if sub.CompletedAt != nil {
		t.Error("leaving complete must clear completed_at")
	}
}

// 1.3/1.4 — awaiting and open submissions terminate -------------------------------

func TestCheckClosures_ClosesStalledAwaitingAndOpen(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-100 * 24 * time.Hour)

	stalled := []model.Submission{
		// only an unreadable file outstanding: nothing broker-actionable, so escalation
		// declines it and no other sweep selects it
		{ID: "flagged-only", State: model.StateAwaiting, PolicyType: "cgl", LastActionAt: stale,
			MissingItems: []model.MissingItem{{ID: "acord_125", Code: model.ReasonUnreadable}}},
		// an unthreaded bounce created this and left it open; open has no other route out
		{ID: "orphan-bounce", State: model.StateOpen, PolicyType: "cgl", LastActionAt: stale},
	}
	subs.On("ListCompletedBefore", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
	subs.On("ListEscalatedSince", mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
	subs.On("ListStalledBefore", mock.Anything, mock.Anything, mock.Anything).Return(stalled, nil)

	var closed []string
	subs.On("UpsertSubmission", mock.Anything, mock.Anything).Return(nil).Run(func(a mock.Arguments) {
		s := a.Get(1).(*model.Submission)
		if s.State == model.StateClosed {
			closed = append(closed, s.ID)
		}
	})
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()

	cfg := baseCfg()
	cfg.Escalation.AutoCloseAfterHours = 336
	cfg.Escalation.AwaitingAutoCloseAfterHours = 2160
	svc := newSvcCfg(t, subs, aud, &fakeMail{}, &fakeStore{cl: smallChecklist()}, cfg, nil, nil)
	svc.setClock(func() time.Time { return now })

	if err := svc.CheckClosures(context.Background()); err != nil {
		t.Fatalf("closures: %v", err)
	}
	if len(closed) != 2 {
		t.Errorf("both stalled submissions should close, got %v", closed)
	}
}

// 2.3 — suppression is re-checked at dispatch, not only at build ------------------

func TestRedeliverOutbox_SkipsHeldSubmission(t *testing.T) {
	ctx := context.Background()
	ob := newFakeOutbox()
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()
	mail := &fakeMail{}
	svc := outboxSvc(ob, mail, subs, aud, 3)
	// the hold landed after the reply was queued but before its window elapsed
	subs.ExpectedCalls = nil
	subs.On("ReplyBlockedIDs", mock.Anything, mock.Anything).Return([]string{"s-held"}, nil)

	_ = ob.Enqueue(ctx, &model.OutboxEntry{SubmissionID: "s-held", Reply: model.Reply{ToAddress: "b@x", Subject: "re"}})
	if err := svc.RedeliverOutbox(ctx); err != nil {
		t.Fatalf("redeliver: %v", err)
	}
	if mail.sentCount() != 0 {
		t.Errorf("a held submission must not be sent to at dispatch, got %d", mail.sentCount())
	}
}

func TestRedeliverOutbox_KillSwitchSendsNothing(t *testing.T) {
	ctx := context.Background()
	ob := newFakeOutbox()
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	svc := outboxSvc(ob, mail, subs, aud, 3)
	svc.cfg = &config.Config{Reply: config.ReplyConfig{RepliesEnabled: boolp(false)}}

	_ = ob.Enqueue(ctx, &model.OutboxEntry{SubmissionID: "s1", Reply: model.Reply{ToAddress: "b@x", Subject: "re"}})
	if err := svc.RedeliverOutbox(ctx); err != nil {
		t.Fatalf("redeliver: %v", err)
	}
	if mail.sentCount() != 0 {
		t.Errorf("the kill switch must stop the sweeper too, got %d", mail.sentCount())
	}
}

// A reply queued before a hold describes pre-hold state; releasing must not send it.
func TestReconcileHold_ReleaseDropsPreHoldReply(t *testing.T) {
	ctx := context.Background()
	ob := newFakeOutbox()
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()
	svc := outboxSvc(ob, &fakeMail{}, subs, aud, 3)

	_ = ob.Enqueue(ctx, &model.OutboxEntry{SubmissionID: "s-rel", Reply: model.Reply{ToAddress: "b@x", Subject: "stale"}})
	subs.ExpectedCalls = nil
	subs.On("SubmissionIDsByMessageIDs", mock.Anything, mock.Anything).Return(nil, nil)
	subs.On("ListHeldIDs", mock.Anything).Return([]string{"s-rel"}, nil)
	subs.On("SetOnHold", mock.Anything, mock.Anything, mock.Anything).Return(nil)

	if err := svc.ReconcileHold(ctx, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, e := range listAllPending(t, ob) {
		if e.SubmissionID == "s-rel" {
			t.Error("a reply queued before the hold must not survive the release")
		}
	}
}

// 2.6 — encrypted detection --------------------------------------------------------

func TestIsEncrypted(t *testing.T) {
	locked := append([]byte("%PDF-1.7\n"), make([]byte, 200)...)
	locked = append(locked, []byte("trailer<</Encrypt 5 0 R>>\n%%EOF")...)

	cases := []struct {
		name string
		att  model.Attachment
		want bool
	}{
		{"declared pdf", model.Attachment{Filename: "a.pdf", ContentType: "application/pdf", Content: locked}, true},
		// mail clients routinely send PDFs as octet-stream; without sniffing this
		// routes to the agency as unreadable instead of asking the broker to unlock it
		{"octet-stream pdf", model.Attachment{Filename: "a.pdf", ContentType: "application/octet-stream", Content: locked}, true},
		{"magic-only pdf", model.Attachment{Filename: "scan", ContentType: "application/octet-stream", Content: locked}, true},
		// the literal appears in the body, not the trailer: a corrupt file, not a locked one
		{"literal in body", model.Attachment{
			Filename: "a.pdf", ContentType: "application/pdf",
			Content: append(append([]byte("%PDF-1.7\n/Encrypt here\n"), make([]byte, 8<<10)...), []byte("trailer<</Root 1 0 R>>")...),
		}, false},
		{"plain pdf", model.Attachment{Filename: "a.pdf", ContentType: "application/pdf", Content: []byte("%PDF-1.7\ntrailer<</Root 1 0 R>>")}, false},
		{"not a pdf", model.Attachment{Filename: "a.xlsx", ContentType: "application/vnd.ms-excel", Content: []byte("/Encrypt")}, false},
		{"empty", model.Attachment{Filename: "a.pdf", ContentType: "application/pdf"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEncrypted(tc.att); got != tc.want {
				t.Errorf("isEncrypted: got %v, want %v", got, tc.want)
			}
		})
	}
}

// A dead-lettered reply never reached the broker, so the repeat-suppression gate has
// to be released — otherwise the submission is stranded holding an ask nobody saw.
func TestRedeliverOutbox_DeadLetterClearsReplyHash(t *testing.T) {
	ctx := context.Background()
	ob := newFakeOutbox()
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()
	mail := &fakeMail{shouldFail: true}
	svc := outboxSvc(ob, mail, subs, aud, 1) // dead-letter on the first failure

	_ = ob.Enqueue(ctx, &model.OutboxEntry{SubmissionID: "s-dead", Reply: model.Reply{ToAddress: "b@x", Subject: "re"}})
	if err := svc.RedeliverOutbox(ctx); err != nil {
		t.Fatalf("redeliver: %v", err)
	}
	subs.AssertCalled(t, "ClearReplyHash", mock.Anything, "s-dead")

	// and the row is settled, not left to retry forever
	for _, e := range listAllPending(t, ob) {
		if e.SubmissionID == "s-dead" {
			t.Error("a dead-lettered row must not stay pending")
		}
	}
}

// The kill switch never resends, so the digest is the only place a withheld reply
// surfaces. A submission that has since been replied to is no longer withheld.
func TestSendDigest_ListsWithheldReplies(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	open := []model.Submission{
		{ID: "withheld", PolicyType: "cgl", State: model.StateAwaiting, SubjectLine: "Never sent",
			FromAddress: "a@x", CreatedAt: now, LastActionAt: now},
		{ID: "replied", PolicyType: "cgl", State: model.StateAwaiting, SubjectLine: "Got a reply",
			FromAddress: "b@x", CreatedAt: now, LastActionAt: now, LastReplyAt: now},
	}
	subs.On("ListOpen", mock.Anything, mock.Anything).Return(open, nil)
	subs.On("FilenameOnlyItems", mock.Anything, mock.Anything).Return(map[string][]string(nil), nil).Maybe()
	// both were skipped while the switch was off; only one is still un-replied
	aud.On("ListSubmissionIDsByEvent", mock.Anything, model.EventReplyDisabled, mock.Anything).
		Return([]string{"withheld", "replied"}, nil)
	aud.On("Append", mock.Anything, mock.Anything).Return(nil).Maybe()

	cfg := baseCfg()
	cfg.Digest = config.DigestConfig{IntervalHours: 24, Recipient: "ops@example.com", MaxRows: 500}
	svc := newSvcCfg(t, subs, aud, mail, &fakeStore{cl: smallChecklist()}, cfg, nil, nil)
	svc.setClock(func() time.Time { return now })

	if err := svc.SendDigest(context.Background()); err != nil {
		t.Fatalf("digest: %v", err)
	}
	if mail.sentCount() != 1 {
		t.Fatalf("expected one digest, got %d", mail.sentCount())
	}
	body := mail.sent[0].BodyText
	if !strings.Contains(body, "Reply withheld") {
		t.Errorf("digest should carry a withheld heading:\n%s", body)
	}
	withheldAt := strings.Index(body, "Never sent")
	repliedAt := strings.Index(body, "Got a reply")
	if withheldAt < 0 || repliedAt < 0 || withheldAt > repliedAt {
		t.Errorf("only the un-replied submission should lead under the withheld heading:\n%s", body)
	}
}

// With no recipient the digest cannot be delivered and escalation sends no mail of
// its own, so the service has no human-visible output at all.
func TestSendDigest_NoRecipientSendsNothing(t *testing.T) {
	subs := repomocks.NewSubmissionRepository(t)
	aud := repomocks.NewAuditRepository(t)
	mail := &fakeMail{}
	cfg := baseCfg()
	cfg.Digest = config.DigestConfig{IntervalHours: 24}
	svc := newSvcCfg(t, subs, aud, mail, &fakeStore{cl: smallChecklist()}, cfg, nil, nil)

	if err := svc.SendDigest(context.Background()); err != nil {
		t.Fatalf("digest: %v", err)
	}
	if mail.sentCount() != 0 {
		t.Errorf("no recipient means no send, got %d", mail.sentCount())
	}
	subs.AssertNotCalled(t, "ListOpen", mock.Anything, mock.Anything)
}
