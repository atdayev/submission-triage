package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"

	"github.com/atdayev/submission-triage/internal/config"
	"github.com/atdayev/submission-triage/internal/infrastructure/checklist"
	"github.com/atdayev/submission-triage/internal/infrastructure/classifier"
	"github.com/atdayev/submission-triage/internal/infrastructure/email"
	"github.com/atdayev/submission-triage/internal/infrastructure/llm"
	"github.com/atdayev/submission-triage/internal/model"
	"github.com/atdayev/submission-triage/internal/repository"
	"github.com/atdayev/submission-triage/pkg/logger"
	"github.com/atdayev/submission-triage/pkg/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var policyAliases = []struct {
	match  string
	policy string
}{
	{"general liability", "cgl"},
	{"cgl", "cgl"},
	{"business owners", "bop"},
	{"bop", "bop"},
	{"workers' comp", "workers_compensation"},
	{"workers comp", "workers_compensation"},
	{"property", "commercial_property"},
	{"cyber", "cyber_liability"},
}

type TextExtractor interface {
	Extract(data []byte) (string, error)
}

type clock func() time.Time

type Dependencies struct {
	Config         *config.Config
	Repository     *repository.Repository
	EmailSender    email.Sender
	Classifier     classifier.Classifier
	ChecklistStore checklist.Store
	TextExtractors map[string]TextExtractor
	LLM            llm.Client
	Metrics        *telemetry.Metrics
	Log            *logrus.Entry
}

type SubmissionsService struct {
	cfg        *config.Config
	repo       *repository.Repository
	mail       email.Sender
	classifier classifier.Classifier
	checklists checklist.Store
	extractors map[string]TextExtractor
	llm        llm.Client
	metrics    *telemetry.Metrics
	now        clock
	log        *logrus.Entry
	ingestSF   singleflight.Group

	replyJobs  chan replyJob
	replyWG    sync.WaitGroup // worker lifecycle
	inFlightWG sync.WaitGroup // per-enqueued-job

	replyCtx    context.Context // reply lifetime; canceled at Shutdown
	replyCancel context.CancelFunc
	drainGrace  time.Duration
}

const defaultReplyDrainGrace = 10 * time.Second

type replyJob struct {
	ctx           context.Context
	sub           model.Submission
	missing       []model.MissingItem
	inbound       model.Email
	policyUnknown bool
}

type IngestRequest struct {
	MessageID   string
	InReplyTo   string
	References  []string
	FromAddress string
	FromName    string
	ToAddresses []string
	Subject     string
	BodyText    string
	ReceivedAt  time.Time
	Attachments []model.Attachment
	Source      string // inbound channel: "postmark" | "imap"
}

type IngestResult struct {
	SubmissionID string
	State        model.State
	IsDuplicate  bool
	MissingItems []model.MissingItem
	ReplyQueued  bool
}

func NewSubmissionsService(d Dependencies) *SubmissionsService {
	workers := d.Config.Reply.Workers
	if workers <= 0 {
		workers = 4
	}
	queue := d.Config.Reply.QueueSize
	if queue <= 0 {
		queue = 64
	}
	metrics := d.Metrics
	if metrics == nil {
		metrics = telemetry.NoopMetrics()
	}
	s := &SubmissionsService{
		cfg:        d.Config,
		repo:       d.Repository,
		mail:       d.EmailSender,
		classifier: d.Classifier,
		checklists: d.ChecklistStore,
		extractors: d.TextExtractors,
		llm:        d.LLM,
		metrics:    metrics,
		now:        time.Now,
		log:        d.Log,
		replyJobs:  make(chan replyJob, queue),
	}
	s.replyCtx, s.replyCancel = context.WithCancel(context.Background())
	s.drainGrace = d.Config.HTTP.ShutdownTimeout()
	if s.drainGrace <= 0 {
		s.drainGrace = defaultReplyDrainGrace
	}
	for i := 0; i < workers; i++ {
		s.replyWG.Add(1)
		go s.replyWorker()
	}
	return s
}

// replyJobContext detaches a reply from the request lifecycle but keeps it
// cancelable at shutdown, carrying over request-scoped logging values.
func (s *SubmissionsService) replyJobContext(reqCtx context.Context) context.Context {
	jobCtx := logger.ContextWithLogger(s.replyCtx, logger.GetLoggerFromContext(reqCtx))
	if rid := logger.RequestIDFromContext(reqCtx); rid != "" {
		jobCtx = logger.ContextWithRequestID(jobCtx, rid)
	}
	return jobCtx
}

func (s *SubmissionsService) replyWorker() {
	defer s.replyWG.Done()
	for job := range s.replyJobs {
		s.sendAndRecordReply(job.ctx, &job.sub, job.missing, job.inbound, job.policyUnknown)
		s.inFlightWG.Done()
	}
}

func (s *SubmissionsService) setClock(c clock) { s.now = c }

func (s *SubmissionsService) IngestEmail(ctx context.Context, req IngestRequest) (IngestResult, error) {
	if req.ReceivedAt.IsZero() {
		req.ReceivedAt = s.now()
	}

	emailID := computeEmailID(req.MessageID, req.BodyText, req.Attachments)
	start := time.Now()

	// collapse parallel deliveries of the same email, else duplicate rows
	v, err, _ := s.ingestSF.Do(emailID, func() (any, error) {
		return s.ingestEmailInner(ctx, req, emailID)
	})

	s.metrics.IngestDuration.Record(ctx, time.Since(start).Seconds())
	if err != nil {
		s.metrics.IngestTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("state", "error"),
		))
		return IngestResult{}, err
	}
	result := v.(IngestResult)
	s.metrics.IngestTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("state", string(result.State)),
		attribute.Bool("duplicate", result.IsDuplicate),
	))
	return result, nil
}

func (s *SubmissionsService) ingestEmailInner(ctx context.Context, req IngestRequest, emailID string) (IngestResult, error) {
	refs := cleanThreadRefs(req.MessageID, req.InReplyTo, req.References)
	sub, ambiguous, err := s.repo.Submissions.FindByEmailReference(ctx, refs)
	switch {
	case err == nil:
		for _, e := range sub.Emails {
			if e.DeterministicID == emailID {
				s.audit(ctx, sub.ID, model.EventEmailDuplicate, map[string]any{
					"deterministic_id": emailID,
					"source":           req.Source,
				})
				return IngestResult{
					SubmissionID: sub.ID,
					State:        sub.State,
					IsDuplicate:  true,
				}, nil
			}
		}
		if ambiguous {
			s.audit(ctx, sub.ID, model.EventThreadAmbiguous, map[string]any{
				"refs": refs,
			})
		}
		s.audit(ctx, sub.ID, model.EventThreadMatched, map[string]any{
			"matched_by": "thread_headers",
			"message_id": req.MessageID,
		})
	case errors.Is(err, model.ErrSubmissionNotFound):
		sub, err = s.createSubmission(ctx, req)
		if err != nil {
			return IngestResult{}, fmt.Errorf("create submission: %w", err)
		}
	default:
		return IngestResult{}, fmt.Errorf("find by reference: %w", err)
	}

	inbound := model.Email{
		DeterministicID: emailID,
		SubmissionID:    sub.ID,
		Direction:       model.DirectionInbound,
		MessageID:       req.MessageID,
		InReplyTo:       req.InReplyTo,
		References:      req.References,
		FromAddress:     req.FromAddress,
		FromName:        req.FromName,
		ToAddresses:     req.ToAddresses,
		Subject:         req.Subject,
		BodyText:        req.BodyText,
		ReceivedAt:      req.ReceivedAt,
		Attachments:     req.Attachments,
	}
	sub.AttachEmail(inbound)
	sub.MarkAction(s.now())

	s.audit(ctx, sub.ID, model.EventEmailReceived, map[string]any{
		"deterministic_id": emailID,
		"message_id":       req.MessageID,
		"attachment_count": len(req.Attachments),
		"source":           req.Source,
	})

	checklistDef, hasChecklist := s.checklists.Get(sub.PolicyType)
	policyUnknown := !hasChecklist
	if policyUnknown {
		sub.PolicyType = model.PolicyTypeUnknown
		s.audit(ctx, sub.ID, model.EventPolicyUnknown, map[string]any{
			"subject_hint": req.Subject,
		})
	} else {
		docs := s.classifyAttachments(ctx, sub, inbound, checklistDef)
		for _, d := range docs {
			sub.AttachDocument(d)
		}
	}

	var missing []model.MissingItem
	if !policyUnknown {
		missing = model.EvaluateChecklist(*sub, checklistDef)
	} else {
		missing = []model.MissingItem{{
			ID:          "policy_unknown",
			Description: "Policy type not yet determined",
			Reason:      "awaiting clarification from sender",
		}}
	}
	sub.MissingItems = missing
	missingIDs := make([]string, 0, len(missing))
	for _, m := range missing {
		missingIDs = append(missingIDs, m.ID)
	}

	s.audit(ctx, sub.ID, model.EventChecklistEvaluated, map[string]any{
		"policy_type":   sub.PolicyType,
		"missing_count": len(missing),
		"missing_ids":   missingIDs,
	})

	prevState := sub.State
	next := model.StateComplete
	if len(missing) > 0 {
		next = model.StateAwaiting
	}
	if err := sub.TransitionTo(next, s.now()); err != nil {
		return IngestResult{}, fmt.Errorf("transition: %w", err)
	}
	if prevState != sub.State {
		s.audit(ctx, sub.ID, model.EventStateTransitioned, map[string]any{
			"from": string(prevState),
			"to":   string(sub.State),
		})
	}

	// age from the email's own time, not when we processed it
	sub.LastActionAt = inbound.ReceivedAt

	if err := s.repo.Submissions.UpsertSubmission(ctx, sub); err != nil {
		return IngestResult{}, fmt.Errorf("upsert: %w", err)
	}

	result := IngestResult{
		SubmissionID: sub.ID,
		State:        sub.State,
		MissingItems: missing,
	}
	job := replyJob{
		ctx:           s.replyJobContext(ctx),
		sub:           *sub,
		missing:       missing,
		inbound:       inbound,
		policyUnknown: policyUnknown,
	}
	s.inFlightWG.Add(1)
	select {
	case s.replyJobs <- job:
		result.ReplyQueued = true
	default:
		// queue full: drop instead of blocking the handler (audited + metered)
		s.inFlightWG.Done()
		s.metrics.ReplyDroppedTotal.Add(ctx, 1)
		s.audit(ctx, sub.ID, model.EventReplyFailed, map[string]any{
			"error":   "reply queue full; reply dropped",
			"backlog": cap(s.replyJobs),
		})
	}
	return result, nil
}

// Wait blocks until queued replies finish. Re-entrant; doesn't stop the pool.
func (s *SubmissionsService) Wait() { s.inFlightWG.Wait() }

// Shutdown drains queued replies, then stops the workers. Call once. Past the
// grace window, in-flight sends are canceled so the process still exits.
func (s *SubmissionsService) Shutdown() {
	defer s.replyCancel()
	drained := make(chan struct{})
	go func() {
		s.inFlightWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(s.drainGrace):
		s.log.Warn("reply drain exceeded grace; canceling in-flight sends")
		s.replyCancel()
		s.inFlightWG.Wait()
	}
	close(s.replyJobs)
	s.replyWG.Wait()
}

func (s *SubmissionsService) knownChecklistNames() []string {
	all := s.checklists.All()
	out := make([]string, 0, len(all))
	for _, c := range all {
		if c.Name != "" {
			out = append(out, c.Name)
		}
	}
	return out
}

func (s *SubmissionsService) sendAndRecordReply(ctx context.Context, sub *model.Submission, missing []model.MissingItem, inbound model.Email, policyUnknown bool) {
	var reply model.Reply
	switch {
	case policyUnknown:
		reply = model.BuildPolicyUnknownReply(*sub, inbound, s.knownChecklistNames())
	case len(missing) > 0:
		reply = model.BuildMissingItemsReply(*sub, missing, inbound)
	default:
		reply = model.BuildCompletionReply(*sub, inbound)
	}

	providerMsgID, err := s.mail.SendThreadedReply(ctx, reply)
	if err != nil {
		s.metrics.ReplySendTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "failed")))
		logger.GetLoggerFromContext(ctx).WithError(err).Warn("reply send failed; submission state preserved")
		s.audit(ctx, sub.ID, model.EventReplyFailed, map[string]any{
			"error": err.Error(),
		})
		return
	}

	s.metrics.ReplySendTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "success")))
	outbound := outboundEmail(reply, providerMsgID, s.now())
	if err := s.repo.Submissions.UpsertEmail(ctx, &outbound); err != nil {
		s.log.WithError(err).WithField("submission_id", sub.ID).Warn("outbound email upsert failed")
	}
	s.audit(ctx, sub.ID, model.EventReplySent, map[string]any{
		"provider_msg_id": providerMsgID,
		"to":              reply.ToAddress,
		"subject":         reply.Subject,
		"via":             s.mail.Name(),
	})
}

func (s *SubmissionsService) CheckEscalations(ctx context.Context) error {
	now := s.now()
	globalThreshold := s.cfg.Escalation.Threshold()
	minThreshold := globalThreshold
	for _, c := range s.checklists.All() {
		t := time.Duration(c.Escalation.ThresholdHours) * time.Hour
		if t > 0 && t < minThreshold {
			minThreshold = t
		}
	}
	cutoff := now.Add(-minThreshold)
	stale, err := s.repo.Submissions.ListStale(ctx, cutoff.UnixNano(), 100)
	if err != nil {
		return fmt.Errorf("list stale: %w", err)
	}
	for i := range stale {
		sub := &stale[i]
		threshold := globalThreshold
		if cl, ok := s.checklists.Get(sub.PolicyType); ok && cl.Escalation.ThresholdHours > 0 {
			threshold = time.Duration(cl.Escalation.ThresholdHours) * time.Hour
		}
		if now.Sub(sub.LastActionAt) < threshold {
			continue
		}
		if err := sub.TransitionTo(model.StateEscalated, now); err != nil {
			s.log.WithError(err).WithField("submission_id", sub.ID).Warn("could not escalate")
			continue
		}
		if err := s.repo.Submissions.UpsertSubmission(ctx, sub); err != nil {
			s.log.WithError(err).WithField("submission_id", sub.ID).Warn("escalation upsert failed")
			continue
		}
		s.metrics.EscalatedTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("policy_type", sub.PolicyType),
		))
		s.audit(ctx, sub.ID, model.EventEscalated, map[string]any{
			"last_action_at": sub.LastActionAt,
			"threshold":      threshold.String(),
		})
	}
	return nil
}

func (s *SubmissionsService) CheckClosures(ctx context.Context) error {
	autoClose := s.cfg.Escalation.AutoCloseAfter()
	if autoClose <= 0 {
		return nil
	}
	now := s.now()
	cutoff := now.Add(-autoClose)
	subs, err := s.repo.Submissions.ListCompletedBefore(ctx, cutoff.UnixNano(), 100)
	if err != nil {
		return fmt.Errorf("list completed-before: %w", err)
	}
	for i := range subs {
		sub := &subs[i]
		if err := sub.TransitionTo(model.StateClosed, now); err != nil {
			s.log.WithError(err).WithField("submission_id", sub.ID).Warn("could not auto-close")
			continue
		}
		if err := s.repo.Submissions.UpsertSubmission(ctx, sub); err != nil {
			s.log.WithError(err).WithField("submission_id", sub.ID).Warn("auto-close upsert failed")
			continue
		}
		s.metrics.ClosedTotal.Add(ctx, 1)
		s.audit(ctx, sub.ID, model.EventClosed, map[string]any{
			"reason":        "auto_close_after_complete",
			"auto_close_at": now,
		})
	}
	return nil
}

// SendEscalationDigest no-ops without a recipient or any escalations.
func (s *SubmissionsService) SendEscalationDigest(ctx context.Context) error {
	interval := s.cfg.Escalation.DigestInterval()
	recipient := s.firstDigestRecipient()
	if recipient == "" || interval <= 0 {
		return nil
	}
	since := s.now().Add(-interval)
	subs, err := s.repo.Submissions.ListEscalatedSince(ctx, since.UnixNano(), 500)
	if err != nil {
		return fmt.Errorf("list escalated-since: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}

	var body strings.Builder
	fmt.Fprintf(&body, "Escalated submissions in the last %s:\n\n", interval)
	for _, sub := range subs {
		fmt.Fprintf(&body, "  - %s | %s | from %s | last action %s\n",
			sub.ID, sub.PolicyType, sub.FromAddress, sub.LastActionAt.UTC().Format(time.RFC3339))
	}
	reply := model.Reply{
		ToAddress: recipient,
		Subject:   fmt.Sprintf("[submission-triage] Escalation digest (%d cases)", len(subs)),
		BodyText:  body.String(),
	}
	if _, err := s.mail.SendThreadedReply(ctx, reply); err != nil {
		return fmt.Errorf("send digest: %w", err)
	}
	s.metrics.DigestSentTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.Int("count", len(subs)),
	))
	s.audit(ctx, "", model.EventDigestSent, map[string]any{
		"recipient": recipient,
		"count":     len(subs),
	})
	return nil
}

func (s *SubmissionsService) firstDigestRecipient() string {
	for _, c := range s.checklists.All() {
		if c.Escalation.DigestRecipient != "" {
			return c.Escalation.DigestRecipient
		}
	}
	return s.cfg.Escalation.DigestRecipient
}

func (s *SubmissionsService) createSubmission(ctx context.Context, req IngestRequest) (*model.Submission, error) {
	policy := inferPolicyType(req.Subject, s.checklists.All())
	threadKey := req.MessageID
	if len(req.References) > 0 {
		threadKey = req.References[0]
	}
	sub := model.NewSubmission(
		uuid.NewString(),
		policy,
		req.Subject,
		req.FromAddress,
		req.FromName,
		threadKey,
		s.now(),
	)
	s.audit(ctx, sub.ID, model.EventStateTransitioned, map[string]any{
		"from": "",
		"to":   string(sub.State),
		"note": "created",
	})
	return &sub, nil
}

func (s *SubmissionsService) classifyAttachments(ctx context.Context, sub *model.Submission, e model.Email, cl model.Checklist) []model.Document {
	out := make([]model.Document, 0, len(e.Attachments))
	itemByID := make(map[string]model.RequiredItem, len(cl.Required))
	for _, item := range cl.Required {
		itemByID[item.ID] = item
	}
	for _, a := range e.Attachments {
		text := s.extractText(a)
		result, err := s.classifier.Classify(ctx, classifier.Input{
			Filename:    a.Filename,
			ContentType: a.ContentType,
			BodyText:    text,
			PolicyType:  cl.PolicyType,
			Checklist:   cl,
		})
		if err != nil {
			s.audit(ctx, sub.ID, model.EventLLMFailed, map[string]any{
				"filename": a.Filename,
				"op":       "classify",
				"error":    err.Error(),
			})
		}
		if result.Usage != nil {
			s.auditLLMCall(ctx, sub.ID, "classify", a.Filename, *result.Usage)
		}

		doc := model.Document{
			ID:            uuid.NewString(),
			SubmissionID:  sub.ID,
			EmailID:       e.DeterministicID,
			Filename:      a.Filename,
			ContentType:   a.ContentType,
			SizeBytes:     a.Size,
			SHA256:        a.SHA256,
			ClassifiedAs:  result.CandidateID,
			Confidence:    result.Confidence,
			ClassifiedBy:  result.By,
			ExtractedText: text,
			CreatedAt:     s.now(),
		}

		s.audit(ctx, sub.ID, model.EventDocumentClassified, map[string]any{
			"filename":      a.Filename,
			"classified_as": result.CandidateID,
			"confidence":    result.Confidence,
			"by":            result.By,
			"reason":        result.Reason,
		})

		if item, ok := itemByID[result.CandidateID]; ok && item.RequiresField != nil {
			if extracted := s.extractField(ctx, sub.ID, a.Filename, text, item.RequiresField); extracted != nil {
				doc.ExtractedFields = map[string]any{item.RequiresField.Name: extracted}
			}
		}
		out = append(out, doc)
	}
	return out
}

// returns nil if no LLM or the call fails; the checklist then soft-passes.
func (s *SubmissionsService) extractField(ctx context.Context, submissionID, filename, text string, rf *model.RequiresField) any {
	if s.llm == nil {
		return nil
	}
	resp, err := s.llm.ExtractField(ctx, llm.FieldExtractionRequest{
		Filename:         filename,
		TextSample:       text,
		FieldName:        rf.Name,
		FieldDescription: fmt.Sprintf("expected type %s", rf.Type),
		FieldType:        string(rf.Type),
	})
	s.auditLLMCall(ctx, submissionID, "extract_field", filename, resp.Usage)
	if err != nil {
		s.audit(ctx, submissionID, model.EventLLMFailed, map[string]any{
			"filename":   filename,
			"op":         "extract_field",
			"field_name": rf.Name,
			"error":      err.Error(),
		})
		return nil
	}
	s.audit(ctx, submissionID, model.EventFieldExtracted, map[string]any{
		"filename":   filename,
		"field_name": rf.Name,
		"value":      resp.Value,
		"confidence": resp.Confidence,
		"reason":     resp.Reason,
	})
	return resp.Value
}

func (s *SubmissionsService) auditLLMCall(ctx context.Context, submissionID, op, filename string, u llm.Usage) {
	if u.PromptHash == "" && u.InputTokens == 0 && u.OutputTokens == 0 {
		return
	}
	s.metrics.LLMCallTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("model", u.Model),
		attribute.String("op", op),
		attribute.String("outcome", "success"),
	))
	if u.LatencyMs > 0 {
		s.metrics.LLMCallDuration.Record(ctx, float64(u.LatencyMs)/1000.0)
	}
	s.audit(ctx, submissionID, model.EventLLMCall, map[string]any{
		"op":                 op,
		"filename":           filename,
		"model":              u.Model,
		"prompt_hash":        u.PromptHash,
		"latency_ms":         u.LatencyMs,
		"input_tokens":       u.InputTokens,
		"output_tokens":      u.OutputTokens,
		"estimated_cost_usd": u.EstimatedCostUSD,
	})
}

func (s *SubmissionsService) extractText(a model.Attachment) string {
	if len(a.Content) == 0 {
		return ""
	}
	ext, ok := s.extractors[strings.ToLower(a.ContentType)]
	if !ok || ext == nil {
		return ""
	}
	text, err := ext.Extract(a.Content)
	if err != nil {
		s.log.WithError(err).WithField("filename", a.Filename).Warn("text extraction failed")
		return ""
	}
	return text
}

func (s *SubmissionsService) audit(ctx context.Context, submissionID string, evt model.EventType, payload map[string]any) {
	requestID := logger.RequestIDFromContext(ctx)
	if err := s.repo.Audit.Append(ctx, &model.AuditEntry{
		SubmissionID: submissionID,
		EventType:    evt,
		Payload:      payload,
		RequestID:    requestID,
	}); err != nil {
		s.log.WithError(err).Warn("audit append failed")
	}
}

func computeEmailID(messageID, body string, atts []model.Attachment) string {
	h := sha256.New()
	h.Write([]byte(messageID))
	h.Write([]byte{0})
	h.Write([]byte(body))
	hashes := make([]string, 0, len(atts))
	for _, a := range atts {
		if a.SHA256 != "" {
			hashes = append(hashes, a.SHA256)
			continue
		}
		ah := sha256.Sum256(a.Content)
		hashes = append(hashes, hex.EncodeToString(ah[:]))
	}
	sort.Strings(hashes)
	for _, hash := range hashes {
		h.Write([]byte{0})
		h.Write([]byte(hash))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func cleanThreadRefs(messageID, inReplyTo string, references []string) []string {
	raw := append([]string{messageID, inReplyTo}, references...)
	seen := map[string]struct{}{}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func inferPolicyType(subject string, all []model.Checklist) string {
	lower := strings.ToLower(subject)
	for _, c := range all {
		if strings.Contains(lower, strings.ToLower(c.Name)) {
			return c.PolicyType
		}
		if strings.Contains(lower, strings.ToLower(c.PolicyType)) {
			return c.PolicyType
		}
	}
	for _, a := range policyAliases {
		if strings.Contains(lower, a.match) {
			return a.policy
		}
	}
	return model.PolicyTypeUnknown
}

func outboundEmail(r model.Reply, providerMsgID string, now time.Time) model.Email {
	det := sha256.Sum256([]byte(providerMsgID + r.Subject + r.ToAddress))
	return model.Email{
		DeterministicID: hex.EncodeToString(det[:]),
		SubmissionID:    r.SubmissionID,
		Direction:       model.DirectionOutbound,
		ToAddresses:     []string{r.ToAddress},
		Subject:         r.Subject,
		BodyText:        r.BodyText,
		ReceivedAt:      now,
		ProviderMsgID:   providerMsgID,
		InReplyTo:       r.InReplyTo,
		References:      r.References,
	}
}
